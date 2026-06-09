/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// A non-empty endpoint must build the full OTLP exporter + resource +
// sampler + provider without a live collector (otlptrace dials lazily, so
// construction succeeds against a dead address). Exercises the whole build
// path the empty-endpoint no-op tests skip.
func TestInitTracer_NonEmptyEndpointBuildsProvider(t *testing.T) {
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	sd, err := InitTracer(context.Background(), TracingConfig{
		Endpoint:       "127.0.0.1:4317",
		Insecure:       true,
		ServiceVersion: "test",
		SampleRatio:    1.0,
	})
	if err != nil {
		t.Fatalf("InitTracer with a non-empty endpoint: %v", err)
	}
	if sd == nil {
		t.Fatal("nil shutdown func")
	}
	// Shutdown may error flushing to a dead collector — the build path is
	// what we're covering, so a shutdown error is acceptable.
	_ = sd(context.Background())
}

func TestInitTracer_EmptyEndpointInstallsNoopProvider(t *testing.T) {
	shutdown, err := InitTracer(context.Background(), TracingConfig{})
	if err != nil {
		t.Fatalf("InitTracer: %v", err)
	}
	defer shutdown(context.Background())

	tp := otel.GetTracerProvider()
	if tp == nil {
		t.Fatal("global tracer provider is nil")
	}
	// Tracer() must always work — even with the no-op provider.
	_, span := Tracer().Start(context.Background(), "test")
	defer span.End()
	if span == nil {
		t.Fatal("Span returned nil with no-op tracer")
	}
}

func TestInitTracer_ShutdownIsNopWithEmptyEndpoint(t *testing.T) {
	shutdown, err := InitTracer(context.Background(), TracingConfig{})
	if err != nil {
		t.Fatalf("InitTracer: %v", err)
	}
	// Multiple shutdowns must remain safe — no panic on second call.
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("first shutdown returned %v, want nil", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("second shutdown returned %v, want nil", err)
	}
}
