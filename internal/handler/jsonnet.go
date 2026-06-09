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

// Stable machine-readable identifiers for the JSON error bodies returned on
// non-2xx responses. They are part of the HTTP contract — programmatic callers
// (e.g. the flux-jaas-controller bridge) match on these. Adding a new code is
// a backwards-compatible change; renaming or removing one is not.
const (
	ErrCodeMethodNotAllowed      = "method_not_allowed"
	ErrCodeSnippetNotFound       = "snippet_not_found"
	ErrCodeEvaluationTimeout     = "evaluation_timeout"
	ErrCodeEvaluationFailed      = "evaluation_failed"
	ErrCodeEvaluationUnavailable = "evaluation_unavailable"
)

// ErrorResponse is the wire shape of a non-2xx body.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
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
			writeJSONError(ctx, logger, writer, http.StatusMethodNotAllowed, ErrorResponse{
				Error:   ErrCodeMethodNotAllowed,
				Message: "only GET is supported",
			})
			return
		}

		snippetName := request.PathValue("snippet")
		logger.DebugContext(ctx, "Extracted snippet name", slog.String("snippet-name", snippetName))

		fileName, ok := resolveSnippet(snippetName, cfg.Snippets, cfg.SnippetDirectories)
		if !ok {
			logger.ErrorContext(ctx, "Snippet not found", slog.String("snippet-name", snippetName))
			writeJSONError(ctx, logger, writer, http.StatusNotFound, ErrorResponse{
				Error:   ErrCodeSnippetNotFound,
				Message: fmt.Sprintf("snippet %q not found", snippetName),
				Snippet: snippetName,
			})
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
			writeJSONError(ctx, logger, writer, http.StatusServiceUnavailable, ErrorResponse{
				Error:   ErrCodeEvaluationUnavailable,
				Message: "concurrent-eval cap is full; retry after backoff",
				Snippet: snippetName,
			})
			return
		case errors.Is(err, context.DeadlineExceeded):
			logger.ErrorContext(ctx, "Jsonnet evaluation timed out",
				slog.Duration("timeout", cfg.EvaluationTimeout),
				slog.String("file-name", fileName))
			writeJSONError(ctx, logger, writer, http.StatusGatewayTimeout, ErrorResponse{
				Error:   ErrCodeEvaluationTimeout,
				Message: fmt.Sprintf("evaluation exceeded %s", cfg.EvaluationTimeout),
				Snippet: snippetName,
			})
			return
		case errors.Is(err, context.Canceled):
			logger.WarnContext(ctx, "Jsonnet evaluation cancelled by caller",
				slog.String("file-name", fileName))
			return
		case err != nil:
			logger.ErrorContext(ctx, "Cannot evaluate Jsonnet", slog.Any("error", err))
			writeJSONError(ctx, logger, writer, http.StatusBadRequest, ErrorResponse{
				Error:   ErrCodeEvaluationFailed,
				Message: err.Error(),
				Snippet: snippetName,
			})
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

// writeJSONError serialises body as the response payload alongside status.
// Marshal cannot fail: ErrorResponse's fields are all strings, which have no
// Marshal-error path in encoding/json, so the error return is intentionally
// discarded (matches the pattern in applyTLAs).
func writeJSONError(ctx context.Context, logger *slog.Logger, w http.ResponseWriter, status int, body ErrorResponse) {
	payload, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
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
