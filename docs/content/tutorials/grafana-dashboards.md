---
title: Grafana dashboards
description: Author a Grafana dashboard in Jsonnet with grafonnet, render it through the JaaS operator, and publish the dashboard JSON as an ExternalArtifact.
tags: [grafana, grafonnet, externalartifact]
---

JaaS pairs with the
[grafana-operator](https://grafana.github.io/grafana-operator/) to manage
Grafana dashboards as code: you author the dashboard in Jsonnet, the JaaS
operator renders it and publishes the dashboard JSON as a Flux
`ExternalArtifact`, and the grafana-operator reconciles that artifact into a live
Grafana instance.

This tutorial covers the JaaS side — authoring the dashboard, importing
grafonnet as a `JsonnetLibrary`, and publishing the rendered JSON. For the
grafana-operator side (the `GrafanaDashboard` CR, datasources, folders), see the
links at the end.

## Prerequisites

- The JaaS operator installed and a tenant ServiceAccount granted the
  `externalartifacts` write verbs. The [Quickstart](/tutorials/quickstart/)
  covers both.
- The grafana-operator installed, if you intend to follow the handoff section
  and reconcile the dashboard into Grafana.

This tutorial uses the namespace `default` and the tenant ServiceAccount
`dashboards-tenant`.

## Step 1 — Grant the tenant ServiceAccount its verbs

The snippet imports a `JsonnetLibrary`, so on top of the `externalartifacts`
write verbs the tenant needs `get` on `jsonnetlibraries`:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dashboards-tenant
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: default
  name: dashboards-tenant
rules:
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [externalartifacts]
    verbs: [get, create, update, patch]
  - apiGroups: [jaas.metio.wtf]
    resources: [jsonnetlibraries]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: default
  name: dashboards-tenant
subjects:
  - kind: ServiceAccount
    name: dashboards-tenant
    namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: dashboards-tenant
EOF
```

Verify the ServiceAccount and binding:

```shell
kubectl --namespace default get serviceaccount dashboards-tenant
kubectl --namespace default get rolebinding dashboards-tenant
```

## Step 2 — Publish the dashboard helpers as a JsonnetLibrary

A `JsonnetLibrary` holds reusable `.libsonnet` files that snippets in the same
namespace import by alias. The example below carries a minimal set of dashboard
constructors. In a production setup this is where grafonnet lives — see
[Jsonnet libraries](/usage/jsonnet-libraries/) for serving the full grafonnet
tree from an OCIRepository.

```shell
cat <<EOF | kubectl apply -f -
apiVersion: jaas.metio.wtf/v1
kind: JsonnetLibrary
metadata:
  name: grafana-helpers
  namespace: default
spec:
  files:
    dashboard.libsonnet: |
      {
        new(title): {
          title: title,
          schemaVersion: 38,
          panels: [],
        },
      }
    panel.libsonnet: |
      {
        timeseries(title, expr): {
          type: 'timeseries',
          title: title,
          targets: [{ expr: expr }],
        },
        stat(title, expr): {
          type: 'stat',
          title: title,
          targets: [{ expr: expr }],
        },
      }
EOF
```

Verify the library:

```shell
kubectl --namespace default get jsonnetlibrary grafana-helpers
```

## Step 3 — Author and apply the dashboard snippet

The `JsonnetSnippet` imports the library by the alias declared in
`spec.libraries[*].importPath`, composes a dashboard from its constructors, and
leaves `spec.output` at its default `rendered` so the published artifact carries
the evaluated dashboard JSON:

```shell
cat <<EOF | kubectl apply -f -
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: api-latency
  namespace: default
spec:
  serviceAccountName: dashboards-tenant
  output: rendered
  files:
    main.jsonnet: |
      local dashboard = import 'grafana/dashboard.libsonnet';
      local panel = import 'grafana/panel.libsonnet';
      dashboard.new('API Latency') + {
        panels: [
          panel.timeseries('p99 by route', 'histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))'),
          panel.stat('error rate', 'sum(rate(http_requests_total{code=~"5.."}[5m]))'),
        ],
      }
  libraries:
    - kind: JsonnetLibrary
      name: grafana-helpers
      importPath: grafana
EOF
```

The `importPath: grafana` ties `import 'grafana/dashboard.libsonnet'` to the
`grafana-helpers` library. It defaults to the library's `metadata.name`, so
naming the library `grafana` would let you drop the field. `kind` is always
`JsonnetLibrary`.

## Step 4 — Confirm the dashboard rendered

```shell
kubectl --namespace default get jsonnetsnippet api-latency
# NAME          READY   URL                                                                                         AGE
# api-latency   True    http://jaas-storage.jaas-system.svc.cluster.local:8082/default/api-latency/<sha256>.tar.gz  5s
```

If `READY` is `False`, describe the snippet — the Ready condition's `Reason` and
`Message` name the cause (an RBAC gap on the library, an import alias collision,
or a Jsonnet error):

```shell
kubectl --namespace default describe jsonnetsnippet api-latency
```

## Step 5 — Inspect the published dashboard JSON

Fetch the artifact from a one-shot pod to see the rendered dashboard:

```shell
URL=$(kubectl --namespace default get jsonnetsnippet api-latency -o jsonpath='{.status.artifactURL}')
kubectl run --rm -i --restart=Never --image=docker.io/curlimages/curl:8.10.1 fetch -- \
    sh -c "curl -fsSL '$URL' | tar -xzO rendered.json"
# {
#    "panels": [ ... ],
#    "schemaVersion": 38,
#    "title": "API Latency"
# }
```

`rendered.json` is the Grafana dashboard model — the exact JSON the
grafana-operator hands to Grafana's dashboard API.

## Use real grafonnet instead of the toy helpers

`grafana-helpers` kept this tutorial self-contained, but in production you import
the real [grafonnet](https://github.com/grafana/grafonnet) library from a JOI
image rather than hand-rolling constructors. Install it as a `JsonnetLibrary`
with the [`joi` Helm chart](https://github.com/metio/helm-charts/tree/main/charts/joi):

```shell
helm upgrade --install joi oci://ghcr.io/metio/helm-charts/joi \
  --namespace default \
  --set libraries.grafonnet.enabled=true
```

That renders an `OCIRepository` plus a `JsonnetLibrary` named `grafonnet`,
sourcing `ghcr.io/metio/joi-grafana-grafonnet`. The snippet then references that
library in place of `grafana-helpers` and imports the real grafonnet API by its
full jb-vendor path:

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: api-latency
  namespace: default
spec:
  serviceAccountName: dashboards-tenant
  libraries:
    - kind: JsonnetLibrary
      name: grafonnet
  files:
    main.jsonnet: |
      local g = import 'github.com/grafana/grafonnet/gen/grafonnet-latest/main.libsonnet';
      g.dashboard.new('API Latency')
      + g.dashboard.withUid('api-latency')
```

The rest of the flow is the same — the snippet reconciles and publishes an
`ExternalArtifact` exactly as in Steps 4–5; only the source of the library
differs.

## Handoff: reconcile the dashboard into Grafana

The published `ExternalArtifact` is now ready for the grafana-operator to
consume. The grafana-operator reconciles a JaaS-published dashboard into Grafana
through a `GrafanaDashboard` CR that references the artifact. That configuration
— the `GrafanaDashboard` resource, the datasource and folder wiring, and the
`Grafana` instance — lives on the grafana-operator's own documentation:

- **grafana-operator JaaS example:**
  <https://grafana.github.io/grafana-operator/docs/examples/dashboard/jaas/readme/>
- **grafana-operator project:** <https://grafana.github.io/grafana-operator/>

Follow that example for the Grafana side; it picks up exactly where this
tutorial leaves off — at the published `ExternalArtifact`.

## Where to go next

- [Jsonnet libraries](/usage/jsonnet-libraries/) — serve the full grafonnet
  tree as a `JsonnetLibrary` backed by an OCIRepository, with the empty-`path`
  whole-vendor-tree pattern.
- [Snippet sources](/usage/snippet-sources/) — back the dashboard with a
  `GitRepository` or OCIRepository instead of inline `spec.files`, and point
  `spec.entryFile` at one dashboard in a multi-dashboard tree.
