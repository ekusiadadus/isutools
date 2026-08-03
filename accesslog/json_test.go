package accesslog

import (
	"strings"
	"testing"
	"time"
)

func TestParseNginxJSONWithIsutoolsKeys(t *testing.T) {
	line := `{"time":"2026-08-04T00:00:00+09:00","method":"GET","uri":"/posts/1","status":200,"reqtime":0.250,"upstime":"0.120","bytes":4096,"cache":"MISS","ctype":"text/html"}`
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
	if !rec.UpstreamValid || rec.UpstreamTotal != 120*time.Millisecond {
		t.Errorf("upstream = %#v", rec)
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
	rec, err := ParseNginxLTSV("method:GET\turi:/\tstatus:200\treqtime:0.1\tupstime:-\tbytes:0\tcache:\tctype:\tsess:abc123")
	if err != nil {
		t.Fatalf("ltsv: %v", err)
	}
	if rec.Session != "abc123" {
		t.Errorf("ltsv session = %q", rec.Session)
	}
	rec, err = ParseNginxJSON(`{"method":"GET","uri":"/","status":200,"reqtime":0.1,"sess":"xyz"}`)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if rec.Session != "xyz" {
		t.Errorf("json session = %q", rec.Session)
	}
}
