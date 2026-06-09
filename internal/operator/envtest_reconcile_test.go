/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

// directReconciler returns a SnippetReconciler that talks to the envtest
// apiserver and (when withPublisher) writes ExternalArtifacts via a real
// Store.
func directReconciler(t *testing.T, c client.Client, withPublisher bool) *SnippetReconciler {
	t.Helper()
	r := &SnippetReconciler{
		Client: c,
		Scheme: envtestScheme(t),
		Logger: discardLoggerEnvtest(),
	}
	if withPublisher {
		store, err := storage.New(t.TempDir())
		if err != nil {
			t.Fatalf("storage.New: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		r.Publisher = NewPublisher(store, "http://jaas-storage.test.svc.cluster.local:8082")
	}
	return r
}

// driveToReady calls Reconcile repeatedly (up to maxRounds) until the
// snippet's Ready condition matches expectation, faithfully simulating the
// manager's watch-driven reconcile loop.
func driveToReady(t *testing.T, r *SnippetReconciler, c client.Client, key types.NamespacedName, wantStatus metav1.ConditionStatus, wantReason string, maxRounds int) {
	t.Helper()
	for i := 0; i < maxRounds; i++ {
		// Reconcile may return an error on TRANSIENT classifications
		// (source-not-ready, network blip) — controller-runtime would
		// engage backoff and retry. The status is still written
		// before the return, so we keep polling the condition; the
		// error itself is not a test failure.
		_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		var snip jaasv1.JsonnetSnippet
		if err := c.Get(context.Background(), key, &snip); err != nil {
			t.Fatalf("refetch: %v", err)
		}
		cond := apimeta.FindStatusCondition(snip.Status.Conditions, jaasv1.ConditionReady)
		if cond != nil && cond.Status == wantStatus && cond.Reason == wantReason {
			return
		}
	}
	t.Fatalf("Ready never reached status=%v reason=%q within %d reconciles",
		wantStatus, wantReason, maxRounds)
}

func TestEnvtest_Reconcile_HappyPath_SetsReadyAndPublishes(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := directReconciler(t, c, true)
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	// Confirm Status.Revision was written.
	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Status.Revision, "sha256:") {
		t.Errorf("Status.Revision = %q, want sha256: prefix", got.Status.Revision)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("ObservedGeneration = %d, want %d", got.Status.ObservedGeneration, got.Generation)
	}

	// And the ExternalArtifact materialized on the same name+namespace.
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(), key, ea); err != nil {
		t.Fatalf("ExternalArtifact not created: %v", err)
	}
	url, _, _ := unstructured.NestedString(ea.Object, "status", "artifact", "url")
	if !strings.HasPrefix(url, "http://jaas-storage.test.svc.cluster.local:8082/") {
		t.Errorf("status.artifact.url = %q, want absolute URL", url)
	}
}

func TestEnvtest_Reconcile_LibraryResolution_RealCRDs(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: ns},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ shared: "value" }`},
			},
		},
	}
	if err := c.Create(context.Background(), lib); err != nil {
		t.Fatalf("create library: %v", err)
	}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{
					"main.jsonnet": `(import "u") + { extra: true }`,
				},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "u"},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := directReconciler(t, c, true)
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)
}

func TestEnvtest_Reconcile_LibraryNotFound_FailsLibraryNotFound(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{}`},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "ghost"},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := directReconciler(t, c, false)
	driveToReady(t, r, c, key, metav1.ConditionFalse, ReasonLibraryNotFound, 5)
}

func TestEnvtest_Reconcile_EvaluationFailureSurfacesReason(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "local x ="},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := directReconciler(t, c, false)
	driveToReady(t, r, c, key, metav1.ConditionFalse, ReasonEvaluationFailed, 5)
}

func TestEnvtest_Reconcile_Deletion_RemovesArtifactAndFinalizer(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{}`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := directReconciler(t, c, true)
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	// Verify the artifact exists, then delete the snippet.
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(), key, ea); err != nil {
		t.Fatalf("ExternalArtifact missing pre-delete: %v", err)
	}

	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &got); err != nil {
		t.Fatalf("delete snippet: %v", err)
	}

	// The finalizer holds the object alive; one more Reconcile cycles
	// through reconcileDelete which Withdraws + removes the finalizer.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}

	pollUntil(t, 5*time.Second, func() (bool, string) {
		ea := &unstructured.Unstructured{}
		ea.SetGroupVersionKind(externalArtifactGVK)
		err := c.Get(context.Background(), key, ea)
		if err == nil {
			return false, "ExternalArtifact still present"
		}
		return true, ""
	})
}

// TestEnvtest_Reconcile_BulkDelete_AllFinalizersFire models the
// `helm uninstall` pre-delete hook: many snippets get deleted at once,
// and each finalizer must drop its ExternalArtifact + tarball before
// the operator goes away. Catches deadlocks or state-machine bugs that
// only show up under N>1 simultaneous deletes.
func TestEnvtest_Reconcile_BulkDelete_AllFinalizersFire(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	const n = 5
	keys := make([]types.NamespacedName, n)
	for i := 0; i < n; i++ {
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bulk-" + strconv.Itoa(i),
				Namespace: ns,
			},
			Spec: jaasv1.JsonnetSnippetSpec{
				ServiceAccountName: "tenant",
				SnippetSource: jaasv1.SnippetSource{
					Files: map[string]string{"main.jsonnet": `{ i: ` + strconv.Itoa(i) + ` }`},
				},
				Output: jaasv1.OutputRendered,
			},
		}
		keys[i] = applyJsonnetSnippet(t, c, snip)
	}

	r := directReconciler(t, c, true)
	for _, key := range keys {
		driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 5)
	}

	// Confirm every ExternalArtifact landed.
	for _, key := range keys {
		ea := &unstructured.Unstructured{}
		ea.SetGroupVersionKind(externalArtifactGVK)
		if err := c.Get(context.Background(), key, ea); err != nil {
			t.Fatalf("EA missing for %s pre-delete: %v", key.Name, err)
		}
	}

	// Bulk delete — mirrors what `kubectl delete jsonnetsnippet --all`
	// does. The apiserver tombstones each one, finalizers hold them
	// alive until reconcileDelete runs.
	for _, key := range keys {
		var got jaasv1.JsonnetSnippet
		if err := c.Get(context.Background(), key, &got); err != nil {
			t.Fatalf("get %s: %v", key.Name, err)
		}
		if err := c.Delete(context.Background(), &got); err != nil {
			t.Fatalf("delete %s: %v", key.Name, err)
		}
	}

	// One Reconcile per snippet runs reconcileDelete → Withdraw →
	// finalizer drop. Bulk delete must NOT cause any single Withdraw
	// to fail because of contention with the others.
	for _, key := range keys {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Errorf("delete reconcile for %s: %v", key.Name, err)
		}
	}

	// Every ExternalArtifact must be gone within a short window.
	pollUntil(t, 10*time.Second, func() (bool, string) {
		for _, key := range keys {
			ea := &unstructured.Unstructured{}
			ea.SetGroupVersionKind(externalArtifactGVK)
			if err := c.Get(context.Background(), key, ea); err == nil {
				return false, "EA still present for " + key.Name
			}
		}
		return true, ""
	})
}

func TestEnvtest_Reconcile_RateLimiterReturnsRequeueAfter(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{}`},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := directReconciler(t, c, false)
	r.Limiter = NewRateLimiter(1.0, 1) // burst 1 ⇒ second pass is throttled
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	// Round 1: finalizer add.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	// Round 2: spec eval consumes the bucket's only token.
	res2, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if res2.RequeueAfter != 0 {
		t.Fatalf("round 2 unexpectedly throttled: RequeueAfter=%v", res2.RequeueAfter)
	}
	// Round 3: rate-limited.
	counterBefore := testutil.ToFloat64(snippetRateLimitedTotal.WithLabelValues(ns, snip.Name))
	res3, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if res3.RequeueAfter == 0 {
		t.Errorf("round 3 RequeueAfter = 0, want > 0 (rate-limited)")
	}

	// The denied Reserve must surface as both a Warning event (for
	// kubectl describe / notification-controller) AND a counter bump
	// (for Prometheus dashboards). Without these surfaces the Debug
	// log alone leaves operators guessing why the snippet hasn't
	// reconciled.
	counterAfter := testutil.ToFloat64(snippetRateLimitedTotal.WithLabelValues(ns, snip.Name))
	if counterAfter-counterBefore != 1 {
		t.Errorf("jaas_snippet_rate_limited_total moved by %v, want 1", counterAfter-counterBefore)
	}
	gotEvents := drainEvents(rec)
	var found bool
	for _, ev := range gotEvents {
		if strings.HasPrefix(ev, "Warning RateLimited") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no Warning RateLimited event among %v", gotEvents)
	}
}

// TestEnvtest_PodRestart_LocalStorageRecovery models an emptyDir-style
// restart: snippet reaches Ready=True against store-1; "the pod
// restarts" — we close store-1, swap in a fresh empty store-2
// (simulating emptyDir's contents being lost), and Reconcile again.
// The reconciler must re-render and re-publish the tarball into store-2,
// returning the snippet to Ready=True without operator intervention.
//
// This pins the recovery story documented in the chart: when persistence
// is OFF, downstream Flux consumers see a brief 404 window after a
// restart, but the operator self-heals on next reconcile.
func TestEnvtest_PodRestart_LocalStorageRecovery(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "recover", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true, n: 42 }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	// First "pod incarnation": store-1, publisher-1.
	store1, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New (1): %v", err)
	}
	r1 := &SnippetReconciler{
		Client:    c,
		Scheme:    envtestScheme(t),
		Publisher: NewPublisher(store1, "http://jaas-storage.test/"),
		Logger:    discardLoggerEnvtest(),
	}
	driveToReady(t, r1, c, key, metav1.ConditionTrue, ReasonSynced, 5)

	// Capture the tarball path the publisher wrote, then prove it
	// exists on disk before the "restart".
	var beforeSnip jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &beforeSnip); err != nil {
		t.Fatalf("get snippet pre-restart: %v", err)
	}
	beforeRev := strings.TrimPrefix(beforeSnip.Status.Revision, "sha256:")
	if beforeRev == "" {
		t.Fatal("Status.Revision empty after first reconcile")
	}

	// Simulate emptyDir loss: close store-1 and replace its directory
	// with an empty one for store-2.
	_ = store1.Close()
	store2Dir := t.TempDir()
	store2, err := storage.New(store2Dir)
	if err != nil {
		t.Fatalf("storage.New (2): %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	// New "pod incarnation": fresh reconciler with the empty store. Note
	// the snippet CR is unchanged, but ObservedGeneration == Generation
	// already. The reconciler must still notice the artifact is missing
	// from its store and re-render it. Because the existing code path
	// rewrites the tarball on every spec reconcile (idempotent Put), a
	// single Reconcile is enough.
	r2 := &SnippetReconciler{
		Client:    c,
		Scheme:    envtestScheme(t),
		Publisher: NewPublisher(store2, "http://jaas-storage.test/"),
		Logger:    discardLoggerEnvtest(),
	}
	if _, err := r2.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("post-restart reconcile: %v", err)
	}

	// The new store must now contain the same tarball — same digest,
	// since inputs are deterministic and the tar.gz format is
	// reproducible across backends.
	relPath := filepath.Join(store2Dir, ns, "recover", beforeRev+".tar.gz")
	if _, err := os.Stat(relPath); err != nil {
		t.Errorf("tarball missing from store-2 after restart-and-reconcile: %v", err)
	}

	// Snippet stays Ready=True.
	var afterSnip jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &afterSnip); err != nil {
		t.Fatalf("get snippet post-restart: %v", err)
	}
	cond := apimeta.FindStatusCondition(afterSnip.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonSynced {
		t.Errorf("post-restart Ready = %+v, want True/Synced", cond)
	}
	if afterSnip.Status.Revision != beforeSnip.Status.Revision {
		t.Errorf("revision drifted across restart: %q → %q",
			beforeSnip.Status.Revision, afterSnip.Status.Revision)
	}

	// And the ExternalArtifact's URL still points at the same revision
	// — downstream Flux consumers see a continuous chain.
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(), key, ea); err != nil {
		t.Fatalf("ExternalArtifact post-restart: %v", err)
	}
	url, _, _ := unstructured.NestedString(ea.Object, "status", "artifact", "url")
	if !strings.Contains(url, beforeRev) {
		t.Errorf("ExternalArtifact URL %q lost the revision after restart", url)
	}
}
