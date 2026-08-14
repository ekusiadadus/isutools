package accesslog

import (
	"errors"
	"regexp"
	"strings"
)

const (
	MaxPathRuleSpecBytes = 8192
	MaxPathRules         = 64
	MaxPathPatternBytes  = 256
	MaxPathOutputBytes   = 256
	MaxPathInputBytes    = 4096
	UnmatchedURI         = "(unmatched)"
)

type UnmatchedPolicy string

const (
	UnmatchedKeep     UnmatchedPolicy = "keep"
	UnmatchedCollapse UnmatchedPolicy = "collapse"
)

type PathRuleError struct{ Code string }

func (e *PathRuleError) Error() string { return "accesslog path rules: " + e.Code }

type pathRule struct {
	pattern     *regexp.Regexp
	replacement string
}

// PathRules is an immutable, ordered, full-match URI grouping policy.
type PathRules struct {
	rules     []pathRule
	unmatched UnmatchedPolicy
}

func ParsePathRules(spec string, unmatched UnmatchedPolicy) (*PathRules, error) {
	if unmatched != UnmatchedKeep && unmatched != UnmatchedCollapse {
		return nil, &PathRuleError{Code: "invalid-unmatched-policy"}
	}
	if len(spec) > MaxPathRuleSpecBytes {
		return nil, &PathRuleError{Code: "spec-too-large"}
	}
	result := &PathRules{unmatched: unmatched}
	if strings.TrimSpace(spec) == "" {
		return result, nil
	}
	parts := strings.Split(spec, ";")
	if len(parts) > MaxPathRules {
		return nil, &PathRuleError{Code: "too-many-rules"}
	}
	for _, part := range parts {
		at := strings.LastIndex(part, "=")
		if at <= 0 || at == len(part)-1 {
			return nil, &PathRuleError{Code: "invalid-rule"}
		}
		pattern, output := strings.TrimSpace(part[:at]), strings.TrimSpace(part[at+1:])
		if len(pattern) > MaxPathPatternBytes || len(output) > MaxPathOutputBytes {
			return nil, &PathRuleError{Code: "rule-too-large"}
		}
		if !strings.HasPrefix(output, "/") || strings.ContainsAny(output, "$\\?#\r\n\t") {
			return nil, &PathRuleError{Code: "invalid-output"}
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, &PathRuleError{Code: "invalid-regexp"}
		}
		result.rules = append(result.rules, pathRule{pattern: re, replacement: output})
	}
	return result, nil
}

func (r *PathRules) Normalize(uri string) string {
	if r == nil {
		return uri
	}
	if len(uri) > MaxPathInputBytes {
		return UnmatchedURI
	}
	for _, rule := range r.rules {
		match := rule.pattern.FindStringIndex(uri)
		if len(match) == 2 && match[0] == 0 && match[1] == len(uri) {
			return rule.replacement
		}
	}
	if r.unmatched == UnmatchedCollapse {
		return UnmatchedURI
	}
	return uri
}

func pathRuleReason(err error) string {
	var ruleErr *PathRuleError
	if errors.As(err, &ruleErr) {
		return ruleErr.Code
	}
	return "invalid-rule"
}
