---
title: JaaS vs the jsonnet CLI
description: What the JaaS service adds over running the jsonnet and jb command-line tools yourself.
tags: [comparison, jsonnet-cli]
---

The [`jsonnet`](https://jsonnet.org/) command-line tool — usually paired with
[`jsonnet-bundler`](https://github.com/jsonnet-bundler/jsonnet-bundler) (`jb`)
for vendoring libraries — evaluates Jsonnet to JSON on your machine. JaaS runs
the **same go-jsonnet core** as a service. This is not a question of which
implementation is correct; it is a question of *where the evaluation runs and
what surrounds it*.

## What the service adds

Over a local binary invocation, JaaS adds:

- **An HTTP endpoint other systems can call.** `GET /jsonnet/<snippet>` returns
  the evaluated JSON, with Top Level Arguments supplied as query parameters and
  external variables configured on the service. Anything that speaks HTTP can
  request a render without installing the toolchain or the vendor tree. See the
  [rendering endpoint](/usage/rendering-endpoint/) usage page.
- **An operator that turns a snippet into a revisioned Flux artifact.** With
  `--enable-flux-integration`, a `JsonnetSnippet` is evaluated continuously and
  published as a content-addressed `ExternalArtifact` that Flux consumers apply
  in-cluster — re-rendered automatically when its source changes. See
  [operator mode](/usage/operator-mode/).
- **Import resolution that matches `jsonnet -J vendor`.** JaaS resolves imports
  with the same semantics as the CLI's JPATH/vendor search, so the JSON a
  snippet produces under the service matches what the CLI produces locally.
- **Evaluation caps.** `--evaluation-timeout` bounds wall-clock time per render,
  `--max-stack` bounds call-stack depth, and `--max-concurrent-evals` bounds how
  many evaluations run at once — so one expensive snippet cannot exhaust a
  shared server. The CLI imposes none of these on its own.
- **Read-scope sandboxing.** Snippet-name resolution goes through Go's
  `os.Root`, which rejects `..` traversal and symlinks that escape the
  configured snippet directory, so a crafted request cannot read arbitrary
  files. The [evaluation and security](/usage/evaluation-and-security/) page
  details the caps and the boundaries.

## When the plain CLI is the right tool

The CLI is the better choice for work that is local and one-off:

- **One-off local renders** — inspecting what a snippet produces, debugging a
  library, iterating on a dashboard before committing it.
- **CI scripts** — a build step that renders Jsonnet to JSON and hands it to
  another tool, where standing up a service would add a moving part for no gain.
- **Anywhere a service is unwanted** — no HTTP endpoint to call, no cluster, no
  artifact to consume.

Because JaaS runs the same go-jsonnet core, these are not mutually exclusive:
you can keep `jsonnet` and `jb` on your workstation and in CI, and run JaaS
in-cluster for the server-side and GitOps paths, with both producing the same
JSON for the same input. The [local rendering](/tutorials/local-rendering/)
tutorial shows JaaS used purely as a renderer, which keeps the local and
in-cluster output aligned.
