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

- A [Kubernetes](https://kubernetes.io/) cluster, **v1.28 or later**, with
  `kubectl` configured against it.
- [Helm](https://helm.sh/) **v3.14 or later** — OCI chart support is required to
  pull the chart from `ghcr.io`.

The Flux CR-based mode (below) additionally needs:

- [Flux](https://fluxcd.io/) **v2.7.0 or later** in the cluster — the
  `ExternalArtifact` CRD that JaaS publishes lands in v2.7.0.
- [cert-manager](https://cert-manager.io/) — **only** if you set the admission
  webhook to `cert-manager` mode. The chart defaults to `self-signed`, which
  provisions and rotates the webhook's TLS in-process and needs no cert-manager;
  see [Production](/installation/production/#admission-webhook-tls) for the
  trade-off.

The OCI volume-mounting mode needs neither Flux nor cert-manager.

## Install and update

`helm upgrade --install` is idempotent: the same command installs the chart the
first time and applies your changes on every subsequent run, so it's the only
deploy command you need. To update later, re-run it with an updated `--values`
file or `--set` flags.

The chart runs JaaS in one of two mutually exclusive modes in a single release.
Pick the one that matches your use case; you **cannot** combine them in one
release — the chart's pre-install preflight rejects the combination.

### Mode 1 — OCI volume mounting (HTTP renderer)

JaaS evaluates Jsonnet snippets on demand and returns JSON over HTTP. Snippets and
libraries are mounted into the pod from OCI artifacts as image volumes (the
`snippets` and `additionalLibraries` chart values), read straight from a registry.
There are no CRDs, no leader election, and no persistent storage — the pod is
stateless.

```shell
helm upgrade --install jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system --create-namespace \
  --values my-values.yaml \
  --wait
```

A minimal `my-values.yaml` that mounts a snippet image and a library image:

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
`arguments.port`).

### Mode 2 — Flux CR-based (operator)

JaaS watches `JsonnetSnippet` and `JsonnetLibrary` CRs, evaluates snippets, and
publishes the results as `ExternalArtifact` resources. Downstream Flux consumers
(kustomize-controller, helm-controller, stageset-controller) fetch the rendered
JSON from the artifact server.

```shell
helm upgrade --install jaas oci://ghcr.io/metio/helm-charts/jaas \
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
    # local backend with a PVC — enough for a single-replica install. For
    # multi-replica HA, switch to backend: s3 (see /installation/production/).
    backend: local
    persistence:
      enabled: true
      size: 10Gi
```

The operator publishes artifacts at the URL configured via
`operator.storageBaseURL` (required); downstream snippets reference that URL in
their `ExternalArtifact` status.

### How CRDs are handled

The chart ships its CRDs (`JsonnetSnippet`, `JsonnetLibrary`) inside the regular
templates — not Helm's special `crds/` directory — so a `helm upgrade --install`
applies schema changes like any other resource, governed by `crds.create`
(default `true`). The CRDs carry `helm.sh/resource-policy: keep`, so a
`helm uninstall` leaves them — and your existing resources — in place; remove them
by hand only if you really mean to.

Check [MIGRATIONS.md](https://github.com/metio/jaas/blob/main/MIGRATIONS.md)
before upgrading across a release that changes an immutable field such as a
Deployment's `spec.selector.matchLabels` — those require a manual
`kubectl --namespace jaas-system delete deploy jaas` first.

If you manage CRDs out of band, the raw definitions are published in the
repository under `config/crd/bases/` and can be applied with
`kubectl apply --server-side -f`.

## Customize

Every setting the chart exposes — the two modes above, storage backend, leader
election, the admission webhook, NetworkPolicy, service mesh, metrics, and the
rest — is a Helm value. Two references cover them:

- [Helm chart values](/installation/helm-values/) — the full values reference,
  generated from the chart's own schema.
- [Configuration reference](/installation/configuration/) — every binary flag and
  the chart value that drives it.

For production sizing — S3 storage, multi-replica HA, observability, and webhook
hardening — see the [Production guide](/installation/production/).

## Verify

For the operator shape, confirm the Deployment is available and the CRDs are
registered:

```shell
kubectl --namespace jaas-system rollout status deploy/jaas
kubectl get crd jsonnetsnippets.jaas.metio.wtf jsonnetlibraries.jaas.metio.wtf
```

For the HTTP renderer, confirm the pod is ready and the endpoint answers:

```shell
kubectl --namespace jaas-system get pods --selector app.kubernetes.io/name=jaas
kubectl --namespace jaas-system port-forward svc/jaas 8080:8080 &
curl http://localhost:8080/jsonnet/my-dashboard
```

## Next steps

- [Quickstart tutorial](/tutorials/quickstart/) — five steps from a Helm install
  to a published artifact.
- [Production hardening](/installation/production/) — storage, observability, the
  admission webhook, and multi-replica HA.
