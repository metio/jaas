/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandler_SetsJSONContentType(t *testing.T) {
	h := HealthHandler()
	req := httptest.NewRequest(http.MethodGet, "/live", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := rr.Header().Get("Content-Type"), "application/json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

func TestHealthHandler_MethodNotAllowed(t *testing.T) {
	h := HealthHandler()
	req := httptest.NewRequest(http.MethodPost, "/live", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if got, want := rr.Code, http.StatusMethodNotAllowed; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}
