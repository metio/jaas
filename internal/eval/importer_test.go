/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"strings"
	"sync"
	"testing"
)

func TestInMemoryImporter_AliasedFileResolves(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"colors.libsonnet": `{ red: "#f00" }`}},
		},
	}
	contents, foundAt, err := im.Import("", "utils/colors.libsonnet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundAt != "utils/colors.libsonnet" {
		t.Errorf("foundAt = %q, want %q", foundAt, "utils/colors.libsonnet")
	}
	if !strings.Contains(contents.String(), "red") {
		t.Errorf("contents = %q, want it to contain 'red'", contents.String())
	}
}

func TestInMemoryImporter_BareAliasUsesDefaultEntry(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"main.libsonnet": `{ entry: true }`}},
		},
	}
	contents, foundAt, err := im.Import("", "utils")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundAt != "utils/main.libsonnet" {
		t.Errorf("foundAt = %q, want %q", foundAt, "utils/main.libsonnet")
	}
	if !strings.Contains(contents.String(), "entry") {
		t.Errorf("contents = %q, want it to contain 'entry'", contents.String())
	}
}

// A second Import of the same path (a cache hit) must return the same
// foundAt as the first (a cache miss). go-jsonnet keys its parsed-AST
// cache on foundAt, so an inconsistent value would make it parse the
// library twice or mis-resolve a transitive relative import.
func TestInMemoryImporter_CachedFoundAtMatchesFirstResolve(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"main.libsonnet": `{ entry: true }`}},
		},
	}
	c1, foundAt1, err := im.Import("", "utils")
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	c2, foundAt2, err := im.Import("", "utils") // cache hit
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if foundAt1 != "utils/main.libsonnet" {
		t.Fatalf("first foundAt = %q, want utils/main.libsonnet", foundAt1)
	}
	if foundAt2 != foundAt1 {
		t.Errorf("cached foundAt = %q, want %q (must match first resolve)", foundAt2, foundAt1)
	}
	if c1.String() != c2.String() {
		t.Errorf("cached contents differ: %q vs %q", c1.String(), c2.String())
	}
}

func TestInMemoryImporter_BareAliasWithoutDefaultEntryErrors(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"only.libsonnet": `{}`}},
		},
	}
	_, _, err := im.Import("", "utils")
	if err == nil || !strings.Contains(err.Error(), "no main.libsonnet") {
		t.Fatalf("got %v, want a 'no main.libsonnet' error", err)
	}
}

func TestInMemoryImporter_SiblingFileResolves(t *testing.T) {
	im := &InMemoryImporter{
		Self: Library{Files: map[string]string{"helper.libsonnet": `{ helper: true }`}},
	}
	contents, foundAt, err := im.Import("", "helper.libsonnet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundAt != "helper.libsonnet" {
		t.Errorf("foundAt = %q, want %q", foundAt, "helper.libsonnet")
	}
	if !strings.Contains(contents.String(), "helper") {
		t.Errorf("contents = %q, want 'helper'", contents.String())
	}
}

func TestInMemoryImporter_SiblingTakesPrecedenceOverLibraryDefaultEntry(t *testing.T) {
	im := &InMemoryImporter{
		Self: Library{Files: map[string]string{"utils": `{ from: "self" }`}},
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"main.libsonnet": `{ from: "library" }`}},
		},
	}
	contents, _, err := im.Import("", "utils")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(contents.String(), "self") {
		t.Errorf("contents = %q, want sibling to win over library", contents.String())
	}
}

// A slash-prefixed path whose head is not a registered alias is not assumed to
// be an alias — it falls through to the vendor search (so jb absolute imports
// like github.com/... can resolve). With nothing to match, it is a plain
// not-found.
func TestInMemoryImporter_UnknownSlashPathFallsThroughToNotFound(t *testing.T) {
	im := &InMemoryImporter{}
	_, _, err := im.Import("", "ghost/file.libsonnet")
	if err == nil || !strings.Contains(err.Error(), "matches no sibling file or library alias") {
		t.Fatalf("got %v, want not-found error", err)
	}
}

func TestInMemoryImporter_UnknownFileWithinKnownAliasErrors(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"main.libsonnet": `{}`}},
		},
	}
	_, _, err := im.Import("", "utils/missing.libsonnet")
	if err == nil || !strings.Contains(err.Error(), `file "missing.libsonnet" not found`) {
		t.Fatalf("got %v, want file-not-found error", err)
	}
}

func TestInMemoryImporter_UnmatchedBarePathErrors(t *testing.T) {
	im := &InMemoryImporter{}
	_, _, err := im.Import("", "nothing")
	if err == nil || !strings.Contains(err.Error(), "matches no sibling file or library alias") {
		t.Fatalf("got %v, want unmatched-name error", err)
	}
}

func TestInMemoryImporter_EmptyPathErrors(t *testing.T) {
	im := &InMemoryImporter{}
	_, _, err := im.Import("", "")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("got %v, want empty-path error", err)
	}
}

func TestInMemoryImporter_CacheReturnsSameContentsOnRepeatedCalls(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"main.libsonnet": `{ ok: true }`}},
		},
	}
	first, _, _ := im.Import("", "utils")
	second, _, _ := im.Import("", "utils")
	if first.String() != second.String() {
		t.Errorf("cache returned different Contents for the same path")
	}
}

func TestInMemoryImporter_CacheIsConcurrencySafe(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"main.libsonnet": `{}`}},
		},
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = im.Import("", "utils")
		}()
	}
	wg.Wait()
}

func TestInMemoryImporter_PathCleansDotSegments(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"utils": {Files: map[string]string{"colors.libsonnet": `{}`}},
		},
	}
	_, foundAt, err := im.Import("", "utils/./colors.libsonnet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundAt != "utils/colors.libsonnet" {
		t.Errorf("foundAt = %q, want %q", foundAt, "utils/colors.libsonnet")
	}
}
