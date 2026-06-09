/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"context"
	"strings"
	"testing"
)

// BenchmarkStore_Put writes a 5 KiB payload to a freshly-opened Store
// per-iteration. Captures the steady-state cost of a publish cycle
// (build deterministic tar.gz, hash, write to disk, rename).
func BenchmarkStore_Put(b *testing.B) {
	dir := b.TempDir()
	s, err := New(dir)
	if err != nil {
		b.Fatalf("storage.New: %v", err)
	}
	defer s.Close()
	body := strings.Repeat("a", 5*1024)
	entries := []FileEntry{{Path: "main.json", Content: []byte(body)}}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := s.Put(context.Background(), "bench", "snippet", "rev1", entries); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
}

// BenchmarkStore_PutLargeArtifact pushes a 1 MiB payload through the
// same path. Useful when tuning operator.storage.maxArtifactBytes — the
// cost grows roughly linearly with payload size because the gzip pass
// dominates.
func BenchmarkStore_PutLargeArtifact(b *testing.B) {
	dir := b.TempDir()
	s, err := New(dir)
	if err != nil {
		b.Fatalf("storage.New: %v", err)
	}
	defer s.Close()
	body := strings.Repeat("x", 1024*1024) // 1 MiB
	entries := []FileEntry{{Path: "main.json", Content: []byte(body)}}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := s.Put(context.Background(), "bench", "snippet", "rev-large", entries); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
}
