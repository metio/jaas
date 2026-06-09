/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package statusretry holds the retry-on-Conflict status-update
// pattern that every reconciler in this repo uses. Centralising it
// keeps the retry policy uniform (same default backoff, same
// "re-Get then mutate then Update" shape) and means a future
// "switch from Status().Update to Status().Patch" change is a
// one-file edit instead of an audit across every reconciler.
//
// The function is generic over the resource type via the
// `PT interface { *T; client.Object }` trick: callers parameterise
// with their CR's value type (e.g. `JsonnetArtifact`,
// `JaaSInstance`) and the helper handles the pointer + interface
// wiring internally.
package statusretry

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UpdateWithRetry fetches the object identified by `key`, applies
// `mutate`, and persists via `Status().Update`. On
// `apierrors.IsConflict`, the cycle re-runs against a freshly-Get'd
// object, bounded by `backoff` (a zero-value backoff falls back to
// `retry.DefaultBackoff`).
//
// Why re-Get every cycle: `mutate` operates on the latest object
// the apiserver returned, so a concurrent spec edit since the
// caller's last Get is preserved — the helper never overwrites
// spec, only status. `instance.Generation` read inside `mutate`
// reflects the most recent generation rather than a cached stale
// one, so `ObservedGeneration = instance.Generation` accurately
// describes what the status block reflects.
//
// Why retry locally on Conflict: a sibling controller (or this
// reconciler's own prior status update) may have bumped
// `resourceVersion` between Get and Update; returning the
// Conflict to controller-runtime would re-queue the whole reconcile
// — re-fetching artifacts, re-rendering, etc. A local retry is
// cheaper.
//
// Generic type parameter notes:
//   - `T` is the value type (e.g. `jaasv1.JsonnetSnippet`).
//   - `PT` is the pointer type with `client.Object` methods.
//     The pair lets the helper allocate `new(T)` while still
//     satisfying the client's interface contract.
//
// Returns nil on success. On any non-Conflict error from Get or
// Update, the error surfaces immediately — those are typically
// apiserver / transport problems that controller-runtime's
// outer-loop backoff handles better than this inner retry.
func UpdateWithRetry[T any, PT interface {
	*T
	client.Object
}](
	ctx context.Context,
	c client.Client,
	backoff wait.Backoff,
	key types.NamespacedName,
	mutate func(PT),
) error {
	if backoff.Steps == 0 {
		backoff = retry.DefaultBackoff
	}
	return retry.OnError(backoff, apierrors.IsConflict, func() error {
		var zero T
		latest := PT(&zero)
		if err := c.Get(ctx, key, latest); err != nil {
			return err
		}
		mutate(latest)
		return c.Status().Update(ctx, latest)
	})
}

// UpdateUnstructuredStatusWithRetry is the unstructured-typed sibling
// of UpdateWithRetry. Same re-Get + retry-on-Conflict semantics, but
// the caller supplies the GVK explicitly: a zero-value
// `unstructured.Unstructured` carries no kind information, so the
// client has nothing to dispatch on without it.
//
// Used by the JsonnetArtifact reconciler for Flux's ExternalArtifact
// CR. We don't import source-controller's Go types (it would balloon
// the dependency graph for a single CRD shape we only ever touch as
// an opaque wire format), so the typed UpdateWithRetry isn't
// applicable — but the same retry posture is desirable: a `Conflict`
// at the Status().Update phase (typically source-controller bumping
// resourceVersion as it observes the new artifact) would otherwise
// propagate out of the reconcile and force a full re-fetch +
// re-render + re-upload cycle. Retrying locally is cheaper.
//
// `mutate` receives the apiserver-latest object on every attempt.
// Set `latest.Object["status"]` to the desired status block; do not
// mutate spec or metadata — those were already settled by the prior
// spec-phase write. The helper preserves whatever resourceVersion
// the Get returned, so the Status().Update sees the most recent.
//
// Returns nil on success. On a non-Conflict error from Get or
// Update, the error surfaces immediately.
func UpdateUnstructuredStatusWithRetry(
	ctx context.Context,
	c client.Client,
	backoff wait.Backoff,
	gvk schema.GroupVersionKind,
	key types.NamespacedName,
	mutate func(*unstructured.Unstructured),
) error {
	if backoff.Steps == 0 {
		backoff = retry.DefaultBackoff
	}
	return retry.OnError(backoff, apierrors.IsConflict, func() error {
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, key, latest); err != nil {
			return err
		}
		mutate(latest)
		return c.Status().Update(ctx, latest)
	})
}

// BackoffForTests is a deterministic, short-cycle backoff for use
// in unit tests. Pulled out so test files in every consumer
// package share one tuned value and tests aren't throttled by the
// production-grade `retry.DefaultBackoff`.
func BackoffForTests() wait.Backoff {
	return wait.Backoff{
		Steps:    5,
		Duration: time.Millisecond,
		Factor:   1.0,
		Jitter:   0,
	}
}
