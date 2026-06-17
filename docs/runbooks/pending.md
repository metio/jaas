---
title: Pending
description: The snippet has been observed by the operator but its first reconcile pass has not yet completed
tags: [runbooks, troubleshooting, lifecycle]
---

## Symptom

`kubectl get jsonnetsnippet` shows `READY=Unknown` (or `False` with `REASON=Pending`) immediately after the snippet was created or its spec was updated.

## Cause

The operator has observed the CR but hasn't completed its first reconcile pass yet. Transient by design.

## Diagnosis

```shell
kubectl describe jsonnetsnippet <name>
```

If the timestamp on the `Pending` condition is older than ~30 seconds, the operator is either:

- not running (`kubectl --namespace <jaas-namespace> get pods`)
- backed up on its work queue (check `kubectl logs deploy/jaas` and the `workqueue_depth` metric)
- not the leader (multi-replica install, `kubectl --namespace <jaas-namespace> get lease` shows the holder)

## Remediation

If transient, wait. If persistent:

- restart the operator: `kubectl rollout restart deploy/jaas`
- inspect the operator's logs for errors

If the snippet is stuck in `Pending` because the work queue is saturated, increase replicas (with leader election ON) or raise the rate-limiter budget (`--rerender-rate`, `--rerender-burst`).
