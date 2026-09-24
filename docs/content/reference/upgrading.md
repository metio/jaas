---
title: Upgrading
description: Actions required when upgrading JaaS between releases — a release with no section here needs no migration.
tags: [installation, upgrade, migration, helm]
---

This page lists the actions required when upgrading JaaS. A release with no
section here needs no migration — a plain `helm upgrade` (or new image tag)
suffices.

## 2026.9.24

An operator that cannot reach the apiserver no longer ends the process. The pod
stays `Running` and retries its manager in place, so the failures that used to
show up as `CrashLoopBackOff` — egress denied to the apiserver, an incomplete
ClusterRole, CRDs not installed — now show up as a pod that is `Running`, not
`Ready`, and reconciling nothing. `/jsonnet` keeps serving throughout.

Alerting that watched container restarts for this class of failure goes quiet.
Watch the operator's own signals instead:

```promql
# The operator is not reconciling at all.
jaas_operator_available == 0
```

`GET /operator` on the management port carries the same state with the reason
attached, and [operator-unavailable](/runbooks/operator-unavailable/) covers
diagnosis. Readiness behaves as before: a pod whose manager has never synced
stays out of its Services, which still halts a rolling update.
