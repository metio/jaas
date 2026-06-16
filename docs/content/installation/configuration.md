---
title: Configuration reference
description: Complete reference for every JaaS command-line flag, organized by subsystem, with defaults and chart value equivalents.
tags: [installation, configuration, flags, reference]
---

Every JaaS flag is listed here with its default and a one-line description. Run
`jaas --help` to see the same list at runtime. The tables on this page are
generated from the binary's own flag definitions, so they never drift from the
runtime contract.

The Helm chart exposes most flags under `arguments.*`; operator-specific flags
are under `operator.*`. The full set of chart values is in the
[Helm chart values](/installation/helm-values/) reference.

## Jsonnet server

The Jsonnet server evaluates snippets and returns JSON. It binds on
`--listen-address:--port` by default.

{{< flag-table group="Jsonnet server" >}}

## Management server

The management server exposes the three Kubernetes probe endpoints. It binds on
`--management-listen-address:--management-port`.

{{< flag-table group="Management server" >}}

Endpoints: `GET /start` (startup probe), `GET /ready` (readiness probe),
`GET /live` (liveness probe). Startup and readiness return `503` with a
`{"status":"…"}` JSON body when the server is not yet ready. Liveness is an
unconditional `200`.

## Snippets and libraries

Flags for declaring the Jsonnet files the server serves.

{{< flag-table group="Snippets and libraries" >}}

Snippet name resolution uses Go's `os.OpenRoot`, which rejects `..` traversal
and symlinks that escape the configured directory. This is security-critical;
see [Evaluation and security](/usage/evaluation-and-security/).

## External variables

{{< flag-table group="External variables" >}}

**Environment variable alternative:** set `JAAS_EXT_VAR_<NAME>=<VALUE>` to
expose `<NAME>` as an external variable. The `--ext-var` flag overrides the env
mechanism on key conflict. See
[External variables and TLAs](/usage/external-variables-and-tlas/) for usage
examples.

## Evaluation limits

{{< flag-table group="Evaluation limits" >}}

`--evaluation-timeout` fires the HTTP response but does not terminate the
underlying go-jsonnet call — the evaluation continues consuming CPU until it
finishes naturally. Size container resources accordingly and use
`--max-concurrent-evals` to bound worst-case goroutine pile-up. See
[Evaluation and security](/usage/evaluation-and-security/) for the full
discussion.

## Lifecycle

{{< flag-table group="Lifecycle" >}}

## Operator (Flux integration)

The following flags are only active when `--enable-flux-integration` is set.

{{< flag-table group="Operator (Flux integration)" >}}

**Environment variable:** `JAAS_WATCH_NAMESPACES` — comma-separated namespace
list. Superseded by `--watch-namespaces` when both are set.

## Storage server (local and S3)

The storage server is the HTTP file server that downstream Flux consumers fetch
artifacts from. It is started only when `--enable-flux-integration` is set.

{{< flag-table group="Storage server (local and S3)" >}}

### S3 flags

Active only when `--storage-backend=s3`.

{{< flag-table group="S3 flags" >}}

## Webhook (TLS provisioning)

Active only when `--enable-webhook` is set (which also requires
`--enable-flux-integration`).

{{< flag-table group="Webhook (TLS provisioning)" >}}

See [Admission webhook](/usage/admission-webhook/) for the full `failurePolicy`
trade-off and cert rotation details.

## Leader election

{{< flag-table group="Leader election" >}}

## Observability

### Metrics

{{< flag-table group="Metrics" >}}

### Tracing

{{< flag-table group="Tracing" >}}

## Logging and lifecycle

{{< flag-table group="Logging and lifecycle" >}}
