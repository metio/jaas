/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"testing"
	"time"
)

func TestRateLimiter_NilReceiverAlwaysAllows(t *testing.T) {
	var l *RateLimiter
	for i := 0; i < 100; i++ {
		if ok, _ := l.Reserve("any"); !ok {
			t.Errorf("nil limiter denied iter %d", i)
		}
	}
}

func TestRateLimiter_AllowsUpToBurst(t *testing.T) {
	l := NewRateLimiter(1.0, 3)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Reserve("k"); !ok {
			t.Errorf("Reserve %d denied within burst", i)
		}
	}
	ok, delay := l.Reserve("k")
	if ok {
		t.Errorf("Reserve over burst was allowed")
	}
	if delay <= 0 {
		t.Errorf("over-burst delay = %v, want > 0", delay)
	}
}

func TestRateLimiter_DistinctKeysHaveSeparateBuckets(t *testing.T) {
	l := NewRateLimiter(0.001, 1)
	if ok, _ := l.Reserve("a"); !ok {
		t.Errorf("a denied on first call")
	}
	if ok, _ := l.Reserve("b"); !ok {
		t.Errorf("b denied on first call (bucket leak from a)")
	}
}

func TestRateLimiter_RejectionDoesNotDrainFutureTokens(t *testing.T) {
	// Rate = very slow, burst = 1: first call wins, second is rejected
	// with delay ~= refill interval. The rejection must NOT permanently
	// push back future calls — calling Reserve again should report the
	// same kind of delay, not a stacked one.
	l := NewRateLimiter(1.0, 1)
	if ok, _ := l.Reserve("x"); !ok {
		t.Fatal("first reserve denied")
	}
	_, d1 := l.Reserve("x")
	_, d2 := l.Reserve("x")
	// Allow a small tolerance: d2 must not be substantially larger than d1.
	if d2 > d1+200*time.Millisecond {
		t.Errorf("subsequent delay grew: d1=%v d2=%v", d1, d2)
	}
}

func TestRateLimiter_ZeroOrNegativeRateFallsBackToPermissive(t *testing.T) {
	l := NewRateLimiter(0, 1)
	for i := 0; i < 100; i++ {
		if ok, _ := l.Reserve("k"); !ok {
			t.Errorf("permissive limiter denied iter %d", i)
		}
	}
}

func TestRateLimiter_ZeroBurstFallsBackToPermissive(t *testing.T) {
	l := NewRateLimiter(1.0, 0)
	for i := 0; i < 100; i++ {
		if ok, _ := l.Reserve("k"); !ok {
			t.Errorf("permissive limiter denied iter %d", i)
		}
	}
}

func TestRateLimiter_Forget_ResetsBucket(t *testing.T) {
	l := NewRateLimiter(1.0, 1)
	if ok, _ := l.Reserve("k"); !ok {
		t.Fatal("first reserve denied")
	}
	if ok, _ := l.Reserve("k"); ok {
		t.Fatal("second reserve allowed when bucket should be drained")
	}
	l.Forget("k")
	if ok, _ := l.Reserve("k"); !ok {
		t.Errorf("Forget did not reset bucket")
	}
}

func TestRateLimiter_Forget_NilReceiverNoPanic(t *testing.T) {
	var l *RateLimiter
	l.Forget("any") // must not panic
}

// Direct construction with a zero-burst limiter hits the defensive !r.OK()
// branch in Reserve. NewRateLimiter clamps to burst>=1, but a caller that
// builds RateLimiter literally — bypassing the constructor — must not get
// stuck in a panic, just a denial.
func TestRateLimiter_DirectConstructionWithBurstBelowOneRejects(t *testing.T) {
	l := &RateLimiter{} // zero-value: rate=0, burst=0
	ok, delay := l.Reserve("k")
	if ok {
		t.Errorf("burst<1 limiter allowed reservation")
	}
	if delay <= 0 {
		t.Errorf("expected positive fallback delay, got %v", delay)
	}
}
