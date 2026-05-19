/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"log/slog"
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
