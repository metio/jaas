---
title: Snippet sources
description: Where a JsonnetSnippet's Jsonnet comes from — inline files, a Flux source, a multi-snippet tree, and chained snippet output.
tags: [operator, sources, chaining]
---

A `JsonnetSnippet` declares exactly one source for its Jsonnet bytes: either
inline `spec.files` or a `spec.sourceRef` pointing at a Flux source. Admission
rejects a snippet that sets both or neither. The operator resolves the source
into an in-memory file tree, evaluates `spec.entryFile` within it, and publishes
the result.

## Inline files

`spec.files` is a map of filename to Jsonnet source. The operator evaluates the
entry file (`spec.entryFile`, default `main.jsonnet`) against the rest of the
map. This is the simplest source — the snippet is self-contained, with no
external dependency to fetch:

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: hello-world
  namespace: default
spec:
  serviceAccountName: hello-world-tenant
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      {
        greeting: 'hello',
        recipient: std.extVar('audience'),
      }
  externalVariables:
    audience: world
```

## A Flux source

`spec.sourceRef` points at a Flux source CR whose artifact tarball the operator
fetches and extracts into the snippet's file tree. The `kind` is one of
`GitRepository`, `OCIRepository`, `Bucket`, or `ExternalArtifact` — see Flux's
[source-controller documentation](https://fluxcd.io/) for how each source CR
publishes its artifact.

When the referenced source republishes — a new commit lands on the
`GitRepository`, a new tag pushes to the `OCIRepository` — the operator's watch
on Flux source kinds re-queues the snippet and re-renders it. No `spec.interval`
is required for this; the watch is event-driven.

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: dashboards-source
  namespace: default
spec:
  interval: 5m
  url: https://github.com/example-org/grafana-dashboards
  ref:
    branch: main
---
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: api-latency-dashboard
  namespace: default
spec:
  serviceAccountName: dashboards-tenant
  entryFile: dashboards/api-latency.jsonnet
  sourceRef:
    kind: GitRepository
    name: dashboards-source
    path: dashboards/
```

`spec.sourceRef.path` narrows extraction to a subdirectory of the artifact's
tarball. Empty means the whole tree. The tenant ServiceAccount needs `get` on
the referenced source kind — see [Tenancy and RBAC](/security/tenancy-and-rbac/).

### The entry file and multi-snippet trees

`spec.entryFile` names the file — relative to the resolved source root — that
go-jsonnet evaluates. It defaults to `main.jsonnet`. The field is restricted to
relative `[A-Za-z0-9._/-]+` paths with no `..` segments, so it cannot traverse
out of the extracted tree.

One Flux source often carries many snippets. A shared dashboards repository, for
example, holds one `.jsonnet` file per dashboard. Rather than one source per
dashboard, point several `JsonnetSnippet` resources at the same `GitRepository`
and give each a different `entryFile`:

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: api-latency-dashboard
  namespace: default
spec:
  serviceAccountName: dashboards-tenant
  entryFile: dashboards/api-latency.jsonnet
  sourceRef:
    kind: GitRepository
    name: dashboards-source
---
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: error-budget-dashboard
  namespace: default
spec:
  serviceAccountName: dashboards-tenant
  entryFile: dashboards/error-budget.jsonnet
  sourceRef:
    kind: GitRepository
    name: dashboards-source
```

Both snippets share the source fetch and re-render together when the repository
republishes, but each publishes its own `ExternalArtifact` from its own entry
file.

## Chaining snippets

A `JsonnetSnippet` can source from the `ExternalArtifact` another snippet
publishes. This composes a pipeline of renders: snippet A evaluates and
publishes its JSON, and snippet B takes that JSON as its input, transforms it,
and publishes a second artifact. A downstream consumer deploys only the final
artifact.

Chaining works because the `ExternalArtifact` is a Flux source like any other.
Snippet B sets `spec.sourceRef` with `kind: ExternalArtifact` and `name` pointing
at the producing snippet — an `ExternalArtifact` is published under the producing
`JsonnetSnippet`'s name. The operator fetches A's artifact tarball into B's file
tree. In the default `rendered` output mode that tarball holds a single
`rendered.json`, so snippet B sets `entryFile: rendered.json` to evaluate A's
output. Because JSON is valid Jsonnet, B's entry file can extend the imported
object directly:

```yaml
# Snippet A renders a shared config blob other snippets consume.
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: base-config
  namespace: default
spec:
  serviceAccountName: chained-tenant
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      {
        cluster: 'prod',
        region: 'eu-west-1',
        retentionDays: 30,
      }
---
# Snippet B sources from base-config's ExternalArtifact and extends it.
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: derived-config
  namespace: default
spec:
  serviceAccountName: chained-tenant
  entryFile: rendered.json
  sourceRef:
    kind: ExternalArtifact
    name: base-config
```

`derived-config` re-emits `base-config`'s rendered JSON as its own artifact. The
operator's watch on `ExternalArtifact` updates re-queues `derived-config`
whenever `base-config` republishes, so the pipeline stays current end to end.

### Source variant

The rendered variant above passes evaluated JSON downstream. The source variant
passes raw Jsonnet downstream instead, for snippet B to re-evaluate itself.

Snippet A sets `spec.output: source`, so its `ExternalArtifact` carries A's raw
`.jsonnet` / `.libsonnet` files rather than the evaluated JSON. Snippet B points
`spec.sourceRef` at A's `ExternalArtifact` and imports A's files as Jsonnet,
re-evaluating them with B's own external variables, TLAs, and libraries. A
becomes a source that the pipeline produces dynamically rather than one an
operator authors by hand:

```yaml
# Snippet A publishes its raw Jsonnet, not its evaluated output.
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: dashboard-template
  namespace: default
spec:
  serviceAccountName: chained-tenant
  output: source
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      function(env='dev') {
        title: 'API latency — ' + env,
        refresh: if env == 'prod' then '30s' else '5m',
      }
---
# Snippet B sources A's raw Jsonnet and re-evaluates it with its own TLAs.
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: dashboard-prod
  namespace: default
spec:
  serviceAccountName: chained-tenant
  entryFile: main.jsonnet
  sourceRef:
    kind: ExternalArtifact
    name: dashboard-template
  tlas:
    env:
      - prod
```

Because A published `source` output, B's `sourceRef` extracts A's raw
`main.jsonnet` into B's file tree, and B evaluates it as the entry file with
`env=prod` supplied as a TLA. When A's template changes, A republishes and the
`ExternalArtifact` watch re-renders B against the new Jsonnet.

Choose between the two by what the downstream snippet needs: rendered chaining
passes JSON data downstream; source chaining passes Jsonnet to be re-evaluated
downstream. For when to reach for a `source`-output snippet instead of a
`JsonnetLibrary`, see
[JsonnetLibrary vs a source-output snippet](/rendering/jsonnet-libraries/#jsonnetlibrary-vs-a-source-output-snippet).

The tenant ServiceAccount needs `get` on
`externalartifacts.source.toolkit.fluxcd.io` for both variants; see
[Tenancy and RBAC](/security/tenancy-and-rbac/).

### Cycle detection

A snippet cannot transitively depend on itself. The operator walks the
dependency graph — `spec.sourceRef` edges to `ExternalArtifact`s and their
producing snippets, plus `spec.libraries` edges through `JsonnetLibrary`
`sourceRef`s — at reconcile time, before any tenant work. If the walk revisits
the snippet it started from, the operator refuses to publish and reports
`Ready=False` with reason `DependencyCycle`. This catches a chain that feeds
back into itself directly or through a library, so a cycle surfaces as a clear
status condition rather than an endless re-render loop.

## Related pages

- [Jsonnet libraries](/rendering/jsonnet-libraries/) — reusable `.libsonnet` files
  referenced via `spec.libraries`.
- [Tenancy and RBAC](/security/tenancy-and-rbac/) — the verbs the tenant
  ServiceAccount needs for each source kind.
