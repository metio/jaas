---
title: Helm chart values
description: Complete reference for every value the jaas and joi Helm charts expose, generated from each chart's values.yaml.
tags: [installation, helm, chart, values, reference]
---

The jaas Helm chart lives in the
[metio/helm-charts](https://github.com/metio/helm-charts/tree/main/charts/jaas)
monorepo and is published at `oci://ghcr.io/metio/helm-charts/jaas`. The tables
below are generated from each chart's `values.yaml`, so they track the chart's
current values rather than a hand-maintained copy.

For how the values map onto the binary's runtime behaviour, see the
[Configuration reference](/installation/configuration/) — every `arguments.*`
value drives the corresponding `--flag`.

## jaas chart

{{< helm-values data="helm-values" >}}

## joi library chart

The [joi](https://github.com/metio/helm-charts/tree/main/charts/joi) chart
publishes [Jsonnet OCI Images](https://github.com/metio/jsonnet-oci-images) as
`JsonnetLibrary` + `OCIRepository` pairs, so snippets can import vendored
libraries (grafonnet, k8s-libsonnet, …) without bundling them. Deploy it
alongside jaas when snippets reference shared libraries.

{{< helm-values data="joi-values" >}}
