/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-jsonnet"
)

const extVarPrefix = "JAAS_EXT_VAR_"

func JsonnetHandler(snippets []string, snippetDirectories []string, libraryPaths []string) http.HandlerFunc {
	importer := &jsonnet.FileImporter{
		JPaths: libraryPaths,
	}

	return func(writer http.ResponseWriter, request *http.Request) {
		ctx := request.Context()

		if request.Method != http.MethodGet {
			slog.ErrorContext(ctx, "Unsupported HTTP method used", slog.String("method", request.Method))
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		snippetName := request.PathValue("snippet")
		slog.DebugContext(ctx, "Extracted snippet name", slog.String("snippet-name", snippetName))

		fileName, ok := resolveSnippet(snippetName, snippets, snippetDirectories)
		if !ok {
			slog.ErrorContext(ctx, "Snippet not found", slog.String("snippet-name", snippetName))
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		slog.DebugContext(ctx, "Resolved snippet", slog.String("snippet-name", snippetName), slog.String("file-name", fileName))

		vm := jsonnet.MakeVM()
		vm.Importer(importer)

		for key, value := range parseExtVars(os.Environ()) {
			vm.ExtVar(key, value)
			slog.DebugContext(ctx, "Set external variable", slog.String("key", key), slog.String("value", value))
		}

		queryParams := request.URL.Query()
		slog.DebugContext(ctx, "Extracted query parameters", slog.Any("queryParams", queryParams))
		if err := applyTLAVars(vm, queryParams); err != nil {
			slog.ErrorContext(ctx, "Cannot apply TLA variables", slog.Any("error", err))
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		jsonStr, err := vm.EvaluateFile(fileName)
		if err != nil {
			slog.ErrorContext(ctx, "Cannot evaluate Jsonnet", slog.Any("error", err))
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		if _, err := writer.Write([]byte(jsonStr)); err != nil {
			slog.ErrorContext(ctx, "Cannot write response", slog.Any("error", err))
			return
		}
	}
}

func parseExtVars(environ []string) map[string]string {
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

func applyTLAVars(vm *jsonnet.VM, queryParams url.Values) error {
	for key, value := range queryParams {
		if len(value) == 1 {
			vm.TLAVar(key, value[0])
			continue
		}
		bytes, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal query parameter %q: %w", key, err)
		}
		vm.TLACode(key, string(bytes))
	}
	return nil
}
