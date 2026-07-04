/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"bytes"
	"os"
	"testing"
)

// The `mcp` subcommand is dispatched from run() before the main flag set, and
// its own flag parsing follows the same exit-code convention: --help is 0, an
// unknown flag is 2, and a runtime input error (bad --ext-var) is 1. Each of
// these returns before the blocking stdio transport starts, so they're safe to
// assert directly. The protocol behavior itself is covered by the in-memory
// round-trip test in internal/mcp.

func TestRun_MCPSubcommand_HelpReturnsZero(t *testing.T) {
	withRestoredSlogDefault(t)
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	if code := run([]string{"mcp", "--help"}, nil, &stdout, &stderr, sigs); code != 0 {
		t.Errorf("exit code = %d, want 0 for `mcp --help`; stderr=%q", code, stderr.String())
	}
}

func TestRun_MCPSubcommand_UnknownFlagReturnsTwo(t *testing.T) {
	withRestoredSlogDefault(t)
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	if code := run([]string{"mcp", "--nonexistent"}, nil, &stdout, &stderr, sigs); code != 2 {
		t.Errorf("exit code = %d, want 2 for an unknown `mcp` flag; stderr=%q", code, stderr.String())
	}
}

func TestRun_MCPSubcommand_InvalidExtVarReturnsTwo(t *testing.T) {
	withRestoredSlogDefault(t)
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	// `FOO` has no '=', so ParseExtVars rejects it — flag misuse (exit 2),
	// surfaced before the stdio transport starts.
	if code := run([]string{"mcp", "--ext-var", "FOO"}, nil, &stdout, &stderr, sigs); code != 2 {
		t.Errorf("exit code = %d, want 2 for an invalid --ext-var; stderr=%q", code, stderr.String())
	}
}

// The embedded MCP server introspects operator resources, so --enable-mcp is a
// flag-usage error (exit 2) without --enable-flux-integration — the same shape
// as the --enable-webhook guard.
func TestRun_EnableMCPWithoutFluxIntegrationFailsWithExit2(t *testing.T) {
	withRestoredSlogDefault(t)
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	if code := run([]string{"--enable-mcp"}, nil, &stdout, &stderr, sigs); code != 2 {
		t.Errorf("exit code = %d, want 2 for --enable-mcp without --enable-flux-integration; stderr=%q", code, stderr.String())
	}
}

// --mcp-allow-mutations is meaningless without --enable-mcp, so it is a
// flag-usage error (exit 2).
func TestRun_MCPAllowMutationsWithoutEnableMCPFailsWithExit2(t *testing.T) {
	withRestoredSlogDefault(t)
	var stdout, stderr bytes.Buffer
	sigs := make(chan os.Signal, 1)
	if code := run([]string{"--enable-flux-integration", "--mcp-allow-mutations"}, nil, &stdout, &stderr, sigs); code != 2 {
		t.Errorf("exit code = %d, want 2 for --mcp-allow-mutations without --enable-mcp; stderr=%q", code, stderr.String())
	}
}
