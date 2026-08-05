package queryplan

import "strings"

// Identifier handling for the two places this package has to read or write
// MySQL identifiers: the schema it puts into USE, and the account and role
// names SHOW GRANTS reports.
//
// The grant text is server output, not user input, but it is parsed with the
// same suspicion either way: an identifier this parser gets wrong is an
// identifier whose privileges are judged against the wrong object, and the
// judgement is the only thing standing between an EXPLAIN and a credential
// that can write.

// maxSchemaLen is MySQL's own identifier limit.
const maxSchemaLen = 64

// validSchema reports whether a schema name may be quoted into USE.
//
// The set is deliberately narrower than MySQL's: an identifier is not a bind
// parameter, so the only safe policy is an allowlist of bytes that cannot
// terminate a quoted identifier or start a comment. A schema outside it is a
// skipped target, not an escaped string.
func validSchema(s string) bool {
	if s == "" || len(s) > maxSchemaLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == '_' || c == '$':
		default:
			return false
		}
	}
	return true
}

// quoteIdent wraps s in backticks, doubling any backtick inside it. It is used
// only on values that already passed a validator or came back from the server.
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// account is a MySQL account or role: a name and, optionally, a host part.
type account struct {
	name string
	host string
}

// quoted renders the account the way SHOW GRANTS ... USING expects it.
func (a account) quoted() string {
	if a.host == "" {
		return quoteIdent(a.name)
	}
	return quoteIdent(a.name) + "@" + quoteIdent(a.host)
}

// parseAccount reads one account starting at i and returns the offset after
// it.
func parseAccount(s string, i int) (account, int, bool) {
	name, next, ok := scanIdent(s, i)
	if !ok {
		return account{}, i, false
	}
	i = next
	if i >= len(s) || s[i] != '@' {
		return account{name: name}, i, true
	}
	host, next, ok := scanIdent(s, i+1)
	if !ok {
		return account{}, i, false
	}
	return account{name: name, host: host}, next, true
}

// parseAccountList parses the comma-separated form CURRENT_ROLE() and the role
// side of a role grant both use. An empty list is not a valid list: the caller
// asked because it believed there was at least one role.
func parseAccountList(s string) ([]account, bool) {
	var out []account
	i := skipSpace(s, 0)
	for i < len(s) {
		acct, next, ok := parseAccount(s, i)
		if !ok {
			return nil, false
		}
		out = append(out, acct)
		i = skipSpace(s, next)
		if i >= len(s) {
			break
		}
		if s[i] != ',' {
			return nil, false
		}
		i = skipSpace(s, i+1)
	}
	return out, len(out) > 0
}

// scanIdent reads one identifier — backtick, single- or double-quoted, or bare
// — and returns its unquoted value plus the offset after it.
func scanIdent(s string, i int) (string, int, bool) {
	i = skipSpace(s, i)
	if i >= len(s) {
		return "", i, false
	}
	switch s[i] {
	case '`', '\'', '"':
		return scanQuoted(s, i)
	default:
		return scanBare(s, i)
	}
}

// scanQuoted reads a quoted identifier. A doubled quote is a literal one, and
// inside single quotes a backslash escapes the next byte, which is how MySQL
// writes host patterns.
func scanQuoted(s string, i int) (string, int, bool) {
	quote := s[i]
	var b strings.Builder
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '\\' && quote != '`' && j+1 < len(s):
			b.WriteByte(s[j+1])
			j++
		case s[j] == quote && j+1 < len(s) && s[j+1] == quote:
			b.WriteByte(quote)
			j++
		case s[j] == quote:
			return b.String(), j + 1, true
		default:
			b.WriteByte(s[j])
		}
	}
	return "", i, false // unterminated
}

// identStop are the bytes that end a bare identifier. '.' is one of them
// because an object reference is db.table and the parser must not swallow the
// separator.
const identStop = " \t\r\n.,@()`'\";"

func scanBare(s string, i int) (string, int, bool) {
	j := i
	for j < len(s) && !strings.ContainsRune(identStop, rune(s[j])) {
		j++
	}
	if j == i {
		return "", i, false
	}
	return s[i:j], j, true
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	return i
}

// indexTopLevel finds sub in s, case-insensitively, ignoring anything inside
// backticks or quotes. It returns -1 when sub does not occur outside them.
//
// The naive strings.Index would find the " ON " in a table called `SEASON ON
// ICE`, and split the grant in the middle of an identifier.
func indexTopLevel(s, sub string) int {
	upper := strings.ToUpper(s)
	sub = strings.ToUpper(sub)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '`', '\'', '"':
			_, next, ok := scanQuoted(s, i)
			if !ok {
				return -1
			}
			i = next - 1
		default:
			if strings.HasPrefix(upper[i:], sub) {
				return i
			}
		}
	}
	return -1
}

// splitTopLevel splits on sep, ignoring separators inside quotes or
// parentheses. Column-level grants — SELECT (id, title) — put commas inside
// parentheses, and splitting those would turn one privilege into two unknown
// ones.
func splitTopLevel(s string, sep byte) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '`', '\'', '"':
			_, next, ok := scanQuoted(s, i)
			if !ok {
				return append(out, s[start:])
			}
			i = next - 1
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
