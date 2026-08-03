package procstats

import (
	"math"
	"testing"
)

func TestParseSystemStatBreakdown(t *testing.T) {
	data := []byte("cpu 100 50 200 600 50 30 20 10 5 5\ncpu0 0 0 0 0 0 0 0 0 0 0\ncpu1 0 0 0 0 0 0 0 0 0 0\n")
	times, cpus, err := parseSystemStat(data)
	if err != nil {
		t.Fatalf("parseSystemStat: %v", err)
	}
	if cpus != 2 {
		t.Errorf("cpus = %d, want 2", cpus)
	}
	if times.user != 150 { // user + nice
		t.Errorf("user = %d, want 150", times.user)
	}
	if times.system != 250 { // system + irq + softirq
		t.Errorf("system = %d, want 250", times.system)
	}
	if times.idle != 600 || times.iowait != 50 || times.steal != 10 {
		t.Errorf("idle/iowait/steal = %d/%d/%d", times.idle, times.iowait, times.steal)
	}
	if times.total != 1060 {
		t.Errorf("total = %d, want 1060", times.total)
	}
}

func TestComputeCPUTotal(t *testing.T) {
	start := cpuTimes{total: 1000, user: 100, system: 100, idle: 700, iowait: 100}
	end := cpuTimes{total: 2000, user: 500, system: 200, idle: 950, iowait: 150}
	got := computeCPUTotal(start, end)
	if got == nil {
		t.Fatal("computeCPUTotal returned nil")
	}
	approx := func(a, b float64) bool { return math.Abs(a-b) < 0.01 }
	if !approx(got.UserPercent, 40) || !approx(got.SystemPercent, 10) {
		t.Errorf("user/system = %.1f/%.1f, want 40/10", got.UserPercent, got.SystemPercent)
	}
	if !approx(got.IdlePercent, 25) || !approx(got.IOWaitPercent, 5) {
		t.Errorf("idle/iowait = %.1f/%.1f, want 25/5", got.IdlePercent, got.IOWaitPercent)
	}
	if !approx(got.BusyPercent, 70) {
		t.Errorf("busy = %.1f, want 70", got.BusyPercent)
	}
}

func TestComputeCPUTotalEmptyInterval(t *testing.T) {
	times := cpuTimes{total: 1000}
	if got := computeCPUTotal(times, times); got != nil {
		t.Errorf("empty interval must return nil, got %#v", got)
	}
}
