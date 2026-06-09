/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
	"github.com/metio/jaas/internal/storage"
)

// BenchmarkReconcile_HappyPath measures the cost of one full reconcile of a
// trivial JsonnetSnippet against a real envtest apiserver, with a real
// Publisher writing to a tempdir-backed filesystem Store. Excludes the
// manager's watch latency — that's controller-runtime, not us — so the
// number captures only what JaaS itself spends per reconcile:
//   - apiserver Get on the snippet + cycle check
//   - go-jsonnet eval of a single-line body
//   - pre-publish staleness gate (APIReader.Get on the snippet)
//   - storage Put (tarball, atomic rename) + ExternalArtifact CreateOrUpdate
//   - status subresource write via statusretry (re-Get + Update on both
//     the snippet and the ExternalArtifact)
//
// Run:
//
//	ilo bash -c 'go test -bench=BenchmarkReconcile -benchmem -run=^$ ./internal/operator/'
//
// Treat the numbers as a *regression baseline* — they vary with envtest's
// apiserver+etcd startup cost and the host's IO speed, so absolute values
// won't generalize across machines. Watch the trend, not the magnitude.
//
// Reference baseline (AMD Ryzen 7 5700U, dev container, post-statusretry +
// post-publishConsistencyGate + post-CreateOrUpdate, on 2026-06-10):
//   - BenchmarkReconcile_HappyPath:    ~18.9 ms/op,  ~60 KiB/op, ~680 allocs/op
//   - BenchmarkReconcile_Concurrent:   ~ 2.9 ms/op,  ~62 KiB/op, ~685 allocs/op
//
// The pre-statusretry numbers were ~3.4× lower (~5.6 ms / ~828 µs). The
// added cost is ~4 extra apiserver round-trips per reconcile:
//
//   - statusretry's re-Get on the snippet's Status().Update
//   - statusretry's re-Get on the ExternalArtifact's Status().Update
//   - publishConsistencyGate's APIReader.Get pre-publish
//   - CreateOrUpdate's internal Get when the EA already exists
//
// The trade-off is correctness vs. throughput. Each Conflict-loss-and-redo
// previously cost a FULL reconcile (re-fetch source, re-eval, re-upload)
// — far more than the marginal Gets statusretry now does up front. Net
// throughput under apiserver contention is HIGHER; net throughput on the
// unloaded happy path is lower. For jaas's expected workload (~33
// reconciles/sec across 10k snippets at 5-min intervals), the concurrent
// throughput of ~345 reconciles/sec leaves an order-of-magnitude headroom.
func BenchmarkReconcile_HappyPath(b *testing.B) {
	restCfg := envtestConfig(b)
	scheme := envtestScheme(b)
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		b.Fatalf("client.New: %v", err)
	}
	ns := freshNamespace(b, c)

	store, err := storage.New(b.TempDir())
	if err != nil {
		b.Fatalf("storage.New: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	r := &SnippetReconciler{
		Client:    c,
		Scheme:    scheme,
		Logger:    discardLoggerEnvtest(),
		Publisher: NewPublisher(store, "http://jaas-storage.bench.svc.cluster.local:8082"),
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := mintBenchSnippet(b, c, ns, i)
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			b.Fatalf("reconcile iter %d: %v", i, err)
		}
	}
}

// BenchmarkReconcile_Concurrent exercises the reconciler under parallel load.
// The real-world workqueue serializes per-snippet — concurrency only matters
// when the operator is reconciling MANY different snippets at once. This
// bench creates one snippet per goroutine and measures the wall-clock spent
// reconciling them concurrently, which is the path where lock contention
// inside the reconciler (token cache, rate limiter, OCI library map) shows
// up.
func BenchmarkReconcile_Concurrent(b *testing.B) {
	restCfg := envtestConfig(b)
	scheme := envtestScheme(b)
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		b.Fatalf("client.New: %v", err)
	}
	ns := freshNamespace(b, c)

	store, err := storage.New(b.TempDir())
	if err != nil {
		b.Fatalf("storage.New: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	r := &SnippetReconciler{
		Client:    c,
		Scheme:    scheme,
		Logger:    discardLoggerEnvtest(),
		Publisher: NewPublisher(store, "http://jaas-storage.bench.svc.cluster.local:8082"),
	}

	var counter atomic.Int64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := counter.Add(1)
			key := mintBenchSnippet(b, c, ns, int(i))
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				b.Fatalf("parallel reconcile %d: %v", i, err)
			}
		}
	})
}

// mintBenchSnippet creates a trivial JsonnetSnippet with a deterministic name
// and returns its key. The body is intentionally minimal — the bench measures
// reconciler overhead, not jsonnet eval cost.
func mintBenchSnippet(b *testing.B, c client.Client, ns string, i int) types.NamespacedName {
	b.Helper()
	name := fmt.Sprintf("bench-%06d", i)
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	if err := c.Create(context.Background(), snip); err != nil {
		b.Fatalf("create snippet %s: %v", name, err)
	}
	return types.NamespacedName{Name: name, Namespace: ns}
}
