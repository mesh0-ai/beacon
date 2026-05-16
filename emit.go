package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	eventName = "pod.health"

	// Bounded in-tick retry. No queueing, no buffering: if all attempts
	// fail within one tick, the sample is dropped and the next tick
	// stands on its own. This preserves the "no spool, no persistence"
	// principle while absorbing transient blips.
	maxAttempts     = 3
	perAttemptLimit = 2 * time.Second
	baseBackoff     = 100 * time.Millisecond
	maxBodyExcerpt  = 512

	// healthGracePeriod gives a freshly-started beacon time to land its
	// first successful emit before /healthz starts reporting !OK.
	healthGracePeriod = 90 * time.Second
)

type Event struct {
	EventName  string         `json:"event_name"`
	Timestamp  string         `json:"timestamp"`
	Attributes map[string]any `json:"attributes"`
}

// permanentError marks a failure the client cannot fix by retrying
// (4xx, malformed URL). Caller logs at error severity and skips retry.
type permanentError struct{ err error }

func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

// Emitter ships one Event per tick to mesh0 (or stdout in dry-run).
// Counters are atomic so /healthz can read them without locking.
type Emitter struct {
	cfg          *Config
	client       *http.Client
	collector    *Collector
	sendFailures atomic.Uint64
	permFailures atomic.Uint64 // 4xx — config error, retries won't help
	retries      atomic.Uint64
	lastSendOK   atomic.Int64 // unix nanos; 0 = never
	startedAt    time.Time
	now          func() time.Time // overridable for tests
	sleep        func(time.Duration)
}

func NewEmitter(cfg *Config, collector *Collector) *Emitter {
	return &Emitter{
		cfg:       cfg,
		client:    newHTTPClient(),
		collector: collector,
		startedAt: time.Now(),
		now:       time.Now,
		sleep:     time.Sleep,
	}
}

// newHTTPClient is leak-resistant by design: no keep-alives (so an idle
// pool can't grow), no HTTP/2 (fewer state-machine corners), and a hard
// outer ceiling. Per-attempt timeouts live on the request context.
func newHTTPClient() *http.Client {
	transport := &http.Transport{
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   -1,
		IdleConnTimeout:       1 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

// Emit builds an event from a sample and ships it with bounded retry.
// Best-effort: any final error is logged + counted but never propagated.
func (e *Emitter) Emit(ctx context.Context, s Sample, now time.Time) {
	ev := e.buildEvent(s, now)
	body, err := json.Marshal(ev)
	if err != nil {
		// Schema is fixed and types are all marshalable; if we hit this
		// at runtime it's a programming error.
		e.sendFailures.Add(1)
		fmt.Fprintf(stderr, "error: marshal event: %v\n", err)
		return
	}
	if e.cfg.DryRun {
		fmt.Fprintln(stdout, string(body))
		e.lastSendOK.Store(now.UnixNano())
		return
	}
	if err := e.sendWithRetry(ctx, body); err != nil {
		e.sendFailures.Add(1)
		var perm *permanentError
		if errors.As(err, &perm) {
			e.permFailures.Add(1)
			fmt.Fprintf(stderr, "error: send (permanent, retries won't help): %v\n", err)
			return
		}
		fmt.Fprintf(stderr, "warn: send: %v\n", err)
		return
	}
	e.lastSendOK.Store(now.UnixNano())
}

func (e *Emitter) buildEvent(s Sample, now time.Time) Event {
	attrs := map[string]any{
		"pod_name":            e.cfg.PodName,
		"node_name":           e.cfg.NodeName,
		"namespace":           e.cfg.Namespace,
		"deployment":          e.cfg.Deployment,
		"container":           e.cfg.Container,
		"uptime_s":            s.UptimeS,
		"mem_used_bytes":      s.MemUsedBytes,
		"mem_limit_bytes":     s.MemLimitBytes,
		"mem_swap_used_bytes": s.MemSwapUsedBytes,
		"cpu_usage_usec":      s.CPUUsageUsec,
		"cpu_pct":             s.CPUPct,
		"cpu_quota_cores":     s.CPUQuotaCores,
		"cpu_throttled_usec":  s.CPUThrottledUsec,
		"load_1":              s.Load1,
		"load_5":              s.Load5,
		"load_15":             s.Load15,
		"procs_running":       s.ProcsRunning,
	}
	for k, v := range e.cfg.Labels {
		// Defense in depth: parseLabels already filters reserved keys
		// at config load. This second check is here so a future code
		// path that injects labels from elsewhere can't silently
		// clobber a system metric.
		if _, reserved := reservedAttrKeys[k]; reserved {
			continue
		}
		attrs[k] = v
	}
	return Event{
		EventName:  eventName,
		Timestamp:  now.UTC().Format(time.RFC3339Nano),
		Attributes: attrs,
	}
}

// sendWithRetry POSTs the body, retrying on transient errors with
// jittered exponential backoff. Returns a *permanentError for 4xx so the
// caller can distinguish a misconfig from a transient blip.
func (e *Emitter) sendWithRetry(ctx context.Context, body []byte) error {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			e.retries.Add(1)
			if err := backoffWait(ctx, e.sleep, attempt); err != nil {
				return err
			}
		}
		err := e.sendOnce(ctx, body)
		if err == nil {
			return nil
		}
		var perm *permanentError
		if errors.As(err, &perm) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// backoffWait sleeps base*2^(attempt-1) plus 0–50% jitter, or returns
// early if ctx is cancelled.
func backoffWait(ctx context.Context, sleep func(time.Duration), attempt int) error {
	d := baseBackoff << (attempt - 1)
	jitter := time.Duration(rand.Int64N(int64(d) / 2))
	wait := d + jitter
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	sleep(wait)
	return nil
}

func (e *Emitter) sendOnce(ctx context.Context, body []byte) error {
	attemptCtx, cancel := context.WithTimeout(ctx, perAttemptLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		// URL parse / construction failure is permanent.
		return &permanentError{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	// req.Close = true forces a Connection: close header regardless of
	// transport state. Belt-and-suspenders with DisableKeepAlives.
	req.Close = true
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyExcerpt))
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 == 2 {
		return nil
	}
	httpErr := fmt.Errorf("non-2xx: %d %s", resp.StatusCode, strings.TrimSpace(string(excerpt)))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
		// 408 + 429 are retryable per RFC convention.
		return &permanentError{err: httpErr}
	}
	return httpErr
}

// Status is the body of /healthz. Schema is part of the public API
// (Kubernetes liveness probe contract); rename = breaking change.
type Status struct {
	OK              bool   `json:"ok"`
	LastSendOKAt    string `json:"last_send_ok_at"`
	SendFailures    uint64 `json:"send_failures"`
	PermFailures    uint64 `json:"permanent_failures"`
	Retries         uint64 `json:"retries"`
	CollectErrors   uint64 `json:"collect_errors"`
	CounterRollback uint64 `json:"cpu_counter_rollbacks"`
	CgroupV2        bool   `json:"cgroup_v2"`
}

func (e *Emitter) Status() Status {
	now := e.now()
	last := ""
	lastNS := e.lastSendOK.Load()
	if lastNS != 0 {
		last = time.Unix(0, lastNS).UTC().Format(time.RFC3339Nano)
	}

	// OK derivation: we've sent recently, OR we're inside the grace
	// window since startup. A pod with permFailures > 0 is never OK
	// (misconfig — auth, endpoint, payload).
	stale := 2*e.cfg.Interval + 30*time.Second
	if stale < healthGracePeriod {
		stale = healthGracePeriod
	}
	ok := e.permFailures.Load() == 0
	if ok {
		if lastNS == 0 {
			ok = now.Sub(e.startedAt) < healthGracePeriod
		} else {
			ok = now.Sub(time.Unix(0, lastNS)) < stale
		}
	}

	st := Status{
		OK:           ok,
		LastSendOKAt: last,
		SendFailures: e.sendFailures.Load(),
		PermFailures: e.permFailures.Load(),
		Retries:      e.retries.Load(),
	}
	if e.collector != nil {
		st.CollectErrors = e.collector.ErrorCount()
		st.CounterRollback = e.collector.CounterRollbacks()
		st.CgroupV2 = e.collector.CgroupV2()
	}
	return st
}
