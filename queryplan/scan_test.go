package queryplan

import (
	"strings"
	"testing"
	"time"
)

// The same column arrives as int64, []byte, string or time.Time depending on
// the driver and the protocol, so the helpers below normalise it. A value that
// cannot be interpreted has to degrade to a zero value: this package runs
// inside the measured application, and a surprising driver type must cost a
// plan rather than the process.

func TestToString(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{name: "string", in: "NO", want: "NO"},
		{name: "bytes", in: []byte("NO"), want: "NO"},
		{name: "null", in: nil, want: ""},
		{name: "unexpected type", in: 12.5, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toString(tc.in); got != tc.want {
				t.Fatalf("toString(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestToInt64(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int64
	}{
		{name: "int64", in: int64(1024), want: 1024},
		{name: "uint64", in: uint64(1024), want: 1024},
		{name: "int", in: 1024, want: 1024},
		{name: "float64", in: 1024.9, want: 1024},
		{name: "bytes", in: []byte(" 1024 "), want: 1024},
		{name: "string", in: "1024", want: 1024},
		{name: "unparseable text", in: "many", want: 0},
		{name: "null", in: nil, want: 0},
		{name: "an unsigned value too large to be signed", in: uint64(1) << 63, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toInt64(tc.in); got != tc.want {
				t.Fatalf("toInt64(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestToTime(t *testing.T) {
	want := time.Date(2026, 8, 4, 12, 0, 15, 500000000, time.UTC)
	tests := []struct {
		name  string
		in    any
		want  time.Time
		valid bool
	}{
		{name: "parsed by the driver", in: want, want: want, valid: true},
		{name: "text", in: "2026-08-04 12:00:15.5", want: want, valid: true},
		{name: "bytes", in: []byte("2026-08-04 12:00:15.5"), want: want, valid: true},
		{name: "whole seconds", in: "2026-08-04 12:00:15", want: want.Truncate(time.Second), valid: true},
		{name: "rfc3339", in: "2026-08-04T12:00:15.5Z", want: want, valid: true},
		{name: "empty", in: "", valid: false},
		{name: "not a timestamp", in: "yesterday", valid: false},
		{name: "null", in: nil, valid: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := toTime(tc.in)
			if ok != tc.valid {
				t.Fatalf("toTime(%v) ok = %v, want %v", tc.in, ok, tc.valid)
			}
			if ok && !got.Equal(tc.want) {
				t.Fatalf("toTime(%v) = %v, want %v", tc.in, got, tc.want)
			}
			if ok && got.Location() != time.UTC {
				t.Fatalf("toTime(%v) is in %v, want UTC: the session is pinned to UTC", tc.in, got.Location())
			}
		})
	}
}

func TestTruncateBytes(t *testing.T) {
	if got := truncateBytes("short", 32); got != "short" {
		t.Fatalf("truncateBytes kept %q", got)
	}
	long := strings.Repeat("a", maxPlanCell+10)
	if got := truncateBytes(long, maxPlanCell); len(got) != maxPlanCell {
		t.Fatalf("truncated to %d bytes, want %d", len(got), maxPlanCell)
	}
	// A multi-byte rune must not be cut in half, or the value stops being
	// valid UTF-8 and cannot be marshalled.
	multi := strings.Repeat("あ", 8) // three bytes each
	got := truncateBytes(multi, 10)
	if len(got) != 9 || !strings.HasSuffix(got, "あ") {
		t.Fatalf("truncateBytes cut a rune in half: %q (%d bytes)", got, len(got))
	}
}

func TestDedupeAccounts(t *testing.T) {
	granted := []account{{name: "r_a", host: "%"}, {name: "r_b", host: "%"}}
	active := []account{{name: "r_b", host: "%"}, {name: "r_c", host: "localhost"}}
	got := dedupeAccounts(granted, active)
	if len(got) != 3 {
		t.Fatalf("accounts = %+v, want three distinct roles", got)
	}
	if got[0].name != "r_a" || got[1].name != "r_b" || got[2].name != "r_c" {
		t.Fatalf("accounts = %+v, want first-seen order so the statement is deterministic", got)
	}
	if want := "`r_a`@`%`"; got[0].quoted() != want {
		t.Fatalf("quoted = %q, want %q", got[0].quoted(), want)
	}
	if bare := (account{name: "r_a"}); bare.quoted() != "`r_a`" {
		t.Fatalf("quoted = %q, want a name with no host part", bare.quoted())
	}
}
