// Package sysinfo resolves static host facts (CPU model, core count, total
// memory, OS) shown in every report so measurements are always attributable
// to the hardware they ran on.
package sysinfo

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Info describes the host the process runs on. Fields degrade gracefully:
// unknown values are placeholders, never empty strings.
type Info struct {
	CPUModel      string `json:"cpu_model"`
	NumCPU        int    `json:"num_cpu"`
	MemTotalBytes uint64 `json:"mem_total_bytes"`
	OS            string `json:"os"`
	Hostname      string `json:"hostname"`
}

var (
	once   sync.Once
	cached Info
)

// Get returns host facts, resolved once per process.
func Get() Info {
	once.Do(func() {
		model, mem := readPlatform()
		if model == "" {
			model = "unknown"
		}
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		cached = Info{
			CPUModel:      model,
			NumCPU:        runtime.NumCPU(),
			MemTotalBytes: mem,
			OS:            runtime.GOOS + "/" + runtime.GOARCH,
			Hostname:      host,
		}
	})
	return cached
}

// parseCPUModel extracts the CPU model from /proc/cpuinfo content,
// falling back to ARM-style "Hardware"/"Model" keys.
func parseCPUModel(r io.Reader) string {
	fallback := ""
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, value, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch key {
		case "model name":
			return value
		case "Hardware", "Model":
			if fallback == "" {
				fallback = value
			}
		}
	}
	return fallback
}

// parseMemTotal extracts MemTotal (bytes) from /proc/meminfo content.
func parseMemTotal(r io.Reader) uint64 {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
