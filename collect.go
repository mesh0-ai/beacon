package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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

// Collector holds state between ticks so we can compute deltas (CPU%).
type Collector struct {
	lastCPUUsageUsec int64
	lastSample       time.Time
	hasBaseline      bool
	cgroupV2         bool
}

func NewCollector() *Collector {
	_, err := os.Stat(cgroupV2Marker)
	c := &Collector{cgroupV2: err == nil}
	if !c.cgroupV2 {
		fmt.Fprintln(os.Stderr, "warn: cgroup v2 not detected; cgroup metrics will be -1")
	}
	return c
}

// Collect reads all sources and returns a Sample. Errors on individual
// files are swallowed and surfaced as -1 fields so a single missing
// file doesn't take down the tick.
func (c *Collector) Collect(now time.Time) Sample {
	s := Sample{
		MemUsedBytes:     -1,
		MemLimitBytes:    -1,
		MemSwapUsedBytes: -1,
		CPUUsageUsec:     -1,
		CPUPct:           -1,
		CPUQuotaCores:    -1,
		CPUThrottledUsec: -1,
		UptimeS:          -1,
		Load1:            -1,
		Load5:            -1,
		Load15:           -1,
		ProcsRunning:     -1,
	}

	if c.cgroupV2 {
		if v, err := readInt64File(memCurrentPath); err == nil {
			s.MemUsedBytes = v
		}
		if b, err := os.ReadFile(memMaxPath); err == nil {
			s.MemLimitBytes = parseMemoryMax(string(b))
		}
		if v, err := readInt64File(memSwapPath); err == nil {
			s.MemSwapUsedBytes = v
		}
		if b, err := os.ReadFile(cpuStatPath); err == nil {
			usage, throttled := parseCPUStat(string(b))
			s.CPUUsageUsec = usage
			s.CPUThrottledUsec = throttled
		}
		if b, err := os.ReadFile(cpuMaxPath); err == nil {
			s.CPUQuotaCores = parseCPUMax(string(b))
		}
	}

	if b, err := os.ReadFile(procUptimePath); err == nil {
		s.UptimeS = parseUptime(string(b))
	}
	if b, err := os.ReadFile(procLoadavgPath); err == nil {
		l1, l5, l15, running := parseLoadavg(string(b))
		s.Load1, s.Load5, s.Load15, s.ProcsRunning = l1, l5, l15, running
	}

	if c.hasBaseline && s.CPUUsageUsec >= 0 {
		deltaCPU := s.CPUUsageUsec - c.lastCPUUsageUsec
		deltaWall := now.Sub(c.lastSample).Microseconds()
		if deltaWall > 0 && deltaCPU >= 0 {
			s.CPUPct = float64(deltaCPU) / float64(deltaWall) * 100.0
		}
	}
	if s.CPUUsageUsec >= 0 {
		c.lastCPUUsageUsec = s.CPUUsageUsec
		c.lastSample = now
		c.hasBaseline = true
	}
	return s
}

func readInt64File(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
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
// Returns quota/period as cores, or -1 for "max".
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
	if err1 != nil || err2 != nil || period == 0 {
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
