package sqlstats

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	// maxQueryLen truncates normalized queries so a single huge statement
	// cannot bloat the report.
	maxQueryLen = 1000
	// maxCacheEntries bounds the normalization cache. Applications that
	// interpolate literals produce unbounded distinct strings; past the cap
	// we normalize on the fly instead of caching.
	maxCacheEntries = 50000
	// maxCacheableLen skips caching very long raw strings.
	maxCacheableLen = 4096
)

var (
	normCache sync.Map // raw query -> normalized query
	cacheSize atomic.Int64
)

var numberLiteral = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)

// normalize masks string/number literals (privacy + cardinality control),
// collapses whitespace, extracts a /* tag */ comment into a "[tag] " prefix,
// and truncates to maxQueryLen. Results are cached per raw string so
// prepared statements pay the cost once.
func normalize(q string) string {
	if v, ok := normCache.Load(q); ok {
		return v.(string)
	}
	n := computeNormalize(q)
	if len(q) <= maxCacheableLen && cacheSize.Load() < maxCacheEntries {
		if _, loaded := normCache.LoadOrStore(q, n); !loaded {
			cacheSize.Add(1)
		}
	}
	return n
}

func computeNormalize(q string) string {
	s := maskStringLiterals(q)
	s = strings.Join(strings.Fields(s), " ")
	tag := ""
	if i := strings.Index(s, "/*"); i >= 0 {
		if j := strings.Index(s[i:], "*/"); j >= 0 {
			tag = strings.TrimSpace(s[i+2 : i+j])
			s = strings.Join(strings.Fields(s[:i]+" "+s[i+j+2:]), " ")
		}
	}
	s = numberLiteral.ReplaceAllString(s, "?")
	if len(s) > maxQueryLen {
		s = s[:maxQueryLen]
	}
	if tag != "" {
		s = "[" + tag + "] " + s
	}
	return s
}

// maskStringLiterals replaces '...' literals with ?, handling '' and \'
// escapes, so interpolated values never reach the report.
func maskStringLiterals(q string) string {
	if !strings.ContainsRune(q, '\'') {
		return q
	}
	var b strings.Builder
	b.Grow(len(q))
	inString := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if !inString {
			if c == '\'' {
				inString = true
				b.WriteByte('?')
				continue
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			i++ // skip escaped character
		case '\'':
			if i+1 < len(q) && q[i+1] == '\'' {
				i++ // '' escape stays inside the literal
				continue
			}
			inString = false
		}
	}
	return b.String()
}
