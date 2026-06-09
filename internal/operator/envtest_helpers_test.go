/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// envtestScheme is the scheme every envtest client uses: JaaS v1 types plus
// an Unstructured-backed ExternalArtifact registration. Separate from the
// fake-client scheme because envtest tests need real apiserver round-trips.
// Accepts testing.TB so benchmarks (testing.B) can share the helper with
// the wider Test* suite without per-helper duplication.
func envtestScheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := jaasv1.AddToScheme(s); err != nil {
		t.Fatalf("jaas v1 AddToScheme: %v", err)
	}
	if err := apiextv1.AddToScheme(s); err != nil {
		t.Fatalf("apiextv1 AddToScheme: %v", err)
	}
	s.AddKnownTypeWithName(externalArtifactGVK, &unstructured.Unstructured{})
	gvkList := externalArtifactGVK
	gvkList.Kind = externalArtifactGVK.Kind + "List"
	s.AddKnownTypeWithName(gvkList, &unstructured.UnstructuredList{})
	return s
}

// envtestClient returns a controller-runtime client against the shared envtest
// apiserver, configured with the envtestScheme.
func envtestClient(t testing.TB) client.Client {
	t.Helper()
	cfg := envtestConfig(t)
	c, err := client.New(cfg, client.Options{Scheme: envtestScheme(t)})
	if err != nil {
		t.Fatalf("envtest client.New: %v", err)
	}
	return c
}

// nsCounter generates monotonically-increasing namespace suffixes so parallel
// envtest tests don't collide on the same name.
var nsCounter struct {
	sync.Mutex
	n int
}

// freshNamespace creates a unique namespace for a single test and registers a
// Cleanup that deletes it on test exit. Returns the namespace name.
func freshNamespace(t testing.TB, c client.Client) string {
	t.Helper()
	nsCounter.Lock()
	nsCounter.n++
	suffix := nsCounter.n
	nsCounter.Unlock()

	// RFC 1123 namespace names are lowercase alphanum + dashes; replace
	// every other rune so test names like Foo_Bar/Baz still produce valid
	// kubectl-style identifiers.
	sanitize := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			default:
				b.WriteByte('-')
			}
		}
		return strings.Trim(b.String(), "-")
	}
	name := fmt.Sprintf("jaas-test-%d-%s", suffix, sanitize(t.Name()))
	// Kubernetes caps namespace names at 63 chars.
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(coreNamespaceGVK)
	ns.SetName(name)
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace %q: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), ns)
	})
	return name
}

// coreNamespaceGVK is the v1.Namespace identity used for raw-create through
// the Unstructured client (the typed corev1 isn't in our scheme).
var coreNamespaceGVK = schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}

// pollUntil retries fn until it returns true or the deadline expires. The
// returned error gives the most recent assertion failure when the deadline
// hits, so failures are actionable.
func pollUntil(t *testing.T, timeout time.Duration, fn func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastReason string
	for time.Now().Before(deadline) {
		ok, reason := fn()
		if ok {
			return
		}
		lastReason = reason
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition never satisfied within %v: %s", timeout, lastReason)
}

// snippetReady refetches the snippet and returns true when its Ready
// condition matches (status, reason). pollUntil's reason argument tells the
// test what the current condition is when the deadline hits.
func snippetReady(c client.Client, key types.NamespacedName, want metav1.ConditionStatus, wantReason string) func() (bool, string) {
	return func() (bool, string) {
		var snip jaasv1.JsonnetSnippet
		if err := c.Get(context.Background(), key, &snip); err != nil {
			if apierrors.IsNotFound(err) {
				return false, "snippet not found"
			}
			return false, "get: " + err.Error()
		}
		cond := apimeta.FindStatusCondition(snip.Status.Conditions, jaasv1.ConditionReady)
		if cond == nil {
			return false, "no Ready condition yet"
		}
		if cond.Status != want {
			return false, fmt.Sprintf("Ready.Status = %v want %v (reason=%q msg=%q)",
				cond.Status, want, cond.Reason, cond.Message)
		}
		if cond.Reason != wantReason {
			return false, fmt.Sprintf("Ready.Reason = %q want %q (msg=%q)",
				cond.Reason, wantReason, cond.Message)
		}
		return true, ""
	}
}

// discardLogger returns a logger that drops every record. Plenty noisy without
// it — controller-runtime's manager logs every reconcile.
func discardLoggerEnvtest() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runManagerInBackground starts an operator manager wired with the package's
// defaultBuilder against the supplied rest.Config and Config, and returns a
// cancel func that stops it cleanly. The wait channel reports the
// manager's final error (if any) when stopped.
func runManagerInBackground(t *testing.T, restCfg *rest.Config, cfg Config) (context.CancelFunc, <-chan error) {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = discardLoggerEnvtest()
	}
	// Multiple envtest cases boot a manager in the same process; without
	// this flag controller-runtime's metrics-registry uniqueness check
	// fails on the second build.
	cfg.SkipControllerNameValidation = true
	// Most envtest cases don't provision per-snippet SAs + RBAC, so the
	// reconciler would Fail with Forbidden once it tried to impersonate.
	// envtest_impersonation_test.go explicitly opts back in by setting
	// SkipImpersonation=false and seeding real SAs.
	cfg.SkipImpersonation = true
	if cfg.MetricsBindAddress == "" {
		cfg.MetricsBindAddress = "0"
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithBuilder(ctx, cfg, restCfg, defaultBuilder)
	}()
	return cancel, done
}

// applyJsonnetSnippet creates a JsonnetSnippet using the supplied typed
// client. Returns the namespaced key for downstream polling.
func applyJsonnetSnippet(t *testing.T, c client.Client, snip *jaasv1.JsonnetSnippet) types.NamespacedName {
	t.Helper()
	if err := c.Create(context.Background(), snip); err != nil {
		t.Fatalf("create JsonnetSnippet %s/%s: %v", snip.Namespace, snip.Name, err)
	}
	return types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
}
