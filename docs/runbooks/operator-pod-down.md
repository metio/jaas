---
title: Operator pod not ready
description: At least one jaas pod has been Ready=False for the configured alert window, so new snippets are not being reconciled
tags: [runbooks, troubleshooting, lifecycle]
---

Linked from the `JaaSOperatorPodDown` alert. Fires when at least one jaas pod has been `Ready=False` for the alert window (default 5m).

## Symptom

```text
ALERTS{alertname="JaaSOperatorPodDown", namespace="<jaas-ns>"}
```

- New JsonnetSnippets stay `Ready=Unknown` indefinitely (no controller reconciling them).
- Existing snippets keep serving stale `ExternalArtifact` content (the storage HTTP server may still respond; reconciliation is the part that stopped).

## Cause

One of the chart's two probes is failing:

- **Liveness** (`/live`) is unconditional 200 — a failure here means the pod's HTTP server itself isn't responding (deadlock, OOM, panic).
- **Readiness** (`/ready`) consults `HealthState`, which goes `false` during `drainBeforeShutdown` or before the listeners bind.
- **Startup** (`/start`) returns 503 until `MarkStarted()` is called — bind failures (port already in use, permission denied) keep the pod stuck here forever.

Frequent causes:

1. **Bind failure** on one of the HTTP servers (jsonnet, management, storage, metrics, webhook). The pod logs a clear "listen tcp: address already in use" or similar at boot.
2. **OOMKilled** — a pathological snippet allocated a huge object; the kubelet killed the pod. `kubectl describe pod` shows `Last State: Terminated, Reason: OOMKilled`.
3. **Image pull failure** — registry rate limit, wrong tag, missing pull secret.
4. **TLS cert missing or unreadable** when `operator.webhook.enabled=true` and the cert-manager Secret hasn't materialized.
5. **Lease contention** that leaves no replica as leader (every replica reconnecting to renew, never holding the lease).

## Diagnosis

```shell
# Which probes are failing? Events tell you.
kubectl -n <jaas-ns> describe pod -l app.kubernetes.io/name=jaas

# Pod logs — the boot sequence prints every listener it binds.
kubectl -n <jaas-ns> logs -l app.kubernetes.io/name=jaas --tail=300

# Compare against the expected listener set.
kubectl -n <jaas-ns> get svc -l app.kubernetes.io/name=jaas
```

For OOM:

```shell
kubectl -n <jaas-ns> top pod -l app.kubernetes.io/name=jaas
kubectl -n <jaas-ns> get pod -l app.kubernetes.io/name=jaas -o yaml \
  | grep -A3 lastState
```

For lease problems (multi-replica only):

```shell
kubectl -n <jaas-ns> get lease <release-name>-operator -o yaml
```

`holderIdentity` flipping every renewal interval is a sign of network flake or apiserver pressure — the replicas can't keep the lease stable.

## Remediation

- **Bind failure.** Free the colliding port (often `8080`, when the controller-runtime metrics endpoint defaults conflict with the jsonnet HTTP port — confirm `--metrics-bind-address` is `:8083`).
- **OOMKilled.** Raise `resources.memory`, then identify the runaway snippet (the bench in `internal/operator/bench_test.go` is a regression baseline; the runaway is usually obvious from `jaas_snippet_rendered_bytes`).
- **Image pull.** Standard k8s drill: check secrets, registry, tag.
- **TLS cert.** With `certMode=cert-manager`, confirm the Issuer / Certificate are ready. With `certMode=self-signed`, the operator regenerates on boot — a permission error on the cert-dir mount blocks it.
- **Lease flap.** Try `kubectl -n <jaas-ns> delete lease <release-name>-operator` to force a fresh election. If it keeps flapping, the cluster has bigger problems than JaaS.

## Prevention

- Pin `replicas.max: 1` and `LeaderElectionReleaseOnCancel: true` (chart defaults). Multi-replica is only worth it for storage-backed HA — single-replica is the simpler operational story.
- Run the cleanup Job (`operator.cleanupOnDelete.enabled: true`, chart default) so a `helm uninstall` of a wedged operator unwinds the finalizers instead of leaving orphaned snippets.
- Pair this alert with `JaaSControllerWorkqueueDepthHigh` ([workqueue-saturation.md](workqueue-saturation.md)). A pod-down event almost always coincides with a saturated queue from snippets piling up.
