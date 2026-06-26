---
title: Tenancy and RBAC
description: Per-snippet ServiceAccount impersonation, the minimal operator ClusterRole, the tenant Role callers must grant, and the watch-scope flags.
tags: [operator, rbac, multitenancy]
---

In [operator mode](/operator/operator-mode/) the JaaS operator never acts with its
own broad privileges when touching tenant resources. Every reconcile of a
`JsonnetSnippet` runs against the RBAC of a tenant ServiceAccount, so a snippet
can only reach what its own ServiceAccount is allowed to reach.

## Per-snippet impersonation

Each `JsonnetSnippet` carries a `spec.serviceAccountName`. On every reconcile the
operator mints a short-lived Bearer token for that ServiceAccount through the
Kubernetes TokenRequest API (`serviceaccounts/token: create`) and performs all
tenant-side API calls — reading `JsonnetLibrary` objects, fetching Flux source
artifacts, and writing the published `ExternalArtifact` — as that ServiceAccount.
The operator does not use the `impersonate` verb; it uses a real token, so the
apiserver evaluates the tenant's own RBAC.

When a snippet omits `spec.serviceAccountName`, the operator falls back to the
ServiceAccount named in `--default-service-account`. If that flag is also empty,
such a snippet is rejected at reconcile time rather than silently running with
elevated rights. Set `--default-service-account` to a low-privilege account if
you want snippets without an explicit ServiceAccount to reconcile at all.

## The operator's own ClusterRole

Because every tenant-side call is the tenant's, the operator's own ClusterRole
stays minimal:

- `serviceaccounts/token: create` — to mint the Bearer tokens above.
- `get`/`list`/`watch` on `customresourcedefinitions.apiextensions.k8s.io` — the
  CRD watcher subscribes to the cluster's CRD stream so that Flux source-kind
  watches engage automatically when a previously-absent CRD becomes established,
  without a process restart.
- Watch verbs on the JaaS CRDs (`JsonnetSnippet`, `JsonnetLibrary`) and on the
  Flux source kinds it chains from (`GitRepository`, `OCIRepository`, `Bucket`,
  `ExternalArtifact`).

The operator does not need `create`/`update`/`patch` on `ExternalArtifact` in its
own ClusterRole — that write is done as the tenant, so the verb lives on the
tenant Role below.

## The tenant Role

The ServiceAccount each snippet runs as needs explicit verbs, or the first
reconcile fails with `Forbidden` and the failure points at the wrong cause. Grant
this Role in the tenant's namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: <tenant-namespace>
  name: jaas-tenant
rules:
  # Required: the operator writes the snippet's ExternalArtifact as
  # the tenant ServiceAccount. Without these the publish step is denied.
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [externalartifacts]
    verbs: [get, create, update, patch]
  # Required only when the snippet uses spec.libraries (JsonnetLibrary refs).
  - apiGroups: [jaas.metio.wtf]
    resources: [jsonnetlibraries]
    verbs: [get, list]
  # Required only when the snippet uses spec.sourceRef. Grant only
  # the source kinds your tenants actually reference.
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [gitrepositories, ocirepositories, buckets, externalartifacts]
    verbs: [get]
```

Notes on each rule:

- The `externalartifacts` write verbs (`create`, `update`, `patch`) are
  mandatory. The operator writes the published artifact CR through the
  impersonating client on purpose, so one tenant Role governs both source-side
  reads and artifact-side writes.
- The `jsonnetlibraries` rule is needed only when a snippet references libraries
  through `spec.libraries`. See [snippet sources](/operator/snippet-sources/) for how
  libraries reach a snippet.
- The source-kind `get` rule is needed only when a snippet has a `spec.sourceRef`.
  Grant only the kinds your tenants reference. The `externalartifacts` entry here
  covers chained snippets — snippet B reading the `ExternalArtifact` snippet A
  publishes.

## Binding per namespace

For namespace-scoped multitenancy, bind the Role to each tenant ServiceAccount in
its own namespace with a `RoleBinding`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: <tenant-namespace>
  name: jaas-tenant
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: jaas-tenant
subjects:
  - kind: ServiceAccount
    name: <tenant-service-account>
    namespace: <tenant-namespace>
```

Each tenant namespace gets its own `Role` + `RoleBinding`, so a snippet's blast
radius is its own namespace's grants.

## Single-tenant clusters

When a cluster runs only your own workloads and snippets do not need isolating from
each other, a Role per namespace is more than you need. The operator still
impersonates a ServiceAccount — it never applies with its own identity — so the
simplest setup is one shared account:

1. Create a single ServiceAccount and grant it the rights your snippets need. On a
   single-tenant cluster that can be broad: a `ClusterRoleBinding` to the built-in
   `cluster-admin` ClusterRole lets any snippet read any source and publish into
   any namespace.

   ```yaml
   apiVersion: v1
   kind: ServiceAccount
   metadata:
     name: jaas-snippets
     namespace: jaas-system
   ---
   apiVersion: rbac.authorization.k8s.io/v1
   kind: ClusterRoleBinding
   metadata:
     name: jaas-snippets-admin
   roleRef:
     apiGroup: rbac.authorization.k8s.io
     kind: ClusterRole
     name: cluster-admin
   subjects:
     - kind: ServiceAccount
       name: jaas-snippets
       namespace: jaas-system
   ```

2. Point the operator's `--default-service-account` at it (set through the chart's
   operator values), and leave `spec.serviceAccountName` off your snippets. Every
   snippet then reconciles as that one account.

This trades isolation for simplicity — every snippet has the same rights, so use it
only where you trust every snippet author. To move to multitenancy later, give
individual snippets their own `spec.serviceAccountName` scoped to a tenant Role as
above; anything still relying on the default keeps working.

## Restricting cross-namespace references

`--no-cross-namespace-refs` defaults to `true`: a `JsonnetSnippet` or
`JsonnetLibrary` whose `sourceRef` targets a different namespace is rejected. Keep
this on for multitenancy — it stops one tenant from pointing a snippet at another
tenant's source. Set it to `false` only when you operate every namespace yourself
and deliberately want cross-namespace chaining.

## Narrowing the watch

Two flags scope which CRs the operator reconciles:

- `--label-selector` narrows the watch to CRs whose labels match the selector.
  Empty (the default) selects every CR in the watched scope. Use it to run an
  operator over only a labelled subset of snippets.
- `--watch-namespaces` (or the `JAAS_WATCH_NAMESPACES` environment variable) takes
  a comma-separated namespace list and restricts the manager's cache to those
  namespaces. Empty (the default) is cluster-wide. The Helm chart's
  `operator.watchNamespaces` mirrors this: when set, it threads the value into the
  deployment's `--watch-namespaces` argument and pivots the rendered RBAC to one
  `RoleBinding` per listed namespace instead of a cluster-wide
  `ClusterRoleBinding`. Cluster-scoped resources (CRDs, the optional
  `ValidatingWebhookConfiguration`) stay bound through a `ClusterRoleBinding`,
  since they are inherently cluster-scoped.

The full flag list, with defaults, is on the
[configuration page](/reference/configuration/).
