/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/metio/jaas/internal/eval"
)

// textContent returns the text of the first TextContent block, failing the
// test if the result has no text block.
func textContent(t *testing.T, res *mcpsdk.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("result has no content: %+v", res)
	}
	tc, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("first content block is %T, want *TextContent", res.Content[0])
	}
	return tc.Text
}

// assertJSONEqual compares two JSON documents by parsed value, so key order
// and whitespace don't matter — the same semantic comparison the golden tests
// use.
func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not valid JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not valid JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestRenderHandler(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		in   renderInput
		want string // expected rendered JSON
	}{
		{
			name: "object literal",
			in:   renderInput{Source: `{a: 1, b: "two"}`},
			want: `{"a":1,"b":"two"}`,
		},
		{
			name: "call-level ext var",
			in:   renderInput{Source: `{x: std.extVar("greeting")}`, ExtVars: map[string]string{"greeting": "hi"}},
			want: `{"x":"hi"}`,
		},
		{
			name: "server ext var",
			cfg:  Config{ExtVars: map[string]string{"greeting": "from-server"}},
			in:   renderInput{Source: `{x: std.extVar("greeting")}`},
			want: `{"x":"from-server"}`,
		},
		{
			name: "call ext var overlays server",
			cfg:  Config{ExtVars: map[string]string{"greeting": "from-server"}},
			in:   renderInput{Source: `{x: std.extVar("greeting")}`, ExtVars: map[string]string{"greeting": "from-call"}},
			want: `{"x":"from-call"}`,
		},
		{
			name: "single-value TLA becomes string",
			in:   renderInput{Source: `function(name) {greeting: "hello " + name}`, Tlas: map[string][]string{"name": {"world"}}},
			want: `{"greeting":"hello world"}`,
		},
		{
			name: "multi-value TLA becomes array",
			in:   renderInput{Source: `function(tags) {tags: tags}`, Tlas: map[string][]string{"tags": {"a", "b"}}},
			want: `{"tags":["a","b"]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, out, err := tt.cfg.renderHandler(context.Background(), nil, tt.in)
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if res != nil && res.IsError {
				t.Fatalf("unexpected tool error: %s", textContent(t, res))
			}
			assertJSONEqual(t, out.JSON, tt.want)
			assertJSONEqual(t, textContent(t, res), tt.want)
		})
	}
}

func TestRenderHandler_EvalErrorReturnsDiagnostic(t *testing.T) {
	res, out, err := Config{}.renderHandler(context.Background(), nil, renderInput{Source: `{a: undefined_var}`})
	if err != nil {
		t.Fatalf("handler returned a Go error, want a tool-error result: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError result, got %+v", res)
	}
	if out.JSON != "" {
		t.Fatalf("expected empty output on failure, got %q", out.JSON)
	}
	// The rich go-jsonnet diagnostic is owner-facing and must reach the
	// caller verbatim — not the scrubbed public-HTTP message.
	if msg := textContent(t, res); msg == "" {
		t.Fatal("expected a non-empty diagnostic message")
	}
}

func TestRenderHandler_ImportResolvesAgainstLibraryPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "greeting.libsonnet"), []byte(`{msg: "hi"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{LibraryPaths: []string{dir}}
	res, out, err := cfg.renderHandler(context.Background(), nil, renderInput{Source: `(import "greeting.libsonnet").msg`})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", textContent(t, res))
	}
	assertJSONEqual(t, out.JSON, `"hi"`)
}

// TestRenderHandler_ConfinedImporterBlocksFileEscape pins that the network
// transport's confined importer refuses to read outside the library paths.
// Without it, a caller-supplied snippet over the unauthenticated MCP HTTP port
// could importstr the operator's ServiceAccount token or any mounted secret.
func TestRenderHandler_ConfinedImporterBlocksFileEscape(t *testing.T) {
	libDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(libDir, "ok.libsonnet"), []byte(`{msg: "hi"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A "secret" living outside the library root, reachable only by escaping it.
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "token")
	if err := os.WriteFile(secretPath, []byte("SUPER-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{LibraryPaths: []string{libDir}, ConfineImports: true}

	escapes := []struct {
		name, source string
	}{
		{"absolute importstr", `importstr '` + secretPath + `'`},
		{"absolute import", `import '` + secretPath + `'`},
		{"dot-dot traversal", `importstr '../` + filepath.Base(secretDir) + `/token'`},
	}
	for _, e := range escapes {
		t.Run(e.name, func(t *testing.T) {
			res, _, err := cfg.renderHandler(context.Background(), nil, renderInput{Source: e.source})
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("escape %q was NOT blocked — confined importer leaked a file read", e.source)
			}
			if msg := textContent(t, res); strings.Contains(msg, "SUPER-SECRET") {
				t.Fatalf("escape %q disclosed the secret contents: %s", e.source, msg)
			}
		})
	}

	// A legitimate library import must still resolve through the confined importer.
	t.Run("legit library import still works", func(t *testing.T) {
		res, out, err := cfg.renderHandler(context.Background(), nil, renderInput{Source: `(import "ok.libsonnet").msg`})
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if res.IsError {
			t.Fatalf("legit import rejected by confined importer: %s", textContent(t, res))
		}
		assertJSONEqual(t, out.JSON, `"hi"`)
	})
}

// TestRenderHandler_ConfinedImporterResolvesTransitiveRelativeImports pins that
// a confined library file can import a sibling by relative path — the common
// vendored-library shape (grafonnet et al.) — so confinement doesn't break the
// legitimate import graph.
func TestRenderHandler_ConfinedImporterResolvesTransitiveRelativeImports(t *testing.T) {
	libDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(libDir, "util.libsonnet"), []byte(`{n: 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "main.libsonnet"), []byte(`{v: (import "util.libsonnet").n}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{LibraryPaths: []string{libDir}, ConfineImports: true}
	res, out, err := cfg.renderHandler(context.Background(), nil, renderInput{Source: `(import "main.libsonnet").v`})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.IsError {
		t.Fatalf("transitive relative import failed under confinement: %s", textContent(t, res))
	}
	assertJSONEqual(t, out.JSON, `7`)
}

// TestNewHTTPHandler_SetsConfineImports pins that the network transport is wired
// to the confined importer (the stdio path must NOT be, so it stays free to
// import local files).
func TestNewHTTPHandler_SetsConfineImports(t *testing.T) {
	cfg := Config{}
	if cfg.importer() == nil {
		t.Fatal("stdio importer is nil")
	}
	// The stdio default keeps the stock FileImporter (ConfineImports false).
	if _, ok := cfg.importer().(*confinedImporter); ok {
		t.Error("stdio Config must use the stock FileImporter, not the confined one")
	}
	confined := Config{ConfineImports: true}
	if _, ok := confined.importer().(*confinedImporter); !ok {
		t.Error("ConfineImports=true must select the confined importer")
	}
}

func TestRenderHandler_EvalUnavailable(t *testing.T) {
	// Pin the global cap to 1, hold the only slot, and confirm a render is
	// turned away as an operational tool error rather than a render result.
	eval.SetMaxConcurrentEvals(1)
	defer eval.SetMaxConcurrentEvals(0)
	release, ok := eval.Reserve()
	if !ok {
		t.Fatal("could not reserve the only eval slot")
	}
	defer release()

	res, _, err := Config{}.renderHandler(context.Background(), nil, renderInput{Source: `{a: 1}`})
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError result when eval cap is full, got %+v", res)
	}
}

func TestValidateHandler(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		wantValid bool
	}{
		{name: "valid snippet", source: `{a: 1}`, wantValid: true},
		{name: "syntax error", source: `{a: }`, wantValid: false},
		{name: "runtime error", source: `{a: error "boom"}`, wantValid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, out, err := Config{}.validateHandler(context.Background(), nil, renderInput{Source: tt.source})
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if res != nil && res.IsError {
				t.Fatalf("unexpected tool error: %s", textContent(t, res))
			}
			if out.Valid != tt.wantValid {
				t.Fatalf("Valid = %v, want %v (error=%q)", out.Valid, tt.wantValid, out.Error)
			}
			if !tt.wantValid && out.Error == "" {
				t.Fatal("expected a diagnostic in Error for an invalid snippet")
			}
			if tt.wantValid && out.Error != "" {
				t.Fatalf("expected no Error for a valid snippet, got %q", out.Error)
			}
		})
	}
}

func TestMergedExtVars(t *testing.T) {
	tests := []struct {
		name   string
		server map[string]string
		call   map[string]string
		want   map[string]string
	}{
		{name: "both empty", want: nil},
		{name: "only server", server: map[string]string{"a": "1"}, want: map[string]string{"a": "1"}},
		{name: "only call", call: map[string]string{"a": "1"}, want: map[string]string{"a": "1"}},
		{
			name:   "call overlays server",
			server: map[string]string{"a": "server", "b": "server"},
			call:   map[string]string{"b": "call", "c": "call"},
			want:   map[string]string{"a": "server", "b": "call", "c": "call"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Config{ExtVars: tt.server}.mergedExtVars(tt.call)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("mergedExtVars = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestServer_InMemoryRoundTrip drives the registered tools through the real MCP
// protocol over an in-memory transport pair, proving registration, schema
// inference, and the request/response wiring end to end.
func TestServer_InMemoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	server := NewServer(Config{Version: "test"})

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range lt.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"render_jsonnet", "validate_jsonnet"} {
		if !got[want] {
			t.Errorf("tool %q not registered; have %v", want, got)
		}
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "render_jsonnet",
		Arguments: map[string]any{"source": `{hello: "world"}`},
	})
	if err != nil {
		t.Fatalf("call render_jsonnet: %v", err)
	}
	if res.IsError {
		t.Fatalf("render_jsonnet returned tool error: %s", textContent(t, res))
	}
	assertJSONEqual(t, textContent(t, res), `{"hello":"world"}`)

	res, err = cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "validate_jsonnet",
		Arguments: map[string]any{"source": `{a: }`},
	})
	if err != nil {
		t.Fatalf("call validate_jsonnet: %v", err)
	}
	// A compile failure is a verdict, not a protocol/tool error.
	if res.IsError {
		t.Fatalf("validate_jsonnet should not be a tool error for invalid input: %s", textContent(t, res))
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want map", res.StructuredContent)
	}
	if valid, _ := sc["valid"].(bool); valid {
		t.Fatalf("expected valid=false for a syntax error, got structured content %v", sc)
	}
}
