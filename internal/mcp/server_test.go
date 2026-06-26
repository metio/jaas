/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"pgregory.net/rapid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/eval"
)

// failingClient builds a fake client whose calls are intercepted by funcs, so a
// test can force List/Get/Patch errors the in-memory fake never produces.
func failingClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := jaasv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

// TestNewHTTPHandler_ServesOverHTTP drives tools through the real streamable HTTP
// transport (the in-cluster path), covering NewHTTPHandler end to end.
func TestNewHTTPHandler_ServesOverHTTP(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		Version:    "test",
		KubeClient: fakeClient(t, newSnippet("team-a", "dash", false, metav1.ConditionTrue, "Synced", "ok")),
	}
	srv := httptest.NewServer(NewHTTPHandler(cfg))
	defer srv.Close()

	c := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := c.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "render_jsonnet",
		Arguments: map[string]any{"source": "{a: 1}"},
	})
	if err != nil {
		t.Fatalf("call render_jsonnet over HTTP: %v", err)
	}
	if res.IsError {
		t.Fatalf("render tool error: %s", textContent(t, res))
	}
}

// TestRun_StdioReturnsOnCancelledContext covers the stdio Run entry point: a
// cancelled context makes the server close and Run return.
func TestRun_StdioReturnsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Config{Version: "test"}) }()
	select {
	case <-done:
		// returned (error or nil both fine — the point is it doesn't hang)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestListSnippets_ClientError(t *testing.T) {
	c := failingClient(t, interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return errors.New("apiserver down")
		},
	})
	res, _, err := Config{KubeClient: c}.listSnippetsHandler(context.Background(), nil, listSnippetsInput{})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError on List failure, got %+v", res)
	}
}

func TestMutate_PatchError(t *testing.T) {
	c := failingClient(t, interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return errors.New("conflict storm")
		},
	}, newSnippet("team-a", "dash", false, metav1.ConditionTrue, "Synced", "ok"))
	res, _, err := Config{KubeClient: c, AllowMutations: true}.suspendSnippetHandler(context.Background(), nil, mutateInput{Namespace: "team-a", Name: "dash"})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError on Patch failure, got %+v", res)
	}
}

func TestGetSnippet_RendersLastSyncTime(t *testing.T) {
	snip := newSnippet("team-a", "dash", false, metav1.ConditionTrue, "Synced", "ok")
	snip.Status.LastSyncTime = new(metav1.Now())
	cfg := Config{KubeClient: fakeClient(t, snip), RunbookBaseURL: testRunbookBase}
	_, out, err := cfg.getSnippetHandler(context.Background(), nil, getSnippetInput{Namespace: "team-a", Name: "dash"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.LastSyncTime == "" {
		t.Fatal("LastSyncTime not rendered")
	}
}

func TestEvalErrorResult_AllBranches(t *testing.T) {
	cfg := Config{EvaluationTimeout: 3 * time.Second}
	cases := map[string]error{
		"unavailable": eval.ErrEvalUnavailable,
		"timeout":     context.DeadlineExceeded,
		"generic":     errors.New("boom diagnostic"),
	}
	for name, err := range cases {
		res := cfg.evalErrorResult(err)
		if res == nil || !res.IsError {
			t.Fatalf("%s: expected IsError result", name)
		}
		if textContent(t, res) == "" {
			t.Fatalf("%s: expected a message", name)
		}
	}
}

// TestValidate_EvalUnavailable covers validate's operational-error path (a full
// eval cap is surfaced as a tool error, not a validation verdict).
func TestValidate_EvalUnavailable(t *testing.T) {
	eval.SetMaxConcurrentEvals(1)
	defer eval.SetMaxConcurrentEvals(0)
	release, ok := eval.Reserve()
	if !ok {
		t.Fatal("could not reserve the only eval slot")
	}
	defer release()

	res, _, err := Config{}.validateHandler(context.Background(), nil, renderInput{Source: "{a: 1}"})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError when eval cap is full, got %+v", res)
	}
}

// TestValidate_ContextCanceled covers validate's cancellation path: a request
// canceled mid-eval (client disconnect / parent ctx cancel) is an operational
// failure surfaced as a tool error — NOT a valid=false verdict. Reporting the
// snippet invalid would mislead the agent about a compilable snippet.
func TestValidate_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before the eval starts
	// A non-trivial snippet so the eval goroutine cannot outrace the parent's
	// select to the already-closed ctx.Done(); the deadline wrapper then
	// returns ctx.Err() (context.Canceled).
	src := `std.foldl(function(a, b) a + b, std.range(1, 50000), 0)`
	res, out, err := Config{}.validateHandler(ctx, nil, renderInput{Source: src})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("a canceled request must surface as a tool error, got res=%+v out=%+v", res, out)
	}
	if out.Valid {
		t.Fatal("a canceled request must not report valid=true")
	}
}

func TestMergedExtVars_Property(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		kv := rapid.MapOf(rapid.StringN(0, 8, 8), rapid.String())
		server := kv.Draw(rt, "server")
		call := kv.Draw(rt, "call")
		got := Config{ExtVars: server}.mergedExtVars(call)

		// Every call key wins; every server key absent from call survives.
		for k, v := range call {
			if got[k] != v {
				rt.Fatalf("call key %q = %q, want %q (call overlays server)", k, got[k], v)
			}
		}
		for k, v := range server {
			if _, overridden := call[k]; !overridden && got[k] != v {
				rt.Fatalf("server key %q = %q, want %q", k, got[k], v)
			}
		}
		// The merged map carries exactly the union of keys — no extras.
		for k := range got {
			_, inServer := server[k]
			_, inCall := call[k]
			if !inServer && !inCall {
				rt.Fatalf("merged map has stray key %q", k)
			}
		}
	})
}

func TestRunbookURL_Property(t *testing.T) {
	cfg := Config{RunbookBaseURL: testRunbookBase}
	rapid.Check(t, func(rt *rapid.T) {
		reason := rapid.String().Draw(rt, "reason")
		got := cfg.runbookURL(reason)
		if reason == "" {
			if got != "" {
				rt.Fatalf("empty reason yielded %q", got)
			}
			return
		}
		if want := testRunbookBase + strings.ToLower(reason) + "/"; got != want {
			rt.Fatalf("runbookURL(%q) = %q, want %q", reason, got, want)
		}
	})
}

// FuzzRenderJsonnet throws arbitrary snippet text at the render tool. The
// contract: it never panics and always returns either a success result (valid
// JSON) or a tool-error result — never a Go error and never nil/nil.
func FuzzRenderJsonnet(f *testing.F) {
	for _, seed := range []string{
		`{a: 1}`,
		`function(x) {y: x}`,
		`std.range(1, 3)`,
		`{`,
		``,
		`local f(x) = f(x); f(0)`,
		`error "boom"`,
	} {
		f.Add(seed)
	}
	cfg := Config{EvaluationTimeout: 2 * time.Second, MaxStack: 50}
	f.Fuzz(func(t *testing.T, source string) {
		res, out, err := cfg.renderHandler(context.Background(), nil, renderInput{Source: source})
		if err != nil {
			t.Fatalf("renderHandler returned a Go error (should be a tool result): %v", err)
		}
		if res == nil && out.JSON == "" {
			t.Fatalf("neither a result nor output for source %q", source)
		}
		if res != nil && !res.IsError {
			// A success result's content must be valid JSON.
			if txt := textContent(t, res); txt != "" {
				var v any
				if jsonErr := json.Unmarshal([]byte(txt), &v); jsonErr != nil {
					t.Fatalf("success result is not valid JSON for source %q: %v", source, jsonErr)
				}
			}
		}
	})
}
