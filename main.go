/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/metio/jaas/internal/handler"
)

type stringArray []string

func (i *stringArray) String() string {
	return fmt.Sprintf("%v", *i)
}

func (i *stringArray) Set(value string) error {
	*i = append(*i, value)
	return nil
}

var jaasVersion = "development"

func main() {
	var libraryPaths stringArray
	var snippets stringArray
	var snippetDirectories stringArray
	var version = flag.Bool("version", false, "Print version and exit")
	var logLevel = flag.String("log-level", "info", "The log level to use (debug, info, warn, error)")
	var listenAddress = flag.String("listen-address", "127.0.0.1", "The listen address to bind to for the Jsonnet server")
	var port = flag.String("port", "8080", "The port to bind to for the Jsonnet server")
	var jsonnetEndpointPath = flag.String("jsonnet-endpoint-path", "jsonnet", "The path to the jsonnet endpoint")
	var writeTimeout = flag.Duration("write-timeout", 10*time.Second, "The maximum duration before timing out writes of the response in the Jsonnet server")
	var readTimeout = flag.Duration("read-timeout", 10*time.Second, "maximum duration for reading the entire request, including the body in the Jsonnet server")
	var managementListenAddress = flag.String("management-listen-address", "127.0.0.1", "The listen address to bind to for the management server")
	var managementPort = flag.String("management-port", "8081", "The port to bind to for the management server")
	var managementWriteTimeout = flag.Duration("management-write-timeout", 10*time.Second, "The maximum duration before timing out writes of the response in the management server")
	var managementReadTimeout = flag.Duration("management-read-timeout", 10*time.Second, "maximum duration for reading the entire request, including the body in the management server")
	var evaluationTimeout = flag.Duration("evaluation-timeout", 5*time.Second, "Maximum duration a single Jsonnet evaluation is allowed to take. Set to 0 to disable.")
	var maxStack = flag.Int("max-stack", 500, "Maximum Jsonnet call-stack depth. Set to 0 to use go-jsonnet's default.")
	flag.Var(&libraryPaths, "library-path", "The path of a directory containing jsonnet libraries (can be specified multiple times). Rightmost matching library will be used.")
	flag.Var(&snippets, "snippet", "The path of a jsonnet file or directory containing snippets (can be specified multiple times). Snippets will be loaded from the given path, where the file name is the snippet name.")
	flag.Var(&snippetDirectories, "snippet-directory", "The path of a directory containing snippets as subdirectories (can be specified multiple times). Snippets will be loaded from subdirectories of the given path, where the directory name is the snippet name.")
	flag.Parse()

	if *version {
		fmt.Println(jaasVersion)
		os.Exit(0)
	}

	configureLogger(logLevel)

	ctx, cancel := context.WithCancel(context.Background())

	slog.InfoContext(ctx, "CLI flags parsed",
		slog.String("log-level", *logLevel),
		slog.String("listen-address", *listenAddress),
		slog.String("port", *port),
		slog.String("jsonnet-endpoint-path", *jsonnetEndpointPath),
		slog.Duration("write-timeout", *writeTimeout),
		slog.Duration("read-timeout", *readTimeout),
		slog.Any("library-paths", libraryPaths),
		slog.Any("snippets", snippets),
		slog.Any("snippets-directories", snippetDirectories),
		slog.String("management-listen-address", *managementListenAddress),
		slog.String("management-port", *managementPort),
		slog.Duration("management-write-timeout", *managementWriteTimeout),
		slog.Duration("management-read-timeout", *managementReadTimeout),
		slog.Duration("evaluation-timeout", *evaluationTimeout),
		slog.Int("max-stack", *maxStack))

	jsonnetMux := http.NewServeMux()
	jsonnetMux.HandleFunc(fmt.Sprintf("/%s/{snippet...}", *jsonnetEndpointPath), handler.JsonnetHandler(handler.Config{
		Snippets:           snippets,
		SnippetDirectories: snippetDirectories,
		LibraryPaths:       libraryPaths,
		EvaluationTimeout:  *evaluationTimeout,
		MaxStack:           *maxStack,
	}))
	slog.DebugContext(ctx, "Jsonnet handler configured")

	jsonnetServer := &http.Server{
		Addr:         fmt.Sprintf("%s:%s", *listenAddress, *port),
		WriteTimeout: *writeTimeout,
		ReadTimeout:  *readTimeout,
		Handler:      jsonnetMux,
	}
	slog.DebugContext(ctx, "Jsonnet server created")

	state := handler.NewHealthState()
	managementMux := http.NewServeMux()
	managementMux.HandleFunc("/start", handler.StartupHandler(state))
	managementMux.HandleFunc("/ready", handler.ReadinessHandler(state))
	managementMux.HandleFunc("/live", handler.LivenessHandler())
	slog.DebugContext(ctx, "Management handlers configured")

	managementServer := &http.Server{
		Addr:         fmt.Sprintf("%s:%s", *managementListenAddress, *managementPort),
		WriteTimeout: *managementWriteTimeout,
		ReadTimeout:  *managementReadTimeout,
		Handler:      managementMux,
	}
	slog.DebugContext(ctx, "Management server created")

	managementListener, err := net.Listen("tcp", managementServer.Addr)
	if err != nil {
		slog.ErrorContext(ctx, "Cannot bind management listener", slog.String("addr", managementServer.Addr), slog.Any("error", err))
		os.Exit(1)
	}

	jsonnetListener, err := net.Listen("tcp", jsonnetServer.Addr)
	if err != nil {
		_ = managementListener.Close()
		slog.ErrorContext(ctx, "Cannot bind jsonnet listener", slog.String("addr", jsonnetServer.Addr), slog.Any("error", err))
		os.Exit(1)
	}

	state.MarkStarted()
	state.SetReady(true)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	serverErrs := make(chan error, 2)

	go func() {
		if err := jsonnetServer.Serve(jsonnetListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrs <- fmt.Errorf("jsonnet server: %w", err)
		}
	}()
	slog.DebugContext(ctx, "Jsonnet server started")

	go func() {
		if err := managementServer.Serve(managementListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrs <- fmt.Errorf("management server: %w", err)
		}
	}()
	slog.DebugContext(ctx, "Management server started")

	select {
	case sig := <-sigs:
		slog.InfoContext(ctx, "Received signal, shutting down", slog.String("signal", sig.String()))
	case err := <-serverErrs:
		slog.ErrorContext(ctx, "Server error, shutting down", slog.Any("error", err))
	}

	state.SetReady(false)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := jsonnetServer.Shutdown(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "Cannot shut down Jsonnet server", slog.Any("error", err))
	}
	if err := managementServer.Shutdown(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "Cannot shut down management server", slog.Any("error", err))
	}

	cancel()
	slog.InfoContext(ctx, "JaaS service has shut down")
}

func configureLogger(logLevel *string) {
	logHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(*logLevel),
	})
	slog.SetDefault(slog.New(logHandler))
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "error":
		return slog.LevelError
	case "warn":
		return slog.LevelWarn
	case "debug":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}
