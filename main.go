/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
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
		slog.Duration("management-read-timeout", *managementReadTimeout))

	jsonnetMux := http.NewServeMux()
	jsonnetMux.HandleFunc(fmt.Sprintf("/%s/{snippet...}", *jsonnetEndpointPath), handler.JsonnetHandler(ctx, snippets, snippetDirectories, libraryPaths))
	slog.DebugContext(ctx, "Jsonnet handler configured")

	jsonnetServer := &http.Server{
		Addr:         fmt.Sprintf("%s:%s", *listenAddress, *port),
		WriteTimeout: *writeTimeout,
		ReadTimeout:  *readTimeout,
		Handler:      jsonnetMux,
	}
	slog.DebugContext(ctx, "Jsonnet server created")

	healthHandler := handler.HealthHandler()
	managementMux := http.NewServeMux()
	managementMux.HandleFunc("/ready", healthHandler)
	managementMux.HandleFunc("/live", healthHandler)
	slog.DebugContext(ctx, "Management handlers configured")

	managementServer := &http.Server{
		Addr:         fmt.Sprintf("%s:%s", *managementListenAddress, *managementPort),
		WriteTimeout: *managementWriteTimeout,
		ReadTimeout:  *managementReadTimeout,
		Handler:      managementMux,
	}
	slog.DebugContext(ctx, "Management server created")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	go func() {
		jsonnetServer.ListenAndServe()
	}()

	go func() {
		managementServer.ListenAndServe()
	}()

	defer func() {
		if err := jsonnetServer.Shutdown(ctx); err != nil {
			slog.ErrorContext(ctx, "Cannot shut down Jsonnet server", slog.Any("error", err))
		}
		if err := managementServer.Shutdown(ctx); err != nil {
			slog.ErrorContext(ctx, "Cannot shut down management server", slog.Any("error", err))
		}
	}()

	sig := <-sigs
	fmt.Println(sig)

	cancel()
	slog.InfoContext(ctx, "JaaS service has shut down")
}

func configureLogger(logLevel *string) {
	var level slog.Level
	switch strings.ToLower(*logLevel) {
	case "error":
		level = slog.LevelError
	case "warn":
		level = slog.LevelWarn
	case "debug":
		level = slog.LevelDebug
	case "info":
	default:
		level = slog.LevelInfo
	}
	logHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(logHandler))
}
