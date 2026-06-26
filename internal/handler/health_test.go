/*
 * SPDX-FileCopyrightText: The jaas Authors
 * SPDX-License-Identifier: 0BSD
 */

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// --- HealthState ---

func TestNewHealthState_InitiallyNotStartedNotReady(t *testing.T) {
	s := NewHealthState()
	if s.Started() {
		t.Error("Started() = true, want false")
	}
	if s.Ready() {
		t.Error("Ready() = true, want false")
	}
}

func TestHealthState_MarkStarted(t *testing.T) {
	s := NewHealthState()
	s.MarkStarted()
	if !s.Started() {
		t.Error("Started() = false, want true after MarkStarted")
	}
}

func TestHealthState_MarkStartedIsIdempotent(t *testing.T) {
	s := NewHealthState()
	for range 5 {
		s.MarkStarted()
	}
	if !s.Started() {
		t.Error("Started() = false after repeated MarkStarted")
	}
}

func TestHealthState_MarkStartedDoesNotImplyReady(t *testing.T) {
	s := NewHealthState()
	s.MarkStarted()
	if s.Ready() {
		t.Error("MarkStarted set Ready() to true; Started and Ready must be independent")
	}
}

func TestHealthState_SetReadyTrue(t *testing.T) {
	s := NewHealthState()
	s.SetReady(true)
	if !s.Ready() {
		t.Error("Ready() = false after SetReady(true)")
	}
}

func TestHealthState_SetReadyFalse(t *testing.T) {
	s := NewHealthState()
	s.SetReady(true)
	s.SetReady(false)
	if s.Ready() {
		t.Error("Ready() = true after SetReady(false)")
	}
}

func TestHealthState_SetReadyDoesNotImplyStarted(t *testing.T) {
	s := NewHealthState()
	s.SetReady(true)
	if s.Started() {
		t.Error("SetReady(true) set Started() to true; Started and Ready must be independent")
	}
}

// TestHealthState_DrainingLatchBlocksSetReadyTrue pins that once draining is
// latched, a later SetReady(true) (e.g. the operator's post-cache-sync onReady)
// cannot flip the pod back to Ready during the drain window.
func TestHealthState_DrainingLatchBlocksSetReadyTrue(t *testing.T) {
	s := NewHealthState()
	s.SetReady(true)
	s.MarkDraining()
	if s.Ready() {
		t.Fatal("Ready() = true immediately after MarkDraining")
	}
	s.SetReady(true) // a racing readiness writer
	if s.Ready() {
		t.Fatal("Ready() = true after SetReady(true) while draining; latch did not hold")
	}
	// SetReady(false) still works (no spurious upgrade).
	s.SetReady(false)
	if s.Ready() {
		t.Fatal("Ready() = true after SetReady(false)")
	}
}

func TestHealthState_ReadyIsCycleable(t *testing.T) {
	s := NewHealthState()
	for i := range 10 {
		s.SetReady(true)
		if !s.Ready() {
			t.Fatalf("iter %d: Ready() = false after SetReady(true)", i)
		}
		s.SetReady(false)
		if s.Ready() {
			t.Fatalf("iter %d: Ready() = true after SetReady(false)", i)
		}
	}
}

func TestHealthState_ConcurrentWritersAndReaders(t *testing.T) {
	s := NewHealthState()

	var wg sync.WaitGroup
	const goroutines = 100
	for i := range goroutines {
		wg.Add(3)
		go func() {
			defer wg.Done()
			s.MarkStarted()
		}()
		go func(i int) {
			defer wg.Done()
			s.SetReady(i%2 == 0)
		}(i)
		go func() {
			defer wg.Done()
			_ = s.Started()
			_ = s.Ready()
		}()
	}
	wg.Wait()

	if !s.Started() {
		t.Error("Started() = false after concurrent MarkStarted calls")
	}
}

func TestHealthState_ManyConcurrentReads(t *testing.T) {
	s := NewHealthState()
	s.MarkStarted()
	s.SetReady(true)

	var wg sync.WaitGroup
	const readers = 1000
	for range readers {
		wg.Go(func() {
			if !s.Started() {
				t.Error("Started() = false in concurrent read")
			}
			if !s.Ready() {
				t.Error("Ready() = false in concurrent read")
			}
		})
	}
	wg.Wait()
}

// --- LivenessHandler ---

func TestLivenessHandler_GETReturns200WithJSONBody(t *testing.T) {
	rr := callProbe(t, LivenessHandler(), http.MethodGet)
	assertStatus(t, rr, http.StatusOK)
	assertJSONContentType(t, rr)
	assertJSONStatus(t, rr, "ok")
}

func TestLivenessHandler_AlwaysReturns200(t *testing.T) {
	h := LivenessHandler()
	for i := range 5 {
		rr := callProbe(t, h, http.MethodGet)
		if rr.Code != http.StatusOK {
			t.Errorf("call %d: status = %d, want 200", i, rr.Code)
		}
	}
}

// --- StartupHandler ---

func TestStartupHandler_NotStartedReturns503(t *testing.T) {
	s := NewHealthState()
	rr := callProbe(t, StartupHandler(s), http.MethodGet)
	assertStatus(t, rr, http.StatusServiceUnavailable)
	assertJSONContentType(t, rr)
	assertJSONStatus(t, rr, "not_started")
}

func TestStartupHandler_StartedReturns200(t *testing.T) {
	s := NewHealthState()
	s.MarkStarted()
	rr := callProbe(t, StartupHandler(s), http.MethodGet)
	assertStatus(t, rr, http.StatusOK)
	assertJSONContentType(t, rr)
	assertJSONStatus(t, rr, "ok")
}

func TestStartupHandler_DoesNotConsultReady(t *testing.T) {
	s := NewHealthState()
	s.MarkStarted()
	s.SetReady(false)
	rr := callProbe(t, StartupHandler(s), http.MethodGet)
	assertStatus(t, rr, http.StatusOK)
}

func TestStartupHandler_StartedRemainsStartedAfterReadyFlipsOff(t *testing.T) {
	s := NewHealthState()
	s.MarkStarted()
	s.SetReady(true)
	s.SetReady(false)
	rr := callProbe(t, StartupHandler(s), http.MethodGet)
	assertStatus(t, rr, http.StatusOK)
}

func TestStartupHandler_ResponseBodyIsValidJSONWhenNotStarted(t *testing.T) {
	s := NewHealthState()
	rr := callProbe(t, StartupHandler(s), http.MethodGet)
	if _, err := decodeStatus(rr); err != nil {
		t.Errorf("body is not valid JSON: %v (body: %q)", err, rr.Body.String())
	}
}

// --- ReadinessHandler ---

func TestReadinessHandler_NotReadyReturns503(t *testing.T) {
	s := NewHealthState()
	rr := callProbe(t, ReadinessHandler(s), http.MethodGet)
	assertStatus(t, rr, http.StatusServiceUnavailable)
	assertJSONContentType(t, rr)
	assertJSONStatus(t, rr, "not_ready")
}

func TestReadinessHandler_ReadyReturns200(t *testing.T) {
	s := NewHealthState()
	s.SetReady(true)
	rr := callProbe(t, ReadinessHandler(s), http.MethodGet)
	assertStatus(t, rr, http.StatusOK)
	assertJSONContentType(t, rr)
	assertJSONStatus(t, rr, "ok")
}

func TestReadinessHandler_FlipsBetweenStatesOverTime(t *testing.T) {
	s := NewHealthState()
	h := ReadinessHandler(s)

	if rr := callProbe(t, h, http.MethodGet); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("initial: status = %d, want 503", rr.Code)
	}
	s.SetReady(true)
	if rr := callProbe(t, h, http.MethodGet); rr.Code != http.StatusOK {
		t.Fatalf("after SetReady(true): status = %d, want 200", rr.Code)
	}
	s.SetReady(false)
	if rr := callProbe(t, h, http.MethodGet); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("after SetReady(false): status = %d, want 503", rr.Code)
	}
	s.SetReady(true)
	if rr := callProbe(t, h, http.MethodGet); rr.Code != http.StatusOK {
		t.Fatalf("after second SetReady(true): status = %d, want 200", rr.Code)
	}
}

func TestReadinessHandler_DoesNotConsultStarted(t *testing.T) {
	s := NewHealthState()
	s.SetReady(true) // started stays false
	rr := callProbe(t, ReadinessHandler(s), http.MethodGet)
	assertStatus(t, rr, http.StatusOK)
}

func TestReadinessHandler_ResponseBodyIsValidJSONWhenNotReady(t *testing.T) {
	s := NewHealthState()
	rr := callProbe(t, ReadinessHandler(s), http.MethodGet)
	if _, err := decodeStatus(rr); err != nil {
		t.Errorf("body is not valid JSON: %v (body: %q)", err, rr.Body.String())
	}
}

// --- Method rejection (covers all three handlers x all non-GET methods) ---

func TestProbeHandlers_RejectNonGETMethods(t *testing.T) {
	s := NewHealthState()
	s.MarkStarted()
	s.SetReady(true)

	handlers := map[string]http.HandlerFunc{
		"Startup":   StartupHandler(s),
		"Readiness": ReadinessHandler(s),
		"Liveness":  LivenessHandler(),
	}
	methods := []string{
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodDelete,
		http.MethodPatch,
		http.MethodOptions,
	}

	for name, h := range handlers {
		for _, m := range methods {
			t.Run(name+"/"+m, func(t *testing.T) {
				rr := callProbe(t, h, m)
				if rr.Code != http.StatusMethodNotAllowed {
					t.Errorf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
				}
				// The 405 carries a JSON content-type and body, consistent
				// with the Jsonnet handler — not a bare status line.
				if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
				// RFC 7231 §6.5.5: a 405 must advertise the supported methods.
				if got := rr.Header().Get("Allow"); got != http.MethodGet {
					t.Errorf("Allow header = %q, want %q", got, http.MethodGet)
				}
				if !json.Valid(rr.Body.Bytes()) {
					t.Errorf("405 body is not valid JSON: %q", rr.Body.String())
				}
			})
		}
	}
}

// --- Full lifecycle: shared state drives all three handlers ---

func TestProbeHandlers_FullLifecycle(t *testing.T) {
	s := NewHealthState()
	startup := StartupHandler(s)
	ready := ReadinessHandler(s)
	live := LivenessHandler()

	t.Run("initial: nothing started, nothing ready", func(t *testing.T) {
		if rr := callProbe(t, startup, http.MethodGet); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("startup status = %d, want 503", rr.Code)
		}
		if rr := callProbe(t, ready, http.MethodGet); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("ready status = %d, want 503", rr.Code)
		}
		if rr := callProbe(t, live, http.MethodGet); rr.Code != http.StatusOK {
			t.Errorf("live status = %d, want 200", rr.Code)
		}
	})

	t.Run("started but not yet ready", func(t *testing.T) {
		s.MarkStarted()
		if rr := callProbe(t, startup, http.MethodGet); rr.Code != http.StatusOK {
			t.Errorf("startup status = %d, want 200", rr.Code)
		}
		if rr := callProbe(t, ready, http.MethodGet); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("ready status = %d, want 503", rr.Code)
		}
		if rr := callProbe(t, live, http.MethodGet); rr.Code != http.StatusOK {
			t.Errorf("live status = %d, want 200", rr.Code)
		}
	})

	t.Run("fully running", func(t *testing.T) {
		s.SetReady(true)
		if rr := callProbe(t, startup, http.MethodGet); rr.Code != http.StatusOK {
			t.Errorf("startup status = %d, want 200", rr.Code)
		}
		if rr := callProbe(t, ready, http.MethodGet); rr.Code != http.StatusOK {
			t.Errorf("ready status = %d, want 200", rr.Code)
		}
		if rr := callProbe(t, live, http.MethodGet); rr.Code != http.StatusOK {
			t.Errorf("live status = %d, want 200", rr.Code)
		}
	})

	t.Run("draining (shutdown initiated)", func(t *testing.T) {
		s.SetReady(false)
		if rr := callProbe(t, startup, http.MethodGet); rr.Code != http.StatusOK {
			t.Errorf("startup status = %d, want 200 (started stays sticky)", rr.Code)
		}
		if rr := callProbe(t, ready, http.MethodGet); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("ready status = %d, want 503", rr.Code)
		}
		if rr := callProbe(t, live, http.MethodGet); rr.Code != http.StatusOK {
			t.Errorf("live status = %d, want 200 (process is still alive)", rr.Code)
		}
	})
}

// --- helpers ---

func callProbe(t *testing.T, h http.HandlerFunc, method string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/probe", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func assertStatus(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Errorf("status = %d, want %d (body: %s)", rr.Code, want, rr.Body.String())
	}
}

func assertJSONContentType(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func assertJSONStatus(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	got, err := decodeStatus(rr)
	if err != nil {
		t.Fatalf("body is not valid JSON: %v (body: %q)", err, rr.Body.String())
	}
	if got != want {
		t.Errorf("status field = %q, want %q (body: %q)", got, want, rr.Body.String())
	}
}

func decodeStatus(rr *httptest.ResponseRecorder) (string, error) {
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(strings.NewReader(rr.Body.String())).Decode(&payload); err != nil {
		return "", err
	}
	return payload.Status, nil
}
