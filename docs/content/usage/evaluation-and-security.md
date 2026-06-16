---
title: Evaluation and security
description: Timeout, stack, and concurrency caps on evaluation, and the security model to lock down before exposing the service.
tags: [security, limits, evaluation]
---

JaaS runs Jsonnet on the server and returns the result over HTTP. Three caps
bound each evaluation, and a small security model governs what a snippet and its
callers can reach. Review and tune both sections before exposing the service to
a wider audience.

## Evaluation caps

| Flag                    | Default                  | Effect                                                                       |
|-------------------------|--------------------------|------------------------------------------------------------------------------|
| `--evaluation-timeout`   | `5s`                     | Wall-clock budget per evaluation. Exceeding it returns `504 evaluation_timeout`. `0` disables the timeout. |
| `--max-stack`            | `500`                    | Maximum Jsonnet call-stack depth. `0` uses go-jsonnet's own default.        |
| `--max-concurrent-evals` | `max(GOMAXPROCS*4, 16)`  | In-flight evaluations allowed at once. Excess requests return `503 evaluation_unavailable`. `0` disables the cap. |

```shell
./jaas \
  --snippet-directory examples/snippets/dashboards \
  --evaluation-timeout 2s \
  --max-stack 1000 \
  --max-concurrent-evals 32
```

The default for `--max-concurrent-evals` bounds worst-case goroutine pile-up
under a runaway snippet. Each in-flight evaluation pins roughly one CPU for its
working set, so raising the cap far above the available parallelism queues work
without adding throughput.

## Security model

**Library paths are an unrestricted read scope.** Any file reachable under a
configured `--library-path`, or under a snippet's own directory, can be
`import`-ed or `importstr`-ed by any snippet — go-jsonnet's importer does not
sandbox per snippet. Scope these directories tightly. Never point them at `/`,
`/etc`, or anywhere holding credentials.

**Snippets are operator-controlled, not caller-controlled.** Callers supply only
top-level arguments through the query string. Jsonnet's `import` and `importstr`
require string-literal paths, so a TLA or external variable cannot construct an
import path. Deploying a snippet authored by someone you do not trust is
equivalent to running their code on the server.

**Snippet name resolution is sandboxed.** The URL's snippet segment resolves
through Go's `os.Root`, which rejects `..` traversal and symlinks that escape the
configured snippet directory. A URL like `/jsonnet/../etc/passwd` returns `404`,
even though the OS would otherwise resolve the path.

**Evaluation has caps but no mid-flight cancellation.** `--evaluation-timeout`
bounds wall-clock time and `--max-stack` bounds call-stack depth, but go-jsonnet
cannot abort an evaluation already running. A slow snippet keeps consuming CPU
until it finishes naturally or the timeout fires the HTTP response. Size
container CPU and memory limits to absorb that worst case.

The Prometheus metrics `jaas_eval_in_flight` (gauge: live in-flight count),
`jaas_eval_unavailable_total` (counter: cumulative cap rejections), and
`jaas_eval_outstanding_timed_out` (gauge: evals still running after their
request timed out) surface how close evaluation runs to these caps. See
[Observability](/usage/observability/) for detail.

The HTTP status codes these caps produce are documented in the
[rendering endpoint](/usage/rendering-endpoint/) error contract.
