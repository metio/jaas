/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package startupcheck

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var snippetGVK = schema.GroupVersionKind{Group: "jaas.metio.wtf", Version: "v1", Kind: "JsonnetSnippet"}

func target() Target {
	return Target{GVK: snippetGVK, Group: "jaas.metio.wtf", Resource: "jsonnetsnippets"}
}

// mapperWith builds a RESTMapper that resolves only the listed GVKs; anything
// else returns a no-match error, standing in for an uninstalled CRD.
func mapperWith(gvks ...schema.GroupVersionKind) apimeta.RESTMapper {
	m := apimeta.NewDefaultRESTMapper(nil)
	for _, gvk := range gvks {
		m.Add(gvk, apimeta.RESTScopeNamespace)
	}
	return m
}

type fakeReviewer struct {
	allowed bool
	err     error
}

func (f fakeReviewer) Create(_ context.Context, sar *authorizationv1.SelfSubjectAccessReview, _ metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error) {
	if f.err != nil {
		return nil, f.err
	}
	sar.Status.Allowed = f.allowed
	return sar, nil
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name        string
		mapper      apimeta.RESTMapper
		reviewer    AccessReviewer
		wantProblem bool
		wantReason  string
	}{
		{
			name:     "crd present and permitted",
			mapper:   mapperWith(snippetGVK),
			reviewer: fakeReviewer{allowed: true},
		},
		{
			name:        "crd not installed",
			mapper:      mapperWith(), // resolves nothing
			reviewer:    fakeReviewer{allowed: true},
			wantProblem: true,
			wantReason:  "CRD is not installed",
		},
		{
			name:        "rbac forbidden",
			mapper:      mapperWith(snippetGVK),
			reviewer:    fakeReviewer{allowed: false},
			wantProblem: true,
			wantReason:  "operator ServiceAccount may not list it — grant list+watch in the operator ClusterRole",
		},
		{
			name:        "access review errors",
			mapper:      mapperWith(snippetGVK),
			reviewer:    fakeReviewer{err: errors.New("boom")},
			wantProblem: true,
			wantReason:  "cannot check RBAC (list): boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Checker{Mapper: tt.mapper, Review: tt.reviewer, Logger: slog.Default()}
			problems := c.Check(context.Background(), []Target{target()})
			if !tt.wantProblem {
				if len(problems) != 0 {
					t.Fatalf("want no problems, got %v", problems)
				}
				return
			}
			if len(problems) != 1 {
				t.Fatalf("want 1 problem, got %d: %v", len(problems), problems)
			}
			if problems[0].Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", problems[0].Reason, tt.wantReason)
			}
		})
	}
}

// TestLogUntilReady_ReturnsWhenSatisfied proves the loop exits (does not block)
// once prerequisites are met.
func TestLogUntilReady_ReturnsWhenSatisfied(t *testing.T) {
	c := &Checker{Mapper: mapperWith(snippetGVK), Review: fakeReviewer{allowed: true}, Logger: slog.Default()}
	done := make(chan struct{})
	go func() {
		c.LogUntilReady(context.Background(), []Target{target()}, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("LogUntilReady did not return when prerequisites were satisfied")
	}
}

// TestLogUntilReady_StopsOnContextCancel proves an unsatisfiable check exits on
// context cancellation rather than looping forever.
func TestLogUntilReady_StopsOnContextCancel(t *testing.T) {
	c := &Checker{Mapper: mapperWith(), Review: fakeReviewer{allowed: false}, Logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.LogUntilReady(ctx, []Target{target()}, time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("LogUntilReady did not stop on context cancel")
	}
}
