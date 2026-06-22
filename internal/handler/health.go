/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"net/http"
	"sync"
)

type HealthState struct {
	mu       sync.RWMutex
	started  bool
	ready    bool
	draining bool
}

func NewHealthState() *HealthState {
	return &HealthState{}
}

func (s *HealthState) MarkStarted() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
}

// MarkDraining latches the state into shutdown: from this point SetReady(true)
// is a no-op, so any readiness writer that runs concurrently with shutdown — the
// operator's post-cache-sync onReady, say — can't flip the pod back to Ready
// during the drain window and pull traffic onto a pod that is about to abort
// connections.
func (s *HealthState) MarkDraining() {
	s.mu.Lock()
	s.draining = true
	s.ready = false
	s.mu.Unlock()
}

func (s *HealthState) SetReady(ready bool) {
	s.mu.Lock()
	// Once draining, readiness only ratchets downward.
	if !s.draining || !ready {
		s.ready = ready
	}
	s.mu.Unlock()
}

func (s *HealthState) Started() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.started
}

func (s *HealthState) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

func StartupHandler(state *HealthState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			// RFC 7231 §6.5.5 requires a 405 to advertise the supported methods.
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"status":"method_not_allowed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if state.Started() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not_started"}`))
	}
}

func ReadinessHandler(state *HealthState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			// RFC 7231 §6.5.5 requires a 405 to advertise the supported methods.
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"status":"method_not_allowed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if state.Ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not_ready"}`))
	}
}

func LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			// RFC 7231 §6.5.5 requires a 405 to advertise the supported methods.
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"status":"method_not_allowed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}
