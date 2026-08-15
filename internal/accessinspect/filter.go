// Package accessinspect provides bounded, offline analysis for normalized
// access logs. It is deliberately independent from the application's request
// path and does not execute user supplied code.
package accessinspect

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ekusiadadus/isutools/accesslog"
)

const (
	MaxFilterBytes      = 4096
	MaxFilterTokens     = 128
	MaxFilterPredicates = 32
	MaxFilterDepth      = 8
	MaxFilterRegexps    = 8
	MaxFilterValueBytes = 256
)

// FilterError reports a stable, secret-free filter validation reason.
type FilterError struct{ Code string }

func (e *FilterError) Error() string { return "accessinspect filter: " + e.Code }

type tokenKind uint8

const (
	tokenEOF tokenKind = iota
	tokenValue
	tokenAnd
	tokenOr
	tokenLParen
	tokenRParen
	tokenLBracket
	tokenRBracket
	tokenComma
	tokenEQ
	tokenNE
	tokenGT
	tokenGE
	tokenLT
	tokenLE
	tokenRegexp
	tokenIn
)

type token struct {
	kind  tokenKind
	value string
}

// Filter is an immutable compiled predicate. A nil filter accepts every row.
type Filter struct{ match func(accesslog.Record) bool }

func (f *Filter) Match(rec accesslog.Record) bool {
	return f == nil || f.match == nil || f.match(rec)
}

// CompileFilter parses a bounded expression. AND binds more tightly than OR.
// Supported values are plain safe tokens or double-quoted strings.
func CompileFilter(expression string) (*Filter, error) {
	if len(expression) > MaxFilterBytes {
		return nil, &FilterError{Code: "expression-too-large"}
	}
	if strings.TrimSpace(expression) == "" {
		return &Filter{}, nil
	}
	tokens, err := lexFilter(expression)
	if err != nil {
		return nil, err
	}
	p := filterParser{tokens: tokens}
	match, err := p.parseOr(0)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokenEOF {
		return nil, &FilterError{Code: "unexpected-token"}
	}
	return &Filter{match: match}, nil
}

func lexFilter(input string) ([]token, error) {
	result := make([]token, 0, 16)
	appendToken := func(tok token) error {
		if len(result) >= MaxFilterTokens {
			return &FilterError{Code: "too-many-tokens"}
		}
		result = append(result, tok)
		return nil
	}
	for pos := 0; pos < len(input); {
		if unicode.IsSpace(rune(input[pos])) {
			pos++
			continue
		}
		var kind tokenKind
		switch {
		case strings.HasPrefix(input[pos:], "&&"):
			kind, pos = tokenAnd, pos+2
		case strings.HasPrefix(input[pos:], "||"):
			kind, pos = tokenOr, pos+2
		case strings.HasPrefix(input[pos:], "!="):
			kind, pos = tokenNE, pos+2
		case strings.HasPrefix(input[pos:], ">="):
			kind, pos = tokenGE, pos+2
		case strings.HasPrefix(input[pos:], "<="):
			kind, pos = tokenLE, pos+2
		default:
			switch input[pos] {
			case '(':
				kind, pos = tokenLParen, pos+1
			case ')':
				kind, pos = tokenRParen, pos+1
			case '[':
				kind, pos = tokenLBracket, pos+1
			case ']':
				kind, pos = tokenRBracket, pos+1
			case ',':
				kind, pos = tokenComma, pos+1
			case '=':
				kind, pos = tokenEQ, pos+1
			case '>':
				kind, pos = tokenGT, pos+1
			case '<':
				kind, pos = tokenLT, pos+1
			case '~':
				kind, pos = tokenRegexp, pos+1
			case '"':
				start := pos
				pos++
				escaped := false
				for pos < len(input) {
					if !escaped && input[pos] == '"' {
						pos++
						break
					}
					if !escaped && input[pos] == '\\' {
						escaped = true
					} else {
						escaped = false
					}
					pos++
				}
				if pos > len(input) || input[pos-1] != '"' {
					return nil, &FilterError{Code: "invalid-string"}
				}
				value, err := strconv.Unquote(input[start:pos])
				if err != nil || !safeFilterValue(value) {
					return nil, &FilterError{Code: "invalid-string"}
				}
				if err := appendToken(token{kind: tokenValue, value: value}); err != nil {
					return nil, err
				}
				continue
			default:
				start := pos
				for pos < len(input) && isBareFilterByte(input[pos]) {
					pos++
				}
				if start == pos {
					return nil, &FilterError{Code: "invalid-value"}
				}
				value := input[start:pos]
				if strings.EqualFold(value, "in") {
					kind = tokenIn
				} else {
					if !safeFilterValue(value) {
						return nil, &FilterError{Code: "invalid-value"}
					}
					if err := appendToken(token{kind: tokenValue, value: value}); err != nil {
						return nil, err
					}
					continue
				}
			}
		}
		if err := appendToken(token{kind: kind}); err != nil {
			return nil, err
		}
	}
	result = append(result, token{kind: tokenEOF})
	return result, nil
}

func isBareFilterByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("_./:+%-", rune(b))
}

func safeFilterValue(value string) bool {
	if value == "" || len(value) > MaxFilterValueBytes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type predicate func(accesslog.Record) bool

type filterParser struct {
	tokens     []token
	pos        int
	predicates int
	regexps    int
}

func (p *filterParser) peek() token { return p.tokens[p.pos] }

func (p *filterParser) take(kind tokenKind) bool {
	if p.peek().kind != kind {
		return false
	}
	p.pos++
	return true
}

func (p *filterParser) parseOr(depth int) (predicate, error) {
	left, err := p.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	for p.take(tokenOr) {
		right, err := p.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		previous := left
		left = func(rec accesslog.Record) bool { return previous(rec) || right(rec) }
	}
	return left, nil
}

func (p *filterParser) parseAnd(depth int) (predicate, error) {
	left, err := p.parsePrimary(depth)
	if err != nil {
		return nil, err
	}
	for p.take(tokenAnd) {
		right, err := p.parsePrimary(depth)
		if err != nil {
			return nil, err
		}
		previous := left
		left = func(rec accesslog.Record) bool { return previous(rec) && right(rec) }
	}
	return left, nil
}

func (p *filterParser) parsePrimary(depth int) (predicate, error) {
	if p.take(tokenLParen) {
		if depth >= MaxFilterDepth {
			return nil, &FilterError{Code: "expression-too-deep"}
		}
		expression, err := p.parseOr(depth + 1)
		if err != nil {
			return nil, err
		}
		if !p.take(tokenRParen) {
			return nil, &FilterError{Code: "missing-parenthesis"}
		}
		return expression, nil
	}
	return p.parsePredicate()
}

func (p *filterParser) parsePredicate() (predicate, error) {
	if p.predicates >= MaxFilterPredicates {
		return nil, &FilterError{Code: "too-many-predicates"}
	}
	field := p.peek()
	if field.kind != tokenValue {
		return nil, &FilterError{Code: "expected-field"}
	}
	p.pos++
	op := p.peek().kind
	switch op {
	case tokenEQ, tokenNE, tokenGT, tokenGE, tokenLT, tokenLE, tokenRegexp, tokenIn:
		p.pos++
	default:
		return nil, &FilterError{Code: "expected-operator"}
	}
	p.predicates++
	if op == tokenIn {
		return p.compileSet(field.value)
	}
	value := p.peek()
	if value.kind != tokenValue {
		return nil, &FilterError{Code: "expected-value"}
	}
	p.pos++
	return p.compileComparison(field.value, op, value.value)
}

func (p *filterParser) compileSet(field string) (predicate, error) {
	if !p.take(tokenLBracket) {
		return nil, &FilterError{Code: "expected-set"}
	}
	values := make(map[string]struct{}, 4)
	for {
		value := p.peek()
		if value.kind != tokenValue {
			return nil, &FilterError{Code: "expected-value"}
		}
		p.pos++
		values[value.value] = struct{}{}
		if len(values) > 16 {
			return nil, &FilterError{Code: "set-too-large"}
		}
		if p.take(tokenRBracket) {
			break
		}
		if !p.take(tokenComma) {
			return nil, &FilterError{Code: "invalid-set"}
		}
	}
	getter, ok := stringGetter(field)
	if !ok || field == "status_class" {
		return nil, &FilterError{Code: "unsupported-field"}
	}
	return func(rec accesslog.Record) bool {
		_, exists := values[getter(rec)]
		return exists
	}, nil
}

func (p *filterParser) compileComparison(field string, op tokenKind, value string) (predicate, error) {
	if getter, ok := stringGetter(field); ok {
		if op == tokenRegexp {
			if field != "path" {
				return nil, &FilterError{Code: "unsupported-operator"}
			}
			if p.regexps >= MaxFilterRegexps {
				return nil, &FilterError{Code: "too-many-regexps"}
			}
			re, err := regexp.Compile(value)
			if err != nil {
				return nil, &FilterError{Code: "invalid-regexp"}
			}
			p.regexps++
			return func(rec accesslog.Record) bool { return re.MatchString(getter(rec)) }, nil
		}
		if op != tokenEQ && op != tokenNE {
			return nil, &FilterError{Code: "unsupported-operator"}
		}
		return func(rec accesslog.Record) bool {
			equal := getter(rec) == value
			return equal == (op == tokenEQ)
		}, nil
	}
	if field == "status_class" {
		if op != tokenEQ && op != tokenNE || len(value) != 3 || value[1:] != "xx" || value[0] < '1' || value[0] > '5' {
			return nil, &FilterError{Code: "invalid-status-class"}
		}
		class := int(value[0] - '0')
		return func(rec accesslog.Record) bool {
			equal := rec.Status/100 == class
			return equal == (op == tokenEQ)
		}, nil
	}
	getter, kind, ok := numericGetter(field)
	if !ok {
		return nil, &FilterError{Code: "unsupported-field"}
	}
	if op == tokenRegexp {
		return nil, &FilterError{Code: "unsupported-operator"}
	}
	var expected int64
	var err error
	if kind == "duration" {
		var duration time.Duration
		duration, err = time.ParseDuration(value)
		if err == nil && duration < 0 {
			err = fmt.Errorf("negative")
		}
		expected = int64(duration)
	} else {
		expected, err = strconv.ParseInt(value, 10, 64)
		if err == nil && expected < 0 {
			err = fmt.Errorf("negative")
		}
	}
	if err != nil {
		return nil, &FilterError{Code: "invalid-number"}
	}
	return func(rec accesslog.Record) bool {
		actual, available := getter(rec)
		return available && compareInt(actual, expected, op)
	}, nil
}

func stringGetter(field string) (func(accesslog.Record) string, bool) {
	switch field {
	case "method":
		return func(r accesslog.Record) string { return r.Method }, true
	case "path":
		return func(r accesslog.Record) string { return r.URI }, true
	case "cache":
		return func(r accesslog.Record) string { return r.CacheStatus }, true
	case "content_type":
		return func(r accesslog.Record) string { return r.ContentType }, true
	case "protocol":
		return func(r accesslog.Record) string { return r.Protocol }, true
	default:
		return nil, false
	}
}

func numericGetter(field string) (func(accesslog.Record) (int64, bool), string, bool) {
	switch field {
	case "status":
		return func(r accesslog.Record) (int64, bool) { return int64(r.Status), true }, "integer", true
	case "bytes":
		return func(r accesslog.Record) (int64, bool) { return r.Bytes, true }, "integer", true
	case "duration", "request":
		return func(r accesslog.Record) (int64, bool) { return int64(r.RequestTime), r.Status != 101 }, "duration", true
	case "upstream":
		return func(r accesslog.Record) (int64, bool) {
			return int64(r.UpstreamTotal), r.UpstreamValid && r.Status != 101
		}, "duration", true
	case "residual":
		return func(r accesslog.Record) (int64, bool) {
			ok := r.UpstreamValid && r.UpstreamComplete && r.RequestTime >= r.UpstreamTotal && r.Status != 101
			return int64(r.RequestTime - r.UpstreamTotal), ok
		}, "duration", true
	default:
		return nil, "", false
	}
}

func compareInt(actual, expected int64, op tokenKind) bool {
	switch op {
	case tokenEQ:
		return actual == expected
	case tokenNE:
		return actual != expected
	case tokenGT:
		return actual > expected
	case tokenGE:
		return actual >= expected
	case tokenLT:
		return actual < expected
	case tokenLE:
		return actual <= expected
	default:
		return false
	}
}
