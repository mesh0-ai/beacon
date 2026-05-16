package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Indirected for tests.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

var version = "dev"

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "beacon %s starting (interval=%ds dry_run=%v)\n", version, cfg.Interval, cfg.DryRun)

	collector := NewCollector()
	emitter := NewEmitter(cfg)

	go serveHealth(emitter)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// First tick immediately.
	emitter.Emit(ctx, collector.Collect(time.Now()), time.Now())

	ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// One final emit on graceful shutdown.
			finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			emitter.Emit(finalCtx, collector.Collect(time.Now()), time.Now())
			cancel()
			fmt.Fprintln(os.Stderr, "beacon shutting down")
			return
		case now := <-ticker.C:
			emitter.Emit(ctx, collector.Collect(now), now)
		}
	}
}

func serveHealth(e *Emitter) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e.Status())
	})
	srv := &http.Server{
		Addr:              healthAddr,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "warn: healthz: %v\n", err)
	}
}
