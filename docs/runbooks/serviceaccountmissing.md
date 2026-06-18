---
title: ServiceAccountMissing
description: The snippet specifies no ServiceAccount and the operator has no --default-service-account configured
tags: [runbooks, troubleshooting, rbac]
---

## Symptom

`READY=False`, `REASON=ServiceAccountMissing`.

## Cause

The snippet omitted `spec.serviceAccountName` AND the operator was started without `--default-service-account`. The operator refuses to reconcile a snippet with no effective ServiceAccount because every reconcile mints a tenant token from that SA — without one, there's nothing to impersonate.

## Diagnosis

```shell
kubectl --namespace <ns> get jsonnetsnippet <name> --output jsonpath='{.spec.serviceAccountName}'
```

Empty? Either the snippet must set it, or the cluster operator must configure a default.

## Remediation

Pick one:

1. **Snippet-side (preferred for multi-tenant setups):** set `spec.serviceAccountName: <existing-sa>` on every snippet. Each tenant uses its own SA → least-privilege impersonation.
2. **Cluster-side (single-tenant clusters):** start the operator with `--default-service-account=<sa-name>`. Every snippet without an explicit SA impersonates this one. The default SA must exist in **every snippet's namespace** — the operator looks it up per-reconcile.
