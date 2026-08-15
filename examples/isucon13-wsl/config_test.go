package isucon13wsl

import (
	"os"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/accesslog"
)

func TestSystemdAccessLogPathRulesMatchISUCON13Routes(t *testing.T) {
	body, err := os.ReadFile("isupipe-go.isutools.conf")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = `Environment="ISUTOOLS_ACCESS_LOG_PATH_RULES=`
	var spec string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, `"`) {
			spec = strings.TrimSuffix(strings.TrimPrefix(line, prefix), `"`)
			break
		}
	}
	if spec == "" {
		t.Fatal("ISUTOOLS_ACCESS_LOG_PATH_RULES is missing from the systemd example")
	}
	rules, err := accesslog.ParsePathRules(spec, accesslog.UnmatchedKeep)
	if err != nil {
		t.Fatalf("parse example path rules: %v", err)
	}

	tests := map[string]string{
		"/api/livestream/7522/livecomment/991/report": "/api/livestream/:livestream_id/livecomment/:livecomment_id/report",
		"/api/livestream/7522/reaction":               "/api/livestream/:livestream_id/reaction",
		"/api/livestream/7522":                        "/api/livestream/:livestream_id",
		"/api/user/junishikawa0/icon":                 "/api/user/:username/icon",
		"/api/user/junishikawa0":                      "/api/user/:username",
		"/api/livestream/search":                      "/api/livestream/search",
	}
	for input, want := range tests {
		if got := rules.Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRemoteBenchRetriesBoundedAccessLogCollection(t *testing.T) {
	body, err := os.ReadFile("remote-bench.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{"for attempt in $(seq 1 16)", `"$collect_status" == 204`, `"$collect_status" != 503`, `"$attempt" == 16`} {
		if !strings.Contains(script, want) {
			t.Errorf("remote-bench.sh is missing %q", want)
		}
	}
}
