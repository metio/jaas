/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestApplyTLAs_NilMapIsNoOp(t *testing.T) {
	out, err := EvaluateAnonymousSnippet(context.Background(), "demo",
		`{ ok: true }`, Options{TLAs: nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `"ok": true`) {
		t.Errorf("got %q, want it to render normally with nil TLAs", out)
	}
}

func TestApplyTLAs_EmptyMapIsNoOp(t *testing.T) {
	out, err := EvaluateAnonymousSnippet(context.Background(), "demo",
		`{ ok: true }`, Options{TLAs: map[string][]string{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `"ok": true`) {
		t.Errorf("got %q, want it to render normally with empty TLAs", out)
	}
}

func TestApplyTLAs_DoesNotMutateInputMap(t *testing.T) {
	// jaas owns the TLA map across the request boundary; mutation here
	// would surface as cross-request leakage on the HTTP path where
	// concurrent requests can share an Options template.
	tlas := map[string][]string{
		"single": {"a"},
		"multi":  {"x", "y", "z"},
	}
	original := map[string][]string{
		"single": append([]string{}, tlas["single"]...),
		"multi":  append([]string{}, tlas["multi"]...),
	}
	_, _ = EvaluateAnonymousSnippet(context.Background(), "demo",
		`function(single, multi) { single: single, multi: multi }`,
		Options{TLAs: tlas})
	if !slices.Equal(tlas["single"], original["single"]) {
		t.Errorf("TLAs[single] mutated: got %v, want %v", tlas["single"], original["single"])
	}
	if !slices.Equal(tlas["multi"], original["multi"]) {
		t.Errorf("TLAs[multi] mutated: got %v, want %v", tlas["multi"], original["multi"])
	}
}

func TestApplyTLAs_DispatchBoundary(t *testing.T) {
	// 1 element → string TLA (TLAVar); 2+ elements → JSON-array TLA
	// (TLACode). The HTTP path relies on this dispatch — a multi-value
	// query param must reach the snippet as an array, not a comma-joined
	// string.
	gotSingle, err := EvaluateAnonymousSnippet(context.Background(), "demo",
		`function(v) { kind: std.type(v) }`,
		Options{TLAs: map[string][]string{"v": {"only"}}})
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	if !strings.Contains(gotSingle, `"kind": "string"`) {
		t.Errorf("single-value: got %q, want kind=string", gotSingle)
	}
	gotMulti, err := EvaluateAnonymousSnippet(context.Background(), "demo",
		`function(v) { kind: std.type(v), len: std.length(v) }`,
		Options{TLAs: map[string][]string{"v": {"a", "b"}}})
	if err != nil {
		t.Fatalf("multi: %v", err)
	}
	if !strings.Contains(gotMulti, `"kind": "array"`) || !strings.Contains(gotMulti, `"len": 2`) {
		t.Errorf("multi-value: got %q, want kind=array len=2", gotMulti)
	}
}

func TestApplyTLAs_ConcurrentEvalsAreIsolated(t *testing.T) {
	// Each goroutine runs its own evaluation with a private TLA value.
	// The package-level state (semaphore, leak counter) must not let
	// one goroutine's TLA bleed into another's VM.
	const workers = 100
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("value-%d", i)
			got, err := EvaluateAnonymousSnippet(context.Background(), "demo",
				`function(v) { v: v }`,
				Options{TLAs: map[string][]string{"v": {want}}})
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
				return
			}
			expected := fmt.Sprintf(`"v": "value-%d"`, i)
			if !strings.Contains(got, expected) {
				errs <- fmt.Errorf("worker %d: got %q, want %s", i, got, expected)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
