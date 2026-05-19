/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"io"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
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
