package sqlrows

import (
	"testing"
	"time"
)

// Driver values reach the collector as int64, uint64, []byte, string or nil
// depending on the driver and the protocol, so the conversions are pinned for
// every shape rather than for the one a single driver happens to use.

func TestToUint64(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  uint64
	}{
		{name: "nil", value: nil},
		{name: "uint64", value: uint64(1 << 63), want: 1 << 63},
		{name: "int64", value: int64(42), want: 42},
		{name: "negative int64 clamps", value: int64(-1)},
		{name: "int", value: 7, want: 7},
		{name: "negative int clamps", value: -7},
		{name: "float64", value: float64(9), want: 9},
		{name: "negative float clamps", value: float64(-9)},
		{name: "bytes", value: []byte("12345"), want: 12345},
		{name: "string", value: " 99 ", want: 99},
		{name: "unparseable text", value: "not a number"},
		{name: "unknown type", value: struct{}{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toUint64(tc.value); got != tc.want {
				t.Fatalf("toUint64(%#v) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestToInt64(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int64
	}{
		{name: "nil", value: nil},
		{name: "int64", value: int64(-5), want: -5},
		{name: "uint64", value: uint64(5), want: 5},
		{name: "huge uint64 is refused", value: uint64(1 << 63)},
		{name: "float64", value: float64(3.9), want: 3},
		{name: "bytes", value: []byte("1000"), want: 1000},
		{name: "string", value: "-2", want: -2},
		{name: "unparseable text", value: "x"},
		{name: "unknown type", value: struct{}{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toInt64(tc.value); got != tc.want {
				t.Fatalf("toInt64(%#v) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestNullableString(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		want    string
		wantSet bool
	}{
		{name: "sql null", value: nil},
		{name: "empty string is not null", value: "", wantSet: true},
		{name: "string", value: "isuconp", want: "isuconp", wantSet: true},
		{name: "bytes", value: []byte("aaa"), want: "aaa", wantSet: true},
		{name: "unknown type counts as null", value: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nullableString(tc.value)
			if got != tc.want || ok != tc.wantSet {
				t.Fatalf("nullableString(%#v) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.wantSet)
			}
		})
	}
}

func TestToString(t *testing.T) {
	if got := toString(nil); got != "" {
		t.Fatalf("toString(nil) = %q", got)
	}
	if got := toString([]byte("x")); got != "x" {
		t.Fatalf("toString([]byte) = %q", got)
	}
	if got := toString(int64(1)); got != "" {
		t.Fatalf("toString(int64) = %q, want it ignored", got)
	}
}

func TestToTime(t *testing.T) {
	utc := time.Date(2026, 8, 4, 12, 0, 0, 500000000, time.UTC)
	cases := []struct {
		name  string
		value any
		want  time.Time
		ok    bool
	}{
		{name: "time.Time", value: utc, want: utc, ok: true},
		{
			name:  "time.Time in another zone is normalised",
			value: utc.In(time.FixedZone("JST", 9*3600)),
			want:  utc,
			ok:    true,
		},
		{name: "mysql text", value: []byte("2026-08-04 12:00:00.500000"), want: utc, ok: true},
		{name: "mysql text without fraction", value: "2026-08-04 12:00:00", want: utc.Add(-500 * time.Millisecond), ok: true},
		{name: "rfc3339", value: "2026-08-04T12:00:00.5Z", want: utc, ok: true},
		{name: "empty", value: ""},
		{name: "garbage", value: "not a timestamp"},
		{name: "nil", value: nil},
		{name: "unknown type", value: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := toTime(tc.value)
			if ok != tc.ok {
				t.Fatalf("toTime(%#v) ok = %v, want %v", tc.value, ok, tc.ok)
			}
			if ok && !got.Equal(tc.want) {
				t.Fatalf("toTime(%#v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestTruthy(t *testing.T) {
	cases := []struct {
		value any
		want  bool
	}{
		{value: int64(1), want: true},
		{value: int64(0)},
		{value: uint64(2), want: true},
		{value: "ON", want: true},
		{value: "on", want: true},
		{value: []byte("YES"), want: true},
		{value: []byte("OFF")},
		{value: "NO"},
		{value: "false"},
		{value: true, want: true},
		{value: float64(1), want: true},
		{value: nil},
		{value: "maybe"},
	}
	for _, tc := range cases {
		if got := truthy(tc.value); got != tc.want {
			t.Fatalf("truthy(%#v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestTruncateBytes(t *testing.T) {
	if got := truncateBytes("short", 512); got != "short" {
		t.Fatalf("truncateBytes kept %q", got)
	}
	if got := truncateBytes("abcdef", 3); got != "abc" {
		t.Fatalf("truncateBytes = %q, want %q", got, "abc")
	}
	// A multi-byte rune must not be cut in half, or the section renders as
	// replacement characters.
	if got := truncateBytes("投稿", 4); got != "投" {
		t.Fatalf("truncateBytes = %q, want the whole first rune", got)
	}
	if got := truncateBytes("投", 2); got != "" {
		t.Fatalf("truncateBytes = %q, want an empty string rather than half a rune", got)
	}
}

// TestDigestTextIsTruncatedOnTheWayIn covers the defensive cut on the value
// the server returns, independent of the server-side LEFT().
func TestDigestTextIsTruncatedOnTheWayIn(t *testing.T) {
	long := make([]byte, digestTextMaxBytes*2)
	for i := range long {
		long[i] = 'a'
	}
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	server.texts = [][]any{{"aaa", string(long)}}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	if _, err := c.CaptureBaseline(t.Context(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	q.answer(digestRows, digestRow("isuconp", "aaa", DigestRow{CountStar: 5, TimerWait: 50}))
	final, err := c.CaptureFinal(t.Context(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}
	text := sampleOfResult(t, final).Targets["db1"].Texts["aaa"]
	if len(text) != digestTextMaxBytes {
		t.Fatalf("stored text is %d bytes, want it capped at %d", len(text), digestTextMaxBytes)
	}
}
