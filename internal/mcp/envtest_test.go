/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// envtestClient boots a real kube-apiserver + etcd with the jaas CRDs installed
// and returns a client wired to it. It skips when KUBEBUILDER_ASSETS is unset
// (no asset bundle), matching the operator package's envtest convention.
func envtestClient(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets not available (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}
	_, here, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file via runtime.Caller")
	}
	crdDir := filepath.Join(filepath.Dir(here), "..", "..", "config", "crd", "bases")

	env := &envtest.Environment{CRDDirectoryPaths: []string{crdDir}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := jaasv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// TestEnvtest_ToolsAgainstRealAPIServer exercises the read and write tools end
// to end against a real apiserver: a real CRD schema, real List/Get, and real
// MergeFrom patches — the integration layer the fake client can't fully model.
func TestEnvtest_ToolsAgainstRealAPIServer(t *testing.T) {
	c := envtestClient(t)
	ctx := context.Background()

	// The "default" namespace exists in a fresh envtest; create the snippet there.
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "envtest-demo"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{Files: map[string]string{"main.jsonnet": "{}"}},
		},
	}
	if err := c.Create(ctx, snip); err != nil {
		t.Fatalf("create snippet: %v", err)
	}

	cfg := Config{KubeClient: c, RunbookBaseURL: testRunbookBase, AllowMutations: true}

	// list_snippets finds it.
	_, listOut, err := cfg.listSnippetsHandler(ctx, nil, listSnippetsInput{Namespace: "default"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listOut.Snippets) != 1 || listOut.Snippets[0].Name != "envtest-demo" {
		t.Fatalf("list returned %+v", listOut.Snippets)
	}

	// get_snippet returns it (no status yet → Ready Unknown).
	_, getOut, err := cfg.getSnippetHandler(ctx, nil, getSnippetInput{Namespace: "default", Name: "envtest-demo"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if getOut.Ready != "Unknown" || getOut.Suspended {
		t.Fatalf("get returned %+v", getOut)
	}

	// suspend_snippet patches spec.suspend on the real server.
	if _, _, err := cfg.suspendSnippetHandler(ctx, nil, mutateInput{Namespace: "default", Name: "envtest-demo"}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	var after jaasv1.JsonnetSnippet
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "envtest-demo"}, &after); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if !after.Spec.Suspend {
		t.Fatal("suspend did not persist on the real apiserver")
	}

	// reconcile_snippet stamps the annotation on the real server.
	if _, _, err := cfg.reconcileSnippetHandler(ctx, nil, mutateInput{Namespace: "default", Name: "envtest-demo"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "envtest-demo"}, &after); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if after.Annotations["reconcile.fluxcd.io/requestedAt"] == "" {
		t.Fatal("reconcile annotation did not persist")
	}
}
