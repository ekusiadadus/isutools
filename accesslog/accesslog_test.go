package accesslog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	lineA = "time:2026-08-03T21:56:51+09:00\tmethod:GET\turi:/posts/1\tstatus:200\treqtime:0.250\tupstime:0.120, 0.030 : 0.010\tbytes:4096\tcache:MISS\tctype:text/html\n"
	lineB = "time:2026-08-03T21:56:52+09:00\tmethod:GET\turi:/posts/1\tstatus:304\treqtime:0.050\tupstime:-\tbytes:0\tcache:HIT\tctype:text/html\n"
	lineC = "time:2026-08-03T21:56:53+09:00\tmethod:GET\turi:/image/1.jpg\tstatus:200\treqtime:0.100\tupstime:0.080\tbytes:2048\tcache:HIT\tctype:image/jpeg\n"
)

func TestParseNginxLTSV(t *testing.T) {
	line := strings.Replace(lineA, "uri:/posts/1", `uri:/posts/1?q=secret`, 1)
	line = strings.TrimSpace(line) + "\tproto:HTTP/3.0"
	rec, err := ParseNginxLTSV(line)
	if err != nil {
		t.Fatalf("ParseNginxLTSV: %v", err)
	}
	if rec.Method != "GET" || rec.URI != "/posts/1" || !rec.QueryStripped {
		t.Fatalf("request identity = %#v", rec)
	}
	if rec.Status != 200 || rec.RequestTime != 250*time.Millisecond || rec.Bytes != 4096 {
		t.Fatalf("response fields = %#v", rec)
	}
	if rec.Protocol != "HTTP/3.0" {
		t.Fatalf("protocol = %q", rec.Protocol)
	}
	if rec.UpstreamRaw != "0.120, 0.030 : 0.010" || rec.UpstreamAttempts != 3 || rec.UpstreamTotal != 160*time.Millisecond {
		t.Fatalf("upstream fields = %#v", rec)
	}
	if !rec.UpstreamValid || !rec.UpstreamComplete || rec.NoUpstreamTiming {
		t.Fatalf("upstream validity = %#v", rec)
	}
}

func TestAggregatorProtocolBreakdown(t *testing.T) {
	a := NewAggregator(100)
	for i := 0; i < 3; i++ {
		a.Observe(Record{Method: "GET", URI: "/", Protocol: "HTTP/3.0", Status: 200, RequestTime: time.Duration(i+1) * time.Millisecond})
	}
	a.Observe(Record{Method: "GET", URI: "/", Protocol: "HTTP/2.0", Status: 503, RequestTime: 10 * time.Millisecond})
	snap := a.Snapshot()
	if len(snap.Protocols) != 2 {
		t.Fatalf("protocols = %#v", snap.Protocols)
	}
	if got := snap.Protocols[0]; got.Protocol != "HTTP/3.0" || got.Count != 3 || got.Status5xx != 0 || got.RequestP95 == 0 {
		t.Errorf("HTTP/3 aggregate = %#v", got)
	}
	if got := snap.Protocols[1]; got.Protocol != "HTTP/2.0" || got.Count != 1 || got.Status5xx != 1 {
		t.Errorf("HTTP/2 aggregate = %#v", got)
	}

	a.Reset()
	if got := a.Snapshot().Protocols; len(got) != 0 {
		t.Errorf("protocols must reset with generation: %#v", got)
	}
}

func TestParseNginxLTSVDashAndInvalidUpstream(t *testing.T) {
	rec, err := ParseNginxLTSV(strings.TrimSpace(lineB))
	if err != nil {
		t.Fatalf("dash is a valid nginx value: %v", err)
	}
	if !rec.UpstreamValid || rec.UpstreamComplete || !rec.NoUpstreamTiming || rec.UpstreamAttempts != 0 {
		t.Fatalf("dash upstream = %#v", rec)
	}

	rec, err = ParseNginxLTSV(strings.TrimSpace(strings.Replace(lineA, "upstime:0.120, 0.030 : 0.010", "upstime:0.120, broken", 1)))
	if err != nil {
		t.Fatalf("invalid optional timing should preserve the rest of the record: %v", err)
	}
	if rec.UpstreamValid || !rec.Partial || rec.Issue == "" {
		t.Fatalf("invalid upstream must be marked partial: %#v", rec)
	}
}

func TestParseNginxLTSVEmptyUpstreamForStaticResponse(t *testing.T) {
	line := strings.Replace(lineC, "upstime:0.080", "upstime:", 1)
	rec, err := ParseNginxLTSV(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("empty nginx $upstream_response_time must be a valid static response: %v", err)
	}
	if !rec.UpstreamValid || rec.UpstreamComplete || !rec.NoUpstreamTiming || rec.UpstreamAttempts != 0 {
		t.Fatalf("empty upstream timing = %#v", rec)
	}
	if rec.Partial || rec.Issue != "" {
		t.Fatalf("a static response must not degrade access-log health: %#v", rec)
	}

	a := NewAggregator(10)
	a.Observe(rec)
	if got := a.Snapshot(); got.PartialLines != 0 || got.Lines != 1 {
		t.Fatalf("static response aggregate = %#v", got)
	}
}

func TestParseNginxLTSVRejectsMalformedRequiredFields(t *testing.T) {
	tests := []string{
		"method:GET\turi:/\tstatus:200\treqtime:0.1\tupstime:-\tbytes:1\tcache:-", // ctype missing
		"method:GET\turi:/\tstatus:nope\treqtime:0.1\tupstime:-\tbytes:1\tcache:-\tctype:-",
		"method:GET\turi:/\tstatus:200\treqtime:-1\tupstime:-\tbytes:1\tcache:-\tctype:-",
		"method:GET\turi:/\tstatus:200\treqtime:NaN\tupstime:-\tbytes:1\tcache:-\tctype:-",
		"method:GET\turi:/\tstatus:200\treqtime:+Inf\tupstime:-\tbytes:1\tcache:-\tctype:-",
		"method:GET\turi:/\tstatus:200\treqtime:0.1\tupstime:-\tbytes:-1\tcache:-\tctype:-",
		"method:GET\turi:/\tstatus:200\treqtime:0.1\tupstime:-\tbytes:1\tcache:-\tctype:-\tstatus:201",
	}
	for _, line := range tests {
		if _, err := ParseNginxLTSV(line); err == nil {
			t.Errorf("ParseNginxLTSV(%q) unexpectedly succeeded", line)
		}
	}
}

func TestParseNginxLTSVDecodesJSONEscapes(t *testing.T) {
	line := "method:GET\turi:/images/hello\\u0020\\\"world\\\"\tstatus:200\treqtime:0.001\tupstime:-\tbytes:1\tcache:-\tctype:text/plain"
	rec, err := ParseNginxLTSV(line)
	if err != nil {
		t.Fatal(err)
	}
	if rec.URI != `/images/hello "world"` {
		t.Fatalf("URI = %q", rec.URI)
	}
}

func TestParseNginxLTSVMultipleMissingUpstreams(t *testing.T) {
	rec, err := ParseNginxLTSV(strings.TrimSpace(strings.Replace(lineA, "upstime:0.120, 0.030 : 0.010", "upstime:-, - : -", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.UpstreamValid || !rec.NoUpstreamTiming || rec.UpstreamComplete || rec.UpstreamAttempts != 0 {
		t.Fatalf("missing upstream list = %#v", rec)
	}
}

func TestAggregatorAxesAndWebSocketExclusion(t *testing.T) {
	a := NewAggregator(100)
	for _, line := range []string{
		lineA,
		lineB,
		strings.Replace(lineA, "status:200", "status:499", 1),
		strings.Replace(lineA, "status:200", "status:503", 1),
		strings.Replace(lineA, "status:200", "status:101", 1),
		strings.Replace(lineA, "upstime:0.120, 0.030 : 0.010", "upstime:broken", 1),
	} {
		rec, err := ParseNginxLTSV(strings.TrimSpace(line))
		if err != nil {
			t.Fatal(err)
		}
		a.Observe(rec)
	}

	snap := a.Snapshot()
	if len(snap.Entries) != 1 {
		t.Fatalf("entries = %#v", snap.Entries)
	}
	e := snap.Entries[0]
	if e.Method != "GET" || e.URI != "/posts/1" || e.Count != 6 || e.LatencyCount != 5 {
		t.Fatalf("identity/count = %#v", e)
	}
	if e.Status304 != 1 || e.Status499 != 1 || e.Status5xx != 1 {
		t.Fatalf("status axes = %#v", e)
	}
	if e.BytesTotal != 5*4096 || e.UpstreamAttempts != 9 || e.NoUpstreamCount != 1 {
		t.Fatalf("bytes/upstream axes = %#v", e)
	}
	if e.ResidualCount != 3 || e.ResidualTotal != 3*90*time.Millisecond {
		t.Fatalf("residual = %#v", e)
	}
	if snap.PartialLines != 1 {
		t.Fatalf("partial lines = %d", snap.PartialLines)
	}
	if got := dimensionCount(e.CacheStatuses, "HIT"); got != 1 {
		t.Fatalf("cache HIT = %d, dimensions=%#v", got, e.CacheStatuses)
	}
	if got := dimensionCount(e.ContentTypes, "text/html"); got != 6 {
		t.Fatalf("content type count = %d", got)
	}
	if e.RequestP95 <= 0 || e.RequestMax != 250*time.Millisecond {
		t.Fatalf("latency = %#v", e)
	}
}

func TestAggregatorBoundsKeysAndSorts(t *testing.T) {
	a := NewAggregator(1)
	for _, uri := range []string{"/a", "/b", "/c"} {
		rec, err := ParseNginxLTSV(strings.TrimSpace(strings.Replace(lineC, "/image/1.jpg", uri, 1)))
		if err != nil {
			t.Fatal(err)
		}
		a.Observe(rec)
	}
	snap := a.Snapshot()
	if len(snap.Entries) != 2 || snap.Entries[0].URI != OverflowURI || snap.Entries[0].Count != 2 {
		t.Fatalf("bounded entries = %#v", snap.Entries)
	}
}

func TestCollectorUsesGenerationBaselineAndNoDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, lineA)
	c := New(path)
	t.Cleanup(c.Close)

	appendFile(t, path, lineB+lineC)
	snap := c.Snapshot()
	if snap.Lines != 2 || len(snap.Entries) != 2 {
		t.Fatalf("first snapshot = %#v", snap)
	}
	if again := c.Snapshot(); again.Lines != 2 {
		t.Fatalf("repeated snapshot duplicated lines: %#v", again)
	}

	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.Lines != 0 || len(got.Entries) != 0 {
		t.Fatalf("reset must baseline at current EOF: %#v", got)
	}
	appendFile(t, path, lineC)
	if got := c.Snapshot(); got.Lines != 1 || len(got.Entries) != 1 {
		t.Fatalf("new generation = %#v", got)
	}
}

func TestCollectorDrainsRotationAndHandlesCopytruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	writeFile(t, path, "")
	c := New(path)
	t.Cleanup(c.Close)

	appendFile(t, path, lineA)
	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatal(err)
	}
	appendFile(t, rotated, lineB)
	writeFile(t, path, lineC)
	snap := c.Snapshot()
	if snap.Lines != 3 || snap.Health.Rotations != 1 || snap.Health.Status != StatusPartial {
		t.Fatalf("rotation snapshot = %#v", snap)
	}

	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, lineB)
	snap = c.Snapshot()
	if snap.Lines != 1 || snap.Health.CopyTruncates != 1 || snap.Health.Status != StatusPartial {
		t.Fatalf("copytruncate snapshot = %#v", snap)
	}
}

func TestCollectorMalformedAndIncompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "")
	c := New(path, WithMaxLineBytes(1024))
	t.Cleanup(c.Close)

	appendFile(t, path, "malformed\n"+strings.TrimSuffix(lineA, "\n")[:50])
	snap := c.Snapshot()
	if snap.Lines != 0 || snap.Health.Dropped != 1 || snap.Health.Status != StatusPartial {
		t.Fatalf("malformed/incomplete snapshot = %#v", snap)
	}
	appendFile(t, path, strings.TrimSuffix(lineA, "\n")[50:]+"\n")
	snap = c.Snapshot()
	if snap.Lines != 1 || snap.Health.Dropped != 1 {
		t.Fatalf("completed pending line = %#v", snap)
	}
}

func TestCollectorStartsBeforeFileExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.log")
	c := New(path)
	t.Cleanup(c.Close)
	if h := c.Health(); h.Errors == 0 || h.Status != StatusError {
		t.Fatalf("missing-file health = %#v", h)
	}
	writeFile(t, path, lineC)
	snap := c.Snapshot()
	if snap.Lines != 1 || len(snap.Entries) != 1 {
		t.Fatalf("late file snapshot = %#v", snap)
	}
}

func TestCollectorCollectUntilStableWaitsForBufferedAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "")
	c := New(path)
	t.Cleanup(c.Close)

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(15 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return
		}
		_, _ = f.WriteString(lineA)
		_ = f.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.CollectUntilStable(ctx, 25*time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	<-done
	if snap := c.Snapshot(); snap.Lines != 1 {
		t.Fatalf("buffered append was not collected: %#v", snap)
	}
}

func TestCollectContextHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "")
	c := New(path)
	t.Cleanup(c.Close)
	appendFile(t, path, lineA)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.CollectContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CollectContext error = %v, want context.Canceled", err)
	}
}

func TestCollectBoundsBytesPerCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "")
	c := New(path, WithMaxCollectBytes(int64(len(lineA))))
	t.Cleanup(c.Close)
	appendFile(t, path, lineA+lineB)
	if err := c.Collect(); !errors.Is(err, ErrCollectLimit) {
		t.Fatalf("Collect error = %v, want ErrCollectLimit", err)
	}
}

func TestCollectorOptionsOversizedLineAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	writeFile(t, path, "")
	c := New(path,
		WithFileSystem(osFileSystem{}),
		WithSameFile(os.SameFile),
		WithMaxLineBytes(160),
		WithMaxKeys(1),
	)
	appendFile(t, path, strings.Repeat("x", 200)+"\n"+lineA+lineC)
	snap := c.Snapshot()
	if snap.Health.Dropped != 1 {
		t.Fatalf("oversized line health = %#v", snap.Health)
	}
	if len(snap.Entries) != 2 {
		t.Fatalf("max-key overflow entries = %#v", snap.Entries)
	}
	foundOverflow := false
	for _, e := range snap.Entries {
		foundOverflow = foundOverflow || e.URI == OverflowURI
	}
	if !foundOverflow {
		t.Fatalf("missing overflow entry: %#v", snap.Entries)
	}
	c.Close()
	c.Close()
	if err := c.Collect(); err == nil {
		t.Fatal("Collect after Close unexpectedly succeeded")
	}
	if err := c.Reset(); err == nil {
		t.Fatal("Reset after Close unexpectedly succeeded")
	}
}

func dimensionCount(ds []DimensionCount, value string) int64 {
	for _, d := range ds {
		if d.Value == value {
			return d.Count
		}
	}
	return 0
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
