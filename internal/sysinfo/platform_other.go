//go:build !linux && !darwin

package sysinfo

func readPlatform() (cpuModel string, memTotalBytes uint64) {
	return "", 0
}
