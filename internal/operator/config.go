/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package operator implements the controller-runtime-based reconciler half of
// JaaS, activated by --enable-flux-integration. The HTTP-only evaluator path
// does not import this package; it stays compiled-out unless the flag is set.
package operator

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/metio/jaas/internal/eval"
	"github.com/metio/jaas/internal/storage"
)

// Config groups every operator-facing CLI flag in one struct so main.go can
// pass it to Run as a single argument and tests can build it without flag
// parsing.
type Config struct {
	// DefaultServiceAccount names the ServiceAccount the operator
	// impersonates when a JsonnetSnippet does not set
	// spec.serviceAccountName. Empty means snippets without an explicit SA
	// are rejected.
	DefaultServiceAccount string

	// NoCrossNamespaceRefs mirrors Flux's --no-cross-namespace-refs:
	// when true (the default in Phase 1B's main.go wiring), a snippet
	// referencing a SourceRef in a different namespace is rejected.
	NoCrossNamespaceRefs bool

	// LabelSelector narrows the operator's watch to objects matching this
	// selector. Empty selects all objects in the watched scope, matching
	// controller-runtime's default.
	LabelSelector string

	// WatchNamespaces restricts the manager's cache to the listed
	// namespaces (Cache.DefaultNamespaces). Empty (the default) means
	// cluster-wide watch — the historical behaviour. When set, the
	// cache only observes CRs in these namespaces; CRs outside are
	// invisible to the reconciler even when the SA's RBAC would
	// otherwise grant access. Paired with per-namespace RoleBindings
	// in the chart, this is the multi-tenant operator-instances
	// pattern: one operator per tenant-group, disjoint watch sets.
	//
	// Each entry must be a valid Kubernetes namespace (DNS-1123
	// label). main.go validates the list at flag-parse time and
	// rejects an invalid entry with rc=2.
	WatchNamespaces []string

	// KnownLibraryAliases enumerates the OCI-mounted library aliases
	// available cluster-wide (one per subdirectory of every -library-path).
	// The reconciler + admission webhook use these to reject snippets
	// whose spec.libraries[*].importPath shadows an OCI mount — the CR
	// would silently win and the operator would never consult the OCI
	// volume, surprising operators who set up the mount expecting it to
	// be used. Empty disables the check.
	//
	// Derived at startup from OCILibraries (its keys); kept separately
	// so the admission webhook validator can avoid the eval-package
	// dependency.
	KnownLibraryAliases []string

	// OCILibraries holds the byte contents of every OCI-mounted
	// library the operator was booted with, keyed by alias. The
	// reconciler merges these into the per-snippet library map after
	// the CR-based LibraryRef resolution so snippets can `import
	// "<alias>/file"` against operator-shipped shared libraries
	// without declaring a CR LibraryRef. CR aliases CAN'T collide
	// (admission rejects shadow attempts); the merge is purely
	// additive.
	OCILibraries map[string]eval.Library

	// RerenderRate is the steady-state per-snippet re-render budget, in
	// tokens per second. Parsed from --rerender-rate=N/period via
	// ParseRerenderRate.
	RerenderRate float64

	// RerenderBurst is the per-snippet token-bucket depth.
	RerenderBurst int

	// ExtVars is the operator-level std.extVar map. Keys here take
	// precedence over CR-level spec.externalVariables; conflicts are
	// rejected at admission, with a reconciler fallback that fails Ready.
	ExtVars map[string]string

	// EvaluationTimeout bounds a single reconcile's snippet eval; zero
	// disables the bound. Mirrors the HTTP path's --evaluation-timeout.
	EvaluationTimeout time.Duration

	// MaxStack overrides go-jsonnet's default call-stack depth; zero keeps
	// the default. Mirrors the HTTP path's --max-stack.
	MaxStack int

	// Store is the artifact backend the Publisher writes tarballs to.
	// Nil leaves the reconciler in eval-only mode (no ExternalArtifact
	// upserts) — useful for early bring-up and unit tests. In production
	// main.go wires either a *storage.Store (filesystem, single-pod) or
	// a *storage.S3Backend (object store, HA across replicas) depending
	// on -storage-backend.
	Store storage.Backend

	// StorageBaseURL is the public URL prefix the operator's storage HTTP
	// server serves tarballs from. Combined with Store.Path it forms each
	// artifact's status.artifact.url. Required when Store is set.
	StorageBaseURL string

	// MaxArtifactBytes caps the published content size in bytes — the
	// rendered output in Output=rendered mode, the whole source tree in
	// Output=source mode. A snippet over the cap fails with
	// ReasonArtifactTooLarge before any disk/S3 write. Zero disables.
	MaxArtifactBytes int64

	// ArtifactGCGrace is the minimum time a revision evicted from the
	// keep-set remains fetchable before storage GC removes it. Closes
	// the pin→fetch race in which a Flux consumer reads
	// status.artifact a moment before the operator garbage-collects the
	// superseded revision, then 404s on the dereference. Forwarded
	// verbatim to Publisher.GCGrace and on to storage.Backend.Prune;
	// zero restores the immediate-prune behavior. Default in main.go is
	// 5m — wide enough to cover steady-state consumer fetch latencies
	// (kustomize-controller, helm-controller, stageset-controller) while
	// keeping storage growth bounded.
	ArtifactGCGrace time.Duration

	// EnableWebhook opts into running the validating admission webhook for
	// JsonnetSnippet. The webhook checks operator-level ext-var conflicts
	// at admission time; the reconciler enforces the same invariant as a
	// fallback when the webhook is bypassed.
	EnableWebhook bool

	// WebhookCertDir tells controller-runtime where to find the TLS cert
	// and key for the webhook server. Provisioning of these files is the
	// helm chart's responsibility (cert-manager or an init container).
	WebhookCertDir string

	// WebhookPort is the port the webhook server binds to. Defaults to 9443
	// when unset.
	WebhookPort int

	// SkipControllerNameValidation disables controller-runtime's
	// once-per-process check that every controller name is unique. Only
	// the envtest harness sets this — main.go-driven invocations always
	// boot a single controller per process where the validation is a
	// useful safety net.
	SkipControllerNameValidation bool

	// SkipImpersonation makes the reconciler use the manager's own client
	// for tenant-side operations instead of building a per-snippet
	// impersonating client. Only the envtest harness sets this so its
	// reconcile tests can run without provisioning per-snippet SAs and
	// RBAC; production must keep the default (impersonation on) so a
	// compromised or buggy snippet can't reach beyond the tenant's SA
	// permissions.
	SkipImpersonation bool

	// LeaderElection toggles controller-runtime's leader-election lock.
	// When true the manager only runs reconcilers + cache + the webhook
	// while holding the lease at LeaderElectionNamespace/LeaderElectionID —
	// letting operators scale the Deployment without two replicas
	// double-reconciling the same JsonnetSnippet. Defaults on in main.go
	// when --enable-flux-integration is set; tests opt out via the
	// zero-value (false) to avoid the cost of a lease per envtest case.
	LeaderElection bool

	// LeaderElectionID is the Lease object's name. Multiple installations
	// in the same cluster must pick distinct IDs (helm uses the release
	// name) so they don't fight over a shared lease.
	LeaderElectionID string

	// LeaderElectionNamespace holds the Lease. main.go fills this from a
	// flag (typically the release namespace); empty falls back to
	// controller-runtime's downward-API discovery.
	LeaderElectionNamespace string

	// MaxWithdrawWait bounds how long a deleted JsonnetSnippet may
	// stay stuck in the finalizer while its Publisher.Withdraw keeps
	// failing. Past the bound the reconciler force-drops the
	// finalizer, emits a Warning WithdrawForced event, and lets the
	// snippet be garbage-collected — possibly leaving an orphaned
	// tarball in storage. Without this escape a permanently-broken
	// backend (S3 perma-down, revoked RBAC, deleted bucket) makes
	// snippets undeletable, which blocks namespace teardown.
	//
	// Zero falls back to defaultMaxWithdrawWait (1h). Negative is
	// treated as zero; force-drop is unconditional only when an
	// operator explicitly sets a very small value (test fixtures).
	MaxWithdrawWait time.Duration

	// MetricsBindAddress is the host:port the controller-runtime metrics
	// server listens on. The default "" leaves controller-runtime's own
	// default in place (":8080"), which collides with the jsonnet HTTP
	// server, so main.go always sets this explicitly. "0" disables the
	// metrics server entirely.
	MetricsBindAddress string

	// Logger receives operator-level events. nil falls back to slog.Default.
	Logger *slog.Logger

	// OnReady, when non-nil, is invoked exactly once after the manager's
	// cache has synced — on every replica, leader or not (it is wired as a
	// non-leader-election runnable). main.go threads `HealthState.SetReady`
	// here so the pod's readiness probe stays 503 until the operator has
	// booted and its cache is warm. Gating on leader election instead would
	// leave standby replicas permanently NotReady even though they serve the
	// HTTP renderer + storage and are ready to take over reconciliation.
	OnReady func()
}

// ParseRerenderRate converts strings like "60/min" or "1/sec" into a per-second
// rate. The form is N/period where N is a non-negative float and period is one
// of sec|s|second|seconds, min|m|minute|minutes, hour|h|hr|hours.
//
// Empty input is rejected so callers default explicitly.
func ParseRerenderRate(s string) (float64, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("rerender-rate %q: want N/period (sec|min|hour)", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, fmt.Errorf("rerender-rate %q: parse N: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("rerender-rate %q: N must be non-negative", s)
	}
	var divisor float64
	switch strings.ToLower(strings.TrimSpace(parts[1])) {
	case "sec", "s", "second", "seconds":
		divisor = 1
	case "min", "m", "minute", "minutes":
		divisor = 60
	case "hour", "h", "hr", "hours":
		divisor = 3600
	default:
		return 0, fmt.Errorf("rerender-rate %q: unknown period %q", s, parts[1])
	}
	return n / divisor, nil
}

// ParseExtVars converts KEY=VALUE strings into a map. Empty values are allowed
// (KEY= maps to ""); bare keys with no '=' are an error so a typo doesn't get
// silently swallowed.
func ParseExtVars(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		before, after, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("ext-var %q: missing '='", p)
		}
		k := before
		if k == "" {
			return nil, fmt.Errorf("ext-var %q: empty key", p)
		}
		out[k] = after
	}
	return out, nil
}
