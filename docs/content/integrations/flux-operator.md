---
title: Flux Operator (ResourceSet) integration
description: Template one JsonnetSnippet per input with a Flux Operator ResourceSet so JaaS renders per-cluster or per-tenant content from a single definition.
tags: [usage, flux-operator, resourceset, multi-cluster, dashboard]
---

Render the same Jsonnet for many clusters or tenants without writing a snippet by
hand for each one: let a [Flux Operator](https://fluxoperator.dev/) `ResourceSet`
template a `JsonnetSnippet` per input, and JaaS renders each instance into its own
`ExternalArtifact`. The `ResourceSet` supplies the per-instance values through Go
template substitution; JaaS does the evaluation. There is no `ResourceSet` field
in the JaaS API — you use the two together: the `ResourceSet` templates ordinary
`JsonnetSnippet` CRs, which JaaS reconciles like any other. This needs the Flux
Operator installed alongside JaaS in the operator mode described under
[operator mode](/operator/operator-mode/).

The worked example below fans the maintained operator dashboard (see
[Dashboard](/observability/dashboard/)) out across a fleet: each cluster gets its
own rendered dashboard, scoped to its own Prometheus and labelled with its own
title.

## 1. Declare the per-instance inputs

A `ResourceSetInputProvider` of `.spec.type` `Static` exports a fixed list of
inputs from `.spec.defaultValues`. List one entry per cluster, each carrying the
values the dashboard's top-level arguments need:

```yaml
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: clusters
  namespace: monitoring
spec:
  type: Static
  defaultValues:
    clusters:
      - name: prod
        datasource: prometheus-prod
        selector: 'cluster="prod"'
      - name: staging
        datasource: prometheus-staging
        selector: 'cluster="staging"'
```

Other provider types export inputs from a live source instead — Git branches,
pull requests, or OCI artifact tags — and `.spec.schedule` (with `cron`,
`timeZone`, and `window`) gates when those inputs refresh, so the templated
snippets only change inside the window. The
[time-based delivery guide](https://fluxoperator.dev/docs/resourcesets/time-based-delivery/)
covers that pattern.

## 2. Template a JsonnetSnippet per input

A `ResourceSet` consumes the provider through `.spec.inputsFrom`, then templates
its `.spec.resources` once per resolved input. Each rendered `JsonnetSnippet`
points at the dashboard `OCIRepository` and threads the per-cluster values into
`spec.tlas` with `<< inputs.x >>` substitution:

```yaml
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: cluster-dashboards
  namespace: monitoring
spec:
  serviceAccountName: dashboards-tenant
  inputsFrom:
    - kind: ResourceSetInputProvider
      name: clusters
  resources:
    - apiVersion: jaas.metio.wtf/v1
      kind: JsonnetSnippet
      metadata:
        name: dashboard-<< inputs.name >>
        namespace: monitoring
      spec:
        serviceAccountName: dashboards-tenant
        sourceRef:
          kind: OCIRepository
          name: jaas-dashboard
        libraries:
          - kind: JsonnetLibrary
            name: grafonnet
        tlas:
          datasource: ["<< inputs.datasource >>"]
          title: ["JaaS operator — << inputs.name >>"]
          selector: ['<< inputs.selector >>']
        interval: 10m
        output: rendered
```

The `ResourceSet` creates `dashboard-prod` and `dashboard-staging`, each with its
own datasource UID, title, and query selector. JaaS reconciles each snippet
independently and publishes one `ExternalArtifact` per cluster, named after the
snippet. Point a `GrafanaDashboard` at each artifact exactly as the
[Dashboard](/observability/dashboard/) page shows, and the grafana-operator pushes
the per-cluster JSON to Grafana.

The same shape generalises beyond dashboards: any per-tenant or per-environment
Jsonnet — manifest libraries, config documents — fans out the same way. Add a
cluster to the provider's list and the `ResourceSet` renders one more snippet; no
new snippet YAML by hand.

For where this pattern sits relative to running JaaS or the Flux Operator alone,
see [JaaS vs Flux Operator ResourceSet](/comparisons/flux-operator/).
