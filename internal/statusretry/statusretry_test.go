/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package statusretry_test

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/metio/jaas/internal/statusretry"
)

// Invariants pinned by this file:
//
//  1. The happy path: a single successful Get + Status().Update
//     returns nil and `mutate` has fired exactly once against the
//     latest apiserver object.
//  2. A transient `IsConflict` on Status().Update triggers a
//     re-Get + retry. The retry's `mutate` runs against the
//     re-fetched object, not the caller's stale pointer.
//  3. Persistent Conflicts exhaust the backoff and surface the
//     final Conflict error to the caller.
//  4. Non-Conflict errors (transport, permission, etc.) surface
//     immediately without retrying — controller-runtime's outer
//     backoff is the right home for those.
//  5. A Get failure surfaces immediately too — if we can't read
//     the object there's nothing to mutate-and-write.

// =========================================================================
// test fixtures: a minimal CR-shaped type so we don't depend on the
// project's own v1alpha1 types in this package's tests.
// =========================================================================

// fakeObject is a stand-in for any project CR. It satisfies
// `client.Object` and has a Status field we can mutate.
//
// +kubebuilder:object:root=true
type fakeObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Status            fakeStatus `json:"status,omitempty"`
}

type fakeStatus struct {
	Phase string `json:"phase,omitempty"`
}

func (f *fakeObject) DeepCopyObject() runtime.Object {
	if f == nil {
		return nil
	}
	out := *f
	return &out
}

var fakeObjectGVK = schema.GroupVersionKind{
	Group:   "test.statusretry",
	Version: "v1",
	Kind:    "FakeObject",
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(fakeObjectGVK, &fakeObject{})
	// AddKnownTypeWithName for List would normally also be required, but
	// our tests don't List; the existing registration is enough for
	// Get/Status/Update.
	return s
}

func newFakeClient(t *testing.T, seed *fakeObject) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&fakeObject{}).
		WithObjects(seed).
		Build()
}

func sample(name string) *fakeObject {
	return &fakeObject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 7},
	}
}

// =========================================================================
// Layer 1: happy path
// =========================================================================

func TestUpdateWithRetry_HappyPath_AppliesMutateExactlyOnce(t *testing.T) {
	c := newFakeClient(t, sample("ok"))
	key := types.NamespacedName{Namespace: "default", Name: "ok"}
	calls := 0
	err := statusretry.UpdateWithRetry[fakeObject](
		context.Background(), c, statusretry.BackoffForTests(), key,
		func(latest *fakeObject) {
			calls++
			latest.Status.Phase = "ready"
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("mutate fired %d times, want 1 (no Conflict ⇒ no retry)", calls)
	}
	got := &fakeObject{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "ready" {
		t.Errorf("Status.Phase = %q, want ready", got.Status.Phase)
	}
}

func TestUpdateWithRetry_MutateRunsAgainstLatestNotStale(t *testing.T) {
	// Pin that `mutate` receives the object the Get returned, not
	// some pre-seeded pointer. We Get-intercept to mutate the
	// in-flight object before returning it so mutate's view has
	// `Generation = 99`; the caller never holds a reference to
	// any other generation.
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&fakeObject{}).
		WithObjects(sample("latest")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				obj.(*fakeObject).Generation = 99
				return nil
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "latest"}
	var seenGen int64
	err := statusretry.UpdateWithRetry[fakeObject](
		context.Background(), c, statusretry.BackoffForTests(), key,
		func(latest *fakeObject) {
			seenGen = latest.Generation
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if seenGen != 99 {
		t.Errorf("mutate saw Generation %d, want 99 (the latest returned by Get, not a stale pointer)", seenGen)
	}
}

// =========================================================================
// Layer 1: Conflict retry semantics
// =========================================================================

func TestUpdateWithRetry_TransientConflict_RetriesAndSucceeds(t *testing.T) {
	// First Status().Update returns Conflict; the second succeeds.
	// The helper must re-Get + re-mutate before the second attempt
	// — pin via call count and a fresh mutate invocation.
	statusCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&fakeObject{}).
		WithObjects(sample("conflict")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusCalls++
				if statusCalls == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: fakeObjectGVK.Group, Resource: "fakeobjects"},
						obj.GetName(), errors.New("simulated conflict"),
					)
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "conflict"}
	mutateCalls := 0
	err := statusretry.UpdateWithRetry[fakeObject](
		context.Background(), c, statusretry.BackoffForTests(), key,
		func(latest *fakeObject) {
			mutateCalls++
			latest.Status.Phase = "ready"
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil (retry should have succeeded)", err)
	}
	if statusCalls != 2 {
		t.Errorf("Status().Update fired %d times, want 2 (1 Conflict + 1 retry)", statusCalls)
	}
	if mutateCalls != 2 {
		t.Errorf("mutate fired %d times, want 2 — re-Get must re-run mutate against the latest", mutateCalls)
	}
}

func TestUpdateWithRetry_PersistentConflict_ExhaustsBackoffAndErrors(t *testing.T) {
	// Every Status().Update returns Conflict. The helper retries
	// up to `backoff.Steps` times, then surfaces the final
	// Conflict to the caller so controller-runtime's outer loop
	// can take over.
	statusCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&fakeObject{}).
		WithObjects(sample("perpetual")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusCalls++
				return apierrors.NewConflict(
					schema.GroupResource{Group: fakeObjectGVK.Group, Resource: "fakeobjects"},
					obj.GetName(), errors.New("persistent conflict"),
				)
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "perpetual"}
	backoff := statusretry.BackoffForTests()
	err := statusretry.UpdateWithRetry[fakeObject](
		context.Background(), c, backoff, key,
		func(latest *fakeObject) { latest.Status.Phase = "ready" },
	)
	if err == nil {
		t.Fatal("err = nil, want persistent Conflict propagated")
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("err = %v, want IsConflict to be true (controller-runtime routes Conflicts specially)", err)
	}
	if statusCalls != backoff.Steps {
		t.Errorf("Status().Update fired %d times, want %d (backoff.Steps)", statusCalls, backoff.Steps)
	}
}

// =========================================================================
// Layer 1: non-Conflict error handling
// =========================================================================

func TestUpdateWithRetry_NonConflictUpdateError_NoRetry(t *testing.T) {
	// Per the retry.OnError contract, only IsConflict triggers a
	// retry — every other error surfaces immediately so
	// controller-runtime's outer backoff handles it.
	want := errors.New("simulated transport error")
	statusCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&fakeObject{}).
		WithObjects(sample("transport")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusCalls++
				return want
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "transport"}
	err := statusretry.UpdateWithRetry[fakeObject](
		context.Background(), c, statusretry.BackoffForTests(), key,
		func(latest *fakeObject) {},
	)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
	if statusCalls != 1 {
		t.Errorf("Status().Update fired %d times, want 1 (non-Conflict ⇒ no retry)", statusCalls)
	}
}

func TestUpdateWithRetry_GetError_SurfacesImmediately(t *testing.T) {
	// If the initial Get fails, there's nothing to mutate-and-write.
	// The helper surfaces the Get error directly.
	want := errors.New("simulated get error")
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&fakeObject{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return want
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "absent"}
	called := false
	err := statusretry.UpdateWithRetry[fakeObject](
		context.Background(), c, statusretry.BackoffForTests(), key,
		func(latest *fakeObject) { called = true },
	)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
	if called {
		t.Error("mutate fired despite Get error — it must not run when Get returns")
	}
}

// =========================================================================
// Layer 1: backoff fallback
// =========================================================================

// =========================================================================
// Layer 1 — UpdateUnstructuredStatusWithRetry
// =========================================================================
//
// Mirror of the typed-CR tests above, but exercising the unstructured
// variant the JsonnetArtifact reconciler uses for Flux's
// ExternalArtifact (which we don't import as a typed CR). The
// invariants are identical:
//
//   1. Happy path: a single Get + Status().Update, mutate fires once.
//   2. Transient Conflict triggers a re-Get + retry; mutate runs
//      against the re-fetched object.
//   3. Persistent Conflict exhausts the backoff and surfaces
//      IsConflict.
//   4. Non-Conflict errors surface immediately without retrying.
//   5. Get errors surface immediately and mutate doesn't fire.
//   6. Zero-value Backoff falls back to retry.DefaultBackoff.
//
// The fakeUnstructuredGVK is a fabricated GVK that the fake client
// can dispatch on once registered in the scheme. The CRD shape
// doesn't matter — only the GVK + status-subresource registration.

// fakeUnstructuredGVK stands in for any unstructured-typed CR that
// callers might want to use this helper against. Distinct from
// fakeObjectGVK so the two test suites can't inadvertently share
// scheme state.
var fakeUnstructuredGVK = schema.GroupVersionKind{
	Group:   "test.statusretry",
	Version: "v1",
	Kind:    "FakeUnstructured",
}

// newUnstructuredFakeClient builds a fake client primed with the
// unstructured GVK + status subresource registration, optionally
// seeded with a starter object so the Get inside the retry loop has
// something to return.
func newUnstructuredFakeClient(t *testing.T, seeded *unstructured.Unstructured) client.WithWatch {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(fakeUnstructuredGVK, &unstructured.Unstructured{})
	subresourceStub := &unstructured.Unstructured{}
	subresourceStub.SetGroupVersionKind(fakeUnstructuredGVK)
	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(subresourceStub)
	if seeded != nil {
		builder = builder.WithObjects(seeded)
	}
	return builder.Build()
}

func sampleUnstructured(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(fakeUnstructuredGVK)
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func TestUpdateUnstructuredStatusWithRetry_HappyPath_AppliesMutateExactlyOnce(t *testing.T) {
	c := newUnstructuredFakeClient(t, sampleUnstructured("ok"))
	key := types.NamespacedName{Namespace: "default", Name: "ok"}
	calls := 0
	err := statusretry.UpdateUnstructuredStatusWithRetry(
		context.Background(), c, statusretry.BackoffForTests(),
		fakeUnstructuredGVK, key,
		func(latest *unstructured.Unstructured) {
			calls++
			latest.Object["status"] = map[string]interface{}{"phase": "ready"}
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("mutate fired %d times, want 1 (no Conflict ⇒ no retry)", calls)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(fakeUnstructuredGVK)
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(got.Object, "status", "phase")
	if phase != "ready" {
		t.Errorf("status.phase = %q, want ready", phase)
	}
}

func TestUpdateUnstructuredStatusWithRetry_MutateRunsAgainstLatestNotStale(t *testing.T) {
	// Pin that `mutate` sees the object returned by Get, not the
	// caller's pre-seeded pointer. We Get-intercept to stamp a
	// sentinel field on the in-flight object before returning it;
	// mutate must observe the sentinel.
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(fakeUnstructuredGVK, &unstructured.Unstructured{})
	subresourceStub := &unstructured.Unstructured{}
	subresourceStub.SetGroupVersionKind(fakeUnstructuredGVK)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(subresourceStub).
		WithObjects(sampleUnstructured("latest")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if u, ok := obj.(*unstructured.Unstructured); ok {
					u.SetAnnotations(map[string]string{"sentinel": "fresh"})
				}
				return nil
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "latest"}
	var seen string
	err := statusretry.UpdateUnstructuredStatusWithRetry(
		context.Background(), c, statusretry.BackoffForTests(),
		fakeUnstructuredGVK, key,
		func(latest *unstructured.Unstructured) {
			seen = latest.GetAnnotations()["sentinel"]
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if seen != "fresh" {
		t.Errorf("mutate observed sentinel = %q, want %q (the latest from Get, not a stale pointer)",
			seen, "fresh")
	}
}

func TestUpdateUnstructuredStatusWithRetry_TransientConflict_RetriesAndSucceeds(t *testing.T) {
	statusCalls := 0
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(fakeUnstructuredGVK, &unstructured.Unstructured{})
	subresourceStub := &unstructured.Unstructured{}
	subresourceStub.SetGroupVersionKind(fakeUnstructuredGVK)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(subresourceStub).
		WithObjects(sampleUnstructured("conflict")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusCalls++
				if statusCalls == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: fakeUnstructuredGVK.Group, Resource: "fakeunstructureds"},
						obj.GetName(), errors.New("simulated conflict"),
					)
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "conflict"}
	mutateCalls := 0
	err := statusretry.UpdateUnstructuredStatusWithRetry(
		context.Background(), c, statusretry.BackoffForTests(),
		fakeUnstructuredGVK, key,
		func(latest *unstructured.Unstructured) {
			mutateCalls++
			latest.Object["status"] = map[string]interface{}{"phase": "ready"}
		},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil (retry should have succeeded)", err)
	}
	if statusCalls != 2 {
		t.Errorf("Status().Update fired %d times, want 2 (1 Conflict + 1 retry)", statusCalls)
	}
	if mutateCalls != 2 {
		t.Errorf("mutate fired %d times, want 2 — re-Get must re-run mutate against the latest", mutateCalls)
	}
}

func TestUpdateUnstructuredStatusWithRetry_PersistentConflict_ExhaustsBackoffAndErrors(t *testing.T) {
	statusCalls := 0
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(fakeUnstructuredGVK, &unstructured.Unstructured{})
	subresourceStub := &unstructured.Unstructured{}
	subresourceStub.SetGroupVersionKind(fakeUnstructuredGVK)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(subresourceStub).
		WithObjects(sampleUnstructured("perpetual")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusCalls++
				return apierrors.NewConflict(
					schema.GroupResource{Group: fakeUnstructuredGVK.Group, Resource: "fakeunstructureds"},
					obj.GetName(), errors.New("persistent conflict"),
				)
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "perpetual"}
	backoff := statusretry.BackoffForTests()
	err := statusretry.UpdateUnstructuredStatusWithRetry(
		context.Background(), c, backoff,
		fakeUnstructuredGVK, key,
		func(latest *unstructured.Unstructured) {},
	)
	if err == nil {
		t.Fatal("err = nil, want persistent Conflict propagated")
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("err = %v, want IsConflict to be true", err)
	}
	if statusCalls != backoff.Steps {
		t.Errorf("Status().Update fired %d times, want %d (backoff.Steps)", statusCalls, backoff.Steps)
	}
}

func TestUpdateUnstructuredStatusWithRetry_NonConflictUpdateError_NoRetry(t *testing.T) {
	want := errors.New("simulated transport error")
	statusCalls := 0
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(fakeUnstructuredGVK, &unstructured.Unstructured{})
	subresourceStub := &unstructured.Unstructured{}
	subresourceStub.SetGroupVersionKind(fakeUnstructuredGVK)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(subresourceStub).
		WithObjects(sampleUnstructured("transport")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusCalls++
				return want
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "transport"}
	err := statusretry.UpdateUnstructuredStatusWithRetry(
		context.Background(), c, statusretry.BackoffForTests(),
		fakeUnstructuredGVK, key,
		func(latest *unstructured.Unstructured) {},
	)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
	if statusCalls != 1 {
		t.Errorf("Status().Update fired %d times, want 1 (non-Conflict ⇒ no retry)", statusCalls)
	}
}

func TestUpdateUnstructuredStatusWithRetry_GetError_SurfacesImmediately(t *testing.T) {
	want := errors.New("simulated get error")
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(fakeUnstructuredGVK, &unstructured.Unstructured{})
	subresourceStub := &unstructured.Unstructured{}
	subresourceStub.SetGroupVersionKind(fakeUnstructuredGVK)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(subresourceStub).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				return want
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "absent"}
	called := false
	err := statusretry.UpdateUnstructuredStatusWithRetry(
		context.Background(), c, statusretry.BackoffForTests(),
		fakeUnstructuredGVK, key,
		func(latest *unstructured.Unstructured) { called = true },
	)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
	if called {
		t.Error("mutate fired despite Get error — it must not run when Get returns")
	}
}

func TestUpdateUnstructuredStatusWithRetry_ZeroBackoff_FallsBackToDefault(t *testing.T) {
	// Mirror of the typed-CR sibling test: a zero-value Backoff
	// (Steps==0) must fall back to retry.DefaultBackoff so callers
	// who don't override get a sane retry budget instead of silently
	// skipping the loop.
	statusCalls := 0
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(fakeUnstructuredGVK, &unstructured.Unstructured{})
	subresourceStub := &unstructured.Unstructured{}
	subresourceStub.SetGroupVersionKind(fakeUnstructuredGVK)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(subresourceStub).
		WithObjects(sampleUnstructured("default")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusCalls++
				return apierrors.NewConflict(
					schema.GroupResource{Group: fakeUnstructuredGVK.Group, Resource: "fakeunstructureds"},
					obj.GetName(), errors.New("conflict"),
				)
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "default"}
	_ = statusretry.UpdateUnstructuredStatusWithRetry(
		context.Background(), c, wait.Backoff{},
		fakeUnstructuredGVK, key,
		func(latest *unstructured.Unstructured) {},
	)
	if statusCalls < 2 {
		t.Errorf("Status().Update fired %d times, want > 1 (zero backoff must fall back to default)", statusCalls)
	}
}

// =========================================================================
// Layer 1 — UpdateWithRetry (existing) zero-backoff regression
// =========================================================================

func TestUpdateWithRetry_ZeroBackoff_FallsBackToDefault(t *testing.T) {
	// A zero-value Backoff (Steps==0) is the documented "fall
	// back to retry.DefaultBackoff" path. Verify by feeding a
	// zero-value and checking the loop ran more than once on a
	// persistent Conflict (default has Steps=5 vs zero's 0).
	statusCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&fakeObject{}).
		WithObjects(sample("default")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusCalls++
				return apierrors.NewConflict(
					schema.GroupResource{Group: fakeObjectGVK.Group, Resource: "fakeobjects"},
					obj.GetName(), errors.New("conflict"),
				)
			},
		}).
		Build()
	key := types.NamespacedName{Namespace: "default", Name: "default"}
	_ = statusretry.UpdateWithRetry[fakeObject](
		context.Background(), c, wait.Backoff{}, key,
		func(latest *fakeObject) {},
	)
	if statusCalls < 2 {
		t.Errorf("Status().Update fired %d times, want > 1 (zero backoff must fall back to default)", statusCalls)
	}
}
