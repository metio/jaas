// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package observability

import (
	"io"
	"log/slog"
	"strings"
)

// NewLogger builds the application logger. format "text" selects slog.NewTextHandler,
// anything else slog.NewJSONHandler; level parses debug/info/warn/error (unknown -> info).
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
