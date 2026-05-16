package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newHealthHandler is the same handler startHealth installs, factored
// for testability.
func newHealthHandler(e *Emitter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		st := e.Status()
		w.Header().Set("Content-Type", "application/json")
		if !st.OK {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	return mux
}

func TestHealthz_OKReturns200(t *testing.T) {
	e := newTestEmitter(&Config{Interval: 30 * time.Second})
	e.lastSendOK.Store(time.Now().UnixNano())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	newHealthHandler(e).ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content-type = %q", rr.Header().Get("Content-Type"))
	}
	var st Status
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("non-json body: %v", err)
	}
	if !st.OK {
		t.Errorf("body OK = false, want true")
	}
}

func TestHealthz_FailingReturns503(t *testing.T) {
	// k8s liveness probes treat non-2xx as failure. A beacon stuck on
	// permanent send failures must surface 503 so the probe can act.
	e := newTestEmitter(&Config{Interval: 30 * time.Second})
	e.permFailures.Store(1)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	newHealthHandler(e).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	var st Status
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("non-json body: %v", err)
	}
	if st.OK {
		t.Errorf("body OK = true, want false")
	}
	if st.PermFailures != 1 {
		t.Errorf("PermFailures = %d, want 1", st.PermFailures)
	}
}
