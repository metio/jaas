/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// snippetWithLibrary returns a JsonnetSnippet referencing one library.
func snippetWithLibrary(name, namespace, libKind, libName, libNamespace string) *jaasv1.JsonnetSnippet {
	return &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{
				{Kind: libKind, Name: libName, Namespace: libNamespace},
			},
		},
	}
}

func snippetWithSource(name, namespace string, ref *jaasv1.SourceRef) *jaasv1.JsonnetSnippet {
	return &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{SourceRef: ref},
		},
	}
}

// reqNames extracts namespaced names for assertion. Sorted for stable
// comparison across map-iteration order.
func reqNames(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.NamespacedName.String())
	}
	sort.Strings(out)
	return out
}

// --- mapJsonnetLibrary ------------------------------------------------------

func TestMapJsonnetLibrary_MatchesSameNamespaceReference(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithLibrary("snip", "team-a", "JsonnetLibrary", "utils", "team-a"),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapJsonnetLibrary(context.Background(), &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
	})
	got := reqNames(reqs)
	if len(got) != 1 || got[0] != "team-a/snip" {
		t.Errorf("got %v, want [team-a/snip]", got)
	}
}

func TestMapJsonnetLibrary_MatchesEmptyNamespaceDefaultingToOwner(t *testing.T) {
	// Ref with empty namespace defaults to the snippet's own namespace.
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithLibrary("snip", "team-a", "JsonnetLibrary", "utils", ""),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapJsonnetLibrary(context.Background(), &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
	})
	if got := reqNames(reqs); len(got) != 1 {
		t.Errorf("got %v, want one match", got)
	}
}

func TestMapJsonnetLibrary_DoesNotMatchCrossNamespaceWhenRefIsExplicit(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithLibrary("snip", "team-a", "JsonnetLibrary", "utils", "team-b"),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapJsonnetLibrary(context.Background(), &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
	})
	if got := reqNames(reqs); len(got) != 0 {
		t.Errorf("got %v, want no matches (snippet referenced team-b)", got)
	}
}

func TestMapJsonnetLibrary_MatchesAcrossManySnippets(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithLibrary("a", "team-a", "JsonnetLibrary", "utils", "team-a"),
		snippetWithLibrary("b", "team-a", "JsonnetLibrary", "utils", "team-a"),
		snippetWithLibrary("c", "team-a", "JsonnetLibrary", "other", "team-a"),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapJsonnetLibrary(context.Background(), &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
	})
	want := []string{"team-a/a", "team-a/b"}
	if got := reqNames(reqs); !equalSorted(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMapJsonnetLibrary_WrongObjectTypeReturnsNil(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).Build()
	r := newReconciler(t, c)
	if got := r.mapJsonnetLibrary(context.Background(), &jaasv1.JsonnetSnippet{}); got != nil {
		t.Errorf("got %v, want nil for wrong obj type", got)
	}
}

func TestMapJsonnetLibrary_ListErrorSwallowedReturnsNil(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).Build()
	// No objects, but the empty list is itself a successful result; this
	// case just exercises the "no matches" path.
	r := newReconciler(t, c)
	got := r.mapJsonnetLibrary(context.Background(), &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
	})
	if got != nil {
		t.Errorf("got %v, want nil for empty cluster", got)
	}
}

// --- mapFluxSource ----------------------------------------------------------

func unstructuredSource(kind, apiVersion, name, namespace string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, kind))
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

func TestMapFluxSource_MatchesSpecSourceRef(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithSource("snip", "team-a", &jaasv1.SourceRef{
			Kind:      "GitRepository",
			Name:      "configs",
			Namespace: "team-a",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 1 || got[0] != "team-a/snip" {
		t.Errorf("got %v, want [team-a/snip]", got)
	}
}

func TestMapFluxSource_ImplicitNamespaceMatches(t *testing.T) {
	// ref.Namespace empty → defaults to snippet's own namespace; the source
	// CR in that namespace should match.
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithSource("snip", "team-a", &jaasv1.SourceRef{
			Kind: "GitRepository", Name: "configs",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 1 {
		t.Errorf("got %v, want one match", got)
	}
}

func TestMapFluxSource_DifferentNamespaceDoesNotMatch(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithSource("snip", "team-a", &jaasv1.SourceRef{
			Kind: "GitRepository", Name: "configs",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "other-ns"))
	if got := reqNames(reqs); len(got) != 0 {
		t.Errorf("got %v, want no match (source in other-ns)", got)
	}
}

func TestMapFluxSource_DifferentKindDoesNotMatch(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithSource("snip", "team-a", &jaasv1.SourceRef{
			Kind: "OCIRepository", Name: "configs", Namespace: "team-a",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 0 {
		t.Errorf("got %v, want no match (kind mismatch)", got)
	}
}

// TestMapFluxSource_DifferentAPIVersionStillMatches pins that a snippet
// pinning spec.sourceRef.apiVersion=v1beta2 must still match a watch
// event for the same Kind/Namespace/Name even when the event arrives
// stamped v1 (SetupWithManager registers watches against v1). CRDs
// have one storage version per cluster, so the underlying object is the
// same regardless of which schema version the snippet's spec labels —
// missing the enqueue would silently drop a tenant's source-update
// re-renders.
func TestMapFluxSource_DifferentAPIVersionStillMatches(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetWithSource("snip", "team-a", &jaasv1.SourceRef{
			Kind: "GitRepository", Name: "configs", Namespace: "team-a",
			APIVersion: "source.toolkit.fluxcd.io/v1beta2",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 1 || got[0] != "team-a/snip" {
		t.Errorf("got %v, want [team-a/snip] — v1beta2 ref should still match a v1-stamped event", got)
	}
}

func TestMapFluxSource_SnippetWithoutSourceRefIsSkipped(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		&jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-a"}},
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 0 {
		t.Errorf("got %v, want no match (snippet has no sourceRef)", got)
	}
}

// --- List-error swallowing -------------------------------------------------

// clientThatFailsList returns a fake client whose every List call errors.
// Used by the watch-handler tests to confirm a List failure surfaces as
// nil (no requests) rather than a crash or stale enqueue.
func clientThatFailsList(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errListFailed
			},
		}).Build()
}

var errListFailed = errors.New("apiserver flaky")

func TestMapJsonnetLibrary_ListErrorReturnsNilSilently(t *testing.T) {
	r := newReconciler(t, clientThatFailsList(t))
	if got := r.mapJsonnetLibrary(context.Background(),
		&jaasv1.JsonnetLibrary{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"}}); got != nil {
		t.Errorf("got %v, want nil on List failure", got)
	}
}

func TestMapFluxSource_SnippetListErrorReturnsNil(t *testing.T) {
	r := newReconciler(t, clientThatFailsList(t))
	if got := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a")); got != nil {
		t.Errorf("got %v, want nil on List failure", got)
	}
}

// TestMapFluxSource_LibListErrorKeepsDirectMatches pins that when the
// JsonnetLibrary List fails mid-event (transient 5xx, throttled cache),
// the direct-edge snippets already matched against snippet.spec.sourceRef
// must still be enqueued. Pre-fix the handler returned nil and silently
// dropped them — direct snippets could miss their re-render until the
// source's next periodic resync (minutes for some Flux source kinds).
func TestMapFluxSource_LibListErrorKeepsDirectMatches(t *testing.T) {
	scheme := testScheme(t)
	directSnip := snippetWithSource("direct", "team-a", &jaasv1.SourceRef{
		Kind: "GitRepository", Name: "configs", Namespace: "team-a",
	})
	bareClient := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(scheme)).
		WithObjects(directSnip).Build()
	calls := 0
	c := &listFailOnLibClient{
		WithWatch: bareClient,
		onList: func(obj client.ObjectList) error {
			calls++
			if _, isLibList := obj.(*jaasv1.JsonnetLibraryList); isLibList {
				return errListFailed
			}
			return nil
		},
	}
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 1 || got[0] != "team-a/direct" {
		t.Errorf("got %v, want [team-a/direct] — direct match must survive a library-list failure", got)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 List calls (direct snippets + libraries); got %d", calls)
	}
}

// listFailOnLibClient wraps a client.WithWatch and intercepts List calls
// to inject a per-list error decided by the test. The underlying client
// is consulted only when onList returns nil; otherwise the injected
// error is returned and the underlying client never runs the List.
type listFailOnLibClient struct {
	client.WithWatch
	onList func(obj client.ObjectList) error
}

func (f *listFailOnLibClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := f.onList(list); err != nil {
		return err
	}
	return f.WithWatch.List(ctx, list, opts...)
}

// --- Indirect Flux source watch (snippet → library → sourceRef) -----------

// snippetReferencingLib wraps snippetWithLibrary with the explicit
// "snippet references library, library has no source" shape used by the
// indirect-watch tests below.
func snippetReferencingLib(snipName, snipNs, libKind, libName, libNs string) *jaasv1.JsonnetSnippet {
	return snippetWithLibrary(snipName, snipNs, libKind, libName, libNs)
}

func namespacedLibWithSource(name, namespace string, ref *jaasv1.SourceRef) *jaasv1.JsonnetLibrary {
	return &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{SourceRef: ref},
		},
	}
}

func TestMapFluxSource_Indirect_NamespacedLibraryMatchesAndWakesItsSnippets(t *testing.T) {
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetReferencingLib("snip", "team-a", "JsonnetLibrary", "utils", "team-a"),
		namespacedLibWithSource("utils", "team-a", &jaasv1.SourceRef{
			Kind: "GitRepository", Name: "configs", Namespace: "team-a",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 1 || got[0] != "team-a/snip" {
		t.Errorf("got %v, want [team-a/snip]", got)
	}
}

func TestMapFluxSource_Indirect_LibrarySourceRefDefaultsToSnippetNamespace(t *testing.T) {
	// Library's spec.sourceRef has no Namespace — under the existing
	// resolveSnippetSource rules, it defaults to the SNIPPET's namespace
	// (not the library's). The watch must mirror that defaulting so a
	// source event triggers the snippets actually using it.
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetReferencingLib("snip", "team-a", "JsonnetLibrary", "utils", "team-a"),
		namespacedLibWithSource("utils", "team-a", &jaasv1.SourceRef{
			Kind: "GitRepository", Name: "configs",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 1 {
		t.Errorf("got %v, want one match via defaulting", got)
	}
}

func TestMapFluxSource_Indirect_UnrelatedLibrarySourceRefIsIgnored(t *testing.T) {
	// Library's sourceRef points at a DIFFERENT source; the watched source
	// shouldn't wake snippets that reference this library.
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetReferencingLib("snip", "team-a", "JsonnetLibrary", "utils", "team-a"),
		namespacedLibWithSource("utils", "team-a", &jaasv1.SourceRef{
			Kind: "GitRepository", Name: "other-source", Namespace: "team-a",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 0 {
		t.Errorf("got %v, want no matches (lib's sourceRef points elsewhere)", got)
	}
}

func TestMapFluxSource_Indirect_LibraryWithoutSourceRefIsIgnored(t *testing.T) {
	// A library with inline files (no spec.sourceRef) can't be affected by
	// a Flux source event.
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetReferencingLib("snip", "team-a", "JsonnetLibrary", "utils", "team-a"),
		&jaasv1.JsonnetLibrary{
			ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
			Spec: jaasv1.JsonnetLibrarySpec{
				SnippetSource: jaasv1.SnippetSource{
					Files: map[string]string{"main.libsonnet": "{}"},
				},
			},
		},
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 0 {
		t.Errorf("got %v, want no matches (lib has no sourceRef)", got)
	}
}

func TestMapFluxSource_DirectAndIndirectDeduplicateOnSameSnippet(t *testing.T) {
	// One snippet has BOTH a direct sourceRef and a library indirection
	// pointing at the same source — the snippet must appear once in the
	// resulting requests.
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		&jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{Name: "snip", Namespace: "team-a"},
			Spec: jaasv1.JsonnetSnippetSpec{
				SnippetSource: jaasv1.SnippetSource{
					SourceRef: &jaasv1.SourceRef{
						Kind: "GitRepository", Name: "configs", Namespace: "team-a",
					},
				},
				Libraries: []jaasv1.LibraryRef{
					{Kind: "JsonnetLibrary", Name: "utils", Namespace: "team-a"},
				},
			},
		},
		namespacedLibWithSource("utils", "team-a", &jaasv1.SourceRef{
			Kind: "GitRepository", Name: "configs", Namespace: "team-a",
		}),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 1 || got[0] != "team-a/snip" {
		t.Errorf("got %v, want exactly [team-a/snip] (no duplicates)", got)
	}
}

func TestMapFluxSource_Indirect_OrphanLibraryReferenceIsSkipped(t *testing.T) {
	// Snippet references a library that doesn't exist — the watch must
	// not crash and must not enqueue the snippet on a source event for
	// some other source.
	c := withReconcilerIndexes(fake.NewClientBuilder().WithScheme(testScheme(t))).WithObjects(
		snippetReferencingLib("snip", "team-a", "JsonnetLibrary", "ghost", "team-a"),
	).Build()
	r := newReconciler(t, c)
	reqs := r.mapFluxSource(context.Background(),
		unstructuredSource("GitRepository", "source.toolkit.fluxcd.io/v1", "configs", "team-a"))
	if got := reqNames(reqs); len(got) != 0 {
		t.Errorf("got %v, want no matches (lib doesn't exist)", got)
	}
}

// --- FluxSourceKinds + GVK helpers ------------------------------------------

func TestFluxSourceKinds_IncludesExternalArtifact(t *testing.T) {
	// ExternalArtifact is intentionally watched: chained snippets re-render
	// when an upstream snippet republishes. The cycle detector
	// (detectSourceRefCycle) prevents the publish → watch → reconcile →
	// publish loop a cyclic sourceRef chain would otherwise create.
	for _, k := range FluxSourceKinds {
		if k == "ExternalArtifact" {
			return
		}
	}
	t.Errorf("FluxSourceKinds should include ExternalArtifact")
}

func TestFluxSourceGVK_AlwaysReturnsV1(t *testing.T) {
	gvk := fluxSourceGVK("GitRepository")
	if gvk.Group != "source.toolkit.fluxcd.io" || gvk.Version != "v1" || gvk.Kind != "GitRepository" {
		t.Errorf("unexpected GVK: %+v", gvk)
	}
}

// equalSorted compares two []string for equality after both are sorted.
func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := make([]string, len(a))
	copy(x, a)
	y := make([]string, len(b))
	copy(y, b)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// Silence unused-import warnings.
var (
	_ types.NamespacedName
	_ client.Object
)
