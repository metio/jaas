/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEvaluateAnonymousSnippet_TrivialObject(t *testing.T) {
	got, err := EvaluateAnonymousSnippet(context.Background(), "demo", `{ ok: true }`, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"ok": true`) {
		t.Errorf("got %q, want it to contain '\"ok\": true'", got)
	}
}

func TestEvaluateAnonymousSnippet_ExtVarIsAvailable(t *testing.T) {
	got, err := EvaluateAnonymousSnippet(context.Background(), "demo",
		`{ env: std.extVar("env") }`,
		Options{ExtVars: map[string]string{"env": "prod"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"prod"`) {
		t.Errorf("got %q, want it to contain 'prod'", got)
	}
}

func TestEvaluateAnonymousSnippet_SingleValueTLAUsedAsString(t *testing.T) {
	got, err := EvaluateAnonymousSnippet(context.Background(), "demo",
		`function(env) { env: env }`,
		Options{TLAs: map[string][]string{"env": {"dev"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"dev"`) {
		t.Errorf("got %q, want 'dev'", got)
	}
}

func TestEvaluateAnonymousSnippet_MultiValueTLABecomesArray(t *testing.T) {
	got, err := EvaluateAnonymousSnippet(context.Background(), "demo",
		`function(tags) { tags: tags }`,
		Options{TLAs: map[string][]string{"tags": {"a", "b"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"a"`) || !strings.Contains(got, `"b"`) {
		t.Errorf("got %q, want it to contain both 'a' and 'b'", got)
	}
}

func TestEvaluateAnonymousSnippet_MaxStackAppliedWhenPositive(t *testing.T) {
	// Recursion that just exceeds the configured stack should fail.
	src := `local f(n) = if n == 0 then 0 else f(n-1); f(50)`
	_, err := EvaluateAnonymousSnippet(context.Background(), "deep", src, Options{MaxStack: 5})
	if err == nil {
		t.Fatal("expected stack-depth error, got nil")
	}
}

func TestEvaluateAnonymousSnippet_TimeoutHonored(t *testing.T) {
	src := `local f(n) = if n == 0 then 0 else f(n-1) + 1; f(100000)`
	_, err := EvaluateAnonymousSnippet(context.Background(), "loop", src,
		Options{Timeout: 1 * time.Microsecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
}

// Regression: when the parent times out before the synchronous eval
// finishes, leakedEvals must reflect the orphan so an operator
// scraping jaas_eval_outstanding_timed_out sees the runaway, AND the
// counter must return to baseline once the orphan completes. A
// black-box test using a real recursive snippet races: the eval
// finishes in microseconds and the drain may run before the test
// observes the elevated value. White-box: drive evaluateWithDeadline
// directly with a controllable eval function so the timing is fully
// in the test's hands.
func TestEvaluateWithDeadline_AccountsLeakedEvalThenDrains(t *testing.T) {
	baseline := leakedEvals.Load()

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _ = evaluateWithDeadline(context.Background(), func() (string, error) {
			close(started)
			<-release
			return "{}", nil
		}, 1*time.Millisecond)
	}()

	// Wait for the eval goroutine to enter eval() and for the parent
	// to hit its ctx.Done branch. By that point leakedEvals must be
	// >= baseline+1; if not, the accounting is missing.
	<-started
	deadline := time.Now().Add(2 * time.Second)
	for leakedEvals.Load() < baseline+1 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if got := leakedEvals.Load(); got < baseline+1 {
		t.Fatalf("leakedEvals = %d, want >= %d (parent timed out but leak was not counted)", got, baseline+1)
	}

	// Let the eval finally complete; the drain goroutine must
	// decrement the counter back to baseline.
	close(release)
	deadline = time.Now().Add(2 * time.Second)
	for leakedEvals.Load() > baseline && time.Now().Before(deadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if got := leakedEvals.Load(); got != baseline {
		t.Errorf("after drain: leakedEvals = %d, want %d (drain goroutine never ran)", got, baseline)
	}
	<-done
}

func TestEvaluateAnonymousSnippet_CallerContextCancellationSurfaces(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	src := `local f(n) = if n == 0 then 0 else f(n-1) + 1; f(1000000)`
	_, err := EvaluateAnonymousSnippet(ctx, "loop", src, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestEvaluateAnonymousSnippet_SyntaxErrorBubblesUp(t *testing.T) {
	_, err := EvaluateAnonymousSnippet(context.Background(), "broken", `local x =`, Options{})
	if err == nil {
		t.Fatal("expected syntax error, got nil")
	}
}

func TestEvaluateAnonymousSnippet_ImporterIsApplied(t *testing.T) {
	imp := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"main.libsonnet": `{ ok: "yes" }`}},
		},
	}
	got, err := EvaluateAnonymousSnippet(context.Background(), "snippet",
		`(import "utils") + { extra: true }`,
		Options{Importer: imp})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"yes"`) {
		t.Errorf("got %q, want it to contain 'yes' from the imported library", got)
	}
}

func TestEvaluateFile_NotImplementedWithoutImporter(t *testing.T) {
	// Without an Importer, EvaluateFile falls through to go-jsonnet's default
	// loader which can't find a non-existent file.
	_, err := EvaluateFile(context.Background(), "no-such-file.jsonnet", Options{})
	if err == nil {
		t.Fatal("expected file-not-found error, got nil")
	}
}

// TestEvaluateWithDeadline_PanicInGoroutineSurfacesAsError pins the
// safety net for go-jsonnet panics: an evaluator that panics (rare,
// but observed historically on malformed snippets the parser doesn't
// fully guard against) must NOT take down the operator process. The
// goroutine's recover converts the panic into an error on the result
// channel; the parent observes that and returns it normally.
func TestEvaluateWithDeadline_PanicInGoroutineSurfacesAsError(t *testing.T) {
	out, err := evaluateWithDeadline(context.Background(), func() (string, error) {
		panic("synthetic go-jsonnet panic")
	}, 5*time.Second)
	if err == nil {
		t.Fatal("panic did not surface as an error — the operator would have crashed")
	}
	if !strings.Contains(err.Error(), "synthetic go-jsonnet panic") {
		t.Errorf("err = %v, want to mention the panic value", err)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("err = %v, want the 'panicked' marker so log readers can filter", err)
	}
	if out != "" {
		t.Errorf("out = %q, want empty on panic path", out)
	}
}
