package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseInterval(t *testing.T) {
	cases := []struct {
		in       string
		want     time.Duration
		warnHint string
	}{
		{"", defaultInterval, ""},
		{"30", 30 * time.Second, ""},
		{"0", minInterval, "below minimum"},
		{"3", minInterval, "below minimum"},
		{"5", minInterval, ""},
		{"300", maxInterval, ""},
		{"99999", maxInterval, "above maximum"},
		{"garbage", defaultInterval, "not an integer"},
	}
	for _, c := range cases {
		got, warn := parseInterval(c.in)
		if got != c.want {
			t.Errorf("parseInterval(%q) = %s, want %s", c.in, got, c.want)
		}
		if c.warnHint == "" && warn != "" {
			t.Errorf("parseInterval(%q) unexpected warn %q", c.in, warn)
		}
		if c.warnHint != "" && !strings.Contains(warn, c.warnHint) {
			t.Errorf("parseInterval(%q) warn=%q, want substring %q", c.in, warn, c.warnHint)
		}
	}
}

func TestParseLabels(t *testing.T) {
	got, warns := parseLabels("env=prod, team=platform ,region=us-east-1")
	want := map[string]string{
		"env": "prod", "team": "platform", "region": "us-east-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warns: %v", warns)
	}
}

func TestParseLabels_Empty(t *testing.T) {
	got, warns := parseLabels("")
	if len(got) != 0 || len(warns) != 0 {
		t.Errorf("expected empty, got %v / %v", got, warns)
	}
}

func TestParseLabels_Reserved(t *testing.T) {
	got, warns := parseLabels("env=prod,pod_name=hijacked,cpu_pct=1")
	if got["env"] != "prod" {
		t.Errorf("missing env: %v", got)
	}
	if _, ok := got["pod_name"]; ok {
		t.Errorf("reserved pod_name should be dropped")
	}
	if _, ok := got["cpu_pct"]; ok {
		t.Errorf("reserved cpu_pct should be dropped")
	}
	if len(warns) != 2 {
		t.Errorf("want 2 warns, got %d: %v", len(warns), warns)
	}
}

func TestParseLabels_Malformed(t *testing.T) {
	got, warns := parseLabels("=novalue,nokey,k=v,,onlykey=")
	want := map[string]string{"k": "v", "onlykey": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// =novalue (empty key), nokey (no =) → 2 warns
	if len(warns) != 2 {
		t.Errorf("want 2 warns, got %d: %v", len(warns), warns)
	}
}

func TestParseLabels_ValueWithEquals(t *testing.T) {
	// Connection-string-style values that themselves contain '=' must
	// be preserved (IndexByte returns the first '=' only).
	got, _ := parseLabels("dsn=user=admin password=hunter2")
	if got["dsn"] != "user=admin password=hunter2" {
		t.Errorf("dsn = %q, want preservation of embedded '='", got["dsn"])
	}
}

func TestParseBool(t *testing.T) {
	for _, in := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		if !parseBool(in) {
			t.Errorf("parseBool(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "0", "false", "no", "off", "maybe"} {
		if parseBool(in) {
			t.Errorf("parseBool(%q) = true, want false", in)
		}
	}
}

// envMap returns a getenv function backed by a map, so LoadConfig can be
// tested without mutating process env.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig_Defaults(t *testing.T) {
	buf := &bytes.Buffer{}
	cfg, err := LoadConfig(envMap(map[string]string{"MESH0_API_KEY": "k"}), buf)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cfg.Endpoint != defaultEndpoint {
		t.Errorf("endpoint = %q, want default", cfg.Endpoint)
	}
	if cfg.Interval != defaultInterval {
		t.Errorf("interval = %s, want %s", cfg.Interval, defaultInterval)
	}
	if cfg.DryRun {
		t.Error("dry_run should default false")
	}
}

func TestLoadConfig_MissingKeyRequiresDryRun(t *testing.T) {
	buf := &bytes.Buffer{}
	if _, err := LoadConfig(envMap(map[string]string{}), buf); err == nil {
		t.Fatal("expected error when MESH0_API_KEY missing and not dry-run")
	}
}

func TestLoadConfig_DryRunNeedsNoKey(t *testing.T) {
	buf := &bytes.Buffer{}
	cfg, err := LoadConfig(envMap(map[string]string{"BEACON_DRY_RUN": "1"}), buf)
	if err != nil {
		t.Fatalf("dry-run should not require API key: %v", err)
	}
	if !cfg.DryRun {
		t.Error("dry_run should be true")
	}
}

func TestLoadConfig_DryRunAcceptsTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "yes"} {
		buf := &bytes.Buffer{}
		cfg, err := LoadConfig(envMap(map[string]string{"BEACON_DRY_RUN": v}), buf)
		if err != nil {
			t.Fatalf("BEACON_DRY_RUN=%q errored: %v", v, err)
		}
		if !cfg.DryRun {
			t.Errorf("BEACON_DRY_RUN=%q did not enable dry-run", v)
		}
	}
}

func TestLoadConfig_ContainerFallsBackToDeployment(t *testing.T) {
	buf := &bytes.Buffer{}
	cfg, err := LoadConfig(envMap(map[string]string{
		"MESH0_API_KEY":     "k",
		"BEACON_DEPLOYMENT": "backend",
	}), buf)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Container != "backend" {
		t.Errorf("container = %q, want fallback to deployment", cfg.Container)
	}
}

func TestLoadConfig_RejectsHTTPEndpoint(t *testing.T) {
	buf := &bytes.Buffer{}
	_, err := LoadConfig(envMap(map[string]string{
		"MESH0_API_KEY":  "k",
		"MESH0_ENDPOINT": "http://insecure.example.com/v1/events",
	}), buf)
	if err == nil {
		t.Fatal("expected error for non-https endpoint in non-dry-run")
	}
}

func TestLoadConfig_AllowsHTTPEndpointInDryRun(t *testing.T) {
	buf := &bytes.Buffer{}
	_, err := LoadConfig(envMap(map[string]string{
		"BEACON_DRY_RUN": "1",
		"MESH0_ENDPOINT": "http://localhost:8080/events",
	}), buf)
	if err != nil {
		t.Fatalf("dry-run should accept http endpoint: %v", err)
	}
}

func TestLoadConfig_RejectsMalformedEndpoint(t *testing.T) {
	buf := &bytes.Buffer{}
	_, err := LoadConfig(envMap(map[string]string{
		"MESH0_API_KEY":  "k",
		"MESH0_ENDPOINT": "::not a url",
	}), buf)
	if err == nil {
		t.Fatal("expected error for malformed endpoint")
	}
}

func TestLoadConfig_WarnsOnClampedInterval(t *testing.T) {
	buf := &bytes.Buffer{}
	cfg, err := LoadConfig(envMap(map[string]string{
		"MESH0_API_KEY":           "k",
		"BEACON_INTERVAL_SECONDS": "1",
	}), buf)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != minInterval {
		t.Errorf("interval = %s, want %s", cfg.Interval, minInterval)
	}
	if !strings.Contains(buf.String(), "clamped") {
		t.Errorf("expected clamp warning in stderr, got %q", buf.String())
	}
}
