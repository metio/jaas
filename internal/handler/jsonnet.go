package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/go-jsonnet"
	"log/slog"
	"net/http"
	"os"
	"slices"
)

func JsonnetHandler(ctx context.Context, snippets []string, snippetDirectories []string, libraryPaths []string) http.HandlerFunc {
	importer := &jsonnet.FileImporter{
		JPaths: libraryPaths,
	}

	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			slog.ErrorContext(ctx, "Unsupported HTTP method used", slog.String("method", request.Method))
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		vm := jsonnet.MakeVM()
		vm.Importer(importer)

		queryParams := request.URL.Query()
		slog.DebugContext(ctx, "Extracted query parameters", slog.Any("queryParams", queryParams))
		for key, value := range queryParams {
			if len(value) == 0 {
				vm.ExtVar(key, "")
			} else if len(value) == 1 {
				vm.ExtVar(key, value[0])
			} else {
				bytes, err := json.Marshal(value)
				if err != nil {
					slog.ErrorContext(ctx, "Cannot marshal query parameter value", slog.String("key", key), slog.Any("value", value), slog.Any("error", err))
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				vm.ExtCode(key, string(bytes))
			}
		}

		snippetName := request.PathValue("snippet")
		slog.DebugContext(ctx, "Extracted snippet name", slog.String("snippet-name", snippetName))

		var fileName string
		if slices.Contains(snippets, snippetName) {
			fileName = snippetName
			slog.DebugContext(ctx, "Found exact match for snippet name", slog.String("snippet-name", snippetName))
		} else {
			for _, dir := range snippetDirectories {
				jsonnetPath := fmt.Sprintf("%s/%s/main.jsonnet", dir, snippetName)
				if _, err := os.Stat(jsonnetPath); err == nil {
					fileName = jsonnetPath
					slog.DebugContext(ctx, "Found snippet in directory", slog.String("snippet-name", snippetName), slog.String("file-name", fileName))
					break
				}
			}
		}

		if fileName == "" {
			slog.ErrorContext(ctx, "Snippet not found", slog.String("snippet-name", snippetName))
			writer.WriteHeader(http.StatusNotFound)
			return
		}

		jsonStr, err := vm.EvaluateFile(fileName)
		if err != nil {
			slog.ErrorContext(ctx, "Cannot evaluate Jsonnet", slog.Any("error", err))
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		writer.WriteHeader(http.StatusOK)
		writer.Header().Set("Content-Type", "application/json")
		_, err = writer.Write([]byte(jsonStr))
		if err != nil {
			slog.ErrorContext(ctx, "Cannot write response", slog.Any("error", err))
			return
		}
	}
}
