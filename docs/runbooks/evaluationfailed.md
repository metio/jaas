---
title: EvaluationFailed
description: The Jsonnet snippet failed to evaluate due to a syntax error, runtime error, or unresolved import
tags: [runbooks, troubleshooting, evaluation]
---

## Symptom

`READY=False`, `REASON=EvaluationFailed`. The Message contains the raw go-jsonnet diagnostic — file name, line, column, and the underlying error.

## Cause

The snippet failed to evaluate. Three broad categories:

- **Syntax error** — unclosed brace, missing comma, bad indent.
- **Runtime error** — `std.extVar('missing')` for an unset variable, division by zero, `error '...'` thrown explicitly.
- **Import error** — `import 'missing.libsonnet'` resolves to nothing in the snippet's file map or library imports.

## Diagnosis

Read the Message — it names the file and line. Reproduce locally:

```shell
# Pull the snippet's files into a tempdir, then evaluate.
kubectl get jsonnetsnippet <name> --output json | jq -r '.spec.files["main.jsonnet"]' > /tmp/main.jsonnet
jsonnet /tmp/main.jsonnet
```

For sourceRef-based snippets, fetch the tarball:

```shell
SOURCE_URL=$(kubectl get gitrepository <name> --output jsonpath='{.status.artifact.url}')
curl -sL "$SOURCE_URL" | tar -xz -C /tmp/snippet
jsonnet /tmp/snippet/<entry-file>
```

## Remediation

Fix the snippet (or its libraries / source) and re-apply.

The diagnostic message can leak the on-disk path of the snippet — fine in-cluster, worth gating behind a flag if exposed to untrusted callers in the future.
