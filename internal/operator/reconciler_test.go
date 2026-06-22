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
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/eval"
	"github.com/metio/jaas/internal/sources"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := jaasv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

func sampleSnippet() *jaasv1.JsonnetSnippet {
	return &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "demo",
			Namespace:  "team-a",
			Generation: 1,
			// UID is wire-stable; the apiserver assigns it on Create.
			// Tests that exercise UID-keyed caches (cycleCache, etc.)
			// need a non-empty value or the cache silently no-ops.
			UID: types.UID("uid-demo"),
		},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
}

func newReconciler(t *testing.T, c client.Client) *SnippetReconciler {
	t.Helper()
	return &SnippetReconciler{
		Client: c,
		Scheme: testScheme(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func refetch(t *testing.T, c client.Client, key types.NamespacedName) *jaasv1.JsonnetSnippet {
	t.Helper()
	var out jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &out); err != nil {
		t.Fatalf("refetch %s: %v", key, err)
	}
	return &out
}

func runReconcile(t *testing.T, r *SnippetReconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	return res
}

func assertReady(t *testing.T, snip *jaasv1.JsonnetSnippet, wantStatus metav1.ConditionStatus, wantReason string) {
	t.Helper()
	cond := apimeta.FindStatusCondition(snip.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition not written")
	}
	if cond.Status != wantStatus {
		t.Errorf("Ready.Status = %v, want %v (reason=%q, message=%q)",
			cond.Status, wantStatus, cond.Reason, cond.Message)
	}
	if cond.Reason != wantReason {
		t.Errorf("Ready.Reason = %q, want %q (message=%q)",
			cond.Reason, wantReason, cond.Message)
	}
}

func clientWithStatus(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		Build()
}

// --- Reconcile lifecycle ----------------------------------------------------

func TestReconcile_NotFoundIsNoOp(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := newReconciler(t, c)
	res := runReconcile(t, r, types.NamespacedName{Name: "ghost", Namespace: "team-a"})
	if res != (ctrl.Result{}) {
		t.Errorf("got %+v, want empty Result", res)
	}
}

func TestReconcile_AddsFinalizerOnFirstReconcile(t *testing.T) {
	snip := sampleSnippet()
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	found := false
	for _, f := range got.Finalizers {
		if f == FinalizerName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Finalizers = %v, want %q present", got.Finalizers, FinalizerName)
	}
}

func TestReconcile_DeletionTimestampWithFinalizerRemovesIt(t *testing.T) {
	snip := sampleSnippet()
	now := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &now
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

	// With the finalizer dropped on a deleting object, the fake client
	// removes it from the store, so a follow-up Get returns NotFound.
	var out jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}, &out); err == nil {
		t.Errorf("snippet still present after finalizer removal; finalizers=%v", out.Finalizers)
	}
}

func TestReconcile_Delete_WithoutEffectiveSA_SkipsTokenCacheForget(t *testing.T) {
	// A snippet with neither spec.serviceAccountName nor a default SA
	// goes through reconcileDelete without exercising the TokenCache —
	// the "effective SA empty" branch.
	snip := sampleSnippet()
	snip.Spec.ServiceAccountName = ""
	now := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &now
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	// DefaultServiceAccount intentionally empty; TokenCache set but should
	// never be Touch'd because sa resolves to "".
	r.TokenCache = newTokenCache(&stubMinter{})
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
}

func TestReconcile_DeletionTimestampWithoutOurFinalizerIsNoOp(t *testing.T) {
	// Another controller's finalizer keeps the object alive; ours is absent.
	// We must not error and must leave the foreign finalizer untouched.
	snip := sampleSnippet()
	now := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &now
	snip.Finalizers = []string{"some-other-controller/finalizer"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	res := runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	if res != (ctrl.Result{}) {
		t.Errorf("got %+v, want empty Result on deletion no-op", res)
	}
	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	if len(got.Finalizers) != 1 || got.Finalizers[0] != "some-other-controller/finalizer" {
		t.Errorf("foreign finalizer was modified: got %v", got.Finalizers)
	}
}

// --- Eval pipeline: happy path + each failure branch ------------------------

func TestReconcile_HappyPath_SetsReadyTrueAndRevision(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, got, metav1.ConditionTrue, ReasonSynced)
	if got.Status.ObservedGeneration != snip.Generation {
		t.Errorf("ObservedGeneration = %d, want %d", got.Status.ObservedGeneration, snip.Generation)
	}
	if !strings.HasPrefix(got.Status.Revision, "sha256:") {
		t.Errorf("Revision = %q, want it to start with 'sha256:'", got.Status.Revision)
	}
	// The body of the snippet — {ok: true} — renders to a 14-byte payload.
	// Recompute the expected digest from the actual eval here so the test
	// stays robust to formatting changes.
	want := "sha256:" + hex.EncodeToString(func() []byte {
		// Re-run with the same input to derive the digest.
		s := sha256.Sum256([]byte("{\n   \"ok\": true\n}\n"))
		return s[:]
	}())
	if got.Status.Revision != want {
		t.Errorf("Revision = %q, want %q", got.Status.Revision, want)
	}
}

// TestReconcile_ReadyCondition_CarriesObservedGeneration pins that the Ready
// condition itself (not just status.observedGeneration) records the generation
// the reconcile acted on. kstatus and other condition-aware tooling read
// condition.observedGeneration to tell a stale condition apart from a current
// one; apimeta.SetStatusCondition does not fill it, so the reconciler must.
func TestReconcile_ReadyCondition_CarriesObservedGeneration(t *testing.T) {
	t.Run("synced path", func(t *testing.T) {
		snip := sampleSnippet()
		snip.Generation = 7
		snip.Finalizers = []string{FinalizerName}
		c := clientWithStatus(t, snip)
		r := newReconciler(t, c)
		runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

		got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
		assertReady(t, got, metav1.ConditionTrue, ReasonSynced)
		cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
		if cond.ObservedGeneration == 0 {
			t.Fatal("Ready.ObservedGeneration is zero, want non-zero")
		}
		if cond.ObservedGeneration != got.Generation {
			t.Errorf("Ready.ObservedGeneration = %d, want %d", cond.ObservedGeneration, got.Generation)
		}
	})

	t.Run("failure path", func(t *testing.T) {
		snip := sampleSnippet()
		snip.Generation = 3
		snip.Spec.Files = map[string]string{"main.jsonnet": "{ broken"}
		snip.Finalizers = []string{FinalizerName}
		c := clientWithStatus(t, snip)
		r := newReconciler(t, c)
		runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

		got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
		cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
		if cond == nil {
			t.Fatal("Ready condition not written")
		}
		if cond.Status != metav1.ConditionFalse {
			t.Fatalf("Ready.Status = %v, want False", cond.Status)
		}
		if cond.ObservedGeneration == 0 {
			t.Fatal("Ready.ObservedGeneration is zero, want non-zero")
		}
		if cond.ObservedGeneration != got.Generation {
			t.Errorf("Ready.ObservedGeneration = %d, want %d", cond.ObservedGeneration, got.Generation)
		}
	})
}

func TestReconcile_SourceRefNotYetSupported(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, got, metav1.ConditionFalse, ReasonSourceRefNotYetSupported)
}

func TestReconcile_EmptyFilesAndNoSourceRef_FailsInvalidSpec(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonInvalidSpec)
}

func TestReconcile_FilesMissingMainEntry_FailsInvalidSpec(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = map[string]string{"helper.libsonnet": "{}"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonInvalidSpec)
}

func TestReconcile_MissingServiceAccountAndNoDefault_FailsServiceAccountMissing(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.ServiceAccountName = ""
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c) // DefaultServiceAccount stays empty
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonServiceAccountMissing)
}

func TestReconcile_DefaultServiceAccountSatisfiesEmptySpecSA(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.ServiceAccountName = ""
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.DefaultServiceAccount = "operator-default-sa"
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
}

func TestReconcile_ExtVarConflict_FailsExternalVariableConflict(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.ExternalVariables = map[string]string{"env": "dev"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.ExtVars = map[string]string{"env": "prod"} // operator already owns "env"
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonExternalVariableConflict)
}

func TestReconcile_ExtVarsMerge_NoConflict_PassedToEval(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = map[string]string{
		"main.jsonnet": `{ env: std.extVar("env"), region: std.extVar("region") }`,
	}
	snip.Spec.ExternalVariables = map[string]string{"region": "eu-west-1"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.ExtVars = map[string]string{"env": "prod"}
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
}

func TestReconcile_EvaluationFailure_SetsReasonEvaluationFailed(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = map[string]string{"main.jsonnet": "local x ="}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonEvaluationFailed)
}

func TestReconcile_EvaluationTimeout_SetsReasonEvaluationTimeout(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = map[string]string{
		"main.jsonnet": `local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500000)`,
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.EvaluationTimeout = 1 * time.Microsecond
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonEvaluationTimeout)
}

func TestReconcile_TLAsArePassedToEval(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = map[string]string{
		"main.jsonnet": `function(env, tags) { env: env, tags: tags }`,
	}
	snip.Spec.TLAs = map[string][]string{
		"env":  {"dev"},
		"tags": {"a", "b"},
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
}

// --- Reconcile-request handling (flux reconcile) ----------------------------

func TestReconcile_StampsLastHandledReconcileAt(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Annotations = map[string]string{ReconcileRequestAnnotation: "token-1"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	runReconcile(t, r, key)

	got := refetch(t, c, key)
	assertReady(t, got, metav1.ConditionTrue, ReasonSynced)
	if got.Status.LastHandledReconcileAt != "token-1" {
		t.Errorf("LastHandledReconcileAt = %q, want %q", got.Status.LastHandledReconcileAt, "token-1")
	}
}

func TestReconcile_AnnotationOnlyUpdateReconcilesAndStampsNewToken(t *testing.T) {
	// First reconcile handles token-1; an annotation-only change to token-2
	// (Generation unchanged, since annotations don't bump it) must still be
	// picked up and recorded. This is the `flux reconcile` re-trigger path.
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Annotations = map[string]string{ReconcileRequestAnnotation: "token-1"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	runReconcile(t, r, key)
	first := refetch(t, c, key)
	if first.Status.LastHandledReconcileAt != "token-1" {
		t.Fatalf("first reconcile LastHandledReconcileAt = %q, want token-1", first.Status.LastHandledReconcileAt)
	}
	genAfterFirst := first.Generation

	// Stamp a fresh token without touching the spec.
	first.Annotations[ReconcileRequestAnnotation] = "token-2"
	if err := c.Update(context.Background(), first); err != nil {
		t.Fatalf("annotate update: %v", err)
	}
	bumped := refetch(t, c, key)
	if bumped.Generation != genAfterFirst {
		t.Fatalf("Generation moved on annotation-only update (%d -> %d); test premise broken",
			genAfterFirst, bumped.Generation)
	}

	runReconcile(t, r, key)
	second := refetch(t, c, key)
	if second.Status.LastHandledReconcileAt != "token-2" {
		t.Errorf("LastHandledReconcileAt = %q after re-trigger, want token-2", second.Status.LastHandledReconcileAt)
	}
}

// TestReconcile_FailingSnippetStampsLastHandledReconcileAt pins that a
// terminal failure still acknowledges the reconcile-request token. An operator
// who runs `flux reconcile` on a snippet stuck on a terminal reason polls
// status.lastHandledReconcileAt for completion; if failReady never stamped it,
// the CLI would report a timeout though the controller acted.
func TestReconcile_FailingSnippetStampsLastHandledReconcileAt(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil // forces ReasonInvalidSpec via failReady
	snip.Annotations = map[string]string{ReconcileRequestAnnotation: "token-1"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	runReconcile(t, r, key)

	got := refetch(t, c, key)
	assertReady(t, got, metav1.ConditionFalse, ReasonInvalidSpec)
	if got.Status.LastHandledReconcileAt != "token-1" {
		t.Errorf("LastHandledReconcileAt = %q on failing snippet, want %q",
			got.Status.LastHandledReconcileAt, "token-1")
	}
}

func TestReconcile_UnchangedTokenIsIdempotent(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Annotations = map[string]string{ReconcileRequestAnnotation: "token-1"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	runReconcile(t, r, key)
	runReconcile(t, r, key)
	got := refetch(t, c, key)
	if got.Status.LastHandledReconcileAt != "token-1" {
		t.Errorf("LastHandledReconcileAt = %q after repeated reconcile, want token-1", got.Status.LastHandledReconcileAt)
	}
}

// --- Library resolution -----------------------------------------------------

func TestReconcile_NamespacedLibrary_ResolvesAndImports(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ shared: "value" }`},
			},
		},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "u"},
	}
	snip.Spec.Files = map[string]string{
		"main.jsonnet": `(import "u") + { local _ = "unused" }`,
	}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
}

func TestReconcile_NamespacedLibrary_DefaultsImportPathToName(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ default: true }`},
			},
		},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils"}, // no ImportPath
	}
	snip.Spec.Files = map[string]string{"main.jsonnet": `(import "utils") + {}`}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
}

func TestReconcile_LibraryNotFound_FailsLibraryNotFound(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "ghost"},
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonLibraryNotFound)
}

// TestReconcile_CrossNamespaceLibraryNotFound_ScrubsMessage pins the
// privacy invariant for cross-namespace library lookups: when the
// referenced JsonnetLibrary is in a different namespace than the
// snippet, the failure message must be the constant "not reachable"
// string — same shape as the SourceRef path's scrubber. A
// namespace-specific underlying error (NotFound vs Forbidden) would
// let a tenant fingerprint other namespaces.
func TestReconcile_CrossNamespaceLibraryNotFound_ScrubsMessage(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "secret-lib", Namespace: "team-b"},
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, got, metav1.ConditionFalse, ReasonLibraryNotFound)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if !strings.Contains(cond.Message, "cross-namespace JsonnetLibrary") {
		t.Errorf("Message = %q, want cross-namespace scrubbed form", cond.Message)
	}
	if strings.Contains(cond.Message, "not found") {
		t.Errorf("Message leaks NotFound distinction: %q", cond.Message)
	}
}

func TestReconcile_SameNamespaceLibraryNotFound_UsesVerbatimMessage(t *testing.T) {
	// Sibling test: same-namespace failures stay verbatim. Confirms
	// the cross-ns scrub doesn't accidentally apply to the tenant's
	// own namespace, where the diagnostic detail is the tenant's to
	// see.
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "ghost"},
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if !strings.Contains(cond.Message, "not found") {
		t.Errorf("Message = %q, want verbatim 'not found' for same-namespace miss", cond.Message)
	}
	if strings.Contains(cond.Message, "cross-namespace") {
		t.Errorf("Same-namespace miss incorrectly scrubbed: %q", cond.Message)
	}
}

func TestReconcile_LibrarySourceRef_FailsLibrarySourceUnresolved(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "x"},
			},
		},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils"},
	}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonSourceRefNotYetSupported)
}

func TestReconcile_EmptyLibraryFilesAndNoSourceRef_FailsInvalidSpec(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: "team-a"},
		Spec:       jaasv1.JsonnetLibrarySpec{},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "broken"},
	}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonInvalidSpec)
}

func TestReconcile_CrossNamespaceRef_RejectedWhenFlagOn(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "other-team"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{Files: map[string]string{"main.libsonnet": "{}"}},
		},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils", Namespace: "other-team"},
	}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	r.NoCrossNamespaceRefs = true
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonCrossNamespaceRefRejected)
}

func TestReconcile_CrossNamespaceRef_AllowedWhenFlagOff(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "other-team"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ from: "elsewhere" }`},
			},
		},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils", Namespace: "other-team", ImportPath: "u"},
	}
	snip.Spec.Files = map[string]string{"main.jsonnet": `(import "u") + {}`}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	r.NoCrossNamespaceRefs = false
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
}

func TestReconcile_UnknownLibraryKind_FailsInvalidSpec(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "ConfigMap", Name: "anything"},
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonInvalidSpec)
}

// --- Error propagation through the API client ------------------------------

func TestReconcile_GetErrorPropagates(t *testing.T) {
	want := errors.New("apiserver flaky")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return want
			},
		}).Build()
	r := newReconciler(t, c)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "demo", Namespace: "team-a"},
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v to propagate", err, want)
	}
}

func TestReconcile_UpdateErrorOnFinalizerAddPropagates(t *testing.T) {
	snip := sampleSnippet() // no finalizer
	want := errors.New("conflict")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(snip).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return want
			},
		}).Build()
	r := newReconciler(t, c)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestReconcile_StatusUpdateErrorPropagates(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	want := errors.New("status write failed")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return want
			},
		}).Build()
	r := newReconciler(t, c)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestReconcile_DeleteUpdateErrorPropagates(t *testing.T) {
	snip := sampleSnippet()
	now := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &now
	snip.Finalizers = []string{FinalizerName}
	want := errors.New("conflict on finalizer drop")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(snip).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return want
			},
		}).Build()
	r := newReconciler(t, c)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestReconcile_LibraryGetErrorPropagates(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils"},
	}
	want := errors.New("apiserver oops")
	calls := 0
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				calls++
				// First call is for the snippet itself; let it through.
				if calls == 1 {
					return c.Get(ctx, k, o, opts...)
				}
				return want
			},
		}).Build()
	r := newReconciler(t, c)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestReconcile_ContextCancellationDuringEvalPropagatesError(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = map[string]string{
		"main.jsonnet": `local f(n) = if n == 0 then 0 else f(n-1) + 1; f(1000000)`,
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: the eval goroutine sees Canceled immediately

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestReconcile_FailReady_StatusUpdateErrorPropagates(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil // forces ReasonInvalidSpec via failReady
	want := errors.New("status write failed in failure path")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return want
			},
		}).Build()
	r := newReconciler(t, c)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// --- Rate limiter -----------------------------------------------------------

func TestReconcile_RateLimited_ReturnsRequeueAfter(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	// Burst 1 ⇒ first reconcile passes, second is rate-limited.
	r.Limiter = NewRateLimiter(1.0, 1)

	res1, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if res1.RequeueAfter != 0 {
		t.Errorf("first reconcile RequeueAfter = %v, want 0", res1.RequeueAfter)
	}

	res2, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if res2.RequeueAfter == 0 {
		t.Errorf("second reconcile RequeueAfter = 0, want > 0")
	}
}

func TestReconcile_EvalUnavailable_ReturnsRequeueAfter(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)

	eval.SetMaxConcurrentEvals(1)
	t.Cleanup(func() { eval.SetMaxConcurrentEvals(0) })
	release, ok := eval.Reserve()
	if !ok {
		t.Fatal("could not pin the baseline eval slot")
	}
	defer release()

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("RequeueAfter = 0, want > 0 — backpressure path must defer, not fail")
	}

	// Ready condition must NOT be flipped — backpressure is not a failure.
	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady); cond != nil && cond.Status == metav1.ConditionFalse {
		t.Errorf("Ready=False after eval-unavailable; should stay untouched (reason=%s)", cond.Reason)
	}
}

// TestReconcileDelete_NoFinalizer_StillForgetsCaches pins that a snippet
// that hits reconcileDelete with our finalizer absent (transient 5xx
// during finalizer-add, prior force-drop, manual edit) must still evict
// its per-snippet cache entries. A reconcileDelete that returned early
// here would leak those entries until process restart on high-churn
// workloads.
//
// The fake apiserver refuses to construct an object that carries a
// DeletionTimestamp but no finalizers (it'd be GC'd immediately), so we
// drive reconcileDelete directly — that's the unit-level boundary where
// the early-return lives.
func TestReconcileDelete_NoFinalizer_StillForgetsCaches(t *testing.T) {
	snip := sampleSnippet()
	snip.UID = types.UID("uid-leak")
	snip.Finalizers = nil // load-bearing: no jaas finalizer
	r := newReconciler(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build())
	r.Limiter = NewRateLimiter(0.01, 1)
	r.CycleCache = newCycleCache()

	// Seed both caches to simulate state accumulated on prior reconciles.
	r.Limiter.Reserve(snip.Namespace + "/" + snip.Name)
	r.CycleCache.Store(snip.UID, snip.Generation, 0, false, "no cycle")

	if _, err := r.reconcileDelete(context.Background(), r.logger(), snip); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	// Rate-limit bucket evicted: a fresh Reserve must succeed even though
	// the previous one drained the burst.
	if ok, _ := r.Limiter.Reserve(snip.Namespace + "/" + snip.Name); !ok {
		t.Error("Limiter.Forget did not run on the no-finalizer path; rate-limit bucket still drained")
	}
	// Cycle cache evicted: the verdict we stored above must be gone.
	if _, _, ok := r.CycleCache.Lookup(snip.UID, snip.Generation); ok {
		t.Error("CycleCache.Forget did not run on the no-finalizer path; verdict still cached")
	}
}

func TestReconcile_DeletionForgetsRateLimitBucket(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Limiter = NewRateLimiter(0.01, 1)

	// Drain the bucket.
	r.Limiter.Reserve(snip.Namespace + "/" + snip.Name)

	// Delete the snippet.
	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	if err := c.Delete(context.Background(), got); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	}); err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}

	// After Forget, the next Reserve should succeed immediately.
	if ok, _ := r.Limiter.Reserve(snip.Namespace + "/" + snip.Name); !ok {
		t.Errorf("Reserve after Forget was denied")
	}
}

// --- Publisher integration --------------------------------------------------

func TestReconcile_HappyPathWithPublisher_CreatesExternalArtifact(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		Build()

	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)

	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

	gotSnip := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, gotSnip, metav1.ConditionTrue, ReasonSynced)
	if !strings.HasPrefix(gotSnip.Status.Revision, "sha256:") {
		t.Errorf("snippet.status.revision = %q, want sha256: prefix", gotSnip.Status.Revision)
	}

	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}, ea); err != nil {
		t.Fatalf("ExternalArtifact not created: %v", err)
	}
	gotURL, _, _ := unstructured.NestedString(ea.Object, "status", "artifact", "url")
	if gotURL == "" {
		t.Errorf("status.artifact.url is empty")
	}
}

func TestReconcile_PublishErrorPropagates(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	want := errors.New("create EA failed")
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
				return want
			},
		}).Build()

	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestReconcile_DeletionWithPublisher_WithdrawSucceeds(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	// Seed: publish first so the EA exists.
	if _, err := r.Publisher.Publish(context.Background(), c, snip, `{}`, nil, nil); err != nil {
		t.Fatalf("seed Publish: %v", err)
	}
	// Issue a Delete on the snippet: the finalizer keeps it alive with
	// DeletionTimestamp set, which is what Reconcile observes.
	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	if err := c.Delete(context.Background(), got); err != nil {
		t.Fatalf("delete snippet: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	}); err != nil {
		t.Fatalf("Reconcile on deletion: %v", err)
	}

	// The ExternalArtifact must be gone.
	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}, ea); !apierrors.IsNotFound(err) {
		t.Errorf("ExternalArtifact still present after deletion: %v", err)
	}
}

func TestReconcile_DeletionWithdrawErrorRequeues(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	now := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &now

	want := errors.New("delete denied")
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return want
			},
		}).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	// Pin r.now() inside the MaxWithdrawWait window so the requeue
	// path is exercised rather than the new force-drop fallback.
	r.Clock = func() time.Time { return snip.DeletionTimestamp.Time.Add(1 * time.Minute) }

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, want) {
		t.Errorf("got %v, want %v wrapped", err, want)
	}
}

// TestReconcileDelete_ErrorPathStillForgetsCycleVerdict pins that the cycle
// verdict is evicted on a delete whose Withdraw fails and requeues — even
// though the full forgetPerSnippetCaches (which would re-mint the SA token on
// recovery) is deliberately skipped on that error path. The cycle verdict is
// keyed by a never-reused UID with no TTL, so leaving it pinned across a
// repeatedly-failing delete would leak it.
func TestReconcileDelete_ErrorPathStillForgetsCycleVerdict(t *testing.T) {
	snip := sampleSnippet()
	snip.UID = types.UID("uid-delete-err")
	snip.Finalizers = []string{FinalizerName}
	now := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &now

	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return errors.New("withdraw denied")
			},
		}).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	r.Limiter = NewRateLimiter(0.01, 1)
	r.CycleCache = newCycleCache()
	// Stay inside the MaxWithdrawWait window so the requeue (error) path runs.
	r.Clock = func() time.Time { return snip.DeletionTimestamp.Time.Add(1 * time.Minute) }

	// Seed both per-snippet caches.
	r.Limiter.Reserve(snip.Namespace + "/" + snip.Name)
	r.CycleCache.Store(snip.UID, snip.Generation, 0, false, "no cycle")

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	}); err == nil {
		t.Fatal("expected the Withdraw error to requeue, got nil")
	}

	// Cycle verdict must be gone — the unconditional Forget at the top of
	// reconcileDelete runs even on this error path.
	if _, _, ok := r.CycleCache.Lookup(snip.UID, snip.Generation); ok {
		t.Error("cycle verdict still cached after a failing delete; it leaks")
	}
	// The rate-limit bucket must still be drained — forgetPerSnippetCaches is
	// intentionally skipped on the error path (so a recovering snippet doesn't
	// re-mint its token), confirming this is the targeted cycle-only eviction.
	if ok, _ := r.Limiter.Reserve(snip.Namespace + "/" + snip.Name); ok {
		t.Error("rate-limit bucket was forgotten on the error path; expected it to persist")
	}
}

// Regression: a permanently-broken Withdraw (e.g. S3 perma-down, revoked
// RBAC, deleted bucket) would otherwise pin the snippet in Terminating
// forever and block namespace deletion. After MaxWithdrawWait elapses
// since DeletionTimestamp, reconcileDelete force-drops the finalizer,
// emits a Warning WithdrawForced event, and lets the snippet be GC'd —
// trading an orphan tarball for unblocking the cluster.
func TestReconcile_DeletionWithdrawTimeoutForceDrops(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	deletedAt := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &deletedAt

	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				// Only fail the ExternalArtifact delete (the call inside
				// Publisher.Withdraw). Letting the snippet's own Update
				// land is what makes the force-drop assertable —
				// otherwise the finalizer would never come off.
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == externalArtifactGVK {
					return errors.New("simulated S3 perma-down")
				}
				return nil
			},
		}).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	// Set MaxWithdrawWait to 30m and pin r.now() to 1h past
	// DeletionTimestamp — the elapsed time exceeds the bound, so the
	// reconciler must force-drop rather than requeue.
	r.MaxWithdrawWait = 30 * time.Minute
	r.Clock = func() time.Time { return deletedAt.Time.Add(1 * time.Hour) }
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("force-drop reconcile returned error: %v", err)
	}

	// The snippet's finalizer should be gone — that's the whole
	// point. Either the object is fully deleted (interceptor allowed
	// the snippet's Update with empty finalizers to propagate, after
	// which the apiserver GC's it) or the finalizer slice is empty.
	got := &jaasv1.JsonnetSnippet{}
	err := c.Get(context.Background(), key, got)
	switch {
	case apierrors.IsNotFound(err):
		// Acceptable — the GC ran after the finalizer was dropped.
	case err != nil:
		t.Fatalf("refetch after force-drop: %v", err)
	default:
		if len(got.Finalizers) != 0 {
			t.Errorf("finalizer not dropped: %v", got.Finalizers)
		}
	}

	// And a Warning WithdrawForced event must have been emitted with
	// the elapsed duration in its message.
	gotEvents := drainEvents(rec)
	var found bool
	for _, ev := range gotEvents {
		if strings.HasPrefix(ev, "Warning WithdrawForced") {
			found = true
			if !strings.Contains(ev, "1h0m0s") {
				t.Errorf("WithdrawForced event missing elapsed duration: %q", ev)
			}
			break
		}
	}
	if !found {
		t.Errorf("no Warning WithdrawForced event among %v", gotEvents)
	}
}

// TestReconcile_DeletionWithdrawForbiddenForceDropsImmediately pins
// the permanent-error classification on the deletion path: a
// Forbidden response from the ExternalArtifact Delete (revoked RBAC,
// missing CRD) won't heal by retry, so the reconciler force-drops
// the finalizer immediately rather than pinning the snippet in
// Terminating for MaxWithdrawWait — the orphan-tarball cost is the
// same either way, but the cluster gets unblocked at once.
// TestReconcileDelete_TenantClientForbiddenForceDropsImmediately pins
// a permanent apiserver error from the tenantClient build path
// (Forbidden on the SA TokenRequest after the operator's RBAC was
// narrowed) must drop the finalizer at once, the same way a permanent
// Withdraw error does. Pre-fix this branch fell into
// `return ctrl.Result{}, err`, pinning the snippet in Terminating for
// the full MaxWithdrawWait (1h default) even though retries couldn't
// possibly heal.
func TestReconcileDelete_TenantClientForbiddenForceDropsImmediately(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	deletedAt := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &deletedAt

	forbiddenErr := apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "serviceaccounts/token"},
		"tenant",
		errors.New("RBAC: forbidden"),
	)
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	r.RestConfig = &rest.Config{Host: "http://example.test"}
	r.TokenCache = newTokenCache(&stubMinter{err: forbiddenErr})
	r.Clock = func() time.Time { return deletedAt.Time.Add(1 * time.Second) }
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("force-drop reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 (force-drop is terminal)", res.RequeueAfter)
	}

	got := &jaasv1.JsonnetSnippet{}
	getErr := c.Get(context.Background(), key, got)
	switch {
	case apierrors.IsNotFound(getErr):
		// Acceptable — GC ran after the finalizer was dropped.
	case getErr != nil:
		t.Fatalf("refetch after force-drop: %v", getErr)
	default:
		if len(got.Finalizers) != 0 {
			t.Errorf("finalizer not dropped: %v", got.Finalizers)
		}
	}

	gotEvents := drainEvents(rec)
	var foundWithdrawForced bool
	for _, e := range gotEvents {
		if strings.Contains(e, "WithdrawForced") {
			foundWithdrawForced = true
			break
		}
	}
	if !foundWithdrawForced {
		t.Errorf("expected a WithdrawForced event from force-drop; got %v", gotEvents)
	}
}

func TestReconcile_DeletionWithdrawForbiddenForceDropsImmediately(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	deletedAt := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &deletedAt

	forbiddenErr := apierrors.NewForbidden(
		schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "externalartifacts"},
		snip.Name,
		errors.New("RBAC: forbidden"),
	)
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == externalArtifactGVK {
					return forbiddenErr
				}
				return nil
			},
		}).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	// MaxWithdrawWait stays at default (1h) and r.now() is pinned just
	// 1 second past DeletionTimestamp. The timeout path would NOT
	// trigger here — only the permanent-error fast path can drop the
	// finalizer.
	r.Clock = func() time.Time { return deletedAt.Time.Add(1 * time.Second) }
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("force-drop reconcile returned error: %v", err)
	}

	got := &jaasv1.JsonnetSnippet{}
	err := c.Get(context.Background(), key, got)
	switch {
	case apierrors.IsNotFound(err):
		// Acceptable — GC ran after the finalizer was dropped.
	case err != nil:
		t.Fatalf("refetch after force-drop: %v", err)
	default:
		if len(got.Finalizers) != 0 {
			t.Errorf("finalizer not dropped: %v", got.Finalizers)
		}
	}

	gotEvents := drainEvents(rec)
	var found bool
	for _, ev := range gotEvents {
		if strings.HasPrefix(ev, "Warning WithdrawForced") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no Warning WithdrawForced event among %v", gotEvents)
	}
}

// TestReconcileDelete_ForceDropMetricCountsOnceAcrossFailedUpdate pins that the
// force-drop metric fires once per actual drop, not once per failed
// finalizer-removal Update retry — otherwise a flapping apiserver inflates the
// broken-pipeline alert metric.
func TestReconcileDelete_ForceDropMetricCountsOnceAcrossFailedUpdate(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	deletedAt := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &deletedAt

	forbiddenErr := apierrors.NewForbidden(
		schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "externalartifacts"},
		snip.Name, errors.New("RBAC: forbidden"),
	)

	var updateCalls int
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}, func() client.Object {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(externalArtifactGVK)
			return u
		}()).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == externalArtifactGVK {
					return forbiddenErr
				}
				return nil
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*jaasv1.JsonnetSnippet); ok {
					updateCalls++
					if updateCalls == 1 {
						return apierrors.NewConflict(schema.GroupResource{Resource: "jsonnetsnippets"}, snip.Name, errors.New("conflict"))
					}
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	r.Clock = func() time.Time { return deletedAt.Time.Add(1 * time.Second) }
	r.EventRecorder = events.NewFakeRecorder(8)

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	labels := []string{snip.Namespace, snip.Name, "withdraw_permanent"}
	before := testutil.ToFloat64(snippetForceDropTotal.WithLabelValues(labels...))

	// Round 1: Withdraw forbidden → force-drop decided, but the finalizer Update
	// fails → error returned and NOTHING emitted.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("expected the failed finalizer Update to surface as an error")
	}
	if mid := testutil.ToFloat64(snippetForceDropTotal.WithLabelValues(labels...)); mid != before {
		t.Errorf("force-drop metric moved on a failed-Update reconcile: before=%v mid=%v", before, mid)
	}

	// Round 2: retry. Update succeeds → force-drop emitted exactly once.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	if after := testutil.ToFloat64(snippetForceDropTotal.WithLabelValues(labels...)); after-before != 1 {
		t.Errorf("force-drop metric delta = %v, want exactly 1 across the failed+successful Update", after-before)
	}
}

// withdrawTimedOut sanity-checks the helper directly. The decision logic
// is straightforward enough that the integration test above covers most
// of it, but the boundary cases (DeletionTimestamp zero, MaxWithdrawWait
// zero falls back to default) are worth pinning explicitly.
func TestWithdrawTimedOut_BoundaryCases(t *testing.T) {
	fixed := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	t.Run("zero DeletionTimestamp never times out", func(t *testing.T) {
		r := &SnippetReconciler{MaxWithdrawWait: 1 * time.Minute, Clock: func() time.Time { return fixed.Add(1 * time.Hour) }}
		snip := &jaasv1.JsonnetSnippet{}
		if timedOut, _ := r.withdrawTimedOut(snip); timedOut {
			t.Error("zero DeletionTimestamp should not time out")
		}
	})
	t.Run("MaxWithdrawWait zero uses the default", func(t *testing.T) {
		r := &SnippetReconciler{Clock: func() time.Time { return fixed.Add(2 * time.Hour) }}
		snip := &jaasv1.JsonnetSnippet{}
		snip.DeletionTimestamp = &metav1.Time{Time: fixed}
		if timedOut, _ := r.withdrawTimedOut(snip); !timedOut {
			t.Error("2h elapsed must exceed the 1h default")
		}
	})
	t.Run("elapsed below the bound does not time out", func(t *testing.T) {
		r := &SnippetReconciler{MaxWithdrawWait: 1 * time.Hour, Clock: func() time.Time { return fixed.Add(30 * time.Minute) }}
		snip := &jaasv1.JsonnetSnippet{}
		snip.DeletionTimestamp = &metav1.Time{Time: fixed}
		if timedOut, _ := r.withdrawTimedOut(snip); timedOut {
			t.Error("30m elapsed must not exceed a 1h bound")
		}
	})
}

// --- Fetcher integration ----------------------------------------------------

// stubFetcher satisfies SourceFetcher with canned results. Tests inject
// either a success body (result != nil) or an error shape (err != nil); the
// stubFetcher records call count so tests can assert it was/wasn't reached.
type stubFetcher struct {
	result *sources.Result
	err    error
	calls  int
}

func (s *stubFetcher) Fetch(_ context.Context, _ client.Client, _ *jaasv1.SourceRef, _ string) (*sources.Result, error) {
	s.calls++
	return s.result, s.err
}

func TestReconcile_SnippetSourceRef_WiredFetcher_HappyPath(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{
		Kind: "GitRepository", Name: "configs", Namespace: snip.Namespace,
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Fetcher = &stubFetcher{
		result: &sources.Result{
			Files:    map[string]string{"main.jsonnet": `{ from: "source" }`},
			Revision: "rev1",
		},
	}
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
}

// runReconcileTransient runs Reconcile and expects an error — used for
// transient classifications (source-not-ready, network blip) where the
// reconciler writes the failure status AND returns the error so
// controller-runtime engages exponential backoff.
func runReconcileTransient(t *testing.T, r *SnippetReconciler, key types.NamespacedName) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("expected transient error to engage backoff, got nil")
	}
}

func TestReconcile_SnippetSourceRef_FetcherSourceNotReady_SetsSourceNotReady(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Fetcher = &stubFetcher{err: sources.ErrSourceNotReady}
	runReconcileTransient(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonSourceNotReady)
}

func TestReconcile_SnippetSourceRef_FetcherArtifactMissing_SetsSourceNotReady(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Fetcher = &stubFetcher{err: sources.ErrArtifactMissing}
	runReconcileTransient(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonSourceNotReady)
}

// Digest mismatch is NON-transient — corruption can't be recovered by
// retry. Status flips to Ready=False/SourceFetchFailed and the
// reconcile returns nil (no backoff engaged).
func TestReconcile_SnippetSourceRef_FetcherDigestMismatch_SetsSourceFetchFailed(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Fetcher = &stubFetcher{err: sources.ErrDigestMismatch}
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonSourceFetchFailed)
}

// Generic / unclassified fetch errors are treated as transient — better
// to burn a few backoff cycles on a misclassified permanent failure
// than silently swallow a real transient one. Reconcile returns the
// error to engage backoff.
func TestReconcile_SnippetSourceRef_FetcherGenericErr_SetsSourceFetchFailed(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Fetcher = &stubFetcher{err: errors.New("network down")}
	runReconcileTransient(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonSourceFetchFailed)
}

func TestReconcile_LibrarySourceRef_WiredFetcher_HappyPath(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "lib-source"},
			},
		},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "u"},
	}
	snip.Spec.Files = map[string]string{
		"main.jsonnet": `(import "u") + { wrap: true }`,
	}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	r.Fetcher = &stubFetcher{
		result: &sources.Result{
			Files: map[string]string{"main.libsonnet": `{ from: "fetched-lib" }`},
		},
	}
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
}

func TestReconcile_SnippetSourceRef_FetchedSource_MissingMainJsonnet(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Fetcher = &stubFetcher{
		result: &sources.Result{Files: map[string]string{"helper.libsonnet": "{}"}},
	}
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonInvalidSpec)
}

// --- Dependency cycle detection ---------------------------------------------

func TestReconcile_SelfReferenceCycle_SetsDependencyCycle(t *testing.T) {
	snip := snippetPointingAt("a", "team-a", "a", "team-a")
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.ServiceAccountName = "tenant"
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, got, metav1.ConditionFalse, ReasonDependencyCycle)
}

func TestReconcile_TwoSnippetCycle_BothFail(t *testing.T) {
	a := snippetPointingAt("a", "team-a", "b", "team-a")
	a.Finalizers = []string{FinalizerName}
	a.Spec.ServiceAccountName = "tenant"
	b := snippetPointingAt("b", "team-a", "a", "team-a")
	b.Finalizers = []string{FinalizerName}
	b.Spec.ServiceAccountName = "tenant"
	c := clientWithStatus(t, a, b)
	r := newReconciler(t, c)
	runReconcile(t, r, types.NamespacedName{Name: "a", Namespace: "team-a"})
	gotA := refetch(t, c, types.NamespacedName{Name: "a", Namespace: "team-a"})
	assertReady(t, gotA, metav1.ConditionFalse, ReasonDependencyCycle)
}

func TestReconcile_CycleErrorPropagatesAsTransient(t *testing.T) {
	// A flaky apiserver during cycle detection should bubble up as a
	// transient err (caller requeues), not silently mark Ready=False.
	a := snippetPointingAt("a", "team-a", "b", "team-a")
	a.Finalizers = []string{FinalizerName}
	a.Spec.ServiceAccountName = "tenant"
	want := errors.New("apiserver hiccup")
	calls := 0
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(a).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				calls++
				if calls == 1 { // initial reconcile fetch
					return c.Get(ctx, k, o, opts...)
				}
				return want // subsequent Get during cycle walk fails
			},
		}).Build()
	r := newReconciler(t, c)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "a", Namespace: "team-a"},
	})
	if !errors.Is(err, want) {
		t.Errorf("got %v, want %v wrapped", err, want)
	}
}

// --- crossNamespaceSourceRef helper -----------------------------------------

func TestCrossNamespaceSourceRef_FlagOffAllowsAnything(t *testing.T) {
	r := &SnippetReconciler{NoCrossNamespaceRefs: false}
	reason, _ := r.crossNamespaceSourceRef("snip-ns",
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "x", Namespace: "elsewhere"})
	if reason != "" {
		t.Errorf("got reason %q, want \"\" when flag is off", reason)
	}
}

func TestCrossNamespaceSourceRef_NilRefAllowsAnything(t *testing.T) {
	r := &SnippetReconciler{NoCrossNamespaceRefs: true}
	reason, _ := r.crossNamespaceSourceRef("snip-ns", nil)
	if reason != "" {
		t.Errorf("got reason %q, want \"\" for nil ref", reason)
	}
}

func TestCrossNamespaceSourceRef_EmptyNamespaceDefaultsToOwnerSilently(t *testing.T) {
	r := &SnippetReconciler{NoCrossNamespaceRefs: true}
	reason, _ := r.crossNamespaceSourceRef("snip-ns",
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "x"})
	if reason != "" {
		t.Errorf("got reason %q, want \"\" for empty Namespace (defaults to owner)", reason)
	}
}

func TestCrossNamespaceSourceRef_SameNamespacePasses(t *testing.T) {
	r := &SnippetReconciler{NoCrossNamespaceRefs: true}
	reason, _ := r.crossNamespaceSourceRef("snip-ns",
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "x", Namespace: "snip-ns"})
	if reason != "" {
		t.Errorf("got reason %q, want \"\" for same-namespace", reason)
	}
}

func TestCrossNamespaceSourceRef_DifferentNamespaceRejected(t *testing.T) {
	r := &SnippetReconciler{NoCrossNamespaceRefs: true}
	reason, msg := r.crossNamespaceSourceRef("snip-ns",
		&jaasv1.SourceRef{Kind: "GitRepository", Name: "x", Namespace: "elsewhere"})
	if reason != ReasonCrossNamespaceRefRejected {
		t.Errorf("got reason %q, want %q", reason, ReasonCrossNamespaceRefRejected)
	}
	if !strings.Contains(msg, "elsewhere") {
		t.Errorf("message %q does not mention the offending namespace", msg)
	}
}

// --- cross-namespace SourceRef integration with reconcileSpec / libraries ---

func TestReconcile_SnippetSourceRef_CrossNamespace_FlagOn_Rejected(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{
		Kind: "GitRepository", Name: "configs", Namespace: "other-tenant",
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.NoCrossNamespaceRefs = true
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonCrossNamespaceRefRejected)
}

func TestReconcile_SnippetSourceRef_CrossNamespace_FlagOff_FallsThroughToNotYetSupported(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{
		Kind: "GitRepository", Name: "configs", Namespace: "other-tenant",
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.NoCrossNamespaceRefs = false
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonSourceRefNotYetSupported)
}

func TestReconcile_SnippetSourceRef_SameNamespace_FlagOn_FallsThroughToNotYetSupported(t *testing.T) {
	// Same namespace ⇒ cross-ns check passes ⇒ NotYetSupported fires.
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{
		Kind: "GitRepository", Name: "configs", Namespace: snip.Namespace,
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.NoCrossNamespaceRefs = true
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonSourceRefNotYetSupported)
}

func TestReconcile_LibrarySourceRef_CrossNamespace_FlagOn_Rejected(t *testing.T) {
	// JsonnetLibrary lives in the SAME namespace as the snippet (so the
	// LibraryRef itself passes the cross-ns check), but its OWN spec.sourceRef
	// points to a different namespace.
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "GitRepository", Name: "x", Namespace: "elsewhere",
				},
			},
		},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils"},
	}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	r.NoCrossNamespaceRefs = true
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonCrossNamespaceRefRejected)
}

func TestReconcile_LibrarySourceRef_CrossNamespace_FlagOff_FallsThroughToLibrarySourceUnresolved(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "GitRepository", Name: "x", Namespace: "elsewhere",
				},
			},
		},
	}
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "utils"},
	}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	r.NoCrossNamespaceRefs = false
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionFalse, ReasonSourceRefNotYetSupported)
}

// --- Impersonation helper ---------------------------------------------------

func TestEffectiveServiceAccount_PrefersSpec(t *testing.T) {
	r := &SnippetReconciler{DefaultServiceAccount: "default-sa"}
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{ServiceAccountName: "spec-sa"}}
	if got := r.effectiveServiceAccount(snip); got != "spec-sa" {
		t.Errorf("got %q, want spec-sa", got)
	}
}

func TestEffectiveServiceAccount_FallsBackToDefault(t *testing.T) {
	r := &SnippetReconciler{DefaultServiceAccount: "default-sa"}
	snip := &jaasv1.JsonnetSnippet{}
	if got := r.effectiveServiceAccount(snip); got != "default-sa" {
		t.Errorf("got %q, want default-sa", got)
	}
}

func TestEffectiveServiceAccount_EmptyWhenNeitherSet(t *testing.T) {
	r := &SnippetReconciler{}
	if got := r.effectiveServiceAccount(&jaasv1.JsonnetSnippet{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestTenantClient_NilRestConfigReturnsBareClient(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &SnippetReconciler{Client: c, Scheme: testScheme(t)} // RestConfig nil
	got, err := r.tenantClient(context.Background(), &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c {
		t.Errorf("got different client, expected the bare manager client")
	}
}

func TestTenantClient_NoEffectiveSAFallsBackToBareClient(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &SnippetReconciler{Client: c, Scheme: testScheme(t), RestConfig: &rest.Config{Host: "http://example"}}
	got, err := r.tenantClient(context.Background(), &jaasv1.JsonnetSnippet{}) // empty spec, no default
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c {
		t.Errorf("got different client, expected the bare manager client")
	}
}

// stubMinter is a programmable tokenMinter for unit tests. It returns either
// a stamped token+expiry from `token` and `expires`, or `err` if non-nil.
type stubMinter struct {
	token   string
	expires time.Time
	err     error
	calls   int
}

func (s *stubMinter) Mint(_ context.Context, _, _ string, _ time.Duration) (string, time.Time, error) {
	s.calls++
	if s.err != nil {
		return "", time.Time{}, s.err
	}
	return s.token, s.expires, nil
}

func TestTenantClient_BuildsImpersonatingClient(t *testing.T) {
	// With RestConfig + TokenCache both set, the helper mints a token and
	// stamps it on a clone of the rest config — yielding a distinct client.
	stub := &stubMinter{token: "fake-jwt", expires: time.Now().Add(1 * time.Hour)}
	r := &SnippetReconciler{
		Client:     fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:     testScheme(t),
		RestConfig: &rest.Config{Host: "http://example.test"},
		TokenCache: newTokenCache(stub),
	}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}
	got, err := r.tenantClient(context.Background(), snip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == r.Client {
		t.Errorf("tenant client must be distinct from the bare client when RestConfig is set")
	}
	if stub.calls != 1 {
		t.Errorf("stub.calls = %d, want 1 (token minted exactly once)", stub.calls)
	}
}

// TestBuildTenantRestConfig_PinsInsecureFalse pins the security
// invariant: a dev kubeconfig with `insecure-skip-tls-verify: true`
// on the operator's own connection must NOT propagate to tenant API
// calls. Each tenant-scoped Get/Patch traverses RBAC for a specific
// SA; verifying the apiserver's cert under that scope is the only
// defense against a compromised in-cluster MITM intercepting the
// bearer token.
func TestBuildTenantRestConfig_PinsInsecureFalse(t *testing.T) {
	operatorCfg := &rest.Config{
		Host: "https://example.test",
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true, // simulates a dev kubeconfig
		},
	}
	got := buildTenantRestConfig(operatorCfg, "tok")
	if got.TLSClientConfig.Insecure {
		t.Error("tenant rest.Config inherited Insecure=true from the operator's config; the operator's choice to skip verification must not flow into a tenant-scoped call")
	}
	if got.BearerToken != "tok" {
		t.Errorf("BearerToken = %q, want tok", got.BearerToken)
	}
	// And the operator's own config must remain unchanged — the
	// helper must clone before mutating.
	if !operatorCfg.TLSClientConfig.Insecure {
		t.Error("buildTenantRestConfig mutated the operator's input config")
	}
}

func TestTenantClient_ClientNewErrorPropagates(t *testing.T) {
	// Successful Mint, but a malformed Host on the rest.Config makes
	// client.New fail. The Mint result is discarded; the error bubbles up.
	stub := &stubMinter{token: "fake-jwt", expires: time.Now().Add(1 * time.Hour)}
	r := &SnippetReconciler{
		Client:     fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:     testScheme(t),
		RestConfig: &rest.Config{Host: "://malformed"},
		TokenCache: newTokenCache(stub),
	}
	if _, err := r.tenantClient(context.Background(), &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}); err == nil {
		t.Errorf("expected client.New failure with malformed Host, got nil")
	}
}

func TestTenantClient_MintErrorPropagates(t *testing.T) {
	want := errors.New("token request denied")
	r := &SnippetReconciler{
		Client:     fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:     testScheme(t),
		RestConfig: &rest.Config{Host: "http://example.test"},
		TokenCache: newTokenCache(&stubMinter{err: want}),
	}
	if _, err := r.tenantClient(context.Background(), &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestTenantClient_NilTokenCacheFallsBackToBareClient(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := &SnippetReconciler{
		Client:     c,
		Scheme:     testScheme(t),
		RestConfig: &rest.Config{Host: "http://example.test"},
		// TokenCache deliberately nil — both halves of the impersonation
		// pair must be set for it to engage.
	}
	got, err := r.tenantClient(context.Background(), &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != c {
		t.Errorf("got different client, expected the bare manager client")
	}
}

func TestReconcile_TenantClientErrorOnSpecPropagates(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.RestConfig = &rest.Config{Host: "http://example.test"}
	r.TokenCache = newTokenCache(&stubMinter{err: errors.New("token denied")})

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if err == nil || !strings.Contains(err.Error(), "build impersonation client") {
		t.Errorf("got %v, want a 'build impersonation client' error", err)
	}
}

// TestReconcile_TenantClientPermanentErrorFailsReady pins that a
// permanent apiserver error from the tenantClient build (Forbidden on
// SA TokenRequest, NoMatch on the SA kind) must NOT bubble to
// controller-runtime as a transient error. It must surface as Ready=
// False/RBACDenied with an actionable message naming what the cluster
// operator needs to grant. Mirrors the deletion-path fix.
func TestReconcile_TenantClientPermanentErrorFailsReady(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.RestConfig = &rest.Config{Host: "http://example.test"}
	forbiddenErr := apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "serviceaccounts/token"},
		"tenant",
		errors.New("RBAC: forbidden"),
	)
	r.TokenCache = newTokenCache(&stubMinter{err: forbiddenErr})

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if err != nil {
		t.Fatalf("got %v, want nil — permanent error must route to failReady, not bubble", err)
	}
	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, got, metav1.ConditionFalse, ReasonRBACDenied)
}

// TestReconcile_CycleDetectionPermanentErrorFailsReady pins that a permanent
// apiserver error during the dependency-cycle BFS — the concrete case is a
// snippet referencing kind: ExternalArtifact in a cluster without
// source-controller's CRD, yielding a NoMatchError — surfaces as a terminal
// Ready=False/RBACDenied rather than bubbling up raw and stranding the snippet
// in infinite backoff with an empty Ready status.
func TestReconcile_CycleDetectionPermanentErrorFailsReady(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	// An ExternalArtifact sourceRef makes hasCycleSourceEdge true so the BFS
	// actually walks; the dependency Get is what we fault.
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "upstream"}

	noMatch := &apimeta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "source.toolkit.fluxcd.io", Kind: "ExternalArtifact"},
		SearchedVersions: []string{"v1"},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				// Fault only the BFS's dependency Get (the "upstream"
				// snippet), leaving the reconcile's own snippet Get intact.
				if key.Name == "upstream" {
					return noMatch
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := newReconciler(t, c)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if err != nil {
		t.Fatalf("got %v, want nil — permanent cycle-walk error must route to failReady, not bubble", err)
	}
	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, got, metav1.ConditionFalse, ReasonRBACDenied)
}

// TestReconcile_CycleDetectionTransientErrorWritesStatusAndRequeues pins that
// a transient apiserver error during the cycle BFS still writes an actionable
// Ready=False (so kubectl describe shows something, not empty status) AND
// returns the error so controller-runtime engages backoff.
func TestReconcile_CycleDetectionTransientErrorWritesStatusAndRequeues(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "upstream"}

	flaky := errors.New("etcd leader election in progress")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == "upstream" {
					return flaky
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := newReconciler(t, c)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if !errors.Is(err, flaky) {
		t.Fatalf("got %v, want the transient error to propagate for backoff", err)
	}
	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition not written; transient cycle error left empty status")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %v, want False", cond.Status)
	}
	if !strings.Contains(cond.Message, "cycle") {
		t.Errorf("Ready.Message = %q, want it to mention the cycle-detection failure", cond.Message)
	}
}

func TestReconcile_TenantClientErrorOnDeletePropagates(t *testing.T) {
	snip := sampleSnippet()
	now := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &now
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	r.RestConfig = &rest.Config{Host: "http://example.test"}
	r.TokenCache = newTokenCache(&stubMinter{err: errors.New("token denied")})
	// Pin r.now() inside MaxWithdrawWait so the requeue path runs
	// rather than the force-drop fallback (covered separately by
	// TestReconcile_DeletionWithdrawTimeoutForceDrops).
	r.Clock = func() time.Time { return snip.DeletionTimestamp.Time.Add(1 * time.Minute) }

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if err == nil || !strings.Contains(err.Error(), "build impersonation client") {
		t.Errorf("got %v, want a 'build impersonation client' error", err)
	}
}

// --- Logger fallback --------------------------------------------------------

func TestSnippetReconciler_LoggerNilFallsBackToDefault(t *testing.T) {
	r := &SnippetReconciler{Logger: nil}
	if r.logger() == nil {
		t.Errorf("logger() returned nil with nil Logger field")
	}
}

// --- OCI library merge into resolveLibraries -------------------------------

// TestMarkSynced_StaleGenerationDoesNotWriteStatus pins that when the
// staleness gate's re-Get sees a snippet generation different from the one
// this reconcile evaluated against (judgedGen), markSynced skips every Status
// mutation. The publish has already landed, but stamping this reconcile's
// revision against the fresh-spec generation would mislabel a stale render as
// the up-to-date state. The next reconcile (already enqueued by the spec-edit
// watch event) writes the correct (Revision, Generation) pair.
func TestMarkSynced_StaleGenerationDoesNotWriteStatus(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Generation = 5 // the apiserver-side generation
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)

	// judgedGen=1 simulates "this reconcile started against an
	// older spec; the apiserver now carries a newer generation".
	const staleJudgedGen = int64(1)
	_, err := r.markSynced(context.Background(), snip,
		`{"ok":true}`, "sha256:abc123", "http://artifact/", 2, staleJudgedGen)
	if err != nil {
		t.Fatalf("markSynced: %v", err)
	}

	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	if got.Status.Revision != "" {
		t.Errorf("Status.Revision = %q, want empty (stale-gen write must skip)", got.Status.Revision)
	}
	if got.Status.ArtifactURL != "" {
		t.Errorf("Status.ArtifactURL = %q, want empty", got.Status.ArtifactURL)
	}
	if got.Status.LastSyncTime != nil {
		t.Errorf("Status.LastSyncTime = %v, want nil", got.Status.LastSyncTime)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady); cond != nil {
		t.Errorf("Ready condition written despite stale-gen: %+v", cond)
	}
}

// TestMarkSynced_StaleGenerationAbortsBeforeWrite pins that when the
// staleness gate's re-read sees a moved generation, markSynced does exactly
// one Get and writes no status — no Status().Update and no status patch. A
// write here would stamp this reconcile's revision against a spec that has
// already moved on; the spec-edit watch event has enqueued the next reconcile
// to write the correct (Revision, Generation) pair.
func TestMarkSynced_StaleGenerationAbortsBeforeWrite(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Generation = 5
	bareClient := clientWithStatus(t, snip)
	statusUpdates := 0
	gets := 0
	c := &countingStatusClient{Client: bareClient, statusUpdates: &statusUpdates, gets: &gets}
	r := newReconciler(t, c)

	// judgedGen=1; the apiserver carries Generation=5 → staleGen.
	_, err := r.markSynced(context.Background(), snip,
		`{"ok":true}`, "sha256:abc123", "http://artifact/", 2, 1)
	if err != nil {
		t.Fatalf("markSynced: %v", err)
	}

	if statusUpdates != 0 {
		t.Errorf("Status().Update fired %d times on the stale-gen path; want 0 (must abort before any write)", statusUpdates)
	}
	if gets != 1 {
		t.Errorf("Get fired %d times; want 1 (single staleness re-read, then abort)", gets)
	}
}

// TestMarkSynced_StalenessGateUsesAPIReader pins that the staleness re-read
// consults the uncached APIReader, not the cache-lagging client. The cached
// client carries the judged generation (a cached read would proceed and write),
// but the uncached APIReader sees a moved generation, so markSynced must skip —
// which can only happen if it read through APIReader.
func TestMarkSynced_StalenessGateUsesAPIReader(t *testing.T) {
	key := types.NamespacedName{Name: sampleSnippet().Name, Namespace: sampleSnippet().Namespace}

	cached := sampleSnippet()
	cached.Finalizers = []string{FinalizerName}
	cached.Generation = 1 // matches judgedGen → a cached read would proceed
	cachedClient := clientWithStatus(t, cached)

	fresh := sampleSnippet()
	fresh.Finalizers = []string{FinalizerName}
	fresh.Generation = 5 // apiserver truth: the spec has moved on
	apiReader := clientWithStatus(t, fresh)

	r := newReconciler(t, cachedClient)
	r.APIReader = apiReader

	_, err := r.markSynced(context.Background(), cached,
		`{"ok":true}`, "sha256:abc123", "http://artifact/", 2, 1)
	if err != nil {
		t.Fatalf("markSynced: %v", err)
	}

	got := refetch(t, cachedClient, key)
	if got.Status.Revision != "" {
		t.Errorf("Status.Revision = %q, want empty — markSynced must consult the uncached APIReader (gen 5 ≠ judged 1) and skip", got.Status.Revision)
	}
}

// countingStatusClient counts Get and Status().Update calls without
// changing their semantics. Lets a test prove the staleness gate did a
// single Get and short-circuited before any status write.
type countingStatusClient struct {
	client.Client
	statusUpdates *int
	gets          *int
}

func (c *countingStatusClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	*c.gets++
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *countingStatusClient) Status() client.SubResourceWriter {
	return &countingStatusWriter{base: c.Client.Status(), count: c.statusUpdates}
}

type countingStatusWriter struct {
	base  client.SubResourceWriter
	count *int
}

func (w *countingStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return w.base.Create(ctx, obj, subResource, opts...)
}

func (w *countingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	*w.count++
	return w.base.Update(ctx, obj, opts...)
}

func (w *countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return w.base.Patch(ctx, obj, patch, opts...)
}

func (w *countingStatusWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return w.base.Apply(ctx, obj, opts...)
}

// TestResolveLibraries_NoLibrariesEarlyExit pins the hot-path
// optimization: when the snippet declares no library references AND
// the operator has no OCI-mounted libraries, resolveLibraries
// returns nil immediately without walking the empty CR loop or
// allocating an empty map. The common case for snippet-only installs.
func TestResolveLibraries_NoLibrariesEarlyExit(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.Libraries = nil
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	// OCILibraries explicitly unset.

	libs, reason, msg, err := r.resolveLibraries(context.Background(), c, snip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
	if msg != "" {
		t.Errorf("msg = %q, want empty", msg)
	}
	if libs != nil {
		t.Errorf("libs = %v, want nil (early-exit must skip the allocation)", libs)
	}
}

func TestResolveLibraries_FoldsInOCILibraries(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := newReconciler(t, c)
	r.OCILibraries = map[string]eval.Library{
		"grafonnet": {Files: map[string]string{"main.libsonnet": `{ from: "oci" }`}},
		"xtd":       {Files: map[string]string{"main.libsonnet": `{ from: "xtd" }`}},
	}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec:       jaasv1.JsonnetSnippetSpec{}, // no CR libraries
	}
	libs, reason, _, err := r.resolveLibraries(context.Background(), c, snip)
	if err != nil {
		t.Fatalf("resolveLibraries: %v", err)
	}
	if reason != "" {
		t.Fatalf("got reason %q, want \"\"", reason)
	}
	if _, ok := libs["grafonnet"]; !ok {
		t.Errorf("grafonnet OCI library not in result; got %v", libs)
	}
	if _, ok := libs["xtd"]; !ok {
		t.Errorf("xtd OCI library not in result; got %v", libs)
	}
}

func TestResolveLibraries_MergesCRsThenOCI(t *testing.T) {
	// A CR-declared library wins on alias collision — and the OCI map's
	// other entries still flow through.
	crLib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "team-utils", Namespace: "ns"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ from: "cr" }`},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(crLib).Build()
	r := newReconciler(t, c)
	r.OCILibraries = map[string]eval.Library{
		"grafonnet": {Files: map[string]string{"main.libsonnet": `{ from: "oci" }`}},
	}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "team-utils"},
			},
		},
	}
	libs, _, _, err := r.resolveLibraries(context.Background(), c, snip)
	if err != nil {
		t.Fatalf("resolveLibraries: %v", err)
	}
	if got := libs["team-utils"].Files["main.libsonnet"]; got != `{ from: "cr" }` {
		t.Errorf("team-utils file = %q, want CR contents", got)
	}
	if got := libs["grafonnet"].Files["main.libsonnet"]; got != `{ from: "oci" }` {
		t.Errorf("grafonnet file = %q, want OCI contents", got)
	}
}

// --- EventRecorder / notification-controller -------------------------------

// drainEvents reads everything currently buffered on a FakeRecorder
// without blocking. Returns the list of "<TYPE> <REASON> <MSG>" lines.
func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestReconcile_Events_EmitNormalOnSynced(t *testing.T) {
	snip := sampleSnippet()
	// newPublisherClient registers the ExternalArtifact status
	// subresource the Publisher writes — clientWithStatus alone
	// doesn't cover that path.
	c := newPublisherClientWithSnippet(t, snip)
	r := &SnippetReconciler{
		Client: c, Scheme: publisherScheme(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.Publisher = newTestPublisher(t, c)
	rec := events.NewFakeRecorder(16)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec: %v", err)
	}
	got := drainEvents(rec)
	// Look for at least one "Normal Synced ..." event.
	var found bool
	for _, ev := range got {
		if strings.HasPrefix(ev, "Normal "+ReasonSynced+" ") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no Normal/Synced event emitted; got %v", got)
	}
}

// TestReconcile_Events_EmitWarningOnSyncedToSuspendedTransition reproduces the
// smoke sequence (scenario-fields.sh): a snippet renders to Synced, then
// spec.suspend flips true. The Synced->Suspended transition must emit a
// Warning/Suspended event — emitConditionEvent's dedup only suppresses a repeat
// of the SAME status+reason, and Synced/True differs from Suspended/False.
func TestReconcile_Events_EmitWarningOnSyncedToSuspendedTransition(t *testing.T) {
	snip := sampleSnippet()
	c := newPublisherClientWithSnippet(t, snip)
	r := &SnippetReconciler{
		Client: c, Scheme: publisherScheme(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.Publisher = newTestPublisher(t, c)
	rec := events.NewFakeRecorder(16)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec: %v", err)
	}
	_ = drainEvents(rec) // discard the Synced event

	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Suspend = true
	if err := c.Update(context.Background(), &got); err != nil {
		t.Fatalf("suspend update: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("suspend reconcile: %v", err)
	}

	evs := drainEvents(rec)
	var found bool
	for _, ev := range evs {
		if strings.HasPrefix(ev, "Warning "+ReasonSuspended+" ") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no Warning/Suspended event on Synced->Suspended transition; got %v", evs)
	}
}

// TestReconcile_MarkSynced_PopulatesLastSyncTimeAndFileCount pins the
// UX-honesty contract on the Synced status: kubectl describe must
// surface (a) a non-zero file count derived from the resolved source
// (not len(snip.Spec.Files), which is zero in sourceRef mode), and
// (b) a stamped status.lastSyncTime that downstream tooling can use
// to detect liveness.
func TestReconcile_MarkSynced_PopulatesLastSyncTimeAndFileCount(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.Files = map[string]string{
		"main.jsonnet":     `{ ok: true }`,
		"helper.libsonnet": `{ x: 1 }`,
	}
	c := newPublisherClientWithSnippet(t, snip)
	pinned := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	r := &SnippetReconciler{
		Client: c,
		Scheme: publisherScheme(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  func() time.Time { return pinned },
	}
	r.Publisher = newTestPublisher(t, c)

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec: %v", err)
	}

	got := refetch(t, c, key)
	if got.Status.LastSyncTime == nil {
		t.Fatal("status.lastSyncTime not written on Synced reconcile")
	}
	if !got.Status.LastSyncTime.Time.Equal(pinned) {
		t.Errorf("status.lastSyncTime = %v, want %v", got.Status.LastSyncTime.Time, pinned)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing after Synced reconcile")
	}
	if !strings.Contains(cond.Message, "from 2 files") {
		t.Errorf("Ready.Message = %q, want %q substring (count must come from resolved source)",
			cond.Message, "from 2 files")
	}
}

// TestReconcile_EmptySourceTarball_DistinguishesPathFilter pins the
// honest message: when sourceRef.path filters every entry away, the
// operator must see a message that points at the filter, not the
// generic "must contain main.jsonnet as the entry point" — those
// suggest opposite remediations (widen the filter vs. add a missing
// file upstream). Driven via a fake Fetcher that returns an empty
// map (mimics the post-path-filter state).
func TestReconcile_EmptySourceTarball_DistinguishesPathFilter(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{
		Kind: "GitRepository", Name: "upstream", Path: "subdir/that/matches/nothing",
	}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Fetcher = &emptyFetcher{}
	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})

	got := refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if !strings.Contains(cond.Message, "spec.sourceRef.path") || !strings.Contains(cond.Message, "subdir/that/matches/nothing") {
		t.Errorf("Message = %q, want it to name the path filter and the configured value", cond.Message)
	}
	if strings.Contains(cond.Message, "snippet entry point") {
		t.Errorf("Message points at the entry-file invariant when the real cause is the path filter: %q", cond.Message)
	}
}

// emptyFetcher returns an empty file map regardless of input — the
// post-spec.sourceRef.path-narrowed-everything-away shape.
type emptyFetcher struct{}

func (emptyFetcher) Fetch(_ context.Context, _ client.Client, _ *jaasv1.SourceRef, _ string) (*sources.Result, error) {
	return &sources.Result{Files: map[string]string{}, Revision: "sha256:00"}, nil
}

// erroringFetcher returns a fixed error from every Fetch call.
type erroringFetcher struct{ err error }

func (f erroringFetcher) Fetch(_ context.Context, _ client.Client, _ *jaasv1.SourceRef, _ string) (*sources.Result, error) {
	return nil, f.err
}

// TestReconcile_TransientFetchPreservesSentinel pins the
// contract: when a Fetcher transient error (e.g. ErrSourceNotReady)
// flows back through reconcileSpec, the returned error must still
// satisfy errors.Is for the original sentinel. Wrapping the message
// with errors.New(msg) or fmt.Errorf("...: %s", msg) would erase the
// chain — and any future errors.Is-based circuit breaker, log
// classifier, or controller-runtime backoff hook that wanted to
// distinguish "source not ready" from "network 5xx" would silently
// treat them the same.
func TestReconcile_TransientFetchPreservesSentinel(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Files = nil
	snip.Spec.SourceRef = &jaasv1.SourceRef{Kind: "GitRepository", Name: "upstream"}

	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Fetcher = erroringFetcher{err: fmt.Errorf("source-controller still catching up: %w", sources.ErrSourceNotReady)}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	})
	if err == nil {
		t.Fatal("expected transient error, got nil")
	}
	if !errors.Is(err, sources.ErrSourceNotReady) {
		t.Errorf("returned err %v does not satisfy errors.Is(_, sources.ErrSourceNotReady) — sentinel chain was erased on the way out", err)
	}
}

// newPublisherClientWithSnippet is newPublisherClient that pre-loads a
// snippet object. Used by reconciler-side tests that need both the
// snippet present and the ExternalArtifact status subresource
// registered.
func newPublisherClientWithSnippet(t *testing.T, snip *jaasv1.JsonnetSnippet) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		Build()
}

func TestReconcile_Events_EmitWarningOnFailure(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.ServiceAccountName = "" // triggers ReasonServiceAccountMissing
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	rec := events.NewFakeRecorder(16)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec: %v", err)
	}
	got := drainEvents(rec)
	var found bool
	for _, ev := range got {
		if strings.HasPrefix(ev, "Warning "+ReasonServiceAccountMissing+" ") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no Warning/ServiceAccountMissing event emitted; got %v", got)
	}
}

func TestReconcile_Events_DedupOnRepeatedSameReason(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.ServiceAccountName = ""
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	rec := events.NewFakeRecorder(16)
	r.EventRecorder = rec
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	// Finalizer attach (no events expected — failReady runs in spec
	// branch only). Three more reconciles all hit ServiceAccountMissing.
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	got := drainEvents(rec)
	saw := 0
	for _, ev := range got {
		if strings.HasPrefix(ev, "Warning "+ReasonServiceAccountMissing+" ") {
			saw++
		}
	}
	if saw != 1 {
		t.Errorf("saw %d ServiceAccountMissing events across 3 spec reconciles, want 1 (dedup)", saw)
	}
}

func TestReconcile_Events_NilRecorderDisables(t *testing.T) {
	// Sanity: nil recorder must not panic; covered implicitly by other
	// tests, but make the contract explicit.
	r := &SnippetReconciler{EventRecorder: nil}
	r.emitConditionEvent(&jaasv1.JsonnetSnippet{}, nil, metav1.ConditionFalse, "X", "y")
}

// --- Revision retention / status.history ------------------------------------

func TestKnownRevisionPaths_RendersStoragePathsFromHistory(t *testing.T) {
	got := knownRevisionPaths("team-a/demo", []jaasv1.RevisionEntry{
		{Revision: "sha256:aaa"},
		{Revision: "sha256:bbb"},
	})
	want := "team-a/demo/aaa.tar.gz, team-a/demo/bbb.tar.gz"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestKnownRevisionPaths_EmptyHistoryReturnsEmpty(t *testing.T) {
	if got := knownRevisionPaths("team-a/demo", nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// scrubbedCrossNamespaceMessage must collapse every underlying failure
// (NotFound / Forbidden / digest mismatch / 5xx) into one constant string
// so a tenant can't fingerprint another namespace by the error shape.
func TestScrubbedCrossNamespaceMessage_IsConstantRegardlessOfCause(t *testing.T) {
	ref := &jaasv1.SourceRef{Kind: "GitRepository", Name: "secret-src", Namespace: "other-team"}
	want := `cross-namespace GitRepository "secret-src" is not reachable; check the source CR's status in "other-team"`
	if got := scrubbedCrossNamespaceMessage(ref); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestIsLastSnippetUsingSA(t *testing.T) {
	snip := func(name, sa string, deleting bool) *jaasv1.JsonnetSnippet {
		s := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
			Spec:       jaasv1.JsonnetSnippetSpec{ServiceAccountName: sa},
		}
		if deleting {
			now := metav1.Now()
			s.DeletionTimestamp = &now
			s.Finalizers = []string{"keep"} // fake client rejects a deleting object without a finalizer
		}
		return s
	}

	t.Run("last live user of the SA", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(snip("a", "tenant", false), snip("b", "other-sa", false)).Build()
		r := newReconciler(t, c)
		last, err := r.isLastSnippetUsingSA(context.Background(), "team-a", "tenant", "a")
		if err != nil || !last {
			t.Errorf("last=%v err=%v, want true/nil (b uses a different SA)", last, err)
		}
	})

	t.Run("a live sibling shares the SA", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(snip("a", "tenant", false), snip("b", "tenant", false)).Build()
		r := newReconciler(t, c)
		last, err := r.isLastSnippetUsingSA(context.Background(), "team-a", "tenant", "a")
		if err != nil || last {
			t.Errorf("last=%v err=%v, want false/nil (b shares the SA)", last, err)
		}
	})

	t.Run("a sibling sharing the SA is being deleted — doesn't count", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(snip("a", "tenant", false), snip("b", "tenant", true)).Build()
		r := newReconciler(t, c)
		last, err := r.isLastSnippetUsingSA(context.Background(), "team-a", "tenant", "a")
		if err != nil || !last {
			t.Errorf("last=%v err=%v, want true/nil (b is terminating)", last, err)
		}
	})
}

func TestBuildKeepShortRevs_OnlyNewWhenHistoryOne(t *testing.T) {
	got := buildKeepShortRevs("sha256:new", []jaasv1.RevisionEntry{
		{Revision: "sha256:old1"},
		{Revision: "sha256:old2"},
	}, 1)
	if len(got) != 1 || got[0] != "new" {
		t.Errorf("got %v, want [new]", got)
	}
}

func TestBuildKeepShortRevs_PrependsNewAndCarriesHistory(t *testing.T) {
	got := buildKeepShortRevs("sha256:new", []jaasv1.RevisionEntry{
		{Revision: "sha256:old1"},
		{Revision: "sha256:old2"},
		{Revision: "sha256:old3"},
	}, 3)
	want := []string{"new", "old1", "old2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestBuildKeepShortRevs_DedupsNewIfAlreadyInHistory(t *testing.T) {
	got := buildKeepShortRevs("sha256:repub", []jaasv1.RevisionEntry{
		{Revision: "sha256:repub"},
		{Revision: "sha256:old"},
	}, 5)
	if len(got) != 2 || got[0] != "repub" || got[1] != "old" {
		t.Errorf("got %v, want [repub old]", got)
	}
}

func TestBuildKeepShortRevs_HistoryZeroClampsToOne(t *testing.T) {
	got := buildKeepShortRevs("sha256:x", nil, 0)
	if len(got) != 1 || got[0] != "x" {
		t.Errorf("got %v, want [x]", got)
	}
}

func TestUpdateRevisionHistory_PrependsAndTruncates(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	prior := []jaasv1.RevisionEntry{
		{Revision: "sha256:r1", Time: metav1.NewTime(now.Add(-2 * time.Hour))},
		{Revision: "sha256:r2", Time: metav1.NewTime(now.Add(-4 * time.Hour))},
		{Revision: "sha256:r3", Time: metav1.NewTime(now.Add(-6 * time.Hour))},
	}
	got := updateRevisionHistory(prior, "sha256:r0", 3, now)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Revision != "sha256:r0" || !got[0].Time.Equal(&metav1.Time{Time: now}) {
		t.Errorf("[0] = %+v, want r0@now", got[0])
	}
	if got[1].Revision != "sha256:r1" || got[2].Revision != "sha256:r2" {
		t.Errorf("trailing entries wrong: %+v", got[1:])
	}
}

func TestUpdateRevisionHistory_SameHeadKeepsOriginalTimestamp(t *testing.T) {
	pinTime := metav1.NewTime(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	prior := []jaasv1.RevisionEntry{
		{Revision: "sha256:r", Time: pinTime},
	}
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	got := updateRevisionHistory(prior, "sha256:r", 5, now)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if !got[0].Time.Equal(&pinTime) {
		t.Errorf("timestamp moved to %v despite identical head", got[0].Time)
	}
}

func TestUpdateRevisionHistory_ZeroHistoryClampsToOne(t *testing.T) {
	now := time.Now()
	got := updateRevisionHistory(nil, "sha256:r", 0, now)
	if len(got) != 1 {
		t.Errorf("len = %d, want 1 (zero clamps)", len(got))
	}
}

// --- spec.entryFile ---------------------------------------------------------

func TestReconcile_CustomEntryFile_Resolves(t *testing.T) {
	snip := sampleSnippet()
	// Replace main.jsonnet with a differently-named entry file.
	snip.Spec.Files = map[string]string{
		"dashboards/api.jsonnet": `{ kind: "dashboard" }`,
	}
	snip.Spec.EntryFile = "dashboards/api.jsonnet"

	c := newPublisherClientWithSnippet(t, snip)
	r := &SnippetReconciler{
		Client: c, Scheme: publisherScheme(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.Publisher = newTestPublisher(t, c)

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec: %v", err)
	}
	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonSynced {
		t.Errorf("Ready = %+v, want True/Synced (custom EntryFile resolved)", cond)
	}
}

func TestReconcile_MissingCustomEntryFile_FailsInvalidSpec(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.Files = map[string]string{"main.jsonnet": "{}"}
	snip.Spec.EntryFile = "dashboards/missing.jsonnet"

	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec: %v", err)
	}
	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil || cond.Reason != ReasonInvalidSpec {
		t.Errorf("Ready Reason = %v, want %s", cond, ReasonInvalidSpec)
	}
	if !strings.Contains(cond.Message, "dashboards/missing.jsonnet") {
		t.Errorf("message %q does not name the missing file", cond.Message)
	}
}

// --- spec.interval ----------------------------------------------------------

func TestReconcile_IntervalSetSchedulesRequeueAfter(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.Interval = &metav1.Duration{Duration: 15 * time.Minute}
	c := newPublisherClientWithSnippet(t, snip)
	r := &SnippetReconciler{
		Client: c, Scheme: publisherScheme(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.Publisher = newTestPublisher(t, c)

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	// markSynced jitters the interval-based requeue by up to
	// defaultIntervalJitterFraction so a same-interval fleet doesn't
	// thunder-herd. The global jitter is process-wide and seeded by the first
	// SetupWithManager in the test binary, so whether it's active here depends
	// on test ordering — assert the configured interval is the centre and the
	// result stays inside the jitter band either way.
	const interval = 15 * time.Minute
	band := time.Duration(float64(interval) * defaultIntervalJitterFraction)
	if res.RequeueAfter < interval-band || res.RequeueAfter > interval+band {
		t.Errorf("RequeueAfter = %v, want within %v of 15m (spec.interval, jittered)", res.RequeueAfter, band)
	}
}

func TestReconcile_NoIntervalReturnsZeroRequeueAfter(t *testing.T) {
	snip := sampleSnippet() // Interval nil
	c := newPublisherClientWithSnippet(t, snip)
	r := &SnippetReconciler{
		Client: c, Scheme: publisherScheme(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.Publisher = newTestPublisher(t, c)

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 (no interval)", res.RequeueAfter)
	}
}

func TestReconcile_ZeroIntervalIsNoOp(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.Interval = &metav1.Duration{Duration: 0}
	c := newPublisherClientWithSnippet(t, snip)
	r := &SnippetReconciler{
		Client: c, Scheme: publisherScheme(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.Publisher = newTestPublisher(t, c)

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 (zero interval == disabled)", res.RequeueAfter)
	}
}

// --- spec.suspend -----------------------------------------------------------

func TestReconcile_SuspendSkipsPipelineAndFlipsSuspendedReason(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.Suspend = true
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c) // pipeline MUST not run

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	// Round 1 attaches the finalizer.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	// Round 2 hits the suspend branch.
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("suspend reconcile: %v", err)
	}
	// A paused snippet must not self-requeue — an unsuspend spec edit (or a
	// watch event) drives the next reconcile, not a timer. A non-zero requeue
	// would wake it every minute forever and churn the reconcile-total metric.
	if res.RequeueAfter != 0 {
		t.Errorf("suspend reconcile RequeueAfter = %v, want 0", res.RequeueAfter)
	}

	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonSuspended {
		t.Errorf("Ready = %s/%s, want False/%s", cond.Status, cond.Reason, ReasonSuspended)
	}
	if got.Status.Revision != "" {
		t.Errorf("Status.Revision = %q, want unchanged (suspend must not publish)", got.Status.Revision)
	}
}

func TestReconcile_SuspendThenResumeLeavesSuspendedReason(t *testing.T) {
	// After unsuspending, the next reconcile leaves the Suspended
	// branch and re-enters the pipeline. We don't assert the full
	// pipeline reaches Synced here (that's covered by the happy-path
	// reconcile tests); we just prove the Suspended reason has been
	// replaced by SOMETHING — the snippet is no longer pinned by the
	// suspend gate.
	snip := sampleSnippet()
	snip.Spec.Suspend = true
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c) // no Publisher: pipeline fails on ServiceAccount, which is fine
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Sanity: we're in Suspended.
	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady); cond == nil || cond.Reason != ReasonSuspended {
		t.Fatalf("pre-resume Ready not Suspended: %+v", cond)
	}

	// Flip suspend off and reconcile once. The pipeline runs — even if
	// it fails on a later gate, the Reason must no longer be Suspended.
	got.Spec.Suspend = false
	if err := c.Update(context.Background(), &got); err != nil {
		t.Fatalf("flip suspend off: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("resume reconcile: %v", err)
	}

	var after jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &after); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(after.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready disappeared after resume")
	}
	if cond.Reason == ReasonSuspended {
		t.Errorf("Reason is still %s after resume; want any non-Suspended value", ReasonSuspended)
	}
}

// --- Terminal-failure auto-retry --------------------------------------------

func TestReconcile_FailReady_ReturnsBoundedRequeueAfter(t *testing.T) {
	// A snippet with no ServiceAccount and no operator default fails on a
	// terminal reason (ServiceAccountMissing). failReady returns no error
	// — controller-runtime's backoff doesn't engage — so a bounded
	// RequeueAfter is the only thing that re-checks the snippet after an
	// out-of-band fix (here: the operator grants a default SA / the spec
	// gets one). Without it, an RBAC grant would never trigger recovery.
	snip := sampleSnippet()
	snip.Spec.ServiceAccountName = "" // triggers ReasonServiceAccountMissing
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != permanentRetryInterval {
		t.Errorf("RequeueAfter = %v, want %v (terminal failure must self-retry)", res.RequeueAfter, permanentRetryInterval)
	}
	assertReady(t, refetch(t, c, key), metav1.ConditionFalse, ReasonServiceAccountMissing)
}

func TestReconcile_FailReady_RecoversOnNextReconcileAfterFix(t *testing.T) {
	// The recovery half of the auto-retry contract: once the terminal
	// cause is fixed out-of-band (here the snippet gains a ServiceAccount,
	// mirroring an RBAC grant in the smoke), the requeued reconcile must
	// reach Ready=True rather than stay pinned on the failure reason.
	snip := sampleSnippet()
	snip.Spec.ServiceAccountName = ""
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	assertReady(t, refetch(t, c, key), metav1.ConditionFalse, ReasonServiceAccountMissing)

	// Fix the cause out-of-band, as the smoke does by granting RBAC.
	fixed := refetch(t, c, key)
	fixed.Spec.ServiceAccountName = "tenant"
	if err := c.Update(context.Background(), fixed); err != nil {
		t.Fatalf("apply fix: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("recovery reconcile: %v", err)
	}
	assertReady(t, refetch(t, c, key), metav1.ConditionTrue, ReasonSynced)
}

// --- OCI library alias shadowing --------------------------------------------

func TestCheckLibraryAliasCollisions_EmptyKnownDisables(t *testing.T) {
	r := &SnippetReconciler{KnownLibraryAliases: nil}
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "grafonnet"}},
	}}
	if reason, _ := r.checkLibraryAliasCollisions(snip); reason != "" {
		t.Errorf("got reason %q with empty KnownLibraryAliases, want disabled", reason)
	}
}

func TestCheckLibraryAliasCollisions_ImportPathShadows(t *testing.T) {
	r := &SnippetReconciler{KnownLibraryAliases: []string{"grafonnet"}}
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Libraries: []jaasv1.LibraryRef{
			{Kind: "JsonnetLibrary", Name: "team-utils", ImportPath: "grafonnet"},
		},
	}}
	reason, msg := r.checkLibraryAliasCollisions(snip)
	if reason != ReasonInvalidSpec {
		t.Errorf("reason = %q, want %q", reason, ReasonInvalidSpec)
	}
	if !strings.Contains(msg, "grafonnet") || !strings.Contains(msg, "shadows OCI") {
		t.Errorf("msg %q lacks expected substrings", msg)
	}
}

func TestCheckLibraryAliasCollisions_NameShadowsWhenImportPathEmpty(t *testing.T) {
	r := &SnippetReconciler{KnownLibraryAliases: []string{"grafonnet"}}
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "grafonnet"}},
	}}
	if reason, _ := r.checkLibraryAliasCollisions(snip); reason != ReasonInvalidSpec {
		t.Errorf("reason = %q, want %q (bare Name should be checked)", reason, ReasonInvalidSpec)
	}
}

func TestCheckLibraryAliasCollisions_DistinctNamesPass(t *testing.T) {
	r := &SnippetReconciler{KnownLibraryAliases: []string{"grafonnet", "xtd"}}
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Libraries: []jaasv1.LibraryRef{
			{Kind: "JsonnetLibrary", Name: "team-utils", ImportPath: "team"},
		},
	}}
	if reason, _ := r.checkLibraryAliasCollisions(snip); reason != "" {
		t.Errorf("got reason %q on non-colliding LibraryRef, want \"\"", reason)
	}
}

func TestCheckLibraryAliasCollisions_FirstHitReported(t *testing.T) {
	r := &SnippetReconciler{KnownLibraryAliases: []string{"a", "b"}}
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Libraries: []jaasv1.LibraryRef{
			{Kind: "JsonnetLibrary", Name: "ok"},
			{Kind: "JsonnetLibrary", Name: "a"}, // first collision
			{Kind: "JsonnetLibrary", Name: "b"}, // also a collision but not reported
		},
	}}
	_, msg := r.checkLibraryAliasCollisions(snip)
	if !strings.Contains(msg, `"a"`) {
		t.Errorf("msg %q does not mention the first colliding alias", msg)
	}
	if strings.Contains(msg, `"b"`) {
		t.Errorf("msg %q surfaces 'b' too — should report only the first", msg)
	}
}

func TestCheckDuplicateLibraryImports_RejectsCollision(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Libraries: []jaasv1.LibraryRef{
			{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "shared"},
			{Kind: "JsonnetLibrary", Name: "other", ImportPath: "shared"},
		},
	}}
	reason, msg := checkDuplicateLibraryImports(snip)
	if reason != ReasonInvalidSpec {
		t.Errorf("reason = %q, want %q", reason, ReasonInvalidSpec)
	}
	if !strings.Contains(msg, "shared") {
		t.Errorf("msg %q does not name the colliding import path", msg)
	}
}

func TestCheckDuplicateLibraryImports_CollidesOnNameFallback(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Libraries: []jaasv1.LibraryRef{
			{Kind: "JsonnetLibrary", Name: "duplicated"},
			{Kind: "JsonnetLibrary", Name: "duplicated"},
		},
	}}
	if reason, _ := checkDuplicateLibraryImports(snip); reason != ReasonInvalidSpec {
		t.Errorf("reason = %q, want %q (bare Name should be checked)", reason, ReasonInvalidSpec)
	}
}

func TestCheckDuplicateLibraryImports_DistinctPathsPass(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Libraries: []jaasv1.LibraryRef{
			{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "shared"},
			{Kind: "JsonnetLibrary", Name: "other", ImportPath: "extra"},
		},
	}}
	if reason, _ := checkDuplicateLibraryImports(snip); reason != "" {
		t.Errorf("got reason %q on distinct import paths, want \"\"", reason)
	}
}
