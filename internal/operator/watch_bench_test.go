/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// BenchmarkMapJsonnetLibrary_NSnippets measures the cost of translating a
// JsonnetLibrary watch event into the set of dependent snippets, scaling
// the cluster's snippet count up to 1000.
//
// Caveat: fake.ClientBuilder's indexed List filters in-memory rather than
// using a btree-backed index, so the bench scales linearly in N. The real
// controller-runtime cache uses btree indexes and the production cost is
// flat in N for the same workload — the relative improvement vs. the
// pre-fix cluster-wide List remains the load-bearing observation.
//
// Run:
//
//	ilo bash -c 'go test -bench=BenchmarkMapJsonnetLibrary -benchmem -run=^$ ./internal/operator/'
//
// Reference baseline (AMD Ryzen 7 5700U, dev container, on 2026-06-11):
//
//	BenchmarkMapJsonnetLibrary_NSnippets/N=10     ~58 µs/op,   162 allocs/op
//	BenchmarkMapJsonnetLibrary_NSnippets/N=100    ~520 µs/op,  1158 allocs/op
//	BenchmarkMapJsonnetLibrary_NSnippets/N=1000   ~5.5 ms/op, 11074 allocs/op
//
// Pre-fix (cluster-wide List, profile capture at N=100): ~18.2 ms/op —
// ~35× regression compared to the post-fix indexed lookup, even with the
// fake client's linear filter.
func BenchmarkMapJsonnetLibrary_NSnippets(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			scheme := benchScheme(b)
			objs := make([]client.Object, 0, n+1)
			// Add the library every snippet references.
			objs = append(objs, &jaasv1.JsonnetLibrary{
				ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
			})
			// Single snippet that actually references the library; n-1
			// neighbors reference an unrelated library so the indexed
			// lookup must filter them out.
			objs = append(objs, &jaasv1.JsonnetSnippet{
				ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: "team-a"},
				Spec: jaasv1.JsonnetSnippetSpec{
					Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "utils"}},
				},
			})
			for i := 0; i < n-1; i++ {
				objs = append(objs, &jaasv1.JsonnetSnippet{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("other-%d", i), Namespace: "team-a"},
					Spec: jaasv1.JsonnetSnippetSpec{
						Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "irrelevant"}},
					},
				})
			}
			c := withReconcilerIndexesB(fake.NewClientBuilder().WithScheme(scheme)).WithObjects(objs...).Build()
			r := &SnippetReconciler{Client: c, Scheme: scheme, CycleCache: newCycleCache()}
			lib := &jaasv1.JsonnetLibrary{
				ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
			}

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				reqs := r.mapJsonnetLibrary(context.Background(), lib)
				if len(reqs) != 1 {
					b.Fatalf("iter %d: got %d requests, want 1", i, len(reqs))
				}
			}
		})
	}
}

// BenchmarkMapFluxSource_NSnippets measures the watch-event cost for the
// Flux source path, exercising both the direct edge (snippet's spec.sourceRef)
// and the indirect edge (library's spec.sourceRef) since mapFluxSource fans
// out to both. Same fake-client caveat as BenchmarkMapJsonnetLibrary above.
//
// Run:
//
//	ilo bash -c 'go test -bench=BenchmarkMapFluxSource -benchmem -run=^$ ./internal/operator/'
//
// Reference baseline (AMD Ryzen 7 5700U, dev container, on 2026-06-11):
//
//	BenchmarkMapFluxSource_NSnippets/N=10     ~133 µs/op,    355 allocs/op
//	BenchmarkMapFluxSource_NSnippets/N=100    ~1.05 ms/op,  2169 allocs/op
//	BenchmarkMapFluxSource_NSnippets/N=1000   ~11.2 ms/op, 20199 allocs/op
//
// Pre-fix (cluster-wide List + library cross-walk, profile capture at N=100):
// ~27.4 ms/op — ~26× regression compared to the post-fix indexed lookup.
func BenchmarkMapFluxSource_NSnippets(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			scheme := benchScheme(b)
			objs := []client.Object{
				&jaasv1.JsonnetLibrary{
					ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
					Spec: jaasv1.JsonnetLibrarySpec{
						SnippetSource: jaasv1.SnippetSource{
							SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"},
						},
					},
				},
				&jaasv1.JsonnetSnippet{
					ObjectMeta: metav1.ObjectMeta{Name: "direct", Namespace: "team-a"},
					Spec: jaasv1.JsonnetSnippetSpec{
						SnippetSource: jaasv1.SnippetSource{
							SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "configs"},
						},
					},
				},
				&jaasv1.JsonnetSnippet{
					ObjectMeta: metav1.ObjectMeta{Name: "indirect", Namespace: "team-a"},
					Spec: jaasv1.JsonnetSnippetSpec{
						Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "utils"}},
					},
				},
			}
			for i := 0; i < n-2; i++ {
				objs = append(objs, &jaasv1.JsonnetSnippet{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("other-%d", i), Namespace: "team-a"},
					Spec: jaasv1.JsonnetSnippetSpec{
						SnippetSource: jaasv1.SnippetSource{
							SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "irrelevant"},
						},
					},
				})
			}
			c := withReconcilerIndexesB(fake.NewClientBuilder().WithScheme(scheme)).WithObjects(objs...).Build()
			r := &SnippetReconciler{Client: c, Scheme: scheme, CycleCache: newCycleCache()}
			src := &unstructured.Unstructured{}
			src.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
			})
			src.SetName("configs")
			src.SetNamespace("team-a")

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				reqs := r.mapFluxSource(context.Background(), src)
				if len(reqs) != 2 {
					b.Fatalf("iter %d: got %d requests, want 2 (direct + indirect)", i, len(reqs))
				}
			}
		})
	}
}

// withReconcilerIndexesB is the *testing.B mirror of withReconcilerIndexes —
// fake.ClientBuilder's WithIndex doesn't accept a testing.TB, and the
// helper file expects testing.T. Keeping the bench's local copy avoids
// promoting the helper to testing.TB just for benchmarks.
func withReconcilerIndexesB(b *fake.ClientBuilder) *fake.ClientBuilder {
	return b.
		WithIndex(&jaasv1.JsonnetSnippet{}, snippetByLibraryRefIndex, snippetLibraryRefIDs).
		WithIndex(&jaasv1.JsonnetSnippet{}, snippetBySourceRefIndex, snippetSourceRefIDs).
		WithIndex(&jaasv1.JsonnetLibrary{}, libraryBySourceRefIndex, librarySourceRefIDs)
}
