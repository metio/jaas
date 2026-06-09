/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package selfsigned

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeVWCClient lets tests drive UpdateVWCCABundle without standing up an
// apiserver. It models optimistic concurrency: Get hands out a deep copy
// carrying the current resourceVersion, and Update rejects a stale write
// with a Conflict (when occ is on), bumping the version on success. The
// per-method knobs inject failures and forced conflicts.
type fakeVWCClient struct {
	mu        sync.Mutex
	vwc       *admissionv1.ValidatingWebhookConfiguration
	getErr    error
	updateErr error
	occ       bool // enforce resourceVersion on Update
	conflicts int  // force the next N Updates to Conflict
	updates   int
}

func (f *fakeVWCClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.vwc == nil || f.vwc.Name != name {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: "validatingwebhookconfigurations"}, name)
	}
	return f.vwc.DeepCopy(), nil
}

func (f *fakeVWCClient) Update(_ context.Context, vwc *admissionv1.ValidatingWebhookConfiguration, _ metav1.UpdateOptions) (*admissionv1.ValidatingWebhookConfiguration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.conflicts > 0 {
		f.conflicts--
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "validatingwebhookconfigurations"}, vwc.Name, errors.New("forced conflict"))
	}
	if f.occ && vwc.ResourceVersion != f.vwc.ResourceVersion {
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "validatingwebhookconfigurations"}, vwc.Name, errors.New("resourceVersion mismatch"))
	}
	updated := vwc.DeepCopy()
	rv, _ := strconv.Atoi(f.vwc.ResourceVersion)
	updated.ResourceVersion = strconv.Itoa(rv + 1)
	f.vwc = updated
	return f.vwc.DeepCopy(), nil
}

// caBundle returns the stored caBundle of the first webhook entry.
func (f *fakeVWCClient) caBundle() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vwc.Webhooks[0].ClientConfig.CABundle
}

func makeVWC(name string, n int, existingCABundle []byte) *admissionv1.ValidatingWebhookConfiguration {
	whs := make([]admissionv1.ValidatingWebhook, n)
	for i := range whs {
		whs[i] = admissionv1.ValidatingWebhook{
			Name: "wh-" + string(rune('a'+i)),
			ClientConfig: admissionv1.WebhookClientConfig{
				CABundle: existingCABundle,
			},
		}
	}
	return &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"},
		Webhooks:   whs,
	}
}

func setCA(_ []byte) func([]byte) []byte {
	return func([]byte) []byte { return []byte("---CA---\n") }
}

func TestUpdateVWCCABundle_StampsEveryWebhook(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("vjsonnet", 3, nil)}
	if err := UpdateVWCCABundle(context.Background(), f, "vjsonnet", setCA(nil)); err != nil {
		t.Fatalf("UpdateVWCCABundle: %v", err)
	}
	for i, wh := range f.vwc.Webhooks {
		if string(wh.ClientConfig.CABundle) != "---CA---\n" {
			t.Errorf("webhook %d caBundle = %q, want stamped", i, wh.ClientConfig.CABundle)
		}
	}
}

func TestUpdateVWCCABundle_NoOpWhenUnchanged(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, []byte("---CA---\n"))}
	if err := UpdateVWCCABundle(context.Background(), f, "vjsonnet", setCA(nil)); err != nil {
		t.Fatalf("UpdateVWCCABundle: %v", err)
	}
	if f.updates != 0 {
		t.Errorf("issued %d Updates despite no diff, want 0", f.updates)
	}
}

func TestUpdateVWCCABundle_NotFoundIsClearError(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("other", 1, nil)}
	err := UpdateVWCCABundle(context.Background(), f, "vjsonnet", setCA(nil))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a not-found message", err)
	}
}

func TestUpdateVWCCABundle_GetErrorPropagates(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil), getErr: errors.New("boom")}
	if err := UpdateVWCCABundle(context.Background(), f, "vjsonnet", setCA(nil)); err == nil {
		t.Error("expected Get error to propagate")
	}
}

func TestUpdateVWCCABundle_NonRetriableUpdateErrorPropagates(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil), updateErr: apierrors.NewForbidden(schema.GroupResource{Resource: "vwc"}, "vjsonnet", errors.New("nope"))}
	if err := UpdateVWCCABundle(context.Background(), f, "vjsonnet", setCA(nil)); err == nil {
		t.Error("expected Forbidden to propagate without infinite retry")
	}
}

func TestUpdateVWCCABundle_EmptyNameRejected(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil)}
	if err := UpdateVWCCABundle(context.Background(), f, "", setCA(nil)); err == nil {
		t.Error("empty name must be rejected")
	}
}

func TestUpdateVWCCABundle_NoWebhooksErrors(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("vjsonnet", 0, nil)}
	if err := UpdateVWCCABundle(context.Background(), f, "vjsonnet", setCA(nil)); err == nil {
		t.Error("a VWC with no webhook entries must error")
	}
}

func TestUpdateVWCCABundle_RefusesEmptyResult(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, []byte("---CA---\n"))}
	err := UpdateVWCCABundle(context.Background(), f, "vjsonnet", func([]byte) []byte { return nil })
	if err == nil {
		t.Error("a mutate producing an empty caBundle must be rejected, not written")
	}
}

func TestUpdateVWCCABundle_RetriesOnConflict(t *testing.T) {
	f := &fakeVWCClient{vwc: makeVWC("vjsonnet", 1, nil), conflicts: 1}
	if err := UpdateVWCCABundle(context.Background(), f, "vjsonnet", setCA(nil)); err != nil {
		t.Fatalf("UpdateVWCCABundle: %v", err)
	}
	if f.updates != 2 {
		t.Errorf("Update called %d times, want 2 (conflict then success)", f.updates)
	}
}

// TestUpdateVWCCABundle_ConvergesUnderConcurrentWriters is the load-bearing
// test for multi-replica self-signed installs: two writers each ensure
// their own CA is present, racing against the same VWC under optimistic
// concurrency. The loser of each Update re-reads and re-applies, so BOTH
// CAs must survive — no lost update. Run under -race.
func TestUpdateVWCCABundle_ConvergesUnderConcurrentWriters(t *testing.T) {
	caA, err := Generate(Input{Namespace: "x"})
	if err != nil {
		t.Fatalf("generate A: %v", err)
	}
	caB, err := Generate(Input{Namespace: "x"})
	if err != nil {
		t.Fatalf("generate B: %v", err)
	}
	f := &fakeVWCClient{vwc: makeVWC("v", 1, nil), occ: true}
	now := time.Now()

	var wg sync.WaitGroup
	for _, ca := range [][]byte{caA.CABundle, caB.CABundle} {
		wg.Add(1)
		go func(ca []byte) {
			defer wg.Done()
			if err := UpdateVWCCABundle(context.Background(), f, "v", func(cur []byte) []byte {
				return mergeCABundle(cur, ca, nil, now)
			}); err != nil {
				t.Errorf("UpdateVWCCABundle: %v", err)
			}
		}(ca)
	}
	wg.Wait()

	final := f.caBundle()
	if !bytes.Contains(final, caA.CABundle) {
		t.Error("writer A's CA was lost — concurrent writers did not converge")
	}
	if !bytes.Contains(final, caB.CABundle) {
		t.Error("writer B's CA was lost — concurrent writers did not converge")
	}
}
