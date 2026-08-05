package netstats

import (
	"fmt"
	"strings"
	"testing"
)

// netDevHeader is the exact two-line header the kernel prints. It carries no
// colon, which is what lets the parser skip it without pattern matching.
const netDevHeader = "Inter-|   Receive                                                |  Transmit\n" +
	" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"

// devLine renders one /proc/net/dev row with the given sixteen columns.
func devLine(name string, columns [netDevFields]uint64) string {
	parts := make([]string, 0, netDevFields)
	for _, column := range columns {
		parts = append(parts, fmt.Sprint(column))
	}
	return fmt.Sprintf("%6s: %s\n", name, strings.Join(parts, " "))
}

// TestParseNetDevColumnPositions pins the sixteen column positions. Every
// column gets a distinct value, so a one-position shift in either direction
// changes at least one field we keep.
func TestParseNetDevColumnPositions(t *testing.T) {
	var columns [netDevFields]uint64
	for i := range columns {
		columns[i] = uint64(100 + i)
	}
	devices, problems := parseNetDev([]byte(netDevHeader + devLine("eth0", columns)))
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	got, ok := devices["eth0"]
	if !ok {
		t.Fatalf("devices = %v, want eth0", devices)
	}
	want := devCounters{
		RxBytes: 100, RxPackets: 101, RxErrors: 102, RxDropped: 103,
		TxBytes: 108, TxPackets: 109, TxErrors: 110, TxDropped: 111,
	}
	if got != want {
		t.Fatalf("counters = %+v, want %+v", got, want)
	}
}

// TestParseNetDevNameForms covers both spacings of the name column. The kernel
// pads the name to eight characters but does not separate it from a wide
// receive-byte counter, so "eth0:1234" is a real line, not a corrupt one.
func TestParseNetDevNameForms(t *testing.T) {
	counters := "174372383 1234 0 0 0 0 0 0 987654321 4321 0 0 0 0 0 0"
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "space after colon", line: "  eth0: " + counters, want: "eth0"},
		{name: "no space after colon", line: "  eth0:" + counters, want: "eth0"},
		{name: "no leading padding", line: "eth0:" + counters, want: "eth0"},
		{name: "vlan name with dot", line: " eth0.10:" + counters, want: "eth0.10"},
		{name: "name with at sign", line: "veth1@if7:" + counters, want: "veth1@if7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			devices, problems := parseNetDev([]byte(netDevHeader + tt.line + "\n"))
			if len(problems) != 0 {
				t.Fatalf("problems = %v, want none", problems)
			}
			got, ok := devices[tt.want]
			if !ok {
				t.Fatalf("devices = %v, want key %q", devices, tt.want)
			}
			if got.RxBytes != 174372383 || got.TxBytes != 987654321 {
				t.Fatalf("counters = %+v", got)
			}
		})
	}
}

// TestParseNetDevSkipsHeaderAndLoopback fixes the two rows that never reach
// the table: the header, and loopback traffic that never leaves the host.
func TestParseNetDevSkipsHeaderAndLoopback(t *testing.T) {
	var columns [netDevFields]uint64
	columns[colRxBytes] = 1
	columns[colTxBytes] = 2
	data := netDevHeader + devLine("lo", columns) + devLine("eth0", columns) + devLine("eth1", columns)

	devices, problems := parseNetDev([]byte(data))
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(devices) != 2 {
		t.Fatalf("devices = %v, want eth0 and eth1 only", devices)
	}
	if _, present := devices[loopbackName]; present {
		t.Fatalf("loopback must be excluded by default, got %v", devices)
	}
}

// TestParseNetDevMalformedLines checks that a broken row costs the reader that
// row only. Failing the whole file would hide every healthy interface.
func TestParseNetDevMalformedLines(t *testing.T) {
	var columns [netDevFields]uint64
	columns[colRxBytes] = 7

	tests := []struct {
		name        string
		line        string
		wantProblem string
	}{
		{
			name:        "too few columns",
			line:        "  eth9: 1 2 3\n",
			wantProblem: "fields=3",
		},
		{
			name:        "non numeric column",
			line:        "  eth9: a 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16\n",
			wantProblem: "column 0",
		},
		{
			name:        "empty name",
			line:        "   : 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16\n",
			wantProblem: "name=",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := netDevHeader + devLine("eth0", columns) + tt.line
			devices, problems := parseNetDev([]byte(data))
			if _, ok := devices["eth0"]; !ok || len(devices) != 1 {
				t.Fatalf("devices = %v, want the healthy eth0 only", devices)
			}
			if len(problems) != 1 || !strings.Contains(problems[0], tt.wantProblem) {
				t.Fatalf("problems = %v, want one containing %q", problems, tt.wantProblem)
			}
		})
	}
}

// TestParseNetDevEmptyFile checks the degenerate input: no devices, no
// complaints, no panic.
func TestParseNetDevEmptyFile(t *testing.T) {
	devices, problems := parseNetDev(nil)
	if len(devices) != 0 || len(problems) != 0 {
		t.Fatalf("devices = %v, problems = %v; want both empty", devices, problems)
	}
}
