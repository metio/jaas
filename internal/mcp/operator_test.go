/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	jaasv1 "github.com/metio/jaas/api/v1"
)

const testRunbookBase = "https://jaas.projects.metio.wtf/runbooks/"

func newSnippet(namespace, name string, suspend bool, ready metav1.ConditionStatus, reason, message string) *jaasv1.JsonnetSnippet {
	return &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       jaasv1.JsonnetSnippetSpec{Suspend: suspend},
		Status: jaasv1.SyncStatus{
			ObservedGeneration: 1,
			Revision:           "sha256:" + name,
			ArtifactURL:        "http://store/" + namespace + "/" + name + ".tar.gz",
			Conditions: []metav1.Condition{{
				Type:    jaasv1.ConditionReady,
				Status:  ready,
				Reason:  reason,
				Message: message,
			}},
		},
	}
}

// fakeClient builds a controller-runtime fake client seeded with the given
// snippets — the api/v1 scheme is all the read tools need.
func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := jaasv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestListSnippetsHandler(t *testing.T) {
	cfg := Config{KubeClient: fakeClient(
		t,
		newSnippet("team-a", "dash", false, metav1.ConditionTrue, "Synced", "ok"),
		newSnippet("team-b", "panel", true, metav1.ConditionFalse, "Suspended", "paused"),
	)}

	t.Run("all namespaces", func(t *testing.T) {
		res, out, err := cfg.listSnippetsHandler(context.Background(), nil, listSnippetsInput{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("unexpected tool error: %s", textContent(t, res))
		}
		if len(out.Snippets) != 2 {
			t.Fatalf("got %d snippets, want 2: %+v", len(out.Snippets), out.Snippets)
		}
	})

	t.Run("scoped to namespace", func(t *testing.T) {
		_, out, err := cfg.listSnippetsHandler(context.Background(), nil, listSnippetsInput{Namespace: "team-a"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out.Snippets) != 1 || out.Snippets[0].Name != "dash" {
			t.Fatalf("namespace scope wrong: %+v", out.Snippets)
		}
		s := out.Snippets[0]
		if s.Ready != "True" || s.Reason != "Synced" || s.Suspended {
			t.Fatalf("summary fields wrong: %+v", s)
		}
		if s.Revision != "sha256:dash" || s.ArtifactURL == "" {
			t.Fatalf("revision/artifact missing: %+v", s)
		}
	})

	t.Run("empty namespace yields empty slice not nil", func(t *testing.T) {
		_, out, err := cfg.listSnippetsHandler(context.Background(), nil, listSnippetsInput{Namespace: "does-not-exist"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Snippets == nil {
			t.Fatal("Snippets is nil, want an empty slice so it marshals to []")
		}
		if len(out.Snippets) != 0 {
			t.Fatalf("want 0 snippets, got %+v", out.Snippets)
		}
	})
}

func TestGetSnippetHandler(t *testing.T) {
	snip := newSnippet("team-a", "dash", false, metav1.ConditionFalse, "RBACDenied", "the tenant SA cannot get the source")
	snip.Status.History = []jaasv1.RevisionEntry{
		{Revision: "sha256:new", Time: metav1.Now()},
		{Revision: "sha256:old", Time: metav1.Now()},
	}
	cfg := Config{KubeClient: fakeClient(t, snip), RunbookBaseURL: testRunbookBase}

	t.Run("full detail with runbook link", func(t *testing.T) {
		res, out, err := cfg.getSnippetHandler(context.Background(), nil, getSnippetInput{Namespace: "team-a", Name: "dash"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatalf("unexpected tool error: %s", textContent(t, res))
		}
		if out.Ready != "False" || out.Reason != "RBACDenied" {
			t.Fatalf("ready/reason wrong: %+v", out)
		}
		if want := testRunbookBase + "rbacdenied/"; out.RunbookURL != want {
			t.Fatalf("RunbookURL = %q, want %q", out.RunbookURL, want)
		}
		if len(out.History) != 2 || out.History[0].Revision != "sha256:new" {
			t.Fatalf("history wrong: %+v", out.History)
		}
	})

	t.Run("not found is a tool error", func(t *testing.T) {
		res, _, err := cfg.getSnippetHandler(context.Background(), nil, getSnippetInput{Namespace: "team-a", Name: "missing"})
		if err != nil {
			t.Fatalf("handler returned a Go error: %v", err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("expected IsError for a missing snippet, got %+v", res)
		}
	})

	t.Run("missing namespace or name is a tool error", func(t *testing.T) {
		res, _, _ := cfg.getSnippetHandler(context.Background(), nil, getSnippetInput{Name: "dash"})
		if res == nil || !res.IsError {
			t.Fatalf("expected IsError when namespace is empty, got %+v", res)
		}
	})
}

func TestReadyCondition_NoConditionIsUnknown(t *testing.T) {
	snip := &jaasv1.JsonnetSnippet{}
	status, reason, message := readyCondition(snip)
	if status != "Unknown" || reason != "" || message != "" {
		t.Fatalf("got (%q,%q,%q), want (Unknown,,)", status, reason, message)
	}
}

func TestRunbookURL(t *testing.T) {
	cfg := Config{RunbookBaseURL: testRunbookBase}
	if got := cfg.runbookURL("RBACDenied"); got != testRunbookBase+"rbacdenied/" {
		t.Fatalf("runbookURL = %q", got)
	}
	if got := cfg.runbookURL(""); got != "" {
		t.Fatalf("empty reason should yield empty URL, got %q", got)
	}
	if got := (Config{}).runbookURL("RBACDenied"); got != "" {
		t.Fatalf("no base URL should yield empty URL, got %q", got)
	}
}

// TestOperatorTools_RegisteredWhenClientPresent proves the read tools are wired
// into the server (and absent without a client) and callable over the protocol.
func TestOperatorTools_RegisteredWhenClientPresent(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		Version:    "test",
		KubeClient: fakeClient(t, newSnippet("team-a", "dash", false, metav1.ConditionTrue, "Synced", "ok")),
	}
	server := NewServer(cfg)

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
	got := map[string]bool{}
	for _, tool := range lt.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"render_jsonnet", "validate_jsonnet", "list_snippets", "get_snippet"} {
		if !got[want] {
			t.Errorf("tool %q not registered; have %v", want, got)
		}
	}

	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "list_snippets",
		Arguments: map[string]any{"namespace": "team-a"},
	})
	if err != nil {
		t.Fatalf("call list_snippets: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_snippets tool error: %s", textContent(t, res))
	}
}

func TestOperatorTools_AbsentWithoutClient(t *testing.T) {
	server := NewServer(Config{Version: "test"})
	ctx := context.Background()
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
	for _, tool := range lt.Tools {
		if tool.Name == "list_snippets" || tool.Name == "get_snippet" {
			t.Errorf("operator tool %q registered without a KubeClient", tool.Name)
		}
	}
}
