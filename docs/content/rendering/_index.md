---
title: Rendering Jsonnet
description: How JaaS evaluates Jsonnet — the HTTP endpoint, snippets and libraries, external variables and TLAs, and the limits around evaluation.
tags: [rendering, jsonnet, evaluation]
---

The core of JaaS: evaluate Jsonnet on demand and return JSON. These pages cover
what you send it, how imports resolve, how you parameterize a render, and the
limits that keep evaluation safe.

- **[Rendering endpoint](/rendering/rendering-endpoint/)** — the HTTP API that
  evaluates a snippet and returns JSON.
- **[Snippets and libraries](/rendering/snippets-and-libraries/)** — how snippets
  and their imports are organized and resolved.
- **[External variables and TLAs](/rendering/external-variables-and-tlas/)** —
  parameterize a render with `extVar` and top-level arguments.
- **[Jsonnet libraries](/rendering/jsonnet-libraries/)** — reusable `.libsonnet`
  libraries on the import path.
- **[JOI images](/rendering/joi-images/)** — ship libraries as single-layer OCI
  images.
- **[Evaluation and security](/rendering/evaluation-and-security/)** — timeouts,
  stack limits, and import confinement around untrusted snippets.
