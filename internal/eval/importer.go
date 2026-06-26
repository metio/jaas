/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-jsonnet"
)

// Library is the bytes a single registered library exposes — its file path
// (within the library) → contents. The map is read-only after Importer
// construction; concurrent reads are safe. For a jsonnet-bundler (jb) vendored
// library the keys are the full vendor paths (e.g.
// "github.com/grafana/grafonnet/gen/grafonnet-latest/main.libsonnet").
type Library struct {
	Files map[string]string
}

// InMemoryImporter resolves imports against the snippet's own files (Self) and
// a registry of libraries, with the same semantics a real `jsonnet -J vendor`
// filesystem gives — so jb-vendored libraries (grafonnet, docsonnet, …) render
// identically whether evaluated locally against a mounted vendor tree or
// in-cluster against fetched JsonnetLibrary content.
//
// Resolution order for `import "P"` from a file at foundAt F (mirroring
// go-jsonnet's FileImporter: relative-to-importer first, then the search path):
//
//  1. Relative to F's directory, within F's own root (the snippet or one
//     library). Handles sibling imports (`import 'dashboard.libsonnet'`) and
//     explicit-relative ones (`./x`, `../x`).
//  2. A bare name (no slash) that is a registered library alias loads that
//     library's "main.libsonnet". A sibling of the same bare name (step 1)
//     wins over this.
//  3. An "alias/file" path where the head is a registered library alias loads
//     that file from the library (the explicit-alias convention). A missing
//     file under a registered alias is a hard miss.
//  4. JPATH / vendor search: P is treated as a root-relative path and looked up
//     across Self then every library (sorted for determinism). Handles
//     absolute jb imports like `github.com/grafana/grafonnet/gen/...` against a
//     library whose Files are keyed by full vendor path.
//
// A slash-prefixed path whose head is not a registered alias is NOT an error on
// its own — it falls through to the vendor search, which is what lets
// `github.com/...` imports resolve.
type InMemoryImporter struct {
	// Self holds the snippet's own files (its entry's siblings), the
	// equivalent of a jb project root on the search path.
	Self Library

	// Libraries maps alias → Library.
	Libraries map[string]Library

	// cache satisfies go-jsonnet's "same (contents, foundAt) for the same
	// (importedFrom, importedPath) every call" expectation. Keyed on the pair
	// because relative resolution makes the same importedPath resolve
	// differently depending on the importing file.
	cache sync.Map // map[string]cachedImport, key = importedFrom + "\x00" + importedPath

	// locs records where each returned foundAt lives, so imports from within
	// that file resolve relative to its directory and root.
	locs sync.Map // map[string]location

	aliasesOnce sync.Once
	aliases     []string
}

// location identifies a resolved file's root (alias, "" for Self) and its path
// within that root.
type location struct {
	alias      string
	pathWithin string
}

// cachedImport pairs resolved contents with the canonical foundAt so repeated
// imports of the same (importedFrom, importedPath) return both unchanged.
type cachedImport struct {
	contents jsonnet.Contents
	foundAt  string
}

const defaultLibraryEntryFile = "main.libsonnet"

// Import implements jsonnet.Importer.
func (im *InMemoryImporter) Import(importedFrom, importedPath string) (jsonnet.Contents, string, error) {
	key := importedFrom + "\x00" + importedPath
	if v, ok := im.cache.Load(key); ok {
		c := v.(cachedImport)
		return c.contents, c.foundAt, nil
	}

	body, foundAt, err := im.resolve(importedFrom, importedPath)
	if err != nil {
		return jsonnet.Contents{}, "", err
	}
	contents := jsonnet.MakeContents(body)
	im.cache.Store(key, cachedImport{contents: contents, foundAt: foundAt})
	return contents, foundAt, nil
}

func (im *InMemoryImporter) resolve(importedFrom, importedPath string) (string, string, error) {
	if importedPath == "" {
		return "", "", fmt.Errorf("import path is empty")
	}

	src := im.locationOf(importedFrom)

	// 1. Relative to the importing file's directory, within its own root.
	base := path.Dir(src.pathWithin)
	rel := path.Clean(path.Join(base, importedPath))
	if rel != ".." && !strings.HasPrefix(rel, "../") {
		if body, ok := im.fileIn(src.alias, rel); ok {
			return body, im.record(src.alias, rel), nil
		}
	}

	if i := strings.IndexByte(importedPath, '/'); i < 0 {
		// 2. Bare name: a registered library's default entry.
		if lib, ok := im.Libraries[importedPath]; ok {
			body, ok := lib.Files[defaultLibraryEntryFile]
			if !ok {
				return "", "", fmt.Errorf("library %q has no %s", importedPath, defaultLibraryEntryFile)
			}
			return body, im.record(importedPath, defaultLibraryEntryFile), nil
		}
	} else {
		// 3. Explicit alias/file: authoritative when the head is a registered alias.
		alias := importedPath[:i]
		fileWithin := path.Clean(importedPath[i+1:])
		if lib, ok := im.Libraries[alias]; ok {
			body, ok := lib.Files[fileWithin]
			if !ok {
				return "", "", fmt.Errorf("file %q not found in library %q", fileWithin, alias)
			}
			return body, im.record(alias, fileWithin), nil
		}
	}

	// 4. JPATH / vendor search across Self then every library.
	clean := path.Clean(importedPath)
	if body, ok := im.Self.Files[clean]; ok {
		return body, im.record("", clean), nil
	}
	for _, alias := range im.sortedAliases() {
		if body, ok := im.Libraries[alias].Files[clean]; ok {
			return body, im.record(alias, clean), nil
		}
	}

	return "", "", fmt.Errorf("import %q matches no sibling file or library alias", importedPath)
}

// fileIn looks a path up in a given root ("" = Self).
func (im *InMemoryImporter) fileIn(alias, p string) (string, bool) {
	if alias == "" {
		body, ok := im.Self.Files[p]
		return body, ok
	}
	body, ok := im.Libraries[alias].Files[p]
	return body, ok
}

// record returns the canonical foundAt for a resolved location and remembers it
// so transitive imports resolve relative to it.
//
// A library file's foundAt is "alias/pathWithin", which can equal a Self file's
// path when the snippet has a subtree named like a library alias (e.g. Self
// holds "grafonnet/x.libsonnet" and a library "grafonnet" exposes "x.libsonnet").
// Both would then map to the same foundAt string, and since foundAt is both the
// locs key and the import-cache's importedFrom, their transitive imports would
// cross-wire — a relative import from one file would resolve against the other's
// root, or hit the other's cached contents. To keep the two disjoint, a library
// file shadowed by a Self path of the same name is tagged with a leading NUL,
// which no Self file key can contain. Self keeps the clean foundAt; only the
// shadowed library file is tagged, so every non-colliding foundAt is unchanged.
func (im *InMemoryImporter) record(alias, pathWithin string) string {
	foundAt := pathWithin
	if alias != "" {
		foundAt = alias + "/" + pathWithin
		if _, shadowed := im.Self.Files[foundAt]; shadowed {
			foundAt = "\x00" + foundAt
		}
	}
	im.locs.Store(foundAt, location{alias: alias, pathWithin: pathWithin})
	return foundAt
}

// locationOf maps an importedFrom (a previously returned foundAt) back to its
// root + path. An unrecorded value — the top-level entry's diagnostic label —
// is treated as the snippet root, so the entry's imports resolve against Self
// and the vendor search.
func (im *InMemoryImporter) locationOf(importedFrom string) location {
	if v, ok := im.locs.Load(importedFrom); ok {
		return v.(location)
	}
	return location{}
}

func (im *InMemoryImporter) sortedAliases() []string {
	im.aliasesOnce.Do(func() {
		im.aliases = make([]string, 0, len(im.Libraries))
		for a := range im.Libraries {
			im.aliases = append(im.aliases, a)
		}
		sort.Strings(im.aliases)
	})
	return im.aliases
}
