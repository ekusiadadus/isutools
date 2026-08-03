package sysinfo

import (
	"strings"
	"testing"
)

const sampleCPUInfoX86 = `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz
cpu MHz		: 2600.000

processor	: 1
model name	: Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz
`

const sampleCPUInfoARM = `processor	: 0
BogoMIPS	: 48.00
Hardware	: BCM2835
`

const sampleMemInfo = `MemTotal:       20097024 kB
MemFree:        16000000 kB
`

func TestParseCPUModelX86(t *testing.T) {
	got := parseCPUModel(strings.NewReader(sampleCPUInfoX86))
	want := "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz"
	if got != want {
		t.Errorf("parseCPUModel = %q, want %q", got, want)
	}
}

func TestParseCPUModelARMFallback(t *testing.T) {
	if got := parseCPUModel(strings.NewReader(sampleCPUInfoARM)); got != "BCM2835" {
		t.Errorf("parseCPUModel = %q, want Hardware fallback", got)
	}
}

func TestParseCPUModelEmpty(t *testing.T) {
	if got := parseCPUModel(strings.NewReader("")); got != "" {
		t.Errorf("parseCPUModel = %q, want empty", got)
	}
}

func TestParseMemTotal(t *testing.T) {
	got := parseMemTotal(strings.NewReader(sampleMemInfo))
	want := uint64(20097024) * 1024
	if got != want {
		t.Errorf("parseMemTotal = %d, want %d", got, want)
	}
}

func TestGetAlwaysUsable(t *testing.T) {
	info := Get()
	if info.NumCPU <= 0 {
		t.Errorf("NumCPU = %d, want > 0", info.NumCPU)
	}
	if info.OS == "" {
		t.Error("OS must not be empty")
	}
	if info.CPUModel == "" {
		t.Error("CPUModel must fall back to a non-empty placeholder")
	}
}
