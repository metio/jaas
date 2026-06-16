/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Command flaggen introspects the jaas CLI FlagSet and emits a JSON array
// describing every flag (name, type, default, usage, group) for the docs site
// to render. It builds the same FlagSet run() uses via the shared
// registerFlags seam, so the generated reference can never drift from the
// runtime contract.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/pflag"

	"github.com/metio/jaas/internal/cliflags"
)

// flagDoc is one row of the generated flags table. Field order is the column
// order the Hugo shortcode renders.
type flagDoc struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default"`
	Usage   string `json:"usage"`
	Group   string `json:"group"`
}

func main() {
	out := flag.String("o", "docs/data/flags.json", "output path for the generated flags JSON")
	flag.Parse()

	if err := generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, "flaggen:", err)
		os.Exit(1)
	}
}

func generate(outPath string) error {
	fs := pflag.NewFlagSet("jaas", pflag.ContinueOnError)
	// The generator never reads the --max-concurrent-evals value (its default
	// is documented symbolically below), so a zero stub is fine here.
	cliflags.Register(fs, func() int { return 0 })

	// Group → index for stable, documentation-ordered output. Flags whose
	// group is unset or unknown sort to the end under their literal group
	// string, so a missing annotation surfaces rather than silently vanishing.
	order := map[string]int{}
	for i, g := range cliflags.Groups() {
		order[g] = i
	}

	var docs []flagDoc
	fs.VisitAll(func(fl *pflag.Flag) {
		group := ""
		if vals, ok := fl.Annotations["group"]; ok && len(vals) > 0 {
			group = vals[0]
		}
		docs = append(docs, flagDoc{
			Name:    fl.Name,
			Type:    fl.Value.Type(),
			Default: defaultValue(fl),
			Usage:   fl.Usage,
			Group:   group,
		})
	})

	sort.SliceStable(docs, func(i, j int) bool {
		gi, iok := order[docs[i].Group]
		gj, jok := order[docs[j].Group]
		if iok != jok {
			// Known groups sort before unknown ones.
			return iok
		}
		if iok && gi != gj {
			return gi < gj
		}
		if docs[i].Group != docs[j].Group {
			return docs[i].Group < docs[j].Group
		}
		return docs[i].Name < docs[j].Name
	})

	body, err := json.MarshalIndent(docs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	body = append(body, '\n')
	// #nosec G306 -- generated documentation data, not a secret.
	if err := os.WriteFile(outPath, body, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", outPath, err)
	}
	return nil
}

// defaultValue renders the documented default for a flag. The
// --max-concurrent-evals default is computed from the build host's GOMAXPROCS,
// which would bake a machine-specific number into the docs; emit the symbolic
// formula instead so the published reference stays host-independent.
func defaultValue(fl *pflag.Flag) string {
	if fl.Name == "max-concurrent-evals" {
		return "max(GOMAXPROCS×4, 16)"
	}
	return fl.DefValue
}
