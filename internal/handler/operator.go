/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/metio/jaas/internal/opstate"
)

// OperatorStatus is the body of an /operator response. Reason, Since and
// Attempts are omitted while the operator is available, where they would carry
// nothing an operator can act on.
type OperatorStatus struct {
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Since    string `json:"since,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

// OperatorHandler reports whether the operator subsystem is reconciling. It is
// deliberately separate from the readiness probe: the Jsonnet renderer and the
// artifact server keep working through an apiserver outage, so withdrawing the
// pod from its Services is not the right response to one, while an alert or a
// human looking for the cause of a stalled snippet needs a direct answer.
//
// 200 means reconciling. 503 means not reconciling, with the reason, the time
// the reading last changed, and how many times the manager has been started.
func OperatorHandler(state *opstate.State) http.HandlerFunc {
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
		if state == nil || state.Available() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		snap := state.Snapshot()
		body := OperatorStatus{
			Status:   "unavailable",
			Reason:   snap.Reason,
			Attempts: snap.Attempts,
		}
		if !snap.Since.IsZero() {
			body.Since = snap.Since.UTC().Format(time.RFC3339)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(body)
	}
}
