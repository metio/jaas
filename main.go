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
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
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

// Overwritten at link time via `-ldflags="-X main.version=... -X main.commit=..."` by
// the release workflow. For local builds `version` stays "development", and `commit`
// is refined in init() by reading the VCS revision from runtime/debug — so a plain
// `go build` shows the real SHA without needing any flags. The sentinel below marks
// "no linker override; ask buildinfo".
const commitSentinel = "unknown"

var (
	version = "development"
	commit  = commitSentinel
)

func init() {
	info, ok := debug.ReadBuildInfo()
	commit = resolveCommit(commit, info, ok)
}

// resolveCommit picks the linker-supplied value if it differs from the sentinel,
// otherwise it pulls vcs.revision out of buildinfo, appending "-dirty" if the
// worktree had uncommitted changes at build time. Returns the sentinel when no
// revision is available.
func resolveCommit(linker string, info *debug.BuildInfo, ok bool) string {
	if linker != commitSentinel {
		return linker
	}
	if !ok || info == nil {
		return commitSentinel
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return commitSentinel
	}
	if modified == "true" {
		return rev + "-dirty"
	}
	return rev
}

func main() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr, sigs))
}

// run is the testable seam under main. All process-affecting side effects
// (flag parsing, signal handling, stdout writes, slog.Default mutation) flow
// through its parameters so tests can drive them in isolation.
//
// Return value follows the Unix convention: 0 success, 1 runtime failure
// (bind / shutdown error surfaced before normal exit), 2 flag parse error.
func run(args, env []string, stdout, stderr io.Writer, sigs <-chan os.Signal) int {
	fs := flag.NewFlagSet("jaas", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var libraryPaths stringArray
	var snippets stringArray
	var snippetDirectories stringArray
	var showVersion = fs.Bool("version", false, "Print version and exit")
	var logLevel = fs.String("log-level", "info", "The log level to use (debug, info, warn, error)")
	var listenAddress = fs.String("listen-address", "127.0.0.1", "The listen address to bind to for the Jsonnet server")
	var port = fs.String("port", "8080", "The port to bind to for the Jsonnet server")
	var jsonnetEndpointPath = fs.String("jsonnet-endpoint-path", "jsonnet", "The path to the jsonnet endpoint")
	var writeTimeout = fs.Duration("write-timeout", 10*time.Second, "The maximum duration before timing out writes of the response in the Jsonnet server")
	var readTimeout = fs.Duration("read-timeout", 10*time.Second, "maximum duration for reading the entire request, including the body in the Jsonnet server")
	var managementListenAddress = fs.String("management-listen-address", "127.0.0.1", "The listen address to bind to for the management server")
	var managementPort = fs.String("management-port", "8081", "The port to bind to for the management server")
	var managementWriteTimeout = fs.Duration("management-write-timeout", 10*time.Second, "The maximum duration before timing out writes of the response in the management server")
	var managementReadTimeout = fs.Duration("management-read-timeout", 10*time.Second, "maximum duration for reading the entire request, including the body in the management server")
	var evaluationTimeout = fs.Duration("evaluation-timeout", 5*time.Second, "Maximum duration a single Jsonnet evaluation is allowed to take. Set to 0 to disable.")
	var maxStack = fs.Int("max-stack", 500, "Maximum Jsonnet call-stack depth. Set to 0 to use go-jsonnet's default.")
	var shutdownDelay = fs.Duration("shutdown-delay", 5*time.Second, "Time to wait after readiness flips to false before initiating graceful shutdown; gives Kubernetes time to propagate the not-ready status to endpoint controllers. Set to 0 to disable.")
	fs.Var(&libraryPaths, "library-path", "The path of a directory containing jsonnet libraries (can be specified multiple times). Rightmost matching library will be used.")
	fs.Var(&snippets, "snippet", "The path of a jsonnet file or directory containing snippets (can be specified multiple times). Snippets will be loaded from the given path, where the file name is the snippet name.")
	fs.Var(&snippetDirectories, "snippet-directory", "The path of a directory containing snippets as subdirectories (can be specified multiple times). Snippets will be loaded from subdirectories of the given path, where the directory name is the snippet name.")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintf(stdout, "version: %s\ncommit:  %s\n", version, commit)
		return 0
	}

	configureLogger(stdout, *logLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slog.InfoContext(ctx, "Starting JaaS",
		slog.String("version", version),
		slog.String("commit", commit))

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
		slog.Int("max-stack", *maxStack),
		slog.Duration("shutdown-delay", *shutdownDelay))

	extVars := handler.ParseExtVars(env)
	slog.InfoContext(ctx, "External variables loaded", slog.Int("count", len(extVars)))

	jsonnetMux := http.NewServeMux()
	jsonnetMux.HandleFunc(fmt.Sprintf("/%s/{snippet...}", *jsonnetEndpointPath), handler.JsonnetHandler(handler.Config{
		Snippets:           snippets,
		SnippetDirectories: snippetDirectories,
		LibraryPaths:       libraryPaths,
		ExtVars:            extVars,
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
		return 1
	}

	jsonnetListener, err := net.Listen("tcp", jsonnetServer.Addr)
	if err != nil {
		_ = managementListener.Close()
		slog.ErrorContext(ctx, "Cannot bind jsonnet listener", slog.String("addr", jsonnetServer.Addr), slog.Any("error", err))
		return 1
	}

	state.MarkStarted()
	state.SetReady(true)

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

	exitCode := 0
	select {
	case sig := <-sigs:
		slog.InfoContext(ctx, "Received signal, shutting down", slog.String("signal", sig.String()))
	case err := <-serverErrs:
		slog.ErrorContext(ctx, "Server error, shutting down", slog.Any("error", err))
		exitCode = 1
	}

	drainBeforeShutdown(state, *shutdownDelay, slog.Default())

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := jsonnetServer.Shutdown(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "Cannot shut down Jsonnet server", slog.Any("error", err))
		exitCode = 1
	}
	if err := managementServer.Shutdown(shutdownCtx); err != nil {
		slog.ErrorContext(ctx, "Cannot shut down management server", slog.Any("error", err))
		exitCode = 1
	}

	slog.InfoContext(ctx, "JaaS service has shut down")
	return exitCode
}

func configureLogger(out io.Writer, logLevel string) {
	logHandler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: parseLogLevel(logLevel),
	})
	slog.SetDefault(slog.New(logHandler))
}

// drainBeforeShutdown flips readiness off and (if delay > 0) blocks for `delay`
// so Kubernetes can propagate the not-ready status to its endpoint controllers
// before in-flight requests start being aborted by Shutdown.
func drainBeforeShutdown(state *handler.HealthState, delay time.Duration, logger *slog.Logger) {
	state.SetReady(false)
	if delay <= 0 {
		return
	}
	logger.Info("Draining: waiting for readiness to propagate before shutdown", slog.Duration("delay", delay))
	time.Sleep(delay)
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
