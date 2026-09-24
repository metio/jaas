/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/metio/jaas/internal/opstate"
)

func TestOperatorHandler_AvailableReturnsOK(t *testing.T) {
	state := opstate.New()
	state.MarkAvailable()

	rec := httptest.NewRecorder()
	OperatorHandler(state)(rec, httptest.NewRequest(http.MethodGet, "/operator", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body OperatorStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body.Status != "ok" {
		t.Errorf("status field = %q, want ok", body.Status)
	}
}

func TestOperatorHandler_UnavailableReturns503WithTheReason(t *testing.T) {
	state := opstate.New()
	state.RecordAttempt()
	state.RecordAttempt()
	state.MarkUnavailable(`create manager: Get "https://10.24.64.1:443/api": i/o timeout`)

	rec := httptest.NewRecorder()
	OperatorHandler(state)(rec, httptest.NewRequest(http.MethodGet, "/operator", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body OperatorStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body.Status != "unavailable" {
		t.Errorf("status field = %q, want unavailable", body.Status)
	}
	if body.Reason == "" {
		t.Error("reason field is empty, want the failure that keeps the operator down")
	}
	if body.Attempts != 2 {
		t.Errorf("attempts field = %d, want 2", body.Attempts)
	}
	if body.Since == "" {
		t.Error("since field is empty, want the time the reading last changed")
	}
}

// A renderer-mode process registers no /operator route, but the handler stays
// total so a nil State cannot panic a management request.
func TestOperatorHandler_NilStateReturnsOK(t *testing.T) {
	rec := httptest.NewRecorder()
	OperatorHandler(nil)(rec, httptest.NewRequest(http.MethodGet, "/operator", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestOperatorHandler_RejectsNonGET(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			OperatorHandler(opstate.New())(rec, httptest.NewRequest(method, "/operator", nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want %q", got, http.MethodGet)
			}
		})
	}
}
