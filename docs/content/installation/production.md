---
title: Production
description: A decision-oriented checklist for hardening a JaaS operator install before serving production traffic.
tags: [installation, production, ha, security, observability]
---

The chart's defaults are safe for an initial install but not optimised for
sustained production workloads. Work through these decisions before exposing JaaS
to real traffic. Each links to the detailed guide.

## 1. Pick a storage backend

The single largest decision. Artifacts must survive pod restarts and, for HA,
be readable by every replica simultaneously.

| Backend | Persistence | Multi-replica HA |
|---|---|---|
| `local` + emptyDir (chart default) | No | No |
| `local` + RWO PVC | Yes | No — single replica only |
| `local` + RWX PVC | Yes | Yes — requires RWX storage class |
| `s3` | Yes | Yes — leader writes, all replicas read |

For cloud installs, `s3` (AWS S3, MinIO, Ceph RGW, GCS S3-compat API) is the
recommended backend. Pair it with leader election (on by default) so only the
lease-holder writes. For on-prem, a PVC with the access mode your storage class
supports is the practical path.

Full configuration options and artifact retention are covered in
[Storage and HA](/usage/storage-and-ha/) — including the garbage-collection grace
period (`--artifact-gc-grace`) that keeps a just-superseded revision fetchable for
a short window, so a consumer that read `status.artifact` moments before pruning
doesn't 404 on the revision it pinned.

Minimal S3 values (IRSA on EKS):

```yaml
operator:
  enabled: true
  serviceAccount:
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT:role/jaas-operator
  storage:
    backend: s3
    s3:
      endpoint: s3.amazonaws.com
      bucket: my-jaas-artifacts
      prefix: prod
      region: eu-west-1
      useSSL: true
      # Leave accessKey/secretKey empty — IAM role via SA annotation.
```

## 2. Size CPU and memory

The chart defaults (64 MiB memory, 32m CPU) are fine for a quickstart but will
OOM under sustained snippet rendering. Each in-flight evaluation is essentially
uncancellable mid-flight — go-jsonnet has no mid-evaluation cancellation — so
CPU and memory limits must accommodate the worst-case concurrent eval load.

Set `--max-artifact-bytes` to cap the rendered output size per snippet so a
runaway template can't allocate unbounded memory before the timeout fires.

See [Scale and capacity](/installation/scale-and-capacity/) for the resident-memory
reasoning behind a request/limit, and [Evaluation and security](/usage/evaluation-and-security/)
for the concurrent-eval cap, timeout defaults, and how to tune them.

```yaml
resources:
  memory: 256Mi
  cpu: 100m

operator:
  storage:
    maxArtifactBytes: 16777216  # 16 MiB; fails with ReasonArtifactTooLarge
```

## 3. Enable observability

The chart ships a metrics endpoint (on by default at port `8083`), an opt-in
`ServiceMonitor`, and an opt-in `PrometheusRule` with a starter alert set. Turn
them on and wire the Prometheus selector labels before deploying:

```yaml
operator:
  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      labels:
        release: kube-prom          # match your Prometheus's serviceMonitorSelector
    prometheusRule:
      enabled: true
      labels:
        release: kube-prom          # match your Prometheus's ruleSelector
      extraAlertLabels:
        team: platform              # Alertmanager routing label
```

`serviceMonitor.labels`, `prometheusRule.labels`, and
`prometheusRule.extraAlertLabels` are three distinct label knobs:
`serviceMonitor.labels` and `prometheusRule.labels` control which Prometheus
instance picks up each CRD object; `extraAlertLabels` adds routing labels to
individual alerts (for Alertmanager), not to the rule object.

The shipped alert set and all custom JaaS metrics are documented in
[Observability](/observability/).

## 4. Enable the admission webhook {#admission-webhook-tls}

The webhook rejects spec invariant violations — ext-var key collisions, library
alias shadowing, import cycles — at `kubectl apply` time instead of at
reconcile time. Pick a cert mode:

```yaml
# Option A: cert-manager (recommended when cert-manager is installed)
operator:
  webhook:
    enabled: true
    certMode: cert-manager
    certManager:
      enabled: true
      issuerRef:
        kind: ClusterIssuer
        name: letsencrypt-prod

# Option B: self-signed (no cert-manager required)
operator:
  webhook:
    enabled: true
    certMode: self-signed
```

The default `failurePolicy: Fail` blocks every `JsonnetSnippet` create/update
cluster-wide when the webhook is unavailable. During a rolling update the window
is typically under five seconds (leader election releases the lease on
SIGTERM). If your GitOps tooling cannot tolerate that, scope the webhook via
`operator.webhook.namespaceSelector` or `operator.webhook.objectSelector`, or
switch to `failurePolicy: Ignore` and rely on the reconciler-side fallback.

Full cert provisioning and failurePolicy trade-offs are covered in
[Admission webhook](/usage/admission-webhook/).

## 5. Lock down tenant RBAC

Every `JsonnetSnippet` runs impersonated as its `spec.serviceAccountName` (or
the `--default-service-account` fallback). The operator's own ServiceAccount
only needs `serviceaccounts/token: create` — every other API call (library
reads, source fetches, `ExternalArtifact` writes) is done under the tenant SA's
RBAC, so a compromised snippet can only reach what its SA is allowed to.

Minimum per-tenant `Role`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: <tenant-namespace>
  name: jaas-tenant
rules:
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [externalartifacts]
    verbs: [get, create, update, patch]
  - apiGroups: [jaas.metio.wtf]
    resources: [jsonnetlibraries]
    verbs: [get, list]
  - apiGroups: [source.toolkit.fluxcd.io]
    resources: [gitrepositories, ocirepositories, buckets, externalartifacts]
    verbs: [get]
```

When `operator.watchNamespaces` is set, the chart automatically switches from a
ClusterRoleBinding to per-namespace RoleBindings. Full RBAC layout and
NetworkPolicy notes for `spec.sourceRef` fetches are in
[Tenancy and RBAC](/usage/tenancy-and-rbac/).

## 6. Plan for upgrades and disaster recovery

Calendar-based releases run every Monday. Chart upgrades are `helm upgrade
--install`; the chart ships CRDs under `templates/` so schema changes apply
automatically. Read
[Upgrading](/installation/upgrading/) before
each upgrade — releases that change immutable `spec.selector.matchLabels` fields
require a manual `kubectl delete deploy/jaas` first.

Three runbooks to bookmark before go-live:

- [storage-recovery](/runbooks/storage-recovery/) — PVC loss, S3 outages,
  disk-full, downstream 404s.
- [rbacdenied](/runbooks/rbacdenied/) — tenant SA missing a verb, ExternalArtifact
  write forbidden, Flux source CRD not installed.
- [operator-watch-silent](/runbooks/operator-watch-silent/) — the one failure
  mode JaaS cannot surface in snippet status (operator's own ClusterRole missing
  a verb so controller-runtime's informer silently fails).

## A complete production values.yaml

```yaml
replicas:
  min: 2
  max: 5

resources:
  memory: 256Mi
  cpu: 100m

image:
  pullPolicy: IfNotPresent

namespace:
  create: true
  podSecurity:
    enforce: restricted

operator:
  enabled: true
  defaultServiceAccount: ""   # force per-snippet spec.serviceAccountName

  serviceAccount:
    create: true
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT:role/jaas-operator  # TODO

  storage:
    backend: s3
    s3:
      endpoint: s3.amazonaws.com  # TODO
      bucket: my-jaas-artifacts   # TODO
      prefix: prod
      region: eu-west-1           # TODO
    maxArtifactBytes: 16777216

  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      labels:
        release: kube-prom        # TODO
    prometheusRule:
      enabled: true
      labels:
        release: kube-prom        # TODO
      extraAlertLabels:
        team: platform            # TODO

  webhook:
    enabled: true
    certMode: cert-manager
    certManager:
      issuerRef:
        kind: ClusterIssuer
        name: letsencrypt-prod    # TODO

  leaderElection:
    enabled: true

  cleanupOnDelete:
    enabled: true
```

Apply with:

```shell
helm upgrade --install jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system --create-namespace \
  --values production-values.yaml \
  --wait --timeout 5m
```

## Next steps

- [Operations](/installation/operations/) — day-two tasks: rolling restarts,
  storage sweeping, finalizer teardown.
- [Scale and capacity](/installation/scale-and-capacity/) — replica sizing, the
  HA model, throughput knobs, and the saturation signals to watch.
- [Configuration reference](/installation/configuration/) — every flag and default.
- [Runbooks](/runbooks/) — incident response.
