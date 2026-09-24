---
title: Operations
description: Day-two operational tasks for a running JaaS install — graceful shutdown, leader election, storage GC, and upgrades.
tags: [installation, operations, maintenance, upgrades]
---

Day-two operations for a running JaaS install. Initial install and hardening
decisions are in [Kubernetes](/get-started/kubernetes/) and
[Production](/running/production/).

## Graceful shutdown and drain

When Kubernetes sends `SIGTERM`, JaaS executes a two-phase shutdown to avoid
dropping in-flight requests:

1. The readiness probe flips to `false` (`503` on `/ready`). Kubernetes
   endpoint controllers begin deregistering the pod from Services.
2. JaaS waits for `--shutdown-delay` (default `5s`) before closing its
   listeners. This window lets the endpoint propagation complete so no new
   traffic arrives after the server closes.
3. After the delay, the servers shut down gracefully with a 30-second
   `context.WithTimeout`. The operator goroutine is also cancelled and awaited
   within the same 30-second window.

The distroless runtime image has no `sleep` binary, so the drain delay is
implemented in the binary rather than via a `preStop` hook. A second
`SIGTERM` (or `SIGINT`) during the drain cuts the wait short.

To disable the drain (zero delay):

```shell
--shutdown-delay 0
```

The chart value is `arguments.shutdownDelay`.

## Leader election during rolling updates

In operator mode, leader election is on by default (`--leader-election`,
`operator.leaderElection.enabled: true`). The chart sets
`LeaderElectionReleaseOnCancel: true`, so when the old pod receives `SIGTERM` it
releases the lease immediately instead of waiting out the 15-second
`LeaseDuration`. The new pod picks up the lease within milliseconds.

Snippets that were `Ready=True` before the restart stay in that condition via
cached state. A new pod that takes over as leader reconciles them on the next
watch event. If snippets remain degraded for more than a few seconds after a
restart, check the [operator-watch-silent](/runbooks/operator-watch-silent/)
runbook — it diagnoses the case where the operator's own ClusterRole is missing
a verb so controller-runtime's informer silently fails to start.

To force-restart the operator (e.g. after an upgrade):

```shell
kubectl rollout restart deployment/jaas --namespace jaas-system
```

## Artifact retention and storage GC

Three independent mechanisms govern how long artifacts stay on disk (or in S3).
Full storage backend configuration is in [Storage and HA](/operator/storage-and-ha/).

### GC grace window (`--artifact-gc-grace`, default `5m`)

When a snippet is re-rendered, the superseded revision drops out of the keep-set
but remains fetchable for `--artifact-gc-grace` after supersession. This closes
the pin→fetch race in which a Flux consumer reads `status.artifact.url` a moment
before the operator GC-prunes the old tarball. The window survives operator
restarts — supersession time is derived from on-disk storage metadata, not from
in-memory state.

Set `0` to restore eager pruning (one revision at a time, matching stock Flux
source-controller semantics). Tune lower when storage capacity is tight and all
consumers are in-cluster.

### History retention (`spec.history`, default `1`, max `50`)

Per-snippet deliberate retention for rollback and blue-green flows. A downstream
consumer can pin to a specific `sha256` digest indefinitely as long as that
revision is within the history keep-set. This is separate from the GC grace
window — it is explicit operator intent, not a race-protection mechanism.

### Orphaned `.tmp` sweep (`--storage-sweep-interval`, `--storage-sweep-max-tmp-age`)

A `Put` that dies between writing the tempfile and the atomic rename leaves a
`<rev>.tar.gz.tmp` residue. The sweep goroutine runs on a ticker (default every
`10m`) and removes `.tmp` files older than `--storage-sweep-max-tmp-age`
(default `30m`). The age floor ensures live writers are never raced.

Set `--storage-sweep-interval 0` to disable the sweep entirely. The
`jaas_storage_sweep_failures_total` Prometheus counter signals failing sweep
passes.

## Finalizer teardown and the WithdrawForced safety valve

Every `JsonnetSnippet` holds a finalizer (`jaas.metio.wtf/finalizer`) that
blocks Kubernetes garbage collection until the operator successfully calls
`Publisher.Withdraw` to remove the artifact from storage. If the backend is
permanently unavailable (S3 down, RBAC revoked, bucket deleted), the finalizer
would otherwise hold the snippet — and by extension its namespace — in
`Terminating` forever.

`--max-withdraw-wait` (default `1h`) bounds how long the finalizer can hold.
Once the deadline passes, the operator:

1. Emits a `Warning WithdrawForced` Kubernetes Event on the snippet.
2. Drops the finalizer so the snippet can be garbage-collected.

The trade-off is a possible orphan tarball in storage. Recover it using the
[storage-recovery](/runbooks/storage-recovery/) runbook.

Adjust the bound with the chart value `operator.maxWithdrawWait`. Lower it in
environments where namespace teardown latency is critical; raise it (or remove
the concern by fixing the backend) in environments where artifact-safety is
paramount.

## Upgrades

Calendar-based releases ship every Monday. The chart version and the binary
version advance together.

```shell
helm upgrade --install jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system \
  --values my-values.yaml \
  --wait --timeout 5m
```

The chart ships CRDs under `templates/` so `helm upgrade --install` applies schema
changes automatically.

**Before each upgrade**, read
[Upgrading](/reference/upgrading/):

- Releases that change `spec.selector.matchLabels` on the Deployment require a
  manual `kubectl delete deployment/jaas` first — that field is immutable and
  `helm upgrade --install` will fail otherwise.
- The pre-delete cleanup Job (`operator.cleanupOnDelete.enabled: true`, the
  default) runs on `helm uninstall` and drops every snippet's finalizer so
  `ExternalArtifact` resources are unwound before the operator pod is removed.
  If the cleanup Job hangs, check `operator.cleanupOnDelete.kubectlTimeout`
  (default `2m`) and the backend health.

## Degraded operator

The operator subsystem is supervised independently of the rest of the process.
When the manager cannot be built or started — an apiserver the pod cannot reach,
a ClusterRole missing a verb, a CRD not yet installed — JaaS keeps running and
retries the manager with exponential backoff (1s, doubling, capped at 5m) for as
long as the pod lives. The Jsonnet renderer and the artifact server need no
cluster at all, so they keep serving throughout, and the manager comes up on its
own once the cause is cleared, in the same process and without a restart. A
manager that lasted at least a minute before failing gets a fresh backoff; one
that fails quickly keeps the growing delay, so an operator that cannot stay up
does not hammer the apiserver.

Three signals report the state:

- `GET /operator` on the management port returns `200` while the operator is
  reconciling, and `503` with the reason, the time the reading last changed, and
  the number of manager starts while it is not.
- `jaas_operator_available` is `0` while the operator is down and `1` once its
  cache has synced. `jaas_operator_start_failures_total` separates one long
  outage from a manager that keeps dying.
- The log carries one `Operator unavailable, restarting` line per fresh cause and
  a `still unavailable` line per retry, each naming the reason. The backoff
  paces them, so a long outage thins out to one line every five minutes.

Readiness stays down for a pod whose operator has never synced, which stops a
rolling update from replacing a working replica with one that cannot reconcile. A
manager that dies later does not withdraw a pod that has been serving. Where the
renderer matters more than that rollout gate,
`--readiness-requires-operator=never` ties readiness to the HTTP listeners alone,
leaving `/operator` and the gauge as the operator's own signals.

Liveness is unconditional either way, so a degraded operator never restarts the
pod. Diagnosis is in the
[operator-unavailable runbook](/runbooks/operator-unavailable/).

## Monitoring operational health

Key signals to watch:

- `jaas_operator_available == 0` — the operator is not reconciling at all; see
  [Degraded operator](#degraded-operator).
- `jaas_storage_sweep_failures_total` — non-zero means the sweep goroutine is
  erroring; investigate storage backend health.
- `jaas_snippet_reconcile_total{status!="Synced"}` — elevated rate means
  snippets are failing to render; cross-reference with the `reason` label and
  the relevant runbook.
- `JaaSControllerWorkqueueDepthHigh` PrometheusRule alert — workqueue is backing
  up; the operator cannot keep up with the reconcile rate.
- `/ready` probe on the management port (default `8081`) — `503` after startup
  means the manager has not yet been elected or its cache has not synced.

All metrics are documented in [Observability](/observability/). All shipped
alerts link to [Runbooks](/runbooks/).

## Next steps

- [Scale and capacity](/running/scale-and-capacity/) — replica sizing,
  the leader-election HA model, and the throughput knobs.
- [Configuration reference](/reference/configuration/) — the full flag
  list with defaults and chart value equivalents.
- [Runbooks](/runbooks/) — incident response procedures keyed to each
  `Ready` condition `Reason`.
