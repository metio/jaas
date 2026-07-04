// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package operator

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

// seedStoredRevision writes one revision into the publisher's store and
// returns it, so a test can assert deletion via Backend.Open.
func seedStoredRevision(t *testing.T, p *Publisher, ns, name string) string {
	t.Helper()
	const revision = "sha256:feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	if _, err := p.Store.Put(context.Background(), ns, name, revision, []storage.FileEntry{
		{Path: "rendered.json", Content: []byte(`{"a":1}`)},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return revision
}

func assertRevisionGone(t *testing.T, p *Publisher, ns, name, revision string) {
	t.Helper()
	rc, err := p.Store.Open(context.Background(), ns, name, revision)
	if err == nil {
		_ = rc.Close()
		t.Fatal("stored tarball still present — the deletion path orphaned it")
	}
	if !errors.Is(err, storage.ErrRevisionNotFound) {
		t.Fatalf("Open after withdraw = %v, want ErrRevisionNotFound", err)
	}
}

// WithdrawStorage deletes the stored tarballs without any client — the half of
// Withdraw the deletion path can always perform.
func TestPublisher_WithdrawStorage(t *testing.T) {
	p := newTestPublisher(t, nil)
	snip := sampleSnippet()
	rev := seedStoredRevision(t, p, snip.Namespace, snip.Name)

	if err := p.WithdrawStorage(context.Background(), snip); err != nil {
		t.Fatalf("WithdrawStorage: %v", err)
	}
	assertRevisionGone(t, p, snip.Namespace, snip.Name, rev)

	// Nil store is refused, mirroring Withdraw.
	empty := &Publisher{}
	if err := empty.WithdrawStorage(context.Background(), snip); err == nil {
		t.Fatal("WithdrawStorage with no store must error")
	}
}

// When the tenant client cannot be built at all (the namespace is terminating
// so TokenRequest is refused, the SA is gone, RBAC revoked) the force-drop
// must still delete the stored tarballs — they need no tenant credentials —
// and say so instead of warning about orphans. A routine `kubectl delete
// namespace` must not leak storage.
func TestReconcileDelete_TenantClientForbidden_SalvagesStorage(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	deletedAt := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &deletedAt

	forbiddenErr := apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "serviceaccounts/token"},
		"tenant",
		errors.New("unable to create new content in namespace because it is being terminated"),
	)
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	rev := seedStoredRevision(t, r.Publisher, snip.Namespace, snip.Name)
	r.RestConfig = &rest.Config{Host: "http://example.test"}
	r.TokenCache = newTokenCache(&stubMinter{err: forbiddenErr})
	r.Clock = func() time.Time { return deletedAt.Time.Add(1 * time.Second) }
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Finalizer gone…
	got := &jaasv1.JsonnetSnippet{}
	if err := c.Get(context.Background(), key, got); err == nil && len(got.Finalizers) != 0 {
		t.Errorf("finalizer not dropped: %v", got.Finalizers)
	}
	// …and, crucially, the tarball too.
	assertRevisionGone(t, r.Publisher, snip.Namespace, snip.Name, rev)

	// The event reports the storage as cleaned, not orphaned.
	var withdrawForced string
	for _, ev := range drainEvents(rec) {
		if strings.Contains(ev, "WithdrawForced") {
			withdrawForced = ev
		}
	}
	if withdrawForced == "" {
		t.Fatal("expected a WithdrawForced event")
	}
	if !strings.Contains(withdrawForced, "stored tarballs were deleted") {
		t.Errorf("event should report the salvaged storage, got: %q", withdrawForced)
	}
	if strings.Contains(withdrawForced, "orphaned tarballs") {
		t.Errorf("event must not warn about orphans when storage was cleaned: %q", withdrawForced)
	}
}

// The same salvage applies when the tenant client builds but the
// ExternalArtifact delete itself is permanently refused: Withdraw returns
// before its own Store.Delete, so the salvage must run it.
func TestReconcileDelete_EADeleteForbidden_SalvagesStorage(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	deletedAt := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &deletedAt

	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == externalArtifactGVK {
					return apierrors.NewForbidden(
						schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "externalartifacts"},
						snip.Name, errors.New("RBAC revoked"),
					)
				}
				return nil
			},
		}).
		Build()
	r := newReconciler(t, c)
	r.Publisher = newTestPublisher(t, c)
	rev := seedStoredRevision(t, r.Publisher, snip.Namespace, snip.Name)
	r.Clock = func() time.Time { return deletedAt.Time.Add(1 * time.Second) }
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertRevisionGone(t, r.Publisher, snip.Namespace, snip.Name, rev)
}

// failingDeleteBackend wraps a real backend but refuses Delete — the
// storage-perma-down case where the orphan warning must survive.
type failingDeleteBackend struct {
	storage.Backend
}

func (f *failingDeleteBackend) Delete(context.Context, string, string) error {
	return errors.New("bucket gone")
}

func (f *failingDeleteBackend) HTTPHandler() http.Handler { return nil }

func (f *failingDeleteBackend) Open(ctx context.Context, ns, name, rev string) (io.ReadCloser, error) {
	return f.Backend.Open(ctx, ns, name, rev)
}

// When the backend itself refuses the delete, the trade-off is unchanged: the
// finalizer force-drops and the event keeps the orphan warning, now carrying
// both errors.
func TestReconcileDelete_StorageAlsoBroken_KeepsOrphanWarning(t *testing.T) {
	snip := sampleSnippet()
	snip.Finalizers = []string{FinalizerName}
	deletedAt := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	snip.DeletionTimestamp = &deletedAt

	forbiddenErr := apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "serviceaccounts/token"},
		"tenant", errors.New("namespace terminating"),
	)
	c := fake.NewClientBuilder().
		WithScheme(publisherScheme(t)).
		WithObjects(snip).
		WithStatusSubresource(&jaasv1.JsonnetSnippet{}).
		Build()
	r := newReconciler(t, c)
	p := newTestPublisher(t, c)
	p.Store = &failingDeleteBackend{Backend: p.Store}
	r.Publisher = p
	r.RestConfig = &rest.Config{Host: "http://example.test"}
	r.TokenCache = newTokenCache(&stubMinter{err: forbiddenErr})
	r.Clock = func() time.Time { return deletedAt.Time.Add(1 * time.Second) }
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &jaasv1.JsonnetSnippet{}
	if err := c.Get(context.Background(), key, got); err == nil && len(got.Finalizers) != 0 {
		t.Errorf("finalizer must still force-drop when storage is broken: %v", got.Finalizers)
	}
	var withdrawForced string
	for _, ev := range drainEvents(rec) {
		if strings.Contains(ev, "WithdrawForced") {
			withdrawForced = ev
		}
	}
	if withdrawForced == "" {
		t.Fatal("expected a WithdrawForced event")
	}
	if !strings.Contains(withdrawForced, "orphaned tarballs") {
		t.Errorf("a genuinely-broken backend must keep the orphan warning: %q", withdrawForced)
	}
	if !strings.Contains(withdrawForced, "bucket gone") {
		t.Errorf("the storage error should be surfaced alongside the withdraw error: %q", withdrawForced)
	}
}
