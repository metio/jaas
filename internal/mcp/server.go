/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package mcp exposes jaas's Jsonnet evaluator as a Model Context Protocol
// server so an LLM agent can render and validate snippets directly. The tools
// are thin adapters over internal/eval — the same code path the HTTP handler
// and the operator reconciler use — so MCP rendering is byte-identical to the
// other surfaces.
//
// Phase 1 is the cluster-free authoring loop over stdio: render_jsonnet and
// validate_jsonnet, no Kubernetes client and no RBAC surface.
package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/metio/jaas/internal/storage"
)

// rfc3339 is the timestamp layout used for every time field the tools emit.
const rfc3339 = "2006-01-02T15:04:05Z07:00"

// Config carries the static, server-lifetime knobs. The per-call tool inputs
// (snippet source, TLAs, ext vars) arrive on each request and overlay these.
type Config struct {
	// LibraryPaths are the JPATH directories the FileImporter searches for
	// `import` resolution — the same --library-path semantics as the HTTP
	// path, where the rightmost matching path wins.
	LibraryPaths []string

	// ExtVars are server-level external variables (from --ext-var and
	// JAAS_EXT_VAR_*). A tool call's own extVars overlay these per request.
	ExtVars map[string]string

	// MaxStack bounds go-jsonnet's call-stack depth; 0 keeps the default.
	MaxStack int

	// EvaluationTimeout bounds a single evaluation; 0 disables the bound.
	EvaluationTimeout time.Duration

	// Version is reported to MCP clients as the server implementation version.
	Version string

	// Logger receives the SDK's server-activity logs. It is jaas's shared
	// observability logger so MCP diagnostics share the format, level, and
	// destination of every other surface. A nil value disables SDK logging.
	Logger *slog.Logger

	// KubeClient reads operator resources (JsonnetSnippet). When non-nil the
	// in-cluster read tools (list_snippets, get_snippet) are registered; when
	// nil only the cluster-free eval tools are served. The embedded
	// operator-mode server sets this; the local stdio renderer leaves it nil.
	KubeClient client.Client

	// RunbookBaseURL is the docs-site prefix for per-reason remediation pages,
	// used to build the runbook link in get_snippet. Empty omits the link.
	RunbookBaseURL string

	// AllowMutations registers the gated write tools (reconcile/suspend/resume)
	// in addition to the read tools. Off by default: the server is read-only
	// unless the operator opts in (--mcp-allow-mutations). Has no effect without
	// a KubeClient.
	AllowMutations bool

	// Store reads published artifact revisions in-process for the read-only
	// diff_revisions tool. The embedded operator-mode server sets it to the
	// same artifact backend the reconciler publishes to; when nil (or without a
	// KubeClient) the diff tool is not registered.
	Store storage.Backend

	// ConfineImports restricts render_jsonnet / validate_jsonnet so a snippet's
	// imports resolve only within LibraryPaths (no absolute, working-directory,
	// or ".."-escaping reads). NewHTTPHandler sets it for the network transport,
	// which evaluates caller-supplied source over an unauthenticated port; the
	// stdio renderer leaves it false (a single-user local tool may import local
	// files freely, the same as running go-jsonnet by hand).
	ConfineImports bool
}

// NewServer builds the MCP server with the jaas tool catalog registered. It
// does not start any transport — call Run for stdio, or drive the returned
// server with an in-memory transport in tests.
func NewServer(cfg Config) *mcpsdk.Server {
	server := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "jaas", Version: cfg.Version},
		&mcpsdk.ServerOptions{Logger: cfg.Logger},
	)
	registerRenderTools(server, cfg)
	// The in-cluster read tools need a Kubernetes client; without one (the
	// local stdio renderer) only the eval tools are served.
	if cfg.KubeClient != nil {
		registerOperatorTools(server, cfg)
		// Write tools are a further opt-in on top of having a client.
		if cfg.AllowMutations {
			registerMutationTools(server, cfg)
		}
	}
	return server
}

// Run serves the MCP protocol over stdio until ctx is cancelled or the client
// disconnects. MCP frames the protocol on stdout, so callers must ensure logs
// go to stderr — a stray stdout write corrupts the JSON-RPC stream.
func Run(ctx context.Context, cfg Config) error {
	return NewServer(cfg).Run(ctx, &mcpsdk.StdioTransport{})
}

// NewHTTPHandler builds an http.Handler serving the MCP streamable-HTTP
// transport backed by a single server with the configured tool catalog. The
// embedded in-operator deployment mounts this on its own listener; the same
// server instance is reused across sessions.
func NewHTTPHandler(cfg Config) http.Handler {
	// The HTTP transport is network-reachable and unauthenticated, so a
	// caller-supplied snippet's imports must not escape the library paths.
	cfg.ConfineImports = true
	server := NewServer(cfg)
	return mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return server },
		// SessionTimeout bounds idle sessions: the endpoint is network-reachable
		// and unauthenticated, and a stateful session created by an initialize
		// POST lives in the handler's session map until an explicit DELETE. A
		// client that just drops its connection (a crashed/reconnecting agent)
		// or an attacker minting sessions in a loop would otherwise grow that
		// map without bound until the pod is OOMKilled. With the zero value the
		// SDK never closes idle sessions, so we set an explicit idle bound.
		&mcpsdk.StreamableHTTPOptions{Logger: cfg.Logger, SessionTimeout: mcpSessionIdleTimeout},
	)
}

// mcpSessionIdleTimeout is how long an idle streamable-HTTP session is retained
// before the SDK closes it and reclaims its server-side state.
const mcpSessionIdleTimeout = 10 * time.Minute
