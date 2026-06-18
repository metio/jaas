---
title: Metrics
description: The controller-runtime Prometheus endpoint, the custom jaas_ metric family, scraping with a ServiceMonitor or a plain scrape config, querying with PromQL, and the Helm chart keys that drive it.
tags: [operator, metrics, prometheus, observability]
---

The JaaS operator exposes a Prometheus metrics endpoint covering controller-runtime's
standard families plus a custom `jaas_*` family the reconciler registers. Scrape
it for dashboards and feed it into the shipped [alerts](/observability/alerting/).

## The binary

controller-runtime's Prometheus endpoint binds `--metrics-bind-address` (default
`:8083`), serving the standard text exposition format at `/metrics`. Setting it
to `0` disables the endpoint. The default deliberately avoids controller-runtime's
built-in `:8080`, which would collide with the Jsonnet HTTP port.

The full flag list with defaults is on the
[configuration page](/installation/configuration/).

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
collectors. The shipped [alerts](/observability/alerting/) build on both the `jaas_*`
metrics and these controller-runtime signals.

### Querying with PromQL

Once scraped, a few PromQL queries answer the common questions:

```promql
# Rate of failed reconciles per snippet, excluding healthy/intentional states.
sum by (namespace, name) (
  rate(jaas_snippet_reconcile_total{status="False",reason!~"Synced|Suspended|Pending"}[5m])
)

# Eval semaphore saturation, guarded on the gate being enabled.
jaas_eval_in_flight / jaas_eval_max_concurrent and jaas_eval_max_concurrent > 0

# p99 rendered artifact size per snippet.
histogram_quantile(0.99, sum by (namespace, name, le) (rate(jaas_snippet_rendered_bytes_bucket[30m])))
```

## The Helm chart

The metrics port is set under `ports`, and the chart wires it to a dedicated
`jaas-metrics` Service whenever the operator is enabled:

```yaml
ports:
  # controller-runtime metrics endpoint; maps to --metrics-bind-address.
  # Set to 0 in operator.metrics.enabled to disable entirely.
  metrics: 8083
```

Scraping is configured under `operator.metrics`. A `ServiceMonitor` for the
Prometheus Operator is opt-in — it selects the `jaas-metrics` Service and scrapes
its `metrics` port at `/metrics`:

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
`prometheus.io/scrape` annotation set to the pod through `pod.additionalLabels`
and let a kubernetes-pods scrape job discover it.

To turn the alerts on, see [Alerting](/observability/alerting/).
