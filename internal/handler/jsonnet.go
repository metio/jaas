/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/go-jsonnet"
)

const extVarPrefix = "JAAS_EXT_VAR_"

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
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		snippetName := request.PathValue("snippet")
		logger.DebugContext(ctx, "Extracted snippet name", slog.String("snippet-name", snippetName))

		fileName, ok := resolveSnippet(snippetName, cfg.Snippets, cfg.SnippetDirectories)
		if !ok {
			logger.ErrorContext(ctx, "Snippet not found", slog.String("snippet-name", snippetName))
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		logger.DebugContext(ctx, "Resolved snippet", slog.String("snippet-name", snippetName), slog.String("file-name", fileName))

		vm := jsonnet.MakeVM()
		// go-jsonnet's FileImporter walks JPaths in reverse order: when the
		// same library name is reachable under multiple -library-path entries
		// the *rightmost* path wins. The README documents this; the suite
		// `TestLibraryPathPrecedence_*` pins it directly against FileImporter.
		vm.Importer(&jsonnet.FileImporter{JPaths: cfg.LibraryPaths})
		if cfg.MaxStack > 0 {
			vm.MaxStack = cfg.MaxStack
		}

		for key, value := range cfg.ExtVars {
			vm.ExtVar(key, value)
		}

		queryParams := request.URL.Query()
		logger.DebugContext(ctx, "Extracted query parameters", slog.Any("queryParams", queryParams))
		applyTLAVars(vm, queryParams)

		jsonStr, err := evaluateWithDeadline(ctx, func() (string, error) {
			return vm.EvaluateFile(fileName)
		}, cfg.EvaluationTimeout)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			logger.ErrorContext(ctx, "Jsonnet evaluation timed out",
				slog.Duration("timeout", cfg.EvaluationTimeout),
				slog.String("file-name", fileName))
			writer.WriteHeader(http.StatusGatewayTimeout)
			return
		case errors.Is(err, context.Canceled):
			logger.WarnContext(ctx, "Jsonnet evaluation cancelled by caller",
				slog.String("file-name", fileName))
			return
		case err != nil:
			logger.ErrorContext(ctx, "Cannot evaluate Jsonnet", slog.Any("error", err))
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		if _, err := writer.Write([]byte(jsonStr)); err != nil {
			logger.ErrorContext(ctx, "Cannot write response", slog.Any("error", err))
			return
		}
	}
}

func evaluateWithDeadline(ctx context.Context, eval func() (string, error), timeout time.Duration) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := eval()
		ch <- result{out: out, err: err}
	}()

	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
		return "", ctx.Err()
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

// applyTLAVars maps URL query parameters onto the VM's Top Level Arguments.
// Single-valued parameters become string TLAs via TLAVar; multi-valued ones
// become JSON-array TLAs via TLACode. The encoding cannot fail: url.Values's
// element type is []string, which has no Marshal-error path in encoding/json,
// so the error return value is intentionally discarded.
func applyTLAVars(vm *jsonnet.VM, queryParams url.Values) {
	for key, value := range queryParams {
		if len(value) == 1 {
			vm.TLAVar(key, value[0])
			continue
		}
		bytes, _ := json.Marshal(value)
		vm.TLACode(key, string(bytes))
	}
}
