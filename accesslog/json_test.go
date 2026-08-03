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
