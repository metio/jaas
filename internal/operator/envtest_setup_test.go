/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// sharedEnv is started once per test binary via testMainSetup and torn down by
// testMainTeardown. Individual tests obtain the *rest.Config via envtestConfig.
//
// Setup is lazy and idempotent: any envtest-tagged test that calls
// envtestConfig triggers TestMain logic via sync.Once, which keeps the cost
// off the happy path when envtest tests aren't selected (-run).
var (
	sharedEnv     *envtest.Environment
	sharedRestCfg *rest.Config
	sharedEnvErr  error
	sharedEnvOnce sync.Once
)

// envtestConfig returns the rest.Config of the shared envtest apiserver,
// starting it on first use. It calls t.Skip if KUBEBUILDER_ASSETS is unset
// (no binaries — we still want the package's table tests to pass without
// envtest available). Accepts testing.TB so benchmarks share the same
// setup path as the wider Test* suite.
func envtestConfig(t testing.TB) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets not available (set KUBEBUILDER_ASSETS or run inside the dev shell)")
	}
	sharedEnvOnce.Do(startSharedEnv)
	if sharedEnvErr != nil {
		t.Fatalf("envtest setup failed: %v", sharedEnvErr)
	}
	return sharedRestCfg
}

func startSharedEnv() {
	crdDir, err := resolveCRDDir()
	if err != nil {
		sharedEnvErr = err
		return
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
		CRDs: []*apiextv1.CustomResourceDefinition{
			externalArtifactStubCRD(),
			fluxSourceStubCRD("GitRepository", "gitrepositories", "GitRepositoryList"),
		},
	}
	cfg, err := env.Start()
	if err != nil {
		sharedEnvErr = fmt.Errorf("envtest start: %w", err)
		return
	}
	sharedEnv = env
	sharedRestCfg = cfg
}

// resolveCRDDir walks up from the test's own source file to locate the
// repo-root config/crd/bases/ directory — the canonical controller-gen
// output. envtest points at the bases directly (no chart-style
// templating to strip, no helm.sh/resource-policy annotation drift to
// worry about).
// Doing the lookup via runtime.Caller (rather than os.Getwd) makes it
// robust to test invocations from any cwd.
func resolveCRDDir() (string, error) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("envtest: cannot locate test file via runtime.Caller")
	}
	// here = .../jaas/internal/operator/envtest_setup_test.go
	// repo root = .../jaas
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	crdDir := filepath.Join(repoRoot, "config", "crd", "bases")
	if _, err := os.Stat(crdDir); err != nil {
		return "", fmt.Errorf("envtest: config/crd/bases not found at %q: %w", crdDir, err)
	}
	return crdDir, nil
}

// externalArtifactStubCRD is the minimal ExternalArtifact CRD shape JaaS
// writes to. The real Flux source-controller CRD has richer validation and
// printer columns; we only need enough for the apiserver to accept our spec
// + status writes during reconcile tests.
func externalArtifactStubCRD() *apiextv1.CustomResourceDefinition {
	gv := schema.GroupVersion{Group: "source.toolkit.fluxcd.io", Version: "v1"}
	preserve := true
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "externalartifacts." + gv.Group},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: gv.Group,
			Names: apiextv1.CustomResourceDefinitionNames{
				Kind:     "ExternalArtifact",
				ListKind: "ExternalArtifactList",
				Plural:   "externalartifacts",
				Singular: "externalartifact",
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{
				{
					Name:    gv.Version,
					Served:  true,
					Storage: true,
					Subresources: &apiextv1.CustomResourceSubresources{
						Status: &apiextv1.CustomResourceSubresourceStatus{},
					},
					Schema: &apiextv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextv1.JSONSchemaProps{
								"spec":   {Type: "object", XPreserveUnknownFields: &preserve},
								"status": {Type: "object", XPreserveUnknownFields: &preserve},
							},
						},
					},
				},
			},
		},
	}
}

// fluxSourceStubCRD returns a minimal Flux source CRD shape suitable for
// envtest: enough to install onto the apiserver and accept spec / status
// writes via Unstructured. The real source-controller CRDs carry richer
// validation; we only need the structural skeleton.
func fluxSourceStubCRD(kind, plural, listKind string) *apiextv1.CustomResourceDefinition {
	preserve := true
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".source.toolkit.fluxcd.io"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "source.toolkit.fluxcd.io",
			Names: apiextv1.CustomResourceDefinitionNames{
				Kind:     kind,
				ListKind: listKind,
				Plural:   plural,
				Singular: strings.ToLower(kind),
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Subresources: &apiextv1.CustomResourceSubresources{
						Status: &apiextv1.CustomResourceSubresourceStatus{},
					},
					Schema: &apiextv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextv1.JSONSchemaProps{
								"spec":   {Type: "object", XPreserveUnknownFields: &preserve},
								"status": {Type: "object", XPreserveUnknownFields: &preserve},
							},
						},
					},
				},
			},
		},
	}
}

// teardownSharedEnv is wired up via TestMain so the apiserver+etcd shut down
// cleanly even when individual tests fail.
func teardownSharedEnv() {
	if sharedEnv == nil {
		return
	}
	_ = sharedEnv.Stop()
	sharedEnv = nil
	sharedRestCfg = nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	teardownSharedEnv()
	os.Exit(code)
}
