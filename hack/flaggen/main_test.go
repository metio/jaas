/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spf13/pflag"

	"github.com/metio/jaas/internal/cliflags"
)

// readGenerated runs generate into a temp file and decodes the result.
func readGenerated(t *testing.T) []flagDoc {
	t.Helper()
	out := filepath.Join(t.TempDir(), "flags.json")
	if err := generate(out); err != nil {
		t.Fatalf("generate returned error: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading generated file: %v", err)
	}
	var docs []flagDoc
	if err := json.Unmarshal(body, &docs); err != nil {
		t.Fatalf("generated output is not valid JSON: %v\n%s", err, body)
	}
	return docs
}

func TestGenerate_WritesValidJSONArray(t *testing.T) {
	docs := readGenerated(t)
	if len(docs) == 0 {
		t.Fatal("expected at least one flag doc, got none")
	}
}

func TestGenerate_TrailingNewline(t *testing.T) {
	out := filepath.Join(t.TempDir(), "flags.json")
	if err := generate(out); err != nil {
		t.Fatalf("generate returned error: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading generated file: %v", err)
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Fatalf("expected output to end in a newline, got %q", body)
	}
}

// generate must surface one row per flag the runtime registers, so the docs
// reference can never drift from the CLI contract.
func TestGenerate_CoversEveryRegisteredFlag(t *testing.T) {
	fs := pflag.NewFlagSet("jaas", pflag.ContinueOnError)
	cliflags.Register(fs, func() int { return 0 })

	var registered []string
	fs.VisitAll(func(fl *pflag.Flag) {
		registered = append(registered, fl.Name)
	})

	docs := readGenerated(t)
	generated := make(map[string]flagDoc, len(docs))
	for _, d := range docs {
		generated[d.Name] = d
	}

	if len(generated) != len(docs) {
		t.Fatalf("generated output has duplicate flag names: %d rows, %d unique", len(docs), len(generated))
	}
	if len(registered) != len(docs) {
		t.Fatalf("flag count drift: %d registered, %d generated", len(registered), len(docs))
	}
	for _, name := range registered {
		if _, ok := generated[name]; !ok {
			t.Errorf("registered flag %q is missing from generated output", name)
		}
	}
}

// Each row must carry the metadata the Hugo shortcode renders.
func TestGenerate_RowsAreFullyPopulated(t *testing.T) {
	knownGroups := cliflags.Groups()
	for _, d := range readGenerated(t) {
		if d.Name == "" {
			t.Errorf("flag doc has empty name: %+v", d)
		}
		if d.Type == "" {
			t.Errorf("flag %q has empty type", d.Name)
		}
		if d.Usage == "" {
			t.Errorf("flag %q has empty usage", d.Name)
		}
		if d.Group == "" {
			t.Errorf("flag %q has no group annotation", d.Name)
		} else if !slices.Contains(knownGroups, d.Group) {
			t.Errorf("flag %q has unknown group %q", d.Name, d.Group)
		}
	}
}

// Rows are emitted in documentation-group order; within a group, by name.
func TestGenerate_OrderedByGroupThenName(t *testing.T) {
	order := map[string]int{}
	for i, g := range cliflags.Groups() {
		order[g] = i
	}

	docs := readGenerated(t)
	for i := 1; i < len(docs); i++ {
		prev, cur := docs[i-1], docs[i]
		gp, gc := order[prev.Group], order[cur.Group]
		if gp != gc {
			if gp > gc {
				t.Errorf("group order violated: %q (idx %d) precedes %q (idx %d)",
					prev.Group, gp, cur.Group, gc)
			}
			continue
		}
		if prev.Name > cur.Name {
			t.Errorf("within group %q, %q precedes %q out of name order",
				cur.Group, prev.Name, cur.Name)
		}
	}
}

// The --max-concurrent-evals default is host-derived, so the generator emits a
// symbolic formula rather than the build host's computed number.
func TestGenerate_MaxConcurrentEvalsUsesSymbolicDefault(t *testing.T) {
	for _, d := range readGenerated(t) {
		if d.Name == "max-concurrent-evals" {
			if got, want := d.Default, "max(GOMAXPROCS×4, 16)"; got != want {
				t.Fatalf("max-concurrent-evals default = %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatal("max-concurrent-evals flag not found in generated output")
}

func TestGenerate_ErrorsOnUnwritablePath(t *testing.T) {
	// A path whose parent directory does not exist cannot be written.
	bad := filepath.Join(t.TempDir(), "missing-dir", "flags.json")
	if err := generate(bad); err == nil {
		t.Fatal("expected generate to fail writing to a nonexistent directory")
	}
}

func TestDefaultValue_MaxConcurrentEvalsIsSymbolic(t *testing.T) {
	fs := pflag.NewFlagSet("jaas", pflag.ContinueOnError)
	fs.Int("max-concurrent-evals", 64, "stub usage")
	fl := fs.Lookup("max-concurrent-evals")
	if got, want := defaultValue(fl), "max(GOMAXPROCS×4, 16)"; got != want {
		t.Fatalf("defaultValue = %q, want symbolic %q", got, want)
	}
}

// Every flag other than max-concurrent-evals reports its literal DefValue.
func TestDefaultValue_OtherFlagsUseDefValue(t *testing.T) {
	fs := pflag.NewFlagSet("jaas", pflag.ContinueOnError)
	fs.String("listen-address", "127.0.0.1", "stub")
	fs.Int("max-stack", 500, "stub")
	fs.Bool("enable-mcp", false, "stub")

	cases := []struct {
		flag string
		want string
	}{
		{"listen-address", "127.0.0.1"},
		{"max-stack", "500"},
		{"enable-mcp", "false"},
	}
	for _, c := range cases {
		t.Run(c.flag, func(t *testing.T) {
			fl := fs.Lookup(c.flag)
			if got := defaultValue(fl); got != c.want {
				t.Fatalf("defaultValue(%q) = %q, want %q", c.flag, got, c.want)
			}
		})
	}
}
