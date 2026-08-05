package sqlrows

import "strings"

// classifyTokenLimit bounds the work classification may do. A digest text is
// truncated to digestTextMaxBytes, so this is only a guard against a
// pathological input.
const classifyTokenLimit = 256

// keywordKind maps a leading keyword onto a statement family. WITH is absent
// on purpose: a common table expression's family is decided by the statement
// that follows the definitions, not by WITH itself.
var keywordKind = map[string]StatementKind{
	"SELECT":  KindSelect,
	"INSERT":  KindDML,
	"UPDATE":  KindDML,
	"DELETE":  KindDML,
	"REPLACE": KindDML,
}

// Classify decides which statement family a digest text belongs to.
//
// The family is what makes the numbers comparable: the examined-per-sent ratio
// is a SELECT diagnostic, while DML is read through affected rows. A
// WITH ... SELECT is classified as a SELECT — looking only at the first
// keyword would file every common table expression under "other" and hide the
// heaviest queries of an application that uses them.
func Classify(text string) StatementKind {
	if text == MissingQueryText {
		return KindOther
	}
	tokens := tokenizeSQL(text, classifyTokenLimit)
	if len(tokens) == 0 {
		return KindOther
	}
	if tokens[0].word == "WITH" {
		// Skip the CTE definitions: they are parenthesised, so the statement
		// body is the first keyword that appears back at depth zero.
		for _, token := range tokens[1:] {
			if token.depth != 0 || token.word == "" {
				continue
			}
			if kind, ok := keywordKind[token.word]; ok {
				return kind
			}
		}
		return KindOther
	}
	if kind, ok := keywordKind[tokens[0].word]; ok {
		return kind
	}
	return KindOther
}

// sqlToken is one word of a statement together with its parenthesis depth.
// Quoted identifiers and string literals are emitted with an empty word, so
// they act as separators and can never be mistaken for a keyword.
type sqlToken struct {
	word  string
	depth int
}

// tokenizeSQL splits a statement into keyword candidates, skipping comments
// and treating quoted text as opaque.
func tokenizeSQL(text string, maxTokens int) []sqlToken {
	out := make([]sqlToken, 0, 8)
	depth := 0
	for i := 0; i < len(text) && len(out) < maxTokens; {
		c := text[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == ';':
			i++
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case c == '/' && i+1 < len(text) && text[i+1] == '*':
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return out
			}
			i += end + 4
		case c == '-' && i+1 < len(text) && text[i+1] == '-':
			i = lineEnd(text, i)
		case c == '#':
			i = lineEnd(text, i)
		case c == '`' || c == '\'' || c == '"':
			i = skipQuoted(text, i)
			out = append(out, sqlToken{depth: depth})
		case isWordByte(c):
			start := i
			for i < len(text) && isWordByte(text[i]) {
				i++
			}
			out = append(out, sqlToken{word: strings.ToUpper(text[start:i]), depth: depth})
		default:
			i++
		}
	}
	return out
}

// skipQuoted returns the index just past the quoted run starting at i,
// honouring both doubled quotes and backslash escapes.
func skipQuoted(text string, i int) int {
	quote := text[i]
	for j := i + 1; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++
		case quote:
			if j+1 < len(text) && text[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(text)
}

// lineEnd returns the index just past the end of the line starting at i.
func lineEnd(text string, i int) int {
	if nl := strings.IndexByte(text[i:], '\n'); nl >= 0 {
		return i + nl + 1
	}
	return len(text)
}

// isWordByte reports whether c can appear inside an unquoted SQL word.
func isWordByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '$' || c == '@':
		return true
	default:
		return c >= 0x80 // identifiers may hold multi-byte characters
	}
}
