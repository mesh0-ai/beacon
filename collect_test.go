package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestParseMemoryMax(t *testing.T) {
	if got := parseMemoryMax(read(t, "cgroup_memory_max_unlimited.txt")); got != -1 {
		t.Errorf("unlimited = %d, want -1", got)
	}
	if got := parseMemoryMax(read(t, "cgroup_memory_max_1g.txt")); got != 1073741824 {
		t.Errorf("1g = %d, want 1073741824", got)
	}
	if got := parseMemoryMax("garbage"); got != -1 {
		t.Errorf("garbage = %d, want -1", got)
	}
}

func TestParseCPUStat(t *testing.T) {
	usage, throttled := parseCPUStat(read(t, "cpu_stat.txt"))
	if usage != 123456789 {
		t.Errorf("usage = %d, want 123456789", usage)
	}
	if throttled != 4567 {
		t.Errorf("throttled = %d, want 4567", throttled)
	}
}

func TestParseCPUStat_Missing(t *testing.T) {
	usage, throttled := parseCPUStat("nr_periods 0\n")
	if usage != -1 || throttled != -1 {
		t.Errorf("expected -1/-1, got %d/%d", usage, throttled)
	}
}

func TestParseCPUMax(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{read(t, "cgroup_cpu_max_quota.txt"), 0.5},
		{read(t, "cgroup_cpu_max_unlimited.txt"), -1},
		{"100000 0", -1},
		{"oops", -1},
		{"400000 100000", 4.0}, // multi-core
		{"-1 100000", -1},      // negative quota
	}
	for _, c := range cases {
		if got := parseCPUMax(c.in); got != c.want {
			t.Errorf("parseCPUMax(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseUptime(t *testing.T) {
	if got := parseUptime(read(t, "proc_uptime.txt")); got != 12345 {
		t.Errorf("uptime = %d, want 12345", got)
	}
	if got := parseUptime(""); got != -1 {
		t.Errorf("empty = %d, want -1", got)
	}
}

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, running := parseLoadavg(read(t, "proc_loadavg.txt"))
	if l1 != 0.42 || l5 != 0.35 || l15 != 0.31 {
		t.Errorf("load = %v %v %v, want 0.42 0.35 0.31", l1, l5, l15)
	}
	if running != 2 {
		t.Errorf("running = %d, want 2", running)
	}
}

func TestParseLoadavg_Short(t *testing.T) {
	l1, _, _, running := parseLoadavg("only one field")
	if l1 != -1 || running != -1 {
		t.Errorf("expected -1s, got l1=%v running=%d", l1, running)
	}
}

func TestZeroSample_AllMissing(t *testing.T) {
	// Guard: every numeric field starts at -1 so a newly-added field
	// can't silently default to 0 (a valid reading).
	s := zeroSample()
	if s.UptimeS != -1 || s.MemUsedBytes != -1 || s.MemLimitBytes != -1 ||
		s.MemSwapUsedBytes != -1 || s.CPUUsageUsec != -1 || s.CPUPct != -1 ||
		s.CPUQuotaCores != -1 || s.CPUThrottledUsec != -1 ||
		s.Load1 != -1 || s.Load5 != -1 || s.Load15 != -1 || s.ProcsRunning != -1 {
		t.Fatalf("zeroSample has a non-sentinel field: %+v", s)
	}
}

// newTestCollector returns a Collector with a controllable cgroup state
// and a stderr buffer so tests can assert on warnings.
func newTestCollector(cgroupV2 bool) (*Collector, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	c := &Collector{
		cgroupV2:   cgroupV2,
		errLog:     buf,
		loggedOnce: map[string]struct{}{},
	}
	return c, buf
}

func TestCollect_FirstTickHasNoCPUPercent(t *testing.T) {
	// Collector starts with no baseline; CPU% must be -1 even when
	// usage is readable.
	c, _ := newTestCollector(false) // cgroup off → all -1
	now := time.Unix(1000, 0)
	s := c.Collect(now)
	if s.CPUPct != -1 {
		t.Fatalf("first-tick CPUPct = %v, want -1", s.CPUPct)
	}
}

func TestCpuPercent(t *testing.T) {
	c, _ := newTestCollector(false)
	// No baseline → -1.
	if got := c.cpuPercent(1000, time.Unix(10, 0)); got != -1 {
		t.Errorf("no baseline: got %v want -1", got)
	}
	// Establish baseline.
	c.baseline = &cpuBaseline{usageUsec: 1_000_000, at: time.Unix(10, 0)}
	// 1s wall, 500ms CPU → 50%.
	got := c.cpuPercent(1_500_000, time.Unix(11, 0))
	if got < 49.99 || got > 50.01 {
		t.Errorf("50%% case: got %v", got)
	}
	// deltaWall == 0 → -1 (avoid div-by-zero).
	if got := c.cpuPercent(2_000_000, time.Unix(10, 0)); got != -1 {
		t.Errorf("same instant: got %v want -1", got)
	}
	// Counter rollback → -1, rollback counter increments.
	c.baseline = &cpuBaseline{usageUsec: 1_000_000, at: time.Unix(10, 0)}
	if got := c.cpuPercent(500_000, time.Unix(11, 0)); got != -1 {
		t.Errorf("rollback: got %v want -1", got)
	}
	if c.CounterRollbacks() != 1 {
		t.Errorf("rollback counter = %d, want 1", c.CounterRollbacks())
	}
}

func TestCollect_BaselineTracked(t *testing.T) {
	c, _ := newTestCollector(false)
	c.Collect(time.Unix(1000, 0))
	// /proc/uptime, /proc/loadavg will be read on darwin too — but
	// without cgroup, CPUUsageUsec stays -1 and baseline is not set.
	if c.baseline != nil && c.cgroupV2 {
		// On a real cgroup v2 host this would set baseline; in the test
		// we forced cgroupV2=false, so baseline must remain nil.
		t.Fatalf("unexpected baseline with cgroupV2=false")
	}
}

func TestLogFirst_OncePerSourceErrorClass(t *testing.T) {
	c, buf := newTestCollector(true)
	err := os.ErrNotExist
	c.logFirst("/some/path", err)
	c.logFirst("/some/path", err) // same class, suppressed
	count := bytes.Count(buf.Bytes(), []byte("collect"))
	if count != 1 {
		t.Errorf("expected 1 log line for repeated same-class error, got %d: %s", count, buf.String())
	}
	if c.ErrorCount() != 2 {
		t.Errorf("error counter should still tick up: got %d want 2", c.ErrorCount())
	}
	// Different class on same path → new log line.
	c.logFirst("/some/path", os.ErrPermission)
	count = bytes.Count(buf.Bytes(), []byte("collect"))
	if count != 2 {
		t.Errorf("expected 2 log lines after new error class, got %d", count)
	}
}

func TestClassifyErr(t *testing.T) {
	if classifyErr(nil) != "ok" {
		t.Error("nil → ok")
	}
	if classifyErr(os.ErrNotExist) != "missing" {
		t.Error("ErrNotExist → missing")
	}
	if classifyErr(os.ErrPermission) != "perm" {
		t.Error("ErrPermission → perm")
	}
	if classifyErr(io.EOF) != "other" {
		t.Error("EOF → other")
	}
}
