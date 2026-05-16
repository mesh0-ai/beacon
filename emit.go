package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

const eventName = "pod.health"

type Event struct {
	EventName  string                 `json:"event_name"`
	Timestamp  string                 `json:"timestamp"`
	Attributes map[string]interface{} `json:"attributes"`
}

// Emitter ships one Event per tick to mesh0 (or stdout in dry-run).
// Counters are atomic so /healthz can read them without locking.
type Emitter struct {
	cfg          *Config
	client       *http.Client
	sendFailures atomic.Uint64
	lastSendOK   atomic.Int64 // unix nanos; 0 = never
}

func NewEmitter(cfg *Config) *Emitter {
	return &Emitter{cfg: cfg, client: newHTTPClient()}
}

// newHTTPClient builds a leak-resistant client per plan.md: no keep-alives,
// no HTTP/2, hard 5s ceiling.
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
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

// Emit builds an event from a sample and ships it. Best-effort: any
// error is logged + counted but never propagated to the caller.
func (e *Emitter) Emit(ctx context.Context, s Sample, now time.Time) {
	ev := e.buildEvent(s, now)
	body, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(stderr, "warn: marshal: %v\n", err)
		return
	}
	if e.cfg.DryRun {
		fmt.Fprintln(stdout, string(body))
		e.lastSendOK.Store(now.UnixNano())
		return
	}
	if err := e.send(ctx, body); err != nil {
		e.sendFailures.Add(1)
		fmt.Fprintf(stderr, "warn: send: %v\n", err)
		return
	}
	e.lastSendOK.Store(now.UnixNano())
}

func (e *Emitter) buildEvent(s Sample, now time.Time) Event {
	attrs := map[string]interface{}{
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
		attrs[k] = v
	}
	return Event{
		EventName:  eventName,
		Timestamp:  now.UTC().Format("2006-01-02T15:04:05.000Z"),
		Attributes: attrs,
	}
}

func (e *Emitter) send(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Close = true
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("non-2xx: %d", resp.StatusCode)
	}
	return nil
}

// Status is the body of /healthz.
type Status struct {
	OK           bool   `json:"ok"`
	LastSendOKAt string `json:"last_send_ok_at"`
	SendFailures uint64 `json:"send_failures"`
}

func (e *Emitter) Status() Status {
	last := ""
	if ns := e.lastSendOK.Load(); ns != 0 {
		last = time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
	}
	return Status{OK: true, LastSendOKAt: last, SendFailures: e.sendFailures.Load()}
}
