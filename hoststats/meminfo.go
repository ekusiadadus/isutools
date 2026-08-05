package hoststats

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// bytesPerKB is meminfo's unit. The kernel prints "kB" and means KiB.
const bytesPerKB = 1024

// parseMeminfo converts /proc/meminfo into bytes.
//
// Only MemTotal is required: a meminfo without it is not a meminfo, and this
// is the one source whose absence means "no sample at all". Any other line
// that is missing or unparsable is left at zero rather than failing the read —
// kernels differ in which fields they print, and losing the whole host section
// over an unexpected field would be a far worse outcome than a zero.
func parseMeminfo(data []byte) (MemRaw, error) {
	var mem MemRaw
	seenTotal := false
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := parseMeminfoLine(line)
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			mem.TotalBytes = value
			seenTotal = true
		case "MemAvailable":
			mem.AvailableBytes = value
		case "Cached":
			mem.CachedBytes = value
		case "Dirty":
			mem.DirtyBytes = value
		case "SwapTotal":
			mem.SwapTotalBytes = value
		case "SwapFree":
			mem.SwapFreeBytes = value
		}
	}
	if !seenTotal {
		return MemRaw{}, errors.New("meminfo: MemTotal is missing")
	}
	return mem, nil
}

// parseMeminfoLine splits "MemTotal:  16316948 kB" into a key and a byte
// count. It reports false for anything it cannot read, including the
// occasional non-numeric or overflowing value.
func parseMeminfoLine(line string) (string, uint64, bool) {
	colon := strings.IndexByte(line, ':')
	if colon <= 0 {
		return "", 0, false
	}
	key := strings.TrimSpace(line[:colon])
	fields := strings.Fields(line[colon+1:])
	if key == "" || len(fields) == 0 {
		return "", 0, false
	}
	value, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	// A missing unit means the value is already in bytes; the kernel prints
	// unitless values for counters such as HugePages_Total.
	if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
		if value > math.MaxUint64/bytesPerKB {
			return "", 0, false
		}
		value *= bytesPerKB
	}
	return key, value, true
}

// parseVMStat extracts the cumulative pgmajfault counter: major faults are the
// page faults that had to wait for a disk, which is what "the box ran out of
// page cache" looks like from userspace.
func parseVMStat(data []byte) (uint64, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "pgmajfault" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("vmstat: pgmajfault %q: %w", fields[1], err)
		}
		return value, nil
	}
	return 0, errors.New("vmstat: pgmajfault is missing")
}
