---
title: Operator mode
description: Run JaaS as a Flux source — evaluate JsonnetSnippet resources and publish the result as an ExternalArtifact for downstream consumers.
tags: [operator, flux, sources]
---

With Flux integration enabled, JaaS watches `JsonnetSnippet` resources, evaluates
them, and publishes each result as an `ExternalArtifact` that Flux consumers can
deploy. These pages cover the operator and the sources it reads and writes.

- **[Operator mode](/operator/operator-mode/)** — enable the operator and the
  custom resources it reconciles.
- **[Snippet sources](/operator/snippet-sources/)** — point a snippet at a Flux
  source for its inputs.
- **[Creating source artifacts](/operator/creating-sources/)** — how snippets are
  published as `ExternalArtifact` outputs.
- **[Storage and HA](/operator/storage-and-ha/)** — artifact storage backends and
  leader-elected high availability.
