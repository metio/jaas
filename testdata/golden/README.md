<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
 -->

# Golden files for example end-to-end tests

The `*.json` files in this directory are reference outputs for the jsonnet
files under `examples/`. The corresponding tests live in `examples_test.go`
and use `runInBackground` (the same testable seam as the rest of `main_test.go`)
to boot jaas against the real examples directory, fetch each `/jsonnet/...`
endpoint, and compare the response to the golden.

## Regenerating

After editing an example, run:

```shell
nix develop --command go test -update ./...
```

Inspect the diff under `testdata/golden/` and commit it alongside the example change.

## How comparison works

Both the response and the golden are parsed as JSON and compared with
`reflect.DeepEqual` on the parsed values — so whitespace and key ordering
inside objects don't cause spurious failures. Only the *semantic* JSON
content needs to match.
