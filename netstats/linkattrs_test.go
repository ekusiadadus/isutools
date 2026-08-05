package netstats

import (
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

// sysFSWith builds a sysfs containing the given /sys-relative files.
func sysFSWith(files map[string]string) fstest.MapFS {
	out := make(fstest.MapFS, len(files))
	for name, content := range files {
		out[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return out
}

// linkAttrPath is the sysfs path of one interface attribute.
func linkAttrPath(ifname, attr string) string {
	return "class/net/" + ifname + "/" + attr
}

// errFS injects read errors that fstest.MapFS cannot express, such as the
// EINVAL the kernel returns for the speed of a link that is down.
type errFS struct {
	fs.FS
	errs map[string]error
}

func (f errFS) Open(name string) (fs.File, error) {
	if err, ok := f.errs[name]; ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return f.FS.Open(name)
}

// readAttrs is the test-side shorthand for reading one interface's attributes.
func readAttrs(t *testing.T, sysFS fs.FS, ifname string) linkAttrs {
	t.Helper()
	attrs, err := readLinkAttrs(sysFS, ifname)
	if err != nil {
		t.Fatalf("readLinkAttrs() error = %v", err)
	}
	return attrs
}

// TestReadLinkAttrs_SpeedMinusOne fixes the one rejection that stays silent.
// -1 is the kernel's documented answer for "no ethtool speed", so a host with
// veth and bridge devices would otherwise carry a health note on every run.
func TestReadLinkAttrs_SpeedMinusOne(t *testing.T) {
	attrs := readAttrs(t, sysFSWith(map[string]string{
		linkAttrPath("eth0", "speed"): "-1\n",
	}), "eth0")
	if attrs.SpeedMbit != 0 {
		t.Fatalf("SpeedMbit = %d, want 0 so the field is omitted", attrs.SpeedMbit)
	}
	if len(attrs.Notes) != 0 {
		t.Fatalf("notes = %v, want none for the documented unknown value", attrs.Notes)
	}
}

// TestReadLinkAttrs_SpeedMissing checks that an absent attribute is routine.
func TestReadLinkAttrs_SpeedMissing(t *testing.T) {
	attrs := readAttrs(t, sysFSWith(map[string]string{
		linkAttrPath("eth0", "mtu"): "1500\n",
	}), "eth0")
	if attrs.SpeedMbit != 0 || len(attrs.Notes) != 0 {
		t.Fatalf("attrs = %+v, want a silent omission", attrs)
	}
	if attrs.MTU != 1500 {
		t.Fatalf("MTU = %d, want the sibling attribute to still be read", attrs.MTU)
	}
}

// TestReadLinkAttrs_SpeedNonNumeric checks that garbage is reported.
func TestReadLinkAttrs_SpeedNonNumeric(t *testing.T) {
	attrs := readAttrs(t, sysFSWith(map[string]string{
		linkAttrPath("eth0", "speed"): "unknown\n",
	}), "eth0")
	if attrs.SpeedMbit != 0 {
		t.Fatalf("SpeedMbit = %d, want 0", attrs.SpeedMbit)
	}
	if len(attrs.Notes) != 1 || attrs.Notes[0] != "eth0:speed=unknown" {
		t.Fatalf("notes = %v, want [eth0:speed=unknown]", attrs.Notes)
	}
}

// TestReadLinkAttrs_SpeedOutOfRange separates a corrupt value from the -1 that
// means "unknown": both yield no speed, only one is reported.
func TestReadLinkAttrs_SpeedOutOfRange(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantNotes int
	}{
		{name: "zero", raw: "0", wantNotes: 1},
		{name: "negative other than unknown", raw: "-2", wantNotes: 1},
		{name: "above one terabit", raw: "1000001", wantNotes: 1},
		{name: "unknown marker", raw: "-1", wantNotes: 0},
		{name: "lower bound", raw: "1", wantNotes: 0},
		{name: "upper bound", raw: "1000000", wantNotes: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := readAttrs(t, sysFSWith(map[string]string{
				linkAttrPath("eth0", "speed"): tt.raw,
			}), "eth0")
			accepted := tt.wantNotes == 0 && tt.raw != "-1"
			if accepted && attrs.SpeedMbit == 0 {
				t.Fatalf("SpeedMbit = 0, want %s accepted", tt.raw)
			}
			if !accepted && attrs.SpeedMbit != 0 {
				t.Fatalf("SpeedMbit = %d, want 0 for %s", attrs.SpeedMbit, tt.raw)
			}
			if len(attrs.Notes) != tt.wantNotes {
				t.Fatalf("notes = %v, want %d", attrs.Notes, tt.wantNotes)
			}
		})
	}
}

// TestReadLinkAttrs_MTUMissing checks that a NIC without an MTU attribute
// stays quiet: namespaces and older kernels legitimately lack it.
func TestReadLinkAttrs_MTUMissing(t *testing.T) {
	attrs := readAttrs(t, sysFSWith(map[string]string{
		linkAttrPath("eth0", "speed"): "1000\n",
	}), "eth0")
	if attrs.MTU != 0 {
		t.Fatalf("MTU = %d, want 0 so the JSON key disappears", attrs.MTU)
	}
	if len(attrs.Notes) != 0 {
		t.Fatalf("notes = %v, want none for an absent attribute", attrs.Notes)
	}
}

// TestReadLinkAttrs_MTUNonNumeric checks every shape of unparseable content.
// MTU has no "unknown" spelling, so all of them are corruption.
func TestReadLinkAttrs_MTUNonNumeric(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "word", raw: "abc\n", want: "eth0:mtu=abc"},
		{name: "empty", raw: "", want: "eth0:mtu="},
		{name: "two values", raw: "1500 1500", want: "eth0:mtu=1500 1500"},
		{name: "scientific notation", raw: "1.5e3", want: "eth0:mtu=1.5e3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := readAttrs(t, sysFSWith(map[string]string{
				linkAttrPath("eth0", "mtu"): tt.raw,
			}), "eth0")
			if attrs.MTU != 0 {
				t.Fatalf("MTU = %d, want 0", attrs.MTU)
			}
			if len(attrs.Notes) != 1 || attrs.Notes[0] != tt.want {
				t.Fatalf("notes = %v, want [%s]", attrs.Notes, tt.want)
			}
		})
	}
}

// TestReadLinkAttrs_MTUOutOfRange checks values that parse but cannot be real.
func TestReadLinkAttrs_MTUOutOfRange(t *testing.T) {
	for _, raw := range []string{"0", "-1", "67", "65537", "99999"} {
		t.Run(raw, func(t *testing.T) {
			attrs := readAttrs(t, sysFSWith(map[string]string{
				linkAttrPath("eth0", "mtu"): raw,
			}), "eth0")
			if attrs.MTU != 0 {
				t.Fatalf("MTU = %d, want 0 for out-of-range %s", attrs.MTU, raw)
			}
			if len(attrs.Notes) != 1 || attrs.Notes[0] != "eth0:mtu="+raw {
				t.Fatalf("notes = %v, want [eth0:mtu=%s]", attrs.Notes, raw)
			}
		})
	}
}

// TestReadLinkAttrs_MTUBoundary pins the accepted range one unit either side,
// so a future edit cannot widen or narrow it unnoticed.
func TestReadLinkAttrs_MTUBoundary(t *testing.T) {
	tests := []struct {
		raw  string
		want int64
	}{
		{raw: "67", want: 0},
		{raw: "68", want: 68},
		{raw: "65536", want: 65536},
		{raw: "65537", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			attrs := readAttrs(t, sysFSWith(map[string]string{
				linkAttrPath("eth0", "mtu"): tt.raw,
			}), "eth0")
			if attrs.MTU != tt.want {
				t.Fatalf("MTU = %d, want %d", attrs.MTU, tt.want)
			}
		})
	}
}

// TestReadLinkAttrs_MTUJumbo checks that a jumbo frame MTU passes through
// untouched: no rounding, no unit conversion, and above all no judgement —
// whether jumbo frames help depends on every hop of the path, not on this NIC.
func TestReadLinkAttrs_MTUJumbo(t *testing.T) {
	attrs := readAttrs(t, sysFSWith(map[string]string{
		linkAttrPath("eth0", "mtu"): "9000\n",
	}), "eth0")
	if attrs.MTU != 9000 {
		t.Fatalf("MTU = %d, want 9000 verbatim", attrs.MTU)
	}
	if len(attrs.Notes) != 0 {
		t.Fatalf("notes = %v, want none: jumbo frames are displayed, not judged", attrs.Notes)
	}
}

// TestReadLinkAttrs_MTUTrailingWhitespace checks the trimming both attributes
// need: sysfs terminates values with a newline.
func TestReadLinkAttrs_MTUTrailingWhitespace(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
	}{
		{name: "trailing newline", raw: "9000\n", want: 9000},
		{name: "surrounding spaces", raw: " 1500 \n", want: 1500},
		{name: "carriage return", raw: "1500\r\n", want: 1500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attrs := readAttrs(t, sysFSWith(map[string]string{
				linkAttrPath("eth0", "mtu"): tt.raw,
			}), "eth0")
			if attrs.MTU != tt.want {
				t.Fatalf("MTU = %d, want %d", attrs.MTU, tt.want)
			}
		})
	}
}

// TestReadLinkAttrsReadErrors separates the errors worth reporting from the
// ones that are part of normal operation.
func TestReadLinkAttrsReadErrors(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantNotes int
	}{
		{name: "EINVAL from a down link", err: syscall.EINVAL, wantNotes: 0},
		{name: "not exist", err: fs.ErrNotExist, wantNotes: 0},
		{name: "permission denied", err: fs.ErrPermission, wantNotes: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysFS := errFS{
				FS: sysFSWith(map[string]string{
					linkAttrPath("eth0", "speed"): "1000\n",
					linkAttrPath("eth0", "mtu"):   "1500\n",
				}),
				errs: map[string]error{linkAttrPath("eth0", "mtu"): tt.err},
			}
			attrs := readAttrs(t, sysFS, "eth0")
			if attrs.MTU != 0 {
				t.Fatalf("MTU = %d, want 0 when the read fails", attrs.MTU)
			}
			if attrs.SpeedMbit != 1000 {
				t.Fatalf("SpeedMbit = %d, want the readable sibling to survive", attrs.SpeedMbit)
			}
			if len(attrs.Notes) != tt.wantNotes {
				t.Fatalf("notes = %v, want %d", attrs.Notes, tt.wantNotes)
			}
			if tt.wantNotes == 1 && !strings.HasPrefix(attrs.Notes[0], "eth0:mtu=") {
				t.Fatalf("note = %q, want the eth0:mtu= prefix", attrs.Notes[0])
			}
		})
	}
}

// TestReadLinkAttrsSysfsNotInjected checks the one condition that is an error
// rather than a note: without sysfs there is nothing to attribute per NIC.
func TestReadLinkAttrsSysfsNotInjected(t *testing.T) {
	if _, err := readLinkAttrs(nil, "eth0"); err == nil {
		t.Fatal("readLinkAttrs(nil) error = nil, want an error")
	}
}

// TestReadLinkAttrsBothAttributes checks the happy path where both attributes
// are present, including a jumbo MTU next to a real link speed.
func TestReadLinkAttrsBothAttributes(t *testing.T) {
	attrs := readAttrs(t, sysFSWith(map[string]string{
		linkAttrPath("eth0", "speed"): "10000\n",
		linkAttrPath("eth0", "mtu"):   "9000\n",
	}), "eth0")
	if attrs.SpeedMbit != 10000 || attrs.MTU != 9000 || len(attrs.Notes) != 0 {
		t.Fatalf("attrs = %+v, want speed 10000 and MTU 9000 with no notes", attrs)
	}
}
