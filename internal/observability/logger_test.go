// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLogger_LevelMapping(t *testing.T) {
	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"INFO":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"WARN":    slog.LevelWarn,
		"error":   slog.LevelError,
		"ERROR":   slog.LevelError,
		"":        slog.LevelInfo,
		"unknown": slog.LevelInfo,
		"warning": slog.LevelInfo,
	}
	for level, want := range tests {
		t.Run(level, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLogger(&buf, level, "json")
			h := logger.Handler()
			// The handler enables exactly the levels at or above `want`.
			if h.Enabled(t.Context(), want) == false {
				t.Errorf("level %q: handler should enable %v", level, want)
			}
			if want > slog.LevelDebug && h.Enabled(t.Context(), want-1) {
				t.Errorf("level %q: handler should not enable below %v", level, want)
			}
		})
	}
}

func TestNewLogger_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "info", "json")
	logger.Info("hello", slog.String("k", "v"))

	line := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(line, "{") {
		t.Fatalf("json format did not emit a JSON object: %q", line)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output is not parseable JSON: %v (%q)", err, line)
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	if rec["k"] != "v" {
		t.Errorf("k = %v, want v", rec["k"])
	}
}

func TestNewLogger_TextFormat(t *testing.T) {
	for _, format := range []string{"text", "TEXT", "Text"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLogger(&buf, "info", format)
			logger.Info("hello", slog.String("k", "v"))

			out := strings.TrimSpace(buf.String())
			if strings.HasPrefix(out, "{") {
				t.Fatalf("text format emitted JSON: %q", out)
			}
			if !strings.Contains(out, "msg=hello") {
				t.Errorf("text output missing msg=hello: %q", out)
			}
			if !strings.Contains(out, "k=v") {
				t.Errorf("text output missing k=v: %q", out)
			}
		})
	}
}

func TestNewLogger_UnknownFormatDefaultsToJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "info", "garbage")
	logger.Info("hi")

	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("unknown format should fall back to JSON; got unparseable %q: %v", line, err)
	}
}

func TestNewLogger_FiltersBelowLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "error", "json")
	logger.Debug("drop-debug")
	logger.Info("drop-info")
	logger.Warn("drop-warn")
	logger.Error("keep-error")

	out := buf.String()
	if strings.Contains(out, "drop-") {
		t.Errorf("level=error leaked lower levels: %q", out)
	}
	if !strings.Contains(out, "keep-error") {
		t.Errorf("error record missing: %q", out)
	}
}
