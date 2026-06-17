---
title: Observability
description: The Prometheus metrics endpoint and custom jaas_ metrics, the opt-in ServiceMonitor and PrometheusRule alerts, Kubernetes Events with Flux notification routing, and OTLP tracing.
tags: [operator, metrics, tracing, alerts]
---

The JaaS operator exposes Prometheus metrics, Kubernetes Events, and OTLP
traces. Scrape the metrics endpoint for dashboards and alerts, route the
operator's Events through Flux's notification-controller, and ship traces to an
OpenTelemetry collector.

## Metrics endpoint

controller-runtime's Prometheus endpoint binds `--metrics-bind-address` (default
`:8083`), serving the standard text exposition format at `/metrics`. Setting it
to `0` disables the endpoint. The default deliberately avoids
controller-runtime's built-in `:8080`, which would collide with the Jsonnet HTTP
port.

### Scraping

The Helm chart renders a dedicated `jaas-metrics` Service in the release
namespace whenever the operator is enabled. Two ways reach it:

A `ServiceMonitor` for the Prometheus Operator — opt-in via
`operator.metrics.serviceMonitor.enabled`. It selects the `jaas-metrics` Service
and scrapes its `metrics` port at `/metrics`:

```yaml
operator:
  enabled: true
  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      interval: 30s
      scrapeTimeout: 10s
      # Labels your Prometheus instance selects ServiceMonitors on.
      labels:
        release: kube-prometheus
```

Without the Prometheus Operator, point a plain Prometheus scrape config at the
`jaas-metrics` Service (port `8083`, path `/metrics`), or add the usual
`prometheus.io/scrape` annotation set to the pod through `podAnnotations` and let
a kubernetes-pods scrape job discover it.

### Metrics reference

The operator exports these custom `jaas_*` metrics, registered against
controller-runtime's registry so they ride the same `/metrics` endpoint:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `jaas_snippet_reconcile_total` | counter | `namespace`, `name`, `status`, `reason` | One bump per reconcile that touches the Ready condition. `status` is `True`/`False`; `reason` is the Reason constant from the snippet's condition. |
| `jaas_snippet_rendered_bytes` | histogram | `namespace`, `name` | Rendered artifact size, observed only on `Synced` reconciles. Buckets run 256 B…64 MiB. |
| `jaas_snippet_rate_limited_total` | counter | `namespace`, `name` | Reconciles deferred by the per-snippet token bucket. Paired with the `RateLimited` Warning event. |
| `jaas_snippet_eval_unavailable_total` | counter | `namespace`, `name` | Reconciles deferred because the global concurrent-eval cap was full. Paired with the `EvalUnavailable` Warning event. |
| `jaas_snippet_force_drop_total` | counter | `namespace`, `name`, `reason` | Snippets whose finalizer was force-dropped because `Publisher.Withdraw` kept failing past `--max-withdraw-wait` or hit a permanent API error. `reason` names the trigger (`withdraw_timed_out`, `tenant_client_permanent`, `withdraw_permanent`). Sustained non-zero values mean orphaned tarballs are accumulating; see the [storage-recovery runbook](/runbooks/storage-recovery/). |
| `jaas_eval_in_flight` | gauge | — | Evaluations currently holding a slot in the global concurrent-eval semaphore. Reads through to the live count on every scrape. |
| `jaas_eval_max_concurrent` | gauge | — | Configured ceiling of the semaphore (`--max-concurrent-evals`). Zero means the gate is disabled — any saturation alert must guard on this being non-zero. |
| `jaas_eval_unavailable_total` | counter | — | Process-global accumulator of evaluations the semaphore rejected, across the HTTP and operator paths. Monotonic; resets on restart. |
| `jaas_eval_outstanding_timed_out` | gauge | — | Evaluation goroutines whose parent's context fired before the synchronous go-jsonnet call returned. Sustained non-zero readings flag a runaway snippet. |
| `jaas_storage_sweep_failures_total` | counter | — | Background storage-sweep passes that returned an error. The sweep removes orphaned `.tar.gz.tmp` residue; failures here don't block reconciles but let stale files accumulate. |
| `jaas_webhook_cert_renewal_failures_total` | counter | — | Self-signed cert renewal attempts that returned an error. Sustained non-zero values flag RBAC drift or a write-permission loss on `--webhook-cert-dir`; the existing cert's natural expiry is the deadline before admission breaks cluster-wide. |
| `jaas_tenant_token_mint_failures_total` | counter | `namespace`, `serviceAccount` | `TokenRequest` mints that returned an error. Sustained non-zero values on a pair indicate revoked `serviceaccounts/token: create` or a deleted namespace; affected snippets pin Ready=Unknown. |
| `jaas_crd_watch_engagement_failures_total` | counter | `gvk` | `EngageFluxWatch` calls that returned an error. Sustained non-zero values on a GVK mean dependent snippets won't re-render on upstream source events until the watch engages. |

The eval gauges (`jaas_eval_in_flight`, `jaas_eval_max_concurrent`,
`jaas_eval_outstanding_timed_out`) reflect the global concurrent-eval cap; see
[evaluation and security](/usage/evaluation-and-security/) for how that cap works
and how to size `--max-concurrent-evals`.

Alongside these, controller-runtime contributes its standard families for free —
`controller_runtime_reconcile_total`, `controller_runtime_reconcile_time_seconds`,
the `workqueue_*` series (depth, latency, retries), and the Go/process
collectors. The shipped alerts below build on both the `jaas_*` metrics and these
controller-runtime signals.

## Alerts

`operator.metrics.prometheusRule.enabled` renders a `PrometheusRule` with a
starter alert set on the custom metrics plus a handful of controller-runtime
signals. It needs the Prometheus Operator's `monitoring.coreos.com/v1` API in the
cluster.

```yaml
operator:
  metrics:
    prometheusRule:
      enabled: true
      interval: 30s
      # Labels your Prometheus instance selects PrometheusRules on.
      labels:
        release: kube-prometheus
      # Merged onto every rendered alert — route all jaas alerts
      # through one Alertmanager receiver.
      extraAlertLabels:
        team: platform
```

Every threshold is a knob under `operator.metrics.prometheusRule.thresholds`, so
the noise floor is tunable without copy-pasting rule bodies. The shipped alerts
and their default thresholds:

| Alert | Severity | Fires when | Threshold knobs (default) |
|---|---|---|---|
| `JaaSSnippetReconcileErrorsHigh` | warning | A snippet keeps flipping to Ready=False (excluding `Synced`/`Suspended`/`Pending`). | `reconcileErrorRate` (0.1/s), `reconcileErrorDuration` (10m) |
| `JaaSSnippetArtifactGrowing` | warning | p99 `jaas_snippet_rendered_bytes` exceeds the size ceiling. | `artifactSizeBytes` (16 MiB), `artifactSizeDuration` (30m) |
| `JaaSControllerWorkqueueDepthHigh` | warning | The `jsonnetsnippet` workqueue can't drain. | `workqueueDepth` (50), `workqueueDuration` (15m) |
| `JaaSReconcileLatencyHigh` | warning | p99 reconcile time crosses the ceiling. | `reconcileLatencySeconds` (30), `reconcileLatencyDuration` (15m) |
| `JaaSOperatorPodDown` | critical | A jaas pod stays NotReady. | `podDownDuration` (5m) |
| `JaaSStorageSweepFailures` | warning | Background sweeps fail per hour above the floor. | `sweepFailuresPerHour` (3), `sweepFailuresDuration` (30m) |
| `JaaSWebhookCertRenewalFailing` | critical | Self-signed cert renewal fails per hour above the floor. | `webhookCertRenewalFailuresPerHour` (1), `webhookCertRenewalFailuresDuration` (30m) |
| `JaaSTenantTokenMintFailing` | warning | Token mints fail for a `(namespace, serviceAccount)` pair. | `tenantTokenMintFailureRate` (0.01/s), `tenantTokenMintFailureDuration` (10m) |
| `JaaSForceDropsAccumulating` | warning | Snippet finalizers are force-dropped per hour above the floor. | `forceDropsPerHour` (0), `forceDropsDuration` (5m) |
| `JaaSCRDWatchEngagementFailing` | warning | A Flux source watch won't engage for a GVK. | `crdWatchEngagementFailuresPerHour` (1), `crdWatchEngagementFailuresDuration` (30m) |
| `JaaSEvalSaturation` | warning | In-flight evals exceed the saturation ratio of the cap (guarded on the cap being non-zero). | `evalSaturationRatio` (0.9), `evalSaturationDuration` (10m) |
| `JaaSEvalRejected` | warning | The semaphore turns evals away per second above the floor. | `evalRejectedRate` (0.05/s), `evalRejectedDuration` (10m) |
| `JaaSEvalLeakedGoroutines` | warning | Orphan eval goroutines persist above the floor — a runaway snippet. | `evalLeakedFloor` (0), `evalLeakedDuration` (5m) |

To silence a built-in alert, raise its threshold to an impossibly high value —
there is no per-alert disable toggle, and the threshold pattern keeps "this alert
is intentionally inert" visible in the chart values. Cluster-specific rules
append under a separate group via `operator.metrics.prometheusRule.extraRules`.

## Runbooks

Each alert and each Ready-condition reason maps to a remediation page under
[`/runbooks/`](/runbooks/). The shipped alerts carry the page as a `runbook_url`
annotation (the key is configurable via
`operator.metrics.prometheusRule.runbookAnnotationKey`), so Alertmanager renders
a direct link.

The operator threads the same links into its own status automatically: every
actionable Ready-condition Message gains a
`(runbook: https://jaas.projects.metio.wtf/runbooks/<reason>/)` suffix, so
`kubectl describe jsonnetsnippet` points straight at the matching page. Healthy
or intentional states (`Synced`, `Suspended`, `Pending`) get no suffix.

## Kubernetes Events and notifications

The operator emits a standard Kubernetes `Event` on every Ready-condition
transition — `Normal` for `Synced`, `Warning` for every other reason. The reason
string fills both the event `reason` and `action`.

Routing is Flux's `notification-controller`: target an `Alert` CR at
`kind: JsonnetSnippet` and JaaS needs no `Provider`/`Alert` plumbing of its own.

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Alert
metadata:
  name: jaas-snippets
  namespace: flux-system
spec:
  providerRef:
    name: slack
  eventSeverity: warn   # 'info' to include success events
  eventSources:
    - kind: JsonnetSnippet
      name: '*'
```

Wire whatever `Provider` you already use for Flux source CRs; see the
[Flux notification-controller documentation](https://fluxcd.io/) for provider
configuration.

## Tracing

The operator exports OpenTelemetry traces over OTLP gRPC:

- `--tracing-endpoint` — the OTLP gRPC collector host:port, e.g.
  `otel-collector.observability.svc:4317`. Empty disables tracing entirely.
- `--tracing-insecure` — skip TLS when dialing the collector. Use only for
  in-cluster collectors that do not terminate TLS themselves.
- `--tracing-sample-ratio` — TraceID-ratio sampling between `0.0` and `1.0`
  (default `1`, every trace sampled).

The full flag list with defaults is on the
[configuration page](/installation/configuration/).
