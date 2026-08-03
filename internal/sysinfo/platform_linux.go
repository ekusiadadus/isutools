//go:build linux

package sysinfo

import "os"

func readPlatform() (cpuModel string, memTotalBytes uint64) {
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		cpuModel = parseCPUModel(f)
		_ = f.Close()
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		memTotalBytes = parseMemTotal(f)
		_ = f.Close()
	}
	return cpuModel, memTotalBytes
}
