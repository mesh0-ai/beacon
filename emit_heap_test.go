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

// TestEmit_NoHeapGrowth pins the heap-growth invariant. Uses absolute
// slack rather than multiplicative: a 2x baseline grows the allowed
// drift with the noise floor, which is the opposite of what we want.
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

	const slack = 4 * 1024 * 1024 // 4 MiB absolute slack
	if after.HeapInuse > before.HeapInuse+slack {
		t.Fatalf("heap drift: before=%d after=%d delta=%d (slack=%d)",
			before.HeapInuse, after.HeapInuse, after.HeapInuse-before.HeapInuse, slack)
	}
}
