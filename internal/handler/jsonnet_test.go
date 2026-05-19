/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-jsonnet"
)

type ctxCaptureHandler struct {
	mu       sync.Mutex
	contexts []context.Context
}

func (h *ctxCaptureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *ctxCaptureHandler) Handle(ctx context.Context, _ slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.contexts = append(h.contexts, ctx)
	return nil
}

func (h *ctxCaptureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *ctxCaptureHandler) WithGroup(_ string) slog.Handler      { return h }

func TestParseExtVars(t *testing.T) {
	tests := map[string]struct {
		environ []string
		want    map[string]string
	}{
		"empty input": {
			environ: nil,
			want:    map[string]string{},
		},
		"ignores non-prefixed entries": {
			environ: []string{"PATH=/usr/bin", "HOME=/home/x"},
			want:    map[string]string{},
		},
		"extracts prefixed entries": {
			environ: []string{"JAAS_EXT_VAR_foo=bar", "PATH=/usr/bin"},
			want:    map[string]string{"foo": "bar"},
		},
		"preserves equals signs in value": {
			environ: []string{"JAAS_EXT_VAR_token=a=b=c"},
			want:    map[string]string{"token": "a=b=c"},
		},
		"empty value allowed": {
			environ: []string{"JAAS_EXT_VAR_empty="},
			want:    map[string]string{"empty": ""},
		},
		"skips entries without equals": {
			environ: []string{"JAAS_EXT_VAR_malformed", "JAAS_EXT_VAR_ok=v"},
			want:    map[string]string{"ok": "v"},
		},
		"duplicate keys: last wins": {
			environ: []string{"JAAS_EXT_VAR_x=first", "JAAS_EXT_VAR_x=second"},
			want:    map[string]string{"x": "second"},
		},
		"prefix without trailing key name yields empty key": {
			environ: []string{"JAAS_EXT_VAR_=v"},
			want:    map[string]string{"": "v"},
		},
		"prefix with similar but distinct name rejected": {
			environ: []string{"JAAS_EXT_VAR=v", "JAAS_EXT_VARS_=v"},
			want:    map[string]string{},
		},
		"value preserves tabs": {
			environ: []string{"JAAS_EXT_VAR_t=\tindented\t"},
			want:    map[string]string{"t": "\tindented\t"},
		},
		"value preserves newlines": {
			environ: []string{"JAAS_EXT_VAR_multi=line1\nline2"},
			want:    map[string]string{"multi": "line1\nline2"},
		},
		"unicode key and value": {
			environ: []string{"JAAS_EXT_VAR_naïve=café"},
			want:    map[string]string{"naïve": "café"},
		},
		"value containing only whitespace": {
			environ: []string{"JAAS_EXT_VAR_blank=   "},
			want:    map[string]string{"blank": "   "},
		},
		"deeply prefixed name": {
			environ: []string{"JAAS_EXT_VAR_JAAS_EXT_VAR_x=double"},
			want:    map[string]string{"JAAS_EXT_VAR_x": "double"},
		},
		"mixed prefixed and unprefixed": {
			environ: []string{"PATH=/x", "JAAS_EXT_VAR_a=A", "FOO=bar", "JAAS_EXT_VAR_b=B"},
			want:    map[string]string{"a": "A", "b": "B"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := ParseExtVars(tc.environ)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestParseExtVars_DoesNotMutateInput(t *testing.T) {
	in := []string{"JAAS_EXT_VAR_x=y", "PATH=/usr/bin"}
	cp := append([]string{}, in...)
	_ = ParseExtVars(in)
	for i := range in {
		if in[i] != cp[i] {
			t.Errorf("input[%d] mutated: got %q, want %q", i, in[i], cp[i])
		}
	}
}

func TestParseExtVars_ReturnsNonNilOnEmpty(t *testing.T) {
	got := ParseExtVars(nil)
	if got == nil {
		t.Error("ParseExtVars(nil) = nil, want non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("ParseExtVars(nil) = %v, want empty map", got)
	}
}

func TestParseExtVars_ManyEntries(t *testing.T) {
	environ := make([]string, 0, 1000)
	for i := 0; i < 500; i++ {
		environ = append(environ, fmt.Sprintf("JAAS_EXT_VAR_k%d=v%d", i, i))
		environ = append(environ, fmt.Sprintf("OTHER_%d=ignored", i))
	}
	got := ParseExtVars(environ)
	if len(got) != 500 {
		t.Fatalf("len(got) = %d, want 500", len(got))
	}
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("k%d", i)
		want := fmt.Sprintf("v%d", i)
		if got[key] != want {
			t.Errorf("got[%q] = %q, want %q", key, got[key], want)
		}
	}
}

func TestParseExtVars_LongValue(t *testing.T) {
	long := strings.Repeat("x", 100000)
	got := ParseExtVars([]string{"JAAS_EXT_VAR_big=" + long})
	if got["big"] != long {
		t.Errorf("long value not preserved (got len %d, want %d)", len(got["big"]), len(long))
	}
}

func TestResolveSnippet(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello", "main.jsonnet"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("exact match in snippets list", func(t *testing.T) {
		got, ok := resolveSnippet("path/to/foo.jsonnet", []string{"path/to/foo.jsonnet"}, nil)
		if !ok || got != "path/to/foo.jsonnet" {
			t.Errorf("got %q ok=%v, want exact match", got, ok)
		}
	})

	t.Run("snippet directory match", func(t *testing.T) {
		got, ok := resolveSnippet("hello", nil, []string{dir})
		if !ok {
			t.Fatal("expected match")
		}
		want := dir + "/hello/main.jsonnet"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("first directory wins on multiple hits", func(t *testing.T) {
		other := t.TempDir()
		if err := os.MkdirAll(filepath.Join(other, "hello"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(other, "hello", "main.jsonnet"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := resolveSnippet("hello", nil, []string{dir, other})
		if !ok {
			t.Fatal("expected match")
		}
		if got != dir+"/hello/main.jsonnet" {
			t.Errorf("got %q, want first dir to win", got)
		}
	})

	t.Run("snippets list takes precedence over directories", func(t *testing.T) {
		got, ok := resolveSnippet("hello", []string{"hello"}, []string{dir})
		if !ok || got != "hello" {
			t.Errorf("got %q ok=%v, want exact match precedence", got, ok)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := resolveSnippet("missing", nil, []string{dir})
		if ok {
			t.Error("expected miss")
		}
	})
}

func TestResolveSnippet_RejectsPathTraversal(t *testing.T) {
	snippetDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(snippetDir, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snippetDir, "ok", "main.jsonnet"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret", "main.jsonnet"), []byte(`{"leaked": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	relEscape, err := filepath.Rel(snippetDir, filepath.Join(outside, "secret"))
	if err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"..",
		"../etc",
		"../../etc/passwd",
		"ok/../../etc",
		"/etc/passwd",
		filepath.Join(outside, "secret"),
		relEscape,
	}
	for _, name := range bad {
		t.Run("rejects "+name, func(t *testing.T) {
			if got, ok := resolveSnippet(name, nil, []string{snippetDir}); ok {
				t.Errorf("resolveSnippet(%q) = %q, true; want \"\", false", name, got)
			}
		})
	}

	t.Run("legitimate name still resolves", func(t *testing.T) {
		got, ok := resolveSnippet("ok", nil, []string{snippetDir})
		if !ok {
			t.Fatal("expected match")
		}
		want := filepath.Join(snippetDir, "ok", "main.jsonnet")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("legitimate nested name still resolves", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(snippetDir, "a", "b"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snippetDir, "a", "b", "main.jsonnet"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := resolveSnippet("a/b", nil, []string{snippetDir})
		if !ok {
			t.Fatal("expected match")
		}
		want := filepath.Join(snippetDir, "a", "b", "main.jsonnet")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestResolveSnippet_RejectsSymlinkEscape(t *testing.T) {
	snippetDir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "target", "main.jsonnet"), []byte(`{"leaked": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(snippetDir, "sneaky")); err != nil {
		t.Fatal(err)
	}

	if got, ok := resolveSnippet("sneaky", nil, []string{snippetDir}); ok {
		t.Errorf("resolveSnippet(\"sneaky\") = %q, true; want \"\", false (symlink escapes the snippet directory)", got)
	}
}

func TestResolveSnippet_EmptyNameNotFound(t *testing.T) {
	dir := t.TempDir()
	if _, ok := resolveSnippet("", nil, []string{dir}); ok {
		t.Error("resolveSnippet(\"\") returned ok=true; want false")
	}
}

func TestApplyTLAVars(t *testing.T) {
	dir := t.TempDir()
	snippet := filepath.Join(dir, "tla.jsonnet")
	if err := os.WriteFile(snippet, []byte(`function(s, n) { s: s, n: n }`), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		params  url.Values
		wantSub []string
	}{
		"single value becomes string TLA": {
			params:  url.Values{"s": {"hello"}, "n": {"single"}},
			wantSub: []string{`"s": "hello"`, `"n": "single"`},
		},
		"bare key (empty string value) becomes empty string TLA": {
			params:  url.Values{"s": {""}, "n": {""}},
			wantSub: []string{`"s": ""`, `"n": ""`},
		},
		"multi value becomes JSON-array TLA": {
			params:  url.Values{"s": {"only"}, "n": {"a", "b"}},
			wantSub: []string{`"s": "only"`, `"n":`, `"a"`, `"b"`},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			vm := jsonnet.MakeVM()
			if err := applyTLAVars(vm, tc.params); err != nil {
				t.Fatalf("applyTLAVars: %v", err)
			}
			out, err := vm.EvaluateFile(snippet)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(out, sub) {
					t.Errorf("output %q missing %q", out, sub)
				}
			}
		})
	}
}

func TestJsonnetHandler_SetsJSONContentTypeOnSuccess(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello", "main.jsonnet"), []byte(`{"ok": true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/hello", nil)
	req.SetPathValue("snippet", "hello")
	rr := httptest.NewRecorder()
	h(rr, req)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := rr.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), `"ok"`) {
		t.Errorf("body = %q, want JSON containing \"ok\"", string(body))
	}
}

func TestJsonnetHandler_BareQueryKeyBecomesEmptyTLA(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "echo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "echo", "main.jsonnet"), []byte(`function(v) { v: v }`), 0o644); err != nil {
		t.Fatal(err)
	}

	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/echo?v", nil)
	req.SetPathValue("snippet", "echo")
	rr := httptest.NewRecorder()
	h(rr, req)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d, body: %s", got, want, rr.Body.String())
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), `"v": ""`) {
		t.Errorf("body = %q, want it to contain `\"v\": \"\"`", string(body))
	}
}

func TestJsonnetHandler_LogsCarryRequestContext(t *testing.T) {
	captured := &ctxCaptureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(captured))
	t.Cleanup(func() { slog.SetDefault(prev) })

	type ctxKey struct{}
	const sentinel = "trace-12345"

	h := JsonnetHandler(Config{})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil).
		WithContext(context.WithValue(context.Background(), ctxKey{}, sentinel))
	rr := httptest.NewRecorder()
	h(rr, req)

	if got, want := rr.Code, http.StatusMethodNotAllowed; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if len(captured.contexts) == 0 {
		t.Fatal("expected at least one log record")
	}
	for i, ctx := range captured.contexts {
		if got, _ := ctx.Value(ctxKey{}).(string); got != sentinel {
			t.Errorf("log record %d: ctx value = %q, want %q", i, got, sentinel)
		}
	}
}

func TestJsonnetHandler_TraversalReturnsNotFound(t *testing.T) {
	snippetDir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret", "main.jsonnet"), []byte(`{"leaked": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	relEscape, err := filepath.Rel(snippetDir, filepath.Join(outside, "secret"))
	if err != nil {
		t.Fatal(err)
	}

	h := JsonnetHandler(Config{SnippetDirectories: []string{snippetDir}})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/x", nil)
	req.SetPathValue("snippet", relEscape)
	rr := httptest.NewRecorder()
	h(rr, req)

	if got, want := rr.Code, http.StatusNotFound; got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "leaked") {
		t.Errorf("body leaked outside file: %q", rr.Body.String())
	}
}

func TestJsonnetHandler_MethodNotAllowed(t *testing.T) {
	h := JsonnetHandler(Config{})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if got, want := rr.Code, http.StatusMethodNotAllowed; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}

func TestJsonnetHandler_NotFound(t *testing.T) {
	h := JsonnetHandler(Config{})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/missing", nil)
	req.SetPathValue("snippet", "missing")
	rr := httptest.NewRecorder()
	h(rr, req)
	if got, want := rr.Code, http.StatusNotFound; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}

func TestEvaluateWithDeadline_Success(t *testing.T) {
	out, err := evaluateWithDeadline(context.Background(), func() (string, error) {
		return "ok", nil
	}, time.Second)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out != "ok" {
		t.Errorf("out = %q, want %q", out, "ok")
	}
}

func TestEvaluateWithDeadline_PropagatesEvalError(t *testing.T) {
	sentinel := errors.New("eval failed")
	_, err := evaluateWithDeadline(context.Background(), func() (string, error) {
		return "", sentinel
	}, time.Second)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

func TestEvaluateWithDeadline_TimesOut(t *testing.T) {
	start := time.Now()
	_, err := evaluateWithDeadline(context.Background(), func() (string, error) {
		time.Sleep(2 * time.Second)
		return "late", nil
	}, 50*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("took %v, want well under the eval's 2s sleep", elapsed)
	}
}

func TestEvaluateWithDeadline_RespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := evaluateWithDeadline(ctx, func() (string, error) {
		time.Sleep(2 * time.Second)
		return "late", nil
	}, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestEvaluateWithDeadline_ZeroTimeoutMeansNoTimeout(t *testing.T) {
	out, err := evaluateWithDeadline(context.Background(), func() (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "ok", nil
	}, 0)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out != "ok" {
		t.Errorf("out = %q, want %q", out, "ok")
	}
}

func TestJsonnetHandler_TimeoutReturns504(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "slow"), 0o755); err != nil {
		t.Fatal(err)
	}
	snippet := `local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`
	if err := os.WriteFile(filepath.Join(dir, "slow", "main.jsonnet"), []byte(snippet), 0o644); err != nil {
		t.Fatal(err)
	}

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		EvaluationTimeout:  time.Microsecond,
		MaxStack:           10000,
	})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/slow", nil)
	req.SetPathValue("snippet", "slow")
	rr := httptest.NewRecorder()
	h(rr, req)

	if got, want := rr.Code, http.StatusGatewayTimeout; got != want {
		t.Errorf("status = %d, want %d (body: %s)", got, want, rr.Body.String())
	}
}

func TestJsonnetHandler_MaxStackLimitsRecursion(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	snippet := `local f(n) = if n == 0 then 0 else f(n-1) + 1; f(200)`
	if err := os.WriteFile(filepath.Join(dir, "deep", "main.jsonnet"), []byte(snippet), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("hits stack limit", func(t *testing.T) {
		h := JsonnetHandler(Config{
			SnippetDirectories: []string{dir},
			EvaluationTimeout:  10 * time.Second,
			MaxStack:           20,
		})
		req := httptest.NewRequest(http.MethodGet, "/jsonnet/deep", nil)
		req.SetPathValue("snippet", "deep")
		rr := httptest.NewRecorder()
		h(rr, req)
		if got, want := rr.Code, http.StatusBadRequest; got != want {
			t.Errorf("status = %d, want %d (body: %s)", got, want, rr.Body.String())
		}
	})

	t.Run("succeeds with generous stack", func(t *testing.T) {
		h := JsonnetHandler(Config{
			SnippetDirectories: []string{dir},
			EvaluationTimeout:  10 * time.Second,
			MaxStack:           1000,
		})
		req := httptest.NewRequest(http.MethodGet, "/jsonnet/deep", nil)
		req.SetPathValue("snippet", "deep")
		rr := httptest.NewRecorder()
		h(rr, req)
		if got, want := rr.Code, http.StatusOK; got != want {
			t.Errorf("status = %d, want %d (body: %s)", got, want, rr.Body.String())
		}
	})
}

// --- ExtVars: lift from per-request os.Environ() to startup-built map ---

// writeExtVarSnippet writes a snippet at <dir>/<name>/main.jsonnet that returns
// std.extVar("varName") so handler-level ExtVar wiring can be observed in the
// response body.
func writeExtVarSnippet(t *testing.T, dir, name, varName string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{ v: std.extVar(%q) }`, varName)
	if err := os.WriteFile(filepath.Join(dir, name, "main.jsonnet"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func callExtVarSnippet(t *testing.T, h http.HandlerFunc, snippet string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/"+snippet, nil)
	req.SetPathValue("snippet", snippet)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestJsonnetHandler_NilExtVarsWithSnippetThatDoesNotUseExtVar(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain", "main.jsonnet"), []byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}, ExtVars: nil})
	rr := callExtVarSnippet(t, h, "plain")
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestJsonnetHandler_EmptyExtVarsWithSnippetThatUsesExtVar(t *testing.T) {
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "needs", "missing")
	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}, ExtVars: map[string]string{}})
	rr := callExtVarSnippet(t, h, "needs")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (jsonnet should fail because extVar is undefined; body: %s)", rr.Code, rr.Body.String())
	}
}

func TestJsonnetHandler_SingleExtVarSurfacedToSnippet(t *testing.T) {
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "echo", "greeting")
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            map[string]string{"greeting": "hello"},
	})
	rr := callExtVarSnippet(t, h, "echo")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"hello"`) {
		t.Errorf("body = %q, want it to contain \"hello\"", rr.Body.String())
	}
}

func TestJsonnetHandler_ExtVarValuesPassedVerbatim(t *testing.T) {
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "echo", "v")

	tests := map[string]string{
		"empty string":   "",
		"numeric-like":   "123",
		"json-like":      `{"a":1}`,
		"json array":     `[1,2,3]`,
		"unicode":        "café 🚀",
		"quotes":         `she said "hi"`,
		"backslashes":    `C:\Users\x`,
		"newline":        "line1\nline2",
		"carriage":       "a\rb",
		"tabs":           "a\tb\tc",
		"whitespace":     "   ",
		"with equals":    "k=v=w",
		"long":           strings.Repeat("z", 10000),
		"unicode escape": "éclair",
		"emoji":          "🦀",
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			h := JsonnetHandler(Config{
				SnippetDirectories: []string{dir},
				ExtVars:            map[string]string{"v": value},
			})
			rr := callExtVarSnippet(t, h, "echo")
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
			}

			var payload struct {
				V string `json:"v"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body is not valid JSON: %v (body: %q)", err, rr.Body.String())
			}
			if payload.V != value {
				t.Errorf("payload.v = %q, want %q", payload.V, value)
			}
		})
	}
}

func TestJsonnetHandler_MultipleExtVarsInSameSnippet(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "both"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{ a: std.extVar("a"), b: std.extVar("b"), c: std.extVar("c") }`
	if err := os.WriteFile(filepath.Join(dir, "both", "main.jsonnet"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars: map[string]string{
			"a": "one",
			"b": "two",
			"c": "three",
		},
	})
	rr := callExtVarSnippet(t, h, "both")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	for _, expected := range []string{`"one"`, `"two"`, `"three"`} {
		if !strings.Contains(rr.Body.String(), expected) {
			t.Errorf("body = %q, want it to contain %s", rr.Body.String(), expected)
		}
	}
}

func TestJsonnetHandler_ExtVarsLiftedAtConstruction(t *testing.T) {
	// Confirms #9: the handler does not re-read os.Environ() per request.
	// We set JAAS_EXT_VAR_late AFTER constructing the handler and expect the
	// snippet's std.extVar("late") call to fail because the handler's ExtVars
	// were captured at construction time (and don't include "late").
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "late", "late")

	// Construct handler with explicit ExtVars (none).
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            map[string]string{},
	})

	// Now modify the process environment.
	t.Setenv("JAAS_EXT_VAR_late", "should-not-be-seen")

	rr := callExtVarSnippet(t, h, "late")
	if rr.Code == http.StatusOK {
		t.Errorf("status = 200, want non-2xx — handler must not consult os.Environ() per request (body: %s)", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "should-not-be-seen") {
		t.Errorf("body contains the late-set value %q; lift was not effective", rr.Body.String())
	}
}

func TestJsonnetHandler_ExtVarsIsolatedAcrossHandlers(t *testing.T) {
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "echo", "v")

	h1 := JsonnetHandler(Config{SnippetDirectories: []string{dir}, ExtVars: map[string]string{"v": "ONE"}})
	h2 := JsonnetHandler(Config{SnippetDirectories: []string{dir}, ExtVars: map[string]string{"v": "TWO"}})

	rr1 := callExtVarSnippet(t, h1, "echo")
	rr2 := callExtVarSnippet(t, h2, "echo")

	if !strings.Contains(rr1.Body.String(), `"ONE"`) {
		t.Errorf("h1 body = %q, want \"ONE\"", rr1.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), `"TWO"`) {
		t.Errorf("h2 body = %q, want \"TWO\"", rr2.Body.String())
	}
}

func TestJsonnetHandler_ExtVarsStableAcrossMultipleRequests(t *testing.T) {
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "echo", "v")

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            map[string]string{"v": "fixed"},
	})

	for i := 0; i < 25; i++ {
		rr := callExtVarSnippet(t, h, "echo")
		if rr.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `"fixed"`) {
			t.Errorf("call %d: body = %q, want \"fixed\"", i, rr.Body.String())
		}
	}
}

func TestJsonnetHandler_ExtVarsManyKeysAllExposed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "all"), 0o755); err != nil {
		t.Fatal(err)
	}

	const N = 50
	vars := make(map[string]string, N)
	for i := 0; i < N; i++ {
		vars[fmt.Sprintf("k%d", i)] = fmt.Sprintf("v%d", i)
	}

	// Build a snippet that emits {k0: extVar("k0"), k1: extVar("k1"), …}
	var b strings.Builder
	b.WriteString("{ ")
	for i := 0; i < N; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "k%d: std.extVar(\"k%d\")", i, i)
	}
	b.WriteString(" }")
	if err := os.WriteFile(filepath.Join(dir, "all", "main.jsonnet"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            vars,
	})

	rr := callExtVarSnippet(t, h, "all")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	var payload map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not valid JSON: %v (body: %q)", err, rr.Body.String())
	}
	for k, v := range vars {
		if payload[k] != v {
			t.Errorf("payload[%q] = %q, want %q", k, payload[k], v)
		}
	}
}

func TestJsonnetHandler_ExtVarKeyWithUnderscoresAndDigits(t *testing.T) {
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "weird", "my_ext_var_123")
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            map[string]string{"my_ext_var_123": "weird-name-ok"},
	})
	rr := callExtVarSnippet(t, h, "weird")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"weird-name-ok"`) {
		t.Errorf("body = %q, want \"weird-name-ok\"", rr.Body.String())
	}
}

func TestJsonnetHandler_ExtVarFromParseExtVars_EndToEnd(t *testing.T) {
	// Demonstrates the production wiring: ParseExtVars(env) → Config.ExtVars
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "echo", "v")

	environ := []string{
		"PATH=/usr/bin",
		"JAAS_EXT_VAR_v=from-environ",
		"HOME=/home/x",
	}
	vars := ParseExtVars(environ)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            vars,
	})
	rr := callExtVarSnippet(t, h, "echo")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"from-environ"`) {
		t.Errorf("body = %q, want \"from-environ\"", rr.Body.String())
	}
}

func TestJsonnetHandler_ExtVarUsedTwiceInSnippet(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "twice"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `local v = std.extVar("v"); { a: v, b: v, c: v + v }`
	if err := os.WriteFile(filepath.Join(dir, "twice", "main.jsonnet"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            map[string]string{"v": "X"},
	})
	rr := callExtVarSnippet(t, h, "twice")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	for _, expected := range []string{`"a": "X"`, `"b": "X"`, `"c": "XX"`} {
		if !strings.Contains(rr.Body.String(), expected) {
			t.Errorf("body = %q, want it to contain %s", rr.Body.String(), expected)
		}
	}
}

func TestJsonnetHandler_NoExtVarSlogPerRequestDebug(t *testing.T) {
	// Confirms that the per-request debug log for ExtVars has been removed.
	// We install a capturing slog handler at debug level, then issue a request
	// against a snippet whose ExtVars map contains a unique sentinel value.
	// The sentinel must NOT appear in any captured log record.
	captured := &debugCaptureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(captured))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "echo", "v")

	const sentinel = "DO-NOT-LOG-THIS-VALUE-7f3c"
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            map[string]string{"v": sentinel},
	})
	rr := callExtVarSnippet(t, h, "echo")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	captured.mu.Lock()
	defer captured.mu.Unlock()
	for _, msg := range captured.messages {
		if strings.Contains(msg, sentinel) {
			t.Errorf("found ExtVar value in slog output (per-request log not removed): %q", msg)
		}
	}
}

type debugCaptureHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *debugCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *debugCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	h.mu.Lock()
	h.messages = append(h.messages, b.String())
	h.mu.Unlock()
	return nil
}

func (h *debugCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *debugCaptureHandler) WithGroup(string) slog.Handler      { return h }
