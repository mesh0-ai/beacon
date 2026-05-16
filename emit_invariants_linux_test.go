//go:build linux

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestEmit_NoFDGrowth pins the file-descriptor-growth invariant.
// Linux-only because /proc/self/fd doesn't exist on macOS.
func TestEmit_NoFDGrowth(t *testing.T) {
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

	for i := 0; i < 20; i++ {
		e.Emit(ctx, s, time.Now())
	}
	time.Sleep(100 * time.Millisecond)
	before := countOpenFDs(t)

	const N = 1000
	for i := 0; i < N; i++ {
		e.Emit(ctx, s, time.Now())
	}
	time.Sleep(200 * time.Millisecond) // let TIME_WAIT/close finish

	after := countOpenFDs(t)
	// Allow a small slack for httptest/runtime jitter.
	if after > before+4 {
		t.Fatalf("fd leak: before=%d after=%d", before, after)
	}
}

func countOpenFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(ents)
}
