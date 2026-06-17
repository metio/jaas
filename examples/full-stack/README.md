<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Full-stack demo: Flux → JaaS → Grafana

A worked example that takes a snippet living in a Git repository all the way to a rendered Grafana dashboard, going through every layer of the GitOps stack.

## Architecture

```text
┌──────────────┐    ┌───────────────────┐    ┌────────────────┐    ┌────────────────┐
│ Git repo     │───▶│ Flux              │───▶│ JaaS operator  │───▶│ grafana-       │
│ (Jsonnet     │    │ source-controller │    │ (reconciles    │    │ operator       │
│  snippet)    │    │ (publishes a      │    │  JsonnetSnippet│    │ (mounts        │
│              │    │  GitRepository)   │    │  → publishes   │    │  ExternalArt.  │
│              │    │                   │    │  ExternalArt.) │    │  into Grafana  │
│              │    │                   │    │                │    │  Dashboard)    │
└──────────────┘    └───────────────────┘    └────────────────┘    └────────────────┘
```

Every arrow is a watch — flip any source upstream and the change propagates within seconds.

## Prerequisites

The demo assumes you have the following installed in the cluster:

- **Flux v2.6+** with `source-controller` running in `flux-system`.
- **JaaS** installed via the operator-enabled helm chart:

  ```shell
  helm install jaas oci://ghcr.io/metio/helm-charts/jaas \
    -n jaas-system --create-namespace \
    --set operator.enabled=true \
    --set operator.webhook.enabled=true \
    --set operator.webhook.certMode=self-signed
  ```

- **grafana-operator v5+** with at least one `Grafana` CR running.

## Before you apply: replace the placeholder URL

`02-gitrepository.yaml` points at `https://github.com/REPLACE-ME/grafana-dashboards`. Substitute your own dashboards repo. See the next section for the layout JaaS expects to find inside it.

## Shape your dashboards repo needs to match

The example is wired for a repo organised like this:

```text
<repo root>/
└── dashboards/
    ├── api-latency.jsonnet   ← matches spec.entryFile in 03-jsonnetsnippet.yaml
    ├── error-rate.jsonnet
    └── ...
```

Minimum requirements:

- A `dashboards/` subdirectory (the `ignore:` rule in `02-gitrepository.yaml` strips everything else from the published artifact — narrows what JaaS has to download and evaluate).
- A `.jsonnet` file at the path the JsonnetSnippet's `spec.entryFile` names. For the bundled `03-jsonnetsnippet.yaml`, that's `dashboards/api-latency.jsonnet`.
- The entry file must evaluate to a Grafana-dashboard-compatible JSON object — typically by importing a helper (grafonnet / docsonnet) that produces one.

A minimal placeholder snippet that proves the wiring:

```jsonnet
// dashboards/api-latency.jsonnet
{
  title: 'API latency',
  schemaVersion: 38,
  uid: 'api-latency',
  tags: [std.extVar('severity')],
  panels: [],
}
```

For a real installation, swap that for your actual dashboard sources (typically grafonnet-based).

## Applying the manifests

```shell
kubectl apply -f 01-tenant-rbac.yaml
kubectl apply -f 02-gitrepository.yaml    # ← edit url: first
kubectl apply -f 03-jsonnetsnippet.yaml
kubectl apply -f 04-grafanadashboard.yaml
```

## What each manifest does

### 01-tenant-rbac.yaml

A `ServiceAccount` named `dashboards-tenant` plus a `Role` granting `get` on `GitRepository`. The JsonnetSnippet's `spec.serviceAccountName` points at this SA, so the JaaS operator impersonates it for every Kubernetes call done on behalf of the snippet. Without this, the operator can't read the GitRepository and the reconcile fails with `Forbidden`.

### 02-gitrepository.yaml

A Flux `GitRepository` pointing at an arbitrary public repo. Replace the `url` with your dashboards repo. `interval: 1m` makes Flux poll every minute; in steady state JaaS gets watch events the moment the GitRepository's `status.artifact` advances.

### 03-jsonnetsnippet.yaml

The actual JaaS workload:

- `spec.sourceRef` points at the GitRepository — JaaS fetches the artifact, untars it, picks up `dashboards/api-latency.jsonnet` per `spec.entryFile`, evaluates it, and writes the resulting JSON into an `ExternalArtifact` named after the snippet.
- `spec.history: 5` keeps the last 5 published revisions in storage so a downstream consumer can pin to a previous sha256 (useful for rollback).
- `spec.interval: 5m` re-renders every five minutes even if no watch event fires (catches drift in env-vars or OCI libraries).

### 04-grafanadashboard.yaml

A `GrafanaDashboard` from grafana-operator that pulls its body from the `ExternalArtifact` JaaS publishes. grafana-operator watches the artifact URL and pushes the rendered JSON into Grafana's API.

## Wiring sanity check

After all four are applied:

```shell
# JsonnetSnippet should reach Ready=True/Synced within a few seconds:
kubectl get jsonnetsnippet api-latency -n monitoring -w

# The ExternalArtifact should carry a sha256 digest:
kubectl get externalartifact api-latency -n monitoring -o yaml

# The GrafanaDashboard should report 'OK':
kubectl get grafanadashboard api-latency -n monitoring -o yaml
```

Edit anything in the snippet's git source — within `interval`, the dashboard updates in Grafana with no further action.

## Failure surfaces, with where to look

| Symptom | Where to look |
|---|---|
| `ReasonSourceFetchFailed` | The GitRepository's `.status.conditions` first; then jaas-operator logs |
| `ReasonLibraryNotFound` | `kubectl get jsonnetlibrary -A` plus the tenant SA's RBAC |
| Grafana dashboard not updating | grafana-operator logs; verify the `ExternalArtifact` URL is reachable from the grafana-operator pod |
| `Ready=False/Suspended` | A human (or controller) set `spec.suspend: true` — `kubectl describe` for context |

For each failure mode there's a runbook under [`docs/runbooks/`](../../docs/runbooks/). JaaS auto-surfaces the matching runbook URL on the Ready condition message — a `(runbook: https://jaas.projects.metio.wtf/runbooks/<reason>/)` suffix visible in `kubectl describe`.

## Notifications (optional)

Pair JaaS's standard K8s Events with Flux's `notification-controller` to get Slack / Webhook / PagerDuty alerts when a snippet flips Ready=False:

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Alert
metadata:
  name: jaas-failures
  namespace: flux-system
spec:
  providerRef:
    name: slack-prod
  eventSeverity: warn
  eventSources:
    - kind: JsonnetSnippet
      name: '*'
```

JaaS emits `Warning` events on every transition to a non-`Synced` Reason; notification-controller does the rest.

## Metrics & alerting

The operator exposes custom Prometheus metrics on the chart's `jaas-metrics` Service (set `operator.metrics.serviceMonitor.enabled: true` so the kube-prometheus stack picks them up):

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `jaas_snippet_reconcile_total` | counter | `namespace, name, status, reason` | Bumps once per reconcile that flips/refreshes the Ready condition |
| `jaas_snippet_rendered_bytes` | histogram | `namespace, name` | Observes the rendered artifact size on Synced reconciles |

Drop in [`05-prometheusrule.yaml`](05-prometheusrule.yaml) for a starter set of alerts (reconcile error rate, artifact size growth, workqueue saturation, reconcile latency).
