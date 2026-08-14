package sqlstats

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	maxQueryLen     = 1000
	maxCacheEntries = 50000
	maxCacheableLen = 4096
	maxTagLen       = 64
)

var (
	normCache sync.Map // normalizeCacheKey -> normalized query
	cacheSize atomic.Int64
)

// CommentTagPolicy controls whether the first safe block comment participates
// in the aggregate key. All comments are scrubbed in both modes.
type CommentTagPolicy string

const (
	CommentTagsOn  CommentTagPolicy = "on"
	CommentTagsOff CommentTagPolicy = "off"
	EnvCommentTags                  = "ISUTOOLS_SQL_COMMENT_TAGS"
)

type normalizeCacheKey struct {
	query  string
	policy CommentTagPolicy
}

var defaultCommentTagPolicy = sync.OnceValue(func() CommentTagPolicy {
	policy, _ := ResolveCommentTagPolicy(os.Getenv)
	return policy
})

// ResolveCommentTagPolicy returns a secret-free reason code. Unknown values
// preserve the historical tag-on behavior.
func ResolveCommentTagPolicy(getenv func(string) string) (CommentTagPolicy, string) {
	if getenv == nil {
		return CommentTagsOn, "default-on"
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvCommentTags))) {
	case "off", "0", "false", "no", "disabled":
		return CommentTagsOff, "configured-off"
	case "", "on", "1", "true", "yes", "enabled":
		return CommentTagsOn, "default-on"
	default:
		return CommentTagsOn, "invalid-value"
	}
}

var (
	numberLiteral = regexp.MustCompile(`(?i)(?:\b\d+(?:\.\d*)?|\.\d+)(?:e[+-]?\d+)?\b`)
	hexLiteral    = regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b|\b0b[01]+\b|\b[XB]\?`)
	placeholderIN = regexp.MustCompile(`(?i)\bIN\s*\(\s*(?:\?|\$\?)(?:\s*,\s*(?:\?|\$\?))*\s*\)`)
	safeTag       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]{0,63}$`)
)

// normalize masks literals and comments (privacy + cardinality control),
// collapses whitespace, extracts the first safe /* tag */ into a prefix, and
// bounds the complete result. Results are cached per raw string so prepared
// statements pay the cost once.
func normalize(q string) string {
	policy := defaultCommentTagPolicy()
	key := normalizeCacheKey{query: q, policy: policy}
	if v, ok := normCache.Load(key); ok {
		return v.(string)
	}
	n := computeNormalizeWithPolicy(q, policy)
	if len(q) <= maxCacheableLen && cacheSize.Load() < maxCacheEntries {
		if _, loaded := normCache.LoadOrStore(key, n); !loaded {
			cacheSize.Add(1)
		}
	}
	return n
}

func computeNormalizeWithPolicy(q string, policy CommentTagPolicy) string {
	s, tag := scrubSQL(q, policy == CommentTagsOn)
	s = strings.Join(strings.Fields(s), " ")
	s = hexLiteral.ReplaceAllString(s, "?")
	s = numberLiteral.ReplaceAllString(s, "?")
	s = placeholderIN.ReplaceAllString(s, "IN (?)")
	if tag != "" {
		s = "[" + tag + "] " + s
	}
	if len(s) > maxQueryLen {
		s = s[:maxQueryLen]
	}
	return s
}

// scrubSQL is deliberately conservative rather than dialect-specific. When
// syntax is ambiguous it removes content rather than risking credentials or
// PII in a persisted report.
func scrubSQL(q string, extractTag bool) (string, string) {
	var b strings.Builder
	b.Grow(min(len(q), maxQueryLen))
	tag := ""
	for i := 0; i < len(q); {
		switch {
		case i+1 < len(q) && q[i] == '/' && q[i+1] == '*':
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				b.WriteByte(' ')
				return b.String(), tag
			}
			comment := strings.TrimSpace(q[i+2 : i+2+end])
			if extractTag && tag == "" && len(comment) <= maxTagLen && safeTag.MatchString(comment) {
				tag = comment
			}
			b.WriteByte(' ')
			i += end + 4
		case i+1 < len(q) && q[i] == '-' && q[i+1] == '-':
			if end := strings.IndexByte(q[i+2:], '\n'); end >= 0 {
				b.WriteByte(' ')
				i += end + 3
			} else {
				return b.String(), tag
			}
		case q[i] == '#':
			if end := strings.IndexByte(q[i+1:], '\n'); end >= 0 {
				b.WriteByte(' ')
				i += end + 2
			} else {
				return b.String(), tag
			}
		case q[i] == '\'':
			b.WriteByte('?')
			i = skipQuoted(q, i, '\'')
		case q[i] == '$':
			if next, ok := skipDollarQuoted(q, i); ok {
				b.WriteByte('?')
				i = next
			} else {
				b.WriteByte(q[i])
				i++
			}
		default:
			b.WriteByte(q[i])
			i++
		}
	}
	return b.String(), tag
}

func skipQuoted(q string, start int, quote byte) int {
	for i := start + 1; i < len(q); i++ {
		switch q[i] {
		case '\\':
			if i+1 < len(q) {
				i++
			}
		case quote:
			if i+1 < len(q) && q[i+1] == quote {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(q)
}

func skipDollarQuoted(q string, start int) (int, bool) {
	endTag := strings.IndexByte(q[start+1:], '$')
	if endTag < 0 || endTag > 64 {
		return start, false
	}
	endTag += start + 1
	name := q[start+1 : endTag]
	if name != "" {
		for i := 0; i < len(name); i++ {
			if !isDollarTagByte(name[i], i) {
				return start, false
			}
		}
	}
	delim := q[start : endTag+1]
	closeAt := strings.Index(q[endTag+1:], delim)
	if closeAt < 0 {
		return len(q), true
	}
	return endTag + 1 + closeAt + len(delim), true
}

func isDollarTagByte(c byte, index int) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' ||
		index > 0 && c >= '0' && c <= '9'
}
