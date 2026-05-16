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

// newTestEmitter is a noisy-test-free constructor: it skips real
// time.Sleep so retry-backoff doesn't slow the suite.
func newTestEmitter(cfg *Config) *Emitter {
	return &Emitter{
		cfg:       cfg,
		client:    newHTTPClient(),
		startedAt: time.Now(),
		now:       time.Now,
		sleep:     func(time.Duration) {},
	}
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

func TestBuildEvent_TimestampFormat(t *testing.T) {
	// Timestamp is part of the wire contract; flipping the format
	// would silently break strict downstream parsers.
	e := newTestEmitter(&Config{})
	ev := e.buildEvent(sampleFixture(), time.Date(2026, 5, 16, 12, 30, 45, 123456789, time.UTC))
	parsed, err := time.Parse(time.RFC3339Nano, ev.Timestamp)
	if err != nil {
		t.Fatalf("timestamp %q not RFC3339Nano: %v", ev.Timestamp, err)
	}
	if !strings.HasSuffix(ev.Timestamp, "Z") {
		t.Errorf("timestamp %q must be UTC (Z suffix)", ev.Timestamp)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("timestamp not UTC: %s", parsed.Location())
	}
}

func TestBuildEvent_LabelsCannotOverrideReserved(t *testing.T) {
	// Defense in depth: even if a Labels map sneaks in a reserved key
	// (bypassing parseLabels), buildEvent must not let it overwrite a
	// system metric.
	cfg := &Config{PodName: "real", Labels: map[string]string{"pod_name": "fake", "cpu_pct": "bad"}}
	e := newTestEmitter(cfg)
	ev := e.buildEvent(sampleFixture(), time.Now())
	if ev.Attributes["pod_name"] != "real" {
		t.Fatalf("pod_name = %v, want %q (reserved overlay leaked)", ev.Attributes["pod_name"], "real")
	}
	if ev.Attributes["cpu_pct"] != float64(1.5) {
		t.Fatalf("cpu_pct = %v, want 1.5 (reserved overlay leaked)", ev.Attributes["cpu_pct"])
	}
}

func TestBuildEvent_MissingFieldsPreserveSentinels(t *testing.T) {
	// -1 is the wire contract for "missing". A refactor that maps it to
	// 0 or null silently corrupts downstream "data not available" logic.
	e := newTestEmitter(&Config{})
	missing := zeroSample()
	ev := e.buildEvent(missing, time.Now())
	if ev.Attributes["mem_used_bytes"].(int64) != -1 {
		t.Errorf("mem_used_bytes = %v, want -1", ev.Attributes["mem_used_bytes"])
	}
	if ev.Attributes["cpu_pct"].(float64) != -1 {
		t.Errorf("cpu_pct = %v, want -1", ev.Attributes["cpu_pct"])
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

func TestEmit_RetriesOn5xx(t *testing.T) {
	prev := stderr
	stderr = io.Discard
	defer func() { stderr = prev }()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 2 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "k", Endpoint: srv.URL}
	e := newTestEmitter(cfg)
	e.Emit(context.Background(), sampleFixture(), time.Now())

	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 (one retry)", hits.Load())
	}
	if e.sendFailures.Load() != 0 {
		t.Fatalf("recovered send shouldn't count as failure: %d", e.sendFailures.Load())
	}
	if e.retries.Load() != 1 {
		t.Fatalf("retries = %d, want 1", e.retries.Load())
	}
}

func TestEmit_BoundedRetriesGiveUp(t *testing.T) {
	prev := stderr
	stderr = io.Discard
	defer func() { stderr = prev }()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "k", Endpoint: srv.URL}
	e := newTestEmitter(cfg)
	e.Emit(context.Background(), sampleFixture(), time.Now())

	if hits.Load() != int64(maxAttempts) {
		t.Fatalf("hits = %d, want %d", hits.Load(), maxAttempts)
	}
	if e.sendFailures.Load() != 1 {
		t.Fatalf("send_failures = %d, want 1", e.sendFailures.Load())
	}
}

func TestEmit_4xxDoesNotRetry(t *testing.T) {
	// A 401/403 means bad config; retrying is wasted load and masks the
	// real problem. The permanent-failure counter must increment.
	prev := stderr
	stderr = io.Discard
	defer func() { stderr = prev }()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(401)
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "wrong", Endpoint: srv.URL}
	e := newTestEmitter(cfg)
	e.Emit(context.Background(), sampleFixture(), time.Now())

	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (no retry on 4xx)", hits.Load())
	}
	if e.permFailures.Load() != 1 {
		t.Fatalf("perm_failures = %d, want 1", e.permFailures.Load())
	}
}

func TestEmit_429And408AreRetried(t *testing.T) {
	prev := stderr
	stderr = io.Discard
	defer func() { stderr = prev }()

	for _, status := range []int{408, 429} {
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := hits.Add(1)
			if n < 2 {
				w.WriteHeader(status)
				return
			}
			w.WriteHeader(200)
		}))
		cfg := &Config{APIKey: "k", Endpoint: srv.URL}
		e := newTestEmitter(cfg)
		e.Emit(context.Background(), sampleFixture(), time.Now())
		srv.Close()
		if hits.Load() != 2 {
			t.Errorf("status=%d hits = %d, want 2", status, hits.Load())
		}
		if e.permFailures.Load() != 0 {
			t.Errorf("status=%d should not be permanent", status)
		}
	}
}

func TestEmit_IncludesBodyExcerptInError(t *testing.T) {
	// Operators debugging a 4xx need the server's error message; the
	// emitter must surface it in the logged error.
	buf := &bytes.Buffer{}
	prev := stderr
	stderr = buf
	defer func() { stderr = prev }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"missing required field 'pod_name'"}`))
	}))
	defer srv.Close()

	cfg := &Config{APIKey: "k", Endpoint: srv.URL}
	e := newTestEmitter(cfg)
	e.Emit(context.Background(), sampleFixture(), time.Now())

	if !strings.Contains(buf.String(), "missing required field") {
		t.Fatalf("error log missing response body: %q", buf.String())
	}
}

func TestEmit_NetworkErrorIsTransient(t *testing.T) {
	prev := stderr
	stderr = io.Discard
	defer func() { stderr = prev }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // immediately closed → connection refused

	cfg := &Config{APIKey: "k", Endpoint: srv.URL}
	e := newTestEmitter(cfg)
	e.Emit(context.Background(), sampleFixture(), time.Now())

	if e.permFailures.Load() != 0 {
		t.Fatalf("network error should not be permanent (got perm=%d)", e.permFailures.Load())
	}
	if e.sendFailures.Load() != 1 {
		t.Fatalf("send_failures = %d, want 1", e.sendFailures.Load())
	}
	// Should have hit max attempts.
	if e.retries.Load() != uint64(maxAttempts-1) {
		t.Errorf("retries = %d, want %d", e.retries.Load(), maxAttempts-1)
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

	// Poll instead of fixed sleep: connection-close goroutines have
	// variable wind-down on loaded CI.
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= before+2 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d", before, after)
}

func TestStatus_OKDerivation(t *testing.T) {
	cfg := &Config{Interval: 30 * time.Second}
	e := newTestEmitter(cfg)

	// Fresh emitter: within grace period, OK true.
	if !e.Status().OK {
		t.Error("fresh emitter should be OK during grace period")
	}

	// Past grace period with no successful send → not OK.
	e.startedAt = time.Now().Add(-2 * healthGracePeriod)
	if e.Status().OK {
		t.Error("post-grace with no successful send should be !OK")
	}

	// Recent successful send → OK.
	e.lastSendOK.Store(time.Now().UnixNano())
	if !e.Status().OK {
		t.Error("recent successful send should be OK")
	}

	// Stale successful send → !OK.
	e.lastSendOK.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	if e.Status().OK {
		t.Error("stale successful send should be !OK")
	}

	// Any permanent failure → !OK regardless of recency.
	e.lastSendOK.Store(time.Now().UnixNano())
	e.permFailures.Store(1)
	if e.Status().OK {
		t.Error("permanent failure should force !OK")
	}
}

func TestStatus_CollectorFieldsPropagated(t *testing.T) {
	col, _ := newTestCollector(true)
	col.errCount.Store(7)
	col.rollbacks.Store(2)
	cfg := &Config{Interval: 30 * time.Second}
	e := &Emitter{
		cfg:       cfg,
		client:    newHTTPClient(),
		collector: col,
		startedAt: time.Now(),
		now:       time.Now,
		sleep:     func(time.Duration) {},
	}
	st := e.Status()
	if st.CollectErrors != 7 {
		t.Errorf("CollectErrors = %d, want 7", st.CollectErrors)
	}
	if st.CounterRollback != 2 {
		t.Errorf("CounterRollback = %d, want 2", st.CounterRollback)
	}
	if !st.CgroupV2 {
		t.Errorf("CgroupV2 = false, want true")
	}
}
