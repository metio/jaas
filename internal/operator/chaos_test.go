/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/sources"
)

// flakyFetcher returns transient errors for the first failures-1 calls
// then succeeds on the failures-th. Models a source-controller that's
// briefly 5xx during a rolling restart.
type flakyFetcher struct {
	mu       sync.Mutex
	calls    int
	failures int
	result   *sources.Result
}

func (f *flakyFetcher) Fetch(_ context.Context, _ client.Client, _ *jaasv1.SourceRef, _ string) (*sources.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls < f.failures {
		return nil, errors.New("flaky: transient 503 from upstream")
	}
	return f.result, nil
}

// TestChaos_FlakyFetcher_EventuallyRecovers proves the reconciler
// surfaces transient upstream failures as retryable errors and flips
// to Synced once the source stabilizes — no manual intervention
// required.
func TestChaos_FlakyFetcher_EventuallyRecovers(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "chaos-fetcher", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "GitRepository",
					Name: "fake-source",
				},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	fetcher := &flakyFetcher{
		failures: 3,
		result: &sources.Result{
			Files: map[string]string{"main.jsonnet": `{ recovered: true }`},
		},
	}
	r := directReconciler(t, c, false)
	r.Fetcher = fetcher

	// Round 1: finalizer attach (no fetch yet).
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Rounds 2 and 3: fetcher returns 503. The reconciler classifies
	// the failure as ReasonSourceFetchFailed (Ready=False) rather than
	// crashing; we drive directly to that state.
	driveToReady(t, r, c, key, metav1.ConditionFalse, ReasonSourceFetchFailed, 3)

	// Round 4: fetcher succeeds. Reconciler flips Ready back to True.
	driveToReady(t, r, c, key, metav1.ConditionTrue, ReasonSynced, 3)
	if fetcher.calls < 3 {
		t.Errorf("expected at least 3 fetcher calls, saw %d", fetcher.calls)
	}
}

// TestChaos_TokenMintFailure_BubblesAsRetryable proves a TokenRequest
// failure (RBAC removed, apiserver flake, SA deleted) surfaces as a
// retryable reconcile error rather than crashing or silently re-trying
// without impersonation.
func TestChaos_TokenMintFailure_BubblesAsRetryable(t *testing.T) {
	cfg := envtestConfig(t)
	c := rbacEnvtestClient(t)

	ns := freshNamespace(t, c)
	// Deliberately do NOT seed the SA — TokenRequest will fail with
	// "serviceaccount X/Y not found".
	const missingSA = "ghost-tenant"

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "chaos-token", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: missingSA,
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true }`},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "would-need-tenant-rbac"},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := &SnippetReconciler{
		Client:     c,
		Scheme:     rbacScheme(t),
		RestConfig: cfg,
		TokenCache: realTokenCache(t, cfg),
		Logger:     discardLoggerEnvtest(),
	}

	// Finalizer attach (no impersonation).
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}

	// Spec reconcile must surface the token-mint failure as an error,
	// not a Ready=True false positive.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err == nil {
		t.Fatal("expected token-mint failure to surface as reconcile error")
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "token") && !strings.Contains(low, "serviceaccount") && !strings.Contains(low, "not found") {
		t.Errorf("error %q does not mention token/SA failure", err)
	}
}
