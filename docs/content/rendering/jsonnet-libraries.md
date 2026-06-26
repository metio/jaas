---
title: Jsonnet libraries
description: Reusable .libsonnet files for snippets via the JsonnetLibrary CRD and OCI-mounted shared libraries, and how imports resolve.
tags: [operator, libraries, imports]
---

Snippets import reusable Jsonnet from two places: namespaced `JsonnetLibrary`
custom resources and OCI-mounted shared libraries the operator carries on disk.
Both feed the same import-alias namespace, so a snippet's `import` statements
look identical regardless of where the library comes from.

## The JsonnetLibrary CRD

A `JsonnetLibrary` is a namespaced bundle of `.libsonnet` files. Like a snippet,
it declares exactly one source — inline `spec.files` or a `spec.sourceRef` to a
Flux source (`GitRepository`, `OCIRepository`, `Bucket`, `ExternalArtifact`).
The library carries no registration name of its own; the import alias is chosen
on the snippet side.

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetLibrary
metadata:
  name: grafana-helpers
  namespace: default
spec:
  files:
    dashboard.libsonnet: |
      {
        new(title): {
          title: title,
          panels: [],
          schemaVersion: 38,
        },
      }
    panel.libsonnet: |
      {
        graph(title): { type: 'graph', title: title },
        stat(title): { type: 'stat', title: title },
      }
```

A `JsonnetLibrary` whose `spec.sourceRef` points at an `OCIRepository` lets you
ship a jb-vendored library tree (grafonnet, docsonnet, and similar) as an OCI
artifact and import it from snippets without inlining every file.

## Referencing a library from a snippet

A snippet enumerates the libraries it can import in `spec.libraries[]`. Each
entry is a `LibraryRef`:

- `kind` — `JsonnetLibrary` (the only library kind).
- `name` — the `JsonnetLibrary` resource's name.
- `importPath` — the alias the snippet's `import` statements use. Defaults to the
  library's `name`.

A library not listed in `spec.libraries` is invisible to the snippet even when it
exists in the same namespace — the enumeration is the allowlist.

Each entry must resolve to a distinct import path: an import path holds exactly
one library. Two `spec.libraries` entries that share an effective import path
(the same `importPath`, or the same `name` when `importPath` is omitted) are
rejected at admission, and the reconciler rejects them too if admission is
bypassed — the colliding snippet reports `Ready=False` with reason `InvalidSpec`
naming the import path. Give each library its own `importPath`.

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: my-dashboard
  namespace: default
spec:
  serviceAccountName: grafana-tenant
  entryFile: main.jsonnet
  files:
    main.jsonnet: |
      local dashboard = import 'grafana/dashboard.libsonnet';
      local panel = import 'grafana/panel.libsonnet';
      dashboard.new('API Latency') + {
        panels: [
          panel.graph('p99 by route'),
          panel.stat('error rate'),
        ],
      }
  libraries:
    - kind: JsonnetLibrary
      name: grafana-helpers
      importPath: grafana
```

The snippet imports `grafana/dashboard.libsonnet` because the `LibraryRef` sets
`importPath: grafana`. Drop `importPath` and the alias defaults to the library's
name, `grafana-helpers`. The operator reads the `JsonnetLibrary` through the
tenant's impersonating client, so the snippet's ServiceAccount needs `get` on
`jsonnetlibraries.jaas.metio.wtf` — see
[Tenancy and RBAC](/security/tenancy-and-rbac/).

## OCI-mounted shared libraries

Cluster-wide shared libraries are mounted into the operator pod's filesystem
rather than expressed as CRs. The operator scans every `--library-path`
directory at startup, reads every `.libsonnet` / `.jsonnet` / `.json` file into
memory, and folds those entries into every snippet's import namespace
additively — after the snippet's own `LibraryRef` resolution. A snippet imports
an OCI-mounted library by alias with no `LibraryRef` at all:

```jsonnet
local grafonnet = import 'grafonnet/main.libsonnet';
```

With the Helm chart this is the `additionalLibraries` value, which mounts each
configured OCI artifact under a `--library-path` directory. There is deliberately
no cluster-scoped library CRD: a snippet produces a namespaced
`ExternalArtifact`, so producers stay namespaced, while genuinely cluster-wide
shared libraries take the OCI-mount path. This is also the path the cluster-free
local renderer uses, so the same library tree renders identically on a
workstation and in the cluster.

### Library-alias safety

An OCI-mounted alias and a `LibraryRef` alias must not collide. When the operator
starts with `--library-path` flags it records every mounted alias, and both
admission and the reconciler reject any `LibraryRef` whose `importPath` (or, when
`importPath` is omitted, the library `name`) shadows one of those names. This
catches the case where grafonnet is mounted via OCI but a `JsonnetLibrary`
`LibraryRef` is also aliased `grafonnet` — the additive merge would otherwise
resolve the collision silently in favor of the CR. Rename the import alias or
remove the `LibraryRef` to resolve the rejection.

## JsonnetLibrary vs a source-output snippet

A `JsonnetLibrary` and a `JsonnetSnippet` rendered in `source` output mode both
hand Jsonnet to another snippet, so they can look interchangeable. They differ
on one axis — whether they publish an artifact:

| | `JsonnetLibrary` | `source`-output `JsonnetSnippet` |
|---|---|---|
| Role | Passive dependency | Active producer |
| Reached by | `import` by alias | `spec.sourceRef` to its `ExternalArtifact` |
| When it loads | In-process during a snippet's evaluation | Fetched as a tarball before evaluation |
| Scope | Same namespace as the importing snippet | Cross-namespace via the `ExternalArtifact` |
| Publishes an artifact | No | Yes — content-addressed and revisioned |

A `JsonnetLibrary` is a passive dependency: a snippet lists it in
`spec.libraries`, imports it by alias, and the operator folds its files into the
import namespace in-process while evaluating. It publishes no artifact and is
visible only within its own namespace.

A `source`-output `JsonnetSnippet` is an active producer: it publishes an
`ExternalArtifact` carrying its raw Jsonnet. That artifact is content-addressed,
revisioned, and consumable across namespaces, so a downstream snippet pins it
with `spec.sourceRef` and re-evaluates the Jsonnet itself.

Use a `JsonnetLibrary` for shared helpers your snippets import by alias. Use
`output: source` chaining when one snippet's Jsonnet should feed another as a
pinned Flux artifact — see
[Chaining snippets](/operator/snippet-sources/#chaining-snippets).

## Import resolution

The operator's in-memory importer resolves `import` and `importstr` statements
with the same semantics as `jsonnet -J vendor`. A jb-vendored library tree
renders identically on the operator path and locally — this parity is the reason
the same Jsonnet works on a workstation and in the cluster without change. For
an import path, resolution proceeds:

1. **Sibling-relative** — relative to the importing file within its own root, so
   a bare `import 'dashboard.libsonnet'`, `./x`, or `../x` resolves against the
   importing file's directory first.
2. **Bare alias** — a registered alias on its own resolves to that library's
   `main.libsonnet`.
3. **Alias plus file** — `alias/file` resolves `file` within the registered
   alias's tree; the alias head is authoritative.
4. **JPATH / vendor search** — the import path is searched across the snippet's
   own files and then every library, which is what lets an absolute
   `import 'github.com/grafana/grafonnet/gen/...'` resolve against a library
   whose tree carries the full vendor path.

Sibling files win over a library's default entry, matching `jsonnet -J vendor`.
A slash-prefixed path whose head is not a registered alias is not an error — it
falls through to the vendor search.

## Related pages

- [Snippet sources](/operator/snippet-sources/) — where a snippet's own Jsonnet
  comes from, including the same `sourceRef` mechanism libraries use.
- [Snippets and libraries](/rendering/snippets-and-libraries/) — the on-disk
  equivalent for the HTTP renderer, including `--library-path` precedence.
