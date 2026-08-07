package httpstats

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	ProfileRouteUnmatched            = "(unmatched)"
	ProfileMethodOther               = "OTHER"
	MaxProfileRouteBytes             = 128
	MaxSafeRouteRulesSpecBytes       = 16 << 10
	MaxSafeRouteRules                = 64
	MaxSafeRouteRuleRegexpBytes      = 256
	MaxSafeRouteRuleRegexpTotalBytes = 8 << 10
)

type SafeProfileRouteRule struct {
	pattern     *regexp.Regexp
	replacement string
}

type ProfileLabeler interface {
	DoProfileLabels(context.Context, ProfileLabel, func(context.Context)) bool
}

// ProfileLabel is intentionally narrower than the dashboard aggregation key.
// Route can originate only from router metadata installed by trusted code; it
// never uses URL.Path or heuristic normalization, both of which may contain
// user identifiers, reset tokens, invite slugs, or other secrets.
type ProfileLabel struct {
	Method string
	Route  string
}

func SafeProfileLabel(request *http.Request) ProfileLabel {
	return SafeProfileLabelWithRules(request, nil)
}

func SafeProfileLabelWithRules(request *http.Request, rules []SafeProfileRouteRule) ProfileLabel {
	method := ProfileMethodOther
	if request != nil && safeProfileMethod(request.Method) {
		method = request.Method
	}
	route := ProfileRouteUnmatched
	if request != nil {
		pattern := requestPatternPath(request.Pattern)
		if safeProfileRoute(pattern) {
			route = pattern
		} else if request.URL != nil {
			for _, rule := range rules {
				if rule.pattern != nil && rule.pattern.MatchString(request.URL.Path) {
					route = rule.replacement
					break
				}
			}
		}
	}
	return ProfileLabel{Method: method, Route: route}
}

// ParseSafeProfileRouteRules accepts only full-match regexes with constant,
// non-secret replacements. Captures may be used for matching but are never
// expanded into the emitted route label.
func ParseSafeProfileRouteRules(spec string) ([]SafeProfileRouteRule, error) {
	if len(spec) > MaxSafeRouteRulesSpecBytes {
		return nil, fmt.Errorf("httpstats: safe profile route rules exceed %d bytes", MaxSafeRouteRulesSpecBytes)
	}
	if spec == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ";")
	if len(parts) > MaxSafeRouteRules {
		return nil, fmt.Errorf("httpstats: safe profile route rules exceed %d entries", MaxSafeRouteRules)
	}
	rules := make([]SafeProfileRouteRule, 0, len(parts))
	regexpBytes := 0
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.LastIndexByte(part, '=')
		if eq <= 0 || eq == len(part)-1 {
			return nil, fmt.Errorf("httpstats: safe profile route rule %q is not regexp=constant", part)
		}
		expression, replacement := part[:eq], part[eq+1:]
		if len(expression) > MaxSafeRouteRuleRegexpBytes || !strings.HasPrefix(expression, "^") || !hasUnescapedFinalDollar(expression) {
			return nil, fmt.Errorf("httpstats: safe profile route regexp must be fully anchored and at most %d bytes", MaxSafeRouteRuleRegexpBytes)
		}
		regexpBytes += len(expression)
		if regexpBytes > MaxSafeRouteRuleRegexpTotalBytes {
			return nil, fmt.Errorf("httpstats: safe profile route regexps exceed %d bytes", MaxSafeRouteRuleRegexpTotalBytes)
		}
		if !safeConstantRoute(replacement) {
			return nil, fmt.Errorf("httpstats: unsafe constant profile route %q", replacement)
		}
		pattern, err := regexp.Compile(expression)
		if err != nil {
			return nil, fmt.Errorf("httpstats: safe profile route regexp %q: %w", expression, err)
		}
		rules = append(rules, SafeProfileRouteRule{pattern: pattern, replacement: replacement})
	}
	return rules, nil
}

func hasUnescapedFinalDollar(expression string) bool {
	if len(expression) < 2 || expression[len(expression)-1] != '$' {
		return false
	}
	backslashes := 0
	for i := len(expression) - 2; i >= 0 && expression[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 0
}

func safeConstantRoute(route string) bool {
	if !safeProfileRoute(route) {
		return false
	}
	for i := 0; i < len(route); i++ {
		c := route[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '_' && c != '.' && c != '/' && c != '{' && c != '}' && c != '*' && c != ':' && c != '-' {
			return false
		}
	}
	return true
}

func ProfileLabelMiddleware(next http.Handler, labeler ProfileLabeler, rules []SafeProfileRouteRule) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	if labeler == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		label := SafeProfileLabelWithRules(request, rules)
		labeler.DoProfileLabels(request.Context(), label, func(ctx context.Context) {
			next.ServeHTTP(w, request.WithContext(ctx))
		})
	})
}

func safeProfileMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func safeProfileRoute(route string) bool {
	if route == "" || len(route) > MaxProfileRouteBytes || !utf8.ValidString(route) {
		return false
	}
	return strings.IndexFunc(route, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}
