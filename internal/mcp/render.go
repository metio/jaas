/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/go-jsonnet"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/metio/jaas/internal/eval"
)

// snippetName is the diagnostic label go-jsonnet attaches to errors and stack
// frames for an inline (file-less) snippet.
const snippetName = "snippet.jsonnet"

// renderInput is shared by both tools: a snippet plus the same TLA/ext-var
// knobs the HTTP path accepts, and the code-valued variants the HTTP query
// string cannot express (its values are always strings).
//
// The string and code maps of one kind share a binding namespace, so a name may
// appear in only one of the pair — see conflictingVariableNames.
type renderInput struct {
	Source  string              `json:"source" jsonschema:"the Jsonnet snippet to evaluate"`
	Tlas    map[string][]string `json:"tlas,omitempty" jsonschema:"top-level arguments bound as strings; each value is a list — a single-element list becomes a string TLA, a multi-element list becomes a JSON-array TLA"`
	TlaCode map[string]string   `json:"tlaCode,omitempty" jsonschema:"top-level arguments whose values are Jsonnet source to parse, like jsonnet --tla-code: \"3\" binds the number 3 and [\"a\",\"b\"] an array. A name may not also appear in tlas"`
	ExtVars map[string]string   `json:"extVars,omitempty" jsonschema:"external variables exposed to the snippet via std.extVar, bound as strings; overlays the server's configured ext vars on key conflicts"`
	ExtCode map[string]string   `json:"extCode,omitempty" jsonschema:"external variables whose values are Jsonnet source to parse, like jsonnet --ext-code: \"3\" binds the number 3 and { cpu: 2 } an object. Overlays the server's configured ext vars on key conflicts. A name may not also appear in extVars"`
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
	if bad := variableConflictError(in); bad != nil {
		return bad, renderOutput{}, nil
	}
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
	// A name bound both as a string and as code is a malformed call, not a
	// verdict on the snippet — same reasoning as the operational failures
	// below. valid=false would blame the source for the caller's mistake.
	if bad := variableConflictError(in); bad != nil {
		return bad, validateOutput{}, nil
	}
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
		return nil, validateOutput{Valid: false, Error: cfg.scrubLibraryPaths(err.Error())}, nil
	}
	return nil, validateOutput{Valid: true}, nil
}

// evaluate runs the snippet through the shared eval path with a per-request
// FileImporter rooted at the configured library paths. The importer is built
// per call because go-jsonnet's FileImporter cache is not concurrency-safe.
func (cfg Config) evaluate(ctx context.Context, in renderInput) (string, error) {
	return eval.EvaluateAnonymousSnippet(ctx, snippetName, in.Source, eval.Options{
		ExtVars:  cfg.mergedExtVars(in.ExtVars, in.ExtCode),
		ExtCode:  in.ExtCode,
		TLAs:     in.Tlas,
		TLACode:  in.TlaCode,
		MaxStack: cfg.MaxStack,
		Timeout:  cfg.EvaluationTimeout,
		Importer: cfg.importer(),
	})
}

// conflictingVariableNames returns the names a call binds twice within one
// binding namespace — once as a string and once as code. eval.Options leaves the
// winner unspecified for such a name, so rather than render an arbitrary one of
// the two the tools reject the call. The operator path needs no equivalent
// check: spec.tlas / spec.externalVariables carry listMapKey=name, so the
// apiserver refuses a duplicate before the reconciler ever sees it.
//
// The returned names are sorted so the error text is stable across calls.
func conflictingVariableNames(in renderInput) (tlas, extVars []string) {
	for name := range in.TlaCode {
		if _, dup := in.Tlas[name]; dup {
			tlas = append(tlas, name)
		}
	}
	for name := range in.ExtCode {
		if _, dup := in.ExtVars[name]; dup {
			extVars = append(extVars, name)
		}
	}
	slices.Sort(tlas)
	slices.Sort(extVars)
	return tlas, extVars
}

// variableConflictError returns the tool-error result for a call that binds a
// name in both the string and code map of one kind, or nil when the call is
// well-formed.
func variableConflictError(in renderInput) *mcpsdk.CallToolResult {
	tlas, extVars := conflictingVariableNames(in)
	var msgs []string
	if len(tlas) > 0 {
		msgs = append(msgs, fmt.Sprintf("tlas and tlaCode both bind %v", tlas))
	}
	if len(extVars) > 0 {
		msgs = append(msgs, fmt.Sprintf("extVars and extCode both bind %v", extVars))
	}
	if len(msgs) == 0 {
		return nil
	}
	return errorResult(fmt.Sprintf(
		"%s; each name may be bound as a string or as code, not both", strings.Join(msgs, "; "),
	))
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

// mergedExtVars overlays a call's string-valued ext vars on the
// server-configured set, with the call taking precedence — mirroring how the
// main binary overlays --ext-var on JAAS_EXT_VAR_*.
//
// callExtCode participates only by displacement: the server's ext vars are
// always strings, so a name the call binds as code must be dropped from the
// string map. Otherwise that name would sit in both maps eval.Options receives,
// where the winner is unspecified — and the call, being the more specific
// binding, must win deterministically.
//
// That guarantee is unconditional, so the pass-through shortcuts are available
// only when the call binds no code at all — returning an input map untouched
// would keep a displaced name in it.
func (cfg Config) mergedExtVars(callExtVars, callExtCode map[string]string) map[string]string {
	if len(callExtCode) == 0 {
		if len(cfg.ExtVars) == 0 {
			return callExtVars
		}
		if len(callExtVars) == 0 {
			return cfg.ExtVars
		}
	}
	merged := make(map[string]string, len(cfg.ExtVars)+len(callExtVars))
	maps.Copy(merged, cfg.ExtVars)
	maps.Copy(merged, callExtVars)
	for name := range callExtCode {
		delete(merged, name)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// evalErrorResult maps an eval error to an MCP tool-error result. On stdio the
// rich go-jsonnet diagnostic is returned verbatim (owner-facing, like the
// operator status path); on the network transport the diagnostic is run through
// scrubLibraryPaths so a caller-supplied snippet that fails inside a library
// import can't read back the operator pod's absolute library-mount layout.
func (cfg Config) evalErrorResult(err error) *mcpsdk.CallToolResult {
	var msg string
	switch {
	case errors.Is(err, eval.ErrEvalUnavailable):
		msg = "evaluation unavailable: concurrent-eval cap is full; retry after backoff"
	case errors.Is(err, context.DeadlineExceeded):
		msg = fmt.Sprintf("evaluation timed out after %s", cfg.EvaluationTimeout)
	default:
		msg = cfg.scrubLibraryPaths(err.Error())
	}
	return errorResult(msg)
}

// scrubLibraryPaths strips the configured library-root absolute prefixes from a
// diagnostic on the confined (network) transport, so a path like
// "/libraries/grafonnet/main.libsonnet:12:3" reads as
// "grafonnet/main.libsonnet:12:3" — the file:line and error stay useful, but the
// operator pod's filesystem layout doesn't leak. A no-op on the stdio renderer
// (ConfineImports false), where the paths are the user's own.
func (cfg Config) scrubLibraryPaths(msg string) string {
	if !cfg.ConfineImports {
		return msg
	}
	for _, root := range cfg.LibraryPaths {
		if root == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, strings.TrimRight(root, "/")+"/", "")
	}
	return msg
}

// errorResult builds an MCP tool-error result carrying msg as its text content.
func errorResult(msg string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}},
	}
}
