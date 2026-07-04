/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"context"
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
	for range 50 {
		wg.Go(func() {
			_, _, _ = im.Import("", "utils")
		})
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

// A Self subtree named like a library alias must not cross-wire with that
// library: each "<alias>/<file>" file (one in Self, one in the library) keeps
// its own root for transitive relative imports, and its own import-cache entry.
// Without the per-file foundAt disambiguation, the two share a foundAt, so the
// library file's `import './b'` would hit the Self file's cached sibling.
func TestInMemoryImporter_SelfShadowsLibraryAlias_NoCrossWire(t *testing.T) {
	im := &InMemoryImporter{
		Self: Library{Files: map[string]string{
			"pkg/a.libsonnet": `import './b.libsonnet'`,
			"pkg/b.libsonnet": `"self-b"`,
		}},
		Libraries: map[string]Library{
			"pkg": {Files: map[string]string{
				"a.libsonnet": `import './b.libsonnet'`,
				"b.libsonnet": `"lib-b"`,
			}},
			"z": {Files: map[string]string{"main.libsonnet": `import 'pkg/a.libsonnet'`}},
		},
	}

	// Entry imports the Self pkg/a (sibling), whose './b' is the Self b. This
	// seeds the import cache with ("pkg/a.libsonnet", "./b.libsonnet") -> self-b.
	_, selfA, err := im.Import("", "pkg/a.libsonnet")
	if err != nil {
		t.Fatalf("self pkg/a: %v", err)
	}
	if selfA != "pkg/a.libsonnet" {
		t.Fatalf("self pkg/a foundAt = %q, want pkg/a.libsonnet (Self keeps the clean foundAt)", selfA)
	}
	if body, _, _ := im.Import(selfA, "./b.libsonnet"); !strings.Contains(body.String(), "self-b") {
		t.Fatalf("self pkg/a's sibling b = %q, want self-b", body.String())
	}

	// Reaching the library pkg/a from another root (library z) must resolve its
	// './b' to the LIBRARY b, not the Self b cached above.
	_, zMain, err := im.Import("", "z")
	if err != nil {
		t.Fatalf("z: %v", err)
	}
	_, libA, err := im.Import(zMain, "pkg/a.libsonnet")
	if err != nil {
		t.Fatalf("library pkg/a: %v", err)
	}
	if libA == selfA {
		t.Fatalf("library pkg/a foundAt %q collides with the Self pkg/a foundAt; transitive imports cross-wire", libA)
	}
	body, _, err := im.Import(libA, "./b.libsonnet")
	if err != nil {
		t.Fatalf("library pkg/a's sibling b: %v", err)
	}
	if !strings.Contains(body.String(), "lib-b") {
		t.Fatalf("library pkg/a's sibling b = %q, want lib-b (must not hit the Self b cache)", body.String())
	}
}

// The same shadowing, end-to-end through the VM: rendering must read each root's
// own b.
func TestEvaluateAnonymousSnippet_SelfShadowsLibraryAlias(t *testing.T) {
	im := &InMemoryImporter{
		Self: Library{Files: map[string]string{
			"pkg/a.libsonnet": `import './b.libsonnet'`,
			"pkg/b.libsonnet": `"self-b"`,
		}},
		Libraries: map[string]Library{
			"pkg": {Files: map[string]string{
				"a.libsonnet": `import './b.libsonnet'`,
				"b.libsonnet": `"lib-b"`,
			}},
			"z": {Files: map[string]string{"main.libsonnet": `import 'pkg/a.libsonnet'`}},
		},
	}
	src := `{ fromSelf: import 'pkg/a.libsonnet', fromLib: import 'z' }`
	out, err := EvaluateAnonymousSnippet(context.Background(), "main.jsonnet", src, Options{Importer: im})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !strings.Contains(out, `"self-b"`) || !strings.Contains(out, `"lib-b"`) {
		t.Fatalf("output = %s, want both self-b and lib-b", out)
	}
}

// A sibling import from an entry living in a subdirectory must resolve against
// the entry's own directory — `jsonnet -J vendor dashboards/main.jsonnet`
// resolves `import 'config.libsonnet'` to dashboards/config.libsonnet, never the
// Self root. EntryPath supplies the entry's location, since its importedFrom is
// never returned by the importer and so has no recorded location.
func TestInMemoryImporter_SubdirEntrySiblingResolvesAgainstEntryDir(t *testing.T) {
	im := &InMemoryImporter{
		Self: Library{Files: map[string]string{
			"dashboards/main.jsonnet":     `import 'config.libsonnet'`,
			"dashboards/config.libsonnet": `{ which: "subdir" }`,
			"config.libsonnet":            `{ which: "root-decoy" }`,
		}},
		EntryPath: "dashboards/main.jsonnet",
	}
	// go-jsonnet hands the entry's diagnostic label back as importedFrom for
	// the entry's own imports; it has no recorded location.
	contents, foundAt, err := im.Import("ns/name/dashboards/main.jsonnet", "config.libsonnet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if foundAt != "dashboards/config.libsonnet" {
		t.Errorf("foundAt = %q, want %q", foundAt, "dashboards/config.libsonnet")
	}
	if !strings.Contains(contents.String(), "subdir") {
		t.Errorf("contents = %q, want the subdir sibling, not the root decoy", contents.String())
	}
}

// End-to-end: a subdir entry whose sibling import has a same-named decoy at the
// Self root must render the subdir sibling, not silently return the decoy.
func TestEvaluateAnonymousSnippet_SubdirEntrySiblingNotShadowedByRootDecoy(t *testing.T) {
	im := &InMemoryImporter{
		Self: Library{Files: map[string]string{
			"dashboards/main.jsonnet":     `import 'config.libsonnet'`,
			"dashboards/config.libsonnet": `{ which: "subdir" }`,
			"config.libsonnet":            `{ which: "root-decoy" }`,
		}},
		EntryPath: "dashboards/main.jsonnet",
	}
	out, err := EvaluateAnonymousSnippet(context.Background(), "ns/name/dashboards/main.jsonnet",
		im.Self.Files["dashboards/main.jsonnet"], Options{Importer: im})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !strings.Contains(out, "subdir") || strings.Contains(out, "root-decoy") {
		t.Fatalf("output = %s, want the subdir sibling, not the root decoy", out)
	}
}

// TestInMemoryImporter_DiamondImportRendersThroughVM pins go-jsonnet's contract
// that one foundAt always yields the same Contents instance. The entry and a
// sibling helper both import the same library, so two distinct (importedFrom,
// importedPath) keys resolve to the one library foundAt — a diamond. Without a
// per-foundAt Contents memo, go-jsonnet panics into "a different instance of
// Contents returned" and the render fails. This is the shape of essentially
// every non-trivial jb-vendored import graph (grafonnet et al).
func TestInMemoryImporter_DiamondImportRendersThroughVM(t *testing.T) {
	self := Library{Files: map[string]string{
		"main.jsonnet":     `local h = import 'helper.libsonnet'; local l = import 'mylib'; { a: l.v, b: h.w }`,
		"helper.libsonnet": `local l = import 'mylib'; { w: l.v + 1 }`,
	}}
	im := &InMemoryImporter{
		Self:      self,
		EntryPath: "main.jsonnet",
		Libraries: map[string]Library{"mylib": {Files: map[string]string{"main.libsonnet": `{ v: 41 }`}}},
	}
	out, err := EvaluateAnonymousSnippet(context.Background(), "main.jsonnet", self.Files["main.jsonnet"], Options{Importer: im})
	if err != nil {
		t.Fatalf("diamond import must render, got: %v", err)
	}
	if !strings.Contains(out, `"a": 41`) || !strings.Contains(out, `"b": 42`) {
		t.Fatalf("diamond render = %s, want a=41 b=42", out)
	}
}

// A bare-alias diamond (two files each importing the library via its bare alias,
// reaching the library's main.libsonnet foundAt) is the same trap by a different
// resolution path.
func TestInMemoryImporter_DiamondViaVendorSearchRendersThroughVM(t *testing.T) {
	self := Library{Files: map[string]string{
		"main.jsonnet": `local a = import 'x/util.libsonnet'; local b = import 'helper.libsonnet'; { p: a.n, q: b.m }`,
		// helper reaches the SAME x/util.libsonnet foundAt via the vendor search.
		"helper.libsonnet": `{ m: (import 'x/util.libsonnet').n * 2 }`,
	}}
	im := &InMemoryImporter{
		Self:      self,
		EntryPath: "main.jsonnet",
		Libraries: map[string]Library{"x": {Files: map[string]string{"util.libsonnet": `{ n: 7 }`}}},
	}
	out, err := EvaluateAnonymousSnippet(context.Background(), "main.jsonnet", self.Files["main.jsonnet"], Options{Importer: im})
	if err != nil {
		t.Fatalf("vendor-search diamond must render, got: %v", err)
	}
	if !strings.Contains(out, `"p": 7`) || !strings.Contains(out, `"q": 14`) {
		t.Fatalf("vendor-search diamond render = %s, want p=7 q=14", out)
	}
}
