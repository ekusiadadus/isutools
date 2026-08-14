package isutools

import (
	"os"
	"strings"
	"testing"
)

func TestNginxExamplesUseTrustedUpstreamFlowLabels(t *testing.T) {
	for _, names := range [][]string{
		{"examples/nginx-isutools.conf"},
		{"examples/private-isu/nginx-isutools.conf", "examples/private-isu/nginx-isutools-flow-headers.inc"},
		{"examples/isucon13-wsl/nginx-isutools.conf"},
	} {
		var combined strings.Builder
		for _, name := range names {
			body, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			combined.Write(body)
		}
		text := combined.String()
		for _, required := range []string{
			"sess:$upstream_http_x_isutools_session",
			"scenario:$upstream_http_x_isutools_scenario",
			"proxy_set_header X-Isutools-Session \"\"",
			"proxy_set_header X-Isutools-Scenario \"\"",
			"proxy_hide_header X-Isutools-Session",
			"proxy_hide_header X-Isutools-Scenario",
		} {
			if !strings.Contains(text, required) {
				t.Errorf("%v missing %q", names, required)
			}
		}
	}
}
