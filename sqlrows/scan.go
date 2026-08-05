package sqlrows

import (
	"strconv"
	"strings"
	"time"
)

// Driver values are scanned into any rather than into concrete types, because
// the same column arrives as int64, uint64, []byte or string depending on the
// driver, the protocol (text vs prepared) and the column's signedness. The
// helpers below normalise that without letting a surprising type panic: a
// value that cannot be interpreted degrades to zero, which shows up as a
// missing measurement rather than as a crashed application.

// toUint64 interprets a driver value as an unsigned counter.
func toUint64(v any) uint64 {
	switch n := v.(type) {
	case nil:
		return 0
	case uint64:
		return n
	case int64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case int:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case float64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case []byte:
		return parseUint(string(n))
	case string:
		return parseUint(n)
	default:
		return 0
	}
}

// toInt64 interprets a driver value as a signed counter (Uptime).
func toInt64(v any) int64 {
	switch n := v.(type) {
	case nil:
		return 0
	case int64:
		return n
	case uint64:
		if n > 1<<62 {
			return 0
		}
		return int64(n)
	case float64:
		return int64(n)
	case []byte:
		return parseInt(string(n))
	case string:
		return parseInt(n)
	default:
		return 0
	}
}

// toString interprets a driver value as text.
func toString(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}

// nullableString distinguishes SQL NULL from the empty string, which is what
// the overflow rule (SCHEMA_NAME IS NULL AND DIGEST IS NULL) turns on.
func nullableString(v any) (string, bool) {
	switch s := v.(type) {
	case nil:
		return "", false
	case string:
		return s, true
	case []byte:
		return string(s), true
	default:
		return "", false
	}
}

// dbTimeLayouts are the textual forms a server timestamp can take when the
// driver was not asked to parse it. The inspector DSN sets parseTime=true, so
// this is the fallback path for other drivers.
var dbTimeLayouts = []string{
	"2006-01-02 15:04:05.999999",
	"2006-01-02T15:04:05.999999Z07:00",
	"2006-01-02 15:04:05",
}

// toTime interprets a driver value as a UTC instant. The inspector session is
// pinned to UTC, so a timestamp without a zone is UTC by construction.
func toTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), true
	case []byte:
		return parseDBTime(string(t))
	case string:
		return parseDBTime(t)
	default:
		return time.Time{}, false
	}
}

func parseDBTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range dbTimeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func parseUint(s string) uint64 {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parseInt(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// truthy interprets the many ways a server reports a boolean setting: 1/0,
// ON/OFF, YES/NO, TRUE/FALSE.
func truthy(v any) bool {
	switch s := strings.ToUpper(strings.TrimSpace(toString(v))); s {
	case "1", "ON", "YES", "TRUE":
		return true
	case "0", "OFF", "NO", "FALSE", "":
		// Fall through to the numeric interpretation for drivers that hand
		// back an integer rather than text.
	}
	switch n := v.(type) {
	case int64:
		return n != 0
	case uint64:
		return n != 0
	case bool:
		return n
	case float64:
		return n != 0
	}
	return false
}

// truncateBytes caps a digest text at n bytes without splitting a UTF-8 rune,
// so a truncated statement stays renderable.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start reports whether b begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
