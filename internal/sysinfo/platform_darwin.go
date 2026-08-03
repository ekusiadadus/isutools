//go:build darwin

package sysinfo

import (
	"encoding/binary"
	"syscall"
)

func readPlatform() (cpuModel string, memTotalBytes uint64) {
	if v, err := syscall.Sysctl("machdep.cpu.brand_string"); err == nil {
		cpuModel = v
	}
	// syscall.Sysctl returns raw little-endian bytes for integer sysctls
	// (stripping trailing NULs, which zero-padding restores).
	if v, err := syscall.Sysctl("hw.memsize"); err == nil && len(v) <= 8 {
		b := make([]byte, 8)
		copy(b, v)
		memTotalBytes = binary.LittleEndian.Uint64(b)
	}
	return cpuModel, memTotalBytes
}
