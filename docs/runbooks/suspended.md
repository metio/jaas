---
title: Suspended
description: Reconciliation is intentionally paused because spec.suspend is true; the last published artifact remains intact
tags: [runbooks, troubleshooting, lifecycle]
---

## Symptom

`READY=False`, `REASON=Suspended`. The snippet's `spec.suspend` is set to `true`.

## Cause

An operator (or automation) paused reconciliation for this snippet, typically to investigate a downstream issue without the artifact being rewritten underneath them. The last-published `ExternalArtifact` and its on-disk tarball stay intact — downstream Flux consumers keep serving the last successful render.

This is a normal, intentional state. It is not a failure.

## Diagnosis

```shell
kubectl --namespace <ns> get jsonnetsnippet <name> --output jsonpath='{.spec.suspend}'
```

If the value is `true`, the suspension is set on the spec. Check `kubectl describe` for the last condition transition timestamp to see when it happened.

## Remediation

To resume reconciliation:

```shell
kubectl --namespace <ns> patch jsonnetsnippet <name> --type=merge --patch '{"spec":{"suspend":false}}'
```

Or remove the field entirely:

```shell
kubectl --namespace <ns> edit jsonnetsnippet <name>
# delete the `suspend: true` line under spec
```

The next reconcile picks up the snippet's current spec and republishes if anything has drifted.
