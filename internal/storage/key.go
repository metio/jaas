/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"fmt"
	"strings"
)

// RevisionFilename maps an artifact revision to its on-disk / object filename.
// A revision is the artifact's "<algo>:<hex>" digest; the ':' is replaced with
// '-' so the digest is a safe single path segment / object-name component on
// every backend, and the algorithm is preserved (a future non-sha256 digest
// won't collide with an sha256 one of the same hex). The writer (Publisher via
// Put) and the readers (the HTTP fetch path and the MCP diff tool via Open) all
// derive the name here, so the addressing is one contract that can't drift.
//
// A revision with no ':' (a bare hex stub) maps to "<rev>.tar.gz" unchanged, so
// callers that pass an already-bare token keep their existing layout.
func RevisionFilename(revision string) string {
	return strings.ReplaceAll(revision, ":", "-") + ".tar.gz"
}

// ValidDigest reports whether revision has the canonical "<algo>:<hex>" shape.
// The Publisher always passes a digest it just computed, but a reader such as
// the MCP diff_revisions tool takes the revision from the caller — and it
// becomes part of an object name / path, so a '/'- or '..'-bearing value could
// address a different artifact within the backend. Rejecting anything that
// isn't <algo>:<hex> keeps it a safe single segment on every backend.
func ValidDigest(revision string) error {
	algo, hexPart, ok := strings.Cut(revision, ":")
	if !ok || algo == "" || hexPart == "" {
		return fmt.Errorf("revision %q must be of the form <algo>:<hex>", revision)
	}
	for _, r := range algo {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return fmt.Errorf("revision %q has an invalid algorithm", revision)
		}
	}
	for _, r := range hexPart {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return fmt.Errorf("revision %q has a non-hex value", revision)
		}
	}
	return nil
}
