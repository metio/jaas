/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"strings"
	"testing"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// getSnippet re-reads a snippet from the client so a test can assert a mutation
// actually persisted.
func getSnippet(t *testing.T, c client.Client, namespace, name string) *jaasv1.JsonnetSnippet {
	t.Helper()
	var snip jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &snip); err != nil {
		t.Fatalf("re-get %s/%s: %v", namespace, name, err)
	}
	return &snip
}

func TestSuspendResumeHandlers(t *testing.T) {
	snip := newSnippet("team-a", "dash", false, metav1.ConditionTrue, "Synced", "ok")
	c := fakeClient(t, snip)
	cfg := Config{KubeClient: c, AllowMutations: true}
	in := mutateInput{Namespace: "team-a", Name: "dash"}

	// suspend a running snippet
	_, out, err := cfg.suspendSnippetHandler(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if out.Result != "suspended" {
		t.Fatalf("suspend result = %q, want suspended", out.Result)
	}
	if !getSnippet(t, c, "team-a", "dash").Spec.Suspend {
		t.Fatal("spec.suspend was not set to true")
	}

	// suspend again is a no-op
	_, out, _ = cfg.suspendSnippetHandler(context.Background(), nil, in)
	if out.Result != "already suspended" {
		t.Fatalf("re-suspend result = %q, want 'already suspended'", out.Result)
	}

	// resume clears it
	_, out, err = cfg.resumeSnippetHandler(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if out.Result != "resumed" {
		t.Fatalf("resume result = %q, want resumed", out.Result)
	}
	if getSnippet(t, c, "team-a", "dash").Spec.Suspend {
		t.Fatal("spec.suspend was not cleared")
	}

	// resume again is a no-op
	_, out, _ = cfg.resumeSnippetHandler(context.Background(), nil, in)
	if out.Result != "not suspended; no change" {
		t.Fatalf("re-resume result = %q", out.Result)
	}
}

func TestReconcileSnippetHandler(t *testing.T) {
	snip := newSnippet("team-a", "dash", false, metav1.ConditionTrue, "Synced", "ok")
	c := fakeClient(t, snip)
	cfg := Config{KubeClient: c, AllowMutations: true}

	res, out, err := cfg.reconcileSnippetHandler(context.Background(), nil, mutateInput{Namespace: "team-a", Name: "dash"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected tool error: %s", textContent(t, res))
	}
	if !strings.HasPrefix(out.Result, "reconcile requested at ") {
		t.Fatalf("result = %q, want a 'reconcile requested at' message", out.Result)
	}
	got := getSnippet(t, c, "team-a", "dash")
	if token := got.Annotations[fluxmeta.ReconcileRequestAnnotation]; token == "" {
		t.Fatalf("annotation %s not set", fluxmeta.ReconcileRequestAnnotation)
	}
}

func TestMutateSnippet_Errors(t *testing.T) {
	cfg := Config{KubeClient: fakeClient(t), AllowMutations: true}

	t.Run("not found", func(t *testing.T) {
		res, _, err := cfg.suspendSnippetHandler(context.Background(), nil, mutateInput{Namespace: "x", Name: "missing"})
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("expected IsError for a missing snippet, got %+v", res)
		}
	})

	t.Run("missing namespace or name", func(t *testing.T) {
		res, _, _ := cfg.reconcileSnippetHandler(context.Background(), nil, mutateInput{Name: "dash"})
		if res == nil || !res.IsError {
			t.Fatalf("expected IsError when namespace is empty, got %+v", res)
		}
	})
}

// TestMutationTools_GatedByAllowMutations proves the write tools register only
// when AllowMutations is set (and a client is present), and never otherwise.
func TestMutationTools_GatedByAllowMutations(t *testing.T) {
	mutationTools := []string{"reconcile_snippet", "suspend_snippet", "resume_snippet"}

	tests := []struct {
		name           string
		cfg            Config
		wantMutational bool
	}{
		{
			name:           "client + mutations on",
			cfg:            Config{Version: "test", KubeClient: fakeClient(t), AllowMutations: true},
			wantMutational: true,
		},
		{
			name:           "client + mutations off",
			cfg:            Config{Version: "test", KubeClient: fakeClient(t), AllowMutations: false},
			wantMutational: false,
		},
		{
			name:           "no client, mutations on",
			cfg:            Config{Version: "test", AllowMutations: true},
			wantMutational: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			server := NewServer(tt.cfg)
			clientT, serverT := mcpsdk.NewInMemoryTransports()
			ss, err := server.Connect(ctx, serverT, nil)
			if err != nil {
				t.Fatalf("server connect: %v", err)
			}
			defer func() { _ = ss.Close() }()
			c := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil)
			cs, err := c.Connect(ctx, clientT, nil)
			if err != nil {
				t.Fatalf("client connect: %v", err)
			}
			defer func() { _ = cs.Close() }()

			lt, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("list tools: %v", err)
			}
			present := map[string]bool{}
			for _, tool := range lt.Tools {
				present[tool.Name] = true
			}
			for _, name := range mutationTools {
				if present[name] != tt.wantMutational {
					t.Errorf("tool %q present=%v, want %v", name, present[name], tt.wantMutational)
				}
			}
		})
	}
}
