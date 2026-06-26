/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// genFileEntries draws a non-empty FileEntry slice with unique paths
// drawn from a small ASCII allowlist. Unique paths are required
// because Put's behavior on duplicate-path entries is unspecified;
// we exercise the determinism property, not the duplicate edge case.
//
// The regex stays narrow on purpose: tar accepts almost any bytes
// in a header, but the property under test is byte-for-byte
// reproducibility — wider character coverage adds shrinking cost
// without exercising any new code path through Put.
func genFileEntries() *rapid.Generator[[]FileEntry] {
	// Each segment is 1-12 chars from a safe ASCII subset. The
	// regex permits "." and ".." as segments; those would fail
	// Backend.Put's traversal check, so filter them out so the
	// property exercises the byte-determinism contract rather than
	// the rejection path (which has its own unit tests).
	pathGen := rapid.StringMatching(`[a-z0-9._\-]{1,12}(/[a-z0-9._\-]{1,12}){0,3}`).
		Filter(func(s string) bool {
			for seg := range strings.SplitSeq(s, "/") {
				if seg == "." || seg == ".." {
					return false
				}
			}
			return true
		})
	contentGen := rapid.SliceOfN(rapid.Byte(), 0, 256)
	entryGen := rapid.Custom(func(t *rapid.T) FileEntry {
		return FileEntry{
			Path:    pathGen.Draw(t, "path"),
			Content: contentGen.Draw(t, "content"),
		}
	})
	return rapid.SliceOfNDistinct(entryGen, 1, 8, func(e FileEntry) string { return e.Path })
}

// TestPut_Property_DigestIsDeterministic establishes that
// `Backend.Put` produces a byte-identical tarball for the same
// canonical input regardless of:
//
//   - The order in which entries were supplied (Put internally
//     sorts by Path before emitting tar headers).
//   - The Store instance that processed them (separate tempdirs,
//     separate gzip writers — but the output bytes must agree).
//
// The downstream guarantee this protects: the sha256 revision a
// snippet publishes via Output=source is reproducible across leader
// hand-offs and across backend swaps (filesystem ↔ S3). A regression
// in the tar/gzip pipeline that introduced any non-determinism
// (a wall-clock timestamp leaked back in, map-iteration order, gzip
// dictionary state) would show up here as a digest mismatch on a
// randomly-chosen input.
func TestPut_Property_DigestIsDeterministic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		entries := genFileEntries().Draw(t, "entries")
		shuffled := shuffleEntries(t, entries)

		a := newRapidStore(t)
		ra, err := a.Put(context.Background(), "ns", "a", "r", entries)
		if err != nil {
			t.Fatalf("Put a: %v", err)
		}
		b := newRapidStore(t)
		rb, err := b.Put(context.Background(), "ns", "b", "r", shuffled)
		if err != nil {
			t.Fatalf("Put b: %v", err)
		}

		if ra.DigestSHA256 != rb.DigestSHA256 {
			t.Errorf("digest not deterministic across stores/orderings: a=%q b=%q",
				ra.DigestSHA256, rb.DigestSHA256)
		}
		if ra.SizeBytes != rb.SizeBytes {
			t.Errorf("size not deterministic: a=%d b=%d", ra.SizeBytes, rb.SizeBytes)
		}

		// And the actual bytes on disk must match.
		bytesA := mustReadFile(t, filepath.Join(a.RootPath(), ra.Path))
		bytesB := mustReadFile(t, filepath.Join(b.RootPath(), rb.Path))
		if !bytes.Equal(bytesA, bytesB) {
			t.Errorf("on-disk bytes differ for same input (digest collision masked the issue): len(a)=%d len(b)=%d",
				len(bytesA), len(bytesB))
		}

		// And the digest the Backend reports must match what
		// sha256-ing the on-disk bytes yields. Catches a bug where
		// Put writes one stream and hashes a different one.
		want := sha256.Sum256(bytesA)
		if hex.EncodeToString(want[:]) != ra.DigestSHA256 {
			t.Errorf("reported digest %q doesn't match sha256 of bytes %q",
				ra.DigestSHA256, hex.EncodeToString(want[:]))
		}
	})
}

// shuffleEntries returns a permutation of entries chosen by rapid.
// The property must hold regardless of the order Put receives them.
func shuffleEntries(t *rapid.T, entries []FileEntry) []FileEntry {
	indices := make([]int, len(entries))
	for i := range indices {
		indices[i] = i
	}
	perm := rapid.Permutation(indices).Draw(t, "perm")
	out := make([]FileEntry, len(entries))
	for i, j := range perm {
		out[i] = entries[j]
	}
	return out
}

func newRapidStore(t *rapid.T) *Store {
	dir, err := os.MkdirTemp("", "jaas-rapid-store-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s, err := New(dir)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustReadFile(t *rapid.T, path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
