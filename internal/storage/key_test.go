/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import "testing"

func TestRevisionFilename(t *testing.T) {
	tests := []struct {
		rev  string
		want string
	}{
		{"sha256:abc123", "sha256-abc123.tar.gz"}, // colon → dash, algo preserved
		{"sha512:deadbeef", "sha512-deadbeef.tar.gz"},
		{"abc123", "abc123.tar.gz"}, // bare token unchanged
	}
	for _, tc := range tests {
		if got := RevisionFilename(tc.rev); got != tc.want {
			t.Errorf("RevisionFilename(%q) = %q, want %q", tc.rev, got, tc.want)
		}
	}
}

func TestValidDigest(t *testing.T) {
	valid := []string{"sha256:abc", "sha512:DEADbeef00", "md5:0a1b"}
	for _, d := range valid {
		if err := ValidDigest(d); err != nil {
			t.Errorf("ValidDigest(%q) = %v, want nil", d, err)
		}
	}
	invalid := []string{
		"",                 // empty
		"abc",              // no algo separator
		"sha256:",          // empty hex
		":abc",             // empty algo
		"sha256:xyz",       // non-hex
		"sha256:ab/cd",     // path separator
		"sha256:..",        // traversal
		"SHA256:abc",       // uppercase algo
		"sha256:abc:extra", // the hex part "abc:extra" is non-hex
	}
	for _, d := range invalid {
		if err := ValidDigest(d); err == nil {
			t.Errorf("ValidDigest(%q) = nil, want error", d)
		}
	}
}
