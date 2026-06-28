/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

// TestExtractTarGz_RejectsTraversingEntries pins the defense-in-depth guard:
// a tar entry whose name traverses (`..`), is absolute, or carries NUL/backslash
// is dropped rather than keyed into the diff map under an escaping path.
func TestExtractTarGz_RejectsTraversingEntries(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := map[string]string{
		"good.json":       `{"ok":true}`,
		"../escape.json":  `{"evil":true}`,
		"/abs.json":       `{"evil":true}`,
		"a/../../up.json": `{"evil":true}`,
	}
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := extractTarGz(&buf)
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, ok := files["good.json"]; !ok {
		t.Error("clean entry good.json was dropped")
	}
	for _, bad := range []string{"../escape.json", "/abs.json", "a/../../up.json", "../up.json", "escape.json"} {
		if _, ok := files[bad]; ok {
			t.Errorf("traversing/absolute entry %q was kept: %v", bad, files)
		}
	}
	if len(files) != 1 {
		t.Errorf("expected only the clean entry to survive, got %d: %v", len(files), files)
	}
}

// snippetWithHistory builds a snippet whose status.history lists the given
// revisions most-recent-first, matching the operator's convention.
func snippetWithHistory(namespace, name string, revisions ...string) *jaasv1.JsonnetSnippet {
	s := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
	for _, r := range revisions {
		s.Status.History = append(s.Status.History, jaasv1.RevisionEntry{Revision: r, Time: metav1.Time{}})
	}
	if len(revisions) > 0 {
		s.Status.Revision = revisions[0]
	}
	return s
}

// putRevision writes one revision's files into the store under the SHORT
// revision key, exactly as the Publisher does (it strips the "sha256:" prefix
// before Store.Put). Callers pass the full "sha256:<hex>" form that
// status.history records, so the round-trip exercises the prefix-strip the diff
// tool must apply on read — storing under the full form would mask that
// production key mismatch.
func putRevision(t *testing.T, store storage.Backend, namespace, name, revision string, files map[string]string) {
	t.Helper()
	var entries []storage.FileEntry
	for p, c := range files {
		entries = append(entries, storage.FileEntry{Path: p, Content: []byte(c)})
	}
	shortRev := strings.TrimPrefix(revision, "sha256:")
	if _, err := store.Put(context.Background(), namespace, name, shortRev, entries); err != nil {
		t.Fatalf("put %s: %v", revision, err)
	}
}

func newStore(t *testing.T) storage.Backend {
	t.Helper()
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func FuzzExtractTarGz(f *testing.F) {
	// Seed with a valid single-member tar.gz so the fuzzer mutates outward from
	// a parseable archive, plus a non-gzip blob.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "main.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("{}"))
	_ = tw.Close()
	_ = gz.Close()
	f.Add(buf.Bytes())
	f.Add([]byte("not a gzip stream"))

	f.Fuzz(func(t *testing.T, data []byte) {
		files, err := extractTarGz(bytes.NewReader(data))
		if err != nil {
			return // rejecting malformed input is the expected outcome
		}
		// Anything accepted must carry a safe, non-escaping key.
		for name := range files {
			if name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") ||
				strings.ContainsRune(name, 0) || strings.ContainsRune(name, '\\') {
				t.Errorf("extractTarGz accepted an unsafe entry name %q", name)
			}
		}
	})
}

func TestResolveRevisions(t *testing.T) {
	r1, r2 := "sha256:newer", "sha256:older"
	tests := []struct {
		name             string
		history          []string // most-recent first
		from, to         string
		wantFrom, wantTo string
		wantErr          bool
	}{
		{name: "both explicit needs no history", from: "x", to: "y", wantFrom: "x", wantTo: "y"},
		{name: "both default from two-entry history", history: []string{r1, r2}, wantFrom: r2, wantTo: r1},
		// The bug: an explicit from + one retained revision must default `to`,
		// not error telling the caller to "pass explicit from/to".
		{name: "explicit from, default to, one entry", history: []string{r1}, from: "x", wantFrom: "x", wantTo: r1},
		{name: "explicit to, default from, two entries", history: []string{r1, r2}, to: "y", wantFrom: r2, wantTo: "y"},
		{name: "default to with no history errors", history: nil, from: "x", wantErr: true},
		{name: "default from with one entry errors", history: []string{r1}, to: "y", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snip := snippetWithHistory("ns", "name", tt.history...)
			gotFrom, gotTo, err := resolveRevisions(snip, tt.from, tt.to)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got from=%q to=%q", gotFrom, gotTo)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotFrom != tt.wantFrom || gotTo != tt.wantTo {
				t.Errorf("got (from=%q,to=%q), want (from=%q,to=%q)", gotFrom, gotTo, tt.wantFrom, tt.wantTo)
			}
		})
	}
}

func TestDiffRevisionsHandler(t *testing.T) {
	const ns, name = "team-a", "dash"
	r1, r2 := "sha256:1111111111111111", "sha256:2222222222222222"

	t.Run("modified file defaults to the two most recent revisions", func(t *testing.T) {
		store := newStore(t)
		putRevision(t, store, ns, name, r1, map[string]string{"main.json": "{\n  \"replicas\": 1\n}\n"})
		putRevision(t, store, ns, name, r2, map[string]string{"main.json": "{\n  \"replicas\": 2\n}\n"})
		cfg := Config{KubeClient: fakeClient(t, snippetWithHistory(ns, name, r2, r1)), Store: store}

		res, out, err := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name})
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res != nil {
			t.Fatalf("unexpected tool error: %+v", res)
		}
		if out.From != r1 || out.To != r2 {
			t.Fatalf("default revisions wrong: from=%q to=%q", out.From, out.To)
		}
		if len(out.Files) != 1 || out.Files[0].Path != "main.json" || out.Files[0].Status != "modified" {
			t.Fatalf("want one modified main.json, got %+v", out.Files)
		}
		if !strings.Contains(out.Files[0].Diff, "-  \"replicas\": 1") || !strings.Contains(out.Files[0].Diff, "+  \"replicas\": 2") {
			t.Fatalf("diff missing the changed lines:\n%s", out.Files[0].Diff)
		}
	})

	t.Run("added, removed, and unchanged files with explicit revisions", func(t *testing.T) {
		store := newStore(t)
		putRevision(t, store, ns, name, r1, map[string]string{"gone.json": "x", "same.json": "keep"})
		putRevision(t, store, ns, name, r2, map[string]string{"new.json": "y", "same.json": "keep"})
		cfg := Config{KubeClient: fakeClient(t, snippetWithHistory(ns, name, r2, r1)), Store: store}

		_, out, err := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name, From: r1, To: r2})
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if out.Unchanged != 1 {
			t.Fatalf("want 1 unchanged (same.json), got %d", out.Unchanged)
		}
		byPath := map[string]string{}
		for _, f := range out.Files {
			byPath[f.Path] = f.Status
		}
		if byPath["new.json"] != "added" || byPath["gone.json"] != "removed" {
			t.Fatalf("want new.json added + gone.json removed, got %+v", byPath)
		}
		if _, listed := byPath["same.json"]; listed {
			t.Fatalf("unchanged same.json must not appear in files: %+v", out.Files)
		}
	})

	t.Run("a pruned revision is a tool error", func(t *testing.T) {
		store := newStore(t)
		putRevision(t, store, ns, name, r2, map[string]string{"main.json": "y"}) // r1 never stored
		cfg := Config{KubeClient: fakeClient(t, snippetWithHistory(ns, name, r2, r1)), Store: store}

		res, _, err := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name})
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error for the pruned from-revision, got %+v", res)
		}
	})

	t.Run("fewer than two retained revisions is a tool error", func(t *testing.T) {
		store := newStore(t)
		putRevision(t, store, ns, name, r2, map[string]string{"main.json": "y"})
		cfg := Config{KubeClient: fakeClient(t, snippetWithHistory(ns, name, r2)), Store: store}

		res, _, _ := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name})
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error when only one revision is retained, got %+v", res)
		}
	})

	t.Run("identical revisions report only unchanged", func(t *testing.T) {
		store := newStore(t)
		putRevision(t, store, ns, name, r1, map[string]string{"main.json": "same"})
		putRevision(t, store, ns, name, r2, map[string]string{"main.json": "same"})
		cfg := Config{KubeClient: fakeClient(t, snippetWithHistory(ns, name, r2, r1)), Store: store}

		_, out, _ := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name})
		if len(out.Files) != 0 || out.Unchanged != 1 {
			t.Fatalf("want no file diffs and 1 unchanged, got files=%+v unchanged=%d", out.Files, out.Unchanged)
		}
	})

	t.Run("missing namespace or name is a tool error", func(t *testing.T) {
		cfg := Config{KubeClient: fakeClient(t), Store: newStore(t)}
		res, _, _ := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Name: name})
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error when namespace is empty, got %+v", res)
		}
	})

	t.Run("same from and to revision is a tool error", func(t *testing.T) {
		store := newStore(t)
		putRevision(t, store, ns, name, r2, map[string]string{"main.json": "y"})
		cfg := Config{KubeClient: fakeClient(t, snippetWithHistory(ns, name, r2, r1)), Store: store}

		res, _, _ := cfg.diffRevisionsHandler(context.Background(), nil, diffRevisionsInput{Namespace: ns, Name: name, From: r2, To: r2})
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error when from == to, got %+v", res)
		}
		if !strings.Contains(res.Content[0].(*mcpsdk.TextContent).Text, "same revision") {
			t.Fatalf("error should mention 'same revision', got %+v", res.Content)
		}
	})

	t.Run("same revision in mixed sha256-prefixed forms is a tool error", func(t *testing.T) {
		store := newStore(t)
		putRevision(t, store, ns, name, r2, map[string]string{"main.json": "y"})
		cfg := Config{KubeClient: fakeClient(t, snippetWithHistory(ns, name, r2, r1)), Store: store}

		// From carries the full "sha256:" form, To the stripped form — both
		// resolve to the same stored revision, so this must be rejected too.
		res, _, _ := cfg.diffRevisionsHandler(context.Background(), nil,
			diffRevisionsInput{Namespace: ns, Name: name, From: r2, To: strings.TrimPrefix(r2, "sha256:")})
		if res == nil || !res.IsError {
			t.Fatalf("expected a tool error for the same revision in two forms, got %+v", res)
		}
		if !strings.Contains(res.Content[0].(*mcpsdk.TextContent).Text, "same revision") {
			t.Fatalf("error should mention 'same revision', got %+v", res.Content)
		}
	})
}

func TestDiffTool_RegisteredOnlyWithStore(t *testing.T) {
	// Without a Store the diff tool must not be advertised, even with a client.
	if registeredTools(t, Config{KubeClient: fakeClient(t)})["diff_revisions"] {
		t.Fatal("diff_revisions must not be registered without a Store")
	}
	if !registeredTools(t, Config{KubeClient: fakeClient(t), Store: newStore(t)})["diff_revisions"] {
		t.Fatal("diff_revisions must be registered when a Store is configured")
	}
}

// registeredTools connects an in-memory client to a server built from cfg and
// returns the set of advertised tool names.
func registeredTools(t *testing.T, cfg Config) map[string]bool {
	t.Helper()
	ctx := context.Background()
	server := NewServer(cfg)
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	c := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := c.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	present := map[string]bool{}
	for _, tool := range lt.Tools {
		present[tool.Name] = true
	}
	return present
}

// TestShortRevAndRevisionReadError covers both branches of the display helpers.
func TestShortRevAndRevisionReadError(t *testing.T) {
	if got := shortRev("sha256:0123456789abcdef0000"); got != "0123456789ab" {
		t.Errorf("shortRev(long) = %q", got)
	}
	if got := shortRev("sha256:abcd"); got != "abcd" { // <=12 after trim
		t.Errorf("shortRev(short) = %q", got)
	}
	notFound := revisionReadError("from", "sha256:deadbeefcafe00", storage.ErrRevisionNotFound)
	if !strings.Contains(notFound, "not in the artifact store") || !strings.Contains(notFound, "deadbeefcafe") {
		t.Errorf("revisionReadError(not found) = %q", notFound)
	}
	other := revisionReadError("to", "sha256:deadbeefcafe00", errors.New("boom"))
	if !strings.Contains(other, "cannot read to revision") || !strings.Contains(other, "boom") {
		t.Errorf("revisionReadError(other) = %q", other)
	}
}
