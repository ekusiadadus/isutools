package dbcap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/sqlstats"
)

func TestDialectCapabilitiesAreExplicitAndPerTarget(t *testing.T) {
	targets := []sqlstats.TargetInfo{
		{ID: "mysql8", Driver: "mysql", Display: "password-secret@db", Purposes: []sqlstats.Purpose{sqlstats.PurposeApp, sqlstats.PurposeExplain}},
		{ID: "maria", Driver: "mysql", Purposes: []sqlstats.Purpose{sqlstats.PurposeApp}},
		{ID: "pg", Driver: "pgx", Purposes: []sqlstats.Purpose{sqlstats.PurposeApp}},
		{ID: "sqlite", Driver: "sqlite3", Purposes: []sqlstats.Purpose{sqlstats.PurposeApp}},
	}
	evidence := map[string]Evidence{
		"mysql8": {Flavor: "mysql", Version: "8.0.45", RowEfficiency: Supported, QueryPlan: Supported, CapabilityProbeRan: true},
		"maria":  {Flavor: "mariadb", Version: "11.8.3"},
	}
	got := ForTargets(targets, evidence)
	if len(got) != 4 || got[0].TargetID != "maria" || got[1].TargetID != "mysql8" || got[2].Dialect != "postgresql" || got[3].Dialect != "sqlite" {
		t.Fatalf("targets=%#v", got)
	}
	if state(got[1], "query_plan") != Supported || state(got[0], "query_plan") != Unsupported || state(got[2], "schema") != Unsupported || state(got[3], "sql_aggregate") != Supported {
		t.Fatalf("capabilities=%#v", got)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "password-secret") || strings.Contains(string(body), "@db") {
		t.Fatalf("capabilities leaked display/credential: %s", body)
	}
}

func TestCanonicalMatrixIncludesSQLiteWithoutDeepClaims(t *testing.T) {
	rows := CanonicalMatrix()
	if len(rows) == 0 || rows[0].SQLite != Supported {
		t.Fatalf("matrix = %#v", rows)
	}
	for _, row := range rows {
		if row.Feature != "sql_aggregate" && row.Feature != "db_pool" && row.SQLite != Unsupported {
			t.Fatalf("SQLite deep capability %s = %s", row.Feature, row.SQLite)
		}
	}
}

func TestMissingExplainCredentialDoesNotBorrowAnotherTarget(t *testing.T) {
	got := ForTargets([]sqlstats.TargetInfo{
		{ID: "a", Driver: "mysql", Purposes: []sqlstats.Purpose{sqlstats.PurposeApp, sqlstats.PurposeExplain}},
		{ID: "b", Driver: "mysql", Purposes: []sqlstats.Purpose{sqlstats.PurposeApp}},
	}, nil)
	if state(got[0], "query_plan") != Unverified || state(got[1], "query_plan") != ConfigMissing {
		t.Fatalf("capability credentials crossed targets: %#v", got)
	}
}

func state(target Target, feature string) State {
	for _, capability := range target.Capabilities {
		if capability.Feature == feature {
			return capability.State
		}
	}
	return ""
}
