/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	jaasv1 "github.com/metio/jaas/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// countingClient answers every call with errs[n] for the n-th call, repeating
// the last entry once exhausted. The embedded nil client.Client is never
// reached: the tests only exercise the methods overridden here.
type countingClient struct {
	client.Client
	errs  []error
	calls int
}

func (c *countingClient) next() error {
	err := c.errs[min(c.calls, len(c.errs)-1)]
	c.calls++
	return err
}

func (c *countingClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return c.next()
}

func (c *countingClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return c.next()
}

func (c *countingClient) SubResource(string) client.SubResourceClient {
	return &countingSubResource{parent: c}
}

type countingSubResource struct {
	client.SubResourceClient
	parent *countingClient
}

func (s *countingSubResource) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return s.parent.next()
}

func unauthorized() error {
	return apierrors.NewUnauthorized("Unauthorized")
}

// refreshTo builds a refresh func handing out the supplied clients in order and
// counting how often it was asked.
func refreshTo(calls *int, clients ...client.Client) func(context.Context) (client.Client, error) {
	return func(context.Context) (client.Client, error) {
		c := clients[min(*calls, len(clients)-1)]
		*calls++
		return c, nil
	}
}

func TestRetryUnauthorizedClient_RetriesOnceWithFreshCredential(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{nil}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, fresh))

	if err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("Get after re-mint: %v", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
	if stale.calls != 1 || fresh.calls != 1 {
		t.Errorf("calls: stale = %d fresh = %d, want 1 and 1", stale.calls, fresh.calls)
	}
}

// Once the credential has been refreshed, later calls go straight to the new
// client — a single 401 must not cost a re-mint per call for the rest of the
// reconcile.
func TestRetryUnauthorizedClient_KeepsRefreshedClientForLaterCalls(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{nil}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, fresh))

	for range 3 {
		if err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{}); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
	if stale.calls != 1 {
		t.Errorf("stale client calls = %d, want 1", stale.calls)
	}
	if fresh.calls != 3 {
		t.Errorf("refreshed client calls = %d, want 3", fresh.calls)
	}
}

func TestRetryUnauthorizedClient_SecondUnauthorizedSurfacesWithExplanation(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{unauthorized()}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "tenant", "renderer", refreshTo(&refreshes, fresh))

	err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{})
	if !apierrors.IsUnauthorized(err) {
		t.Fatalf("error = %v, want an Unauthorized", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want exactly 1 (the retry must be bounded)", refreshes)
	}
	if !strings.Contains(err.Error(), "tenant/renderer") {
		t.Errorf("message %q does not name the ServiceAccount", err)
	}
}

func TestRetryUnauthorizedClient_OtherErrorsAreNotRetried(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "cm")
	stale := &countingClient{errs: []error{notFound}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, stale))

	err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("error = %v, want the NotFound passed through", err)
	}
	if refreshes != 0 {
		t.Errorf("refreshes = %d, want 0", refreshes)
	}
}

// A failed re-mint must keep the 401 at the head of the chain: the deletion
// path classifies on it, and a wrapper that swallowed it would turn an
// authentication failure into an unrecognisable minting failure.
func TestRetryUnauthorizedClient_RefreshFailurePreservesUnauthorized(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	c := newRetryUnauthorizedClient(stale, "ns", "sa", func(context.Context) (client.Client, error) {
		return nil, errors.New("TokenRequest refused")
	})

	err := c.Get(context.Background(), client.ObjectKey{}, &corev1.ConfigMap{})
	if !apierrors.IsUnauthorized(err) {
		t.Fatalf("error = %v, want an Unauthorized", err)
	}
	if !strings.Contains(err.Error(), "TokenRequest refused") {
		t.Errorf("message %q drops the minting failure", err)
	}
}

// Withdraw deletes the ExternalArtifact as the tenant, so the delete path is
// the one the stuck-finalizer report turned on.
func TestRetryUnauthorizedClient_DeleteRetries(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{nil}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, fresh))

	if err := c.Delete(context.Background(), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("Delete after re-mint: %v", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
}

func TestRetryUnauthorizedClient_SubresourceCallsRetryToo(t *testing.T) {
	stale := &countingClient{errs: []error{unauthorized()}}
	fresh := &countingClient{errs: []error{nil}}
	refreshes := 0
	c := newRetryUnauthorizedClient(stale, "ns", "sa", refreshTo(&refreshes, fresh))

	if err := c.Status().Patch(context.Background(), &corev1.ConfigMap{}, client.Merge); err != nil {
		t.Fatalf("status patch after re-mint: %v", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes)
	}
}

func TestForgetTenant_DropsTokenAndClientTogether(t *testing.T) {
	fm := &fakeMinter{token: "t1", expires: time.Now().Add(time.Hour)}
	r := &SnippetReconciler{TokenCache: newTokenCache(fm), ClientCache: newTenantClientCache()}
	if _, err := r.TokenCache.Token(context.Background(), "ns", "sa"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	r.ClientCache.Put(tenantCacheKey("ns", "sa"), "t1", 0, &countingClient{errs: []error{nil}})

	r.forgetTenant("ns", "sa")

	if _, _, ok := r.ClientCache.Get(tenantCacheKey("ns", "sa"), "t1"); ok {
		t.Error("client survived forgetTenant")
	}
	if _, err := r.TokenCache.Token(context.Background(), "ns", "sa"); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if fm.callCount() != 2 {
		t.Errorf("mint calls = %d, want 2 (forgetTenant must force a re-mint)", fm.callCount())
	}
}

// An Unauthorized reaching the deletion path has already survived the tenant
// client's own eviction and re-mint, so the ServiceAccount is gone. Waiting out
// MaxWithdrawWait for it would hold its namespace in Terminating for an hour
// behind an ExternalArtifact the namespace teardown is deleting anyway.
func TestClassifyWithdrawFailure_ForceDropsOnUnauthorized(t *testing.T) {
	fixed := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	r := &SnippetReconciler{
		MaxWithdrawWait: 1 * time.Hour,
		Clock:           func() time.Time { return fixed.Add(1 * time.Minute) },
	}
	snip := &jaasv1.JsonnetSnippet{}
	snip.DeletionTimestamp = &metav1.Time{Time: fixed}

	info, err := r.classifyWithdrawFailure(snip, unauthorized(), "timed_out", "permanent", "withdraw artifact")
	if err != nil {
		t.Fatalf("classify returned a requeue error: %v", err)
	}
	if info == nil {
		t.Fatal("Unauthorized did not force-drop the finalizer well inside MaxWithdrawWait")
	}
	if info.dropReason != "permanent" {
		t.Errorf("dropReason = %q, want %q", info.dropReason, "permanent")
	}
}

// A classifier drop fires well inside MaxWithdrawWait, so the event must not
// claim the bound elapsed. Reading "after 21m of failing Withdraw" on a snippet
// whose bound is an hour reads as the whole window having been spent retrying
// something that could never succeed — which is not what happened.
func TestForceDropFinalizer_UnrecoverableDropDoesNotClaimTheBoundElapsed(t *testing.T) {
	c := clientWithStatus(t, sampleSnippet())
	r := newReconciler(t, c)
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	r.forceDropFinalizer(context.Background(), discardLogger(), sampleSnippet(), &forceDropInfo{
		elapsed:       21 * time.Minute,
		dropReason:    "withdraw_permanent",
		unrecoverable: true,
		lastErr:       errors.New("namespace is terminating"),
	})

	msg := strings.Join(drainEvents(rec), "\n")
	if strings.Contains(msg, "of failing Withdraw") {
		t.Errorf("unrecoverable drop worded as a timeout: %q", msg)
	}
	if !strings.Contains(msg, "no retry can clear") {
		t.Errorf("message does not say the retry was pointless: %q", msg)
	}
}

// The timeout branch keeps its original wording: there the bound genuinely did
// elapse.
func TestForceDropFinalizer_TimeoutDropKeepsItsWording(t *testing.T) {
	c := clientWithStatus(t, sampleSnippet())
	r := newReconciler(t, c)
	rec := events.NewFakeRecorder(8)
	r.EventRecorder = rec

	r.forceDropFinalizer(context.Background(), discardLogger(), sampleSnippet(), &forceDropInfo{
		elapsed:    time.Hour,
		dropReason: "withdraw_timed_out",
		lastErr:    errors.New("backend unreachable"),
	})

	if msg := strings.Join(drainEvents(rec), "\n"); !strings.Contains(msg, "of failing Withdraw") {
		t.Errorf("timeout drop lost its wording: %q", msg)
	}
}

// A transient failure inside the bound still requeues — the force-drop must
// stay tied to causes that cannot heal.
func TestClassifyWithdrawFailure_TransientStillRequeues(t *testing.T) {
	fixed := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	r := &SnippetReconciler{
		MaxWithdrawWait: 1 * time.Hour,
		Clock:           func() time.Time { return fixed.Add(1 * time.Minute) },
	}
	snip := &jaasv1.JsonnetSnippet{}
	snip.DeletionTimestamp = &metav1.Time{Time: fixed}

	info, err := r.classifyWithdrawFailure(snip, apierrors.NewServiceUnavailable("apiserver down"), "timed_out", "permanent", "withdraw artifact")
	if info != nil {
		t.Fatal("a 503 force-dropped the finalizer; it should requeue")
	}
	if err == nil {
		t.Fatal("expected a requeue error")
	}
}
