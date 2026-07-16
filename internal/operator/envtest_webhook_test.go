/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// TestEnvtest_Webhook_AdmissionRejectsExtVarConflict spins up a one-off
// envtest with the admission webhook auto-installed, then boots an operator
// manager with the validator. Subtests share that setup to keep the test
// cheap.
func TestEnvtest_Webhook_AdmissionRejectsExtVarConflict(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets not available")
	}

	crdDir, err := resolveCRDDir()
	if err != nil {
		t.Fatalf("resolve CRD dir: %v", err)
	}

	webhookConfig := &admissionregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "jaas-webhook-test"},
		Webhooks: []admissionregv1.ValidatingWebhook{
			{
				Name:                    "vjsonnetsnippet.jaas.metio.wtf",
				FailurePolicy:           new(admissionregv1.Fail),
				SideEffects:             new(admissionregv1.SideEffectClassNone),
				AdmissionReviewVersions: []string{"v1"},
				ClientConfig: admissionregv1.WebhookClientConfig{
					Service: &admissionregv1.ServiceReference{
						Name:      "jaas-webhook",
						Namespace: "default",
						Path:      new("/validate-jaas-metio-wtf-v1-jsonnetsnippet"),
					},
				},
				Rules: []admissionregv1.RuleWithOperations{
					{
						Operations: []admissionregv1.OperationType{
							admissionregv1.Create,
							admissionregv1.Update,
						},
						Rule: admissionregv1.Rule{
							APIGroups:   []string{"jaas.metio.wtf"},
							APIVersions: []string{"v1"},
							Resources:   []string{"jsonnetsnippets"},
							Scope:       new(admissionregv1.NamespacedScope),
						},
					},
				},
			},
		},
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
		CRDs:                  []*apiextv1.CustomResourceDefinition{externalArtifactStubCRD()},
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			ValidatingWebhooks: []*admissionregv1.ValidatingWebhookConfiguration{webhookConfig},
		},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: envtestScheme(t)})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	// Boot a webhook-only manager bound to the host/port/certs envtest
	// allocated for us.
	whOpts := env.WebhookInstallOptions
	if whOpts.LocalServingPort == 0 || whOpts.LocalServingCertDir == "" {
		t.Fatalf("envtest did not allocate webhook serving slot: %+v", whOpts)
	}
	// Sanity-check the cert files exist where envtest claims.
	for _, file := range []string{"tls.crt", "tls.key"} {
		if _, err := os.Stat(filepath.Join(whOpts.LocalServingCertDir, file)); err != nil {
			t.Fatalf("envtest cert %s missing: %v", file, err)
		}
	}
	stopMgr, mgrDone := startValidatorManager(t, cfg, whOpts)
	defer func() {
		stopMgr()
		select {
		case err := <-mgrDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("validator manager exited with %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Errorf("validator manager did not stop within 20s")
		}
	}()

	// Wait until the webhook server is healthy enough for the apiserver to
	// dispatch admission calls. Polling a benign create works.
	waitForWebhookReady(t, c)

	t.Run("conflicting ext-var key is rejected at admission", func(t *testing.T) {
		ns := freshNamespace(t, c)
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{Name: "rejected", Namespace: ns},
			Spec: jaasv1.JsonnetSnippetSpec{
				ServiceAccountName: "tenant",
				SnippetSource: jaasv1.SnippetSource{
					Files: map[string]string{"main.jsonnet": `{}`},
				},
				// Operator-level ExtVars (configured at manager boot below)
				// own "cluster"; the CR setting it must be rejected.
				ExternalVariables: []jaasv1.JsonnetVariable{{Name: "cluster", Value: "dev"}},
			},
		}
		err := c.Create(context.Background(), snip)
		if err == nil {
			t.Fatal("Create succeeded; expected admission denial")
		}
		if !strings.Contains(err.Error(), "cluster") {
			t.Errorf("error %q does not mention the conflicting key", err.Error())
		}
	})

	t.Run("non-conflicting ext-var key is accepted", func(t *testing.T) {
		ns := freshNamespace(t, c)
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{Name: "accepted", Namespace: ns},
			Spec: jaasv1.JsonnetSnippetSpec{
				ServiceAccountName: "tenant",
				SnippetSource: jaasv1.SnippetSource{
					Files: map[string]string{"main.jsonnet": `{}`},
				},
				ExternalVariables: []jaasv1.JsonnetVariable{{Name: "region", Value: "eu-west-1"}},
			},
		}
		if err := c.Create(context.Background(), snip); err != nil {
			t.Fatalf("Create on a clean snippet was rejected: %v", err)
		}
	})

	// Update is the same validator path as Create — `validate(newObj)` —
	// and the unit test in webhook_test.go directly exercises
	// ValidateUpdate's signature with (oldObj, newObj). An envtest Update
	// here tends to lose to RV races against the validator manager's
	// leader-election traffic, so the envtest leg covers only Create.
}

// startValidatorManager boots a controller-runtime manager whose only
// purpose is to serve the validating webhook. No reconciler is wired — the
// envtest tests that need reconcile use the direct-Reconcile helpers in
// envtest_reconcile_test.go.
func startValidatorManager(t *testing.T, restCfg *rest.Config, whOpts envtest.WebhookInstallOptions) (context.CancelFunc, <-chan error) {
	t.Helper()
	opCfg := Config{
		EnableWebhook:                true,
		WebhookCertDir:               whOpts.LocalServingCertDir,
		WebhookPort:                  whOpts.LocalServingPort,
		ExtVars:                      map[string]string{"cluster": "prod"},
		SkipControllerNameValidation: true,
		Logger:                       discardLoggerEnvtest(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithBuilder(ctx, opCfg, restCfg, defaultBuilder)
	}()
	return cancel, done
}

// waitForWebhookReady polls a no-op Get until the webhook responds — the
// apiserver doesn't surface "webhook not yet ready" as a discrete error, so
// we just give it a few seconds with retries.
func waitForWebhookReady(t *testing.T, c client.Client) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		// A benign list to confirm the apiserver is reachable.
		if err := c.List(context.Background(), &jaasv1.JsonnetSnippetList{}); err == nil {
			// Then a probing dryRun create — admission must respond.
			snip := &jaasv1.JsonnetSnippet{
				ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"},
				Spec: jaasv1.JsonnetSnippetSpec{
					ServiceAccountName: "tenant",
					SnippetSource: jaasv1.SnippetSource{
						Files: map[string]string{"main.jsonnet": "{}"},
					},
				},
			}
			err := c.Create(context.Background(), snip, client.DryRunAll)
			if err == nil {
				return
			}
			// "no endpoints available" or "x509" while the manager is
			// still warming up — keep retrying.
			if !strings.Contains(err.Error(), "no endpoints available") &&
				!strings.Contains(err.Error(), "tls") &&
				!strings.Contains(err.Error(), "x509") &&
				!strings.Contains(err.Error(), "connection refused") {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("webhook server did not become ready within 15s")
}
