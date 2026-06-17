<!--
SPDX-FileCopyrightText: The jaas Authors
SPDX-License-Identifier: 0BSD
-->

# Operator runbooks

One page per wire-stable `Reason` constant the JaaS operator sets on the `Ready` condition of a `JsonnetSnippet`. Pages target operators reading `kubectl describe` output — they cover symptom, common cause, diagnosis, and remediation.

| Reason | Page |
|---|---|
| `Pending` | [pending.md](pending.md) |
| `Synced` (healthy) | [synced.md](synced.md) |
| `InvalidSpec` | [invalidspec.md](invalidspec.md) |
| `LibraryNotFound` | [librarynotfound.md](librarynotfound.md) |
| `CrossNamespaceRefRejected` | [crossnamespacerefrejected.md](crossnamespacerefrejected.md) |
| `ExternalVariableConflict` | [externalvariableconflict.md](externalvariableconflict.md) |
| `ServiceAccountMissing` | [serviceaccountmissing.md](serviceaccountmissing.md) |
| `EvaluationFailed` | [evaluationfailed.md](evaluationfailed.md) |
| `EvaluationTimeout` | [evaluationtimeout.md](evaluationtimeout.md) |
| `SourceRefNotYetSupported` | [sourcerefnotyetsupported.md](sourcerefnotyetsupported.md) |
| `SourceNotReady` | [sourcenotready.md](sourcenotready.md) |
| `SourceFetchFailed` | [sourcefetchfailed.md](sourcefetchfailed.md) |
| `DependencyCycle` | [dependencycycle.md](dependencycycle.md) |
| `ArtifactTooLarge` | [artifacttoolarge.md](artifacttoolarge.md) |
| `Suspended` (intentional pause) | [suspended.md](suspended.md) |
| `RBACDenied` | [rbacdenied.md](rbacdenied.md) |

## Cross-cutting runbooks

These cover failures that don't map to a single `Reason` constant — typically infrastructure-level incidents the snippet itself can't surface. The chart's opt-in PrometheusRule (`operator.metrics.prometheusRule.enabled`) links each alert to one of these pages via the `runbook_url` annotation.

| Topic | Page | Linked from alert |
|---|---|---|
| Storage backend recovery (PVC lost, S3 down, downstream 404s) | [storage-recovery.md](storage-recovery.md) | (no direct alert; pointed to from other runbooks) |
| Workqueue saturation | [workqueue-saturation.md](workqueue-saturation.md) | `JaaSControllerWorkqueueDepthHigh` |
| High reconcile latency | [reconcile-latency.md](reconcile-latency.md) | `JaaSReconcileLatencyHigh` |
| Operator pod not ready | [operator-pod-down.md](operator-pod-down.md) | `JaaSOperatorPodDown` |
| Watch-layer silent failure (operator-self RBAC) | [operator-watch-silent.md](operator-watch-silent.md) | (no direct alert; diagnose via deploy logs) |
| Eval-concurrency saturation (cap full, requests shed) | [eval-saturation.md](eval-saturation.md) | `JaaSEvalSaturation` / `JaaSEvalRejected` / `JaaSEvalLeakedGoroutines` |
| Self-signed webhook cert renewal failing | [webhook-cert-renewal.md](webhook-cert-renewal.md) | `JaaSWebhookCertRenewalFailing` |
| Flux source CRD watch can't engage | [crd-watch-engagement.md](crd-watch-engagement.md) | `JaaSCRDWatchEngagementFailing` |

`JaaSSnippetReconcileErrorsHigh`'s runbook URL is templated on `$labels.reason` and resolves to the matching per-`Reason` page above. `JaaSSnippetArtifactGrowing` links to [artifacttoolarge.md](artifacttoolarge.md).

## Runbook links in Ready condition messages

Every Ready condition Message the operator sets carries a `(runbook: https://jaas.projects.metio.wtf/runbooks/<reason>/)` suffix — the reason is lower-cased and appended as a path segment. The suffix shows up in `kubectl describe`, so operators jump straight from the diagnosis output to the matching page. Healthy and intentional states (`Synced`, `Suspended`, `Pending`) get no suffix.
