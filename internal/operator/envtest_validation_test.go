/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// These tests pin the CRD's declarative validation (kubebuilder enum /
// boundary / pattern / CEL markers in api/v1/*_types.go) as enforced by a
// real apiserver via envtest. They assert the apiserver REJECTS malformed
// specs with apierrors.IsInvalid and ACCEPTS the valid forms — the schema is
// the contract downstream tenants author against, so a regression in a marker
// must fail here rather than silently letting bad specs through.

// validInlineFiles is the minimal accepted SnippetSource: exactly one of
// files / sourceRef, satisfying the spec-level CEL rule.
func validInlineFiles() jaasv1.SnippetSource {
	return jaasv1.SnippetSource{
		Files: map[string]string{"main.jsonnet": `{ ok: true }`},
	}
}

func TestCRDValidation_SnippetEntryFile(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	longSegment := strings.Repeat("a", 256)

	tests := map[string]struct {
		entryFile  string
		wantReject bool
	}{
		"valid default-shaped name":   {entryFile: "main.jsonnet", wantReject: false},
		"valid nested path":           {entryFile: "dir/sub/main.jsonnet", wantReject: false},
		"valid hidden-prefixed":       {entryFile: ".config.jsonnet", wantReject: false},
		"parent traversal rejected":   {entryFile: "../escape.jsonnet", wantReject: true},
		"embedded traversal rejected": {entryFile: "dir/../../escape.jsonnet", wantReject: true},
		"space rejected by pattern":   {entryFile: "has space.jsonnet", wantReject: true},
		"colon rejected by pattern":   {entryFile: "a:b.jsonnet", wantReject: true},
		"backslash rejected":          {entryFile: `a\b.jsonnet`, wantReject: true},
		// An empty entryFile is omitempty + defaulted to "main.jsonnet"
		// before validation, so the MinLength=1 marker never sees it —
		// the empty value is accepted and defaulted, not rejected.
		"empty defaults, accepted": {entryFile: "", wantReject: false},
		"over-255-len rejected":    {entryFile: longSegment, wantReject: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			snip := &jaasv1.JsonnetSnippet{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "entryfile-", Namespace: ns},
				Spec: jaasv1.JsonnetSnippetSpec{
					EntryFile:     tc.entryFile,
					SnippetSource: validInlineFiles(),
				},
			}
			err := c.Create(context.Background(), snip)
			if tc.wantReject {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("entryFile=%q: got err=%v, want apierrors.IsInvalid", tc.entryFile, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("entryFile=%q: unexpected reject: %v", tc.entryFile, err)
			}
			_ = c.Delete(context.Background(), snip)
		})
	}
}

// spec.tlas and spec.externalVariables share the JsonnetVariable element type,
// so both inherit its per-entry CEL rule and the listMapKey name-uniqueness.
// The table runs against each field to prove neither marker was dropped from
// one of them.
func TestCRDValidation_SnippetVariables(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	tests := map[string]struct {
		vars       []jaasv1.JsonnetVariable
		wantReject bool
	}{
		"string entry accepted": {
			vars: []jaasv1.JsonnetVariable{{Name: "env", Value: "prod"}},
		},
		"code entry accepted": {
			vars: []jaasv1.JsonnetVariable{{Name: "replicas", Value: "3", Code: true}},
		},
		"omitted value accepted for a string entry": {
			vars: []jaasv1.JsonnetVariable{{Name: "blank"}},
		},
		"empty value accepted for a string entry": {
			vars: []jaasv1.JsonnetVariable{{Name: "blank", Value: ""}},
		},
		// The CEL rule only guards emptiness; a non-empty but unparseable
		// value is a snippet-authoring error surfaced at eval time.
		"unparseable code value accepted by the schema": {
			vars: []jaasv1.JsonnetVariable{{Name: "bad", Value: "{ unterminated:", Code: true}},
		},
		"omitted value rejected for a code entry": {
			vars:       []jaasv1.JsonnetVariable{{Name: "n", Code: true}},
			wantReject: true,
		},
		"empty value rejected for a code entry": {
			vars:       []jaasv1.JsonnetVariable{{Name: "n", Value: "", Code: true}},
			wantReject: true,
		},
		"empty name rejected": {
			vars:       []jaasv1.JsonnetVariable{{Name: "", Value: "x"}},
			wantReject: true,
		},
		"over-253-len name rejected": {
			vars:       []jaasv1.JsonnetVariable{{Name: strings.Repeat("a", 254), Value: "x"}},
			wantReject: true,
		},
		"distinct names accepted": {
			vars: []jaasv1.JsonnetVariable{
				{Name: "a", Value: "1"},
				{Name: "b", Value: "2", Code: true},
			},
		},
		// listMapKey=name makes the apiserver itself refuse a duplicate, so
		// the reconciler never has to pick a winner between two bindings of
		// one name.
		"duplicate name rejected": {
			vars: []jaasv1.JsonnetVariable{
				{Name: "dup", Value: "1"},
				{Name: "dup", Value: "2"},
			},
			wantReject: true,
		},
	}

	fields := map[string]func(*jaasv1.JsonnetSnippetSpec, []jaasv1.JsonnetVariable){
		"tlas": func(s *jaasv1.JsonnetSnippetSpec, v []jaasv1.JsonnetVariable) { s.TLAs = v },
		"externalVariables": func(s *jaasv1.JsonnetSnippetSpec, v []jaasv1.JsonnetVariable) {
			s.ExternalVariables = v
		},
	}

	for field, set := range fields {
		for name, tc := range tests {
			t.Run(field+"/"+name, func(t *testing.T) {
				spec := jaasv1.JsonnetSnippetSpec{SnippetSource: validInlineFiles()}
				set(&spec, tc.vars)
				snip := &jaasv1.JsonnetSnippet{
					ObjectMeta: metav1.ObjectMeta{GenerateName: "vars-", Namespace: ns},
					Spec:       spec,
				}
				err := c.Create(context.Background(), snip)
				if tc.wantReject {
					if !apierrors.IsInvalid(err) {
						t.Fatalf("%s=%+v: got err=%v, want apierrors.IsInvalid", field, tc.vars, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s=%+v: unexpected reject: %v", field, tc.vars, err)
				}
				_ = c.Delete(context.Background(), snip)
			})
		}
	}
}

func TestCRDValidation_SnippetOutput(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	tests := map[string]struct {
		output     string
		wantReject bool
	}{
		"rendered accepted":      {output: "rendered", wantReject: false},
		"source accepted":        {output: "source", wantReject: false},
		"bogus rejected":         {output: "bogus", wantReject: true},
		"empty omits to default": {output: "", wantReject: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			snip := &jaasv1.JsonnetSnippet{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "output-", Namespace: ns},
				Spec: jaasv1.JsonnetSnippetSpec{
					Output:        tc.output,
					SnippetSource: validInlineFiles(),
				},
			}
			err := c.Create(context.Background(), snip)
			if tc.wantReject {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("output=%q: got err=%v, want apierrors.IsInvalid", tc.output, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("output=%q: unexpected reject: %v", tc.output, err)
			}
			_ = c.Delete(context.Background(), snip)
		})
	}
}

func TestCRDValidation_SnippetHistory(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	tests := map[string]struct {
		history    int32
		wantReject bool
	}{
		"minimum 1 accepted":  {history: 1, wantReject: false},
		"mid-range accepted":  {history: 25, wantReject: false},
		"maximum 50 accepted": {history: 50, wantReject: false},
		// history=0 is the int32 zero value with omitempty + default=1, so
		// the apiserver treats it as unset and defaults it to 1 before the
		// Minimum=1 marker applies — accepted, not rejected.
		"zero defaults, accepted": {history: 0, wantReject: false},
		"51 above maximum":        {history: 51, wantReject: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			snip := &jaasv1.JsonnetSnippet{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "history-", Namespace: ns},
				Spec: jaasv1.JsonnetSnippetSpec{
					History:       tc.history,
					SnippetSource: validInlineFiles(),
				},
			}
			err := c.Create(context.Background(), snip)
			if tc.wantReject {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("history=%d: got err=%v, want apierrors.IsInvalid", tc.history, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("history=%d: unexpected reject: %v", tc.history, err)
			}
			_ = c.Delete(context.Background(), snip)
		})
	}
}

func TestCRDValidation_SnippetInterval(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	tests := map[string]struct {
		interval   string
		wantReject bool
	}{
		"30s at lower bound accepted": {interval: "30s", wantReject: false},
		"5m accepted":                 {interval: "5m", wantReject: false},
		"24h at upper bound accepted": {interval: "24h", wantReject: false},
		"29s below lower bound":       {interval: "29s", wantReject: true},
		"25h above upper bound":       {interval: "25h", wantReject: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			d, err := time.ParseDuration(tc.interval)
			if err != nil {
				t.Fatalf("parse interval %q: %v", tc.interval, err)
			}
			snip := &jaasv1.JsonnetSnippet{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "interval-", Namespace: ns},
				Spec: jaasv1.JsonnetSnippetSpec{
					Interval:      &metav1.Duration{Duration: d},
					SnippetSource: validInlineFiles(),
				},
			}
			err = c.Create(context.Background(), snip)
			if tc.wantReject {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("interval=%q: got err=%v, want apierrors.IsInvalid", tc.interval, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("interval=%q: unexpected reject: %v", tc.interval, err)
			}
			_ = c.Delete(context.Background(), snip)
		})
	}
}

func TestCRDValidation_SnippetSourceRefKind(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	tests := map[string]struct {
		kind       string
		wantReject bool
	}{
		"GitRepository accepted":    {kind: "GitRepository", wantReject: false},
		"OCIRepository accepted":    {kind: "OCIRepository", wantReject: false},
		"Bucket accepted":           {kind: "Bucket", wantReject: false},
		"ExternalArtifact accepted": {kind: "ExternalArtifact", wantReject: false},
		"BadKind rejected by enum":  {kind: "BadKind", wantReject: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			snip := &jaasv1.JsonnetSnippet{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "srckind-", Namespace: ns},
				Spec: jaasv1.JsonnetSnippetSpec{
					SnippetSource: jaasv1.SnippetSource{
						SourceRef: &jaasv1.SourceRef{Kind: tc.kind, Name: "some-source"},
					},
				},
			}
			err := c.Create(context.Background(), snip)
			if tc.wantReject {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("sourceRef.kind=%q: got err=%v, want apierrors.IsInvalid", tc.kind, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("sourceRef.kind=%q: unexpected reject: %v", tc.kind, err)
			}
			_ = c.Delete(context.Background(), snip)
		})
	}
}

func TestCRDValidation_SnippetSourceRefNameMinLength(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	t.Run("empty name rejected", func(t *testing.T) {
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "srcname-", Namespace: ns},
			Spec: jaasv1.JsonnetSnippetSpec{
				SnippetSource: jaasv1.SnippetSource{
					SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: ""},
				},
			},
		}
		if err := c.Create(context.Background(), snip); !apierrors.IsInvalid(err) {
			t.Fatalf("empty sourceRef.name: got err=%v, want apierrors.IsInvalid", err)
		}
	})

	t.Run("non-empty name accepted", func(t *testing.T) {
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "srcname-", Namespace: ns},
			Spec: jaasv1.JsonnetSnippetSpec{
				SnippetSource: jaasv1.SnippetSource{
					SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "ok"},
				},
			},
		}
		if err := c.Create(context.Background(), snip); err != nil {
			t.Fatalf("non-empty sourceRef.name: unexpected reject: %v", err)
		}
		_ = c.Delete(context.Background(), snip)
	})
}

func TestCRDValidation_SnippetLibraryRefKind(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	tests := map[string]struct {
		kind       string
		wantReject bool
	}{
		"JsonnetLibrary accepted":  {kind: "JsonnetLibrary", wantReject: false},
		"BadKind rejected by enum": {kind: "BadKind", wantReject: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			snip := &jaasv1.JsonnetSnippet{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "libkind-", Namespace: ns},
				Spec: jaasv1.JsonnetSnippetSpec{
					SnippetSource: validInlineFiles(),
					Libraries: []jaasv1.LibraryRef{
						{Kind: tc.kind, Name: "some-lib"},
					},
				},
			}
			err := c.Create(context.Background(), snip)
			if tc.wantReject {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("libraries[].kind=%q: got err=%v, want apierrors.IsInvalid", tc.kind, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("libraries[].kind=%q: unexpected reject: %v", tc.kind, err)
			}
			_ = c.Delete(context.Background(), snip)
		})
	}
}

func TestCRDValidation_SnippetLibraryRefNameMinLength(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	t.Run("empty name rejected", func(t *testing.T) {
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "libname-", Namespace: ns},
			Spec: jaasv1.JsonnetSnippetSpec{
				SnippetSource: validInlineFiles(),
				Libraries: []jaasv1.LibraryRef{
					{Kind: "JsonnetLibrary", Name: ""},
				},
			},
		}
		if err := c.Create(context.Background(), snip); !apierrors.IsInvalid(err) {
			t.Fatalf("empty libraries[].name: got err=%v, want apierrors.IsInvalid", err)
		}
	})

	t.Run("non-empty name accepted", func(t *testing.T) {
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "libname-", Namespace: ns},
			Spec: jaasv1.JsonnetSnippetSpec{
				SnippetSource: validInlineFiles(),
				Libraries: []jaasv1.LibraryRef{
					{Kind: "JsonnetLibrary", Name: "ok"},
				},
			},
		}
		if err := c.Create(context.Background(), snip); err != nil {
			t.Fatalf("non-empty libraries[].name: unexpected reject: %v", err)
		}
		_ = c.Delete(context.Background(), snip)
	})
}

func TestCRDValidation_SnippetSourceExclusivity(t *testing.T) {
	c := envtestClient(t)
	ns := freshNamespace(t, c)

	t.Run("neither files nor sourceRef rejected", func(t *testing.T) {
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "excl-", Namespace: ns},
			Spec:       jaasv1.JsonnetSnippetSpec{},
		}
		if err := c.Create(context.Background(), snip); !apierrors.IsInvalid(err) {
			t.Fatalf("empty source: got err=%v, want apierrors.IsInvalid", err)
		}
	})

	t.Run("both files and sourceRef rejected", func(t *testing.T) {
		snip := &jaasv1.JsonnetSnippet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "excl-", Namespace: ns},
			Spec: jaasv1.JsonnetSnippetSpec{
				SnippetSource: jaasv1.SnippetSource{
					Files:     map[string]string{"main.jsonnet": "{}"},
					SourceRef: &jaasv1.SourceRef{Kind: "GitRepository", Name: "ok"},
				},
			},
		}
		if err := c.Create(context.Background(), snip); !apierrors.IsInvalid(err) {
			t.Fatalf("both sources: got err=%v, want apierrors.IsInvalid", err)
		}
	})
}
