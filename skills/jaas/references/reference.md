# JaaS quick reference

The authoritative, current docs are at <https://jaas.projects.metio.wtf/>
(`/llms.txt` for a link index, `/llms-full.txt` for everything concatenated).
This is a compact cheat-sheet.

## JsonnetSnippet spec (`jaas.metio.wtf/v1`)

- `serviceAccountName` — impersonated for every tenant-side API call (falls back
  to the operator's `--default-service-account`)
- **exactly one of** `files` (inline `{name: content}`) **or** `sourceRef`
  (`{kind, name}`; kind `GitRepository` / `OCIRepository` / `Bucket` /
  `ExternalArtifact`)
- `libraries[]` — `{name, importPath?}` referencing a `JsonnetLibrary`
  (`importPath` defaults to the library name)
- `tlas` (`map[string][]string`) — top-level arguments; `externalVariables`
  (`map[string]string`) — `std.extVar` seeds
- `output` — `rendered` (default; JSON) or `source` (raw `.jsonnet`/`.libsonnet`)
- `entryFile` (default `main.jsonnet`), `suspend`, `interval`, `history`
  (default 1, max 50)

`status` is controller-owned (conditions, artifact coordinates). Never author it.

## JsonnetLibrary spec

- `files` (inline `.libsonnet`) or `sourceRef` to a Flux source — reusable
  imports for snippets in the same namespace. Publishes no artifact.

## HTTP renderer

- `GET /<jsonnet-endpoint>/<snippet>` → evaluated JSON (endpoint default `jsonnet`)
- TLAs via query string (`?a=1`; repeat a key for a list); external variables via
  `JAAS_EXT_VAR_<name>` env or `-ext-var KEY=VALUE`
- Stable error codes (JSON body, field `error`): `method_not_allowed` (405),
  `snippet_not_found` (404), `evaluation_timeout` (504),
  `evaluation_unavailable` (503), `evaluation_failed` (400)

## Gotchas

- Library alias field is `importPath`, not `as`.
- An `OCIRepository` source artifact must be a **single layer** (`flux push
  artifact`).
- The snippet's ServiceAccount needs RBAC: write `externalartifacts`; read any
  referenced `jsonnetlibraries` and Flux source kinds.

## Runbooks

`status.conditions[Ready].reason` → `https://jaas.projects.metio.wtf/runbooks/<reason-lowercased>/`.
