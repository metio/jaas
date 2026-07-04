// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package operator

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/sources"
)

// recordingFetcher captures the ownerNs each Fetch call resolves against, so a
// test can pin which namespace a namespace-less sourceRef defaults to.
type recordingFetcher struct {
	result   *sources.Result
	err      error
	ownerNSs []string
}

func (s *recordingFetcher) Fetch(_ context.Context, _ client.Client, _ *jaasv1.SourceRef, ownerNs string) (*sources.Result, error) {
	s.ownerNSs = append(s.ownerNSs, ownerNs)
	return s.result, s.err
}

// A cross-namespace JsonnetLibrary whose own spec.sourceRef carries no
// namespace must resolve that source in the LIBRARY's namespace — the source
// lives beside the library, as the API docs state and the watch index assumes.
// Defaulting to the snippet's namespace would fetch a same-named source there,
// or nothing.
func TestReconcile_LibrarySourceRef_DefaultsToLibraryNamespace(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "libs-ns"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				// No namespace: defaults to the library's own (libs-ns).
				SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "lib-source"},
			},
		},
	}
	snip := sampleSnippet() // lives in team-a
	snip.Finalizers = []string{FinalizerName}
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "shared", Namespace: "libs-ns", ImportPath: "u"},
	}
	snip.Spec.Files = map[string]string{
		"main.jsonnet": `(import "u") + { wrap: true }`,
	}
	c := clientWithStatus(t, snip, lib)
	r := newReconciler(t, c)
	r.NoCrossNamespaceRefs = false // shared-library mode
	fetcher := &recordingFetcher{
		result: &sources.Result{
			Files: map[string]string{"main.libsonnet": `{ from: "fetched-lib" }`},
		},
	}
	r.Fetcher = fetcher

	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)

	if len(fetcher.ownerNSs) != 1 || fetcher.ownerNSs[0] != "libs-ns" {
		t.Fatalf("library sourceRef resolved against %v, want [libs-ns] (the library's namespace)", fetcher.ownerNSs)
	}
}

// A same-namespace library keeps resolving in that shared namespace — the two
// defaulting rules coincide and nothing changes for the common case.
func TestReconcile_LibrarySourceRef_SameNamespaceUnchanged(t *testing.T) {
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
	fetcher := &recordingFetcher{
		result: &sources.Result{
			Files: map[string]string{"main.libsonnet": `{ from: "fetched-lib" }`},
		},
	}
	r.Fetcher = fetcher

	runReconcile(t, r, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace})
	assertReady(t, refetch(t, c, types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}),
		metav1.ConditionTrue, ReasonSynced)
	if len(fetcher.ownerNSs) != 1 || fetcher.ownerNSs[0] != "team-a" {
		t.Fatalf("same-namespace library resolved against %v, want [team-a]", fetcher.ownerNSs)
	}
}

// The cycle walk must follow the same defaulting: a library's namespace-less
// ExternalArtifact sourceRef is an edge to the publishing snippet in the
// LIBRARY's namespace, or a chain through a shared library in another
// namespace escapes cycle detection while the fetch path loops.
func TestSnippetDependencies_LibrarySourceRefDefaultsToLibraryNamespace(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "libs-ns"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				// ExternalArtifact ref without a namespace → libs-ns.
				SourceRef: &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "publisher"},
			},
		},
	}
	snip := sampleSnippet() // team-a
	snip.Spec.Libraries = []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "shared", Namespace: "libs-ns"},
	}
	c := clientWithStatus(t, snip, lib)

	deps, err := snippetDependencies(context.Background(), c, snip)
	if err != nil {
		t.Fatalf("snippetDependencies: %v", err)
	}
	want := types.NamespacedName{Namespace: "libs-ns", Name: "publisher"}
	found := false
	for _, d := range deps {
		if d == want {
			found = true
		}
		if d.Namespace == snip.Namespace && d.Name == "publisher" {
			t.Fatalf("dependency edge resolved in the SNIPPET's namespace %v — the fetch will read libs-ns", d)
		}
	}
	if !found {
		t.Fatalf("deps = %v, want an edge to %v", deps, want)
	}
}
