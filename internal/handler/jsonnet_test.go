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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-jsonnet"

	"github.com/metio/jaas/internal/eval"
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

func TestJsonnetHandler_ReturnsServiceUnavailableWhenEvalCapFull(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok", "main.jsonnet"), []byte(`{"ok": true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	eval.SetMaxConcurrentEvals(1)
	t.Cleanup(func() { eval.SetMaxConcurrentEvals(0) })
	// Pin the single slot so the handler's eval call sees a full gate.
	release, ok := eval.Reserve()
	if !ok {
		t.Fatal("could not reserve the baseline slot")
	}
	defer release()

	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
	})
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/ok", nil)
	req.SetPathValue("snippet", "ok")
	rr := httptest.NewRecorder()
	h(rr, req)

	if got, want := rr.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, rr.Body.String())
	}
	body := decodeError(t, rr)
	if body.Error != ErrCodeEvaluationUnavailable {
		t.Errorf("Error code = %q, want %q", body.Error, ErrCodeEvaluationUnavailable)
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
	// The handler must not re-read os.Environ() per request — ExtVars are
	// frozen at construction time. We set JAAS_EXT_VAR_late AFTER constructing
	// the handler and expect the snippet's std.extVar("late") call to fail,
	// because the handler's ExtVars were captured up front (without "late").
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

// --- jsonnet.FileImporter precedence: pinning the dependency contract ----
//
// The README promises that "rightmost matching library will be used" when
// the same library name lives under multiple -library-path entries. That
// promise is load-bearing for operators who layer a "base libraries" path
// with an "overrides" path. We implement it with a single line of glue
// (`vm.Importer(&jsonnet.FileImporter{JPaths: cfg.LibraryPaths})`) plus
// whatever iteration order go-jsonnet's FileImporter uses internally — so
// the contract really sits in that dependency. The tests below pin
// FileImporter's behavior directly, so a future go-jsonnet bump that flips
// iteration direction fails here with an obvious diagnostic.
//
// Integration-level proof lives in
// TestJsonnetHandler_LibraryImport_RightmostPathWins (below) and the e2e
// golden test TestExamples_LibraryPrecedence_*.

func TestLibraryPathPrecedence_TwoPathsBothHaveFile_RightmostWins(t *testing.T) {
	libA := t.TempDir()
	libB := t.TempDir()
	writeLibrary(t, libA, "shared", `{ v: "from-A" }`)
	writeLibrary(t, libB, "shared", `{ v: "from-B" }`)

	imp := &jsonnet.FileImporter{JPaths: []string{libA, libB}}
	_, foundAt, err := imp.Import("", "shared/main.libsonnet")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.HasPrefix(foundAt, libB) {
		t.Errorf("foundAt = %q, want it to start with %q (rightmost path)", foundAt, libB)
	}
}

func TestLibraryPathPrecedence_ThreePathsAllHaveFile_RightmostWins(t *testing.T) {
	libA := t.TempDir()
	libB := t.TempDir()
	libC := t.TempDir()
	writeLibrary(t, libA, "shared", `{ v: "from-A" }`)
	writeLibrary(t, libB, "shared", `{ v: "from-B" }`)
	writeLibrary(t, libC, "shared", `{ v: "from-C" }`)

	imp := &jsonnet.FileImporter{JPaths: []string{libA, libB, libC}}
	_, foundAt, err := imp.Import("", "shared/main.libsonnet")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.HasPrefix(foundAt, libC) {
		t.Errorf("foundAt = %q, want it to start with %q (rightmost of three)", foundAt, libC)
	}
}

func TestLibraryPathPrecedence_FallsBackThroughEmptyPaths(t *testing.T) {
	// File exists only under libA (leftmost). Rightmost path is empty.
	// The importer must fall back through libC, libB, and find it in libA.
	libA := t.TempDir()
	libB := t.TempDir()
	libC := t.TempDir()
	writeLibrary(t, libA, "only-in-a", `{ v: "from-A" }`)

	imp := &jsonnet.FileImporter{JPaths: []string{libA, libB, libC}}
	_, foundAt, err := imp.Import("", "only-in-a/main.libsonnet")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.HasPrefix(foundAt, libA) {
		t.Errorf("foundAt = %q, want it to start with %q (only A has the file)", foundAt, libA)
	}
}

func TestLibraryPathPrecedence_FileOnlyInMiddlePath(t *testing.T) {
	// File exists only under libB. Rightmost (libC) doesn't have it; importer
	// must skip libC, find it in libB, never reach libA.
	libA := t.TempDir()
	libB := t.TempDir()
	libC := t.TempDir()
	writeLibrary(t, libB, "only-in-b", `{ v: "from-B" }`)

	imp := &jsonnet.FileImporter{JPaths: []string{libA, libB, libC}}
	_, foundAt, err := imp.Import("", "only-in-b/main.libsonnet")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.HasPrefix(foundAt, libB) {
		t.Errorf("foundAt = %q, want it to start with %q (middle has file)", foundAt, libB)
	}
}

func TestLibraryPathPrecedence_TwoPathsHaveFileRightmostDoesNot(t *testing.T) {
	// libA and libB both have the file; libC (rightmost) does not. Result
	// must be libB — the *rightmost match*, not the rightmost path.
	libA := t.TempDir()
	libB := t.TempDir()
	libC := t.TempDir()
	writeLibrary(t, libA, "shared", `{ v: "from-A" }`)
	writeLibrary(t, libB, "shared", `{ v: "from-B" }`)

	imp := &jsonnet.FileImporter{JPaths: []string{libA, libB, libC}}
	_, foundAt, err := imp.Import("", "shared/main.libsonnet")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.HasPrefix(foundAt, libB) {
		t.Errorf("foundAt = %q, want it to start with %q (rightmost *match*, not rightmost path)", foundAt, libB)
	}
}

func TestLibraryPathPrecedence_SinglePath_TrivialResolve(t *testing.T) {
	libA := t.TempDir()
	writeLibrary(t, libA, "alone", `{ v: "ok" }`)

	imp := &jsonnet.FileImporter{JPaths: []string{libA}}
	_, foundAt, err := imp.Import("", "alone/main.libsonnet")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.HasPrefix(foundAt, libA) {
		t.Errorf("foundAt = %q, want it to start with %q", foundAt, libA)
	}
}

func TestLibraryPathPrecedence_NoPathHasFile_ReturnsError(t *testing.T) {
	libA := t.TempDir()
	libB := t.TempDir()

	imp := &jsonnet.FileImporter{JPaths: []string{libA, libB}}
	_, _, err := imp.Import("", "nonexistent/main.libsonnet")
	if err == nil {
		t.Error("expected an error when no path has the library")
	}
}

func TestLibraryPathPrecedence_EmptyJPaths_ReturnsError(t *testing.T) {
	imp := &jsonnet.FileImporter{JPaths: nil}
	_, _, err := imp.Import("", "anything/main.libsonnet")
	if err == nil {
		t.Error("expected an error when JPaths is empty")
	}
}

func TestLibraryPathPrecedence_ReversingPathOrderFlipsTheResolution(t *testing.T) {
	// Same two paths, opposite order — verifies the rightmost-wins rule is
	// truly *order-dependent*, not just "B happens to win because it's named B".
	libA := t.TempDir()
	libB := t.TempDir()
	writeLibrary(t, libA, "shared", `{ v: "from-A" }`)
	writeLibrary(t, libB, "shared", `{ v: "from-B" }`)

	tests := []struct {
		name       string
		paths      []string
		wantPrefix string
	}{
		{"A then B", []string{libA, libB}, libB},
		{"B then A", []string{libB, libA}, libA},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			imp := &jsonnet.FileImporter{JPaths: tc.paths}
			_, foundAt, err := imp.Import("", "shared/main.libsonnet")
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if !strings.HasPrefix(foundAt, tc.wantPrefix) {
				t.Errorf("foundAt = %q, want it to start with %q", foundAt, tc.wantPrefix)
			}
		})
	}
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

// --- Logger injection ---

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
// --- single-value dispatch (→ TLAVar / string) ---

// --- multi-value dispatch (→ TLACode / JSON array) ---

// --- dispatch boundary at exactly 1 vs 2 values ---

// --- multi-key scenarios ---

// --- behavioural properties ---

// --- concurrency ---

// --- exotic edge cases that the URL parser can produce ---

// ---- JSON error bodies on non-2xx responses -------------------------------

// decodeError parses rr.Body into an ErrorResponse, failing the test if the
// body isn't valid JSON.
func decodeError(t *testing.T, rr *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var got ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	return got
}

// callHandler is sugar for "create request, set snippet path value, drive
// handler, return the recorder." Used by the matrix tests below.
func callHandler(h http.HandlerFunc, method, snippet string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/jsonnet/"+snippet, nil)
	req.SetPathValue("snippet", snippet)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// silentLogger writes to io.Discard — used by helper-level tests where we don't
// care about log output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- writeJSONError, in isolation -----------------------------------------

func TestWriteJSONError_SetsStatusCode(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSONError(context.Background(), silentLogger(), rr, http.StatusTeapot, ErrorResponse{
		Error:   "x",
		Message: "y",
	})
	if rr.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rr.Code)
	}
}

func TestWriteJSONError_SetsContentTypeApplicationJSON(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSONError(context.Background(), silentLogger(), rr, http.StatusBadRequest, ErrorResponse{
		Error:   "x",
		Message: "y",
	})
	if got, want := rr.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

func TestWriteJSONError_EncodesAllFields(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSONError(context.Background(), silentLogger(), rr, http.StatusBadRequest, ErrorResponse{
		Error:   "boom",
		Message: "stuff broke",
		Snippet: "things/one",
	})
	want := ErrorResponse{Error: "boom", Message: "stuff broke", Snippet: "things/one"}
	if got := decodeError(t, rr); got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
}

func TestWriteJSONError_OmitsEmptySnippet(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSONError(context.Background(), silentLogger(), rr, http.StatusBadRequest, ErrorResponse{
		Error:   "boom",
		Message: "stuff broke",
	})
	if strings.Contains(rr.Body.String(), `"snippet"`) {
		t.Errorf("body unexpectedly contains \"snippet\" key: %s", rr.Body.String())
	}
}

func TestWriteJSONError_IncludesSnippetWhenPresent(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSONError(context.Background(), silentLogger(), rr, http.StatusBadRequest, ErrorResponse{
		Error:   "boom",
		Message: "stuff broke",
		Snippet: "dashboards/x",
	})
	if !strings.Contains(rr.Body.String(), `"snippet":"dashboards/x"`) {
		t.Errorf("body missing snippet field: %s", rr.Body.String())
	}
}

func TestWriteJSONError_BodyIsValidJSONObject(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSONError(context.Background(), silentLogger(), rr, http.StatusBadRequest, ErrorResponse{
		Error:   "x",
		Message: "y",
	})
	var generic map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &generic); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, rr.Body.String())
	}
	if generic["error"] != "x" {
		t.Errorf("error field = %v, want \"x\"", generic["error"])
	}
	if generic["message"] != "y" {
		t.Errorf("message field = %v, want \"y\"", generic["message"])
	}
}

func TestWriteJSONError_DoesNotEmitTrailingNewline(t *testing.T) {
	// json.Marshal returns bytes without a trailing newline (json.Encoder does).
	// Lock in that we use Marshal, so clients that expect to read exactly the
	// content-length bytes aren't surprised by an extra \n.
	rr := httptest.NewRecorder()
	writeJSONError(context.Background(), silentLogger(), rr, http.StatusBadRequest, ErrorResponse{
		Error:   "x",
		Message: "y",
	})
	if strings.HasSuffix(rr.Body.String(), "\n") {
		t.Errorf("body ends with newline: %q", rr.Body.String())
	}
}

// failingWriter exercises writeJSONError's logger branch by failing Write.
type failingWriter struct {
	header http.Header
	status int
}

func (f *failingWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}

func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("simulated write failure") }
func (f *failingWriter) WriteHeader(s int)         { f.status = s }

func TestWriteJSONError_LogsWriteFailure(t *testing.T) {
	cap := newRecordCapture()
	w := &failingWriter{}
	writeJSONError(context.Background(), slog.New(cap), w, http.StatusBadRequest, ErrorResponse{
		Error:   "x",
		Message: "y",
	})
	if cap.count() == 0 {
		t.Error("expected a log record when Write fails; got none")
	}
}

func TestWriteJSONError_StatusStillSetEvenIfWriteFails(t *testing.T) {
	w := &failingWriter{}
	writeJSONError(context.Background(), silentLogger(), w, http.StatusBadRequest, ErrorResponse{
		Error:   "x",
		Message: "y",
	})
	if w.status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (header must be written before body attempt)", w.status, http.StatusBadRequest)
	}
}

func TestWriteJSONError_NoLogRecordsOnSuccess(t *testing.T) {
	// Successful write should not emit any log records; warnings on the happy
	// path would drown out real signals at scale.
	cap := newRecordCapture()
	rr := httptest.NewRecorder()
	writeJSONError(context.Background(), slog.New(cap), rr, http.StatusBadRequest, ErrorResponse{
		Error:   "x",
		Message: "y",
	})
	if cap.count() != 0 {
		t.Errorf("expected zero log records on success, got %d", cap.count())
	}
}

func TestWriteJSONError_RequestContextThreadedToLogger(t *testing.T) {
	// The ctx passed in must reach the logger so request-scoped attributes
	// (trace id, request id) attach to error-write log records.
	captured := &ctxCaptureHandler{}
	type ctxKey struct{}
	const sentinel = "trace-77"
	ctx := context.WithValue(context.Background(), ctxKey{}, sentinel)

	writeJSONError(ctx, slog.New(captured), &failingWriter{}, http.StatusBadRequest, ErrorResponse{
		Error: "x", Message: "y",
	})
	if len(captured.contexts) == 0 {
		t.Fatal("expected log record(s) on write failure")
	}
	for i, c := range captured.contexts {
		if got, _ := c.Value(ctxKey{}).(string); got != sentinel {
			t.Errorf("log record %d: ctx value = %q, want %q", i, got, sentinel)
		}
	}
}

// ---- Method not allowed ---------------------------------------------------

func TestErrorResponse_MethodNotAllowed_AllNonGETMethods(t *testing.T) {
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodPatch, http.MethodOptions, http.MethodHead, http.MethodConnect, http.MethodTrace,
	} {
		t.Run(method, func(t *testing.T) {
			h := JsonnetHandler(Config{})
			req := httptest.NewRequest(method, "/jsonnet/x", nil)
			rr := httptest.NewRecorder()
			h(rr, req)
			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rr.Code)
			}
			if got := rr.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			// HEAD strips the body per RFC 7231; everything else must carry one.
			if method != http.MethodHead {
				body := decodeError(t, rr)
				if body.Error != ErrCodeMethodNotAllowed {
					t.Errorf("Error = %q, want %q", body.Error, ErrCodeMethodNotAllowed)
				}
				if body.Message == "" {
					t.Error("Message must not be empty")
				}
			}
		})
	}
}

func TestErrorResponse_MethodNotAllowed_NoSnippetField(t *testing.T) {
	// The method check fires before snippet resolution, so the snippet name is
	// not yet considered "the user's intent" and is omitted from the body.
	h := JsonnetHandler(Config{})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/anything", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	got := decodeError(t, rr)
	if got.Snippet != "" {
		t.Errorf("Snippet = %q, want empty (method check predates path parsing)", got.Snippet)
	}
	if strings.Contains(rr.Body.String(), `"snippet"`) {
		t.Errorf("raw body unexpectedly contains snippet key: %s", rr.Body.String())
	}
}

func TestErrorResponse_MethodNotAllowed_StableMessage(t *testing.T) {
	h := JsonnetHandler(Config{})
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	got := decodeError(t, rr)
	if !strings.Contains(strings.ToLower(got.Message), "get") {
		t.Errorf("Message = %q, want it to mention GET", got.Message)
	}
}

// ---- Snippet not found ----------------------------------------------------

func TestErrorResponse_SnippetNotFound_BodyShape(t *testing.T) {
	h := JsonnetHandler(Config{})
	rr := callHandler(h, http.MethodGet, "missing")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	got := decodeError(t, rr)
	if got.Error != ErrCodeSnippetNotFound {
		t.Errorf("Error = %q, want %q", got.Error, ErrCodeSnippetNotFound)
	}
	if got.Snippet != "missing" {
		t.Errorf("Snippet = %q, want %q", got.Snippet, "missing")
	}
	if !strings.Contains(got.Message, "missing") {
		t.Errorf("Message = %q, want it to mention the snippet name", got.Message)
	}
}

func TestErrorResponse_SnippetNotFound_ContentType(t *testing.T) {
	h := JsonnetHandler(Config{})
	rr := callHandler(h, http.MethodGet, "missing")
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestErrorResponse_SnippetNotFound_EmptyName(t *testing.T) {
	h := JsonnetHandler(Config{})
	rr := callHandler(h, http.MethodGet, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	got := decodeError(t, rr)
	if got.Error != ErrCodeSnippetNotFound {
		t.Errorf("Error = %q, want %q", got.Error, ErrCodeSnippetNotFound)
	}
	// omitempty kicks in for an empty Snippet field.
	if strings.Contains(rr.Body.String(), `"snippet"`) {
		t.Errorf("body unexpectedly includes \"snippet\" key for empty name: %s", rr.Body.String())
	}
}

func TestErrorResponse_SnippetNotFound_NameWithSpecialCharacters(t *testing.T) {
	h := JsonnetHandler(Config{})
	rr := callHandler(h, http.MethodGet, `weird/"name`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	got := decodeError(t, rr)
	if got.Snippet != `weird/"name` {
		t.Errorf("Snippet = %q, want %q (must be JSON-escaped, not stripped)", got.Snippet, `weird/"name`)
	}
}

func TestErrorResponse_SnippetNotFound_TraversalDoesNotLeakFileContents(t *testing.T) {
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
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "leaked") {
		t.Errorf("body unexpectedly contains 'leaked' from outside file: %s", rr.Body.String())
	}
}

// ---- Evaluation timeout ---------------------------------------------------

func TestErrorResponse_EvaluationTimeout_BodyShape(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "slow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow", "main.jsonnet"),
		[]byte(`local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		EvaluationTimeout:  time.Microsecond,
		MaxStack:           10000,
	})
	rr := callHandler(h, http.MethodGet, "slow")
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body: %s)", rr.Code, rr.Body.String())
	}
	got := decodeError(t, rr)
	if got.Error != ErrCodeEvaluationTimeout {
		t.Errorf("Error = %q, want %q", got.Error, ErrCodeEvaluationTimeout)
	}
	if got.Snippet != "slow" {
		t.Errorf("Snippet = %q, want %q", got.Snippet, "slow")
	}
	if got.Message == "" {
		t.Error("Message must not be empty")
	}
}

func TestErrorResponse_EvaluationTimeout_MessageMentionsTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "slow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow", "main.jsonnet"),
		[]byte(`local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		EvaluationTimeout:  3 * time.Millisecond,
		MaxStack:           10000,
	})
	rr := callHandler(h, http.MethodGet, "slow")
	got := decodeError(t, rr)
	// time.Duration formats as e.g. "3ms" — surface it so operators see the
	// configured limit, not just "timed out."
	if !strings.Contains(got.Message, "3ms") {
		t.Errorf("Message = %q, want it to contain '3ms'", got.Message)
	}
}

func TestErrorResponse_EvaluationTimeout_ContentType(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "slow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow", "main.jsonnet"),
		[]byte(`local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		EvaluationTimeout:  time.Microsecond,
		MaxStack:           10000,
	})
	rr := callHandler(h, http.MethodGet, "slow")
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// ---- Evaluation failed ----------------------------------------------------

func TestErrorResponse_EvaluationFailed_SyntaxError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken", "main.jsonnet"),
		[]byte(`local x =`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}})
	rr := callHandler(h, http.MethodGet, "broken")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	got := decodeError(t, rr)
	if got.Error != ErrCodeEvaluationFailed {
		t.Errorf("Error = %q, want %q", got.Error, ErrCodeEvaluationFailed)
	}
	if got.Snippet != "broken" {
		t.Errorf("Snippet = %q, want %q", got.Snippet, "broken")
	}
	if got.Message == "" {
		t.Error("Message must surface the underlying go-jsonnet diagnostic")
	}
}

func TestErrorResponse_EvaluationFailed_SurfacesUndefinedExtVar(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "noextvar"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "noextvar", "main.jsonnet"),
		[]byte(`{ v: std.extVar("undefined_var") }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}})
	rr := callHandler(h, http.MethodGet, "noextvar")
	got := decodeError(t, rr)
	if !strings.Contains(got.Message, "undefined_var") {
		t.Errorf("Message = %q, want it to name the missing ext-var", got.Message)
	}
}

func TestErrorResponse_EvaluationFailed_SurfacesMissingImport(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "missingimport"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "missingimport", "main.jsonnet"),
		[]byte(`local x = import 'doesnotexist.libsonnet'; { a: x }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}})
	rr := callHandler(h, http.MethodGet, "missingimport")
	got := decodeError(t, rr)
	if !strings.Contains(got.Message, "doesnotexist.libsonnet") {
		t.Errorf("Message = %q, want it to name the missing import", got.Message)
	}
}

func TestErrorResponse_EvaluationFailed_StackLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deep", "main.jsonnet"),
		[]byte(`local f(n) = if n == 0 then 0 else f(n-1) + 1; f(200)`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		EvaluationTimeout:  10 * time.Second,
		MaxStack:           20,
	})
	rr := callHandler(h, http.MethodGet, "deep")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	got := decodeError(t, rr)
	if got.Error != ErrCodeEvaluationFailed {
		t.Errorf("Error = %q, want %q (stack-limit error is a 400, not a 504)", got.Error, ErrCodeEvaluationFailed)
	}
	if !strings.Contains(strings.ToLower(got.Message), "stack") {
		t.Errorf("Message = %q, want it to mention 'stack'", got.Message)
	}
}

// ---- Client cancellation: no body ----------------------------------------

func TestErrorResponse_ClientCancel_WritesNoBody(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "slow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slow", "main.jsonnet"),
		[]byte(`local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{
		SnippetDirectories: []string{dir},
		EvaluationTimeout:  time.Hour,
		MaxStack:           10000,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/slow", nil).WithContext(ctx)
	req.SetPathValue("snippet", "slow")
	rr := httptest.NewRecorder()
	h(rr, req)
	// httptest.NewRecorder defaults Code to 200 as a placeholder; the cancel
	// branch must skip WriteHeader entirely so the recorder stays at the default.
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 placeholder (no WriteHeader on cancel)", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("body = %q, want empty (no body on cancel)", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want empty (no headers on cancel)", got)
	}
}

// ---- Cross-cutting matrix --------------------------------------------------

func TestErrorResponse_AllPaths_ConformToContract(t *testing.T) {
	timeoutDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(timeoutDir, "slow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(timeoutDir, "slow", "main.jsonnet"),
		[]byte(`local f(n) = if n == 0 then 0 else f(n-1) + 1; f(500)`), 0o644); err != nil {
		t.Fatal(err)
	}
	brokenDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brokenDir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "broken", "main.jsonnet"),
		[]byte(`local x =`), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		cfg           Config
		method        string
		snippet       string
		wantStatus    int
		wantCode      string
		wantSnippet   string
		messageSubstr string
	}{
		{
			name:          "method_not_allowed",
			cfg:           Config{},
			method:        http.MethodPost,
			snippet:       "x",
			wantStatus:    http.StatusMethodNotAllowed,
			wantCode:      ErrCodeMethodNotAllowed,
			wantSnippet:   "",
			messageSubstr: "GET",
		},
		{
			name:          "snippet_not_found",
			cfg:           Config{},
			method:        http.MethodGet,
			snippet:       "ghost",
			wantStatus:    http.StatusNotFound,
			wantCode:      ErrCodeSnippetNotFound,
			wantSnippet:   "ghost",
			messageSubstr: "ghost",
		},
		{
			name:          "evaluation_timeout",
			cfg:           Config{SnippetDirectories: []string{timeoutDir}, EvaluationTimeout: time.Microsecond, MaxStack: 10000},
			method:        http.MethodGet,
			snippet:       "slow",
			wantStatus:    http.StatusGatewayTimeout,
			wantCode:      ErrCodeEvaluationTimeout,
			wantSnippet:   "slow",
			messageSubstr: "1µs",
		},
		{
			name:          "evaluation_failed",
			cfg:           Config{SnippetDirectories: []string{brokenDir}},
			method:        http.MethodGet,
			snippet:       "broken",
			wantStatus:    http.StatusBadRequest,
			wantCode:      ErrCodeEvaluationFailed,
			wantSnippet:   "broken",
			messageSubstr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := JsonnetHandler(tc.cfg)
			rr := callHandler(h, tc.method, tc.snippet)
			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if got := rr.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			got := decodeError(t, rr)
			if got.Error != tc.wantCode {
				t.Errorf("Error = %q, want %q", got.Error, tc.wantCode)
			}
			if got.Snippet != tc.wantSnippet {
				t.Errorf("Snippet = %q, want %q", got.Snippet, tc.wantSnippet)
			}
			if got.Message == "" {
				t.Error("Message must not be empty")
			}
			if tc.messageSubstr != "" && !strings.Contains(got.Message, tc.messageSubstr) {
				t.Errorf("Message = %q, want it to contain %q", got.Message, tc.messageSubstr)
			}
		})
	}
}

// Constant freeze: wire-level identifiers callers match on.
// Renaming a constant value is a breaking change — fix the consumer first.
func TestErrorResponse_StableCodeValues(t *testing.T) {
	want := map[string]string{
		"ErrCodeMethodNotAllowed":      "method_not_allowed",
		"ErrCodeSnippetNotFound":       "snippet_not_found",
		"ErrCodeEvaluationTimeout":     "evaluation_timeout",
		"ErrCodeEvaluationUnavailable": "evaluation_unavailable",
		"ErrCodeEvaluationFailed":      "evaluation_failed",
	}
	got := map[string]string{
		"ErrCodeMethodNotAllowed":      ErrCodeMethodNotAllowed,
		"ErrCodeSnippetNotFound":       ErrCodeSnippetNotFound,
		"ErrCodeEvaluationTimeout":     ErrCodeEvaluationTimeout,
		"ErrCodeEvaluationUnavailable": ErrCodeEvaluationUnavailable,
		"ErrCodeEvaluationFailed":      ErrCodeEvaluationFailed,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (do not change without a major bump)", k, got[k], v)
		}
	}
}

// ---- Success path is unchanged -------------------------------------------

func TestErrorResponse_SuccessPath_BodyIsNotErrorShape(t *testing.T) {
	// Negative control: 200 responses must NOT look like ErrorResponse — they
	// are the rendered jsonnet output, which is whatever the user wrote.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fine", "main.jsonnet"), []byte(`{ ok: true }`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}})
	rr := callHandler(h, http.MethodGet, "fine")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"error"`) {
		t.Errorf("body looks like an ErrorResponse: %s", rr.Body.String())
	}
}

func TestErrorResponse_SuccessPath_ContentTypeUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fine", "main.jsonnet"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := JsonnetHandler(Config{SnippetDirectories: []string{dir}})
	rr := callHandler(h, http.MethodGet, "fine")
	if got, want := rr.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}
