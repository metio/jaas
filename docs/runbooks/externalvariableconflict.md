---
title: ExternalVariableConflict
description: The snippet declares an external variable key already claimed by the operator via --ext-var
tags: [runbooks, troubleshooting, evaluation]
---

## Symptom

`READY=False`, `REASON=ExternalVariableConflict`. The Message names the conflicting key.

## Cause

The snippet's `spec.externalVariables` declares a key that the operator already owns via `--ext-var` (cluster operator-level). Operator keys win by design — they're how the cluster admin pins cluster-scoped values like `cluster`, `region`, `environment` so a tenant snippet can't override them.

## Diagnosis

```shell
# Which keys does the operator own?
kubectl --namespace <jaas-ns> get pod --selector app.kubernetes.io/name=jaas --output yaml | grep -A1 "\--ext-var="
```

Cross-reference with the snippet's `spec.externalVariables`.

## Remediation

Rename the conflicting key in the snippet, or remove it from the snippet entirely (the operator-level value flows through automatically).

If the snippet legitimately needs a different value, that's a structural problem — the snippet shouldn't ship with an opinion that overrides a cluster-wide invariant. Re-discuss with the cluster admin.

The validating webhook (`--enable-webhook`) catches this at admission so `kubectl apply` rejects it before it lands. The reconciler enforcing the same rule is a fallback for when admission is bypassed.
