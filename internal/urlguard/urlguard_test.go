/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package urlguard

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// TestForbiddenIP pins the IP-level denylist the sources dialer uses at
// connection time (the DNS-rebinding defence). Private ranges are
// intentionally NOT forbidden — in-cluster sources live on them.
func TestForbiddenIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},       // loopback
		{"::1", true},             // loopback v6
		{"169.254.169.254", true}, // link-local (cloud metadata)
		{"224.0.0.1", true},       // multicast
		{"0.0.0.0", true},         // unspecified
		{"8.8.8.8", false},        // public
		{"10.0.0.5", false},       // RFC1918 — reachable by design
		{"192.168.1.1", false},    // RFC1918 — reachable by design
		{"fd00::1", false},        // IPv6 ULA — reachable by design
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			if got := ForbiddenIP(ip); got != tc.want {
				t.Errorf("ForbiddenIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

// Invariants pinned by this file:
//
//   ValidateHTTPURL is the single source of truth for SSRF defence.
//   Adding a new forbidden range to isForbiddenHost must close the
//   corresponding hole in every caller (controller + sources today)
//   simultaneously. The tests below pin every branch:
//
//     1. Parse failure → ErrParseFailed
//     2. Non-http(s) scheme → ErrInvalidScheme
//     3. Empty host → ErrMissingHost
//     4. Loopback / link-local / multicast / unspecified / literal
//        "localhost" → ErrForbiddenHost
//     5. Routable hosts → nil
//
//   Plus a fuzz target proving the function never panics on any
//   byte sequence.

// =========================================================================
// Layer 1: each error branch
// =========================================================================

func TestValidateHTTPURL_AcceptsRoutableHTTP(t *testing.T) {
	for _, raw := range []string{
		"http://example.com",
		"https://example.com",
		"http://example.com:8080",
		"https://jaas.example.org/jsonnet",
		"http://10.10.10.10",   // RFC1918 — routable per validator
		"http://192.0.2.1",     // documentation range — routable
		"http://[2001:db8::1]", // documentation v6 — routable
		"http://source-controller.flux-system.svc/path/to/artifact.tgz",
		"https://ghcr.io/foo/bar",
	} {
		t.Run(raw, func(t *testing.T) {
			if err := ValidateHTTPURL(raw); err != nil {
				t.Errorf("got %v, want nil", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsNonHTTPSchemes(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://example.com",
		"ftp://example.com/foo",
		"javascript:alert(1)",
		"ssh://example.com",
		"data:text/plain,hello",
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrInvalidScheme) {
				t.Errorf("err = %v, want wrapping ErrInvalidScheme", err)
			}
		})
	}
}

func TestValidateHTTPURL_SchemeCheckIsCaseInsensitive(t *testing.T) {
	// `HTTP://...` and `HTTPS://...` are valid (RFC 3986 §3.1 — scheme
	// matching is case-insensitive).
	for _, raw := range []string{"HTTP://example.com", "HTTPS://example.com", "Http://example.com"} {
		t.Run(raw, func(t *testing.T) {
			if err := ValidateHTTPURL(raw); err != nil {
				t.Errorf("upper/mixed-case http scheme rejected: %v", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsEmptyHost(t *testing.T) {
	// "http:" / "https:" alone parse to scheme-only URLs with no host.
	for _, raw := range []string{"http:", "https:", "http:///foo", "https:///"} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrMissingHost) {
				t.Errorf("err = %v, want ErrMissingHost", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsLoopback(t *testing.T) {
	for _, raw := range []string{
		"http://localhost",
		"http://localhost/foo",
		"http://localhost:8080",
		"http://LOCALHOST", // case-insensitive
		"http://127.0.0.1",
		"http://127.0.0.1:6443",
		"http://127.1.2.3", // entire 127.0.0.0/8 is loopback
		"http://[::1]",
		"http://[::1]:6443",
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrForbiddenHost) {
				t.Errorf("err = %v, want ErrForbiddenHost", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsTrailingDotForbiddenHosts(t *testing.T) {
	// A single trailing dot is the FQDN root marker; libc resolvers
	// strip it before lookup, so these dial loopback / link-local just
	// like their dotless forms. net.ParseIP and the inet_aton parser
	// both reject the trailing dot, so without normalisation they slip
	// the denylist.
	for _, raw := range []string{
		"http://localhost.",
		"http://localhost.:8080",
		"http://127.0.0.1.",
		"http://127.0.0.1.:6443",
		"http://169.254.169.254.", // cloud metadata with trailing dot
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrForbiddenHost) {
				t.Errorf("err = %v, want ErrForbiddenHost (trailing-dot form)", err)
			}
		})
	}
}

func TestValidateHTTPURL_AcceptsTrailingDotRoutableHost(t *testing.T) {
	// A legitimate FQDN-rooted public name must still pass — stripping
	// the trailing dot must not turn a routable name into a rejection.
	if err := ValidateHTTPURL("http://source-controller.flux-system.svc.cluster.local./artifact.tar.gz"); err != nil {
		t.Errorf("trailing-dot routable host rejected: %v", err)
	}
}

func TestValidateHTTPURL_RejectsLinkLocal(t *testing.T) {
	// 169.254.0.0/16 covers AWS / GCP / Azure cloud-metadata endpoints —
	// the canonical SSRF target for tenant-controlled URLs.
	for _, raw := range []string{
		"http://169.254.169.254", // EC2 IMDS, GCE metadata, Azure IMDS
		"http://169.254.169.254/latest/meta-data",
		"http://169.254.170.2",              // ECS task metadata
		"http://[fe80::1]",                  // IPv6 link-local
		"http://[fe80::a00:27ff:fe4e:66a1]", // realistic v6 link-local
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrForbiddenHost) {
				t.Errorf("err = %v, want ErrForbiddenHost", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsMulticast(t *testing.T) {
	for _, raw := range []string{
		"http://224.0.0.1",
		"http://239.255.255.255",
		"http://[ff00::1]",
		"http://[ff02::1]", // link-local multicast
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrForbiddenHost) {
				t.Errorf("err = %v, want ErrForbiddenHost", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsUnspecified(t *testing.T) {
	for _, raw := range []string{
		"http://0.0.0.0",
		"http://0.0.0.0:8080",
		"http://[::]",
		// The whole 0.0.0.0/8 block, not just 0.0.0.0: Linux routes
		// connect() to any 0.0.0.x address to loopback.
		"http://0.0.0.1",
		"http://0.0.0.7:6443/x",
		"http://0.1.2.3",
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrForbiddenHost) {
				t.Errorf("err = %v, want ErrForbiddenHost", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsParseFailure(t *testing.T) {
	// url.Parse is extremely lenient — most "garbage" still produces a
	// parseable URL with no host. The control-character branch is the
	// most reliable parse-failure trigger.
	raw := "http://example.com/\x00invalid"
	err := ValidateHTTPURL(raw)
	if !errors.Is(err, ErrParseFailed) {
		t.Errorf("err = %v, want ErrParseFailed", err)
	}
}

func TestValidateHTTPURL_NonIPHostnamesNotDenylisted(t *testing.T) {
	// DNS-resolution defence is out of scope. A hostile record
	// pointing `internal.example` at 127.0.0.1 passes urlguard;
	// cluster NetworkPolicy / egress proxy is the right layer.
	if err := ValidateHTTPURL("http://internal.example"); err != nil {
		t.Errorf("DNS-named host should pass at the urlguard layer: %v", err)
	}
}

func TestValidateHTTPURL_RejectsAlternativeIPv4ForLoopback(t *testing.T) {
	// libc-style inet_aton(3) accepts each of these as 127.0.0.1.
	// The CGO resolver (and macOS by default) dials them; the
	// pure-Go resolver doesn't. Without this branch a tenant URL
	// like `http://2130706433:6443/` reaches the local kube
	// apiserver — RBAC blocks the request but the dial succeeds,
	// which is one layer of defence-in-depth too few.
	for _, raw := range []string{
		"http://2130706433",   // 0x7f000001 in decimal
		"http://0x7f000001",   // hex form
		"http://017700000001", // octal form
		"http://127.1",        // 2-part short form
		"http://127.0.1",      // 3-part short form
		"http://0x7f.0.0.1",   // hex first octet
		"http://0177.0.0.1",   // octal first octet
		"http://2130706433:6443/probe",
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrForbiddenHost) {
				t.Errorf("err = %v, want ErrForbiddenHost (alt-IPv4 form for 127.0.0.1)", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsAlternativeIPv4ForUnspecified(t *testing.T) {
	// `http://0/` resolves to 0.0.0.0, which Linux's connect(2)
	// silently treats as "localhost" — the kernel rewrites it to
	// the loopback. So a bare-zero host reaches our apiserver,
	// metrics endpoint, anything bound to 127.0.0.1.
	for _, raw := range []string{
		"http://0",
		"http://0:6443",
		"http://0:8080/x",
		"http://00000000", // octal zero
		"http://0x0",      // hex zero
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrForbiddenHost) {
				t.Errorf("err = %v, want ErrForbiddenHost (alt form for 0.0.0.0)", err)
			}
		})
	}
}

func TestValidateHTTPURL_RejectsAlternativeIPv4ForCloudMetadata(t *testing.T) {
	// 169.254.169.254 in alt-IPv4 forms. Same SSRF surface as the
	// canonical literal — cloud metadata endpoints with no auth.
	for _, raw := range []string{
		"http://2852039166",                  // 0xA9FEA9FE in decimal
		"http://0xa9fea9fe",                  // hex form
		"http://169.254.43518",               // 3-part short form (43518 = 0xA9FE)
		"http://169.16689406",                // 2-part short form
		"http://2852039166/latest/meta-data", // EC2 IMDS path
	} {
		t.Run(raw, func(t *testing.T) {
			err := ValidateHTTPURL(raw)
			if !errors.Is(err, ErrForbiddenHost) {
				t.Errorf("err = %v, want ErrForbiddenHost (alt form for 169.254.169.254)", err)
			}
		})
	}
}

func TestValidateHTTPURL_DoesNotMisidentifyRealDNSNamesAsAltIPv4(t *testing.T) {
	// Hostnames that LOOK numeric but resolve as DNS names must
	// still pass the validator. Splitting on `.` and trying to
	// ParseUint each segment leaves alphabetic segments unparseable
	// → falls through to the DNS-pass case. Pin a few realistic
	// shapes so a future refactor of parseAlternativeIPv4 doesn't
	// turn `mycorp.example` into a forbidden host.
	for _, raw := range []string{
		"http://internal.example",
		"http://my-svc.default.svc.cluster.local",
		"http://api-v2.team.corp",
		"http://12345.example.com", // numeric segment, but ".example.com" makes it DNS
		"http://0x7f.example.com",  // hex-looking but compound DNS
		"http://0177.svc.cluster",  // octal-looking but compound DNS
		"http://service.123",       // numeric TLD
		"http://my-app:8080",       // single label, port
		"http://my-app.local:443",  // ".local" mDNS
	} {
		t.Run(raw, func(t *testing.T) {
			if err := ValidateHTTPURL(raw); err != nil {
				t.Errorf("DNS-named host wrongly classified as alt-IPv4: err=%v", err)
			}
		})
	}
}

func TestValidateHTTPURL_AltIPv4OutsideForbiddenRangesStillPass(t *testing.T) {
	// Alt forms for routable addresses must pass — the denylist
	// is about ranges, not about the notation. Pin so a too-broad
	// reject doesn't accidentally swallow legitimate use.
	for _, raw := range []string{
		"http://3232235521", // 0xC0A80001 = 192.168.0.1 (RFC1918, routable per validator)
		"http://0xc0a80001", // same, hex
		"http://192.168.1",  // short form for 192.168.0.1
		"http://10.0.0.1",   // canonical RFC1918 — control case
	} {
		t.Run(raw, func(t *testing.T) {
			if err := ValidateHTTPURL(raw); err != nil {
				t.Errorf("routable alt-IPv4 wrongly rejected: %v", err)
			}
		})
	}
}

// =========================================================================
// Layer 1 — parseAlternativeIPv4 directly: pin the corner cases that
// would otherwise only surface through the wrapped ValidateHTTPURL.
// =========================================================================

func TestParseAlternativeIPv4_RejectsEmpty(t *testing.T) {
	if ip := parseAlternativeIPv4(""); ip != nil {
		t.Errorf("got %v, want nil for empty input", ip)
	}
}

func TestParseAlternativeIPv4_RejectsMoreThanFourParts(t *testing.T) {
	if ip := parseAlternativeIPv4("1.2.3.4.5"); ip != nil {
		t.Errorf("got %v, want nil for 5-part input", ip)
	}
}

func TestParseAlternativeIPv4_RejectsEmptySegment(t *testing.T) {
	// Trailing or leading dots leave empty segments; ParseUint
	// would accept "" as 0 if we let it, silently turning
	// `1..2.3` into a forbidden 1.0.2.3 maybe. Reject up front.
	for _, raw := range []string{".", "1.", ".1", "1..2", "..."} {
		t.Run(raw, func(t *testing.T) {
			if ip := parseAlternativeIPv4(raw); ip != nil {
				t.Errorf("got %v, want nil for empty-segment input", ip)
			}
		})
	}
}

func TestParseAlternativeIPv4_RejectsOversizedSlots(t *testing.T) {
	// Each "early" slot is capped at 0xFF; the last slot at the
	// remaining width. Reject when those caps are blown so we
	// don't silently produce a phantom IP from `300.0.0.1`.
	for _, raw := range []string{
		"256.0.0.1",   // early slot > 0xFF
		"1.256.0.1",   // middle slot > 0xFF
		"1.0.0x10000", // 3-part last slot > 0xFFFF
		"1.0x1000000", // 2-part last slot > 0xFFFFFF
		"0x100000000", // 1-part > 0xFFFFFFFF
	} {
		t.Run(raw, func(t *testing.T) {
			if ip := parseAlternativeIPv4(raw); ip != nil {
				t.Errorf("got %v, want nil for oversized slot %q", ip, raw)
			}
		})
	}
}

func TestParseAlternativeIPv4_AcceptsAllFourCardinalities(t *testing.T) {
	// Each switch arm (1/2/3/4-part) must produce the right
	// address. Drives the table-driven path so coverage hits every
	// case.
	cases := []struct {
		in   string
		want string
	}{
		{"127.0.0.1", "127.0.0.1"},  // canonical 4-part
		{"127.0.1", "127.0.0.1"},    // 3-part: last consumes 16 bits
		{"127.1", "127.0.0.1"},      // 2-part: last consumes 24 bits
		{"2130706433", "127.0.0.1"}, // 1-part: consumes all 32 bits
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseAlternativeIPv4(tc.in)
			if got == nil || got.String() != tc.want {
				t.Errorf("parseAlternativeIPv4(%q) = %v, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// =========================================================================
// Layer 1: error-message hygiene
// =========================================================================

func TestValidateHTTPURL_InvalidSchemeMessageIncludesScheme(t *testing.T) {
	err := ValidateHTTPURL("ftp://example.com")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "ftp") {
		t.Errorf("err = %q, want it to name the offending scheme", err)
	}
}

func TestValidateHTTPURL_ForbiddenHostMessageIncludesHost(t *testing.T) {
	err := ValidateHTTPURL("http://169.254.169.254/foo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("err = %q, want it to name the offending host", err)
	}
}

// =========================================================================
// Layer 1: sentinel identity (errors.Is propagates through wrapping)
// =========================================================================

func TestSentinels_DistinctAndNonNil(t *testing.T) {
	all := []error{ErrInvalidScheme, ErrMissingHost, ErrForbiddenHost, ErrParseFailed}
	for i, a := range all {
		if a == nil {
			t.Errorf("sentinel[%d] is nil", i)
		}
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel[%d] errors.Is sentinel[%d] — must be distinct", i, j)
			}
		}
	}
}

// =========================================================================
// Layer 1: PermissiveHTTPURL — scheme check only
// =========================================================================

func TestPermissiveHTTPURL_AcceptsLoopback(t *testing.T) {
	// The whole point of the permissive variant: 127.0.0.1 (and
	// equivalents) reach the caller. httptest.NewServer uses this.
	for _, raw := range []string{
		"http://127.0.0.1",
		"http://127.0.0.1:8080/path",
		"http://[::1]",
		"http://localhost:6443",
		"http://0.0.0.0",
		"http://169.254.169.254",
	} {
		t.Run(raw, func(t *testing.T) {
			if err := PermissiveHTTPURL(raw); err != nil {
				t.Errorf("permissive validator rejected %q: %v", raw, err)
			}
		})
	}
}

func TestPermissiveHTTPURL_StillRejectsNonHTTPScheme(t *testing.T) {
	// The scheme check is still meaningful: a file:// or gopher:// URL
	// would surface in test data too, and the scheme is the cheapest
	// honesty check.
	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://example.com",
		"ftp://example.com",
		"javascript:alert(1)",
	} {
		t.Run(raw, func(t *testing.T) {
			err := PermissiveHTTPURL(raw)
			if !errors.Is(err, ErrInvalidScheme) {
				t.Errorf("err = %v, want wrapping ErrInvalidScheme", err)
			}
		})
	}
}

func TestPermissiveHTTPURL_RejectsParseFailure(t *testing.T) {
	err := PermissiveHTTPURL("http://example.com/\x00invalid")
	if !errors.Is(err, ErrParseFailed) {
		t.Errorf("err = %v, want ErrParseFailed", err)
	}
}

// FuzzValidateHTTPURL lives in fuzz_test.go — the richer invariant
// check supersedes a panic-only check.
