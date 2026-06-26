/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"strings"
	"testing"
)

// FuzzParseExtVars exercises the environment-variable parser. The
// parser is the sole entry point from process env → eval VM ExtVars
// — anything that slips through here lands in user-controlled
// jsonnet evaluation.
//
// Invariants:
//
//  1. Total: never panics.
//  2. Determinism: same input → same output.
//  3. Spec parity: the implementation matches the in-test
//     reconstruction. The parser strips the prefix exactly once —
//     so `JAAS_EXT_VAR_JAAS_EXT_VAR_X=v` legitimately produces
//     key `JAAS_EXT_VAR_X`. The reconstruction encodes the same
//     single-strip semantics.
//
// The fuzzer takes three strings as the environment to keep input
// shapes diverse — repeated keys, mismatched lines, malformed shapes
// all surface through different combinations.
//
// Run as: go test -fuzz=FuzzParseExtVars ./internal/handler/
func FuzzParseExtVars(f *testing.F) {
	seeds := []struct{ a, b, c string }{
		// Single happy-path entry.
		{"JAAS_EXT_VAR_REGION=eu-west-1", "", ""},
		// Multiple entries.
		{"JAAS_EXT_VAR_X=1", "JAAS_EXT_VAR_Y=2", "JAAS_EXT_VAR_Z=3"},
		// Last-write-wins on duplicate key.
		{"JAAS_EXT_VAR_DUP=first", "JAAS_EXT_VAR_DUP=second", ""},
		// Unrelated entries are skipped.
		{"PATH=/usr/bin", "HOME=/root", "JAAS_EXT_VAR_X=keep"},
		// Empty value is valid; empty key after prefix is also valid.
		{"JAAS_EXT_VAR_X=", "JAAS_EXT_VAR_=empty-key", ""},
		// Malformed entries are skipped.
		{"NO_EQUALS_HERE", "=NO_KEY", "JAAS_EXT_VAR_NOEQ"},
		// Adversarial: prefix-only or near-prefix.
		{"JAAS_EXT_VAR=not-quite-prefix", "jaas_ext_var_LOWERCASE=skip", "JAAS_EXT_VAR_OK=ok"},
		// Embedded special chars in value.
		{"JAAS_EXT_VAR_JSON={\"k\":\"v\"}", "JAAS_EXT_VAR_NEWLINE=line1\nline2", ""},
		// Embedded = in value (everything after first = is value).
		{"JAAS_EXT_VAR_EQ=a=b=c", "", ""},
	}
	for _, s := range seeds {
		f.Add(s.a, s.b, s.c)
	}

	f.Fuzz(func(t *testing.T, a, b, c string) {
		environ := []string{a, b, c}
		got := ParseExtVars(environ)

		// Determinism.
		if again := ParseExtVars(environ); !mapsEqual(got, again) {
			t.Errorf("ParseExtVars not deterministic for %v", environ)
		}

		// Reconstruct the expected map manually and compare. Doing
		// this in the test body rather than re-using the production
		// implementation makes the fuzz target a genuine
		// specification — drift between this expected-derivation
		// and ParseExtVars surfaces as a failure.
		want := map[string]string{}
		for _, env := range environ {
			before, after, ok := strings.Cut(env, "=")
			if !ok {
				continue
			}
			key := before
			value := after
			if !strings.HasPrefix(key, extVarPrefix) {
				continue
			}
			want[strings.TrimPrefix(key, extVarPrefix)] = value
		}
		if !mapsEqual(got, want) {
			t.Errorf("ParseExtVars(%v) = %v, want %v", environ, got, want)
		}
	})
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
