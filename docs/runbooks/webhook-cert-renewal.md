<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Operator runbook: self-signed webhook cert renewal failing

Fires when `jaas_webhook_cert_renewal_failures_total` has increased above the configured per-hour threshold. The `Renewer` background goroutine rotates the self-signed TLS material every `Validity / 3` (typically every few months for a year-long cert). When it can't, the existing cert keeps working until its natural expiry — at which point the apiserver stops trusting the chain and **every JsonnetSnippet admission fails cluster-wide with `x509` errors**.

## Symptoms

- `JaaSWebhookCertRenewalFailing` alert is firing (severity: critical).
- Operator pod logs carry repeated `Self-signed webhook cert renewal failed` warnings at the `Renewer.Interval` cadence.
- `kubectl describe validatingwebhookconfiguration <name>` shows a `caBundle` that hasn't rotated since the failures started.
- The pod stays `Ready=True` — the renewer's failures don't gate the readiness probe.

## Diagnose

The most common causes, in order of frequency:

### Cause A — RBAC drift on the named VWC

The operator's ClusterRole pins `resourceNames: [<VWCName>]` on the `validatingwebhookconfigurations` patch verb. A chart upgrade that changes `operator.webhook.vwcName` (or a manual chart edit) leaves the running pod patching a name it no longer has permission for.

```shell
kubectl auth can-i patch validatingwebhookconfiguration/<vwc-name> \
  --as=system:serviceaccount:<namespace>:<operator-sa>
```

If the answer is "no", the chart's `operator-cluster` ClusterRole needs the current VWC name added to `resourceNames` (or the running pod restarted to pick up the new name).

### Cause B — VWC renamed out from under the operator

A separate controller (admission policy automation, GitOps drift correction) renamed the VWC. The operator is patching a stale name.

```shell
kubectl get validatingwebhookconfigurations \
  -l 'app.kubernetes.io/instance=<release-name>'
```

If the live name differs from the operator's `-webhook-vwc-name` flag, redeploy the operator with the correct flag or rename the VWC back.

### Cause C — `CertDir` gone read-only

The chart mounts `CertDir` as an `emptyDir` by default. A `kubectl apply` that adds a `readOnlyRootFilesystem: true` security context or a sidecar that re-mounts the volume can break writes.

```shell
kubectl exec -n <ns> <operator-pod> -- ls -l /tmp/k8s-webhook-server/serving-certs/
kubectl exec -n <ns> <operator-pod> -- touch /tmp/k8s-webhook-server/serving-certs/.write-probe
```

If the touch fails, the security-context or volume mount needs fixing.

## Remediate

1. **Fix the root cause** (RBAC, name, or mount).
2. **Roll the operator pod** to force a fresh bootstrap of the cert and a re-patch of the VWC:

   ```shell
   kubectl rollout restart deployment -n <ns> <operator-deployment>
   ```

   The new pod's bootstrap path goes through the dual-CA union (DD8), so existing replicas stay trusted across the rotation.

3. **Verify renewal is healthy** after the bounce — the `jaas_webhook_cert_renewal_failures_total` counter should stop increasing, and the alert clears once the `for:` window passes.

## When to consider switching to cert-manager

If the self-signed renewer keeps tripping over your environment's RBAC story or pod-security policies, the chart supports `operator.webhook.certMode: cert-manager` — cert-manager handles the rotation, the operator just mounts the secret. Trade-off: requires cert-manager installed and an Issuer configured.
