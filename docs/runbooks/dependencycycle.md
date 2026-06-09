<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Reason: DependencyCycle

## Symptom

`READY=False`, `REASON=DependencyCycle`. The Message names the snippet that closes the cycle.

## Cause

The snippet's `spec.sourceRef` chain transitively points back at the snippet itself. The reconciler detects this and refuses to publish so chained snippets don't loop forever (each republish would trigger every downstream snippet to re-render, which would re-trigger the upstream, and so on).

Two cycle shapes:

1. **Direct sourceRef cycle:** `A.spec.sourceRef → ExternalArtifact/A`. A snippet sourcing from its own published artifact.
2. **Library-mediated cycle:** `A.spec.libraries → JsonnetLibrary/L`, where `L.spec.sourceRef → ExternalArtifact/A` (or a longer chain back to A).

The validating webhook (`--enable-webhook`) rejects new CRs that introduce a cycle at admission; the reconciler check is a fallback for when admission is bypassed or the cycle is introduced retroactively (e.g., adding a new library that closes a loop with existing snippets).

## Diagnosis

Walk the chain manually:

```shell
kubectl get jsonnetsnippet <name> -o jsonpath='{.spec.sourceRef}' && echo
# Then inspect what that sourceRef points at, and what it sources from in turn.
```

For library-mediated cycles, the chain is:

```text
snippet A.spec.libraries[i] → JsonnetLibrary L → L.spec.sourceRef → ExternalArtifact X → snippet that publishes X
```

If the publishing snippet at the end is A, you have a cycle.

## Remediation

Break the cycle by removing the back-edge. Common fixes:

- detach a library from its sourceRef (inline its files instead, if small)
- have the upstream snippet publish a smaller artifact the downstream doesn't need to re-consume
- restructure so the shared data lives in a static ConfigMap referenced as a sourceRef-equivalent, not in a snippet output
