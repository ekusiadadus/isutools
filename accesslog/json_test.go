package accesslog

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseNginxJSONWithIsutoolsKeys(t *testing.T) {
	line := `{"time":"2026-08-04T00:00:00+09:00","method":"GET","uri":"/posts/1","status":200,"reqtime":0.250,"upstime":"0.120","bytes":4096,"cache":"MISS","ctype":"text/html","proto":"HTTP/3.0"}`
	rec, err := ParseNginxJSON(line)
	if err != nil {
		t.Fatalf("ParseNginxJSON: %v", err)
	}
	if rec.Method != "GET" || rec.URI != "/posts/1" || rec.Status != 200 {
		t.Errorf("record = %#v", rec)
	}
	if rec.RequestTime != 250*time.Millisecond {
		t.Errorf("reqtime = %v, want 250ms", rec.RequestTime)
	}
	if rec.Bytes != 4096 || rec.CacheStatus != "MISS" {
		t.Errorf("bytes/cache = %d/%q", rec.Bytes, rec.CacheStatus)
	}
	if rec.Protocol != "HTTP/3.0" {
		t.Errorf("protocol = %q, want HTTP/3.0", rec.Protocol)
	}
	if !rec.UpstreamValid || rec.UpstreamTotal != 120*time.Millisecond {
		t.Errorf("upstream = %#v", rec)
	}
}

func TestParseJSONProtocolAliases(t *testing.T) {
	for _, key := range []string{"protocol", "request_protocol"} {
		line := `{"method":"GET","uri":"/","status":200,"response_time":0.1,"` + key + `":"HTTP/2.0"}`
		rec, err := ParseNginxJSON(line)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if rec.Protocol != "HTTP/2.0" {
			t.Errorf("%s protocol = %q", key, rec.Protocol)
		}
	}
}

func TestParseCaddyNativeJSON(t *testing.T) {
	line := `{"level":"info","request":{"method":"GET","uri":"/posts/1?q=secret","proto":"HTTP/3.0","headers":{"X-Isutools-Session":["spoofed"],"X-Isutools-Scenario":["spoofed"]}},"status":200,"duration":0.025,"size":1234,"resp_headers":{"Content-Type":["text/html"],"X-Isutools-Session":["safe-session"],"X-Isutools-Scenario":["reader"]}}`
	rec, err := ParseLineFormat(FormatCaddyJSON, line)
	if err != nil {
		t.Fatalf("Caddy JSON: %v", err)
	}
	if rec.Method != "GET" || rec.URI != "/posts/1" || !rec.QueryStripped {
		t.Errorf("request = %#v", rec)
	}
	if rec.Protocol != "HTTP/3.0" || rec.RequestTime != 25*time.Millisecond || rec.Bytes != 1234 {
		t.Errorf("protocol/timing/bytes = %#v", rec)
	}
	if rec.ContentType != "text/html" {
		t.Errorf("content type = %q", rec.ContentType)
	}
	if rec.Session != "safe-session" || rec.Scenario != "reader" {
		t.Errorf("Caddy flow labels = %#v", rec)
	}
}

func TestParseNginxJSONWithAlpKeys(t *testing.T) {
	// alp's default keys from the ISUCON book: method/uri/status/body_bytes/response_time
	line := `{"time":"2021-07-25T05:04:47+00:00","method":"GET","uri":"/","status":200,"body_bytes":34184,"response_time":0.321}`
	rec, err := ParseNginxJSON(line)
	if err != nil {
		t.Fatalf("ParseNginxJSON(alp): %v", err)
	}
	if rec.RequestTime != 321*time.Millisecond {
		t.Errorf("response_time = %v, want 321ms", rec.RequestTime)
	}
	if rec.Bytes != 34184 {
		t.Errorf("body_bytes = %d, want 34184", rec.Bytes)
	}
	if !rec.NoUpstreamTiming {
		t.Error("missing upstime must default to no upstream timing")
	}
}

func TestParseNginxJSONMissingRequired(t *testing.T) {
	if _, err := ParseNginxJSON(`{"method":"GET","uri":"/"}`); err == nil {
		t.Fatal("missing status/reqtime must fail")
	}
}

func TestParseLineAutoDetects(t *testing.T) {
	jsonRec, err := ParseLine(`{"method":"GET","uri":"/x","status":200,"response_time":0.1}`)
	if err != nil || jsonRec.URI != "/x" {
		t.Fatalf("json autodetect: %v %#v", err, jsonRec)
	}
	ltsvRec, err := ParseLine(strings.TrimSpace(lineA))
	if err != nil || ltsvRec.URI != "/posts/1" {
		t.Fatalf("ltsv autodetect: %v %#v", err, ltsvRec)
	}
}

func TestParseApacheJSONRequestTimeMicroseconds(t *testing.T) {
	rec, err := ParseLine(`{"method":"GET","uri":"/x","status":200,"reqtime_us":125000,"bytes":10}`)
	if err != nil {
		t.Fatalf("Apache JSON: %v", err)
	}
	if rec.RequestTime != 125*time.Millisecond {
		t.Errorf("request time = %v, want 125ms", rec.RequestTime)
	}
}

func TestSessionIdentifierIsBoundedAndPseudonymousSafe(t *testing.T) {
	rec, err := ParseNginxJSON(`{"method":"GET","uri":"/","status":200,"reqtime":0.1,"sess":"` + strings.Repeat("raw-cookie-", 30) + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Session != "" || !rec.Partial {
		t.Errorf("unsafe session must be discarded and reported partial: %#v", rec)
	}
}

func TestFlowTransitionMapIsBounded(t *testing.T) {
	a := NewAggregator(0)
	for i := 0; i < 12000; i++ {
		a.Observe(Record{Method: "GET", URI: fmt.Sprintf("/page/%d", i), Status: 200, Session: "safe-session"})
	}
	if len(a.flows) > 10001 { // 10k transitions plus the overflow row.
		t.Fatalf("flow transition identities are unbounded: %d", len(a.flows))
	}
	if a.Snapshot().FlowDropped == 0 {
		t.Error("overflowed flow identities must be reported")
	}
}

func TestSessionFlowAggregation(t *testing.T) {
	a := NewAggregator(0)
	obs := func(sess, method, uri string) {
		a.Observe(Record{Method: method, URI: uri, Status: 200, Session: sess})
	}
	obs("s1", "GET", "/login")
	obs("s1", "GET", "/")
	obs("s1", "GET", "/posts/*")
	obs("s2", "GET", "/login")
	obs("s2", "GET", "/")
	obs("s3", "GET", "/")

	snap := a.Snapshot()
	if len(snap.Flows) == 0 {
		t.Fatal("flows must be aggregated from session transitions")
	}
	top := snap.Flows[0]
	if top.From != "GET /login" || top.To != "GET /" || top.Count != 2 {
		t.Errorf("top flow = %+v, want GET /login -> GET / x2", top)
	}

	a.Reset()
	if got := a.Snapshot(); len(got.Flows) != 0 {
		t.Errorf("flows must reset with generation: %v", got.Flows)
	}
}

func TestSessionFieldParsedFromLTSVAndJSON(t *testing.T) {
	rec, err := ParseNginxLTSV("method:GET\turi:/\tstatus:200\treqtime:0.1\tupstime:-\tbytes:0\tcache:\tctype:\tsess:abc123\tscenario:login_and_browse")
	if err != nil {
		t.Fatalf("ltsv: %v", err)
	}
	if rec.Session != "abc123" {
		t.Errorf("ltsv session = %q", rec.Session)
	}
	if rec.Scenario != "login_and_browse" {
		t.Errorf("ltsv scenario = %q", rec.Scenario)
	}
	rec, err = ParseNginxJSON(`{"method":"GET","uri":"/","status":200,"reqtime":0.1,"sess":"xyz","scenario":"reader"}`)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if rec.Session != "xyz" {
		t.Errorf("json session = %q", rec.Session)
	}
	if rec.Scenario != "reader" {
		t.Errorf("json scenario = %q", rec.Scenario)
	}
}

func TestScenarioLabelIsBoundedAndSafe(t *testing.T) {
	rec, err := ParseNginxJSON(`{"method":"GET","uri":"/","status":200,"reqtime":0.1,"sess":"safe","scenario":"raw bearer token with spaces"}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Scenario != "" || !rec.Partial {
		t.Errorf("unsafe scenario must be discarded: %#v", rec)
	}
}

func TestScenarioStoryAggregation(t *testing.T) {
	a := NewAggregator(100)
	observe := func(sess, scenario, method, uri string) {
		a.Observe(Record{Session: sess, Scenario: scenario, Method: method, URI: uri, Status: 200})
	}
	for _, sess := range []string{"s1", "s2"} {
		observe(sess, "login_and_browse", "POST", "/login")
		observe(sess, "login_and_browse", "GET", "/")
		observe(sess, "login_and_browse", "GET", "/posts/*")
	}
	observe("s3", "login_and_browse", "POST", "/login")
	observe("s3", "login_and_browse", "GET", "/")
	observe("s1", "author", "GET", "/@me")

	snap := a.Snapshot()
	if len(snap.Stories) != 3 {
		t.Fatalf("stories = %#v", snap.Stories)
	}
	top := snap.Stories[0]
	if top.Scenario != "login_and_browse" || top.Sessions != 2 || top.Requests != 6 {
		t.Errorf("top story = %#v", top)
	}
	want := []string{"POST /login", "GET /", "GET /posts/*"}
	if strings.Join(top.Journey, "|") != strings.Join(want, "|") {
		t.Errorf("journey = %#v", top.Journey)
	}

	a.Reset()
	if got := a.Snapshot().Stories; len(got) != 0 {
		t.Errorf("stories must reset: %#v", got)
	}
}

func TestScenarioStoryStepLimit(t *testing.T) {
	a := NewAggregator(100)
	for i := 0; i < maxStorySteps+10; i++ {
		a.Observe(Record{Session: "s1", Scenario: "long", Method: "GET", URI: fmt.Sprintf("/%d", i), Status: 200})
	}
	snap := a.Snapshot()
	if len(snap.Stories) != 1 || len(snap.Stories[0].Journey) > maxStorySteps+1 || snap.StoryDropped == 0 {
		t.Errorf("bounded story = %#v dropped=%d", snap.Stories, snap.StoryDropped)
	}
}

func TestScenarioStoryInternsRepeatedPages(t *testing.T) {
	a := NewAggregator(10)
	for _, session := range []string{"s1", "s2"} {
		a.Observe(Record{Session: session, Scenario: "browse", Method: "GET", URI: "/posts", Status: 200})
	}
	if got := len(a.storyPages); got != 1 {
		t.Fatalf("interned story pages = %d, want 1", got)
	}
}
