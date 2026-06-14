<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Publishing OCI-delivered snippets as ExternalArtifacts

## Decision

To turn an OCI-delivered snippet into a published `ExternalArtifact`, reference
the artifact through a Flux `OCIRepository` and point a `JsonnetSnippet` at it
via `spec.sourceRef` — **not** by mounting the OCI image into the operator pod.
The operator gains no facility to scan a mounted snippet directory and
auto-publish what it finds.

Concretely, the supported shape is:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: dashboards
  namespace: monitoring
spec:
  interval: 10m
  url: oci://ghcr.io/metio/snippets/dashboards
  ref: { tag: latest }
---
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: dashboards
  namespace: monitoring
spec:
  serviceAccountName: dashboards-sa
  entryFile: main.jsonnet
  sourceRef:
    kind: OCIRepository
    name: dashboards
```

The reconciler fetches the `OCIRepository`'s artifact under the tenant's
impersonated ServiceAccount, extracts it, evaluates `entryFile`, and publishes
the namespaced `ExternalArtifact` — the same source-resolution path every other
`sourceRef` snippet uses. The OCI volume mount and the `OCIRepository` sourceRef
are two ways to consume the same image; the mount serves the synchronous HTTP
API, the sourceRef serves the publish path.

## Context

The chart mounts OCI images two ways, threaded into the binary as repeatable
flags:

- `additionalLibraries` → `-library-path` → `OCILibraries`. Scanned at startup
  and folded into library resolution. **Consumed by both** the HTTP handler and
  the operator reconciler.
- `snippets` → `-snippet-directory` → `handler.Config.SnippetDirectories`.
  **Consumed only by the HTTP handler** (`GET /<jsonnet-endpoint>/{snippet…}`).
  The reconciler has no `-snippet-directory` scan and no `OCISnippets` map.

A `JsonnetSnippet`'s bytes come from exactly one of (CEL-enforced at admission):
inline `spec.files`, or `spec.sourceRef` to a Flux source CR (GitRepository /
OCIRepository / Bucket / ExternalArtifact). An OCI snippet mount is therefore
invisible to the operator: mounting it produces no `ExternalArtifact`.

The question this records: should the operator learn to publish what is mounted
— a snippet-side analogue of `OCILibraries` — so an operator can both serve a
snippet over HTTP and publish it as an artifact from one mount? The answer is
no; reference the artifact instead.

## Why reference (sourceRef), not mount-scan

A `JsonnetSnippet` is the **published unit**, and the value of publishing it as
an `ExternalArtifact` is that downstream Flux consumers — `kustomize-controller`,
`helm-controller`, `stageset-controller` — get a versioned, digest-addressed
input whose revision is traceable to a source. Three properties of the mount-scan
alternative break that:

- **Provenance.** An `ExternalArtifact` carries a revision. With a `sourceRef`
  the revision derives from the upstream source's verified digest. A mount has
  no source revision behind it — the operator would have to synthesise one from
  pod-local file bytes, and that revision means nothing to a consumer trying to
  correlate "what produced this".
- **RBAC model.** Every tenant-side fetch runs under the snippet's impersonated
  ServiceAccount, so a tenant can publish only from sources its own RBAC can
  reach. A mounted directory is operator-pod filesystem state with no tenant
  identity attached; auto-publishing it would let a snippet emit an artifact from
  content the tenant never had access to fetch, sidestepping the impersonation
  boundary the whole operator is built around.
- **GitOps surface.** A `sourceRef` snippet's input is an ordinary CR an operator
  can `kubectl get`, a Flux reconciler can drive, and RBAC can govern. A mount is
  fixed at the deployment's argument list — opaque to `kubectl`, changeable only
  by a chart upgrade or pod restart.

`OCIRepository` already gives the platform exactly the OCI-delivery ergonomics
the mount was reached for — pull by tag or digest, periodic refresh, signature
verification via Flux's own OCI verification — while keeping provenance, tenancy,
and GitOps governance intact. There is nothing the mount-scan would add that the
sourceRef does not already cover better.

## Why the asymmetry with OCILibraries is correct

Libraries and snippets are deliberately treated differently, and the split is
not an oversight:

- A **library** is a static, operator-curated building block — shared helpers,
  label builders, org conventions. It is *imported by* snippets, never published
  on its own. Baking it into the image or a mount, fixed at deploy time, fits its
  nature, and it carries no independent artifact revision anyone needs to trace.
- A **snippet** is the producer whose output is the artifact. Its input wants to
  be a versioned, addressable source precisely *because* the output is published
  and consumed elsewhere.

So `OCILibraries` (mount-fed, both paths) and the snippet sourceRef model
(CR-driven, provenance-bearing) are each shaped to their role. Mounting a snippet
the way we mount a library would import the library's deploy-time-static,
provenance-free model into the one place it does not belong.

## Alternatives considered

- **Inline into `spec.files`.** Copy the OCI snippet's bytes into the CR. Works,
  but duplicates content into etcd, runs into object-size limits for anything
  non-trivial, and loses the OCI image as the source of truth. Fine for a handful
  of inline lines; wrong for an OCI-delivered bundle.
- **A new `OCISnippets` scan** mirroring `OCILibraries` (auto-derive a snippet,
  or a new local source kind reading a mounted dir). Rejected for the three
  reasons above — provenance, RBAC, GitOps surface. It is not impossible, but it
  would need its own decision resolving how a mount-sourced artifact gets a
  meaningful revision and how it reconciles with the impersonation boundary;
  nothing here precludes revisiting it if a concrete air-gapped, no-Flux-source
  use case demands it.

## Scope and compatibility

No code change: this records that the existing `sourceRef → OCIRepository` path
is the intended way to publish OCI-delivered snippets, and that a snippet-side
mount-scan is deliberately not offered. The producer→artifact→consumer
invariant the `stageset-controller` integration depends on is preserved —
snippets keep publishing namespaced `ExternalArtifact`s from traceable sources.

The `snippets` OCI mount remains supported for its actual purpose: serving
snippets over the synchronous HTTP API. The two delivery mechanisms coexist; an
operator who wants both behaviours for one snippet pairs a `snippets` mount (for
HTTP) with an `OCIRepository` + `JsonnetSnippet` (for the published artifact),
or simply standardises on the sourceRef path.
