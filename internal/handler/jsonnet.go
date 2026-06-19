/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package handler implements the HTTP surface: the Jsonnet evaluation
// endpoint plus the startup, readiness, and liveness probes.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-jsonnet"

	"github.com/metio/jaas/internal/eval"
)

const extVarPrefix = "JAAS_EXT_VAR_"

// Stable machine-readable identifiers for the error bodies returned on non-2xx
// responses. They surface as the `code` extension member of the RFC 9457
// problem+json body and are part of the HTTP contract — programmatic callers
// (e.g. the flux-jaas-controller bridge) match on these. Adding a new code is
// a backwards-compatible change; renaming or removing one is not.
const (
	ErrCodeMethodNotAllowed      = "method_not_allowed"
	ErrCodeSnippetNotFound       = "snippet_not_found"
	ErrCodeEvaluationTimeout     = "evaluation_timeout"
	ErrCodeEvaluationFailed      = "evaluation_failed"
	ErrCodeEvaluationUnavailable = "evaluation_unavailable"
)

// problemTypeBase is the prefix for the RFC 9457 `type` URI. The canonical
// `type` for a given error is problemTypeBase + its short code.
const problemTypeBase = "https://jaas.projects.metio.wtf/errors/"

// ProblemDetails is the RFC 9457 (application/problem+json) wire shape of a
// non-2xx body. `type` and `code` are intentionally both present: `type` is the
// standards-defined URI (problemTypeBase + code), while `code` is the short,
// stable machine identifier programmatic callers match on.
type ProblemDetails struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
	// Snippet is the requested snippet name, when one was parsed from the URL.
	// Empty for failures that occur before snippet resolution.
	Snippet string `json:"snippet,omitempty"`
}

type Config struct {
	Snippets           []string
	SnippetDirectories []string
	LibraryPaths       []string
	ExtVars            map[string]string
	EvaluationTimeout  time.Duration
	MaxStack           int
	Logger             *slog.Logger
}

func JsonnetHandler(cfg Config) http.HandlerFunc {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(writer http.ResponseWriter, request *http.Request) {
		ctx := request.Context()

		if request.Method != http.MethodGet {
			logger.ErrorContext(ctx, "Unsupported HTTP method used", slog.String("method", request.Method))
			writeProblem(ctx, logger, writer, http.StatusMethodNotAllowed,
				ErrCodeMethodNotAllowed, "Method not allowed", "only GET is supported", "")
			return
		}

		snippetName := request.PathValue("snippet")
		logger.DebugContext(ctx, "Extracted snippet name", slog.String("snippet-name", snippetName))

		fileName, ok := resolveSnippet(snippetName, cfg.Snippets, cfg.SnippetDirectories)
		if !ok {
			logger.ErrorContext(ctx, "Snippet not found", slog.String("snippet-name", snippetName))
			writeProblem(ctx, logger, writer, http.StatusNotFound,
				ErrCodeSnippetNotFound, "Snippet not found",
				fmt.Sprintf("snippet %q not found", snippetName), snippetName)
			return
		}
		logger.DebugContext(ctx, "Resolved snippet", slog.String("snippet-name", snippetName), slog.String("file-name", fileName))

		queryParams := request.URL.Query()
		logger.DebugContext(ctx, "Extracted query parameters", slog.Any("queryParams", queryParams))

		jsonStr, err := eval.EvaluateFile(ctx, fileName, eval.Options{
			ExtVars:  cfg.ExtVars,
			TLAs:     queryParams,
			MaxStack: cfg.MaxStack,
			Timeout:  cfg.EvaluationTimeout,
			// go-jsonnet's FileImporter walks JPaths in reverse order:
			// when the same library name resolves under multiple
			// -library-path entries the *rightmost* path wins. The
			// README documents this; `TestLibraryPathPrecedence_*`
			// pins it against FileImporter directly.
			Importer: &jsonnet.FileImporter{JPaths: cfg.LibraryPaths},
		})
		switch {
		case errors.Is(err, eval.ErrEvalUnavailable):
			logger.WarnContext(ctx, "Jsonnet evaluation rejected; concurrent-eval cap is full",
				slog.String("file-name", fileName))
			writeProblem(ctx, logger, writer, http.StatusServiceUnavailable,
				ErrCodeEvaluationUnavailable, "Evaluation unavailable",
				"concurrent-eval cap is full; retry after backoff", snippetName)
			return
		case errors.Is(err, context.DeadlineExceeded):
			logger.ErrorContext(ctx, "Jsonnet evaluation timed out",
				slog.Duration("timeout", cfg.EvaluationTimeout),
				slog.String("file-name", fileName))
			writeProblem(ctx, logger, writer, http.StatusGatewayTimeout,
				ErrCodeEvaluationTimeout, "Evaluation timed out",
				fmt.Sprintf("evaluation exceeded %s", cfg.EvaluationTimeout), snippetName)
			return
		case errors.Is(err, context.Canceled):
			logger.WarnContext(ctx, "Jsonnet evaluation cancelled by caller",
				slog.String("file-name", fileName))
			return
		case err != nil:
			// The full go-jsonnet diagnostic is logged but never returned: it
			// embeds on-disk snippet paths and line numbers, so echoing it to an
			// HTTP caller is information disclosure. Owner-facing detail surfaces
			// on the JsonnetSnippet status conditions in operator mode instead.
			logger.ErrorContext(ctx, "Cannot evaluate Jsonnet", slog.Any("error", err))
			writeProblem(ctx, logger, writer, http.StatusBadRequest,
				ErrCodeEvaluationFailed, "Jsonnet evaluation failed", "evaluation failed", snippetName)
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		// #nosec G705 -- response is application/json (header above), not
		// HTML; the body is the snippet's evaluated JSON output.
		if _, err := writer.Write([]byte(jsonStr)); err != nil {
			logger.ErrorContext(ctx, "Cannot write response", slog.Any("error", err))
			return
		}
	}
}

// writeProblem serialises an RFC 9457 problem+json body alongside status. The
// canonical `type` is derived as problemTypeBase + code; `code` carries the
// short stable identifier callers match on. Marshal cannot fail: every field is
// a string or int, which have no Marshal-error path in encoding/json, so the
// error return is intentionally discarded (matches the pattern in applyTLAs).
func writeProblem(ctx context.Context, logger *slog.Logger, w http.ResponseWriter, status int, code, title, detail, snippet string) {
	payload, _ := json.Marshal(ProblemDetails{
		Type:    problemTypeBase + code,
		Title:   title,
		Status:  status,
		Detail:  detail,
		Code:    code,
		Snippet: snippet,
	})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if _, err := w.Write(payload); err != nil {
		logger.ErrorContext(ctx, "Cannot write error response", slog.Any("error", err))
	}
}

func ParseExtVars(environ []string) map[string]string {
	result := make(map[string]string)
	for _, env := range environ {
		key, value, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(key, extVarPrefix) {
			continue
		}
		result[strings.TrimPrefix(key, extVarPrefix)] = value
	}
	return result
}

func resolveSnippet(name string, snippets []string, snippetDirectories []string) (string, bool) {
	if slices.Contains(snippets, name) {
		return name, true
	}
	if name == "" {
		return "", false
	}
	relative := name + "/main.jsonnet"
	for _, dir := range snippetDirectories {
		root, err := os.OpenRoot(dir)
		if err != nil {
			continue
		}
		_, statErr := root.Stat(relative)
		_ = root.Close()
		if statErr == nil {
			return filepath.Join(dir, name, "main.jsonnet"), true
		}
	}
	return "", false
}
