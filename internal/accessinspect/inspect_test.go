package accessinspect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
)

func fixtureLines() string {
	return strings.Join([]string{
		`{"schema":"isutools.http-access.v1","method":"GET","uri":"/items?token=never-export","status":200,"duration_ms":100,"bytes":10,"upstream_duration_ms":70,"cache_status":"MISS","content_type":"application/json","protocol":"HTTP/2.0"}`,
		`{"schema":"isutools.http-access.v1","method":"GET","uri":"/items?id=private","status":503,"duration_ms":300,"bytes":30,"upstream_duration_ms":200,"cache_status":"HIT","content_type":"application/json","protocol":"HTTP/2.0"}`,
		`{"schema":"isutools.http-access.v1","method":"POST","uri":"/items","status":201,"duration_ms":200,"bytes":20,"cache_status":"-","content_type":"application/json","protocol":"HTTP/1.1"}`,
		`malformed Cookie: secret Authorization: bearer`,
	}, "\n") + "\n"
}

func TestInspectReadsEveryCanonicalProxyFixture(t *testing.T) {
	file, err := os.Open("../../examples/proxies/fixtures.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	seen := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			t.Fatalf("malformed fixture=%q", line)
		}
		format, err := accesslog.ParseFormat(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		report, err := Inspect(strings.NewReader(parts[2]+"\n"), Options{Format: format})
		if err != nil || report.Health.Parsed != 1 || len(report.Rows) != 1 || report.Rows[0].Count != 1 {
			t.Fatalf("%s report=%+v err=%v", parts[0], report, err)
		}
		seen++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != 12 {
		t.Fatalf("fixtures=%d", seen)
	}
}

func TestOfflineAndOnlineCommonAggregatesAgree(t *testing.T) {
	offline, err := Inspect(strings.NewReader(fixtureLines()), Options{Format: accesslog.FormatIsutoolsJSON})
	if err != nil {
		t.Fatal(err)
	}
	online := accesslog.NewAggregator(100)
	for _, line := range strings.Split(strings.TrimSpace(fixtureLines()), "\n") {
		record, parseErr := accesslog.ParseLineFormat(accesslog.FormatIsutoolsJSON, line)
		if parseErr == nil {
			online.Observe(record)
		}
	}
	snapshot := online.Snapshot()
	var offlineCount, offline5xx, offlineBytes int64
	var offlineDuration time.Duration
	for _, row := range offline.Rows {
		offlineCount += row.Count
		offline5xx += row.StatusClasses[4]
		offlineBytes += row.Body.Sum
		offlineDuration += row.Request.Sum
	}
	var onlineCount, online5xx, onlineBytes int64
	var onlineDuration time.Duration
	for _, row := range snapshot.Entries {
		onlineCount += row.Count
		online5xx += row.Status5xx
		onlineBytes += row.BytesTotal
		onlineDuration += row.RequestTotal
	}
	if offlineCount != onlineCount || offline5xx != online5xx || offlineBytes != onlineBytes || offlineDuration != onlineDuration {
		t.Fatalf("offline=(%d,%d,%d,%s) online=(%d,%d,%d,%s)", offlineCount, offline5xx, offlineBytes, offlineDuration, onlineCount, online5xx, onlineBytes, onlineDuration)
	}
}

func TestInspectBoundsTenThousandHighCardinalityPaths(t *testing.T) {
	var input strings.Builder
	for index := 0; index < 10_050; index++ {
		_, _ = fmt.Fprintf(&input, "{\"schema\":\"isutools.http-access.v1\",\"method\":\"GET\",\"uri\":\"/item/%d?token=private-%d\",\"status\":200,\"duration_ms\":1,\"bytes\":1}\n", index, index)
	}
	report, err := Inspect(strings.NewReader(input.String()), Options{Format: accesslog.FormatIsutoolsJSON, MaxKeys: 10_000, MaxRecords: 20_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Rows) != 10_000 || report.Health.OverflowKeys != 51 || report.Health.QueryStripped != 10_050 {
		t.Fatalf("rows=%d health=%+v", len(report.Rows), report.Health)
	}
	if report.Rows[len(report.Rows)-1].Path == "" {
		t.Fatal("empty aggregation path")
	}
}

func FuzzInspectBounded(f *testing.F) {
	f.Add(fixtureLines())
	f.Add("partial Cookie: secret")
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 64<<10 {
			t.Skip()
		}
		_, _ = Inspect(strings.NewReader(input), Options{Format: accesslog.FormatAuto, MaxInputBytes: 64 << 10, MaxLineBytes: 8 << 10, MaxRecords: 1000, MaxKeys: 100})
	})
}

func BenchmarkInspectTenThousandRecords(b *testing.B) {
	var input strings.Builder
	for index := 0; index < 10_000; index++ {
		_, _ = fmt.Fprintf(&input, "{\"schema\":\"isutools.http-access.v1\",\"method\":\"GET\",\"uri\":\"/item/%d\",\"status\":200,\"duration_ms\":1,\"bytes\":1}\n", index%100)
	}
	body := input.String()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for index := 0; index < b.N; index++ {
		if _, err := Inspect(strings.NewReader(body), Options{Format: accesslog.FormatIsutoolsJSON}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestInspectProducesExactBoundedStatistics(t *testing.T) {
	report, err := Inspect(strings.NewReader(fixtureLines()), Options{
		Format:      accesslog.FormatIsutoolsJSON,
		Percentiles: []float64{50, 90, 99},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Health.Lines != 4 || report.Health.Parsed != 3 || report.Health.Malformed != 1 {
		t.Fatalf("health=%+v", report.Health)
	}
	if report.Health.Filtered != 3 || report.Health.QueryStripped != 2 {
		t.Fatalf("health=%+v", report.Health)
	}
	if len(report.Rows) != 2 {
		t.Fatalf("rows=%+v", report.Rows)
	}
	row := report.Rows[0]
	if row.Method != "GET" || row.Path != "/items" || row.Count != 2 {
		t.Fatalf("first row=%+v", row)
	}
	if row.StatusClasses != [5]int64{0, 1, 0, 0, 1} {
		t.Fatalf("status classes=%v", row.StatusClasses)
	}
	if row.Request.Min != 100*time.Millisecond || row.Request.Max != 300*time.Millisecond || row.Request.Avg != 200*time.Millisecond {
		t.Fatalf("request=%+v", row.Request)
	}
	if row.Request.Stddev != 100*time.Millisecond {
		t.Fatalf("stddev=%v", row.Request.Stddev)
	}
	if got := row.Request.Percentiles; len(got) != 3 || got[0].Value != 100*time.Millisecond || got[2].Value != 300*time.Millisecond {
		t.Fatalf("percentiles=%+v", got)
	}
	if row.Body.Min != 10 || row.Body.Max != 30 || row.Body.Sum != 40 || row.Body.Avg != 20 {
		t.Fatalf("body=%+v", row.Body)
	}
	if row.Upstream.Count != 2 || row.Upstream.Sum != 270*time.Millisecond || row.Residual.Sum != 130*time.Millisecond {
		t.Fatalf("upstream=%+v residual=%+v", row.Upstream, row.Residual)
	}
	if row.NoUpstream != 0 {
		t.Fatalf("no upstream=%d", row.NoUpstream)
	}
}

func TestInspectFilterAndPathRules(t *testing.T) {
	rules, err := accesslog.ParsePathRules(`^/items$=/items/:id`, accesslog.UnmatchedKeep)
	if err != nil {
		t.Fatal(err)
	}
	filter, err := CompileFilter(`status >= 500`)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(strings.NewReader(fixtureLines()), Options{
		Format: accesslog.FormatIsutoolsJSON, Filter: filter, PathRules: rules,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Health.Parsed != 3 || report.Health.Filtered != 1 || len(report.Rows) != 1 || report.Rows[0].Path != "/items/:id" {
		t.Fatalf("report=%+v", report)
	}
}

func TestInspectLimitsDoNotLeakMalformedLines(t *testing.T) {
	_, err := Inspect(strings.NewReader("authorization=secret-and-too-long\n"), Options{
		Format: accesslog.FormatIsutoolsJSON, MaxLineBytes: 8,
	})
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Code != "line-too-large" {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked input: %v", err)
	}

	_, err = Inspect(strings.NewReader(fixtureLines()), Options{
		Format: accesslog.FormatIsutoolsJSON, MaxRecords: 2,
	})
	if !errors.As(err, &limitErr) || limitErr.Code != "records-limit" {
		t.Fatalf("error=%v", err)
	}
}

func TestInspectContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Inspect(strings.NewReader(fixtureLines()), Options{Context: ctx})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestInspectCountsRotationDuplicatesAndDropsTrailingPartialFragment(t *testing.T) {
	line := `{"schema":"isutools.http-access.v1","method":"GET","uri":"/duplicated","status":200,"duration_ms":10,"bytes":1}`
	report, err := Inspect(strings.NewReader(line+"\n"+line+"\n"+`{"schema":"isutools.http-access.v1","method":`), Options{Format: accesslog.FormatIsutoolsJSON})
	if err != nil {
		t.Fatal(err)
	}
	if report.Health.Lines != 3 || report.Health.Parsed != 2 || report.Health.Malformed != 1 || len(report.Rows) != 1 || report.Rows[0].Count != 2 {
		t.Fatalf("report=%+v", report)
	}
}
