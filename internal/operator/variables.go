/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"maps"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// Binding a snippet's variables means answering two questions per entry: which
// go-jsonnet namespace it lands in (std.extVar vs. a top-level argument) and
// whether its value is a string or Jsonnet source to parse. spec.tlas and
// spec.externalVariables answer the first by which field they are; the entry's
// code flag answers the second. The helpers here translate that CR shape into
// the four flat maps eval.Options takes.

// mergeExtVars layers the snippet's CR-level vars over the operator's set.
// checkExtVarConflicts has already rejected overlapping keys, so the result
// is just the union.
func mergeExtVars(opLevel, snipLevel map[string]string) map[string]string {
	if len(opLevel) == 0 && len(snipLevel) == 0 {
		return nil
	}
	out := make(map[string]string, len(opLevel)+len(snipLevel))
	maps.Copy(out, opLevel)
	maps.Copy(out, snipLevel)
	return out
}

// splitVariables partitions a spec.tlas / spec.externalVariables list into the
// string-bound and code-bound maps go-jsonnet takes. The two maps are disjoint
// because the CRD's listMapKey makes the name unique across the list.
//
// Both maps are nil when no entry of that kind exists, so a snippet binding
// only strings passes no code map to eval at all.
func splitVariables(vars []jaasv1.JsonnetVariable) (str, code map[string]string) {
	for _, v := range vars {
		if v.Code {
			if code == nil {
				code = make(map[string]string, len(vars))
			}
			code[v.Name] = v.Value
			continue
		}
		if str == nil {
			str = make(map[string]string, len(vars))
		}
		str[v.Name] = v.Value
	}
	return str, code
}

// tlaValues adapts the string-bound TLAs to eval.Options.TLAs, whose
// multi-value shape carries the HTTP path's query-param convention. One
// element per name is the single-value case, which binds via vm.TLAVar —
// the same binding `jsonnet --tla-str` produces.
func tlaValues(str map[string]string) map[string][]string {
	if len(str) == 0 {
		return nil
	}
	out := make(map[string][]string, len(str))
	for k, v := range str {
		out[k] = []string{v}
	}
	return out
}
