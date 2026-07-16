---
title: Quickstart
description: Install the JaaS operator and publish your first ExternalArtifact from an inline-files JsonnetSnippet.
tags: [operator, externalartifact, helm]
---

This tutorial takes you from an empty cluster to one published Flux
`ExternalArtifact` carrying rendered JSON. The path is operator mode with no
optional knobs — no webhook, no S3, no Flux source CRs — and a single
`JsonnetSnippet` whose source is inline `spec.files`.

## Prerequisites

- A Kubernetes cluster. `kind`, `minikube`, or a managed cluster all work.
- `kubectl` configured to talk to it.
- `helm` 3.x.
- Flux installed, at **v2.7.0 or newer**. A `JsonnetSnippet` publishes its
  result as a Flux `ExternalArtifact`, and the `ExternalArtifact` CRD lands in
  source-controller v1.7.0 (Flux v2.7.0) — earlier bundles have no such CRD and
  the publish path fails. Install all of Flux:

  ```shell
  kubectl apply -f https://github.com/fluxcd/flux2/releases/download/v2.7.0/install.yaml
  ```

## Step 1 — Install the chart

```shell
helm upgrade --install jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system --create-namespace \
  --set operator.enabled=true \
  --set operator.defaultServiceAccount=default \
  --wait --timeout 5m
```

`operator.defaultServiceAccount=default` tells the operator which
ServiceAccount to impersonate in a tenant namespace when a snippet does not name
its own. That is fine for this tutorial; production assigns a dedicated SA per
tenant — see [Tenancy and RBAC](/security/tenancy-and-rbac/).

Verify the operator is running:

```shell
kubectl --namespace jaas-system get deploy jaas
# NAME   READY   UP-TO-DATE   AVAILABLE   AGE
# jaas   1/1     1            1           30s
```

## Step 2 — Grant the tenant ServiceAccount the minimum verbs

The `default` ServiceAccount's built-in RBAC does not include the verbs the
operator needs to publish the artifact. In the tenant namespace — here `default`
— apply a `Role` and `RoleBinding`:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: default
  name: jaas-tenant
rules:
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [externalartifacts]
    verbs: [get, create, update, patch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: default
  name: jaas-tenant
subjects:
  - kind: ServiceAccount
    name: default
    namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: jaas-tenant
EOF
```

The operator impersonates the tenant ServiceAccount to upsert the
`ExternalArtifact` it publishes. Without `create`, `update`, and `patch` on
`externalartifacts`, the first reconcile fails with `Reason=RBACDenied`.

Verify the binding:

```shell
kubectl --namespace default get rolebinding jaas-tenant
# NAME          ROLE               AGE
# jaas-tenant   Role/jaas-tenant   5s
```

## Step 3 — Apply your first snippet

```shell
cat <<EOF | kubectl apply -f -
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: hello
  namespace: default
spec:
  serviceAccountName: default
  files:
    main.jsonnet: |
      {
        greeting: "hello from jaas",
        rendered_at: std.extVar("now"),
      }
  externalVariables:
    - name: now
      value: "quickstart"
EOF
```

This is a complete `JsonnetSnippet`. Three fields carry it:

- `spec.serviceAccountName` — the ServiceAccount the operator impersonates for
  this snippet's API calls.
- `spec.files.<filename>` — inline Jsonnet source. The default entry file is
  `main.jsonnet`; override it with `spec.entryFile`.
- `spec.externalVariables` — `std.extVar()` lookups available to the snippet.
  Each entry binds one name to a string; add `code: true` to bind a number,
  array, or object instead.

Verify the resource exists:

```shell
kubectl --namespace default get jsonnetsnippet hello
```

## Step 4 — Confirm it reconciled

```shell
kubectl --namespace default get jsonnetsnippet hello
# NAME    READY   URL                                                                                    AGE
# hello   True    http://jaas-storage.jaas-system.svc.cluster.local:8082/default/hello/<sha256>.tar.gz   5s
```

The `URL` column is the artifact's address. If `READY` is `False`, describe the
resource — the `Reason` and `Message` on the Ready condition name the problem
(most commonly an RBAC gap or a Jsonnet syntax error):

```shell
kubectl --namespace default describe jsonnetsnippet hello
```

The `ExternalArtifact` is the resource downstream Flux consumers read:

```shell
kubectl --namespace default get externalartifact hello -o yaml
# status:
#   artifact:
#     url: http://jaas-storage.jaas-system.svc.cluster.local:8082/default/hello/<sha256>.tar.gz
#     digest: sha256:<hex>
#     revision: sha256:<hex>
#     size: <bytes>
```

## Step 5 — Fetch the rendered bytes

The URL resolves in-cluster only. Fetch the tarball from a one-shot pod:

```shell
URL=$(kubectl --namespace default get jsonnetsnippet hello -o jsonpath='{.status.artifactURL}')
kubectl run --rm -i --restart=Never --image=docker.io/curlimages/curl:8.10.1 fetch -- \
    sh -c "curl -fsSL '$URL' | tar -xzO rendered.json"
# {
#    "greeting": "hello from jaas",
#    "rendered_at": "quickstart"
# }
```

The tarball carries a single `rendered.json` because `spec.output` defaults to
`rendered` (the evaluated JSON). Setting `spec.output: source` publishes the raw
`.jsonnet` files instead, for consumers that re-evaluate themselves.

## Clean up

```shell
kubectl --namespace default delete jsonnetsnippet hello
kubectl --namespace default delete rolebinding jaas-tenant
kubectl --namespace default delete role jaas-tenant
helm --namespace jaas-system uninstall jaas
kubectl delete namespace jaas-system
```

The chart's pre-delete hook waits for the snippet's finalizer to drop — which
removes the `ExternalArtifact` — before the operator pod is removed, so the
uninstall leaves no orphans.

## Where to go next

- [Grafana dashboards](/guides/grafana-dashboards/) — render grafonnet
  dashboards and hand them to the grafana-operator.
- [Deploying manifests with StageSet](/guides/deploying-manifests/) — render
  Kubernetes manifests and roll them out with stageset-controller.
- [Operator mode](/operator/operator-mode/) — the full operator reference: source
  kinds, leader election, the artifact contract.
- [Usage](/rendering/) — every configuration knob, one page per concern.
