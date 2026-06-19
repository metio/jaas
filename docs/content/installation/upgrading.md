---
title: Upgrading
description: Actions required when upgrading JaaS between releases — a release with no section here needs no migration.
tags: [installation, upgrade, migration, helm]
---

This page lists the actions required when upgrading JaaS. A release with no
section here needs no migration — a plain `helm upgrade` (or new image tag)
suffices.

## 2026.6.15

JaaS gains an opt-in operator mode that watches `JsonnetSnippet` and `JsonnetLibrary` CRDs (`jaas.metio.wtf/v1`) and publishes evaluated results as Flux `ExternalArtifact` resources. **Operator mode ships as GA on this release** — the CRDs are at `v1` (no `v1alpha1`/`v1beta1` ever published), and the wire contracts in scope are committed: every `Reason*` constant on the Ready condition, the `ErrCode*` HTTP error codes, the `ExternalArtifact` spec/status shape, the chart values keys under `operator.*`, and the `JsonnetSnippet` / `JsonnetLibrary` field set. Breaking changes to any of these require a new CRD version + deprecation window, never an in-place rename. See the [API reference](/api/) for the committed field sets.

The container image now ships from GitHub Container Registry: `ghcr.io/metio/jaas` (previously `docker.io/metio/jaas`). The chart's `image.registry` default moves to `ghcr.io` to match, so a plain `helm upgrade` pulls from the new location automatically. Pin-by-registry installs (`--set image.registry=docker.io`) and any infrastructure that mirrors or allow-lists the image by registry must repoint at `ghcr.io`; the Docker Hub repository is no longer published.

The image is now a multi-arch manifest covering `linux/amd64`, `arm64`, `arm/v7`, `ppc64le`, `riscv64`, and `s390x`, so the operator schedules onto any node architecture (and matches the JOI library images it consumes). The runtime base also moves from `cgr.dev/chainguard/static` to `gcr.io/distroless/static:nonroot` — both are shell-less static bases, so behavior is unchanged, but image-signature/SBOM tooling that pinned the chainguard base digest needs updating.

The Helm chart now lives in the [metio/helm-charts](https://github.com/metio/helm-charts/tree/main/charts/jaas) monorepo and is published at **`oci://ghcr.io/metio/helm-charts/jaas`** (previously `oci://ghcr.io/metio/jaas`, released from this repo). Repoint your `helm install` / `helm upgrade` source — for example `helm upgrade --install jaas oci://ghcr.io/metio/helm-charts/jaas …`. The chart's templates, values, and CRD set are unchanged by the move; only the artifact's location differs. Chart `version` and `appVersion` now advance independently of the jaas binary, since the chart is released on the helm-charts repo's own cadence.

The HTTP-only evaluator continues to work unchanged — operators that don't enable the new mode see no behavioral difference.

A new flag `--max-concurrent-evals` caps in-flight Jsonnet evaluations across both surfaces; excess requests return HTTP `503 evaluation_unavailable` or — on the operator path — a `Warning EvalUnavailable` event plus `RequeueAfter`. The default (`max(GOMAXPROCS*4, 16)`) is sized to bound worst-case goroutine pile-up under a runaway snippet; set to `0` to disable the gate entirely. The chart's `arguments.maxConcurrentEvals` value threads through to the flag. A new wire-stable HTTP error code `evaluation_unavailable` joins the existing four (`method_not_allowed`, `snippet_not_found`, `evaluation_timeout`, `evaluation_failed`) — programmatic callers that switch on the error code should add a branch for 503/`evaluation_unavailable`; treat it as transient and retry with backoff.

To enable the operator mode, pass `--enable-flux-integration` along with `--storage-path` (a writable directory for tarballs) and `--storage-base-url` (the URL prefix downstream Flux consumers will dereference). The operator opens a third HTTP server at `--storage-listen-address:--storage-port` (defaulting `0.0.0.0:8082`) to serve artifacts.

The validating admission webhook is independently opt-in via `--enable-webhook` and requires a TLS cert/key in `--webhook-cert-dir`. The reconciler enforces the same ext-var conflict invariant as a fallback, so the webhook is purely a UX improvement on `kubectl apply`.

CRDs ship with the helm chart under its `templates/crd-*.yaml`. `helm install` provisions them on first apply and `helm upgrade` applies any schema changes automatically — no manual `kubectl apply` of CRDs required. Each CRD template carries a `helm.sh/resource-policy: keep` annotation so `helm uninstall` does NOT wipe the CRDs (or every `JsonnetSnippet` / `JsonnetLibrary` bound to them). An operator who genuinely wants to remove the CRDs runs `kubectl delete crd jsonnetsnippets.jaas.metio.wtf jsonnetlibraries.jaas.metio.wtf` by hand. The `ValidatingWebhookConfiguration`, `Service`s, RBAC, and the optional cert-manager `Certificate` for the webhook are rendered when `operator.enabled` / `operator.webhook.enabled` are toggled in the chart values.

A new flag `--artifact-gc-grace` (default `5m`) sets the minimum time a superseded artifact revision is retained before storage GC removes it. Closes the pin→fetch race in which a Flux consumer reads `status.artifact` a moment before the operator garbage-collects the superseded revision and then 404s on the URL. Supersession time is derived from on-disk storage metadata so the window survives operator restarts. `0` restores the prior eager-prune behavior; the snippet teardown path (finalizer `Withdraw`) is unaffected. See [Revision retention and rollback](/usage/storage-and-ha/#revision-retention-and-rollback) for when to leave the default vs raise `spec.history` for deliberate rollback retention.

The `ExternalArtifact.spec.sourceRef` JaaS writes — `{apiVersion: jaas.metio.wtf/v1, kind: JsonnetSnippet, name: <snippet name>}` in the snippet's own namespace — is now documented as a public contract. Producer-aware consumers (`stageset-controller` and others) reverse-resolve `JsonnetSnippet` references through exactly this triple; renaming a field, splitting `apiVersion` into `group`/`version`, or moving the back-pointer out of `spec.sourceRef` is a breaking change. No spec change in this release — the shape was already produced as documented — but the commitment to it is now load-bearing.

### Known limits at GA

- **`vm.EvaluateFile` has no context cancellation** (go-jsonnet upstream constraint). An evaluation that exceeds `--evaluation-timeout` keeps a goroutine alive until the snippet finishes naturally — observable via the `jaas_eval_outstanding_timed_out` Prometheus gauge. `--max-concurrent-evals` caps how many such goroutines can pile up at once; the cap, combined with `--max-stack` and `--evaluation-timeout`, bounds the worst-case blast radius. The opt-in PrometheusRule template ships `JaaSEvalSaturation` + `JaaSEvalRejected` alerts; [Eval saturation](/runbooks/eval-saturation/) covers the runaway-snippet vs. genuine-load diagnosis paths.
- **Watch-namespace RBAC pivots automatically** when `operator.watchNamespaces` is set. The cluster-scoped `operator-cluster` role (CRDs + optional VWC) stays bound cluster-wide. The `operator-tenants` role (snippets, libraries, ExternalArtifact, Flux sources, SAs, token mint, events) is bound either cluster-wide (default, when `watchNamespaces` is empty) or via one `RoleBinding` per listed namespace (when `watchNamespaces` is non-empty). Both the deployment's `-watch-namespaces` arg and the rendered RBAC are driven from the same value, so multi-tenant installs running disjoint operator instances per tenant-group get tight RBAC + cache scoping automatically.
- **Single-replica is the supported topology by default.** Leader election ships on so scaling out works without double-reconciliation, but the chart-default storage backend is `local` with an emptyDir / RWO PVC — both effectively single-pod. Multi-replica HA needs `operator.storage.backend: s3` plus a shared S3 bucket.

## 2026.5.25

The labels in the Deployment and PDB `spec.selector.matchLabels` have changed. Kubernetes treats those fields as immutable, so `helm upgrade` from an older chart version will fail with `field is immutable`.

Delete both resources before upgrading:

```shell
kubectl -n <namespace> delete deployment jaas
kubectl -n <namespace> delete pdb jaas --ignore-not-found
```

Expect a brief outage while the new Pod comes up.
