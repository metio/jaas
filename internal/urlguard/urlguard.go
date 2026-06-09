/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package urlguard hosts the SSRF-defence checks the operator applies
// to every URL it follows on behalf of a tenant. Two call sites share
// the logic today:
//
//   - internal/controller — JsonnetArtifact.spec.serviceURL (tenant-
//     supplied via the CR's spec; the most direct attack surface).
//   - internal/sources — status.artifact.url on Flux source objects
//     (mostly operator-managed by source-controller, but a tenant
//     with status-write RBAC could poison it).
//
// Centralising the rules prevents drift: adding a new forbidden
// range here protects both call sites at once. Each caller wraps the
// urlguard sentinels with their own contextual prefix so the
// operator log identifies the boundary that fired.
package urlguard

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Sentinel errors for SSRF-defence. Wire-stable: operators may script
// alerting off them via errors.Is, and downstream packages (the
// controller's `ErrServiceURL*` aliases, in particular) name them as
// the underlying identity.
var (
	// ErrInvalidScheme fires when the URL's scheme isn't http or https.
	// Any other scheme that Go's HTTP client would follow (or fail to
	// follow loudly) is rejected — file://, gopher://, ftp://, javascript://.
	ErrInvalidScheme = errors.New("URL scheme must be http or https")

	// ErrMissingHost fires when the URL parses but carries no host.
	ErrMissingHost = errors.New("URL must include a host")

	// ErrForbiddenHost fires when the URL's host literal is on the
	// denylist (loopback, link-local, multicast, unspecified, or the
	// "localhost" string).
	ErrForbiddenHost = errors.New("URL host targets a forbidden surface (loopback/link-local/multicast/unspecified)")

	// ErrParseFailed wraps url.Parse failures so callers can
	// classify them as "URL is structurally broken" rather than
	// generic apiserver errors.
	ErrParseFailed = errors.New("URL failed to parse")
)

// ValidateHTTPURL applies SSRF defence to rawURL. It must:
//
//   - parse cleanly via url.Parse
//   - use the http or https scheme (case-insensitive)
//   - carry a host
//   - NOT name a literal loopback, link-local, multicast, or
//     unspecified address, and NOT name the literal "localhost"
//
// The denylist deliberately covers only the ranges that are dangerous
// regardless of cluster topology: loopback, link-local (which includes
// the cloud metadata endpoint 169.254.169.254), multicast, and the
// unspecified address, plus the "localhost" string. It does NOT block
// RFC1918 / CGNAT / IPv6-ULA private ranges, because the operator's
// primary, legitimate fetch target — a Flux source-controller serving
// artifacts in-cluster — is reached on exactly those private addresses
// (pod IPs, ClusterIP service VIPs). Blocking them would break the main
// use case; scoping who may reach internal private services is
// NetworkPolicy / egress-proxy territory, not this validator's.
//
// String-level only: a hostile DNS record pointing `mycorp.example` at
// `127.0.0.1` passes this layer because it validates the URL string, not
// the resolved address. The connection-time defence against that
// (re-resolving and pinning the dialed IP through the same denylist via
// ForbiddenIP) lives in the dialer of the package that performs the
// fetch — internal/sources — so rebinding is caught even though this
// string check can't see it.
//
// Returns nil iff the URL is safe to follow.
func ValidateHTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidScheme, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return ErrMissingHost
	}
	if isForbiddenHost(host) {
		return fmt.Errorf("%w: %s", ErrForbiddenHost, host)
	}
	return nil
}

// PermissiveHTTPURL is the test-and-dev-only relaxed variant: it
// validates the scheme (http or https) but skips the host denylist
// so httptest.NewServer's 127.0.0.1 listeners still reach the
// caller. Exported so test code in any consumer package can install
// it as a URLValidator override; also reachable by a future operator
// flag (`-allow-loopback`) for dev-cluster ergonomics.
//
// The scheme check is still meaningful: a file:// or gopher:// URL
// would surface in tests too, and the scheme is the cheapest check
// to keep honest.
func PermissiveHTTPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	}
	return fmt.Errorf("%w: got %q", ErrInvalidScheme, u.Scheme)
}

// isForbiddenHost reports whether `host` (the URL's parsed Hostname)
// lands on the denylist. The literal string "localhost" is included
// because it's a common alias for 127.0.0.1 across libc resolvers and
// container DNS — operators who mean "loopback" generally name it
// either way.
//
// `host` is also checked against the inet_aton(3)-style alternative
// IPv4 forms (single integer, hex/octal/short-dotted). Go's pure-Go
// resolver rejects these, but the system resolver (CGO=1, macOS,
// many libc-based stacks) happily passes them through — so without
// this check `http://2130706433/` or `http://0:6443/` would slip past
// `net.ParseIP` and dial loopback. The check is on the literal host
// string only; a DNS record pointing `mycorp.example` at `127.0.0.1`
// still needs egress-proxy / NetworkPolicy defence upstream.
func isForbiddenHost(host string) bool {
	// A single trailing dot is the FQDN root marker. libc resolvers
	// strip it before lookup, so `127.0.0.1.` and `localhost.` dial the
	// same targets as their dotless forms — but net.ParseIP and the
	// inet_aton parser both reject the trailing dot, which would let
	// these slip the denylist. Normalise it away first.
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipIsForbidden(ip)
	}
	if ip := parseAlternativeIPv4(host); ip != nil {
		return ipIsForbidden(ip)
	}
	// Non-IP hostnames (DNS names) are NOT denylisted here — see
	// the package docstring for why.
	return false
}

// ForbiddenIP reports whether ip lands on the SSRF denylist (loopback,
// link-local incl. cloud metadata, multicast, unspecified). Exported so
// a fetch dialer can re-check a resolved address at connection time,
// closing the DNS-rebinding gap the string-only ValidateHTTPURL leaves
// open. Mirrors the same scoping: private ranges are intentionally NOT
// forbidden (see the package doc).
func ForbiddenIP(ip net.IP) bool { return ipIsForbidden(ip) }

// ipIsForbidden classifies a parsed IP against the SSRF denylist.
// Shared between the canonical-form and alternative-form paths so
// adding a new range never has to be repeated.
func ipIsForbidden(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// parseAlternativeIPv4 mirrors inet_aton(3): it accepts the
// non-canonical IPv4 forms a libc resolver would expand into a
// 32-bit address — single-integer decimal/hex/octal, and 2-, 3-,
// or 4-part dotted with any of the per-part bases. Returns nil
// for inputs that aren't interpretable as IPv4 (a real DNS name,
// an IPv6 literal, an over-large number, garbage). The standard
// `net.ParseIP` accepts only the strict 4-octet dotted form, so
// without this helper a `http://0:6443/` reaches loopback.
//
// Bounds per slot mirror inet_aton: the LAST slot consumes the
// remaining 8/16/24/32 bits; earlier slots are capped at 8 bits.
func parseAlternativeIPv4(host string) net.IP {
	if host == "" {
		return nil
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return nil
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" {
			return nil
		}
		// base=0 → autodetect: `0x...` hex, `0o...` / leading-`0` octal,
		// otherwise decimal. Matches what glibc's inet_aton accepts.
		n, err := strconv.ParseUint(p, 0, 64)
		if err != nil {
			return nil
		}
		nums[i] = n
	}
	var addr uint32
	switch len(parts) {
	case 1:
		if nums[0] > 0xFFFFFFFF {
			return nil
		}
		addr = uint32(nums[0]) // #nosec G115 -- each slot is range-checked above to fit uint32
	case 2:
		if nums[0] > 0xFF || nums[1] > 0xFFFFFF {
			return nil
		}
		addr = uint32(nums[0])<<24 | uint32(nums[1]) // #nosec G115 -- each slot is range-checked above to fit uint32
	case 3:
		if nums[0] > 0xFF || nums[1] > 0xFF || nums[2] > 0xFFFF {
			return nil
		}
		addr = uint32(nums[0])<<24 | uint32(nums[1])<<16 | uint32(nums[2]) // #nosec G115 -- each slot is range-checked above to fit uint32
	case 4:
		if nums[0] > 0xFF || nums[1] > 0xFF || nums[2] > 0xFF || nums[3] > 0xFF {
			return nil
		}
		addr = uint32(nums[0])<<24 | uint32(nums[1])<<16 | uint32(nums[2])<<8 | uint32(nums[3]) // #nosec G115 -- each slot is range-checked above to fit uint32
	}
	return net.IPv4(byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr))
}
