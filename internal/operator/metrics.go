/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/metio/jaas/internal/eval"
)

// snippetReconcileTotal counts how many reconciles ended in each
// (status, reason) bucket. Lets dashboards graph the rate of Synced
// reconciles vs each failure mode and alert on a flip of the ratio.
//
// Labels:
//   - namespace, name: identify the snippet
//   - status: "True" | "False" matching metav1.ConditionStatus
//   - reason: the Reason* constant from conditions.go
var snippetReconcileTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "jaas_snippet_reconcile_total",
		Help: "Number of reconciles that ended in each (status, reason) bucket on a JsonnetSnippet.",
	},
	[]string{"namespace", "name", "status", "reason"},
)

// snippetRateLimitedTotal counts reconciles deferred by the per-snippet
// token bucket. Lets dashboards graph backpressure and alert on a
// runaway snippet exhausting its bucket continuously (a sign the
// snippet's update cadence outpaces --reconcile-rate-limit, or that
// a sibling controller is repeatedly bumping the spec).
//
// Paired with the RateLimited Warning event so kubectl describe also
// surfaces the deferral — a Debug log line alone would leave operators
// guessing why a snippet hasn't reconciled.
var snippetRateLimitedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "jaas_snippet_rate_limited_total",
		Help: "Reconciles deferred by the per-snippet rate limiter. Tracks backpressure independent of failure metrics.",
	},
	[]string{"namespace", "name"},
)

// snippetEvalUnavailableTotal counts reconciles deferred because the
// global concurrent-eval cap (--max-concurrent-evals) was full when
// the reconciler attempted to evaluate the snippet. Distinct from
// snippetRateLimitedTotal: rate limiting is per-snippet token bucket,
// this is global resource exhaustion that affects every tenant.
// Paired with the EvalUnavailable Warning event so kubectl describe
// also surfaces the deferral.
var snippetEvalUnavailableTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "jaas_snippet_eval_unavailable_total",
		Help: "Reconciles deferred because the global eval-concurrency cap was full. Sustained non-zero values flag a saturating snippet workload or a stuck eval.",
	},
	[]string{"namespace", "name"},
)

// evalInFlightGauge mirrors the live count of evaluations actively
// holding a semaphore slot. Pairs with jaas_eval_unavailable_total
// (the rejected counter) — saturation manifests as in_flight
// pegged at the cap while unavailable_total grows.
var evalInFlightGauge = prometheus.NewGaugeFunc(
	prometheus.GaugeOpts{
		Name: "jaas_eval_in_flight",
		Help: "Evaluations currently holding a slot in the global concurrent-eval semaphore.",
	},
	func() float64 { return float64(eval.InFlightEvals()) },
)

// evalMaxConcurrentGauge exposes the configured cap so dashboards can
// plot in_flight against the ceiling. Zero means the gate is disabled
// (--max-concurrent-evals=0); any saturation alert MUST guard on this
// being non-zero.
var evalMaxConcurrentGauge = prometheus.NewGaugeFunc(
	prometheus.GaugeOpts{
		Name: "jaas_eval_max_concurrent",
		Help: "Configured ceiling of the global concurrent-eval semaphore. 0 means the gate is disabled.",
	},
	func() float64 { return float64(eval.MaxConcurrentEvals()) },
)

// evalUnavailableTotalCounter is the process-global accumulator the
// eval package maintains; the labeled snippetEvalUnavailableTotal
// covers the operator path while this gauge exposes the un-labeled
// total (HTTP rejections included).
var evalUnavailableTotal = prometheus.NewCounterFunc(
	prometheus.CounterOpts{
		Name: "jaas_eval_unavailable_total",
		Help: "Total evaluations rejected by the concurrent-eval semaphore (HTTP + operator). Monotonic; resets on process restart.",
	},
	func() float64 { return float64(eval.RejectedEvalCount()) },
)

// snippetRenderedBytes records the size of the rendered artifact a
// Synced reconcile produced. Histogram bucketing follows the standard
// "small to multi-MB" range — the default buckets cap at 64 MiB which
// pairs with the operator's --max-artifact-bytes recommendation.
var snippetRenderedBytes = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "jaas_snippet_rendered_bytes",
		Help:    "Distribution of the rendered artifact byte size on every Synced reconcile.",
		Buckets: []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864},
	},
	[]string{"namespace", "name"},
)

// evalLeakedGauge exposes the live count of evaluation goroutines whose
// parent reconcile timed out before the underlying vm.EvaluateFile
// completed. go-jsonnet has no context-aware entry point, so a runaway
// snippet keeps a goroutine alive past the reconcile's timeout —
// invisible to the synchronous API. The gauge surfaces that state to
// operators so a `jaas_eval_outstanding_timed_out > 0 for 5m` alert
// catches a snippet whose evaluation cost dwarfs --evaluation-timeout.
//
// Backed by an atomic counter inside the eval package — registered
// here as a CollectorFunc so the gauge value reads through every
// scrape rather than being copied at write time.
var evalLeakedGauge = prometheus.NewGaugeFunc(
	prometheus.GaugeOpts{
		Name: "jaas_eval_outstanding_timed_out",
		Help: "Evaluation goroutines whose parent timed out before the synchronous go-jsonnet call returned.",
	},
	func() float64 { return float64(eval.OutstandingTimedOutEvals()) },
)

// storageSweepFailuresTotal counts how many storage-Sweep passes
// returned an error. The sweep is the background GC that removes
// orphaned .tar.gz.tmp residue left by Puts whose process died after
// the tmpfile landed but before the rename. Sustained failures here
// (filesystem permissions revoked, disk full, S3 listing throttled)
// don't break the hot reconcile path — Put still works — but the
// store accumulates stale .tmp files until the underlying issue is
// fixed. The counter has no labels because the sweep is pod-wide
// background work, not tied to a specific CR.
//
// Chosen over a Kubernetes Event because the sweep has no clean
// involvedObject (it walks the whole tree, not one CR), and the
// Prometheus surface fits the existing observability story —
// `JaaSStorageSweepFailures` is one more alert in the opt-in
// PrometheusRule template.
var storageSweepFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "jaas_storage_sweep_failures_total",
		Help: "Storage-Sweep passes that returned an error. Sustained non-zero values flag a degraded artifact store (permissions, disk full, S3 throttling) that doesn't block the reconcile hot path but leaves .tar.gz.tmp residue accumulating.",
	},
)

// RecordSweepFailure increments the sweep-failure counter. Called
// from main.go's runStorageSweep on every Sweep error. Exported so
// the main package can wire it without importing prometheus directly.
func RecordSweepFailure() {
	storageSweepFailuresTotal.Inc()
}

// Per-goroutine failure counters for the background workers whose
// failures would otherwise only emit slog.Warn lines. Three failure
// modes silently degrade without these:
//   - cert-renewal: Renewer.renewOnce → cert expires → admission breaks
//   - tenant-token mint: TokenCache.Mint → snippets pin Ready=Unknown
//   - CRD watch engagement: crdWatcher.EngageFluxWatch → snippets
//     don't re-render on upstream Flux source events
// Each counter is named after the failing pipeline and paired with a
// PrometheusRule alert. Runbooks live under docs/runbooks/.

// webhookCertRenewalFailuresTotal counts Renewer.renewOnce attempts
// that returned an error. The metric is process-wide (the renewer
// rotates one cert per pod). Sustained non-zero values flag RBAC
// drift (resourceNames in the operator-cluster ClusterRole no longer
// covers the live VWC name), CertDir write-perm loss, or apiserver
// patch-verb revocation. Once the existing cert's natural expiry
// elapses, admission breaks cluster-wide — the alert needs to fire
// well before then.
var webhookCertRenewalFailuresTotal = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "jaas_webhook_cert_renewal_failures_total",
		Help: "Self-signed webhook cert renewal attempts (Renewer.renewOnce) that returned an error. Sustained non-zero values flag RBAC drift or CertDir write-perm loss; the existing cert's natural expiry is the deadline.",
	},
)

// RecordWebhookCertRenewalFailure increments the cert-renewal failure
// counter. Exported so internal/webhook/selfsigned can wire it without
// pulling Prometheus into the package's import graph.
func RecordWebhookCertRenewalFailure() {
	webhookCertRenewalFailuresTotal.Inc()
}

// tenantTokenMintFailuresTotal counts TokenCache.Mint calls that
// returned an error. Labels: namespace (the snippet's namespace, which
// is also the SA's namespace) and serviceAccount (the SA being
// impersonated). Bounded cardinality in practice — one entry per
// (namespace, SA) pair the operator has tried to impersonate, mirroring
// the existing tokenCache state.
var tenantTokenMintFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "jaas_tenant_token_mint_failures_total",
		Help: "TokenRequest mints that returned an error. Sustained non-zero values on a (namespace, serviceAccount) pair indicate revoked `serviceaccounts/token: create` on that SA or its namespace was deleted out from under live snippets.",
	},
	[]string{"namespace", "serviceAccount"},
)

// crdWatchEngagementFailuresTotal counts crdWatcher's EngageFluxWatch
// failures. Labels: gvk (the Flux source kind the watch was for). On
// a stable Established CRD, the apiextensions informer fires no further
// events; a one-time engagement failure permanently un-engages the GVK
// until the operator restarts. The counter pairs with a bounded retry
// mechanism, but the metric stands alone as the alert hook.
var crdWatchEngagementFailuresTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "jaas_crd_watch_engagement_failures_total",
		Help: "EngageFluxWatch calls that returned an error. Sustained non-zero values on a GVK flag a stuck watch pipeline; dependent snippets won't re-render on upstream source events until the watch engages.",
	},
	[]string{"gvk"},
)

// snippetForceDropTotal counts how many snippets had their finalizer
// force-dropped because Publisher.Withdraw kept failing past
// MaxWithdrawWait OR the failure was classified permanent via
// isPermanentAPIError. A force-drop leaves an orphaned tarball that
// an operator must clean by hand; sustained values flag a
// permanently-broken backend (revoked RBAC, deleted S3 bucket,
// CRD removed) that needs intervention.
//
// Labels: namespace, name (the snippet identity at force-drop time),
// reason (free-form short string naming what triggered the drop:
// "withdraw_timed_out", "tenant_client_permanent",
// "withdraw_permanent"). Bounded cardinality in practice — one
// counter entry per (snippet, force-drop trigger) pair.
var snippetForceDropTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "jaas_snippet_force_drop_total",
		Help: "Snippets whose finalizer was force-dropped (MaxWithdrawWait exceeded or permanent apiserver error). Sustained non-zero values flag a permanently-broken storage / RBAC pipeline that orphan-tarballs are accumulating against. See docs/runbooks/storage-recovery.md.",
	},
	[]string{"namespace", "name", "reason"},
)

// recordForceDrop increments the force-drop counter. Called from
// forceDropFinalizer; the reason argument is a short stable string
// naming which decision branch triggered the drop (timeout vs.
// permanent classification on tenantClient vs. on Withdraw).
func recordForceDrop(namespace, name, reason string) {
	snippetForceDropTotal.WithLabelValues(namespace, name, reason).Inc()
}

func init() {
	metrics.Registry.MustRegister(
		snippetReconcileTotal,
		snippetRateLimitedTotal,
		snippetEvalUnavailableTotal,
		snippetRenderedBytes,
		evalLeakedGauge,
		evalInFlightGauge,
		evalMaxConcurrentGauge,
		evalUnavailableTotal,
		storageSweepFailuresTotal,
		webhookCertRenewalFailuresTotal,
		tenantTokenMintFailuresTotal,
		crdWatchEngagementFailuresTotal,
		snippetForceDropTotal,
	)
}

// recordRateLimited increments the rate-limit counter for a snippet.
// Called when Limiter.Reserve denies a token; pairs with the
// RateLimited event emitted at the same site.
func recordRateLimited(namespace, name string) {
	snippetRateLimitedTotal.WithLabelValues(namespace, name).Inc()
}

// recordEvalUnavailable increments the eval-unavailable counter for a
// snippet. Called when eval.EvaluateAnonymousSnippet returns
// ErrEvalUnavailable; pairs with the EvalUnavailable Warning event
// emitted at the same site.
func recordEvalUnavailable(namespace, name string) {
	snippetEvalUnavailableTotal.WithLabelValues(namespace, name).Inc()
}

// recordReconcileOutcome bumps snippetReconcileTotal for the given
// snippet's terminal state. status is the Ready condition's Status,
// reason is the matching Reason. namespace/name come from the snippet
// itself.
func recordReconcileOutcome(namespace, name, status, reason string) {
	snippetReconcileTotal.WithLabelValues(namespace, name, status, reason).Inc()
}

// recordRenderedBytes observes the post-eval byte count on a Synced
// reconcile. Skipped on failures because the histogram would skew
// toward zero.
func recordRenderedBytes(namespace, name string, bytes int) {
	snippetRenderedBytes.WithLabelValues(namespace, name).Observe(float64(bytes))
}
