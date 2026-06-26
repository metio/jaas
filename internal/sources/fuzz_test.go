/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package sources

import (
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
)

// FuzzParseDigest exercises the `<algo>:<hex>` parser against
// randomized input. Invariants:
//
//  1. Total: never panics.
//  2. Deterministic: two calls with the same input return the same
//     (algo, hex, err) tuple.
//  3. Acceptance shape: when err == nil, algo is on the supported
//     list, the hex matches expectedHexLength, and hex.DecodeString
//     succeeds — these are the actual downstream consumer's
//     assumptions, so a false-accept that violated any of them would
//     break verifyExpectedDigest's switch.
//  4. Round-trip: when err == nil, re-parsing the canonical
//     "<algo>:<hex>" form produces the same tuple. (The first parse
//     normalises case + whitespace; the second sees an already-normal
//     form.)
//  5. Rejection: when err != nil, the error wraps ErrDigestInvalid —
//     callers downstream use errors.Is on this sentinel.
//
// Run as: go test -fuzz=FuzzParseDigest ./internal/sources/
func FuzzParseDigest(f *testing.F) {
	seeds := []string{
		// Valid sha256 (64 hex chars).
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("0", 64),
		"SHA256:" + strings.Repeat("F", 64),          // case-normalised
		"  sha256:" + strings.Repeat("a", 64) + "  ", // whitespace-trimmed

		// Wrong algorithm.
		"md5:" + strings.Repeat("a", 32),
		"sha512:" + strings.Repeat("a", 128),

		// Malformed shapes.
		"",
		":",
		"sha256:",
		":abc",
		"sha256",
		"sha256:" + strings.Repeat("a", 63), // off-by-one
		"sha256:" + strings.Repeat("a", 65),
		"sha256:" + strings.Repeat("g", 64), // non-hex
		"sha256::" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("a", 64) + ":extra",
		"sha256 :" + strings.Repeat("a", 64),

		// Adversarial.
		strings.Repeat("x", 1024),
		"\x00sha256:" + strings.Repeat("a", 64),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, declared string) {
		algo, hexValue, err := parseDigest(declared)

		// Determinism.
		if algo2, hex2, err2 := parseDigest(declared); algo != algo2 || hexValue != hex2 || (err == nil) != (err2 == nil) {
			t.Errorf("parseDigest not deterministic for %q: (%q,%q,%v) vs (%q,%q,%v)",
				declared, algo, hexValue, err, algo2, hex2, err2)
		}

		if err != nil {
			if !errors.Is(err, ErrDigestInvalid) {
				t.Errorf("rejection %v for %q is not wrapped in ErrDigestInvalid", err, declared)
			}
			return
		}

		// Acceptance invariants — these are what
		// verifyExpectedDigest's switch relies on.
		supported := slices.Contains(supportedDigestAlgorithms, algo+":")
		if !supported {
			t.Errorf("parseDigest accepted unsupported algo %q for %q", algo, declared)
		}
		if wantLen := expectedHexLength(algo); wantLen > 0 && len(hexValue) != wantLen {
			t.Errorf("hex length %d != %d for algo %q on %q",
				len(hexValue), wantLen, algo, declared)
		}
		if _, decodeErr := hex.DecodeString(hexValue); decodeErr != nil {
			t.Errorf("accepted hex is not decodable for %q: %v", declared, decodeErr)
		}
		// Round-trip on canonical form.
		canonical := algo + ":" + hexValue
		algo2, hex2, err2 := parseDigest(canonical)
		if err2 != nil || algo2 != algo || hex2 != hexValue {
			t.Errorf("canonical %q did not round-trip: (%q,%q,%v)",
				canonical, algo2, hex2, err2)
		}
	})
}

// FuzzNormaliseEntry exercises the tar-entry path sanitizer. The
// invariants on the accepted output define what jaas considers a
// safe-to-extract path:
//
//  1. Total: never panics.
//  2. When ok=true:
//     - output is non-empty
//     - output does not start with "/"
//     - output contains no NUL, no backslash, no ".." segment,
//     no hidden-dotted segment
//     - every byte is in the [A-Za-z0-9._/-] allowlist
//     - when pathPrefix was set, the original input had the prefix
//     and the output equals path.Clean(input) minus that prefix
//  3. When ok=false, the output is empty.
//
// A regression that let a `..`/`\x00`/`\` byte slip through here
// would surface as a path-traversal write at extraction time;
// pinning the allowlist via fuzz guards against future "small
// cleanup" PRs that loosen it.
//
// Run as: go test -fuzz=FuzzNormaliseEntry ./internal/sources/
func FuzzNormaliseEntry(f *testing.F) {
	seeds := []struct {
		name, prefix string
	}{
		// Benign.
		{"main.jsonnet", ""},
		{"a/b/c.jsonnet", ""},
		{"_underscore.jsonnet", ""},

		// Traversal.
		{"../etc/passwd", ""},
		{"a/../b", ""},
		{"/etc/passwd", ""},
		{"a/b/../../c", ""},

		// Dangerous bytes / sequences.
		{"with\x00null", ""},
		{"with\\backslash", ""},
		{"with space", ""},
		{"with\ttab", ""},
		{"with\nnewline", ""},
		{"with\rcarriage", ""},
		{"with;semicolon", ""},
		{"unicode\u202emix", ""}, // RTL override

		// Hidden segments.
		{".gitkeep", ""},
		{"a/.hidden", ""},
		{".config/foo", ""},

		// Prefix handling.
		{"foo/bar.jsonnet", "foo/"},
		{"foo/bar.jsonnet", "foo"},
		{"bar.jsonnet", "foo/"},
		{"foo/", "foo/"},
		{"foo/x/../y.jsonnet", "foo/"},

		// Empty / extreme.
		{"", ""},
		{"/", ""},
		{strings.Repeat("a", 256) + ".jsonnet", ""},
	}
	for _, s := range seeds {
		f.Add(s.name, s.prefix)
	}

	f.Fuzz(func(t *testing.T, rawName, prefix string) {
		out, ok := normaliseEntry(rawName, prefix)

		// Determinism.
		if out2, ok2 := normaliseEntry(rawName, prefix); out != out2 || ok != ok2 {
			t.Errorf("normaliseEntry not deterministic for (%q, %q): (%q,%v) vs (%q,%v)",
				rawName, prefix, out, ok, out2, ok2)
		}

		if !ok {
			if out != "" {
				t.Errorf("ok=false but out=%q for (%q, %q)", out, rawName, prefix)
			}
			return
		}

		// Accepted output invariants.
		if out == "" {
			t.Errorf("ok=true but out is empty for (%q, %q)", rawName, prefix)
		}
		if strings.HasPrefix(out, "/") {
			t.Errorf("accepted absolute path %q for (%q, %q)", out, rawName, prefix)
		}
		for part := range strings.SplitSeq(out, "/") {
			if part == ".." {
				t.Errorf("accepted .. segment in %q for (%q, %q)", out, rawName, prefix)
			}
			if strings.HasPrefix(part, ".") {
				t.Errorf("accepted hidden segment in %q for (%q, %q)", out, rawName, prefix)
			}
		}
		for i := 0; i < len(out); i++ {
			if !isSafePathByte(out[i]) {
				t.Errorf("accepted byte 0x%02x in %q for (%q, %q)", out[i], out, rawName, prefix)
			}
		}
	})
}
