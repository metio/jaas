---
title: SourceNotReady
description: The referenced Flux source CR exists but has not yet reported Ready=True or has no published artifact
tags: [runbooks, troubleshooting, sources]
---

## Symptom

`READY=False`, `REASON=SourceNotReady`. The Message names the source CR (`GitRepository/foo`, `ExternalArtifact/bar`, etc.).

## Cause

The Flux source CR the snippet references exists but its own `status.conditions[Ready]` is not yet True (or `status.artifact` is empty). The operator refuses to fetch from a source it can't trust as ready.

For chained snippets specifically: the upstream snippet may have failed reconciliation, so its ExternalArtifact is stale or unpopulated.

## Diagnosis

```shell
kubectl describe <kind> <source-name>
# Look for the Ready condition and any error messages.
```

For Flux sources, also check the source-controller logs:

```shell
kubectl --namespace flux-system logs deploy/source-controller | grep <source-name>
```

## Remediation

Fix the upstream source. The operator watches Flux source kinds and will re-reconcile the snippet automatically when the source flips to Ready=True — no manual nudge required.
