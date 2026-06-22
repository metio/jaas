// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package storage

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStore_Open_RoundTripAndErrors covers the in-process reader the MCP
// diff_revisions tool uses: a stored revision opens and gunzips, a missing one
// is ErrRevisionNotFound, and empty/traversal components are rejected.
func TestStore_Open_RoundTripAndErrors(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if _, err := s.Put(ctx, "ns", "n", "deadbeef", []FileEntry{{Path: "main.json", Content: []byte(`{"ok":true}`)}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Open(ctx, "ns", "n", "deadbeef")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("the opened object must be gzip: %v", err)
	}
	if _, err := io.Copy(io.Discard, gz); err != nil {
		t.Fatalf("read gz: %v", err)
	}

	if _, err := s.Open(ctx, "ns", "n", "missing"); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("Open(missing) = %v, want ErrRevisionNotFound", err)
	}
	if _, err := s.Open(ctx, "", "n", "r"); err == nil {
		t.Error("Open with empty namespace must error")
	}
	if _, err := s.Open(ctx, "ns", "..", "r"); err == nil {
		t.Error("Open with a traversal component must error")
	}
}

func TestLooksLikeOurArtifactFilename(t *testing.T) {
	cases := map[string]bool{
		"deadbeef.tar.gz":     true,
		"ABC123.tar.gz.tmp":   true,
		"main.tar.gz":         false, // 'm','n' aren't hex
		".tar.gz":             false, // empty revision
		"deadbeef":            false, // no recognized suffix
		"deadbeef.tar":        false,
		"deadbeef.tar.gz.bak": false,
	}
	for name, want := range cases {
		if got := looksLikeOurArtifactFilename(name); got != want {
			t.Errorf("looksLikeOurArtifactFilename(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestStore_Sweep_RemovesAgedTmp drives the orphan-tmp sweep on an injected
// clock: a .tmp older than maxTmpAge is reaped, a fresh one is kept.
func TestStore_Sweep_RemovesAgedTmp(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetNow(func() time.Time { return base })

	kdir := filepath.Join(dir, "ns", "n")
	if err := os.MkdirAll(kdir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(kdir, "aaaa.tar.gz.tmp")
	fresh := filepath.Join(kdir, "bbbb.tar.gz.tmp")
	for _, f := range []string{old, fresh} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Age the orphan an hour before "now"; leave the fresh one at now.
	if err := os.Chtimes(old, base.Add(-time.Hour), base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fresh, base, base); err != nil {
		t.Fatal(err)
	}

	removed, err := s.Sweep(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("Sweep removed %d, want 1 (only the aged .tmp)", removed)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Error("the aged .tmp should have been removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("the fresh .tmp must be kept")
	}
}
