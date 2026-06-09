/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package storage

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestSelectPruneVictims unit-tests the keep-set + grace decision both
// storage backends now share, including the equal-mtime case that a
// strictly-newer-only proxy would pin forever.
func TestSelectPruneVictims(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	cand := func(rev string, ageMin int) pruneCandidate {
		return pruneCandidate{keepKey: rev + ".tar.gz", removeKey: "path/" + rev, mtime: now.Add(time.Duration(-ageMin) * time.Minute)}
	}
	keep := func(revs ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, r := range revs {
			m[r+".tar.gz"] = struct{}{}
		}
		return m
	}

	cases := []struct {
		name  string
		cands []pruneCandidate
		keep  map[string]struct{}
		grace time.Duration
		want  []string
	}{
		{"grace=0 removes everything not kept", []pruneCandidate{cand("a", 10), cand("b", 1)}, keep("b"), 0, []string{"path/a"}},
		{"superseded within grace is retained", []pruneCandidate{cand("a", 10), cand("b", 1)}, keep("b"), 5 * time.Minute, nil},
		{"superseded past grace is removed", []pruneCandidate{cand("a", 20), cand("b", 10)}, keep("b"), 5 * time.Minute, []string{"path/a"}},
		{"equal-mtime orphan past grace is reclaimed", []pruneCandidate{cand("a", 10), cand("b", 10)}, keep("b"), 5 * time.Minute, []string{"path/a"}},
		{"newest-but-not-kept past grace is removed", []pruneCandidate{cand("a", 10)}, keep("b"), 5 * time.Minute, []string{"path/a"}},
		{"kept revisions are never victims", []pruneCandidate{cand("a", 10), cand("b", 1)}, keep("a", "b"), 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectPruneVictims(tc.cands, tc.keep, now, tc.grace)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// touchMtime sets the on-disk mtime of <root>/<rel> to t. The grace
// tests use real mtimes against an injected clock so the supersession
// proxy (earliest-newer-mtime) runs the production code path without
// time.Sleep.
func touchMtime(t *testing.T, root, rel string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(root, rel), when, when); err != nil {
		t.Fatalf("chtimes %s: %v", rel, err)
	}
}

func TestPrune_GraceZeroIsEager(t *testing.T) {
	s := newTestStore(t)
	for _, rev := range []string{"a1", "a2"} {
		if _, err := s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	// Even with the candidate's mtime "right now" and no real wall-
	// clock delay, grace=0 must prune unconditionally — the historical
	// behavior the flag's zero value restores.
	if err := s.Prune(context.Background(), "ns", "n", []string{"a2"}, 0); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "a1.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("a1 should have been pruned eagerly under grace=0")
	}
}

func TestPrune_GraceRetainsSupersededWithinWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	for _, rev := range []string{"a1", "a2"} {
		if _, err := s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	// a1 is the older revision; a2 displaces it. Set a1's mtime to
	// 10 minutes ago and a2's to 1 minute ago. With grace=5m, "now -
	// supersessionTime(a1) = now - a2.mtime = 1m" is below grace, so
	// a1 stays.
	touchMtime(t, s.fs.Name(), "ns/n/a1.tar.gz", now.Add(-10*time.Minute))
	touchMtime(t, s.fs.Name(), "ns/n/a2.tar.gz", now.Add(-1*time.Minute))

	if err := s.Prune(context.Background(), "ns", "n", []string{"a2"}, 5*time.Minute); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "a1.tar.gz")); err != nil {
		t.Errorf("a1 should still be present within grace window: %v", err)
	}
}

// When the superseded revision and its successor share an mtime (coarse
// filesystem granularity, or two Puts in the same tick), the supersession
// proxy finds no strictly-newer mtime. The non-keep revision must still
// be reclaimed once grace elapses from its own mtime — not pinned forever.
func TestPrune_GraceReclaimsEqualMtimeOrphanPastWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	for _, rev := range []string{"a1", "a2"} {
		if _, err := s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	// Identical mtimes, both well past the grace window.
	same := now.Add(-10 * time.Minute)
	touchMtime(t, s.fs.Name(), "ns/n/a1.tar.gz", same)
	touchMtime(t, s.fs.Name(), "ns/n/a2.tar.gz", same)

	if err := s.Prune(context.Background(), "ns", "n", []string{"a2"}, 5*time.Minute); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "a1.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("equal-mtime orphan a1 should have been pruned past grace, not pinned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "a2.tar.gz")); err != nil {
		t.Errorf("keep-set member a2 must survive: %v", err)
	}
}

func TestPrune_GraceDeletesSupersededPastWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	for _, rev := range []string{"a1", "a2"} {
		if _, err := s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	// a2 was written 10 minutes ago — that's when a1 was superseded.
	// With grace=5m, the window has expired.
	touchMtime(t, s.fs.Name(), "ns/n/a1.tar.gz", now.Add(-20*time.Minute))
	touchMtime(t, s.fs.Name(), "ns/n/a2.tar.gz", now.Add(-10*time.Minute))

	if err := s.Prune(context.Background(), "ns", "n", []string{"a2"}, 5*time.Minute); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "a1.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("a1 should have been pruned past grace: %v", err)
	}
}

func TestPrune_GraceSurvivesRestart(t *testing.T) {
	// Design (a): supersession time comes from on-disk mtime, not
	// in-memory bookkeeping. Closing and reopening the Store across the
	// grace window must keep the grace decision consistent.
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	for _, rev := range []string{"a1", "a2"} {
		if _, err := s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	touchMtime(t, root, "ns/n/a1.tar.gz", now.Add(-10*time.Minute))
	touchMtime(t, root, "ns/n/a2.tar.gz", now.Add(-2*time.Minute))
	_ = s.Close()

	// Restart: reopen against the same root with a fresh now.
	s2, err := New(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	// Advance clock so the grace window has expired since a2 was
	// written (a2.mtime + 5m = -2m + 5m = +3m vs now+5m → expired).
	s2.now = func() time.Time { return now.Add(5 * time.Minute) }

	if err := s2.Prune(context.Background(), "ns", "n", []string{"a2"}, 5*time.Minute); err != nil {
		t.Fatalf("Prune after restart: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ns", "n", "a1.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("a1 should have been pruned past grace after restart: %v", err)
	}
}

func TestPrune_GraceDoesNotAffectKeepSetMembers(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	for _, rev := range []string{"a1", "a2", "a3"} {
		if _, err := s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
			t.Fatalf("Put %s: %v", rev, err)
		}
	}
	// All three written long enough ago that grace has elapsed — yet
	// a2 + a3 are in the keep-set, so they must stay regardless.
	touchMtime(t, s.fs.Name(), "ns/n/a1.tar.gz", now.Add(-60*time.Minute))
	touchMtime(t, s.fs.Name(), "ns/n/a2.tar.gz", now.Add(-40*time.Minute))
	touchMtime(t, s.fs.Name(), "ns/n/a3.tar.gz", now.Add(-20*time.Minute))

	if err := s.Prune(context.Background(), "ns", "n", []string{"a2", "a3"}, 5*time.Minute); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, kept := range []string{"a2", "a3"} {
		if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", kept+".tar.gz")); err != nil {
			t.Errorf("%s should have survived: %v", kept, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", "a1.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("a1 should have been pruned past grace: %v", err)
	}
}

// TestPrune_KeepSetAndGraceProperty drives randomized keep-sets +
// mtimes through Prune and asserts the invariant: a non-keep file
// survives iff (now - earliest-newer-mtime) < grace. Mirrors the
// shape of history_property_test.go.
func TestPrune_KeepSetAndGraceProperty(t *testing.T) {
	cases := []struct {
		name        string
		mtimeOffset map[string]time.Duration // -X minutes from "now"
		keep        []string
		grace       time.Duration
		want        map[string]bool // expected presence after Prune
	}{
		{
			name: "single supersession, within grace",
			mtimeOffset: map[string]time.Duration{
				"a1": -10 * time.Minute,
				"a2": -1 * time.Minute,
			},
			keep:  []string{"a2"},
			grace: 5 * time.Minute,
			want:  map[string]bool{"a1": true, "a2": true},
		},
		{
			name: "single supersession, past grace",
			mtimeOffset: map[string]time.Duration{
				"a1": -20 * time.Minute,
				"a2": -10 * time.Minute,
			},
			keep:  []string{"a2"},
			grace: 5 * time.Minute,
			want:  map[string]bool{"a1": false, "a2": true},
		},
		{
			name: "chained eviction; oldest past grace, middle within grace",
			mtimeOffset: map[string]time.Duration{
				"a1": -30 * time.Minute,
				"a2": -25 * time.Minute,
				"a3": -1 * time.Minute,
			},
			keep:  []string{"a3"},
			grace: 5 * time.Minute,
			// a1's supersession proxy = a2.mtime (-25m) → past grace.
			// a2's supersession proxy = a3.mtime (-1m) → within grace.
			want: map[string]bool{"a1": false, "a2": true, "a3": true},
		},
		{
			name: "grace zero prunes both non-keep entries",
			mtimeOffset: map[string]time.Duration{
				"a1": -10 * time.Minute,
				"a2": -5 * time.Minute,
				"a3": -1 * time.Minute,
			},
			keep:  []string{"a3"},
			grace: 0,
			want:  map[string]bool{"a1": false, "a2": false, "a3": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return now }

			for rev := range tc.mtimeOffset {
				if _, err := s.Put(context.Background(), "ns", "n", rev, []FileEntry{{Path: "f", Content: []byte(rev)}}); err != nil {
					t.Fatalf("Put %s: %v", rev, err)
				}
			}
			for rev, offset := range tc.mtimeOffset {
				touchMtime(t, s.fs.Name(), "ns/n/"+rev+".tar.gz", now.Add(offset))
			}
			if err := s.Prune(context.Background(), "ns", "n", tc.keep, tc.grace); err != nil {
				t.Fatalf("Prune: %v", err)
			}
			for rev, expected := range tc.want {
				_, statErr := os.Stat(filepath.Join(s.fs.Name(), "ns", "n", rev+".tar.gz"))
				present := !os.IsNotExist(statErr)
				if present != expected {
					t.Errorf("%s presence after Prune = %v, want %v", rev, present, expected)
				}
			}
		})
	}
}
