/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Default TokenRequest TTL and refresh margin. The reconciler mints tokens
// per (namespace, ServiceAccount) and reuses them while at least
// refreshMargin remains on the clock — keeping per-reconcile traffic to the
// TokenRequest API low without leaving stale tokens in the cache.
const (
	defaultTokenTTL    = 1 * time.Hour
	defaultTokenMargin = 5 * time.Minute
)

// tokenMinter is the seam over Kubernetes' ServiceAccounts.CreateToken API.
// The cache calls Mint when its entry is missing or near-expiry. Tests
// substitute a fake minter so they can drive expiry and failure paths
// without an apiserver.
type tokenMinter interface {
	Mint(ctx context.Context, namespace, serviceAccount string, ttl time.Duration) (token string, expires time.Time, err error)
}

// clientsetTokenMinter is the production implementation, backed by the
// typed Kubernetes clientset's CreateToken subresource call.
type clientsetTokenMinter struct {
	kc kubernetes.Interface
}

// Mint posts a TokenRequest for namespace/serviceAccount and returns the
// signed bearer token + its expiry as reported by the apiserver. The
// apiserver may shorten the requested TTL (e.g., to align with the
// service-account-token controller's bounds); we trust whatever it returns.
func (m clientsetTokenMinter) Mint(ctx context.Context, namespace, serviceAccount string, ttl time.Duration) (string, time.Time, error) {
	if m.kc == nil {
		return "", time.Time{}, errors.New("tokenMinter: nil Kubernetes clientset")
	}
	out, err := m.kc.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccount,
		&authnv1.TokenRequest{
			Spec: authnv1.TokenRequestSpec{ExpirationSeconds: new(int64(ttl.Seconds()))},
		}, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, err
	}
	return out.Status.Token, out.Status.ExpirationTimestamp.Time, nil
}

// cachedToken is one entry in tokenCache. token is the JWT the apiserver
// signed; expires is the absolute expiration timestamp reported with it;
// refreshAt is the instant at/after which lookup treats the entry as stale —
// expires minus the effective refresh margin (see refreshMarginFor).
type cachedToken struct {
	token     string
	expires   time.Time
	refreshAt time.Time
}

// tokenCache is the per-(namespace, ServiceAccount) cache of minted tokens.
// Concurrent Token calls for the same key are deduplicated through
// singleflight: only one goroutine reaches the underlying minter at a time
// per key; everyone else gets the cached value once it's been minted.
// Lifetime bounds: tokens are evicted automatically when refreshMargin
// remains.
type tokenCache struct {
	minter        tokenMinter
	ttl           time.Duration
	refreshMargin time.Duration
	now           func() time.Time // injectable for tests

	mu     sync.Mutex
	tokens map[string]cachedToken

	// epochs increments per key on every Forget. A Mint captures the
	// key's epoch before calling the apiserver and drops its cache write
	// if the epoch moved in the meantime — so a Forget landing between a
	// concurrent Mint completing and its write can't be silently
	// resurrected. Without this the cache is only correct while reconciles
	// serialize (MaxConcurrentReconciles == 1); the guard makes it correct
	// at any concurrency, mirroring cycleCache.
	epochs map[string]int64

	// flight dedupes concurrent Mint calls for the same key. The first
	// caller wins; subsequent callers wait and observe the cached result.
	flight singleflight.Group
}

// newTokenCache wraps minter with defaults (1h TTL, 5min refresh margin)
// suitable for production. Tests override the fields directly.
func newTokenCache(minter tokenMinter) *tokenCache {
	return &tokenCache{
		minter:        minter,
		ttl:           defaultTokenTTL,
		refreshMargin: defaultTokenMargin,
		now:           time.Now,
		tokens:        map[string]cachedToken{},
		epochs:        map[string]int64{},
	}
}

// Token returns a valid bearer token for namespace/serviceAccount, minting
// (or re-minting) one through the underlying minter when the cache is empty
// or the cached token is within refreshMargin of expiry. Concurrent callers
// share a single in-flight Mint via singleflight.
func (c *tokenCache) Token(ctx context.Context, namespace, serviceAccount string) (string, error) {
	key := namespace + "/" + serviceAccount

	if tok, ok := c.lookup(key); ok {
		return tok, nil
	}

	res, err, _ := c.flight.Do(key, func() (any, error) {
		// A double-check inside the singleflight closes the window where
		// a previous Do completed and populated the cache while this
		// caller was waiting for the lock.
		if tok, ok := c.lookup(key); ok {
			return tok, nil
		}
		// Capture the key's epoch before the mint. A Forget landing
		// while CreateToken is in flight bumps it, so the post-mint
		// write below drops the entry rather than resurrecting a token
		// the deletion path intended to evict.
		c.mu.Lock()
		epochAtMint := c.epochs[key]
		c.mu.Unlock()
		// Detach the mint from the first caller's ctx. singleflight
		// returns the same (result, err) pair to every waiter, so a
		// transient cancellation on the originating reconcile would
		// otherwise surface as context.Canceled on every concurrent
		// reconcile sharing the same SA — turning one slow reconcile
		// into a wave of unrelated Ready=False transitions. A bounded
		// background ctx keeps the cache lifetime independent of any
		// single caller's deadline; CreateToken on the apiserver
		// typically returns in well under a second, so the timeout is
		// only a hang guard.
		mintCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		token, expires, err := c.minter.Mint(mintCtx, namespace, serviceAccount, c.ttl)
		if err != nil {
			// Surface mint failures as Prometheus signal. The log line
			// at the reconciler.go call site is the only other channel;
			// the counter lets a cluster-wide mint outage drive a
			// JaaSTenantTokenMintFailing alert instead of relying on
			// per-pod log scraping.
			tenantTokenMintFailuresTotal.WithLabelValues(namespace, serviceAccount).Inc()
			return "", err
		}
		c.mu.Lock()
		if c.epochs[key] == epochAtMint {
			margin := c.refreshMarginFor(expires)
			c.tokens[key] = cachedToken{token: token, expires: expires, refreshAt: expires.Add(-margin)}
		}
		c.mu.Unlock()
		// Return the freshly-minted token to this caller (and every
		// singleflight waiter) regardless of the cache write: the token
		// is valid; a concurrent Forget only means we don't persist it,
		// so the next Token call re-mints.
		return token, nil
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

// lookup returns (token, true) when a cached entry for key has not yet reached
// its refreshAt instant; otherwise ("", false).
func (c *tokenCache) lookup(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.tokens[key]
	if !ok {
		return "", false
	}
	if !c.now().Before(cached.refreshAt) {
		return "", false
	}
	return cached.token, true
}

// refreshMarginFor returns the refresh margin to apply to a token expiring at
// expires. It is the configured refreshMargin, but clamped to a third of the
// token's actual lifetime: an apiserver that caps the SA token TTL below the
// margin (e.g. --service-account-max-token-expiration=2m) would otherwise mint
// tokens that are already past the fixed margin, so the cache would never serve
// a hit and every reconcile would hammer the TokenRequest API. Clamping keeps
// the cache useful (a short-lived token is reused for ~2/3 of its life) while
// still refreshing before expiry.
func (c *tokenCache) refreshMarginFor(expires time.Time) time.Duration {
	margin := c.refreshMargin
	if lifetime := expires.Sub(c.now()); lifetime > 0 && lifetime/3 < margin {
		margin = lifetime / 3
	}
	return margin
}

// Forget evicts the cached token for namespace/serviceAccount. Called when
// a snippet is deleted so a re-created snippet with the same SA mints a
// fresh token rather than reusing a stale entry. nil-safe.
func (c *tokenCache) Forget(namespace, serviceAccount string) {
	if c == nil {
		return
	}
	key := namespace + "/" + serviceAccount
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, key)
	c.epochs[key]++
}
