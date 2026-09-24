/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

// Package opstate carries whether the operator subsystem is currently able to
// reconcile. Three readers share it — the supervisor that starts the manager
// writes it, the management endpoint reports it, and a Prometheus gauge alerts
// on it — and none of them owns the others, so the state lives here rather than
// in any one of them.
package opstate

import (
	"sync"
	"time"
)

// State is the operator subsystem's availability, plus enough context to make
// an unavailable reading actionable: why, since when, and how many times the
// manager has been started.
//
// The zero value is unavailable with no reason, which is what a process that
// runs the operator starts as — the manager has not synced yet. A process
// without the operator never constructs one.
type State struct {
	mu        sync.RWMutex
	available bool
	reason    string
	since     time.Time
	attempts  int
}

// Snapshot is a consistent read of a State, taken under one lock so the fields
// cannot disagree with each other.
type Snapshot struct {
	Available bool
	Reason    string
	Since     time.Time
	Attempts  int
}

// New returns a State in its initial unavailable reading, stamped now.
func New() *State {
	return &State{since: time.Now()}
}

// MarkAvailable records that the manager is reconciling. It reports whether
// this changed the reading, so a caller can log the transition and stay quiet
// on the repeats — a manager that is restarted in a loop calls this once per
// successful start.
func (s *State) MarkAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.available {
		return false
	}
	s.available = true
	s.reason = ""
	s.since = time.Now()
	return true
}

// MarkUnavailable records that the manager is not reconciling, with the reason
// to show an operator. It reports whether this changed the reading. The reason
// is refreshed even when the reading does not change, so a failure that shifts
// from "cannot reach the apiserver" to "forbidden" is visible without the
// transition log line repeating.
func (s *State) MarkUnavailable(reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reason = reason
	if !s.available {
		return false
	}
	s.available = false
	s.since = time.Now()
	return true
}

// RecordAttempt counts one manager start and returns the running total.
func (s *State) RecordAttempt() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	return s.attempts
}

// Available reports whether the manager is reconciling. A nil State reads as
// available: a process that runs no operator has no operator to be down, and
// the gauge and the endpoint both need a total function.
func (s *State) Available() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.available
}

// Snapshot reads every field under one lock.
func (s *State) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		Available: s.available,
		Reason:    s.reason,
		Since:     s.since,
		Attempts:  s.attempts,
	}
}
