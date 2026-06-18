---
title: Alerting
description: The opt-in PrometheusRule alert catalog with tunable thresholds and runbook links, plus Kubernetes Events routed through Flux's notification-controller.
tags: [operator, alerts, prometheus, observability]
---

JaaS turns a sustained problem into a notification two ways: a Prometheus
`PrometheusRule` that pages on its [metrics](/observability/metrics/), and Kubernetes
Events that Flux's notification-controller can route to chat or e-mail.

## The binary

The operator emits a standard Kubernetes `Event` on every Ready-condition
transition — `Normal` for `Synced`, `Warning` for every other reason. The reason
string fills both the event `reason` and `action`. These Events need no flag to
enable; they are written whenever the operator reconciles.

The operator also threads runbook links into its own status automatically: every
actionable Ready-condition Message gains a
`(runbook: https://jaas.projects.metio.wtf/runbooks/<reason>/)` suffix, so
`kubectl describe jsonnetsnippet` points straight at the matching page. Healthy
or intentional states (`Synced`, `Suspended`, `Pending`) get no suffix.

### Routing Events through Flux

Routing the Events is Flux's `notification-controller`: target an `Alert` CR at
`kind: JsonnetSnippet` and JaaS needs no `Provider`/`Alert` plumbing of its own.

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Alert
metadata:
  name: jaas-snippets
  namespace: flux-system
spec:
  providerRef:
    name: slack
  eventSeverity: warn   # 'info' to include success events
  eventSources:
    - kind: JsonnetSnippet
      name: '*'
```

Wire whatever `Provider` you already use for Flux source CRs; see the
[Flux notification-controller documentation](https://fluxcd.io/) for provider
configuration.

### The alert catalog

The chart ships a starter alert set on the custom metrics plus a handful of
controller-runtime signals. Each alert carries its remediation page as a
`runbook_url` annotation so Alertmanager renders a direct link:

| Alert | Severity | Fires when | Threshold knobs (default) | Runbook |
|---|---|---|---|---|
| `JaaSSnippetReconcileErrorsHigh` | warning | A snippet keeps flipping to Ready=False (excluding `Synced`/`Suspended`/`Pending`). | `reconcileErrorRate` (0.1/s), `reconcileErrorDuration` (10m) | per-reason page under [`/runbooks/`](/runbooks/) |
| `JaaSSnippetArtifactGrowing` | warning | p99 `jaas_snippet_rendered_bytes` exceeds the size ceiling. | `artifactSizeBytes` (16 MiB), `artifactSizeDuration` (30m) | [artifacttoolarge](/runbooks/artifacttoolarge/) |
| `JaaSControllerWorkqueueDepthHigh` | warning | The `jsonnetsnippet` workqueue can't drain. | `workqueueDepth` (50), `workqueueDuration` (15m) | [workqueue-saturation](/runbooks/workqueue-saturation/) |
| `JaaSReconcileLatencyHigh` | warning | p99 reconcile time crosses the ceiling. | `reconcileLatencySeconds` (30), `reconcileLatencyDuration` (15m) | [reconcile-latency](/runbooks/reconcile-latency/) |
| `JaaSOperatorPodDown` | critical | A jaas pod stays NotReady. | `podDownDuration` (5m) | [operator-pod-down](/runbooks/operator-pod-down/) |
| `JaaSStorageSweepFailures` | warning | Background sweeps fail per hour above the floor. | `sweepFailuresPerHour` (3), `sweepFailuresDuration` (30m) | [storage-recovery](/runbooks/storage-recovery/) |
| `JaaSWebhookCertRenewalFailing` | critical | Self-signed cert renewal fails per hour above the floor. | `webhookCertRenewalFailuresPerHour` (1), `webhookCertRenewalFailuresDuration` (30m) | [webhook-cert-renewal](/runbooks/webhook-cert-renewal/) |
| `JaaSTenantTokenMintFailing` | warning | Token mints fail for a `(namespace, serviceAccount)` pair. | `tenantTokenMintFailureRate` (0.01/s), `tenantTokenMintFailureDuration` (10m) | [rbacdenied](/runbooks/rbacdenied/) |
| `JaaSForceDropsAccumulating` | warning | Snippet finalizers are force-dropped per hour above the floor. | `forceDropsPerHour` (0), `forceDropsDuration` (5m) | [storage-recovery](/runbooks/storage-recovery/) |
| `JaaSCRDWatchEngagementFailing` | warning | A Flux source watch won't engage for a GVK. | `crdWatchEngagementFailuresPerHour` (1), `crdWatchEngagementFailuresDuration` (30m) | [crd-watch-engagement](/runbooks/crd-watch-engagement/) |
| `JaaSEvalSaturation` | warning | In-flight evals exceed the saturation ratio of the cap (guarded on the cap being non-zero). | `evalSaturationRatio` (0.9), `evalSaturationDuration` (10m) | [eval-saturation](/runbooks/eval-saturation/) |
| `JaaSEvalRejected` | warning | The semaphore turns evals away per second above the floor. | `evalRejectedRate` (0.05/s), `evalRejectedDuration` (10m) | [eval-saturation](/runbooks/eval-saturation/) |
| `JaaSEvalLeakedGoroutines` | warning | Orphan eval goroutines persist above the floor — a runaway snippet. | `evalLeakedFloor` (0), `evalLeakedDuration` (5m) | [eval-saturation](/runbooks/eval-saturation/) |

`JaaSSnippetReconcileErrorsHigh` templates its runbook URL on the failing reason,
so it lands on the matching per-reason page under [`/runbooks/`](/runbooks/). Each
Ready-condition reason and each alert maps to a remediation page there.

## The Helm chart

The `PrometheusRule` is opt-in under `operator.metrics.prometheusRule` and needs
the Prometheus Operator's `monitoring.coreos.com/v1` API in the cluster:

```yaml
operator:
  enabled: true
  metrics:
    enabled: true
    prometheusRule:
      enabled: true
      interval: 30s
      # Labels your Prometheus instance selects PrometheusRules on.
      labels:
        release: kube-prometheus
      # Merged onto every rendered alert — route all jaas alerts
      # through one Alertmanager receiver.
      extraAlertLabels:
        team: platform
      # Annotation key the runbook URL lands under (Prometheus-operator
      # convention is runbook_url).
      runbookAnnotationKey: runbook_url
```

Every threshold is a knob under `operator.metrics.prometheusRule.thresholds`, so
the noise floor is tunable without copy-pasting rule bodies. To silence a
built-in alert, raise its threshold to an impossibly high value — there is no
per-alert disable toggle, and the threshold pattern keeps "this alert is
intentionally inert" visible in the chart values. Cluster-specific rules append
under a separate group via `operator.metrics.prometheusRule.extraRules`.
