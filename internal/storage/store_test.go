/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func readTarMembers(t *testing.T, rootPath, relPath string) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join(rootPath, relPath))
	if err != nil {
		t.Fatalf("open %s: %v", relPath, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		buf := &bytes.Buffer{}
		if _, err := io.Copy(buf, tr); err != nil {
			t.Fatalf("copy: %v", err)
		}
		out[hdr.Name] = buf.String()
	}
}

func TestNew_EmptyRootRejected(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Errorf("New(\"\") = nil error, want error")
	}
}

func TestNew_CreatesRootDirectory(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "deeply", "nested")
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat(root); err != nil {
		t.Errorf("expected %s to exist: %v", root, err)
	}
}

func TestNew_RootInsideUnwritableParent(t *testing.T) {
	// /proc/1 cannot be MkdirAll'd; New must surface the error rather than
	// silently swallow it.
	if _, err := New("/proc/1/jaas-test-store"); err == nil {
		t.Errorf("expected error for unwritable parent")
	}
}

func TestPut_WritesTarballAndReturnsResult(t *testing.T) {
	s := newTestStore(t)
	rootPath := s.fs.Name()

	entries := []FileEntry{
		{Path: "main.jsonnet", Content: []byte(`{ a: 1 }`)},
		{Path: "helper.libsonnet", Content: []byte(`{ helper: true }`)},
	}
	got, err := s.Put(context.Background(), "team-a", "demo", "abc123", entries)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	want := filepath.Join("team-a", "demo", "abc123.tar.gz")
	if got.Path != want {
		t.Errorf("Path = %q, want %q", got.Path, want)
	}
	if got.SizeBytes == 0 {
		t.Errorf("SizeBytes = 0, want > 0")
	}
	if got.DigestSHA256 == "" {
		t.Errorf("DigestSHA256 is empty")
	}

	members := readTarMembers(t, rootPath, got.Path)
	if members["main.jsonnet"] != `{ a: 1 }` {
		t.Errorf("main.jsonnet = %q, want %q", members["main.jsonnet"], `{ a: 1 }`)
	}
	if members["helper.libsonnet"] != `{ helper: true }` {
		t.Errorf("helper.libsonnet = %q, want %q", members["helper.libsonnet"], `{ helper: true }`)
	}
}

func TestPut_DigestIsDeterministicAcrossRuns(t *testing.T) {
	a := newTestStore(t)
	b := newTestStore(t)
	entries := []FileEntry{
		{Path: "main.jsonnet", Content: []byte("x")},
		{Path: "helper.libsonnet", Content: []byte("y")},
	}
	ra, err := a.Put(context.Background(), "ns", "n", "feed", entries)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.Put(context.Background(), "ns", "n", "feed", entries)
	if err != nil {
		t.Fatal(err)
	}
	if ra.DigestSHA256 != rb.DigestSHA256 {
		t.Errorf("digest non-deterministic: a=%q b=%q", ra.DigestSHA256, rb.DigestSHA256)
	}
}

func TestPut_DigestIsIndependentOfFileOrder(t *testing.T) {
	s := newTestStore(t)
	ascending, err := s.Put(context.Background(), "ns", "asc", "r", []FileEntry{
		{Path: "a.jsonnet", Content: []byte("1")},
		{Path: "b.jsonnet", Content: []byte("2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	descending, err := s.Put(context.Background(), "ns", "desc", "r", []FileEntry{
		{Path: "b.jsonnet", Content: []byte("2")},
		{Path: "a.jsonnet", Content: []byte("1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ascending.DigestSHA256 != descending.DigestSHA256 {
		t.Errorf("digest changed with input order")
	}
}

func TestPut_OverwritesPreviousFileAtSameRevision(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(context.Background(), "ns", "n", "r", []FileEntry{{Path: "f", Content: []byte("v1")}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Put(context.Background(), "ns", "n", "r", []FileEntry{{Path: "f", Content: []byte("v2")}})
	if err != nil {
		t.Fatal(err)
	}
	members := readTarMembers(t, s.fs.Name(), got.Path)
	if members["f"] != "v2" {
		t.Errorf("got %q, want overwrite with v2", members["f"])
	}
}

func TestPut_TraversalInComponentRejected(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		ns, name, rev string
	}{
		{"..", "n", "r"},
		{"ns", "../n", "r"},
		{"ns", "n", "../r"},
		{"", "n", "r"},
		{"ns", "", "r"},
		{"ns", "n", ""},
		{"ns/x", "n", "r"},
	}
	for _, c := range cases {
		if _, err := s.Put(context.Background(), c.ns, c.name, c.rev, nil); err == nil {
			t.Errorf("Put(%q,%q,%q) = nil error, want rejection", c.ns, c.name, c.rev)
		}
	}
}

func TestPut_TraversalInEntryPathRejected(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Put(context.Background(), "ns", "n", "r", []FileEntry{
		{Path: "../escape", Content: []byte("x")},
	})
	if err == nil || !strings.Contains(err.Error(), "traversal") {
		t.Errorf("got %v, want traversal rejection", err)
	}
}

func TestDelete_RemovesEverythingUnderNamespaceAndName(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(context.Background(), "ns", "n", "a1", []FileEntry{{Path: "f", Content: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "ns", "n", "a2", []FileEntry{{Path: "f", Content: []byte("y")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), "ns", "n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n")); !os.IsNotExist(err) {
		t.Errorf("dir still exists after Delete: %v", err)
	}
}

func TestDelete_TraversalRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete(context.Background(), "../escape", "n"); err == nil {
		t.Errorf("Delete with traversal accepted")
	}
}

func TestDelete_EmptyComponentRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete(context.Background(), "", "n"); err == nil {
		t.Errorf("Delete with empty namespace accepted")
	}
	if err := s.Delete(context.Background(), "ns", ""); err == nil {
		t.Errorf("Delete with empty name accepted")
	}
}

func TestPrune_DropsRevisionsNotInKeepSet(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(context.Background(), "ns", "n", "deadbeef", []FileEntry{{Path: "f", Content: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "ns", "n", "facecab", []FileEntry{{Path: "f", Content: []byte("y")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(context.Background(), "ns", "n", []string{"facecab"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "old.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("old revision still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "facecab.tar.gz")); err != nil {
		t.Errorf("new revision missing after prune: %v", err)
	}
}

func TestPrune_KeepsMultipleRevisions(t *testing.T) {
	s := newTestStore(t)
	for _, rev := range []string{"a1", "a2", "a3", "a4"} {
		if _, err := s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	// Keep r2 and r4 — r1 and r3 must go.
	if err := s.Prune(context.Background(), "ns", "n", []string{"a2", "a4"}, 0); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"a1", "a3"} {
		if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", gone+".tar.gz")); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned: %v", gone, err)
		}
	}
	for _, kept := range []string{"a2", "a4"} {
		if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", kept+".tar.gz")); err != nil {
			t.Errorf("%s should have survived prune: %v", kept, err)
		}
	}
}

func TestPrune_EmptyKeepSetIsNoOp(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(context.Background(), "ns", "n", "abadbabe", []FileEntry{{Path: "f", Content: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(context.Background(), "ns", "n", nil, 0); err != nil {
		t.Errorf("empty keep-set Prune = %v, want nil (no-op)", err)
	}
	// The existing file must survive — empty keep-set must NOT wipe.
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "abadbabe.tar.gz")); err != nil {
		t.Errorf("file removed by empty-keep-set Prune: %v", err)
	}
}

func TestPrune_IsNoOpWhenDirAbsent(t *testing.T) {
	s := newTestStore(t)
	if err := s.Prune(context.Background(), "ns", "missing", []string{"feed"}, 0); err != nil {
		t.Errorf("Prune on absent dir = %v, want nil", err)
	}
}

func TestPrune_EmptyComponentRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.Prune(context.Background(), "", "n", []string{"r"}, 0); err == nil {
		t.Errorf("Prune with empty namespace accepted")
	}
	if err := s.Prune(context.Background(), "ns", "n", []string{"../r"}, 0); err == nil {
		t.Errorf("Prune with traversal in keep entry accepted")
	}
}

func TestStore_ConcurrentPutsAreSafe(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rev := "feed" + string(rune('a'+i%26))
			_, _ = s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}})
		}(i)
	}
	wg.Wait()
}

func TestValidNoTraversal_RejectsAllUnsafeShapes(t *testing.T) {
	cases := []struct{ name string }{
		{""}, {"."}, {".."}, {"a/b"}, {"a\\b"},
	}
	for _, c := range cases {
		if err := validNoTraversal(c.name); err == nil {
			t.Errorf("validNoTraversal(%q) = nil, want error", c.name)
		}
	}
}

func TestValidNoTraversal_AcceptsCleanComponents(t *testing.T) {
	if err := validNoTraversal("ns", "name", "feed"); err != nil {
		t.Errorf("validNoTraversal on clean components = %v", err)
	}
}

func TestValidTarEntryPath_RejectsAllUnsafeShapes(t *testing.T) {
	cases := []string{
		"", "/abs/path", "../escape", "a/../escape", "deeply/nested/../escape",
		// NUL truncates the name for C extractors; backslash is a Windows path
		// separator — both can escape on extraction and must be rejected, the
		// same as the Fetcher rejects them on incoming artifacts.
		"a\x00b", "evil\\..\\x", "a\\b",
	}
	for _, c := range cases {
		if err := validTarEntryPath(c); err == nil {
			t.Errorf("validTarEntryPath(%q) = nil, want error", c)
		}
	}
}

func TestValidTarEntryPath_AcceptsCleanRelativePaths(t *testing.T) {
	cases := []string{
		"main.jsonnet", "helper.libsonnet", "subdir/file.libsonnet",
	}
	for _, c := range cases {
		if err := validTarEntryPath(c); err != nil {
			t.Errorf("validTarEntryPath(%q) = %v, want nil", c, err)
		}
	}
}

func TestStore_Sweep_RemovesOldTmpFilesOnly(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(context.Background(), "ns", "snip", strings.Repeat("a", 64), []FileEntry{{Path: "f", Content: []byte("ok")}}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	root := s.RootPath()
	// Sweep only touches files matching our <64-hex>.tar.gz.tmp shape;
	// fixtures must use a real sha256-shaped revision name.
	oldHex := strings.Repeat("b", 64)
	freshHex := strings.Repeat("c", 64)
	oldTmp := filepath.Join(root, "ns/snip/"+oldHex+".tar.gz.tmp")
	if err := os.WriteFile(oldTmp, []byte("crash residue"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldTmp, past, past); err != nil {
		t.Fatal(err)
	}
	freshTmp := filepath.Join(root, "ns/snip/"+freshHex+".tar.gz.tmp")
	if err := os.WriteFile(freshTmp, []byte("in-flight"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := s.Sweep(context.Background(), 1*time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("Sweep removed=%d, want 1", removed)
	}
	if _, err := os.Stat(oldTmp); err == nil {
		t.Error("old .tmp survived Sweep")
	}
	if _, err := os.Stat(freshTmp); err != nil {
		t.Error("fresh .tmp was incorrectly swept")
	}
	if _, err := os.Stat(filepath.Join(root, "ns/snip/"+strings.Repeat("a", 64)+".tar.gz")); err != nil {
		t.Errorf("final tarball lost to sweep: %v", err)
	}
}

// TestStore_Sweep_BlockedByInFlightPutOnSameKey pins the per-key
// lock invariant: Sweep must wait for an in-flight Put on the same
// (ns, name) before stat-and-remove runs. A Put running longer than
// maxTmpAge would otherwise see its in-flight .tmp reaped — Rename
// fails with "no such file" and the snippet's reconcile errors out.
//
// The test drives a Put on a goroutine while Sweep runs from the
// main goroutine. To force the ordering, we acquire the per-key
// lock manually, start Sweep (which will block), then release the
// lock and let Sweep proceed.
func TestStore_Sweep_BlockedByInFlightPutOnSameKey(t *testing.T) {
	s := newTestStore(t)
	root := s.RootPath()

	// Seed a stale .tmp that should be reaped.
	revHex := strings.Repeat("d", 64)
	dir := filepath.Join(root, "ns/snip")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(dir, revHex+".tar.gz.tmp")
	if err := os.WriteFile(tmpPath, []byte("stale residue"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(tmpPath, past, past); err != nil {
		t.Fatal(err)
	}

	// Hold the per-key lock as if a Put were in flight. Sweep on
	// this key must block.
	hold := s.lockFor("ns", "snip")
	hold.Lock()

	swept := make(chan int, 1)
	go func() {
		removed, err := s.Sweep(context.Background(), 1*time.Hour)
		if err != nil {
			t.Errorf("Sweep error: %v", err)
		}
		swept <- removed
	}()

	// Sweep should still be blocked — give it a brief window to
	// race to completion (which would be a bug).
	select {
	case removed := <-swept:
		hold.Unlock()
		t.Fatalf("Sweep completed (removed=%d) while per-key lock was held — Sweep is not waiting for in-flight Put", removed)
	case <-time.After(50 * time.Millisecond):
	}

	// Release the lock; Sweep should now run to completion and
	// reap the stale .tmp.
	hold.Unlock()
	select {
	case removed := <-swept:
		if removed != 1 {
			t.Errorf("Sweep removed=%d, want 1", removed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sweep did not complete after lock release")
	}
}

func TestStore_Sweep_EmptyRootIsNoop(t *testing.T) {
	s := newTestStore(t)
	removed, err := s.Sweep(context.Background(), time.Hour)
	if err != nil {
		t.Errorf("Sweep: %v", err)
	}
	if removed != 0 {
		t.Errorf("Sweep on empty store removed=%d, want 0", removed)
	}
}

func TestStore_PutInUnopenedRootFailsCleanly(t *testing.T) {
	s := newTestStore(t)
	_ = s.Close()
	defer func() {
		// The os.Root use-after-close panics inside the std lib;
		// catch and ignore so the test doesn't fail. We're confirming
		// that Close releases the root, not that Put recovers.
		recover()
	}()
	_, _ = s.Put(context.Background(), "ns", "n", "r", nil)
}

// TestStore_HTTPHandler_ServesTarballAndRejectsDirectories pins the
// HTTPHandler's allowlist: only `.tar.gz` paths reach the underlying
// http.FileServer. Directory listings (which would enumerate every
// snippet's revision history to any in-cluster caller that can reach
// the storage port) and unrelated paths return 404 with no body.
// A symlink planted under the store root that points outside it must
// not let an HTTP caller read the out-of-root target. Serving through
// os.Root's fs.FS refuses the escaping symlink; http.Dir/os.DirFS would
// follow it. The .tar.gz suffix allowlist alone does not help — the
// symlink carries that suffix.
func TestStore_HTTPHandler_RejectsSymlinkEscape(t *testing.T) {
	s := newTestStore(t)
	root := s.RootPath()

	// A secret file living outside the store root.
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Plant <root>/team-a/demo/escape.tar.gz -> <secretDir>/secret.
	linkDir := filepath.Join(root, "team-a", "demo")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatalf("mkdir link dir: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(linkDir, "escape.tar.gz")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	srv := httptest.NewServer(s.HTTPHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/team-a/demo/escape.tar.gz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("escaping symlink served with 200; body=%q", body)
	}
	if strings.Contains(string(body), "TOP-SECRET") {
		t.Errorf("out-of-root secret leaked through symlink: body=%q", body)
	}
}

func TestStore_HTTPHandler_ServesTarballAndRejectsDirectories(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Put(context.Background(), "team-a", "demo", "abc123",
		[]FileEntry{{Path: "rendered.json", Content: []byte(`{"ok":true}`)}}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	srv := httptest.NewServer(s.HTTPHandler())
	defer srv.Close()

	cases := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{"tarball is served", "/team-a/demo/abc123.tar.gz", http.StatusOK},
		{"root directory listing rejected", "/", http.StatusNotFound},
		{"namespace directory rejected", "/team-a/", http.StatusNotFound},
		{"snippet directory rejected", "/team-a/demo/", http.StatusNotFound},
		{"snippet dir without trailing slash rejected", "/team-a/demo", http.StatusNotFound},
		{"unrelated suffix rejected", "/team-a/demo/abc123.json", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("GET %s: status %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
			}
		})
	}
}
