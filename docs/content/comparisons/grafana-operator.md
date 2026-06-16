---
title: JaaS and grafana-operator
description: How JaaS renders Grafana dashboard JSON from Jsonnet while grafana-operator reconciles it into Grafana.
tags: [comparison, grafana-operator]
---

JaaS and the [grafana-operator](https://grafana.github.io/grafana-operator/) are
not alternatives — they do different jobs and are commonly used together. JaaS
*produces* dashboard JSON from Jsonnet; grafana-operator *consumes* dashboard
JSON and reconciles it into a Grafana instance.

## Division of labour

grafana-operator manages Grafana itself. It reconciles `GrafanaDashboard`,
`GrafanaDatasource`, `GrafanaFolder`, and related resources into one or more
Grafana instances, handling authentication, folder placement, datasource wiring,
and drift correction inside Grafana. A `GrafanaDashboard` can take its dashboard
model from inline JSON, a URL, a ConfigMap, a `grafana.com` dashboard ID, or a
remote source.

JaaS evaluates Jsonnet — including [grafonnet](https://grafana.github.io/grafonnet/)
— and publishes the rendered dashboard JSON as a Flux `ExternalArtifact`. It
knows nothing about Grafana: it renders JSON and stops there.

So the two compose along a clean seam. You author dashboards in grafonnet, JaaS
renders them to JSON, and grafana-operator takes that JSON and reconciles it into
Grafana. Each tool owns one half of the pipeline and neither reaches into the
other's domain.

## When grafana-operator alone is enough

If your dashboards are already plain JSON, or you consume them by `grafana.com`
dashboard ID, or you maintain them in the Grafana UI and export the model, then
grafana-operator covers the whole workflow on its own. There is no Jsonnet to
render, so there is nothing for JaaS to do. Reach for grafana-operator by itself
whenever the dashboard model exists as static JSON.

## When to add JaaS

Add JaaS when your dashboards are authored in grafonnet (or any Jsonnet),
typically to share panels, variables, and layout helpers across many dashboards
instead of duplicating JSON. JaaS turns that Jsonnet into the JSON
grafana-operator expects, with the same `jsonnet -J vendor` import resolution
you use locally, so a dashboard renders identically on your workstation and
in-cluster. grafana-operator then reconciles the rendered output as it would any
other dashboard JSON.

## Wiring them together

The grafana-operator project documents the JaaS integration directly, including
the `GrafanaDashboard` configuration that points at a JaaS-rendered artifact:
[grafana-operator dashboard example with JaaS](https://grafana.github.io/grafana-operator/docs/examples/dashboard/jaas/readme/).
Keep all `GrafanaDashboard`, datasource, and folder configuration on the
grafana-operator side; JaaS contributes only the rendering step and the
`ExternalArtifact` it publishes.

The [Grafana dashboards](/tutorials/grafana-dashboards/) tutorial shows the JaaS
side — authoring a grafonnet dashboard as a `JsonnetSnippet` and publishing the
rendered JSON.
