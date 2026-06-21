/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package cliflags

import (
	"testing"

	"github.com/spf13/pflag"
)

// TestGroups_MatchFlagAnnotations is a drift gate: every registered flag must
// carry exactly one "group" annotation, and the set of groups actually used
// must equal Groups() exactly. Catches an orphaned group (present in the
// registration map but missing from Groups(), as "MCP" was — which made flaggen
// bucket those flags last) and an un-annotated flag (likewise mis-ordered).
func TestGroups_MatchFlagAnnotations(t *testing.T) {
	fs := pflag.NewFlagSet("jaas", pflag.ContinueOnError)
	Register(fs, func() int { return 0 })

	declared := map[string]bool{}
	for _, g := range Groups() {
		declared[g] = true
	}

	used := map[string]bool{}
	fs.VisitAll(func(f *pflag.Flag) {
		gs := f.Annotations["group"]
		if len(gs) != 1 {
			t.Errorf("flag --%s has group annotation %v, want exactly one group", f.Name, gs)
			return
		}
		used[gs[0]] = true
		if !declared[gs[0]] {
			t.Errorf("flag --%s is in group %q, which is missing from Groups()", f.Name, gs[0])
		}
	})

	for g := range declared {
		if !used[g] {
			t.Errorf("Groups() lists %q but no registered flag is annotated with it", g)
		}
	}
}
