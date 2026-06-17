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

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/sources"
)

// builder is the seam between Run and controller-runtime's actual manager
// constructor. Tests substitute a fake builder so they can exercise Run's
// orchestration without contacting a Kubernetes API server. The cfg argument
// lets the production builder construct reconcilers with the operator's
// runtime settings.
type builder func(restCfg *rest.Config, opts ctrl.Options, cfg Config) (runner, error)

// runner is the subset of manager.Manager that Run depends on.
type runner interface {
	Start(ctx context.Context) error
	// Elected returns a channel that is closed when this manager is
	// the elected leader OR when leader election is disabled. Used by
	// the OnReady wiring to gate the pod's readiness probe on "manager
	// has reached steady state".
	Elected() <-chan struct{}
}

var defaultBuilder builder = func(restCfg *rest.Config, opts ctrl.Options, cfg Config) (runner, error) {
	if cfg.SkipControllerNameValidation {
		skip := true
		opts.Controller = ctrlconfig.Controller{SkipNameValidation: &skip}
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
		}
		if err := validator.SetupWithManager(mgr); err != nil {
			return nil, fmt.Errorf("setup SnippetValidator: %w", err)
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

	scheme := runtime.NewScheme()
	if err := jaasv1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register jaas v1 scheme: %w", err)
	}

	opts := ctrl.Options{Scheme: scheme}
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
		// The chart's RBAC stays cluster-wide for backwards compat,
		// so the apiserver still permits the (now-empty) list/watch
		// against outside namespaces; the cache scoping is the real
		// boundary here. Operators wanting tighter RBAC switch to
		// per-namespace RoleBindings — tracked separately in
		// open-items.md.
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

	if cfg.OnReady != nil {
		// Don't flip the pod's Ready probe until the manager has been
		// elected (or LE is off — Elected closes immediately).
		// mgr.Start sets up the cache and waits for sync before closing
		// Elected, so a single read here covers both milestones.
		go func() {
			select {
			case <-mgr.Elected():
				cfg.OnReady()
			case <-ctx.Done():
				// Shutdown beat us to it; the probe stays not-ready.
			}
		}()
	}

	return mgr.Start(ctx)
}
