/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package eval wraps go-jsonnet with the ergonomics JaaS needs: per-call VM
// construction, deadline-aware evaluation, TLA/ExtVar plumbing, and a
// pluggable Importer. Both the HTTP handler (FileImporter, file-resolved
// snippets) and the operator reconciler (InMemoryImporter, inline files)
// route through this package.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/google/go-jsonnet"
)

// ErrEvalUnavailable is returned by EvaluateFile / EvaluateAnonymousSnippet
// when the global concurrent-eval cap is full. The synchronous go-jsonnet
// API has no cancellation entry point, so an unbounded queue of evals
// would let a runaway snippet pile up goroutines that outlive every
// caller's ctx. The cap bounds the actual number of in-flight evaluation
// goroutines (not just parent-attached reservations): slots are released
// inside the eval goroutine when the synchronous call returns, NOT when
// the parent gives up via timeout. Callers turn the rejection into
// backpressure (503 / RequeueAfter) rather than failing the snippet
// outright.
var ErrEvalUnavailable = errors.New("evaluation slots are full; eval rejected")

// leakedEvals counts evaluations whose parent returned via timeout or
// context cancellation BEFORE the underlying vm.EvaluateFile /
// vm.EvaluateAnonymousSnippet completed. go-jsonnet has no context-aware
// entry point, so once an evaluation has started the goroutine
// continues until the snippet finishes naturally — the counter goes
// up when the parent gives up and back down when the orphan completes.
//
// Operators read this via OutstandingTimedOutEvals() and wire it into
// the metrics endpoint (see internal/operator/metrics.go). Sustained
// non-zero readings flag a malicious-client or runaway-snippet pattern
// that the synchronous-eval API can't otherwise surface.
var leakedEvals atomic.Int64

// OutstandingTimedOutEvals returns the current count of evaluation
// goroutines whose parent already gave up via timeout/cancel. The value
// transiently spikes during snippet bursts and settles to zero when
// every orphan finishes. Persistent non-zero readings indicate a
// snippet whose evaluation cost dwarfs the configured timeout — tune
// EvaluationTimeout or MaxStack until the value returns to zero.
func OutstandingTimedOutEvals() int64 { return leakedEvals.Load() }

// Options carries every per-evaluation knob. The zero value evaluates a
// snippet with no ExtVars, no TLAs, no Importer (so any `import` fails),
// no timeout, and go-jsonnet's default stack depth.
type Options struct {
	// ExtVars are std.extVar lookups available to the snippet.
	ExtVars map[string]string

	// TLAs follow the URL-query convention used by the HTTP path: a
	// single-element value becomes a string TLA via vm.TLAVar; a
	// multi-element value becomes a JSON-encoded array TLA via vm.TLACode.
	// Operator-driven evaluations build this map from spec.tlas; the
	// reconciler and HTTP handler share semantics so behavior is identical
	// across both surfaces.
	TLAs map[string][]string

	// MaxStack overrides go-jsonnet's default call-stack depth when > 0.
	MaxStack int

	// Timeout bounds a single evaluation. Zero disables the bound.
	Timeout time.Duration

	// Importer resolves `import` / `importstr` calls inside the snippet.
	// Leaving it nil makes any `import` fail; the HTTP path passes a
	// FileImporter, the reconciler an InMemoryImporter.
	Importer jsonnet.Importer
}

// EvaluateFile runs vm.EvaluateFile for fileName. The supplied Importer (or
// go-jsonnet's default search) must be able to resolve the path. Returns
// ErrEvalUnavailable when the global concurrent-eval cap is full.
//
// The semaphore slot is held until the synchronous go-jsonnet call actually
// returns — including the timed-out path where this function has already
// returned ctx.Err() to its caller. That's why the `defer release()` lives
// inside the eval closure (which runs in the eval goroutine) rather than
// at this function's scope: the cap must bound real eval goroutines, not
// just parent-attached reservations.
func EvaluateFile(ctx context.Context, fileName string, opts Options) (string, error) {
	release, ok := reserveEvalSlot()
	if !ok {
		return "", ErrEvalUnavailable
	}
	// buildVM runs inside the eval closure so it shares the closure's
	// `defer release()` and the goroutine's panic-recover: a panic in
	// VM construction (the ExtVar/TLACode parsing go-jsonnet does
	// eagerly) frees the slot and surfaces as a normal eval error
	// rather than leaking the reservation and ratcheting the cap toward
	// permanent ErrEvalUnavailable.
	return evaluateWithDeadline(ctx, func() (string, error) {
		defer release()
		return buildVM(opts).EvaluateFile(fileName)
	}, opts.Timeout)
}

// EvaluateAnonymousSnippet runs vm.EvaluateAnonymousSnippet — useful when
// the source is in memory (no file path). `name` is the diagnostic label
// go-jsonnet attaches to errors and stack frames. Returns ErrEvalUnavailable
// when the global concurrent-eval cap is full. See EvaluateFile for the
// inside-the-closure release rationale.
func EvaluateAnonymousSnippet(ctx context.Context, name, source string, opts Options) (string, error) {
	release, ok := reserveEvalSlot()
	if !ok {
		return "", ErrEvalUnavailable
	}
	return evaluateWithDeadline(ctx, func() (string, error) {
		defer release()
		return buildVM(opts).EvaluateAnonymousSnippet(name, source)
	}, opts.Timeout)
}

func buildVM(opts Options) *jsonnet.VM {
	vm := jsonnet.MakeVM()
	if opts.Importer != nil {
		vm.Importer(opts.Importer)
	}
	if opts.MaxStack > 0 {
		vm.MaxStack = opts.MaxStack
	}
	for k, v := range opts.ExtVars {
		vm.ExtVar(k, v)
	}
	applyTLAs(vm, opts.TLAs)
	return vm
}

// evaluateWithDeadline wraps a synchronous evaluator in a deadline so a
// runaway VM can't outlive its request/reconcile. Cancellation comes from
// either the caller's ctx or the explicit timeout, whichever fires first.
//
// go-jsonnet has no context-aware evaluator: the goroutine started here
// runs until eval() returns naturally, even after this function
// returns. When ctx fires first, leakedEvals is incremented; a small
// drain goroutine decrements it once the evaluation finally completes.
// Operators expose the live count via OutstandingTimedOutEvals to spot
// runaway snippets that the synchronous API otherwise hides.
func evaluateWithDeadline(ctx context.Context, eval func() (string, error), timeout time.Duration) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		// go-jsonnet panics on a small set of malformed inputs that
		// the parser doesn't fully guard against (deep-import cycles
		// that overflow internal state, etc.). The parent's select
		// expects a single send on ch; a propagated panic would
		// terminate the operator process. Convert panic into an
		// error result so the reconcile completes with a
		// classifiable failure.
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("jsonnet evaluation panicked: %v\n%s", r, debug.Stack())}
			}
		}()
		out, err := eval()
		ch <- result{out: out, err: err}
	}()

	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
		// The eval goroutine is still running and will send to ch
		// when it eventually finishes. A drain goroutine receives
		// that send and decrements the leak counter — keeps the
		// gauge accurate without racy shared-flag bookkeeping.
		leakedEvals.Add(1)
		go func() {
			<-ch
			leakedEvals.Add(-1)
		}()
		return "", ctx.Err()
	}
}

// applyTLAs translates a TLA map onto vm. The Marshal error is intentionally
// discarded — map[string][]string has no Marshal-error path in encoding/json.
func applyTLAs(vm *jsonnet.VM, tlas map[string][]string) {
	for key, value := range tlas {
		if len(value) == 1 {
			vm.TLAVar(key, value[0])
			continue
		}
		bytes, _ := json.Marshal(value)
		vm.TLACode(key, string(bytes))
	}
}
