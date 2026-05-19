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
	"slices"
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

func TestResolveSnippet_NonExistentDirectoryIsSkipped(t *testing.T) {
	// First directory doesn't exist (os.OpenRoot returns ENOENT); resolveSnippet
	// must skip it and find the snippet in the second, working directory.
	real := t.TempDir()
	writeSnippet(t, real, "ok", `{}`)

	got, ok := resolveSnippet("ok", nil, []string{"/this/path/does/not/exist", real})
	if !ok {
		t.Fatal("expected match in the second directory")
	}
	want := filepath.Join(real, "ok", "main.jsonnet")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveSnippet_AllDirectoriesNonExistentReturnsNotFound(t *testing.T) {
	if _, ok := resolveSnippet("anything", nil, []string{"/nope/one", "/nope/two"}); ok {
		t.Error("expected not-found when all snippet directories are missing")
	}
}

func TestResolveSnippet_EmptyNameNotFound(t *testing.T) {
	dir := t.TempDir()
	if _, ok := resolveSnippet("", nil, []string{dir}); ok {
		t.Error("resolveSnippet(\"\") returned ok=true; want false")
	}
}

func TestApplyTLAVars(t *testing.T) {
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
			applyTLAVars(vm, tc.params)
			out, err := vm.EvaluateAnonymousSnippet("tla.jsonnet", `function(s, n) { s: s, n: n }`)
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
	type ctxKey struct{}
	const sentinel = "trace-12345"

	h := JsonnetHandler(Config{Logger: slog.New(captured)})
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
	// We install a capturing slog handler at debug level via Config.Logger,
	// then issue a request against a snippet whose ExtVars map contains a
	// unique sentinel value. The sentinel must NOT appear in any captured
	// log record.
	captured := &debugCaptureHandler{}

	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "echo", "v")

	const sentinel = "DO-NOT-LOG-THIS-VALUE-7f3c"
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            map[string]string{"v": sentinel},
		Logger:             slog.New(captured),
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

// --- FileImporter is freshly allocated per request: race-free + cache-free ---

func writeLibrary(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "main.libsonnet"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSnippet(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "main.jsonnet"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestJsonnetHandler_ConcurrentRequestsAreRaceFree(t *testing.T) {
	dir := t.TempDir()
	writeExtVarSnippet(t, dir, "echo", "v")
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		ExtVars:            map[string]string{"v": "shared"},
	})

	var wg sync.WaitGroup
	const workers = 100
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := callExtVarSnippet(t, h, "echo")
			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rr.Code)
				return
			}
			if !strings.Contains(rr.Body.String(), `"shared"`) {
				t.Errorf("body = %q, want \"shared\"", rr.Body.String())
			}
		}()
	}
	wg.Wait()
}

func TestJsonnetHandler_LibraryImport_Basic(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "mylib", `{ greeting: "hi from lib" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local lib = import 'mylib/main.libsonnet'; { msg: lib.greeting }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})
	rr := callExtVarSnippet(t, h, "use")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hi from lib") {
		t.Errorf("body = %q, want 'hi from lib'", rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_RaceFreeUnderLoad(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "mylib", `{ greeting: "race-free" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local lib = import 'mylib/main.libsonnet'; { msg: lib.greeting }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})

	var wg sync.WaitGroup
	const workers = 100
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := callExtVarSnippet(t, h, "use")
			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
				return
			}
			if !strings.Contains(rr.Body.String(), "race-free") {
				t.Errorf("body = %q, want 'race-free'", rr.Body.String())
			}
		}()
	}
	wg.Wait()
}

func TestJsonnetHandler_LibraryImport_RightmostPathWins(t *testing.T) {
	libLeft := t.TempDir()
	libRight := t.TempDir()
	writeLibrary(t, libLeft, "shared", `{ v: "from-left" }`)
	writeLibrary(t, libRight, "shared", `{ v: "from-right" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local s = import 'shared/main.libsonnet'; { which: s.v }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libLeft, libRight},
	})
	rr := callExtVarSnippet(t, h, "use")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "from-right") {
		t.Errorf("body = %q, want 'from-right' (rightmost path)", rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_MissingLibraryReturns400(t *testing.T) {
	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local x = import 'doesnotexist/lib.libsonnet'; { a: x }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{},
	})
	rr := callExtVarSnippet(t, h, "use")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_ChangesVisibleImmediately(t *testing.T) {
	// Side effect of constructing a fresh FileImporter per request.
	libDir := t.TempDir()
	libPath := filepath.Join(libDir, "mylib", "main.libsonnet")
	writeLibrary(t, libDir, "mylib", `{ v: "v1" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local lib = import 'mylib/main.libsonnet'; { r: lib.v }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})

	rr := callExtVarSnippet(t, h, "use")
	if !strings.Contains(rr.Body.String(), `"v1"`) {
		t.Errorf("phase 1: body = %q, want it to contain \"v1\"", rr.Body.String())
	}

	if err := os.WriteFile(libPath, []byte(`{ v: "v2" }`), 0o644); err != nil {
		t.Fatal(err)
	}

	rr = callExtVarSnippet(t, h, "use")
	if !strings.Contains(rr.Body.String(), `"v2"`) {
		t.Errorf("phase 2: body = %q, want it to contain \"v2\"", rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_NoLibraryPaths(t *testing.T) {
	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "plain", `{ ok: true }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       nil,
	})
	rr := callExtVarSnippet(t, h, "plain")
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_EmptyLibraryPathsSlice(t *testing.T) {
	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "plain", `{ ok: true }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{},
	})
	rr := callExtVarSnippet(t, h, "plain")
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_Subpath(t *testing.T) {
	libDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(libDir, "lib", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "lib", "sub", "thing.libsonnet"), []byte(`{ s: "nested" }`), 0o644); err != nil {
		t.Fatal(err)
	}

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local x = import 'lib/sub/thing.libsonnet'; { msg: x.s }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})
	rr := callExtVarSnippet(t, h, "use")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "nested") {
		t.Errorf("body = %q, want 'nested'", rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_Transitive(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "a", `local b = import 'b/main.libsonnet'; { result: b.message }`)
	writeLibrary(t, libDir, "b", `{ message: "transitive" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local a = import 'a/main.libsonnet'; { final: a.result }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})
	rr := callExtVarSnippet(t, h, "use")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "transitive") {
		t.Errorf("body = %q, want 'transitive'", rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_PerHandlerIsolation(t *testing.T) {
	libA := t.TempDir()
	libB := t.TempDir()
	writeLibrary(t, libA, "common", `{ from: "handler-A" }`)
	writeLibrary(t, libB, "common", `{ from: "handler-B" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local c = import 'common/main.libsonnet'; { who: c.from }`)

	hA := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libA},
	})
	hB := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libB},
	})

	rrA := callExtVarSnippet(t, hA, "use")
	rrB := callExtVarSnippet(t, hB, "use")

	if !strings.Contains(rrA.Body.String(), "handler-A") {
		t.Errorf("hA body = %q, want 'handler-A'", rrA.Body.String())
	}
	if !strings.Contains(rrB.Body.String(), "handler-B") {
		t.Errorf("hB body = %q, want 'handler-B'", rrB.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_NonExistentPathDoesNotFailSnippetsWithoutImports(t *testing.T) {
	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "plain", `{ ok: true }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{"/this/path/does/not/exist"},
	})
	rr := callExtVarSnippet(t, h, "plain")
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (snippet has no imports; missing lib path must not break it; body: %s)", rr.Code, rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_UsesStdFunctions(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "utils", `{ greeting(name): "hello, " + std.asciiUpper(name) }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local u = import 'utils/main.libsonnet'; { msg: u.greeting("world") }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})
	rr := callExtVarSnippet(t, h, "use")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hello, WORLD") {
		t.Errorf("body = %q, want 'hello, WORLD'", rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_LibraryReadsExtVar(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "envread", `{ from_env: std.extVar("region") }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local e = import 'envread/main.libsonnet'; { region: e.from_env }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
		ExtVars:            map[string]string{"region": "us-east-1"},
	})
	rr := callExtVarSnippet(t, h, "use")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "us-east-1") {
		t.Errorf("body = %q, want 'us-east-1'", rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_MultipleImportsInOneSnippet(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "alpha", `{ v: "A" }`)
	writeLibrary(t, libDir, "beta", `{ v: "B" }`)
	writeLibrary(t, libDir, "gamma", `{ v: "C" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "all", `
		local a = import 'alpha/main.libsonnet';
		local b = import 'beta/main.libsonnet';
		local g = import 'gamma/main.libsonnet';
		{ a: a.v, b: b.v, g: g.v }
	`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})
	rr := callExtVarSnippet(t, h, "all")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	for _, want := range []string{`"A"`, `"B"`, `"C"`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("body = %q, want it to contain %s", rr.Body.String(), want)
		}
	}
}

func TestJsonnetHandler_LibraryImport_SameLibraryImportedTwice(t *testing.T) {
	// Within a single VM evaluation, jsonnet's per-VM cache makes this efficient.
	// We just need to assert the values come out right.
	libDir := t.TempDir()
	writeLibrary(t, libDir, "x", `{ n: 42 }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "twice", `
		local a = import 'x/main.libsonnet';
		local b = import 'x/main.libsonnet';
		{ a: a.n, b: b.n, sum: a.n + b.n }
	`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})
	rr := callExtVarSnippet(t, h, "twice")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	for _, want := range []string{`"a": 42`, `"b": 42`, `"sum": 84`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("body = %q, want it to contain %s", rr.Body.String(), want)
		}
	}
}

func TestJsonnetHandler_LibraryImport_InvalidLibrarySyntaxReturns400(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "broken", `{ this is not valid jsonnet`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local b = import 'broken/main.libsonnet'; { a: b }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})
	rr := callExtVarSnippet(t, h, "use")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_CombinedWithTLAsAndExtVars(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "fmt", `{ join(a, b, sep): a + sep + b }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "compose",
		`function(prefix) local f = import 'fmt/main.libsonnet'; { msg: f.join(prefix, std.extVar("suffix"), "-") }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
		ExtVars:            map[string]string{"suffix": "tail"},
	})

	req := httptest.NewRequest(http.MethodGet, "/jsonnet/compose?prefix=head", nil)
	req.SetPathValue("snippet", "compose")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "head-tail") {
		t.Errorf("body = %q, want 'head-tail'", rr.Body.String())
	}
}

func TestJsonnetHandler_LibraryImport_HighConcurrencyStress(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "shared", `{ stamp: "stress-ok" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "use", `local s = import 'shared/main.libsonnet'; { stamp: s.stamp }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})

	var wg sync.WaitGroup
	const workers = 500
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := callExtVarSnippet(t, h, "use")
			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rr.Code)
				return
			}
			if !strings.Contains(rr.Body.String(), "stress-ok") {
				t.Errorf("body = %q, want 'stress-ok'", rr.Body.String())
			}
		}()
	}
	wg.Wait()
}

func TestJsonnetHandler_LibraryImport_ConcurrentMixedSnippets(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "lib", `{ v: 1 }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "one", `local l = import 'lib/main.libsonnet'; { name: "one", v: l.v }`)
	writeSnippet(t, snipDir, "two", `local l = import 'lib/main.libsonnet'; { name: "two", v: l.v + 1 }`)
	writeSnippet(t, snipDir, "three", `local l = import 'lib/main.libsonnet'; { name: "three", v: l.v + 2 }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})

	cases := map[string]string{
		"one":   `"name": "one"`,
		"two":   `"name": "two"`,
		"three": `"name": "three"`,
	}

	var wg sync.WaitGroup
	for snippet, want := range cases {
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func(snippet, want string) {
				defer wg.Done()
				rr := callExtVarSnippet(t, h, snippet)
				if rr.Code != http.StatusOK {
					t.Errorf("snippet=%s: status = %d, want 200", snippet, rr.Code)
					return
				}
				if !strings.Contains(rr.Body.String(), want) {
					t.Errorf("snippet=%s: body = %q, want substring %s", snippet, rr.Body.String(), want)
				}
			}(snippet, want)
		}
	}
	wg.Wait()
}

func TestJsonnetHandler_LibraryImport_ConcurrentWithExtVarsAndTLAs(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "lib", `{ build(prefix, env, query): prefix + ":" + env + ":" + query }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "compose",
		`function(q) local l = import 'lib/main.libsonnet'; { out: l.build("p", std.extVar("e"), q) }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
		ExtVars:            map[string]string{"e": "production"},
	})

	var wg sync.WaitGroup
	const workers = 50
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := fmt.Sprintf("query%d", i)
			req := httptest.NewRequest(http.MethodGet, "/jsonnet/compose?q="+q, nil)
			req.SetPathValue("snippet", "compose")
			rr := httptest.NewRecorder()
			h(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("worker %d: status = %d, want 200 (body: %s)", i, rr.Code, rr.Body.String())
				return
			}
			want := fmt.Sprintf(`"out": "p:production:%s"`, q)
			if !strings.Contains(rr.Body.String(), want) {
				t.Errorf("worker %d: body = %q, want substring %q", i, rr.Body.String(), want)
			}
		}(i)
	}
	wg.Wait()
}

// --- Logger injection (item #13) ---

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]slog.Value
	ctx   context.Context
}

type recordCapture struct {
	mu       sync.Mutex
	level    slog.Level
	useLevel bool
	records  []capturedRecord
}

func newRecordCapture() *recordCapture { return &recordCapture{} }

func newRecordCaptureAtLevel(l slog.Level) *recordCapture {
	return &recordCapture{level: l, useLevel: true}
}

func (h *recordCapture) Enabled(_ context.Context, l slog.Level) bool {
	if h.useLevel {
		return l >= h.level
	}
	return true
}

func (h *recordCapture) Handle(ctx context.Context, r slog.Record) error {
	rec := capturedRecord{
		level: r.Level,
		msg:   r.Message,
		attrs: map[string]slog.Value{},
		ctx:   ctx,
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *recordCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordCaptureChild{base: h, attrs: attrs}
}
func (h *recordCapture) WithGroup(_ string) slog.Handler { return h }

type recordCaptureChild struct {
	base  *recordCapture
	attrs []slog.Attr
}

func (c *recordCaptureChild) Enabled(ctx context.Context, l slog.Level) bool {
	return c.base.Enabled(ctx, l)
}

func (c *recordCaptureChild) Handle(ctx context.Context, r slog.Record) error {
	for _, a := range c.attrs {
		r.AddAttrs(a)
	}
	return c.base.Handle(ctx, r)
}

func (c *recordCaptureChild) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := append([]slog.Attr{}, c.attrs...)
	merged = append(merged, attrs...)
	return &recordCaptureChild{base: c.base, attrs: merged}
}

func (c *recordCaptureChild) WithGroup(_ string) slog.Handler { return c }

func (h *recordCapture) snapshot() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedRecord, len(h.records))
	copy(out, h.records)
	return out
}

func (h *recordCapture) findMessage(msg string) (capturedRecord, bool) {
	for _, r := range h.snapshot() {
		if r.msg == msg {
			return r, true
		}
	}
	return capturedRecord{}, false
}

func (h *recordCapture) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// --- injection semantics ---

func TestJsonnetHandler_Logger_InjectedLoggerReceivesRecords(t *testing.T) {
	cap := newRecordCapture()
	h := JsonnetHandler(Config{Logger: slog.New(cap)})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if cap.count() == 0 {
		t.Error("injected logger received zero records; expected at least one")
	}
}

func TestJsonnetHandler_Logger_NilFallsBackToDefault(t *testing.T) {
	cap := newRecordCapture()
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := JsonnetHandler(Config{Logger: nil})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	if cap.count() == 0 {
		t.Error("default logger received zero records; nil-fallback did not engage")
	}
}

func TestJsonnetHandler_Logger_DoesNotPanicOnNil(t *testing.T) {
	h := JsonnetHandler(Config{Logger: nil})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/missing", nil)
	req.SetPathValue("snippet", "missing")
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestJsonnetHandler_Logger_InjectedLoggerOverridesGlobal(t *testing.T) {
	global := newRecordCapture()
	injected := newRecordCapture()
	prev := slog.Default()
	slog.SetDefault(slog.New(global))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := JsonnetHandler(Config{Logger: slog.New(injected)})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	if injected.count() == 0 {
		t.Error("injected logger should have received records")
	}
	if global.count() != 0 {
		t.Errorf("global logger received %d records; injection should have bypassed it", global.count())
	}
}

func TestJsonnetHandler_Logger_PerHandlerIsolation(t *testing.T) {
	capA := newRecordCapture()
	capB := newRecordCapture()
	hA := JsonnetHandler(Config{Logger: slog.New(capA)})
	hB := JsonnetHandler(Config{Logger: slog.New(capB)})

	// Fire on A only.
	hA(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil))
	if capA.count() == 0 {
		t.Error("logger A received no records after A request")
	}
	if capB.count() != 0 {
		t.Errorf("logger B received %d records from handler A's request; loggers should be isolated", capB.count())
	}

	// Fire on B only; A's count should not change.
	priorA := capA.count()
	hB(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil))
	if capB.count() == 0 {
		t.Error("logger B received no records after B request")
	}
	if capA.count() != priorA {
		t.Errorf("logger A count moved from %d to %d after a B request; isolation broken", priorA, capA.count())
	}
}

func TestJsonnetHandler_Logger_StableAcrossMultipleRequests(t *testing.T) {
	cap := newRecordCapture()
	h := JsonnetHandler(Config{Logger: slog.New(cap)})
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
		rr := httptest.NewRecorder()
		h(rr, req)
	}
	if cap.count() < 10 {
		t.Errorf("count = %d, want >= 10 (one record per request, all on the same logger)", cap.count())
	}
}

func TestJsonnetHandler_Logger_LevelFilterDropsDebugLogs(t *testing.T) {
	// Logger gated at Error level; the success-path Debug logs should be dropped
	// and the lone Error log should still pass through (we trigger one with POST).
	cap := newRecordCaptureAtLevel(slog.LevelError)
	h := JsonnetHandler(Config{Logger: slog.New(cap)})

	// POST → method-not-allowed Error log.
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	for _, r := range cap.snapshot() {
		if r.level < slog.LevelError {
			t.Errorf("record at level %v leaked through Error-only filter: msg=%q", r.level, r.msg)
		}
	}
	if cap.count() == 0 {
		t.Error("expected at least one Error record to pass through")
	}
}

func TestJsonnetHandler_Logger_AcceptsLoggerWithPresetAttrs(t *testing.T) {
	cap := newRecordCapture()
	logger := slog.New(cap).With(slog.String("service", "jaas"), slog.String("env", "test"))
	h := JsonnetHandler(Config{Logger: logger})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	if cap.count() == 0 {
		t.Fatal("no records captured")
	}
	for _, r := range cap.snapshot() {
		if v, ok := r.attrs["service"]; !ok || v.String() != "jaas" {
			t.Errorf("record %q missing preset attr service=jaas; attrs=%v", r.msg, r.attrs)
		}
		if v, ok := r.attrs["env"]; !ok || v.String() != "test" {
			t.Errorf("record %q missing preset attr env=test; attrs=%v", r.msg, r.attrs)
		}
	}
}

// --- per-log-site coverage ---

func TestJsonnetHandler_Logger_LogsMethodNotAllowed(t *testing.T) {
	cap := newRecordCapture()
	h := JsonnetHandler(Config{Logger: slog.New(cap)})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	rec, ok := cap.findMessage("Unsupported HTTP method used")
	if !ok {
		t.Fatalf("expected record %q; got messages: %v", "Unsupported HTTP method used", messages(cap))
	}
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want Error", rec.level)
	}
	if v, ok := rec.attrs["method"]; !ok || v.String() != "POST" {
		t.Errorf("attrs missing method=POST; got %v", rec.attrs)
	}
}

func TestJsonnetHandler_Logger_LogsSnippetNotFound(t *testing.T) {
	cap := newRecordCapture()
	h := JsonnetHandler(Config{Logger: slog.New(cap)})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/missing", nil)
	req.SetPathValue("snippet", "missing")
	rr := httptest.NewRecorder()
	h(rr, req)

	rec, ok := cap.findMessage("Snippet not found")
	if !ok {
		t.Fatalf("expected record %q; got messages: %v", "Snippet not found", messages(cap))
	}
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want Error", rec.level)
	}
	if v, ok := rec.attrs["snippet-name"]; !ok || v.String() != "missing" {
		t.Errorf("attrs missing snippet-name=missing; got %v", rec.attrs)
	}
}

func TestJsonnetHandler_Logger_LogsExtractedSnippetNameAtDebug(t *testing.T) {
	cap := newRecordCapture()
	h := JsonnetHandler(Config{Logger: slog.New(cap)})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/anything", nil)
	req.SetPathValue("snippet", "anything")
	rr := httptest.NewRecorder()
	h(rr, req)

	rec, ok := cap.findMessage("Extracted snippet name")
	if !ok {
		t.Fatalf("expected record %q; got messages: %v", "Extracted snippet name", messages(cap))
	}
	if rec.level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug", rec.level)
	}
	if v, ok := rec.attrs["snippet-name"]; !ok || v.String() != "anything" {
		t.Errorf("attrs missing snippet-name=anything; got %v", rec.attrs)
	}
}

func TestJsonnetHandler_Logger_LogsResolvedSnippetOnSuccess(t *testing.T) {
	cap := newRecordCapture()
	dir := t.TempDir()
	writeSnippet(t, dir, "hello", `{ok:true}`)
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		Logger:             slog.New(cap),
	})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/hello", nil)
	req.SetPathValue("snippet", "hello")
	rr := httptest.NewRecorder()
	h(rr, req)

	rec, ok := cap.findMessage("Resolved snippet")
	if !ok {
		t.Fatalf("expected record %q; got messages: %v", "Resolved snippet", messages(cap))
	}
	if rec.level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug", rec.level)
	}
	if v, ok := rec.attrs["snippet-name"]; !ok || v.String() != "hello" {
		t.Errorf("attrs missing snippet-name=hello; got %v", rec.attrs)
	}
	if _, ok := rec.attrs["file-name"]; !ok {
		t.Errorf("attrs missing file-name; got %v", rec.attrs)
	}
}

func TestJsonnetHandler_Logger_LogsExtractedQueryParams(t *testing.T) {
	cap := newRecordCapture()
	dir := t.TempDir()
	writeSnippet(t, dir, "hello", `{ok:true}`)
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		Logger:             slog.New(cap),
	})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/hello?foo=bar", nil)
	req.SetPathValue("snippet", "hello")
	rr := httptest.NewRecorder()
	h(rr, req)

	rec, ok := cap.findMessage("Extracted query parameters")
	if !ok {
		t.Fatalf("expected record %q; got messages: %v", "Extracted query parameters", messages(cap))
	}
	if rec.level != slog.LevelDebug {
		t.Errorf("level = %v, want Debug", rec.level)
	}
}

func TestJsonnetHandler_Logger_LogsEvaluationTimeout(t *testing.T) {
	cap := newRecordCapture()
	dir := t.TempDir()
	writeSnippet(t, dir, "slow", `local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`)
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		EvaluationTimeout:  time.Microsecond,
		MaxStack:           10000,
		Logger:             slog.New(cap),
	})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/slow", nil)
	req.SetPathValue("snippet", "slow")
	rr := httptest.NewRecorder()
	h(rr, req)

	rec, ok := cap.findMessage("Jsonnet evaluation timed out")
	if !ok {
		t.Fatalf("expected record %q; got messages: %v", "Jsonnet evaluation timed out", messages(cap))
	}
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want Error", rec.level)
	}
	if _, ok := rec.attrs["timeout"]; !ok {
		t.Errorf("attrs missing timeout; got %v", rec.attrs)
	}
	if _, ok := rec.attrs["file-name"]; !ok {
		t.Errorf("attrs missing file-name; got %v", rec.attrs)
	}
}

func TestJsonnetHandler_Logger_LogsEvaluationCancelled(t *testing.T) {
	cap := newRecordCapture()
	dir := t.TempDir()
	writeSnippet(t, dir, "slow", `local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`)
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		MaxStack:           10000,
		Logger:             slog.New(cap),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so evaluation is cancelled immediately
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/slow", nil).WithContext(ctx)
	req.SetPathValue("snippet", "slow")
	rr := httptest.NewRecorder()
	h(rr, req)

	rec, ok := cap.findMessage("Jsonnet evaluation cancelled by caller")
	if !ok {
		t.Fatalf("expected record %q; got messages: %v", "Jsonnet evaluation cancelled by caller", messages(cap))
	}
	if rec.level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", rec.level)
	}
}

func TestJsonnetHandler_Logger_LogsCannotEvaluateOnSyntaxError(t *testing.T) {
	cap := newRecordCapture()
	dir := t.TempDir()
	writeSnippet(t, dir, "broken", `{ this is not valid jsonnet`)
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		Logger:             slog.New(cap),
	})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/broken", nil)
	req.SetPathValue("snippet", "broken")
	rr := httptest.NewRecorder()
	h(rr, req)

	rec, ok := cap.findMessage("Cannot evaluate Jsonnet")
	if !ok {
		t.Fatalf("expected record %q; got messages: %v", "Cannot evaluate Jsonnet", messages(cap))
	}
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want Error", rec.level)
	}
	if _, ok := rec.attrs["error"]; !ok {
		t.Errorf("attrs missing error; got %v", rec.attrs)
	}
}

func TestJsonnetHandler_Logger_RecordsCarryRequestContext(t *testing.T) {
	// Same idea as the older ctxCaptureHandler test, but goes through the
	// injected-logger path and asserts the request context arrives on every
	// captured record across multiple log call sites.
	type ctxKey struct{}
	const sentinel = "trace-injected-7c2"

	cap := newRecordCapture()
	dir := t.TempDir()
	writeSnippet(t, dir, "hello", `{ok:true}`)
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		Logger:             slog.New(cap),
	})

	ctx := context.WithValue(context.Background(), ctxKey{}, sentinel)
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/hello", nil).WithContext(ctx)
	req.SetPathValue("snippet", "hello")
	rr := httptest.NewRecorder()
	h(rr, req)

	if cap.count() == 0 {
		t.Fatal("no records captured")
	}
	for i, r := range cap.snapshot() {
		got, _ := r.ctx.Value(ctxKey{}).(string)
		if got != sentinel {
			t.Errorf("record %d (%q): ctx value = %q, want %q", i, r.msg, got, sentinel)
		}
	}
}

// --- success-path debug record coverage ---

func TestJsonnetHandler_Logger_SuccessPathEmitsExpectedDebugRecords(t *testing.T) {
	cap := newRecordCapture()
	dir := t.TempDir()
	writeSnippet(t, dir, "hello", `{ok:true}`)
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		Logger:             slog.New(cap),
	})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/hello", nil)
	req.SetPathValue("snippet", "hello")
	rr := httptest.NewRecorder()
	h(rr, req)

	expected := []string{
		"Extracted snippet name",
		"Resolved snippet",
		"Extracted query parameters",
	}
	for _, msg := range expected {
		if _, ok := cap.findMessage(msg); !ok {
			t.Errorf("expected record %q on success path; got messages: %v", msg, messages(cap))
		}
	}
}

func TestJsonnetHandler_Logger_LevelsAreDistinct(t *testing.T) {
	// A single test that hits Error (POST → 405), Debug (Extracted snippet name)
	// and confirms each surfaces with the right level on its own request.
	cap := newRecordCapture()
	h := JsonnetHandler(Config{Logger: slog.New(cap)})

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil))

	rec, ok := cap.findMessage("Unsupported HTTP method used")
	if !ok {
		t.Fatalf("expected Error record; got %v", messages(cap))
	}
	if rec.level != slog.LevelError {
		t.Errorf("Error path: level = %v, want Error", rec.level)
	}

	// reset captures
	cap.mu.Lock()
	cap.records = nil
	cap.mu.Unlock()

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/anything", nil)
	req.SetPathValue("snippet", "anything")
	h(rr, req)
	rec, ok = cap.findMessage("Extracted snippet name")
	if !ok {
		t.Fatalf("expected Debug record; got %v", messages(cap))
	}
	if rec.level != slog.LevelDebug {
		t.Errorf("Debug path: level = %v, want Debug", rec.level)
	}
}

// --- concurrency ---

func TestJsonnetHandler_Logger_ConcurrentRequestsAllCaptured(t *testing.T) {
	cap := newRecordCapture()
	h := JsonnetHandler(Config{Logger: slog.New(cap)})

	var wg sync.WaitGroup
	const workers = 100
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			h(rr, httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil))
		}()
	}
	wg.Wait()

	if cap.count() < workers {
		t.Errorf("captured %d records from %d concurrent POSTs; want >= %d", cap.count(), workers, workers)
	}
}

func TestJsonnetHandler_Logger_HandlerWithoutLoggerDoesNotMutateDefaultLoggerOutsideCall(t *testing.T) {
	// Two consecutive constructions of JsonnetHandler with Config{} must not
	// reset slog.Default between calls.
	mark := newRecordCapture()
	prev := slog.Default()
	slog.SetDefault(slog.New(mark))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_ = JsonnetHandler(Config{})
	if mark.count() != 0 {
		t.Errorf("construction emitted %d log records; constructor should not log", mark.count())
	}
	_ = JsonnetHandler(Config{})
	if mark.count() != 0 {
		t.Errorf("second construction emitted %d records; constructor must be log-silent", mark.count())
	}
}

func messages(cap *recordCapture) []string {
	out := make([]string, 0, cap.count())
	for _, r := range cap.snapshot() {
		out = append(out, r.msg)
	}
	return out
}

func TestJsonnetHandler_LibraryImport_LibraryReadsTLA(t *testing.T) {
	libDir := t.TempDir()
	writeLibrary(t, libDir, "f", `{ wrap(s): "[" + s + "]" }`)

	snipDir := t.TempDir()
	writeSnippet(t, snipDir, "wrap",
		`function(s) local f = import 'f/main.libsonnet'; { out: f.wrap(s) }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{snipDir},
		LibraryPaths:       []string{libDir},
	})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/wrap?s=value", nil)
	req.SetPathValue("snippet", "wrap")
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "[value]") {
		t.Errorf("body = %q, want '[value]'", rr.Body.String())
	}
}

// --- writer.Write failure path ---

// failingResponseWriter is an http.ResponseWriter whose Write always returns
// an error, so we can drive JsonnetHandler down the "Cannot write response"
// log branch without needing a flaky network.
type failingResponseWriter struct {
	header http.Header
	status int
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *failingResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *failingResponseWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("simulated write failure")
}

func TestJsonnetHandler_LogsErrorWhenResponseWriteFails(t *testing.T) {
	captured := newRecordCapture()
	dir := t.TempDir()
	writeSnippet(t, dir, "hello", `{ ok: true }`)

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		Logger:             slog.New(captured),
	})

	fw := &failingResponseWriter{}
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/hello", nil)
	req.SetPathValue("snippet", "hello")
	h(fw, req)

	if fw.status != http.StatusOK {
		t.Errorf("status set to %d before write attempt; want 200", fw.status)
	}
	if got := fw.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (must be set before WriteHeader)", got)
	}

	rec, ok := captured.findMessage("Cannot write response")
	if !ok {
		t.Fatalf("expected %q log record after Write failure; got messages: %v",
			"Cannot write response", messages(captured))
	}
	if rec.level != slog.LevelError {
		t.Errorf("level = %v, want Error", rec.level)
	}
	if _, ok := rec.attrs["error"]; !ok {
		t.Errorf("attrs missing error; got %v", rec.attrs)
	}
}

// --- applyTLAVars: rigorous coverage after dropping the dead error return ---

// evalTLA wires applyTLAVars + an anonymous-snippet evaluation. Returns the
// evaluated JSON string for inspection by the caller.
func evalTLA(t *testing.T, snippet string, params url.Values) string {
	t.Helper()
	vm := jsonnet.MakeVM()
	applyTLAVars(vm, params)
	out, err := vm.EvaluateAnonymousSnippet("tla.jsonnet", snippet)
	if err != nil {
		t.Fatalf("evaluate(%q, %+v): %v", snippet, params, err)
	}
	return out
}

func TestApplyTLAVars_NilMap_DoesNotPanic(t *testing.T) {
	vm := jsonnet.MakeVM()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panicked on nil queryParams: %v", r)
		}
	}()
	applyTLAVars(vm, nil)
}

func TestApplyTLAVars_EmptyMap_DoesNotPanic(t *testing.T) {
	vm := jsonnet.MakeVM()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panicked on empty queryParams: %v", r)
		}
	}()
	applyTLAVars(vm, url.Values{})
}

func TestApplyTLAVars_NilMapLeavesNoTLAs(t *testing.T) {
	out := evalTLA(t, `{ ok: true }`, nil)
	if !strings.Contains(out, `"ok": true`) {
		t.Errorf("output = %q, want it to contain 'ok: true'", out)
	}
}

func TestApplyTLAVars_EmptyMapLeavesNoTLAs(t *testing.T) {
	out := evalTLA(t, `{ ok: true }`, url.Values{})
	if !strings.Contains(out, `"ok": true`) {
		t.Errorf("output = %q, want it to contain 'ok: true'", out)
	}
}

// --- single-value dispatch (→ TLAVar / string) ---

func TestApplyTLAVars_SingleValue_RoundTrips(t *testing.T) {
	tests := map[string]string{
		"plain string":          "hello",
		"empty string":          "",
		"single space":          " ",
		"unicode latin":         "café",
		"unicode emoji":         "🚀",
		"unicode mixed":         "café 🚀",
		"newline":               "a\nb",
		"carriage return":       "a\rb",
		"tab":                   "a\tb",
		"crlf":                  "a\r\nb",
		"null byte tolerated":   "a\x00b",
		"double quote":          `say "hi"`,
		"single quote":          "it's",
		"backslash":             `C:\Users\x`,
		"backslash and quote":   `\"`,
		"numeric-looking":       "123",
		"negative-looking":      "-42",
		"float-looking":         "3.14",
		"boolean-looking true":  "true",
		"boolean-looking false": "false",
		"null-looking":          "null",
		"json-object-looking":   `{"x":1}`,
		"json-array-looking":    `[1,2]`,
		"whitespace only":       "   ",
		"leading whitespace":    "   x",
		"trailing whitespace":   "x   ",
		"surrounded whitespace": "  x  ",
		"very long (10000)":     strings.Repeat("x", 10000),
		"control character":     "a\x01b",
		"high unicode":          string([]rune{0x1F600}), // 😀
	}
	for name, val := range tests {
		t.Run(name, func(t *testing.T) {
			out := evalTLA(t, `function(v) { v: v }`, url.Values{"v": {val}})
			var payload struct {
				V string `json:"v"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("parse %q: %v", out, err)
			}
			if payload.V != val {
				t.Errorf("v = %q, want %q", payload.V, val)
			}
		})
	}
}

func TestApplyTLAVars_SingleValueIsStringNotCode(t *testing.T) {
	// Crucial dispatch property: with len==1 we use TLAVar (string), not
	// TLACode (jsonnet-evaluated). So a value like "true" must arrive as the
	// 5-character string "true", not the boolean true.
	tests := map[string]string{
		"keyword true":           "true",
		"keyword false":          "false",
		"keyword null":           "null",
		"integer":                "42",
		"object-literal-looking": `{"x":1}`,
		"array-literal-looking":  `[1,2,3]`,
	}
	for name, val := range tests {
		t.Run(name, func(t *testing.T) {
			out := evalTLA(t, `function(v) { kind: std.type(v), raw: v }`, url.Values{"v": {val}})
			var payload struct {
				Kind string `json:"kind"`
				Raw  string `json:"raw"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("parse %q: %v", out, err)
			}
			if payload.Kind != "string" {
				t.Errorf("kind = %q, want \"string\"; this value was evaluated as code, not passed as a string TLA", payload.Kind)
			}
			if payload.Raw != val {
				t.Errorf("raw = %q, want %q", payload.Raw, val)
			}
		})
	}
}

// --- multi-value dispatch (→ TLACode / JSON array) ---

func TestApplyTLAVars_MultiValue_RoundTrips(t *testing.T) {
	tests := map[string][]string{
		"two distinct":         {"a", "b"},
		"two identical":        {"x", "x"},
		"three distinct":       {"a", "b", "c"},
		"five strings":         {"1", "2", "3", "4", "5"},
		"ten strings":          {"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"},
		"mix of empty":         {"", "value", ""},
		"all empty":            {"", "", ""},
		"unicode entries":      {"café", "🚀", "naïve"},
		"special chars":        {`a"b`, `c\d`, "e\nf"},
		"numeric-looking":      {"1", "2", "3"},
		"boolean-looking":      {"true", "false", "true"},
		"long values":          {strings.Repeat("a", 1000), strings.Repeat("b", 1000)},
		"single element slice": {"only"},
	}
	for name, vals := range tests {
		t.Run(name, func(t *testing.T) {
			out := evalTLA(t, `function(v) { v: v }`, url.Values{"v": vals})
			if len(vals) == 1 {
				// Single-element slice → still uses TLAVar (string), per the
				// len==1 branch. We verify the round-trip is the string itself,
				// not a single-element JSON array.
				var payload struct {
					V string `json:"v"`
				}
				if err := json.Unmarshal([]byte(out), &payload); err != nil {
					t.Fatalf("parse %q: %v", out, err)
				}
				if payload.V != vals[0] {
					t.Errorf("v = %q, want %q", payload.V, vals[0])
				}
				return
			}
			var payload struct {
				V []string `json:"v"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("parse %q: %v", out, err)
			}
			if !slices.Equal(payload.V, vals) {
				t.Errorf("v = %v, want %v", payload.V, vals)
			}
		})
	}
}

func TestApplyTLAVars_MultiValueIsArrayNotConcatenated(t *testing.T) {
	// Multi-value TLA must arrive as a *jsonnet array*, not a comma-joined
	// string or anything else. We check std.type and array length explicitly.
	out := evalTLA(t,
		`function(v) { kind: std.type(v), len: std.length(v), zero: v[0], one: v[1] }`,
		url.Values{"v": {"alpha", "beta"}})

	var payload struct {
		Kind string `json:"kind"`
		Len  int    `json:"len"`
		Zero string `json:"zero"`
		One  string `json:"one"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if payload.Kind != "array" {
		t.Errorf("kind = %q, want \"array\"", payload.Kind)
	}
	if payload.Len != 2 {
		t.Errorf("len = %d, want 2", payload.Len)
	}
	if payload.Zero != "alpha" {
		t.Errorf("v[0] = %q, want \"alpha\"", payload.Zero)
	}
	if payload.One != "beta" {
		t.Errorf("v[1] = %q, want \"beta\"", payload.One)
	}
}

// --- dispatch boundary at exactly 1 vs 2 values ---

func TestApplyTLAVars_DispatchBoundary(t *testing.T) {
	t.Run("one value → string", func(t *testing.T) {
		out := evalTLA(t,
			`function(v) { kind: std.type(v) }`,
			url.Values{"v": {"only"}})
		var payload struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatalf("parse %q: %v", out, err)
		}
		if payload.Kind != "string" {
			t.Errorf("kind = %q, want \"string\" at len==1", payload.Kind)
		}
	})
	t.Run("two values → array", func(t *testing.T) {
		out := evalTLA(t,
			`function(v) { kind: std.type(v) }`,
			url.Values{"v": {"a", "b"}})
		var payload struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatalf("parse %q: %v", out, err)
		}
		if payload.Kind != "array" {
			t.Errorf("kind = %q, want \"array\" at len==2", payload.Kind)
		}
	})
}

// --- multi-key scenarios ---

func TestApplyTLAVars_DistinctKeysAreIndependent(t *testing.T) {
	out := evalTLA(t,
		`function(a, b, c) { a: a, b: b, c: c }`,
		url.Values{
			"a": {"alpha"},
			"b": {"bravo"},
			"c": {"charlie"},
		})
	var payload struct {
		A, B, C string
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if payload.A != "alpha" || payload.B != "bravo" || payload.C != "charlie" {
		t.Errorf("payload = %+v, want a=alpha b=bravo c=charlie", payload)
	}
}

func TestApplyTLAVars_MixedSingleAndMultiKeys(t *testing.T) {
	out := evalTLA(t,
		`function(s, m) { s: s, m: m }`,
		url.Values{
			"s": {"only"},
			"m": {"x", "y", "z"},
		})

	var payload struct {
		S string   `json:"s"`
		M []string `json:"m"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if payload.S != "only" {
		t.Errorf("s = %q, want \"only\"", payload.S)
	}
	if !slices.Equal(payload.M, []string{"x", "y", "z"}) {
		t.Errorf("m = %v, want [x y z]", payload.M)
	}
}

func TestApplyTLAVars_ManyKeysAllApplied(t *testing.T) {
	const N = 50
	params := make(url.Values, N)
	for i := 0; i < N; i++ {
		params[fmt.Sprintf("k%d", i)] = []string{fmt.Sprintf("v%d", i)}
	}

	var b strings.Builder
	b.WriteString("function(")
	for i := 0; i < N; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "k%d", i)
	}
	b.WriteString(") { ")
	for i := 0; i < N; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "k%d: k%d", i, i)
	}
	b.WriteString(" }")

	out := evalTLA(t, b.String(), params)
	var payload map[string]string
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	for i := 0; i < N; i++ {
		want := fmt.Sprintf("v%d", i)
		key := fmt.Sprintf("k%d", i)
		if payload[key] != want {
			t.Errorf("%s = %q, want %q", key, payload[key], want)
		}
	}
}

func TestApplyTLAVars_VeryLargeMultiValue(t *testing.T) {
	const N = 200
	vals := make([]string, N)
	for i := 0; i < N; i++ {
		vals[i] = fmt.Sprintf("entry-%d", i)
	}
	out := evalTLA(t, `function(v) { len: std.length(v), first: v[0], last: v[std.length(v)-1] }`,
		url.Values{"v": vals})
	var payload struct {
		Len   int    `json:"len"`
		First string `json:"first"`
		Last  string `json:"last"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if payload.Len != N {
		t.Errorf("len = %d, want %d", payload.Len, N)
	}
	if payload.First != "entry-0" {
		t.Errorf("first = %q, want \"entry-0\"", payload.First)
	}
	if payload.Last != fmt.Sprintf("entry-%d", N-1) {
		t.Errorf("last = %q, want \"entry-%d\"", payload.Last, N-1)
	}
}

// --- behavioural properties ---

func TestApplyTLAVars_DoesNotMutateInputMap(t *testing.T) {
	params := url.Values{
		"single": {"a"},
		"multi":  {"x", "y", "z"},
	}
	cp := url.Values{
		"single": append([]string{}, params["single"]...),
		"multi":  append([]string{}, params["multi"]...),
	}

	vm := jsonnet.MakeVM()
	applyTLAVars(vm, params)

	if !slices.Equal(params["single"], cp["single"]) {
		t.Errorf("params[single] mutated: %v vs %v", params["single"], cp["single"])
	}
	if !slices.Equal(params["multi"], cp["multi"]) {
		t.Errorf("params[multi] mutated: %v vs %v", params["multi"], cp["multi"])
	}
	if len(params) != len(cp) {
		t.Errorf("params length changed: %d vs %d", len(params), len(cp))
	}
}

func TestApplyTLAVars_SecondCallOverwritesFirst(t *testing.T) {
	vm := jsonnet.MakeVM()
	applyTLAVars(vm, url.Values{"v": {"first"}})
	applyTLAVars(vm, url.Values{"v": {"second"}})
	out, err := vm.EvaluateAnonymousSnippet("t.jsonnet", `function(v) { v: v }`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !strings.Contains(out, `"v": "second"`) {
		t.Errorf("output = %q, want \"v\": \"second\" after second call", out)
	}
}

func TestApplyTLAVars_SecondCallSwitchesSingleToMulti(t *testing.T) {
	vm := jsonnet.MakeVM()
	applyTLAVars(vm, url.Values{"v": {"single"}})
	applyTLAVars(vm, url.Values{"v": {"a", "b", "c"}})
	out, err := vm.EvaluateAnonymousSnippet("t.jsonnet", `function(v) { kind: std.type(v), v: v }`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !strings.Contains(out, `"kind": "array"`) {
		t.Errorf("output = %q, want kind:array after switch from single to multi", out)
	}
}

func TestApplyTLAVars_SecondCallSwitchesMultiToSingle(t *testing.T) {
	vm := jsonnet.MakeVM()
	applyTLAVars(vm, url.Values{"v": {"a", "b", "c"}})
	applyTLAVars(vm, url.Values{"v": {"single"}})
	out, err := vm.EvaluateAnonymousSnippet("t.jsonnet", `function(v) { kind: std.type(v), v: v }`)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !strings.Contains(out, `"kind": "string"`) {
		t.Errorf("output = %q, want kind:string after switch from multi to single", out)
	}
}

func TestApplyTLAVars_KeyShapes(t *testing.T) {
	tests := []string{
		"x",
		"longishKey",
		"snake_case_key",
		"camelCaseKey",
		"PascalCaseKey",
		"with123digits",
		strings.Repeat("k", 200),
	}
	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			snippet := fmt.Sprintf(`function(%s) { v: %s }`, key, key)
			out := evalTLA(t, snippet, url.Values{key: {"value"}})
			if !strings.Contains(out, `"v": "value"`) {
				t.Errorf("key %q: output = %q, want \"v\": \"value\"", key, out)
			}
		})
	}
}

// --- concurrency ---

func TestApplyTLAVars_ParallelOnDistinctVMs(t *testing.T) {
	// Each goroutine builds its own VM and runs applyTLAVars on it. Since the
	// function is a pure local-mutation helper, this should be race-free even
	// without internal locking.
	var wg sync.WaitGroup
	const workers = 100
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vm := jsonnet.MakeVM()
			applyTLAVars(vm, url.Values{"v": {fmt.Sprintf("value-%d", i)}})
			out, err := vm.EvaluateAnonymousSnippet("t.jsonnet", `function(v) { v: v }`)
			if err != nil {
				t.Errorf("worker %d: evaluate: %v", i, err)
				return
			}
			want := fmt.Sprintf(`"v": "value-%d"`, i)
			if !strings.Contains(out, want) {
				t.Errorf("worker %d: output %q missing %q", i, out, want)
			}
		}(i)
	}
	wg.Wait()
}

// --- exotic edge cases that the URL parser can produce ---

func TestApplyTLAVars_BareKeyFromParsedURL(t *testing.T) {
	// `?v` arrives as url.Values{"v": [""]} — a single-element slice, hence
	// the string-TLA branch.
	parsed, err := url.ParseQuery("v")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := evalTLA(t, `function(v) { kind: std.type(v), v: v }`, parsed)
	var payload struct {
		Kind string `json:"kind"`
		V    string `json:"v"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if payload.Kind != "string" {
		t.Errorf("kind = %q, want \"string\"", payload.Kind)
	}
	if payload.V != "" {
		t.Errorf("v = %q, want empty string", payload.V)
	}
}

func TestApplyTLAVars_RepeatedKeyFromParsedURL(t *testing.T) {
	// `?v=a&v=b` arrives as url.Values{"v": ["a", "b"]} — multi-value branch.
	parsed, err := url.ParseQuery("v=a&v=b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := evalTLA(t, `function(v) { kind: std.type(v), len: std.length(v) }`, parsed)
	var payload struct {
		Kind string `json:"kind"`
		Len  int    `json:"len"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if payload.Kind != "array" || payload.Len != 2 {
		t.Errorf("payload = %+v, want kind:array len:2", payload)
	}
}

func TestApplyTLAVars_EmptySliceValue_TreatedAsCode(t *testing.T) {
	// url.Values normally produces []string{""} for a present key, never
	// []string{}. Defensive: if someone hand-constructs url.Values with an
	// empty slice, the function falls into the multi-value branch and emits
	// an empty JSON array TLA, not a string.
	out := evalTLA(t, `function(v) { kind: std.type(v), len: std.length(v) }`,
		url.Values{"v": {}})
	var payload struct {
		Kind string `json:"kind"`
		Len  int    `json:"len"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if payload.Kind != "array" {
		t.Errorf("kind = %q, want \"array\"", payload.Kind)
	}
	if payload.Len != 0 {
		t.Errorf("len = %d, want 0", payload.Len)
	}
}
