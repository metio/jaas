/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// --- publishConsistencyGate -------------------------------------------------

// When the snippet's generation still matches the one judged at reconcile
// entry, the gate clears and returns the freshly-read object.
func TestPublishConsistencyGate_GenerationMatchClears(t *testing.T) {
	snip := sampleSnippet() // Generation 1
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	latest, ok, err := r.publishConsistencyGate(context.Background(), key, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || latest == nil {
		t.Fatalf("gate should clear on matching generation, got ok=%v latest=%v", ok, latest)
	}
}

// A spec edit between reconcile entry and the pre-publish recheck moves the
// generation; the gate defers the publish (ok=false, no error).
func TestPublishConsistencyGate_GenerationMismatchDefers(t *testing.T) {
	snip := sampleSnippet() // Generation 1
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	latest, ok, err := r.publishConsistencyGate(context.Background(), key, 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || latest != nil {
		t.Fatalf("gate should defer on moved generation, got ok=%v latest=%v", ok, latest)
	}
}

// A snippet deleted in the gap reads as NotFound; the gate treats it as
// nothing-to-publish rather than an error (the deletion reconcile is already
// enqueued).
func TestPublishConsistencyGate_NotFoundIsNoOp(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: "ghost", Namespace: "team-a"}

	latest, ok, err := r.publishConsistencyGate(context.Background(), key, 1)
	if err != nil {
		t.Fatalf("NotFound must not error: %v", err)
	}
	if ok || latest != nil {
		t.Fatalf("NotFound must yield (nil,false), got ok=%v latest=%v", ok, latest)
	}
}

// A non-NotFound Get failure during the recheck wraps and propagates so the
// caller requeues with backoff.
func TestPublishConsistencyGate_GetErrorPropagates(t *testing.T) {
	getErr := errors.New("etcd unreachable")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return getErr
			},
		}).
		Build()
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: "demo", Namespace: "team-a"}

	_, ok, err := r.publishConsistencyGate(context.Background(), key, 1)
	if !errors.Is(err, getErr) {
		t.Fatalf("got %v, want wrapped %v", err, getErr)
	}
	if ok {
		t.Error("ok must be false on error")
	}
}

// With APIReader unset the gate falls back to the cached Client rather than
// nil-dereferencing the reader.
func TestPublishConsistencyGate_NilAPIReaderFallsBackToClient(t *testing.T) {
	snip := sampleSnippet()
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.APIReader = nil
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	_, ok, err := r.publishConsistencyGate(context.Background(), key, 1)
	if err != nil || !ok {
		t.Fatalf("client fallback path failed: ok=%v err=%v", ok, err)
	}
}

// --- warnLikelySelfReference ------------------------------------------------

// A same-namespace ExternalArtifact whose name equals the snippet's own name
// is flagged at admission time.
func TestWarnLikelySelfReference_SameNamespaceWarns(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "demo"},
			},
		},
	}
	if got := warnLikelySelfReference(snip); got == "" {
		t.Fatal("expected self-reference warning")
	}
}

// An ExternalArtifact named "demo" but living in another namespace is not the
// snippet's own published artifact, so no warning fires (the cross-namespace
// branch returns the empty string).
func TestWarnLikelySelfReference_OtherNamespaceNoWarning(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "demo", Namespace: "team-b"},
			},
		},
	}
	if got := warnLikelySelfReference(snip); got != "" {
		t.Errorf("cross-namespace ref must not warn, got %q", got)
	}
}

// A non-ExternalArtifact sourceRef is never a self-reference.
func TestWarnLikelySelfReference_NonExternalArtifactNoWarning(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "demo"},
			},
		},
	}
	if got := warnLikelySelfReference(snip); got != "" {
		t.Errorf("GitRepository ref must not warn, got %q", got)
	}
}

// A name that differs from the snippet's own is not a self-reference even
// when the kind is ExternalArtifact.
func TestWarnLikelySelfReference_DifferentNameNoWarning(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "upstream"},
			},
		},
	}
	if got := warnLikelySelfReference(snip); got != "" {
		t.Errorf("different name must not warn, got %q", got)
	}
}

// --- libraryAliasCollision --------------------------------------------------

// A LibraryRef.ImportPath that shadows an OCI-mounted alias is reported.
func TestLibraryAliasCollision_ImportPathHit(t *testing.T) {
	v := &SnippetValidator{KnownLibraryAliases: []string{"shared"}}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "lib", ImportPath: "shared"},
			},
		},
	}
	if got := v.libraryAliasCollision(snip); got != "shared" {
		t.Errorf("collision = %q, want %q", got, "shared")
	}
}

// When ImportPath is empty the ref's Name is used as the alias for the
// collision check.
func TestLibraryAliasCollision_NameFallbackHit(t *testing.T) {
	v := &SnippetValidator{KnownLibraryAliases: []string{"grafonnet"}}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "grafonnet"},
			},
		},
	}
	if got := v.libraryAliasCollision(snip); got != "grafonnet" {
		t.Errorf("collision = %q, want %q", got, "grafonnet")
	}
}

// No overlap between the snippet's aliases and the operator's OCI mounts
// yields an empty string.
func TestLibraryAliasCollision_NoCollision(t *testing.T) {
	v := &SnippetValidator{KnownLibraryAliases: []string{"shared"}}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "lib", ImportPath: "unique"},
			},
		},
	}
	if got := v.libraryAliasCollision(snip); got != "" {
		t.Errorf("expected no collision, got %q", got)
	}
}

// With no OCI mounts configured the collision check is disabled and returns
// the empty string regardless of the snippet's refs.
func TestLibraryAliasCollision_DisabledWithoutMounts(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "anything"}},
		},
	}
	if got := v.libraryAliasCollision(snip); got != "" {
		t.Errorf("collision check should be disabled, got %q", got)
	}
}

// --- hasCycleSourceEdge nil -------------------------------------------------

// A nil snippet has no dependency edge to follow.
func TestHasCycleSourceEdge_NilSnippet(t *testing.T) {
	if hasCycleSourceEdge(nil) {
		t.Error("nil snippet must report no cycle edge")
	}
}

// --- reconstructCyclePath ---------------------------------------------------

// A snippet whose own dependency loops straight back renders as the
// two-element self-cycle path.
func TestReconstructCyclePath_SelfCycle(t *testing.T) {
	start := types.NamespacedName{Name: "a", Namespace: "ns"}
	path := reconstructCyclePath(nil, start, start)
	if len(path) != 2 || path[0] != start || path[1] != start {
		t.Fatalf("self-cycle path = %v, want [a a]", path)
	}
}

// A multi-hop chain reconstructs start → … → start in dependency order.
func TestReconstructCyclePath_MultiHop(t *testing.T) {
	start := types.NamespacedName{Name: "a", Namespace: "ns"}
	mid := types.NamespacedName{Name: "b", Namespace: "ns"}
	cur := types.NamespacedName{Name: "c", Namespace: "ns"}
	parent := map[types.NamespacedName]types.NamespacedName{
		cur: mid,
		mid: start,
	}
	path := reconstructCyclePath(parent, cur, start)
	want := []types.NamespacedName{start, mid, cur, start}
	if len(path) != len(want) {
		t.Fatalf("path = %v, want %v", path, want)
	}
	for i := range want {
		if path[i] != want[i] {
			t.Errorf("path[%d] = %v, want %v", i, path[i], want[i])
		}
	}
}

// A broken parent chain (a node with no recorded parent) stops the walk
// without panicking; the path renders what it could reach.
func TestReconstructCyclePath_BrokenParentChain(t *testing.T) {
	start := types.NamespacedName{Name: "a", Namespace: "ns"}
	cur := types.NamespacedName{Name: "c", Namespace: "ns"}
	// No parent entry for cur, so the loop breaks immediately.
	path := reconstructCyclePath(map[types.NamespacedName]types.NamespacedName{}, cur, start)
	if len(path) < 2 || path[0] != start || path[len(path)-1] != start {
		t.Fatalf("path must be bracketed by start, got %v", path)
	}
}

// --- setReadyCondition malformed entries ------------------------------------

// A non-map entry in status.conditions is skipped rather than crashing the
// rebuild, and the Ready condition is still appended.
func TestSetReadyCondition_SkipsMalformedConditionEntry(t *testing.T) {
	ea := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := unstructured.SetNestedSlice(ea.Object, []interface{}{
		"not-a-map",
		map[string]interface{}{"type": "Stalled", "status": "False"},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}

	setReadyCondition(ea, "2026-06-22T00:00:00Z")

	conds, _, _ := unstructured.NestedSlice(ea.Object, "status", "conditions")
	var sawReady, sawStalled bool
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			t.Errorf("malformed entry survived the rebuild: %v", c)
			continue
		}
		switch m["type"] {
		case "Ready":
			sawReady = true
		case "Stalled":
			sawStalled = true
		}
	}
	if !sawReady {
		t.Error("Ready condition missing after rebuild")
	}
	if !sawStalled {
		t.Error("valid Stalled condition dropped")
	}
}

// A fresh artifact with a Ready=False existing condition takes the supplied
// now as its lastTransitionTime (the True-preservation branch is bypassed).
func TestSetReadyCondition_NotPreviouslyTrueUsesNow(t *testing.T) {
	ea := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := unstructured.SetNestedSlice(ea.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "lastTransitionTime": "2020-01-01T00:00:00Z"},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}

	const now = "2026-06-22T12:00:00Z"
	setReadyCondition(ea, now)

	conds, _, _ := unstructured.NestedSlice(ea.Object, "status", "conditions")
	var ltt string
	for _, c := range conds {
		m, _ := c.(map[string]interface{})
		if m["type"] == "Ready" {
			ltt, _ = m["lastTransitionTime"].(string)
		}
	}
	if ltt != now {
		t.Errorf("lastTransitionTime = %q, want now %q (False→True must reset)", ltt, now)
	}
}

// --- decorateMessage --------------------------------------------------------

// Terminal reasons get a runbook link appended; happy reasons stay verbatim.
func TestDecorateMessage_RunbookSuffixVsHappy(t *testing.T) {
	r := &SnippetReconciler{}

	withLink := r.decorateMessage(ReasonRBACDenied, "denied")
	if !strings.Contains(withLink, runbookBaseURL) {
		t.Errorf("terminal reason missing runbook link: %q", withLink)
	}
	if !strings.Contains(withLink, strings.ToLower(ReasonRBACDenied)) {
		t.Errorf("runbook link missing lower-cased reason: %q", withLink)
	}

	happy := r.decorateMessage(ReasonSynced, "all good")
	if happy != "all good" {
		t.Errorf("happy reason must not be decorated, got %q", happy)
	}
}

// --- forceDropFinalizer -----------------------------------------------------

// forceDropFinalizer with no EventRecorder wired must not panic — the
// metric and log still fire, the event is skipped.
func TestForceDropFinalizer_NilEventRecorderNoPanic(t *testing.T) {
	c := clientWithStatus(t, sampleSnippet())
	r := newReconciler(t, c)
	r.EventRecorder = nil
	snip := sampleSnippet()
	r.forceDropFinalizer(context.Background(), discardLogger(), snip, 0, "perma-down", errors.New("s3 gone"))
}

// With history recorded the message names concrete revision paths rather than
// the generic placeholder.
func TestForceDropFinalizer_WithHistoryNamesPaths(t *testing.T) {
	c := clientWithStatus(t, sampleSnippet())
	r := newReconciler(t, c)
	r.EventRecorder = nil
	snip := sampleSnippet()
	snip.Status.History = []jaasv1.RevisionEntry{{Revision: "sha256:deadbeef"}}
	r.forceDropFinalizer(context.Background(), discardLogger(), snip, 0, "perma-down", errors.New("boom"))
}

// --- shortRevs / knownRevisionPaths edge cases ------------------------------

// shortRevs strips the sha256: prefix and drops entries that are empty after
// stripping.
func TestShortRevs_StripsPrefixAndDropsEmpty(t *testing.T) {
	got := shortRevs([]jaasv1.RevisionEntry{
		{Revision: "sha256:aaa"},
		{Revision: "sha256:"}, // empty after strip → dropped
		{Revision: "bbb"},     // no prefix → kept as-is
	})
	want := []string{"aaa", "bbb"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("shortRevs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// knownRevisionPaths joins each short revision into a storage path.
func TestKnownRevisionPaths_JoinsMultiple(t *testing.T) {
	got := knownRevisionPaths("ns/demo", []jaasv1.RevisionEntry{
		{Revision: "sha256:one"},
		{Revision: "sha256:two"},
	})
	if !strings.Contains(got, "ns/demo/one.tar.gz") || !strings.Contains(got, "ns/demo/two.tar.gz") {
		t.Errorf("paths = %q, want both revisions", got)
	}
}

// --- EngageFluxWatch nil cache ----------------------------------------------

// fakeWatchController satisfies controller.Controller so the nil-cache guard
// in EngageFluxWatch is reachable past the nil-controller check.
type fakeWatchController struct{ watchErr error }

func (f *fakeWatchController) Reconcile(context.Context, reconcile.Request) (reconcile.Result, error) {
	return reconcile.Result{}, nil
}

func (f *fakeWatchController) Watch(source.TypedSource[reconcile.Request]) error {
	return f.watchErr
}

func (f *fakeWatchController) Start(context.Context) error { return nil }

func (f *fakeWatchController) GetLogger() logr.Logger { return logr.Discard() }

// With a controller wired but no cache, EngageFluxWatch reports the nil-cache
// misconfiguration distinctly from the nil-controller case.
func TestEngageFluxWatch_NilCacheErrors(t *testing.T) {
	r := &SnippetReconciler{controller: &fakeWatchController{}}
	err := r.EngageFluxWatch(context.Background(), fluxSourceGVK("GitRepository"))
	if err == nil || !strings.Contains(err.Error(), "nil cache") {
		t.Fatalf("got %v, want nil-cache error", err)
	}
}

// --- isCrossNamespaceRef ----------------------------------------------------

// isCrossNamespaceRef only fires on an explicit, differing namespace; an
// empty ref namespace defaults to the owner's and is same-namespace.
func TestIsCrossNamespaceRef_Cases(t *testing.T) {
	tests := []struct {
		name    string
		ownerNs string
		ref     *jaasv1.SourceRef
		want    bool
	}{
		{"nil ref", "team-a", nil, false},
		{"empty ns defaults to owner", "team-a", &jaasv1.SourceRef{Name: "x"}, false},
		{"same ns explicit", "team-a", &jaasv1.SourceRef{Name: "x", Namespace: "team-a"}, false},
		{"different ns", "team-a", &jaasv1.SourceRef{Name: "x", Namespace: "team-b"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCrossNamespaceRef(tc.ownerNs, tc.ref); got != tc.want {
				t.Errorf("isCrossNamespaceRef = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- scrubbedCrossNamespaceLibraryMessage -----------------------------------

// The library cross-namespace scrub collapses to a constant that names only
// the library and its namespace, never the underlying failure cause.
func TestScrubbedCrossNamespaceLibraryMessage_NamesLibAndNs(t *testing.T) {
	got := scrubbedCrossNamespaceLibraryMessage("team-b", "shared")
	if !strings.Contains(got, "shared") || !strings.Contains(got, "team-b") {
		t.Errorf("message = %q, want library and namespace named", got)
	}
	if strings.Contains(got, "NotFound") || strings.Contains(got, "Forbidden") {
		t.Errorf("message must not leak failure cause: %q", got)
	}
}

// --- isPermanentAPIError ----------------------------------------------------

// isPermanentAPIError recognizes Forbidden as terminal and a plain network
// error as recoverable.
func TestIsPermanentAPIError_ForbiddenVsTransient(t *testing.T) {
	gr := schema.GroupResource{Group: "jaas.metio.wtf", Resource: "jsonnetsnippets"}
	if !isPermanentAPIError(apierrors.NewForbidden(gr, "demo", errors.New("no verb"))) {
		t.Error("Forbidden must be permanent")
	}
	if isPermanentAPIError(errors.New("connection reset")) {
		t.Error("plain network error must be transient")
	}
}
