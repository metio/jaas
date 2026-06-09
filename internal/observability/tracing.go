/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package observability holds the OpenTelemetry tracer plumbing the
// operator opts into via --tracing-endpoint. Tracing is OFF unless the
// flag is set — JaaS has no opinion on whether you run a collector.
package observability

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.36.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// TracingConfig bundles the operator-facing tracing knobs main.go threads in.
type TracingConfig struct {
	// Endpoint is the gRPC host:port of an OTLP collector. Empty
	// disables tracing entirely — JaaS uses a no-op tracer and emits
	// no spans.
	Endpoint string

	// Insecure skips TLS when dialing the collector. Set for local
	// dev / in-cluster collectors that don't terminate TLS themselves.
	Insecure bool

	// ServiceName is the resource.service.name attribute attached to
	// every span. Defaults to "jaas" when empty.
	ServiceName string

	// ServiceVersion is the resource.service.version attribute. Filled
	// from main.version so spans cross-reference the release.
	ServiceVersion string

	// SampleRatio controls TraceID-ratio sampling (0.0..1.0). Zero or
	// negative pins always-off (no spans); >=1 pins always-on. Defaults
	// to 1.0 when unset and Endpoint is non-empty.
	SampleRatio float64
}

// Shutdown is returned by InitTracer; main.go defers it with a bounded
// context so a slow collector doesn't hold up termination.
type Shutdown func(ctx context.Context) error

// InitTracer wires the OTel SDK against the configured OTLP endpoint and
// registers a global tracer provider. When cfg.Endpoint is empty, a
// no-op tracer is installed so callers that call otel.Tracer() are
// always safe. The returned Shutdown flushes pending spans and tears
// down the provider.
func InitTracer(ctx context.Context, cfg TracingConfig) (Shutdown, error) {
	if cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "jaas"
	}
	if cfg.SampleRatio <= 0 {
		cfg.SampleRatio = 1.0
	}

	var dialOpts []otlptracegrpc.Option
	dialOpts = append(dialOpts, otlptracegrpc.WithEndpoint(cfg.Endpoint))
	if cfg.Insecure {
		dialOpts = append(dialOpts, otlptracegrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	} else {
		dialOpts = append(dialOpts, otlptracegrpc.WithDialOption(grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, ""))))
	}

	exp, err := otlptrace.New(ctx, otlptracegrpc.NewClient(dialOpts...))
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(cfg.ServiceName),
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersionKey.String(cfg.ServiceVersion))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRatio)),
	)
	otel.SetTracerProvider(tp)

	return func(shutdownCtx context.Context) error {
		// Tear down the tracer first (flush in-flight spans), then the
		// exporter. errors.Join keeps both errors visible.
		tpErr := tp.Shutdown(shutdownCtx)
		expErr := exp.Shutdown(shutdownCtx)
		return errors.Join(tpErr, expErr)
	}, nil
}

// Tracer returns a tracer scoped to the JaaS instrumentation package.
// Safe before InitTracer — the global tracer provider defaults to a
// no-op so the call cost is a function indirection, nothing more.
func Tracer() trace.Tracer {
	return otel.Tracer("github.com/metio/jaas")
}
