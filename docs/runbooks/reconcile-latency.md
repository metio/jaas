---
title: High reconcile latency
description: Individual reconcile calls are taking longer than the configured p99 threshold, indicating slow source fetches, heavy evaluation, or a sluggish storage backend
tags: [runbooks, troubleshooting, metrics]
---

Linked from the `JaaSReconcileLatencyHigh` alert. Fires when the controller-runtime `controller_runtime_reconcile_time_seconds` histogram p99 exceeds the configured threshold (default 30s) for the alert window.

## Symptom

```text
ALERTS{alertname="JaaSReconcileLatencyHigh", controller="jsonnetsnippet"}
```

- `kubectl get jsonnetsnippet` shows status updates trickling in well after spec changes.
- Operator pod CPU is moderate-to-high but the queue is draining (distinguishes this from [workqueue-saturation.md](workqueue-saturation.md), where the queue itself is growing).

## Cause

Reconcile latency is the wall-clock cost of one `Reconcile()` call. Inside the call, JaaS does (in order):

1. `Get` the snippet from the cache.
2. Run the dependency-cycle BFS (one Get per touched node).
3. Resolve the source (inline files, or sourceRef → Fetcher: source-CR Get + tarball HTTP fetch + tar extract).
4. Resolve libraries (one Get per `LibraryRef`).
5. Evaluate the snippet via go-jsonnet.
6. Publish via the storage backend (`Put`).
7. Status update + ExternalArtifact upsert.

Slow reconciles are almost always one of:

- **Slow `Fetcher`** — a large tarball over a slow network, or a misbehaving source-controller (digest mismatch retries).
- **Heavy jsonnet evaluation** — a snippet that imports lots of large libraries or runs unbounded recursion below the stack limit.
- **Slow `Publisher`** — S3 throttling, a slow PVC, or large rendered output (close to `--max-artifact-bytes`).
- **Cycle-detection blowup** — a dense graph of snippets cross-referencing via `sourceRef`. The BFS is O(V+E) but each visit is a `Get`.

## Diagnosis

```shell
# Where is the time going? OTel spans break Reconcile into sub-stages.
# Requires --tracing-endpoint set on the operator.
kubectl -n <jaas-ns> get deploy jaas \
  -o jsonpath='{.spec.template.spec.containers[0].args}' \
  | tr ',' '\n' | grep tracing

# Without tracing: the histograms expose enough to triangulate.
kubectl -n <jaas-ns> port-forward svc/jaas-metrics 8083:8083 &
curl -s localhost:8083/metrics | grep -E 'reconcile_time|rendered_bytes'
```

The `jaas_snippet_rendered_bytes` histogram tells you whether a slow Publisher is the cause (large outputs) vs. a slow Fetcher (small outputs but the histogram is dominated by upstream IO).

For a single suspect snippet, force a reconcile under load and observe:

```shell
kubectl annotate jsonnetsnippet <ns>/<name> \
  jaas.metio.wtf/reconcile-at=$(date -u +%FT%TZ) --overwrite
kubectl -n <jaas-ns> logs deploy/jaas --tail=50 | grep <name>
```

## Remediation

- **Slow Fetcher.** Narrow `spec.sourceRef.path` to the subdirectory the snippet actually needs. Tarballs balloon when an entire monorepo is published; the filter trims what JaaS has to download.
- **Heavy eval.** Cap `--max-stack` to bound runaway recursion. Profile the snippet locally via `jsonnet` (the CLI) — the operator's evaluation is identical.
- **Slow Publisher.** See [storage-recovery.md](storage-recovery.md) for backend-specific tuning.
- **Cycle-detection blowup.** Reorganize snippets so the cross-reference graph is shallow; cycle detection visits every reachable node, so a fan-out of N snippets multiplies the cost.
- **OTel for forensics.** Enable `--tracing-endpoint` and the per-stage spans turn this from guessing into measurement. The chart values key is `operator.tracing.endpoint`.

## Prevention

- Pair the alert with `JaaSSnippetArtifactGrowing` ([artifacttoolarge.md](artifacttoolarge.md)). A snippet whose rendered bytes are climbing is almost always headed for a latency spike too.
- For multi-replica HA, leader election keeps only one replica in the reconcile loop — sustained latency on the lease-holder is what matters; standby latency is not measured.
