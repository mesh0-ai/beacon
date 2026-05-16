package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	cfg, err := loadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "Beacon %s starting (interval=%s dry_run=%v)\n", version, cfg.Interval, cfg.DryRun)

	collector := NewCollector(os.Stderr)
	emitter := NewEmitter(cfg, collector)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	healthDone := make(chan struct{})
	healthSrv, healthErr := startHealth(emitter, healthDone)
	if healthErr != nil {
		// A bound port is the difference between a beacon you can probe
		// and a beacon flying blind. Fail loud.
		fmt.Fprintf(os.Stderr, "fatal: healthz: %v\n", healthErr)
		os.Exit(3)
	}

	// First tick immediately so a freshly-started pod is visible in
	// less than Interval seconds.
	emitter.Emit(ctx, collector.Collect(time.Now()), time.Now())

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			runShutdown(emitter, collector, healthSrv, healthDone)
			return
		case now := <-ticker.C:
			emitter.Emit(ctx, collector.Collect(now), now)
		}
	}
}

func runShutdown(emitter *Emitter, collector *Collector, healthSrv *http.Server, healthDone <-chan struct{}) {
	// Detached context: we want the final-emit to land even though the
	// signal context just cancelled. The per-attempt and overall client
	// timeouts inside emit cap how long this can take.
	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	emitter.Emit(finalCtx, collector.Collect(time.Now()), time.Now())

	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	_ = healthSrv.Shutdown(shutdownCtx)
	<-healthDone
	fmt.Fprintln(os.Stderr, "Beacon shutting down")
}

// startHealth binds the healthz listener synchronously so a port
// conflict fails fast at startup instead of silently leaving beacon
// without a liveness endpoint.
func startHealth(e *Emitter, done chan<- struct{}) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		st := e.Status()
		w.Header().Set("Content-Type", "application/json")
		if !st.OK {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	srv := &http.Server{
		Addr:              healthAddr,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	ln, err := net.Listen("tcp", healthAddr)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "warn: healthz: %v\n", err)
		}
	}()
	return srv, nil
}
