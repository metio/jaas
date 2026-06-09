/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package operator

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	jaasv1 "github.com/metio/jaas/api/v1"
)

// genRevision draws an sha256-prefixed revision string. The body is
// 8 hex digits — wide enough for collisions to be rare but narrow
// enough that the shrinker can show useful counterexamples.
func genRevision() *rapid.Generator[string] {
	return rapid.StringMatching(`sha256:[0-9a-f]{8}`)
}

// genShortRevision draws a revision string with no "sha256:" prefix
// (the form Status.Revision often appears in pre-trim).
func genShortRevision() *rapid.Generator[string] {
	return rapid.StringMatching(`[0-9a-f]{8}`)
}

// genRevisionEntry pairs a revision with an arbitrary RFC3339-ish
// time. Times don't need to be sensible — only used as opaque
// metadata that the function under test preserves verbatim.
func genRevisionEntry() *rapid.Generator[jaasv1.RevisionEntry] {
	return rapid.Custom(func(t *rapid.T) jaasv1.RevisionEntry {
		// Encode the time as a small offset from a fixed epoch so
		// the shrinker has a useful gradient.
		epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		secs := rapid.IntRange(0, 1_000_000).Draw(t, "sec")
		return jaasv1.RevisionEntry{
			Revision: genRevision().Draw(t, "rev"),
			Time:     metav1.NewTime(epoch.Add(time.Duration(secs) * time.Second)),
		}
	})
}

// TestUpdateRevisionHistory_Property pins the invariants
// `updateRevisionHistory` must hold for every (prior, revision,
// historyMax, now):
//
//  1. Length cap: the output has at most max(1, historyMax) entries.
//     historyMax <= 0 always degrades to "keep exactly one entry".
//
//  2. Head identity: when prior is non-empty AND prior[0].Revision
//     equals the new revision, output[0] equals prior[0] verbatim —
//     the original timestamp is preserved. (A republish of the same
//     content must not rewrite the head's wall-clock time, which is
//     what downstream pinning consumers key off.)
//     Otherwise output[0] is {revision, now}.
//
//  3. Suffix preservation: when not the same-head case, output[1:]
//     equals prior[:len(output)-1] — the prior list is preserved
//     verbatim, just truncated to fit the cap.
//
//  4. Same-head idempotence: the same-head case treats output as
//     prior truncated to the cap — no entries shift, no timestamps
//     move.
//
// Together these define the function: a future refactor that, say,
// added a dedup pass across all-of-prior would still satisfy the
// length cap but would break suffix preservation — surfacing as a
// failing property.
func TestUpdateRevisionHistory_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prior := rapid.SliceOfN(genRevisionEntry(), 0, 6).Draw(t, "prior")
		revision := genRevision().Draw(t, "revision")
		historyMax := rapid.Int32Range(-3, 8).Draw(t, "historyMax")
		now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

		out := updateRevisionHistory(prior, revision, historyMax, now)

		// Invariant 1: length cap.
		cap := int(historyMax)
		if cap < 1 {
			cap = 1
		}
		if len(out) > cap {
			t.Errorf("len(out)=%d exceeds cap %d (historyMax=%d)", len(out), cap, historyMax)
		}
		if len(out) == 0 {
			t.Errorf("len(out)=0 — same-head idempotence requires at least one entry")
		}

		sameHead := len(prior) > 0 && prior[0].Revision == revision
		if sameHead {
			// Invariant 2 + 4: output is prior truncated to cap;
			// the head's timestamp is preserved.
			wantLen := len(prior)
			if wantLen > cap {
				wantLen = cap
			}
			if len(out) != wantLen {
				t.Errorf("same-head: len(out)=%d, want %d", len(out), wantLen)
			}
			for i := 0; i < len(out); i++ {
				if out[i].Revision != prior[i].Revision {
					t.Errorf("same-head out[%d].Revision = %q, want %q",
						i, out[i].Revision, prior[i].Revision)
				}
				if !out[i].Time.Equal(&prior[i].Time) {
					t.Errorf("same-head out[%d].Time = %v, want %v (timestamp not preserved)",
						i, out[i].Time, prior[i].Time)
				}
			}
			return
		}

		// Invariant 2 (head identity, fresh-revision case).
		if out[0].Revision != revision {
			t.Errorf("fresh-rev out[0].Revision = %q, want %q", out[0].Revision, revision)
		}
		if !out[0].Time.Time.Equal(now) {
			t.Errorf("fresh-rev out[0].Time = %v, want %v (now)", out[0].Time.Time, now)
		}

		// Invariant 3 (suffix preservation).
		for i := 1; i < len(out); i++ {
			if out[i].Revision != prior[i-1].Revision {
				t.Errorf("out[%d].Revision = %q, want prior[%d].Revision = %q",
					i, out[i].Revision, i-1, prior[i-1].Revision)
			}
			if !out[i].Time.Equal(&prior[i-1].Time) {
				t.Errorf("out[%d].Time = %v, want prior[%d].Time = %v",
					i, out[i].Time, i-1, prior[i-1].Time)
			}
		}
	})
}

// TestBuildKeepShortRevs_Property pins the invariants of the
// keep-set assembler:
//
//  1. Length cap: at most max(1, history) entries.
//
//  2. Always non-empty: at least one element. Pruning never runs
//     with an empty keep-set in production (the Backend's contract
//     short-circuits to "no-op" on empty), but downstream code
//     keys off len(out) >= 1 — so the function must guarantee it.
//
//  3. New revision at head: output[0] is shortForm(newRev). The
//     reconciler always calls this with the just-published rev as
//     newRev so it must occupy slot 0.
//
//  4. No "sha256:" prefix on any element: short-form throughout.
//
//  5. Dedup: every element is unique. A buggy assembler that
//     emitted duplicates would waste Backend.Prune cap budget and
//     could prune revisions the operator meant to keep.
//
//  6. Suffix membership: every element after the head appears in
//     prior as the short-form of one of its Revisions. We don't
//     order-check the suffix beyond "appears in prior" because
//     prior may already contain the newRev as a later entry,
//     which the dedup step skips — the surviving order is
//     unambiguous but tedious to spell out.
func TestBuildKeepShortRevs_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Mix prior entries that share a revision with newRev with
		// ones that don't, so the dedup case fires.
		newRev := rapid.OneOf(
			genRevision(),
			genShortRevision(),
		).Draw(t, "newRev")
		prior := rapid.SliceOfN(genRevisionEntry(), 0, 6).Draw(t, "prior")
		history := rapid.Int32Range(-3, 8).Draw(t, "history")

		out := buildKeepShortRevs(newRev, prior, history)

		// Invariant 1: length cap.
		cap := int(history)
		if cap < 1 {
			cap = 1
		}
		if len(out) > cap {
			t.Errorf("len(out)=%d exceeds cap %d (history=%d)", len(out), cap, history)
		}

		// Invariant 2: non-empty (newRev is the always-include head,
		// and the generator's regex always produces a non-empty
		// short-form after trimming "sha256:").
		shortNew := strings.TrimPrefix(newRev, "sha256:")
		if shortNew == "" {
			// Generator can't produce this, but pin the contract.
			t.Skip("empty newRev — unrepresentable by the generator")
		}
		if len(out) == 0 {
			t.Errorf("len(out)=0 for non-empty newRev")
			return
		}

		// Invariant 3: newRev at head, short-form.
		if out[0] != shortNew {
			t.Errorf("out[0]=%q, want short-form of newRev %q (=%q)", out[0], newRev, shortNew)
		}

		// Invariant 4 + 5: no prefix, dedup.
		seen := map[string]struct{}{}
		priorShorts := map[string]struct{}{}
		for _, e := range prior {
			priorShorts[strings.TrimPrefix(e.Revision, "sha256:")] = struct{}{}
		}
		for i, e := range out {
			if strings.HasPrefix(e, "sha256:") {
				t.Errorf("out[%d]=%q carries the sha256: prefix", i, e)
			}
			if _, dup := seen[e]; dup {
				t.Errorf("out[%d]=%q duplicates a previous element", i, e)
			}
			seen[e] = struct{}{}
		}

		// Invariant 6: every suffix element is a short-form found
		// in prior. The head (out[0]) is the just-published rev so
		// it may or may not be in prior; the suffix is exclusively
		// drawn from prior.
		for i := 1; i < len(out); i++ {
			if _, ok := priorShorts[out[i]]; !ok {
				t.Errorf("out[%d]=%q is not the short-form of any prior entry %v",
					i, out[i], prior)
			}
		}
	})
}
