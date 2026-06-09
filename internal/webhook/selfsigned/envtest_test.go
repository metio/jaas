/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package selfsigned

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestEnvtest_SelfSignedFlow_PatchesVWCEndToEnd boots a real envtest
// apiserver, applies a placeholder VWC, then drives the full
// Generate → UpdateVWCCABundle pipeline through a real clientset.
// Catches the integration gaps a fakeVWCClient hides: optimistic-
// concurrency Update semantics, RBAC if we ever scope it differently, etc.
func TestEnvtest_SelfSignedFlow_PatchesVWCEndToEnd(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets not available — KUBEBUILDER_ASSETS unset")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	vwcs := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations()

	failurePolicy := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "test-selfsigned"},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name:                    "vtest.example.com",
				AdmissionReviewVersions: []string{"v1"},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffects,
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      "jaas-webhook",
						Namespace: "default",
					},
					// CABundle deliberately empty — the patcher fills it.
				},
				Rules: []admissionv1.RuleWithOperations{{
					Operations: []admissionv1.OperationType{admissionv1.Create},
					Rule: admissionv1.Rule{
						APIGroups:   []string{"example.com"},
						APIVersions: []string{"v1"},
						Resources:   []string{"foos"},
					},
				}},
			},
		},
	}
	if _, err := vwcs.Create(context.Background(), vwc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create VWC: %v", err)
	}
	t.Cleanup(func() {
		_ = vwcs.Delete(context.Background(), vwc.Name, metav1.DeleteOptions{})
	})

	bundle, err := Generate(Input{Namespace: "default"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if err := UpdateVWCCABundle(context.Background(), vwcs, vwc.Name, func(cur []byte) []byte {
		return CombineCABundles(cur, bundle.CABundle)
	}); err != nil {
		t.Fatalf("UpdateVWCCABundle: %v", err)
	}

	// Re-fetch and confirm the caBundle landed verbatim.
	got, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get VWC: %v", err)
	}
	if len(got.Webhooks) != 1 {
		t.Fatalf("VWC has %d webhooks, want 1", len(got.Webhooks))
	}
	gotCA := got.Webhooks[0].ClientConfig.CABundle
	if string(gotCA) != string(bundle.CABundle) {
		t.Errorf("caBundle mismatch:\n  got:  %s\n  want: %s", gotCA, bundle.CABundle)
	}

	// And the CA actually parses as an x509 cert so the apiserver
	// would accept it.
	block, _ := pem.Decode(gotCA)
	if block == nil {
		t.Fatal("PEM decode failed")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Errorf("CA bytes do not parse: %v", err)
	}

	// Second call must short-circuit (no diff) — no Update issued, so the
	// object's resourceVersion is unchanged.
	before, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get before idempotent call: %v", err)
	}
	if err := UpdateVWCCABundle(context.Background(), vwcs, vwc.Name, func(cur []byte) []byte {
		return CombineCABundles(cur, bundle.CABundle)
	}); err != nil {
		t.Fatalf("idempotent UpdateVWCCABundle: %v", err)
	}
	after, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after idempotent call: %v", err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("idempotent call issued a write: resourceVersion %s → %s", before.ResourceVersion, after.ResourceVersion)
	}
}

// TestEnvtest_SelfSignedFlow_RotateRePatchesVWC walks a full rotation
// against a real apiserver: bootstrap cert → rotate → confirm caBundle
// changed AND parses cleanly. The renewer's per-rotation contract is
// "produce different bytes" — this test pins that the patcher writes
// those different bytes through to the apiserver, not just to the fake.
func TestEnvtest_SelfSignedFlow_RotateRePatchesVWC(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets not available — KUBEBUILDER_ASSETS unset")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	vwcs := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations()

	failurePolicy := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "test-rotate"},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name:                    "vrotate.example.com",
				AdmissionReviewVersions: []string{"v1"},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffects,
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{Name: "x", Namespace: "default"},
				},
				Rules: []admissionv1.RuleWithOperations{{
					Operations: []admissionv1.OperationType{admissionv1.Create},
					Rule: admissionv1.Rule{
						APIGroups:   []string{""},
						APIVersions: []string{"v1"},
						Resources:   []string{"pods"},
					},
				}},
			},
		},
	}
	if _, err := vwcs.Create(context.Background(), vwc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create VWC: %v", err)
	}
	t.Cleanup(func() {
		_ = vwcs.Delete(context.Background(), vwc.Name, metav1.DeleteOptions{})
	})

	dir := t.TempDir()
	r := &Renewer{
		Input:     Input{Namespace: "default"},
		CertDir:   dir,
		VWCName:   vwc.Name,
		VWCClient: vwcs,
	}

	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("first renewOnce: %v", err)
	}
	first, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after first rotation: %v", err)
	}
	firstCA := append([]byte(nil), first.Webhooks[0].ClientConfig.CABundle...)
	if len(firstCA) == 0 {
		t.Fatal("first rotation did not populate caBundle")
	}

	if err := r.renewOnce(context.Background()); err != nil {
		t.Fatalf("second renewOnce: %v", err)
	}
	second, err := vwcs.Get(context.Background(), vwc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after second rotation: %v", err)
	}
	secondCA := second.Webhooks[0].ClientConfig.CABundle

	if string(firstCA) == string(secondCA) {
		t.Errorf("second rotation produced identical caBundle — renewer is not rolling the CA")
	}
	if block, _ := pem.Decode(secondCA); block == nil {
		t.Fatal("second rotation caBundle is not PEM")
	}
}
