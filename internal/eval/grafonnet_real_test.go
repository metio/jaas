/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end proof that the operator path renders a REAL jb-vendored library
// (grafonnet) via the InMemoryImporter — keyed by full vendor paths exactly as
// a JsonnetLibrary with path="" (whole artifact) produces, and imported with
// the same statement a developer uses locally. Reads the extracted grafonnet
// gen tree from $GRAFONNET_GEN (a mounted dir); skips when unset.
func TestImporter_RealGrafonnet_RendersDashboard(t *testing.T) {
	root := os.Getenv("GRAFONNET_GEN") // dir containing github.com/grafana/grafonnet/gen
	if root == "" {
		t.Skip("set GRAFONNET_GEN to a dir containing the grafonnet vendor tree")
	}
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p) // keys like github.com/grafana/grafonnet/gen/...
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("load grafonnet: %v", err)
	}
	im := &InMemoryImporter{Libraries: map[string]Library{"grafonnet": {Files: files}}}

	src := `local g = import 'github.com/grafana/grafonnet/gen/grafonnet-latest/main.libsonnet';
	        g.dashboard.new('demo') + g.dashboard.withUid('x')`
	out, err := EvaluateAnonymousSnippet(context.Background(), "ns/snip/main.jsonnet", src, Options{Importer: im})
	if err != nil {
		t.Fatalf("grafonnet render FAILED: %v", err)
	}
	if !strings.Contains(out, `"demo"`) || !strings.Contains(out, `"uid"`) {
		t.Errorf("rendered output missing expected dashboard fields: %s", out)
	}
	t.Logf("grafonnet rendered OK (%d bytes)", len(out))
}
