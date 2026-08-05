package queryplan

import (
	"strconv"
	"strings"
	"time"
)

// Driver values arrive as int64, uint64, []byte, string or time.Time
// depending on the driver, the protocol and the column, so they are scanned
// into any and normalised here. A value that cannot be interpreted degrades to
// a zero value rather than panicking: this package runs inside the measured
// application, and a surprising driver type must cost a plan, not the process.

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

// toInt64 interprets a driver value as a signed integer.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		if n > 1<<62 {
			return 0
		}
		return int64(n)
	case int:
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

// dbTimeLayouts are the textual forms a server timestamp takes when the driver
// was not asked to parse it. Inspector connections set parseTime=true, so this
// is the fallback path.
var dbTimeLayouts = []string{
	"2006-01-02 15:04:05.999999",
	"2006-01-02T15:04:05.999999Z07:00",
	"2006-01-02 15:04:05",
}

// toTime interprets a driver value as a UTC instant. The session is pinned to
// UTC by the registry, so a timestamp without a zone is UTC by construction —
// which is what makes comparing it with the database's own clock readings, and
// never with this process's clock, meaningful.
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

func parseInt(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// truncateBytes caps a plan cell without splitting a UTF-8 rune, so the value
// stays renderable and a pathological Extra column cannot inflate a snapshot.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
