/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/metio/jaas/internal/eval"
	"github.com/metio/jaas/internal/handler"
	"github.com/metio/jaas/internal/mcp"
	"github.com/metio/jaas/internal/observability"
	"github.com/metio/jaas/internal/operator"
)

// runMCP serves the jaas MCP server over stdio. It is dispatched from run()
// when the first CLI argument is "mcp". Its flag set is a focused subset of
// the main binary's — only the knobs that affect cluster-free rendering.
//
// Exit codes follow run()'s convention: 0 success, 1 runtime failure, 2 flag
// parse error.
func runMCP(args, env []string, _, stderr io.Writer, sigs <-chan os.Signal) int {
	fs := pflag.NewFlagSet("jaas mcp", pflag.ContinueOnError)
	fs.SetOutput(stderr)

	libraryPaths := fs.StringArray("library-path", nil, "Directory of jsonnet libraries for import resolution (repeatable; rightmost matching path wins).")
	extVarFlags := fs.StringArray("ext-var", nil, "External variable as KEY=VALUE for std.extVar lookups (repeatable). Takes precedence over JAAS_EXT_VAR_* env vars on conflict.")
	maxStack := fs.Int("max-stack", 500, "Maximum Jsonnet call-stack depth. Set to 0 to use go-jsonnet's default.")
	evalTimeout := fs.Duration("evaluation-timeout", 5*time.Second, "Maximum duration a single Jsonnet evaluation is allowed to take. Set to 0 to disable.")
	logLevel := fs.String("log-level", "info", "The log level to use (debug, info, warn, error).")
	logFormat := fs.String("log-format", "json", "The log output format to use (json, text).")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0
		}
		return 2
	}

	// MCP frames JSON-RPC on stdout, so every log line MUST go to stderr or
	// it corrupts the protocol stream. Pin the logger to stderr regardless of
	// the normal stdout default. The same logger is handed to the MCP server
	// (below) so the SDK's own activity logs share jaas's format and level.
	logger := observability.NewLogger(stderr, *logLevel, *logFormat)
	slog.SetDefault(logger)

	cliExtVars, err := operator.ParseExtVars(*extVarFlags)
	if err != nil {
		fmt.Fprintf(stderr, "Invalid --ext-var: %v\n", err)
		return 1
	}
	// CLI --ext-var overlays env-derived JAAS_EXT_VAR_* on key conflicts,
	// matching the main binary.
	extVars := handler.ParseExtVars(env)
	for k, v := range cliExtVars {
		extVars[k] = v
	}

	// Bound concurrent evaluations with the same default the HTTP path uses so
	// a runaway snippet from a client can't pile up unbounded goroutines.
	eval.SetMaxConcurrentEvals(defaultMaxConcurrentEvals())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Translate a SIGINT/SIGTERM into ctx cancellation so the stdio server
	// closes its connection and Run returns.
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	slog.InfoContext(ctx, "Starting JaaS MCP server",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.Any("library-paths", *libraryPaths),
		slog.Int("ext-vars", len(extVars)),
		slog.Int("max-stack", *maxStack),
		slog.Duration("evaluation-timeout", *evalTimeout))

	if err := mcp.Run(ctx, mcp.Config{
		LibraryPaths:      *libraryPaths,
		ExtVars:           extVars,
		MaxStack:          *maxStack,
		EvaluationTimeout: *evalTimeout,
		Version:           version,
		Logger:            logger,
	}); err != nil && !errors.Is(err, context.Canceled) {
		slog.ErrorContext(ctx, "MCP server error", slog.Any("error", err))
		return 1
	}

	slog.InfoContext(ctx, "JaaS MCP server has shut down")
	return 0
}
