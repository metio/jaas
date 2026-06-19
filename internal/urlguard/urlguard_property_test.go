/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package urlguard

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"pgregory.net/rapid"
)

// These rapid property tests complement the unit tables in urlguard_test.go
// and FuzzValidateHTTPURL: rather than pinning specific literals, they assert
// the structural invariants over the whole input space — every forbidden
// address denotes a rejection regardless of which inet_aton-style form names
// it, and every routable address is allowed by the IP-level check.

// alternativeForms expands a 32-bit IPv4 into the spellings a libc resolver
// (and parseAlternativeIPv4) honor: canonical dotted-quad, single decimal /
// hex / octal integer, and the short dotted forms. Each must collapse to the
// same address, so a denied address stays denied in every form.
func alternativeForms(a, b, c, d byte) []string {
	v := uint32(a)<<24 | uint32(b)<<16 | uint32(c)<<8 | uint32(d)
	return []string{
		fmt.Sprintf("%d.%d.%d.%d", a, b, c, d),
		fmt.Sprintf("%d", v),   // single decimal int
		fmt.Sprintf("0x%x", v), // single hex int
		fmt.Sprintf("0%o", v),  // single octal int
		fmt.Sprintf("%d.%d", a, uint32(b)<<16|uint32(c)<<8|uint32(d)), // 2-part
		fmt.Sprintf("%d.%d.%d", a, b, uint32(c)<<8|uint32(d)),         // 3-part
		fmt.Sprintf("0x%x.0x%x.0x%x.0x%x", a, b, c, d),                // dotted hex
	}
}

// genForbiddenIPv4 draws a byte tuple that denotes a forbidden IPv4 address:
// loopback (127.0.0.0/8), link-local incl. cloud metadata (169.254.0.0/16),
// unspecified (0.0.0.0), or multicast (224.0.0.0/4). ipIsForbidden classifies
// exactly these.
func genForbiddenIPv4() *rapid.Generator[[4]byte] {
	return rapid.Custom(func(t *rapid.T) [4]byte {
		kind := rapid.IntRange(0, 3).Draw(t, "kind")
		b := rapid.Byte().Draw(t, "b")
		c := rapid.Byte().Draw(t, "c")
		d := rapid.Byte().Draw(t, "d")
		switch kind {
		case 0: // loopback 127.0.0.0/8
			return [4]byte{127, b, c, d}
		case 1: // link-local 169.254.0.0/16 (cloud metadata lives here)
			return [4]byte{169, 254, c, d}
		case 2: // unspecified
			return [4]byte{0, 0, 0, 0}
		default: // multicast 224.0.0.0/4 → first octet 224..239
			return [4]byte{byte(224 + rapid.IntRange(0, 15).Draw(t, "mcast")), b, c, d}
		}
	})
}

// genRoutableIPv4 draws a byte tuple that denotes a publicly-routable IPv4
// address — none of loopback / link-local / unspecified / multicast. Private
// ranges (RFC1918 etc.) are deliberately permitted by the validator, so they
// are also "routable" for ForbiddenIP's purposes; the generator picks first
// octets that avoid every forbidden range so the invariant is unambiguous.
func genRoutableIPv4() *rapid.Generator[[4]byte] {
	return rapid.Custom(func(t *rapid.T) [4]byte {
		// First octet in 1..223 excludes 0 (unspecified head) and the
		// 224..255 multicast/reserved block; we further exclude 127.
		first := rapid.IntRange(1, 223).Filter(func(n int) bool { return n != 127 }).Draw(t, "first")
		b := rapid.Byte().Draw(t, "b")
		c := rapid.Byte().Draw(t, "c")
		d := rapid.Byte().Draw(t, "d")
		ip := [4]byte{byte(first), b, c, d}
		// Reject the 169.254/16 link-local block explicitly.
		if ip[0] == 169 && ip[1] == 254 {
			ip[1] = 253
		}
		return ip
	})
}

// A forbidden IPv4 is rejected by ValidateHTTPURL in every inet_aton spelling,
// and the rejection always wraps ErrForbiddenHost so downstream errors.Is
// callers classify it.
func TestValidateHTTPURL_RejectsForbiddenIPv4_AllForms(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ip := genForbiddenIPv4().Draw(t, "ip")
		// Confirm the address really is on the denylist; the generator's
		// contract is that it only ever yields forbidden addresses.
		if !ForbiddenIP(net.IPv4(ip[0], ip[1], ip[2], ip[3])) {
			t.Fatalf("generator produced non-forbidden IP %v", ip)
		}
		for _, host := range alternativeForms(ip[0], ip[1], ip[2], ip[3]) {
			rawURL := "http://" + host + "/path"
			err := ValidateHTTPURL(rawURL)
			if err == nil {
				t.Fatalf("ValidateHTTPURL(%q) = nil, want rejection (ip %v)", rawURL, ip)
			}
			if !errors.Is(err, ErrForbiddenHost) {
				t.Fatalf("ValidateHTTPURL(%q) err = %v, want wrapped ErrForbiddenHost", rawURL, err)
			}
		}
	})
}

// A routable IPv4 is never reported forbidden by ForbiddenIP, and the
// canonical-form URL passes ValidateHTTPURL.
func TestValidateHTTPURL_AcceptsRoutableIPv4(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ip := genRoutableIPv4().Draw(t, "ip")
		addr := net.IPv4(ip[0], ip[1], ip[2], ip[3])
		if ForbiddenIP(addr) {
			t.Fatalf("ForbiddenIP(%v) = true, want false for a routable address", addr)
		}
		rawURL := fmt.Sprintf("http://%d.%d.%d.%d/path", ip[0], ip[1], ip[2], ip[3])
		if err := ValidateHTTPURL(rawURL); err != nil {
			t.Fatalf("ValidateHTTPURL(%q) = %v, want nil for a routable address", rawURL, err)
		}
	})
}
