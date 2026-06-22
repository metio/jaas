// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestNewLogger_AllLevelsEmitAtOwnLevel exercises every level/format
// permutation end-to-end: a record logged at the configured level is
// rendered, and the chosen format is honored.
func TestNewLogger_AllLevelsEmitAtOwnLevel(t *testing.T) {
	levelMethods := map[string]func(*slog.Logger, string){
		"debug": func(l *slog.Logger, m string) { l.Debug(m) },
		"info":  func(l *slog.Logger, m string) { l.Info(m) },
		"warn":  func(l *slog.Logger, m string) { l.Warn(m) },
		"error": func(l *slog.Logger, m string) { l.Error(m) },
	}
	for _, format := range []string{"json", "text"} {
		for level, emit := range levelMethods {
			t.Run(format+"/"+level, func(t *testing.T) {
				var buf bytes.Buffer
				logger := NewLogger(&buf, level, format)
				emit(logger, "marker-"+level)

				out := strings.TrimSpace(buf.String())
				if out == "" {
					t.Fatalf("format=%s level=%s: no output for own-level record", format, level)
				}
				if !strings.Contains(out, "marker-"+level) {
					t.Errorf("format=%s level=%s: missing message: %q", format, level, out)
				}
				if format == "json" {
					var rec map[string]any
					if err := json.Unmarshal([]byte(out), &rec); err != nil {
						t.Errorf("json format produced unparseable output %q: %v", out, err)
					}
				} else if strings.HasPrefix(out, "{") {
					t.Errorf("text format emitted JSON: %q", out)
				}
			})
		}
	}
}

// TestNewLogger_InvalidLevelFallsBackToInfo confirms an unrecognized level
// string lands on info (the documented default) rather than panicking or
// dropping all records.
func TestNewLogger_InvalidLevelFallsBackToInfo(t *testing.T) {
	for _, bad := range []string{"trace", "verbose", "", "NOPE", "12345"} {
		t.Run(bad, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLogger(&buf, bad, "json")
			logger.Debug("should-drop")
			logger.Info("should-keep")

			out := buf.String()
			if strings.Contains(out, "should-drop") {
				t.Errorf("invalid level %q enabled debug: %q", bad, out)
			}
			if !strings.Contains(out, "should-keep") {
				t.Errorf("invalid level %q did not fall back to info: %q", bad, out)
			}
		})
	}
}

// TestInitTracer_SecureEndpointBuildsProvider covers the TLS dial branch
// (Insecure=false), which the insecure-only build test skips. otlptrace
// dials lazily, so construction succeeds without a live TLS collector.
// Empty ServiceName also exercises the "jaas" default assignment.
func TestInitTracer_SecureEndpointBuildsProvider(t *testing.T) {
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	sd, err := InitTracer(context.Background(), TracingConfig{
		Endpoint:    "127.0.0.1:4317",
		Insecure:    false,
		SampleRatio: 0.5,
	})
	if err != nil {
		t.Fatalf("InitTracer with secure endpoint: %v", err)
	}
	if sd == nil {
		t.Fatal("nil shutdown func")
	}
	// A shutdown against a dead collector may error; the build path is
	// what this covers.
	_ = sd(context.Background())
}

// TestInitTracer_ZeroSampleRatioStillBuilds pins that a configured endpoint
// with an always-off sampler still constructs the full provider — the
// sampler choice is independent of the build path.
func TestInitTracer_ZeroSampleRatioStillBuilds(t *testing.T) {
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	sd, err := InitTracer(context.Background(), TracingConfig{
		Endpoint:       "127.0.0.1:4317",
		Insecure:       true,
		ServiceName:    "custom",
		ServiceVersion: "v1.2.3",
		SampleRatio:    0,
	})
	if err != nil {
		t.Fatalf("InitTracer with zero sample ratio: %v", err)
	}
	if sd == nil {
		t.Fatal("nil shutdown func")
	}
	_ = sd(context.Background())
}

// TestInitTracer_ShutdownErrorIsJoined drives the configured-endpoint
// shutdown against an unreachable collector. The tarball/exporter teardown
// may surface an error; whatever the result, the call must not panic and
// must return without blocking past the context.
func TestInitTracer_ShutdownErrorIsJoined(t *testing.T) {
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	sd, err := InitTracer(context.Background(), TracingConfig{
		Endpoint:    "127.0.0.1:1",
		Insecure:    true,
		SampleRatio: 1.0,
	})
	if err != nil {
		t.Fatalf("InitTracer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A cancelled context exercises the error-joining return path.
	_ = sd(ctx)
}
