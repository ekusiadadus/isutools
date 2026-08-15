package slowlog

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const sampleSlowLog = `# Time: 2026-08-15T01:00:00.000000Z
# User@Host: isucon[isucon] @ localhost []  Id: 10
# Query_time: 0.010000  Lock_time: 0.001000 Rows_sent: 1  Rows_examined: 100
SET timestamp=1786755600;
SELECT * FROM users WHERE id = 123 AND email = 'alice@example.com';
# Time: 2026-08-15T01:00:01.000000Z
# User@Host: isucon[isucon] @ localhost []  Id: 11
# Query_time: 1.010000  Lock_time: 0.500000 Rows_sent: 1  Rows_examined: 200
SET timestamp=1786755601;
SELECT * FROM users WHERE id = 999 AND email = 'bob@example.com';
# Time: 2026-08-15T01:00:02.000000Z
# Query_time: 0.020000  Lock_time: 0.000000 Rows_sent: 0  Rows_examined: 1 Rows_affected: 1
SET timestamp=1786755602;
UPDATE users SET last_seen = '2026-08-15' WHERE id = 123;
`

func TestParseAggregatesEventsWithoutRawSQL(t *testing.T) {
	report, err := Parse(strings.NewReader(sampleSlowLog), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Schema != ReportSchema || report.Health.Events != 3 || report.Health.Classes != 2 {
		t.Fatalf("report=%+v", report)
	}
	selectClass := report.Classes[0]
	if selectClass.Operation != "SELECT" || selectClass.Count != 2 || len(selectClass.FingerprintSHA256) != 64 {
		t.Fatalf("select=%+v", selectClass)
	}
	if selectClass.QueryTime.Min != 10*time.Millisecond || selectClass.QueryTime.Max != 1010*time.Millisecond || selectClass.QueryTime.Median != 10*time.Millisecond || selectClass.QueryTime.P95 != 1010*time.Millisecond {
		t.Fatalf("query time=%+v", selectClass.QueryTime)
	}
	if selectClass.LockTime.Max != 500*time.Millisecond || selectClass.RowsExamined.Sum != 300 || selectClass.RowsSent.Sum != 2 {
		t.Fatalf("select=%+v", selectClass)
	}
	if selectClass.FirstEvent.IsZero() || selectClass.LastEvent.Sub(selectClass.FirstEvent) != time.Second {
		t.Fatalf("event range=%s..%s", selectClass.FirstEvent, selectClass.LastEvent)
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"alice@example.com", "bob@example.com", "123", "999", "isucon", "localhost", "last_seen"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("report leaked %q: %s", secret, body)
		}
	}
}

func TestParseMarksPartialAndBoundsInput(t *testing.T) {
	partial := `# Query_time: 1.0 Lock_time: 0 Rows_sent: 0 Rows_examined: 1
SELECT * FROM users WHERE token = 'secret'
`
	report, err := Parse(strings.NewReader(partial), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Health.PartialEvents != 1 || report.Coverage.Complete || report.Coverage.Reason != "partial-event" {
		t.Fatalf("report=%+v", report)
	}
	_, err = Parse(strings.NewReader(sampleSlowLog), Options{MaxEvents: 1})
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Code != "events-limit" {
		t.Fatalf("error=%v", err)
	}
	_, err = Parse(strings.NewReader(strings.Repeat("x", 100)), Options{MaxLineBytes: 8})
	if !errors.As(err, &limitErr) || limitErr.Code != "line-too-large" || strings.Contains(err.Error(), "xxx") {
		t.Fatalf("error=%v", err)
	}
}

func TestFingerprintGroupsLiteralsButNotStatements(t *testing.T) {
	a := fingerprint("SELECT * FROM users WHERE id=123 AND name='alice'")
	b := fingerprint("select * from users where id=999 and name='bob'")
	c := fingerprint("DELETE FROM users WHERE id=123")
	if a.hash != b.hash || a.operation != "SELECT" || a.hash == c.hash {
		t.Fatalf("a=%+v b=%+v c=%+v", a, b, c)
	}
}
