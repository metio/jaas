/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package mcp

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/google/go-jsonnet"
)

// confinedImporter resolves `import` / `importstr` only within the configured
// library roots, using os.OpenRoot so a snippet can never read a file outside
// them — no filesystem-absolute paths, no ".." escape, no working-directory
// relative reads, no symlink escape.
//
// The network MCP transport (the in-operator HTTP server) evaluates ARBITRARY
// caller-supplied snippet source over an unauthenticated port. go-jsonnet's
// stock FileImporter would happily resolve `importstr '/var/run/secrets/...'`
// or a CWD-relative path against the operator pod's filesystem, disclosing the
// ServiceAccount token and any mounted secret. This importer applies the same
// os.OpenRoot confinement the public HTTP snippet handler uses, extended across
// the whole import graph. The stdio renderer stays on the stock importer — it
// is a single-user local tool whose trust boundary is the user's own shell.
//
// Resolution mirrors FileImporter's order: first relative to the importing
// file's directory (within the root it was found in, for transitive relative
// imports), then a search of each root in turn. A root is opened per Import
// call rather than held open, so a timed-out eval's orphan goroutine — which
// keeps running after the parent returns — never reads through a descriptor the
// parent already closed.
type confinedImporter struct {
	roots []string
}

func newConfinedImporter(roots []string) *confinedImporter {
	return &confinedImporter{roots: roots}
}

// Import implements jsonnet.Importer. foundAt is the cleaned root-joined path,
// which is unique per (root, file) so transitive relative imports can locate
// the root their importing file came from.
func (imp *confinedImporter) Import(importedFrom, importedPath string) (jsonnet.Contents, string, error) {
	type candidate struct{ root, rel string }
	var candidates []candidate

	// (1) Relative to the importing file, within the root it resolved in. A
	// foundAt we returned earlier is "<root>/<clean-rel>"; recover both so a
	// sibling import (./util.libsonnet) stays inside the same root.
	if root, dir, ok := imp.locate(importedFrom); ok {
		candidates = append(candidates, candidate{root, path.Join(dir, importedPath)})
	}
	// (2) Each root, searched RIGHTMOST FIRST — go-jsonnet's FileImporter
	// iterates JPaths in reverse, so the rightmost --library-path wins on a
	// collision. The confined importer must agree, or the same flags resolve
	// differently between the HTTP/stdio paths and the confined MCP path.
	for _, root := range slices.Backward(imp.roots) {
		candidates = append(candidates, candidate{root, importedPath})
	}

	for _, c := range candidates {
		data, err := readWithinRoot(c.root, c.rel)
		if err != nil {
			continue
		}
		foundAt := filepath.Join(c.root, filepath.Clean(c.rel))
		return jsonnet.MakeContents(string(data)), foundAt, nil
	}
	return jsonnet.Contents{}, "", fmt.Errorf("import not found within library paths: %q", importedPath)
}

// locate maps a foundAt path we previously returned back to its root and the
// directory of the file within that root, so a transitive relative import can
// be resolved against the same root.
func (imp *confinedImporter) locate(foundAt string) (root, dir string, ok bool) {
	if foundAt == "" {
		return "", "", false
	}
	for _, r := range imp.roots {
		rel, err := filepath.Rel(r, foundAt)
		if err != nil {
			continue
		}
		// Rel returns a "../"-prefixed path when foundAt is outside r.
		if rel == ".." || filepath.IsAbs(rel) || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
			continue
		}
		return r, path.Dir(filepath.ToSlash(rel)), true
	}
	return "", "", false
}

// readWithinRoot opens root with os.OpenRoot and reads rel through it. os.Root
// rejects absolute paths, ".." escape, and symlinks that leave the root, so the
// read is confined to root regardless of what rel contains.
func readWithinRoot(root, rel string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
