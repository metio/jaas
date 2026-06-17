---
title: Watch-layer silent failure
description: The operator's own ClusterRole is missing a verb on a watched kind, so controller-runtime's informer retries silently and no snippet status reflects the problem
tags: [runbooks, troubleshooting, rbac]
---

Not tied to a per-snippet `Reason`. This page covers the one RBAC-denial path the reconciler cannot surface itself: when the **operator's own** ClusterRole is missing a verb on a watched resource kind, controller-runtime's informer fails to start its watch, logs warnings, and retries silently. The reconciler never sees the failure — and no snippet's status condition will tell you about it.

If a per-snippet runbook (`rbacdenied.md`, `sourcefetchfailed.md`, `sourcenotready.md`) doesn't match the symptoms, this is where to look next.

## Symptom

- Snippets that worked yesterday stop receiving watch-driven re-renders. They still reconcile on edits or `spec.interval` ticks, but not on upstream source changes.
- `kubectl describe jsonnetsnippet` shows healthy or stale state — never `Reason=RBACDenied` or any other failure.
- Operator pod is `Ready=True`, all probes pass.
- A `Flux GitRepository` (or `OCIRepository` / `Bucket` / `ExternalArtifact` / `JsonnetLibrary`) advances its `status.artifact` but no JaaS reconcile fires.

## Why JaaS can't tell you directly

controller-runtime's informer is what watches resource kinds at the apiserver. If the operator SA lacks `list`/`watch` on a kind:

1. The informer's initial LIST returns `Forbidden`.
2. controller-runtime logs `Failed to watch *v1.X: forbidden: ...` at error level.
3. The informer retries with exponential backoff — forever.
4. The reconciler's reconcile loop never gets events from that kind.
5. The reconciler itself is unaware that the watch is non-functional. The "no events arriving" condition is indistinguishable from "no actual changes upstream."

This is the one diagnostic surface the operator can't unify with its other RBAC-denial paths (Fetcher / library Get / Publisher write — all per-reconcile and surfaced via `Reason=RBACDenied`).

## Diagnosis

The smoking gun is in the operator's logs:

```shell
kubectl --namespace <jaas-ns> logs deploy/jaas --tail=2000 \
  | grep -E 'Failed to watch|"reflector.go"' \
  | head -30
```

Expected output if the watch layer is healthy: nothing.

If broken, you'll see lines like:

```text
E0610 12:34:56.789  reflector.go:227 "Failed to watch" err="failed to list *v1.GitRepository: ... forbidden: ServiceAccount \"jaas\" cannot list resource \"gitrepositories\" in API group \"source.toolkit.fluxcd.io\" at the cluster scope" type="*v1.GitRepository"
```

The error names the SA, verb, resource, and API group — that's the exact gap to close.

Check what the operator SA can actually do:

```shell
kubectl auth can-i list gitrepositories.source.toolkit.fluxcd.io \
    --as=system:serviceaccount:<jaas-ns>:jaas
```

Compare against the chart-rendered ClusterRole:

```shell
kubectl get clusterrole <release>-operator --output yaml | grep -A2 source.toolkit.fluxcd.io
```

## Remediation

The operator's ClusterRole verbs are defined in the chart's `templates/clusterrole-operator.yaml` (in the metio/helm-charts repo, under `charts/jaas/`). Three causes warrant separate fixes:

### 1. Chart upgraded with `rbac.create: false`

Someone disabled chart-rendered RBAC (`operator.rbac.create: false`) and the external RBAC source missed a verb. Either re-enable chart-rendered RBAC, or update whatever owns the ClusterRole to grant the missing verbs.

### 2. Manual chart edit removed verbs

A `kubectl edit clusterrole <release>-operator` or a hand-rolled overlay removed verbs the chart originally rendered. Restore via `helm upgrade --install` (idempotent for an installed chart).

### 3. New source kind added but chart's drift gate didn't catch it

The drift-gate test in the chart's `tests/clusterrole-operator_test.yaml` (metio/helm-charts, under `charts/jaas/`) — the "ClusterRole drift gate" case — is supposed to fail at PR time if `operator.FluxSourceKinds` adds a kind without a matching ClusterRole entry. If you reach this runbook page because of a new kind, **it means the test passed but production RBAC is still missing it** — investigate why (test bypassed, chart drift, etc.). Add the verb manually as a hotfix, then file a bug against the drift gate.

After granting the verb, restart the operator pod so a fresh informer picks up the new ServiceAccount-token permissions:

```shell
kubectl --namespace <jaas-ns> rollout restart deploy/jaas
```

Watch-driven re-renders resume within seconds.

## Why not detect this automatically

A startup probe (operator does a test `LIST` per kind and refuses to boot on Forbidden) was considered and rejected:

- It would block startup on transient apiserver flakes during deploys.
- The CRD-watcher pattern already handles the missing-CRD case gracefully (the `crdWatcher` engages a watch dynamically when the CRD becomes `Established=True`). Layering "and also fail on Forbidden" complicates that contract.
- A misconfigured cluster should surface the issue via the existing logs + the `kubectl auth can-i` workflow, which is the standard k8s troubleshooting path.

The diagnostic trail above is the supported recovery story. If a user reports hitting this in the wild and finds the log-grep step too obscure, a follow-up is a `jaas_informer_watch_failures_total` Prometheus counter plus a `JaaSInformerWatchFailing` alert — same shape as `JaaSStorageSweepFailures` from the storage layer. Track in `open-items.md` if it comes up.

## Related runbooks

- [rbacdenied.md](rbacdenied.md) — per-reconcile RBAC denials the reconciler CAN surface (tenant SA can't read a source CR / library, can't write ExternalArtifact). If a snippet's status says `Reason=RBACDenied`, start there instead.
- [storage-recovery.md](storage-recovery.md) — different failure surface (storage backend rather than apiserver), same "graceful degradation, diagnosis via logs + metrics" shape.
