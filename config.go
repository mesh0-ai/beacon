package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultEndpoint = "https://api.mesh0.ai/v1/events"
	defaultInterval = 30 * time.Second
	minInterval     = 5 * time.Second
	maxInterval     = 300 * time.Second
	healthAddr      = "127.0.0.1:8128"
)

// reservedAttrKeys cannot be overridden by BEACON_LABELS. Keep in sync with
// the attribute keys written in (*Emitter).buildEvent — a rename in one
// without the other lets a user label silently clobber a real metric.
var reservedAttrKeys = map[string]struct{}{
	"pod_name": {}, "node_name": {}, "namespace": {},
	"deployment": {}, "container": {},
	"uptime_s":            {},
	"mem_used_bytes":      {},
	"mem_limit_bytes":     {},
	"mem_swap_used_bytes": {},
	"cpu_usage_usec":      {},
	"cpu_pct":             {},
	"cpu_quota_cores":     {},
	"cpu_throttled_usec":  {},
	"load_1":              {}, "load_5": {}, "load_15": {},
	"procs_running": {},
}

type Config struct {
	APIKey     string
	Endpoint   string
	Interval   time.Duration
	PodName    string
	NodeName   string
	Namespace  string
	Deployment string
	Container  string
	Labels     map[string]string
	DryRun     bool
}

// LoadConfig reads the environment into a *Config. Warnings are written
// to warnOut so callers (and tests) control the sink; fatal misconfig
// returns an error.
func LoadConfig(getenv func(string) string, warnOut io.Writer) (*Config, error) {
	c := &Config{
		APIKey:     getenv("MESH0_API_KEY"),
		Endpoint:   envOr(getenv, "MESH0_ENDPOINT", defaultEndpoint),
		PodName:    getenv("BEACON_POD_NAME"),
		NodeName:   getenv("BEACON_NODE_NAME"),
		Namespace:  getenv("BEACON_NAMESPACE"),
		Deployment: getenv("BEACON_DEPLOYMENT"),
		Container:  getenv("BEACON_CONTAINER"),
	}
	if c.Container == "" {
		c.Container = c.Deployment
	}
	c.DryRun = parseBool(getenv("BEACON_DRY_RUN"))

	interval, warn := parseInterval(getenv("BEACON_INTERVAL_SECONDS"))
	c.Interval = interval
	if warn != "" {
		fmt.Fprintf(warnOut, "warn: %s\n", warn)
	}

	labels, warns := parseLabels(getenv("BEACON_LABELS"))
	c.Labels = labels
	for _, w := range warns {
		fmt.Fprintf(warnOut, "warn: %s\n", w)
	}

	if !c.DryRun && c.APIKey == "" {
		return nil, errors.New("MESH0_API_KEY required unless BEACON_DRY_RUN=1")
	}
	if err := validateEndpoint(c.Endpoint, c.DryRun); err != nil {
		return nil, err
	}
	return c, nil
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// parseBool accepts the conventional truthy strings; anything else is false.
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// parseInterval returns the clamped duration plus a warning if the input
// was unparseable or out of range. Empty input is the silent default.
func parseInterval(s string) (time.Duration, string) {
	if s == "" {
		return defaultInterval, ""
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultInterval, fmt.Sprintf("BEACON_INTERVAL_SECONDS=%q is not an integer; using default %ds", s, int(defaultInterval/time.Second))
	}
	d := time.Duration(n) * time.Second
	if d < minInterval {
		return minInterval, fmt.Sprintf("BEACON_INTERVAL_SECONDS=%d below minimum; clamped to %ds", n, int(minInterval/time.Second))
	}
	if d > maxInterval {
		return maxInterval, fmt.Sprintf("BEACON_INTERVAL_SECONDS=%d above maximum; clamped to %ds", n, int(maxInterval/time.Second))
	}
	return d, ""
}

// validateEndpoint requires HTTPS in non-dry-run mode so we never ship
// telemetry (or an Authorization header) in cleartext by accident.
func validateEndpoint(endpoint string, dryRun bool) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("MESH0_ENDPOINT %q is not a valid URL: %w", endpoint, err)
	}
	if u.Host == "" {
		return fmt.Errorf("MESH0_ENDPOINT %q is missing a host", endpoint)
	}
	if !dryRun && u.Scheme != "https" {
		return fmt.Errorf("MESH0_ENDPOINT must use https (got scheme %q)", u.Scheme)
	}
	return nil
}

// parseLabels parses "k=v,k2=v2" into a map. Reserved keys and malformed
// pairs are both surfaced as warnings; silent drops here turned into
// missing dashboard dimensions during the MVP.
func parseLabels(s string) (map[string]string, []string) {
	out := map[string]string{}
	var warns []string
	if s == "" {
		return out, warns
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			warns = append(warns, fmt.Sprintf("BEACON_LABELS: skipping malformed pair %q (expected k=v)", pair))
			continue
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		if k == "" {
			warns = append(warns, fmt.Sprintf("BEACON_LABELS: skipping pair %q with empty key", pair))
			continue
		}
		if _, reserved := reservedAttrKeys[k]; reserved {
			warns = append(warns, fmt.Sprintf("BEACON_LABELS: dropping reserved key %q", k))
			continue
		}
		out[k] = v
	}
	return out, warns
}

// loadFromEnv is the production entry point.
func loadFromEnv() (*Config, error) {
	return LoadConfig(os.Getenv, os.Stderr)
}
