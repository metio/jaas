---
title: Rendering endpoint
description: The GET /jsonnet/{snippet} request, snippet resolution, the management probes, and the stable error contract.
tags: [http, endpoint, errors]
---

Send a `GET` to the rendering endpoint with a snippet name and JaaS returns the
evaluated Jsonnet as JSON:

```shell
curl http://127.0.0.1:8080/jsonnet/example1
```

The Jsonnet server binds `127.0.0.1:8080` by default (`--listen-address`,
`--port`). The URL shape is `GET /<jsonnet-endpoint-path>/{snippet...}`, where
`{snippet...}` is a trailing path segment that may contain slashes. A successful
response carries `Content-Type: application/json` and the rendered document.

## The endpoint path

The leading path segment defaults to `jsonnet` and is set with
`--jsonnet-endpoint-path`. Running with `--jsonnet-endpoint-path render` moves the
endpoint to `GET /render/{snippet...}`:

```shell
./jaas --jsonnet-endpoint-path render --snippet-directory examples/snippets/dashboards
curl http://127.0.0.1:8080/render/example1
```

## Snippet resolution

The `{snippet...}` segment names which file JaaS evaluates. Resolution checks
the `--snippet` files first, then looks for `<name>/main.jsonnet` under each
`--snippet-directory`. See
[Snippets and libraries](/usage/snippets-and-libraries/) for how to declare
both.

Resolution is sandboxed through Go's `os.Root`, which rejects `..` traversal and
symlinks that escape the configured directory. A crafted URL never reaches a
file outside the snippet roots:

```shell
curl -i http://127.0.0.1:8080/jsonnet/../etc/passwd
# HTTP/1.1 404 Not Found
```

## Management probes

A second HTTP server — the management server — exposes the Kubernetes lifecycle
probes. It binds `127.0.0.1:8081` by default (`--management-listen-address`,
`--management-port`):

| Path     | Meaning                                                                      |
|----------|------------------------------------------------------------------------------|
| `/live`  | Liveness. Unconditional `200`.                                               |
| `/start` | Startup. Consults health state; `200` once started, otherwise `503` + JSON.  |
| `/ready` | Readiness. Consults health state; `200` when ready, otherwise `503` + JSON.  |

A not-ready probe returns a JSON body naming the state:

```shell
curl -i http://127.0.0.1:8081/ready
# HTTP/1.1 503 Service Unavailable
# {"status":"not ready"}
```

## Error contract

Every non-2xx response is an [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457)
problem document with `Content-Type: application/problem+json` so programmatic
callers can pick the failure apart:

```json
{
  "type":    "https://jaas.projects.metio.wtf/errors/snippet_not_found",
  "title":   "Snippet not found",
  "status":  404,
  "detail":  "snippet \"missing\" not found",
  "code":    "snippet_not_found",
  "snippet": "missing"
}
```

- `type` — the RFC 9457 problem-type URI, formed as
  `https://jaas.projects.metio.wtf/errors/` + the `code`.
- `title` — a short, human-readable summary of the problem.
- `status` — the HTTP status, mirrored into the body.
- `detail` — human-readable specifics for this occurrence.
- `code` — the stable short identifier. Callers match on this; these strings do
  not change. It is the matcher that pairs with the `type` URI.
- `snippet` — echoes the requested name when one was parsed, and is omitted
  otherwise.

Error bodies use `application/problem+json`; a successful `200` response stays
`application/json` and carries the rendered document.

| `code`                   | `status` | When                                                          |
|--------------------------|---------:|---------------------------------------------------------------|
| `method_not_allowed`     | `405`    | Anything other than `GET` on the endpoint.                    |
| `snippet_not_found`      | `404`    | The requested snippet name resolves to no file.              |
| `evaluation_timeout`     | `504`    | Evaluation exceeded `--evaluation-timeout`.                    |
| `evaluation_unavailable` | `503`    | The concurrent-eval cap (`--max-concurrent-evals`) is full.   |
| `evaluation_failed`      | `400`    | go-jsonnet returned an error (syntax, missing import, stack-limit exceeded). |

For `evaluation_failed`, `detail` is the constant `"evaluation failed"`. The full
go-jsonnet diagnostic — syntax error, missing import, stack-limit, with file and
line numbers — is written to the server logs instead of the response body, since
it names on-disk snippet paths. Read the controller logs to debug a failing
snippet; in operator mode the snippet's failure also surfaces on its
`JsonnetSnippet` status condition.

A client that closes the connection mid-evaluation receives no body and no
status line — the handler detects the cancellation and returns without writing
anything.

The timeout, stack, and concurrency caps that drive `evaluation_timeout` and
`evaluation_unavailable` are documented in
[Evaluation and security](/usage/evaluation-and-security/). To pass values into
a render, see
[External variables and TLAs](/usage/external-variables-and-tlas/).
