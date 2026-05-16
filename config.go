package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultEndpoint = "https://api.mesh0.ai/v1/events"
	defaultInterval = 30
	minInterval     = 5
	maxInterval     = 300
	healthAddr      = "127.0.0.1:8128"
)

// reservedAttrKeys cannot be overridden by BEACON_LABELS.
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
	APIKey      string
	Endpoint    string
	Interval    int
	PodName     string
	NodeName    string
	Namespace   string
	Deployment  string
	Container   string
	Labels      map[string]string
	DryRun      bool
	LogLevel    string
}

func LoadConfig() (*Config, error) {
	c := &Config{
		APIKey:     os.Getenv("MESH0_API_KEY"),
		Endpoint:   envOr("MESH0_ENDPOINT", defaultEndpoint),
		PodName:    os.Getenv("BEACON_POD_NAME"),
		NodeName:   os.Getenv("BEACON_NODE_NAME"),
		Namespace:  os.Getenv("BEACON_NAMESPACE"),
		Deployment: os.Getenv("BEACON_DEPLOYMENT"),
		Container:  os.Getenv("BEACON_CONTAINER"),
		LogLevel:   envOr("BEACON_LOG_LEVEL", "info"),
	}
	if c.Container == "" {
		c.Container = c.Deployment
	}
	c.DryRun = os.Getenv("BEACON_DRY_RUN") == "1"
	c.Interval = parseInterval(os.Getenv("BEACON_INTERVAL_SECONDS"))

	labels, warns := parseLabels(os.Getenv("BEACON_LABELS"))
	c.Labels = labels
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "warn: %s\n", w)
	}

	if !c.DryRun && c.APIKey == "" {
		return nil, errors.New("MESH0_API_KEY required unless BEACON_DRY_RUN=1")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseInterval(s string) int {
	if s == "" {
		return defaultInterval
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultInterval
	}
	if n < minInterval {
		return minInterval
	}
	if n > maxInterval {
		return maxInterval
	}
	return n
}

// parseLabels parses "k=v,k2=v2" into a map. Reserved keys are skipped and
// returned as warning strings. Malformed pairs are skipped silently.
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
			continue
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		if k == "" {
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
