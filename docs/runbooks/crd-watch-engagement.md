---
title: CRD watch engagement failing
description: A runtime watch on a Flux source CRD failed to engage, so snippets referencing that kind no longer re-render on upstream changes
tags: [runbooks, troubleshooting, lifecycle]
---

Fires when `jaas_crd_watch_engagement_failures_total{gvk=...}` has increased above the per-hour threshold for the alert window. JaaS lazy-watches Flux source CRDs: at boot, only the CRDs already installed get a watch; when a previously-missing CRD becomes `Established=True` (operator installed source-controller post hoc, say), the `crdWatcher` engages a runtime watch on it via `Controller.Watch`. **When that engagement fails, the apiextensions informer fires no further events on a stable CRD** — meaning the watch stays un-engaged forever until either the CRD object's metadata/status is changed by something else, or the operator restarts.

The visible symptom is that snippets with `spec.sourceRef.Kind=<the affected kind>` stop re-rendering on upstream source updates. There is no per-snippet status signal — they sit at their last-rendered revision, drifting from upstream.

## Symptom

- `JaaSCRDWatchEngagementFailing` alert is firing with `gvk` labelling the affected kind.
- `kubectl describe jsonnetsnippet` on snippets referencing that GVK shows a Ready condition that hasn't moved in hours/days.
- Upstream Flux source CRs (GitRepository, OCIRepository, Bucket, ExternalArtifact) show recent `status.artifact.revision` changes that the jaas snippets aren't picking up.

## Diagnosis

### Step 1 — confirm the CRD is actually installed and Established

```shell
kubectl get crd <plural>.source.toolkit.fluxcd.io \
  -o jsonpath='{.status.conditions[?(@.type=="Established")].status}{"\n"}'
```

Expect `True`. If the CRD is not installed or not yet Established, the watcher is correct to skip; install / wait.

### Step 2 — check the operator's RBAC on the source kind

```shell
kubectl auth can-i list <plural>.source.toolkit.fluxcd.io \
  --as=system:serviceaccount:<ns>:<operator-sa>
kubectl auth can-i watch <plural>.source.toolkit.fluxcd.io \
  --as=system:serviceaccount:<ns>:<operator-sa>
```

If either is "no", the chart's `operator-tenants` ClusterRole (or per-namespace RoleBinding when `watchNamespaces` is set) is missing the `get/list/watch` verbs on this kind. Update the chart's `FluxSourceKinds` mapping or add the verb manually.

### Step 3 — check controller-runtime cache state

```shell
kubectl logs -n <ns> <operator-pod> | grep -E 'engage|Failed to watch|cache' | tail -20
```

Look for `cache reconnect`, `informer failed`, or `Watch failed: forbidden`. A transient cache reconnect during a heavy load period can trip engagement once; the DD7 bounded-retry mechanism re-engages automatically. Sustained failures point at RBAC or a misconfigured `MetricsBindAddress`.

## Remediation

1. **Fix the verb / kind / RBAC** issue identified above.
2. **Roll the operator pod** to force a fresh `SetupWithManager` pass, which re-detects every Flux CRD and re-engages watches that succeed on first try:

   ```shell
   kubectl rollout restart deployment -n <ns> <operator-deployment>
   ```

3. **Verify** the counter stops increasing and the alert clears.

## When the alert is noisy

If `jaas_crd_watch_engagement_failures_total` ticks once at boot but never again, that's the expected DD7 bounded-retry behavior: the first attempt failed (transient race during cache start), the retry succeeded. Raise `crdWatchEngagementFailuresPerHour` if the boot-time blip is noisy enough to page.
