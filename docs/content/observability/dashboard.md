---
title: Dashboard
description: JaaS publishes a maintained Grafana dashboard for its operator as a single-layer OCI image — render it through a JsonnetSnippet and hand it to the grafana-operator.
tags: [operator, grafana, grafonnet, slo, dashboard]
---

JaaS publishes a maintained Grafana dashboard for its own operator as a
single-layer OCI image at `ghcr.io/metio/jaas-dashboard`. Render it through JaaS the
same way you'd render any snippet — no manual JSON to copy around.

The dashboard opens with an [SLO](/observability/slos/) band — reconcile
availability against its objective, error budget remaining, reconcile-latency p95
against its objective, and an availability-versus-objective trend — over a band of
operator internals that explain any movement: reconciles by reason, evaluation
throughput and backpressure, rendered output size, storage sweeps, and the
controller-runtime workqueue. The series are the [metrics](/observability/metrics/)
the operator exports.

To author your *own* dashboards in grafonnet, see the
[Grafana dashboards tutorial](/tutorials/grafana-dashboards/); this page covers
consuming the ready-made one.

## 1. Point a Flux source at the image

It's an OCI artifact, so use an `OCIRepository`. The dashboard imports grafonnet,
so also install the grafonnet `JsonnetLibrary` (the
[`joi` chart](https://github.com/metio/helm-charts/tree/main/charts/joi),
`--set libraries.grafonnet.enabled=true`).

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: jaas-dashboard
  namespace: monitoring
spec:
  interval: 1h
  url: oci://ghcr.io/metio/jaas-dashboard
  ref:
    tag: latest        # or a dated tag like 2026.6.22 to pin
```

The dashboard source also lives in the JaaS repository under `dashboards/`; if you
mirror it into Git, a `GitRepository` pointing there works identically.

## 2. Render it with a JsonnetSnippet, passing the dashboard's TLAs

The dashboard is a function of top-level arguments — `datasource` (the Prometheus
datasource UID, default `prometheus`), `title` (default `JaaS operator`), a
`selector` label matcher folded into every query (default empty), and the SLO knobs
`window` (default `28d`), `availabilityTarget` (default `0.99`), and `latencyTarget`
seconds (default `30`) — supplied through `spec.tlas`.

`selector` is how you adapt to your Prometheus: scope the dashboard to one scrape
job or cluster, e.g. `selector: ['job="jaas"']` or `['cluster="prod"']`. The queries
never touch the `namespace` label, so a Prometheus that relabels it to
`exported_namespace` doesn't affect them; `selector` pins anything else.

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: jaas-operator-dashboard
  namespace: monitoring
spec:
  serviceAccountName: dashboards-tenant
  # The OCI image's only file is main.jsonnet, which is spec.entryFile's default,
  # so no entryFile is needed.
  sourceRef:
    kind: OCIRepository
    name: jaas-dashboard
  # grafonnet, which the dashboard imports by its full jb-vendor path, is served
  # by the JOI library installed above.
  libraries:
    - kind: JsonnetLibrary
      name: grafonnet
  # The dashboard's top-level arguments. Each value is a list; a single element
  # becomes a string TLA.
  tlas:
    datasource: ["prometheus"]         # your Prometheus datasource UID
    title: ["JaaS operator — prod"]
    selector: ['job="jaas"']           # scope every query to your scrape job/cluster
    window: ["28d"]                    # SLO window
    availabilityTarget: ["0.99"]       # reconcile-availability objective (99%)
    latencyTarget: ["30"]              # reconcile-latency p95 objective (seconds)
  interval: 10m
  output: rendered
```

This is zero-maintenance: the `OCIRepository` polls `ghcr.io/metio/jaas-dashboard`,
so when a new dashboard is published JaaS re-renders, the `ExternalArtifact` digest
changes, and the grafana-operator pushes the new JSON to Grafana — you reference the
upstream image once and get every update automatically. Pin `ref.tag` to a dated
tag instead of `latest` if you'd rather adopt updates deliberately.

JaaS renders the dashboard JSON and publishes it as an `ExternalArtifact` named
after the snippet — `jaas-operator-dashboard`.

To fan this dashboard out per cluster — one rendered instance per cluster, each
scoped to its own Prometheus — template the snippet with a Flux Operator
`ResourceSet`; see the
[Flux Operator integration](/usage/flux-operator/) page.

## 3. Hand the artifact to the grafana-operator

A `GrafanaDashboard` that references the published `ExternalArtifact` reconciles it
into Grafana:

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: jaas-operator
  namespace: monitoring
spec:
  instanceSelector:
    matchLabels:
      dashboards: "grafana"
  resyncPeriod: 30s
  sourceRef:
    apiVersion: source.toolkit.fluxcd.io/v1
    kind: ExternalArtifact
    name: jaas-operator-dashboard
    namespace: monitoring
```

When the published image updates (or you change the TLAs), JaaS re-renders, the
`ExternalArtifact` digest changes, and the grafana-operator pushes the new JSON to
Grafana — no manual export step.
