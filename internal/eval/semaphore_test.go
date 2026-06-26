/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-jsonnet"
)

// resetEvalSemaphore restores concurrent-eval state between tests. The
// package-level atomics persist across t.Run boundaries, so tests that
// flip the cap MUST register this via t.Cleanup.
func resetEvalSemaphore(t *testing.T) {
	t.Helper()
	concurrencyLimit.Store(0)
	inFlightEvals.Store(0)
	rejectedEvalsTotal.Store(0)
	t.Cleanup(func() {
		concurrencyLimit.Store(0)
		inFlightEvals.Store(0)
		rejectedEvalsTotal.Store(0)
	})
}

func TestReserveEvalSlot_DisabledCapAlwaysAcquires(t *testing.T) {
	resetEvalSemaphore(t)
	for i := range 8 {
		release, ok := reserveEvalSlot()
		if !ok {
			t.Fatalf("iter %d: cap=0 should never reject", i)
		}
		defer release()
	}
	if got := InFlightEvals(); got != 0 {
		t.Errorf("cap=0 must not bump inFlightEvals; got %d", got)
	}
}

func TestReserveEvalSlot_BlocksOnFullCap(t *testing.T) {
	resetEvalSemaphore(t)
	SetMaxConcurrentEvals(2)

	r1, ok1 := reserveEvalSlot()
	if !ok1 {
		t.Fatal("first acquire rejected with empty semaphore")
	}
	r2, ok2 := reserveEvalSlot()
	if !ok2 {
		t.Fatal("second acquire rejected with one slot free")
	}
	if _, ok := reserveEvalSlot(); ok {
		t.Fatal("third acquire succeeded past cap=2")
	}
	if got := RejectedEvalCount(); got != 1 {
		t.Errorf("RejectedEvalCount = %d, want 1", got)
	}
	if got := InFlightEvals(); got != 2 {
		t.Errorf("InFlightEvals = %d, want 2", got)
	}

	r1()
	r3, ok3 := reserveEvalSlot()
	if !ok3 {
		t.Fatal("release should have freed a slot")
	}
	r2()
	r3()
	if got := InFlightEvals(); got != 0 {
		t.Errorf("InFlightEvals after all releases = %d, want 0", got)
	}
}

func TestReserveEvalSlot_ConcurrentNeverExceedsCap(t *testing.T) {
	// CAS loop must prevent over-allocation under contention. Fire a
	// burst of acquirers above the cap and assert no acquire ever
	// pushes inFlightEvals past the limit.
	resetEvalSemaphore(t)
	const cap = 4
	const burst = 64
	SetMaxConcurrentEvals(cap)

	var peak atomic.Int64
	var accepted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range burst {
		wg.Go(func() {
			<-start
			release, ok := reserveEvalSlot()
			if !ok {
				return
			}
			accepted.Add(1)
			cur := inFlightEvals.Load()
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			release()
		})
	}
	close(start)
	wg.Wait()

	if got := peak.Load(); got > cap {
		t.Errorf("peak inFlightEvals = %d exceeded cap %d", got, cap)
	}
	if got := accepted.Load() + RejectedEvalCount(); got != burst {
		t.Errorf("accepted+rejected = %d, want %d", got, burst)
	}
}

func TestEvaluateAnonymousSnippet_ReturnsErrEvalUnavailableWhenFull(t *testing.T) {
	resetEvalSemaphore(t)
	SetMaxConcurrentEvals(1)
	// Hold the one slot so the eval below has nothing to reserve.
	release, ok := reserveEvalSlot()
	if !ok {
		t.Fatal("could not reserve baseline slot")
	}
	defer release()

	_, err := EvaluateAnonymousSnippet(context.Background(), "rejected.jsonnet", `1`, Options{})
	if !errors.Is(err, ErrEvalUnavailable) {
		t.Fatalf("got %v, want ErrEvalUnavailable", err)
	}
}

func TestEvaluateFile_ReturnsErrEvalUnavailableWhenFull(t *testing.T) {
	resetEvalSemaphore(t)
	SetMaxConcurrentEvals(1)
	release, _ := reserveEvalSlot()
	defer release()

	_, err := EvaluateFile(context.Background(), "does-not-matter.jsonnet", Options{})
	if !errors.Is(err, ErrEvalUnavailable) {
		t.Fatalf("got %v, want ErrEvalUnavailable; the gate must reject before reaching the disk", err)
	}
}

// TestEvaluateAnonymousSnippet_SlotHeldUntilEvalCompletes_NotParentReturn
// pins that when the parent's ctx fires before the synchronous eval
// finishes, the semaphore slot MUST stay reserved until the orphan
// goroutine actually returns. Releasing on parent return would let the
// next caller start another eval while the orphan is still consuming
// CPU — defeating the cap's bounded-worst-case promise.
//
// Mechanism: a blocking Importer pauses the eval inside go-jsonnet's
// import machinery. We observe inFlightEvals while the parent has already
// returned DeadlineExceeded, then unblock and observe the slot drain.
func TestEvaluateAnonymousSnippet_SlotHeldUntilEvalCompletes_NotParentReturn(t *testing.T) {
	resetEvalSemaphore(t)
	SetMaxConcurrentEvals(2)

	importerHold := make(chan struct{})
	imp := &blockingImporter{block: importerHold}

	const snippet = `(import "blocked") + { ok: true }`
	parentDone := make(chan struct{})
	var parentErr error
	go func() {
		_, parentErr = EvaluateAnonymousSnippet(context.Background(), "slot-held",
			snippet, Options{Importer: imp, Timeout: 50 * time.Millisecond})
		close(parentDone)
	}()

	// Wait for the parent to time out. After this returns, the orphan
	// goroutine is still inside the importer's <-block.
	<-parentDone
	if !errors.Is(parentErr, context.DeadlineExceeded) {
		t.Fatalf("parent error = %v, want context.DeadlineExceeded", parentErr)
	}

	// THIS is the load-bearing assertion. The slot must stay held while
	// the orphaned eval goroutine runs, even though the parent has already
	// returned. A `defer release()` that fired on parent return would let
	// this read observe 0.
	if got := InFlightEvals(); got != 1 {
		t.Errorf("InFlightEvals after parent timeout = %d, want 1 (orphan still running, slot must stay held)", got)
	}

	// Release the orphan. The slot drains via the closure's `defer release()`
	// running inside the eval goroutine — same code path as a non-timed-out
	// eval, just delayed by however long the eval naturally took.
	close(importerHold)
	deadline := time.Now().Add(2 * time.Second)
	for InFlightEvals() != 0 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if got := InFlightEvals(); got != 0 {
		t.Errorf("InFlightEvals after orphan completion = %d, want 0", got)
	}
}

// blockingImporter blocks on <-block whenever go-jsonnet calls Import.
// Tests use it to pause an eval inside the goroutine without timing
// dependence.
type blockingImporter struct {
	block chan struct{}
}

func (b *blockingImporter) Import(importedFrom, importedPath string) (jsonnet.Contents, string, error) {
	<-b.block
	return jsonnet.MakeContents("{}"), importedPath, nil
}

// TestReserve_ReleaseIsIdempotent pins that the release closure
// returned by Reserve must be safe to call more than once. A
// double-release without sync.Once would push inFlightEvals negative,
// permanently slackening the cap-check `cur >= limit` and silently
// disabling the gate (a future defer + early-explicit-release pattern
// is the realistic footgun).
func TestReserve_ReleaseIsIdempotent(t *testing.T) {
	resetEvalSemaphore(t)
	SetMaxConcurrentEvals(4)

	release, ok := Reserve()
	if !ok {
		t.Fatal("Reserve was denied with an empty semaphore")
	}
	if got := InFlightEvals(); got != 1 {
		t.Fatalf("InFlightEvals after Reserve = %d, want 1", got)
	}
	release()
	release()
	release()
	if got := InFlightEvals(); got != 0 {
		t.Errorf("InFlightEvals after triple-release = %d, want 0 (release must be idempotent)", got)
	}
	// And a fresh Reserve must still see the full cap available.
	if got := MaxConcurrentEvals() - InFlightEvals(); got != 4 {
		t.Errorf("free slots = %d, want 4 (negative counter would slacken the cap-check)", got)
	}
}

func TestSetMaxConcurrentEvals_NegativeIsTreatedAsZero(t *testing.T) {
	resetEvalSemaphore(t)
	SetMaxConcurrentEvals(-5)
	if got := MaxConcurrentEvals(); got != 0 {
		t.Errorf("MaxConcurrentEvals after Set(-5) = %d, want 0", got)
	}
	// And with cap=0, the gate is disabled.
	for i := range 4 {
		if _, ok := reserveEvalSlot(); !ok {
			t.Fatalf("iter %d: negative cap must disable the gate", i)
		}
	}
}
