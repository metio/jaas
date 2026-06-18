---
title: Watch-layer silent failure
description: The operator's own ClusterRole is missing a verb on a watched kind, so controller-runtime's informer retries silently and no snippet status reflects the problem
tags: [runbooks, troubleshooting, rbac]
---

The one RBAC-denial path the reconciler cannot surface itself: when the **operator's own** ClusterRole is missing a verb on a watched resource kind, controller-runtime's informer fails to start its watch, logs warnings, and retries silently. The reconciler never sees the failure, and no snippet's status condition reflects it. This is not tied to a per-snippet `Reason`.

If a per-snippet runbook ([rbacdenied](/runbooks/rbacdenied/), [sourcefetchfailed](/runbooks/sourcefetchfailed/), [sourcenotready](/runbooks/sourcenotready/)) doesn't match the symptoms, this is where to look next.

## Symptom

- Snippets that worked yesterday stop receiving watch-driven re-renders. They still reconcile on edits or `spec.interval` ticks, but not on upstream source changes.
- `kubectl describe jsonnetsnippet` shows healthy or stale state — never `Reason=RBACDenied` or any other failure.
- Operator pod is `Ready=True`, all probes pass.
- A `Flux GitRepository` (or `OCIRepository` / `Bucket` / `ExternalArtifact` / `JsonnetLibrary`) advances its `status.artifact` but no JaaS reconcile fires.

## Why no status condition surfaces this

controller-runtime's informer is what watches resource kinds at the apiserver. If the operator SA lacks `list`/`watch` on a kind:

1. The informer's initial LIST returns `Forbidden`.
2. controller-runtime logs `Failed to watch *v1.X: forbidden: ...` at error level.
3. The informer retries with exponential backoff — forever.
4. The reconciler's reconcile loop never gets events from that kind.
5. The reconciler itself is unaware that the watch is non-functional. The "no events arriving" condition is indistinguishable from "no actual changes upstream."

This is the one diagnostic surface the operator can't unify with its other RBAC-denial paths (Fetcher / library Get / Publisher write — all per-reconcile and surfaced via `Reason=RBACDenied`; see [rbacdenied](/runbooks/rbacdenied/)).

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

## Why no automatic detection

The operator does not pre-flight a test `LIST` per kind at boot and refuse to start on Forbidden:

- It would block startup on transient apiserver flakes during deploys.
- The `crdWatcher` already handles the missing-CRD case gracefully — it engages a watch dynamically when the CRD becomes `Established=True`. Adding "and also fail on Forbidden" complicates that contract.
- A misconfigured cluster surfaces the issue via the operator logs and the `kubectl auth can-i` workflow above, the standard Kubernetes troubleshooting path.

## Related runbooks

- [rbacdenied](/runbooks/rbacdenied/) — per-reconcile RBAC denials the reconciler CAN surface (tenant SA can't read a source CR / library, can't write ExternalArtifact). If a snippet's status says `Reason=RBACDenied`, start there instead.
- [storage-recovery](/runbooks/storage-recovery/) — a different failure surface (storage backend rather than apiserver), same graceful-degradation shape diagnosed via logs and metrics.
