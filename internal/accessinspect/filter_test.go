package accessinspect

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
)

func TestCompileFilterEvaluatesBoundedExpression(t *testing.T) {
	filter, err := CompileFilter(`method in [GET,POST] && (status >= 500 || path ~ "^/slow/") && duration >= 100ms`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		rec  accesslog.Record
		want bool
	}{
		{"status", accesslog.Record{Method: "POST", URI: "/api", Status: 503, RequestTime: 150 * time.Millisecond}, true},
		{"path", accesslog.Record{Method: "GET", URI: "/slow/1", Status: 200, RequestTime: 200 * time.Millisecond}, true},
		{"duration", accesslog.Record{Method: "GET", URI: "/slow/1", Status: 200, RequestTime: 20 * time.Millisecond}, false},
		{"method", accesslog.Record{Method: "DELETE", URI: "/slow/1", Status: 503, RequestTime: time.Second}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := filter.Match(tc.rec); got != tc.want {
				t.Fatalf("Match()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestCompileFilterSupportsMetricsAndDimensions(t *testing.T) {
	rec := accesslog.Record{
		Method: "GET", URI: "/items", Status: 204, RequestTime: 200 * time.Millisecond,
		UpstreamTotal: 150 * time.Millisecond, UpstreamValid: true, UpstreamComplete: true,
		Bytes: 42, CacheStatus: "HIT", ContentType: "application/json", Protocol: "HTTP/2.0",
	}
	for _, expression := range []string{
		`status_class = 2xx`, `upstream >= 100ms`, `residual = 50ms`, `bytes <= 42`,
		`cache = HIT`, `content_type = "application/json"`, `protocol != HTTP/1.1`,
	} {
		filter, err := CompileFilter(expression)
		if err != nil {
			t.Fatalf("CompileFilter(%q): %v", expression, err)
		}
		if !filter.Match(rec) {
			t.Fatalf("CompileFilter(%q) did not match", expression)
		}
	}
}

func TestCompileFilterRejectsUnsafeOrUnboundedInput(t *testing.T) {
	tests := []struct {
		name string
		expr string
		code string
	}{
		{"unknown field", `cookie = secret`, "unsupported-field"},
		{"shell-like", `method = $(id)`, "invalid-value"},
		{"unterminated", `path ~ "secret`, "invalid-string"},
		{"bad regexp", `path ~ "["`, "invalid-regexp"},
		{"deep", strings.Repeat("(", MaxFilterDepth+1) + `status=200` + strings.Repeat(")", MaxFilterDepth+1), "expression-too-deep"},
		{"large", strings.Repeat("x", MaxFilterBytes+1), "expression-too-large"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileFilter(tc.expr)
			var filterErr *FilterError
			if !errors.As(err, &filterErr) || filterErr.Code != tc.code {
				t.Fatalf("error=%v want code %q", err, tc.code)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "$(id)") {
				t.Fatalf("error leaked input: %v", err)
			}
		})
	}
}
