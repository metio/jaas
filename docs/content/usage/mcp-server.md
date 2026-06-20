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
| `render_jsonnet` | Evaluate an inline snippet (with `tlas` and `extVars`) and return the resulting JSON. Imports resolve against `--library-path`, exactly like the HTTP renderer. |
| `validate_jsonnet` | Evaluate a snippet and report whether it compiles, returning the full go-jsonnet diagnostic (file and line) on failure without the rendered output. |

Both accept the same inputs:

- `source` — the Jsonnet snippet.
- `tlas` — top-level arguments as a map of string lists; a single-element list
  becomes a string TLA, a multi-element list becomes a JSON-array TLA.
- `extVars` — external variables for `std.extVar`, overlaying any the server was
  started with.

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
condition. See [Evaluation and security](../evaluation-and-security/).

## Inspect the operator in-cluster

When JaaS runs as an [operator](../operator-mode/), add `--enable-mcp` to serve
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

The in-cluster server adds two read tools:

| Tool | Purpose |
|---|---|
| `list_snippets` | List `JsonnetSnippet` resources with their Ready status, reason, suspend state, revision, and artifact URL. Omit the namespace to list across every namespace the operator can read. |
| `get_snippet` | One snippet's full status: the Ready condition (status, reason, message), the per-reason [runbook](../../runbooks/) URL, suspend state, revision, artifact URL, and the retained revision history. |

These tools are read-only. They report status and surface the runbook for a
failing reason; they do not change cluster state.
