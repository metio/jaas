---
title: InvalidSpec
description: The snippet's spec contains a field combination the reconciler cannot process, such as a missing entryFile or conflicting source fields
tags: [runbooks, troubleshooting, lifecycle]
---

## Symptom

`READY=False`, `REASON=InvalidSpec`. The condition Message names which field is at fault.

## Cause

Spec-level validation that admission should have caught but the reconciler is enforcing as a fallback:

- `spec.entryFile` is empty
- both `spec.files` and `spec.sourceRef` are set (mutually exclusive)
- neither `spec.files` nor `spec.sourceRef` is set
- `spec.entryFile` does not match any key in the resolved file map

## Diagnosis

```shell
kubectl --namespace <ns> describe jsonnetsnippet <name>
```

Read the Message — it names the field.

## Remediation

Fix the spec and reapply. If the validating webhook is enabled (`--enable-webhook`), `kubectl apply` rejects the invalid spec at admission instead of letting it land and fail later.

If you're seeing `InvalidSpec` on apply through the webhook, that's a bug — file an issue with the rejected manifest.
