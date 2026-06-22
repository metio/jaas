/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/go-jsonnet"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/metio/jaas/internal/eval"
)

// snippetName is the diagnostic label go-jsonnet attaches to errors and stack
// frames for an inline (file-less) snippet.
const snippetName = "snippet.jsonnet"

// renderInput is shared by both tools: a snippet plus the same TLA/ext-var
// knobs the HTTP path accepts.
type renderInput struct {
	Source  string              `json:"source" jsonschema:"the Jsonnet snippet to evaluate"`
	Tlas    map[string][]string `json:"tlas,omitempty" jsonschema:"top-level arguments; each value is a list — a single-element list becomes a string TLA, a multi-element list becomes a JSON-array TLA"`
	ExtVars map[string]string   `json:"extVars,omitempty" jsonschema:"external variables exposed to the snippet via std.extVar; overlays the server's configured ext vars on key conflicts"`
}

type renderOutput struct {
	JSON string `json:"json" jsonschema:"the evaluated snippet rendered as JSON"`
}

type validateOutput struct {
	Valid bool   `json:"valid" jsonschema:"true when the snippet evaluates without error"`
	Error string `json:"error,omitempty" jsonschema:"the go-jsonnet diagnostic (file and line) when valid is false"`
}

func registerRenderTools(server *mcpsdk.Server, cfg Config) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "render_jsonnet",
		Description: "Evaluate an inline Jsonnet snippet and return the resulting JSON. Imports resolve against the server's configured library paths (JPATH), identical to the jaas HTTP renderer. The core cluster-free authoring loop.",
	}, cfg.renderHandler)

	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "validate_jsonnet",
		Description: "Evaluate an inline Jsonnet snippet and report whether it compiles, returning the full go-jsonnet diagnostic (file and line) on failure without the rendered output.",
	}, cfg.validateHandler)
}

func (cfg Config) renderHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in renderInput) (*mcpsdk.CallToolResult, renderOutput, error) {
	out, err := cfg.evaluate(ctx, in)
	if err != nil {
		return cfg.evalErrorResult(err), renderOutput{}, nil
	}
	// Set Content to the raw rendered JSON so an agent reads the value
	// directly; the SDK still populates StructuredContent from renderOutput
	// for typed consumers.
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: out}},
	}, renderOutput{JSON: out}, nil
}

func (cfg Config) validateHandler(ctx context.Context, _ *mcpsdk.CallToolRequest, in renderInput) (*mcpsdk.CallToolResult, validateOutput, error) {
	_, err := cfg.evaluate(ctx, in)
	if err != nil {
		// A full eval-slot, a timeout, or a cancelled request (client
		// disconnect / parent ctx cancel) is an operational failure of the
		// server, not a verdict on the snippet — surface it as a tool error.
		// Reporting valid=false there would tell the agent a perfectly valid
		// snippet failed to compile. A genuine compile error is the validation
		// verdict valid=false.
		if errors.Is(err, eval.ErrEvalUnavailable) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			return cfg.evalErrorResult(err), validateOutput{}, nil
		}
		return nil, validateOutput{Valid: false, Error: err.Error()}, nil
	}
	return nil, validateOutput{Valid: true}, nil
}

// evaluate runs the snippet through the shared eval path with a per-request
// FileImporter rooted at the configured library paths. The importer is built
// per call because go-jsonnet's FileImporter cache is not concurrency-safe.
func (cfg Config) evaluate(ctx context.Context, in renderInput) (string, error) {
	return eval.EvaluateAnonymousSnippet(ctx, snippetName, in.Source, eval.Options{
		ExtVars:  cfg.mergedExtVars(in.ExtVars),
		TLAs:     in.Tlas,
		MaxStack: cfg.MaxStack,
		Timeout:  cfg.EvaluationTimeout,
		Importer: cfg.importer(),
	})
}

// importer returns the jsonnet importer for the eval tools. The network
// transport gets a root-confined importer so caller-supplied source cannot read
// outside the library paths; the local stdio renderer keeps the stock
// FileImporter so a user may import their own files freely.
func (cfg Config) importer() jsonnet.Importer {
	if cfg.ConfineImports {
		return newConfinedImporter(cfg.LibraryPaths)
	}
	return &jsonnet.FileImporter{JPaths: cfg.LibraryPaths}
}

// mergedExtVars overlays a call's ext vars on the server-configured set, with
// the call taking precedence — mirroring how the main binary overlays
// --ext-var on JAAS_EXT_VAR_*.
func (cfg Config) mergedExtVars(callExtVars map[string]string) map[string]string {
	if len(cfg.ExtVars) == 0 {
		return callExtVars
	}
	if len(callExtVars) == 0 {
		return cfg.ExtVars
	}
	merged := make(map[string]string, len(cfg.ExtVars)+len(callExtVars))
	for k, v := range cfg.ExtVars {
		merged[k] = v
	}
	for k, v := range callExtVars {
		merged[k] = v
	}
	return merged
}

// evalErrorResult maps an eval error to an MCP tool-error result. The rich
// go-jsonnet diagnostic is returned verbatim: this server is owner-facing
// (local stdio), so the disclosure-parity model treats it like the operator
// status path, not the scrubbed public HTTP path.
func (cfg Config) evalErrorResult(err error) *mcpsdk.CallToolResult {
	var msg string
	switch {
	case errors.Is(err, eval.ErrEvalUnavailable):
		msg = "evaluation unavailable: concurrent-eval cap is full; retry after backoff"
	case errors.Is(err, context.DeadlineExceeded):
		msg = fmt.Sprintf("evaluation timed out after %s", cfg.EvaluationTimeout)
	default:
		msg = err.Error()
	}
	return errorResult(msg)
}

// errorResult builds an MCP tool-error result carrying msg as its text content.
func errorResult(msg string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}},
	}
}
