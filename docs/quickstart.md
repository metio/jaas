<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Quickstart: from `helm install` to a published artifact in 5 minutes

This walks through the minimum viable JaaS install — operator mode on, no webhook, no S3, no Flux source CRs. The goal is to apply one `JsonnetSnippet` and see its rendered JSON show up as an `ExternalArtifact` whose URL is reachable in-cluster.

For depth — sourceRef chains, libraries, OCI mounts, multi-replica HA, webhook self-signed cert mode — see [`README.md`](../README.md) and the worked examples under [`../examples/`](../examples/).

## Prerequisites

- A Kubernetes cluster (any flavour; `kind`, `minikube`, or a real cluster all work).
- `kubectl` configured to talk to it.
- `helm` 3.x.
- **Flux's `source-controller`** installed in the cluster — `JsonnetSnippet` publishes its result as a Flux `ExternalArtifact`, which means the `source.toolkit.fluxcd.io/v1` CRDs need to exist. Quickest path:

  ```shell
  kubectl apply -f https://github.com/fluxcd/flux2/releases/download/v2.6.0/install.yaml
  ```

  That installs all of Flux. If you want only the bits JaaS needs, the `source-controller` Deployment + its CRDs are sufficient — but the full install gives you `notification-controller` for the event-routing story.

## Step 1 — Install the chart

```shell
helm install jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system --create-namespace \
  --set operator.enabled=true \
  --set operator.defaultServiceAccount=default \
  --wait --timeout 5m
```

`operator.defaultServiceAccount=default` tells the operator to impersonate the `default` ServiceAccount in each tenant's namespace when the snippet doesn't pick its own. Fine for the quickstart; production should use a dedicated SA per tenant — see [Tenant ServiceAccount RBAC](../README.md#tenant-serviceaccount-rbac).

Confirm the operator is running:

```shell
kubectl -n jaas-system get deploy jaas
# NAME   READY   UP-TO-DATE   AVAILABLE   AGE
# jaas   1/1     1            1           30s
```

## Step 2 — Grant the tenant SA the minimum verbs

`default`'s default RBAC doesn't include the verbs JaaS needs. In a tenant namespace (here we'll use `default`), apply:

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

Why these verbs: the operator impersonates `default` to upsert the `ExternalArtifact` it publishes. Without `create`/`update`/`patch` on `externalartifacts`, the first reconcile fails with `Reason=RBACDenied`. (`get` covers chained-snippet sourceRefs — not needed for this quickstart but adds nothing to grant.)

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
    now: "quickstart"
EOF
```

That's a complete `JsonnetSnippet`. Three required-ish fields:

- `spec.serviceAccountName` — the SA the operator impersonates for this snippet's API calls.
- `spec.files.<filename>` — inline Jsonnet source. The default entry file is `main.jsonnet`; override with `spec.entryFile` if you want.
- `spec.externalVariables` — `std.extVar()` lookups available to the snippet.

## Step 4 — Confirm it reconciled

```shell
kubectl get jsonnetsnippet hello
# NAME    READY   URL                                                                                    AGE
# hello   True    http://jaas-storage.jaas-system.svc.cluster.local:8082/default/hello/<sha256>.tar.gz   5s
```

The `URL` column is the artifact's public URL. If `READY` is `False`, run `kubectl describe jsonnetsnippet hello` — the `Reason` + `Message` on the Ready condition names the problem (likely an RBAC gap or a Jsonnet syntax error), and most reasons link to a [runbook page](runbooks/).

The `ExternalArtifact` is the downstream-Flux-consumable resource:

```shell
kubectl get externalartifact hello -o yaml
# ...
# status:
#   artifact:
#     url: http://jaas-storage.jaas-system.svc.cluster.local:8082/default/hello/<sha256>.tar.gz
#     digest: sha256:<hex>
#     revision: sha256:<hex>
#     size: <bytes>
```

## Step 5 — Fetch the rendered bytes (optional)

The URL is in-cluster only by default. To fetch the tarball without port-forwarding, run a one-shot pod:

```shell
URL=$(kubectl get jsonnetsnippet hello -o jsonpath='{.status.artifactURL}')
kubectl run --rm -i --restart=Never --image=docker.io/curlimages/curl:8.10.1 fetch -- \
    sh -c "curl -fsSL '$URL' | tar -xzO rendered.json"
# {
#    "greeting": "hello from jaas",
#    "rendered_at": "quickstart"
# }
```

The tarball contains a single `rendered.json` because `spec.output` defaults to `rendered`. If you want the source files instead (for downstream consumers that re-evaluate), set `spec.output: source`.

## Where to go next

- **Going to production:** [`docs/production.md`](production.md) — what to enable for a multi-replica, observable, secured install
- **Wiring Flux consumers to JaaS artifacts:** [`docs/consumers.md`](consumers.md) — direct vs producer-aware reference styles, `spec.history` vs `--artifact-gc-grace`
- **Templates that import libraries:** [`examples/operator/with-library.yaml`](../examples/operator/with-library.yaml)
- **Snippet whose source is a Git repo:** [`examples/operator/source-gitrepository.yaml`](../examples/operator/source-gitrepository.yaml)
- **Chained snippets (one snippet imports another's output):** [`examples/operator/chained-snippets.yaml`](../examples/operator/chained-snippets.yaml)
- **Production-shape full stack with Grafana:** [`examples/full-stack/`](../examples/full-stack/)
- **Operator concepts and full chart values:** [`README.md`](../README.md)
- **When things go wrong:** [`docs/runbooks/`](runbooks/)

## Cleanup

```shell
kubectl delete jsonnetsnippet hello -n default
kubectl delete rolebinding jaas-tenant -n default
kubectl delete role jaas-tenant -n default
helm uninstall jaas -n jaas-system
kubectl delete namespace jaas-system
```

The chart's pre-delete hook waits for the snippet's finalizer to drop (which removes the `ExternalArtifact`) before the operator pod itself is removed — so `helm uninstall` cleanly tears down without orphans.
