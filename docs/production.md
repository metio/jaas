<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Production deployment guide

This page is the "what to enable for prod" complement to [`quickstart.md`](quickstart.md). The chart's defaults are conservative — many features ship opt-in so a fresh `helm install` doesn't surprise operators with knobs they didn't ask for. Production-shape installs flip several of those switches; this guide names which and why.

For full chart-values reference, see [`charts/jaas/values.yaml`](https://github.com/metio/helm-charts/blob/main/charts/jaas/values.yaml) in the metio/helm-charts repo (every field is commented). For the operator architecture, see [`README.md`'s "Operator Mode"](../README.md#operator-mode). For incident response, see [`docs/runbooks/`](runbooks/).

## What "production" means here

Six things this guide assumes you care about that the quickstart skips:

1. **Durable storage** for published artifacts (tarballs survive pod restarts).
2. **Observability** wired into Prometheus + Alertmanager.
3. **Admission webhook** on so misconfigurations are caught at `kubectl apply`.
4. **Scaling headroom** — either multi-replica HA or sized resources for sustained load.
5. **Disaster recovery** — runbooks and upgrade hygiene rehearsed.
6. **Tenant isolation** — per-tenant ServiceAccount RBAC documented and applied.

If any of those don't apply to your install, ignore the relevant sections. Nothing in this guide is required; defaults are safe even for production, just not optimized.

## 1. Pick a storage backend

The single biggest production decision. JaaS publishes tarballs to a backend; downstream Flux consumers fetch them via a stable HTTP URL.

| Backend | When to pick | Multi-replica HA? | Persistence across pod restarts? |
|---|---|---|---|
| `local` (chart default) with **emptyDir** | Demo / non-critical | No (single replica only) | No — tarballs lost on restart, all snippets re-render |
| `local` with **PVC (RWO)** | Single-replica production, simple ops | No (RWO can only attach to one pod) | Yes |
| `local` with **PVC (RWX)** | Multi-replica + cluster has RWX storage class (CephFS, NFS subdir, EFS-CSI) | Yes (every replica reads/writes same volume) | Yes |
| `s3` | Multi-replica + cloud-native | Yes (every replica reads from same bucket, lease-holder writes) | Yes |

**Recommendation for cloud installs:** `s3`. AWS S3, GCS via S3-compat API, MinIO, or Ceph RGW all work. Pairs with leader election so writes don't conflict; reads are cluster-wide and immediate.

**Recommendation for on-prem:** `local` + a PVC with whatever access mode your storage class supports. RWX if you want multi-replica.

```yaml
# Minimum production values for s3 backend with IRSA on EKS:
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
      # Leave accessKey/secretKey empty — IAM role attached via SA annotation.

# Or a PVC-backed local store:
operator:
  enabled: true
  storage:
    backend: local
    persistence:
      enabled: true
      storageClassName: gp3              # or your cluster's default RWO class
      size: 20Gi
      accessModes: [ReadWriteOnce]
```

### Closing the pin→fetch race

Downstream Flux consumers (kustomize-controller, helm-controller, stageset-controller) typically read `status.artifact.url` from an `ExternalArtifact` and then dereference the URL in a separate HTTP fetch. If JaaS re-renders the snippet and immediately garbage-collects the superseded revision between those two steps, the consumer's `GET` returns 404. The standard contract requires consumers to re-resolve and retry, but a short producer-side retention window eliminates the race for every consumer at negligible cost.

`--artifact-gc-grace` (default `5m`) is how long a revision that has dropped out of the keep-set stays fetchable. Inside the grace window the tarball is still served at its original URL; once `now - supersession_time` reaches the grace, the next reconcile prunes it. Supersession time is derived from on-disk storage metadata (the earliest mtime newer than the candidate), so the window survives operator restarts without any in-memory bookkeeping.

The default is wide enough to cover steady-state fetch latencies — controller-runtime workqueue depth, sourceref reconcile interval, and downstream HTTP round-trips. Tune lower if storage capacity is tight and your consumers are all in-cluster (sub-second fetches); set `0` to restore eager pruning, which makes the JaaS install indistinguishable from stock Flux source-controller's single-revision semantics. The grace applies only to supersession — when a `JsonnetSnippet` is deleted, the finalizer's `Withdraw` removes everything immediately. Suspended snippets continue to have grace-expired revisions cleaned on every watch tick + `spec.interval` reconcile, so a paused snippet doesn't pin storage forever.

`spec.history` is a separate knob with a different goal: deliberate retention for rollback / blue-green flows in which a downstream pin to an older sha256 must keep working indefinitely. It is **not** the race-protection mechanism; use `--artifact-gc-grace` for that.

## 2. Enable observability

The chart ships two opt-in Prometheus integrations and a comprehensive event surface for Flux's `notification-controller`. Turn them all on:

```yaml
operator:
  metrics:
    enabled: true                         # default; the metrics endpoint
    serviceMonitor:
      enabled: true                       # opt-in; Prometheus Operator picks it up
      labels:
        release: kube-prom                # match your Prometheus's serviceMonitorSelector
    prometheusRule:
      enabled: true                       # opt-in; ships a starter alert set
      labels:
        release: kube-prom                # match your Prometheus's ruleSelector
                                          # (the selector for PrometheusRule CRs —
                                          # distinct from serviceMonitorSelector above)
      # extraAlertLabels merge onto every alert — route all jaas pages
      # to one Alertmanager receiver:
      extraAlertLabels:
        team: platform
      # Override individual thresholds if the defaults are noisy in your cluster:
      thresholds:
        reconcileErrorRate: 0.1           # alerts per second; 0.1 = 1 per 10s
        reconcileLatencySeconds: 30
        workqueueDepth: 50
```

There are three independent label knobs here, easily confused:

- `serviceMonitor.labels` — copied onto the **ServiceMonitor** object; Prometheus selects it via `serviceMonitorSelector`.
- `prometheusRule.labels` — copied onto the **PrometheusRule** object; Prometheus selects it via `ruleSelector`. Set this or your Prometheus simply won't load the alerts — it's the rule-CR counterpart of `serviceMonitor.labels`, not something `extraAlertLabels` covers.
- `prometheusRule.extraAlertLabels` — merged onto every individual **alert** (for Alertmanager routing), not onto the rule object, so they have no effect on rule selection.

The shipped alerts: `JaaSSnippetReconcileErrorsHigh`, `JaaSSnippetArtifactGrowing`, `JaaSControllerWorkqueueDepthHigh`, `JaaSReconcileLatencyHigh`, `JaaSOperatorPodDown`, `JaaSStorageSweepFailures`. Each has a `runbook_url` annotation pointing at [`docs/runbooks/`](runbooks/).

**Notification routing (optional):** Flux's `notification-controller` listens on Kubernetes Events. The reconciler emits a `Warning` event on every non-Synced Ready transition with the `Reason` as the event reason. Pair with an `Alert` CR:

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Alert
metadata:
  name: jaas-failures
  namespace: flux-system
spec:
  providerRef: { name: slack-prod }
  eventSeverity: warn
  eventSources:
    - kind: JsonnetSnippet
      name: '*'
```

## 3. Enable the admission webhook

The webhook rejects spec invariant violations (ext-var conflicts, library-alias shadowing, sourceRef cycles) at `kubectl apply` time and emits warnings for soft pitfalls (missing entryFile, duplicate import paths, likely self-references). Pick one cert mode:

```yaml
# Option A: cert-manager (recommended if cert-manager is already in the cluster)
operator:
  webhook:
    enabled: true
    certMode: cert-manager
    certManager:
      enabled: true
      issuerRef:
        kind: ClusterIssuer
        name: letsencrypt-prod  # or your in-cluster Issuer

# Option B: self-signed (no cert-manager required)
operator:
  webhook:
    enabled: true
    certMode: self-signed
    selfSignedValidity: 8760h  # 1 year; operator rotates every validity/3
```

The chart's render-time preflight fails fast if `webhook.enabled=true` without cert wiring — no silent breakage.

## 4. Tune for scale

The chart's resource defaults are conservative (64Mi memory, 32m CPU) — safe for a hello-world install, undersized for sustained load. Production sizing depends on snippet count + render size:

| Snippets reconciled per minute | Recommended resources |
|---|---|
| < 10 | Defaults are fine |
| 10-100 | `resources.memory: 256Mi`, `resources.cpu: 100m` |
| 100-1000 | `resources.memory: 512Mi`, `resources.cpu: 500m`, consider raising `MaxConcurrentReconciles` via a future flag (default 1 today — bench shows ~1,200 reconciles/sec ceiling per pod even at 1) |
| > 1000 | You're past where the bench data is honest. Open an issue with workqueue depth + reconcile p99 metrics. |

For multi-replica HA (only useful with `backend: s3` OR `backend: local` with RWX persistence):

```yaml
replicas:
  min: 2
  max: 5  # HPA target; only used if max > min
# Leader election is on by default with operator.enabled=true.
# Only the lease-holder writes; reads are cluster-wide.
```

**Set the runaway-snippet cap:**

```yaml
operator:
  storage:
    maxArtifactBytes: 16777216  # 16 MiB; snippets past this fail with
                                # ReasonArtifactTooLarge instead of OOM-killing
                                # the operator pod
```

## 5. Disaster recovery

Three runbooks are worth bookmarking and threading into your on-call docs:

- **[`storage-recovery.md`](runbooks/storage-recovery.md)** — PVC lost, S3 down, downstream 404s, disk-full handling, OOM-mid-render. The one runbook your storage-incident response should already know about.
- **[`rbacdenied.md`](runbooks/rbacdenied.md)** — every Forbidden surface: tenant SA missing a verb, source CR's CRD not installed, ExternalArtifact write rejected. Links to the verb table.
- **[`operator-watch-silent.md`](runbooks/operator-watch-silent.md)** — the one failure mode JaaS can't surface itself (operator's own ClusterRole missing a verb so controller-runtime's informer silently fails). Diagnosis via `kubectl logs | grep 'Failed to watch'`.

**Pod-failure rehearsal:** Restart the operator pod (`kubectl rollout restart deploy/jaas`). With `LeaderElectionReleaseOnCancel: true` (chart default), the lease hands over in <1s. Snippets that were Ready=True stay Ready=True via cached state until the new leader reconciles them. If you see >5s of unreadiness on snippets after a restart, something's wrong — start with the operator-watch-silent runbook.

## 6. Tenant ServiceAccount RBAC

Each `JsonnetSnippet` runs against its `spec.serviceAccountName` (or `--default-service-account` fallback). The operator impersonates that SA for every API call. Tenants need:

- `source.toolkit.fluxcd.io/externalartifacts`: `get`, `create`, `update`, `patch` — **required**.
- `source.toolkit.fluxcd.io/{gitrepositories,ocirepositories,buckets,externalartifacts}`: `get` — only if the snippet uses `spec.sourceRef`.
- `jaas.metio.wtf/jsonnetlibraries`: `get`, `list` — only if the snippet imports libraries via `spec.libraries`.

The minimum `Role` + `RoleBinding` shape is in [`README.md`'s "Tenant ServiceAccount RBAC"](../README.md#tenant-serviceaccount-rbac). Apply per-tenant-namespace.

**`spec.sourceRef` also needs network reach, not just RBAC.** Fetching a Flux source means an HTTP GET against source-controller's artifact server (port `9090`) in `flux-system`. Flux's default `allow-egress` NetworkPolicy admits ingress to its controllers only from inside `flux-system`, so on a cluster whose CNI enforces NetworkPolicies the operator is blocked and the snippet stalls on `SourceFetchFailed` / `context deadline exceeded while awaiting headers` — RBAC is fine, the SYN is dropped. Open the artifact port to the operator's namespace with a kustomize patch on the Flux-managed policy (keeps the rule under Flux's own GitOps); the patch is in the chart's [`README.md` → "Consuming Flux sources"](https://github.com/metio/helm-charts/tree/main/charts/jaas#consuming-flux-sources-specsourceref). Snippets that render only from inline `spec.files` never touch source-controller and need none of this.

**Single shared tenant (not recommended for multi-tenant):** Bind the chart's operator SA to itself in the tenant namespace. Works but every snippet shares the same blast radius.

**Per-tenant SAs (recommended):** One `ServiceAccount` per tenant team, bound to a Role with only the verbs that tenant's snippets need. Compromised snippet → bounded to that team's namespace.

## 7. Upgrade hygiene

Calendar-based releases run weekly (Mondays). The chart honors `helm upgrade --install`; it ships the CRDs under `templates/` so `helm upgrade` applies schema changes automatically.

**Two upgrade gotchas:**

1. **`spec.selector.matchLabels` is immutable.** If you upgrade across a release that changed labels, `helm upgrade` fails. [`MIGRATIONS.md`](../MIGRATIONS.md) calls out which releases changed labels (and the `kubectl delete deploy/jaas` workaround).

2. **`helm uninstall` runs a pre-delete Job** (cleanup-on-delete, default `true`) that drops every JsonnetSnippet's finalizer so ExternalArtifacts get unwound before the operator pod itself is removed. If the cleanup Job hangs (slow S3, RBAC issues), check `operator.cleanupOnDelete.kubectlTimeout` (default 2m). If you need to force a teardown without the cleanup, set `operator.cleanupOnDelete.enabled: false` and clean up snippets manually after.

**Test upgrades in non-prod first.** The kind-smoke CI exercises the upgrade path on every PR, but cluster-specific config (storage class, network policy, IAM bindings) is yours to validate.

## A complete production values.yaml

```yaml
# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD
#
# Production-shape values for a multi-replica S3-backed install with
# observability + admission webhook on. Adjust the marked TODOs.

replicas:
  min: 2
  max: 5

resources:
  memory: 256Mi
  cpu: 100m

image:
  pullPolicy: IfNotPresent  # default; use Always only for mutable tags

namespace:
  create: true              # render the namespace with PSS labels
  podSecurity:
    enforce: restricted     # requires k8s 1.33+; drop to baseline for older

operator:
  enabled: true
  defaultServiceAccount: ""  # force per-snippet spec.serviceAccountName

  serviceAccount:
    create: true
    name: jaas
    annotations:
      # TODO: IRSA / Workload Identity / Azure WI annotation for cloud auth
      eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT:role/jaas-operator

  storage:
    backend: s3
    s3:
      endpoint: s3.amazonaws.com   # TODO
      bucket: my-jaas-artifacts    # TODO
      prefix: prod
      region: eu-west-1            # TODO
    maxArtifactBytes: 16777216     # 16 MiB cap

  metrics:
    enabled: true
    serviceMonitor:
      enabled: true
      labels:
        release: kube-prom         # TODO: match your Prometheus selector
    prometheusRule:
      enabled: true
      labels:
        release: kube-prom         # TODO: match your Prometheus ruleSelector
      extraAlertLabels:
        team: platform             # TODO: your Alertmanager routing label

  webhook:
    enabled: true
    certMode: cert-manager
    certManager:
      issuerRef:
        kind: ClusterIssuer
        name: letsencrypt-prod     # TODO: your cluster's Issuer

  leaderElection:
    enabled: true                  # default with operator.enabled

  cleanupOnDelete:
    enabled: true                  # default; pre-delete Job unwinds finalizers
```

Apply with:

```shell
helm install jaas oci://ghcr.io/metio/helm-charts/jaas \
  --namespace jaas-system --create-namespace \
  -f production-values.yaml \
  --wait --timeout 5m
```

## Where to go next

- **Tenant RBAC details:** [`README.md`'s "Tenant ServiceAccount RBAC"](../README.md#tenant-serviceaccount-rbac).
- **Architecture deep-dive:** [`README.md`'s "Operator Mode"](../README.md#operator-mode) and [`CLAUDE.md`](../CLAUDE.md) (the project's design map).
- **Migration notes per release:** [`MIGRATIONS.md`](../MIGRATIONS.md).
- **All runbooks:** [`docs/runbooks/`](runbooks/).
- **The full chart values reference:** [`charts/jaas/values.yaml`](https://github.com/metio/helm-charts/blob/main/charts/jaas/values.yaml) in the metio/helm-charts repo.
- **End-to-end Grafana-dashboards-via-JaaS example:** [`examples/full-stack/`](../examples/full-stack/).
