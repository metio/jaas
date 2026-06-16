---
title: Kubernetes
description: Install JaaS on Kubernetes with the Helm chart in either of its two modes — OCI volume mounting or Flux CR-based.
tags: [installation, helm, kubernetes, operator]
---

JaaS ships as a container image at `ghcr.io/metio/jaas:latest` and as a Helm
chart at `oci://ghcr.io/metio/helm-charts/jaas`. Pre-built binaries for Linux,
macOS, and Windows are attached to each GitHub release for operators who prefer
to run the binary directly.

## Prerequisites

- Kubernetes 1.28 or later
- Helm 3.14 or later (OCI chart support)

For the Flux operator shape:

- [Flux](https://fluxcd.io/) v2.7.0 or later installed in the cluster (the
  `ExternalArtifact` CRD lands in v2.7.0)

## Two modes

The chart runs JaaS in one of two mutually exclusive modes in a single release.
Pick the one that matches your use case; you cannot combine them in one
`helm install`.

### Mode 1 — OCI volume mounting (HTTP renderer)

JaaS evaluates Jsonnet snippets on demand and returns JSON over HTTP. Snippets and
libraries are mounted into the pod from OCI artifacts as image volumes (the
`snippets` and `additionalLibraries` chart values), read straight from a registry.
There are no CRDs, no leader election, and no persistent storage — the pod is
stateless.

Minimal `helm install`:

```shell
helm install jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system --create-namespace \
  --wait
```

A minimal values snippet that mounts a snippet image and a library image:

```yaml
snippets:
  - name: dashboards
    image: ghcr.io/my-org/my-dashboards:latest
    mountPath: /snippets/dashboards

additionalLibraries:
  - name: grafonnet
    image: ghcr.io/metio/jsonnet-oci-images/grafonnet:latest
    mountPath: /libraries/grafonnet

arguments:
  snippetDirectories:
    - /snippets/dashboards
  libraryPaths:
    - /libraries/grafonnet
```

The Jsonnet HTTP server listens on port `8080` (configurable via
`arguments.port`). Call it with:

```shell
curl http://<service>:8080/jsonnet/my-dashboard
```

### Mode 2 — Flux CR-based (operator)

JaaS watches `JsonnetSnippet` and `JsonnetLibrary` CRs, evaluates snippets, and
publishes the results as `ExternalArtifact` resources. Downstream Flux consumers
(kustomize-controller, helm-controller, stageset-controller) fetch the rendered
JSON from the artifact server.

**Static OCI mounts (`snippets`, `additionalLibraries`) and `operator.enabled:
true` are mutually exclusive in one release.** The chart's pre-install preflight
rejects the combination. Use OCI volume mounting when you want OCI-mounted snippets
served over HTTP without CRs; use the Flux CR-based mode when you want
Kubernetes-native reconciliation that publishes artifacts.

Minimal `helm install`:

```shell
helm install jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system --create-namespace \
  --set operator.enabled=true \
  --set operator.storage.persistence.enabled=true \
  --wait
```

A minimal values snippet for the operator shape:

```yaml
operator:
  enabled: true
  storage:
    # local backend with a PVC — enough for a single-replica install.
    # For multi-replica HA, switch to backend: s3 (see /installation/production/).
    backend: local
    persistence:
      enabled: true
      size: 10Gi
```

The operator publishes artifacts at the URL configured via
`operator.storageBaseURL` (required). Downstream snippets reference that URL in
their `ExternalArtifact` status.

## Upgrading

```shell
helm upgrade jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system \
  -f my-values.yaml \
  --wait
```

The chart ships CRDs under `templates/` so `helm upgrade` applies schema changes
automatically. Check [MIGRATIONS.md](https://github.com/metio/jaas/blob/main/MIGRATIONS.md)
before upgrading across releases that change `spec.selector.matchLabels` — those
require a manual `kubectl delete deploy/jaas` first.

## Next steps

- [Quickstart tutorial](/tutorials/quickstart/) — five steps from `helm install`
  to a published artifact.
- [Configuration reference](/installation/configuration/) — every flag and its
  chart value equivalent.
- [Production hardening](/installation/production/) — storage, observability, the
  admission webhook, and multi-replica HA.
