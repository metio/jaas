---
title: ExternalArtifact output contract
description: The shape JaaS writes to the Flux ExternalArtifact CR and the contract downstream consumers depend on.
tags: [api, flux, operator, snippets]
---

For every successfully evaluated `JsonnetSnippet`, the JaaS operator upserts a
Flux `ExternalArtifact` CR (`source.toolkit.fluxcd.io/v1`) in the same namespace
as the snippet. JaaS does not own the `ExternalArtifact` CRD — it is defined and
installed by Flux's source-controller. The full CRD schema is in the
[Flux ExternalArtifact reference](https://fluxcd.io/flux/components/source/externalartifacts/);
below is the subset JaaS writes and the invariants downstream consumers can rely on.

For task-oriented context, see [Operator mode](/usage/operator-mode/).

## What JaaS writes

### `spec.sourceRef`

JaaS stamps a back-pointer to the originating snippet on `spec.sourceRef`:

```yaml
spec:
  sourceRef:
    apiVersion: jaas.metio.wtf/v1
    kind: JsonnetSnippet
    name: <snippet-name>
```

The namespace is always the snippet's own namespace — JaaS never publishes an
`ExternalArtifact` to a different namespace. The three fields
(`apiVersion`, `kind`, `name`) are wire-stable: downstream consumers that do
producer-aware reverse lookup (such as stageset-controller's RFC-0012 resolution)
match on this triple. Renaming any field is a breaking change.

### `status.artifact`

After a successful publish, JaaS writes the following fields under `status.artifact`:

| Field | Type | Description |
|---|---|---|
| `url` | string | HTTP URL of the published tarball. Revision-addressed: `<storage-base-url>/<namespace>/<name>/<sha256-hex>.tar.gz`. Byte-stable for the lifetime of the revision in the keep-set — re-publishing a different revision does not mutate the bytes at this URL. |
| `path` | string | Storage-backend-relative path of the tarball. |
| `revision` | string | `sha256:<hex>` content hash of the artifact. In `rendered` output mode this is the sha256 of the evaluated JSON; in `source` mode it is a deterministic hash over all source files (sorted by filename). |
| `digest` | string | `sha256:<hex>` of the tarball bytes (the `.tar.gz` itself, not the content). Used by Flux consumers to verify integrity after download. |
| `size` | int64 | Tarball size in bytes. |
| `lastUpdateTime` | string | RFC3339 timestamp of the most recent successful publish. |

### `status.conditions`

JaaS writes a single `Ready` condition on every successful publish:

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      reason: Succeeded
      message: artifact published
      lastTransitionTime: <RFC3339>
      observedGeneration: <generation>
```

`lastTransitionTime` is preserved across steady-state republishes (same
`Ready=True`, new revision) so the timestamp does not churn. It advances only
when the condition transitions (e.g. from `False` to `True` after a failure
clears).

## What downstream consumers rely on

**Gate on `Ready=True` before fetching.** Every Flux consumer — including
`kustomize-controller`, `helm-controller`, and JaaS's own chained-snippet
`sourceRef` resolver — treats an `ExternalArtifact` as not-yet-consumable until
`status.conditions[Ready].status == "True"`. A snippet that has not yet
completed its first successful reconcile will have no `Ready` condition (or
`Ready=False`) and leaves chained snippets blocked with reason `SourceNotReady`.

**URL is revision-addressed and byte-stable.** The URL published in
`status.artifact.url` has the form
`<storage-base-url>/<namespace>/<name>/<sha256-hex>.tar.gz`. The bytes at that
URL are immutable for as long as the revision is in the snippet's keep-set
(`spec.history`). Consumers can safely re-fetch a pinned URL (e.g. during
rollback) and verify it against the recorded `digest`. Once a revision leaves
the keep-set it is garbage-collected after the operator's GC grace period; a
fetch after that point returns 404.

**Revision identifies content, not time.** Two publishes that produce identical
content (same evaluated JSON or same source files) yield the same `revision`.
Consumers that cache by revision can skip a re-fetch when the revision has not
changed.

**The snippet mirrors `status.artifactURL`.** To avoid a second lookup, the
originating `JsonnetSnippet` also carries the URL in its own
`status.artifactURL`. `kubectl describe jsonnetsnippet` therefore surfaces the
artifact location directly.

## Tarball contents

The tarball layout depends on `spec.output` of the originating `JsonnetSnippet`:

| `spec.output` | Tarball contents |
|---|---|
| `rendered` (default) | A single `rendered.json` holding the evaluated JSON output. |
| `source` | Every source file from the resolved snippet source (inline `spec.files` or the files extracted from the `spec.sourceRef` tarball), with their original relative paths. |

All tarballs are produced deterministically: entries are sorted by path and
`ModTime` is zeroed. Two publishes from the same input produce byte-identical
`.tar.gz` files and therefore the same `revision` and `digest`.
