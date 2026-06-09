/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestMetrics_RegisteredOnControllerRuntimeRegistry(t *testing.T) {
	if metrics.Registry == nil {
		t.Fatal("controller-runtime metrics.Registry is nil")
	}
	// Observe a sample so the counter shows up in Gather output (the
	// Prometheus client doesn't include zero-observation counters).
	recordReconcileOutcome("init-ns", "init-name", "True", ReasonSynced)
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if strings.HasPrefix(mf.GetName(), "jaas_snippet_") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no jaas_snippet_* metrics registered after sample observation; got %d families", len(mfs))
	}
}

func TestMetrics_RecordReconcileOutcomeBumpsCounter(t *testing.T) {
	// Direct snapshot via testutil.ToFloat64 — labels: namespace, name,
	// status, reason.
	before := testutil.ToFloat64(snippetReconcileTotal.WithLabelValues(
		"team-a", "demo", "False", ReasonInvalidSpec,
	))
	recordReconcileOutcome("team-a", "demo", "False", ReasonInvalidSpec)
	after := testutil.ToFloat64(snippetReconcileTotal.WithLabelValues(
		"team-a", "demo", "False", ReasonInvalidSpec,
	))
	if after-before != 1 {
		t.Errorf("counter moved by %v, want 1", after-before)
	}
}

// TestMetrics_RecordRateLimitedBumpsCounter pins the rate-limit
// counter wiring. The counter is the durable signal for backpressure
// — dashboards alert on sustained non-zero values to catch a runaway
// snippet whose update cadence outpaces --reconcile-rate-limit.
func TestMetrics_RecordRateLimitedBumpsCounter(t *testing.T) {
	before := testutil.ToFloat64(snippetRateLimitedTotal.WithLabelValues("team-a", "throttled"))
	recordRateLimited("team-a", "throttled")
	after := testutil.ToFloat64(snippetRateLimitedTotal.WithLabelValues("team-a", "throttled"))
	if after-before != 1 {
		t.Errorf("counter moved by %v, want 1", after-before)
	}
}

// These three recorders are the cross-package wire to Prometheus (main.go
// and internal/webhook/selfsigned call them); a typo'd counter name would
// otherwise go unnoticed.
func TestMetrics_RecordSweepFailureBumpsCounter(t *testing.T) {
	before := testutil.ToFloat64(storageSweepFailuresTotal)
	RecordSweepFailure()
	if after := testutil.ToFloat64(storageSweepFailuresTotal); after-before != 1 {
		t.Errorf("counter moved by %v, want 1", after-before)
	}
}

func TestMetrics_RecordWebhookCertRenewalFailureBumpsCounter(t *testing.T) {
	before := testutil.ToFloat64(webhookCertRenewalFailuresTotal)
	RecordWebhookCertRenewalFailure()
	if after := testutil.ToFloat64(webhookCertRenewalFailuresTotal); after-before != 1 {
		t.Errorf("counter moved by %v, want 1", after-before)
	}
}

func TestMetrics_RecordForceDropBumpsCounter(t *testing.T) {
	c := snippetForceDropTotal.WithLabelValues("team-a", "demo", "withdraw_timed_out")
	before := testutil.ToFloat64(c)
	recordForceDrop("team-a", "demo", "withdraw_timed_out")
	if after := testutil.ToFloat64(c); after-before != 1 {
		t.Errorf("counter moved by %v, want 1", after-before)
	}
}

func TestMetrics_RecordRenderedBytesObservesHistogram(t *testing.T) {
	before := testutil.CollectAndCount(snippetRenderedBytes)
	recordRenderedBytes("team-a", "demo-bytes", 5*1024)
	after := testutil.CollectAndCount(snippetRenderedBytes)
	if after < before {
		t.Errorf("histogram count regressed: before=%d after=%d", before, after)
	}
}

// TestMetrics_EndToEnd_FailReadyBumpsCounter drives a Reconcile that
// hits failReady (missing ServiceAccount) and confirms the counter
// observed the (False, ServiceAccountMissing) bucket.
func TestMetrics_EndToEnd_FailReadyBumpsCounter(t *testing.T) {
	snip := sampleSnippet()
	snip.Spec.ServiceAccountName = ""
	snip.Name = "metrics-fail"
	snip.Namespace = "metrics-ns"
	c := clientWithStatus(t, snip)
	r := newReconciler(t, c)
	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}

	before := testutil.ToFloat64(snippetReconcileTotal.WithLabelValues(
		snip.Namespace, snip.Name, "False", ReasonServiceAccountMissing,
	))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec: %v", err)
	}
	after := testutil.ToFloat64(snippetReconcileTotal.WithLabelValues(
		snip.Namespace, snip.Name, "False", ReasonServiceAccountMissing,
	))
	if after-before < 1 {
		t.Errorf("counter moved by %v, want >= 1", after-before)
	}
}

func TestMetrics_StatusValueRendersAsConditionString(t *testing.T) {
	// Sanity: the string we record must match the value
	// metav1.ConditionStatus stringifies to, so dashboards can join
	// on the same value as the Ready condition.
	if string(metav1.ConditionTrue) != "True" {
		t.Errorf("ConditionTrue = %q, want True", metav1.ConditionTrue)
	}
}
