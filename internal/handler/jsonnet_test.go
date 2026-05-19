/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"context"
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
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := parseExtVars(tc.environ)
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

	h := JsonnetHandler(nil, []string{dir}, nil)
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

	h := JsonnetHandler(nil, []string{dir}, nil)
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

	h := JsonnetHandler(nil, nil, nil)
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

func TestJsonnetHandler_MethodNotAllowed(t *testing.T) {
	h := JsonnetHandler(nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/jsonnet/x", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if got, want := rr.Code, http.StatusMethodNotAllowed; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}

func TestJsonnetHandler_NotFound(t *testing.T) {
	h := JsonnetHandler(nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/jsonnet/missing", nil)
	req.SetPathValue("snippet", "missing")
	rr := httptest.NewRecorder()
	h(rr, req)
	if got, want := rr.Code, http.StatusNotFound; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}
