---
title: Tracing
description: The JaaS operator exports OpenTelemetry traces over OTLP gRPC. Pointing it at a collector, sampling, viewing spans, and the Helm chart keys that drive it.
tags: [operator, tracing, observability]
---

The JaaS operator exports OpenTelemetry traces over OTLP gRPC. With an endpoint
configured, each reconcile and the work it fans out into — source fetch, library
resolution, evaluation, publish — becomes a span you can follow in a tracing
backend. When no endpoint is set, the OpenTelemetry SDK runs in no-op mode and
emits nothing, so tracing carries no cost until you opt in.

## The binary

Three flags configure the exporter:

- `--tracing-endpoint` — the OTLP gRPC collector `host:port`, e.g.
  `otel-collector.observability.svc:4317`. Empty (the default) disables tracing
  entirely.
- `--tracing-insecure` — skip TLS when dialing the collector. Default `false`.
  Use only for in-cluster collectors that do not terminate TLS themselves.
- `--tracing-sample-ratio` — TraceID-ratio sampling between `0.0` and `1.0`.
  Default `1.0` samples every trace.

The full flag list with defaults is on the
[configuration page](/installation/configuration/).

### Viewing spans

Point `--tracing-endpoint` at any OTLP-gRPC-speaking collector — the
OpenTelemetry Collector, Jaeger, Tempo, or a vendor agent — and view the spans
in whatever backend that collector feeds. A reconcile span carries the snippet's
namespace and name plus the spec generation it acted on
(`jaas.generation`), so you can search for one snippet and see the latency
breakdown across its fetch, eval, and publish phases. That is the fastest way to
tell a slow upstream source fetch apart from a slow evaluation when a snippet's
reconcile latency climbs.

When a phase fails — source resolution, library resolution, evaluation, or
publish — its span records the error and is marked with an error status, so the
failed span shows up as `status=error` and is directly queryable in the tracing
backend. Searching for error spans surfaces the exact phase a failing reconcile
broke in without reading through the full trace.

On a busy operator, drop `--tracing-sample-ratio` below `1.0` to keep only a
fraction of traces — `0.1` samples one in ten. Leave it at `1.0` while
diagnosing a specific problem so no trace is dropped.

## The Helm chart

Tracing lives under `operator.tracing`. It only takes effect in operator mode
(`operator.enabled: true`):

```yaml
operator:
  enabled: true
  tracing:
    endpoint: otel-collector.observability.svc:4317
    insecure: true
    sampleRatio: 1.0
```

The keys map directly onto the flags: `endpoint` → `--tracing-endpoint`,
`insecure` → `--tracing-insecure`, `sampleRatio` → `--tracing-sample-ratio`.
Leaving `endpoint` empty (the default) keeps the SDK in no-op mode.
