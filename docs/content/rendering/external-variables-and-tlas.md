---
title: External variables and TLAs
description: Passing values into a render through external variables and top-level arguments.
tags: [extvars, tlas, query]
---

JaaS feeds two kinds of input into an evaluation: external variables, set by the
process owner at startup, and top-level arguments, supplied per request through
the URL query string.

## External variables

External variables come from two sources. The environment mechanism reads every
variable prefixed with `JAAS_EXT_VAR_` — the suffix is the variable name:

```shell
JAAS_EXT_VAR_name=Alice \
JAAS_EXT_VAR_key=secret \
  ./jaas --snippet-directory examples/snippets/dashboards
```

The `--ext-var KEY=VALUE` flag does the same and is repeatable. On a key
conflict, the flag takes precedence over the environment value:

```shell
./jaas \
  --snippet-directory examples/snippets/dashboards \
  --ext-var name=Alice \
  --ext-var key=secret
```

A snippet reads a variable with `std.extVar`:

```jsonnet
{
  person1: {
    name: std.extVar('name'),
    external: std.extVar('key'),
  },
}
```

Fetching `example1` with those variables set produces:

```shell
curl http://127.0.0.1:8080/jsonnet/example1
```

```json
{
  "person1": {
    "external": "secret",
    "name": "Alice",
    "welcome": "Hello Alice!"
  }
}
```

External variables are fixed at startup. Callers cannot set them per request —
that is what top-level arguments are for.

## Top-level arguments

A snippet that evaluates to a function receives top-level arguments (TLAs) from
the URL query string. The `tla-example` snippet is such a function:

```jsonnet
function(something="value", other="more", required)
  {
    person1: {
      welcome: 'Hello ' + something + '!',
      key: other,
      required: std.parseJson(required),
    },
  }
```

Each query parameter sets a TLA. A single value becomes a string:

```shell
curl 'http://127.0.0.1:8080/jsonnet/tla-example?something=Ada&required=42'
# {"person1":{"key":"more","required":42,"welcome":"Hello Ada!"},...}
```

A repeated parameter becomes a list. The `multi-tla` snippet joins whatever it
receives:

```jsonnet
function(tags=["default"])
  {
    count: std.length(tags),
    list: tags,
    joined: std.join(", ", tags),
  }
```

```shell
curl 'http://127.0.0.1:8080/jsonnet/multi-tla?tags=blue&tags=green'
# {"count":2,"joined":"blue, green","list":["blue","green"]}
```

A bare parameter with no value sets the TLA to an empty string:

```shell
curl 'http://127.0.0.1:8080/jsonnet/tla-example?something&required=0'
```

For the request and response shape these examples ride on, see the
[rendering endpoint](/rendering/rendering-endpoint/).
