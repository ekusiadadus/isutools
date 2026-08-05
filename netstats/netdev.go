package netstats

import (
	"fmt"
	"strconv"
	"strings"
)

// loopbackName is excluded from the interface table by default. Loopback
// traffic never leaves the host, so including it would inflate the totals a
// reader uses to judge the network path.
const loopbackName = "lo"

// netDevFields is the number of counters /proc/net/dev prints per interface.
// The header names them, but the header is not machine-readable, so the column
// positions below are the contract.
const netDevFields = 16

// Column positions of the counters we keep, in /proc/net/dev order:
// receive bytes packets errs drop fifo frame compressed multicast, then
// transmit bytes packets errs drop fifo colls carrier compressed.
const (
	colRxBytes   = 0
	colRxPackets = 1
	colRxErrors  = 2
	colRxDropped = 3
	colTxBytes   = 8
	colTxPackets = 9
	colTxErrors  = 10
	colTxDropped = 11
)

// devCounters is one interface's cumulative counters as read from
// /proc/net/dev. The kernel wraps these at 64 bits and resets them when a NIC
// is re-created, which is why Collect checks for a rewind.
type devCounters struct {
	RxBytes   uint64
	RxPackets uint64
	RxErrors  uint64
	RxDropped uint64
	TxBytes   uint64
	TxPackets uint64
	TxErrors  uint64
	TxDropped uint64
}

// parseNetDev parses /proc/net/dev and returns the counters per interface plus
// a description of every line it had to skip.
//
// Two shapes have to be handled. The two header lines carry no colon and are
// skipped by that fact alone. Interface lines are split on the first colon
// rather than on whitespace, because a large receive-byte counter runs into
// the name with no separating space:
//
//	eth0: 1234 …   (normal)
//	eth0:174372383 …   (counter wide enough to touch the colon)
//
// Malformed lines are reported instead of failing the whole file: one broken
// row must not cost the reader every other interface.
func parseNetDev(data []byte) (map[string]devCounters, []string) {
	devices := make(map[string]devCounters)
	var problems []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue // header line
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" || strings.ContainsAny(name, " \t|") {
			problems = append(problems, fmt.Sprintf("net/dev name=%s", truncateRaw(strings.TrimSpace(line[:colon]))))
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < netDevFields {
			problems = append(problems, fmt.Sprintf("net/dev %s fields=%d", name, len(fields)))
			continue
		}
		counters, err := parseDevCounters(fields)
		if err != nil {
			problems = append(problems, fmt.Sprintf("net/dev %s=%s", name, truncateErr(err.Error())))
			continue
		}
		if name == loopbackName {
			continue
		}
		devices[name] = counters
	}
	return devices, problems
}

// parseDevCounters reads the eight counters we keep out of the sixteen columns.
func parseDevCounters(fields []string) (devCounters, error) {
	var out devCounters
	targets := map[int]*uint64{
		colRxBytes:   &out.RxBytes,
		colRxPackets: &out.RxPackets,
		colRxErrors:  &out.RxErrors,
		colRxDropped: &out.RxDropped,
		colTxBytes:   &out.TxBytes,
		colTxPackets: &out.TxPackets,
		colTxErrors:  &out.TxErrors,
		colTxDropped: &out.TxDropped,
	}
	for column := 0; column < netDevFields; column++ {
		target, wanted := targets[column]
		if !wanted {
			continue
		}
		value, err := strconv.ParseUint(fields[column], 10, 64)
		if err != nil {
			return devCounters{}, fmt.Errorf("column %d %q", column, fields[column])
		}
		*target = value
	}
	return out, nil
}
