/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"sync"
	"sync/atomic"
)

// concurrencyLimit caps the number of evaluations active at once.
// Zero (the default) disables the gate — every caller proceeds. The
// flag-level setting in main.go wires SetMaxConcurrentEvals at startup
// so the cap survives process restarts.
var concurrencyLimit atomic.Int64

// inFlightEvals counts evaluations actively running through eval(). It
// rises on reserveEvalSlot success and falls on the matching release().
// The Prometheus jaas_eval_in_flight gauge reads it via InFlightEvals.
var inFlightEvals atomic.Int64

// rejectedEvalsTotal counts evaluations the semaphore turned away
// because the cap was full. Monotonic; never decremented. The
// Prometheus jaas_eval_unavailable_total counter reads it via
// RejectedEvalCount and resets on process restart.
var rejectedEvalsTotal atomic.Int64

// SetMaxConcurrentEvals configures the global concurrent-eval cap.
// n <= 0 disables the gate; the eval functions then never reject.
// Called from main.go after flag parsing; subsequent calls take effect
// atomically on the next reservation attempt.
func SetMaxConcurrentEvals(n int) {
	if n < 0 {
		n = 0
	}
	concurrencyLimit.Store(int64(n))
}

// MaxConcurrentEvals returns the current cap. Zero means disabled.
// Exposed for the Prometheus jaas_eval_max_concurrent gauge so
// dashboards can plot the live cap alongside the in-flight value.
func MaxConcurrentEvals() int64 { return concurrencyLimit.Load() }

// InFlightEvals returns the number of evaluations currently holding a
// semaphore slot. Spikes during snippet bursts; sits at zero on idle.
// Used by the jaas_eval_in_flight Prometheus gauge.
func InFlightEvals() int64 { return inFlightEvals.Load() }

// RejectedEvalCount returns the total evaluations rejected by the
// semaphore since process start. Monotonic. Used by the
// jaas_eval_unavailable_total Prometheus counter.
func RejectedEvalCount() int64 { return rejectedEvalsTotal.Load() }

// Reserve acquires a semaphore slot without performing an evaluation —
// useful for tests pinning the cap and for advanced callers that need
// to gate work that wraps EvaluateFile / EvaluateAnonymousSnippet. On
// success returns (release, true); on a full cap returns (nil, false).
// The returned release closure is idempotent — calling it more than
// once is harmless (only the first call decrements inFlightEvals).
// Idempotency matters because a double-release would push the counter
// negative, which permanently slackens the cap-check `cur >= limit`
// and silently disables the protection the cap is meant to enforce.
func Reserve() (func(), bool) { return reserveEvalSlot() }

// reserveEvalSlot acquires a semaphore slot. Returns (release, true)
// on success — release is idempotent via sync.Once. Returns (nil,
// false) when the cap is set and full; the caller surfaces
// ErrEvalUnavailable. The cap-zero (disabled) branch returns a no-op
// release so the deferred call is harmless.
//
// Uses a CAS loop so a transient overshoot of the cap never occurs —
// add-then-decrement would briefly count a rejecter against in-flight.
func reserveEvalSlot() (func(), bool) {
	limit := concurrencyLimit.Load()
	if limit <= 0 {
		return func() {}, true
	}
	for {
		cur := inFlightEvals.Load()
		if cur >= limit {
			rejectedEvalsTotal.Add(1)
			return nil, false
		}
		if inFlightEvals.CompareAndSwap(cur, cur+1) {
			var once sync.Once
			return func() { once.Do(func() { inFlightEvals.Add(-1) }) }, true
		}
	}
}
