/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"

	ctrl "sigs.k8s.io/controller-runtime"
	crbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	fluxconditions "github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/jitter"
	fluxpatch "github.com/fluxcd/pkg/runtime/patch"
	fluxpredicates "github.com/fluxcd/pkg/runtime/predicates"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/eval"
	"github.com/metio/jaas/internal/observability"
	"github.com/metio/jaas/internal/sources"
	"github.com/metio/jaas/internal/urlguard"
)

// defaultMaxWithdrawWait is how long a deleted snippet's finalizer can
// hold while Publisher.Withdraw keeps failing before reconcileDelete
// force-drops it. One hour is generous enough to ride out transient
// apiserver/S3 incidents but short enough that a permanently-broken
// backend doesn't block namespace teardown indefinitely.
const defaultMaxWithdrawWait = 1 * time.Hour

// EntryFileName is the snippet entry point inside spec.files. Snippets that
// omit it are rejected at reconcile time — the convention matches the HTTP
// path, where the resolver looks for main.jsonnet under each
// -snippet-directory.
const EntryFileName = "main.jsonnet"

// evalUnavailableRequeueAfter bounds how soon a snippet rejected by the
// concurrent-eval cap re-enters the workqueue. The eval gate clears as
// in-flight snippets finish (at most after EvaluationTimeout, default 5s),
// so a 1s requeue keeps backpressure responsive without spinning the
// queue against a still-full cap.
const evalUnavailableRequeueAfter = 1 * time.Second

// permanentRetryInterval bounds how soon a snippet sitting on a terminal
// Ready=False reason re-enters the workqueue. Terminal failures (RBAC
// denied, missing CRD, invalid spec) don't engage controller-runtime's
// error backoff — failReady returns no error so the queue doesn't spin.
// But several of them heal out-of-band without producing a watch event the
// snippet sees: granting the tenant SA an RBAC verb, installing a missing
// CRD, or fixing a referenced library in another namespace. A bounded
// RequeueAfter gives those a self-healing re-check roughly once a minute
// without hot-looping — the gap between "operator grants RBAC" and "snippet
// recovers" stays at worst this interval.
const permanentRetryInterval = 1 * time.Minute

// defaultIntervalJitterFraction is the +/- fraction of an interval-based
// RequeueAfter that the jitter package spreads the wakeup across. 0.05 is the
// Flux default (5%): enough to break up a same-interval thundering herd
// without meaningfully shifting any single snippet's re-render cadence.
const defaultIntervalJitterFraction = 0.05

// maxCycleVerdictRetries caps cycleVerdict's walk-Store retry loop when a
// concurrent watch-driven Forget keeps invalidating the in-flight verdict.
// In steady-state operation Forget-during-walk is rare; under a tight
// burst of library/source changes the loop may retry once or twice before
// settling. Three attempts is generous; on the rare miss we fall through
// to one uncached walk so a reconcile under pathological load still makes
// forward progress.
const maxCycleVerdictRetries = 3

// SnippetReconciler is the controller-runtime reconciler for JsonnetSnippet.
//
// Source resolution + eval + publish all flow through this reconciler. The
// Client field is the manager's broad-permission client and is used only for
// operations on the snippet itself (Get/Update/Status). Every tenant-side
// API call — library Gets, ExternalArtifact CRUD — runs through a fresh
// impersonating client built from RestConfig + spec.serviceAccountName so
// the operator can't read or write resources the tenant SA shouldn't reach.
type SnippetReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme

	// APIReader bypasses the manager's cache for the pre-publish
	// staleness gate. The cache can lag behind the apiserver under
	// load, so a fresh Get just before the downstream-visible commit
	// catches a spec edit that landed during this reconcile's fetch +
	// eval phase. nil falls back to Client — fine for tests but
	// production wiring (defaultBuilder) sets it from mgr.GetAPIReader.
	APIReader client.Reader

	// RestConfig is the manager's rest.Config, cloned per-reconcile and
	// stamped with a freshly-minted ServiceAccount token to mint a tenant
	// client. nil disables impersonation — fake-client tests that don't
	// model TokenRequest set this to nil and the reconciler falls back to
	// Client for tenant operations.
	RestConfig *rest.Config

	// TokenCache mints + caches bearer tokens for the tenant SAs the
	// reconciler impersonates. nil pairs with RestConfig nil — both off
	// means "use r.Client". With RestConfig set, TokenCache must be set
	// too; defaultBuilder wires the pair.
	TokenCache *tokenCache

	// ClientCache memoizes the impersonating controller-runtime client per
	// (namespace, SA) so a reconcile against a cached, unchanged tenant
	// token can skip client.New entirely (which builds a fresh RESTMapper
	// + transport — non-trivial on the per-event hot path). nil disables
	// caching; tenantClient still works, just constructs a client on every
	// call. defaultBuilder wires this together with TokenCache.
	ClientCache *tenantClientCache

	// CycleCache memoizes the dependency-cycle verdict per snippet UID,
	// keyed by snip.Generation. The watch handlers (mapJsonnetLibrary,
	// mapFluxSource) Forget the entries they enqueue so a library or
	// upstream-source change re-triggers the walk — generation alone does
	// not catch a transitively-introduced cycle. nil disables caching;
	// detectSourceRefCycle still works, just walks the graph every
	// reconcile.
	CycleCache *cycleCache

	// DefaultServiceAccount fills in for snippets that omit
	// spec.serviceAccountName. Empty leaves such snippets rejected with
	// ReasonServiceAccountMissing.
	DefaultServiceAccount string

	// NoCrossNamespaceRefs mirrors Config.NoCrossNamespaceRefs; when true,
	// a snippet referencing a library outside its own namespace fails with
	// ReasonCrossNamespaceRefRejected.
	NoCrossNamespaceRefs bool

	// ExtVars is the operator-level external-variable map. Conflicting CR
	// keys are rejected with ReasonExternalVariableConflict.
	ExtVars map[string]string

	// EvaluationTimeout bounds a single eval; zero disables the bound.
	EvaluationTimeout time.Duration

	// MaxStack overrides go-jsonnet's default; zero keeps the default.
	MaxStack int

	// Fetcher resolves spec.sourceRef into in-memory files via Flux source
	// CRs. nil makes any sourceRef return ReasonSourceRefNotYetSupported
	// — useful for tests that don't model source-controller. Production
	// defaultBuilder always wires sources.New.
	Fetcher SourceFetcher

	// Publisher writes the artifact tarball and upserts the matching
	// ExternalArtifact CR. nil disables publication — useful for unit tests
	// that only exercise the eval pipeline.
	Publisher *Publisher

	// Limiter applies per-snippet rate limiting before each eval+publish.
	// nil disables the limiter (tests, or operator started with
	// --rerender-rate=0).
	Limiter *RateLimiter

	// Clock is the time source for RevisionEntry timestamps. nil falls
	// back to time.Now — tests inject a fake.
	Clock func() time.Time

	// EventRecorder emits standard Kubernetes Events on Ready-condition
	// transitions so Flux's notification-controller (or any other
	// Event-sourced alerter) can route via Alert CRs targeting
	// JsonnetSnippet. nil disables event emission. The reason and
	// message mirror what's written to the Ready condition; severity
	// is Normal for Synced and Warning for every other reason.
	//
	// Uses the events.v1 API (k8s.io/client-go/tools/events) — the
	// older record.EventRecorder was deprecated in controller-runtime.
	// notification-controller listens on both forms.
	EventRecorder events.EventRecorder

	// OCILibraries mirrors Config.OCILibraries — the byte contents of
	// every operator-shipped (OCI-mounted) library, keyed by alias.
	// resolveLibraries merges these into the per-snippet library map
	// after the CR loop, so snippets can `import "<alias>/file"`
	// against shared libraries without a CR LibraryRef.
	OCILibraries map[string]eval.Library

	// KnownLibraryAliases mirrors Config.KnownLibraryAliases — populated
	// at SetupWithManager time. The reconciler consults it to reject
	// LibraryRef.ImportPath values that collide with OCI-mounted
	// library names.
	KnownLibraryAliases []string

	// MaxWithdrawWait bounds the time a deleted snippet's finalizer
	// can hold while Publisher.Withdraw keeps failing. Past the
	// bound, reconcileDelete force-drops the finalizer, emits a
	// Warning WithdrawForced event, and the snippet is GC'd —
	// possibly leaving an orphan tarball. Zero falls back to
	// defaultMaxWithdrawWait. See Config.MaxWithdrawWait for the
	// rationale.
	MaxWithdrawWait time.Duration

	// Logger receives reconcile-level logs. nil falls back to slog.Default.
	Logger *slog.Logger

	// missingFluxKinds accumulates Flux source GVKs that SetupWithManager
	// found uninstalled. The crdWatcher subscribes to this list and
	// engages a live watch via engageFluxWatch when the CRD becomes
	// Established — no process restart required.
	missingFluxKinds []schema.GroupVersionKind

	// controller is the controller-runtime Controller this reconciler
	// is registered with. Held so engageFluxWatch can add new watches
	// at runtime when a previously-missing CRD becomes available.
	// Populated by SetupWithManager (via builder.Build).
	controller controller.Controller

	// mgrCache is the manager's cache, captured at SetupWithManager
	// time. Dynamic watches need it to construct source.Kind sources.
	mgrCache cache.Cache

	// missingMu serializes reads/writes to missingFluxKinds. The slice
	// is written by SetupWithManager (once) and EngageFluxWatch (per
	// CRD install event from crdWatcher), and read by
	// MissingFluxSourceKinds (crdWatcher boot, tests, future
	// observability hooks).
	missingMu sync.RWMutex
}

// Reconcile is invoked for every JsonnetSnippet create/update/delete event
// the manager surfaces to this controller.
func (r *SnippetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, span := observability.Tracer().Start(ctx, "JsonnetSnippet.Reconcile",
		trace.WithAttributes(
			attribute.String("jaas.namespace", req.Namespace),
			attribute.String("jaas.name", req.Name),
		))
	defer span.End()

	logger := r.logger().With(
		slog.String("namespace", req.Namespace),
		slog.String("name", req.Name),
	)

	var snip jaasv1.JsonnetSnippet
	if err := r.Client.Get(ctx, req.NamespacedName, &snip); err != nil {
		if apierrors.IsNotFound(err) {
			logger.DebugContext(ctx, "JsonnetSnippet not found; assuming deletion")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	span.SetAttributes(attribute.Int64("jaas.generation", snip.Generation))

	if !snip.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, &snip)
	}

	if !controllerutil.ContainsFinalizer(&snip, FinalizerName) {
		controllerutil.AddFinalizer(&snip, FinalizerName)
		if err := r.Client.Update(ctx, &snip); err != nil {
			return ctrl.Result{}, err
		}
		// Adding a finalizer doesn't change metadata.generation, so the
		// resulting Update event is dropped by the GenerationChangedPredicate
		// on the For() watch — the next reconcile has to be requested
		// explicitly rather than relying on that event.
		logger.DebugContext(ctx, "Finalizer added; requeuing to render")
		return ctrl.Result{Requeue: true}, nil
	}

	return r.reconcileSpec(ctx, logger, &snip)
}

// reconcileDelete withdraws the published artifact and drops the finalizer.
// Withdraw failures requeue so a flaky API server doesn't orphan tarballs,
// but a permanently-broken backend (S3 perma-down, revoked RBAC, deleted
// bucket) would otherwise make the snippet undeletable and block namespace
// teardown. MaxWithdrawWait caps that wait: once exceeded, the reconciler
// emits a Warning WithdrawForced event, drops the finalizer anyway, and
// logs the orphaned-storage risk so operators can clean up by hand.
func (r *SnippetReconciler) reconcileDelete(ctx context.Context, logger *slog.Logger, snip *jaasv1.JsonnetSnippet) (ctrl.Result, error) {
	// Drop the cycle verdict up front, on every delete reconcile. The full
	// forgetPerSnippetCaches runs only on the GC-able exits below (it skips the
	// requeuing error paths so a snippet that comes back doesn't re-mint its
	// token), but the cycle verdict is keyed by a UID that is never reused and
	// is cheap to recompute, so leaving it pinned on a delete that keeps failing
	// its Withdraw would leak an entry with no TTL. Forget is idempotent and
	// only the live reconcile path computes cycles, so dropping it here is safe.
	r.CycleCache.Forget(snip.UID)
	if !controllerutil.ContainsFinalizer(snip, FinalizerName) {
		// Snippet is on its way out without our finalizer — either we
		// never got to add it (transient apiserver 5xx during the Update
		// in Reconcile), a prior force-drop cleared it, or an operator
		// edited it off manually. Either way the apiserver will GC the
		// object on its own; our caches need to forget it now or they'll
		// leak per-snippet entries (rate-limit bucket, cycle verdict,
		// tenant SA token / client) until process restart.
		r.forgetPerSnippetCaches(ctx, logger, snip)
		return ctrl.Result{}, nil
	}
	var forced *forceDropInfo
	if r.Publisher != nil {
		tenant, err := r.tenantClient(ctx, snip)
		if err != nil {
			info, requeueErr := r.classifyWithdrawFailure(snip, err,
				"tenant_client_timeout", "tenant_client_permanent", "build impersonation client")
			if info == nil {
				return ctrl.Result{}, requeueErr
			}
			forced = info // fall through to RemoveFinalizer + Update below.
		} else if err := r.Publisher.Withdraw(ctx, tenant, snip); err != nil {
			info, requeueErr := r.classifyWithdrawFailure(snip, err,
				"withdraw_timed_out", "withdraw_permanent", "withdraw artifact")
			if info == nil {
				return ctrl.Result{}, requeueErr
			}
			forced = info
		}
	}
	controllerutil.RemoveFinalizer(snip, FinalizerName)
	if err := r.Client.Update(ctx, snip); err != nil {
		// Force-drop event + metric are NOT emitted here: a failed Update means
		// the finalizer is still on. The retry re-decides and emits once the
		// Update lands, so the alert metric counts one drop, not one per retry.
		return ctrl.Result{}, err
	}
	if forced != nil {
		r.forceDropFinalizer(ctx, logger, snip, forced.elapsed, forced.dropReason, forced.lastErr)
	}
	r.forgetPerSnippetCaches(ctx, logger, snip)
	logger.InfoContext(ctx, "Finalizer removed; JsonnetSnippet may now be garbage-collected")
	return ctrl.Result{}, nil
}

// classifyWithdrawFailure decides how a deletion-path failure — building the
// tenant impersonation client, or Publisher.Withdraw itself — is handled. A
// permanent apiserver error (Forbidden on the SA TokenRequest or the
// ExternalArtifact Delete, NoMatch on a missing kind, etc.) won't heal by
// retry, so it force-drops the finalizer immediately rather than pinning the
// snippet in Terminating for the full MaxWithdrawWait; the same applies once
// that wait elapses. The orphan-tarball cost is identical whether the
// timeout or the classifier fires. timeoutReason / permanentReason label the
// jaas_snippet_force_drop_total metric; wrapPrefix contextualises err.
//
// Returns (info, nil) when the finalizer should be force-dropped — the caller
// removes the finalizer and then calls forceDropFinalizer(info) to emit the
// Warning event + metric, so they fire exactly once, AFTER the finalizer Update
// actually lands. Returns (nil, requeueErr) for the caller to return and let
// controller-runtime retry. Emitting before the Update would double-count the
// jaas_snippet_force_drop_total alert metric on every failed-Update retry.
func (r *SnippetReconciler) classifyWithdrawFailure(snip *jaasv1.JsonnetSnippet, err error, timeoutReason, permanentReason, wrapPrefix string) (*forceDropInfo, error) {
	wrapped := fmt.Errorf("%s: %w", wrapPrefix, err)
	forceDrop, elapsed := r.withdrawTimedOut(snip)
	dropReason := timeoutReason
	if !forceDrop && isPermanentAPIError(err) {
		forceDrop = true
		elapsed = r.now().Sub(snip.DeletionTimestamp.Time)
		dropReason = permanentReason
	}
	if forceDrop {
		return &forceDropInfo{elapsed: elapsed, dropReason: dropReason, lastErr: wrapped}, nil
	}
	return nil, wrapped
}

// forceDropInfo carries the force-drop decision from classifyWithdrawFailure to
// the post-Update emission point, so the event + metric fire once the finalizer
// is genuinely gone, not on a retry that hasn't dropped it yet.
type forceDropInfo struct {
	elapsed    time.Duration
	dropReason string
	lastErr    error
}

// forgetPerSnippetCaches evicts every cache entry keyed by this snippet
// (rate-limit bucket, cycle verdict) plus the tenant SA token + client
// when no other live snippet still references that SA. Called from both
// reconcileDelete exit paths that represent "snippet is now GC-able":
// the no-finalizer early-return and the successful-Withdraw + finalizer
// removal. Error paths intentionally skip — the snippet may come back
// for another reconcile and would have to re-mint a token / re-walk.
//
// Forgetting on an SA-shared-with-others would force unrelated snippets
// to re-mint on their next reconcile; isLastSnippetUsingSA gates that.
// A List failure during the check conservatively skips eviction — the
// 1h token TTL handles cleanup eventually.
func (r *SnippetReconciler) forgetPerSnippetCaches(ctx context.Context, logger *slog.Logger, snip *jaasv1.JsonnetSnippet) {
	r.Limiter.Forget(snip.Namespace + "/" + snip.Name)
	r.CycleCache.Forget(snip.UID)
	deleteSnippetMetrics(snip.Namespace, snip.Name)
	sa := r.effectiveServiceAccount(snip)
	if sa == "" || r.TokenCache == nil {
		return
	}
	last, err := r.isLastSnippetUsingSA(ctx, snip.Namespace, sa, snip.Name)
	if err != nil {
		logger.DebugContext(ctx, "Skipping TokenCache.Forget; List failed",
			slog.Any("error", err))
		return
	}
	if last {
		r.TokenCache.Forget(snip.Namespace, sa)
		r.ClientCache.Forget(snip.Namespace + "/" + sa)
	}
}

// withdrawTimedOut returns (true, elapsed) when the snippet has been in
// the deletion path longer than the configured MaxWithdrawWait, signalling
// reconcileDelete to force-drop the finalizer. The elapsed value is
// surfaced in the Warning event + log line so operators can see how long
// the snippet was stuck.
//
// A zero DeletionTimestamp (impossible in practice — reconcileDelete is
// only entered after the timestamp lands) means "not yet timed out"; the
// caller will go through the normal Withdraw path.
func (r *SnippetReconciler) withdrawTimedOut(snip *jaasv1.JsonnetSnippet) (bool, time.Duration) {
	if snip.DeletionTimestamp.IsZero() {
		return false, 0
	}
	wait := r.MaxWithdrawWait
	if wait <= 0 {
		wait = defaultMaxWithdrawWait
	}
	elapsed := r.now().Sub(snip.DeletionTimestamp.Time)
	return elapsed >= wait, elapsed
}

// forceDropFinalizer emits the Warning event + metric + diagnostic log that
// accompanies a force-drop of the finalizer. reconcileDelete calls it only
// AFTER the RemoveFinalizer + Client.Update has succeeded, so the event and the
// jaas_snippet_force_drop_total metric fire exactly once per actual drop rather
// than once per failed-Update retry.
//
// dropReason is a short stable string naming which decision branch
// triggered the drop ("tenant_client_timeout", "tenant_client_permanent",
// "withdraw_timed_out", "withdraw_permanent"). It labels the
// jaas_snippet_force_drop_total counter so a permanently-broken
// pipeline shows up in Prometheus rather than only in event-stream +
// logs.
func (r *SnippetReconciler) forceDropFinalizer(ctx context.Context, logger *slog.Logger, snip *jaasv1.JsonnetSnippet, elapsed time.Duration, dropReason string, lastErr error) {
	prefix := snip.Namespace + "/" + snip.Name
	orphans := prefix + "/<rev>.tar.gz"
	if revs := knownRevisionPaths(prefix, snip.Status.History); revs != "" {
		orphans = revs
	}
	msg := fmt.Sprintf("WithdrawForced after %s of failing Withdraw — finalizer dropped; storage may carry orphaned tarballs under %s/ that an operator must remove by hand (known revisions: %s). Last error: %v", elapsed.Round(time.Second), prefix, orphans, lastErr)
	logger.WarnContext(ctx, "Force-dropping finalizer after MaxWithdrawWait",
		slog.Duration("elapsed", elapsed),
		slog.String("reason", dropReason),
		slog.Any("lastError", lastErr))
	recordForceDrop(snip.Namespace, snip.Name, dropReason)
	if r.EventRecorder != nil {
		r.EventRecorder.Eventf(snip, nil, "Warning", "WithdrawForced", "WithdrawForced", "%s", msg)
	}
}

// knownRevisionPaths renders the storage paths of every revision the
// snippet's status.history records, as a comma-separated list, so the
// WithdrawForced message points an operator at concrete files to remove
// rather than a placeholder. Returns "" when no history is recorded —
// the caller falls back to the generic "<rev>" form. The path shape
// matches storage.Backend.Put: <namespace>/<name>/<shortRev>.tar.gz.
func knownRevisionPaths(prefix string, history []jaasv1.RevisionEntry) string {
	revs := shortRevs(history)
	if len(revs) == 0 {
		return ""
	}
	paths := make([]string, 0, len(revs))
	for _, short := range revs {
		paths = append(paths, prefix+"/"+short+".tar.gz")
	}
	return strings.Join(paths, ", ")
}

// shortRevs maps a revision-history slice to its short revision strings
// (the "sha256:" prefix stripped), dropping empty entries. The short form
// is what storage keys tarballs by and what Prune's keep-set matches.
func shortRevs(history []jaasv1.RevisionEntry) []string {
	out := make([]string, 0, len(history))
	for _, h := range history {
		if short := strings.TrimPrefix(h.Revision, "sha256:"); short != "" {
			out = append(out, short)
		}
	}
	return out
}

// reconcileSpec runs the inline-files pipeline end-to-end and writes the
// resulting status. Errors are converted to Ready=False/<reason> rather than
// returned, so a failed reconcile does not requeue endlessly — the watch
// fires again when the snippet (or a referenced library) is updated.
//
// judgedGen captures metadata.generation at entry so the eventual
// publish gate (and ObservedGeneration writes) refer to the spec we
// actually evaluated — not whatever the apiserver's latest is at write
// time, which could carry a mid-reconcile spec edit.
func (r *SnippetReconciler) reconcileSpec(ctx context.Context, logger *slog.Logger, snip *jaasv1.JsonnetSnippet) (ctrl.Result, error) {
	judgedGen := snip.Generation
	if snip.Spec.Suspend {
		// Pause reconciliation without disturbing the existing artifact.
		// The Ready=False/Suspended condition flips on every reconcile
		// pass while the spec stays suspended — failReady's
		// SetStatusCondition no-ops when the new condition is
		// equivalent to the previous one, so this doesn't burn writes.
		//
		// Grace-expired revisions still need to drain: a snippet that
		// stays suspended for the operator's lifetime would otherwise
		// hold every prior-keep-set revision on storage forever. Run
		// the GC pass here using the existing Status.History keep-set
		// (no new revision to add since publish is paused). Watch ticks
		// + spec.interval drive the cadence; nothing else is needed
		// because the deletion path's Withdraw bypasses grace entirely.
		if r.Publisher != nil && len(snip.Status.History) > 0 {
			keep := shortRevs(snip.Status.History)
			if err := r.Publisher.PruneStored(ctx, snip.Namespace, snip.Name, keep); err != nil {
				logger.WarnContext(ctx, "Suspended snippet GC prune failed", slog.Any("error", err))
			}
		}
		return r.failReady(ctx, snip, ReasonSuspended,
			"spec.suspend is true; pause until the operator unsets it")
	}

	if reason, msg := r.checkServiceAccount(snip); reason != "" {
		return r.failReady(ctx, snip, reason, msg)
	}

	if reason, msg := r.checkExtVarConflicts(snip); reason != "" {
		return r.failReady(ctx, snip, reason, msg)
	}

	if reason, msg := r.checkLibraryAliasCollisions(snip); reason != "" {
		return r.failReady(ctx, snip, reason, msg)
	}

	if reason, msg := checkDuplicateLibraryImports(snip); reason != "" {
		return r.failReady(ctx, snip, reason, msg)
	}

	// Cycle detection runs before any tenant work: with ExternalArtifact
	// in the watch set, a sourceRef chain back into this snippet would
	// loop forever without this gate. The CycleCache short-circuits the
	// walk when neither this snippet's Generation nor any watched
	// dependency CR has changed since the last verdict — the watch
	// handlers Forget the entry on library / upstream-source events.
	cycle, path, err := r.cycleVerdict(ctx, snip)
	if err != nil {
		// The BFS walks ExternalArtifact / JsonnetLibrary references via the
		// operator client. A permanent apiserver error in that walk — most
		// concretely a snippet referencing kind: ExternalArtifact in a
		// cluster without source-controller's CRD, which yields a
		// NoMatchError — won't heal by retry. Without classification it
		// bubbles up raw, stranding the snippet in infinite backoff with an
		// empty Ready status (this is the one reconcile path that otherwise
		// escapes failReady). Surface it as a terminal Ready=False so
		// kubectl describe shows an actionable message.
		if isPermanentAPIError(err) {
			logger.ErrorContext(ctx, "Permanent error during cycle detection", slog.Any("error", err))
			return r.failReady(ctx, snip, ReasonRBACDenied,
				rbacDenialMessage("walking the dependency graph for cycle detection", err))
		}
		// Transient: still write Ready=False with the error so describe
		// surfaces something actionable, then return the error to engage
		// controller-runtime's backoff.
		logger.ErrorContext(ctx, "Cycle detection errored", slog.Any("error", err))
		if _, ferr := r.failReady(ctx, snip, ReasonSourceFetchFailed,
			"dependency-cycle detection could not complete: "+err.Error()); ferr != nil {
			return ctrl.Result{}, ferr
		}
		return ctrl.Result{}, err
	}
	if cycle {
		return r.failReady(ctx, snip, ReasonDependencyCycle, "spec.sourceRef chain forms a cycle: "+path)
	}

	if allowed, delay := r.Limiter.Reserve(snip.Namespace + "/" + snip.Name); !allowed {
		logger.DebugContext(ctx, "Snippet rate-limited; deferring",
			slog.Duration("retryAfter", delay))
		recordRateLimited(snip.Namespace, snip.Name)
		// Backpressure event, not a failure — the Ready condition
		// stays as it was. The EventRecorder's own dedupe collapses
		// bursts (same reason within the cache window become one
		// event with an updated count), so this is safe to fire on
		// every denied Reserve.
		if r.EventRecorder != nil {
			r.EventRecorder.Eventf(snip, nil, corev1.EventTypeWarning, "RateLimited", "RateLimited",
				"reconcile deferred for %s by --reconcile-rate-limit", delay)
		}
		return ctrl.Result{RequeueAfter: delay}, nil
	}

	tenant, err := r.tenantClient(ctx, snip)
	if err != nil {
		// Symmetric with the reconcileDelete tenantClient path. A Forbidden
		// on the SA TokenRequest (operator SA missing
		// `serviceaccounts/token: create`), a NoMatch on the SA kind,
		// or any other permanent apiserver error won't heal by retry —
		// the cluster operator has to grant a verb or install a CRD.
		// Surface as Ready=False/RBACDenied with an actionable message
		// instead of pinning the snippet at Ready=Unknown while the
		// workqueue burns retries.
		if isPermanentAPIError(err) {
			logger.ErrorContext(ctx, "Permanent error building impersonation client", slog.Any("error", err))
			return r.failReady(ctx, snip, ReasonRBACDenied,
				rbacDenialMessage("minting the tenant SA token", err))
		}
		logger.ErrorContext(ctx, "Cannot build impersonation client", slog.Any("error", err))
		return ctrl.Result{}, fmt.Errorf("build impersonation client: %w", err)
	}

	files, reason, msg, transient, transientErr := r.tracedResolveSource(ctx, tenant, snip)
	if reason != "" {
		// Always write the status so kubectl describe surfaces the
		// failure. If the error class is transient (source-controller
		// hasn't caught up yet, a 5xx blip, etc.), also return an
		// error so controller-runtime's exponential backoff retries
		// faster than waiting for the next watch tick. Non-transient
		// stays steady-state — the next genuine watch event re-runs.
		if _, err := r.failReady(ctx, snip, reason, msg); err != nil {
			return ctrl.Result{}, err
		}
		if transient {
			// Wrapping the original Fetcher error preserves the
			// sentinel chain (ErrSourceNotReady, ErrArtifactMissing,
			// urlguard.*) so errors.Is downstream — controller-
			// runtime's classifier, future circuit-breaker hooks —
			// can match. A fresh errors.New(msg) would erase the
			// chain.
			return ctrl.Result{}, fmt.Errorf("%w", transientErr)
		}
		return ctrl.Result{}, nil
	}

	entry := snip.Spec.EntryFile
	if entry == "" {
		entry = EntryFileName
	}
	// An empty files map can mean two different things to the
	// operator: the tarball really was empty, or spec.sourceRef.path
	// narrowed every entry away. The two failures map to opposite
	// actions (fix the upstream vs. widen the filter), so the message
	// has to distinguish.
	if len(files) == 0 {
		if snip.Spec.SourceRef != nil && snip.Spec.SourceRef.Path != "" {
			return r.failReady(ctx, snip, ReasonInvalidSpec,
				fmt.Sprintf("spec.sourceRef.path %q matched no files in the artifact; widen the path filter or remove it",
					snip.Spec.SourceRef.Path))
		}
		return r.failReady(ctx, snip, ReasonInvalidSpec,
			"source contains no files; check the upstream artifact")
	}
	mainSource, ok := files[entry]
	if !ok {
		return r.failReady(ctx, snip, ReasonInvalidSpec,
			fmt.Sprintf("source must contain %q as the snippet entry point", entry))
	}

	libs, reason, msg, err := r.tracedResolveLibraries(ctx, tenant, snip)
	if err != nil {
		// Transient: surface the error so the manager requeues with backoff.
		logger.ErrorContext(ctx, "Library resolution errored", slog.Any("error", err))
		return ctrl.Result{}, err
	}
	if reason != "" {
		return r.failReady(ctx, snip, reason, msg)
	}

	rendered, reason, msg, err := r.tracedEvaluate(ctx, snip, files, mainSource, libs)
	if errors.Is(err, eval.ErrEvalUnavailable) {
		// Concurrent-eval cap is full. Backpressure event + metric so
		// operators can see why the snippet hasn't reconciled; Ready
		// condition stays untouched (overwriting Ready=True with a
		// transient throttle would mask the real state). The
		// EventRecorder's own dedupe collapses bursts into one event
		// with an updated count.
		logger.DebugContext(ctx, "Snippet evaluation rejected; concurrent-eval cap full",
			slog.Duration("retryAfter", evalUnavailableRequeueAfter))
		recordEvalUnavailable(snip.Namespace, snip.Name)
		if r.EventRecorder != nil {
			r.EventRecorder.Eventf(snip, nil, corev1.EventTypeWarning, "EvalUnavailable", "EvalUnavailable",
				"reconcile deferred for %s by --max-concurrent-evals", evalUnavailableRequeueAfter)
		}
		return ctrl.Result{RequeueAfter: evalUnavailableRequeueAfter}, nil
	}
	if err != nil {
		logger.ErrorContext(ctx, "Eval errored on transient path", slog.Any("error", err))
		return ctrl.Result{}, err
	}
	if reason != "" {
		return r.failReady(ctx, snip, reason, msg)
	}

	// Pre-publish staleness gate. The fetch + eval phases above can
	// take seconds; if the spec moved during that window, we'd be
	// publishing a stale rendering. publishConsistencyGate re-Gets
	// the snippet through the uncached APIReader; on mismatch we
	// defer — the watch event from the spec edit has already
	// enqueued the next reconcile.
	latest, proceed, err := r.publishConsistencyGate(ctx, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}, judgedGen)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("publish consistency gate: %w", err)
	}
	if !proceed {
		logger.InfoContext(ctx, "Publish deferred: spec moved since reconcile started",
			slog.Int64("judgedGeneration", judgedGen))
		return ctrl.Result{}, nil
	}

	// Build the keep-set the Publisher's Prune will retain: the
	// rendered content's revision (computed eagerly here so we can
	// merge it with prior history) plus the most-recent (history-1)
	// entries from Status.History. shortRev strips the "sha256:" prefix
	// to match what the Backend wants on disk. Derive it from the gate's
	// fresh read (latest), not the lagging cache (snip), so the pruned-on-
	// disk set agrees with the history markSynced writes from the same
	// fresh apiserver state — otherwise a kept revision's tarball could be
	// pruned out from under a downstream consumer.
	renderedRev := sha256ContentHash(rendered)
	keepShortRevs := buildKeepShortRevs(renderedRev, latest.Status.History, latest.Spec.History)

	pr, err := r.tracedPublish(ctx, tenant, snip, rendered, files, keepShortRevs)
	if err != nil {
		// ErrArtifactTooLarge is a snippet-author problem, not a
		// transient operator issue — flip Ready=False with a stable
		// Reason instead of bubbling up to controller-runtime's retry.
		if errors.Is(err, ErrArtifactTooLarge) {
			return r.failReady(ctx, snip, ReasonArtifactTooLarge, err.Error())
		}
		// RBAC denial / missing CRD during the ExternalArtifact
		// upsert — non-recoverable by retry until the cluster
		// operator grants the verb. Stop engaging backoff.
		if isPermanentAPIError(err) {
			return r.failReady(ctx, snip, ReasonRBACDenied,
				rbacDenialMessage("publishing the ExternalArtifact", err))
		}
		logger.ErrorContext(ctx, "Publish errored", slog.Any("error", err))
		return ctrl.Result{}, err
	}
	return r.markSynced(ctx, snip, rendered, pr.Revision, pr.URL, len(files), judgedGen)
}

// sha256ContentHash returns the "sha256:<hex>" revision string the
// Publisher would produce for rendered. Lives here so reconcileSpec can
// compute the keep-set ahead of the actual Publish without re-running
// the gzip pipeline.
func sha256ContentHash(rendered string) string {
	sum := sha256.Sum256([]byte(rendered))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// buildKeepShortRevs assembles the short-revision keep-set that pairs
// with the publish about to happen. Order: new revision first, then
// up to (history-1) most-recent entries from prior status.History,
// dedup'd. Always at least 1 (the new rev). history<=0 falls back to 1.
func buildKeepShortRevs(newRev string, prior []jaasv1.RevisionEntry, history int32) []string {
	limit := int(history)
	if limit < 1 {
		limit = 1
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, limit)

	add := func(rev string) bool {
		short := strings.TrimPrefix(rev, "sha256:")
		if short == "" {
			return false
		}
		if _, dup := seen[short]; dup {
			return false
		}
		seen[short] = struct{}{}
		out = append(out, short)
		return len(out) >= limit
	}
	if add(newRev) {
		return out
	}
	for _, e := range prior {
		if add(e.Revision) {
			break
		}
	}
	return out
}

// tracedResolveSource wraps resolveSnippetSource in an OTel child span
// carrying source-mode + size attributes. Inline files vs. sourceRef
// are split via SnippetSource shape so flame graphs separate the two
// cost profiles cleanly.
func (r *SnippetReconciler) tracedResolveSource(ctx context.Context, tenant client.Client, snip *jaasv1.JsonnetSnippet) (map[string]string, string, string, bool, error) {
	mode := "inline"
	if snip.Spec.SourceRef != nil {
		mode = "sourceRef"
	}
	ctx, span := observability.Tracer().Start(ctx, "snippet.resolveSource",
		trace.WithAttributes(
			attribute.String("jaas.source.mode", mode),
		))
	defer span.End()
	files, reason, msg, transient, transientErr := r.resolveSnippetSource(ctx, tenant, snip.Namespace, snip.Spec.SnippetSource)
	span.SetAttributes(attribute.Int("jaas.source.fileCount", len(files)))
	if reason != "" {
		span.SetAttributes(attribute.String("jaas.reason", reason))
		span.SetAttributes(attribute.Bool("jaas.transient", transient))
	}
	if transientErr != nil {
		span.RecordError(transientErr)
		span.SetStatus(codes.Error, "source resolution failed")
	}
	return files, reason, msg, transient, transientErr
}

// tracedResolveLibraries wraps resolveLibraries in a child span.
// jaas.library.count distinguishes "snippet uses zero libraries" (fast)
// from "snippet pulls a 20-library tree" (potentially slow).
func (r *SnippetReconciler) tracedResolveLibraries(ctx context.Context, tenant client.Client, snip *jaasv1.JsonnetSnippet) (map[string]eval.Library, string, string, error) {
	ctx, span := observability.Tracer().Start(ctx, "snippet.resolveLibraries",
		trace.WithAttributes(
			attribute.Int("jaas.library.count", len(snip.Spec.Libraries)),
		))
	defer span.End()
	libs, reason, msg, err := r.resolveLibraries(ctx, tenant, snip)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "library resolution failed")
	}
	if reason != "" {
		span.SetAttributes(attribute.String("jaas.reason", reason))
	}
	return libs, reason, msg, err
}

// tracedEvaluate wraps the eval phase. jaas.eval.renderedBytes is the
// most useful single attribute when correlating an OTel trace with a
// memory-spike investigation.
func (r *SnippetReconciler) tracedEvaluate(ctx context.Context, snip *jaasv1.JsonnetSnippet, files map[string]string, mainSource string, libs map[string]eval.Library) (string, string, string, error) {
	ctx, span := observability.Tracer().Start(ctx, "snippet.evaluate",
		trace.WithAttributes(
			attribute.Int("jaas.source.fileCount", len(files)),
			attribute.Int("jaas.library.count", len(libs)),
		))
	defer span.End()
	rendered, reason, msg, err := r.evaluate(ctx, snip, files, mainSource, libs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "evaluation failed")
	}
	span.SetAttributes(attribute.Int("jaas.eval.renderedBytes", len(rendered)))
	if reason != "" {
		span.SetAttributes(attribute.String("jaas.reason", reason))
	}
	return rendered, reason, msg, err
}

// tracedPublish wraps the publish phase. jaas.publish.revision lets a
// trace cross-reference with the resulting ExternalArtifact. The
// keepShortRevs parameter is forwarded verbatim to the Publisher's
// Prune step. sourceFiles carries the resolved snippet source (inline
// spec.files or sourceRef-fetched content) so source-mode publishes
// the actual file map rather than only inline files.
func (r *SnippetReconciler) tracedPublish(ctx context.Context, tenant client.Client, snip *jaasv1.JsonnetSnippet, rendered string, sourceFiles map[string]string, keepShortRevs []string) (PublishResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "snippet.publish",
		trace.WithAttributes(
			attribute.Int("jaas.eval.renderedBytes", len(rendered)),
			attribute.Bool("jaas.publish.enabled", r.Publisher != nil),
			attribute.Int("jaas.publish.retainCount", len(keepShortRevs)),
		))
	defer span.End()
	pr, err := r.publish(ctx, tenant, snip, rendered, sourceFiles, keepShortRevs)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish failed")
	}
	if pr.Revision != "" {
		span.SetAttributes(attribute.String("jaas.publish.revision", pr.Revision))
	}
	return pr, err
}

// publish hands the rendered bytes to the optional Publisher. With no
// publisher (tests or a not-yet-wired operator), the reconciler derives the
// revision locally so Status.Revision still moves forward.
//
// keepShortRevs is the sha256-short keep-set (no "sha256:" prefix) the
// Publisher's Prune retains. Built from Status.History the caller is
// about to write — see prependHistory.
func (r *SnippetReconciler) publish(ctx context.Context, tenant client.Client, snip *jaasv1.JsonnetSnippet, rendered string, sourceFiles map[string]string, keepShortRevs []string) (PublishResult, error) {
	if r.Publisher == nil {
		// No publisher wired (tests, eval-only mode) — derive a revision
		// locally so status.revision still moves forward, but leave URL
		// empty since there's no on-disk artifact to point at.
		sum := sha256.Sum256([]byte(rendered))
		return PublishResult{Revision: "sha256:" + hex.EncodeToString(sum[:])}, nil
	}
	return r.Publisher.Publish(ctx, tenant, snip, rendered, sourceFiles, keepShortRevs)
}

// cycleVerdict consults the CycleCache before walking the dependency graph.
// On hit (same UID + Generation as the cached entry) it returns the cached
// verdict; on miss it walks via detectSourceRefCycle and stores the result.
// The watch handlers Forget cache entries on library / upstream-source
// events, so a transitively-introduced cycle still triggers a fresh walk
// on the next reconcile.
//
// Forget-during-walk handling: the Lookup returns the per-UID epoch and the
// post-walk Store includes it; a Forget landing between Lookup and Store
// bumps the epoch, dropping the (now-stale) write. The caller retries the
// walk so the invalidating event isn't silently absorbed. After
// maxCycleVerdictRetries attempts the loop falls back to a final un-cached
// walk — a pathological tight Forget loop shouldn't spin forever.
func (r *SnippetReconciler) cycleVerdict(ctx context.Context, snip *jaasv1.JsonnetSnippet) (bool, string, error) {
	if snip.UID == "" {
		// CycleCache keys on UID. An empty UID makes every Store /
		// Lookup / Forget a no-op, so the cache cannot engage. The
		// apiserver populates UID on object Create, so this only
		// surfaces in unit tests that build snippets via fake clients
		// without seeding metadata.uid. Log at Debug so a test runner
		// that watches the logs surfaces the gap; the reconcile still
		// proceeds with an uncached walk on every call.
		r.logger().DebugContext(ctx, "Snippet has empty UID; cycleCache cannot engage",
			slog.String("namespace", snip.Namespace), slog.String("name", snip.Name))
	}
	for attempt := 0; attempt < maxCycleVerdictRetries; attempt++ {
		v, epoch, ok := r.CycleCache.Lookup(snip.UID, snip.Generation)
		if ok {
			return v.hasCycle, v.path, nil
		}
		cycle, path, err := detectSourceRefCycle(ctx, r.Client, snip)
		if err != nil {
			return false, "", err
		}
		if r.CycleCache.Store(snip.UID, snip.Generation, epoch, cycle, path) {
			return cycle, path, nil
		}
		// Forget landed mid-walk; retry to pick up the post-Forget state.
	}
	// Pathological: Forget keeps racing every walk. Fall through to one
	// uncached walk so the reconcile makes forward progress on the
	// freshest verdict we can compute, even if a subsequent watch event
	// will re-evaluate.
	return detectSourceRefCycle(ctx, r.Client, snip)
}

// checkServiceAccount returns ("", "") if the snippet has an effective SA,
// otherwise (reason, message) for the status condition.
func (r *SnippetReconciler) checkServiceAccount(snip *jaasv1.JsonnetSnippet) (string, string) {
	if snip.Spec.ServiceAccountName != "" {
		return "", ""
	}
	if r.DefaultServiceAccount != "" {
		return "", ""
	}
	return ReasonServiceAccountMissing,
		"neither spec.serviceAccountName nor --default-service-account is set"
}

// checkExtVarConflicts walks spec.externalVariables and reports the first
// key that collides with the operator-level set.
func (r *SnippetReconciler) checkExtVarConflicts(snip *jaasv1.JsonnetSnippet) (string, string) {
	for k := range snip.Spec.ExternalVariables {
		if _, exists := r.ExtVars[k]; exists {
			return ReasonExternalVariableConflict,
				fmt.Sprintf("spec.externalVariables[%q] conflicts with --ext-var", k)
		}
	}
	return "", ""
}

// checkLibraryAliasCollisions rejects snippets whose LibraryRef.ImportPath
// (or .Name when ImportPath is empty) shadows an OCI-mounted library
// alias the operator was started with. The CR would silently win at
// eval time and the OCI mount would be invisible — confusing for
// operators who set up the mount expecting it to be used. Better to
// surface the conflict at admission than to silently ignore the mount.
//
// Returns ("", "") when KnownLibraryAliases is empty (validation
// disabled) or no collision is found.
func (r *SnippetReconciler) checkLibraryAliasCollisions(snip *jaasv1.JsonnetSnippet) (string, string) {
	if len(r.KnownLibraryAliases) == 0 {
		return "", ""
	}
	known := make(map[string]struct{}, len(r.KnownLibraryAliases))
	for _, a := range r.KnownLibraryAliases {
		known[a] = struct{}{}
	}
	for i, ref := range snip.Spec.Libraries {
		alias := ref.ImportPath
		if alias == "" {
			alias = ref.Name
		}
		if _, hit := known[alias]; hit {
			return ReasonInvalidSpec,
				fmt.Sprintf("spec.libraries[%d] importPath %q shadows OCI-mounted library; rename the import alias or remove the LibraryRef",
					i, alias)
		}
	}
	return "", ""
}

// checkDuplicateLibraryImports rejects snippets whose spec.libraries
// entries collide on their effective import path (ImportPath, or Name
// when empty). The import-alias namespace can hold only one library per
// path, so a collision would silently drop one library — the admission
// webhook denies it, and this is the reconciler fallback for a bypassed
// or disabled webhook. Mirrors checkLibraryAliasCollisions for OCI
// aliases.
//
// Returns ("", "") when fewer than two entries are present or every
// entry resolves to a distinct path.
func checkDuplicateLibraryImports(snip *jaasv1.JsonnetSnippet) (string, string) {
	if dup := duplicateLibraryImportPath(snip); dup != "" {
		return ReasonInvalidSpec,
			fmt.Sprintf("spec.libraries import path %q is used by more than one entry; each library must resolve to a distinct import path", dup)
	}
	return "", ""
}

// resolveLibraries fetches every CR named in spec.libraries and returns them
// keyed by the import path the snippet's source uses. Library Gets run
// through the supplied (impersonated) client so a tenant SA without
// permissions to read a library fails at the apiserver rather than the
// operator silently leaking data across tenants. The error return is
// reserved for transient API failures; spec-level problems (cross-namespace,
// unknown kind, library not found) surface as (reason, message).
func (r *SnippetReconciler) resolveLibraries(ctx context.Context, tenant client.Client, snip *jaasv1.JsonnetSnippet) (map[string]eval.Library, string, string, error) {
	// Common path: snippets with no library references against an
	// operator with no OCI-mounted libraries skip every per-CR Get,
	// the merge loop, and the map allocation. nil is a valid Library
	// map — eval.InMemoryImporter.Libraries treats nil identically
	// to an empty map.
	if len(snip.Spec.Libraries) == 0 && len(r.OCILibraries) == 0 {
		return nil, "", "", nil
	}
	out := make(map[string]eval.Library, len(snip.Spec.Libraries))
	for _, ref := range snip.Spec.Libraries {
		importPath := ref.ImportPath
		if importPath == "" {
			importPath = ref.Name
		}
		switch ref.Kind {
		case "JsonnetLibrary":
			ns := ref.Namespace
			if ns == "" {
				ns = snip.Namespace
			}
			if r.NoCrossNamespaceRefs && ns != snip.Namespace {
				return nil, ReasonCrossNamespaceRefRejected,
					fmt.Sprintf("spec.libraries[%s] points at namespace %q but --no-cross-namespace-refs is set", ref.Name, ns), nil
			}
			var lib jaasv1.JsonnetLibrary
			if err := tenant.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &lib); err != nil {
				crossNS := ns != snip.Namespace
				if apierrors.IsNotFound(err) {
					msg := fmt.Sprintf("JsonnetLibrary %q in %q not found", ref.Name, ns)
					if crossNS {
						msg = scrubbedCrossNamespaceLibraryMessage(ns, ref.Name)
					}
					return nil, ReasonLibraryNotFound, msg, nil
				}
				if isPermanentAPIError(err) {
					msg := rbacDenialMessage(fmt.Sprintf("reading JsonnetLibrary %s/%s", ns, ref.Name), err)
					if crossNS {
						msg = scrubbedCrossNamespaceLibraryMessage(ns, ref.Name)
					}
					return nil, ReasonRBACDenied, msg, nil
				}
				return nil, "", "", fmt.Errorf("get JsonnetLibrary %s/%s: %w", ns, ref.Name, err)
			}
			files, reason, msg, transient, transientErr := r.resolveSnippetSource(ctx, tenant, snip.Namespace, lib.Spec.SnippetSource)
			if reason != "" {
				// Surface library-source transients via the same backoff
				// path the snippet's own source uses: return the error so
				// controller-runtime requeues with backoff. %w preserves
				// the sentinel chain (ErrSourceNotReady, urlguard.*)
				// across the library prefix.
				if transient {
					return nil, "", "", fmt.Errorf("JsonnetLibrary %q in %q: %w", ref.Name, ns, transientErr)
				}
				return nil, reason, fmt.Sprintf("JsonnetLibrary %q in %q: %s", ref.Name, ns, msg), nil
			}
			// Reject rather than overwrite on an import-path collision.
			// checkDuplicateLibraryImports (and admission) already gate
			// this, so reaching here means a bypassed webhook AND the
			// pre-check skipped — keep the invariant local so the map
			// can never silently drop a library.
			if _, dup := out[importPath]; dup {
				return nil, ReasonInvalidSpec,
					fmt.Sprintf("spec.libraries import path %q is used by more than one entry; each library must resolve to a distinct import path", importPath), nil
			}
			out[importPath] = eval.Library{Files: files}
		default:
			return nil, ReasonInvalidSpec,
				fmt.Sprintf("spec.libraries[%s] has unsupported kind %q", ref.Name, ref.Kind), nil
		}
	}
	// Fold in OCI-mounted libraries. The admission webhook + reconciler
	// pre-check guarantee no CR alias can shadow an OCI alias, so this
	// is purely additive — every OCI alias the operator was booted with
	// is available to every snippet's eval.
	for alias, lib := range r.OCILibraries {
		if _, conflict := out[alias]; conflict {
			// Defensive: should never happen since admission rejects.
			// If it does, the CR wins (it was added first).
			continue
		}
		out[alias] = lib
	}
	return out, "", "", nil
}

// resolveSnippetSource materialises a SnippetSource into a file map. It
// handles both the snippet's own .Spec.SnippetSource and a library's
// .Spec.SnippetSource using identical rules:
//
//  1. SourceRef set → check cross-namespace, then fetch via the Fetcher.
//     With no Fetcher wired (tests, mis-deployed binary), return
//     ReasonSourceRefNotYetSupported. Fetcher errors map to one of
//     ReasonSourceNotReady / ReasonSourceFetchFailed via
//     classifyFetchError.
//  2. Files set → return inline map.
//  3. Neither → ReasonInvalidSpec.
//
// Returns (files, reason, msg, transient, transientErr). A non-empty
// reason means "set status to Ready=False with this reason"; the
// caller routes through failReady. transientErr is non-nil only when
// transient is true; it carries the original fetch error so
// downstream errors.Is checks (and the controller-runtime backoff
// path's wrapping) see the underlying sentinel — wrapping with %s
// here would erase it.
func (r *SnippetReconciler) resolveSnippetSource(ctx context.Context, tenant client.Client, ownerNs string, src jaasv1.SnippetSource) (files map[string]string, reason, msg string, transient bool, transientErr error) {
	if src.SourceRef != nil {
		if reason, msg := r.crossNamespaceSourceRef(ownerNs, src.SourceRef); reason != "" {
			return nil, reason, msg, false, nil
		}
		if r.Fetcher == nil {
			return nil, ReasonSourceRefNotYetSupported,
				"spec.sourceRef is declared but no Fetcher is wired", false, nil
		}
		res, err := r.Fetcher.Fetch(ctx, tenant, src.SourceRef, ownerNs)
		if err != nil {
			reason, msg, transient := classifyFetchError(err)
			// Cross-namespace error scrubbing: when the sourceRef
			// targets a namespace other than the snippet's own, replace
			// the raw fetcher error with a constant message. The raw
			// error would otherwise let a tenant fingerprint other
			// namespaces (NotFound vs Forbidden vs digest mismatch vs
			// 5xx tell different stories about target namespace state).
			// Same-namespace errors stay verbatim — that's the
			// tenant's own namespace and there's nothing to hide.
			if isCrossNamespaceRef(ownerNs, src.SourceRef) {
				msg = scrubbedCrossNamespaceMessage(src.SourceRef)
			}
			var origErr error
			if transient {
				origErr = err
			}
			return nil, reason, msg, transient, origErr
		}
		return res.Files, "", "", false, nil
	}
	if len(src.Files) == 0 {
		return nil, ReasonInvalidSpec,
			"neither spec.files nor spec.sourceRef is set; admission should have caught this", false, nil
	}
	return src.Files, "", "", false, nil
}

// publishConsistencyGate is the uncached re-Get that runs just before
// Publisher.Publish. If metadata.generation has moved since this
// reconcile started (judgedGen), the publish is deferred — the
// generation-bump watch event has already enqueued the next reconcile,
// which will work against the fresh spec. Stops a render against a
// stale spec from landing as a stale ExternalArtifact.
//
// The check goes through APIReader (uncached) because the manager's
// cache can lag the apiserver by tens of milliseconds under load —
// long enough for a fast spec-edit + reconcile-trigger cycle to miss.
// Returning the same (cached) generation twice would defeat the gate.
//
// (proceed=false, err=nil) means "defer cleanly"; the caller returns
// ctrl.Result{} with no error.
// publishConsistencyGate re-reads the snippet through the uncached APIReader and
// reports whether the publish should proceed (Generation still matches the one
// the render was judged against). On success it also returns the fresh object so
// the caller can derive the prune keep-set from the same snapshot the post-
// publish history write uses — building the keep-set from the lagging cache read
// taken at reconcile entry could otherwise prune a tarball that markSynced then
// records in status.History.
func (r *SnippetReconciler) publishConsistencyGate(ctx context.Context, key types.NamespacedName, judgedGen int64) (*jaasv1.JsonnetSnippet, bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var latest jaasv1.JsonnetSnippet
	if err := reader.Get(ctx, key, &latest); err != nil {
		if apierrors.IsNotFound(err) {
			// Snippet was deleted in the gap. The deletion reconcile
			// is already enqueued; nothing to publish.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("pre-publish consistency Get: %w", err)
	}
	if latest.Generation != judgedGen {
		return nil, false, nil
	}
	return &latest, true, nil
}

// isCrossNamespaceRef reports whether ref names a different namespace
// than ownerNs. An empty ref.Namespace defaults to ownerNs (the
// snippet's own namespace), so this only fires on an explicit
// out-of-namespace reference.
func isCrossNamespaceRef(ownerNs string, ref *jaasv1.SourceRef) bool {
	return ref != nil && ref.Namespace != "" && ref.Namespace != ownerNs
}

// scrubbedCrossNamespaceMessage replaces a cross-namespace fetch
// failure's raw message with a constant string that names only what
// the tenant already knows (the kind + name they wrote on their own
// CR's spec). The target namespace is included in the original error
// — but a tenant who specified it already has it; redacting it would
// just confuse debugging.
//
// The message DOES NOT include:
//   - the underlying error's text (would distinguish NotFound /
//     Forbidden / digest-mismatch / 5xx)
//   - the source-controller endpoint URL
//   - any DNS / connection / TLS detail
//
// Operators investigating a cross-namespace failure check
// source-controller's logs in the target namespace, not jaas's status.
func scrubbedCrossNamespaceMessage(ref *jaasv1.SourceRef) string {
	return fmt.Sprintf("cross-namespace %s %q is not reachable; check the source CR's status in %q",
		ref.Kind, ref.Name, ref.Namespace)
}

// scrubbedCrossNamespaceLibraryMessage is the JsonnetLibrary-equivalent
// of scrubbedCrossNamespaceMessage. Collapses NotFound / Forbidden /
// 5xx to one constant so a tenant can't fingerprint other namespaces
// via the library's reachability error.
func scrubbedCrossNamespaceLibraryMessage(libNs, libName string) string {
	return fmt.Sprintf("cross-namespace JsonnetLibrary %q is not reachable; check the library in %q",
		libName, libNs)
}

// isPermanentAPIError reports whether err is an apiserver-side
// failure that retry can't recover from. Two shapes qualify:
//
//   - apierrors.IsForbidden(err) — the SA making the call lacks the
//     required verb. Cluster operator must grant it.
//   - apimeta.IsNoMatchError / runtime.IsNotRegisteredError — the
//     resource kind isn't registered with the apiserver. Cluster
//     operator must install the CRD.
//   - apierrors.IsInvalid / IsBadRequest / IsMethodNotSupported —
//     schema-level rejections (a required field is missing, the CRD
//     installed has a tighter validation than the operator expects,
//     an admission webhook rejected the write, the apiserver doesn't
//     accept the requested verb on this resource). Retry can't reshape
//     the payload; the operator must rebuild the artifact.
//
// Callers (Fetcher classification, library resolution, Publisher
// upserts, tenantClient build) route these to non-transient failure
// paths so the workqueue doesn't burn cycles on permanently-failing
// snippets. Every other API error (NotFound, Conflict, transient
// transport failures) stays in the default "engage backoff" path.
//
// Caveat: `apierrors.IsForbidden` keys off HTTP 403, which in Kubernetes
// practice is always RBAC (quota → 429, admission webhook rejection
// → 422). Degraded clusters can theoretically surface 403 for non-RBAC
// reasons (etcd-level errors, webhook timeouts proxied as 403 by
// pathological setups). Those misclassify as "permanent" and stop
// retrying; the snippet's status reflects the underlying error
// verbatim and the next genuine watch event re-triggers a reconcile.
// Impact in practice is tiny — this hasn't been observed in real
// clusters — but worth knowing when investigating unusual 403s.
func isPermanentAPIError(err error) bool {
	return apierrors.IsForbidden(err) ||
		apimeta.IsNoMatchError(err) ||
		runtime.IsNotRegisteredError(err) ||
		apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) ||
		apierrors.IsMethodNotSupported(err)
}

// rbacDenialMessage builds the user-facing message for a permanent
// API error. The Forbidden path quotes the apiserver's verbatim
// error (which names the SA, verb, and resource); decorateMessage
// appends the rbacdenied runbook link with the remediation steps. The
// NoMatchError path names the missing kind so the operator knows which
// CRD to install.
// The Invalid / BadRequest / MethodNotSupported paths name the
// schema or verb mismatch — the operator must rebuild the artifact or
// adjust spec, not retry.
func rbacDenialMessage(context string, err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return "RBAC denied " + context + " — grant the tenant ServiceAccount the missing verb. " + err.Error()
	case apimeta.IsNoMatchError(err), runtime.IsNotRegisteredError(err):
		return context + " refers to a kind not registered with the apiserver — install the corresponding CRD. " + err.Error()
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return "apiserver rejected " + context + " — the payload violates the CRD's validation or an admission webhook's policy; retry won't help. " + err.Error()
	case apierrors.IsMethodNotSupported(err):
		return "apiserver does not support the requested verb on " + context + " — the resource may be deprecated or the operator is talking to an unexpected schema version. " + err.Error()
	default:
		return context + ": " + err.Error()
	}
}

// classifyFetchError maps a sources.Fetcher error to a Reason+message
// plus a `transient` flag the caller uses to decide whether to engage
// controller-runtime's exponential backoff (transient) or treat the
// failure as steady-state (non-transient — the next watch event drives
// the next reconcile).
//
// Classification ladder, in order of precedence:
//
//   - apiserver-level RBAC denial (apierrors.IsForbidden) is
//     NON-transient. Retry can't grant a verb; the cluster operator
//     must update the chart's ClusterRole or the tenant's RoleBinding.
//     Message names "RBAC denied" so kubectl describe surfaces it,
//     with the rbacdenied runbook link appended by decorateMessage.
//   - CRD not installed (meta.IsNoMatchError) is NON-transient. The
//     cluster operator must install the CRD (typically Flux's
//     source-controller). Message names the missing kind.
//   - Integrity errors (digest mismatch, malformed digest declaration)
//     are NON-transient. A retry can't fix corruption; the upstream
//     must republish or the buggy CRD must emit a parseable digest.
//   - SSRF-defence errors (urlguard sentinels) are NON-transient. The
//     same URL would be rejected the same way next time.
//   - Tarball-shape errors (oversized archive, path traversal) are
//     NON-transient. The upstream must shrink / sanitize.
//   - Source-not-ready / artifact-not-yet-published are TRANSIENT.
//     Source-controller is still catching up; backoff lets us retry
//     on schedule rather than waiting for the next watch tick.
//   - Everything else (network / 5xx / unclassified HTTP) is
//     TRANSIENT by default. Better to burn a few retries on a
//     misclassified perma-failure than silently swallow a real
//     transient one — the workqueue's backoff caps the cost.
func classifyFetchError(err error) (reason, msg string, transient bool) {
	switch {
	case isPermanentAPIError(err):
		return ReasonRBACDenied, rbacDenialMessage("reading the source CR", err), false
	case errors.Is(err, sources.ErrDigestMismatch), errors.Is(err, sources.ErrDigestInvalid):
		return ReasonSourceFetchFailed, err.Error(), false
	case errors.Is(err, sources.ErrArtifactNotFound):
		// Permanent 4xx on the artifact body (404/403/...). The upstream
		// must re-publish or fix serving; retry can't help.
		return ReasonSourceFetchFailed, err.Error(), false
	case errors.Is(err, urlguard.ErrForbiddenHost),
		errors.Is(err, urlguard.ErrInvalidScheme),
		errors.Is(err, urlguard.ErrParseFailed),
		errors.Is(err, urlguard.ErrMissingHost):
		return ReasonSourceFetchFailed, err.Error(), false
	case errors.Is(err, sources.ErrArtifactBodyTooLarge),
		errors.Is(err, sources.ErrTarballTooLarge),
		errors.Is(err, sources.ErrTarEntryTooLarge),
		errors.Is(err, sources.ErrDecompressedTooLarge):
		// Tarball-shape errors are non-transient — the upstream
		// must shrink / sanitize / re-publish. Without these arms the
		// errors fell through to the default transient branch and
		// pinned the snippet in an apiserver-burning retry loop.
		return ReasonSourceFetchFailed, err.Error(), false
	case errors.Is(err, sources.ErrSourceNotReady), errors.Is(err, sources.ErrArtifactMissing):
		return ReasonSourceNotReady, err.Error(), true
	default:
		return ReasonSourceFetchFailed, err.Error(), true
	}
}

// evaluate builds the Importer (self files + libraries) and runs the entry
// snippet through the eval package. Returns (rendered, "", "", nil) on
// success, ("", reason, msg, nil) on a classifiable Jsonnet failure, or
// ("", "", "", err) only on truly transient errors (none today).
func (r *SnippetReconciler) evaluate(ctx context.Context, snip *jaasv1.JsonnetSnippet, selfFiles map[string]string, mainSource string, libs map[string]eval.Library) (string, string, string, error) {
	imp := &eval.InMemoryImporter{
		Self:      eval.Library{Files: selfFiles},
		Libraries: libs,
	}
	merged := mergeExtVars(r.ExtVars, snip.Spec.ExternalVariables)
	entryLabel := snip.Spec.EntryFile
	if entryLabel == "" {
		entryLabel = EntryFileName
	}
	rendered, err := eval.EvaluateAnonymousSnippet(ctx, snip.Namespace+"/"+snip.Name+"/"+entryLabel, mainSource, eval.Options{
		ExtVars:  merged,
		TLAs:     snip.Spec.TLAs,
		MaxStack: r.MaxStack,
		Timeout:  r.EvaluationTimeout,
		Importer: imp,
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "", ReasonEvaluationTimeout,
			fmt.Sprintf("evaluation exceeded %s", r.EvaluationTimeout), nil
	case errors.Is(err, context.Canceled):
		// Caller's ctx was canceled (manager shutting down). Surface so the
		// queue requeues; not a steady-state status.
		return "", "", "", err
	case errors.Is(err, eval.ErrEvalUnavailable):
		// Concurrent-eval cap full — surface so the reconciler can apply
		// backpressure (RequeueAfter + event + metric) instead of marking
		// the snippet failed. Transient by definition.
		return "", "", "", err
	case err != nil:
		return "", ReasonEvaluationFailed, err.Error(), nil
	}
	return rendered, "", "", nil
}

// mergeExtVars layers the snippet's CR-level vars over the operator's set.
// checkExtVarConflicts has already rejected overlapping keys, so the result
// is just the union.
func mergeExtVars(opLevel, snipLevel map[string]string) map[string]string {
	if len(opLevel) == 0 && len(snipLevel) == 0 {
		return nil
	}
	out := make(map[string]string, len(opLevel)+len(snipLevel))
	for k, v := range opLevel {
		out[k] = v
	}
	for k, v := range snipLevel {
		out[k] = v
	}
	return out
}

// failReady writes Ready=False with the given reason+message and returns
// without requeuing — the next watch event drives the next reconcile.
//
// The status write goes through the Flux patch.Helper: the Ready condition
// is patched via the helper's internal re-Get + optimistic-lock backoff loop,
// so a Conflict (a sibling controller or a manual kubectl edit bumping
// resourceVersion) is resolved by re-applying the condition diff to the
// latest object rather than bubbling up and forcing controller-runtime to
// redo the whole reconcile. The non-condition status fields
// (ObservedGeneration) merge-patch without a resourceVersion precondition,
// so they can't conflict.
func (r *SnippetReconciler) failReady(ctx context.Context, snip *jaasv1.JsonnetSnippet, reason, message string) (ctrl.Result, error) {
	// FindStatusCondition returns a pointer INTO snip.Status.Conditions, and the
	// fluxconditions.Set below updates that same element in place. Snapshot the
	// prior status+reason by value first, so emitConditionEvent's dedup compares
	// against the real previous condition rather than the one we just wrote —
	// otherwise a transition into a new reason (e.g. Synced -> Suspended) reads
	// prev as already-equal and suppresses the event.
	var prev *metav1.Condition
	if cur := apimeta.FindStatusCondition(snip.Status.Conditions, jaasv1.ConditionReady); cur != nil {
		snapshot := *cur
		prev = &snapshot
	}
	decorated := r.decorateMessage(reason, message)
	helper, err := fluxpatch.NewHelper(snip, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	snip.Status.ObservedGeneration = snip.Generation
	// Acknowledge the reconcile.fluxcd.io/requestedAt token even on a failure
	// so `flux reconcile` (and any controller polling status.lastHandledReconcileAt)
	// detects completion. Without this, a snippet stuck on a terminal reason
	// (RBAC denied, invalid spec, source fetch failed) never acknowledges a
	// manual `flux reconcile`, which is exactly when an operator is most likely
	// to issue one — the CLI would report a timeout though the controller acted.
	snip.Status.LastHandledReconcileAt = snip.Annotations[ReconcileRequestAnnotation]
	fluxconditions.Set(snip, &metav1.Condition{
		Type:    jaasv1.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: decorated,
	})
	if err := helper.Patch(ctx, snip, fluxpatch.WithOwnedConditions{Conditions: []string{jaasv1.ConditionReady}}); err != nil {
		return ctrl.Result{}, err
	}
	r.emitConditionEvent(snip, prev, metav1.ConditionFalse, reason, message)
	recordReconcileOutcome(snip.Namespace, snip.Name, string(metav1.ConditionFalse), reason)
	// Intentional/healthy states (Suspended, Pending) are not failures awaiting
	// out-of-band repair — a spec edit (unsuspend) or the next watch event
	// drives them, so they get no self-heal requeue. Without this a suspended
	// snippet would wake every minute forever, re-running the GC pass and
	// bumping the reconcile-total metric, contradicting "paused".
	if happyReasonsNoRunbook[reason] {
		return ctrl.Result{}, nil
	}
	// Terminal failures don't engage controller-runtime's error backoff
	// (we return nil error so the queue doesn't spin). Several of them —
	// RBAC denied, missing CRD, a library fixed in another namespace —
	// heal out-of-band without producing a watch event this snippet sees,
	// so a bounded RequeueAfter re-checks roughly once a minute. The caller
	// that returns failReady's result on the transient-error path still
	// returns the error itself for backoff; that path is unaffected because
	// it discards this Result.
	return ctrl.Result{RequeueAfter: permanentRetryInterval}, nil
}

// happyReasonsNoRunbook names Ready Reason values that describe a
// healthy or intentionally-operator-set state and therefore do NOT
// carry a runbook link. Synced is the happy path; Suspended is the
// "operator paused this on purpose" state — neither has a remediation
// page because there's nothing to remediate. Pending fires once at
// snippet creation before the first reconcile finishes — also a
// healthy transition state.
var happyReasonsNoRunbook = map[string]bool{
	ReasonSynced:    true,
	ReasonSuspended: true,
	ReasonPending:   true,
}

// RunbookBaseURL is the fixed location of the per-reason remediation pages on the JaaS docs site. Pages live at <base><reason-lowercased>/.
const RunbookBaseURL = "https://jaas.projects.metio.wtf/runbooks/"

// runbookBaseURL is the unexported alias used internally; RunbookBaseURL is
// exported so other surfaces (the MCP server) can build the same links without
// importing the operator package's heavier internals.
const runbookBaseURL = RunbookBaseURL

// decorateMessage appends a "(runbook: <url>)" suffix so kubectl describe
// surfaces a direct link to the per-reason remediation page. The reason is
// lower-cased and appended to runbookBaseURL as a path segment ending in "/".
//
// Reasons in happyReasonsNoRunbook (Synced, Suspended, Pending) get
// no suffix — those states are healthy or intentional, not actionable.
func (r *SnippetReconciler) decorateMessage(reason, message string) string {
	if happyReasonsNoRunbook[reason] {
		return message
	}
	return message + " (runbook: " + runbookBaseURL + strings.ToLower(reason) + "/)"
}

// markSynced records the successful render via Status.Revision and flips
// Ready=True/Synced. The revision string comes from the publisher (or the
// reconciler's local hash when no publisher is wired).
//
// Status.History tracks the last spec.History revisions (default 1).
// When the publish produced a revision identical to the current head,
// we don't add a duplicate entry — the timestamp moves via the
// existing entry's Time staying as-was. Otherwise we prepend the new
// entry and truncate to spec.History.
//
// judgedGen extends the pre-publish staleness gate into the status
// retry: the statusretry helper re-Gets the snippet on every Conflict
// retry, and the publish + retry window is long enough for a tenant
// spec edit to land. Stamping this reconcile's revision against the
// freshly-edited generation would mark a stale render as the
// up-to-date state. When the re-Get sees a moved generation, the
// mutate fn is a no-op — the next reconcile (already enqueued by the
// spec-edit watch event) writes the right pair.
func (r *SnippetReconciler) markSynced(ctx context.Context, snip *jaasv1.JsonnetSnippet, rendered, revision, artifactURL string, sourceFileCount int, judgedGen int64) (ctrl.Result, error) {
	prev := apimeta.FindStatusCondition(snip.Status.Conditions, jaasv1.ConditionReady)
	// sourceFileCount is the resolved source's file count — same value
	// for inline spec.files and for sourceRef-fetched content. Using
	// len(snip.Spec.Files) would report 0 for every sourceRef snippet.
	msg := fmt.Sprintf("Rendered %d bytes from %d files", len(rendered), sourceFileCount)
	history := snip.Spec.History
	now := r.now()
	syncTime := metav1.NewTime(now)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	// Staleness gate: a re-read confirms the spec we rendered is still
	// current before stamping its revision. Generation is monotonic, so
	// a moved generation means a spec edit landed during the publish window;
	// stamping this render's revision against the edited generation would mark
	// a stale render as up-to-date. The spec-edit watch event has already
	// enqueued the next reconcile against the fresh spec, so skipping the
	// write here (and the event / metric / requeue below) is correct — the
	// stale reconcile leaves no trail.
	//
	// The re-read goes through APIReader (uncached) for the same reason
	// publishConsistencyGate does: the manager's cache can lag the apiserver
	// by tens of milliseconds, long enough for a fast spec-edit to be missed,
	// so a cached read here would defeat the gate it backs.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var latest jaasv1.JsonnetSnippet
	if err := reader.Get(ctx, key, &latest); err != nil {
		return ctrl.Result{}, err
	}
	if latest.Generation != judgedGen {
		return ctrl.Result{}, nil
	}

	// Patch the status onto the freshly-read object via the Flux patch.Helper.
	// The Ready condition is patched through the helper's internal re-Get +
	// optimistic-lock retry loop (conflict-safe against sibling controllers);
	// the plain status fields merge-patch without a resourceVersion
	// precondition, so they can't conflict.
	helper, err := fluxpatch.NewHelper(&latest, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	latest.Status.Revision = revision
	// artifactURL is empty when the publisher is unwired (eval-only mode,
	// tests). Only overwrite the status field when we have a real URL —
	// preserves the last-known-good URL across eval-only reconciles.
	if artifactURL != "" {
		latest.Status.ArtifactURL = artifactURL
	}
	latest.Status.ObservedGeneration = latest.Generation
	latest.Status.LastSyncTime = &syncTime
	// Record that this reconcile handled the current
	// reconcile.fluxcd.io/requestedAt token so `flux reconcile` can detect
	// completion. The ReconcileRequestedPredicate fired on this token, so the
	// freshly-read object carries it.
	latest.Status.LastHandledReconcileAt = latest.Annotations[ReconcileRequestAnnotation]
	latest.Status.History = updateRevisionHistory(latest.Status.History, revision, history, now)
	fluxconditions.Set(&latest, &metav1.Condition{
		Type:    jaasv1.ConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonSynced,
		Message: msg,
	})
	if err := helper.Patch(ctx, &latest, fluxpatch.WithOwnedConditions{Conditions: []string{jaasv1.ConditionReady}}); err != nil {
		return ctrl.Result{}, err
	}
	r.emitConditionEvent(snip, prev, metav1.ConditionTrue, ReasonSynced, msg)
	recordReconcileOutcome(snip.Namespace, snip.Name, string(metav1.ConditionTrue), ReasonSynced)
	recordRenderedBytes(snip.Namespace, snip.Name, len(rendered))
	// If spec.interval is set, schedule a steady-state re-reconcile.
	// Watch events (snippet update, library change, Flux source flip)
	// also wake the snippet up — interval is the floor, not the
	// ceiling.
	if snip.Spec.Interval != nil && snip.Spec.Interval.Duration > 0 {
		// Jitter the steady-state requeue so a fleet of snippets configured
		// with the same interval doesn't thunder-herd the reconciler (and the
		// upstream sources) on a shared deadline. The configured interval is
		// the floor/centre; jitter only spreads the wakeups around it. With
		// jitter uninitialised (percentage 0) JitteredRequeueInterval is the
		// identity, so the interval semantics are unchanged.
		return jitter.JitteredRequeueInterval(ctrl.Result{RequeueAfter: snip.Spec.Interval.Duration}), nil
	}
	return ctrl.Result{}, nil
}

// emitConditionEvent records an Event on the JsonnetSnippet whenever
// the Ready condition transitions (status flips or reason changes).
// notification-controller's Alert CRs key off this Event stream — see
// the runbook README for the Slack/Webhook routing pattern.
//
// Severity policy: Synced → Normal, everything else → Warning. Suppress
// noisy duplicates by skipping when both Status and Reason match prev.
//
// The events.v1 API takes a separate `action` parameter. We pass the
// Reason for both reason and action since a transition into a given
// Reason IS the action — keeps the wire payload consistent with what
// notification-controller's older API consumers expected.
func (r *SnippetReconciler) emitConditionEvent(snip *jaasv1.JsonnetSnippet, prev *metav1.Condition, status metav1.ConditionStatus, reason, message string) {
	if r.EventRecorder == nil {
		return
	}
	if prev != nil && prev.Status == status && prev.Reason == reason {
		return
	}
	eventType := corev1.EventTypeWarning
	if reason == ReasonSynced {
		eventType = corev1.EventTypeNormal
	}
	r.EventRecorder.Eventf(snip, nil, eventType, reason, reason, "%s", message)
}

// now returns r.Clock() when set, time.Now() otherwise. Tests inject a
// fake clock to pin RevisionEntry timestamps deterministically.
func (r *SnippetReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// updateRevisionHistory prepends revision to prior, truncates to
// historyMax (clamped to >=1), and dedups consecutive identical heads.
// A re-publish of the same content produces the same revision string —
// in that case the existing head stays at its original timestamp
// rather than rewriting it on every reconcile.
func updateRevisionHistory(prior []jaasv1.RevisionEntry, revision string, historyMax int32, now time.Time) []jaasv1.RevisionEntry {
	limit := int(historyMax)
	if limit < 1 {
		limit = 1
	}
	if len(prior) > 0 && prior[0].Revision == revision {
		// Same head — leave the original timestamp in place, just
		// truncate in case the limit shrank.
		if len(prior) > limit {
			return append([]jaasv1.RevisionEntry(nil), prior[:limit]...)
		}
		return prior
	}
	out := make([]jaasv1.RevisionEntry, 0, limit)
	out = append(out, jaasv1.RevisionEntry{Revision: revision, Time: metav1.NewTime(now)})
	for _, e := range prior {
		if len(out) >= limit {
			break
		}
		out = append(out, e)
	}
	return out
}

// FluxSourceKinds are the Flux source-controller kinds the reconciler
// re-reconciles snippets against. ExternalArtifact is in this set so
// chained snippets re-render when an upstream snippet republishes; the
// cycle detector (detectSourceRefCycle) prevents the publish → watch →
// reconcile → publish loop a sourceRef cycle would otherwise create.
//
// Drift gate: the chart's ClusterRole must grant `get` on every kind
// here or the Fetcher's first sourceRef lookup fails with Forbidden.
// The chart — and its ClusterRole drift-gate test — lives in the
// metio/helm-charts repo; when a new kind is added here, update the
// chart's source-CR rule and that test there in the same change.
var FluxSourceKinds = []string{"GitRepository", "OCIRepository", "Bucket", "ExternalArtifact"}

// MissingFluxSourceKinds returns a snapshot of the Flux source kinds
// that aren't installed in the cluster yet. Populated by
// SetupWithManager and pruned by EngageFluxWatch as the crdWatcher
// engages dynamic watches over time.
//
// Returns a defensive copy so callers can iterate freely without
// holding the lock.
func (r *SnippetReconciler) MissingFluxSourceKinds() []schema.GroupVersionKind {
	r.missingMu.RLock()
	defer r.missingMu.RUnlock()
	out := make([]schema.GroupVersionKind, len(r.missingFluxKinds))
	copy(out, r.missingFluxKinds)
	return out
}

// SetupWithManager registers this reconciler with mgr, watching JsonnetSnippet
// objects plus three secondary kinds whose changes affect a snippet's
// rendered output:
//
//   - JsonnetLibrary — a referenced library's bytes change → re-render
//     every snippet that imports it.
//   - Flux source CRs (GitRepository, OCIRepository, Bucket,
//     ExternalArtifact) — a referenced source's status.artifact flips →
//     re-fetch and re-render every snippet whose spec.sourceRef points at
//     it. The Flux watches are gated on the RESTMapper resolving each GVK
//     so the operator boots cleanly in clusters without source-controller
//     installed; missing kinds are accumulated on the reconciler so a
//     crdPoller can pick them up.
func (r *SnippetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Seed the process-global interval jitter so steady-state requeues
	// (markSynced's spec.interval RequeueAfter) get spread around their
	// configured deadline. SetGlobalIntervalJitter is sync.Once-guarded, so
	// repeated SetupWithManager calls (multiple test cases in one binary) are
	// safe and only the first takes effect. nil rand picks a time-seeded one.
	jitter.SetGlobalIntervalJitter(defaultIntervalJitterFraction, nil)
	if err := registerWatchIndexes(context.Background(), mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("register watch indexes: %w", err)
	}
	b := ctrl.NewControllerManagedBy(mgr).
		Named("jsonnetsnippet").
		For(&jaasv1.JsonnetSnippet{}, crbuilder.WithPredicates(
			// Wake on a spec change (generation bump) OR a fresh
			// reconcile.fluxcd.io/requestedAt token. Filtering out the
			// status-only updates the reconciler writes itself keeps the
			// workqueue from churning on its own condition/observedGeneration
			// stamps; spec.interval (jittered RequeueAfter) drives the
			// steady-state re-render, and library / Flux-source watches drive
			// dependency-triggered re-renders.
			predicate.Or(
				predicate.GenerationChangedPredicate{},
				fluxpredicates.ReconcileRequestedPredicate{},
			),
		)).
		Watches(&jaasv1.JsonnetLibrary{}, handler.EnqueueRequestsFromMapFunc(r.mapJsonnetLibrary))

	mapper := mgr.GetRESTMapper()
	// Force a fresh discovery before checking — controller-runtime's
	// DeferredDiscoveryRESTMapper caches misses, so a CRD installed just
	// before SetupWithManager runs (envtest, helm install with Flux side
	// by side) would be reported missing without a Reset.
	if resetter, ok := mapper.(interface{ Reset() }); ok {
		resetter.Reset()
	}
	if err := ensureOwnCRDsInstalled(mapper); err != nil {
		return err
	}
	r.missingMu.Lock()
	r.missingFluxKinds = nil
	for _, kind := range FluxSourceKinds {
		gvk := fluxSourceGVK(kind)
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			r.logger().Info("Skipping watch on Flux source kind not installed in cluster",
				slog.String("gvk", gvk.String()))
			r.missingFluxKinds = append(r.missingFluxKinds, gvk)
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		b = b.Watches(obj, handler.EnqueueRequestsFromMapFunc(r.mapFluxSource))
	}
	r.missingMu.Unlock()
	c, err := b.Build(r)
	if err != nil {
		return err
	}
	r.controller = c
	r.mgrCache = mgr.GetCache()
	return nil
}

// EngageFluxWatch wires a new Flux source kind into the live controller.
// Called by the crdWatcher when a previously-missing CRD becomes
// Established. The newly-installed kind's initial-list events fan out
// through mapFluxSource into snippets that reference any instance, so
// dependents get retried automatically — no process restart, no
// manual nudge needed.
//
// Idempotent: re-engaging an already-watched GVK is a no-op (the
// source already exists in the controller's source list, and the
// underlying informer is shared per cache).
func (r *SnippetReconciler) EngageFluxWatch(ctx context.Context, gvk schema.GroupVersionKind) error {
	if r.controller == nil {
		return fmt.Errorf("engageFluxWatch called before SetupWithManager")
	}
	if r.mgrCache == nil {
		return fmt.Errorf("engageFluxWatch: nil cache")
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	src := source.Kind(r.mgrCache, client.Object(obj),
		handler.EnqueueRequestsFromMapFunc(r.mapFluxSource))
	if err := r.controller.Watch(src); err != nil {
		return fmt.Errorf("engageFluxWatch %s: %w", gvk.String(), err)
	}
	// Drop the kind from missingFluxKinds so the crdWatcher stops
	// caring about it. DeleteFunc preserves order for stable logs.
	r.missingMu.Lock()
	r.missingFluxKinds = slices.DeleteFunc(r.missingFluxKinds, func(k schema.GroupVersionKind) bool { return k == gvk })
	r.missingMu.Unlock()
	r.logger().InfoContext(ctx, "Engaged dynamic watch on newly-installed Flux source kind",
		slog.String("gvk", gvk.String()))
	return nil
}

// ensureOwnCRDsInstalled fails fast when the operator's own CRDs are absent.
// The Flux source kinds are optional — watched only when installed — but the
// operator cannot do anything without JsonnetSnippet/JsonnetLibrary:
// controller-runtime would otherwise spin indefinitely logging "failed to
// watch" with no actionable hint, and the pod can still report Ready. Naming
// the missing kind and the remedy turns that silent hang into a clear startup
// failure.
func ensureOwnCRDsInstalled(mapper apimeta.RESTMapper) error {
	for _, gvk := range []schema.GroupVersionKind{
		jaasv1.GroupVersion.WithKind("JsonnetSnippet"),
		jaasv1.GroupVersion.WithKind("JsonnetLibrary"),
	} {
		if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
			return fmt.Errorf("%s CRD (%s) is not installed — apply the JaaS CRDs (config/crd/bases/, or the Helm chart's CRDs) before starting the operator: %w", gvk.Kind, gvk.GroupVersion().String(), err)
		}
	}
	return nil
}

func fluxSourceGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   fluxSourceGroup,
		Version: fluxSourceVersion,
		Kind:    kind,
	}
}

// mapJsonnetLibrary translates an event on a JsonnetLibrary into the list
// of snippets that import it. Backed by the snippetByLibraryRefIndex
// field indexer: a library event hits an O(K) lookup over the K snippets
// that name the library directly, instead of a cluster-wide List.
func (r *SnippetReconciler) mapJsonnetLibrary(ctx context.Context, obj client.Object) []reconcile.Request {
	lib, ok := obj.(*jaasv1.JsonnetLibrary)
	if !ok {
		return nil
	}
	key := libID("JsonnetLibrary", lib.Namespace, lib.Name)
	var list jaasv1.JsonnetSnippetList
	if err := r.Client.List(ctx, &list, client.MatchingFields{snippetByLibraryRefIndex: key}); err != nil {
		r.logger().Error("List JsonnetSnippet for library watch", slog.Any("error", err))
		return nil
	}
	if len(list.Items) == 0 {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, snip := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
		})
		r.CycleCache.Forget(snip.UID)
	}
	return reqs
}

// mapFluxSource translates an event on a Flux source CR (an Unstructured)
// into the list of snippets that reference it — either directly via
// spec.sourceRef, or indirectly via a JsonnetLibrary whose own
// spec.sourceRef points at this source.
//
// Backed by two field indexers:
//
//   - snippetBySourceRefIndex covers the direct edge.
//   - libraryBySourceRefIndex covers the indirect edge: libraries whose
//     own sourceRef points at this object are looked up directly, then
//     each matching library funnels through snippetByLibraryRefIndex to
//     reach the dependent snippets.
//
// Each step is an O(K) cache lookup over the K objects that name this
// specific source — replacing two cluster-wide Lists with three indexed
// lookups.
func (r *SnippetReconciler) mapFluxSource(ctx context.Context, obj client.Object) []reconcile.Request {
	gvk := obj.GetObjectKind().GroupVersionKind()
	srcName := obj.GetName()
	srcNs := obj.GetNamespace()
	directKey := sourceRefIndexKey(gvk.Kind, srcNs, srcName)

	matched := map[types.NamespacedName]types.UID{}

	var direct jaasv1.JsonnetSnippetList
	if err := r.Client.List(ctx, &direct, client.MatchingFields{snippetBySourceRefIndex: directKey}); err != nil {
		r.logger().Error("List JsonnetSnippet (direct) for Flux source watch", slog.Any("error", err))
		return nil
	}
	for _, snip := range direct.Items {
		key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
		matched[key] = snip.UID
	}

	var libs jaasv1.JsonnetLibraryList
	if err := r.Client.List(ctx, &libs, client.MatchingFields{libraryBySourceRefIndex: directKey}); err != nil {
		// Bias toward over-enqueue: the direct-edge `matched` map already
		// holds the snippets that reference this source via spec.sourceRef.
		// Discarding them on a transient library-list failure would
		// silently drop their re-render until the source emits its next
		// event (which could be its next periodic resync — minutes for
		// some Flux source kinds). The indirect-edge contribution is
		// lost for this event; the next event picks it up.
		r.logger().Error("List JsonnetLibrary for Flux source watch; indirect edge skipped",
			slog.Any("error", err))
		libs.Items = nil
	}
	for _, lib := range libs.Items {
		libKey := libID("JsonnetLibrary", lib.Namespace, lib.Name)
		var indirect jaasv1.JsonnetSnippetList
		if err := r.Client.List(ctx, &indirect, client.MatchingFields{snippetByLibraryRefIndex: libKey}); err != nil {
			r.logger().Error("List JsonnetSnippet (indirect) for Flux source watch", slog.Any("error", err))
			continue
		}
		for _, snip := range indirect.Items {
			key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
			matched[key] = snip.UID
		}
	}

	if len(matched) == 0 {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(matched))
	for k, uid := range matched {
		reqs = append(reqs, reconcile.Request{NamespacedName: k})
		r.CycleCache.Forget(uid)
	}
	return reqs
}

// libID is the (kind, namespace, name) identity used to look up a library
// in the per-event index.
func libID(kind, namespace, name string) string {
	return kind + "|" + namespace + "|" + name
}

// SourceFetcher is the small interface SnippetReconciler depends on for
// resolving spec.sourceRef. *sources.Fetcher (production) and test stubs
// satisfy it; the indirection lets reconciler tests inject failure shapes
// without standing up an HTTP server.
type SourceFetcher interface {
	Fetch(ctx context.Context, c client.Client, ref *jaasv1.SourceRef, ownerNs string) (*sources.Result, error)
}

// crossNamespaceSourceRef returns ("", "") when the SourceRef may proceed —
// either --no-cross-namespace-refs is off, the ref is nil, the ref's
// Namespace is empty (defaults to the owner's), or the namespaces match.
// Otherwise returns (ReasonCrossNamespaceRefRejected, message) so the caller
// can plumb it onto the status condition.
//
// The "owner" namespace for every check is the JsonnetSnippet's namespace —
// the snippet is the authority for tenancy decisions.
func (r *SnippetReconciler) crossNamespaceSourceRef(ownerNs string, ref *jaasv1.SourceRef) (string, string) {
	if !r.NoCrossNamespaceRefs || ref == nil {
		return "", ""
	}
	ns := ref.Namespace
	if ns == "" || ns == ownerNs {
		return "", ""
	}
	return ReasonCrossNamespaceRefRejected,
		fmt.Sprintf("spec.sourceRef points at namespace %q but --no-cross-namespace-refs is set", ns)
}

func (r *SnippetReconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// effectiveServiceAccount picks spec.serviceAccountName when set, otherwise
// falls back to the operator's --default-service-account.
// checkServiceAccount guards against the empty-empty case before we get here.
func (r *SnippetReconciler) effectiveServiceAccount(snip *jaasv1.JsonnetSnippet) string {
	if snip.Spec.ServiceAccountName != "" {
		return snip.Spec.ServiceAccountName
	}
	return r.DefaultServiceAccount
}

// isLastSnippetUsingSA reports whether the snippet about to be GC'd
// (named excludeName, in namespace ns) is the last live JsonnetSnippet
// in the namespace that resolves to sa as its effective ServiceAccount.
// Used to decide whether evicting the cached SA token in reconcileDelete
// would punish any other still-active snippet.
//
// Snippets carrying a non-zero DeletionTimestamp are treated as already
// gone — they're walking the same finalizer path and their tokens are
// about to be Forgotten as well.
//
// On List failure the caller skips the eviction conservatively; the
// token's natural TTL takes over. A stale cache entry costs at most one
// extra TokenRequest on the next reconcile that uses the SA.
func (r *SnippetReconciler) isLastSnippetUsingSA(ctx context.Context, ns, sa, excludeName string) (bool, error) {
	var list jaasv1.JsonnetSnippetList
	if err := r.Client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return false, err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == excludeName {
			continue
		}
		if !other.DeletionTimestamp.IsZero() {
			continue
		}
		if r.effectiveServiceAccount(other) == sa {
			return false, nil
		}
	}
	return true, nil
}

// tenantClient returns a controller-runtime client whose every API call is
// authenticated as the snippet's effective ServiceAccount. The TokenCache
// mints (or reuses) a Bearer token via the SA TokenRequest API; we stamp
// that token onto a clone of the manager's RestConfig.
//
// RestConfig nil (or TokenCache nil) returns the manager's Client unchanged
// — fake-client tests don't model TokenRequest. With both wired, the
// ClientCache memoizes the constructed client per (namespace, SA) keyed by
// the token it was built with: while the token is unchanged, every reconcile
// reuses the same client and skips client.New's RESTMapper + transport
// construction. A new token replaces the cached entry.
func (r *SnippetReconciler) tenantClient(ctx context.Context, snip *jaasv1.JsonnetSnippet) (client.Client, error) {
	if r.RestConfig == nil || r.TokenCache == nil {
		return r.Client, nil
	}
	sa := r.effectiveServiceAccount(snip)
	if sa == "" {
		return r.Client, nil
	}
	token, err := r.TokenCache.Token(ctx, snip.Namespace, sa)
	if err != nil {
		return nil, err
	}
	cacheKey := snip.Namespace + "/" + sa
	cached, epoch, ok := r.ClientCache.Get(cacheKey, token)
	if ok {
		return cached, nil
	}
	cfg := buildTenantRestConfig(r.RestConfig, token)
	c, err := client.New(cfg, client.Options{Scheme: r.Scheme})
	if err != nil {
		return nil, err
	}
	r.ClientCache.Put(cacheKey, token, epoch, c)
	return c, nil
}

// buildTenantRestConfig assembles the rest.Config for a tenant-scoped
// client. Extracted so the Insecure=false invariant can be tested
// without standing up a full apiserver: a dev kubeconfig carrying
// `insecure-skip-tls-verify: true` for the operator's own connection
// must not flow into tenant API calls — otherwise a compromised
// snippet could read tenant secrets over an unverified TLS session.
func buildTenantRestConfig(operatorCfg *rest.Config, token string) *rest.Config {
	cfg := rest.AnonymousClientConfig(operatorCfg)
	cfg.BearerToken = token
	cfg.TLSClientConfig.CAData = operatorCfg.CAData
	cfg.TLSClientConfig.CAFile = operatorCfg.CAFile
	cfg.TLSClientConfig.ServerName = operatorCfg.ServerName
	cfg.TLSClientConfig.Insecure = false
	return cfg
}
