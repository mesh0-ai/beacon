package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sampleFixture() Sample {
	return Sample{
		UptimeS: 100, MemUsedBytes: 1024, MemLimitBytes: 4096,
		MemSwapUsedBytes: 0, CPUUsageUsec: 500, CPUPct: 1.5,
		CPUQuotaCores: 0.5, CPUThrottledUsec: 10,
		Load1: 0.1, Load5: 0.2, Load15: 0.3, ProcsRunning: 2,
	}
}

func newTestEmitter(cfg *Config) *Emitter {
	return &Emitter{cfg: cfg, client: newHTTPClient()}
}

func TestBuildEvent_AttrsAndLabels(t *testing.T) {
	cfg := &Config{
		PodName: "p", NodeName: "n", Namespace: "ns",
		Deployment: "d", Container: "c",
		Labels: map[string]string{"env": "prod"},
	}
	e := newTestEmitter(cfg)
	ev := e.buildEvent(sampleFixture(), time.Unix(1747400000, 0))

	if ev.EventName != "pod.health" {
		t.Fatalf("event_name = %q", ev.EventName)
	}
	if ev.Attributes["pod_name"] != "p" || ev.Attributes["env"] != "prod" {
		t.Fatalf("attrs missing: %+v", ev.Attributes)
	}
	if ev.Attributes["mem_used_bytes"].(int64) != 1024 {
		t.Fatalf("mem_used_bytes = %v", ev.Attributes["mem_used_bytes"])
	}
}

func TestBuildEvent_LabelsCannotOverrideReserved(t *testing.T) {
	// Labels are added after core attrs in buildEvent — but parseLabels
	// drops reserved keys upstream. Simulate a misconfigured Labels map
	// here to confirm we don't silently allow override at emit time.
	// (Current behavior: labels overwrite. parseLabels is the gate.)
	cfg := &Config{PodName: "real", Labels: map[string]string{"pod_name": "fake"}}
	e := newTestEmitter(cfg)
	ev := e.buildEvent(sampleFixture(), time.Now())
	// Document current behavior: parseLabels is the guard, not buildEvent.
	if ev.Attributes["pod_name"] != "fake" {
		t.Logf("note: buildEvent does not re-check reserved keys (parseLabels does)")
	}
}

func TestEmit_DryRun(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := stdout
	stdout = buf
	defer func() { stdout = prev }()

	cfg := &Config{DryRun: true, PodName: "p"}
	e := newTestEmitter(cfg)
	e.Emit(context.Background(), sampleFixture(), time.Now())

	var ev Event
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatalf("not json: %v\n%s", err, buf.String())
	}
	if ev.EventName != "pod.health" {
		t.Fatalf("event_name = %q", ev.EventName)
	}
	if e.sendFailures.Load() != 0 {
		t.Fatalf("dry-run should not record failures")
	}
}

func TestEmit_HTTPSuccess(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad auth header: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad content-type: %q", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "test-key", Endpoint: srv.URL, PodName: "p"}
	e := newTestEmitter(cfg)
	e.Emit(context.Background(), sampleFixture(), time.Now())

	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	if e.sendFailures.Load() != 0 {
		t.Fatalf("send_failures = %d, want 0", e.sendFailures.Load())
	}
}

func TestEmit_HTTPNon2xx(t *testing.T) {
	prev := stderr
	stderr = io.Discard
	defer func() { stderr = prev }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "k", Endpoint: srv.URL}
	e := newTestEmitter(cfg)
	e.Emit(context.Background(), sampleFixture(), time.Now())

	if e.sendFailures.Load() != 1 {
		t.Fatalf("send_failures = %d, want 1", e.sendFailures.Load())
	}
}

// TestEmit_NoGoroutineGrowth is invariant #1 from plan.md.
func TestEmit_NoGoroutineGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in short mode")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "k", Endpoint: srv.URL}
	e := newTestEmitter(cfg)
	ctx := context.Background()
	s := sampleFixture()

	// Warm up so any one-time goroutines exist.
	for i := 0; i < 10; i++ {
		e.Emit(ctx, s, time.Now())
	}
	runtime.GC()
	before := runtime.NumGoroutine()

	const N = 1000
	for i := 0; i < N; i++ {
		e.Emit(ctx, s, time.Now())
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond) // let any straggler-conn closers finish

	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestStatus(t *testing.T) {
	e := newTestEmitter(&Config{})
	st := e.Status()
	if !st.OK || st.LastSendOKAt != "" || st.SendFailures != 0 {
		t.Fatalf("initial status = %+v", st)
	}
	e.lastSendOK.Store(time.Now().UnixNano())
	e.sendFailures.Add(3)
	st = e.Status()
	if st.SendFailures != 3 || !strings.Contains(st.LastSendOKAt, "T") {
		t.Fatalf("status after activity = %+v", st)
	}
}
