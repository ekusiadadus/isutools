//go:build linux

package netstats

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"testing"
)

// TestSmokeAgainstRealHost runs a whole boundary pair against the host's own
// /proc and /sys. Every other test in this package is a fixture, so this is
// the only place where a real kernel's formatting is exercised: it is the test
// that would notice /proc/net/dev growing a column.
//
// It skips rather than fails wherever the environment legitimately cannot
// answer — a container without /sys, or a namespace with only loopback — so
// that CI reports parsing problems and nothing else.
func TestSmokeAgainstRealHost(t *testing.T) {
	c := New(os.DirFS("/proc"), os.DirFS("/sys"))
	ctx := context.Background()

	base, err := c.CaptureBaseline(ctx, "smoke", 1)
	if err != nil {
		t.Skipf("procfs is not readable here: %v", err)
	}
	final, err := c.CaptureFinal(ctx, "smoke", 1)
	if err != nil {
		t.Fatalf("CaptureFinal() error = %v", err)
	}
	defer c.Release(base.Handle)
	defer c.Release(final.Handle)

	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	stats, ok := value.(*NetworkStats)
	if !ok {
		t.Fatalf("Collect() = %T, want *NetworkStats", value)
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	t.Logf("network section: %s", encoded)

	if len(stats.Interfaces) == 0 {
		t.Skip("this host has no interface besides loopback")
	}
	if _, err := fs.ReadDir(os.DirFS("/sys"), "class/net"); err != nil {
		t.Skipf("sysfs is not mounted here: %v", err)
	}
	found := false
	for _, iface := range stats.Interfaces {
		if iface.MTU != 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no interface reported an MTU, got %s", encoded)
	}
}
