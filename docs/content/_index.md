---
title: Jsonnet-as-a-Service
---

# Jsonnet-as-a-Service

**Jsonnet-as-a-Service (JaaS)** evaluates [Jsonnet](https://jsonnet.org/) and
returns JSON. It runs in one of two modes:

- **OCI volume mounting** — the chart mounts your snippets and libraries from OCI
  artifacts as image volumes, and JaaS serves the evaluated JSON over HTTP
  (`GET /jsonnet/<snippet>`). Static content, no custom resources.
- **Flux CR-based** — JaaS watches `JsonnetSnippet` and `JsonnetLibrary` resources
  and publishes the rendered output as a Flux
  [`ExternalArtifact`](https://fluxcd.io/flux/components/source/externalartifacts/)
  that any Flux consumer deploys.

The two modes are mutually exclusive in one chart release;
[Installation](/installation/kubernetes/) covers choosing one.

## What teams build with it

- **Grafana dashboards with grafana-operator.** Author dashboards in Jsonnet
  (grafonnet), let JaaS render them, and have the
  [grafana-operator](https://grafana.github.io/grafana-operator/) reconcile the
  result into Grafana. See [Grafana dashboards](/tutorials/grafana-dashboards/).
- **Kubernetes manifests with stageset-controller.** Render manifests from
  Jsonnet and roll them out in ordered, gated stages with
  [stageset-controller](https://stageset.projects.metio.wtf/). See
  [Deploying manifests with StageSet](/tutorials/deploying-manifests/).

Both build on the same core: a snippet renders to an `ExternalArtifact`, and a
downstream controller consumes it. JaaS only renders — what happens to the JSON is
the consumer's concern, documented on that consumer's own site.

## Where to start

- [Quickstart](/tutorials/quickstart/) — from a Helm install to a published
  artifact in a few steps.
- [Tutorials](/tutorials/) — the two integrations above, plus running JaaS as a
  cluster-free local renderer.
- [Usage](/usage/) — one page per feature, for both the HTTP renderer and the
  operator.
- [Installation](/installation/) — Helm install, production hardening, and the
  full configuration reference.
- [API reference](/api/) — every field of `JsonnetSnippet`, `JsonnetLibrary`, and
  the `ExternalArtifact` output contract.
- [Runbooks](/runbooks/) — symptom, cause, and remediation for every
  Ready-condition reason.

## Project

- Source, releases, and the container image: [github.com/metio/jaas](https://github.com/metio/jaas)
- Helm chart: [`oci://ghcr.io/metio/helm-charts/jaas`](https://github.com/metio/helm-charts/tree/main/charts/jaas)
