<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Migrating from Tanka

[Grafana Tanka](https://tanka.dev) renders Kubernetes manifests from Jsonnet on
the client and applies them with `tk apply`. JaaS renders the same Jsonnet
**in-cluster** and publishes the result as a Flux `ExternalArtifact`, which Flux
— and, for ordered rollouts, the
[stageset-controller](https://github.com/metio/stageset-controller) — then
applies. You keep your Jsonnet and your libraries; you trade a client-side
`tk apply` for a GitOps pull loop.

This guide maps every Tanka concept to its JaaS equivalent and walks a real
environment through the conversion.

## Read this first: your imports resolve identically

JaaS's importer implements the **same resolution as `jsonnet -J vendor`** — the
exact scheme Tanka uses. A bare `import 'foo/main.libsonnet'` finds the library
by alias; an absolute `import 'github.com/.../gen/...'` resolves against the
vendored tree; sibling and `../` relative imports work from the importing file.
So a `jb`-vendored `vendor/` (k8s-libsonnet, grafonnet, …) renders
**byte-for-byte the same** through JaaS as it does under `tk show`. Migration is
mostly about *where the files live*, not *rewriting Jsonnet*.

One behaviour to plan for: Tanka walks the evaluated object and extracts every
nested `{apiVersion, kind, …}` into a resource stream. JaaS publishes exactly
what your entry file evaluates to. Make the entry file emit a **flat manifest
stream** — wrap resources in a `v1` `List`, or `std.objectValues(...)` over your
Tanka object — so the consuming Flux `Kustomization` applies every resource.

## Concept map

| Tanka | JaaS / Flux / stageset |
|---|---|
| `vendor/` (jb-installed libs) | `JsonnetLibrary` with a `sourceRef`, or OCI-mounted libraries (the JOI pattern) |
| `lib/` (project-shared libs) | `JsonnetLibrary` with inline `files` in the same namespace |
| `environments/<env>/main.jsonnet` | one `JsonnetSnippet` (`spec.entryFile` + `spec.files`/`spec.sourceRef`) |
| `import` resolution (`-J vendor`) | identical — JaaS's in-memory importer |
| per-env values in `spec.json` / conditionals | `spec.externalVariables` (`std.extVar`) and `spec.tlas` (top-level args) |
| `spec.json` `namespace` | Flux `Kustomization.spec.targetNamespace` (or set it in Jsonnet) |
| `spec.json` `injectLabels` | Flux `Kustomization.spec.commonMetadata.labels` (or stageset patches) |
| `spec.json` `apiServer` (which cluster) | the cluster where the artifact is consumed — not a JaaS field |
| `tk show` / `tk export` (render) | the JaaS operator, continuously → `ExternalArtifact` |
| `tk apply` (push) | Flux kustomize-/helm-controller (pull) |
| `tk diff` | Flux drift detection; stageset verification between stages |
| _(no equivalent)_ | ordered, gated, multi-stage rollout via stageset-controller |
| `tk env list` | `kubectl get jsonnetsnippets -A` |

## Step by step

### 1. Move the libraries

Your `vendor/` and `lib/` trees become `JsonnetLibrary` resources (or
OCI-mounted libraries):

- **Shared, versioned libraries** (k8s-libsonnet, grafonnet) — publish them as
  single-layer OCI images and consume them as a `JsonnetLibrary` whose
  `sourceRef` is an `OCIRepository` (the
  [JOI](https://github.com/metio/jsonnet-oci-images) pattern), or mount them
  statically via the chart's `additionalLibraries`. The import alias is
  preserved either way.
- **Project-local `lib/`** — a `JsonnetLibrary` with inline `files`, referenced
  from the snippet via `spec.libraries` with an `importPath` that matches the
  alias your Jsonnet imports.

### 2. Turn each environment into a JsonnetSnippet

`environments/team-a/main.jsonnet` becomes one snippet. Prefer `spec.sourceRef`
(a Flux `GitRepository`/`OCIRepository`/`Bucket`) over inline `files` for real
repos, so Flux versions the source and JaaS re-renders on every commit:

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: team-a
  namespace: team-a
spec:
  serviceAccountName: team-a-deployer
  entryFile: main.jsonnet
  sourceRef:
    kind: GitRepository
    name: team-a-config
    path: environments/team-a
  libraries:
    - kind: JsonnetLibrary
      name: k8s-libsonnet
      importPath: k        # the alias your `import 'k/...'` uses
```

### 3. Parameterize per environment

Tanka differentiates environments with `spec.json` plus Jsonnet conditionals; in
JaaS the same knobs become snippet fields:

- `spec.externalVariables` → `std.extVar('name')`
- `spec.tlas` → top-level arguments (a single value is a string; multiple values
  become a JSON array)

```yaml
spec:
  externalVariables:
    cluster: prod-eu
  tlas:
    replicas: ["3"]
```

So `environments/{dev,prod}` sharing one `lib/` become two `JsonnetSnippet`s
that reference the same `JsonnetLibrary` and differ only in their
`externalVariables` / `tlas`.

### 4. Namespace and common labels

Tanka's `spec.json` injects a default `namespace` and `injectLabels`. JaaS
renders pure manifests, so apply these **downstream**, where they are
first-class Flux features, on the `Kustomization` that consumes the artifact:

```yaml
spec:
  targetNamespace: team-a
  commonMetadata:
    labels:
      app.kubernetes.io/part-of: team-a
```

(You can also set them in Jsonnet, or with stageset patches.) Keeping rendering
free of placement leaves cluster/namespace targeting to the GitOps layer.

### 5. Replace apply with GitOps

`tk apply` goes away. A Flux `Kustomization` (or `HelmRelease`) points its
`sourceRef` at the snippet's `ExternalArtifact`; Flux applies it and
continuously reconciles it. For an ordered, gated rollout across stages
(dev → staging → prod, with health gates between them), wrap the artifacts in a
`StageSet`.

## Worked example

Tanka layout:

```text
environments/
  promtail/
    main.jsonnet      # imports k, project promtail lib
    spec.json         # namespace: logging, apiServer: ...
lib/
  promtail/
    promtail.libsonnet
vendor/
  github.com/jsonnet-libs/k8s-libsonnet/...
```

JaaS equivalent:

```yaml
# shared k8s library (here as an OCIRepository-backed JsonnetLibrary)
apiVersion: jaas.metio.wtf/v1
kind: JsonnetLibrary
metadata: { name: k8s-libsonnet, namespace: logging }
spec:
  sourceRef:
    kind: OCIRepository
    name: k8s-libsonnet
---
# project-local promtail library
apiVersion: jaas.metio.wtf/v1
kind: JsonnetLibrary
metadata: { name: promtail-lib, namespace: logging }
spec:
  files:
    promtail.libsonnet: |
      { new(): { /* ... */ } }
---
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata: { name: promtail, namespace: logging }
spec:
  serviceAccountName: logging-deployer
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      local k = import 'k/main.libsonnet';
      local promtail = import 'promtail/promtail.libsonnet';
      // flat manifest stream, not a nested Tanka object
      { apiVersion: 'v1', kind: 'List', items: std.objectValues(promtail.new()) }
  libraries:
    - { kind: JsonnetLibrary, name: k8s-libsonnet, importPath: k }
    - { kind: JsonnetLibrary, name: promtail-lib, importPath: promtail }
```

A Flux `Kustomization` whose `sourceRef` is the `promtail` `ExternalArtifact`,
with `targetNamespace: logging`, replaces `tk apply`.

## What you gain — and what changes

You gain a true pull-based GitOps loop (no cluster credentials on laptops or
CI), continuous reconciliation and drift correction (Tanka renders once, on
demand), server-side rendering (developers no longer need the Jsonnet toolchain
or the vendor tree to ship), multi-tenant isolation (each snippet renders under
its tenant ServiceAccount's RBAC), and — through stageset-controller — ordered,
gated progressive delivery that Tanka has no equivalent for.

What changes: there is no single `tk diff` preview today (use Flux's diff and
stageset's between-stage verification), and namespace/label injection moves from
`spec.json` to the consuming Flux `Kustomization`. Your Jsonnet and libraries
come across unchanged.
