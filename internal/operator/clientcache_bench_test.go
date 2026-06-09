/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	jaasv1 "github.com/metio/jaas/api/v1"
)

func benchScheme(b *testing.B) *runtime.Scheme {
	b.Helper()
	s := runtime.NewScheme()
	if err := jaasv1.AddToScheme(s); err != nil {
		b.Fatalf("AddToScheme: %v", err)
	}
	return s
}

// BenchmarkTenantClient_WithCache pins the per-reconcile cost of the
// impersonating-client lookup once a token is cached. The cached path skips
// client.New (RESTMapper + transport construction) — dropping the call from
// the per-event hot path.
//
// Reference baseline (AMD Ryzen 7 5700U, dev container, on 2026-06-11):
//
//	BenchmarkTenantClient_WithCache: ~170 ns/op,    2 allocs/op (cache hit path)
//	BenchmarkTenantClient_NoCache:   ~18 µs/op,   146 allocs/op (client.New per call)
//
// Treat as a regression baseline; absolute numbers vary by host. The 100×
// gap is the load-bearing observation — client.New builds a fresh
// RESTMapper + transport per call, so dropping it from the per-event hot
// path frees most of the per-reconcile setup cost.
func BenchmarkTenantClient_WithCache(b *testing.B) {
	scheme := benchScheme(b)
	stub := &stubMinter{token: "tok-1", expires: time.Now().Add(1 * time.Hour)}
	r := &SnippetReconciler{
		Client:      fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme:      scheme,
		RestConfig:  &rest.Config{Host: "http://example.test"},
		TokenCache:  newTokenCache(stub),
		ClientCache: newTenantClientCache(),
	}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}
	// Prime the cache so b.N measures hit-path only.
	if _, err := r.tenantClient(context.Background(), snip); err != nil {
		b.Fatalf("prime: %v", err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := r.tenantClient(context.Background(), snip); err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
	}
}

func BenchmarkTenantClient_NoCache(b *testing.B) {
	scheme := benchScheme(b)
	stub := &stubMinter{token: "tok-1", expires: time.Now().Add(1 * time.Hour)}
	r := &SnippetReconciler{
		Client:     fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme:     scheme,
		RestConfig: &rest.Config{Host: "http://example.test"},
		TokenCache: newTokenCache(stub),
		// ClientCache deliberately nil — every call hits client.New.
	}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a"},
		Spec:       jaasv1.JsonnetSnippetSpec{ServiceAccountName: "tenant"},
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := r.tenantClient(context.Background(), snip); err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
	}
}
