package netstats

import (
	"fmt"
	"strconv"
	"strings"
)

// parseSockstat extracts the TCP line of /proc/net/sockstat.
//
// The line is a bag of key/value pairs whose membership and order have changed
// across kernel versions, so it is parsed as pairs rather than by position:
//
//	TCP: inuse 12 orphan 0 tw 5 alloc 20 mem 3
//
// Keys we do not use are ignored, and keys that are absent stay zero, so a
// kernel that stops emitting "orphan" degrades one field instead of the file.
func parseSockstat(data []byte) (TCPSummary, error) {
	pairs, ok := sockstatLine(data, "TCP:")
	if !ok {
		return TCPSummary{}, fmt.Errorf("netstats: sockstat has no TCP: line")
	}
	var out TCPSummary
	for key, raw := range pairs {
		target := (*int64)(nil)
		switch key {
		case "inuse":
			target = &out.InUse
		case "tw":
			target = &out.TimeWait
		case "orphan":
			target = &out.Orphan
		default:
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return TCPSummary{}, fmt.Errorf("netstats: sockstat TCP %s=%q: %w", key, raw, err)
		}
		*target = value
	}
	return out, nil
}

// parseSockstat6 extracts TCP6 inuse from /proc/net/sockstat6. The v6 file
// carries no tw or orphan counters, which is why only in-use is reported for
// IPv6.
func parseSockstat6(data []byte) (int64, error) {
	pairs, ok := sockstatLine(data, "TCP6:")
	if !ok {
		return 0, fmt.Errorf("netstats: sockstat6 has no TCP6: line")
	}
	raw, ok := pairs["inuse"]
	if !ok {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("netstats: sockstat6 TCP6 inuse=%q: %w", raw, err)
	}
	return value, nil
}

// sockstatLine returns the key/value pairs of the line starting with prefix. A
// trailing key with no value is dropped rather than treated as an error: a
// truncated read should cost that one field only.
func sockstatLine(data []byte, prefix string) (map[string]string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != prefix {
			continue
		}
		pairs := make(map[string]string, (len(fields)-1)/2)
		for i := 1; i+1 < len(fields); i += 2 {
			pairs[fields[i]] = fields[i+1]
		}
		return pairs, true
	}
	return nil, false
}
