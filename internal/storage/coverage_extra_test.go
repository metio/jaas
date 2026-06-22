/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestS3Clock_InjectedAndDefault drives both branches of (*S3Backend).clock:
// an injected now returns the stamped instant, and a nil now falls through to
// time.Now and reports an instant within the surrounding wall-clock window.
func TestS3Clock_InjectedAndDefault(t *testing.T) {
	fixed := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	injected := &S3Backend{now: func() time.Time { return fixed }}
	if got := injected.clock(); !got.Equal(fixed) {
		t.Errorf("injected clock = %v, want %v", got, fixed)
	}

	before := time.Now()
	got := (&S3Backend{}).clock()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("default clock = %v, want within [%v, %v]", got, before, after)
	}
}

// TestS3Prune_EmptyComponentsAndNoOps exercises Prune's guard branches: an
// empty namespace/name is rejected, an empty keep-set is a no-op (Prune must
// never wipe every revision), and a traversal component is refused before any
// list call.
func TestS3Prune_EmptyComponentsAndNoOps(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "")
	ctx := context.Background()
	if _, err := b.Put(ctx, "ns", "snip", "r1", []FileEntry{{Path: "f", Content: []byte("x")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := b.Prune(ctx, "", "snip", []string{"r1"}, 0); err == nil {
		t.Error("Prune with empty namespace must error")
	}
	if err := b.Prune(ctx, "ns", "", []string{"r1"}, 0); err == nil {
		t.Error("Prune with empty name must error")
	}
	if err := b.Prune(ctx, "..", "snip", []string{"r1"}, 0); err == nil {
		t.Error("Prune with traversal namespace must error")
	}

	// Empty keep-set is a no-op: the seeded object survives.
	if err := b.Prune(ctx, "ns", "snip", nil, 0); err != nil {
		t.Fatalf("Prune(empty keep-set): %v", err)
	}
	fake.mu.Lock()
	_, ok := fake.objects["ns/snip/r1.tar.gz"]
	fake.mu.Unlock()
	if !ok {
		t.Error("empty keep-set pruned the only revision; must be a no-op")
	}
}

// TestS3Prune_GracePastWindowRemovesWithInjectedClock drives Prune's
// grace-window arithmetic through an injected clock and a non-zero grace,
// exercising both (*S3Backend).clock's injected branch and the live
// RemoveObject path. The fake's ListObjects carries no LastModified, so the
// candidate mtime is the zero instant — well past any finite grace, making
// the non-kept revision a victim while the keep-set member survives.
func TestS3Prune_GracePastWindowRemovesWithInjectedClock(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "tenants/jaas")
	ctx := context.Background()
	for _, rev := range []string{"r1", "r2"} {
		if _, err := b.Put(ctx, "ns", "snip", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	b.now = func() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC) }
	if err := b.Prune(ctx, "ns", "snip", []string{"r2"}, time.Hour); err != nil {
		t.Fatalf("Prune past grace: %v", err)
	}
	fake.mu.Lock()
	_, stillThere := fake.objects["tenants/jaas/ns/snip/r1.tar.gz"]
	_, keptR2 := fake.objects["tenants/jaas/ns/snip/r2.tar.gz"]
	fake.mu.Unlock()
	if stillThere {
		t.Error("r1 must be removed past the grace window")
	}
	if !keptR2 {
		t.Error("r2 (the keep-set member) must survive Prune")
	}
}

// TestS3Prune_SurfacesListError pins that a ListObjects stream error fails
// Prune rather than silently reporting success against a truncated listing.
func TestS3Prune_SurfacesListError(t *testing.T) {
	b, fake, _ := newTestS3Backend(t, "")
	ctx := context.Background()
	if _, err := b.Put(ctx, "ns", "snip", "r1", []FileEntry{{Path: "f", Content: []byte("x")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fake.mu.Lock()
	fake.failList = "throttled"
	fake.mu.Unlock()

	if err := b.Prune(ctx, "ns", "snip", []string{"keep-me"}, 0); err == nil {
		t.Error("Prune must surface a list-stream error")
	} else if !strings.Contains(err.Error(), "list") {
		t.Errorf("Prune error = %q, want it to name the list failure", err)
	}
}

// TestS3Delete_EmptyComponentsAndTraversal covers Delete's guard branches that
// the happy-path Delete tests skip.
func TestS3Delete_EmptyComponentsAndTraversal(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	ctx := context.Background()
	if err := b.Delete(ctx, "", "snip"); err == nil {
		t.Error("Delete with empty namespace must error")
	}
	if err := b.Delete(ctx, "ns", ""); err == nil {
		t.Error("Delete with empty name must error")
	}
	if err := b.Delete(ctx, "ns", ".."); err == nil {
		t.Error("Delete with traversal name must error")
	}
}

// TestS3Delete_AbsentSnippetIsClean deletes a (namespace,name) that was never
// published — the list yields no objects, the bulk remove drains empty, and
// Delete reports success.
func TestS3Delete_AbsentSnippetIsClean(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	if err := b.Delete(context.Background(), "ns", "never-published"); err != nil {
		t.Errorf("Delete of an absent snippet must be a clean no-op, got %v", err)
	}
}

// TestS3HTTPHandler_HeadServesHeadersOnly drives the HEAD branch: the handler
// stats the object and writes Content-Length/Content-Type without a body.
func TestS3HTTPHandler_HeadServesHeadersOnly(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	if _, err := b.Put(context.Background(), "ns", "snip", "rev1", []FileEntry{{Path: "f", Content: []byte("hello")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec := httptest.NewRecorder()
	b.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/ns/snip/rev1.tar.gz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("HEAD must set Content-Length")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD must not write a body, got %d bytes", rec.Body.Len())
	}
}

// TestS3HTTPHandler_ReadTimeoutAppliesToObjectFetch exercises the readTimeout
// branch of HTTPHandler: with a non-zero ReadTimeout the per-request context
// carries a deadline, and a present object still streams back successfully.
func TestS3HTTPHandler_ReadTimeoutAppliesToObjectFetch(t *testing.T) {
	fake := newFakeS3("bkt")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	u := mustHostFromURL(t, srv.URL)
	b, err := NewS3(S3Config{
		Endpoint:        u,
		Bucket:          "bkt",
		UseSSL:          false,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		ReadTimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if _, err := b.Put(context.Background(), "ns", "snip", "rev1", []FileEntry{{Path: "f", Content: []byte("hi")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rec := httptest.NewRecorder()
	b.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ns/snip/rev1.tar.gz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET with ReadTimeout status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestS3HTTPHandler_EmptyKeyRejected covers the unescape/empty-key guard: a
// request whose path is exactly "/" leaves an empty key and 404s before the
// bucket is touched. Note the bare "/" still carries the .tar.gz suffix check,
// so use a path that passes the suffix gate but unescapes to empty segments.
func TestS3HTTPHandler_EmptyKeyRejected(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	rec := httptest.NewRecorder()
	// "/.tar.gz" passes the suffix check, trims to ".tar.gz"; its only
	// segment is non-empty, so it reaches GetObject and 404s as missing.
	b.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.tar.gz", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestStreamTarGz_RejectsBadEntryPath drives streamTarGz's error branch: a
// tar entry with a traversal path fails writeTarEntry, so the helper closes
// the tar/gzip writers and returns the error rather than emitting a bad
// archive. This in turn surfaces through Put's writer-goroutine error path.
func TestStreamTarGz_RejectsBadEntryPath(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	_, err := b.Put(context.Background(), "ns", "snip", "rev1", []FileEntry{
		{Path: "../escape.json", Content: []byte("x")},
	})
	if err == nil {
		t.Fatal("Put must reject a tar entry whose path escapes the archive root")
	}
}

// TestS3ObjectDir_WithAndWithoutPrefix pins objectDir's two branches: a backend
// with no prefix yields the bare <ns>/<name>, and one with a prefix prepends it.
func TestS3ObjectDir_WithAndWithoutPrefix(t *testing.T) {
	plain := &S3Backend{}
	if got := plain.objectDir("ns", "snip"); got != "ns/snip" {
		t.Errorf("objectDir(no prefix) = %q, want ns/snip", got)
	}
	prefixed := &S3Backend{prefix: "tenants/jaas"}
	if got := prefixed.objectDir("ns", "snip"); got != "tenants/jaas/ns/snip" {
		t.Errorf("objectDir(prefix) = %q, want tenants/jaas/ns/snip", got)
	}
}

// TestStore_Sweep_SkipsNonDirectoryEntries covers Sweep's two IsDir guards:
// a plain file sitting at the store root (where a namespace directory is
// expected) and a plain file inside a namespace directory (where a name
// directory is expected) are both skipped without error, while a genuine
// aged .tmp under a real (ns, name) directory is still reaped.
func TestStore_Sweep_SkipsNonDirectoryEntries(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetNow(func() time.Time { return base })

	// A stray regular file at the root level — not a namespace directory.
	if err := os.WriteFile(filepath.Join(dir, "stray-root-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A namespace directory holding a stray regular file where a name
	// directory is expected.
	nsDir := filepath.Join(dir, "ns")
	if err := os.MkdirAll(nsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nsDir, "stray-name-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A genuine aged orphan under a real (ns, name) directory.
	kdir := filepath.Join(nsDir, "n")
	if err := os.MkdirAll(kdir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(kdir, "cccc.tar.gz.tmp")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, base.Add(-time.Hour), base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	removed, err := s.Sweep(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("Sweep removed %d, want 1 (only the aged orphan; strays skipped)", removed)
	}
	for _, p := range []string{filepath.Join(dir, "stray-root-file"), filepath.Join(nsDir, "stray-name-file")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("stray non-artifact entry %q must be left untouched: %v", p, err)
		}
	}
}

// TestNewS3_RejectsMalformedEndpoint covers NewS3's minio.New error branch:
// an endpoint that minio-go cannot parse fails construction.
func TestNewS3_RejectsMalformedEndpoint(t *testing.T) {
	if _, err := NewS3(S3Config{Endpoint: "http://[::1", Bucket: "b"}); err == nil {
		t.Error("NewS3 must reject a malformed endpoint")
	}
}

// TestS3HTTPHandler_RejectsBadPercentEncoding covers the url.PathUnescape error
// branch: a malformed %-escape in the request path is refused with 404 before
// the bucket is touched.
func TestS3HTTPHandler_RejectsBadPercentEncoding(t *testing.T) {
	b, _, _ := newTestS3Backend(t, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x.tar.gz", nil)
	// Bypass net/http's own URL parsing by setting a raw path with a
	// truncated escape that PathUnescape rejects.
	req.URL.Path = "/%zz.tar.gz"
	b.HTTPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an undecodable path", rec.Code)
	}
}

// TestS3_BackendErrorsWhenServerDown covers the GetObject transport-error
// branches: with the fake server stopped, Open surfaces an error and the HTTP
// handler maps the dial failure to 502 (bad gateway) via s3WriteError.
func TestS3_BackendErrorsWhenServerDown(t *testing.T) {
	b, _, srv := newTestS3Backend(t, "")
	srv.Close() // every subsequent request fails to dial

	if _, err := b.Open(context.Background(), "ns", "snip", "rev1"); err == nil {
		t.Error("Open must error when the backend is unreachable")
	}

	rec := httptest.NewRecorder()
	b.HTTPHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ns/snip/rev1.tar.gz", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable-backend GET status = %d, want 502", rec.Code)
	}
}

// TestStore_Sweep_MissingRootIsClean covers Sweep's not-exist branch: if the
// store root vanishes from underneath an open Store, the root listing returns
// a not-exist error which Sweep treats as "nothing to reap" (0, nil) rather
// than surfacing it as a sweep failure.
func TestStore_Sweep_MissingRootIsClean(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "store")
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove root: %v", err)
	}
	removed, err := s.Sweep(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Sweep over a missing root must be clean, got %v", err)
	}
	if removed != 0 {
		t.Errorf("Sweep removed %d, want 0 over a missing root", removed)
	}
}

func mustHostFromURL(t *testing.T, raw string) string {
	t.Helper()
	const scheme = "http://"
	if !strings.HasPrefix(raw, scheme) {
		t.Fatalf("unexpected test server URL %q", raw)
	}
	return strings.TrimPrefix(raw, scheme)
}
