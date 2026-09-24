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
	"time"

	"github.com/metio/jaas/internal/opstate"
)

// noDelay keeps the retry loop from waiting out a real backoff.
func noDelay(int) time.Duration { return time.Microsecond }

// TestSupervise_RetriesAFailingManager is the whole point of the supervisor: a
// manager that cannot be built — an apiserver the pod cannot reach, a missing
// RBAC verb — must not end the process, and must be retried until it works.
func TestSupervise_RetriesAFailingManager(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := opstate.New()
	var mu sync.Mutex
	calls := 0
	done := make(chan struct{})

	go func() {
		defer close(done)
		supervise(ctx, state, discardLogger(), func(ctx context.Context) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n < 3 {
				return errors.New("create manager: i/o timeout")
			}
			// The third attempt is the one that "works": block like a running
			// manager until the process shuts down.
			<-ctx.Done()
			return ctx.Err()
		}, noDelay, time.Hour)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 3
	}, "three manager starts")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervise did not return after the context was cancelled")
	}
	if got := state.Snapshot().Attempts; got < 3 {
		t.Errorf("Attempts = %d, want at least 3", got)
	}
}

func TestSupervise_RecordsTheFailureReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := opstate.New()
	done := make(chan struct{})
	go func() {
		defer close(done)
		supervise(ctx, state, discardLogger(), func(context.Context) error {
			return errors.New("register watch indexes: failed to get server groups")
		}, noDelay, time.Hour)
	}()

	waitFor(t, func() bool {
		return strings.Contains(state.Snapshot().Reason, "failed to get server groups")
	}, "the failure reason to reach the state")

	if state.Available() {
		t.Error("Available() = true while every manager start fails")
	}
	cancel()
	<-done
}

// A cancelled context is a shutdown, not a degradation: the reading must not be
// rewritten on the way out, or a pod that exits cleanly would log and report a
// failure it never had.
func TestSupervise_TreatsCancellationAsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	state := opstate.New()
	state.MarkAvailable()

	done := make(chan struct{})
	go func() {
		defer close(done)
		supervise(ctx, state, discardLogger(), func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}, noDelay, time.Hour)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervise did not return after the context was cancelled")
	}
	if !state.Available() {
		t.Error("Available() = false after a shutdown, want the last reading left alone")
	}
}

// A manager that ran for a while and then died gets a fresh backoff: the delay
// that grew during an earlier outage says nothing about this failure.
func TestSupervise_RestartsTheBackoffAfterAHealthyRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const healthyRun = 20 * time.Millisecond
	state := opstate.New()
	var mu sync.Mutex
	var attempts []int
	calls := 0

	done := make(chan struct{})
	go func() {
		defer close(done)
		supervise(ctx, state, discardLogger(), func(ctx context.Context) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			switch n {
			case 1, 2:
				// Two builds that fail immediately.
				return errors.New("create manager: i/o timeout")
			case 3:
				// One that lasts long enough to count as healthy, then dies.
				time.Sleep(2 * healthyRun)
				return errors.New("leader election lost")
			default:
				<-ctx.Done()
				return ctx.Err()
			}
		}, func(attempt int) time.Duration {
			mu.Lock()
			attempts = append(attempts, attempt)
			mu.Unlock()
			return time.Microsecond
		}, healthyRun)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(attempts) >= 3
	}, "three backoff decisions")
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 1}
	for i, w := range want {
		if attempts[i] != w {
			t.Errorf("backoff attempt[%d] = %d, want %d (sequence %v)", i, attempts[i], w, attempts[:len(want)])
		}
	}
}

// A manager that reaches a synced cache and then dies at once keeps the growing
// delay. Resetting on availability alone would retry about once a second for as
// long as the cause lasted — a webhook server whose certificate has not been
// issued yet is the shape of failure that does this.
func TestSupervise_KeepsTheBackoffWhenAnAvailableManagerDiesAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := opstate.New()
	var mu sync.Mutex
	var attempts []int
	calls := 0

	done := make(chan struct{})
	go func() {
		defer close(done)
		supervise(ctx, state, discardLogger(), func(ctx context.Context) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n > 3 {
				<-ctx.Done()
				return ctx.Err()
			}
			// Each attempt syncs its cache and then fails immediately.
			state.MarkAvailable()
			return errors.New("open /tmp/serving-certs/tls.crt: no such file or directory")
		}, func(attempt int) time.Duration {
			mu.Lock()
			attempts = append(attempts, attempt)
			mu.Unlock()
			return time.Microsecond
		}, time.Hour)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(attempts) >= 3
	}, "three backoff decisions")
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 3}
	for i, w := range want {
		if attempts[i] != w {
			t.Errorf("backoff attempt[%d] = %d, want %d (sequence %v)", i, attempts[i], w, attempts[:len(want)])
		}
	}
}

func TestSuperviseDelay_GrowsThenCaps(t *testing.T) {
	// Jitter makes each delay a range rather than a value, so the assertions
	// bracket the exponential rather than naming it.
	for _, tc := range []struct {
		attempt  int
		min, max time.Duration
	}{
		{1, 800 * time.Millisecond, 1200 * time.Millisecond},
		{2, 1600 * time.Millisecond, 2400 * time.Millisecond},
		{4, 6400 * time.Millisecond, 9600 * time.Millisecond},
		{100, 4 * time.Minute, 6 * time.Minute},
	} {
		got := superviseDelay(tc.attempt)
		if got < tc.min || got > tc.max {
			t.Errorf("superviseDelay(%d) = %v, want within [%v, %v]", tc.attempt, got, tc.min, tc.max)
		}
	}
}

func TestSuperviseDelay_NeverExceedsTheCap(t *testing.T) {
	for attempt := 1; attempt < 200; attempt++ {
		got := superviseDelay(attempt)
		if got <= 0 {
			t.Fatalf("superviseDelay(%d) = %v, want a positive delay", attempt, got)
		}
		if limit := superviseMaxDelay + time.Duration(float64(superviseMaxDelay)*superviseJitterFraction); got > limit {
			t.Fatalf("superviseDelay(%d) = %v, want at most %v", attempt, got, limit)
		}
	}
}

// waitFor polls cond until it holds, failing the test rather than hanging when
// the supervisor never gets there.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
