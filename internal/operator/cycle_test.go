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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// snippetPointingAt is a tiny constructor for a snippet whose spec.sourceRef
// is a Kind=ExternalArtifact reference. Used by the cycle-detection tests.
func snippetPointingAt(name, namespace, eaName, eaNamespace string) *jaasv1.JsonnetSnippet {
	return &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind:      "ExternalArtifact",
					Name:      eaName,
					Namespace: eaNamespace,
				},
			},
		},
	}
}

func TestDetectSourceRefCycle_NilSnippet(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cycle {
		t.Errorf("nil snippet reported a cycle")
	}
}

func TestDetectSourceRefCycle_NoSourceRef(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	snip := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}}
	cycle, _, err := detectSourceRefCycle(context.Background(), c, snip)
	if err != nil {
		t.Fatal(err)
	}
	if cycle {
		t.Errorf("snippet without sourceRef reported a cycle")
	}
}

func TestDetectSourceRefCycle_NonEAKindIsTerminal(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "GitRepository", Name: "configs", Namespace: "ns",
				},
			},
		},
	}
	cycle, _, err := detectSourceRefCycle(context.Background(), c, snip)
	if err != nil {
		t.Fatal(err)
	}
	if cycle {
		t.Errorf("non-EA sourceRef reported a cycle")
	}
}

func TestDetectSourceRefCycle_SelfReferenceIsACycle(t *testing.T) {
	snip := snippetPointingAt("a", "ns", "a", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(snip).Build()
	cycle, path, err := detectSourceRefCycle(context.Background(), c, snip)
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Fatal("self-reference not detected")
	}
	if !strings.Contains(path, "ns/a") {
		t.Errorf("path %q does not include the snippet's identity", path)
	}
}

func TestDetectSourceRefCycle_SelfReferenceWithImplicitNamespace(t *testing.T) {
	// sourceRef.Namespace empty → defaults to snippet's own ns; still a cycle.
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "ExternalArtifact", Name: "a"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(snip).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, snip)
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Error("self-reference with implicit namespace not detected")
	}
}

func TestDetectSourceRefCycle_TwoSnippetCycle(t *testing.T) {
	a := snippetPointingAt("a", "ns", "b", "ns")
	b := snippetPointingAt("b", "ns", "a", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b).Build()
	cycle, path, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Fatalf("two-snippet cycle not detected; path=%q", path)
	}
	if !strings.Contains(path, "ns/a") || !strings.Contains(path, "ns/b") {
		t.Errorf("path %q does not show both nodes", path)
	}
}

func TestDetectSourceRefCycle_ThreeSnippetCycle(t *testing.T) {
	a := snippetPointingAt("a", "ns", "b", "ns")
	b := snippetPointingAt("b", "ns", "c", "ns")
	cs := snippetPointingAt("c", "ns", "a", "ns")
	cli := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b, cs).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), cli, a)
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Error("three-snippet cycle not detected")
	}
}

func TestDetectSourceRefCycle_LinearChainTerminatesWithNotFound(t *testing.T) {
	// a → b → (b doesn't exist) is fine — the chain ends.
	a := snippetPointingAt("a", "ns", "b", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if cycle {
		t.Errorf("linear chain reported a cycle")
	}
}

func TestDetectSourceRefCycle_LinearChainTerminatesAtNonCyclicSource(t *testing.T) {
	// a → b → (b has no sourceRef). No cycle.
	a := snippetPointingAt("a", "ns", "b", "ns")
	b := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if cycle {
		t.Errorf("linear chain reported a cycle")
	}
}

func TestDetectSourceRefCycle_CrossNamespaceChain(t *testing.T) {
	a := snippetPointingAt("a", "team-a", "b", "team-b")
	b := snippetPointingAt("b", "team-b", "a", "team-a")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Error("cross-namespace cycle not detected")
	}
}

func TestDetectSourceRefCycle_GetErrorOtherThanNotFoundPropagates(t *testing.T) {
	want := errors.New("apiserver flaky")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return want
			},
		}).Build()
	a := snippetPointingAt("a", "ns", "b", "ns")
	_, _, err := detectSourceRefCycle(context.Background(), c, a)
	if !errors.Is(err, want) {
		t.Errorf("got %v, want %v wrapped", err, want)
	}
}

// formatCyclePath helper smoke test
func TestFormatCyclePath_EmptySliceReturnsEmpty(t *testing.T) {
	if got := formatCyclePath(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// --- Library-mediated cycles (snippet → library → sourceRef → snippet) ----

// snippetWithLibRef is a snippet that references one library; the library
// itself may carry an ExternalArtifact sourceRef to form a cycle.
func snippetWithLibRef(name, namespace, libKind, libName, libNamespace string) *jaasv1.JsonnetSnippet {
	return &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{
				{Kind: libKind, Name: libName, Namespace: libNamespace},
			},
		},
	}
}

func TestDetectSourceRefCycle_NamespacedLibrarySourceRefCyclesBackToSnippet(t *testing.T) {
	a := snippetWithLibRef("a", "ns", "JsonnetLibrary", "utils", "ns")
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "ns"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "ExternalArtifact", Name: "a", Namespace: "ns",
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, lib).Build()
	cycle, path, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Fatalf("library-mediated cycle not detected; path=%q", path)
	}
	if !strings.Contains(path, "ns/a") {
		t.Errorf("path %q does not mention the snippet", path)
	}
}

func TestDetectSourceRefCycle_LibrarySourceRefDefaultsNamespaceToSnippet(t *testing.T) {
	// Library's sourceRef.Namespace is empty; under ownerNs=snip.Namespace
	// defaulting, it points back at the snippet's own EA — still a cycle.
	a := snippetWithLibRef("a", "ns", "JsonnetLibrary", "utils", "")
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "ns"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "ExternalArtifact", Name: "a",
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, lib).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Error("implicit-namespace library cycle not detected")
	}
}

func TestDetectSourceRefCycle_LibraryWithoutSourceRefIsNotACycle(t *testing.T) {
	// Inline-files library — no edge from a to itself even though a uses it.
	a := snippetWithLibRef("a", "ns", "JsonnetLibrary", "utils", "ns")
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "ns"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": "{}"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, lib).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if cycle {
		t.Errorf("inline-files library reported a cycle")
	}
}

func TestDetectSourceRefCycle_LibraryNotFoundIsLeafEdge(t *testing.T) {
	// Snippet references a library that doesn't exist. The reconciler
	// would later fail with LibraryNotFound, but cycle detection treats
	// the missing edge as a leaf — no false positive.
	a := snippetWithLibRef("a", "ns", "JsonnetLibrary", "ghost", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if cycle {
		t.Errorf("missing library reported as cycle")
	}
}

func TestDetectSourceRefCycle_MixedDirectAndLibraryEdges(t *testing.T) {
	// a → b via library, b → a via direct sourceRef → cycle through both paths.
	a := snippetWithLibRef("a", "ns", "JsonnetLibrary", "utils", "ns")
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "ns"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "ExternalArtifact", Name: "b", Namespace: "ns",
				},
			},
		},
	}
	b := snippetPointingAt("b", "ns", "a", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b, lib).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if !cycle {
		t.Error("mixed direct+library cycle not detected")
	}
}

func TestDetectSourceRefCycle_LibraryGetErrorPropagates(t *testing.T) {
	a := snippetWithLibRef("a", "ns", "JsonnetLibrary", "utils", "ns")
	want := errors.New("apiserver hiccup")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(a).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, k client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				if k.Name == "utils" {
					return want
				}
				return nil
			},
		}).Build()
	_, _, err := detectSourceRefCycle(context.Background(), c, a)
	if !errors.Is(err, want) {
		t.Errorf("got %v, want %v wrapped", err, want)
	}
}

func TestDetectSourceRefCycle_UnknownLibraryKindIsLeafEdge(t *testing.T) {
	// snippet references a library of an unsupported kind — cycle
	// detection treats the edge as absent (the reconciler later fails the
	// snippet for InvalidSpec, but cycle detection doesn't double-fail).
	a := snippetWithLibRef("a", "ns", "ConfigMap", "weird", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a).Build()
	cycle, _, err := detectSourceRefCycle(context.Background(), c, a)
	if err != nil {
		t.Fatal(err)
	}
	if cycle {
		t.Errorf("unknown library kind reported as cycle")
	}
}
