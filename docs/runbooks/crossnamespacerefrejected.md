<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Reason: CrossNamespaceRefRejected

## Symptom

`READY=False`, `REASON=CrossNamespaceRefRejected`. The Message names the offending reference (a library or a sourceRef).

## Cause

The operator is running with `--no-cross-namespace-refs=true` (the chart default) and the snippet references a library or Flux source in a different namespace.

This is a deliberate isolation control — it mirrors Flux's `--no-cross-namespace-refs` and stops a tenant in namespace A from reaching libraries / sources in namespace B without an explicit relationship.

## Diagnosis

Inspect the spec and identify which reference points outside the snippet's namespace:

```shell
kubectl get jsonnetsnippet <name> -n <ns> -o yaml | grep -E "namespace:|sourceRef:|libraries:"
```

## Remediation

Three options, by isolation strength:

1. **(recommended)** Duplicate the library / source CR into the snippet's namespace.
2. Promote the library to an OCI volume — mount via the chart's `additionalLibraries` map. Becomes part of the operator's filesystem, available to every snippet without a cross-namespace CR ref.
3. **(loosen isolation, cluster-wide)** Set `--no-cross-namespace-refs=false` on the operator. Affects every tenant in the cluster — only do this when tenants are mutually trusting.
