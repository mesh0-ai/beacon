package main

import (
	"os"
	"path/filepath"
	"testing"
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
	if got := parseCPUMax(read(t, "cgroup_cpu_max_quota.txt")); got != 0.5 {
		t.Errorf("quota = %v, want 0.5", got)
	}
	if got := parseCPUMax(read(t, "cgroup_cpu_max_unlimited.txt")); got != -1 {
		t.Errorf("unlimited = %v, want -1", got)
	}
	if got := parseCPUMax("100000 0"); got != -1 {
		t.Errorf("zero period = %v, want -1", got)
	}
	if got := parseCPUMax("oops"); got != -1 {
		t.Errorf("garbage = %v, want -1", got)
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
