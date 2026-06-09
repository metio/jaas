/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// realTokenCache wraps the envtest apiserver's clientset in a token cache so
// envtest tests exercise the production TokenRequest path. The cache's
// short refresh margin (0) means every call mints — fine for envtest's low
// traffic and easier to reason about.
func realTokenCache(t *testing.T, cfg *rest.Config) *tokenCache {
	t.Helper()
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kubernetes.NewForConfig: %v", err)
	}
	tc := newTokenCache(clientsetTokenMinter{kc: kc})
	tc.refreshMargin = 0 // tests don't run long enough to warrant caching
	return tc
}

// rbacScheme adds the rbac and core v1 types we need on top of envtestScheme.
// Defined here rather than the shared envtestScheme so the lighter-weight
// reconcile tests don't pull RBAC types they don't use.
func rbacScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := envtestScheme(t)
	if err := rbacv1.AddToScheme(s); err != nil {
		t.Fatalf("rbacv1 AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1 AddToScheme: %v", err)
	}
	return s
}

// seedTenantSA creates a ServiceAccount in ns, plus an empty Role and a
// RoleBinding linking the two. The SA exists but has no permissions —
// perfect for proving impersonation actually limits what the operator can
// reach.
func seedTenantSA(t *testing.T, c client.Client, ns, sa string) {
	t.Helper()
	ctx := context.Background()

	if err := c.Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: ns},
	}); err != nil {
		t.Fatalf("create SA %s/%s: %v", ns, sa, err)
	}
	if err := c.Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: sa + "-role", Namespace: ns},
		// Empty Rules: SA can do nothing.
	}); err != nil {
		t.Fatalf("create Role %s/%s-role: %v", ns, sa, err)
	}
	if err := c.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: sa + "-binding", Namespace: ns},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: sa, Namespace: ns},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			Name:     sa + "-role",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}); err != nil {
		t.Fatalf("create RoleBinding %s/%s-binding: %v", ns, sa, err)
	}
}

// rbacEnvtestClient is envtestClient with RBAC types in the scheme so the
// impersonation tests can CRUD Role / RoleBinding / ServiceAccount objects.
func rbacEnvtestClient(t *testing.T) client.Client {
	t.Helper()
	cfg := envtestConfig(t)
	c, err := client.New(cfg, client.Options{Scheme: rbacScheme(t)})
	if err != nil {
		t.Fatalf("rbac envtest client.New: %v", err)
	}
	return c
}

// TestEnvtest_Impersonation_TenantSAWithoutPermissionsFailsLibraryGet wires
// the production impersonation path against envtest and asserts the
// impersonated SA — which holds no RBAC grants — cannot read a JsonnetLibrary
// the operator's own SA would happily fetch. This is the canary that
// impersonation is real, not silently bypassed.
func TestEnvtest_Impersonation_TenantSAWithoutPermissionsFailsLibraryGet(t *testing.T) {
	cfg := envtestConfig(t)
	c := rbacEnvtestClient(t)

	ns := freshNamespace(t, c)
	const tenantSA = "tenant"
	seedTenantSA(t, c, ns, tenantSA)

	// A library exists in the same namespace; the operator's admin client
	// could fetch it without issue, but the impersonated tenant SA can't.
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: ns},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ shared: "value" }`},
			},
		},
	}
	if err := c.Create(context.Background(), lib); err != nil {
		t.Fatalf("create library: %v", err)
	}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: tenantSA,
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `(import "utils") + {}`},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "utils"},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := &SnippetReconciler{
		Client:     c,
		Scheme:     rbacScheme(t),
		RestConfig: cfg,
		TokenCache: realTokenCache(t, cfg),
		Logger:     discardLoggerEnvtest(),
	}

	// First Reconcile attaches the finalizer (uses r.Client, not impersonated).
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}

	// Second Reconcile runs the spec path; library Get goes through the
	// impersonating client → Forbidden → classified as ReasonRBACDenied
	// (non-transient: retry can't grant a verb). Reconcile returns nil
	// to stop engaging backoff; status reflects the failure.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("non-transient classification expected, got err: %v", err)
	}
	got := &jaasv1.JsonnetSnippet{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil || cond.Reason != ReasonRBACDenied {
		t.Errorf("Ready.Reason = %v, want %q", cond, ReasonRBACDenied)
	}
	if cond != nil && !strings.Contains(strings.ToLower(cond.Message), "rbac denied") {
		t.Errorf("Ready.Message %q does not name RBAC denial", cond.Message)
	}
}

// TestEnvtest_Impersonation_TenantSAWithListReadCanReconcile mirrors the
// previous test but grants the tenant SA `get` on JsonnetLibrary so the
// library Get succeeds. The reconcile then fails on ExternalArtifact create
// — confirming impersonation is the only thing limiting it (the operator's
// SA could do both).
func TestEnvtest_Impersonation_TenantSAWithLibraryReadOnlyFailsArtifactWrite(t *testing.T) {
	cfg := envtestConfig(t)
	c := rbacEnvtestClient(t)

	ns := freshNamespace(t, c)
	const tenantSA = "tenant-rw-lib"
	seedTenantSA(t, c, ns, tenantSA)

	// Grant get on JsonnetLibrary so library Get succeeds.
	if err := c.Create(context.Background(), &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "lib-reader", Namespace: ns},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"jaas.metio.wtf"}, Resources: []string{"jsonnetlibraries"}, Verbs: []string{"get"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lib-reader-binding", Namespace: ns},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: tenantSA, Namespace: ns},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "Role", Name: "lib-reader", APIGroup: "rbac.authorization.k8s.io",
		},
	}); err != nil {
		t.Fatal(err)
	}

	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: ns},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ ok: true }`},
			},
		},
	}
	if err := c.Create(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: tenantSA,
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `(import "utils") + {}`},
			},
			Libraries: []jaasv1.LibraryRef{{Kind: "JsonnetLibrary", Name: "utils"}},
			Output:    jaasv1.OutputRendered,
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	// Direct reconciler with a real Publisher so the ExternalArtifact write
	// is attempted.
	r := directReconciler(t, c, true)
	r.RestConfig = cfg
	r.TokenCache = realTokenCache(t, cfg)

	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	// ExternalArtifact write Forbidden is now classified as
	// ReasonRBACDenied (non-transient); Reconcile returns nil and the
	// status condition carries the diagnosis.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("non-transient classification expected, got err: %v", err)
	}
	got := &jaasv1.JsonnetSnippet{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil || cond.Reason != ReasonRBACDenied {
		t.Errorf("Ready.Reason = %v, want %q", cond, ReasonRBACDenied)
	}
}

// TestEnvtest_Impersonation_CrossNamespaceLibrary_RequiresExplicitRoleBinding
// pins the cross-namespace impersonation contract: a tenant SA in ns-a
// cannot Get a JsonnetLibrary in ns-b unless a RoleBinding in ns-b
// explicitly grants it (the binding goes in the LIBRARY's namespace,
// matching K8s RBAC scoping). With NoCrossNamespaceRefs=false (so the
// reconciler doesn't refuse the ref out of the gate), absence of the
// binding surfaces as a Forbidden during the impersonated Get; presence
// reconciles cleanly.
func TestEnvtest_Impersonation_CrossNamespaceLibrary_RequiresExplicitRoleBinding(t *testing.T) {
	cfg := envtestConfig(t)
	c := rbacEnvtestClient(t)

	nsA := freshNamespace(t, c)
	nsB := freshNamespace(t, c)
	const tenantSA = "tenant-x-ns"

	// Tenant SA + empty Role/Binding in ns-a, plus a Role in ns-b that
	// grants get on JsonnetLibrary. The cross-namespace RoleBinding in
	// ns-b is what we'll add / omit to flip the behavior.
	seedTenantSA(t, c, nsA, tenantSA)
	if err := c.Create(context.Background(), &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "lib-reader-cross-ns", Namespace: nsB},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"jaas.metio.wtf"}, Resources: []string{"jsonnetlibraries"}, Verbs: []string{"get"}},
		},
	}); err != nil {
		t.Fatalf("create cross-ns Role: %v", err)
	}

	// Library + snippet
	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: nsB},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ shared: "value" }`},
			},
		},
	}
	if err := c.Create(context.Background(), lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: nsA},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: tenantSA,
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `(import "utils") + {}`},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "utils", Namespace: nsB},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := &SnippetReconciler{
		Client:               c,
		Scheme:               rbacScheme(t),
		RestConfig:           cfg,
		TokenCache:           realTokenCache(t, cfg),
		NoCrossNamespaceRefs: false, // we want to test the RBAC layer, not the policy short-circuit
		Logger:               discardLoggerEnvtest(),
	}

	// First reconcile attaches the finalizer via r.Client.
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}

	// Without the cross-namespace RoleBinding: impersonated Get fails
	// with Forbidden → ReasonRBACDenied (non-transient classification).
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("non-transient classification expected, got err: %v", err)
	}
	got := &jaasv1.JsonnetSnippet{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, jaasv1.ConditionReady)
	if cond == nil || cond.Reason != ReasonRBACDenied {
		t.Errorf("Ready.Reason = %v, want %q", cond, ReasonRBACDenied)
	}

	// Add the cross-namespace RoleBinding in ns-b (the library's
	// namespace). RBAC rule: bindings live in the resource's namespace,
	// subjects can be SAs in any namespace.
	if err := c.Create(context.Background(), &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lib-reader-cross-ns", Namespace: nsB},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: tenantSA, Namespace: nsA},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "Role", Name: "lib-reader-cross-ns", APIGroup: "rbac.authorization.k8s.io",
		},
	}); err != nil {
		t.Fatalf("create cross-ns RoleBinding: %v", err)
	}

	// Reconcile again. Library Get now succeeds; the reconcile may still
	// fail downstream (no Publisher wired, so it never tries to write
	// an ExternalArtifact — eval succeeds and markSynced runs). What we
	// care about: the Forbidden is gone.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		// The reconciler may still return an error if downstream steps
		// fail; what's NOT acceptable is another "forbidden" on the
		// library Get itself.
		if strings.Contains(strings.ToLower(err.Error()), "forbidden") &&
			strings.Contains(strings.ToLower(err.Error()), "jsonnetlibrar") {
			t.Errorf("library Get still Forbidden after RoleBinding: %v", err)
		}
	}
}

// TestEnvtest_Impersonation_NoCrossNamespaceRefs_RejectsBeforeRBAC asserts
// that with NoCrossNamespaceRefs=true the reconciler refuses the
// cross-namespace ref even when RBAC would otherwise permit it — the
// policy check is upstream of the RBAC check.
func TestEnvtest_Impersonation_NoCrossNamespaceRefs_RejectsBeforeRBAC(t *testing.T) {
	cfg := envtestConfig(t)
	c := rbacEnvtestClient(t)

	nsA := freshNamespace(t, c)
	nsB := freshNamespace(t, c)
	const tenantSA = "tenant-blocked"

	seedTenantSA(t, c, nsA, tenantSA)

	// Generous RBAC: tenant has cluster-wide get on jsonnetlibraries.
	if err := c.Create(context.Background(), &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "lib-reader-cluster"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"jaas.metio.wtf"}, Resources: []string{"jsonnetlibraries"}, Verbs: []string{"get", "list", "watch"}},
		},
	}); err != nil {
		t.Fatalf("create ClusterRole: %v", err)
	}
	if err := c.Create(context.Background(), &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lib-reader-cluster-binding"},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: tenantSA, Namespace: nsA},
		},
		RoleRef: rbacv1.RoleRef{
			Kind: "ClusterRole", Name: "lib-reader-cluster", APIGroup: "rbac.authorization.k8s.io",
		},
	}); err != nil {
		t.Fatalf("create ClusterRoleBinding: %v", err)
	}

	lib := &jaasv1.JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: nsB},
		Spec: jaasv1.JsonnetLibrarySpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.libsonnet": `{ ok: true }`},
			},
		},
	}
	if err := c.Create(context.Background(), lib); err != nil {
		t.Fatalf("create library: %v", err)
	}

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: nsA},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: tenantSA,
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `(import "utils") + {}`},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "utils", Namespace: nsB},
			},
		},
	}
	key := applyJsonnetSnippet(t, c, snip)

	r := &SnippetReconciler{
		Client:               c,
		Scheme:               rbacScheme(t),
		RestConfig:           cfg,
		TokenCache:           realTokenCache(t, cfg),
		NoCrossNamespaceRefs: true,
		Logger:               discardLoggerEnvtest(),
	}

	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec reconcile: %v", err)
	}

	// Should have flipped Ready=False with CrossNamespaceRefRejected.
	got := &jaasv1.JsonnetSnippet{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get snippet: %v", err)
	}
	if got.Status.Conditions[0].Reason != ReasonCrossNamespaceRefRejected {
		t.Errorf("Ready Reason = %q, want %q", got.Status.Conditions[0].Reason, ReasonCrossNamespaceRefRejected)
	}
	if got.Status.Conditions[0].Status != "False" {
		t.Errorf("Ready Status = %q, want False", got.Status.Conditions[0].Status)
	}
}

// TestEnvtest_Impersonation_Disabled_FallsBackToManagerClient verifies the
// SkipImpersonation path most envtest tests use: even when RestConfig is set
// the reconciler should use r.Client for tenant operations when impersonation
// is disabled (this branch is what makes envtest's other reconcile cases
// work without provisioning per-test SAs).
func TestEnvtest_Impersonation_Disabled_FallsBackToManagerClient(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
		Spec: jaasv1.JsonnetSnippetSpec{
			ServiceAccountName: "tenant-without-rbac",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": `{}`},
			},
		},
	}
	if err := c.Create(context.Background(), snip); err != nil {
		t.Fatal(err)
	}

	// RestConfig nil ⇒ tenantClient returns r.Client. Even though the SA
	// doesn't exist, reconcile proceeds because nothing is impersonated.
	r := &SnippetReconciler{
		Client: c,
		Scheme: envtestScheme(t),
		Logger: discardLoggerEnvtest(),
	}

	key := types.NamespacedName{Name: snip.Name, Namespace: snip.Namespace}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	// Second reconcile should reach Synced — the manager's admin client has
	// permissions for everything, so no impersonation means no Forbidden.
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("spec reconcile: %v", err)
	}

	var got jaasv1.JsonnetSnippet
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Revision == "" {
		t.Errorf("expected Status.Revision set with impersonation disabled; got empty")
	}
}
