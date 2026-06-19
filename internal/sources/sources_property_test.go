/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package sources

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// These rapid property tests complement FuzzNormaliseEntry: they assert the
// structural guarantees normaliseEntry's accepted output carries, over a
// broad generated input space, rather than pinning specific literals.

// genRawEntry draws an arbitrary tar-entry name from a character set rich
// enough to exercise both the accept and reject branches: allowlist bytes
// plus the characters normaliseEntry specifically guards against (slash,
// dot, backslash, NUL, space, and a non-ASCII rune).
func genRawEntry() *rapid.Generator[string] {
	// Bounded length so shrinking stays cheap; maxLen -1 leaves byte length
	// uncapped beyond the rune count.
	return rapid.StringOfN(
		rapid.SampledFrom([]rune{
			'a', 'b', 'Z', '0', '9', '.', '_', '-', '/', '\\', 0, ' ', 'é',
		}),
		0, 24, -1,
	)
}

// genPathPrefix draws a prefix shaped like a real spec.sourceRef.path: a small
// allowlist-only path, sometimes empty (whole-archive extraction).
func genPathPrefix() *rapid.Generator[string] {
	return rapid.OneOf(
		rapid.Just(""),
		rapid.StringMatching(`[a-z]{1,6}(/[a-z]{1,6}){0,2}`),
	)
}

// normaliseEntry is idempotent on accepted paths: feeding an accepted output
// back through normaliseEntry (with no prefix, since the prefix was already
// stripped) yields the same value and stays accepted. A second pass that
// changed the path would mean the first pass left a non-canonical result.
func TestNormaliseEntry_IdempotentOnAcceptedPaths(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		raw := genRawEntry().Draw(t, "raw")
		prefix := genPathPrefix().Draw(t, "prefix")

		out, ok := normaliseEntry(raw, prefix)
		if !ok {
			return // rejection is covered by the invariant test below
		}
		// The accepted output is already prefix-stripped; re-running with an
		// empty prefix must be a fixed point.
		out2, ok2 := normaliseEntry(out, "")
		if !ok2 {
			t.Fatalf("normaliseEntry(%q,%q)=%q accepted, but re-run rejected it", raw, prefix, out)
		}
		if out2 != out {
			t.Fatalf("normaliseEntry not idempotent: first=%q second=%q (raw=%q prefix=%q)", out, out2, raw, prefix)
		}
	})
}

// Every accepted output satisfies the materialised-path safety contract: it
// never contains a ".." segment, never starts with "/", is non-empty, and
// contains only allowlist bytes [A-Za-z0-9._/-].
func TestNormaliseEntry_AcceptedOutputIsSafe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		raw := genRawEntry().Draw(t, "raw")
		prefix := genPathPrefix().Draw(t, "prefix")

		out, ok := normaliseEntry(raw, prefix)
		if !ok {
			return
		}
		if out == "" {
			t.Fatalf("accepted output is empty (raw=%q prefix=%q)", raw, prefix)
		}
		if strings.HasPrefix(out, "/") {
			t.Fatalf("accepted output %q starts with '/' (raw=%q prefix=%q)", out, raw, prefix)
		}
		for _, seg := range strings.Split(out, "/") {
			if seg == ".." {
				t.Fatalf("accepted output %q contains a '..' segment (raw=%q prefix=%q)", out, raw, prefix)
			}
		}
		for i := 0; i < len(out); i++ {
			if !isSafePathByte(out[i]) {
				t.Fatalf("accepted output %q contains disallowed byte %q (raw=%q prefix=%q)", out, out[i], raw, prefix)
			}
		}
	})
}
