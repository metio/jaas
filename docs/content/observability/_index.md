---
title: Observability
description: How to watch JaaS in production — structured logs, OTLP traces, Prometheus metrics, and the shipped alert catalog with Kubernetes Events and Flux notification routing.
tags: [operator, logging, tracing, metrics, alerts]
---

JaaS gives you four ways to see what it is doing in a cluster. Structured logs
tell you what happened on a single request or reconcile; traces follow one
operation across its spans; metrics aggregate behaviour into time series for
dashboards and alerts; and alerts plus Kubernetes Events turn a sustained
problem into a page or a notification.

Each pillar has its own page covering both the binary's flags and the Helm chart
keys that drive them:

- [Logging](/observability/logging/) — the `log/slog` logger, `--log-level` and
  `--log-format`, and reading JSON logs with `kubectl logs` and `jq`.
- [Tracing](/observability/tracing/) — OTLP gRPC export to an OpenTelemetry collector,
  sampling, and viewing spans.
- [Metrics](/observability/metrics/) — the Prometheus endpoint, the custom `jaas_*`
  metric family, scraping with a `ServiceMonitor`, and querying with PromQL.
- [Alerting](/observability/alerting/) — the opt-in `PrometheusRule` alert catalog with
  its runbook links, plus Kubernetes Events routed through Flux's
  notification-controller.

Logging applies to every mode JaaS runs in. Tracing, metrics, and alerting are
operator-mode concerns and take effect once `--enable-flux-integration` is set
(`operator.enabled` in the chart).
