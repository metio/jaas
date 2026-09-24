/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package opstate

import (
	"sync"
	"testing"
)

func TestNew_StartsUnavailable(t *testing.T) {
	s := New()
	if s.Available() {
		t.Error("Available() = true, want false on a fresh State")
	}
	snap := s.Snapshot()
	if snap.Since.IsZero() {
		t.Error("Snapshot().Since is zero, want the construction time")
	}
	if snap.Attempts != 0 {
		t.Errorf("Snapshot().Attempts = %d, want 0", snap.Attempts)
	}
}

func TestMarkAvailable_ReportsOnlyTheTransition(t *testing.T) {
	s := New()
	if !s.MarkAvailable() {
		t.Error("first MarkAvailable() = false, want true")
	}
	if s.MarkAvailable() {
		t.Error("second MarkAvailable() = true, want false")
	}
	if !s.Available() {
		t.Error("Available() = false after MarkAvailable")
	}
}

func TestMarkUnavailable_ReportsOnlyTheTransition(t *testing.T) {
	s := New()
	// Already unavailable, so this is not a transition.
	if s.MarkUnavailable("first") {
		t.Error("MarkUnavailable() on a fresh State = true, want false")
	}
	s.MarkAvailable()
	if !s.MarkUnavailable("second") {
		t.Error("MarkUnavailable() after MarkAvailable = false, want true")
	}
	if s.Available() {
		t.Error("Available() = true after MarkUnavailable")
	}
}

func TestMarkUnavailable_RefreshesTheReasonWithoutATransition(t *testing.T) {
	s := New()
	s.MarkUnavailable("cannot reach the apiserver")
	s.MarkUnavailable("forbidden")
	if got := s.Snapshot().Reason; got != "forbidden" {
		t.Errorf("Snapshot().Reason = %q, want the latest reason", got)
	}
}

func TestMarkAvailable_ClearsTheReason(t *testing.T) {
	s := New()
	s.MarkUnavailable("i/o timeout")
	s.MarkAvailable()
	if got := s.Snapshot().Reason; got != "" {
		t.Errorf("Snapshot().Reason = %q, want empty while available", got)
	}
}

func TestSince_MovesOnlyOnATransition(t *testing.T) {
	s := New()
	s.MarkAvailable()
	first := s.Snapshot().Since
	s.MarkAvailable()
	if got := s.Snapshot().Since; !got.Equal(first) {
		t.Errorf("Snapshot().Since = %v, want it unchanged at %v without a transition", got, first)
	}
}

func TestRecordAttempt_Counts(t *testing.T) {
	s := New()
	for want := 1; want <= 3; want++ {
		if got := s.RecordAttempt(); got != want {
			t.Errorf("RecordAttempt() = %d, want %d", got, want)
		}
	}
	if got := s.Snapshot().Attempts; got != 3 {
		t.Errorf("Snapshot().Attempts = %d, want 3", got)
	}
}

// A nil State is what a renderer-mode process has, and both the gauge and the
// management endpoint read it without a nil check of their own.
func TestAvailable_NilStateReadsAvailable(t *testing.T) {
	var s *State
	if !s.Available() {
		t.Error("(*State)(nil).Available() = false, want true")
	}
}

func TestState_ConcurrentReadersAndWriters(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 100 {
				if i%2 == 0 {
					s.MarkAvailable()
				} else {
					s.MarkUnavailable("churn")
				}
				s.RecordAttempt()
				_ = s.Snapshot()
				_ = s.Available()
			}
		}(i)
	}
	wg.Wait()
	if got := s.Snapshot().Attempts; got != 800 {
		t.Errorf("Snapshot().Attempts = %d, want 800", got)
	}
}
