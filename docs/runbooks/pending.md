<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Reason: Pending

## Symptom

`kubectl get jsonnetsnippet` shows `READY=Unknown` (or `False` with `REASON=Pending`) and the snippet has just been created or just had its spec updated.

## Cause

The operator has observed the CR but hasn't completed its first reconcile pass yet. Transient by design.

## Diagnosis

```shell
kubectl describe jsonnetsnippet <name>
```

If the timestamp on the `Pending` condition is older than ~30 seconds, the operator is either:

- not running (`kubectl get pods -n <jaas-namespace>`)
- backed up on its work queue (check `kubectl logs deploy/jaas` and the `workqueue_depth` metric)
- not the leader (multi-replica install, `kubectl get lease -n <jaas-namespace>` shows the holder)

## Remediation

If transient, wait. If persistent:

- restart the operator: `kubectl rollout restart deploy/jaas`
- inspect the operator's logs for errors

If the snippet is stuck in `Pending` because the work queue is saturated, increase replicas (with leader election ON) or raise the rate-limiter budget (`--rerender-rate`, `--rerender-burst`).
