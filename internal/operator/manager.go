/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/sources"
	"github.com/metio/jaas/internal/startupcheck"
)

// GracefulShutdownTimeout bounds how long the manager waits for in-flight
// runnables (reconcilers, the cache, the webhook server) to drain after its
// context is cancelled before Start returns. main.go awaits the operator
// goroutine for strictly longer than this, so a correct-but-slow shutdown
// always closes the done channel before the await deadline — otherwise the two
// windows could race and report a clean shutdown as a hang (exit code 1).
const GracefulShutdownTimeout = 25 * time.Second

// builder is the seam between Run and controller-runtime's actual manager
// constructor. Tests substitute a fake builder so they can exercise Run's
// orchestration without contacting a Kubernetes API server. The cfg argument
// lets the production builder construct reconcilers with the operator's
// runtime settings.
type builder func(restCfg *rest.Config, opts ctrl.Options, cfg Config) (runner, error)

// runner is the subset of manager.Manager that Run depends on.
type runner interface {
	Start(ctx context.Context) error
}

// readinessSignal flips the pod's readiness probe once the manager's cache has
// synced. It is NOT a leader-election runnable, so controller-runtime starts it
// on every replica after cache sync — leader or not — which keeps standby
// replicas Ready: they serve the HTTP renderer + storage and stay warm for
// failover. Gating readiness on leadership instead leaves every non-leader
// replica permanently NotReady.
type readinessSignal struct{ onReady func() }

func (*readinessSignal) NeedLeaderElection() bool { return false }

func (r *readinessSignal) Start(ctx context.Context) error {
	r.onReady()
	<-ctx.Done()
	return nil
}

// cacheSyncTimeout is effectively unbounded: a controller-runtime controller
// treats a source that does not sync within this window as fatal and exits the
// process, so the default (2m) turns a missing CRD or an incomplete operator
// ClusterRole into a crash-loop. Waiting indefinitely instead keeps the pod alive
// and retrying the informer, so the failure degrades to "not ready, waiting" (see
// internal/startupcheck for the actionable log; readinessSignal keeps the pod
// NotReady) and self-heals the moment the CRD is installed or the RBAC is granted
// — no restart. In a healthy cluster the caches sync in well under a second.
const cacheSyncTimeout = 100 * 365 * 24 * time.Hour

var defaultBuilder builder = func(restCfg *rest.Config, opts ctrl.Options, cfg Config) (runner, error) {
	if cfg.SkipControllerNameValidation {
		// Set the field rather than replacing the struct, so the caller's
		// CacheSyncTimeout (set in runWithBuilder) survives.
		opts.Controller.SkipNameValidation = new(true)
	}
	if cfg.EnableWebhook {
		port := cfg.WebhookPort
		if port == 0 {
			port = 9443
		}
		opts.WebhookServer = webhook.NewServer(webhook.Options{
			Port:    port,
			CertDir: cfg.WebhookCertDir,
		})
	}
	mgr, err := ctrl.NewManager(restCfg, opts)
	if err != nil {
		return nil, err
	}
	reconciler, err := newSnippetReconciler(mgr, cfg)
	if err != nil {
		return nil, err
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("setup SnippetReconciler: %w", err)
	}
	if missing := reconciler.MissingFluxSourceKinds(); len(missing) > 0 {
		if err := mgr.Add(&crdWatcher{
			restCfg: restCfg,
			kinds:   missing,
			logger:  cfg.Logger,
			engager: reconciler,
		}); err != nil {
			return nil, fmt.Errorf("register CRD watcher: %w", err)
		}
	}
	if cfg.EnableWebhook {
		validator := &SnippetValidator{
			OperatorExtVars:     cfg.ExtVars,
			KnownLibraryAliases: cfg.KnownLibraryAliases,
			Client:              mgr.GetClient(),
			// Uncached: the cycle walk must see dependencies in namespaces the
			// cache does not scope / labels it filters, matching the tenant fetch.
			APIReader: mgr.GetAPIReader(),
		}
		if err := validator.SetupWithManager(mgr); err != nil {
			return nil, fmt.Errorf("setup SnippetValidator: %w", err)
		}
	}
	if cfg.OnReady != nil {
		// Flip the readiness probe once the cache has synced, on every
		// replica. readinessSignal is non-leader-election, so it starts
		// after cache sync regardless of who holds the lease — standby
		// replicas must be Ready (they serve HTTP + storage and stay warm
		// for failover), which leader-gated readiness would prevent.
		if err := mgr.Add(&readinessSignal{onReady: cfg.OnReady}); err != nil {
			return nil, fmt.Errorf("register readiness signal: %w", err)
		}
	}
	return mgr, nil
}

// newSnippetReconciler assembles the SnippetReconciler from the manager and
// config: the manager-derived clients, the optional impersonation token +
// client caches, the optional publisher, and the optional rate limiter.
// SetupWithManager and the watcher/validator wiring stay with the builder so
// this helper does only field construction.
func newSnippetReconciler(mgr ctrl.Manager, cfg Config) (*SnippetReconciler, error) {
	reconciler := &SnippetReconciler{
		Client:                mgr.GetClient(),
		APIReader:             mgr.GetAPIReader(),
		Scheme:                mgr.GetScheme(),
		DefaultServiceAccount: cfg.DefaultServiceAccount,
		NoCrossNamespaceRefs:  cfg.NoCrossNamespaceRefs,
		ExtVars:               cfg.ExtVars,
		EvaluationTimeout:     cfg.EvaluationTimeout,
		MaxStack:              cfg.MaxStack,
		Fetcher:               sources.New(),
		KnownLibraryAliases:   cfg.KnownLibraryAliases,
		OCILibraries:          cfg.OCILibraries,
		MaxWithdrawWait:       cfg.MaxWithdrawWait,
		// GetEventRecorder returns events.v1 EventRecorder.
		// notification-controller listens on both the v1 events API
		// and the older corev1 Events API, so the migration is
		// transparent to operators.
		EventRecorder: mgr.GetEventRecorder("jaas-operator"),
		Logger:        cfg.Logger,
		CycleCache:    newCycleCache(),
	}
	if !cfg.SkipImpersonation {
		reconciler.RestConfig = mgr.GetConfig()
		kc, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			return nil, fmt.Errorf("build clientset for token minting: %w", err)
		}
		reconciler.TokenCache = newTokenCache(clientsetTokenMinter{kc: kc})
		reconciler.ClientCache = newTenantClientCache()
	}
	if cfg.Store != nil {
		reconciler.Publisher = NewPublisher(cfg.Store, cfg.StorageBaseURL)
		reconciler.Publisher.MaxArtifactBytes = cfg.MaxArtifactBytes
		reconciler.Publisher.GCGrace = cfg.ArtifactGCGrace
	}
	if cfg.RerenderRate > 0 && cfg.RerenderBurst > 0 {
		reconciler.Limiter = NewRateLimiter(cfg.RerenderRate, cfg.RerenderBurst)
	}
	return reconciler, nil
}

// Run boots a controller-runtime manager wired with the JaaS v1 scheme and
// blocks until ctx is canceled. The manager carries no reconcilers in Phase
// 1B; subsequent phases register them via the manager's builder API.
//
// restCfg must be a valid *rest.Config; the kubeconfig-resolution chain lives
// in main.go so the operator package stays free of process-level concerns.
func Run(ctx context.Context, cfg Config, restCfg *rest.Config) error {
	return runWithBuilder(ctx, cfg, restCfg, defaultBuilder)
}

func runWithBuilder(ctx context.Context, cfg Config, restCfg *rest.Config, build builder) error {
	if restCfg == nil {
		return errors.New("operator: nil rest.Config")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Route controller-runtime's own logs (leader election, cache, manager,
	// internal reconcile) through the same slog handler so they share its
	// level and JSON/text format instead of being dropped.
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	scheme := runtime.NewScheme()
	if err := jaasv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register jaas v1 scheme: %w", err)
	}

	opts := ctrl.Options{Scheme: scheme, GracefulShutdownTimeout: new(GracefulShutdownTimeout)}
	// Never let a controller exit the process because a source could not sync — a
	// missing CRD or an incomplete ClusterRole degrades to "not ready, waiting",
	// not a crash-loop. See cacheSyncTimeout.
	opts.Controller.CacheSyncTimeout = cacheSyncTimeout
	if cfg.MetricsBindAddress != "" {
		opts.Metrics = metricsserver.Options{BindAddress: cfg.MetricsBindAddress}
	}
	if cfg.LabelSelector != "" {
		sel, err := labels.Parse(cfg.LabelSelector)
		if err != nil {
			return fmt.Errorf("parse label-selector %q: %w", cfg.LabelSelector, err)
		}
		// Restrict the cache (and therefore every reconciler watch
		// rooted in it) to objects matching the selector. Applies
		// uniformly to JaaS CRs and to the Flux source kinds we
		// watch for upstream republishes — operators who scope by
		// label expect both to be filtered consistently.
		byObject := map[client.Object]cache.ByObject{
			&jaasv1.JsonnetSnippet{}: {Label: sel},
			&jaasv1.JsonnetLibrary{}: {Label: sel},
		}
		opts.Cache.ByObject = byObject
	}
	if len(cfg.WatchNamespaces) > 0 {
		// Restrict the manager's informers to the listed namespaces.
		// CRs outside this set never enter the cache — the reconciler
		// can't see them even if the SA's RBAC would otherwise grant
		// access. Multi-tenant operator-instances pattern: one
		// operator deployment per tenant-group, disjoint watch sets.
		//
		// Cache scoping is the boundary enforced here in the binary.
		// The chart narrows RBAC to match by binding the tenant
		// ClusterRole through one RoleBinding per listed namespace
		// instead of a cluster-wide ClusterRoleBinding.
		nsCache := make(map[string]cache.Config, len(cfg.WatchNamespaces))
		for _, ns := range cfg.WatchNamespaces {
			nsCache[ns] = cache.Config{}
		}
		opts.Cache.DefaultNamespaces = nsCache
	}
	if cfg.LeaderElection {
		opts.LeaderElection = true
		opts.LeaderElectionID = cfg.LeaderElectionID
		opts.LeaderElectionNamespace = cfg.LeaderElectionNamespace
		opts.LeaderElectionResourceLock = "leases"
		// Release immediately on context cancel so a rolling update or
		// SIGTERM doesn't leave the next replica waiting out the full
		// 15s lease-duration before taking over.
		opts.LeaderElectionReleaseOnCancel = true
	}
	mgr, err := build(restCfg, opts, cfg)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	logger.Info("Operator manager ready",
		slog.String("defaultServiceAccount", cfg.DefaultServiceAccount),
		slog.Bool("noCrossNamespaceRefs", cfg.NoCrossNamespaceRefs),
		slog.String("labelSelector", cfg.LabelSelector),
		slog.Float64("rerenderRatePerSec", cfg.RerenderRate),
		slog.Int("rerenderBurst", cfg.RerenderBurst),
		slog.Int("extVarCount", len(cfg.ExtVars)))

	// Diagnose an un-syncable watch out of band: if a watched CRD is missing or
	// the ClusterRole can't list/watch it, log one actionable line per resource
	// until the prerequisite appears. The manager retries the informer meanwhile
	// (cacheSyncTimeout), so this never gates startup — it only explains the wait,
	// turning raw reflector "forbidden" spam into a clear cause.
	startupPreflight(ctx, restCfg, logger)

	// OnReady (the pod's readiness flip) is wired by the builder as a
	// non-leader-election runnable, so it fires after cache sync on every
	// replica — leader or not. See readinessSignal.
	return mgr.Start(ctx)
}

// startupPreflight launches the watch-prerequisite logger. It builds its own
// discovery-backed RESTMapper and authorization client (the manager's are not
// reachable through the runner seam) and returns immediately; the check loops in
// a goroutine until every watched CRD is installed and permitted or ctx ends.
func startupPreflight(ctx context.Context, restCfg *rest.Config, logger *slog.Logger) {
	// A config with no apiserver host is a unit test's empty rest.Config, not a
	// real cluster; skip the check rather than spin a goroutine that can never
	// reach discovery. In-cluster and kubeconfig-derived configs always set Host.
	if restCfg.Host == "" {
		return
	}
	dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		logger.Error("startup preflight disabled: cannot build discovery client", slog.Any("error", err))
		return
	}
	authz, err := authorizationv1client.NewForConfig(restCfg)
	if err != nil {
		logger.Error("startup preflight disabled: cannot build authorization client", slog.Any("error", err))
		return
	}
	checker := &startupcheck.Checker{
		Mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
		Review: authz.SelfSubjectAccessReviews(),
		Logger: logger,
	}
	go checker.LogUntilReady(ctx, watchedResources(), 30*time.Second)
}

// watchedResources are the CRDs the operator keeps informers on. A missing or
// unreadable one is what the preflight names.
func watchedResources() []startupcheck.Target {
	const group = "jaas.metio.wtf"
	return []startupcheck.Target{
		{GVK: schema.GroupVersionKind{Group: group, Version: "v1", Kind: "JsonnetSnippet"}, Group: group, Resource: "jsonnetsnippets"},
		{GVK: schema.GroupVersionKind{Group: group, Version: "v1", Kind: "JsonnetLibrary"}, Group: group, Resource: "jsonnetlibraries"},
	}
}
