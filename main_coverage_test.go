/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/metio/jaas/internal/handler"
)

// drainBeforeShutdown's second-signal branch: a delay is set, but a signal
// arrives on the channel before the timer fires, so the wait is cut short and
// the cut-short message is logged.
func TestDrainBeforeShutdown_SecondSignalCutsWaitShort(t *testing.T) {
	state := handler.NewHealthState()

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sigs := make(chan os.Signal, 1)
	sigs <- os.Interrupt

	start := time.Now()
	drainBeforeShutdown(sigs, state, time.Hour, logger)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("drain took %v; a queued signal must cut the long delay short", elapsed)
	}
	if state.Ready() {
		t.Error("readiness must be latched off even when the wait is cut short")
	}
	out := buf.String()
	if !strings.Contains(out, "cutting drain short") {
		t.Errorf("expected the cut-short message; got %q", out)
	}
}

// defaultMaxConcurrentEvals clamps to a floor of 16 when GOMAXPROCS*4 is below
// it. Driving GOMAXPROCS to 1 forces 1*4=4, which the clamp must lift to 16.
func TestDefaultMaxConcurrentEvals_ClampsToFloor(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	if got := defaultMaxConcurrentEvals(); got != 16 {
		t.Errorf("with GOMAXPROCS=1 got %d; want the clamped floor 16", got)
	}
}

// defaultMaxConcurrentEvals scales above the floor when GOMAXPROCS*4 exceeds
// 16. GOMAXPROCS=8 yields 32, above the floor, so the clamp must not engage.
func TestDefaultMaxConcurrentEvals_ScalesAboveFloor(t *testing.T) {
	prev := runtime.GOMAXPROCS(8)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	if got := defaultMaxConcurrentEvals(); got != 32 {
		t.Errorf("with GOMAXPROCS=8 got %d; want 8*4=32 (above the floor)", got)
	}
}

// readOCILibraryFiles returns an error when libDir cannot be walked at all —
// here because the directory does not exist, so WalkDir's first callback
// carries a non-nil err that the walk func propagates.
func TestReadOCILibraryFiles_NonexistentDirReturnsError(t *testing.T) {
	if _, err := readOCILibraryFiles(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected an error walking a nonexistent directory")
	}
}

// readOCILibraryFiles reads importable files, normalises nested paths to "/",
// and silently skips both non-importable extensions and an unreadable
// individual file (the read error returns nil from the walk func, so the
// file is simply absent from the result without failing the whole library).
func TestReadOCILibraryFiles_SkipsUnreadableFileAndKeepsRest(t *testing.T) {
	libDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(libDir, "main.libsonnet"), []byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(libDir, "panels")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "graph.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-importable extension: filtered before any read attempt.
	if err := os.WriteFile(filepath.Join(libDir, "NOTES.txt"), []byte(`notes`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Importable extension but unreadable (mode 0) — the per-file read error is
	// swallowed so the file is dropped while the library still loads.
	unreadable := filepath.Join(libDir, "secret.libsonnet")
	if err := os.WriteFile(unreadable, []byte(`{ secret: true }`), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	files, err := readOCILibraryFiles(libDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files["main.libsonnet"] != `{ ok: true }` {
		t.Errorf("main.libsonnet = %q", files["main.libsonnet"])
	}
	if files["panels/graph.json"] != `{}` {
		t.Errorf("nested file = %q; want it keyed by a slash-normalised relative path", files["panels/graph.json"])
	}
	if _, ok := files["NOTES.txt"]; ok {
		t.Error("non-importable .txt must be filtered out")
	}
	if _, ok := files["secret.libsonnet"]; ok {
		t.Error("unreadable importable file must be silently skipped, not surfaced")
	}
}

// ociLibrariesFromPaths skips an unreadable --library-path entry (a directory
// with no read permission yields a non-IsNotExist error → warn-and-continue),
// loads the readable one, and warns-and-skips a sub-library whose files cannot
// be read at all (the inner dir is unreadable so readOCILibraryFiles errors).
func TestOCILibrariesFromPaths_SkipsUnreadablePathsAndLibraries(t *testing.T) {
	unreadableRoot := filepath.Join(t.TempDir(), "noperm")
	if err := os.MkdirAll(unreadableRoot, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadableRoot, 0o755) })

	good := t.TempDir()
	goodLib := filepath.Join(good, "examplonet")
	if err := os.MkdirAll(goodLib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodLib, "main.libsonnet"), []byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sub-library whose own directory is unreadable: readOCILibraryFiles
	// cannot walk it, so it is warn-and-skipped without aborting the scan.
	brokenLib := filepath.Join(good, "broken")
	if err := os.MkdirAll(brokenLib, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(brokenLib, 0o755) })

	got := ociLibrariesFromPaths([]string{unreadableRoot, good})
	if _, ok := got["examplonet"]; !ok {
		t.Fatalf("examplonet must load past the unreadable siblings; got %v", got)
	}
	if _, ok := got["broken"]; ok {
		t.Error("a library with an unreadable directory must be skipped, not surfaced")
	}
}

// loadKubeconfig returns a *rest.Config built from an explicit kubeconfig path.
func TestLoadKubeconfig_ExplicitPath(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	const body = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: abc
`
	if err := os.WriteFile(kubeconfig, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadKubeconfig(kubeconfig)
	if err != nil {
		t.Fatalf("loadKubeconfig(%q) failed: %v", kubeconfig, err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want it to reflect the kubeconfig server", cfg.Host)
	}
}

// loadKubeconfig surfaces an error when the explicit path cannot be parsed as a
// kubeconfig (here a non-existent file).
func TestLoadKubeconfig_BadPathErrors(t *testing.T) {
	if _, err := loadKubeconfig(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error for a missing kubeconfig path")
	}
}

// parseWatchNamespaces falls back to JAAS_WATCH_NAMESPACES in env when the flag
// is empty, splitting and trimming entries while dropping empties.
func TestParseWatchNamespaces_EnvFallbackAndTrimming(t *testing.T) {
	got := parseWatchNamespaces("", []string{
		"PATH=/usr/bin",
		"JAAS_WATCH_NAMESPACES= a , ,b , ",
		"OTHER=ignored",
	})
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// awaitGoroutines reports false (and logs) when a goroutine's done channel
// never closes within the timeout, and true when every channel is already
// closed or nil.
func TestAwaitGoroutines_MixedAndTimeout(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	if !awaitGoroutines(context.Background(), 50*time.Millisecond, map[string]<-chan struct{}{
		"closed": closed,
		"nil":    nil,
	}) {
		t.Error("want true when all channels are closed or nil")
	}

	never := make(chan struct{})
	if awaitGoroutines(context.Background(), 10*time.Millisecond, map[string]<-chan struct{}{
		"stuck": never,
	}) {
		t.Error("want false when a goroutine never stops before the deadline")
	}
}
