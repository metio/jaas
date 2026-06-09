/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter bounds how frequently a single snippet can drive an end-to-end
// reconcile. The bucket is per-key (namespace/name) so a runaway snippet
// cannot starve unrelated ones.
//
// The zero value is unusable; construct via NewRateLimiter. A nil receiver is
// allowed and always permits; callers can leave the field nil to disable
// rate limiting without conditional logic at the call site.
type RateLimiter struct {
	rate  rate.Limit
	burst int

	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

// NewRateLimiter returns a limiter whose per-key bucket refills at perSec
// tokens per second up to a maximum of burst. Both arguments must be > 0;
// values that don't satisfy that fall back to a permissive limiter.
func NewRateLimiter(perSec float64, burst int) *RateLimiter {
	if perSec <= 0 || burst <= 0 {
		return &RateLimiter{rate: rate.Inf, burst: 1, buckets: map[string]*rate.Limiter{}}
	}
	return &RateLimiter{
		rate:    rate.Limit(perSec),
		burst:   burst,
		buckets: map[string]*rate.Limiter{},
	}
}

// Reserve consumes one token for key. Returns allowed=true with delay=0 when
// a token was available; returns allowed=false with delay set to the wait
// time until the next token is ready. The bucket is NOT drained on rejection
// so a runaway caller can't push the wait time past its natural value.
func (l *RateLimiter) Reserve(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	b := l.bucket(key)
	r := b.Reserve()
	if !r.OK() {
		// burst is configured smaller than 1 — the limiter rejects any
		// reservation. Returning a small delay lets the manager requeue.
		return false, time.Second
	}
	d := r.Delay()
	if d == 0 {
		return true, 0
	}
	r.Cancel()
	return false, d
}

// Forget drops the bucket for key. Called when a snippet is deleted so the
// map doesn't grow unbounded.
func (l *RateLimiter) Forget(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

func (l *RateLimiter) bucket(key string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		// Direct &RateLimiter{} construction (bypassing NewRateLimiter)
		// leaves the map nil; lazily allocate so the limiter still works.
		l.buckets = make(map[string]*rate.Limiter)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = rate.NewLimiter(l.rate, l.burst)
		l.buckets[key] = b
	}
	return b
}
