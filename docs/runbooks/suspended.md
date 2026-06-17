---
title: Suspended
description: Reconciliation is intentionally paused because spec.suspend is true; the last published artifact remains intact
tags: [runbooks, troubleshooting, lifecycle]
---

## Symptom

`READY=False`, `REASON=Suspended`. The snippet's `spec.suspend` is set to `true`.

## Cause

An operator (or automation) paused reconciliation for this snippet, typically to investigate a downstream issue without the artifact being rewritten underneath them. The previously-published `ExternalArtifact` and the on-disk tarball are left intact — downstream Flux consumers continue serving the last successful render.

This is a normal, intentional state. It is not a failure.

## Diagnosis

```shell
kubectl get jsonnetsnippet <name> --output jsonpath='{.spec.suspend}'
```

If the value is `true`, the suspension is set on the spec. Check `kubectl describe` for the last condition transition timestamp to see when it happened.

## Remediation

To resume reconciliation:

```shell
kubectl patch jsonnetsnippet <name> --type=merge --patch '{"spec":{"suspend":false}}'
```

Or remove the field entirely:

```shell
kubectl edit jsonnetsnippet <name>
# delete the `suspend: true` line under spec
```

The next reconcile picks up the snippet's current spec and republishes if anything has drifted.
