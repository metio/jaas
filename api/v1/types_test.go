/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package v1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The Output and condition-type constants are part of the CRD's wire
// contract. Programmatic clients pattern-match on these strings; renaming
// them is a breaking change.
func TestOutputAndConditionConstants_StableValues(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"OutputRendered", OutputRendered, "rendered"},
		{"OutputSource", OutputSource, "source"},
		{"ConditionReady", ConditionReady, "Ready"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestGroupVersion_StableValues(t *testing.T) {
	if GroupVersion.Group != "jaas.metio.wtf" {
		t.Errorf("Group: got %q, want %q", GroupVersion.Group, "jaas.metio.wtf")
	}
	if GroupVersion.Version != "v1" {
		t.Errorf("Version: got %q, want %q", GroupVersion.Version, "v1")
	}
}

func TestAddToScheme_RegistersAllKnownKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	wantKinds := []string{
		"JsonnetSnippet", "JsonnetSnippetList",
		"JsonnetLibrary", "JsonnetLibraryList",
	}
	for _, k := range wantKinds {
		gvk := GroupVersion.WithKind(k)
		if _, err := s.New(gvk); err != nil {
			t.Errorf("kind %s not registered: %v", k, err)
		}
	}
}

// --- DeepCopy independence: mutating a deep copy must NOT mutate the source.

func TestSourceRef_DeepCopy_IsIndependent(t *testing.T) {
	src := &SourceRef{
		APIVersion: "source.toolkit.fluxcd.io/v1",
		Kind:       "GitRepository",
		Name:       "team-a-config",
		Namespace:  "team-a",
		Path:       "snippets",
	}
	cp := src.DeepCopy()
	cp.Kind = "OCIRepository"
	if src.Kind != "GitRepository" {
		t.Errorf("source mutated through copy: src.Kind = %q", src.Kind)
	}
}

func TestSourceRef_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var sr *SourceRef
	if got := sr.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestLibraryRef_DeepCopy_IsIndependent(t *testing.T) {
	src := &LibraryRef{Kind: "JsonnetLibrary", Name: "team-a-utils", ImportPath: "utils"}
	cp := src.DeepCopy()
	cp.ImportPath = "other"
	if src.ImportPath != "utils" {
		t.Errorf("source mutated through copy")
	}
}

func TestLibraryRef_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var lr *LibraryRef
	if got := lr.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestSnippetSource_DeepCopy_MapAndPointerAreIndependent(t *testing.T) {
	src := &SnippetSource{
		Files:     map[string]string{"main.jsonnet": "{}"},
		SourceRef: &SourceRef{Kind: "GitRepository", Name: "x"},
	}
	cp := src.DeepCopy()
	cp.Files["main.jsonnet"] = "[]"
	cp.SourceRef.Name = "y"
	if src.Files["main.jsonnet"] != "{}" {
		t.Errorf("Files map shared: src = %v", src.Files)
	}
	if src.SourceRef.Name != "x" {
		t.Errorf("SourceRef pointer shared: src.SourceRef.Name = %q", src.SourceRef.Name)
	}
}

func TestSnippetSource_DeepCopy_NilMapsAndPointersStayNil(t *testing.T) {
	src := &SnippetSource{}
	cp := src.DeepCopy()
	if cp.Files != nil {
		t.Errorf("nil Files map became non-nil: %v", cp.Files)
	}
	if cp.SourceRef != nil {
		t.Errorf("nil SourceRef became non-nil: %v", cp.SourceRef)
	}
}

func TestSnippetSource_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var s *SnippetSource
	if got := s.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestSyncStatus_DeepCopy_ConditionsAndTimeAreIndependent(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	src := &SyncStatus{
		ObservedGeneration: 7,
		Revision:           "sha256:abc",
		LastSyncTime:       &now,
		Conditions: []metav1.Condition{
			{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Synced", Message: "ok"},
		},
	}
	cp := src.DeepCopy()
	cp.Conditions[0].Message = "tampered"
	cp.LastSyncTime = nil
	if src.Conditions[0].Message != "ok" {
		t.Errorf("Conditions slice shared: src message = %q", src.Conditions[0].Message)
	}
	if src.LastSyncTime == nil || !src.LastSyncTime.Equal(&now) {
		t.Errorf("LastSyncTime pointer shared")
	}
}

func TestSyncStatus_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var s *SyncStatus
	if got := s.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestJsonnetSnippetSpec_DeepCopy_AllMapsAndSlicesAreIndependent(t *testing.T) {
	src := &JsonnetSnippetSpec{
		ServiceAccountName: "tenant-a",
		SnippetSource: SnippetSource{
			Files: map[string]string{"main.jsonnet": "{}"},
		},
		Libraries: []LibraryRef{
			{Kind: "JsonnetLibrary", Name: "utils", ImportPath: "u"},
		},
		TLAs: map[string][]string{
			"tags": {"a", "b"},
		},
		ExternalVariables: map[string]string{"env": "prod"},
		Output:            OutputRendered,
	}
	cp := src.DeepCopy()
	cp.Libraries[0].Name = "other"
	cp.TLAs["tags"][0] = "z"
	cp.ExternalVariables["env"] = "dev"
	cp.Files["main.jsonnet"] = "[]"

	if src.Libraries[0].Name != "utils" {
		t.Errorf("Libraries slice shared: src[0].Name = %q", src.Libraries[0].Name)
	}
	if src.TLAs["tags"][0] != "a" {
		t.Errorf("TLAs[tags][0] shared: %q", src.TLAs["tags"][0])
	}
	if src.ExternalVariables["env"] != "prod" {
		t.Errorf("ExternalVariables shared: env = %q", src.ExternalVariables["env"])
	}
	if src.Files["main.jsonnet"] != "{}" {
		t.Errorf("Files map shared")
	}
}

func TestJsonnetSnippetSpec_DeepCopy_NilValueInTLAsMapStaysNil(t *testing.T) {
	src := &JsonnetSnippetSpec{
		TLAs: map[string][]string{
			"missing": nil,
			"present": {"a"},
		},
	}
	cp := src.DeepCopy()
	if cp.TLAs["missing"] != nil {
		t.Errorf("nil value became non-nil in copy: %v", cp.TLAs["missing"])
	}
	if len(cp.TLAs["present"]) != 1 || cp.TLAs["present"][0] != "a" {
		t.Errorf("present value mangled: %v", cp.TLAs["present"])
	}
}

func TestJsonnetSnippetSpec_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var s *JsonnetSnippetSpec
	if got := s.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestJsonnetSnippet_DeepCopy_AndDeepCopyObject_AreIndependent(t *testing.T) {
	src := &JsonnetSnippet{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team-a", Labels: map[string]string{"k": "v"}},
		Spec: JsonnetSnippetSpec{
			SnippetSource: SnippetSource{Files: map[string]string{"main.jsonnet": "{}"}},
			Output:        OutputRendered,
		},
	}

	cp := src.DeepCopy()
	cp.Spec.Files["main.jsonnet"] = "[]"
	cp.ObjectMeta.Labels["k"] = "v2"
	if src.Spec.Files["main.jsonnet"] != "{}" {
		t.Errorf("Files shared through DeepCopy")
	}
	if src.ObjectMeta.Labels["k"] != "v" {
		t.Errorf("Labels shared through DeepCopy")
	}

	// DeepCopyObject path: same independence guarantee.
	obj := src.DeepCopyObject().(*JsonnetSnippet)
	obj.Spec.Files["main.jsonnet"] = "tampered"
	if src.Spec.Files["main.jsonnet"] != "{}" {
		t.Errorf("Files shared through DeepCopyObject")
	}
}

func TestJsonnetSnippet_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var s *JsonnetSnippet
	if got := s.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestJsonnetSnippetList_DeepCopy_IsIndependent(t *testing.T) {
	src := &JsonnetSnippetList{Items: []JsonnetSnippet{
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: JsonnetSnippetSpec{
			SnippetSource: SnippetSource{Files: map[string]string{"main.jsonnet": "{}"}},
		}},
	}}
	cp := src.DeepCopy()
	cp.Items[0].Spec.Files["main.jsonnet"] = "[]"
	if src.Items[0].Spec.Files["main.jsonnet"] != "{}" {
		t.Errorf("List Items shared")
	}

	if obj, ok := src.DeepCopyObject().(*JsonnetSnippetList); !ok || obj == nil {
		t.Errorf("DeepCopyObject returned wrong type or nil")
	}
}

func TestJsonnetSnippetList_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var s *JsonnetSnippetList
	if got := s.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestJsonnetLibrary_DeepCopy_AndDeepCopyObject_AreIndependent(t *testing.T) {
	src := &JsonnetLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "utils", Namespace: "team-a"},
		Spec:       JsonnetLibrarySpec{SnippetSource: SnippetSource{Files: map[string]string{"u.libsonnet": "{}"}}},
	}
	cp := src.DeepCopy()
	cp.Spec.Files["u.libsonnet"] = "tampered"
	if src.Spec.Files["u.libsonnet"] != "{}" {
		t.Errorf("library Files shared")
	}
	if obj, ok := src.DeepCopyObject().(*JsonnetLibrary); !ok || obj == nil {
		t.Errorf("DeepCopyObject returned wrong type or nil")
	}
}

func TestJsonnetLibrary_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var l *JsonnetLibrary
	if got := l.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestJsonnetLibrarySpec_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var s *JsonnetLibrarySpec
	if got := s.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

func TestJsonnetLibrarySpec_DeepCopy_NonNilIsIndependent(t *testing.T) {
	src := &JsonnetLibrarySpec{SnippetSource: SnippetSource{Files: map[string]string{"a.libsonnet": "{}"}}}
	cp := src.DeepCopy()
	cp.Files["a.libsonnet"] = "tampered"
	if src.Files["a.libsonnet"] != "{}" {
		t.Errorf("Files map shared via Spec.DeepCopy()")
	}
}

func TestJsonnetLibraryList_DeepCopy_IsIndependent(t *testing.T) {
	src := &JsonnetLibraryList{Items: []JsonnetLibrary{
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: JsonnetLibrarySpec{SnippetSource: SnippetSource{Files: map[string]string{"a.libsonnet": "{}"}}}},
	}}
	cp := src.DeepCopy()
	cp.Items[0].Spec.Files["a.libsonnet"] = "tampered"
	if src.Items[0].Spec.Files["a.libsonnet"] != "{}" {
		t.Errorf("List Items shared")
	}
	if obj, ok := src.DeepCopyObject().(*JsonnetLibraryList); !ok || obj == nil {
		t.Errorf("DeepCopyObject returned wrong type or nil")
	}
}

func TestJsonnetLibraryList_DeepCopy_NilReceiverReturnsNil(t *testing.T) {
	var l *JsonnetLibraryList
	if got := l.DeepCopy(); got != nil {
		t.Errorf("DeepCopy on nil returned %v, want nil", got)
	}
}

// DeepCopyObject on a nil pointer must return a typed-nil-free nil interface
// so callers can compare against `nil` directly.
func TestDeepCopyObject_NilReceiversReturnNil(t *testing.T) {
	t.Run("JsonnetSnippet", func(t *testing.T) {
		var s *JsonnetSnippet
		if got := s.DeepCopyObject(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("JsonnetSnippetList", func(t *testing.T) {
		var s *JsonnetSnippetList
		if got := s.DeepCopyObject(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("JsonnetLibrary", func(t *testing.T) {
		var s *JsonnetLibrary
		if got := s.DeepCopyObject(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("JsonnetLibraryList", func(t *testing.T) {
		var s *JsonnetLibraryList
		if got := s.DeepCopyObject(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

// A SyncStatus with a non-nil but empty Conditions slice must round-trip
// through DeepCopy without converting nilness either way.
func TestSyncStatus_DeepCopy_EmptyConditionsAndNoTimestamp(t *testing.T) {
	src := &SyncStatus{Conditions: []metav1.Condition{}}
	cp := src.DeepCopy()
	if cp.Conditions == nil {
		t.Errorf("Conditions: nil after copy from non-nil empty")
	}
	if len(cp.Conditions) != 0 {
		t.Errorf("Conditions: got len %d, want 0", len(cp.Conditions))
	}
	if cp.LastSyncTime != nil {
		t.Errorf("LastSyncTime: got %v, want nil", cp.LastSyncTime)
	}
}

// TestConditionAccessors exercises the GetConditions/SetConditions helpers the
// Flux conditions machinery calls on both CR kinds.
func TestConditionAccessors(t *testing.T) {
	conds := []metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Synced"}}

	snip := &JsonnetSnippet{}
	snip.SetConditions(conds)
	if got := snip.GetConditions(); len(got) != 1 || got[0].Type != ConditionReady {
		t.Fatalf("JsonnetSnippet conditions round-trip = %+v", got)
	}
	lib := &JsonnetLibrary{}
	lib.SetConditions(conds)
	if got := lib.GetConditions(); len(got) != 1 || got[0].Reason != "Synced" {
		t.Fatalf("JsonnetLibrary conditions round-trip = %+v", got)
	}
}

// TestSyncStatusDeepCopy exercises SyncStatus (incl. its History []RevisionEntry
// and LastSyncTime pointer) and RevisionEntry deep-copies, and asserts the copy
// is independent of the original.
func TestSyncStatusDeepCopy(t *testing.T) {
	now := metav1.NewTime(time.Now())
	orig := &SyncStatus{
		ObservedGeneration: 3,
		Revision:           "sha256:aaaa",
		LastSyncTime:       &now,
		History: []RevisionEntry{
			{Revision: "sha256:aaaa", Time: now},
			{Revision: "sha256:bbbb", Time: now},
		},
		Conditions: []metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue}},
	}
	cp := orig.DeepCopy()
	if cp.Revision != "sha256:aaaa" || len(cp.History) != 2 || cp.LastSyncTime == nil {
		t.Fatalf("deep copy lost fields: %+v", cp)
	}
	// Mutating the copy must not touch the original (proves the slice/pointer
	// were copied, not aliased).
	cp.History[0].Revision = "sha256:mutated"
	cp.Conditions[0].Reason = "Changed"
	if orig.History[0].Revision != "sha256:aaaa" || orig.Conditions[0].Reason != "" {
		t.Fatal("deep copy aliased the original's History/Conditions")
	}
	// Exercise RevisionEntry's own DeepCopy entry point.
	re := orig.History[1].DeepCopy()
	if re.Revision != "sha256:bbbb" {
		t.Fatalf("RevisionEntry.DeepCopy = %+v", re)
	}
}
