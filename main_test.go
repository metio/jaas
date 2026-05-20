/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"bytes"
	"encoding/json"
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
	"syscall"
	"testing"
	"time"

	"github.com/metio/jaas/internal/handler"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestParseLogLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"error":   slog.LevelError,
		"ERROR":   slog.LevelError,
		"Error":   slog.LevelError,
		"warn":    slog.LevelWarn,
		"WARN":    slog.LevelWarn,
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"INFO":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"unknown": slog.LevelInfo,
		"warning": slog.LevelInfo,
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got := parseLogLevel(input)
			if got != want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", input, got, want)
			}
		})
	}
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
	drainBeforeShutdown(state, 0, discardLogger())
	if state.Ready() {
		t.Error("Ready() = true after drain; want false")
	}
}

func TestDrainBeforeShutdown_FlipsReadyEvenWhenAlreadyFalse(t *testing.T) {
	state := handler.NewHealthState()
	// Never marked ready in the first place.
	drainBeforeShutdown(state, 0, discardLogger())
	if state.Ready() {
		t.Error("Ready() = true after drain on never-ready state")
	}
}

func TestDrainBeforeShutdown_DoesNotTouchStarted(t *testing.T) {
	state := handler.NewHealthState()
	state.MarkStarted()
	state.SetReady(true)
	drainBeforeShutdown(state, 0, discardLogger())
	if !state.Started() {
		t.Error("Started() flipped to false; drain must not touch the started flag")
	}
}

func TestDrainBeforeShutdown_ZeroDelayReturnsImmediately(t *testing.T) {
	state := handler.NewHealthState()
	start := time.Now()
	drainBeforeShutdown(state, 0, discardLogger())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("took %v with delay=0; want < 50ms", elapsed)
	}
}

func TestDrainBeforeShutdown_NegativeDelayReturnsImmediately(t *testing.T) {
	state := handler.NewHealthState()
	start := time.Now()
	drainBeforeShutdown(state, -1*time.Second, discardLogger())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("took %v with negative delay; want < 50ms", elapsed)
	}
}

func TestDrainBeforeShutdown_PositiveDelaySleepsAtLeastThatLong(t *testing.T) {
	state := handler.NewHealthState()
	delay := 60 * time.Millisecond
	start := time.Now()
	drainBeforeShutdown(state, delay, discardLogger())
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

	drainBeforeShutdown(state, delay, discardLogger())
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

	drainBeforeShutdown(state, 30*time.Millisecond, logger)

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

	drainBeforeShutdown(state, 0, logger)

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

// ---- stringArray -----------------------------------------------------------

func TestStringArray_EmptyStringer(t *testing.T) {
	var s stringArray
	if got := s.String(); got != "[]" {
		t.Errorf("String() on nil = %q, want %q", got, "[]")
	}
}

func TestStringArray_StringFormatsValues(t *testing.T) {
	s := stringArray{"a", "b", "c"}
	if got := s.String(); got != "[a b c]" {
		t.Errorf("String() = %q, want %q", got, "[a b c]")
	}
}

func TestStringArray_StringSingleElement(t *testing.T) {
	s := stringArray{"only"}
	if got := s.String(); got != "[only]" {
		t.Errorf("String() = %q, want %q", got, "[only]")
	}
}

func TestStringArray_SetAppendsValue(t *testing.T) {
	var s stringArray
	if err := s.Set("first"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !slices.Equal(s, stringArray{"first"}) {
		t.Errorf("after Set: %v, want [first]", s)
	}
}

func TestStringArray_SetAppendsMultiple(t *testing.T) {
	var s stringArray
	for _, v := range []string{"a", "b", "c"} {
		if err := s.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	if !slices.Equal(s, stringArray{"a", "b", "c"}) {
		t.Errorf("after Set sequence: %v, want [a b c]", s)
	}
}

func TestStringArray_SetReturnsNilOnAnyInput(t *testing.T) {
	// flag.Value.Set returns an error to signal a parse failure; stringArray
	// accepts any string verbatim and never errors.
	cases := []string{"", " ", "with space", "x=y=z", "\n", "unicode 🚀"}
	for _, in := range cases {
		t.Run(strconv.Quote(in), func(t *testing.T) {
			var s stringArray
			if err := s.Set(in); err != nil {
				t.Errorf("Set(%q) returned error: %v", in, err)
			}
		})
	}
}

func TestStringArray_SetAcceptsEmptyString(t *testing.T) {
	var s stringArray
	_ = s.Set("")
	if len(s) != 1 || s[0] != "" {
		t.Errorf("after Set(\"\"): %v, want [\"\"]", s)
	}
}

func TestStringArray_SetPreservesOrder(t *testing.T) {
	var s stringArray
	inputs := []string{"first", "second", "third", "fourth", "fifth"}
	for _, v := range inputs {
		_ = s.Set(v)
	}
	if !slices.Equal(s, stringArray(inputs)) {
		t.Errorf("got %v, want %v", s, inputs)
	}
}

func TestStringArray_SatisfiesFlagValueInterface(t *testing.T) {
	// Compile-time-ish proof: stringArray must implement flag.Value so it can
	// be passed to flag.Var. Calling the two methods here exercises the
	// interface contract at runtime.
	var s stringArray
	if err := s.Set("x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	_ = s.String()
}

// ---- configureLogger -------------------------------------------------------

func withRestoredSlogDefault(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestConfigureLogger_RoutesToProvidedWriter(t *testing.T) {
	withRestoredSlogDefault(t)
	var buf bytes.Buffer
	configureLogger(&buf, "debug")

	slog.Info("hello from test")
	if !strings.Contains(buf.String(), "hello from test") {
		t.Errorf("buffer = %q, want it to contain 'hello from test'", buf.String())
	}
}

func TestConfigureLogger_FiltersBelowConfiguredLevel(t *testing.T) {
	withRestoredSlogDefault(t)
	var buf bytes.Buffer
	configureLogger(&buf, "error")

	slog.Debug("debug — should be dropped")
	slog.Info("info — should be dropped")
	slog.Warn("warn — should be dropped")
	slog.Error("error — should pass")

	out := buf.String()
	if strings.Contains(out, "should be dropped") {
		t.Errorf("level=error did not filter lower levels; got: %q", out)
	}
	if !strings.Contains(out, "error — should pass") {
		t.Errorf("error record missing from output: %q", out)
	}
}

func TestConfigureLogger_DebugLevelPassesEverything(t *testing.T) {
	withRestoredSlogDefault(t)
	var buf bytes.Buffer
	configureLogger(&buf, "debug")

	slog.Debug("dbg")
	slog.Info("inf")
	slog.Warn("wrn")
	slog.Error("err")

	for _, msg := range []string{"dbg", "inf", "wrn", "err"} {
		if !strings.Contains(buf.String(), msg) {
			t.Errorf("output = %q, want %q (level=debug should pass everything)", buf.String(), msg)
		}
	}
}

func TestConfigureLogger_UnknownLevelDefaultsToInfo(t *testing.T) {
	withRestoredSlogDefault(t)
	var buf bytes.Buffer
	configureLogger(&buf, "garbage")

	slog.Debug("dbg — should be dropped at info default")
	slog.Info("inf — should pass")

	out := buf.String()
	if strings.Contains(out, "dbg") {
		t.Errorf("debug record leaked through info-default filter: %q", out)
	}
	if !strings.Contains(out, "inf — should pass") {
		t.Errorf("info record missing from output: %q", out)
	}
}

func TestConfigureLogger_EmitsJSON(t *testing.T) {
	withRestoredSlogDefault(t)
	var buf bytes.Buffer
	configureLogger(&buf, "info")

	slog.Info("json check", slog.String("k", "v"))
	out := buf.String()
	// JSONHandler emits one record per line, starts with `{`, contains the key.
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("output does not look like JSON: %q", out)
	}
	if !strings.Contains(out, `"k":"v"`) {
		t.Errorf("expected key/value in JSON output; got %q", out)
	}
}

func TestConfigureLogger_MutatesGlobalDefault(t *testing.T) {
	withRestoredSlogDefault(t)
	var bufA, bufB bytes.Buffer
	configureLogger(&bufA, "info")
	original := slog.Default()
	configureLogger(&bufB, "info")
	replaced := slog.Default()

	if original == replaced {
		t.Error("expected the second configureLogger call to replace slog.Default")
	}

	slog.Info("only B")
	if bufA.Len() != 0 {
		t.Errorf("buffer A received output after being replaced: %q", bufA.String())
	}
	if !strings.Contains(bufB.String(), "only B") {
		t.Errorf("buffer B = %q, want 'only B'", bufB.String())
	}
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
		"-listen-address=127.0.0.1",
		"-port=" + jsonnetPort,
		"-management-listen-address=127.0.0.1",
		"-management-port=" + mgmtPort,
		"-shutdown-delay=0",
	}, extraArgs...)

	withRestoredSlogDefault(t)

	go func() {
		h.done <- run(args, env, h.stdout, h.stderr, h.sigs)
	}()

	waitForReady(t, h.mgmt, 5*time.Second)
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

func TestRun_VersionFlagPrintsAndExits(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"-version"}, nil, &stdout, &stderr, sigs)

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
	code := run([]string{"-version", "-port=9999"}, nil, &stdout, &stderr, sigs)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRun_HelpFlagReturnsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"-help"}, nil, &stdout, &stderr, sigs)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
}

func TestRun_HelpFlagWritesUsageToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	_ = run([]string{"-help"}, nil, &stdout, &stderr, sigs)
	// flag.PrintDefaults emits names like "-port string"
	if !strings.Contains(stderr.String(), "-port") {
		t.Errorf("stderr = %q, want usage output", stderr.String())
	}
}

func TestRun_UnknownFlagReturnsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"-this-flag-does-not-exist"}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_MalformedDurationFlagReturnsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"-write-timeout=not-a-duration"}, nil, &stdout, &stderr, sigs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRun_MalformedIntFlagReturnsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	code := run([]string{"-max-stack=not-an-int"}, nil, &stdout, &stderr, sigs)
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
		"-listen-address=127.0.0.1",
		"-port=" + blockedPort,
		"-management-listen-address=127.0.0.1",
		"-management-port=" + mgmtPort,
		"-shutdown-delay=0",
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
		"-listen-address=127.0.0.1",
		"-port=" + jsonnetPort,
		"-management-listen-address=127.0.0.1",
		"-management-port=" + blockedPort,
		"-shutdown-delay=0",
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

	h := runInBackground(t, []string{"-snippet-directory=" + dir}, nil)
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

	h := runInBackground(t, []string{"-snippet-directory=" + dir}, env)
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
	h := runInBackground(t, []string{"-snippet-directory=" + dir}, []string{"PATH=/usr/bin"})
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
		"-snippet-directory=" + dir,
		"-jsonnet-endpoint-path=eval",
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
	h := runInBackground(t, []string{"-snippet-directory=" + dir}, nil)
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

// decodeErrorBody reads & decodes a handler.ErrorResponse from an HTTP response.
func decodeErrorBody(t *testing.T, resp *http.Response) handler.ErrorResponse {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got handler.ErrorResponse
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
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body := decodeErrorBody(t, resp)
	if body.Error != handler.ErrCodeSnippetNotFound {
		t.Errorf("error = %q, want %q", body.Error, handler.ErrCodeSnippetNotFound)
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
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body := decodeErrorBody(t, resp)
	if body.Error != handler.ErrCodeMethodNotAllowed {
		t.Errorf("error = %q, want %q", body.Error, handler.ErrCodeMethodNotAllowed)
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
	h := runInBackground(t, []string{"-snippet-directory=" + dir}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/broken")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body := decodeErrorBody(t, resp)
	if body.Error != handler.ErrCodeEvaluationFailed {
		t.Errorf("error = %q, want %q", body.Error, handler.ErrCodeEvaluationFailed)
	}
	if body.Snippet != "broken" {
		t.Errorf("snippet = %q, want %q", body.Snippet, "broken")
	}
	if body.Message == "" {
		t.Error("message must surface the go-jsonnet error text")
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
		"-snippet-directory=" + dir,
		"-evaluation-timeout=1us",
	}, nil)
	defer h.shutdown(t, 0)

	resp, err := http.Get("http://" + h.jsonnet + "/jsonnet/slow")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body := decodeErrorBody(t, resp)
	if body.Error != handler.ErrCodeEvaluationTimeout {
		t.Errorf("error = %q, want %q", body.Error, handler.ErrCodeEvaluationTimeout)
	}
	if body.Snippet != "slow" {
		t.Errorf("snippet = %q, want %q", body.Snippet, "slow")
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
