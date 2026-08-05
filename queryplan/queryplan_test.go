package queryplan

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/sqlrows"
)

// TestEnabledIsOptIn: EXPLAIN issues extra statements against the measured
// database, so anything other than an explicit yes leaves the feature off.
func TestEnabledIsOptIn(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "0", want: false},
		{value: "off", want: false},
		{value: "no", want: false},
		{value: "maybe", want: false},
		{value: "1", want: true},
		{value: "on", want: true},
		{value: "ON", want: true},
		{value: " true ", want: true},
		{value: "yes", want: true},
		{value: "enabled", want: true},
	}
	for _, tc := range tests {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(EnvFlag, tc.value)
			if got := Enabled(); got != tc.want {
				t.Fatalf("Enabled() with %q = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestTopN(t *testing.T) {
	tests := []struct {
		value string
		want  int
	}{
		{value: "", want: DefaultTop},
		{value: "  ", want: DefaultTop},
		{value: "0", want: DefaultTop},
		{value: "-3", want: DefaultTop},
		{value: "twelve", want: DefaultTop},
		{value: "3", want: 3},
		{value: strconv.Itoa(MaxTop + 1), want: MaxTop},
	}
	for _, tc := range tests {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(EnvTop, tc.value)
			if got := TopN(); got != tc.want {
				t.Fatalf("TopN() with %q = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// TestCaptureUsesTheEnvironmentCeilingWhenTopIsUnset keeps the wiring honest:
// an integration that does not pass Top gets the configured ceiling rather
// than an unbounded selection.
func TestCaptureUsesTheEnvironmentCeilingWhenTopIsUnset(t *testing.T) {
	t.Setenv(EnvTop, "1")
	server := newServer().
		withSample("d1", "SELECT 1", baseTime.Add(10*time.Second)).
		withSample("d2", "SELECT 2", baseTime.Add(10*time.Second))
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1",
		selectStat("d1", "SELECT ?", 200),
		selectStat("d2", "SELECT ?", 100),
	))
	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if section.Top != 1 {
		t.Fatalf("top = %d, want the environment's ceiling", section.Top)
	}
	target, _ := findTarget(section, "db1")
	if len(target.Plans) != 1 {
		t.Fatalf("plans = %d, want the ceiling honoured", len(target.Plans))
	}
}

// TestSectionJSONShape pins the wire format the dashboard and the diff read.
func TestSectionJSONShape(t *testing.T) {
	rows, queriers, _ := oneTarget("SELECT id FROM posts")
	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers), Top: 10})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	encoded, err := json.Marshal(section)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Targets []struct {
			TargetID  string `json:"target_id"`
			Schema    string `json:"schema"`
			Explained bool   `json:"explained"`
			Plans     []struct {
				Digest      string `json:"digest"`
				Query       string `json:"query"`
				SampleSeen  string `json:"sample_seen"`
				Freshness   string `json:"freshness"`
				FreshReason string `json:"fresh_reason"`
				Rows        []struct {
					Type  string `json:"type"`
					Rows  int64  `json:"rows"`
					Extra string `json:"extra"`
					Key   string `json:"key"`
				} `json:"rows"`
			} `json:"plans"`
		} `json:"targets"`
		Top int `json:"top"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Targets) != 1 || decoded.Top != 10 {
		t.Fatalf("decoded = %+v", decoded)
	}
	target := decoded.Targets[0]
	if target.TargetID != "db1" || target.Schema != "isuconp" || !target.Explained {
		t.Fatalf("target = %+v", target)
	}
	plan := target.Plans[0]
	if plan.Freshness != string(FreshnessFresh) || plan.FreshReason != string(FreshInInterval) {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.SampleSeen != baseTime.Add(15*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("sample_seen = %q, want the database's own UTC reading", plan.SampleSeen)
	}
	if plan.Rows[0].Type != "ALL" || plan.Rows[0].Rows != 12345 || plan.Rows[0].Key != "" {
		t.Fatalf("plan row = %+v, want the NULL key omitted", plan.Rows[0])
	}
}

// TestSectionSurvivesAnEmptyInterval: a run with no database targets is a
// normal run, not a failure.
func TestSectionSurvivesAnEmptyInterval(t *testing.T) {
	section, err := Capture(context.Background(), Input{Rows: &sqlrows.Section{}})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(section.Targets) != 0 || len(section.Health) != 0 {
		t.Fatalf("section = %+v, want an empty one", section)
	}
	if _, err := json.Marshal(section); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}
