/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"reflect"
	"testing"

	jaasv1 "github.com/metio/jaas/api/v1"
)

func TestSplitVariables(t *testing.T) {
	tests := []struct {
		name     string
		vars     []jaasv1.JsonnetVariable
		wantStr  map[string]string
		wantCode map[string]string
	}{
		{
			name: "nil list yields two nil maps",
		},
		{
			name: "empty list yields two nil maps",
			vars: []jaasv1.JsonnetVariable{},
		},
		{
			name:    "string-only entries leave the code map nil",
			vars:    []jaasv1.JsonnetVariable{{Name: "env", Value: "prod"}},
			wantStr: map[string]string{"env": "prod"},
		},
		{
			name:     "code-only entries leave the string map nil",
			vars:     []jaasv1.JsonnetVariable{{Name: "n", Value: "3", Code: true}},
			wantCode: map[string]string{"n": "3"},
		},
		{
			name: "mixed entries are partitioned by the code flag",
			vars: []jaasv1.JsonnetVariable{
				{Name: "env", Value: "prod"},
				{Name: "tags", Value: `["a"]`, Code: true},
				{Name: "region", Value: "eu-west-1"},
			},
			wantStr:  map[string]string{"env": "prod", "region": "eu-west-1"},
			wantCode: map[string]string{"tags": `["a"]`},
		},
		{
			// An omitted value is the empty string, which is a legal string
			// binding. Admission separately refuses it for a code entry.
			name:    "an omitted value binds the empty string",
			vars:    []jaasv1.JsonnetVariable{{Name: "blank"}},
			wantStr: map[string]string{"blank": ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			str, code := splitVariables(tt.vars)
			if !reflect.DeepEqual(str, tt.wantStr) {
				t.Errorf("str map: got %v, want %v", str, tt.wantStr)
			}
			if !reflect.DeepEqual(code, tt.wantCode) {
				t.Errorf("code map: got %v, want %v", code, tt.wantCode)
			}
		})
	}
}

func TestTLAValues_WrapsEachValueAsSingleElement(t *testing.T) {
	got := tlaValues(map[string]string{"env": "dev", "region": "eu"})
	want := map[string][]string{"env": {"dev"}, "region": {"eu"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A single element is what makes eval bind via vm.TLAVar rather than
// JSON-encoding the slice into a TLACode array — a CR string TLA must never
// arrive at the snippet as ["dev"].
func TestTLAValues_NeverProducesMultiElementValues(t *testing.T) {
	for name, v := range tlaValues(map[string]string{"a": "1", "b": "2"}) {
		if len(v) != 1 {
			t.Errorf("tlaValues[%q] = %v, want exactly one element", name, v)
		}
	}
}

func TestTLAValues_EmptyInputIsNil(t *testing.T) {
	if got := tlaValues(nil); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := tlaValues(map[string]string{}); got != nil {
		t.Errorf("empty input: got %v, want nil", got)
	}
}

func TestMergeExtVars(t *testing.T) {
	tests := []struct {
		name      string
		opLevel   map[string]string
		snipLevel map[string]string
		want      map[string]string
	}{
		{name: "both empty is nil"},
		{
			name:    "operator-only passes through",
			opLevel: map[string]string{"cluster": "prod"},
			want:    map[string]string{"cluster": "prod"},
		},
		{
			name:      "snippet-only passes through",
			snipLevel: map[string]string{"region": "eu"},
			want:      map[string]string{"region": "eu"},
		},
		{
			name:      "disjoint sets union",
			opLevel:   map[string]string{"cluster": "prod"},
			snipLevel: map[string]string{"region": "eu"},
			want:      map[string]string{"cluster": "prod", "region": "eu"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeExtVars(tt.opLevel, tt.snipLevel); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeExtVars_DoesNotMutateInputs(t *testing.T) {
	opLevel := map[string]string{"cluster": "prod"}
	snipLevel := map[string]string{"region": "eu"}
	mergeExtVars(opLevel, snipLevel)
	if len(opLevel) != 1 || opLevel["cluster"] != "prod" {
		t.Errorf("operator map mutated: %v", opLevel)
	}
	if len(snipLevel) != 1 || snipLevel["region"] != "eu" {
		t.Errorf("snippet map mutated: %v", snipLevel)
	}
}
