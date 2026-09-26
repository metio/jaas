---
title: Operator unavailable
description: The jaas process is running and serving Jsonnet, but its controller manager cannot start, so no JsonnetSnippet is being reconciled
tags: [runbooks, troubleshooting, operator, networking, rbac]
---

Linked from the `JaaSOperatorUnavailable` and `JaaSOperatorFlapping` alerts, and
from `GET /operator` returning `503`. The pod is up and the Jsonnet renderer
answers; the controller manager is what cannot start.

## Symptom

```text
jaas_operator_available 0
```

```shell
kubectl --namespace <jaas-ns> exec deploy/<release> -- \
  wget --quiet --output-document=- http://127.0.0.1:8081/operator
# {"status":"unavailable","reason":"create manager: setup SnippetReconciler: register watch indexes: failed to get server groups: Get \"https://10.24.64.1:443/api\": dial tcp 10.24.64.1:443: i/o timeout","since":"2026-09-24T09:14:02Z","attempts":7}
```

- Every `JsonnetSnippet` stays at its last status; new ones stay
  `Ready=Unknown`. Nothing reconciles.
- `/jsonnet` evaluations still succeed, and the storage server still serves the
  tarballs published before the outage. Neither needs an apiserver.
- The pod is `Running`, and `Ready` only if it had synced once before the
  failure or `--readiness-requires-operator=never` is set. It does **not**
  restart: liveness is unconditional, and the manager is retried in place.

## Cause

Everything the manager needs from the apiserver happens while it is being built —
discovery for the field indexes, the RESTMapper lookups behind the Flux source
watches — so anything that blocks those calls keeps the operator down:

1. **Egress to the apiserver is denied.** A cluster-wide default-deny
   NetworkPolicy with no matching allow rule is the common case. The reason
   string names the apiserver address and ends in `i/o timeout` or
   `connection refused`. See [Network policy](/security/network-policy/) for the
   rule each policy engine needs.
2. **The operator's own ClusterRole is incomplete.** The reason carries
   `forbidden`, naming the verb and resource. The watch-layer variant of this is
   in [operator-watch-silent](/runbooks/operator-watch-silent/).
3. **The JaaS CRDs are not installed.** The reason names the missing kind. A
   chart install with `crds.create=false` and no out-of-band apply lands here.
4. **The apiserver is genuinely unreachable** — control-plane outage, DNS
   failure inside the cluster, a mesh sidecar that has not started yet.
5. **The webhook has no certificate.** With
   `operator.webhook.certMode=cert-manager` the reason is
   `open …/tls.crt: no such file or directory` — the Certificate has not been
   issued, or the Secret is not mounted. The manager syncs its cache and then
   fails on the webhook server, so the reading alternates between available and
   unavailable until the certificate lands; that is the shape
   `JaaSOperatorFlapping` catches and `JaaSOperatorUnavailable` can miss.
6. **The leader-election lease was lost** after a period of healthy operation.
   The reason is `leader election lost`; the next manager blocks on acquiring
   the lease again, which is the behaviour the lease exists for. Repeated losses
   point at apiserver latency or a clock problem, not at JaaS.

## Diagnosis

```shell
# The reason, verbatim, with how long it has been true and how many starts.
kubectl --namespace <jaas-ns> port-forward deploy/<release> 8081:8081 &
curl --silent http://127.0.0.1:8081/operator

# The same reason in the logs, once per fresh cause plus one per retry.
kubectl --namespace <jaas-ns> logs deploy/<release> | grep -i "operator unavailable\|still unavailable"

# Is it reachability? Resolve and dial the apiserver from the pod's namespace.
kubectl --namespace <jaas-ns> get networkpolicy
kubectl --namespace default get endpoints kubernetes
```

An `i/o timeout` against the address that `get endpoints kubernetes` reports,
with a default-deny policy in the namespace, is cause 1. A `forbidden` naming a
resource is cause 2. A `no matches for kind` is cause 3. A missing `tls.crt`,
with an attempt count that keeps climbing while the gauge flips between 0 and 1,
is cause 5.

## Remediation

Fix the cause; no restart is needed. The manager is retried on a backoff that
caps at five minutes, so a repaired cluster recovers within that at worst, and
`jaas_operator_available` returns to `1` on its own.

For denied egress, admit the apiserver. On the Calico, Cilium, and
ClusterNetworkPolicy engines the chart renders the rule for you once
`networkPolicy.egress.enabled` is set; on the upstream `kubernetes` engine it
needs the endpoint CIDRs:

```yaml
networkPolicy:
  enabled: true
  egress:
    enabled: true
    kubernetesAPI:
      ipBlocks:
        - 10.24.64.1/32
      port: 6443
```

For an incomplete ClusterRole, reinstall or upgrade the chart so the generated
roles match the running binary, then confirm:

```shell
kubectl auth can-i --as=system:serviceaccount:<jaas-ns>:<release> \
  list jsonnetsnippets.jaas.metio.wtf --all-namespaces
```

For missing CRDs, apply them and wait — the operator picks them up without a
restart:

```shell
kubectl apply --server-side --filename config/crd/bases/
```

## Related

- [Degraded operator](/running/operations/#degraded-operator) — the supervision
  behaviour and the three signals.
- [operator-pod-down](/runbooks/operator-pod-down/) — the pod itself is not
  ready, which is a different failure.
- [operator-watch-silent](/runbooks/operator-watch-silent/) — an informer that
  cannot start after the manager already has.
