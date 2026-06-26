---
title: JaaS vs jsonnet-controller
description: How JaaS separates rendering from deployment, where jsonnet-controller builds and applies Jsonnet in one controller.
tags: [comparison, jsonnet-controller]
---

[pelotech/jsonnet-controller](https://github.com/pelotech/jsonnet-controller) is
a Flux-style controller that builds Jsonnet inside the controller and applies the
result to the cluster, with a configuration model compatible with
[kubecfg](https://github.com/kubecfg/kubecfg). JaaS and jsonnet-controller both
turn Jsonnet into Kubernetes objects under Flux, but they draw the boundary
between rendering and deployment in different places.

## Coupled build-and-apply vs a rendering service

jsonnet-controller couples the two halves: one controller reads a Jsonnet
source, evaluates it, and applies the resulting objects to the cluster. The
rendered output lives inside the controller's reconcile loop; the unit of
configuration is "build this Jsonnet and apply it here."

JaaS separates them. The JaaS operator renders a `JsonnetSnippet` and publishes
the result as a content-addressed Flux `ExternalArtifact` — a tarball any
source-controller-speaking consumer can fetch. JaaS does not apply anything. A
separate consumer (a Flux `Kustomization`, a `HelmRelease`, or a
[stageset-controller](https://stageset.projects.metio.wtf/) `StageSet`) reads the
artifact and applies it. The rendered bytes are a first-class, addressable object
that more than one consumer can reference, pin to a revision, or roll back to.

That same rendering is also reachable over HTTP, so callers that are not Flux
consumers — a CI step, another service — can request a render from the same
engine that produces the in-cluster artifacts. See
[operator mode](/operator/operator-mode/) for how a snippet becomes an artifact.

## Where jsonnet-controller fits

jsonnet-controller is the more direct choice when its model matches your needs:

- **kubecfg compatibility.** If you already organise Jsonnet the kubecfg way —
  its import conventions, its top-level structure — jsonnet-controller consumes
  that directly without restructuring.
- **One object per build-and-apply.** When you want a single Flux-style
  resource that both renders a source and applies it, with no intermediate
  artifact to manage, jsonnet-controller keeps the pipeline to one moving part.

JaaS is the better fit when you want the rendered output to be an addressable,
revisioned artifact that several consumers can share, when you want the same
renderer available over HTTP to non-Flux callers, or when you want rendering and
deployment owned by separate, independently-evolving controllers.

## The deployment-side comparison

The comparison above is from the rendering angle — rendering as a service that
produces an artifact, versus build-and-apply in one controller. For the
deployment-side comparison against jsonnet-controller — ordered and gated apply,
health gating between stages, rollback — see stageset-controller's own page:
[stageset-controller vs jsonnet-controller](https://stageset.projects.metio.wtf/comparisons/jsonnet-controller/).
