/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	jaasv1 "github.com/metio/jaas/api/v1"
)

func TestTenantClientCache_NilReceiverIsSafe(t *testing.T) {
	var c *tenantClientCache
	if got, _, ok := c.Get("k", "tok"); ok || got != nil {
		t.Errorf("nil.Get = (%v, %v), want (nil, false)", got, ok)
	}
	c.Put("k", "tok", 0, nil) // must not panic
	c.Forget("k")             // must not panic
}

// A Forget landing between a Get-miss and the rebuild's Put must drop the
// Put: the epoch the Get handed out is now stale, so a later Get must miss
// rather than return a resurrected client the deletion path evicted.
func TestTenantClientCache_PutDroppedWhenForgetRacesRebuild(t *testing.T) {
	cache := newTenantClientCache()
	_, epoch, ok := cache.Get("team-a/tenant", "tok-1")
	if ok {
		t.Fatal("expected initial miss")
	}
	// A Forget lands while the (simulated) rebuild is in flight.
	cache.Forget("team-a/tenant")
	// The rebuild completes and tries to store under the now-stale epoch.
	cache.Put("team-a/tenant", "tok-1", epoch, nil)
	if _, _, ok := cache.Get("team-a/tenant", "tok-1"); ok {
		t.Error("Put under a stale epoch resurrected the entry; want it dropped")
	}
}

func TestTenantClientCache_HitReturnsSameClient(t *testing.T) {
	cache := newTenantClientCache()
	stub := &stubMinter{token: "tok-1", expires: time.Now().Add(1 * time.Hour)}
	r := &SnippetReconciler{
		Client:      fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:      testScheme(t),
		RestConfig:  &rest.Config{Host: "http://example.test"},
		TokenCache:  newTokenCache(stub),
		ClientCache: cache,
	}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}
	first, err := r.tenantClient(context.Background(), snip)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := r.tenantClient(context.Background(), snip)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first != second {
		t.Errorf("expected the second call to return the cached client; got a different pointer")
	}
}

func TestTenantClientCache_TokenChangeReplacesEntry(t *testing.T) {
	// Drive the minter to return a different token on the second call so
	// the cache must rebuild the client. Tokens are normally rotated by
	// the apiserver every ~hour; this test pins the cache's behavior
	// when that rotation happens.
	cache := newTenantClientCache()
	stub := &swappableMinter{tokens: []string{"tok-1", "tok-2"}, exp: time.Now().Add(1 * time.Hour)}
	// Force the underlying tokenCache to miss on the second call by
	// driving the cache's `now` past the refresh margin between calls.
	tokens := newTokenCache(stub)
	clock := time.Now()
	tokens.now = func() time.Time { return clock }
	r := &SnippetReconciler{
		Client:      fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:      testScheme(t),
		RestConfig:  &rest.Config{Host: "http://example.test"},
		TokenCache:  tokens,
		ClientCache: cache,
	}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}
	first, err := r.tenantClient(context.Background(), snip)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Advance into the refresh margin so the next Token() re-mints.
	clock = stub.exp.Add(-1 * time.Minute)
	second, err := r.tenantClient(context.Background(), snip)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first == second {
		t.Errorf("expected token rotation to rebuild the client; got the same pointer")
	}
	if stub.calls != 2 {
		t.Fatalf("minter calls = %d, want 2 (initial + rotation)", stub.calls)
	}
}

func TestTenantClientCache_ForgetEvicts(t *testing.T) {
	cache := newTenantClientCache()
	stub := &stubMinter{token: "tok-1", expires: time.Now().Add(1 * time.Hour)}
	r := &SnippetReconciler{
		Client:      fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:      testScheme(t),
		RestConfig:  &rest.Config{Host: "http://example.test"},
		TokenCache:  newTokenCache(stub),
		ClientCache: cache,
	}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}
	first, err := r.tenantClient(context.Background(), snip)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	cache.Forget("team-a/tenant")
	second, err := r.tenantClient(context.Background(), snip)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first == second {
		t.Errorf("Forget should have evicted the cached client; got the same pointer back")
	}
}

func TestTenantClientCache_NilCacheStillFunctions(t *testing.T) {
	// ClientCache nil must not break tenantClient — it just skips the
	// memoization. Every call constructs a fresh client; verifying the
	// no-panic + non-nil-client invariant is enough.
	stub := &stubMinter{token: "tok-1", expires: time.Now().Add(1 * time.Hour)}
	r := &SnippetReconciler{
		Client:     fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Scheme:     testScheme(t),
		RestConfig: &rest.Config{Host: "http://example.test"},
		TokenCache: newTokenCache(stub),
		// ClientCache deliberately nil.
	}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}
	got, err := r.tenantClient(context.Background(), snip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("tenantClient with ClientCache=nil returned a nil client")
	}
}

// swappableMinter returns each entry in tokens in turn, then the last
// indefinitely. Lets the rotation test observe a fresh token on the second
// Mint without time-travelling the apiserver.
type swappableMinter struct {
	tokens []string
	exp    time.Time
	calls  int
}

func (s *swappableMinter) Mint(_ context.Context, _, _ string, _ time.Duration) (string, time.Time, error) {
	tok := s.tokens[len(s.tokens)-1]
	if s.calls < len(s.tokens) {
		tok = s.tokens[s.calls]
	}
	s.calls++
	return tok, s.exp, nil
}
