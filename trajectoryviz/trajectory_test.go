package trajectoryviz

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestParseNDJSONSupportsReassignmentAndOutOfOrderPoints(t *testing.T) {
	input := strings.NewReader(`
{"type":"meta","schema":1,"title":"rolling window"}
{"type":"point","agent_id":"taxi-1","at":"2026-08-05T12:00:02Z","x":2,"y":3}
{"type":"agent","id":"taxi-1","label":"first"}
{"type":"point","agent_id":"taxi-1","at":"2026-08-05T12:00:01Z","x":1,"y":1}
{"type":"agent","id":"taxi-2"}
{"type":"job","id":"job-1","requested_at":"2026-08-05T12:00:00Z","pickup":{"x":4,"y":4},"destination":{"x":9,"y":9}}
{"type":"assignment","job_id":"job-1","agent_id":"taxi-1","at":"2026-08-05T12:00:01Z"}
{"type":"assignment","job_id":"job-1","agent_id":"taxi-2","at":"2026-08-05T12:00:03Z"}
`)
	dataset, err := ParseNDJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(dataset.Agents) != 2 || dataset.Agents[0].ID != "taxi-1" {
		t.Fatalf("agents = %#v", dataset.Agents)
	}
	if got := dataset.Agents[0].Points[0].X; got != 1 {
		t.Fatalf("first sorted point x = %v, want 1", got)
	}
	if len(dataset.Assignments) != 2 || dataset.Assignments[1].AgentID != "taxi-2" {
		t.Fatalf("assignments = %#v", dataset.Assignments)
	}
}

func TestRenderHTMLCarriesNormalizedDatasetWithoutHTMLInjection(t *testing.T) {
	dataset, err := ParseNDJSON(strings.NewReader(`
{"type":"meta","schema":1,"title":"</script><b>unsafe</b>"}
{"type":"agent","id":"a"}
{"type":"point","agent_id":"a","at":"2026-08-05T12:00:00Z","x":1,"y":2}
`))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RenderHTML(&out, dataset); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<title></script>") || strings.Contains(out.String(), "<h1></script>") {
		t.Fatal("title escaped its HTML context")
	}
	match := regexp.MustCompile(`atob\("([A-Za-z0-9+/=]+)"\)`).FindStringSubmatch(out.String())
	if len(match) != 2 {
		t.Fatal("embedded base64 payload was not found")
	}
	raw, err := base64.StdEncoding.DecodeString(match[1])
	if err != nil {
		t.Fatal(err)
	}
	var decoded Dataset
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Title != dataset.Title || len(decoded.Agents) != 1 {
		t.Fatalf("decoded dataset = %#v", decoded)
	}
}

func TestParseNDJSONRejectsUnknownReferences(t *testing.T) {
	_, err := ParseNDJSON(strings.NewReader(`
{"type":"job","id":"job","requested_at":"2026-08-05T12:00:00Z","pickup":{"x":0,"y":0},"destination":{"x":1,"y":1}}
{"type":"assignment","job_id":"job","agent_id":"missing","at":"2026-08-05T12:00:01Z"}
`))
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("error = %v, want unknown agent", err)
	}
}
