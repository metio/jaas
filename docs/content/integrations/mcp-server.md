---
title: MCP server
description: Run the JaaS Model Context Protocol server so an agent renders Jsonnet locally over stdio and reads operator status in-cluster over HTTP.
tags: [mcp, claude, operator, rendering]
---

JaaS exposes its evaluator and operator status as [Model Context Protocol](https://modelcontextprotocol.io/)
tools, so an LLM agent (Claude Code, Claude Desktop, or any MCP client) calls
them directly. There are two modes, each with its own transport:

- **Local rendering over stdio** — `jaas mcp` wraps the same evaluator the HTTP
  endpoint and the operator use, with no cluster and no Kubernetes client. This
  is the cluster-free authoring loop.
- **In-cluster operator introspection over HTTP** — `--enable-mcp` adds a
  streamable-HTTP server inside the operator pod that reads `JsonnetSnippet`
  status as the operator's ServiceAccount.

The tools are thin adapters over existing code, so an agent's render is
byte-identical to the HTTP endpoint and the operator.

## Render Jsonnet locally

Run the stdio server as a subcommand. Point it at the same library directories
you would pass the HTTP renderer:

```shell
jaas mcp --library-path ./vendor
```

Wire it into an MCP client by launching that command. For Claude Code, add it to
`.mcp.json`:

```json
{
  "mcpServers": {
    "jaas": {
      "command": "jaas",
      "args": ["mcp", "--library-path", "./vendor"]
    }
  }
}
```

The stdio server registers two tools:

| Tool | Purpose |
|---|---|
| `render_jsonnet` | Evaluate an inline snippet (with its variables bound) and return the resulting JSON. Imports resolve against `--library-path`, exactly like the HTTP renderer. |
| `validate_jsonnet` | Evaluate a snippet and report whether it compiles, returning the full go-jsonnet diagnostic (file and line) on failure without the rendered output. |

Both accept the same inputs:

- `source` — the Jsonnet snippet.
- `tlas` — top-level arguments bound as strings, as a map of string lists; a
  single-element list becomes a string TLA, a multi-element list becomes a
  JSON-array TLA.
- `tlaCode` — top-level arguments whose values are Jsonnet source to parse, like
  `jsonnet --tla-code`. `"3"` binds the number 3, `["a","b"]` an array.
- `extVars` — external variables for `std.extVar` bound as strings, overlaying
  any the server was started with.
- `extCode` — external variables whose values are Jsonnet source to parse, like
  `jsonnet --ext-code`. `"3"` binds the number 3, `{ cpu: 2 }` an object. Also
  overlays the server's `--ext-var` set, since a call is the more specific
  binding.

A name may be bound as a string or as code, not both: `tlas` and `tlaCode`
share one namespace, as do `extVars` and `extCode`. A call that binds a name in
both is rejected with a tool error naming it. The two kinds are separate,
though — a TLA and an external variable may share a name freely.

To render `{ replicas: 3 }` rather than `{ replicas: "3" }`:

```json
{
  "source": "function(replicas) { replicas: replicas }",
  "tlaCode": { "replicas": "3" }
}
```

The flags that shape evaluation:

| Flag | Default | Effect |
|---|---|---|
| `--library-path` | _(none)_ | Directory searched for `import` resolution; repeatable, rightmost wins. |
| `--ext-var KEY=VALUE` | _(none)_ | Server-level external variable; repeatable. Overlays `JAAS_EXT_VAR_*` env on conflict. |
| `--max-stack` | `500` | Maximum call-stack depth; `0` uses go-jsonnet's default. |
| `--evaluation-timeout` | `5s` | Per-evaluation bound; `0` disables it. |
| `--log-level` / `--log-format` | `info` / `json` | Diagnostics go to stderr — stdout carries the protocol, so logs never corrupt it. |

Unlike the broadly-reachable HTTP path, this server returns the full go-jsonnet
diagnostic on a failed render: it is owner-facing, like the operator status
condition. See [Evaluation and security](/rendering/evaluation-and-security/).

## Inspect the operator in-cluster

When JaaS runs as an [operator](/operator/operator-mode/), add `--enable-mcp` to serve
the read tools over streamable HTTP from the same pod:

```shell
jaas --enable-flux-integration --enable-mcp --mcp-bind-address :8084
```

`--enable-mcp` requires `--enable-flux-integration` — the tools introspect
operator resources, so there is nothing to serve without the operator. The
server binds `--mcp-bind-address` (default `:8084`, chosen to avoid the jsonnet
`:8080`, management `:8081`, storage `:8082`, and metrics `:8083` ports) and
reads as the operator's ServiceAccount, so it can never see more than the
operator's own RBAC allows. The local render tools above are served here too.

Reach it from your machine with a port-forward:

```shell
kubectl --namespace jaas-system port-forward deploy/jaas 8084:8084
```

The in-cluster server adds these read tools:

| Tool | Purpose |
|---|---|
| `list_snippets` | List `JsonnetSnippet` resources with their Ready status, reason, suspend state, revision, and artifact URL. Omit the namespace to list across every namespace the operator can read. |
| `get_snippet` | One snippet's full status: the Ready condition (status, reason, message), the per-reason [runbook](/runbooks/) URL, suspend state, revision, artifact URL, and the retained revision history. |
| `diff_revisions` | A per-file unified diff of a snippet's published output between two retained revisions. Omit the revisions to compare the two most recent in `status.history`; pass `from`/`to` (sha256) to diff specific ones. Reads the artifacts straight from the operator's store. Needs `spec.history` greater than 1 to retain a second revision. |

## Gated mutations

The server is read-only by default. Add `--mcp-allow-mutations` (which requires
`--enable-mcp`) to also expose write tools:

```shell
jaas --enable-flux-integration --enable-mcp --mcp-allow-mutations
```

| Tool | Effect |
|---|---|
| `reconcile_snippet` | Stamp the `reconcile.fluxcd.io/requestedAt` annotation to request an immediate reconcile — the same trigger as `flux reconcile`. |
| `suspend_snippet` | Set `spec.suspend=true` so the operator stops re-rendering the snippet. |
| `resume_snippet` | Clear `spec.suspend` to resume reconciliation. |

These act on the `JsonnetSnippet` resource as the operator's ServiceAccount, so
they can never exceed the operator's own RBAC. Keep them off unless you intend
the agent to drive reconciliation, and have your MCP client confirm each call.
