/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tenantClientCache memoizes the impersonating controller-runtime client
// built by SnippetReconciler.tenantClient. Each entry is keyed by
// "<namespace>/<serviceAccount>" and stores the bearer token the client was
// constructed with; a subsequent Token() call for the same key returns the
// cached client only while the token is unchanged.
//
// Why cache at all: client.New builds a fresh RESTMapper + transport per
// call. Under steady reconcile load that allocation lands in the per-event
// hot path even though the underlying token rotates roughly every hour. A
// token-equality check is cheap; a full client construction is not.
//
// Why single-entry-per-key: when the apiserver issues a new token, the
// previous client's baked-in BearerToken is useless. Replacing the entry
// (rather than letting both linger) keeps the cache bounded by the number
// of live (namespace, SA) pairs the operator has reconciled.
type tenantClientCache struct {
	mu      sync.Mutex
	entries map[string]tenantClientEntry
	// gen is a single monotonic counter bumped on every Forget. Get hands the
	// caller the current gen; Put writes only when it's unchanged, so a Forget
	// landing between a concurrent Get-miss and its rebuild+Put can't be
	// resurrected. Makes the cache correct at any MaxConcurrentReconciles,
	// mirroring tokenCache and cycleCache.
	gen int64
}

type tenantClientEntry struct {
	token  string
	client client.Client
}

func newTenantClientCache() *tenantClientCache {
	return &tenantClientCache{entries: map[string]tenantClientEntry{}}
}

// Get returns the cached client when one exists for key AND was built with
// the supplied token. A token mismatch returns miss. The second return is
// the current gen, which the caller hands back to Put so a Forget
// in between drops the stale write.
func (c *tenantClientCache) Get(key, token string) (client.Client, int64, bool) {
	if c == nil {
		return nil, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	gen := c.gen
	e, ok := c.entries[key]
	if !ok || e.token != token {
		return nil, gen, false
	}
	return e.client, gen, true
}

// Put stores cl under key with the token it was built with, provided the
// gen hasn't moved since epochAtGet. A mismatch means a Forget
// evicted the entry mid-rebuild; the write is dropped. A subsequent
// Get(key, token) returns cl until Forget evicts it or a different token
// triggers a replace.
func (c *tenantClientCache) Put(key, token string, epochAtGet int64, cl client.Client) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != epochAtGet {
		return
	}
	c.entries[key] = tenantClientEntry{token: token, client: cl}
}

// Forget evicts the cached client for key and bumps gen. Called from
// the finalizer path in lock-step with tokenCache.Forget so a re-created
// snippet against the same SA mints a fresh token AND rebuilds the client
// around it. nil-safe.
func (c *tenantClientCache) Forget(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
	c.gen++
}
