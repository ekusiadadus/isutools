package netstats

import (
	"strings"
	"testing"
)

// TestTruncate checks the two bounds a health detail respects: file contents
// are cut short because they are evidence, not data, while error messages get
// more room because their meaning often sits at the end.
func TestTruncate(t *testing.T) {
	long := strings.Repeat("x", 200)
	tests := []struct {
		name string
		got  string
		want int
	}{
		{name: "short raw is untouched", got: truncateRaw("1500"), want: 4},
		{name: "long raw is cut", got: truncateRaw(long), want: maxRawDetail},
		{name: "raw at the limit", got: truncateRaw(strings.Repeat("y", maxRawDetail)), want: maxRawDetail},
		{name: "short error is untouched", got: truncateErr("permission denied"), want: 17},
		{name: "long error is cut", got: truncateErr(long), want: maxErrDetail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.got) != tt.want {
				t.Fatalf("length = %d, want %d (%q)", len(tt.got), tt.want, tt.got)
			}
		})
	}
}

// TestNoteSetAggregates checks the three rules that keep a run's health
// readable: one note per key, no repeats, and a bounded enumeration.
func TestNoteSetAggregates(t *testing.T) {
	notes := newNoteSet()
	notes.add("", "ignored")
	notes.add(HealthSysfsUnreadable, "eth0:mtu=abc")
	notes.add(HealthSysfsUnreadable, "eth0:mtu=abc") // observed at both boundaries
	notes.add(HealthCounterRewind, "eth1")
	for i := 0; i < maxHealthDetails+5; i++ {
		notes.add(HealthLinkChanged, string(rune('a'+i)))
	}

	got := notes.notes()
	if len(got) != 3 {
		t.Fatalf("notes = %+v, want one per key", got)
	}
	if got[0].Key != HealthCounterRewind || got[1].Key != HealthLinkChanged || got[2].Key != HealthSysfsUnreadable {
		t.Fatalf("notes = %+v, want them sorted by key", got)
	}
	if got[2].Detail != "eth0:mtu=abc" {
		t.Fatalf("detail = %q, want the duplicate dropped", got[2].Detail)
	}
	if parts := strings.Split(got[1].Detail, ","); len(parts) != maxHealthDetails {
		t.Fatalf("detail lists %d items, want the enumeration capped at %d", len(parts), maxHealthDetails)
	}
}

// TestNoteSetEmpty checks that a clean run carries no health array at all,
// so the JSON stays quiet when there is nothing to say.
func TestNoteSetEmpty(t *testing.T) {
	if got := newNoteSet().notes(); got != nil {
		t.Fatalf("notes() = %+v, want nil", got)
	}
}
