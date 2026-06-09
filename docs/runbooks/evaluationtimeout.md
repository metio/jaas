<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Reason: EvaluationTimeout

## Symptom

`READY=False`, `REASON=EvaluationTimeout`. The snippet's eval ran longer than the operator's `--evaluation-timeout`.

## Cause

Snippets are evaluated synchronously per reconcile. The deadline is wall-clock, not CPU — but go-jsonnet has no mid-evaluation cancellation, so a snippet that runs over the deadline still keeps consuming CPU on the operator pod until it returns naturally.

Common triggers:

- a snippet recursing deeper than necessary (try lowering `--max-stack` to surface this as a stack-limit error instead, then optimize)
- a snippet that loads a huge sourceRef tarball and walks it
- a snippet that calls `std.set` / `std.uniq` over a very large array

## Diagnosis

Time it locally:

```shell
time jsonnet /tmp/snippet/<entry-file>
```

If it takes seconds locally, the operator's bound is too tight. If it takes minutes locally, the snippet itself is the problem.

## Remediation

Two paths:

1. **Optimize the snippet.** Memoize repeated work into `local` bindings, narrow the input set, avoid `std.flattenDeepArrays` over deep trees.
2. **Raise the operator's bound.** `--evaluation-timeout=30s` (default `5s`) gives more headroom. Pair with `resources.cpu` headroom in the chart so the slow snippet doesn't starve other reconciles.

For pathological inputs, consider splitting the snippet — render the slow part less often via a separate snippet others source from (see `examples/operator/chained-snippets.yaml`).
