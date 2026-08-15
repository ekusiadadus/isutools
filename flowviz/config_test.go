package flowviz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAcceptsBoundedYAMLAndJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "yaml", body: `version: 1
funnels:
  - id: checkout
    scenario: buyer
    mode: ordered
    within: 2m
    steps:
      - id: cart
        route: GET /cart
      - id: complete
        route: POST /checkout
`},
		{name: "json", body: `{"version":1,"funnels":[{"id":"checkout","scenario":"buyer","mode":"ordered","within":"2m","steps":[{"id":"cart","route":"GET /cart"},{"id":"complete","route":"POST /checkout"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "funnels."+tc.name)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Version != SchemaVersion || len(cfg.Funnels) != 1 || cfg.Funnels[0].Within != "2m" {
				t.Fatalf("config = %#v", cfg)
			}
		})
	}
}

func TestLoadConfigFailsClosed(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.yaml")
	if err := os.WriteFile(valid, []byte("version: 1\nunknown: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(valid); err == nil || !strings.Contains(err.Error(), "unknown-field") {
		t.Fatalf("unknown field error = %v", err)
	}

	symlink := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(symlink); err == nil || !strings.Contains(err.Error(), "not-regular") {
		t.Fatalf("symlink error = %v", err)
	}

	oversized := filepath.Join(dir, "large.yaml")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", MaxConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(oversized); err == nil || !strings.Contains(err.Error(), "too-large") {
		t.Fatalf("large config error = %v", err)
	}

	for name, body := range map[string]string{
		"empty":    "version: 1\n",
		"alias":    "version: 1\nfunnels: &f []\ncopy: *f\n",
		"multiple": "version: 1\nfunnels: []\n---\nversion: 1\n",
	} {
		path := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s config unexpectedly accepted", name)
		}
	}
}

func TestConfigValidationRejectsAmbiguousOrUnsafeFunnels(t *testing.T) {
	tests := []Config{
		{Version: 2},
		{Version: 1, Funnels: []FunnelDefinition{{ID: "bad id", Scenario: "buyer", Mode: ModeOrdered, Steps: validSteps()}}},
		{Version: 1, Funnels: []FunnelDefinition{{ID: "x", Scenario: "buyer", Mode: "guess", Steps: validSteps()}}},
		{Version: 1, Funnels: []FunnelDefinition{{ID: "x", Scenario: "buyer", Mode: ModeOrdered, Within: "forever", Steps: validSteps()}}},
		{Version: 1, Funnels: []FunnelDefinition{{
			ID: "x", Scenario: "buyer", Mode: ModeOrdered,
			Steps: []StepDefinition{{ID: "a", Route: "GET /raw?secret=x"}, {ID: "b", Route: "POST /done"}},
		}}},
		{Version: 1, Funnels: []FunnelDefinition{{
			ID: "x", Scenario: "buyer", Mode: ModeOrdered,
			Steps: []StepDefinition{{ID: "a", Route: "GET /same"}, {ID: "b", Route: "GET /same"}},
		}}},
	}
	for i, cfg := range tests {
		if err := cfg.Validate(); err == nil {
			t.Errorf("case %d unexpectedly valid: %#v", i, cfg)
		}
	}
}

func TestParseConfigRejectsExcessiveStructureBeforeYAMLDecode(t *testing.T) {
	deep := []byte(strings.Repeat("[", MaxConfigNesting+1) + strings.Repeat("]", MaxConfigNesting+1))
	if _, err := parseConfig(deep); err == nil || !strings.Contains(err.Error(), "invalid-yaml") {
		t.Fatalf("deep structure error = %v", err)
	}
	dense := []byte(strings.Repeat("[]", MaxStructuralBytes/2+1))
	if _, err := parseConfig(dense); err == nil || !strings.Contains(err.Error(), "invalid-yaml") {
		t.Fatalf("dense structure error = %v", err)
	}
	var block strings.Builder
	block.WriteString("version: 1\nfunnels: []\nextra:\n")
	for depth := 1; depth <= MaxConfigNesting+1; depth++ {
		block.WriteString(strings.Repeat(" ", depth))
		block.WriteString("nested:\n")
	}
	block.WriteString(strings.Repeat(" ", MaxConfigNesting+2))
	block.WriteString("value\n")
	if _, err := parseConfig([]byte(block.String())); err == nil || !strings.Contains(err.Error(), "invalid-yaml") {
		t.Fatalf("deep block YAML error = %v", err)
	}
}

func TestSafeConfigStructureRejectsBareCarriageReturns(t *testing.T) {
	if safeConfigStructure([]byte(strings.Repeat("\r", 32))) {
		t.Fatal("bare carriage-return stream reached the YAML parser")
	}
	if !safeConfigStructure([]byte("version: 1\r\nfunnels: []\r\n")) {
		t.Fatal("ordinary CRLF YAML was rejected")
	}
}

func validSteps() []StepDefinition {
	return []StepDefinition{{ID: "start", Route: "GET /start"}, {ID: "done", Route: "POST /done"}}
}

func FuzzParseConfig(f *testing.F) {
	f.Add([]byte("version: 1\nfunnels: []\n"))
	f.Add([]byte("version: 1\nunknown: true\n"))
	f.Add([]byte("---\n&a [*a]\n"))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = parseConfig(body)
	})
}
