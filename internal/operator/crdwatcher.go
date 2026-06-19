/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
)

// crdWatchMaxRetries caps how many times we'll re-attempt to engage a
// Flux source watch after a transient controller.Watch failure. The
// backoff doubles each attempt starting from crdWatchInitialDelay, so
// 6 attempts cover roughly 1+2+4+8+16+32 = ~63 seconds of total wait
// — long enough to ride out a cache reconnect, short enough that an
// operator who fixes the RBAC pad sees a re-engagement within minutes
// rather than at the next process restart.
// Vars (not consts) so tests can shrink them via t.Cleanup-restored
// overrides without exposing a public knob.
var (
	crdWatchMaxRetries   = 6
	crdWatchInitialDelay = time.Second
)

// crdWatchBackoff returns the delay for the (attempt+1)'th attempt.
// Exponential — attempt 0 returns crdWatchInitialDelay, attempt 1
// returns 2× that, etc.
func crdWatchBackoff(attempt int) time.Duration {
	d := crdWatchInitialDelay
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	return d
}

// fluxSourceCRDNames maps a Flux source kind to the CustomResourceDefinition
// `metadata.name` it lives under (`<plural>.<group>`). The watcher uses these
// strings to match incoming CRD events against the missing-kinds set.
var fluxSourceCRDNames = map[string]string{
	"GitRepository":    "gitrepositories.source.toolkit.fluxcd.io",
	"OCIRepository":    "ocirepositories.source.toolkit.fluxcd.io",
	"Bucket":           "buckets.source.toolkit.fluxcd.io",
	"ExternalArtifact": "externalartifacts.source.toolkit.fluxcd.io",
}

// engager is the subset of SnippetReconciler crdWatcher uses to hand
// off newly-installed Flux source kinds to the live controller.
// Production wires SnippetReconciler.EngageFluxWatch; tests substitute
// a recording fake.
type engager interface {
	EngageFluxWatch(ctx context.Context, gvk schema.GroupVersionKind) error
}

// crdWatcher is the manager-runnable that watches the cluster's
// CustomResourceDefinition stream for newly installed Flux source CRDs.
// When a missing kind becomes Established=True, it calls
// engager.EngageFluxWatch(gvk) — the controller adds the new watch
// live, no process restart needed.
//
// Sub-second event delivery beats the previous 1-minute poll, at the
// cost of `get/list/watch` on `customresourcedefinitions.apiextensions.k8s.io`
// — granted by the helm chart's ClusterRole. Uses client-go's
// apiextensions informer factory directly rather than the
// controller-runtime manager cache so the watcher's lifetime is
// decoupled from the manager's other cache wiring (the manager-cache
// approach exhibited initial-sync races in envtest).
type crdWatcher struct {
	restCfg *rest.Config
	kinds   []schema.GroupVersionKind
	logger  *slog.Logger
	engager engager
}

// Start subscribes to the CRD stream via a dedicated client-go informer
// and runs until ctx is canceled. Every time a previously-missing Flux
// source CRD becomes Established the watcher calls engager.EngageFluxWatch
// to wire the live controller into the new kind. Returns nil on
// ctx cancellation (clean shutdown).
func (w *crdWatcher) Start(ctx context.Context) error {
	if len(w.kinds) == 0 {
		<-ctx.Done()
		return nil
	}
	if w.engager == nil {
		return fmt.Errorf("crd watcher: engager is required")
	}
	logger := w.logger
	if logger == nil {
		logger = slog.Default()
	}

	client, err := apiextclient.NewForConfig(w.restCfg)
	if err != nil {
		return fmt.Errorf("crd watcher: build apiext client: %w", err)
	}

	factory := apiextinformers.NewSharedInformerFactory(client, 0)
	informer := factory.Apiextensions().V1().CustomResourceDefinitions().Informer()

	// Per-GVK dedup + bounded retry. The apiextensions informer fires no
	// further events on a stable Established CRD whose metadata/status
	// doesn't change, so a one-time engagement failure would permanently
	// un-engage the GVK if we relied on event re-delivery alone. Schedule
	// an explicit exponential-backoff retry via time.AfterFunc instead.
	//
	// State is mutex-protected because AfterFunc callbacks run on their
	// own goroutines and can race the (serially-invoked) informer event
	// handlers. The `engaging` flag single-flights EngageFluxWatch per
	// GVK: a timer retry and a fresh informer event for the same kind can
	// fire concurrently, and controller-runtime does NOT dedup watches —
	// two overlapping EngageFluxWatch calls would register two
	// source.Kind watches (duplicate informer + duplicate enqueues). Only
	// one attempt per GVK is ever in flight; others observe the flag and
	// return. The same flag closes the read-modify-write window on
	// attempts[gvk] that an unlocked EngageFluxWatch call would open.
	var (
		stateMu  sync.Mutex
		engaged  = map[schema.GroupVersionKind]bool{}
		engaging = map[schema.GroupVersionKind]bool{}
		attempts = map[schema.GroupVersionKind]int{}
	)
	var attempt func(gvk schema.GroupVersionKind)
	attempt = func(gvk schema.GroupVersionKind) {
		stateMu.Lock()
		if engaged[gvk] || engaging[gvk] {
			stateMu.Unlock()
			return
		}
		engaging[gvk] = true
		stateMu.Unlock()

		if err := w.engager.EngageFluxWatch(ctx, gvk); err != nil {
			// Bump the counter on every failure (initial + each retry)
			// so a sustained-failure pattern surfaces in Prometheus even
			// when the bounded retry hides intermittent hiccups.
			crdWatchEngagementFailuresTotal.WithLabelValues(gvk.String()).Inc()
			stateMu.Lock()
			engaging[gvk] = false
			attempts[gvk]++
			n := attempts[gvk]
			stateMu.Unlock()
			if n >= crdWatchMaxRetries {
				logger.WarnContext(ctx, "Gave up engaging Flux source watch after retries; will retry on next CRD event",
					slog.String("gvk", gvk.String()),
					slog.Int("attempts", n),
					slog.Any("error", err))
				return
			}
			delay := crdWatchBackoff(n - 1)
			logger.WarnContext(ctx, "Failed to engage Flux source watch; retrying",
				slog.String("gvk", gvk.String()),
				slog.Int("attempt", n),
				slog.Duration("retryAfter", delay),
				slog.Any("error", err))
			time.AfterFunc(delay, func() {
				select {
				case <-ctx.Done():
				default:
					attempt(gvk)
				}
			})
			return
		}
		stateMu.Lock()
		engaged[gvk] = true
		engaging[gvk] = false
		attempts[gvk] = 0
		stateMu.Unlock()
	}
	check := func(obj interface{}) {
		gvk, ok := w.matchedCRD(obj)
		if !ok {
			return
		}
		stateMu.Lock()
		if engaged[gvk] {
			stateMu.Unlock()
			return
		}
		// Reset the attempt count when an event fires — the operator
		// may have fixed the RBAC / CRD state out-of-band, so the
		// next attempt deserves a clean budget.
		attempts[gvk] = 0
		stateMu.Unlock()
		attempt(gvk)
	}
	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { check(obj) },
		UpdateFunc: func(_, obj interface{}) { check(obj) },
	}); err != nil {
		return fmt.Errorf("crd watcher: add event handler: %w", err)
	}

	// Start the informer in its own context bound to ctx; the factory
	// uses a stop-channel rather than a context, so we bridge it here.
	stopCh := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()
	factory.Start(stopCh)
	if !waitForCacheSync(stopCh, informer.HasSynced) {
		return w.degradeOnSyncFailure(ctx, logger)
	}

	// Block until ctx is canceled. The informer's event handlers drive
	// engagement; nothing in Start needs to react beyond shutdown.
	<-ctx.Done()
	return nil
}

// waitForCacheSync is the cache-sync wait, behind a package var so a unit
// test can drive the failure branch without standing up a never-syncing
// informer. Production points at client-go's WaitForCacheSync.
var waitForCacheSync = func(stopCh <-chan struct{}, hasSynced ...toolscache.InformerSynced) bool {
	return toolscache.WaitForCacheSync(stopCh, hasSynced...)
}

// degradeOnSyncFailure handles a cache-sync failure by logging a warning and
// returning nil. The watcher's whole purpose is to let the operator boot
// cleanly in clusters without the Flux source CRDs and engage their watches
// later. Returning an error here would propagate out of Start and take down
// the entire manager — including the snippet/library reconcilers that are
// working fine — defeating that purpose. Degrading keeps everything else
// running; dynamic Flux-source engagement is disabled until the process
// restarts.
func (w *crdWatcher) degradeOnSyncFailure(ctx context.Context, logger *slog.Logger) error {
	logger.WarnContext(ctx, "CRD watcher could not start; dynamic Flux-source engagement disabled, restart to retry")
	return nil
}

// matchedCRD returns (gvk, true) when obj is a CustomResourceDefinition
// whose name matches one of w.kinds AND whose status reports
// Established=True. Pure function exported via the receiver so unit tests
// can drive it directly without standing up a cache.
func (w *crdWatcher) matchedCRD(obj interface{}) (schema.GroupVersionKind, bool) {
	crd, ok := obj.(*apiextv1.CustomResourceDefinition)
	if !ok {
		return schema.GroupVersionKind{}, false
	}
	if !isCRDEstablished(crd) {
		return schema.GroupVersionKind{}, false
	}
	for _, gvk := range w.kinds {
		if name, ok := fluxSourceCRDNames[gvk.Kind]; ok && crd.Name == name {
			return gvk, true
		}
	}
	return schema.GroupVersionKind{}, false
}

// isCRDEstablished returns true when the apiserver has installed the CRD
// and registered its discovery endpoint. Only then will controller-runtime
// be able to start a watch on its instances.
func isCRDEstablished(crd *apiextv1.CustomResourceDefinition) bool {
	for _, c := range crd.Status.Conditions {
		if c.Type == apiextv1.Established && c.Status == apiextv1.ConditionTrue {
			return true
		}
	}
	return false
}
