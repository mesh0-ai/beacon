package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// TestEmit_NoHeapGrowth is invariant #3 from plan.md.
func TestEmit_NoHeapGrowth(t *testing.T) {
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

	// Warm up so steady-state allocations stabilize.
	for i := 0; i < 100; i++ {
		e.Emit(ctx, s, time.Now())
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)

	const N = 1000
	for i := 0; i < N; i++ {
		e.Emit(ctx, s, time.Now())
	}

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)

	// HeapInuse fluctuates per GC pacing; allow up to 2x baseline as
	// a loose ceiling. Real leaks balloon by orders of magnitude.
	if after.HeapInuse > before.HeapInuse*2 {
		t.Fatalf("heap drift: before=%d after=%d", before.HeapInuse, after.HeapInuse)
	}
}
