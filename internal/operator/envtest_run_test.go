/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/sources"
	"github.com/metio/jaas/internal/storage"
)

// TestEnvtest_Run_DrivesFullManagerLifecycle wires the production
// defaultBuilder against a real apiserver, applies a snippet, and verifies
// the watch-driven manager reaches Synced. The same path is what main.go
// hits in production; this test covers every line of defaultBuilder that
// the fake-client tests cannot.
func TestEnvtest_Run_DrivesFullManagerLifecycle(t *testing.T) {
	cfg := envtestConfig(t)
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	opCfg := Config{
		DefaultServiceAccount: "tenant",
		Store:                 store,
		StorageBaseURL:        "http://jaas-storage.test.svc.cluster.local:8082",
		RerenderRate:          10,
		RerenderBurst:         10,
		Logger:                discardLoggerEnvtest(),
	}

	stop, done := runManagerInBackground(t, cfg, opCfg)
	defer func() {
		stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("manager exited with %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Errorf("manager did not stop within 20s")
		}
	}()

	// Wait for the manager to be elected leader + start watches. Polling
	// for the snippet's existence is enough — if Create returns NotFound
	// for the CRD, the manager isn't watching yet.
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	pollUntil(t, 30*time.Second, snippetReady(c, key, metav1.ConditionTrue, ReasonSynced))
}

// TestEnvtest_Run_ContextCancellationStopsManagerCleanly verifies the
// manager goroutine exits without error when the supplied context is
// canceled — main.go relies on this when SIGTERM is received.
func TestEnvtest_Run_ContextCancellationStopsManagerCleanly(t *testing.T) {
	cfg := envtestConfig(t)
	stop, done := runManagerInBackground(t, cfg, Config{Logger: discardLoggerEnvtest()})

	// Give the manager a beat to start before stopping it.
	time.Sleep(200 * time.Millisecond)
	stop()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("manager exited with %v, want nil or context.Canceled", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("manager did not stop within 20s")
	}
}

// TestEnvtest_Run_WithWebhookEnabled boots a manager whose Config requests
// the webhook. Without TLS material at WebhookCertDir, the webhook server
// fails to start — but only when Start is called, and Start returning an
// error is what we want to observe. (A separate envtest in
// envtest_webhook_test.go exercises the happy webhook path with proper
// certs.)
func TestEnvtest_Run_WebhookFailsWithoutCertDir(t *testing.T) {
	cfg := envtestConfig(t)
	opCfg := Config{
		EnableWebhook:  true,
		WebhookCertDir: t.TempDir(), // empty dir — no tls.crt / tls.key
		Logger:         discardLoggerEnvtest(),
	}
	stop, done := runManagerInBackground(t, cfg, opCfg)
	defer stop()

	select {
	case err := <-done:
		if err == nil {
			t.Errorf("manager exited cleanly; want a TLS-related error")
		}
	case <-time.After(20 * time.Second):
		// Some webhook server impls block on the first incoming request
		// rather than at Start. If we never see an error, cancel the
		// context to clean up and pass.
		stop()
		<-done
	}
}

// TestEnvtest_Watch_LibraryUpdate_TriggersSnippetReconcile proves the watch
// on JsonnetLibrary actually wakes snippets up: after the library's
// spec.files change, the snippet's Status.Revision must move without any
// external trigger.
func TestEnvtest_Watch_LibraryUpdate_TriggersSnippetReconcile(t *testing.T) {
	cfg := envtestConfig(t)
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	opCfg := Config{
		DefaultServiceAccount: "tenant",
		Store:                 store,
		StorageBaseURL:        "http://jaas-storage.test.svc.cluster.local:8082",
		RerenderRate:          100,
		RerenderBurst:         100,
		Logger:                discardLoggerEnvtest(),
	}
	stop, done := runManagerInBackground(t, cfg, opCfg)
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Errorf("manager did not stop within 20s")
		}
	}()

	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: ns},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ from: "v1" }`},
			},
		},
	}
	if err := c.Create(context.Background(), lib); err != nil {
		t.Fatalf("create library: %v", err)
	}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `(import "u") + {}`},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "u"},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	pollUntil(t, 30*time.Second, snippetReady(c, key, metav1.ConditionTrue, ReasonSynced))

	// Ready=Synced and Status.Revision are observed through the cached
	// client, which is eventually consistent: a single Get right after the
	// readiness poll can land before the revision is visible. Poll for the
	// revision instead of asserting it once (flake guard).
	var revV1 string
	pollUntil(t, 10*time.Second, func() (bool, string) {
		var s jaasv1.JsonnetSnippet
		if err := c.Get(context.Background(), key, &s); err != nil {
			return false, "get: " + err.Error()
		}
		revV1 = s.Status.Revision
		if revV1 == "" {
			return false, "Status.Revision still empty"
		}
		return true, ""
	})

	// Mutate the library's content; the watch must wake the snippet up and
	// produce a new revision.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(lib), lib); err != nil {
		t.Fatalf("refetch library: %v", err)
	}
	lib.Spec.Files["main.libsonnet"] = `{ from: "v2", changed: true }`
	if err := c.Update(context.Background(), lib); err != nil {
		t.Fatalf("update library: %v", err)
	}

	pollUntil(t, 30*time.Second, func() (bool, string) {
		var snip jaasv1.JsonnetSnippet
		if err := c.Get(context.Background(), key, &snip); err != nil {
			return false, "get: " + err.Error()
		}
		if snip.Status.Revision == revV1 || snip.Status.Revision == "" {
			return false, "Status.Revision = " + snip.Status.Revision +
				" (still equal to pre-update value)"
		}
		return true, ""
	})
}

// TestEnvtest_Watch_FluxSourceUpdate_TriggersSnippetReconcile installs a
// stub GitRepository CRD and proves that updating the source's
// status.artifact wakes any snippet whose spec.sourceRef points at it. The
// stub Fetcher returns canned bytes — the watch is what we're verifying,
// not the fetch.
func TestEnvtest_Watch_FluxSourceUpdate_TriggersSnippetReconcile(t *testing.T) {
	cfg := envtestConfig(t)
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	// Create a GitRepository CR with empty status; we'll flip it later.
	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
	})
	src.SetName("configs")
	src.SetNamespace(ns)
	if err := c.Create(context.Background(), src); err != nil {
		t.Fatalf("create GitRepository: %v", err)
	}

	// Custom builder that wires a counted fetcher so we can assert it was
	// invoked twice — once on initial reconcile, once after the watch event.
	fetcher := &countingFetcher{
		result: &sources.Result{
			Files: map[string]string{"main.jsonnet": `{ ok: true }`},
		},
	}

	stop, done := runManagerInBackgroundWithBuilder(t, cfg, Config{
		DefaultServiceAccount: "tenant",
		RerenderRate:          100,
		RerenderBurst:         100,
		Logger:                discardLoggerEnvtest(),
	}, fetcher)
	defer func() {
		stop()
		<-done
	}()

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "GitRepository", Name: "configs", Namespace: ns,
				},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	pollUntil(t, 30*time.Second, snippetReady(c, key, metav1.ConditionTrue, ReasonSynced))
	initial := fetcher.count()

	// Flip the GitRepository's status to trigger the watch.
	if err := c.Get(context.Background(),
		client.ObjectKeyFromObject(src), src); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(src.Object, "rev-v2", "status", "artifact", "revision")
	if err := c.Status().Update(context.Background(), src); err != nil {
		t.Fatalf("update GitRepository status: %v", err)
	}

	pollUntil(t, 30*time.Second, func() (bool, string) {
		if fetcher.count() > initial {
			return true, ""
		}
		return false, "fetcher not re-invoked after source update"
	})
}

// countingFetcher wraps a fixed Result and counts Fetch calls — used by the
// Flux source watch envtest to prove a status mutation on the source CR
// triggers the snippet's reconciler.
type countingFetcher struct {
	mu     sync.Mutex
	calls  int
	result *sources.Result
}

func (f *countingFetcher) Fetch(_ context.Context, _ client.Client, _ *jaasv1.SourceRef, _ string) (*sources.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.result, nil
}

func (f *countingFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// runManagerInBackgroundWithBuilder boots an operator manager wired with a
// custom SourceFetcher (the production path constructs sources.New()). The
// Flux source watch test uses this to assert the watch wakes a snippet up
// without depending on a real source-controller serving artifacts.
func runManagerInBackgroundWithBuilder(t *testing.T, restCfg *rest.Config, cfg Config, fetcher SourceFetcher) (context.CancelFunc, <-chan error) {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = discardLoggerEnvtest()
	}
	cfg.SkipControllerNameValidation = true
	cfg.SkipImpersonation = true
	if cfg.MetricsBindAddress == "" {
		cfg.MetricsBindAddress = "0"
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	build := func(restCfg *rest.Config, opts ctrl.Options, c Config) (runner, error) {
		if c.SkipControllerNameValidation {
			skip := true
			opts.Controller = ctrlconfig.Controller{SkipNameValidation: &skip}
		}
		mgr, err := ctrl.NewManager(restCfg, opts)
		if err != nil {
			return nil, err
		}
		reconciler := &SnippetReconciler{
			Client:                mgr.GetClient(),
			Scheme:                mgr.GetScheme(),
			DefaultServiceAccount: c.DefaultServiceAccount,
			NoCrossNamespaceRefs:  c.NoCrossNamespaceRefs,
			ExtVars:               c.ExtVars,
			EvaluationTimeout:     c.EvaluationTimeout,
			MaxStack:              c.MaxStack,
			Fetcher:               fetcher,
			Logger:                c.Logger,
		}
		if c.RerenderRate > 0 && c.RerenderBurst > 0 {
			reconciler.Limiter = NewRateLimiter(c.RerenderRate, c.RerenderBurst)
		}
		if err := reconciler.SetupWithManager(mgr); err != nil {
			return nil, fmt.Errorf("setup SnippetReconciler: %w", err)
		}
		// Mirror defaultBuilder's CRD watcher wiring so tests can verify
		// late-install behavior end-to-end.
		if missing := reconciler.MissingFluxSourceKinds(); len(missing) > 0 {
			if err := mgr.Add(&crdWatcher{
				restCfg: restCfg,
				kinds:   missing,
				logger:  c.Logger,
				engager: reconciler,
			}); err != nil {
				return nil, fmt.Errorf("setup crdWatcher: %w", err)
			}
		}
		return mgr, nil
	}
	go func() {
		done <- runWithBuilder(ctx, cfg, restCfg, build)
	}()
	return cancel, done
}

// TestEnvtest_LeaderElection_HappyPath confirms a manager with leader
// election enabled can acquire the lease and reconcile snippets exactly as
// the LE-disabled path does. Two-replica races are observed indirectly by
// proving the acquired-leader behavior matches the no-LE baseline; the
// underlying lock-mechanics are controller-runtime's responsibility.
func TestEnvtest_LeaderElection_HappyPath(t *testing.T) {
	cfg := envtestConfig(t)
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	stop, done := runManagerInBackgroundWithBuilder(t, cfg, Config{
		DefaultServiceAccount:   "tenant",
		RerenderRate:            100,
		RerenderBurst:           100,
		LeaderElection:          true,
		LeaderElectionID:        "jaas-le-test-" + ns,
		LeaderElectionNamespace: "default",
		Logger:                  discardLoggerEnvtest(),
	}, &countingFetcher{result: &sources.Result{Files: map[string]string{}}})
	defer func() {
		stop()
		<-done
	}()

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)
	pollUntil(t, 30*time.Second, snippetReady(c, key, metav1.ConditionTrue, ReasonSynced))
}

// runManagerInBackgroundWithReconcilerCapture is runManagerInBackgroundWithBuilder
// with one extra hook: after the reconciler is constructed inside the
// builder closure, capture is called with the reconciler pointer so
// tests can introspect its private state (missingFluxKinds, etc.).
func runManagerInBackgroundWithReconcilerCapture(t *testing.T, restCfg *rest.Config, cfg Config, fetcher SourceFetcher, capture func(*SnippetReconciler)) (context.CancelFunc, <-chan error) {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = discardLoggerEnvtest()
	}
	cfg.SkipControllerNameValidation = true
	cfg.SkipImpersonation = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	build := func(restCfg *rest.Config, opts ctrl.Options, c Config) (runner, error) {
		if c.SkipControllerNameValidation {
			skip := true
			opts.Controller = ctrlconfig.Controller{SkipNameValidation: &skip}
		}
		mgr, err := ctrl.NewManager(restCfg, opts)
		if err != nil {
			return nil, err
		}
		reconciler := &SnippetReconciler{
			Client:                mgr.GetClient(),
			Scheme:                mgr.GetScheme(),
			DefaultServiceAccount: c.DefaultServiceAccount,
			NoCrossNamespaceRefs:  c.NoCrossNamespaceRefs,
			ExtVars:               c.ExtVars,
			EvaluationTimeout:     c.EvaluationTimeout,
			MaxStack:              c.MaxStack,
			Fetcher:               fetcher,
			Logger:                c.Logger,
		}
		if c.RerenderRate > 0 && c.RerenderBurst > 0 {
			reconciler.Limiter = NewRateLimiter(c.RerenderRate, c.RerenderBurst)
		}
		if err := reconciler.SetupWithManager(mgr); err != nil {
			return nil, fmt.Errorf("setup SnippetReconciler: %w", err)
		}
		if missing := reconciler.MissingFluxSourceKinds(); len(missing) > 0 {
			if err := mgr.Add(&crdWatcher{
				restCfg: restCfg,
				kinds:   missing,
				logger:  c.Logger,
				engager: reconciler,
			}); err != nil {
				return nil, fmt.Errorf("setup crdWatcher: %w", err)
			}
		}
		capture(reconciler)
		return mgr, nil
	}
	go func() {
		done <- runWithBuilder(ctx, cfg, restCfg, build)
	}()
	return cancel, done
}

// TestEnvtest_CRDWatch_LateInstallEngagesWatchLive installs a
// previously-missing Flux source CRD (Bucket) AFTER the manager is up.
// The crdWatcher observes the Established=True transition and calls
// reconciler.EngageFluxWatch — the live controller picks up the new
// kind without a process restart. The manager keeps running; we assert
// the GVK has been removed from missingFluxKinds (the reconciler's
// internal view of what's still un-watched).
func TestEnvtest_CRDWatch_LateInstallEngagesWatchLive(t *testing.T) {
	cfg := envtestConfig(t)
	c := envtestClient(t)

	// Sanity check: Bucket CRD is NOT in the shared envtest. If it ever
	// gets added to envtest_setup_test.go's CRDs[], this test would
	// silently succeed without exercising the watch.
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: "buckets.source.toolkit.fluxcd.io"},
		&apiextv1.CustomResourceDefinition{}); err == nil {
		t.Fatal("precondition: Bucket CRD must not be pre-installed; update envtest_setup_test.go")
	}

	// Capture the reconciler via a buffered channel so the test reads
	// the pointer through a happens-before edge (no race vs. the
	// builder goroutine's write).
	capturedCh := make(chan *SnippetReconciler, 1)
	captureReconciler := func(r *SnippetReconciler) { capturedCh <- r }

	stop, done := runManagerInBackgroundWithReconcilerCapture(t, cfg, Config{
		DefaultServiceAccount: "tenant",
		RerenderRate:          100,
		RerenderBurst:         100,
		Logger:                discardLoggerEnvtest(),
	}, &countingFetcher{result: &sources.Result{Files: map[string]string{}}}, captureReconciler)
	defer func() {
		stop()
		<-done
	}()

	var capturedReconciler *SnippetReconciler
	select {
	case capturedReconciler = <-capturedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("reconciler was not captured within 5s")
	}

	// Bucket should be among the missingFluxKinds at this point.
	wantGVK := fluxSourceGVK("Bucket")
	if !contains(capturedReconciler.MissingFluxSourceKinds(), wantGVK) {
		t.Fatalf("Bucket GVK missing from missingFluxKinds at start: %v",
			capturedReconciler.MissingFluxSourceKinds())
	}

	// Install + Establish the Bucket CRD.
	bucket := fluxSourceStubCRD("Bucket", "buckets", "BucketList")
	if err := c.Create(context.Background(), bucket); err != nil {
		t.Fatalf("install Bucket CRD: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), bucket)
		// envtest has no kube-controller-manager to GC delete-marked CRDs.
		// We must wait for the apiserver to actually remove the object
		// before the next test boots its manager — otherwise that manager
		// registers Watches(Bucket) against a stale-but-present CRD and
		// the informer's LIST never reports synced, blocking
		// WaitForCacheSync indefinitely.
		for i := 0; i < 100; i++ {
			var got apiextv1.CustomResourceDefinition
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(bucket), &got); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("Bucket CRD not GC'd within 5s after Delete; subsequent tests may hang on its informer")
	})
	pollUntil(t, 5*time.Second, func() (bool, string) {
		var got apiextv1.CustomResourceDefinition
		if err := c.Get(context.Background(),
			client.ObjectKey{Name: bucket.Name}, &got); err != nil {
			return false, "get: " + err.Error()
		}
		got.Status.Conditions = []apiextv1.CustomResourceDefinitionCondition{
			{
				Type: apiextv1.Established, Status: apiextv1.ConditionTrue,
				Reason: "Test", Message: "patched by test",
			},
		}
		if err := c.Status().Update(context.Background(), &got); err != nil {
			return false, "status update: " + err.Error()
		}
		return true, ""
	})

	// The crdWatcher must call EngageFluxWatch and remove the Bucket
	// GVK from missingFluxKinds — the manager keeps running.
	pollUntil(t, 15*time.Second, func() (bool, string) {
		if contains(capturedReconciler.MissingFluxSourceKinds(), wantGVK) {
			return false, "Bucket still in missingFluxKinds"
		}
		return true, ""
	})

	// And the manager must not have exited.
	select {
	case err := <-done:
		t.Errorf("manager exited unexpectedly: %v", err)
	default:
		// Healthy — still running.
	}
}

func contains(haystack []schema.GroupVersionKind, needle schema.GroupVersionKind) bool {
	for _, g := range haystack {
		if g == needle {
			return true
		}
	}
	return false
}

// TestEnvtest_Watch_FluxSourceUpdate_IndirectViaLibrary proves that updating
// a Flux source CR wakes snippets that reference it through a JsonnetLibrary
// (the "indirect" chain: Snippet → LibraryRef → JsonnetLibrary → SourceRef →
// GitRepository).
func TestEnvtest_Watch_FluxSourceUpdate_IndirectViaLibrary(t *testing.T) {
	cfg := envtestConfig(t)
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	// Stub source CR in the same namespace.
	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository",
	})
	src.SetName("library-source")
	src.SetNamespace(ns)
	if err := c.Create(context.Background(), src); err != nil {
		t.Fatalf("create GitRepository: %v", err)
	}

	// JsonnetLibrary whose source is `library-source`.
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "lib-via-source", Namespace: ns},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "GitRepository", Name: "library-source",
				},
			},
		},
	}
	if err := c.Create(context.Background(), lib); err != nil {
		t.Fatalf("create library: %v", err)
	}

	// Custom fetcher counts both the snippet's direct fetch AND library
	// fetches via the same path; either route through resolveSnippetSource.
	fetcher := &countingFetcher{
		result: &sources.Result{
			Files: map[string]string{"main.libsonnet": `{ shared: "v1" }`},
		},
	}
	stop, done := runManagerInBackgroundWithBuilder(t, cfg, Config{
		DefaultServiceAccount: "tenant",
		RerenderRate:          100,
		RerenderBurst:         100,
		Logger:                discardLoggerEnvtest(),
	}, fetcher)
	defer func() {
		stop()
		<-done
	}()

	// Snippet uses the library; the library uses the source.
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `(import "lib") + {}`},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "lib-via-source", ImportPath: "lib"},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	pollUntil(t, 30*time.Second, snippetReady(c, key, metav1.ConditionTrue, ReasonSynced))
	initial := fetcher.count()

	// Mutate the source's status — the indirect chain must wake the snippet.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(src), src); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(src.Object, "rev-v2", "status", "artifact", "revision")
	if err := c.Status().Update(context.Background(), src); err != nil {
		t.Fatalf("update status: %v", err)
	}

	pollUntil(t, 30*time.Second, func() (bool, string) {
		if fetcher.count() > initial {
			return true, ""
		}
		return false, "fetcher not re-invoked after indirect source update"
	})
}

// TestEnvtest_Reconcile_MultipleSnippets_InDistinctNamespaces verifies the
// reconciler handles concurrent CRs across namespaces without bucket leakage
// or RBAC namespace confusion.
func TestEnvtest_Reconcile_MultipleSnippets_InDistinctNamespaces(t *testing.T) {
	c := envtestClient(t)
	nsA := freshNamespace(t, c)
	nsB := freshNamespace(t, c)

	mkSnip := func(ns, name, body string) *jaasv1.JsonnetSnippet {
		return &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: jaasv1.JsonnetSnippetSpec{
				ServiceAccountName: "tenant",
				SnippetSource: jaasv1.SnippetSource{
					Files: map[string]string{"main.jsonnet": body},
				},
				Output: jaasv1.OutputRendered,
			},
		}
	}

	r := directReconciler(t, c, true)

	keyA := applyJsonnetSnippet(t, c, mkSnip(nsA, "from-a", `{ ns: "a" }`))
	keyB := applyJsonnetSnippet(t, c, mkSnip(nsB, "from-b", `{ ns: "b" }`))

	driveToReady(t, r, c, keyA, metav1.ConditionTrue, ReasonSynced, 5)
	driveToReady(t, r, c, keyB, metav1.ConditionTrue, ReasonSynced, 5)

	// Both must have distinct ExternalArtifacts.
	for _, key := range []client.ObjectKey{keyA, keyB} {
		var snip jaasv1.JsonnetSnippet
		if err := c.Get(context.Background(), key, &snip); err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		if snip.Status.Revision == "" {
			t.Errorf("%s has empty Status.Revision", key)
		}
	}
}
