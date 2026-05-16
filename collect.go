package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	cgroupRoot      = "/sys/fs/cgroup"
	cgroupV2Marker  = cgroupRoot + "/cgroup.controllers"
	memCurrentPath  = cgroupRoot + "/memory.current"
	memMaxPath      = cgroupRoot + "/memory.max"
	memSwapPath     = cgroupRoot + "/memory.swap.current"
	cpuStatPath     = cgroupRoot + "/cpu.stat"
	cpuMaxPath      = cgroupRoot + "/cpu.max"
	procUptimePath  = "/proc/uptime"
	procLoadavgPath = "/proc/loadavg"
)

// Sample is the raw set of numbers read from a single tick. -1 means
// "not available" (file missing, "max" literal, or no baseline yet).
type Sample struct {
	UptimeS          int64
	MemUsedBytes     int64
	MemLimitBytes    int64
	MemSwapUsedBytes int64
	CPUUsageUsec     int64
	CPUPct           float64
	CPUQuotaCores    float64
	CPUThrottledUsec int64
	Load1            float64
	Load5            float64
	Load15           float64
	ProcsRunning     int64
}

// zeroSample is the all-missing sample. Use this so a new field added
// later defaults to -1 (missing) instead of 0 (a real reading).
func zeroSample() Sample {
	return Sample{
		UptimeS: -1, MemUsedBytes: -1, MemLimitBytes: -1, MemSwapUsedBytes: -1,
		CPUUsageUsec: -1, CPUPct: -1, CPUQuotaCores: -1, CPUThrottledUsec: -1,
		Load1: -1, Load5: -1, Load15: -1, ProcsRunning: -1,
	}
}

type cpuBaseline struct {
	usageUsec int64
	at        time.Time
}

// Collector holds state between ticks so we can compute deltas (CPU%).
// Not safe for concurrent Collect calls — beacon calls it from one
// goroutine.
type Collector struct {
	cgroupV2   bool
	baseline   *cpuBaseline
	errLog     io.Writer
	errCount   atomic.Uint64
	rollbacks  atomic.Uint64
	loggedMu   sync.Mutex
	loggedOnce map[string]struct{}
}

func NewCollector(errLog io.Writer) *Collector {
	_, err := os.Stat(cgroupV2Marker)
	c := &Collector{
		cgroupV2:   err == nil,
		errLog:     errLog,
		loggedOnce: map[string]struct{}{},
	}
	if !c.cgroupV2 {
		fmt.Fprintln(errLog, "warn: cgroup v2 not detected; cgroup metrics will be -1")
	}
	return c
}

// ErrorCount is the running total of per-field collection errors. Read by
// /healthz so a misconfigured mount (permission errors, missing files)
// becomes visible without grepping logs.
func (c *Collector) ErrorCount() uint64 { return c.errCount.Load() }

// CounterRollbacks counts the times cpu_usage_usec went backwards
// (cgroup re-created, container reset). One-off; persistent growth is
// the signal.
func (c *Collector) CounterRollbacks() uint64 { return c.rollbacks.Load() }

// CgroupV2 reports whether cgroup v2 was detected at startup.
func (c *Collector) CgroupV2() bool { return c.cgroupV2 }

// logFirst records the error, increments the counter, and logs once per
// (source, error-class) so a permanently-broken /sys mount doesn't flood
// stderr every tick.
func (c *Collector) logFirst(source string, err error) {
	c.errCount.Add(1)
	key := source + "|" + classifyErr(err)
	c.loggedMu.Lock()
	_, seen := c.loggedOnce[key]
	if !seen {
		c.loggedOnce[key] = struct{}{}
	}
	c.loggedMu.Unlock()
	if !seen {
		fmt.Fprintf(c.errLog, "warn: collect %s: %v (further occurrences suppressed)\n", source, err)
	}
}

func classifyErr(err error) string {
	if err == nil {
		return "ok"
	}
	if os.IsNotExist(err) {
		return "missing"
	}
	if os.IsPermission(err) {
		return "perm"
	}
	return "other"
}

// Collect reads all sources and returns a Sample. Per-file errors are
// recorded in counters + first-time logged; the tick always produces a
// Sample so a single broken file doesn't take down telemetry.
func (c *Collector) Collect(now time.Time) Sample {
	s := zeroSample()

	if c.cgroupV2 {
		c.readInt64(memCurrentPath, &s.MemUsedBytes)
		c.readParse(memMaxPath, func(b []byte) {
			s.MemLimitBytes = parseMemoryMax(string(b))
		})
		c.readInt64(memSwapPath, &s.MemSwapUsedBytes)
		c.readParse(cpuStatPath, func(b []byte) {
			s.CPUUsageUsec, s.CPUThrottledUsec = parseCPUStat(string(b))
		})
		c.readParse(cpuMaxPath, func(b []byte) {
			s.CPUQuotaCores = parseCPUMax(string(b))
		})
	}

	c.readParse(procUptimePath, func(b []byte) { s.UptimeS = parseUptime(string(b)) })
	c.readParse(procLoadavgPath, func(b []byte) {
		s.Load1, s.Load5, s.Load15, s.ProcsRunning = parseLoadavg(string(b))
	})

	s.CPUPct = c.cpuPercent(s.CPUUsageUsec, now)
	if s.CPUUsageUsec >= 0 {
		c.baseline = &cpuBaseline{usageUsec: s.CPUUsageUsec, at: now}
	}
	return s
}

// cpuPercent returns the cpu% over [baseline, now). Returns -1 when:
// no baseline yet, deltaWall == 0 (same tick), or counter regression
// (cgroup re-created mid-process; counted via rollbacks).
func (c *Collector) cpuPercent(usage int64, now time.Time) float64 {
	if c.baseline == nil || usage < 0 {
		return -1
	}
	deltaCPU := usage - c.baseline.usageUsec
	deltaWall := now.Sub(c.baseline.at).Microseconds()
	if deltaWall <= 0 {
		return -1
	}
	if deltaCPU < 0 {
		c.rollbacks.Add(1)
		c.logFirst("cpu.stat:rollback", fmt.Errorf("usage_usec decreased %d -> %d", c.baseline.usageUsec, usage))
		return -1
	}
	return float64(deltaCPU) / float64(deltaWall) * 100.0
}

func (c *Collector) readInt64(path string, dst *int64) {
	v, err := readInt64File(path)
	if err != nil {
		c.logFirst(path, err)
		return
	}
	*dst = v
}

func (c *Collector) readParse(path string, parse func([]byte)) {
	b, err := os.ReadFile(path)
	if err != nil {
		c.logFirst(path, err)
		return
	}
	parse(b)
}

func readInt64File(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return n, nil
}

// parseMemoryMax reads cgroup v2 memory.max. Literal "max" → -1.
func parseMemoryMax(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "max" {
		return -1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// parseCPUStat reads cgroup v2 cpu.stat lines like "usage_usec 12345".
// Returns (usage_usec, throttled_usec). Missing lines → -1.
func parseCPUStat(s string) (int64, int64) {
	usage, throttled := int64(-1), int64(-1)
	for _, line := range strings.Split(s, "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		v, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		switch parts[0] {
		case "usage_usec":
			usage = v
		case "throttled_usec":
			throttled = v
		}
	}
	return usage, throttled
}

// parseCPUMax reads cgroup v2 cpu.max ("quota period" or "max period").
// Returns quota/period as cores, or -1 for "max" / parse failure.
func parseCPUMax(s string) float64 {
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) != 2 {
		return -1
	}
	if parts[0] == "max" {
		return -1
	}
	quota, err1 := strconv.ParseFloat(parts[0], 64)
	period, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || period == 0 || quota < 0 {
		return -1
	}
	return quota / period
}

// parseUptime reads /proc/uptime. First field is seconds since boot.
func parseUptime(s string) int64 {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return -1
	}
	f, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return -1
	}
	return int64(f)
}

// parseLoadavg reads /proc/loadavg: "0.42 0.35 0.31 2/345 12345".
func parseLoadavg(s string) (float64, float64, float64, int64) {
	parts := strings.Fields(s)
	if len(parts) < 4 {
		return -1, -1, -1, -1
	}
	l1, err1 := strconv.ParseFloat(parts[0], 64)
	l5, err2 := strconv.ParseFloat(parts[1], 64)
	l15, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		l1, l5, l15 = -1, -1, -1
	}
	running := int64(-1)
	if slash := strings.IndexByte(parts[3], '/'); slash > 0 {
		if v, err := strconv.ParseInt(parts[3][:slash], 10, 64); err == nil {
			running = v
		}
	}
	return l1, l5, l15, running
}
