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
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/cliflags"
	"github.com/metio/jaas/internal/eval"
	"github.com/metio/jaas/internal/handler"
	"github.com/metio/jaas/internal/mcp"
	"github.com/metio/jaas/internal/observability"
	"github.com/metio/jaas/internal/operator"
	"github.com/metio/jaas/internal/storage"
	"github.com/metio/jaas/internal/webhook/selfsigned"
)

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
// (bind / shutdown error surfaced before normal exit), 2 flag misuse — a parse
// error, an out-of-set flag value, or a missing required flag combination.
func run(args, env []string, stdout, stderr io.Writer, sigs <-chan os.Signal) int {
	// The `mcp` subcommand serves the cluster-free Jsonnet renderer over the
	// Model Context Protocol on stdio. It owns a focused flag subset, so it is
	// dispatched before the main flag set parses.
	if len(args) > 0 && args[0] == "mcp" {
		return runMCP(args[1:], env, stdout, stderr, sigs)
	}

	fs := pflag.NewFlagSet("jaas", pflag.ContinueOnError)
	fs.SetOutput(stderr)

	f := cliflags.Register(fs, defaultMaxConcurrentEvals)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *f.ShowVersion {
		// Single Fprintln per line so an empty commit (or version)
		// doesn't leave behind the "label:  \n" double-space artifact
		// the old %s format produced when -ldflags set the value to
		// "".
		fmt.Fprintln(stdout, "version:", version)
		fmt.Fprintln(stdout, "commit:", commit)
		return 0
	}

	// The webhook server is wired only inside the operator boot path, so
	// --enable-webhook without --enable-flux-integration would silently
	// boot an HTTP-only evaluator with no admission validation — a
	// security-relevant no-op. Reject it as a flag error rather than
	// ignore it.
	if *f.EnableWebhook && !*f.EnableFluxIntegration {
		fmt.Fprintln(stderr, "jaas: --enable-webhook requires --enable-flux-integration")
		return 2
	}

	// The MCP server's read tools introspect operator resources, so it only
	// makes sense alongside the operator. Reject the combination explicitly
	// rather than booting an MCP endpoint with nothing to read.
	if *f.EnableMCP && !*f.EnableFluxIntegration {
		fmt.Fprintln(stderr, "jaas: --enable-mcp requires --enable-flux-integration")
		return 2
	}

	// Exposing the MCP write tools without the MCP server is a no-op; reject it
	// so the intent (enable MCP, with mutations) is unambiguous.
	if *f.MCPAllowMutations && !*f.EnableMCP {
		fmt.Fprintln(stderr, "jaas: --mcp-allow-mutations requires --enable-mcp")
		return 2
	}

	if err := f.Validate(); err != nil {
		fmt.Fprintln(stderr, "jaas:", err)
		return 2
	}

	slog.SetDefault(observability.NewLogger(stdout, *f.LogLevel, *f.LogFormat))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tracingShutdown, err := observability.InitTracer(ctx, observability.TracingConfig{
		Endpoint:       *f.TracingEndpoint,
		Insecure:       *f.TracingInsecure,
		ServiceVersion: version,
		SampleRatio:    *f.TracingSampleRatio,
	})
	if err != nil {
		slog.ErrorContext(ctx, "Cannot init tracer", slog.Any("error", err))
		return 1
	}
	defer func() {
		// Bounded shutdown so a slow collector doesn't hang the
		// process — five seconds is generous for a flush.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracingShutdown(shutdownCtx)
	}()

	slog.InfoContext(ctx, "Starting JaaS",
		slog.String("version", version),
		slog.String("commit", commit))

	slog.InfoContext(ctx, "CLI flags parsed",
		slog.String("log-level", *f.LogLevel),
		slog.String("listen-address", *f.ListenAddress),
		slog.String("port", *f.Port),
		slog.String("jsonnet-endpoint-path", *f.JsonnetEndpointPath),
		slog.Duration("write-timeout", *f.WriteTimeout),
		slog.Duration("read-timeout", *f.ReadTimeout),
		slog.Any("library-paths", *f.LibraryPaths),
		slog.Any("snippets", *f.Snippets),
		slog.Any("snippets-directories", *f.SnippetDirectories),
		slog.String("management-listen-address", *f.ManagementListenAddress),
		slog.String("management-port", *f.ManagementPort),
		slog.Duration("management-write-timeout", *f.ManagementWriteTimeout),
		slog.Duration("management-read-timeout", *f.ManagementReadTimeout),
		slog.Duration("evaluation-timeout", *f.EvaluationTimeout),
		slog.Int("max-stack", *f.MaxStack),
		slog.Int("max-concurrent-evals", *f.MaxConcurrentEvals),
		slog.Duration("shutdown-delay", *f.ShutdownDelay),
		slog.Bool("enable-flux-integration", *f.EnableFluxIntegration),
		slog.String("default-service-account", *f.DefaultServiceAccount),
		slog.Bool("no-cross-namespace-refs", *f.NoCrossNamespaceRefs),
		slog.String("label-selector", *f.LabelSelector),
		slog.String("rerender-rate", *f.RerenderRate),
		slog.Int("rerender-burst", *f.RerenderBurst))

	eval.SetMaxConcurrentEvals(*f.MaxConcurrentEvals)

	cliExtVars, err := operator.ParseExtVars(*f.ExtVarFlags)
	if err != nil {
		fmt.Fprintf(stderr, "Invalid --ext-var: %v\n", err)
		return 2
	}

	// CLI --ext-var overlays env-derived JAAS_EXT_VAR_* on key conflicts;
	// the env mechanism predates the flag and is kept for the HTTP-only path.
	extVars := handler.ParseExtVars(env)
	maps.Copy(extVars, cliExtVars)
	slog.InfoContext(ctx, "External variables loaded",
		slog.Int("count", len(extVars)),
		slog.Int("from-cli", len(cliExtVars)))

	var (
		opCfg     operator.Config
		opRestCfg *rest.Config
		opStore   storage.Backend
		// webhookRenewerDone is closed when the self-signed cert renewer
		// goroutine exits; the shutdown path awaits it so a rotation in
		// flight during SIGTERM isn't abandoned mid-patch. Stays nil when
		// the renewer never starts, in which case the await is skipped.
		webhookRenewerDone <-chan struct{}
	)
	if *f.EnableFluxIntegration {
		rerenderRatePerSec, err := operator.ParseRerenderRate(*f.RerenderRate)
		if err != nil {
			fmt.Fprintf(stderr, "Invalid --rerender-rate: %v\n", err)
			return 2
		}
		if *f.RerenderBurst < 1 {
			fmt.Fprintf(stderr, "Invalid --rerender-burst: must be >= 1, got %d\n", *f.RerenderBurst)
			return 2
		}
		if *f.StorageBaseURL == "" {
			fmt.Fprintln(stderr, "Invalid --storage-base-url: required when --enable-flux-integration is set")
			return 2
		}
		var code int
		opStore, code = newStorageBackend(ctx, stderr, *f.StorageBackend, *f.StoragePath, storage.S3Config{
			Endpoint:        *f.S3Endpoint,
			Bucket:          *f.S3Bucket,
			Prefix:          *f.S3Prefix,
			Region:          *f.S3Region,
			UseSSL:          *f.S3UseSSL,
			AccessKeyID:     *f.S3AccessKey,
			SecretAccessKey: *f.S3SecretKey,
			SessionToken:    *f.S3SessionToken,
			UseAnonymous:    *f.S3Anonymous,
			ReadTimeout:     *f.StorageReadTimeout,
		})
		if code != 0 {
			return code
		}
		defer func() { _ = opStore.Close() }()

		opCfg = operator.Config{
			DefaultServiceAccount:   *f.DefaultServiceAccount,
			NoCrossNamespaceRefs:    *f.NoCrossNamespaceRefs,
			LabelSelector:           *f.LabelSelector,
			WatchNamespaces:         parseWatchNamespaces(*f.WatchNamespaces, env),
			RerenderRate:            rerenderRatePerSec,
			RerenderBurst:           *f.RerenderBurst,
			ExtVars:                 extVars,
			EvaluationTimeout:       *f.EvaluationTimeout,
			MaxStack:                *f.MaxStack,
			Store:                   opStore,
			StorageBaseURL:          *f.StorageBaseURL,
			EnableWebhook:           *f.EnableWebhook,
			WebhookCertDir:          *f.WebhookCertDir,
			WebhookPort:             *f.WebhookPort,
			LeaderElection:          *f.LeaderElection,
			LeaderElectionID:        *f.LeaderElectionID,
			LeaderElectionNamespace: *f.LeaderElectionNamespace,
			KnownLibraryAliases:     ociLibraryAliasesFromPaths(*f.LibraryPaths),
			OCILibraries:            loadOCILibraries(ctx, *f.LibraryPaths),
			MetricsBindAddress:      *f.MetricsBindAddress,
			MaxWithdrawWait:         *f.MaxWithdrawWait,
			MaxArtifactBytes:        *f.MaxArtifactBytes,
			ArtifactGCGrace:         *f.ArtifactGCGrace,
			// OnReady is wired below once state is constructed so the
			// probe only flips ready after the manager is reconcile-ready.
		}
		opRestCfg, err = loadKubeconfig(*f.Kubeconfig)
		if err != nil {
			slog.ErrorContext(ctx, "Cannot load kubeconfig", slog.String("kubeconfig", *f.Kubeconfig), slog.Any("error", err))
			return 1
		}

		if *f.EnableWebhook {
			switch *f.WebhookCertMode {
			case "cert-manager":
				// External tooling provisions tls.crt/tls.key under
				// WebhookCertDir. Nothing to do here.
			case "self-signed":
				if *f.WebhookVWCName == "" {
					fmt.Fprintln(stderr, "Invalid --webhook-validating-config-name: required when --webhook-cert-mode=self-signed")
					return 2
				}
				ns := resolveSelfSignedNamespace(*f.WebhookServiceNamespace, *f.LeaderElectionNamespace, env)
				done, err := provisionSelfSignedWebhookCert(ctx, opRestCfg, selfsignedConfig{
					Namespace:   ns,
					ServiceName: *f.WebhookServiceName,
					CertDir:     *f.WebhookCertDir,
					Validity:    *f.WebhookCertValidity,
					VWCName:     *f.WebhookVWCName,
				})
				if err != nil {
					slog.ErrorContext(ctx, "Cannot provision self-signed webhook cert", slog.Any("error", err))
					return 1
				}
				webhookRenewerDone = done
				slog.InfoContext(ctx, "Self-signed webhook cert provisioned",
					slog.String("certDir", *f.WebhookCertDir),
					slog.String("vwc", *f.WebhookVWCName))
			default:
				fmt.Fprintf(stderr, "Invalid --webhook-cert-mode %q: must be \"cert-manager\" or \"self-signed\"\n", *f.WebhookCertMode)
				return 2
			}
		}
	}

	jsonnetMux := http.NewServeMux()
	jsonnetMux.HandleFunc(fmt.Sprintf("/%s/{snippet...}", *f.JsonnetEndpointPath), handler.JsonnetHandler(handler.Config{
		Snippets:           *f.Snippets,
		SnippetDirectories: *f.SnippetDirectories,
		LibraryPaths:       *f.LibraryPaths,
		ExtVars:            extVars,
		EvaluationTimeout:  *f.EvaluationTimeout,
		MaxStack:           *f.MaxStack,
	}))
	slog.DebugContext(ctx, "Jsonnet handler configured")

	jsonnetServer := &http.Server{
		Addr:         net.JoinHostPort(*f.ListenAddress, *f.Port),
		WriteTimeout: *f.WriteTimeout,
		ReadTimeout:  *f.ReadTimeout,
		Handler:      jsonnetMux,
	}
	slog.DebugContext(ctx, "Jsonnet server created")

	state := handler.NewHealthState()
	if *f.EnableFluxIntegration {
		// Capture state.SetReady so the operator manager flips the pod's
		// readiness probe only after mgr.Elected() closes (cache synced,
		// leader elected — or LE off). Set here, post-state-construction.
		opCfg.OnReady = func() { state.SetReady(true) }
	}
	managementMux := http.NewServeMux()
	managementMux.HandleFunc("/start", handler.StartupHandler(state))
	managementMux.HandleFunc("/ready", handler.ReadinessHandler(state))
	managementMux.HandleFunc("/live", handler.LivenessHandler())
	slog.DebugContext(ctx, "Management handlers configured")

	managementServer := &http.Server{
		Addr:         net.JoinHostPort(*f.ManagementListenAddress, *f.ManagementPort),
		WriteTimeout: *f.ManagementWriteTimeout,
		ReadTimeout:  *f.ManagementReadTimeout,
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

	var (
		storageServer   *http.Server
		storageListener net.Listener
		mcpServer       *http.Server
		mcpListener     net.Listener
	)
	if *f.EnableFluxIntegration {
		storageServer = &http.Server{
			Addr:         net.JoinHostPort(*f.StorageListenAddress, *f.StoragePort),
			WriteTimeout: *f.StorageWriteTimeout,
			ReadTimeout:  *f.StorageReadTimeout,
			Handler:      opStore.HTTPHandler(),
		}
		storageListener, err = net.Listen("tcp", storageServer.Addr)
		if err != nil {
			_ = managementListener.Close()
			_ = jsonnetListener.Close()
			slog.ErrorContext(ctx, "Cannot bind storage listener", slog.String("addr", storageServer.Addr), slog.Any("error", err))
			return 1
		}

		if *f.EnableMCP {
			// The MCP read tools introspect operator resources. A direct
			// (uncached) client reading as the operator SA is the right fit:
			// reads are on-demand and low-QPS, and the operator's RBAC already
			// covers get/list of JsonnetSnippets. RunbookBaseURL lets
			// get_snippet surface the same per-reason remediation link the
			// reconciler stamps on status.
			mcpScheme := apiruntime.NewScheme()
			if err := jaasv1.AddToScheme(mcpScheme); err != nil {
				_ = managementListener.Close()
				_ = jsonnetListener.Close()
				_ = storageListener.Close()
				slog.ErrorContext(ctx, "Cannot build MCP scheme", slog.Any("error", err))
				return 1
			}
			mcpKubeClient, err := client.New(opRestCfg, client.Options{Scheme: mcpScheme})
			if err != nil {
				_ = managementListener.Close()
				_ = jsonnetListener.Close()
				_ = storageListener.Close()
				slog.ErrorContext(ctx, "Cannot build MCP Kubernetes client", slog.Any("error", err))
				return 1
			}
			mcpServer = newMCPHTTPServer(*f.MCPBindAddress, mcp.NewHTTPHandler(mcp.Config{
				Version:           version,
				Logger:            slog.Default(),
				LibraryPaths:      *f.LibraryPaths,
				ExtVars:           extVars,
				MaxStack:          *f.MaxStack,
				EvaluationTimeout: *f.EvaluationTimeout,
				KubeClient:        mcpKubeClient,
				RunbookBaseURL:    operator.RunbookBaseURL,
				AllowMutations:    *f.MCPAllowMutations,
				Store:             opStore,
			}))
			mcpListener, err = net.Listen("tcp", mcpServer.Addr)
			if err != nil {
				_ = managementListener.Close()
				_ = jsonnetListener.Close()
				_ = storageListener.Close()
				slog.ErrorContext(ctx, "Cannot bind MCP listener", slog.String("addr", mcpServer.Addr), slog.Any("error", err))
				return 1
			}
		}
	}

	state.MarkStarted()
	// In HTTP-only mode the pod is ready as soon as the listeners
	// are bound. In operator mode the readiness probe stays 503 until the
	// manager's cache has synced — on every replica, leader or not — so a
	// pod whose operator goroutine failed to boot never reports Ready, while
	// standby (non-leader) replicas still go Ready and serve HTTP + storage.
	// opCfg.OnReady (wired above when --enable-flux-integration is set) flips
	// the probe; the operator manager fires it from a non-leader-election
	// runnable after cache sync.
	if !*f.EnableFluxIntegration {
		state.SetReady(true)
	}

	serverErrs := make(chan error, 4)

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

	if storageServer != nil {
		go func() {
			if err := storageServer.Serve(storageListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrs <- fmt.Errorf("storage server: %w", err)
			}
		}()
		slog.DebugContext(ctx, "Storage server started", slog.String("addr", storageServer.Addr))
	}

	if mcpServer != nil {
		go func() {
			if err := mcpServer.Serve(mcpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrs <- fmt.Errorf("mcp server: %w", err)
			}
		}()
		slog.InfoContext(ctx, "MCP server started", slog.String("addr", mcpServer.Addr))
	}

	operatorDone := make(chan struct{})
	// sweepDone gates the deferred opStore.Close() on any in-flight
	// Sweep finishing: cancel() stops the ticker between passes but does
	// not interrupt a synchronous Sweep already walking the *os.Root, so
	// without this await the deferred Close() could pull the root out
	// from under an active walk (ErrClosed, a spurious sweep-failure
	// metric, possibly a half-completed removal pass).
	sweepDone := make(chan struct{})
	if *f.EnableFluxIntegration {
		go func() {
			defer close(operatorDone)
			if err := operator.Run(ctx, opCfg, opRestCfg); err != nil && !errors.Is(err, context.Canceled) {
				select {
				case serverErrs <- fmt.Errorf("operator: %w", err):
				default:
				}
			}
		}()
		slog.DebugContext(ctx, "Operator manager started")

		// Periodic storage GC: sweep orphaned .tmp residue left by Puts
		// that died after writing the tmpfile but before the rename.
		if *f.StorageSweepInterval > 0 {
			go func() {
				defer close(sweepDone)
				runStorageSweep(ctx, opStore, *f.StorageSweepInterval, *f.StorageSweepMaxTmpAge)
			}()
		} else {
			close(sweepDone)
		}
	} else {
		close(operatorDone)
		close(sweepDone)
	}

	exitCode := 0
	select {
	case sig := <-sigs:
		slog.InfoContext(ctx, "Received signal, shutting down", slog.String("signal", sig.String()))
	case err := <-serverErrs:
		slog.ErrorContext(ctx, "Server error, shutting down", slog.Any("error", err))
		exitCode = 1
	}

	drainBeforeShutdown(sigs, state, *f.ShutdownDelay, slog.Default())

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
	if storageServer != nil {
		if err := storageServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Cannot shut down storage server", slog.Any("error", err))
			exitCode = 1
		}
	}
	if mcpServer != nil {
		if err := mcpServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Cannot shut down MCP server", slog.Any("error", err))
			exitCode = 1
		}
	}

	// Cancel the operator's context so mgr.Start returns and the sweep +
	// renewer goroutines drain. The deferred cancel above would do the
	// same, but only after run() returns — cancelling here keeps the
	// graceful-shutdown window predictable. All three were signalled
	// together and drain concurrently, so one shared 30s window covers
	// the set. Awaiting the sweep before run() returns matters: the
	// deferred opStore.Close() must not run while a sweep walk is in
	// flight, or it would pull the *os.Root out from under it.
	cancel()
	if !awaitGoroutines(ctx, operatorAwaitTimeout, map[string]<-chan struct{}{
		"operator":             operatorDone,
		"storage sweep":        sweepDone,
		"webhook cert renewer": webhookRenewerDone,
	}) {
		exitCode = 1
	}

	slog.InfoContext(ctx, "JaaS service has shut down")
	return exitCode
}

// operatorAwaitTimeout bounds how long shutdown waits for the operator and
// background goroutines to stop. Derived from operator.GracefulShutdownTimeout
// so it is always strictly larger: the manager's own drain window completes
// (closing operatorDone) before this deadline, so a correct-but-slow shutdown
// never trips the "did not stop" path and reports exit code 1.
const operatorAwaitTimeout = operator.GracefulShutdownTimeout + 5*time.Second

// awaitGoroutines waits for every named done-channel to close, bounded by a
// single timeout shared across the set: they are all signalled by the same
// ctx cancel() and drain concurrently, so one window covers them. A nil
// channel is skipped (the goroutine was never started). Returns true iff
// every channel closed before the deadline; each that didn't is logged by
// name so a hung component is identifiable.
func awaitGoroutines(ctx context.Context, timeout time.Duration, dones map[string]<-chan struct{}) bool {
	deadline := time.Now().Add(timeout)
	allStopped := true
	for name, done := range dones {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(time.Until(deadline)):
			slog.ErrorContext(ctx, "Goroutine did not stop before shutdown deadline",
				slog.String("goroutine", name))
			allStopped = false
		}
	}
	return allStopped
}

// defaultMaxConcurrentEvals scales the eval-semaphore cap with available
// parallelism: GOMAXPROCS*4 floor 16. Each in-flight eval pins ~one CPU
// for its working set, so going far above this just queues without
// throughput gain; the cap exists to bound worst-case goroutine
// pile-up under a runaway snippet, not to clip steady-state throughput.
func defaultMaxConcurrentEvals() int {
	n := max(runtime.GOMAXPROCS(0)*4, 16)
	return n
}

// parseWatchNamespaces splits a comma-separated list into namespace
// names, falling back to JAAS_WATCH_NAMESPACES in env when the flag is
// empty. Empty inputs from both sources yield nil — the operator's
// historical cluster-wide watch behaviour.
//
// Each entry is trimmed; empty entries (from a trailing comma, double
// comma, etc.) are dropped. The Kubernetes-level namespace-name
// validation (DNS-1123 label) is left to the apiserver, which
// rejects malformed names when the cache tries to list them.
func parseWatchNamespaces(flagValue string, env []string) []string {
	raw := strings.TrimSpace(flagValue)
	if raw == "" {
		for _, e := range env {
			if k, v, ok := strings.Cut(e, "="); ok && k == "JAAS_WATCH_NAMESPACES" {
				raw = strings.TrimSpace(v)
				break
			}
		}
	}
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// serviceAccountNamespaceFile is the projected service-account namespace path
// the kubelet mounts into every pod. A package var so tests can point it at a
// fixture.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// resolveSelfSignedNamespace picks the namespace the self-signed webhook cert
// and ValidatingWebhookConfiguration live in. It prefers --webhook-service-namespace,
// then --leader-election-namespace, then the in-cluster downward-API fallback
// (POD_NAMESPACE env, then the projected service-account namespace file) —
// matching the --webhook-service-namespace help text. controller-runtime's own
// downward-API discovery for an empty --leader-election-namespace is internal to
// the manager and never reaches this cert-provisioning path, so the fallback is
// resolved here explicitly. Returns "" only when every source is empty, which
// the caller reports as a hard error.
func resolveSelfSignedNamespace(svcNs, leNs string, env []string) string {
	if svcNs != "" {
		return svcNs
	}
	if leNs != "" {
		return leNs
	}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == "POD_NAMESPACE" {
			if ns := strings.TrimSpace(v); ns != "" {
				return ns
			}
		}
	}
	if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return ""
}

// loadKubeconfig resolves a *rest.Config for the operator. An explicit
// --kubeconfig path is used verbatim; the empty default chains through
// KUBECONFIG env, the default kubeconfig path, and finally in-cluster
// service-account credentials via controller-runtime's resolver.
func loadKubeconfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	return ctrl.GetConfig()
}

// newMCPHTTPServer builds the MCP endpoint's http.Server. The MCP transport
// is streamable HTTP: a standing GET carries the SSE stream and hanging POSTs
// carry tool-call responses, both alive far longer than any request-read
// budget — a ReadTimeout (or WriteTimeout) arms a whole-request deadline that
// severs those streams mid-session. The server therefore bounds only the
// request HEADERS (the slowloris guard) and leaves the bodies to the
// protocol's own lifecycle.
func newMCPHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		Handler:           handler,
	}
}

// newStorageBackend selects and constructs the operator's artifact store from
// the backend flags. It returns the exit code the caller should use: 0 on
// success, 2 for flag misuse (missing path / endpoint / unknown backend —
// written to stderr as an "Invalid --flag" message), and 1 for a construction
// failure (logged via slog). The two failure classes carry different codes so
// flag misuse stays a usage error, matching run's convention.
func newStorageBackend(ctx context.Context, stderr io.Writer, backend, localPath string, s3cfg storage.S3Config) (storage.Backend, int) {
	switch backend {
	case "local":
		if localPath == "" {
			fmt.Fprintln(stderr, "Invalid --storage-path: required when --storage-backend=local")
			return nil, 2
		}
		store, err := storage.New(localPath)
		if err != nil {
			slog.ErrorContext(ctx, "Cannot open storage", slog.String("path", localPath), slog.Any("error", err))
			return nil, 1
		}
		return store, 0
	case "s3":
		if s3cfg.Endpoint == "" || s3cfg.Bucket == "" {
			fmt.Fprintln(stderr, "Invalid S3 config: --s3-endpoint and --s3-bucket are required when --storage-backend=s3")
			return nil, 2
		}
		b, err := storage.NewS3(s3cfg)
		if err != nil {
			slog.ErrorContext(ctx, "Cannot init S3 backend", slog.Any("error", err))
			return nil, 1
		}
		return b, 0
	default:
		fmt.Fprintf(stderr, "Invalid --storage-backend %q: must be \"local\" or \"s3\"\n", backend)
		return nil, 2
	}
}

// loadOCILibraries scans the operator's --library-path mounts for shared
// libraries and logs the discovered alias set. Folded out of the operator
// Config literal so the field assignment stays a single readable call.
func loadOCILibraries(ctx context.Context, libraryPaths []string) map[string]eval.Library {
	libs := ociLibrariesFromPaths(libraryPaths)
	if len(libs) > 0 {
		aliases := make([]string, 0, len(libs))
		for a := range libs {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		slog.InfoContext(ctx, "Loaded OCI libraries for operator eval path",
			slog.Int("count", len(libs)),
			slog.Any("aliases", aliases))
	}
	return libs
}

// drainBeforeShutdown flips readiness off and (if delay > 0) blocks for `delay`
// so Kubernetes can propagate the not-ready status to its endpoint controllers
// before in-flight requests start being aborted by Shutdown. A second signal
// on sigs cuts the wait short — a frustrated user hitting Ctrl-C twice gets
// what they asked for without waiting out the full delay.
func drainBeforeShutdown(sigs <-chan os.Signal, state *handler.HealthState, delay time.Duration, logger *slog.Logger) {
	// Latch draining first so a concurrent readiness writer (the operator's
	// post-cache-sync onReady runs only after the cache syncs, which can land
	// during the drain window) can't flip readiness back to true while we wait.
	state.MarkDraining()
	if delay <= 0 {
		return
	}
	logger.Info("Draining: waiting for readiness to propagate before shutdown", slog.Duration("delay", delay))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case sig := <-sigs:
		logger.Info("Second signal received; cutting drain short", slog.String("signal", sig.String()))
	}
}

// runStorageSweep periodically invokes opStore.Sweep, dropping orphaned
// .tmp residue. Returns when ctx is canceled. Each pass's count is logged
// at debug; non-zero passes log at info so operators see actual cleanup.
func runStorageSweep(ctx context.Context, opStore storage.Backend, interval, maxTmpAge time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := opStore.Sweep(ctx, maxTmpAge)
			if err != nil {
				operator.RecordSweepFailure()
				slog.WarnContext(ctx, "Storage sweep failed", slog.Any("error", err))
				continue
			}
			if n > 0 {
				slog.InfoContext(ctx, "Storage sweep removed orphaned .tmp files",
					slog.Int("count", n))
			} else {
				slog.DebugContext(ctx, "Storage sweep clean", slog.Int("count", 0))
			}
		}
	}
}

// selfsignedConfig is the local glue between main.go's flag parsing
// and the internal/webhook/selfsigned package — kept here so main.go
// stays the single source of "operator wiring" knobs.
type selfsignedConfig struct {
	Namespace   string
	ServiceName string
	CertDir     string
	Validity    time.Duration
	VWCName     string
}

// provisionSelfSignedWebhookCert generates the in-pod CA + serving cert,
// stamps the CA into the VWC's caBundle, and starts the background
// renewer. The returned channel is closed when the renewer goroutine
// exits; run() awaits it on shutdown so a rotation in flight during
// SIGTERM isn't abandoned mid-patch.
func provisionSelfSignedWebhookCert(ctx context.Context, restCfg *rest.Config, cfg selfsignedConfig) (<-chan struct{}, error) {
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("webhook self-signed: service namespace is required (set --webhook-service-namespace or --leader-election-namespace)")
	}
	if err := os.MkdirAll(cfg.CertDir, 0o750); err != nil {
		return nil, fmt.Errorf("webhook self-signed: mkdir cert dir %q: %w", cfg.CertDir, err)
	}
	input := selfsigned.Input{
		ServiceName: cfg.ServiceName,
		Namespace:   cfg.Namespace,
		Validity:    cfg.Validity,
	}
	bundle, err := selfsigned.Generate(input)
	if err != nil {
		return nil, fmt.Errorf("webhook self-signed: generate: %w", err)
	}
	if err := bundle.WriteTo(cfg.CertDir); err != nil {
		return nil, fmt.Errorf("webhook self-signed: write cert: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("webhook self-signed: clientset: %w", err)
	}
	vwcs := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	// Union this pod's fresh CA into the existing caBundle rather than
	// overwriting it: other already-running replicas (multi-replica HA,
	// rolling update) must stay trusted across the rollout, and
	// CombineCABundles also prunes expired CA blocks so the bundle stays
	// bounded. UpdateVWCCABundle does this read-merge-write under
	// optimistic concurrency — so several pods bootstrapping at once
	// during a rolling update converge instead of clobbering each other's
	// CAs — and retries transient apiserver errors so a brief hiccup
	// during restart doesn't fail the bootstrap.
	if err := selfsigned.UpdateVWCCABundle(ctx, vwcs, cfg.VWCName, func(cur []byte) []byte {
		return selfsigned.CombineCABundles(cur, bundle.CABundle)
	}); err != nil {
		return nil, fmt.Errorf("webhook self-signed: %w", err)
	}

	// Start the in-process renewer. controller-runtime's webhook server
	// polls the cert files via fsnotify (certwatcher), so writing fresh
	// tls.crt/tls.key is enough to hot-reload TLS without restarting the
	// pod. CurrentCA seeds the renewer with this pod's bootstrap CA so its
	// trim step drops exactly this block on the next rotation, never a
	// peer replica's.
	renewer := &selfsigned.Renewer{
		Input:     input,
		CertDir:   cfg.CertDir,
		VWCName:   cfg.VWCName,
		VWCClient: vwcs,
		CurrentCA: bundle.CABundle,
		// Wire the operator-side counter so renewal failures surface as
		// Prometheus signal, not just slog.Warn.
		OnFailure: func(_ error) { operator.RecordWebhookCertRenewalFailure() },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Recover a panic in the renewer so it can't kill the goroutine
		// silently and stop all future rotation; the existing cert keeps
		// working until expiry either way, but the failure is logged.
		defer func() {
			if p := recover(); p != nil {
				operator.RecordWebhookCertRenewalFailure()
				slog.ErrorContext(ctx, "Self-signed webhook cert renewer panicked",
					slog.Any("panic", p))
			}
		}()
		if err := renewer.Run(ctx); err != nil {
			slog.WarnContext(ctx, "Self-signed webhook cert renewer exited",
				slog.Any("error", err))
		}
	}()
	return done, nil
}

// ociLibraryAliasesFromPaths walks every --library-path entry and
// returns the basenames of the subdirectories it contains. Each
// subdirectory name is the alias a snippet would `import "<name>/..."`
// against — the same alias the operator's admission webhook + reconciler
// reject if a LibraryRef tries to shadow it.
//
// Missing or unreadable dirs are silently skipped: at startup we don't
// want to crash the binary just because one optional mount is empty.
func ociLibraryAliasesFromPaths(paths []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, dir := range paths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// ociLibrariesFromPaths walks every --library-path entry and loads
// every `.libsonnet` / `.jsonnet` / `.json` file under each
// subdirectory into an eval.Library, keyed by the subdirectory name.
// First write wins on duplicate aliases (matching
// ociLibraryAliasesFromPaths's order), so later --library-path entries
// don't silently override earlier ones — operators see whichever was
// declared first.
//
// Returns an empty map when no paths are readable; the reconciler
// treats nil and empty as equivalent. Path-level errors are logged at
// Warn so operators see a partially-broken mount on startup; the
// scan continues so the binary doesn't fail to boot for one bad lib.
func ociLibrariesFromPaths(paths []string) map[string]eval.Library {
	out := map[string]eval.Library{}
	for _, root := range paths {
		entries, err := os.ReadDir(root)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("Skipping unreadable --library-path entry",
					slog.String("path", root), slog.Any("error", err))
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			alias := e.Name()
			if _, dup := out[alias]; dup {
				continue
			}
			libDir := filepath.Join(root, alias)
			files, err := readOCILibraryFiles(libDir)
			if err != nil {
				slog.Warn("Skipping OCI library with unreadable files",
					slog.String("alias", alias),
					slog.String("dir", libDir),
					slog.Any("error", err))
				continue
			}
			if len(files) == 0 {
				slog.Info("OCI library directory has no importable files",
					slog.String("alias", alias),
					slog.String("dir", libDir))
				continue
			}
			out[alias] = eval.Library{Files: files}
		}
	}
	return out
}

// readOCILibraryFiles recursively reads every importable file under
// libDir into a map keyed by the path relative to libDir. Returns
// (nil, err) when libDir cannot be walked at all. Individual file
// read failures are silently skipped so a single bad file doesn't
// disqualify the whole library; the resulting map will just miss
// those files (a subsequent `import` resolution fails noisily).
func readOCILibraryFiles(libDir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(libDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch filepath.Ext(p) {
		case ".libsonnet", ".jsonnet", ".json":
		default:
			return nil
		}
		// #nosec G304,G122 -- p is enumerated by WalkDir over the
		// operator-configured --library-path, never request input.
		body, err := os.ReadFile(p)
		if err != nil {
			return nil // skip unreadable individual files
		}
		rel, err := filepath.Rel(libDir, p)
		if err != nil {
			return nil
		}
		// Normalize path separators to "/" so the eval.Importer's
		// lookups (which always use "/") work on Windows too.
		files[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
