---
title: vs Flux Operator ResourceSet
description: JaaS renders Jsonnet into an ExternalArtifact; the Flux Operator's ResourceSet fans out instances from inputs — complementary tools that compose.
tags: [comparison, flux-operator, resourceset]
---

JaaS and the [Flux Operator](https://fluxoperator.dev/)'s `ResourceSet` solve
different halves of the same delivery problem, so reach for them together rather
than choosing between them. JaaS *renders content*: it evaluates a `JsonnetSnippet`
and publishes the resulting JSON as a Flux `ExternalArtifact`. A `ResourceSet`
*fans out instances*: it takes a set of inputs and templates one copy of a set of
Kubernetes or Flux objects per input. Neither does the other's job — a
`ResourceSet` has no Jsonnet evaluator, and JaaS has no input matrix — so the two
slot together cleanly.

## What each one produces

JaaS turns Jsonnet into bytes. You hand the operator a `JsonnetSnippet` (inline
`spec.files` or a `spec.sourceRef` to a Flux source), it runs go-jsonnet, and it
writes the evaluated JSON to a content-addressed `ExternalArtifact` that any Flux
consumer can fetch and pin. The input is a Jsonnet program plus its top-level
arguments and external variables; the output is one rendered document.

`ResourceSet` turns a list of inputs into a fleet of objects. You give it
`.spec.inputs` (static value sets) and `.spec.inputsFrom` (references to
`ResourceSetInputProvider` objects that export inputs from Git branches, pull
requests, OCI tags, or a static list), combine them with `.spec.inputStrategy`
(`Flatten` or `Permute`), and template the result into `.spec.resources` (or
`.spec.resourcesTemplate`) using Go templates with `<< inputs.x >>` delimiters.
The output is N rendered objects, one per resolved input.

## Where each one fits

| You want to… | Reach for |
|---|---|
| Evaluate Jsonnet (grafonnet dashboards, manifest libraries) into JSON | JaaS `JsonnetSnippet` |
| Re-render automatically when a Flux source republishes | JaaS `spec.sourceRef` |
| Create one copy of a resource per tenant, cluster, or environment | `ResourceSet` |
| Drive that fan-out from Git branches, PRs, or OCI tags | `ResourceSetInputProvider` |
| Gate when inputs refresh to a time window | `ResourceSetInputProvider` `.spec.schedule` |

A `ResourceSet` can template a `JsonnetSnippet` as one of its `.spec.resources`,
which is exactly where they compose: the `ResourceSet` produces one snippet per
input, and JaaS renders each one. You get per-tenant or per-cluster Jsonnet output
without authoring a snippet by hand for every instance.

## They compose

The practical pattern — a `ResourceSet` that templates a `JsonnetSnippet` per
input so JaaS renders per-cluster or per-tenant content — has a complete, working
example on the [Flux Operator integration](/usage/flux-operator/) page.

Neither tool replaces the other: drop the Flux Operator if you only ever render
one document per snippet, and drop JaaS if your objects need no Jsonnet
evaluation. When both are true — many instances, each rendered from Jsonnet — run
them side by side.
