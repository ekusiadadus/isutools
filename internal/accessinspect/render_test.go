package accessinspect

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func renderFixture() Report {
	return Report{
		Schema:      ReportSchema,
		Percentiles: []float64{50, 99},
		Health:      Health{Lines: 1, Parsed: 1, Filtered: 1},
		Rows: []Row{{
			Method: "GET", Path: `=HYPERLINK("https://bad")`, Count: 1,
			StatusClasses: [5]int64{0, 1, 0, 0, 0},
			Request: DurationStats{Count: 1, Min: time.Millisecond, Max: time.Millisecond, Sum: time.Millisecond, Avg: time.Millisecond,
				Percentiles: []Percentile{{Percent: 50, Value: time.Millisecond}, {Percent: 99, Value: time.Millisecond}}},
			Body: ByteStats{Count: 1, Min: 3, Max: 3, Sum: 3, Avg: 3},
		}},
	}
}

func TestRenderDeterministicFormats(t *testing.T) {
	for _, format := range []OutputFormat{OutputTable, OutputJSON, OutputMarkdown, OutputTSV, OutputCSV} {
		t.Run(string(format), func(t *testing.T) {
			var a, b bytes.Buffer
			if err := Render(&a, renderFixture(), RenderOptions{Format: format}); err != nil {
				t.Fatal(err)
			}
			if err := Render(&b, renderFixture(), RenderOptions{Format: format}); err != nil {
				t.Fatal(err)
			}
			if a.String() != b.String() || a.Len() == 0 {
				t.Fatalf("non-deterministic or empty output")
			}
			if strings.Contains(a.String(), "never-export") {
				t.Fatalf("secret value appeared in output")
			}
		})
	}
}

func TestRenderJSONRoundTrip(t *testing.T) {
	var output bytes.Buffer
	if err := Render(&output, renderFixture(), RenderOptions{Format: OutputJSON}); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != ReportSchema || len(decoded.Rows) != 1 {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestDelimitedOutputNeutralizesSpreadsheetFormula(t *testing.T) {
	for _, format := range []OutputFormat{OutputCSV, OutputTSV} {
		var output bytes.Buffer
		if err := Render(&output, renderFixture(), RenderOptions{Format: format}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), `,=HYPERLINK`) || strings.Contains(output.String(), "\t=HYPERLINK") {
			t.Fatalf("formula was not neutralized: %q", output.String())
		}
		if !strings.Contains(output.String(), `'=`) {
			t.Fatalf("safe prefix missing: %q", output.String())
		}
	}
}

func TestRenderSortLimitAndColumns(t *testing.T) {
	report := renderFixture()
	second := report.Rows[0]
	second.Path = "/fast"
	second.Count = 10
	report.Rows = append(report.Rows, second)
	var output bytes.Buffer
	err := Render(&output, report, RenderOptions{Format: OutputMarkdown, Sort: "count", Reverse: true, Limit: 1, Columns: []string{"method", "path", "count"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "/fast") || strings.Contains(output.String(), "request_avg") {
		t.Fatalf("output=%q", output.String())
	}
}
