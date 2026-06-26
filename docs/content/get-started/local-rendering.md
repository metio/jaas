---
title: Local rendering
description: Run JaaS as a cluster-free Jsonnet renderer over HTTP, with snippet directories, library paths, TLAs, and external variables.
tags: [local, renderer, http]
---

JaaS runs as a cluster-free Jsonnet renderer: point it at a directory of
snippets and a directory of libraries, then `GET` a snippet name to receive the
evaluated JSON. No Kubernetes, no operator mode, no Flux. The evaluation core is
the same one the operator uses, so a snippet that renders correctly here renders
identically in-cluster.

This tutorial runs against this repository's `examples/` layout, so clone the
repo first:

```shell
git clone https://github.com/metio/jaas
cd jaas
```

## Step 1 — Get the binary or container image

Pre-built binaries are attached to each
[GitHub release](https://github.com/metio/jaas/releases). Download the archive
for your platform, unpack it, and the `jaas` binary is inside.

A container image is published at `ghcr.io/metio/jaas:latest`:

```shell
docker pull ghcr.io/metio/jaas:latest
```

The examples below use a `jaas` binary on your `PATH`. To run the container
instead, mount `examples/` and map the port — for example
`docker run --rm -p 8080:8080 -v "$PWD/examples:/examples" ghcr.io/metio/jaas:latest`
with the flags adjusted to the in-container `/examples` paths, and
`--listen-address 0.0.0.0` so the port is reachable from the host.

## Step 2 — Run JaaS over the examples directory

Start JaaS with one snippet directory and one library path:

```shell
jaas \
  --snippet-directory examples/snippets/dashboards \
  --library-path examples/libraries
```

`--snippet-directory` exposes each subdirectory as a snippet whose name is the
directory name and whose entry file is `main.jsonnet`. `--library-path` makes the
libraries under `examples/libraries` importable by alias. Both flags repeat, so
you can pass several of each. The Jsonnet server binds `127.0.0.1:8080` by
default.

Confirm it started by hitting the readiness probe on the management server:

```shell
curl -i http://127.0.0.1:8081/ready
# HTTP/1.1 200 OK
```

## Step 3 — Render a snippet

`examples/snippets/dashboards/inheritance` is a self-contained snippet. Request
it by directory name:

```shell
curl http://127.0.0.1:8080/jsonnet/inheritance
```

JaaS returns the evaluated Jsonnet as JSON with `Content-Type:
application/json`. The `library-precedence` snippet imports the `examplonet`
library you exposed with `--library-path`:

```shell
curl http://127.0.0.1:8080/jsonnet/library-precedence
```

A snippet name that resolves to no file returns a `404` with a JSON error body; a
Jsonnet error returns a `400` whose body is a generic `evaluation_failed`. The full
go-jsonnet diagnostic (with file and line) is written to the terminal where jaas
runs, so locally you see it right next to the request.

## Step 4 — Pass a top-level argument

Top-level arguments arrive as URL query parameters. The `multi-tla` snippet is
`function(tags=["default"])` and joins its `tags` argument. Repeating a query
key passes a list, which becomes a JSON array TLA:

```shell
curl 'http://127.0.0.1:8080/jsonnet/multi-tla?tags=prod&tags=eu-west'
# {
#    "count": 2,
#    "joined": "prod, eu-west",
#    "list": [ "prod", "eu-west" ]
# }
```

A single occurrence of a query key (`?tags=prod`) passes a string instead of a
one-element array.

## Step 5 — Set an external variable

External variables are supplied through environment variables prefixed
`JAAS_EXT_VAR_`. The variable after the prefix is the `std.extVar` key. Restart
JaaS with the variables the `example1` snippet reads (`name` and `key`):

```shell
JAAS_EXT_VAR_name=Alice \
JAAS_EXT_VAR_key=secret-value \
jaas \
  --snippet-directory examples/snippets/dashboards \
  --library-path examples/libraries
```

Then render the snippet:

```shell
curl http://127.0.0.1:8080/jsonnet/example1
# {
#    ...
#    "person1": {
#       "external": "secret-value",
#       "name": "Alice",
#       "welcome": "Hello Alice!"
#    },
#    ...
# }
```

`std.extVar('name')` and `std.extVar('key')` resolve to the values from the
environment. External variables are read once at startup, not per request.

## Same core as the operator

The `jaas` binary evaluates Jsonnet through the same evaluation core whether it
serves HTTP locally or reconciles a `JsonnetSnippet` in operator mode. Local
rendering is the fast feedback loop for snippet authoring: a snippet that renders
here — with the same libraries available — renders identically when the operator
publishes it as an `ExternalArtifact`.

## Where to go next

- [Rendering endpoint](/rendering/rendering-endpoint/) — the request shape, snippet
  resolution, the management probes, and the stable error contract.
- [Snippets and libraries](/rendering/snippets-and-libraries/) — declaring snippets
  with `--snippet` and `--snippet-directory`, and libraries with `--library-path`.
- [External variables and TLAs](/rendering/external-variables-and-tlas/) — the full
  `JAAS_EXT_VAR_*` and query-parameter rules.
