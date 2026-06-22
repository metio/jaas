/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// --- PruneStored ------------------------------------------------------------

// A Publisher with no Store can't prune; PruneStored must report the
// misconfiguration instead of nil-dereferencing the backend.
func TestPruneStored_NilStoreErrors(t *testing.T) {
	p := &Publisher{}
	if err := p.PruneStored(context.Background(), "team-a", "demo", nil); err == nil {
		t.Fatal("expected error when Store is nil")
	}
}

// A backend failure surfaces wrapped through PruneStored so callers see
// the cause rather than a bare nil.
func TestPruneStored_PruneErrorPropagates(t *testing.T) {
	want := errors.New("prune denied")
	p := &Publisher{Store: &mockStore{pruneErr: want}}
	err := p.PruneStored(context.Background(), "team-a", "demo", []string{"abc123"})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want wrapped %v", err, want)
	}
}

// --- rbacDenialMessage ------------------------------------------------------

// Each permanent-API-error shape routes to a distinct human-facing
// message; the default arm passes anything else through verbatim.
func TestRbacDenialMessage_PerErrorShape(t *testing.T) {
	gr := schema.GroupResource{Group: "jaas.metio.wtf", Resource: "jsonnetsnippets"}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "forbidden",
			err:  apierrors.NewForbidden(gr, "demo", errors.New("no verb")),
			want: "RBAC denied reading the source CR",
		},
		{
			name: "no-match",
			err:  &apimeta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Resource: "buckets"}},
			want: "refers to a kind not registered",
		},
		{
			name: "invalid",
			err:  apierrors.NewInvalid(schema.GroupKind{Kind: "JsonnetSnippet"}, "demo", nil),
			want: "apiserver rejected reading the source CR",
		},
		{
			name: "bad-request",
			err:  apierrors.NewBadRequest("malformed"),
			want: "apiserver rejected reading the source CR",
		},
		{
			name: "method-not-supported",
			err:  apierrors.NewMethodNotSupported(gr, "patch"),
			want: "apiserver does not support the requested verb",
		},
		{
			name: "default-passthrough",
			err:  errors.New("transient blip"),
			want: "reading the source CR: transient blip",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rbacDenialMessage("reading the source CR", tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("rbacDenialMessage = %q, want substring %q", got, tc.want)
			}
		})
	}
}

// --- forgetPerSnippetCaches -------------------------------------------------

// With no service account resolvable the per-SA caches are skipped; the
// per-snippet caches (limiter, cycle, metrics) are still cleared.
func TestForgetPerSnippetCaches_NoServiceAccountEarlyExit(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := newReconciler(t, c)
	r.Limiter = NewRateLimiter(1, 1)
	r.CycleCache = newCycleCache()
	// TokenCache left nil and no SA on the snippet, so the per-SA branch
	// returns early.
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a", UID: types.UID("u1")},
	}
	r.forgetPerSnippetCaches(context.Background(), discardLogger(), snip)
}

// When this is the only snippet using the SA, the SA-scoped token and
// client caches are forgotten alongside the per-snippet ones.
func TestForgetPerSnippetCaches_LastSnippetForgetsSACaches(t *testing.T) {
	snip := sampleSnippet() // ServiceAccountName "tenant"
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	r.Limiter = NewRateLimiter(1, 1)
	r.CycleCache = newCycleCache()
	r.TokenCache = newTokenCache(&stubMinter{token: "tok", expires: time.Now().Add(time.Hour)})
	r.ClientCache = newTenantClientCache()
	r.forgetPerSnippetCaches(context.Background(), discardLogger(), snip)
}

// A List failure while checking "am I the last snippet on this SA" is
// non-fatal: the per-SA forget is skipped, the function returns cleanly.
func TestForgetPerSnippetCaches_ListErrorSkipsSAForget(t *testing.T) {
	snip := sampleSnippet()
	listErr := errors.New("apiserver down")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return listErr
			},
		}).
		Build()
	r := newReconciler(t, c)
	r.Limiter = NewRateLimiter(1, 1)
	r.CycleCache = newCycleCache()
	r.TokenCache = newTokenCache(&stubMinter{token: "tok", expires: time.Now().Add(time.Hour)})
	r.ClientCache = newTenantClientCache()
	r.forgetPerSnippetCaches(context.Background(), discardLogger(), snip)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tracedResolveLibraries -------------------------------------------------

// A referenced-but-missing library yields a non-empty reason; the trace
// span records the reason attribute without raising an error.
func TestTracedResolveLibraries_ReasonOnMissingLibrary(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	r := newReconciler(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "absent"}},
		},
	}
	_, reason, _, err := r.tracedResolveLibraries(context.Background(), c, snip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reason != ReasonLibraryNotFound {
		t.Errorf("reason = %q, want %q", reason, ReasonLibraryNotFound)
	}
}

// A non-NotFound, non-permanent Get failure propagates as an error; the
// span records it via the error branch.
func TestTracedResolveLibraries_ErrorOnGetFailure(t *testing.T) {
	getErr := errors.New("etcd timeout")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if _, ok := obj.(*jaasv1.JsonnetLibrary); ok {
					return getErr
				}
				return nil
			},
		}).
		Build()
	r := newReconciler(t, c)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "lib"}},
		},
	}
	_, _, _, err := r.tracedResolveLibraries(context.Background(), c, snip)
	if !errors.Is(err, getErr) {
		t.Fatalf("got %v, want wrapped %v", err, getErr)
	}
}

// --- EngageFluxWatch error guards -------------------------------------------

// EngageFluxWatch refuses to run before SetupWithManager has wired the
// controller reference.
func TestEngageFluxWatch_NilControllerErrors(t *testing.T) {
	r := &SnippetReconciler{}
	err := r.EngageFluxWatch(context.Background(), fluxSourceGVK("GitRepository"))
	if err == nil {
		t.Fatal("expected error with nil controller")
	}
}

// --- index ID extractors ----------------------------------------------------

// snippetLibraryRefIDs encodes one key per valid library ref, defaulting
// an empty namespace to the snippet's own and skipping incomplete refs.
func TestSnippetLibraryRefIDs(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "same-ns"},
				{Kind: "JsonnetLibrary", Name: "explicit", Namespace: "other"},
				{Kind: "JsonnetLibrary", Name: ""}, // skipped: no name
				{Kind: "", Name: "no-kind"},        // skipped: no kind
			},
		},
	}
	got := snippetLibraryRefIDs(snip)
	want := []string{
		libID("JsonnetLibrary", "team-a", "same-ns"),
		libID("JsonnetLibrary", "other", "explicit"),
	}
	assertStrings(t, got, want)

	if snippetLibraryRefIDs(&jaasv1.JsonnetLibrary{}) != nil {
		t.Error("non-snippet object must return nil")
	}
}

// snippetSourceRefIDs returns the source-ref key (or nil for no ref),
// defaulting the namespace to the snippet's own.
func TestSnippetSourceRefIDs(t *testing.T) {
	withRef := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "repo"},
			},
		},
	}
	assertStrings(t, snippetSourceRefIDs(withRef),
		[]string{sourceRefIndexKey("GitRepository", "team-a", "repo")})

	noRef := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"}}
	if snippetSourceRefIDs(noRef) != nil {
		t.Error("snippet without sourceRef must return nil")
	}
	if snippetSourceRefIDs(&jaasv1.JsonnetLibrary{}) != nil {
		t.Error("non-snippet object must return nil")
	}
}

// librarySourceRefIDs mirrors the snippet extractor for JsonnetLibrary.
func TestLibrarySourceRefIDs(t *testing.T) {
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-b"},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "OCIRepository", Name: "oci"},
			},
		},
	}
	assertStrings(t, librarySourceRefIDs(lib),
		[]string{sourceRefIndexKey("OCIRepository", "team-b", "oci")})

	if librarySourceRefIDs(&jaasv1.JsonnetSnippet{}) != nil {
		t.Error("non-library object must return nil")
	}
}

// recordingIndexer captures every IndexField registration so the test
// can assert registerWatchIndexes installs the full set. failAt makes the
// Nth (1-based) registration fail, exercising each early-return arm.
type recordingIndexer struct {
	fields  []string
	failAt  int
	failErr error
}

func (r *recordingIndexer) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	r.fields = append(r.fields, field)
	if r.failErr != nil && len(r.fields) == r.failAt {
		return r.failErr
	}
	return nil
}

// registerWatchIndexes installs all three field indexers when the indexer
// accepts them.
func TestRegisterWatchIndexes_RegistersAll(t *testing.T) {
	ri := &recordingIndexer{}
	if err := registerWatchIndexes(context.Background(), ri); err != nil {
		t.Fatalf("registerWatchIndexes: %v", err)
	}
	if len(ri.fields) != 3 {
		t.Fatalf("registered %d indexes, want 3: %v", len(ri.fields), ri.fields)
	}
}

// Each registration's error short-circuits registerWatchIndexes, so a
// failure at any of the three positions surfaces and stops the rest.
func TestRegisterWatchIndexes_PropagatesErrorAtEachPosition(t *testing.T) {
	for pos := 1; pos <= 3; pos++ {
		want := errors.New("indexer down")
		ri := &recordingIndexer{failAt: pos, failErr: want}
		err := registerWatchIndexes(context.Background(), ri)
		if !errors.Is(err, want) {
			t.Errorf("failAt %d: got %v, want %v", pos, err, want)
		}
		if len(ri.fields) != pos {
			t.Errorf("failAt %d: registered %d before failing, want %d", pos, len(ri.fields), pos)
		}
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
