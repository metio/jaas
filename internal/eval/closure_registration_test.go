// SPDX-FileCopyrightText: The jaas Authors
// SPDX-License-Identifier: 0BSD

package eval

import (
	"context"
	"strings"
	"testing"
)

// The importer resolves cross-library imports only across the libraries
// REGISTERED for the snippet — there is no automatic dependency-closure
// resolution. This pins the contract the JOI split-image convention (and the
// grafonnet documentation) is built on: a library that vendor-imports another
// library's tree renders only when the snippet registers BOTH; registering
// just the importing library fails with an unresolved import.
func TestClosureLibrariesMustBeRegisteredPerSnippet(t *testing.T) {
	// "grafonnet" stands in for any library whose files import another
	// library's jb-vendor path; "xtd" is its dependency, shipped separately.
	// Like the JOI images (JsonnetLibrary with an empty sourceRef.path), each
	// library carries its whole vendor subtree keyed by full paths.
	grafonnet := Library{Files: map[string]string{
		"github.com/grafana/grafonnet/main.libsonnet": `local xtd = import 'github.com/jsonnet-libs/xtd/main.libsonnet'; { grid: xtd.helper }`,
	}}
	xtd := Library{Files: map[string]string{
		"github.com/jsonnet-libs/xtd/main.libsonnet": `{ helper: "resolved" }`,
	}}
	source := `local g = import 'github.com/grafana/grafonnet/main.libsonnet'; { out: g.grid }`
	register := func(libs map[string]Library) (string, error) {
		return EvaluateAnonymousSnippet(context.Background(), "closure-test", source, Options{
			Importer: &InMemoryImporter{
				Self:      Library{Files: map[string]string{"main.jsonnet": source}},
				Libraries: libs,
			},
		})
	}

	t.Run("importing library alone cannot resolve its dependency", func(t *testing.T) {
		_, err := register(map[string]Library{
			"grafonnet": grafonnet,
		})
		if err == nil {
			t.Fatal("a dependency that is not registered must fail the import, not resolve implicitly")
		}
		if !strings.Contains(err.Error(), "xtd") {
			t.Fatalf("error should name the unresolved dependency import, got: %v", err)
		}
	})

	t.Run("registering the closure resolves the vendor import", func(t *testing.T) {
		out, err := register(map[string]Library{
			"grafonnet": grafonnet,
			"xtd":       xtd,
		})
		if err != nil {
			t.Fatalf("registered closure should render: %v", err)
		}
		if !strings.Contains(out, "resolved") {
			t.Fatalf("rendered output should carry the dependency's value, got: %s", out)
		}
	})
}
