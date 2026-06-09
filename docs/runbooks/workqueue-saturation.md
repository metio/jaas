<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Operator runbook: workqueue saturation

Linked from the `JaaSControllerWorkqueueDepthHigh` alert. Fires when the reconciler's workqueue holds more items than the configured threshold (default 50) for the alert window. Not tied to a `Reason` constant — workqueue depth is a controller-runtime signal, not a per-snippet status.

## Symptom

```text
ALERTS{alertname="JaaSControllerWorkqueueDepthHigh", controller="jsonnetsnippet"}
```

- New snippet writes settle slowly (status takes minutes to flip, not seconds).
- Existing snippets re-render later than `spec.interval` would suggest.
- `kubectl describe jsonnetsnippet` shows a stale `ObservedGeneration`.

## Cause

The operator is dequeuing reconciles slower than the API server enqueues them. Common causes, in observed-frequency order:

1. **Slow Publisher backend.** S3 throttling, a slow PVC, or a stalled object-store transaction — each reconcile blocks on the storage `Put`.
2. **API server pressure.** The cluster's apiserver is slow on `GET` / `UPDATE` (often during a control-plane upgrade or under heavy general load).
3. **Per-snippet rate-limiter exhaustion.** A flapping snippet eats its token-bucket budget; the controller's exponential backoff stretches the queue.
4. **A large fan-out from a single source watch.** One Flux source republishes and 100 snippets reference it; every snippet's reconcile lands in the queue at once.
5. **Webhook latency.** When `--enable-webhook` is on, every snippet write traverses the validating webhook. A wedged webhook (cert issue, slow tenant client) holds the apiserver's call open and indirectly enlarges the queue.

## Diagnosis

```shell
# Per-controller queue depth — confirm which controller is saturated
kubectl -n <jaas-ns> port-forward svc/jaas-metrics 8083:8083 &
curl -s localhost:8083/metrics | grep -E 'workqueue_depth|workqueue_adds_total'

# Reconcile-time histogram — separates "lots of queued items" (fan-out)
# from "each reconcile is slow" (storage / apiserver).
curl -s localhost:8083/metrics | grep 'controller_runtime_reconcile_time_seconds'
```

Cross-reference operator logs for the slow path:

```shell
kubectl -n <jaas-ns> logs deploy/jaas --tail=500 \
  | grep -E 'reconcile|publisher|s3|webhook'
```

If `controller_runtime_reconcile_time_seconds` p99 is also high, the alert is the symptom — `JaaSReconcileLatencyHigh` is the more useful page; see [reconcile-latency.md](reconcile-latency.md).

## Remediation

- **Storage backend slow.** Switch from `local` (PVC) to `s3` for higher write throughput, or vice versa if S3 is throttled. See [storage-recovery.md](storage-recovery.md).
- **Apiserver slow.** Pause spec-update churn (`spec.interval` longer on hot snippets), then wait for control-plane health to return.
- **Rate-limiter exhaustion.** Increase `operator.rerenderBurst` to absorb the spike, then investigate why a snippet is flapping (typically a `Reason*` other than `Synced` keeps firing — check `kubectl get events`).
- **Fan-out from a single source.** Stagger snippet intervals so their watch events don't all settle at once. The controller serializes per-snippet; concurrency across snippets is bounded by `MaxConcurrentReconciles` (set high enough at chart default — 5 — that drag from a single fan-out is unusual).
- **Webhook latency.** `kubectl get validatingwebhookconfiguration jaas-jsonnetsnippet -o yaml` and confirm the `caBundle` is current; restart the operator if the cert was rotated externally.

## Prevention

- Run `operator.metrics.prometheusRule.enabled: true` so this alert fires *before* downstream consumers notice.
- Cap `--max-artifact-bytes` so a runaway snippet can't slow every Publisher write behind it.
- For multi-replica HA, leader election keeps only one replica reconciling — workqueue depth on the lease-holder is the only one that matters.
