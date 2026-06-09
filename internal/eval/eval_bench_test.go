/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package eval

import (
	"context"
	"testing"
)

// BenchmarkEvaluateAnonymousSnippet_Tiny captures the per-eval floor —
// a JsonnetSnippet that resolves no imports and produces a literal.
func BenchmarkEvaluateAnonymousSnippet_Tiny(b *testing.B) {
	src := `{ ok: true, n: 42 }`
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := EvaluateAnonymousSnippet(context.Background(), "main.jsonnet", src, Options{}); err != nil {
			b.Fatalf("eval: %v", err)
		}
	}
}

// BenchmarkEvaluateAnonymousSnippet_RecursiveJoin captures the cost
// when go-jsonnet does real work — a 200-element list joined into a
// string. Useful when sizing -evaluation-timeout.
func BenchmarkEvaluateAnonymousSnippet_RecursiveJoin(b *testing.B) {
	src := `
local list(n) = std.makeArray(n, function(i) "item-" + i);
{ result: std.join(",", list(200)) }
`
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := EvaluateAnonymousSnippet(context.Background(), "main.jsonnet", src, Options{}); err != nil {
			b.Fatalf("eval: %v", err)
		}
	}
}
