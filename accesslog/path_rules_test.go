package accesslog

import (
	"strings"
	"testing"
	"time"
)

func TestPathRulesNormalizeBeforeEveryAggregate(t *testing.T) {
	rules, err := ParsePathRules(`^/posts/[0-9]+$=/posts/*;^/users/[0-9a-f-]+$=/users/*`, UnmatchedKeep)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAggregator(DefaultMaxKeys)
	for i, path := range []string{"/posts/123?token=secret", "/posts/456"} {
		rec := Record{Method: "GET", URI: strings.Split(path, "?")[0], Status: 200,
			RequestTime: time.Duration(i+1) * time.Second, Bytes: int64(10 + i),
			CacheStatus: "HIT", ContentType: "text/html", Protocol: "HTTP/2.0", Session: "safe-session"}
		rec.URI = rules.Normalize(rec.URI)
		a.Observe(rec)
	}
	snapshot := a.Snapshot()
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].URI != "/posts/*" ||
		snapshot.Entries[0].Count != 2 || snapshot.Entries[0].BytesTotal != 21 ||
		snapshot.Entries[0].RequestTotal != 3*time.Second {
		t.Fatalf("entries = %#v", snapshot.Entries)
	}
	if len(snapshot.Protocols) != 1 || snapshot.Protocols[0].Count != 2 {
		t.Fatalf("protocols = %#v", snapshot.Protocols)
	}
}

func TestPathRulesFirstMatchAndUnmatchedPolicy(t *testing.T) {
	rules, err := ParsePathRules(`^/x/[0-9]+$=/first;^/x/.*$=/second`, UnmatchedCollapse)
	if err != nil {
		t.Fatal(err)
	}
	if got := rules.Normalize("/x/42"); got != "/first" {
		t.Fatalf("first match = %q", got)
	}
	if got := rules.Normalize("/private/slug"); got != UnmatchedURI {
		t.Fatalf("unmatched = %q", got)
	}
}

func TestPathRulesRejectInvalidInputsWithoutEcho(t *testing.T) {
	for _, spec := range []string{
		"([=/x",
		"^/x$=$1",
		strings.Repeat("a", MaxPathRuleSpecBytes+1),
	} {
		_, err := ParsePathRules(spec, UnmatchedKeep)
		if err == nil {
			t.Fatalf("ParsePathRules(%d bytes) succeeded", len(spec))
		}
		if strings.Contains(err.Error(), spec) {
			t.Fatalf("error leaked raw rule: %q", err)
		}
	}
}

func TestPathRulesNormalizeSupportedParserFixtures(t *testing.T) {
	rules, err := ParsePathRules(`^/posts/[0-9]+$=/posts/*`, UnmatchedKeep)
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"method:GET\turi:/posts/1?secret=x\tstatus:200\treqtime:0.1\tupstime:-\tbytes:1\tcache:HIT\tctype:text/html",
		`{"method":"GET","uri":"/posts/2","status":200,"response_time":0.1}`,
		`{"request":{"method":"GET","uri":"/posts/3","proto":"HTTP/2.0"},"status":200,"duration":0.1,"size":1}`,
	}
	for _, line := range lines {
		rec, parseErr := ParseLine(line)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if got := rules.Normalize(rec.URI); got != "/posts/*" {
			t.Fatalf("normalized URI = %q", got)
		}
	}
}
