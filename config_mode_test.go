package isutools

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/sqlstats"
)

func TestResolveGlobalModeAcceptedOffSpellings(t *testing.T) {
	for _, raw := range []string{"off", "OFF", " false ", "0", "no", "disabled"} {
		t.Run(raw, func(t *testing.T) {
			cfg := resolveGlobalConfig(func(string) string { return raw })
			if !cfg.Off || cfg.Code != "hard-off" {
				t.Fatalf("resolveGlobalConfig(%q) = %+v, want hard off", raw, cfg)
			}
		})
	}
}

func TestResolveGlobalModeUnknownFailsOpenWithoutEchoingRawValue(t *testing.T) {
	cfg := resolveGlobalConfig(func(string) string { return "token-shaped-unknown-value" })
	if cfg.Off || cfg.Code != "unknown-value" {
		t.Fatalf("config = %+v, want enabled fail-open with unknown-value", cfg)
	}
	if cfg.Message == "" || strings.Contains(cfg.Message, "token-shaped-unknown-value") {
		t.Fatalf("message = %q, want bounded reason without raw value", cfg.Message)
	}
}

func TestProcessConfigIsResolvedOnlyOnce(t *testing.T) {
	raw := "off"
	state := newProcessConfig(func(string) string { return raw })
	if !state.Resolve().Off {
		t.Fatal("first resolution must be off")
	}
	raw = "on"
	if !state.Resolve().Off {
		t.Fatal("mode changed after startup resolution")
	}
}

func TestHardOffEntryPointsAreSideEffectFree(t *testing.T) {
	restore := replaceProcessConfigForTest(func(string) string { return "off" })
	t.Cleanup(restore)

	next := &fixedHandler{}
	if got := HTTP(next); got != next {
		t.Fatal("HTTP wrapped the application handler while hard-off")
	}
	if got := SQLDriverName("unregistered-driver"); got != "unregistered-driver" {
		t.Fatalf("SQLDriverName = %q, want raw driver", got)
	}
	if err := RegisterSQL("unregistered-driver"); err != nil {
		t.Fatalf("RegisterSQL hard-off = %v", err)
	}
	if err := RegisterDBTarget("target", "mysql", "secret-dsn"); err != nil {
		t.Fatalf("RegisterDBTarget hard-off = %v", err)
	}
	if err := RegisterDBInspector("target", sqlstats.PurposeStats, "mysql", "secret-dsn"); err != nil {
		t.Fatalf("RegisterDBInspector hard-off = %v", err)
	}
	if err := WatchDBPool("not-registered", (*sql.DB)(nil)); err != nil {
		t.Fatalf("WatchDBPool hard-off = %v, want unconditional no-op", err)
	}
	if err := UnwatchDBPool("not-registered"); err != nil {
		t.Fatalf("UnwatchDBPool hard-off = %v", err)
	}
	Count("ignored")
	AddCount("ignored", 10)
	if got, err := ResetNow(context.Background()); err != nil || got.RunID != "" {
		t.Fatalf("ResetNow hard-off = (%+v, %v)", got, err)
	}

	before := measurementCore
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("hard-off Handler status = %d, want 404", rec.Code)
	}
	if measurementCore != before {
		t.Fatal("hard-off entry points constructed the measurement singleton")
	}
}

func setHardOffForTest(t *testing.T) {
	t.Helper()
	restore := replaceProcessConfigForTest(func(string) string { return "off" })
	t.Cleanup(restore)
}
