---
name: jaas
description: >-
  Author and operate JaaS (Jsonnet-as-a-Service, apiVersion jaas.metio.wtf/v1) — a
  webservice that evaluates Jsonnet and returns JSON, and a Flux operator that
  publishes the result as an ExternalArtifact. Use this when writing or editing a
  JsonnetSnippet or JsonnetLibrary, wiring a snippet to inline files or a Flux
  source (GitRepository / OCIRepository / Bucket / ExternalArtifact), chaining one
  snippet's output into another, pairing JaaS with grafana-operator (dashboards) or
  stageset-controller (manifest delivery), configuring per-snippet ServiceAccount
  impersonation, or calling the HTTP rendering endpoint (GET /jsonnet/<snippet>)
  with top-level arguments and external variables. Applies whenever a repo has
  JsonnetSnippet / JsonnetLibrary manifests or JaaS is in play.
allowed-tools: Bash(kubectl *), Bash(curl *)
---

# Using JaaS

**JaaS** (`jaas.metio.wtf/v1`) evaluates [Jsonnet](https://jsonnet.org/) and
returns JSON. It runs two ways over one evaluation core:

- **HTTP renderer** — `GET /jsonnet/<snippet>` returns the evaluated JSON. No
  cluster required.
- **Flux operator** (`--enable-flux-integration`) — watches `JsonnetSnippet` and
  `JsonnetLibrary` resources and publishes the rendered output as a Flux
  `ExternalArtifact` that any Flux consumer reads.

Reach for JaaS when manifests or dashboards are authored in Jsonnet and must be
rendered server-side. The two flagship pairings are **grafana-operator** (Grafana
dashboards from grafonnet) and **stageset-controller** (Kubernetes manifest
delivery). JaaS only renders; what happens to the JSON is the consumer's concern.

## The docs are the source of truth

The full, current documentation lives at <https://jaas.projects.metio.wtf/>, with
a machine-readable index at `/llms.txt` and the whole site concatenated at
`/llms-full.txt`. When you need an exact field, default, or example, prefer those
over memory. Key pages: [Usage](https://jaas.projects.metio.wtf/usage/) (one
feature each), the [API reference](https://jaas.projects.metio.wtf/api/jsonnetsnippet/),
and [Tutorials](https://jaas.projects.metio.wtf/tutorials/).

`references/reference.md` in this skill is a compact cheat-sheet of the same.

## Authoring a JsonnetSnippet

Start minimal — inline `spec.files` with an `entryFile` (default `main.jsonnet`):

```yaml
apiVersion: jaas.metio.wtf/v1
kind: JsonnetSnippet
metadata:
  name: hello
  namespace: default
spec:
  serviceAccountName: hello-renderer   # impersonated for every tenant-side call
  files:
    main.jsonnet: |
      { greeting: 'hello ' + std.extVar('who') }
  externalVariables:
    who: world
```

Then layer options on, in roughly this order of need:

- **`spec.sourceRef`** instead of `files` — pull the Jsonnet from a Flux
  `GitRepository` / `OCIRepository` / `Bucket` / `ExternalArtifact`. The snippet
  re-renders when the source republishes. Exactly one of `files` or `sourceRef`.
- **`spec.libraries`** — reference a `JsonnetLibrary` by `importPath` (defaults to
  the library's name); the snippet imports it as `import '<importPath>/file'`.
- **`spec.tlas`** / **`spec.externalVariables`** — top-level arguments and
  `std.extVar` seeds at render time.
- **`spec.output`** — `rendered` (default; the artifact carries evaluated JSON) or
  `source` (the artifact carries the raw `.jsonnet`/`.libsonnet`, for a downstream
  snippet to re-evaluate).
- **`spec.entryFile`** — the file evaluated (default `main.jsonnet`); point each
  snippet at a different file in a multi-snippet source tree.
- **`spec.suspend`** / **`spec.interval`** / **`spec.history`** — pause; re-render
  on a cadence; retain N revisions for rollback.

### Gotchas to honor

- A snippet's bytes come from **exactly one** of `spec.files` or `spec.sourceRef`
  (CEL-enforced at admission).
- The library alias field is **`importPath`**, not `as`.
- An `OCIRepository` source artifact **must be a single layer** — build it with
  `flux push artifact` (an image with multiple layers is not consumed correctly).
  See the [creating-sources](https://jaas.projects.metio.wtf/usage/creating-sources/) guide.
- The operator renders under **`spec.serviceAccountName`** impersonation; that
  ServiceAccount needs the tenant RBAC (write `externalartifacts`; read referenced
  `jsonnetlibraries` and Flux source kinds). Without `serviceAccountName`, the
  operator's `--default-service-account` is used; if that is empty, the snippet is
  rejected.
- `externalVariables` keys that collide with the operator's `--ext-var` set are
  rejected at admission.

## Calling the HTTP renderer

```bash
# evaluate a snippet; pass a TLA via the query string
curl 'http://127.0.0.1:8080/jsonnet/example1?env=prod'
```

Top-level arguments are query parameters (repeat a key for a list). External
variables come from `JAAS_EXT_VAR_<name>` environment variables (or `-ext-var`).
Non-2xx responses carry a JSON body with a stable `error` code
(`snippet_not_found`, `evaluation_failed`, `evaluation_timeout`,
`evaluation_unavailable`, `method_not_allowed`).

## Debugging a snippet

`status.conditions[Ready].reason` names the failure; each reason has a runbook at
`https://jaas.projects.metio.wtf/runbooks/<reason>/` (lower-cased). `kubectl
describe jsonnetsnippet <name>` shows the Ready condition and the message;
`kubectl get externalartifact <name>` shows the published revision and digest.

## MCP server

`jaas mcp` runs a Model Context Protocol server over stdio for cluster-free
rendering: `render_jsonnet` (snippet + `tlas`/`extVars` → JSON) and
`validate_jsonnet` (compile check + full diagnostic). Imports resolve against
`--library-path`, the same as the HTTP renderer. In operator mode, `--enable-mcp`
adds read-only `list_snippets` / `get_snippet` tools (Ready status, reason,
runbook URL, revision, artifact URL, history) over streamable HTTP on
`--mcp-bind-address` (default `:8084`), reading as the operator's ServiceAccount.
`--mcp-allow-mutations` additionally exposes gated `reconcile_snippet` /
`suspend_snippet` / `resume_snippet` write tools.
Full reference: `https://jaas.projects.metio.wtf/usage/mcp-server/`.
