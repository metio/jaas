---
title: Scale and capacity
description: Size and tune a JaaS install for your workload — replica CPU and memory, the leader-election HA model, the throughput knobs, and the saturation signals that tell you when you've run out of headroom.
tags: [installation, scale, capacity, ha, performance, tuning]
---

Size and tune JaaS for the snippet workload you run. This page covers how a single
replica's CPU and memory scale with evaluation load, what adding replicas does (and
doesn't) buy you, the knobs that govern throughput, and the metrics that tell you
when you're at capacity.

## Sizing a replica

A replica's resource use is driven by two things: **Jsonnet evaluation** (CPU) and
the **per-fetch byte budget plus eval working set** (memory).

**CPU is evaluation.** Each in-flight evaluation pins roughly one CPU for its
working set, and go-jsonnet has no mid-evaluation cancellation — once an evaluation
starts it runs to completion or to its memory ceiling, even after
`--evaluation-timeout` (default `5s`) fires on the parent. Provision CPU for the
worst-case concurrent eval load, not the average.

**Memory has a bounded worst case.** Resident memory during a publish is
approximately:

```text
concurrent fetches × fetch byte budget  +  concurrent evals × per-eval working set
```

The fetch byte budget is bounded by internal caps in the source fetcher (these are
**not** flags — they are fixed constants, not operator-tunable):

| Cap | Value | Bounds |
|---|---|---|
| `MaxArchiveBytes` | 64 MiB | the compressed download body |
| `MaxExtractedBytes` | 64 MiB | the sum of every extracted entry held in memory |
| `MaxPerEntryBytes` | 16 MiB | any single tar entry |
| `MaxDecompressedBytes` | 512 MiB | the gzip stream's decompressed output (gzip-bomb defence) |

Concurrent downloads are themselves capped (4 in-flight), so the peak
ephemeral-storage cost of in-flight downloads is bounded by that count × the 64 MiB
archive cap. The render output adds to this: cap it per snippet with
`--max-artifact-bytes` (default `0`, disabled — snippets that exceed it fail with
`ReasonArtifactTooLarge`), and watch the `jaas_snippet_rendered_bytes` histogram to
see the real distribution before you pick a value.

The chart defaults (64 MiB memory, 32m CPU) are fine for a quickstart but will OOM
under sustained rendering. A reasonable starting point for a production operator:

```yaml
resources:
  memory: 256Mi
  cpu: 100m

operator:
  storage:
    maxArtifactBytes: 16777216  # 16 MiB; snippets above this fail ReasonArtifactTooLarge
```

Raise the memory request toward `concurrent evals × largest expected artifact` plus
the fetch budget if you render large dashboards or run many tenants. See
[Production](/installation/production/) for the same sizing decision in the context
of a full hardening checklist.

## Replicas and HA

JaaS uses controller-runtime leader election, **on by default** whenever
`--enable-flux-integration` is set (`--leader-election`, `true`). The lease lives at
`--leader-election-namespace` / `--leader-election-id` (`jaas-operator`). The lease
is released on shutdown (`LeaderElectionReleaseOnCancel`), so a rolling update hands
off in seconds rather than waiting out the lease duration.

What scales by adding replicas, and what does not:

| Component | Runs on | Scales with replicas |
|---|---|---|
| Jsonnet render HTTP server | every replica | Yes — render throughput is per-pod |
| Management probes (`/live`, `/ready`, `/start`) | every replica | Yes |
| Storage HTTP server (artifact downloads) | every replica | Yes |
| Validating admission webhook | every replica | Yes |
| Reconcilers + the manager cache + storage writes | **leader only** | No — reconcile is single-leader |

Adding replicas raises HTTP render and download throughput and gives the webhook
and probes redundancy, but it does **not** parallelise reconciliation: only the
lease-holder reconciles and writes artifacts. The non-leaders stand by to take the
lease over on failure.

Because every replica serves artifact downloads but only the leader writes them,
**multi-replica installs require shared-readable storage**: either the `s3` backend
(`--storage-backend=s3` — all replicas read the same bucket, the leader writes) or
the `local` backend on an RWX PVC. The default `local` backend on an emptyDir or RWO
PVC is single-replica only. The backend matrix and the full HA setup live in
[Storage and HA](/usage/storage-and-ha/); pick a backend there before scaling out.

## Tuning throughput

Each knob below names what it controls, its default, when to move it, and the metric
that signals the need.

### `--max-concurrent-evals`

Global cap on in-flight Jsonnet evaluations across the HTTP and operator paths.
Default is `max(GOMAXPROCS × 4, 16)`. Excess work is rejected — HTTP requests get a
`503`, the operator requeues with backoff. `0` disables the gate entirely.

- **Raise it** when `jaas_eval_unavailable_total` climbs while CPU still has
  headroom and evals are short — you're rejecting work you could run.
- **Lower it** when the pod approaches its memory limit under load — fewer
  concurrent evals means a smaller worst-case working set.
- **Watch** `jaas_eval_in_flight` against `jaas_eval_max_concurrent`: in-flight
  pegged at the cap while `jaas_eval_unavailable_total` grows is saturation.

### `--max-stack`

Maximum Jsonnet call-stack depth, default `500`; `0` falls back to go-jsonnet's own
default. Raise only for legitimately deep recursive templates that hit the limit;
keep it bounded so a runaway recursive snippet can't exhaust the stack.

### `--evaluation-timeout`

Maximum wall-clock time a single evaluation may take, default `5s`; `0` disables it.
Lower it to fail pathological snippets faster; raise it for genuinely expensive
renders. Because evaluation is uncancellable, a timed-out parent leaves the eval
goroutine running until it finishes naturally — watch
`jaas_eval_outstanding_timed_out`, which counts exactly those orphans. A sustained
non-zero value means your timeout is shorter than some snippet's real cost.

### `--rerender-rate` and `--rerender-burst`

The per-snippet token bucket that bounds steady-state re-render churn. Defaults are
`60/min` (rate, as `N/period` where period is `sec|min|hour`) and `120` (burst
depth). Raise them for snippets whose upstream sources legitimately change faster
than the budget; lower them to throttle a snippet whose update cadence is hammering
the operator. The `jaas_snippet_rate_limited_total` counter (paired with a
`RateLimited` Warning event) tells you when a snippet is hitting its bucket.

### `spec.interval` and `--storage-sweep-interval`

A snippet's `spec.interval` sets how often the operator re-renders it absent a watch
event — lengthen it for snippets that rarely change to cut reconcile load.
`--storage-sweep-interval` (default `10m`; `0` disables) governs the background GC
that clears orphaned `.tar.gz.tmp` residue; the sweep is off the hot reconcile path,
so leave it unless `jaas_storage_sweep_failures_total` is climbing.

## Knowing when you're at capacity

Watch these saturation signals; each maps to a starter alert in the opt-in
`PrometheusRule` (see [Alerting](/observability/alerting/)):

| Signal | At capacity when | Alert |
|---|---|---|
| `jaas_eval_in_flight` vs `jaas_eval_max_concurrent` | in-flight pegged at the cap | `JaaSEvalSaturation` (ratio ≥ 0.9 for 10m, guarded on the cap being non-zero) |
| `jaas_eval_unavailable_total` | climbing — evals are being rejected | (component of eval-saturation diagnosis) |
| `jaas_eval_outstanding_timed_out` | sustained non-zero — evals outrun the timeout | watch directly |
| `controller_runtime` workqueue depth | can't drain | `JaaSControllerWorkqueueDepthHigh` (depth > 50 for 15m) |
| reconcile p95 latency | crosses the SLO ceiling | `JaaSReconcileLatencyHigh` |

These signals, the dashboard that plots them, and the reconcile SLOs are detailed
under [Observability](/observability/) — start with [Metrics](/observability/metrics/),
[Alerting](/observability/alerting/), and the [SLOs](/observability/slos/).

To get a throughput baseline for **your** hardware before sizing, run the in-repo
benchmarks: `BenchmarkReconcile_HappyPath` / `BenchmarkReconcile_Concurrent` in
`internal/operator`, the `BenchmarkEvaluateAnonymousSnippet_*` evaluation
benchmarks in `internal/eval`, and `BenchmarkStore_Put` /
`BenchmarkStore_PutLargeArtifact` in `internal/storage`.

## Next steps

- [Storage and HA](/usage/storage-and-ha/) — pick the backend that lets you scale
  past one replica.
- [Production](/installation/production/) — the full pre-production hardening
  checklist.
- [Observability](/observability/) — the metrics, alerts, dashboard, and SLOs that
  back the capacity signals above.
