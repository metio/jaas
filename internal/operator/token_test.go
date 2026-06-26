/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fakeMinter is a configurable tokenMinter for token-cache unit tests.
type fakeMinter struct {
	mu      sync.Mutex
	token   string
	expires time.Time
	err     error
	calls   int
}

func (f *fakeMinter) Mint(_ context.Context, _, _ string, _ time.Duration) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return f.token, f.expires, nil
}

func (f *fakeMinter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestTokenCache_FirstCallMintsAndReturnsToken(t *testing.T) {
	fm := &fakeMinter{token: "t1", expires: time.Now().Add(1 * time.Hour)}
	c := newTokenCache(fm)
	tok, err := c.Token(context.Background(), "ns", "sa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "t1" {
		t.Errorf("got %q, want t1", tok)
	}
	if fm.callCount() != 1 {
		t.Errorf("mint calls = %d, want 1", fm.callCount())
	}
}

func TestTokenCache_SecondCallReusesCachedToken(t *testing.T) {
	fm := &fakeMinter{token: "t1", expires: time.Now().Add(1 * time.Hour)}
	c := newTokenCache(fm)
	_, _ = c.Token(context.Background(), "ns", "sa")
	_, _ = c.Token(context.Background(), "ns", "sa")
	if fm.callCount() != 1 {
		t.Errorf("mint calls = %d, want 1 (cache must reuse)", fm.callCount())
	}
}

func TestTokenCache_DistinctSAsHaveSeparateEntries(t *testing.T) {
	fm := &fakeMinter{token: "t", expires: time.Now().Add(1 * time.Hour)}
	c := newTokenCache(fm)
	_, _ = c.Token(context.Background(), "ns", "sa1")
	_, _ = c.Token(context.Background(), "ns", "sa2")
	_, _ = c.Token(context.Background(), "other-ns", "sa1")
	if fm.callCount() != 3 {
		t.Errorf("mint calls = %d, want 3 (one per (ns, sa) key)", fm.callCount())
	}
}

func TestTokenCache_RefreshesWhenTokenWithinMargin(t *testing.T) {
	// 1h token, 5m margin: once the clock advances to within the margin of
	// expiry the cache treats the token as near-expiry and re-mints. Drive the
	// clock deterministically rather than waiting real time.
	base := time.Now()
	clock := base
	fm := &fakeMinter{token: "t1", expires: base.Add(1 * time.Hour)}
	c := newTokenCache(fm)
	c.now = func() time.Time { return clock }
	_, _ = c.Token(context.Background(), "ns", "sa") // refreshAt = expires - 5m = base+55m

	fm.mu.Lock()
	fm.token = "t2"
	fm.expires = base.Add(2 * time.Hour)
	fm.mu.Unlock()

	clock = base.Add(56 * time.Minute) // 4m before expiry — inside the 5m margin
	tok, err := c.Token(context.Background(), "ns", "sa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "t2" {
		t.Errorf("got %q, want t2 (refresh should have minted again)", tok)
	}
	if fm.callCount() != 2 {
		t.Errorf("mint calls = %d, want 2", fm.callCount())
	}
}

// TestTokenCache_ShortLivedTokenStillCached pins the refresh-margin clamp: an
// apiserver capping the SA token TTL below the 5m margin must NOT make every
// reconcile re-mint (a TokenRequest storm). The token is reused until ~2/3
// through its life, then refreshed before expiry.
func TestTokenCache_ShortLivedTokenStillCached(t *testing.T) {
	base := time.Now()
	clock := base
	fm := &fakeMinter{token: "t1", expires: base.Add(120 * time.Second)} // 2m TTL
	c := newTokenCache(fm)
	c.now = func() time.Time { return clock }

	if tok, _ := c.Token(context.Background(), "ns", "sa"); tok != "t1" {
		t.Fatalf("first call got %q, want t1", tok)
	}
	// margin = min(5m, 120s/3) = 40s, so refreshAt = base+80s. At base+30s the
	// token must still be served from cache — the storm the clamp prevents.
	clock = base.Add(30 * time.Second)
	if tok, _ := c.Token(context.Background(), "ns", "sa"); tok != "t1" {
		t.Errorf("at +30s got %q, want cached t1", tok)
	}
	if n := fm.callCount(); n != 1 {
		t.Fatalf("mint calls at +30s = %d, want 1 (clamp must keep the token cached)", n)
	}
	// Past refreshAt the cache re-mints before expiry.
	fm.mu.Lock()
	fm.token = "t2"
	fm.expires = base.Add(4 * time.Minute)
	fm.mu.Unlock()
	clock = base.Add(85 * time.Second) // past base+80s refreshAt, still before 120s expiry
	if tok, _ := c.Token(context.Background(), "ns", "sa"); tok != "t2" {
		t.Errorf("at +85s got %q, want re-minted t2", tok)
	}
	if n := fm.callCount(); n != 2 {
		t.Errorf("mint calls at +85s = %d, want 2", n)
	}
}

func TestTokenCache_MintErrorPropagatesAndDoesNotCache(t *testing.T) {
	want := errors.New("token denied")
	fm := &fakeMinter{err: want}
	c := newTokenCache(fm)
	if _, err := c.Token(context.Background(), "ns", "sa"); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
	// A retry triggers a fresh Mint — the cache must not have stored the
	// failed result.
	fm.mu.Lock()
	fm.err = nil
	fm.token = "recovered"
	fm.expires = time.Now().Add(1 * time.Hour)
	fm.mu.Unlock()
	tok, err := c.Token(context.Background(), "ns", "sa")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if tok != "recovered" {
		t.Errorf("retry got %q, want recovered", tok)
	}
}

func TestTokenCache_ForgetReMintsOnNextCall(t *testing.T) {
	fm := &fakeMinter{token: "t1", expires: time.Now().Add(1 * time.Hour)}
	c := newTokenCache(fm)
	_, _ = c.Token(context.Background(), "ns", "sa")
	c.Forget("ns", "sa")
	_, _ = c.Token(context.Background(), "ns", "sa")
	if fm.callCount() != 2 {
		t.Errorf("mint calls = %d, want 2 (Forget should invalidate)", fm.callCount())
	}
}

func TestTokenCache_ForgetIsIdempotent(t *testing.T) {
	fm := &fakeMinter{}
	c := newTokenCache(fm)
	c.Forget("ns", "sa")
	c.Forget("ns", "sa") // must not panic on absent keys
	if fm.callCount() != 0 {
		t.Errorf("mint calls = %d, want 0", fm.callCount())
	}
}

func TestTokenCache_ForgetOnNilReceiverIsNoOp(t *testing.T) {
	var c *tokenCache
	c.Forget("ns", "sa") // must not panic
}

func TestTokenCache_ConcurrentTokenCallsForSameSA_SingleflightDedupesToOneMint(t *testing.T) {
	// Slow minter: holds the singleflight inside Mint long enough that
	// every other concurrent caller queues up behind it. Without
	// singleflight, all 50 racing callers would each see an empty cache
	// and call Mint; with singleflight, only the first reaches Mint and
	// the rest receive its result.
	gate := make(chan struct{})
	minter := minterFunc(func(_ context.Context, _, _ string, _ time.Duration) (string, time.Time, error) {
		<-gate // wait for the test to release us
		return "t", time.Now().Add(1 * time.Hour), nil
	})
	calls := &countingMinter{inner: minter}
	c := newTokenCache(calls)

	const n = 50
	start := make(chan struct{})
	done := make(chan string, n)
	for range n {
		go func() {
			<-start
			tok, err := c.Token(context.Background(), "ns", "sa")
			if err != nil {
				done <- "err:" + err.Error()
				return
			}
			done <- tok
		}()
	}
	close(start)

	// Give callers time to enter the singleflight before releasing Mint.
	time.Sleep(20 * time.Millisecond)
	close(gate)

	for i := range n {
		got := <-done
		if got != "t" {
			t.Errorf("caller %d got %q, want t", i, got)
		}
	}
	if c := calls.count(); c != 1 {
		t.Errorf("mint calls = %d, want exactly 1 (singleflight should dedupe)", c)
	}
}

func TestTokenCache_ConcurrentDistinctSAsMintConcurrently(t *testing.T) {
	// Different keys must NOT be deduplicated against each other — each
	// (ns, sa) gets its own Mint, even when calls overlap.
	gate := make(chan struct{})
	calls := &countingMinter{inner: minterFunc(func(_ context.Context, _, _ string, _ time.Duration) (string, time.Time, error) {
		<-gate
		return "t", time.Now().Add(1 * time.Hour), nil
	})}
	c := newTokenCache(calls)
	const n = 10
	done := make(chan struct{}, n)
	for i := range n {
		sa := "sa-" + string(rune('a'+i))
		go func() {
			_, _ = c.Token(context.Background(), "ns", sa)
			done <- struct{}{}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	for range n {
		<-done
	}
	if got := calls.count(); got != n {
		t.Errorf("mint calls = %d, want %d (one per distinct key)", got, n)
	}
}

// TestTokenCache_FirstCallerCancellationDoesNotPropagateToMinter pins
// the singleflight ctx-detachment invariant: when the first caller's
// ctx is already cancelled, the inner Mint must still see a live ctx
// — otherwise singleflight would propagate the cancellation to every
// waiter and a transient timeout on one reconcile would flip every
// concurrent reconcile sharing the same SA to Ready=False.
func TestTokenCache_FirstCallerCancellationDoesNotPropagateToMinter(t *testing.T) {
	var minterCtxErr error
	minter := minterFunc(func(ctx context.Context, _, _ string, _ time.Duration) (string, time.Time, error) {
		minterCtxErr = ctx.Err()
		return "t", time.Now().Add(1 * time.Hour), nil
	})
	c := newTokenCache(minter)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tok, err := c.Token(cancelled, "ns", "sa")
	if err != nil {
		t.Fatalf("Token returned %v even though minter succeeded", err)
	}
	if tok != "t" {
		t.Errorf("tok = %q, want t", tok)
	}
	if minterCtxErr != nil {
		t.Errorf("minter received ctx with Err()=%v; expected detached ctx with no error", minterCtxErr)
	}
}

// countingMinter wraps an inner tokenMinter and counts how many times Mint
// is reached. Concurrency-safe via the embedded mutex.
type countingMinter struct {
	inner tokenMinter
	mu    sync.Mutex
	calls int
}

func (m *countingMinter) Mint(ctx context.Context, ns, sa string, ttl time.Duration) (string, time.Time, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return m.inner.Mint(ctx, ns, sa, ttl)
}

func (m *countingMinter) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func TestTokenCache_CustomTTLPassedToMinter(t *testing.T) {
	var seenTTL time.Duration
	minter := minterFunc(func(_ context.Context, _, _ string, ttl time.Duration) (string, time.Time, error) {
		seenTTL = ttl
		return "x", time.Now().Add(1 * time.Hour), nil
	})
	c := newTokenCache(minter)
	c.ttl = 30 * time.Minute
	_, _ = c.Token(context.Background(), "ns", "sa")
	if seenTTL != 30*time.Minute {
		t.Errorf("minter saw TTL %v, want 30m", seenTTL)
	}
}

func TestClientsetTokenMinter_NilClientsetReturnsError(t *testing.T) {
	m := clientsetTokenMinter{kc: nil}
	if _, _, err := m.Mint(context.Background(), "ns", "sa", time.Hour); err == nil {
		t.Errorf("nil clientset accepted")
	}
}

func TestClientsetTokenMinter_HappyPath_ReturnsTokenFromCreateToken(t *testing.T) {
	wantExpiry := metav1.NewTime(time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC))
	kc := fake.NewSimpleClientset()
	// fake.Clientset doesn't implement subresource Create by default; we
	// install a Reactor that satisfies CreateToken with a synthesised
	// TokenRequest reply.
	kc.PrependReactor("create", "serviceaccounts/token",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authnv1.TokenRequest{
				Status: authnv1.TokenRequestStatus{
					Token:               "synthetic-jwt",
					ExpirationTimestamp: wantExpiry,
				},
			}, nil
		})
	m := clientsetTokenMinter{kc: kc}
	tok, expires, err := m.Mint(context.Background(), "ns", "sa", 30*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "synthetic-jwt" {
		t.Errorf("token = %q, want synthetic-jwt", tok)
	}
	if !expires.Equal(wantExpiry.Time) {
		t.Errorf("expires = %v, want %v", expires, wantExpiry.Time)
	}
}

func TestClientsetTokenMinter_CreateTokenErrorPropagates(t *testing.T) {
	want := errors.New("apiserver said no")
	kc := fake.NewSimpleClientset()
	kc.PrependReactor("create", "serviceaccounts/token",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, want
		})
	m := clientsetTokenMinter{kc: kc}
	if _, _, err := m.Mint(context.Background(), "ns", "sa", time.Hour); !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

// minterFunc adapts a closure to the tokenMinter interface so tests can
// inject inline implementations.
type minterFunc func(context.Context, string, string, time.Duration) (string, time.Time, error)

func (f minterFunc) Mint(ctx context.Context, ns, sa string, ttl time.Duration) (string, time.Time, error) {
	return f(ctx, ns, sa, ttl)
}

// A Forget landing while a Mint is in flight must drop the cache write so
// the evicted token isn't resurrected. The caller still gets the valid
// freshly-minted token; the next lookup misses and re-mints.
func TestTokenCache_ForgetDuringMintDropsCacheWrite(t *testing.T) {
	var c *tokenCache
	c = newTokenCache(minterFunc(func(_ context.Context, ns, sa string, _ time.Duration) (string, time.Time, error) {
		c.Forget(ns, sa) // delete lands mid-mint, bumping the epoch
		return "tok", time.Now().Add(time.Hour), nil
	}))
	tok, err := c.Token(context.Background(), "team-a", "tenant")
	if err != nil || tok != "tok" {
		t.Fatalf("Token = (%q, %v), want (\"tok\", nil)", tok, err)
	}
	if _, ok := c.lookup("team-a/tenant"); ok {
		t.Error("token cached despite a Forget racing the mint; want dropped")
	}
}
