---
title: Snippets and libraries
description: Declaring snippets and libraries on disk for the HTTP renderer, and how imports resolve.
tags: [snippets, libraries, imports]
---

In renderer mode you declare snippets and libraries on disk through command-line
flags. A snippet becomes reachable at the
[rendering endpoint](/usage/rendering-endpoint/); a library is importable by any
snippet.

## Directory snippets

Point `--snippet-directory` at a directory whose subdirectories each hold a
`main.jsonnet`. Each subdirectory name becomes a snippet name:

```shell
./jaas --snippet-directory examples/snippets/dashboards
curl http://127.0.0.1:8080/jsonnet/example1
```

Given this layout, `example1` resolves to
`examples/snippets/dashboards/example1/main.jsonnet`:

```text
examples/snippets/dashboards
├── example1
│   └── main.jsonnet
├── tla-example
│   └── main.jsonnet
└── multi-tla
    └── main.jsonnet
```

## File snippets

Point `--snippet` at an individual Jsonnet file. The file path becomes the
snippet name:

```shell
./jaas --snippet examples/snippets/example.jsonnet
curl http://127.0.0.1:8080/jsonnet/examples/snippets/example.jsonnet
```

Both `--snippet` and `--snippet-directory` are repeatable, so one process serves
several roots:

```shell
./jaas \
  --snippet-directory examples/snippets/dashboards \
  --snippet examples/snippets/example.jsonnet
```

## Libraries

Point `--library-path` at a directory that holds importable Jsonnet libraries. A
snippet imports a library by its path under that directory:

```text
examples/libraries
├── examplonet
│   └── main.libsonnet
└── text
    └── welcome.txt
```

```shell
./jaas \
  --snippet-directory examples/snippets/dashboards \
  --library-path examples/libraries
```

A snippet then imports the library with a string-literal path:

```jsonnet
local examplonet = import 'examplonet/main.libsonnet';

{
  person1: {
    name: examplonet.standard,
    welcome: 'Hello ' + self.name + '!',
  },
}
```

`--library-path` is repeatable. When the same import path matches under more than
one library directory, the rightmost matching directory wins — list the override
directory last.

## Embedding non-Jsonnet files

Use `importstr` to pull the raw contents of a file under a library path into a
snippet as a string. The `embed-text` example reads a text file from the
`text/` library:

```jsonnet
{
  banner: importstr 'text/welcome.txt',
  length: std.length(self.banner),
}
```

Any file reachable under a `--library-path` directory or the snippet's own
directory can be `import`-ed or `importstr`-ed by any snippet. Scope these
directories tightly — see
[Evaluation and security](/usage/evaluation-and-security/).

For the operator-side equivalent — `JsonnetLibrary` CRDs and OCI-mounted shared
libraries — see [Jsonnet libraries](/usage/jsonnet-libraries/).
