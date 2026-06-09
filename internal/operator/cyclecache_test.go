/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	jaasv1 "github.com/metio/jaas/api/v1"
)

func TestCycleCache_NilReceiverIsSafe(t *testing.T) {
	var c *cycleCache
	if _, _, ok := c.Lookup("uid", 1); ok {
		t.Error("nil.Lookup returned hit")
	}
	if ok := c.Store("uid", 1, 0, false, ""); ok {
		t.Error("nil.Store reported success")
	}
	c.Forget("uid") // must not panic
}

func TestCycleCache_EmptyUIDIsMiss(t *testing.T) {
	c := newCycleCache()
	if ok := c.Store("", 1, 0, false, ""); ok {
		t.Error("empty UID Store should be a no-op (returns false)")
	}
	if _, _, ok := c.Lookup("", 1); ok {
		t.Error("empty UID should never hit; cycle decisions must run for unowned snippets")
	}
}

func TestCycleCache_GenerationMismatchMisses(t *testing.T) {
	c := newCycleCache()
	c.Store("uid", 1, 0, false, "")
	if _, _, ok := c.Lookup("uid", 2); ok {
		t.Error("generation 2 returned a hit against a generation-1 entry")
	}
}

func TestCycleCache_ForgetEvicts(t *testing.T) {
	c := newCycleCache()
	c.Store("uid", 1, 0, true, "ns/a → ns/a")
	c.Forget("uid")
	if _, _, ok := c.Lookup("uid", 1); ok {
		t.Error("entry remained after Forget")
	}
}

// TestCycleCache_StoreDropsWhenEpochMovedAfterLookup pins that a Forget
// landing between a walk's Lookup and its post-walk Store must drop the
// (now-stale) write. The epoch counter encodes "the world hasn't changed
// since I looked"; a non-matching epoch means a concurrent Forget
// invalidated the entry while we were walking.
func TestCycleCache_StoreDropsWhenEpochMovedAfterLookup(t *testing.T) {
	c := newCycleCache()
	_, epoch, _ := c.Lookup("uid", 1)        // observe initial epoch
	c.Forget("uid")                          // concurrent invalidation
	if c.Store("uid", 1, epoch, false, "") { // walk completes; tries to Store
		t.Error("Store wrote with a stale epoch; Forget invalidation was silently lost")
	}
	// The next Lookup must miss — the dropped Store left the cache empty,
	// not seeded with the stale verdict.
	if _, _, ok := c.Lookup("uid", 1); ok {
		t.Error("post-drop Lookup hit; cache should be empty after Forget+stale-Store")
	}
}

// TestCycleCache_StoreSucceedsWhenEpochUnchanged is the positive control:
// without a concurrent Forget, the post-walk Store writes through and a
// subsequent Lookup hits.
func TestCycleCache_StoreSucceedsWhenEpochUnchanged(t *testing.T) {
	c := newCycleCache()
	_, epoch, _ := c.Lookup("uid", 1)
	if !c.Store("uid", 1, epoch, false, "no cycle") {
		t.Fatal("Store reported failure despite unchanged epoch")
	}
	v, _, ok := c.Lookup("uid", 1)
	if !ok {
		t.Fatal("Store-then-Lookup did not hit")
	}
	if v.hasCycle || v.path != "no cycle" {
		t.Errorf("Lookup returned wrong verdict: %+v", v)
	}
}

// TestCycleCache_ForgetWithNoEntryStillBumpsEpoch covers the edge case
// where a watch event fires Forget before any walk has populated the
// cache. The epoch must still bump so a subsequent walk-and-Store that
// raced with the Forget drops its write.
func TestCycleCache_ForgetWithNoEntryStillBumpsEpoch(t *testing.T) {
	c := newCycleCache()
	_, epoch, _ := c.Lookup("uid", 1) // epoch=0, entry absent
	c.Forget("uid")                   // bumps epoch even though entry absent
	if c.Store("uid", 1, epoch, false, "") {
		t.Error("Store wrote with the pre-Forget epoch; Forget on an empty cache must still invalidate")
	}
}

func TestHasCycleSourceEdge_TrivialSnippetReturnsFalse(t *testing.T) {
	// No sourceRef + no libraries → no edges to walk; fast-path skips
	// the BFS entirely. This is the dominant production case.
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true }`},
			},
		},
	}
	if hasCycleSourceEdge(snip) {
		t.Error("inline-files snippet incorrectly classified as having a dependency edge")
	}
}

func TestHasCycleSourceEdge_NonEASourceRefReturnsFalse(t *testing.T) {
	// GitRepository / OCIRepository / Bucket sourceRefs cannot loop back
	// to a JaaS snippet — only ExternalArtifact carries a publishing
	// snippet identity.
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"},
			},
		},
	}
	if hasCycleSourceEdge(snip) {
		t.Error("GitRepository sourceRef incorrectly classified as a cycle-capable edge")
	}
}

func TestHasCycleSourceEdge_ExternalArtifactReturnsTrue(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "upstream"},
			},
		},
	}
	if !hasCycleSourceEdge(snip) {
		t.Error("ExternalArtifact sourceRef should require a walk")
	}
}

func TestHasCycleSourceEdge_LibraryRefReturnsTrue(t *testing.T) {
	// Library refs short-circuit to true even when the library itself
	// has inline files — the library may be modified later to point at
	// an ExternalArtifact, and we have no way to know without a Get.
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "common"}},
		},
	}
	if !hasCycleSourceEdge(snip) {
		t.Error("snippet with library refs should require a walk")
	}
}

func TestCycleVerdict_CachedHitSkipsWalk(t *testing.T) {
	// Build a fake client that fails on any Get — if the cache hits, the
	// walk never runs and the Get isn't reached.
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SnippetReconciler{Client: c, Scheme: scheme, CycleCache: newCycleCache()}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a", Namespace: "team-a", UID: types.UID("uid-a"), Generation: 5,
		},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "upstream"},
			},
		},
	}
	r.CycleCache.Store(snip.UID, snip.Generation, 0, false, "")

	cycle, _, err := r.cycleVerdict(context.Background(), snip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cycle {
		t.Error("cached no-cycle verdict was overridden")
	}
}

func TestCycleVerdict_GenerationBumpInvalidates(t *testing.T) {
	// Cached entry is for Generation=1; snip arrives at Generation=2.
	// The cache misses, the walk runs (snippet has no deps via the
	// fast-path), and the verdict is recomputed and stored.
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SnippetReconciler{Client: c, Scheme: scheme, CycleCache: newCycleCache()}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a", Namespace: "team-a", UID: types.UID("uid-a"), Generation: 2,
		},
	}
	r.CycleCache.Store(snip.UID, 1, 0, false, "") // stale entry at generation 1

	if _, _, err := r.cycleVerdict(context.Background(), snip); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Subsequent lookup at Generation=2 must now hit the freshly-stored entry.
	if v, _, ok := r.CycleCache.Lookup(snip.UID, 2); !ok || v.hasCycle {
		t.Errorf("re-stored verdict missing or wrong: hit=%v verdict=%+v", ok, v)
	}
}

// TestCycleVerdict_RetriesOnForgetDuringWalk pins the end-to-end fix:
// when a watch handler's Forget races with an in-flight walk, the walk's
// Store is dropped AND the next walk picks up the freshest state. Drive
// the race deterministically by injecting a fake client whose Get fires
// Forget on the first call — simulating "a watch event landed mid-walk".
//
// First call: walk runs, Forget bumps epoch mid-Get, walk's Store is
// dropped, cycleVerdict retries → second walk runs cleanly → Store
// succeeds. The cache ends with the verdict from the second walk.
func TestCycleVerdict_RetriesOnForgetDuringWalk(t *testing.T) {
	scheme := testScheme(t)
	libSnip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a", Namespace: "team-a", UID: types.UID("uid-a"), Generation: 1,
		},
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "common"}},
		},
	}
	cache := newCycleCache()
	calls := 0
	bareClient := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(scheme)).
		WithObjects(libSnip,
			&jaasv1.JsonnetLibrary{ObjectMeta: metav1.ObjectMeta{Name: "common", Namespace: "team-a"}}).
		Build()
	c := &forgetMidWalkClient{
		Client: bareClient,
		onGet: func() {
			calls++
			// Only Forget on the FIRST Get the walk does — otherwise the
			// retry loop would spin until maxCycleVerdictRetries fell
			// through, which is fine for the race but not the test's
			// intent (we want to assert the cache gets populated on the
			// retry).
			if calls == 1 {
				cache.Forget(libSnip.UID)
			}
		},
	}
	r := &SnippetReconciler{Client: c, Scheme: scheme, CycleCache: cache}

	if _, _, err := r.cycleVerdict(context.Background(), libSnip); err != nil {
		t.Fatalf("cycleVerdict errored: %v", err)
	}
	// The retry walk must have produced a stored verdict.
	if _, _, ok := cache.Lookup(libSnip.UID, libSnip.Generation); !ok {
		t.Error("expected cycleVerdict's retry to leave a cached verdict; cache is still empty")
	}
	// Without the epoch check, the first walk would happily Store and the
	// retry would skip the walk (cache hit). The fix forces a second
	// walk after the Forget bumps the epoch.
	if calls < 2 {
		t.Errorf("walk Get calls = %d, want >= 2 (initial walk + retry after Forget)", calls)
	}
}

// forgetMidWalkClient lets a test drive a Forget side-effect from inside
// a fake-client Get — simulating a watch event landing mid-walk without
// goroutine-level timing.
type forgetMidWalkClient struct {
	client.Client
	onGet func()
}

func (f *forgetMidWalkClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if f.onGet != nil {
		f.onGet()
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

// TestCycleVerdict_EmptyUIDLogsDebug pins that an empty UID makes the
// cycleCache silently no-op (the apiserver only assigns UIDs on Create,
// so this can only happen in unit tests that build snippets via fake
// clients without seeding metadata.uid). A Debug log surfaces the gap
// so a test that intends to assert cache-hit semantics doesn't silently
// pass by always missing.
func TestCycleVerdict_EmptyUIDLogsDebug(t *testing.T) {
	scheme := testScheme(t)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "no-uid", Namespace: "team-a", Generation: 1,
			// UID intentionally absent
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	var buf bytes.Buffer
	r := &SnippetReconciler{
		Client:     c,
		Scheme:     scheme,
		CycleCache: newCycleCache(),
		Logger:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if _, _, err := r.cycleVerdict(context.Background(), snip); err != nil {
		t.Fatalf("cycleVerdict: %v", err)
	}
	if !strings.Contains(buf.String(), "cycleCache cannot engage") {
		t.Errorf("expected an empty-UID debug log; got %q", buf.String())
	}
}

func TestMapJsonnetLibrary_InvalidatesCycleCacheForDependents(t *testing.T) {
	// One library, two snippets — only A references it. The library
	// watch must invalidate A's cycle cache entry but leave B's intact.
	scheme := testScheme(t)
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "common", Namespace: "team-a"},
	}
	snipA := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a", Namespace: "team-a", UID: types.UID("uid-a"), Generation: 1,
		},
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "common"}},
		},
	}
	snipB := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "b", Namespace: "team-a", UID: types.UID("uid-b"), Generation: 1,
		},
	}
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(scheme)).WithObjects(snipA, snipB).Build()
	r := &SnippetReconciler{Client: c, Scheme: scheme, CycleCache: newCycleCache()}
	r.CycleCache.Store(snipA.UID, 1, 0, false, "")
	r.CycleCache.Store(snipB.UID, 1, 0, false, "")

	reqs := r.mapJsonnetLibrary(context.Background(), lib)
	if len(reqs) != 1 {
		t.Fatalf("expected 1 reconcile request (A only), got %d", len(reqs))
	}
	if _, _, ok := r.CycleCache.Lookup(snipA.UID, 1); ok {
		t.Error("A's cache entry survived a library change it depends on")
	}
	if _, _, ok := r.CycleCache.Lookup(snipB.UID, 1); !ok {
		t.Error("B's cache entry was invalidated even though B doesn't reference the library")
	}
}

func TestReconcile_CycleCachePopulatedOnFirstReconcile(t *testing.T) {
	// Smoke test: a normal reconcile of a trivial snippet stamps a
	// no-cycle entry in the cache. Future reconciles at the same
	// Generation skip the BFS entirely.
	snip := sampleSnippet()
	snip.UID = types.UID("uid-smoke")
	snip.Finalizers = []string{FinalizerName}
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.CycleCache = newCycleCache()

	runReconcile(t, r, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace},
	}.NamespacedName)
	if v, _, ok := r.CycleCache.Lookup(snip.UID, snip.Generation); !ok || v.hasCycle {
		t.Errorf("cycle cache not populated with no-cycle verdict: hit=%v verdict=%+v", ok, v)
	}
}
