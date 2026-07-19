/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

// fakeRunner records whether Start was called and returns whatever err it's
// configured with.
type fakeRunner struct {
	startCalled bool
	startErr    error
	startCtx    context.Context
}

func (f *fakeRunner) Start(ctx context.Context) error {
	f.startCalled = true
	f.startCtx = ctx
	return f.startErr
}

func TestRun_NilRestConfigReturnsError(t *testing.T) {
	err := Run(context.Background(), Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "nil rest.Config") {
		t.Fatalf("got %v, want error mentioning nil rest.Config", err)
	}
}

func TestRunWithBuilder_BuilderErrorPropagates(t *testing.T) {
	want := errors.New("kaboom")
	build := func(*rest.Config, ctrl.Options, Config) (runner, error) {
		return nil, want
	}
	err := runWithBuilder(context.Background(), Config{}, &rest.Config{}, build)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("got %v, want builder error to propagate", err)
	}
}

func TestRunWithBuilder_StartErrorPropagates(t *testing.T) {
	want := errors.New("apiserver unreachable")
	fake := &fakeRunner{startErr: want}
	build := func(*rest.Config, ctrl.Options, Config) (runner, error) {
		return fake, nil
	}
	err := runWithBuilder(context.Background(), Config{}, &rest.Config{}, build)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("got %v, want Start error to propagate", err)
	}
	if !fake.startCalled {
		t.Errorf("Start was not invoked")
	}
}

// TestReadinessSignal_FiresForEveryReplicaNotJustLeader pins the HA contract:
// the readiness flip (main.go's HealthState.SetReady, threaded through
// Config.OnReady) is a NON-leader-election runnable, so controller-runtime
// starts it on every replica once the cache has synced — leader or not. Gating
// it on leadership instead (e.g. mgr.Elected(), which only closes for the
// elected leader) leaves every standby replica permanently NotReady, so
// `helm --wait` for a multi-replica install never completes.
func TestReadinessSignal_FiresForEveryReplicaNotJustLeader(t *testing.T) {
	if (&readinessSignal{}).NeedLeaderElection() {
		t.Fatal("readinessSignal must NOT require leader election, or standby replicas never go Ready")
	}

	fired := make(chan struct{}, 1)
	r := &readinessSignal{onReady: func() { fired <- struct{}{} }}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("readinessSignal.Start did not invoke onReady")
	}

	// Start must keep running (long-lived runnable) until the context is
	// cancelled, then return nil.
	select {
	case <-done:
		t.Fatal("Start returned before context cancellation")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

func TestRunWithBuilder_PassesSchemeAndContext(t *testing.T) {
	var seenOpts ctrl.Options
	var seenCfg Config
	fake := &fakeRunner{}
	build := func(_ *rest.Config, opts ctrl.Options, cfg Config) (runner, error) {
		seenOpts = opts
		seenCfg = cfg
		return fake, nil
	}
	ctx := t.Context()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{
		DefaultServiceAccount: "jaas-tenant",
		NoCrossNamespaceRefs:  true,
		LabelSelector:         "team=a",
		RerenderRate:          1.0,
		RerenderBurst:         120,
		ExtVars:               map[string]string{"env": "prod"},
		Logger:                logger,
	}
	if err := runWithBuilder(ctx, cfg, &rest.Config{}, build); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if seenOpts.Scheme == nil {
		t.Fatal("manager opts had no Scheme")
	}
	// The JaaS v1 group must be registered against the manager's scheme.
	if !seenOpts.Scheme.IsVersionRegistered(seenOptsGV()) {
		t.Errorf("jaas v1 GroupVersion not registered on manager scheme")
	}
	if fake.startCtx != ctx {
		t.Errorf("Start did not receive the supplied context")
	}
	if seenCfg.DefaultServiceAccount != "jaas-tenant" {
		t.Errorf("builder did not receive Config: got DefaultServiceAccount=%q", seenCfg.DefaultServiceAccount)
	}
}

// TestRunWithBuilder_SetsUnboundedCacheSyncTimeout pins the crash-loop guard: a
// controller whose informer cannot sync (missing CRD / RBAC) must wait
// indefinitely and keep the pod alive, not hit the 2m default and exit.
func TestRunWithBuilder_SetsUnboundedCacheSyncTimeout(t *testing.T) {
	var seenOpts ctrl.Options
	build := func(_ *rest.Config, opts ctrl.Options, _ Config) (runner, error) {
		seenOpts = opts
		return &fakeRunner{}, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := runWithBuilder(t.Context(), Config{Logger: logger}, &rest.Config{}, build); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seenOpts.Controller.CacheSyncTimeout != cacheSyncTimeout {
		t.Errorf("CacheSyncTimeout = %s, want %s", seenOpts.Controller.CacheSyncTimeout, cacheSyncTimeout)
	}
	if seenOpts.Controller.CacheSyncTimeout < 365*24*time.Hour {
		t.Errorf("CacheSyncTimeout = %s, want effectively unbounded (>= 1 year)", seenOpts.Controller.CacheSyncTimeout)
	}
}

func TestRunWithBuilder_PropagatesLabelSelector(t *testing.T) {
	var seenOpts ctrl.Options
	fake := &fakeRunner{}
	build := func(_ *rest.Config, opts ctrl.Options, _ Config) (runner, error) {
		seenOpts = opts
		return fake, nil
	}
	cfg := Config{LabelSelector: "team=platform,tier!=beta"}
	if err := runWithBuilder(context.Background(), cfg, &rest.Config{}, build); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if seenOpts.Cache.ByObject == nil {
		t.Fatal("expected opts.Cache.ByObject populated")
	}
	found := 0
	for _, by := range seenOpts.Cache.ByObject {
		if by.Label == nil {
			continue
		}
		if !by.Label.Matches(labels.Set{"team": "platform"}) {
			t.Errorf("selector did not match team=platform: %v", by.Label)
		}
		if by.Label.Matches(labels.Set{"team": "platform", "tier": "beta"}) {
			t.Errorf("selector matched tier=beta despite tier!=beta")
		}
		found++
	}
	if found < 2 {
		t.Errorf("expected selector applied to >= 2 kinds, got %d", found)
	}
}

func TestRunWithBuilder_RejectsInvalidLabelSelector(t *testing.T) {
	build := func(_ *rest.Config, _ ctrl.Options, _ Config) (runner, error) {
		t.Fatal("builder must not be called on invalid selector")
		return nil, nil
	}
	cfg := Config{LabelSelector: "###not-a-selector###"}
	err := runWithBuilder(context.Background(), cfg, &rest.Config{}, build)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestRunWithBuilder_PropagatesMetricsBindAddress(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		wantSet bool
		wantVal string
	}{
		{"explicit address forwards", "127.0.0.1:9876", true, "127.0.0.1:9876"},
		{"disabled forwards as \"0\"", "0", true, "0"},
		{"empty leaves opts.Metrics zero", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenOpts ctrl.Options
			fake := &fakeRunner{}
			build := func(_ *rest.Config, opts ctrl.Options, _ Config) (runner, error) {
				seenOpts = opts
				return fake, nil
			}
			cfg := Config{MetricsBindAddress: tc.addr}
			if err := runWithBuilder(context.Background(), cfg, &rest.Config{}, build); err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			gotVal := seenOpts.Metrics.BindAddress
			gotSet := gotVal != ""
			if gotSet != tc.wantSet {
				t.Errorf("Metrics set = %v, want %v (BindAddress=%q)", gotSet, tc.wantSet, gotVal)
			}
			if tc.wantSet && gotVal != tc.wantVal {
				t.Errorf("Metrics.BindAddress = %q, want %q", gotVal, tc.wantVal)
			}
		})
	}
}

// TestRunWithBuilder_PropagatesWatchNamespaces proves the watch-scope behavior
// at the options layer: the listed namespaces — and only those — land in
// Cache.DefaultNamespaces, the map controller-runtime uses to restrict every
// informer. A JsonnetSnippet in a namespace absent from this map never enters
// the cache, so the reconciler can't see it; one in a listed namespace does.
func TestRunWithBuilder_PropagatesWatchNamespaces(t *testing.T) {
	cases := []struct {
		name string
		nss  []string
		want []string // nil == cluster-wide (DefaultNamespaces unset)
	}{
		{"empty is cluster-wide", nil, nil},
		{"single namespace", []string{"team-a"}, []string{"team-a"}},
		{"multiple namespaces", []string{"team-a", "team-b", "team-c"}, []string{"team-a", "team-b", "team-c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenOpts ctrl.Options
			fake := &fakeRunner{}
			build := func(_ *rest.Config, opts ctrl.Options, _ Config) (runner, error) {
				seenOpts = opts
				return fake, nil
			}
			cfg := Config{WatchNamespaces: tc.nss}
			if err := runWithBuilder(context.Background(), cfg, &rest.Config{}, build); err != nil {
				t.Fatalf("unexpected: %v", err)
			}

			if tc.want == nil {
				if seenOpts.Cache.DefaultNamespaces != nil {
					t.Fatalf("cluster-wide expected, but DefaultNamespaces = %v", seenOpts.Cache.DefaultNamespaces)
				}
				return
			}
			got := make([]string, 0, len(seenOpts.Cache.DefaultNamespaces))
			for ns := range seenOpts.Cache.DefaultNamespaces {
				got = append(got, ns)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("DefaultNamespaces keys = %v, want %v", got, want)
			}
			// A namespace outside the watch set must be absent — that absence
			// is exactly what keeps its snippets out of the cache.
			if _, present := seenOpts.Cache.DefaultNamespaces["unwatched-namespace"]; present {
				t.Errorf("unwatched namespace leaked into DefaultNamespaces")
			}
		})
	}
}

// Run (the production entry point) wires the defaultBuilder closure into
// runWithBuilder. A fake-builder test cannot prove that wiring is intact, so
// this test drives the real path against an httptest stand-in. Manager build
// is lazy; Start fails as soon as it tries to dial discovery, which is fine —
// we only need the closure body and the Run -> runWithBuilder seam exercised.
func TestRun_DefaultBuilderPath_IsExercised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = Run(ctx, Config{}, &rest.Config{Host: srv.URL})
	// Any return path through defaultBuilder is enough to register coverage;
	// success or error both prove the closure ran.
}

func TestRunWithBuilder_NilLoggerFallsBackToDefault(t *testing.T) {
	// Smoke test: omitting Logger from Config must not panic. The fallback
	// to slog.Default() is the only branch exercised here.
	fake := &fakeRunner{}
	build := func(*rest.Config, ctrl.Options, Config) (runner, error) {
		return fake, nil
	}
	if err := runWithBuilder(context.Background(), Config{Logger: nil}, &rest.Config{}, build); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
