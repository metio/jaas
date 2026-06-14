/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

func publisherScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := jaasv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme jaas: %v", err)
	}
	// Register ExternalArtifact as an unstructured type so fake client
	// CRUD against this GVK works without a typed Go shape.
	s.AddKnownTypeWithName(externalArtifactGVK, &unstructured.Unstructured{})
	gvkList := externalArtifactGVK
	gvkList.Kind = externalArtifactGVK.Kind + "List"
	s.AddKnownTypeWithName(gvkList, &unstructured.UnstructuredList{})
	return s
}

func newTestPublisher(t *testing.T, _ client.Client) *Publisher {
	t.Helper()
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := NewPublisher(store, "http://jaas-operator.jaas-system.svc.cluster.local:8082")
	p.Clock = func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) }
	return p
}

func newPublisherClient(t *testing.T, opts ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(opts...).
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

func fetchExternalArtifact(t *testing.T, c client.Client, name, namespace string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(externalArtifactGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, u); err != nil {
		t.Fatalf("get ExternalArtifact %s/%s: %v", namespace, name, err)
	}
	return u
}

func TestPublish_RenderedMode_WritesArtifactAndStatus(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{Output: jaasv1.OutputRendered},
	}
	rendered := `{"ok":true}`
	res, err := p.Publish(context.Background(), c, snip, rendered, nil, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Revision == "" {
		t.Fatalf("Publish returned empty revision")
	}
	if res.URL == "" {
		t.Fatalf("Publish returned empty URL")
	}

	ea := fetchExternalArtifact(t, c, snip.Name, snip.Namespace)
	artifact, found, err := unstructured.NestedMap(ea.Object, "status", "artifact")
	if err != nil || !found {
		t.Fatalf("status.artifact missing: found=%v err=%v", found, err)
	}
	if got, _ := artifact["revision"].(string); got != res.Revision {
		t.Errorf("status.artifact.revision = %q, want %q", got, res.Revision)
	}
	if got, _ := artifact["url"].(string); got != res.URL {
		t.Errorf("EA status.artifact.url = %q, want %q (matches Publish return)", got, res.URL)
	}
	if got, _ := artifact["digest"].(string); got == "" {
		t.Errorf("status.artifact.digest is empty")
	}
	if got, _ := artifact["lastUpdateTime"].(string); got != "2026-06-09T12:00:00Z" {
		t.Errorf("status.artifact.lastUpdateTime = %q, want 2026-06-09T12:00:00Z", got)
	}
	if got, _ := artifact["size"].(int64); got <= 0 {
		t.Errorf("status.artifact.size = %d, want > 0", got)
	}
}

// readyConditionOf returns the Ready condition map from an EA's
// status.conditions, or nil if absent.
func readyConditionOf(t *testing.T, ea *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	conds, _, err := unstructured.NestedSlice(ea.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("status.conditions malformed: %v", err)
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if condType, _ := m["type"].(string); condType == "Ready" {
			return m
		}
	}
	return nil
}

// A published ExternalArtifact must carry a Ready=True condition, not
// just status.artifact. Every Flux consumer — kustomize/helm-controller,
// JaaS's own internal/sources.readyState, RFC-0012 producer-aware
// resolvers — treats an artifact without Ready=True as not-yet-consumable,
// so a chained snippet's sourceRef would hang on ErrSourceNotReady forever.
func TestPublish_WritesReadyTrueCondition(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{Output: jaasv1.OutputRendered},
	}
	if _, err := p.Publish(context.Background(), c, snip, `{"ok":true}`, nil, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ready := readyConditionOf(t, fetchExternalArtifact(t, c, snip.Name, snip.Namespace))
	if ready == nil {
		t.Fatal("ExternalArtifact has no Ready condition; downstream consumers treat it as not-ready")
	}
	if got, _ := ready["status"].(string); got != "True" {
		t.Errorf("Ready condition status = %q, want True", got)
	}
	if got, _ := ready["reason"].(string); got == "" {
		t.Error("Ready condition has empty reason")
	}
	if got, _ := ready["lastTransitionTime"].(string); got == "" {
		t.Error("Ready condition has empty lastTransitionTime")
	}
}

// lastTransitionTime must not churn across a steady republish: while the
// status stays True, the timestamp from the first transition is retained
// so observers don't see a fresh transition (and the resourceVersion
// bump it implies) on every reconcile.
func TestPublish_ReadyConditionPreservesLastTransitionTime(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{Output: jaasv1.OutputRendered},
	}

	p.Clock = func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) }
	if _, err := p.Publish(context.Background(), c, snip, `{"v":1}`, nil, nil); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	first, _ := readyConditionOf(t, fetchExternalArtifact(t, c, snip.Name, snip.Namespace))["lastTransitionTime"].(string)

	// Second publish a day later, still Ready=True.
	p.Clock = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	if _, err := p.Publish(context.Background(), c, snip, `{"v":2}`, nil, nil); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	second, _ := readyConditionOf(t, fetchExternalArtifact(t, c, snip.Name, snip.Namespace))["lastTransitionTime"].(string)

	if first == "" || second == "" {
		t.Fatalf("empty lastTransitionTime: first=%q second=%q", first, second)
	}
	if first != second {
		t.Errorf("lastTransitionTime churned on steady republish: first=%q second=%q", first, second)
	}
}

func TestPublish_SourceMode_PacksAllSpecFiles(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			Output: jaasv1.OutputSource,
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{
					"main.jsonnet":     `{ a: 1 }`,
					"helper.libsonnet": `{ helper: true }`,
				},
			},
		},
	}
	_, err := p.Publish(context.Background(), c, snip, "ignored-in-source-mode", snip.Spec.Files, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ea := fetchExternalArtifact(t, c, snip.Name, snip.Namespace)
	path, _, _ := unstructured.NestedString(ea.Object, "status", "artifact", "path")
	if path == "" {
		t.Fatal("status.artifact.path missing")
	}
	members := readTarballAt(t, p.Store.(*storage.Store), path)
	if got := members["main.jsonnet"]; got != `{ a: 1 }` {
		t.Errorf("main.jsonnet = %q, want %q", got, `{ a: 1 }`)
	}
	if got := members["helper.libsonnet"]; got != `{ helper: true }` {
		t.Errorf("helper.libsonnet = %q, want %q", got, `{ helper: true }`)
	}
}

func TestPublish_SourceMode_RevisionIsDeterministic(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"},
		Spec: jaasv1.JsonnetSnippetSpec{
			Output: jaasv1.OutputSource,
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "x", "extra": "y"},
			},
		},
	}
	cA := newPublisherClient(t)
	a, err := newTestPublisher(t, cA).Publish(context.Background(), cA, snip, "", snip.Spec.Files, nil)
	if err != nil {
		t.Fatal(err)
	}
	cB := newPublisherClient(t)
	b, err := newTestPublisher(t, cB).Publish(context.Background(), cB, snip, "", snip.Spec.Files, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("source-mode revision varies between runs: %q vs %q", a, b)
	}
}

func TestPublish_SourceMode_PacksResolvedSourceFiles_NotSpecFiles(t *testing.T) {
	// A snippet using sourceRef has no inline spec.files — the source
	// content lives in the resolved-files map the reconciler passes in.
	// Pre-fix, source-mode published an empty tarball in this case
	// because buildEntries only read snip.Spec.Files. This test pins
	// the invariant: the sourceFiles arg is what gets packed.
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "from-source-ref", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			Output: jaasv1.OutputSource,
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "upstream"},
			},
		},
	}
	fetched := map[string]string{
		"main.jsonnet":     `{ source: "ref" }`,
		"helper.libsonnet": `{ from: "fetched-tarball" }`,
	}
	if _, err := p.Publish(context.Background(), c, snip, "ignored-in-source-mode", fetched, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	ea := fetchExternalArtifact(t, c, snip.Name, snip.Namespace)
	path, _, _ := unstructured.NestedString(ea.Object, "status", "artifact", "path")
	if path == "" {
		t.Fatal("status.artifact.path missing")
	}
	members := readTarballAt(t, p.Store.(*storage.Store), path)
	if len(members) != len(fetched) {
		t.Errorf("tarball contains %d entries, want %d (sourceRef files were silently dropped)", len(members), len(fetched))
	}
	for k, want := range fetched {
		if got := members[k]; got != want {
			t.Errorf("members[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestPublish_UnknownOutputModeErrors(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{Output: "novel"},
	}
	if _, err := p.Publish(context.Background(), c, snip, "", nil, nil); err == nil {
		t.Errorf("Publish accepted unknown output mode")
	}
}

func TestPublish_DefaultOutputUsesRendered(t *testing.T) {
	// Empty Output (zero value) must behave identically to OutputRendered
	// so a snippet that elides the field still publishes.
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{},
	}
	if _, err := p.Publish(context.Background(), c, snip, `{"ok":true}`, nil, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func TestPublish_UpdateExistingArtifact_OverwritesStatus(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{Output: jaasv1.OutputRendered},
	}
	if _, err := p.Publish(context.Background(), c, snip, `{"v":1}`, nil, nil); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	res2, err := p.Publish(context.Background(), c, snip, `{"v":2}`, nil, nil)
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	ea := fetchExternalArtifact(t, c, snip.Name, snip.Namespace)
	got, _, _ := unstructured.NestedString(ea.Object, "status", "artifact", "revision")
	if got != res2.Revision {
		t.Errorf("status.artifact.revision = %q, want %q from second publish", got, res2.Revision)
	}
}

func TestWithdraw_DeletesArtifactAndStorageTree(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{Output: jaasv1.OutputRendered},
	}
	if _, err := p.Publish(context.Background(), c, snip, `{}`, nil, nil); err != nil {
		t.Fatalf("seed Publish: %v", err)
	}

	if err := p.Withdraw(context.Background(), c, snip); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	ea := &unstructured.Unstructured{}
	ea.SetGroupVersionKind(externalArtifactGVK)
	err := c.Get(context.Background(), types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}, ea)
	if !apierrors.IsNotFound(err) {
		t.Errorf("ExternalArtifact still present after Withdraw: err=%v", err)
	}

	rootPath := p.Store.(*storage.Store).RootPath()
	dir := filepath.Join(rootPath, snip.Namespace, snip.Name)
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("storage dir still present after Withdraw: %v", statErr)
	}
}

func TestWithdraw_IsIdempotent(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: "team-a"},
	}
	if err := p.Withdraw(context.Background(), c, snip); err != nil {
		t.Errorf("Withdraw on never-published snippet = %v, want nil", err)
	}
}

// PruneStored is the suspend-path GC: a paused snippet still re-enters
// the reconciler, and PruneStored keeps grace-expired non-keep revisions
// from leaking without a fresh Put or ExternalArtifact upsert.
func TestPruneStored_RemovesNonKeepRevisions(t *testing.T) {
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	ctx := context.Background()
	// Revisions must be hex — Prune only touches files matching the
	// <hex>.tar.gz artifact shape (the sweep-allowlist hardening).
	for _, rev := range []string{"abc123", "def456"} {
		if _, err := p.Store.Put(ctx, "team-a", "demo", rev, []storage.FileEntry{{Path: "rendered.json", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	if err := p.PruneStored(ctx, "team-a", "demo", []string{"def456"}); err != nil {
		t.Fatalf("PruneStored: %v", err)
	}
	root := p.Store.(*storage.Store).RootPath()
	if _, err := os.Stat(filepath.Join(root, "team-a", "demo", "abc123.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("abc123 (not in keep-set) should have been pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "team-a", "demo", "def456.tar.gz")); err != nil {
		t.Errorf("def456 (keep-set) must survive: %v", err)
	}
}

func TestPublish_NilStoreOrClientReturnsError(t *testing.T) {
	t.Run("nil store", func(t *testing.T) {
		p := &Publisher{BaseURL: "x"}
		if _, err := p.Publish(context.Background(), newPublisherClient(t),
			&jaasv1.JsonnetSnippet{}, "", nil, nil); err == nil {
			t.Error("nil store accepted")
		}
	})
	t.Run("nil client", func(t *testing.T) {
		store, _ := storage.New(t.TempDir())
		defer store.Close()
		p := &Publisher{Store: store, BaseURL: "x"}
		if _, err := p.Publish(context.Background(), nil,
			&jaasv1.JsonnetSnippet{}, "", nil, nil); err == nil {
			t.Error("nil client accepted")
		}
	})
}

func TestWithdraw_NilStoreOrClientReturnsError(t *testing.T) {
	t.Run("nil store", func(t *testing.T) {
		p := &Publisher{}
		if err := p.Withdraw(context.Background(), newPublisherClient(t),
			&jaasv1.JsonnetSnippet{}); err == nil {
			t.Error("nil store accepted")
		}
	})
	t.Run("nil client", func(t *testing.T) {
		store, _ := storage.New(t.TempDir())
		defer store.Close()
		p := &Publisher{Store: store}
		if err := p.Withdraw(context.Background(), nil,
			&jaasv1.JsonnetSnippet{}); err == nil {
			t.Error("nil client accepted")
		}
	})
}

func TestPublish_GetExternalArtifactErrorPropagates(t *testing.T) {
	want := errors.New("apiserver oops")
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return want
			},
		}).Build()
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"}}
	if _, err := p.Publish(context.Background(), c, snip, "{}", nil, nil); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestPublish_CreateExternalArtifactErrorPropagates(t *testing.T) {
	want := errors.New("create failed")
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
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
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"}}
	if _, err := p.Publish(context.Background(), c, snip, "{}", nil, nil); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestPublish_StatusUpdateErrorPropagates(t *testing.T) {
	want := errors.New("status write failed")
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return want
			},
		}).Build()
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"}}
	if _, err := p.Publish(context.Background(), c, snip, "{}", nil, nil); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestPublisher_NowFallsBackToTimeNowWhenClockNil(t *testing.T) {
	p := &Publisher{}
	got := p.now()
	if got.IsZero() {
		t.Errorf("now() with nil Clock returned zero time")
	}
}

func TestPublish_UpdateExistingArtifactErrorPropagates(t *testing.T) {
	want := errors.New("update conflict")
	c := newPublisherClient(t)
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"}}
	if _, err := p.Publish(context.Background(), c, snip, "{}", nil, nil); err != nil {
		t.Fatalf("seed Publish: %v", err)
	}

	// Rewire the client to fail Update on the second go.
	c2 := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(func() client.Object {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(externalArtifactGVK)
			u.SetName(snip.Name)
			u.SetNamespace(snip.Namespace)
			return u
		}()).
		WithStatusSubresource(
			&jaasv1.JsonnetSnippet{},
			func() client.Object {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(externalArtifactGVK)
				return u
			}(),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
				return want
			},
		}).Build()
	p2 := newTestPublisher(t, c2)
	if _, err := p2.Publish(context.Background(), c2, snip, `{"v":2}`, nil, nil); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestWithdraw_DeleteErrorOtherThanNotFoundPropagates(t *testing.T) {
	want := errors.New("permission denied")
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return want
			},
		}).Build()
	p := newTestPublisher(t, c)
	snip := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"}}
	if err := p.Withdraw(context.Background(), c, snip); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

// mockStore lets tests inject Put / Prune / Delete failures into Publisher
// without standing up a real filesystem.
type mockStore struct {
	putResult storage.Result
	putErr    error
	pruneErr  error
	deleteErr error
}

func (m *mockStore) Put(_ context.Context, namespace, name, revision string, entries []storage.FileEntry) (storage.Result, error) {
	if m.putErr != nil {
		return storage.Result{}, m.putErr
	}
	if m.putResult.Path == "" {
		return storage.Result{
			Path:         namespace + "/" + name + "/" + revision + ".tar.gz",
			SizeBytes:    int64(len(entries)),
			DigestSHA256: "deadbeef",
		}, nil
	}
	return m.putResult, nil
}

func (m *mockStore) Prune(_ context.Context, namespace, name string, keepRevisions []string, _ time.Duration) error {
	return m.pruneErr
}

func (m *mockStore) Delete(_ context.Context, namespace, name string) error {
	return m.deleteErr
}

func (m *mockStore) HTTPHandler() http.Handler { return http.NotFoundHandler() }

func (m *mockStore) Close() error { return nil }

func (m *mockStore) Sweep(_ context.Context, _ time.Duration) (int, error) { return 0, nil }

func TestPublish_StorePutErrorPropagates(t *testing.T) {
	want := errors.New("disk full")
	c := newPublisherClient(t)
	p := &Publisher{Store: &mockStore{putErr: want}, BaseURL: "x"}
	if _, err := p.Publish(context.Background(), c, &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"},
	}, "{}", nil, nil); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestPublish_StorePruneErrorPropagates(t *testing.T) {
	want := errors.New("prune denied")
	c := newPublisherClient(t)
	p := &Publisher{Store: &mockStore{pruneErr: want}, BaseURL: "x"}
	if _, err := p.Publish(context.Background(), c, &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"},
	}, "{}", nil, nil); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestWithdraw_StoreDeleteErrorPropagates(t *testing.T) {
	want := errors.New("delete denied")
	c := newPublisherClient(t)
	p := &Publisher{Store: &mockStore{deleteErr: want}, BaseURL: "x"}
	err := p.Withdraw(context.Background(), c, &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "n"},
	})
	if !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

func TestPublish_MaxArtifactBytes_RejectsOversizedRender(t *testing.T) {
	c := newPublisherClient(t)
	store := &mockStore{}
	p := &Publisher{Store: store, BaseURL: "x", MaxArtifactBytes: 10}
	rendered := "abcdefghijklmnop" // 16 bytes, cap is 10
	_, err := p.Publish(context.Background(), c, &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: "n"},
	}, rendered, nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Errorf("got %v, want ErrArtifactTooLarge", err)
	}
}

func TestPublish_MaxArtifactBytes_ZeroDisablesCap(t *testing.T) {
	c := newPublisherClient(t)
	p := &Publisher{Store: &mockStore{}, BaseURL: "x", MaxArtifactBytes: 0}
	rendered := strings.Repeat("a", 10_000_000) // 10 MB
	if _, err := p.Publish(context.Background(), c, &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: "n"},
	}, rendered, nil, nil); err != nil {
		t.Errorf("Publish with cap=0 returned %v", err)
	}
}

func TestExternalArtifactGVK_Exported(t *testing.T) {
	gvk := ExternalArtifactGVK()
	if gvk.Group != "source.toolkit.fluxcd.io" {
		t.Errorf("Group = %q, want source.toolkit.fluxcd.io", gvk.Group)
	}
	if gvk.Kind != "ExternalArtifact" {
		t.Errorf("Kind = %q, want ExternalArtifact", gvk.Kind)
	}
}

// readTarballAt reads the tarball at storePath relative to store.RootPath()
// and returns its members as path → content.
func readTarballAt(t *testing.T, store *storage.Store, storePath string) map[string]string {
	t.Helper()
	abs := filepath.Join(store.RootPath(), storePath)
	f, err := os.Open(abs)
	if err != nil {
		t.Fatalf("open %s: %v", abs, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		buf := &bytes.Buffer{}
		if _, err := io.Copy(buf, tr); err != nil {
			t.Fatalf("copy: %v", err)
		}
		out[hdr.Name] = buf.String()
	}
}
