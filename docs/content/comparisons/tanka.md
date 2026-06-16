---
title: JaaS vs Tanka
description: How JaaS replaces a client-side `tk apply` workflow with server-side rendering and a Flux pull loop.
tags: [comparison, tanka]
---

[Grafana Tanka](https://tanka.dev) and JaaS both render Kubernetes manifests
from Jsonnet, and both build on the same [`jsonnet-bundler`](https://github.com/jsonnet-bundler/jsonnet-bundler)
vendoring conventions. The difference is *where the rendering runs and how the
result reaches the cluster*.

## The two models

Tanka renders and applies from a developer workstation or a CI runner. You
organise your code as environments (`environments/<env>/main.jsonnet` plus a
`spec.json`), run `tk show` or `tk export` to inspect the rendered objects, and
`tk apply` to push them to the cluster the environment names in its `apiServer`
field. The workstation or CI runner needs the Jsonnet toolchain, the vendored
library tree, and credentials for the target cluster.

JaaS renders in-cluster. A `JsonnetSnippet` names the same Jsonnet entry file;
the JaaS operator evaluates it continuously and publishes the result as a Flux
`ExternalArtifact`. A Flux `Kustomization` (or `HelmRelease`, or for ordered
rollouts a [stageset-controller](https://stageset.projects.metio.wtf/)
`StageSet`) consumes that artifact and applies it through the cluster's own
GitOps pull loop. No workstation or CI runner holds cluster credentials, and
developers do not need the Jsonnet toolchain or the vendor tree to ship a
change.

## When Tanka is the better fit

Tanka stays the stronger choice when its model matches the work:

- **Ad-hoc and exploratory renders.** `tk show` / `tk diff` give an immediate,
  local preview of exactly what would be applied, with no operator, no
  `ExternalArtifact`, and no consumer to configure.
- **Environments-as-code as the organising abstraction.** Tanka's
  environment/`spec.json` model — `namespace`, `injectLabels`, `apiServer`
  per environment, `tk env list` to enumerate them — is a first-class feature
  with no direct JaaS equivalent; JaaS pushes namespace and label concerns down
  to the consuming Flux `Kustomization` instead.
- **Direct, imperative apply.** When a human running `tk apply` against a
  named cluster is the intended workflow — small teams, bootstrap steps,
  break-glass operations — a pull loop adds machinery you may not need.

JaaS becomes the better fit when you want a pull-based GitOps loop, continuous
reconciliation and drift correction, server-side rendering so laptops and CI
hold no cluster credentials, per-tenant RBAC isolation on each render, and —
through stageset-controller — ordered, gated progressive delivery.

## Your imports resolve identically

JaaS's importer implements the **same resolution as `jsonnet -J vendor`**, the
scheme Tanka uses. A bare `import 'foo/main.libsonnet'` finds the library by
alias; an absolute `import 'github.com/.../gen/...'` resolves against the
vendored tree; sibling and `../` relative imports resolve from the importing
file. A `jb`-vendored tree (k8s-libsonnet, grafonnet, and the like) renders the
same bytes through JaaS as it does under `tk show`. Migration is mostly about
*where the files live*, not *rewriting Jsonnet*. See
[Jsonnet libraries](/usage/jsonnet-libraries/) for how libraries reach a
snippet.

One behaviour to plan for: Tanka walks the evaluated object and extracts every
nested `{apiVersion, kind, …}` into a resource stream. JaaS publishes exactly
what the entry file evaluates to. Make the entry file emit a flat manifest
stream — wrap resources in a `v1` `List`, or apply `std.objectValues(...)` over
your Tanka object — so the consuming Flux `Kustomization` applies every
resource.

## A migration path

| Tanka | JaaS / Flux |
|---|---|
| `vendor/` (jb-installed libs) | `JsonnetLibrary` with a `sourceRef`, or OCI-mounted libraries |
| `lib/` (project-shared libs) | `JsonnetLibrary` with inline `files` in the same namespace |
| `environments/<env>/main.jsonnet` | one `JsonnetSnippet` (`spec.entryFile` + `spec.files`/`spec.sourceRef`) |
| `import` resolution (`-J vendor`) | identical — JaaS's in-memory importer |
| per-env `spec.json` / conditionals | `spec.externalVariables` (`std.extVar`) and `spec.tlas` (top-level args) |
| `spec.json` `namespace` | Flux `Kustomization.spec.targetNamespace` |
| `spec.json` `injectLabels` | Flux `Kustomization.spec.commonMetadata.labels` |
| `tk show` / `tk export` | the JaaS operator, continuously → `ExternalArtifact` |
| `tk apply` | Flux kustomize-/helm-controller (pull) |
| `tk diff` | Flux drift detection; stageset verification between stages |
| `tk env list` | `kubectl get jsonnetsnippets -A` |

The conversion in three moves:

1. **Move the libraries.** Shared, versioned libraries (k8s-libsonnet,
   grafonnet) become a `JsonnetLibrary` backed by an `OCIRepository`, or a
   static OCI-mounted library on the operator. A project-local `lib/` becomes a
   `JsonnetLibrary` with inline `files`. The import alias is preserved either
   way.

2. **Turn each environment into a `JsonnetSnippet`.** `environments/team-a/main.jsonnet`
   becomes one snippet. Prefer `spec.sourceRef` (a Flux
   `GitRepository`/`OCIRepository`/`Bucket`) over inline `files` for real
   repositories, so Flux versions the source and JaaS re-renders on every
   commit. Per-environment differences move to `spec.externalVariables` and
   `spec.tlas`, so two environments sharing one library become two snippets that
   differ only in those fields.

3. **Replace apply with GitOps.** `tk apply` goes away. A Flux `Kustomization`
   points its `sourceRef` at the snippet's `ExternalArtifact`; Flux applies it
   and reconciles it continuously. The
   [Deploying manifests](/tutorials/deploying-manifests/) tutorial walks this
   end to end.

## What changes

There is no single `tk diff` preview — use Flux's drift detection and
stageset's between-stage verification instead. Namespace and label injection moves from
`spec.json` to the consuming Flux `Kustomization`, where both are first-class
fields. Your Jsonnet and libraries come across unchanged.
