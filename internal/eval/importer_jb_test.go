/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"context"
	"strings"
	"testing"
)

// These tests pin the jsonnet-bundler (jb) semantics the operator path needs:
// a library file resolves its own siblings, relative parents, and absolute
// `github.com/...` vendor imports exactly as `jsonnet -J vendor` would — so the
// same import statements work locally and in-cluster.

func eval1(t *testing.T, im *InMemoryImporter, src string) (string, error) {
	t.Helper()
	return EvaluateAnonymousSnippet(context.Background(), "ns/snip/main.jsonnet", src, Options{Importer: im})
}

func TestImporter_LibrarySiblingResolvesRelativeToImportingFile(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"lib": {Files: map[string]string{
				"main.libsonnet":   "{ v: (import 'helper.libsonnet').v }", // bare sibling
				"helper.libsonnet": "{ v: 42 }",
			}},
		},
	}
	out, err := eval1(t, im, "(import 'lib/main.libsonnet').v")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Errorf("got %q, want 42", strings.TrimSpace(out))
	}
}

func TestImporter_LibraryRelativeParentImport(t *testing.T) {
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"lib": {Files: map[string]string{
				"a/b/leaf.libsonnet": "{ v: (import '../shared.libsonnet').v }",
				"a/shared.libsonnet": "{ v: 7 }",
			}},
		},
	}
	out, err := eval1(t, im, "(import 'lib/a/b/leaf.libsonnet').v")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "7" {
		t.Errorf("got %q, want 7", strings.TrimSpace(out))
	}
}

// The grafonnet shape: a `-latest` alias file that re-imports the versioned
// tree by its full absolute vendor path, whose files in turn use bare-sibling
// imports. Mirrors path="" (whole-artifact) extraction.
func TestImporter_AbsoluteVendorImport_GrafonnetShape(t *testing.T) {
	const base = "github.com/acme/lib/gen"
	im := &InMemoryImporter{
		Libraries: map[string]Library{
			"acme": {Files: map[string]string{
				base + "/lib-latest/main.libsonnet": "import '" + base + "/lib-v1/main.libsonnet'",
				base + "/lib-v1/main.libsonnet":     "{ panel: import 'panel.libsonnet' }",
				base + "/lib-v1/panel.libsonnet":    "{ kind: 'panel' }",
			}},
		},
	}
	// Imported by the full jb vendor path — identical to local `jsonnet -J vendor`.
	out, err := eval1(t, im, "(import '"+base+"/lib-latest/main.libsonnet').panel.kind")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != `"panel"` {
		t.Errorf("got %q, want \"panel\"", strings.TrimSpace(out))
	}
}

// The snippet's own inline files still resolve as siblings (project root).
func TestImporter_SnippetSiblingStillResolves(t *testing.T) {
	im := &InMemoryImporter{
		Self: Library{Files: map[string]string{"helper.libsonnet": "{ v: 1 }"}},
	}
	out, err := eval1(t, im, "(import 'helper.libsonnet').v")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "1" {
		t.Errorf("got %q, want 1", strings.TrimSpace(out))
	}
}
