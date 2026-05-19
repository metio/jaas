/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"log/slog"
	"runtime/debug"
	"strings"
	"testing"
)

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
