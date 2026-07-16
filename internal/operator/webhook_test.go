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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	jaasv1 "github.com/metio/jaas/api/v1"
)

func TestSnippetValidator_NoConflictPasses(t *testing.T) {
	v := &SnippetValidator{OperatorExtVars: map[string]string{"cluster": "prod"}}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			ExternalVariables: []jaasv1.JsonnetVariable{{Name: "region", Value: "eu-west-1"}},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

func TestSnippetValidator_ConflictRejected(t *testing.T) {
	v := &SnippetValidator{OperatorExtVars: map[string]string{"cluster": "prod"}}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			ExternalVariables: []jaasv1.JsonnetVariable{{Name: "cluster", Value: "dev"}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), snip)
	if err == nil {
		t.Fatal("conflict not rejected")
	}
	if !strings.Contains(err.Error(), "cluster") {
		t.Errorf("error %q does not mention conflicting key", err.Error())
	}
}

func TestSnippetValidator_MultipleConflictsListedSorted(t *testing.T) {
	v := &SnippetValidator{
		OperatorExtVars: map[string]string{"cluster": "x", "region": "y", "tier": "z"},
	}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			ExternalVariables: []jaasv1.JsonnetVariable{{Name: "tier", Value: "a"}, {Name: "cluster", Value: "b"}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), snip)
	if err == nil {
		t.Fatal("conflicts not rejected")
	}
	msg := err.Error()
	idxCluster := strings.Index(msg, "cluster")
	idxTier := strings.Index(msg, "tier")
	if idxCluster < 0 || idxTier < 0 {
		t.Fatalf("error %q missing one or both keys", msg)
	}
	if idxCluster > idxTier {
		t.Errorf("conflicts not sorted: %q", msg)
	}
}

func TestSnippetValidator_EmptyOperatorExtVarsAlwaysPasses(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			ExternalVariables: []jaasv1.JsonnetVariable{{Name: "cluster", Value: "prod"}},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

func TestSnippetValidator_EmptyCRExternalVariablesAlwaysPasses(t *testing.T) {
	v := &SnippetValidator{OperatorExtVars: map[string]string{"cluster": "prod"}}
	snip := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Errorf("got %v, want nil", err)
	}
}

func TestSnippetValidator_ValidateUpdateChecksNewObj(t *testing.T) {
	v := &SnippetValidator{OperatorExtVars: map[string]string{"cluster": "x"}}
	oldSnip := &jaasv1.JsonnetSnippet{}
	newSnip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			ExternalVariables: []jaasv1.JsonnetVariable{{Name: "cluster", Value: "tampered"}},
		},
	}
	if _, err := v.ValidateUpdate(context.Background(), oldSnip, newSnip); err == nil {
		t.Errorf("update with conflict accepted")
	}
}

// TestSnippetValidator_ValidateUpdateSkipsObjectsUnderDeletion pins that an
// update to a snippet carrying a deletionTimestamp is admitted even when its
// (unchanged) spec would otherwise be rejected — otherwise the reconciler's own
// finalizer-removal update is blocked and the snippet wedges in Terminating.
func TestSnippetValidator_ValidateUpdateSkipsObjectsUnderDeletion(t *testing.T) {
	v := &SnippetValidator{OperatorExtVars: map[string]string{"cluster": "x"}}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{
			DeletionTimestamp: new(metav1.Now()),
			Finalizers:        []string{FinalizerName},
		},
		Spec: jaasv1.JsonnetSnippetSpec{
			ExternalVariables: []jaasv1.JsonnetVariable{{Name: "cluster", Value: "tampered"}}, // would conflict
		},
	}
	if _, err := v.ValidateUpdate(context.Background(), snip, snip); err != nil {
		t.Errorf("update of a deleting snippet rejected: %v", err)
	}
}

func TestSnippetValidator_ValidateDeleteAlwaysPasses(t *testing.T) {
	v := &SnippetValidator{OperatorExtVars: map[string]string{"cluster": "x"}}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			ExternalVariables: []jaasv1.JsonnetVariable{{Name: "cluster", Value: "y"}}, // conflict, but delete ignores
		},
	}
	if _, err := v.ValidateDelete(context.Background(), snip); err != nil {
		t.Errorf("Delete returned %v, want nil", err)
	}
}

// --- Cycle detection at admission ------------------------------------------

func TestSnippetValidator_NilClientSkipsCycleCheck(t *testing.T) {
	// Without a client, the validator can't walk the graph; it should still
	// accept (defer to the reconciler's fallback check).
	v := &SnippetValidator{Client: nil}
	snip := snippetPointingAt("a", "ns", "a", "ns")
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Errorf("got %v, want nil — Client=nil should skip cycle detection", err)
	}
}

func TestSnippetValidator_SelfReferenceRejected(t *testing.T) {
	snip := snippetPointingAt("a", "ns", "a", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(snip).Build()
	v := &SnippetValidator{Client: c}
	_, err := v.ValidateCreate(context.Background(), snip)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("got %v, want cycle rejection", err)
	}
}

func TestSnippetValidator_TwoSnippetCycleRejected(t *testing.T) {
	a := snippetPointingAt("a", "ns", "b", "ns")
	b := snippetPointingAt("b", "ns", "a", "ns")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b).Build()
	v := &SnippetValidator{Client: c}
	_, err := v.ValidateCreate(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("got %v, want cycle rejection", err)
	}
}

func TestSnippetValidator_LinearChainAccepted(t *testing.T) {
	a := snippetPointingAt("a", "ns", "b", "ns")
	b := &jaasv1.JsonnetSnippet{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b).Build()
	v := &SnippetValidator{Client: c}
	if _, err := v.ValidateCreate(context.Background(), a); err != nil {
		t.Errorf("linear chain rejected: %v", err)
	}
}

func TestSnippetValidator_CycleCheckErrorPropagatesToCaller(t *testing.T) {
	want := errors.New("apiserver flaky")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return want
			},
		}).Build()
	v := &SnippetValidator{Client: c}
	snip := snippetPointingAt("a", "ns", "b", "ns")
	if _, err := v.ValidateCreate(context.Background(), snip); !errors.Is(err, want) {
		t.Errorf("got %v, want %v wrapped", err, want)
	}
}

func TestSnippetValidator_ConflictTakesPriorityOverCycle(t *testing.T) {
	// Ext-var conflict short-circuits before cycle detection — that's a
	// fast-path failure, no need to walk the graph.
	v := &SnippetValidator{
		OperatorExtVars: map[string]string{"cluster": "prod"},
		Client:          fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
	}
	snip := snippetPointingAt("a", "ns", "a", "ns")
	snip.Spec.ExternalVariables = []jaasv1.JsonnetVariable{{Name: "cluster", Value: "tampered"}}
	_, err := v.ValidateCreate(context.Background(), snip)
	if err == nil || !strings.Contains(err.Error(), "externalVariables") {
		t.Errorf("got %v, want ext-var conflict (not cycle)", err)
	}
}

// --- KnownLibraryAliases / OCI-shadow rejection ----------------------------

func TestSnippetValidator_LibraryAliasShadow_Rejected(t *testing.T) {
	v := &SnippetValidator{KnownLibraryAliases: []string{"grafonnet"}}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "{}"},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "team-utils", ImportPath: "grafonnet"},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), snip)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "grafonnet") || !strings.Contains(err.Error(), "shadows OCI") {
		t.Errorf("got %v, want a shadow-OCI error mentioning grafonnet", err)
	}
}

func TestSnippetValidator_LibraryAliasShadow_NoKnownAliasesPasses(t *testing.T) {
	v := &SnippetValidator{KnownLibraryAliases: nil}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "{}"},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "anything"},
			},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Errorf("got %v, want pass when KnownLibraryAliases is empty", err)
	}
}

func TestSnippetValidator_LibraryAliasShadow_DistinctNamesPass(t *testing.T) {
	v := &SnippetValidator{KnownLibraryAliases: []string{"grafonnet"}}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "ns"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "{}"},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "team-utils", ImportPath: "team"},
			},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Errorf("got %v, want pass when aliases don't collide", err)
	}
}

// --- Soft admission warnings ------------------------------------------------

func TestSnippetValidator_WarnsWhenEntryFileNotInFiles(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			EntryFile: "dashboards/api.jsonnet",
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "{}"},
			},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), snip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "dashboards/api.jsonnet") {
		t.Errorf("expected one warning mentioning dashboards/api.jsonnet, got %v", warnings)
	}
}

func TestSnippetValidator_WarnsWhenEntryFileDefaultMissing(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"helper.libsonnet": "{}"},
			},
		},
	}
	warnings, _ := v.ValidateCreate(context.Background(), snip)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "main.jsonnet") {
		t.Errorf("expected warning about missing default entry main.jsonnet, got %v", warnings)
	}
}

func TestSnippetValidator_NoEntryFileWarningForSourceRef(t *testing.T) {
	// SourceRef path skips the warning — we can't introspect the
	// upstream tarball at admission time.
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			EntryFile: "missing.jsonnet",
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "src"},
			},
		},
	}
	warnings, _ := v.ValidateCreate(context.Background(), snip)
	for _, w := range warnings {
		if strings.Contains(w, "entryFile") {
			t.Errorf("entryFile warning should not fire on sourceRef snippets, got %q", w)
		}
	}
}

func TestSnippetValidator_RejectsDuplicateLibraryImports(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "{}"},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "shared"},
				{Kind: "JsonnetLibrary", Name: "other", ImportPath: "shared"},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), snip)
	if err == nil {
		t.Fatal("expected duplicate-importPath rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "shared") || !strings.Contains(err.Error(), "import path") {
		t.Errorf("error should name the colliding import path, got %q", err)
	}
}

func TestSnippetValidator_RejectsDuplicateImportPathFallingBackToName(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "{}"},
			},
			// Both default to ImportPath=Name. Collision via the
			// metadata-name path the reconciler also uses.
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "duplicated"},
				{Kind: "JsonnetLibrary", Name: "duplicated"},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), snip)
	if err == nil {
		t.Fatal("expected duplicate-importPath rejection when ImportPath is empty, got nil error")
	}
	if !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("error should name the colliding import path, got %q", err)
	}
}

func TestSnippetValidator_DistinctImportPathsPass(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "{}"},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "shared"},
				{Kind: "JsonnetLibrary", Name: "other", ImportPath: "extra"},
			},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Errorf("distinct import paths must pass admission, got %q", err)
	}
}

func TestSnippetValidator_WarnsOnLikelySelfReference(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				SourceRef: &jaasv1.SourceRef{
					Kind: "ExternalArtifact",
					Name: "demo", // same name as the snippet itself
				},
			},
		},
	}
	warnings, _ := v.ValidateCreate(context.Background(), snip)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "own published artifact") && strings.Contains(w, "cycle") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected self-reference warning, got %v", warnings)
	}
}

func TestSnippetValidator_NoWarningsOnHealthyShape(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{
				Files: map[string]string{"main.jsonnet": "{}"},
			},
			Libraries: []jaasv1.LibraryRef{
				{Kind: "JsonnetLibrary", Name: "utils"},
				{Kind: "JsonnetLibrary", Name: "extra", ImportPath: "other"},
			},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), snip)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings on healthy shape, got %v", warnings)
	}
}

// Output=source ships file NAMES to consumers, whose extractors silently drop
// unsafe ones — admission rejects such keys so the author hears it on apply.
func TestSnippetValidator_SourceModeUnsafeFileNameRejected(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			Output: jaasv1.OutputSource,
			SnippetSource: jaasv1.SnippetSource{Files: map[string]string{
				"main.jsonnet":      "{}",
				"deploy config.yml": "{}", // space: every consumer drops it
			}},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), snip); err == nil {
		t.Fatal("a source-mode file name consumers drop must be rejected at admission")
	} else if !strings.Contains(err.Error(), "deploy config.yml") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// Rendered mode publishes only rendered.json — the same key stays legal there,
// where it is purely a local eval file name.
func TestSnippetValidator_RenderedModeFileNamesUnrestricted(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			SnippetSource: jaasv1.SnippetSource{Files: map[string]string{
				"main.jsonnet":      "{}",
				"deploy config.yml": "{}",
			}},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Fatalf("rendered mode must not restrict local file names: %v", err)
	}
}

// Safe source-mode names pass admission.
func TestSnippetValidator_SourceModeSafeFileNamesPass(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{
		Spec: jaasv1.JsonnetSnippetSpec{
			Output: jaasv1.OutputSource,
			SnippetSource: jaasv1.SnippetSource{Files: map[string]string{
				"main.jsonnet":          "{}",
				"lib/helpers.libsonnet": "{}",
			}},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Fatalf("consumer-safe source names must pass admission: %v", err)
	}
}

// A slash-bearing library import alias makes the importer's foundAt encoding
// non-injective across libraries (alias "vendor" file "lib/x" collides with
// alias "vendor/lib" file "x"), so it must be rejected at admission — both the
// explicit ImportPath and the Name fallback.
func TestSnippetValidator_SlashLibraryAliasRejected(t *testing.T) {
	cases := map[string]jaasv1.LibraryRef{
		"import-path slash": {Kind: "JsonnetLibrary", Name: "utils", ImportPath: "vendor/lib"},
		"name slash":        {Kind: "JsonnetLibrary", Name: "vendor/lib"},
		"traversal":         {Kind: "JsonnetLibrary", Name: "utils", ImportPath: ".."},
		"backslash":         {Kind: "JsonnetLibrary", Name: "utils", ImportPath: `a\b`},
	}
	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			v := &SnippetValidator{}
			snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{Libraries: []jaasv1.LibraryRef{ref}}}
			_, err := v.ValidateCreate(context.Background(), snip)
			if err == nil {
				t.Fatal("slash/traversal alias not rejected")
			}
			if !strings.Contains(err.Error(), "single path segment") {
				t.Errorf("error %q should explain the single-segment rule", err.Error())
			}
		})
	}
}

// A single-segment alias with dots/dashes/underscores stays valid — the guard
// must not overshoot the alias charset.
func TestSnippetValidator_SingleSegmentLibraryAliasPasses(t *testing.T) {
	v := &SnippetValidator{}
	snip := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{Libraries: []jaasv1.LibraryRef{
		{Kind: "JsonnetLibrary", Name: "graf-onnet_v2.0"},
	}}}
	if _, err := v.ValidateCreate(context.Background(), snip); err != nil {
		t.Fatalf("a single-segment alias must pass: %v", err)
	}
}

// An update that changes only fields the webhook does not validate (spec.suspend
// here) must be admitted even when the snippet already violates an invariant an
// EXTERNAL change introduced (an operator restart adding a colliding --ext-var,
// a library edited into a cycle). Suspend is the documented remediation for a
// wedged snippet, so re-validating an unchanged violation on the suspend update
// would deny the very fix.
func TestSnippetValidator_ValidateUpdate_SuspendTogglePassesDespiteStaleViolation(t *testing.T) {
	v := &SnippetValidator{OperatorExtVars: map[string]string{"cluster": "prod"}}
	spec := jaasv1.JsonnetSnippetSpec{
		ExternalVariables: []jaasv1.JsonnetVariable{{Name: "cluster", Value: "dev"}}, // collides with the operator ext-var
	}
	old := &jaasv1.JsonnetSnippet{Spec: spec}
	updated := old.DeepCopy()
	updated.Spec.Suspend = true // the only change

	if _, err := v.ValidateUpdate(context.Background(), old, updated); err != nil {
		t.Fatalf("suspend toggle on an externally-invalidated snippet must be admitted: %v", err)
	}
}

// The escape hatch is narrow: an update that DOES change a validated input is
// still fully re-checked, so a suspend toggle cannot be used to smuggle a new
// conflicting ext-var past admission.
func TestSnippetValidator_ValidateUpdate_ChangingInputsStillValidated(t *testing.T) {
	v := &SnippetValidator{OperatorExtVars: map[string]string{"cluster": "prod"}}
	old := &jaasv1.JsonnetSnippet{}
	updated := &jaasv1.JsonnetSnippet{Spec: jaasv1.JsonnetSnippetSpec{
		Suspend:           true,
		ExternalVariables: []jaasv1.JsonnetVariable{{Name: "cluster", Value: "dev"}}, // newly introduced conflict
	}}
	if _, err := v.ValidateUpdate(context.Background(), old, updated); err == nil {
		t.Fatal("an update that introduces a conflicting ext-var must still be rejected")
	}
}
