/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// withSpanRecorder installs an in-memory SpanRecorder as the global
// tracer provider for the duration of the test and returns it. Tests
// drive the reconciler under this provider, then inspect Ended() spans
// to assert the expected tree was emitted.
func withSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return sr
}

// spanNames returns the names of every Ended() span, in order.
func spanNames(sr *tracetest.SpanRecorder) []string {
	out := make([]string, 0, len(sr.Ended()))
	for _, s := range sr.Ended() {
		out = append(out, s.Name())
	}
	return out
}

// findSpan returns the first ended span whose Name equals name, or
// nil if none matched.
func findSpan(sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	for _, s := range sr.Ended() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// TestEnvtest_Tracing_HappyReconcileEmitsFullTree drives one
// end-to-end Reconcile under the recording tracer and asserts the
// expected span hierarchy: Reconcile root → resolveSource +
// resolveLibraries + evaluate + publish children. Pairs attribute
// checks for the most operator-useful fields so a future drift in
// attribute names is caught immediately.
func TestEnvtest_Tracing_HappyReconcileEmitsFullTree(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "trace-demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{ ok: true, n: 42 }`},
			},
			Output: jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := directReconciler(t, c, true)
	// Run the finalizer-attach reconcile under the default tracer
	// (uninstrumented) so the recorded trace starts fresh on the
	// spec-eval pass we actually care about.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}

	sr := withSpanRecorder(t)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec reconcile: %v", err)
	}

	wantAll := []string{
		"JsonnetSnippet.Reconcile",
		"snippet.resolveSource",
		"snippet.resolveLibraries",
		"snippet.evaluate",
		"snippet.publish",
	}
	got := spanNames(sr)
	for _, want := range wantAll {
		found := slices.Contains(got, want)
		if !found {
			t.Errorf("span %q never emitted; got=%v", want, got)
		}
	}

	// Attribute checks — pin the names so a refactor that renames
	// them gets caught here (telemetry pipelines key off these).
	resolveSrc := findSpan(sr, "snippet.resolveSource")
	if resolveSrc == nil {
		t.Fatal("resolveSource span not found")
	}
	if !hasAttribute(resolveSrc, "jaas.source.mode", "inline") {
		t.Errorf("resolveSource missing jaas.source.mode=inline; attrs=%v",
			resolveSrc.Attributes())
	}

	evalSpan := findSpan(sr, "snippet.evaluate")
	if evalSpan == nil {
		t.Fatal("evaluate span not found")
	}
	if !hasAttributeKey(evalSpan, "jaas.eval.renderedBytes") {
		t.Errorf("evaluate missing jaas.eval.renderedBytes; attrs=%v",
			evalSpan.Attributes())
	}

	pub := findSpan(sr, "snippet.publish")
	if pub == nil {
		t.Fatal("publish span not found")
	}
	if !hasAttributeKey(pub, "jaas.publish.revision") {
		t.Errorf("publish missing jaas.publish.revision; attrs=%v",
			pub.Attributes())
	}

	// And the child spans share the root's trace ID so a
	// trace-explorer renders them under one trace, not separate ones.
	root := findSpan(sr, "JsonnetSnippet.Reconcile")
	if root == nil {
		t.Fatal("root span not found")
	}
	rootTrace := root.SpanContext().TraceID()
	for _, child := range []string{"snippet.resolveSource", "snippet.resolveLibraries", "snippet.evaluate", "snippet.publish"} {
		s := findSpan(sr, child)
		if s == nil {
			continue
		}
		if s.SpanContext().TraceID() != rootTrace {
			t.Errorf("%s has trace=%s, want root trace=%s", child,
				s.SpanContext().TraceID(), rootTrace)
		}
	}
}

// TestReconcile_Tracing_RecordsErrorOnEvalFailure pins the
// span.RecordError contract for the failure path: a syntactically-
// broken snippet must produce a evaluate span flagged with the error.
func TestReconcile_Tracing_RecordsErrorOnEvalFailure(t *testing.T) {
	sr := withSpanRecorder(t)

	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "trace-broken", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "local x ="},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)
	r := directReconciler(t, c, false)
	// Drive past finalizer + into eval failure.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec reconcile: %v", err)
	}

	evalSpan := findSpan(sr, "snippet.evaluate")
	if evalSpan == nil {
		t.Fatal("evaluate span not found")
	}
	if !hasAttribute(evalSpan, "jaas.reason", ReasonEvaluationFailed) {
		t.Errorf("evaluate span missing jaas.reason=%s; attrs=%v",
			ReasonEvaluationFailed, evalSpan.Attributes())
	}
}

func hasAttribute(s sdktrace.ReadOnlySpan, key, value string) bool {
	for _, attr := range s.Attributes() {
		if string(attr.Key) == key && attr.Value.AsString() == value {
			return true
		}
	}
	return false
}

func hasAttributeKey(s sdktrace.ReadOnlySpan, key string) bool {
	for _, attr := range s.Attributes() {
		if string(attr.Key) == key {
			return true
		}
	}
	return false
}
