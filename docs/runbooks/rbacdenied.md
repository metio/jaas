---
title: RBACDenied
description: The apiserver returned Forbidden on a call the reconciler made with the tenant ServiceAccount's impersonated identity
tags: [runbooks, troubleshooting, rbac]
---

## Symptom

```text
kubectl describe jsonnetsnippet <name>
...
Status:
  Conditions:
    Reason:  RBACDenied
    Status:  False
    Type:    Ready
    Message: RBAC denied reading the source CR — grant the tenant ServiceAccount get on the source kind ...
```

Or for a missing CRD:

```text
    Message: source CR's kind is not registered with the apiserver — install the corresponding CRD ...
```

The reconciler logs at warn level and stops engaging backoff for this snippet. The next reconcile happens only when the snippet's spec changes, a referenced library / source CR's status flips, or `spec.interval` ticks — so the workqueue isn't burning cycles on a permanently-failing call.

## Cause

The apiserver returned `Forbidden` on a call the reconciler had to make. Three call sites can surface this:

1. **Source-CR read.** The tenant ServiceAccount lacks `get` on the kind named by `spec.sourceRef.kind`. The fix is on the tenant's `Role` / `RoleBinding`.
2. **Library-CR read.** The tenant SA lacks `get` (and typically `list`) on `jsonnetlibraries` in the snippet's namespace.
3. **ExternalArtifact write.** The tenant SA lacks `create` / `update` / `patch` on `externalartifacts`. This is the publish step — the rendered bytes are computed but the operator can't write them back as the impersonating client.

The `NoMatchError` variant means the apiserver doesn't know about the resource kind at all — typically because the corresponding CRD (usually Flux's source-controller) isn't installed in the cluster.

## Diagnosis

`kubectl describe` shows the operator's classified message. The verbatim apiserver error (`forbidden: ServiceAccount X cannot get resource Y in namespace Z`) is appended after the operator's classification, so you can read off:

- Which SA tried the call (`system:serviceaccount:<namespace>:<sa-name>`)
- Which verb it lacked (`cannot get`, `cannot create`, `cannot patch`)
- Which resource (`gitrepositories.source.toolkit.fluxcd.io`, `jsonnetlibraries.jaas.metio.wtf`, `externalartifacts.source.toolkit.fluxcd.io`)

Verify the SA exists and inspect its current permissions:

```shell
kubectl --namespace <tenant-namespace> get sa <sa-name>
kubectl auth can-i --as=system:serviceaccount:<tenant-namespace>:<sa-name> \
    --namespace <tenant-namespace> \
    <verb> <resource>
```

For the `NoMatchError` variant:

```shell
# Verify the CRD is actually installed:
kubectl get crd | grep -E 'source.toolkit.fluxcd.io|jaas.metio.wtf'

# If source-controller's CRDs are missing, install Flux:
# https://fluxcd.io/flux/installation/
```

## Remediation

Grant the missing verb to the tenant SA. The minimum verbs JaaS expects are documented in the [Tenancy and RBAC](https://jaas.projects.metio.wtf/usage/tenancy-and-rbac/#the-tenant-role) guide. Typical fix:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: <tenant-namespace>
  name: jaas-tenant
rules:
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [externalartifacts]
    verbs: [get, create, update, patch]
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [gitrepositories, ocirepositories, buckets, externalartifacts]
    verbs: [get]
  - apiGroups: [jaas.metio.wtf]
    resources: [jsonnetlibraries]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: <tenant-namespace>
  name: jaas-tenant
subjects:
  - kind: ServiceAccount
    name: <sa-name>
    namespace: <tenant-namespace>
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: jaas-tenant
```

After the RBAC change, force the next reconcile (the snippet's last spec edit doesn't auto-retrigger because the failure was non-transient):

```shell
kubectl annotate jsonnetsnippet <name> jaas.metio.wtf/reconcile-at=$(date -u +%FT%TZ) --overwrite
```

For the missing-CRD case, installing the CRD fires the operator's `crdWatcher`, which engages the watch automatically — no manual nudge needed.

## Why this is non-transient

`Forbidden` doesn't recover by retry. The cluster operator (or whoever owns the tenant's RBAC) has to grant the verb. Retrying every 16 minutes would pile up wasted API calls and obscure the workqueue's signal. The non-transient classification lets the workqueue depth metric remain meaningful — anything on it is genuinely live work.

`NoMatchError` is the same shape: until the CRD is installed, the kind doesn't exist. Retry can't conjure it.
