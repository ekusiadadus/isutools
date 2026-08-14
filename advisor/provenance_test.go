package advisor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEveryAdvisorCheckHasSecretFreeDeterministicProvenance(t *testing.T) {
	options := Options{
		DriverName: "mysql", DSN: "user:password-secret@tcp(db:3306)/app?token=token-secret",
		NginxConf: []byte("# authorization-secret\nworker_connections 100;"),
	}
	first := Collect(context.Background(), options)
	second := Collect(context.Background(), options)
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("checks=%d/%d", len(first), len(second))
	}
	for i, check := range first {
		p := check.Provenance
		if p.RuleVersion == "" || p.Category == "" || p.Source == "" || p.Freshness == "" || p.Scope == "" || p.Formula == "" || p.Actual == "" || p.Unit == "" || p.Limitation == "" || p.Docs == "" {
			t.Fatalf("check %q provenance=%#v", check.ID, p)
		}
		left, _ := json.Marshal(p)
		right, _ := json.Marshal(second[i].Provenance)
		if string(left) != string(right) {
			t.Fatalf("nondeterministic provenance for %q: %s != %s", check.ID, left, right)
		}
		for _, secret := range []string{"password-secret", "token-secret", "authorization-secret", "user:"} {
			if strings.Contains(string(left), secret) {
				t.Fatalf("provenance leaked %q: %s", secret, left)
			}
		}
	}
}

func TestDynamicAdvisorReplacementKeepsProvenance(t *testing.T) {
	checks := WithCacheTelemetry(Collect(context.Background(), Options{}), &CacheTelemetry{Hits: 10, Misses: 2}, nil)
	for _, check := range checks {
		if check.Provenance.RuleVersion == "" {
			t.Fatalf("dynamic check %q lost provenance", check.ID)
		}
	}
}
