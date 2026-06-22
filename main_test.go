/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/metio/jaas/internal/handler"
	"github.com/metio/jaas/internal/storage"
	"k8s.io/client-go/rest"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestResolveCommit(t *testing.T) {
	bi := func(settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Settings: settings}
	}

	tests := []struct {
		name   string
		linker string
		info   *debug.BuildInfo
		ok     bool
		want   string
	}{
		{
			name:   "linker value wins over buildinfo",
			linker: "abc123",
			info: bi(
				debug.BuildSetting{Key: "vcs.revision", Value: "from-buildinfo"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "abc123",
		},
		{
			name:   "linker empty string is respected as override",
			linker: "",
			info:   nil,
			ok:     false,
			want:   "",
		},
		{
			name:   "sentinel with trailing space is NOT sentinel",
			linker: commitSentinel + " ",
			info:   bi(debug.BuildSetting{Key: "vcs.revision", Value: "x"}),
			ok:     true,
			want:   commitSentinel + " ",
		},
		{
			name:   "sentinel + no buildinfo → sentinel",
			linker: commitSentinel,
			info:   nil,
			ok:     false,
			want:   commitSentinel,
		},
		{
			name:   "sentinel + non-nil info but ok=false → sentinel",
			linker: commitSentinel,
			info:   bi(),
			ok:     false,
			want:   commitSentinel,
		},
		{
			name:   "sentinel + empty buildinfo → sentinel",
			linker: commitSentinel,
			info:   bi(),
			ok:     true,
			want:   commitSentinel,
		},
		{
			name:   "sentinel + buildinfo without vcs.revision → sentinel",
			linker: commitSentinel,
			info: bi(
				debug.BuildSetting{Key: "GOOS", Value: "linux"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: commitSentinel,
		},
		{
			name:   "sentinel + empty vcs.revision → sentinel",
			linker: commitSentinel,
			info:   bi(debug.BuildSetting{Key: "vcs.revision", Value: ""}),
			ok:     true,
			want:   commitSentinel,
		},
		{
			name:   "sentinel + revision only (no modified field) → revision",
			linker: commitSentinel,
			info:   bi(debug.BuildSetting{Key: "vcs.revision", Value: "halfinfo"}),
			ok:     true,
			want:   "halfinfo",
		},
		{
			name:   "sentinel + revision + modified=false → revision",
			linker: commitSentinel,
			info: bi(
				debug.BuildSetting{Key: "vcs.revision", Value: "deadbeef"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "deadbeef",
		},
		{
			name:   "sentinel + revision + modified=true → revision-dirty",
			linker: commitSentinel,
			info: bi(
				debug.BuildSetting{Key: "vcs.revision", Value: "deadbeef"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			ok:   true,
			want: "deadbeef-dirty",
		},
		{
			name:   "sentinel + revision + modified=garbage → not dirty",
			linker: commitSentinel,
			info: bi(
				debug.BuildSetting{Key: "vcs.revision", Value: "deadbeef"},
				debug.BuildSetting{Key: "vcs.modified", Value: "maybe"},
			),
			ok:   true,
			want: "deadbeef",
		},
		{
			name:   "sentinel + revision + modified=empty → not dirty",
			linker: commitSentinel,
			info: bi(
				debug.BuildSetting{Key: "vcs.revision", Value: "deadbeef"},
				debug.BuildSetting{Key: "vcs.modified", Value: ""},
			),
			ok:   true,
			want: "deadbeef",
		},
		{
			name:   "sentinel + full 40-char SHA preserved",
			linker: commitSentinel,
			info: bi(debug.BuildSetting{
				Key:   "vcs.revision",
				Value: "1234567890abcdef1234567890abcdef12345678",
			}),
			ok:   true,
			want: "1234567890abcdef1234567890abcdef12345678",
		},
		{
			name:   "sentinel + irrelevant settings around revision",
			linker: commitSentinel,
			info: bi(
				debug.BuildSetting{Key: "GOOS", Value: "linux"},
				debug.BuildSetting{Key: "GOARCH", Value: "amd64"},
				debug.BuildSetting{Key: "vcs", Value: "git"},
				debug.BuildSetting{Key: "vcs.revision", Value: "abc123"},
				debug.BuildSetting{Key: "vcs.time", Value: "2026-01-01T00:00:00Z"},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"},
			),
			ok:   true,
			want: "abc123",
		},
		{
			name:   "duplicate vcs.revision settings: last wins",
			linker: commitSentinel,
			info: bi(
				debug.BuildSetting{Key: "vcs.revision", Value: "first"},
				debug.BuildSetting{Key: "vcs.revision", Value: "second"},
			),
			ok:   true,
			want: "second",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCommit(tc.linker, tc.info, tc.ok)
			if got != tc.want {
				t.Errorf("resolveCommit(%q, ...) = %q, want %q", tc.linker, got, tc.want)
			}
		})
	}
}

func TestDrainBeforeShutdown_FlipsReadyToFalse(t *testing.T) {
	state := handler.NewHealthState()
	state.SetReady(true)
	drainBeforeShutdown(make(chan os.Signal), state, 0, discardLogger())
	if state.Ready() {
		t.Error("Ready() = true after drain; want false")
	}
}

func TestDrainBeforeShutdown_FlipsReadyEvenWhenAlreadyFalse(t *testing.T) {
	state := handler.NewHealthState()
	// Never marked ready in the first place.
	drainBeforeShutdown(make(chan os.Signal), state, 0, discardLogger())
	if state.Ready() {
		t.Error("Ready() = true after drain on never-ready state")
	}
}

func TestDrainBeforeShutdown_DoesNotTouchStarted(t *testing.T) {
	state := handler.NewHealthState()
	state.MarkStarted()
	state.SetReady(true)
	drainBeforeShutdown(make(chan os.Signal), state, 0, discardLogger())
	if !state.Started() {
		t.Error("Started() flipped to false; drain must not touch the started flag")
	}
}

func TestDrainBeforeShutdown_ZeroDelayReturnsImmediately(t *testing.T) {
	state := handler.NewHealthState()
	start := time.Now()
	drainBeforeShutdown(make(chan os.Signal), state, 0, discardLogger())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("took %v with delay=0; want < 50ms", elapsed)
	}
}

func TestDrainBeforeShutdown_NegativeDelayReturnsImmediately(t *testing.T) {
	state := handler.NewHealthState()
	start := time.Now()
	drainBeforeShutdown(make(chan os.Signal), state, -1*time.Second, discardLogger())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("took %v with negative delay; want < 50ms", elapsed)
	}
}

func TestDrainBeforeShutdown_PositiveDelaySleepsAtLeastThatLong(t *testing.T) {
	state := handler.NewHealthState()
	delay := 60 * time.Millisecond
	start := time.Now()
	drainBeforeShutdown(make(chan os.Signal), state, delay, discardLogger())
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("took %v; want at least %v", elapsed, delay)
	}
}

func TestDrainBeforeShutdown_FlipsReadyBeforeSleeping(t *testing.T) {
	// Concurrent reader observes Ready()==false while drainBeforeShutdown is
	// still sleeping — proves the flip happens before the sleep, not after.
	state := handler.NewHealthState()
	state.SetReady(true)

	delay := 200 * time.Millisecond
	done := make(chan struct{})
	var observedReady bool
	var mu sync.Mutex

	go func() {
		// Wait until well into the sleep, then sample Ready().
		time.Sleep(delay / 4)
		mu.Lock()
		observedReady = state.Ready()
		mu.Unlock()
		close(done)
	}()

	drainBeforeShutdown(make(chan os.Signal), state, delay, discardLogger())
	<-done

	mu.Lock()
	defer mu.Unlock()
	if observedReady {
		t.Error("observed Ready()==true mid-sleep; flip must happen before the sleep")
	}
}

func TestDrainBeforeShutdown_LogsAtInfoLevelWhenDelaying(t *testing.T) {
	state := handler.NewHealthState()

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	drainBeforeShutdown(make(chan os.Signal), state, 30*time.Millisecond, logger)

	output := buf.String()
	if !strings.Contains(output, "level=INFO") {
		t.Errorf("expected an INFO record; got %q", output)
	}
	if !strings.Contains(output, "Draining") {
		t.Errorf("expected message about draining; got %q", output)
	}
	if !strings.Contains(output, "delay=30ms") {
		t.Errorf("expected delay attribute; got %q", output)
	}
}

func TestDrainBeforeShutdown_DoesNotLogWhenDelayIsZero(t *testing.T) {
	state := handler.NewHealthState()

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	drainBeforeShutdown(make(chan os.Signal), state, 0, logger)

	if buf.Len() != 0 {
		t.Errorf("expected no log output with delay=0; got %q", buf.String())
	}
}

func TestCommitPackageVariable_IsResolvedAtInit(t *testing.T) {
	// After init() the commit var must not be empty, and (when buildinfo gave
	// us a real revision) it must have a plausible shape. The dev container
	// mounts the repo so buildinfo's vcs settings are populated.
	if commit == "" {
		t.Fatal("commit is empty; init() failed to resolve")
	}
	if commit == commitSentinel {
		t.Skip("buildinfo lacks vcs.revision in this environment; nothing to assert beyond non-empty")
	}
	trimmed := strings.TrimSuffix(commit, "-dirty")
	if len(trimmed) < 7 {
		t.Errorf("commit = %q; trimmed revision %q looks too short to be a real SHA", commit, trimmed)
	}
}

// ---- pflag CLI convention --------------------------------------------------

// The CLI is POSIX double-dash (github.com/spf13/pflag): a single-dash long
// flag is not a long flag at all, so it must fail parse with the exit-2
// flag-error code rather than be silently accepted. This pins the convention
// against an accidental swap back to the standard-library flag package, which
// would accept both -flag and --flag.
func TestRun_SingleDashLongFlagFailsParse(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{"-port", "9999"}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 for single-dash long flag; stderr=%q", code, stderr.String())
	}
}

// The double-dash form of the same flag parses; the snippet directory is
// observable over the jsonnet endpoint, proving the value reached the handler.
func TestRun_DoubleDashLongFlagParses(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok", "main.jsonnet"), []byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := runInBackground(t, []string{"--snippet-directory", dir}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/ok")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with --snippet-directory", resp.StatusCode)
	}
}

// Repeatable flags use pflag StringArray, which appends one value per flag
// occurrence and must NOT comma-split — a single --ext-var carrying a comma
// (or a path containing one) stays a single value. This asserts each
// occurrence appends rather than overwrites, and that a comma in the value
// is preserved verbatim.
func TestRun_ExtVarFlag_RepeatableAndNotCommaSplit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "show"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "show", "main.jsonnet"),
		[]byte(`{ a: std.extVar("a"), b: std.extVar("b") }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := runInBackground(t, []string{
		"--snippet-directory=" + dir,
		"--ext-var=a=one",
		"--ext-var=b=x,y", // comma must survive — StringArray, not StringSlice
	}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/show")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"one"`) {
		t.Errorf("body = %q, want first --ext-var value 'one'", string(body))
	}
	if !strings.Contains(string(body), `"x,y"`) {
		t.Errorf("body = %q, want comma preserved verbatim ('x,y'); StringArray must not comma-split", string(body))
	}
}

// The webhook server is wired only inside the operator boot path, so
// --enable-webhook without --enable-flux-integration would silently boot an
// HTTP-only evaluator with no admission validation. run rejects that combo as
// a flag error (exit 2) rather than booting a security-relevant no-op. This is
// a pure flag-level check that needs no apiserver.
func TestRun_EnableWebhookWithoutFluxIntegrationFailsWithExit2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{"--enable-webhook"}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--enable-webhook requires --enable-flux-integration") {
		t.Errorf("stderr = %q, want it to explain the flag dependency", stderr.String())
	}
}

func withRestoredSlogDefault(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// ---- run: helpers ----------------------------------------------------------

// freePort opens an ephemeral listener, captures its address, and closes it.
// There is an inherent race between releasing the port and the next caller
// re-binding it; in practice this is reliable enough for tests on a single
// machine and is the standard "find a free port" pattern.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if err := l.Close(); err != nil {
		t.Fatalf("freePort close: %v", err)
	}
	return port
}

// waitForReady polls an /live endpoint until it responds 200 or the deadline
// hits, so tests don't have to time.Sleep for indeterminate startup windows.
func waitForReady(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/live")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become live within %v", addr, timeout)
}

// runInBackground starts run() in a goroutine and returns a way to drive its
// shutdown and recover its exit code. Tests should defer cleanup to make sure
// the goroutine doesn't leak if assertions fail.
type runHandle struct {
	t       *testing.T
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	sigs    chan os.Signal
	done    chan int
	jsonnet string // host:port
	mgmt    string // host:port
}

func runInBackground(t *testing.T, extraArgs []string, env []string) *runHandle {
	t.Helper()
	jsonnetPort := freePort(t)
	mgmtPort := freePort(t)

	h := &runHandle{
		t:       t,
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
		sigs:    make(chan os.Signal, 1),
		done:    make(chan int, 1),
		jsonnet: "127.0.0.1:" + jsonnetPort,
		mgmt:    "127.0.0.1:" + mgmtPort,
	}

	args := append([]string{
		"--listen-address=127.0.0.1",
		"--port=" + jsonnetPort,
		"--management-listen-address=127.0.0.1",
		"--management-port=" + mgmtPort,
		"--shutdown-delay=0",
	}, extraArgs...)

	withRestoredSlogDefault(t)

	go func() {
		h.done <- run(args, env, h.stdout, h.stderr, h.sigs)
	}()

	waitForReady(t, h.mgmt, 30*time.Second)
	return h
}

func (h *runHandle) shutdown(t *testing.T, want int) {
	t.Helper()
	h.sigs <- syscall.SIGTERM
	select {
	case got := <-h.done:
		if got != want {
			t.Errorf("exit code = %d, want %d; stdout=%s; stderr=%s",
				got, want, h.stdout.String(), h.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("run did not return within 10s")
	}
}

// ---- run: behavior --------------------------------------------------------

// TestRun_DualStackBindAddressBoots proves the chart-default `::`
// (IPv6 wildcard, with IPV6_V6ONLY=0 it accepts IPv4 too on Linux)
// listen address gets through net.JoinHostPort without an
// "address X: too many colons" stumble, and the resulting listener
// accepts a connection from 127.0.0.1.
func TestOCILibraryAliasesFromPaths(t *testing.T) {
	// Two paths, each with two subdirs. One overlapping name.
	dirA := t.TempDir()
	dirB := t.TempDir()
	for _, sub := range []string{"grafonnet", "docsonnet"} {
		if err := os.Mkdir(filepath.Join(dirA, sub), 0o755); err != nil {
			t.Fatalf("mkdir A/%s: %v", sub, err)
		}
	}
	for _, sub := range []string{"xtd", "grafonnet"} { // grafonnet duplicates dirA's
		if err := os.Mkdir(filepath.Join(dirB, sub), 0o755); err != nil {
			t.Fatalf("mkdir B/%s: %v", sub, err)
		}
	}
	// Drop a non-dir entry under dirA — must be filtered.
	if err := os.WriteFile(filepath.Join(dirA, "junk.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ociLibraryAliasesFromPaths([]string{dirA, dirB})

	// Order matches walk order — assert membership + dedup.
	want := map[string]bool{"grafonnet": true, "docsonnet": true, "xtd": true}
	if len(got) != len(want) {
		t.Errorf("got %v (len %d), want %d unique aliases", got, len(got), len(want))
	}
	seen := map[string]int{}
	for _, name := range got {
		seen[name]++
	}
	for k := range want {
		if seen[k] != 1 {
			t.Errorf("alias %q appeared %d times, want 1", k, seen[k])
		}
	}
	if _, present := seen["junk.txt"]; present {
		t.Error("junk.txt was incorrectly classified as an alias (filter dropped non-dirs)")
	}
}

func TestOCILibraryAliasesFromPaths_MissingDirSkippedSilently(t *testing.T) {
	got := ociLibraryAliasesFromPaths([]string{"/this/does/not/exist", t.TempDir()})
	if len(got) != 0 {
		t.Errorf("got %v, want empty slice (no readable subdirs)", got)
	}
}

func TestOCILibraryAliasesFromPaths_EmptyInput(t *testing.T) {
	got := ociLibraryAliasesFromPaths(nil)
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestOCILibrariesFromPaths_LoadsFileContents(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "grafonnet")
	if err := os.MkdirAll(filepath.Join(libDir, "panels"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "main.libsonnet"), []byte(`{ root: true }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "panels", "graph.libsonnet"), []byte(`{ kind: "graph" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-importable extension: must be skipped.
	if err := os.WriteFile(filepath.Join(libDir, "README.md"), []byte(`# grafonnet`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ociLibrariesFromPaths([]string{dir})
	lib, ok := got["grafonnet"]
	if !ok {
		t.Fatalf("grafonnet alias missing; got %v", got)
	}
	if lib.Files["main.libsonnet"] != `{ root: true }` {
		t.Errorf("main.libsonnet = %q", lib.Files["main.libsonnet"])
	}
	if lib.Files["panels/graph.libsonnet"] != `{ kind: "graph" }` {
		t.Errorf("panels/graph.libsonnet = %q", lib.Files["panels/graph.libsonnet"])
	}
	if _, surfaced := lib.Files["README.md"]; surfaced {
		t.Error("README.md unexpectedly included; non-jsonnet files must be filtered")
	}
}

func TestOCILibrariesFromPaths_FirstWriteWinsOnDuplicateAlias(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirA, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dirB, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "shared", "main.libsonnet"), []byte(`{ from: "A" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "shared", "main.libsonnet"), []byte(`{ from: "B" }`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ociLibrariesFromPaths([]string{dirA, dirB})
	lib := got["shared"]
	if lib.Files["main.libsonnet"] != `{ from: "A" }` {
		t.Errorf("got %q, want first-write-wins from dirA", lib.Files["main.libsonnet"])
	}
}

func TestOCILibrariesFromPaths_MissingDirsAreSkipped(t *testing.T) {
	got := ociLibrariesFromPaths([]string{"/does/not/exist"})
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestLoadOCILibraries_LoadsReadableAndSkipsEmpty(t *testing.T) {
	// One path holds a usable library; the other is an empty directory with
	// a content-free subdir. loadOCILibraries must fold in the usable one
	// and warn-and-continue past the empty subdir without failing.
	good := t.TempDir()
	libDir := filepath.Join(good, "examplonet")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "main.libsonnet"), []byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}

	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "nothing"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := loadOCILibraries(context.Background(), []string{good, empty})
	if _, ok := got["examplonet"]; !ok {
		t.Fatalf("examplonet missing; got %v", got)
	}
	if _, ok := got["nothing"]; ok {
		t.Error("empty library directory must not surface an alias")
	}
	if len(got) != 1 {
		t.Errorf("loaded %d libraries, want exactly 1", len(got))
	}
}

func TestLoadOCILibraries_EmptyResultIsEmptyMap(t *testing.T) {
	got := loadOCILibraries(context.Background(), nil)
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestProvisionSelfSignedWebhookCert_EmptyNamespaceErrors(t *testing.T) {
	_, err := provisionSelfSignedWebhookCert(context.Background(), &rest.Config{}, selfsignedConfig{
		Namespace: "",
		CertDir:   t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for empty namespace, got nil")
	}
	if !strings.Contains(err.Error(), "namespace is required") {
		t.Errorf("error = %v, want it to mention the missing namespace", err)
	}
}

func TestProvisionSelfSignedWebhookCert_UnwritableCertDirErrors(t *testing.T) {
	// CertDir is rooted under a regular file, so MkdirAll cannot create it.
	parent := t.TempDir()
	notADir := filepath.Join(parent, "regular-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := provisionSelfSignedWebhookCert(context.Background(), &rest.Config{}, selfsignedConfig{
		Namespace: "jaas-system",
		CertDir:   filepath.Join(notADir, "certs"),
	})
	if err == nil {
		t.Fatal("expected error for unwritable cert dir, got nil")
	}
	if !strings.Contains(err.Error(), "mkdir cert dir") {
		t.Errorf("error = %v, want mkdir cert dir failure", err)
	}
}

func TestRun_DualStackBindAddressBoots(t *testing.T) {
	jsonnetPort := freePort(t)
	mgmtPort := freePort(t)

	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	done := make(chan int, 1)
	withRestoredSlogDefault(t)
	go func() {
		done <- run([]string{
			"--listen-address=::",
			"--port=" + jsonnetPort,
			"--management-listen-address=::",
			"--management-port=" + mgmtPort,
			"--shutdown-delay=0",
		}, nil, &stdout, &stderr, sigs)
	}()

	// /live must be reachable over IPv4 — proves the dual-stack
	// socket accepts v4-mapped connections.
	waitForReady(t, "127.0.0.1:"+mgmtPort, 30*time.Second)

	sigs <- syscall.SIGTERM
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0; stdout=%s stderr=%s",
				code, stdout.String(), stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s")
	}
}

func TestRun_VersionFlagPrintsAndExits(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"--version"}, nil, &stdout, &stderr, sigs)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "version: ") {
		t.Errorf("stdout = %q, want it to start with 'version: '", out)
	}
	if !strings.Contains(out, "commit:") {
		t.Errorf("stdout = %q, want it to contain 'commit:'", out)
	}
}

func TestRun_VersionFlagBeforeOtherFlagsStillWorks(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"--version", "--port=9999"}, nil, &stdout, &stderr, sigs)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRun_HelpFlagReturnsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"--help"}, nil, &stdout, &stderr, sigs)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
}

func TestRun_HelpFlagWritesUsageToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	_ = run([]string{"--help"}, nil, &stdout, &stderr, sigs)
	// pflag's usage output lists flags in their POSIX double-dash form, e.g. "--port".
	if !strings.Contains(stderr.String(), "--port") {
		t.Errorf("stderr = %q, want usage output", stderr.String())
	}
}

func TestRun_UnknownFlagReturnsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"--this-flag-does-not-exist"}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestParseWatchNamespaces(t *testing.T) {
	cases := []struct {
		name string
		flag string
		env  []string
		want []string
	}{
		{"empty is cluster-wide (nil)", "", nil, nil},
		{"comma list", "a,b,c", nil, []string{"a", "b", "c"}},
		{"trims and drops empties", " a , ,b, ", nil, []string{"a", "b"}},
		{"env fallback when flag empty", "", []string{"FOO=x", "JAAS_WATCH_NAMESPACES=team-a,team-b"}, []string{"team-a", "team-b"}},
		{"flag wins over env", "only", []string{"JAAS_WATCH_NAMESPACES=ignored"}, []string{"only"}},
		{"blank flag and blank env is nil", "   ", []string{"JAAS_WATCH_NAMESPACES="}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseWatchNamespaces(tc.flag, tc.env)
			if !slices.Equal(got, tc.want) {
				t.Errorf("parseWatchNamespaces(%q, %v) = %v, want %v", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

func TestAwaitGoroutines_TimesOutOnUnclosedChannel(t *testing.T) {
	never := make(chan struct{})
	if awaitGoroutines(context.Background(), 10*time.Millisecond, map[string]<-chan struct{}{"stuck": never}) {
		t.Error("awaitGoroutines returned true while a channel never closes")
	}
}

func TestAwaitGoroutines_ClosedAndNilChannelsBothOK(t *testing.T) {
	done := make(chan struct{})
	close(done)
	ok := awaitGoroutines(context.Background(), time.Second, map[string]<-chan struct{}{
		"closed":  done,
		"skipped": nil, // a goroutine that never started → skipped
	})
	if !ok {
		t.Error("awaitGoroutines returned false; a closed channel and a nil (skipped) one should both pass")
	}
}

// fakeSweepBackend satisfies storage.Backend (via the embedded nil
// interface) but only implements Sweep, which is all runStorageSweep calls.
type fakeSweepBackend struct {
	storage.Backend
	calls atomic.Int32
	n     int
	err   error
}

func (f *fakeSweepBackend) Sweep(_ context.Context, _ time.Duration) (int, error) {
	f.calls.Add(1)
	return f.n, f.err
}

func TestRunStorageSweep_SurvivesErrorAndCountsRemovals(t *testing.T) {
	for _, tc := range []struct {
		name string
		be   *fakeSweepBackend
	}{
		{"error path keeps ticking", &fakeSweepBackend{err: errors.New("boom")}},
		{"success path with removals", &fakeSweepBackend{n: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { runStorageSweep(ctx, tc.be, time.Millisecond, time.Minute); close(done) }()
			deadline := time.After(2 * time.Second)
			for tc.be.calls.Load() < 2 {
				select {
				case <-deadline:
					cancel()
					<-done
					t.Fatalf("sweep ran only %d times before timeout", tc.be.calls.Load())
				default:
					time.Sleep(time.Millisecond)
				}
			}
			cancel()
			<-done
		})
	}
}

func TestNewStorageBackend(t *testing.T) {
	t.Run("local missing path", func(t *testing.T) {
		var stderr bytes.Buffer
		b, ok := newStorageBackend(context.Background(), &stderr, "local", "", storage.S3Config{})
		if ok || b != nil {
			t.Errorf("got (%v, %v), want (nil, false)", b, ok)
		}
		if !strings.Contains(stderr.String(), "storage-path") {
			t.Errorf("stderr = %q, want it to name -storage-path", stderr.String())
		}
	})
	t.Run("local ok", func(t *testing.T) {
		var stderr bytes.Buffer
		b, ok := newStorageBackend(context.Background(), &stderr, "local", t.TempDir(), storage.S3Config{})
		if !ok || b == nil {
			t.Fatalf("got (%v, %v), want a backend", b, ok)
		}
		_ = b.Close()
	})
	t.Run("s3 missing endpoint/bucket", func(t *testing.T) {
		var stderr bytes.Buffer
		if _, ok := newStorageBackend(context.Background(), &stderr, "s3", "", storage.S3Config{}); ok {
			t.Error("want ok=false for missing S3 endpoint/bucket")
		}
		if !strings.Contains(stderr.String(), "s3-endpoint") {
			t.Errorf("stderr = %q, want it to name -s3-endpoint", stderr.String())
		}
	})
	t.Run("s3 ok", func(t *testing.T) {
		var stderr bytes.Buffer
		b, ok := newStorageBackend(context.Background(), &stderr, "s3", "",
			storage.S3Config{Endpoint: "s3.example.com", Bucket: "b", UseSSL: true})
		if !ok || b == nil {
			t.Fatalf("got (%v, %v), want a backend", b, ok)
		}
		_ = b.Close()
	})
	t.Run("unknown backend", func(t *testing.T) {
		var stderr bytes.Buffer
		if _, ok := newStorageBackend(context.Background(), &stderr, "bogus", "", storage.S3Config{}); ok {
			t.Error("want ok=false for an unknown backend")
		}
		if !strings.Contains(stderr.String(), "storage-backend") {
			t.Errorf("stderr = %q, want it to name -storage-backend", stderr.String())
		}
	})
}

func TestRun_WebhookWithoutFluxIntegrationReturnsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"--enable-webhook"}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (webhook requires flux integration)", code)
	}
	if !strings.Contains(stderr.String(), "enable-flux-integration") {
		t.Errorf("stderr = %q, want it to name the missing flag", stderr.String())
	}
}

func TestRun_MalformedDurationFlagReturnsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"--write-timeout=not-a-duration"}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_MalformedIntFlagReturnsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"--max-stack=not-an-int"}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_BindFailureOnJsonnetPortReturnsOne(t *testing.T) {
	// Pre-bind the jsonnet port, then ask run to bind the same one.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("preb-bind: %v", err)
	}
	defer blocker.Close()
	blockedPort := strconv.Itoa(blocker.Addr().(*net.TCPAddr).Port)

	mgmtPort := freePort(t)

	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{
		"--listen-address=127.0.0.1",
		"--port=" + blockedPort,
		"--management-listen-address=127.0.0.1",
		"--management-port=" + mgmtPort,
		"--shutdown-delay=0",
	}, nil, &stdout, &stderr, sigs)

	if code != 1 {
		t.Errorf("exit code = %d, want 1; stdout=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Cannot bind jsonnet listener") {
		t.Errorf("expected jsonnet bind error in logs; got %q", stdout.String())
	}
}

func TestRun_BindFailureOnManagementPortReturnsOne(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind: %v", err)
	}
	defer blocker.Close()
	blockedPort := strconv.Itoa(blocker.Addr().(*net.TCPAddr).Port)

	jsonnetPort := freePort(t)

	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{
		"--listen-address=127.0.0.1",
		"--port=" + jsonnetPort,
		"--management-listen-address=127.0.0.1",
		"--management-port=" + blockedPort,
		"--shutdown-delay=0",
	}, nil, &stdout, &stderr, sigs)

	if code != 1 {
		t.Errorf("exit code = %d, want 1; stdout=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "Cannot bind management listener") {
		t.Errorf("expected management bind error in logs; got %q", stdout.String())
	}
}

func TestRun_GracefulShutdownOnSIGTERM(t *testing.T) {
	h := runInBackground(t, nil, nil)
	h.shutdown(t, 0)
}

func TestRun_GracefulShutdownOnSIGINT(t *testing.T) {
	h := runInBackground(t, nil, nil)
	h.sigs <- syscall.SIGINT
	select {
	case code := <-h.done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return on SIGINT")
	}
}

func TestRun_ReadinessFlipsToReadyAfterStartup(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.mgmt + "/ready")
	if err != nil {
		t.Fatalf("GET /ready: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/ready status = %d, want 200 after startup", resp.StatusCode)
	}
}

func TestRun_StartedProbeReturnsOKAfterStartup(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.mgmt + "/start")
	if err != nil {
		t.Fatalf("GET /start: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/start status = %d, want 200 after startup", resp.StatusCode)
	}
}

func TestRun_ServesJsonnetSnippet(t *testing.T) {
	// Wire a snippet directory so we can verify the jsonnet handler is reachable.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello", "main.jsonnet"), []byte(`{ greeting: "hi" }`), 0o644); err != nil {
		t.Fatal(err)
	}

	h := runInBackground(t, []string{"--snippet-directory=" + dir}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/hello")
	if err != nil {
		t.Fatalf("GET /jsonnet/hello: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"hi"`) {
		t.Errorf("body = %q, want it to contain \"hi\"", string(body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestRun_LoadsExtVarsFromEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "echo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "echo", "main.jsonnet"),
		[]byte(`{ region: std.extVar("region") }`), 0o644); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"PATH=/usr/bin",
		"JAAS_EXT_VAR_region=us-east-1",
		"HOME=/home/x",
	}

	h := runInBackground(t, []string{"--snippet-directory=" + dir}, env)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/echo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"us-east-1"`) {
		t.Errorf("body = %q, want it to contain 'us-east-1'", string(body))
	}
}

func TestRun_DoesNotLoadExtVarsFromActualOSEnviron(t *testing.T) {
	// Sanity check that env is passed through; setting JAAS_EXT_VAR_* in the
	// real process env should have no effect on a run() called with explicit
	// env that doesn't contain it.
	t.Setenv("JAAS_EXT_VAR_should_not_leak", "leaked-into-test")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe", "main.jsonnet"),
		// std.extVar("should_not_leak") will fail because we didn't pass it.
		[]byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pass a sanitized env without the JAAS_EXT_VAR_ entry.
	h := runInBackground(t, []string{"--snippet-directory=" + dir}, []string{"PATH=/usr/bin"})
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/probe")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "leaked-into-test") {
		t.Errorf("value from os.Environ leaked into run's ExtVars: %q", string(body))
	}
}

func TestRun_LogsStartupBanner(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)
	out := h.stdout.String()
	if !strings.Contains(out, "Starting JaaS") {
		t.Errorf("expected 'Starting JaaS' banner in logs; got %q", out)
	}
	if !strings.Contains(out, `"version":`) {
		t.Errorf("expected version attr in logs; got %q", out)
	}
}

func TestRun_LogsCliFlagsAtInfo(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)
	if !strings.Contains(h.stdout.String(), "CLI flags parsed") {
		t.Errorf("expected 'CLI flags parsed' in logs; got %q", h.stdout.String())
	}
}

func TestRun_LogsShutdownMessage(t *testing.T) {
	h := runInBackground(t, nil, nil)
	h.shutdown(t, 0)
	if !strings.Contains(h.stdout.String(), "JaaS service has shut down") {
		t.Errorf("expected shutdown message in logs; got %q", h.stdout.String())
	}
}

func TestRun_LogsSignalNameOnShutdown(t *testing.T) {
	h := runInBackground(t, nil, nil)
	h.shutdown(t, 0)
	if !strings.Contains(h.stdout.String(), "Received signal") {
		t.Errorf("expected 'Received signal' in logs; got %q", h.stdout.String())
	}
}

func TestRun_LivenessProbeAvailable(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)
	resp, err := http.Get("http://" + h.mgmt + "/live")
	if err != nil {
		t.Fatalf("GET /live: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/live status = %d, want 200", resp.StatusCode)
	}
}

func TestRun_RespectsCustomJsonnetEndpointPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x", "main.jsonnet"), []byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := runInBackground(t, []string{
		"--snippet-directory=" + dir,
		"--jsonnet-endpoint-path=eval",
	}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/eval/x")
	if err != nil {
		t.Fatalf("GET /eval/x: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 at custom endpoint path", resp.StatusCode)
	}

	// And the default `/jsonnet/` path should NOT be served.
	resp2, err := http.Get("http://" + h.jsonnet + "/jsonnet/x")
	if err != nil {
		t.Fatalf("GET /jsonnet/x: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Error("default /jsonnet/ path served even though endpoint was customized")
	}
}

func TestRun_TwoConsecutiveCallsWorkInSameProcess(t *testing.T) {
	// Critical for testability: the refactor must not stash state in the
	// global flag.CommandLine. Two run() calls in sequence should both succeed
	// without "flag redefined" panics from the standard library.
	h1 := runInBackground(t, nil, nil)
	h1.shutdown(t, 0)

	h2 := runInBackground(t, nil, nil)
	h2.shutdown(t, 0)
}

func TestRun_ManagementServerDoesNotExposeJsonnetEndpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x", "main.jsonnet"), []byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := runInBackground(t, []string{"--snippet-directory=" + dir}, nil)
	defer h.shutdown(t, 0)

	// Jsonnet endpoint on the management port should 404, not serve.
	resp, err := http.Get("http://" + h.mgmt + "/jsonnet/x")
	if err != nil {
		t.Fatalf("GET on management port: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("management port served jsonnet endpoint; ports must be isolated")
	}
}

func TestRun_JsonnetServerDoesNotExposeProbes(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)

	// Probes on the jsonnet port should 404, not serve.
	resp, err := http.Get("http://" + h.jsonnet + "/live")
	if err != nil {
		t.Fatalf("GET on jsonnet port: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("jsonnet port served /live; ports must be isolated")
	}
}

// ---- JSON error bodies on non-2xx, observed over the wire -----------------

// decodeErrorBody reads & decodes a handler.ProblemDetails from an HTTP response.
func decodeErrorBody(t *testing.T, resp *http.Response) handler.ProblemDetails {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got handler.ProblemDetails
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return got
}

func TestRun_ErrorBody_SnippetNotFound_OverTCP(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/no-such-snippet")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	body := decodeErrorBody(t, resp)
	if body.Code != handler.ErrCodeSnippetNotFound {
		t.Errorf("code = %q, want %q", body.Code, handler.ErrCodeSnippetNotFound)
	}
	if body.Snippet != "no-such-snippet" {
		t.Errorf("snippet = %q, want %q", body.Snippet, "no-such-snippet")
	}
}

func TestRun_ErrorBody_MethodNotAllowed_OverTCP(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)

	req, err := http.NewRequest(http.MethodPost, "http://"+h.jsonnet+"/jsonnet/whatever", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	body := decodeErrorBody(t, resp)
	if body.Code != handler.ErrCodeMethodNotAllowed {
		t.Errorf("code = %q, want %q", body.Code, handler.ErrCodeMethodNotAllowed)
	}
}

func TestRun_ErrorBody_EvaluationFailed_OverTCP(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken", "main.jsonnet"), []byte(`local x =`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := runInBackground(t, []string{"--snippet-directory=" + dir}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/broken")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	body := decodeErrorBody(t, resp)
	if body.Code != handler.ErrCodeEvaluationFailed {
		t.Errorf("code = %q, want %q", body.Code, handler.ErrCodeEvaluationFailed)
	}
	if body.Snippet != "broken" {
		t.Errorf("snippet = %q, want %q", body.Snippet, "broken")
	}
	// The detail is the scrubbed constant — the go-jsonnet diagnostic stays in
	// the server logs, never the body — but it must still be non-empty.
	if body.Detail == "" {
		t.Error("detail must not be empty")
	}
}

func TestRun_ErrorBody_EvaluationTimeout_OverTCP(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "slow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow", "main.jsonnet"),
		[]byte(`local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := runInBackground(t, []string{
		"--snippet-directory=" + dir,
		"--evaluation-timeout=1us",
	}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/slow")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	body := decodeErrorBody(t, resp)
	if body.Code != handler.ErrCodeEvaluationTimeout {
		t.Errorf("code = %q, want %q", body.Code, handler.ErrCodeEvaluationTimeout)
	}
	if body.Snippet != "slow" {
		t.Errorf("snippet = %q, want %q", body.Snippet, "slow")
	}
}

// ---- new in v2: --ext-var, --enable-flux-integration and friends -----------

func TestRun_ExtVarFlag_PopulatesHandlerExtVars(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "show"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "show", "main.jsonnet"),
		[]byte(`{ env: std.extVar("env") }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := runInBackground(t,
		[]string{"--snippet-directory=" + dir, "--ext-var=env=dev"}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/show")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"dev"`) {
		t.Errorf("body = %q, want it to contain \"dev\"", string(body))
	}
}

func TestRun_ExtVarFlag_OverlaysJaasExtVarEnvOnConflict(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "show"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "show", "main.jsonnet"),
		[]byte(`{ region: std.extVar("region") }`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Env says us-east-1; CLI -ext-var says us-west-2; CLI wins.
	h := runInBackground(t,
		[]string{"--snippet-directory=" + dir, "--ext-var=region=us-west-2"},
		[]string{"JAAS_EXT_VAR_region=us-east-1"})
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/show")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"us-west-2"`) {
		t.Errorf("body = %q, want CLI to override env (\"us-west-2\")", string(body))
	}
}

func TestRun_ExtVarFlag_InvalidFormatReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{"--ext-var=noequals"}, nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Invalid --ext-var") {
		t.Errorf("stderr = %q, want it to mention --ext-var", stderr.String())
	}
}

func TestRun_ExtVarFlag_EmptyKeyReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{"--ext-var==orphan-value"}, nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
}

func TestRun_FluxIntegration_DefaultDisabledLeavesOperatorOff(t *testing.T) {
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)
	if strings.Contains(h.stdout.String(), "Operator manager ready") {
		t.Errorf("operator booted with default flags; stdout=%s", h.stdout.String())
	}
}

func TestRun_FluxIntegration_InvalidRerenderRateReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{"--enable-flux-integration", "--rerender-rate=garbage"},
		nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Invalid --rerender-rate") {
		t.Errorf("stderr = %q, want it to mention --rerender-rate", stderr.String())
	}
}

func TestRun_FluxIntegration_ZeroRerenderBurstReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{"--enable-flux-integration", "--rerender-burst=0"},
		nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "rerender-burst") {
		t.Errorf("stderr = %q, want it to mention rerender-burst", stderr.String())
	}
}

func TestRun_FluxIntegration_BadKubeconfigPathReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	storage := t.TempDir()
	code := run([]string{
		"--enable-flux-integration",
		"--kubeconfig=" + missing,
		"--storage-path=" + storage,
		"--storage-base-url=http://example",
	}, nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stdout=%s; stderr=%s",
			code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Cannot load kubeconfig") {
		t.Errorf("expected 'Cannot load kubeconfig' in logs; got %q", stdout.String())
	}
}

func TestRun_FluxIntegration_MissingStoragePathReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	// Supply a base-url so we exit on the path check (which is now
	// guarded by -storage-backend=local), not on the base-url check
	// that runs first.
	code := run([]string{
		"--enable-flux-integration",
		"--storage-base-url=http://x",
	}, nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "storage-path") {
		t.Errorf("stderr = %q, want it to mention storage-path", stderr.String())
	}
}

func TestRun_FluxIntegration_MissingStorageBaseURLReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{
		"--enable-flux-integration",
		"--storage-path=" + t.TempDir(),
	}, nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "storage-base-url") {
		t.Errorf("stderr = %q, want it to mention storage-base-url", stderr.String())
	}
}

func TestRun_FluxIntegration_BadStoragePathReturnsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{
		"--enable-flux-integration",
		"--storage-path=/proc/1/jaas-cannot-mkdir-here",
		"--storage-base-url=http://example",
	}, nil, &stdout, &stderr, sigs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "Cannot open storage") {
		t.Errorf("stdout = %q, want 'Cannot open storage'", stdout.String())
	}
}

func TestRun_FluxIntegration_UnknownBackendReturnsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	withRestoredSlogDefault(t)
	code := run([]string{
		"--enable-flux-integration",
		"--storage-base-url=http://x",
		"--storage-backend=disk",
	}, nil, &stdout, &stderr, sigs)
	// An out-of-set enum is a usage error caught by Flags.Validate at parse
	// time (exit 2), not a runtime failure (exit 1).
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "storage-backend") {
		t.Errorf("stderr = %q, want it to mention storage-backend", stderr.String())
	}
}

func TestRun_FluxIntegration_S3RequiresEndpointAndBucket(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing endpoint",
			args: []string{"--s3-bucket=b"},
			want: "s3-endpoint",
		},
		{
			name: "missing bucket",
			args: []string{"--s3-endpoint=s3.example"},
			want: "s3-bucket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			sigs := make(chan os.Signal, 1)
			withRestoredSlogDefault(t)
			args := append([]string{
				"--enable-flux-integration",
				"--storage-base-url=http://x",
				"--storage-backend=s3",
			}, tc.args...)
			code := run(args, nil, &stdout, &stderr, sigs)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to mention %s", stderr.String(), tc.want)
			}
		})
	}
}

func TestLoadKubeconfig_NonexistentPathReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-file.yaml")
	if _, err := loadKubeconfig(missing); err == nil {
		t.Errorf("loadKubeconfig(%q) = nil, want error", missing)
	}
}

func TestLoadKubeconfig_EmptyPathDelegatesToCtrlGetConfig(t *testing.T) {
	// With no in-cluster credentials, no KUBECONFIG env, and no
	// $HOME/.kube/config available in the dev shell, ctrl.GetConfig should
	// return an error. We only assert "delegates" by observing the error
	// shape, not the specific text — which varies by k8s.io versions.
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", t.TempDir()) // no ~/.kube/config in this temp HOME
	if _, err := loadKubeconfig(""); err == nil {
		// If running inside a cluster, this path could actually succeed.
		// Skip rather than fail in that exotic environment.
		t.Skip("loadKubeconfig(empty) returned a config; running in-cluster?")
	}
}

func TestRun_ErrorBody_AllPathsCarryContentLengthHeader(t *testing.T) {
	// Confirms net/http auto-computes Content-Length for the JSON bodies, so
	// the bridge can stream-allocate without surprises.
	h := runInBackground(t, nil, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/no-such")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Length") == "" {
		t.Errorf("Content-Length missing on 404; headers = %v", resp.Header)
	}
}
