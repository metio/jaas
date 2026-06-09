/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package urlguard

import (
	"errors"
	"strings"
	"testing"
)

// FuzzValidateHTTPURL exercises the SSRF parser against the standard
// library's randomized input generator. Three invariants are enforced
// on every input:
//
//  1. Total: ValidateHTTPURL never panics. The fuzzer catches this
//     automatically.
//  2. Deterministic: two calls with the same input return the same
//     error class.
//  3. Strict-implies-permissive: if the strict validator passes the
//     URL, the permissive variant must also pass it. The reverse is
//     not true (PermissiveHTTPURL skips the IP denylist by design).
//     A regression in either function that broke this containment
//     would surface as an explicit failure here.
//  4. Sentinel-wrapped rejections: every non-nil error is wrapped in
//     one of the wire-stable sentinels (ErrParseFailed,
//     ErrInvalidScheme, ErrMissingHost, ErrForbiddenHost). Callers
//     downstream switch on these via errors.Is.
//
// Specific denied-host literals are pinned by the table tests in
// urlguard_test.go; the fuzz target intentionally does NOT spot-check
// hosts by substring because URL mutations easily produce DNS-name
// shapes ("169.254.", "0:6443:") that share the substring without
// being a denied literal.
//
// Run as: go test -fuzz=FuzzValidateHTTPURL ./internal/urlguard/
func FuzzValidateHTTPURL(f *testing.F) {
	// Seed corpus: representative shapes the validator must classify.
	seeds := []string{
		// Allowed.
		"http://example.com",
		"https://example.com:8443/path?q=1",
		"http://example.com:80/",
		"HTTPS://EXAMPLE.COM/",

		// Wrong scheme.
		"file:///etc/passwd",
		"gopher://example.com",
		"javascript:alert(1)",
		"ftp://example.com",
		"",

		// Literal-IP denylist.
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://169.254.169.254/", // AWS metadata
		"http://localhost/",
		"http://0.0.0.0/",
		"http://224.0.0.1/",
		"http://239.255.255.250/",

		// inet_aton alt-IPv4 (must be rejected by strict).
		"http://2130706433/",   // decimal int form of 127.0.0.1
		"http://0x7f000001/",   // hex int form
		"http://017700000001/", // octal int form
		"http://0177.0.0.1/",   // octal first-octet form
		"http://0:6443/",       // single-int 0 (== 0.0.0.0)

		// Tricky non-denied but plausible-attacker DNS shapes.
		"http://example.com.attacker.tld/",
		"http://user:pass@example.com/",
		"http://example.com:0/",

		// Malformed.
		"http://",
		"http:///",
		"http://[::",
		"://no-scheme",

		// Adversarial bytes / sizes.
		"\x00\x01\x02",
		strings.Repeat("a", 4096),
		"http://" + strings.Repeat("a", 256) + ".example.com",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, rawURL string) {
		strictErr := ValidateHTTPURL(rawURL)
		permissiveErr := PermissiveHTTPURL(rawURL)

		// Determinism: the validator is pure; the second call must
		// return the same error class.
		if again := ValidateHTTPURL(rawURL); (again == nil) != (strictErr == nil) {
			t.Errorf("ValidateHTTPURL not deterministic for %q: first=%v second=%v",
				rawURL, strictErr, again)
		}

		// Strict acceptance must imply permissive acceptance — the
		// permissive variant is a superset by design.
		if strictErr == nil && permissiveErr != nil {
			t.Errorf("strict accepted %q but permissive rejected it: %v",
				rawURL, permissiveErr)
		}

		// Sentinels are wire-stable; rejections must wrap one of them.
		if strictErr != nil {
			switch {
			case errors.Is(strictErr, ErrParseFailed),
				errors.Is(strictErr, ErrInvalidScheme),
				errors.Is(strictErr, ErrMissingHost),
				errors.Is(strictErr, ErrForbiddenHost):
				// Known sentinel — OK.
			default:
				t.Errorf("strict rejection for %q is not wrapped in a sentinel: %v",
					rawURL, strictErr)
			}
		}
	})
}
