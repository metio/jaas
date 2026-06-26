---
title: JsonnetLibrary
description: Field-by-field reference for the JsonnetLibrary custom resource at apiVersion jaas.metio.wtf/v1.
tags: [api, libraries, operator]
---

`JsonnetLibrary` (`jlib`) is a namespaced bundle of `.libsonnet` files that
`JsonnetSnippet` CRs in the same namespace can import. The library carries no
artifact of its own and no controller reconciles it — it serves purely as a
supply-side source for snippets. The import alias is set on the snippet side
via `LibraryRef.importPath` (defaulting to the library's `metadata.name`); the
library itself carries no registration name. Task-oriented guidance lives in
[Jsonnet libraries](/rendering/jsonnet-libraries/).

## Example

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetLibrary
metadata:
  name: mylib
  namespace: default
spec:
  files:
    main.libsonnet: |
      {
        dashboard(env, cluster):: {
          title: '%s / %s' % [env, cluster],
        },
      }
```

Exactly one of `spec.files` or `spec.sourceRef` must be set. Admission rejects
CRs that set neither or both.

## Spec fields

`JsonnetLibrarySpec` embeds `SnippetSource` directly (the same source shape used
by `JsonnetSnippetSpec`).

| Field | Type | Default | Description |
|---|---|---|---|
| `files` | map[string]string | — | Inline map of filename to Jsonnet/libsonnet source. Exactly one of `files` or `sourceRef` must be set. |
| `sourceRef.apiVersion` | string | `source.toolkit.fluxcd.io/v1` | APIVersion of the referenced Flux source CR. |
| `sourceRef.kind` | string | — | Kind of the referenced source. One of: `GitRepository`, `OCIRepository`, `Bucket`, `ExternalArtifact`. Required when `sourceRef` is set. |
| `sourceRef.name` | string | — | Name of the referenced source CR. Required when `sourceRef` is set. Minimum length 1. |
| `sourceRef.namespace` | string | library's namespace | Namespace of the referenced source CR. Cross-namespace references are rejected by default; they are allowed only when the operator runs with `--no-cross-namespace-refs=false`. |
| `sourceRef.path` | string | — (artifact root) | Subdirectory within the fetched tarball to treat as the library root. Empty means the archive root — required for jb-vendored trees (e.g. a `sourceRef` pointing at a Flux `OCIRepository` for a JOI image) where the library aliases resolve against the full vendor tree. |

## Status

`JsonnetLibrary` shares the `SyncStatus` type with `JsonnetSnippet`. No controller
populates it, so every field stays empty; the subresource is present on the type so
a library reconciler can populate it without a schema change.

| Field | Type | Description |
|---|---|---|
| `observedGeneration` | int64 | `.metadata.generation` last reconciled. Unpopulated. |
| `conditions` | []Condition | Standard apimachinery conditions. Unpopulated. |
| `revision` | string | Unpopulated. |
| `artifactURL` | string | Unpopulated. |
| `lastSyncTime` | Time | Unpopulated. |
| `history` | []RevisionEntry | Unpopulated. |

For how snippets reference libraries, see [/api/jsonnetsnippet/](/api/jsonnetsnippet/)
(`spec.libraries`) and [Jsonnet libraries](/rendering/jsonnet-libraries/).
