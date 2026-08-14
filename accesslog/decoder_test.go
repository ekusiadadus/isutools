package accesslog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseFormatAliasesAndRejectsUnknown(t *testing.T) {
	tests := map[string]Format{
		"": FormatAuto, "nginx-ltsv": FormatIsutoolsLTSV,
		"canonical-json": FormatIsutoolsJSON, "caddy": FormatCaddyJSON,
		"traefik": FormatTraefikJSON, "w3c": FormatIISW3C,
	}
	for input, want := range tests {
		got, err := ParseFormat(input)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseFormat("mystery-proxy"); err == nil {
		t.Fatal("unknown format must fail closed")
	}
}

func TestCanonicalJSONV1UsesExplicitUnits(t *testing.T) {
	rec, err := ParseLineFormat(FormatIsutoolsJSON, `{"schema":"isutools.http-access.v1","method":"GET","uri":"/items/42?token=secret","protocol":"h2","status":200,"duration_ns":25000001,"upstream_duration_us":12000,"bytes":512,"cache_status":"MISS","content_type":"application/json","sess":"pseudonym","scenario":"browse"}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RequestTime != 25000001*time.Nanosecond || rec.UpstreamTotal != 12*time.Millisecond {
		t.Fatalf("durations = %v/%v", rec.RequestTime, rec.UpstreamTotal)
	}
	if rec.Protocol != "HTTP/2.0" || rec.URI != "/items/42" || !rec.QueryStripped {
		t.Fatalf("identity = %#v", rec)
	}
}

func TestCanonicalNanosecondsRemainExactAboveJSONFloatIntegerLimit(t *testing.T) {
	const nanoseconds = int64(9007199254740993)
	rec, err := ParseLineFormat(FormatIsutoolsJSON, `{"method":"GET","uri":"/","status":200,"duration_ns":9007199254740993}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RequestTime != time.Duration(nanoseconds) {
		t.Fatalf("duration = %d, want %d", rec.RequestTime, nanoseconds)
	}
}

func TestCanonicalJSONRejectsAmbiguousOrUnknownDuration(t *testing.T) {
	for _, line := range []string{
		`{"method":"GET","uri":"/","status":200,"duration_ns":1,"duration_ms":1}`,
		`{"method":"GET","uri":"/","status":200,"duration":1}`,
		`{"schema":"isutools.http-access.v2","method":"GET","uri":"/","status":200,"duration_ns":1}`,
	} {
		if _, err := ParseLineFormat(FormatIsutoolsJSON, line); err == nil {
			t.Errorf("ambiguous/unknown contract accepted: %s", line)
		}
	}
}

func TestTraefikNativeJSONNanoseconds(t *testing.T) {
	rec, err := ParseLineFormat(FormatTraefikJSON, `{"RequestMethod":"POST","RequestPath":"/orders/9?secret=x","RequestProtocol":"HTTP/1.1","DownstreamStatus":201,"DownstreamContentSize":99,"Duration":25000000,"OriginDuration":12000000,"request_X-Isutools-Session":"spoofed","response_X-Isutools-Session":"trusted","response_X-Isutools-Scenario":"checkout"}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RequestTime != 25*time.Millisecond || rec.UpstreamTotal != 12*time.Millisecond {
		t.Fatalf("durations = %#v", rec)
	}
	if rec.Session != "" || rec.Scenario != "" {
		t.Fatalf("Traefik request-shaped labels were trusted = %#v", rec)
	}
}

func TestCaddyNeverTrustsRequestFlowHeaders(t *testing.T) {
	rec, err := ParseLineFormat(FormatCaddyJSON, `{"request":{"method":"GET","uri":"/","proto":"HTTP/2","headers":{"X-Isutools-Session":["spoofed"],"X-Isutools-Scenario":["spoofed"]}},"status":200,"duration":0.001,"size":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Session != "" || rec.Scenario != "" {
		t.Fatalf("public request labels were trusted: %#v", rec)
	}
}

func TestIISW3CFixedContract(t *testing.T) {
	if _, err := ParseLineFormat(FormatIISW3C, "#Fields: date time cs-method cs-uri-stem sc-status sc-bytes time-taken cs-version"); !errors.Is(err, ErrSkipLine) {
		t.Fatalf("directive = %v", err)
	}
	rec, err := ParseLineFormat(FormatIISW3C, "2026-08-14 12:34:56 GET /images/logo.png 304 0 17 HTTP/2")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != 304 || rec.RequestTime != 17*time.Millisecond || rec.Protocol != "HTTP/2.0" {
		t.Fatalf("record = %#v", rec)
	}
}

func TestCollectorExplicitFormatSkipsW3CHeaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(path, WithFormat(FormatIISW3C))
	t.Cleanup(c.Close)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("#Version: 1.0\n#Fields: date time cs-method cs-uri-stem sc-status sc-bytes time-taken cs-version\n2026-08-14 12:34:56 GET / 200 10 2 HTTP/1.1\n")
	_ = f.Close()
	if err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	snap := c.Snapshot()
	if snap.Lines != 1 || snap.Health.Dropped != 0 {
		t.Fatalf("snapshot = %#v", snap)
	}
}

func TestWithFormatCanonicalizesAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(path, WithFormat("caddy"))
	t.Cleanup(c.Close)
	if c.format != FormatCaddyJSON {
		t.Fatalf("format = %q", c.format)
	}
}

func TestCollectorInvalidFormatIsBoundedHealth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(path, WithFormatSpec("secret-invalid-value"))
	t.Cleanup(c.Close)
	if health := c.Health(); health.Status != StatusPartial || health.Message != "invalid-format" {
		t.Fatalf("health = %#v", health)
	}
}
