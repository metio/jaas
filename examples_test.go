/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// updateGolden flips the golden-test framework into "write" mode. Run as
// `go test -update ./...` after changing an example file; commit the diff
// under testdata/golden/.
var updateGolden = flag.Bool("update", false, "regenerate golden files in testdata/golden/ instead of comparing")

type goldenCase struct {
	name    string     // doubles as the golden filename under testdata/golden/
	args    []string   // extra flags for run (snippet/library/etc.)
	env     []string   // process env passed to run; nil means no ExtVars
	urlPath string     // the path after /jsonnet/
	query   url.Values // optional query parameters (URL-encoded by net/url)
}

// runGoldenCase boots jaas via runInBackground, fetches the configured URL,
// and either compares the canonicalized response against testdata/golden/<name>.json
// or rewrites that file when -update is set.
func runGoldenCase(t *testing.T, tc goldenCase) {
	t.Helper()
	h := runInBackground(t, tc.args, tc.env)
	defer h.shutdown(t, 0)

	u := "http://" + h.jsonnet + "/jsonnet/" + tc.urlPath
	if len(tc.query) > 0 {
		u += "?" + tc.query.Encode()
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s; stderr=%s", resp.StatusCode, body, h.stderr.String())
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	var gotV any
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("response is not valid JSON: %v; body=%q", err, got)
	}
	gotCanon, err := json.MarshalIndent(gotV, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gotCanon = append(gotCanon, '\n')

	goldenPath := filepath.Join("testdata", "golden", tc.name+".json")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, gotCanon, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v\n(run `go test -update ./...` to create it)", goldenPath, err)
	}
	var wantV any
	if err := json.Unmarshal(want, &wantV); err != nil {
		t.Fatalf("golden %s is not valid JSON: %v", goldenPath, err)
	}
	if !reflect.DeepEqual(gotV, wantV) {
		wantCanon, _ := json.MarshalIndent(wantV, "", "  ")
		t.Errorf("response does not match golden %s.\n--- got ---\n%s\n--- want ---\n%s\n(run `go test -update ./...` to regenerate)",
			goldenPath, gotCanon, wantCanon)
	}
}

// ---- example fixtures ------------------------------------------------------

func TestExamples_FileSnippetWithLibraryImport(t *testing.T) {
	// `examples/snippets/example.jsonnet` is the file-mode snippet — accessed
	// via the same path you passed on the CLI.
	runGoldenCase(t, goldenCase{
		name: "example_jsonnet",
		args: []string{
			"-snippet=examples/snippets/example.jsonnet",
			"-library-path=examples/libraries",
		},
		urlPath: "examples/snippets/example.jsonnet",
	})
}

func TestExamples_DirectorySnippetWithExtVarsAndLibraryImport(t *testing.T) {
	// `examples/snippets/dashboards/example1/main.jsonnet` reads two ExtVars
	// (`name`, `key`) and imports the `examplonet` library.
	runGoldenCase(t, goldenCase{
		name: "dashboards_example1",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
			"-library-path=examples/libraries",
		},
		env: []string{
			"JAAS_EXT_VAR_name=Alice",
			"JAAS_EXT_VAR_key=secret",
		},
		urlPath: "example1",
	})
}

func TestExamples_DirectorySnippetWithTLAs(t *testing.T) {
	// `examples/snippets/dashboards/tla-example/main.jsonnet` is a function-
	// shaped snippet: `something` and `other` have defaults, `required` doesn't
	// and is parsed back from JSON via std.parseJson.
	runGoldenCase(t, goldenCase{
		name: "dashboards_tla_example",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
			"-library-path=examples/libraries",
		},
		urlPath: "tla-example",
		query: url.Values{
			"required":  {`{"hello":"world"}`},
			"something": {"there"},
		},
	})
}

func TestExamples_DirectorySnippetWithTLAs_UsesDefaults(t *testing.T) {
	// Only the `required` TLA is provided; `something` and `other` fall back
	// to their default values declared in the function signature.
	runGoldenCase(t, goldenCase{
		name: "dashboards_tla_example_defaults",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
			"-library-path=examples/libraries",
		},
		urlPath: "tla-example",
		query: url.Values{
			"required": {`42`},
		},
	})
}

// ---- recursion-depth probe -------------------------------------------------

func TestExamples_RecursionDepth_GoldenAtSafeDepth(t *testing.T) {
	runGoldenCase(t, goldenCase{
		name: "dashboards_recursion_depth_50",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
		},
		urlPath: "recursion-depth",
		query:   url.Values{"depth": {"50"}},
	})
}

func TestExamples_RecursionDepth_FindsLimitAtDefaultMaxStack(t *testing.T) {
	// Empirically discovers the deepest recursion the `recursion-depth`
	// snippet can complete with our default MaxStack=500. Logs the answer
	// to the test output so a `go test -v -run RecursionDepth_Finds` invocation
	// prints the boundary. Sanity-asserts a generous range so the test fails
	// only if go-jsonnet's stack accounting shifts dramatically.
	h := runInBackground(t, []string{
		"-snippet-directory=examples/snippets/dashboards",
	}, nil)
	defer h.shutdown(t, 0)

	lo, hi := 1, 5000
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if recursionDepthOK(t, h, mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	t.Logf("at -max-stack=500 the recursion-depth snippet succeeds up to depth=%d", lo)

	if lo < 50 {
		t.Errorf("limit %d is suspiciously low; investigate", lo)
	}
	if lo > 2000 {
		t.Errorf("limit %d is suspiciously high; investigate", lo)
	}
}

func TestExamples_RecursionDepth_HighDepthOverflowsStack(t *testing.T) {
	h := runInBackground(t, []string{
		"-snippet-directory=examples/snippets/dashboards",
	}, nil)
	defer h.shutdown(t, 0)

	u := fmt.Sprintf("http://%s/jsonnet/recursion-depth?depth=10000", h.jsonnet)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400 (stack overflow); body=%s", resp.StatusCode, body)
	}
}

func TestExamples_RecursionDepth_MaxStackFlagShrinksLimit(t *testing.T) {
	// Operator-visible knob: setting -max-stack=100 makes the snippet fail at
	// a depth that succeeds with the default -max-stack=500.
	h := runInBackground(t, []string{
		"-snippet-directory=examples/snippets/dashboards",
		"-max-stack=100",
	}, nil)
	defer h.shutdown(t, 0)

	if !recursionDepthOK(t, h, 10) {
		t.Error("depth=10 should still succeed at -max-stack=100")
	}
	if recursionDepthOK(t, h, 500) {
		t.Error("depth=500 should overflow at -max-stack=100 (default would have allowed it)")
	}
}

func recursionDepthOK(t *testing.T, h *runHandle, depth int) bool {
	t.Helper()
	u := fmt.Sprintf("http://%s/jsonnet/recursion-depth?depth=%d", h.jsonnet, depth)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ---- additional feature combinations --------------------------------------

func TestExamples_MultiValueTLA(t *testing.T) {
	// `?tags=red&tags=blue&tags=green` exercises the multi-value TLA branch in
	// applyTLAVars (JSON-array via TLACode). The snippet uses std.join and
	// std.length on the resulting array.
	runGoldenCase(t, goldenCase{
		name: "dashboards_multi_tla",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
		},
		urlPath: "multi-tla",
		query: url.Values{
			"tags": {"red", "blue", "green"},
		},
	})
}

func TestExamples_MultiValueTLA_FallsBackToDefault(t *testing.T) {
	// No `?tags` → the function's default `["default"]` is used. Tests that
	// jsonnet TLA defaults survive when the URL doesn't supply the key.
	runGoldenCase(t, goldenCase{
		name: "dashboards_multi_tla_default",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
		},
		urlPath: "multi-tla",
	})
}

func TestExamples_ImportStrLoadsLiteralFile(t *testing.T) {
	// `importstr` pulls in a non-jsonnet file (text/welcome.txt) as a string.
	// Useful for embedding configs, banners, certs etc. into a manifest.
	runGoldenCase(t, goldenCase{
		name: "dashboards_embed_text",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
			"-library-path=examples/libraries",
		},
		urlPath: "embed-text",
	})
}

func TestExamples_ObjectInheritanceAndHiddenFields(t *testing.T) {
	// `+`, `+:`, and `::` (hidden) fields, plus dynamic `self` recomputation.
	// The expected output excludes both hidden fields (`meta`, `count`) and
	// shows the appended `tags+: [...]` merging with the base's `tags`.
	runGoldenCase(t, goldenCase{
		name: "dashboards_inheritance",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
		},
		urlPath: "inheritance",
	})
}

func TestExamples_TransitiveLibraryImports(t *testing.T) {
	// `greeter/main.libsonnet` imports `utils/main.libsonnet`, then the
	// snippet imports `greeter`. Pins the multi-hop library resolution path.
	runGoldenCase(t, goldenCase{
		name: "dashboards_transitive_imports",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
			"-library-path=examples/libraries",
		},
		urlPath: "transitive-imports",
	})
}

func TestExamples_KubernetesManifest(t *testing.T) {
	// Realistic shape: a Deployment manifest assembled from four TLAs. This
	// is the closest existing test to "what an actual JaaS user would use it
	// for" — a parameterised k8s/grafana-style YAML/JSON resource builder.
	runGoldenCase(t, goldenCase{
		name: "dashboards_k8s_manifest",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
		},
		urlPath: "k8s-manifest",
		query: url.Values{
			"name":      {"myapp"},
			"namespace": {"staging"},
			"replicas":  {"3"},
			"image":     {"myorg/myapp:v1.2.3"},
		},
	})
}

// ---- library-path precedence ----------------------------------------------
//
// Both these tests share a snippet that imports `examplonet/main.libsonnet`.
// `examples/libraries/examplonet/main.libsonnet` defines `standard = "value"`.
// `examples/libraries-overrides/examplonet/main.libsonnet` redefines it as
// `"overridden-value"`. The two tests differ only in which `-library-path`
// flags are passed.

func TestExamples_LibraryPrecedence_SinglePath(t *testing.T) {
	runGoldenCase(t, goldenCase{
		name: "dashboards_library_precedence_default",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
			"-library-path=examples/libraries",
		},
		urlPath: "library-precedence",
	})
}

func TestExamples_LibraryPrecedence_WithOverride(t *testing.T) {
	// Two -library-path entries, both containing an `examplonet` library.
	// The README claims "rightmost matching library will be used" — this test
	// pins the actual behaviour. If go-jsonnet's FileImporter resolves to a
	// different match than the README states, the regenerated golden makes
	// the discrepancy obvious.
	runGoldenCase(t, goldenCase{
		name: "dashboards_library_precedence_override",
		args: []string{
			"-snippet-directory=examples/snippets/dashboards",
			"-library-path=examples/libraries",
			"-library-path=examples/libraries-overrides",
		},
		urlPath: "library-precedence",
	})
}
