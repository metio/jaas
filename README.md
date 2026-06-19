<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
 -->

# Jsonnet-as-a-Service (JaaS)

**JaaS** evaluates [Jsonnet](https://jsonnet.org/) and returns JSON. Run it as a
stateless HTTP renderer (`GET /jsonnet/<snippet>`), or enable operator mode to
watch `JsonnetSnippet` and `JsonnetLibrary` resources and publish the rendered
output as a Flux
[`ExternalArtifact`](https://fluxcd.io/flux/components/source/externalartifacts/)
that any consumer can deploy — the two flagship pairings are
[grafana-operator](https://grafana.github.io/grafana-operator/) for dashboards and
[stageset-controller](https://stageset.projects.metio.wtf/) for manifest delivery.

📖 Documentation — installation, usage, API reference, tutorials, and
contributing — lives at <https://jaas.projects.metio.wtf/>.

📦 The Helm chart is published at `oci://ghcr.io/metio/helm-charts/jaas` and
listed on [ArtifactHub](https://artifacthub.io/packages/helm/jaas/jaas).

Licensed under [0BSD](LICENSE); the repository is [REUSE](https://reuse.software/)
compliant.
