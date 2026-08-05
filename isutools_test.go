package isutools

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/counters"
	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/netstats"
	"github.com/ekusiadadus/isutools/queryplan"
	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
	"github.com/ekusiadadus/isutools/web"
)

func TestMain(m *testing.M) {
	// Bind the admin server to an ephemeral port for the whole test binary
	// so tests never collide with a real 19191 listener.
	_ = os.Setenv("ISUTOOLS_ADDR", "127.0.0.1:0")
	os.Exit(m.Run())
}

type rootFakeDriver struct{}

func (rootFakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not used") }

func init() {
	sql.Register("rootfake", rootFakeDriver{})
}

func TestSQLDriverNameEnabled(t *testing.T) {
	if got := SQLDriverName("rootfake"); got != "rootfake:isutools" {
		t.Errorf("SQLDriverName = %q, want rootfake:isutools", got)
	}
}

func TestSQLDriverNameDisabled(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	if got := SQLDriverName("rootfake"); got != "rootfake" {
		t.Errorf("SQLDriverName = %q, want raw name when disabled", got)
	}
}

func TestSQLDriverNameFailOpen(t *testing.T) {
	// Unknown base driver: measurement must never break startup, so the
	// raw name is returned and the app fails (or not) exactly as without us.
	if got := SQLDriverName("never-registered"); got != "never-registered" {
		t.Errorf("SQLDriverName = %q, want raw name on registration failure", got)
	}
}

func TestResolveAdminAddr(t *testing.T) {
	tests := []struct {
		env  string
		want string
	}{
		{"", defaultAdminAddr},
		{"off", ""},
		{":19999", ":19999"},
	}
	for _, tt := range tests {
		if got := resolveAdminAddr(func(string) string { return tt.env }); got != tt.want {
			t.Errorf("resolveAdminAddr(env=%q) = %q, want %q", tt.env, got, tt.want)
		}
	}
}

func TestResolveAccessLogPathPrefersGenericName(t *testing.T) {
	values := map[string]string{
		"ISUTOOLS_ACCESS_LOG": "/var/log/apache2/isutools.json",
		"ISUTOOLS_NGINX_LOG":  "/var/log/nginx/legacy.log",
	}
	if got := resolveAccessLogPath(func(key string) string { return values[key] }); got != values["ISUTOOLS_ACCESS_LOG"] {
		t.Fatalf("access log path = %q", got)
	}
	delete(values, "ISUTOOLS_ACCESS_LOG")
	if got := resolveAccessLogPath(func(key string) string { return values[key] }); got != values["ISUTOOLS_NGINX_LOG"] {
		t.Fatalf("legacy access log path = %q", got)
	}
}

func TestCollectAdviceReadsGenericProxyAndExternalHTTP3Evidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(path, []byte("example.com { reverse_proxy 127.0.0.1:8080 }"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"ISUTOOLS_PROXY_CONF":         path,
		"ISUTOOLS_PROXY_KIND":         "caddy",
		"ISUTOOLS_HTTP3_UDP443":       "blocked",
		"ISUTOOLS_HTTP3_EDGE":         "example-edge",
		"ISUTOOLS_HTTP3_EDGE_ENABLED": "true",
	}
	checks := collectAdviceWithEnv(context.Background(), advisor.Options{}, func(key string) string { return values[key] })
	byID := make(map[string]advisor.Check, len(checks))
	for _, check := range checks {
		byID[check.ID] = check
	}
	if got := byID["http3-server"].Status; got != advisor.StatusOK {
		t.Errorf("server = %q, want ok", got)
	}
	if got := byID["http3-network-path"].Status; got != advisor.StatusWarn {
		t.Errorf("network = %q, want warn", got)
	}
	if got := byID["http3-edge"].Status; got != advisor.StatusOK {
		t.Errorf("edge = %q, want ok", got)
	}
}

func TestReadQUICTelemetryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quic.json")
	if err := os.WriteFile(path, []byte(`{"packets_sent":1000,"packets_retransmitted":5,"udp_datagrams_dropped":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readQUICTelemetryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PacketsSent != 1000 || got.PacketsRetransmitted != 5 || got.UDPDatagramsDropped != 0 {
		t.Errorf("telemetry = %#v", got)
	}
	if err := os.WriteFile(path, []byte(`{"packets_sent":"invalid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readQUICTelemetryFile(path); err == nil {
		t.Fatal("invalid telemetry JSON must fail")
	}
}

func TestReadCacheTelemetryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, []byte(`{"hits":900,"misses":100,"evictions":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readCacheTelemetryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hits != 900 || got.Misses != 100 || got.Evictions != 3 {
		t.Errorf("telemetry = %#v", got)
	}
	if err := os.WriteFile(path, []byte(`{"hits":"invalid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCacheTelemetryFile(path); err == nil {
		t.Fatal("invalid telemetry JSON must fail")
	}
}

func TestCollectAdviceLegacyNginxConfigStillWorks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nginx.conf")
	if err := os.WriteFile(path, []byte("events {} http { server { listen 443 quic; } }"), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := collectAdviceWithEnv(context.Background(), advisor.Options{}, func(key string) string {
		if key == "ISUTOOLS_NGINX_CONF" {
			return path
		}
		return ""
	})
	found := false
	for _, check := range checks {
		if check.ID == "http3-server" {
			found = true
			if check.Status != advisor.StatusOK || !strings.Contains(check.Detail, "quic") {
				t.Errorf("legacy nginx HTTP/3 check = %#v", check)
			}
		}
	}
	if !found {
		t.Fatal("http3-server check missing")
	}
}

func TestAdminServerServesJSON(t *testing.T) {
	startAdmin()
	addr := adminAddr()
	if addr == "" {
		t.Fatal("admin server did not start")
	}
	resp, err := http.Get("http://" + addr + "/json")
	if err != nil {
		t.Fatalf("GET /json: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestHandlerServesJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRegisterSQLUnknownDriver(t *testing.T) {
	if err := RegisterSQL("definitely-not-registered"); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestRegisterSQLDisabled(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	if err := RegisterSQL("definitely-not-registered"); err != nil {
		t.Fatalf("disabled RegisterSQL must be a no-op, got %v", err)
	}
}

// envMap builds a getenv function over a fixed table, so wiring tests never
// depend on the process environment.
func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// newTestMeasurement builds a core whose Controller is private to the test
// and runs no watchdog goroutine.
func newTestMeasurement(t *testing.T, values map[string]string) *measurement {
	t.Helper()
	m := newMeasurementWith(envMap(values), runctl.Options{DisableWatchdog: true}, isolatedGenerationCollectors())
	t.Cleanup(m.ctrl.Close)
	return m
}

// isolatedGenerationCollectors builds a generation collector set private to
// one test.
//
// A generation collector keeps its boundary epoch inside itself, so handing
// the process-wide collectors to a second Controller would make that
// Controller's first boundary look stale and silently drop the section — and
// would advance the shared epoch past the one the real Controller is using.
func isolatedGenerationCollectors() generationCollectors {
	return generationCollectors{
		http:     httpstats.New(),
		sql:      sqlstats.NewGenerationCollector(sqlstats.NewStore(agg.DefaultMaxKeys)),
		counters: counters.NewGenerationCollector(counters.NewRegistry()),
	}
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func TestFeatureEnabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", true},
		{"on", true},
		{"1", true},
		{"anything", true},
		{"off", false},
		{" OFF ", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"disabled", false},
	}
	for _, tt := range tests {
		got := featureEnabled(envMap(map[string]string{"FLAG": tt.value}), "FLAG")
		if got != tt.want {
			t.Errorf("featureEnabled(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestResolveProfileSettingsDefaultsOff(t *testing.T) {
	settings := resolveProfileSettings(envMap(nil))
	if settings.mutexSet || settings.blockSet || settings.heap {
		t.Fatalf("runtime profiles must default to off, got %+v", settings)
	}
	if len(settings.invalid) != 0 {
		t.Errorf("unset variables are not invalid values, got %v", settings.invalid)
	}
}

func TestResolveProfileSettings(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantMutex   int
		wantMutexOK bool
		wantBlock   int
		wantBlockOK bool
		wantHeap    bool
		wantInvalid int
	}{
		{
			name:        "all configured",
			env:         map[string]string{envMutexFraction: "100", envBlockRateNS: "100000", envHeapProfile: "1"},
			wantMutex:   100,
			wantMutexOK: true,
			wantBlock:   100000,
			wantBlockOK: true,
			wantHeap:    true,
		},
		{
			name: "explicit zero is an explicit off",
			// Distinct from "unset": an operator asking for zero gets the
			// setter called, an operator who said nothing does not.
			env:         map[string]string{envMutexFraction: "0", envBlockRateNS: "0"},
			wantMutexOK: true,
			wantBlockOK: true,
		},
		{
			name:        "invalid values are ignored",
			env:         map[string]string{envMutexFraction: "many", envBlockRateNS: "-5"},
			wantInvalid: 2,
		},
		{
			name:     "heap accepts the documented spellings only",
			env:      map[string]string{envHeapProfile: "yes"},
			wantHeap: true,
		},
		{
			name: "heap stays off for other values",
			env:  map[string]string{envHeapProfile: "please"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveProfileSettings(envMap(tt.env))
			if got.mutex != tt.wantMutex || got.mutexSet != tt.wantMutexOK {
				t.Errorf("mutex = (%d, %v), want (%d, %v)", got.mutex, got.mutexSet, tt.wantMutex, tt.wantMutexOK)
			}
			if got.block != tt.wantBlock || got.blockSet != tt.wantBlockOK {
				t.Errorf("block = (%d, %v), want (%d, %v)", got.block, got.blockSet, tt.wantBlock, tt.wantBlockOK)
			}
			if got.heap != tt.wantHeap {
				t.Errorf("heap = %v, want %v", got.heap, tt.wantHeap)
			}
			if len(got.invalid) != tt.wantInvalid {
				t.Errorf("invalid = %v, want %d entries", got.invalid, tt.wantInvalid)
			}
		})
	}
}

// TestProfileApplyKeepsApplicationRate pins the contract that matters most
// here: an unset variable must not disturb a rate the application chose for
// itself, because overriding it would change the very behaviour the profile
// is supposed to describe.
func TestProfileApplyKeepsApplicationRate(t *testing.T) {
	previous := runtime.SetMutexProfileFraction(7)
	t.Cleanup(func() { runtime.SetMutexProfileFraction(previous) })

	kinds := resolveProfileSettings(envMap(nil)).apply(nil)

	if got := runtime.SetMutexProfileFraction(-1); got != 7 {
		t.Fatalf("mutex fraction = %d, want the application's 7 untouched", got)
	}
	// The rate is on, so the profile is worth capturing even though isutools
	// did not enable it.
	if !contains(kinds, "mutex") {
		t.Errorf("kinds = %v, want mutex captured while its rate is non-zero", kinds)
	}
	if contains(kinds, "block") || contains(kinds, "heap") {
		t.Errorf("kinds = %v, want block and heap off by default", kinds)
	}
}

func TestProfileApplyEnablesEveryConfiguredKind(t *testing.T) {
	previous := runtime.SetMutexProfileFraction(0)
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(previous)
		// The runtime exposes no reader for the block rate, so the only safe
		// restoration is the default: off.
		runtime.SetBlockProfileRate(0)
	})
	registry := health.NewRegistry()

	kinds := resolveProfileSettings(envMap(map[string]string{
		envMutexFraction: "100",
		envBlockRateNS:   "1000000000",
		envHeapProfile:   "1",
	})).apply(registry)

	// Capture order is fixed cheapest first, so writing a large heap profile
	// cannot delay the moment the mutex profile is taken.
	want := []string{"mutex", "block", "heap"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i, kind := range want {
		if kinds[i] != kind {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if got := runtime.SetMutexProfileFraction(-1); got != 100 {
		t.Errorf("mutex fraction = %d, want the configured 100", got)
	}
	entries, _ := registry.Snapshot()
	for _, entry := range entries {
		if entry.Collector != healthProfiles {
			continue
		}
		if entry.Status != health.StatusOK {
			t.Errorf("status = %q, want ok", entry.Status)
		}
		for _, want := range []string{"mutex=100", "block=1000000000", "heap=1"} {
			if !strings.Contains(entry.Message, want) {
				t.Errorf("message = %q, want it to contain %q", entry.Message, want)
			}
		}
	}
}

func TestProfileApplyRecordsHealth(t *testing.T) {
	previous := runtime.SetMutexProfileFraction(0)
	t.Cleanup(func() { runtime.SetMutexProfileFraction(previous) })
	registry := health.NewRegistry()

	kinds := resolveProfileSettings(envMap(map[string]string{envMutexFraction: "0"})).apply(registry)
	if len(kinds) != 0 {
		t.Fatalf("kinds = %v, want nothing captured", kinds)
	}
	if got := runtime.SetMutexProfileFraction(-1); got != 0 {
		t.Fatalf("mutex fraction = %d, want the explicit 0 applied", got)
	}
	entries, _ := registry.Snapshot()
	found := false
	for _, entry := range entries {
		if entry.Collector != healthProfiles {
			continue
		}
		found = true
		if entry.Status != health.StatusDisabled {
			t.Errorf("status = %q, want disabled", entry.Status)
		}
		if !strings.Contains(entry.Message, "mutex=off") {
			t.Errorf("message = %q, want the resolved rates", entry.Message)
		}
	}
	if !found {
		t.Fatalf("health entry %q missing", healthProfiles)
	}
}

func TestProfileApplyReportsInvalidValues(t *testing.T) {
	registry := health.NewRegistry()
	resolveProfileSettings(envMap(map[string]string{envMutexFraction: "many"})).apply(registry)
	entries, partial := registry.Snapshot()
	if !partial {
		t.Error("an unparseable profile rate must mark the snapshot partial")
	}
	for _, entry := range entries {
		if entry.Collector == healthProfiles && !strings.Contains(entry.Message, "many") {
			t.Errorf("message = %q, want the rejected value named", entry.Message)
		}
	}
}

func TestRegisterCollectorsDefaultsOn(t *testing.T) {
	ctrl, err := runctl.New(runctl.Options{DisableWatchdog: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()

	names := registerCollectors(ctrl, envMap(nil))

	for _, want := range []string{sqlrows.Name, dbpool.Name} {
		if !contains(names, want) {
			t.Errorf("names = %v, want %q registered by default", names, want)
		}
	}
	// The host collectors read procfs and sysfs, so they only register where
	// those exist. Skipping cleanly is the contract, not registering.
	if runtime.GOOS != "linux" {
		if contains(names, hoststats.CollectorName) || contains(names, "network") {
			t.Errorf("names = %v, want the procfs collectors skipped on %s", names, runtime.GOOS)
		}
	}
}

func TestRegisterCollectorsHonoursFeatureFlags(t *testing.T) {
	ctrl, err := runctl.New(runctl.Options{DisableWatchdog: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()

	names := registerCollectors(ctrl, envMap(map[string]string{
		hoststats.EnvEnable: "off",
		envNetStats:         "off",
		sqlrows.EnvFlag:     "off",
		envDBPool:           "off",
	}))

	if len(names) != 0 {
		t.Fatalf("names = %v, want no collector registered", names)
	}
	// An unregistered collector must be genuinely absent: registering it now
	// has to succeed, which it could not if the flag had merely disabled it.
	if err := ctrl.RegisterBaseline(runctl.Registration{Name: dbpool.Name}, dbpool.Default); err != nil {
		t.Errorf("disabled collector still occupied its name: %v", err)
	}
}

// panicNameGeneration is a generation collector whose Name panics. Nothing
// else about it matters: the wiring never gets past the introduction.
type panicNameGeneration struct{}

func (panicNameGeneration) Name() string { panic("this collector cannot say its own name") }

func (panicNameGeneration) BeginBoundary(context.Context, string, runctl.Epoch) (runctl.BoundaryResult, error) {
	return runctl.BoundaryResult{}, nil
}

func (panicNameGeneration) Freeze(context.Context, string, runctl.Epoch) (runctl.BoundaryResult, error) {
	return runctl.BoundaryResult{}, nil
}

func (panicNameGeneration) Drain(context.Context, runctl.GenerationHandle) error { return nil }

func (panicNameGeneration) Collect(runctl.GenerationHandle) (any, error) { return nil, nil }

func (panicNameGeneration) Release(runctl.GenerationHandle) {}

// TestRegisterGenerationCollectorsSurvivesAPanickingName is the regression
// test for a collector call that used to run bare.
//
// Registration happens inside the measured application's startup, so a
// collector that panics while introducing itself would take the application's
// process down with it — the exact outcome the safe-call barrier around every
// other collector call exists to prevent. The panicking collector must lose
// its own registration, leave a health note behind, and cost the collectors
// after it nothing.
func TestRegisterGenerationCollectorsSurvivesAPanickingName(t *testing.T) {
	restoreCollectorHealth(t, httpstats.CollectorName)
	ctrl, err := runctl.New(runctl.Options{DisableWatchdog: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()

	gens := isolatedGenerationCollectors()
	gens.http = panicNameGeneration{}

	names := registerGenerationCollectors(ctrl, gens)

	if contains(names, httpstats.CollectorName) {
		t.Errorf("names = %v, want the collector that panicked in Name left unregistered", names)
	}
	for _, want := range []string{sqlstats.SectionName, counters.SectionName} {
		if !contains(names, want) {
			t.Errorf("names = %v, want %q still registered after the earlier collector panicked", names, want)
		}
	}
	if got := healthStatus(t, httpstats.CollectorName); got != health.StatusFailed {
		t.Errorf("status = %q, want failed for a registration that panicked", got)
	}
	if got := healthMessage(t, httpstats.CollectorName); !strings.Contains(got, "panicked in Name") {
		t.Errorf("message = %q, want it to name the panicking call", got)
	}
	// The name is genuinely free afterwards: a collector that never registered
	// must not leave its slot occupied.
	if err := ctrl.RegisterGeneration(runctl.Registration{Name: httpstats.CollectorName}, httpstats.New()); err != nil {
		t.Errorf("the failed registration still occupied its name: %v", err)
	}
}

func TestSafeCollectorNameResolvesAWellBehavedCollector(t *testing.T) {
	name, err := safeCollectorName("fallback", func() string { return "hoststats" })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if name != "hoststats" {
		t.Errorf("name = %q, want the collector's own name", name)
	}
}

func TestSafeCollectorNameReportsAPanicUnderTheFallbackKey(t *testing.T) {
	name, err := safeCollectorName("network", func() string { panic("boom") })
	if err == nil {
		t.Fatal("a panicking Name must be reported as a failed registration")
	}
	if name != "network" {
		t.Errorf("name = %q, want the fallback so the failure has a health key", name)
	}
	for _, want := range []string{"network", "panicked in Name", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
}

// unprintablePanic panics again while being rendered. A barrier that cannot
// survive that is no barrier at all.
type unprintablePanic struct{}

func (unprintablePanic) String() string { panic("rendering this value panics too") }

func TestShortPanicText(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "collapses whitespace", value: "two\n  lines", want: "two lines"},
		{name: "empty value", value: "", want: "unprintable panic value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shortPanicText(tt.value); got != tt.want {
				t.Errorf("shortPanicText(%v) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
	// A value whose own renderer panics must still come back as one readable
	// line. fmt catches that panic itself today, and the barrier's own recover
	// covers the day it does not; either way nothing escapes.
	if got := shortPanicText(unprintablePanic{}); got == "" || strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("shortPanicText(unprintable) = %q, want one non-empty line", got)
	}
	// A long value is truncated: the message ends up in a health view an
	// operator reads, and an unbounded panic string would bury it.
	long := shortPanicText(strings.Repeat("x", panicNameTextMax*2))
	if len([]rune(long)) != panicNameTextMax+3 || !strings.HasSuffix(long, "...") {
		t.Errorf("long panic text = %d runes (%q...), want it truncated to %d plus an ellipsis",
			len([]rune(long)), long[:20], panicNameTextMax)
	}
}

func TestNewMeasurementWhenOffRegistersNothing(t *testing.T) {
	m := newTestMeasurement(t, map[string]string{"ISUTOOLS": "off"})
	if err := m.ctrl.RegisterBaseline(runctl.Registration{Name: dbpool.Name}, dbpool.Default); err != nil {
		t.Errorf("ISUTOOLS=off must register no collector, got %v", err)
	}
}

func TestStartRunRecordsUnserializedInitialize(t *testing.T) {
	collectorHealth.Set(healthInitializeUnserialized, health.StatusOK, "")
	m := newTestMeasurement(t, nil)

	result, err := m.startRun(context.Background(), runctl.StartRunOptions{
		Preempt: true, Reason: reasonInitialize, Trigger: "api",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if result.RunID == "" {
		t.Fatal("startRun returned no run id")
	}
	if got := healthStatus(t, healthInitializeUnserialized); got != health.StatusDegraded {
		t.Errorf("status = %q, want degraded for an unguarded initialize", got)
	}
}

func TestStartRunInsideGuardIsNotFlagged(t *testing.T) {
	collectorHealth.Set(healthInitializeUnserialized, health.StatusOK, "")
	m := newTestMeasurement(t, nil)

	err := SerializeInitialize(context.Background(), func(ctx context.Context) error {
		_, startErr := m.startRun(ctx, runctl.StartRunOptions{
			Preempt: true, Reason: reasonInitialize, Trigger: "api",
		})
		return startErr
	})
	if err != nil {
		t.Fatalf("SerializeInitialize: %v", err)
	}
	if got := healthStatus(t, healthInitializeUnserialized); got != health.StatusOK {
		t.Errorf("status = %q, want a guarded initialize to stay clean", got)
	}
}

func healthStatus(t *testing.T, collector string) health.Status {
	t.Helper()
	entries, _ := collectorHealth.Snapshot()
	for _, entry := range entries {
		if entry.Collector == collector {
			return entry.Status
		}
	}
	return ""
}

func TestSerializeInitializePassesTheGuardMarker(t *testing.T) {
	called := false
	err := SerializeInitialize(context.Background(), func(ctx context.Context) error {
		called = true
		if !runctl.HasInitializeGuard(ctx) {
			t.Error("fn must run with the initialize guard marker on its context")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("SerializeInitialize: %v", err)
	}
	if !called {
		t.Fatal("fn was not called")
	}
}

func TestSerializeInitializePropagatesFailure(t *testing.T) {
	want := errors.New("rebuild failed")
	if err := SerializeInitialize(context.Background(), func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestSerializeInitializeWhenOffStillRuns(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	called := false
	if err := SerializeInitialize(context.Background(), func(context.Context) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("SerializeInitialize: %v", err)
	}
	if !called {
		t.Fatal("initialize must still run when measurement is off")
	}
}

func TestResetNowWhenOffIsANoOp(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	result, err := ResetNow(context.Background())
	if err != nil {
		t.Fatalf("ResetNow: %v", err)
	}
	if result.RunID != "" || result.Validity == ValidityInvalid {
		t.Fatalf("result = %+v, want the zero value when measurement is off", result)
	}
}

func TestResetNowWithNonceReplaysTheSameRun(t *testing.T) {
	m := newTestMeasurement(t, nil)
	options := runctl.StartRunOptions{Nonce: "nonce-1", Preempt: true, Reason: "api", Trigger: "api"}

	first, err := m.startRun(context.Background(), options)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := m.startRun(context.Background(), options)
	if err != nil {
		t.Fatalf("replayed start: %v", err)
	}
	if first.RunID != second.RunID {
		t.Errorf("run ids %q and %q differ; the nonce must make the start idempotent", first.RunID, second.RunID)
	}
}

func TestLatestSectionsIsEmptyWhileTheRunIsInFlight(t *testing.T) {
	m := newTestMeasurement(t, nil)
	if got := m.latestSections(); got != nil {
		t.Fatalf("sections = %v, want none before any run started", got)
	}
	if _, err := m.startRun(context.Background(), runctl.StartRunOptions{Preempt: true, Reason: "api"}); err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if got := m.latestSections(); got != nil {
		t.Fatalf("sections = %v, want none until the run has produced a snapshot", got)
	}
}

func TestAbortResetRunClearsCurrentRunAndAllowsTheNext(t *testing.T) {
	m := newTestMeasurement(t, nil)
	started, err := m.startResetRun(context.Background())
	if err != nil {
		t.Fatalf("startResetRun: %v", err)
	}
	aborted, err := m.abortResetRun(context.Background())
	if err != nil {
		t.Fatalf("abortResetRun: %v", err)
	}
	if aborted.RunID != started.RunID || aborted.Reason != runctl.ReasonExplicit {
		t.Fatalf("abort = %+v, started = %+v", aborted, started)
	}
	if got := m.currentRunID(); got != "" {
		t.Fatalf("current run after abort = %q, want empty", got)
	}
	status, ok := m.ctrl.Status(started.RunID)
	if !ok || status.State != runctl.StateAborted || status.Validity != runctl.ValidityInvalid {
		t.Fatalf("aborted status = %+v (found=%v)", status, ok)
	}
	if _, err := m.startResetRun(context.Background()); err != nil {
		t.Fatalf("next run after abort: %v", err)
	}
}

func TestWatchDBPoolRejectsArgumentBugs(t *testing.T) {
	if err := WatchDBPool("whatever", nil); !errors.Is(err, dbpool.ErrNilDB) {
		t.Errorf("nil handle: err = %v, want ErrNilDB", err)
	}
	if err := WatchDBPool("never-registered", &sql.DB{}); !errors.Is(err, sqlstats.ErrUnknownTarget) {
		t.Errorf("unknown target: err = %v, want ErrUnknownTarget", err)
	}
}

func TestWatchDBPoolValidatesEvenWhenDisabled(t *testing.T) {
	// A wiring bug must surface in the configuration the application ships,
	// not only in the one it benchmarks.
	t.Setenv("ISUTOOLS", "off")
	if err := WatchDBPool("never-registered", &sql.DB{}); !errors.Is(err, sqlstats.ErrUnknownTarget) {
		t.Errorf("err = %v, want ErrUnknownTarget even when measurement is off", err)
	}
}

func TestWatchAndUnwatchDBPool(t *testing.T) {
	const id = "watchtest"
	if err := RegisterDBTarget(id, "rootfake", "user:pass@tcp(127.0.0.1:3306)/watchtest"); err != nil {
		t.Fatalf("RegisterDBTarget: %v", err)
	}
	db, err := sql.Open("rootfake", "user:pass@tcp(127.0.0.1:3306)/watchtest")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if err := WatchDBPool(id, db); err != nil {
		t.Fatalf("WatchDBPool: %v", err)
	}
	if !contains(dbpool.Default.Watched(), id) {
		t.Fatalf("watched = %v, want %q", dbpool.Default.Watched(), id)
	}
	if err := WatchDBPool(id, db); !errors.Is(err, dbpool.ErrDuplicatePool) {
		t.Errorf("second watch: err = %v, want ErrDuplicatePool", err)
	}
	if err := UnwatchDBPool(id); err != nil {
		t.Fatalf("UnwatchDBPool: %v", err)
	}
	if contains(dbpool.Default.Watched(), id) {
		t.Errorf("watched = %v, want %q removed", dbpool.Default.Watched(), id)
	}
}

func TestRegisterDBTargetRejectsInvalidID(t *testing.T) {
	if err := RegisterDBTarget("", "rootfake", "dsn"); !errors.Is(err, sqlstats.ErrInvalidTargetID) {
		t.Fatalf("err = %v, want ErrInvalidTargetID", err)
	}
}

func TestRegisterDBInspectorRejectsUnknownTarget(t *testing.T) {
	err := RegisterDBInspector("never-registered", sqlstats.PurposeExplain, "rootfake",
		"explain:pass@tcp(127.0.0.1:3306)/")
	if !errors.Is(err, sqlstats.ErrUnknownTarget) {
		t.Fatalf("err = %v, want ErrUnknownTarget", err)
	}
}

func TestRegisterDBTargetDisabled(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	if err := RegisterDBTarget("", "rootfake", "dsn"); err != nil {
		t.Fatalf("disabled RegisterDBTarget must be a no-op, got %v", err)
	}
	if err := RegisterDBInspector("nope", sqlstats.PurposeStats, "rootfake", "dsn"); err != nil {
		t.Fatalf("disabled RegisterDBInspector must be a no-op, got %v", err)
	}
}

// TestHandlersShareOneRunLifecycle is the point of the singleton: two
// handlers must drive one run lifecycle, so the second reset preempts the run
// the first one opened instead of starting a second, parallel truth.
func TestHandlersShareOneRunLifecycle(t *testing.T) {
	first, second := Handler(), Handler()

	firstID := postReset(t, first)
	secondID := postReset(t, second)

	if firstID == "" || secondID == "" {
		t.Fatalf("run ids = %q and %q, want both handlers to name their run", firstID, secondID)
	}
	if firstID == secondID {
		t.Fatalf("both resets reported run %q; each reset opens a new run", firstID)
	}
	status, ok := defaultMeasurement().ctrl.Status(firstID)
	if !ok {
		t.Fatalf("run %q is unknown to the shared controller", firstID)
	}
	if status.State != runctl.StateAborted && status.State != runctl.StateAborting {
		t.Errorf("state = %q, want the preempted run abandoned", status.State)
	}
}

// TestResetNowOpensARunOnTheSharedController proves the public entry point
// reaches the same Controller the admin handlers use; an initialize that
// opened a run nobody else could see would be worse than none.
func TestResetNowOpensARunOnTheSharedController(t *testing.T) {
	result, err := ResetNow(context.Background())
	if err != nil {
		t.Fatalf("ResetNow: %v", err)
	}
	if result.RunID == "" {
		t.Fatal("ResetNow returned no run id")
	}
	status, ok := defaultMeasurement().ctrl.Status(result.RunID)
	if !ok {
		t.Fatalf("run %q is unknown to the shared controller", result.RunID)
	}
	if status.State != runctl.StateStarted {
		t.Errorf("state = %q, want the run started", status.State)
	}
}

func TestResetNowWithNonceWhenOff(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	result, err := ResetNowWithNonce(context.Background(), "nonce-off")
	if err != nil || result.RunID != "" {
		t.Fatalf("ResetNowWithNonce = (%+v, %v), want the zero value and no error", result, err)
	}
}

func TestUnwatchDBPoolWhenDisabled(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	if err := UnwatchDBPool("never-registered"); err != nil {
		t.Fatalf("disabled UnwatchDBPool must be a no-op, got %v", err)
	}
}

func TestUnwatchDBPoolUnknownTarget(t *testing.T) {
	if err := UnwatchDBPool("never-watched"); err == nil {
		t.Fatal("unwatching a pool that was never watched must report it")
	}
}

func TestPprofDuration(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"-3", 0},
		{"not a number", 0},
		{"30", 30 * time.Second},
	}
	for _, tt := range tests {
		got := pprofDuration(envMap(map[string]string{"ISUTOOLS_PPROF_SECONDS": tt.value}))
		if got != tt.want {
			t.Errorf("pprofDuration(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestInferProxyKindFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/etc/caddy/Caddyfile", "caddy"},
		{"/etc/caddy/caddy.json", "caddy"},
		{"/etc/envoy/envoy.yaml", "envoy"},
		{"/etc/nginx/nginx.conf", "nginx"},
		{"/etc/httpd/httpd.conf", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := inferProxyKindFromPath(tt.path); got != tt.want {
			t.Errorf("inferProxyKindFromPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestReadProxyConf(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(file, []byte("worker_processes auto;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := string(readProxyConf(file)); !strings.Contains(got, "worker_processes") {
		t.Errorf("file read = %q", got)
	}
	// A directory is concatenated from its *.conf files only, so unrelated
	// files in a conf.d directory cannot be mistaken for configuration.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not config"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := string(readProxyConf(dir))
	if !strings.Contains(got, "worker_processes") || strings.Contains(got, "not config") {
		t.Errorf("directory read = %q", got)
	}
	if readProxyConf(filepath.Join(dir, "missing.conf")) != nil {
		t.Error("a missing path must read as no configuration")
	}
}

func TestHTTPMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	wrapped := HTTP(next)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/instrumented", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want the application's response untouched", rec.Code)
	}
}

func TestHTTPMiddlewareDisabled(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if got := HTTP(next); fmt.Sprintf("%p", got) != fmt.Sprintf("%p", next) {
		t.Error("ISUTOOLS=off must return the handler unwrapped so the request path is untouched")
	}
}

func TestCountersRecordAndDisable(t *testing.T) {
	Count("isutools-test-counter")
	AddCount("isutools-test-counter", 2)
	if got := counterValue(t, "isutools-test-counter"); got != 3 {
		t.Errorf("counter = %d, want 3", got)
	}
	t.Setenv("ISUTOOLS", "off")
	AddCount("isutools-test-counter", 10)
	if got := counterValue(t, "isutools-test-counter"); got != 3 {
		t.Errorf("counter = %d, want counting to stop when measurement is off", got)
	}
}

func counterValue(t *testing.T, name string) int64 {
	t.Helper()
	for _, entry := range counters.Default.Snapshot() {
		if entry.Name == name {
			return entry.Count
		}
	}
	return 0
}

func postReset(t *testing.T, handler http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /reset = %d, want 204", rec.Code)
	}
	return rec.Header().Get("X-Isutools-Run-Id")
}

// TestRunEndToEnd_SectionsReachSnapshot drives the documented
// reset → traffic → save loop through the real wiring and insists that what
// the collectors measured actually reaches a snapshot.
//
// It is the test a run lifecycle with no terminator fails: every boundary can
// be perfect and every collector correct, and the whole feature is still dead
// while nothing ever closes the run.
func TestRunEndToEnd_SectionsReachSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ISUTOOLS_DATA_DIR", dir)
	handler := Handler()

	runID := postReset(t, handler)
	if runID == "" {
		t.Fatal("POST /reset opened no run")
	}

	// Real traffic through the real middleware, plus a real SQL observation on
	// the store the proxied driver reports to.
	app := HTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	for i := 0; i < 3; i++ {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/e2e/items/7", nil))
	}
	sqlstats.Default.Observe("SELECT * FROM e2e_items WHERE id = ?", 5*time.Millisecond)
	AddCount("e2e-cache-hit", 2)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save?score=4242", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /save = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Isutools-Run-Id"); got != runID {
		t.Fatalf("save closed run %q, want the run the reset opened (%q)", got, runID)
	}

	ctrl := defaultMeasurement().ctrl
	status, ok := ctrl.Status(runID)
	if !ok || status.State != runctl.StateAcknowledged {
		t.Fatalf("run %q: state=%q known=%v, want the save to finish and acknowledge it",
			runID, status.State, ok)
	}
	snapshot, err := ctrl.SnapshotOf(runID)
	if err != nil {
		t.Fatalf("SnapshotOf(%s): %v", runID, err)
	}
	for _, want := range []string{httpstats.CollectorName, sqlstats.SectionName, counters.SectionName} {
		if _, present := snapshot.Sections[want]; !present {
			t.Errorf("run snapshot has no %q section; it holds %v", want, sectionNames(snapshot.Sections))
		}
	}

	payload := readSavedSnapshot(t, dir, rec)
	if payload.Meta.Score != "4242" {
		t.Errorf("meta.score = %q, want the score the save carried", payload.Meta.Score)
	}
	requests := int64(0)
	for _, entry := range payload.HTTP {
		if strings.HasPrefix(entry.Path, "/e2e/items") {
			requests += entry.Count
		}
	}
	if requests != 3 {
		t.Errorf("http = %+v, want the 3 requests the middleware measured", payload.HTTP)
	}
	if !hasSQLKey(payload.SQL, "e2e_items") {
		t.Errorf("sql = %+v, want the statement observed during the run", payload.SQL)
	}
	if got := counterCount(payload.Counters, "e2e-cache-hit"); got != 2 {
		t.Errorf("counter e2e-cache-hit = %d, want the 2 counted during the run", got)
	}

	// The baseline collectors are platform gated. Where their prerequisites
	// exist the section has to be there; where they do not, the snapshot must
	// still say why it is missing, because a hole with no explanation is
	// indistinguishable from a collector that silently broke.
	for _, gated := range []struct {
		collector string
		section   json.RawMessage
	}{
		{hoststats.CollectorName, payload.HostStats},
		{netstats.Default.Name(), payload.Network},
	} {
		if runtime.GOOS == "linux" {
			if len(gated.section) == 0 {
				t.Errorf("%s section is missing on linux, where its sources exist", gated.collector)
			}
			continue
		}
		if len(gated.section) != 0 {
			t.Errorf("%s section = %s, want none on %s", gated.collector, gated.section, runtime.GOOS)
		}
		entry, found := healthEntry(payload.Meta.Health, gated.collector)
		if !found || entry.Status == health.StatusOK || entry.Message == "" {
			t.Errorf("%s health = %+v (found=%v), want a skip note explaining the missing section",
				gated.collector, entry, found)
		}
	}
}

// TestResetNowOpensTheProfilePairThatSaveCloses drives the other documented way
// a run begins — the initialize contract — with runtime profiles enabled, and
// insists that the pair on disk comes out complete.
//
// It is the test whose absence hid a permanently silent feature. The opening
// capture used to live only inside the POST /reset handler, so an application
// that follows the initialize contract published no opening artifact at all;
// every closing capture then found no opening half to be differenced against
// and wrote nothing, for the whole life of the process.
func TestResetNowOpensTheProfilePairThatSaveCloses(t *testing.T) {
	dir := t.TempDir()
	core := newTestMeasurement(t, map[string]string{
		envDataDir:     dir,
		envHeapProfile: "1",
	})
	if !contains(core.profiles, "heap") {
		t.Fatalf("profiles = %v, want the heap capture the environment asked for", core.profiles)
	}
	// Pin the generation the artifacts are filed under so their names can be
	// asserted rather than merely counted.
	core.generation = func() int64 { return 7 }
	handler := web.NewHandler(web.Provider{
		Health:          health.NewRegistry(),
		DataDir:         dir,
		RuntimeProfiles: core.profiles,
		FinishRun:       core.finishResetRun,
		CompleteRun:     core.completeResetRun,
		Sections:        core.latestSections,
	})

	// The documented initialize: the whole handler serialized, the boundary
	// taken inside it, and no POST /reset anywhere in the flow.
	var started StartResult
	err := SerializeInitialize(context.Background(), func(ctx context.Context) error {
		var startErr error
		started, startErr = core.resetNow(ctx, runctl.StartRunOptions{
			Preempt: true, Reason: reasonInitialize, Trigger: "api",
		})
		return startErr
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if started.RunID == "" {
		t.Fatal("ResetNow opened no run")
	}
	// The opening half exists before anything is saved: it is taken at the
	// boundary, not reconstructed at the end of the run.
	opens := profileArtifacts(t, dir, "*_heap_open.pprof")
	if len(opens) != 1 {
		t.Fatalf("opening artifacts = %v, want exactly one, published by ResetNow", opens)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /save = %d: %s", rec.Code, rec.Body.String())
	}

	closes := profileArtifacts(t, dir, "*_heap_close.pprof")
	if len(closes) != 1 {
		t.Fatalf("closing artifacts = %v, want the second half of the pair", closes)
	}
	// The two halves differ in the point and in nothing else, which is what
	// makes the pair reassemblable from a directory listing.
	base := strings.TrimSuffix(opens[0], "_open.pprof")
	if want := base + "_close.pprof"; closes[0] != want {
		t.Errorf("closing artifact = %q, want %q", closes[0], want)
	}
	if !strings.Contains(base, "_gen7_") || !strings.Contains(base, started.RunID[:8]) {
		t.Errorf("artifact prefix = %q, want the run's generation and id in the name", base)
	}
	// Both records have to be on disk too: the sidecar is the durable primary
	// record, and an opening capture is written long before any snapshot exists.
	for _, point := range []string{"open", "close"} {
		name := base + "_" + point + ".meta.json"
		capture := readProfileCapture(t, dir, name)
		if capture.RunID != started.RunID || capture.Epoch != uint64(started.Epoch) {
			t.Errorf("%s records run %s/%d, want %s/%d",
				name, capture.RunID, capture.Epoch, started.RunID, started.Epoch)
		}
		if capture.Status != "ok" || capture.File != base+"_"+point+".pprof" {
			t.Errorf("%s = %+v, want an ok record naming its artifact", name, capture)
		}
		if string(capture.Point) != point {
			t.Errorf("%s records point %q, want %q", name, capture.Point, point)
		}
	}
	if leftovers := profileArtifacts(t, dir, "*.tmp"); len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}

	// And the saved report carries the pair, so a run read back months later
	// still knows which two files to difference.
	manifest := readSavedSnapshot(t, dir, rec).Meta.Profiles
	if manifest == nil || len(manifest.Pairs) != 1 {
		t.Fatalf("saved manifest = %+v, want exactly one pair", manifest)
	}
	if manifest.RunID != started.RunID {
		t.Errorf("manifest run = %q, want the run ResetNow opened (%q)", manifest.RunID, started.RunID)
	}
	pair := manifest.Pairs[0]
	if pair.Kind != "heap" || pair.OpenFile != opens[0] || pair.CloseFile != closes[0] {
		t.Errorf("pair = %+v, want the two heap artifacts on disk (%v, %v)", pair, opens, closes)
	}
	if !strings.Contains(pair.DiffCommand, pair.OpenFile) || !strings.Contains(pair.DiffCommand, pair.CloseFile) {
		t.Errorf("diff command = %q, want it to difference both halves", pair.DiffCommand)
	}
}

// TestResetNowExportedEntryPointOpensThePair covers the one line the test above
// cannot: whether an application calling the exported ResetNow reaches the
// capture at all. The mechanism and the wiring are separate failures — a
// perfect capture path that the public API routes around is still a feature
// nobody can use.
func TestResetNowExportedEntryPointOpensThePair(t *testing.T) {
	dir := t.TempDir()
	core := newTestMeasurement(t, map[string]string{
		envDataDir:     dir,
		envHeapProfile: "1",
	})
	core.generation = func() int64 { return 3 }
	useDefaultMeasurement(t, core)

	var started StartResult
	err := SerializeInitialize(context.Background(), func(ctx context.Context) error {
		var startErr error
		started, startErr = ResetNow(ctx)
		return startErr
	})
	if err != nil {
		t.Fatalf("ResetNow: %v", err)
	}
	if started.RunID == "" {
		t.Fatal("ResetNow opened no run")
	}
	opens := profileArtifacts(t, dir, "*_heap_open.pprof")
	if len(opens) != 1 {
		t.Fatalf("opening artifacts = %v, want the pair ResetNow opened", opens)
	}
	if !strings.Contains(opens[0], "_gen3_") || !strings.Contains(opens[0], started.RunID[:8]) {
		t.Errorf("artifact = %q, want the run's generation and id in the name", opens[0])
	}
}

// TestResetNowPublishesNothingWithoutABoundary keeps the capture behind the
// boundary that justifies it. A start that failed measured nothing, and an
// opening artifact from it would be indistinguishable from half of a pair whose
// other half is still coming.
func TestResetNowPublishesNothingWithoutABoundary(t *testing.T) {
	dir := t.TempDir()
	core := newTestMeasurement(t, map[string]string{
		envDataDir:     dir,
		envHeapProfile: "1",
	})
	ctx := context.Background()
	if _, err := core.resetNow(ctx, runctl.StartRunOptions{Reason: "http", Trigger: "reset"}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// The zero options do not preempt, so this start collides with the run in
	// flight and never reaches a boundary.
	if _, err := core.resetNow(ctx, runctl.StartRunOptions{Reason: "http", Trigger: "reset"}); err == nil {
		t.Fatal("a colliding start must fail rather than open a second run")
	}
	// Nor is a boundary that names no run worth an artifact: nothing could ever
	// be paired with it.
	core.captureOpenProfiles(runctl.StartResult{})

	if names := profileArtifacts(t, dir, "*_heap_open.pprof"); len(names) != 1 {
		t.Errorf("opening artifacts = %v, want only the successful start's", names)
	}
}

// useDefaultMeasurement points the process-wide core at a test's own for the
// duration of one test.
//
// The exported ResetNow family reaches the core through the singleton, so this
// is the only way to drive the entry point an application actually calls
// against a private Controller. The singleton is read from API entry points
// only — never from a background goroutine — and tests in this package never
// run in parallel, so the swap is observed by nobody else.
func useDefaultMeasurement(t *testing.T, core *measurement) {
	t.Helper()
	// Fire the sync.Once first: an initialization that ran after the swap would
	// overwrite it.
	previous := defaultMeasurement()
	t.Cleanup(func() { measurementCore = previous })
	measurementCore = core
}

// profileArtifacts lists the data directory's entries matching a pattern, by
// base name and in a stable order.
func profileArtifacts(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, filepath.Base(match))
	}
	sort.Strings(names)
	return names
}

// readProfileCapture decodes one capture record from the data directory.
func readProfileCapture(t *testing.T, dir, name string) web.ProfileCapture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read sidecar %s: %v", name, err)
	}
	var capture web.ProfileCapture
	if err := json.Unmarshal(body, &capture); err != nil {
		t.Fatalf("decode sidecar %s: %v", name, err)
	}
	return capture
}

// TestFinishIsTerminalAndCollectIsNot pins the two halves of the compatibility
// guarantee at the level the endpoints actually run: /finish ends the run,
// /collect never does.
func TestFinishIsTerminalAndCollectIsNot(t *testing.T) {
	handler := Handler()
	runID := postReset(t, handler)
	if runID == "" {
		t.Fatal("POST /reset opened no run")
	}

	collect := httptest.NewRecorder()
	handler.ServeHTTP(collect, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if collect.Code != http.StatusNoContent {
		t.Fatalf("POST /collect = %d, want 204", collect.Code)
	}
	ctrl := defaultMeasurement().ctrl
	if status, ok := ctrl.Status(runID); !ok || status.State != runctl.StateStarted {
		t.Fatalf("after /collect: state=%q known=%v, want the run still started", status.State, ok)
	}

	finish := httptest.NewRecorder()
	handler.ServeHTTP(finish, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if finish.Code != http.StatusAccepted {
		t.Fatalf("POST /finish = %d: %s", finish.Code, finish.Body.String())
	}
	var accepted struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(finish.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("finish response: %v", err)
	}
	if accepted.RunID != runID {
		t.Fatalf("finish closed %q, want %q", accepted.RunID, runID)
	}
	if _, err := ctrl.Await(context.Background(), runID); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if status, _ := ctrl.Status(runID); status.State != runctl.StateFinished {
		t.Fatalf("after /finish: state = %q, want finished", status.State)
	}
	if snapshot, err := ctrl.SnapshotOf(runID); err != nil || snapshot == nil {
		t.Fatalf("SnapshotOf after /finish = (%v, %v), want the immutable snapshot", snapshot, err)
	}
}

// savedSnapshot is the subset of the persisted JSON the end-to-end test reads.
type savedSnapshot struct {
	Meta struct {
		Score    string               `json:"score"`
		Health   []health.Entry       `json:"health"`
		Profiles *web.ProfileManifest `json:"profiles"`
	} `json:"meta"`
	SQL  []agg.Entry `json:"sql"`
	HTTP []struct {
		Path  string `json:"path"`
		Count int64  `json:"count"`
	} `json:"http"`
	Counters  []counters.Entry `json:"counters"`
	HostStats json.RawMessage  `json:"hoststats"`
	Network   json.RawMessage  `json:"network"`
}

// readSavedSnapshot loads the JSON half of the pair POST /save persisted.
func readSavedSnapshot(t *testing.T, dir string, rec *httptest.ResponseRecorder) savedSnapshot {
	t.Helper()
	var saved struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("save response %q: %v", rec.Body.String(), err)
	}
	data, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(saved.File, ".html")+".json"))
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	var payload savedSnapshot
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode persisted snapshot: %v", err)
	}
	return payload
}

func sectionNames(sections map[string]any) []string {
	names := make([]string, 0, len(sections))
	for name := range sections {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func hasSQLKey(entries []agg.Entry, substring string) bool {
	for _, entry := range entries {
		if strings.Contains(entry.Key, substring) {
			return true
		}
	}
	return false
}

func counterCount(entries []counters.Entry, name string) int64 {
	for _, entry := range entries {
		if entry.Name == name {
			return entry.Count
		}
	}
	return 0
}

func healthEntry(entries []health.Entry, collector string) (health.Entry, bool) {
	for _, entry := range entries {
		if entry.Collector == collector {
			return entry, true
		}
	}
	return health.Entry{}, false
}

// restoreCollectorHealth captures the process-wide health entries for the
// named collectors and puts them back when the test ends.
//
// collectorHealth is a package-level registry shared by every test in this
// package, so a test that seeds an entry to build its own scenario would
// otherwise leave that value behind for whichever test runs next. Entries the
// registry did not hold yet are left alone on the way out: the only writer
// that can create them is the defaultMeasurement init, which sets every one of
// them itself the first time it runs.
func restoreCollectorHealth(t *testing.T, collectors ...string) {
	t.Helper()
	entries, _ := collectorHealth.Snapshot()
	saved := make(map[string]health.Entry, len(collectors))
	for _, entry := range entries {
		saved[entry.Collector] = entry
	}
	t.Cleanup(func() {
		for _, name := range collectors {
			if entry, ok := saved[name]; ok {
				collectorHealth.Set(name, entry.Status, entry.Message)
			}
		}
	})
}

// TestForwardRunHealthPublishesCollectorNotes proves the diagnostics a
// finished run produced actually reach the process-wide registry — the
// boundary record that says why a section is missing, and the collector's own
// note that says why a section that is present is incomplete.
func TestForwardRunHealthPublishesCollectorNotes(t *testing.T) {
	// This test seeds and forwards notes into the process-wide registry; put
	// every entry it touches back so a later test still sees the real ones.
	restoreCollectorHealth(t, hoststats.CollectorName, sqlrows.Name, dbpool.Name, netstats.Default.Name())
	m := newTestMeasurement(t, nil)
	const runID = "run-health-forwarding"
	snapshot := &runctl.Snapshot{
		RunID: runID,
		Collectors: []runctl.CollectorBoundary{
			{
				Name: sqlrows.Name, Kind: runctl.KindBaseline, Phase: runctl.PhaseCollect,
				Code: runctl.CodeCollectFailed, Err: "performance_schema is off", Dropped: true,
			},
			{
				Name: dbpool.Name, Kind: runctl.KindBaseline, Phase: runctl.PhaseCollect,
				Code: runctl.CodeDrainTimeout, Err: "the drain budget ran out",
			},
			// A clean boundary must not be reported at all.
			{Name: hoststats.CollectorName, Kind: runctl.KindBaseline, Phase: runctl.PhaseFinishFinal},
		},
		Sections: map[string]any{
			netstats.Default.Name(): &netstats.NetworkStats{
				Health: []netstats.HealthNote{{Key: netstats.HealthCounterRewind, Detail: "eth0"}},
			},
		},
	}
	collectorHealth.Set(hoststats.CollectorName, health.StatusOK, "")
	m.forwardRunHealth(runID, snapshot)

	if got := healthStatus(t, sqlrows.Name); got != health.StatusFailed {
		t.Errorf("sqlrows status = %q, want failed for a dropped section", got)
	}
	if got := healthMessage(t, sqlrows.Name); !strings.Contains(got, "performance_schema is off") {
		t.Errorf("sqlrows message = %q, want the boundary's own reason", got)
	}
	if got := healthStatus(t, dbpool.Name); got != health.StatusDegraded {
		t.Errorf("dbpool status = %q, want degraded for an incomplete section", got)
	}
	if got := healthMessage(t, netstats.Default.Name()); !strings.Contains(got, "eth0") {
		t.Errorf("network message = %q, want the section's own note", got)
	}
	if got := healthStatus(t, hoststats.CollectorName); got != health.StatusOK {
		t.Errorf("hoststats status = %q, want a clean boundary left alone", got)
	}

	// The notes belong to an immutable snapshot, so a dashboard refreshing
	// every second must not keep rewriting them over newer information.
	collectorHealth.Set(sqlrows.Name, health.StatusOK, "")
	m.forwardRunHealth(runID, snapshot)
	if got := healthStatus(t, sqlrows.Name); got != health.StatusOK {
		t.Errorf("sqlrows status = %q, want the second forward of one run to be a no-op", got)
	}
}

func TestDBPoolNotRegisteredIsInformational(t *testing.T) {
	if got := dbpoolNotesStatus([]string{dbpool.HealthNotRegistered + ": WatchDBPool was not called"}); got != health.StatusInfo {
		t.Fatalf("status = %q, want info", got)
	}
	if got := dbpoolNotesStatus([]string{dbpool.HealthRegisteredMidRun + ": db1"}); got != health.StatusDegraded {
		t.Fatalf("mid-run registration status = %q, want degraded", got)
	}
}

func TestForwardRunHealthClearsARecoveredCollector(t *testing.T) {
	restoreCollectorHealth(t, sqlrows.Name)
	m := newTestMeasurement(t, nil)
	m.forwardRunHealth("run-bad", &runctl.Snapshot{
		RunID: "run-bad",
		Collectors: []runctl.CollectorBoundary{{
			Name: sqlrows.Name, Kind: runctl.KindBaseline, Phase: runctl.PhaseCollect,
			Code: runctl.CodeCollectFailed, Err: "temporary failure", Dropped: true,
		}},
		Sections: map[string]any{},
	})
	if got := healthStatus(t, sqlrows.Name); got != health.StatusFailed {
		t.Fatalf("bad run status = %q, want failed", got)
	}

	m.forwardRunHealth("run-good", &runctl.Snapshot{
		RunID: "run-good",
		Collectors: []runctl.CollectorBoundary{{
			Name: sqlrows.Name, Kind: runctl.KindBaseline, Phase: runctl.PhaseCollect,
		}},
		Sections: map[string]any{sqlrows.Name: &sqlrows.Section{Validity: runctl.ValidityValid}},
	})
	if got := healthStatus(t, sqlrows.Name); got != health.StatusOK {
		t.Fatalf("recovered run status = %q, want ok", got)
	}
	if got := healthMessage(t, sqlrows.Name); got != "" {
		t.Fatalf("recovered run message = %q, want empty", got)
	}
}

func healthMessage(t *testing.T, collector string) string {
	t.Helper()
	entries, _ := collectorHealth.Snapshot()
	for _, entry := range entries {
		if entry.Collector == collector {
			return entry.Message
		}
	}
	return ""
}

// accessLogLine builds one LTSV record of the shape the proxy writes.
func accessLogLine(uri string) string {
	return "time:2026-08-05T10:00:00+09:00\tmethod:GET\turi:" + uri +
		"\tstatus:200\treqtime:0.010\tupstime:0.005\tbytes:100\tcache:MISS\tctype:text/html\n"
}

func appendAccessLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	for _, line := range lines {
		if _, err := f.WriteString(line); err != nil {
			_ = f.Close()
			t.Fatalf("append %s: %v", path, err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// accessLogURIs lists the URIs an access-log section aggregated.
func accessLogURIs(snap accesslog.Snapshot) []string {
	out := make([]string, 0, len(snap.Entries))
	for _, entry := range snap.Entries {
		out = append(out, entry.URI)
	}
	sort.Strings(out)
	return out
}

// TestRunEndToEnd_AccessLogSectionStopsAtTheFreezePoint is the access-log half
// of the end-to-end coverage: real log collector, real generation adapter, real
// run Controller, real transport, and a run whose section has to contain
// exactly the lines the run measured.
//
// It drives its own Controller rather than the process-wide Handler() because
// the access log's generation adapter is registered at most once per process
// (see watchAccessLogGeneration): whichever test in this package builds a
// handler with an access log first would own that registration, and under
// -shuffle=on that is not this one. The components and the wiring are the ones
// Handler() builds.
//
// The post-boundary line is written after POST /finish has answered, so it is
// beyond the freeze point no matter when the background drain runs — the
// assertion does not depend on winning a race.
func TestRunEndToEnd_AccessLogSectionStopsAtTheFreezePoint(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	appendAccessLog(t, logPath)

	core := newTestMeasurement(t, nil)
	collector := accesslog.New(logPath)
	t.Cleanup(collector.Close)
	if !core.watchAccessLogGeneration(collector) {
		t.Fatal("the access log's generation collector was not registered")
	}
	handler := web.NewHandler(web.Provider{
		Health:                     health.NewRegistry(),
		AccessLog:                  collector,
		AccessLogGenerationManaged: true,
		DataDir:                    dir,
		StartRun:                   core.startResetRun,
		FinishRun:                  core.finishResetRun,
		CompleteRun:                core.completeResetRun,
		Sections:                   core.latestSections,
	})

	runID := postReset(t, handler)
	if runID == "" {
		t.Fatal("POST /reset opened no run")
	}
	appendAccessLog(t, logPath,
		accessLogLine("/e2e/items"), accessLogLine("/e2e/items"), accessLogLine("/e2e/login"))

	finish := httptest.NewRecorder()
	handler.ServeHTTP(finish, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if finish.Code != http.StatusAccepted {
		t.Fatalf("POST /finish = %d: %s", finish.Code, finish.Body.String())
	}
	// Traffic the benchmarker produced after measurement stopped. It is past
	// the freeze point, so no drain and no dashboard read may pull it in.
	appendAccessLog(t, logPath, accessLogLine("/after-the-boundary"))
	// A dashboard refresh aimed straight at the window /finish opens: the
	// boundary is fixed and the drain has not necessarily landed yet.
	for _, path := range []string{"/", "/json"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s during the drain window = %d", path, rec.Code)
		}
	}

	if _, err := core.ctrl.Await(context.Background(), runID); err != nil {
		t.Fatalf("Await: %v", err)
	}
	snapshot, err := core.ctrl.SnapshotOf(runID)
	if err != nil {
		t.Fatalf("SnapshotOf(%s): %v", runID, err)
	}
	section, ok := snapshot.Sections[accesslog.SectionName].(accesslog.Snapshot)
	if !ok {
		t.Fatalf("run snapshot has no %q section; it holds %v",
			accesslog.SectionName, sectionNames(snapshot.Sections))
	}
	if section.Lines != 3 {
		t.Errorf("run section counted %d lines, want the 3 logged inside the run", section.Lines)
	}
	want := []string{"/e2e/items", "/e2e/login"}
	if got := accessLogURIs(section); !slices.Equal(got, want) {
		t.Errorf("run section = %v, want exactly %v", got, want)
	}

	// The section has to reach the report, not just the coordinator.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /json = %d", rec.Code)
	}
	var payload struct {
		AccessLog *accesslog.Snapshot `json:"accesslog"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /json: %v", err)
	}
	if payload.AccessLog == nil {
		t.Fatal("the report carries no access-log section")
	}
	if got := accessLogURIs(*payload.AccessLog); !slices.Equal(got, want) {
		t.Fatalf("reported access log = %v, want exactly the run's traffic %v", got, want)
	}
}

// TestExplainEnabledAgreesWithQueryplan pins the wiring's flag reader to the
// capture package's own. This file reads an injected environment so the
// wiring can be tested without process-wide state, and two readers of one
// variable that disagree would turn the feature on in one place and off in
// the other.
func TestExplainEnabledAgreesWithQueryplan(t *testing.T) {
	for _, value := range []string{
		"", "1", "0", "on", "off", "true", "false", "yes", "no",
		"enabled", "disabled", "ON", " 1 ", "maybe",
	} {
		t.Run("value="+value, func(t *testing.T) {
			t.Setenv(queryplan.EnvFlag, value)
			if got, want := explainEnabled(os.Getenv), queryplan.Enabled(); got != want {
				t.Errorf("explainEnabled(%q) = %v, want queryplan.Enabled() = %v", value, got, want)
			}
		})
	}
}

func TestExplainHookIsInstalledOnlyWhenTheFlagIsOn(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "off by default", env: map[string]string{}},
		{name: "explicitly off", env: map[string]string{queryplan.EnvFlag: "0"}},
		{name: "on", env: map[string]string{queryplan.EnvFlag: "1"}, want: true},
		{
			name: "measurement disabled wins",
			env:  map[string]string{queryplan.EnvFlag: "1", "ISUTOOLS": "off"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestMeasurement(t, tt.env)
			if (m.explain != nil) != tt.want {
				t.Errorf("explain capture = %v, want installed=%v", m.explain, tt.want)
			}
		})
	}
}

func TestComposeEnrichRunsCallerAndQueryPlanHooks(t *testing.T) {
	wantFirst := errors.New("caller enrichment failed")
	wantSecond := errors.New("query-plan enrichment failed")
	var calls []string
	hook := composeEnrich(
		func(context.Context, *runctl.Snapshot) error {
			calls = append(calls, "caller")
			return wantFirst
		},
		func(context.Context, *runctl.Snapshot) error {
			calls = append(calls, "queryplan")
			return wantSecond
		},
	)
	err := hook(context.Background(), &runctl.Snapshot{})
	if strings.Join(calls, ",") != "caller,queryplan" {
		t.Fatalf("calls=%v, want both hooks in caller-first order", calls)
	}
	if !errors.Is(err, wantFirst) || !errors.Is(err, wantSecond) {
		t.Fatalf("error=%v, want both failures preserved", err)
	}
	if composeEnrich(nil, nil) != nil {
		t.Fatal("two nil hooks must remain nil")
	}
}

// TestExplainCredentialShortcut covers the ISUTOOLS_EXPLAIN_DSN path, which is
// only defined for a single-target process: with two databases registered
// there is nothing that says which one the DSN reaches.
func TestExplainCredentialShortcut(t *testing.T) {
	const secretDSN = "explainer:hunter2@tcp(127.0.0.1:3306)/"
	type call struct {
		target  string
		purpose sqlstats.Purpose
		driver  string
		dsn     string
	}
	tests := []struct {
		name        string
		env         map[string]string
		targets     []sqlstats.TargetInfo
		registerErr error
		wantCalls   []call
		wantStatus  health.Status
		wantMessage string
	}{
		{
			name:    "no dsn configured registers nothing",
			env:     map[string]string{},
			targets: []sqlstats.TargetInfo{{ID: "db1"}},
		},
		{
			name:      "single target gets the credential",
			env:       map[string]string{envExplainDSN: secretDSN},
			targets:   []sqlstats.TargetInfo{{ID: "db1"}},
			wantCalls: []call{{target: "db1", purpose: sqlstats.PurposeExplain, driver: "mysql", dsn: secretDSN}},
			// The note names the target, never the DSN.
			wantStatus: health.StatusOK, wantMessage: `target "db1"`,
		},
		{
			name: "an explicit driver is honoured",
			env: map[string]string{
				envExplainDSN: secretDSN, envExplainDriver: "pgx",
			},
			targets:    []sqlstats.TargetInfo{{ID: "db1"}},
			wantCalls:  []call{{target: "db1", purpose: sqlstats.PurposeExplain, driver: "pgx", dsn: secretDSN}},
			wantStatus: health.StatusOK,
		},
		{
			name:        "two targets refuse the shortcut",
			env:         map[string]string{envExplainDSN: secretDSN},
			targets:     []sqlstats.TargetInfo{{ID: "db1"}, {ID: "db2"}},
			wantStatus:  health.StatusDegraded,
			wantMessage: "RegisterDBInspector",
		},
		{
			name:        "no target yet refuses it too",
			env:         map[string]string{envExplainDSN: secretDSN},
			wantStatus:  health.StatusDegraded,
			wantMessage: envExplainDSN,
		},
		{
			name:        "an already registered credential is the steady state",
			env:         map[string]string{envExplainDSN: secretDSN},
			targets:     []sqlstats.TargetInfo{{ID: "db1"}},
			registerErr: sqlstats.ErrDuplicatePurpose,
			wantCalls:   []call{{target: "db1", purpose: sqlstats.PurposeExplain, driver: "mysql", dsn: secretDSN}},
			wantStatus:  health.StatusOK,
		},
		{
			name:        "a registration failure is reported from a fixed table",
			env:         map[string]string{envExplainDSN: secretDSN},
			targets:     []sqlstats.TargetInfo{{ID: "db1"}},
			registerErr: sqlstats.ErrUnknownDriver,
			wantCalls:   []call{{target: "db1", purpose: sqlstats.PurposeExplain, driver: "mysql", dsn: secretDSN}},
			wantStatus:  health.StatusDegraded,
			wantMessage: envExplainDriver,
		},
		{
			name:        "an unparseable DSN is refused, not opened",
			env:         map[string]string{envExplainDSN: secretDSN},
			targets:     []sqlstats.TargetInfo{{ID: "db1"}},
			registerErr: sqlstats.ErrUnparsedDSN,
			wantCalls:   []call{{target: "db1", purpose: sqlstats.PurposeExplain, driver: "mysql", dsn: secretDSN}},
			wantStatus:  health.StatusDegraded,
			wantMessage: "接続衛生",
		},
		{
			name:        "a target that vanished between the read and the write",
			env:         map[string]string{envExplainDSN: secretDSN},
			targets:     []sqlstats.TargetInfo{{ID: "db1"}},
			registerErr: sqlstats.ErrUnknownTarget,
			wantCalls:   []call{{target: "db1", purpose: sqlstats.PurposeExplain, driver: "mysql", dsn: secretDSN}},
			wantStatus:  health.StatusDegraded,
			wantMessage: "target",
		},
		{
			name:        "an unclassified failure still says something",
			env:         map[string]string{envExplainDSN: secretDSN},
			targets:     []sqlstats.TargetInfo{{ID: "db1"}},
			registerErr: errors.New("driver exploded: dsn=" + secretDSN),
			wantCalls:   []call{{target: "db1", purpose: sqlstats.PurposeExplain, driver: "mysql", dsn: secretDSN}},
			wantStatus:  health.StatusDegraded,
			wantMessage: "登録に失敗しました",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := health.NewRegistry()
			var calls []call
			capture := &explainCapture{
				getenv:  envMap(tt.env),
				health:  registry,
				targets: func() []sqlstats.TargetInfo { return tt.targets },
				register: func(targetID string, purpose sqlstats.Purpose, driverName, dsn string) error {
					calls = append(calls, call{targetID, purpose, driverName, dsn})
					return tt.registerErr
				},
			}
			capture.registerCredential()

			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("registrations = %+v, want %+v", calls, tt.wantCalls)
			}
			entries, _ := registry.Snapshot()
			entry, found := healthEntry(entries, healthExplainCredential)
			if tt.wantStatus == "" {
				if found {
					t.Errorf("health = %+v, want no note when the shortcut is unused", entry)
				}
				return
			}
			if !found || entry.Status != tt.wantStatus {
				t.Fatalf("health = %+v (found=%v), want status %q", entry, found, tt.wantStatus)
			}
			if tt.wantMessage != "" && !strings.Contains(entry.Message, tt.wantMessage) {
				t.Errorf("health message = %q, want it to mention %q", entry.Message, tt.wantMessage)
			}
			// A DSN carries a password and this string is published in a
			// snapshot, so no part of it may survive into the note.
			if strings.Contains(entry.Message, "hunter2") || strings.Contains(entry.Message, secretDSN) {
				t.Errorf("health message leaked the DSN: %q", entry.Message)
			}
		})
	}
}

// fakeSQLRowsCollector publishes a fixed interval under sqlrows' own name, so
// the enrich hook has the ranking and the database clock it needs without a
// database behind it.
type fakeSQLRowsCollector struct{ section *sqlrows.Section }

func (fakeSQLRowsCollector) Name() string { return sqlrows.Name }

func (c fakeSQLRowsCollector) CaptureBaseline(_ context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.sample(runID, ep, runctl.PhaseStartBaseline), nil
}

func (c fakeSQLRowsCollector) CaptureFinal(_ context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.sample(runID, ep, runctl.PhaseFinishFinal), nil
}

func (c fakeSQLRowsCollector) sample(runID string, ep runctl.Epoch, phase runctl.Phase) runctl.SampleResult {
	at := time.Now()
	return runctl.SampleResult{
		Handle:    runctl.NewBaselineHandle(runID, ep, sqlrows.Name, phase, at, 0),
		At:        at,
		Committed: true,
	}
}

func (c fakeSQLRowsCollector) Collect(runctl.BaselineHandle, runctl.BaselineHandle) (any, error) {
	return c.section, nil
}

func (fakeSQLRowsCollector) Release(runctl.BaselineHandle) {}

// explainableInterval is an interval the capture will act on: a usable target
// with a SELECT digest and a database clock wide enough to judge a sample by.
func explainableInterval() *sqlrows.Section {
	now := time.Now().UTC()
	return &sqlrows.Section{
		Validity: runctl.ValidityValid,
		Limit:    sqlrows.DigestTextFetchLimit,
		Targets: []sqlrows.TargetSection{{
			TargetID: "db1", Schema: "isuconp", Usable: true, Shown: 1, Total: 1,
			Digests: []sqlrows.DigestStat{{
				Digest: "d1", Query: "SELECT plan_marker FROM posts WHERE id = ?",
				Kind: sqlrows.KindSelect, Count: 42, TimerWaitPicos: 5_000_000_000,
			}},
			DBClock: sqlrows.DBClock{
				BaselineBefore: now.Add(-90 * time.Second),
				BaselineAfter:  now.Add(-89 * time.Second),
				FinalBefore:    now.Add(-2 * time.Second),
				FinalAfter:     now.Add(-time.Second),
				Monotonic:      true,
			},
		}},
	}
}

// TestExplainRunsOnlyInTheEnrichPhase is the regression this feature's whole
// design rests on.
//
// EXPLAIN issues statements against the database the benchmark is hammering.
// It must therefore happen exactly once per run, in the finishing worker, and
// never on a dashboard GET — which a bench operator refreshes while the
// benchmark runs — nor on POST /collect, which is a non-terminal flush with no
// interval to rank digests by.
func TestExplainRunsOnlyInTheEnrichPhase(t *testing.T) {
	restoreCollectorHealth(t, "runctl", sqlrows.Name, queryplan.Name)
	core := newTestMeasurement(t, map[string]string{
		queryplan.EnvFlag: "1",
		// sqlrows' own collector is replaced by the fake below, so the flag
		// keeps the real one from claiming the name first.
		sqlrows.EnvFlag: "off",
	})
	if core.explain == nil {
		t.Fatal("ISUTOOLS_EXPLAIN=1 installed no capture")
	}
	if err := core.ctrl.RegisterBaseline(
		runctl.Registration{Name: sqlrows.Name},
		fakeSQLRowsCollector{section: explainableInterval()},
	); err != nil {
		t.Fatalf("registering the interval collector: %v", err)
	}

	var inspects atomic.Int64
	core.explain.inspect = func(context.Context, string, sqlstats.Purpose, func(context.Context, sqlstats.Querier) error) error {
		inspects.Add(1)
		// No credential, which is the honest answer for a process with no
		// database: the capture records the reason and moves on.
		return sqlstats.ErrPurposeNotRegistered
	}

	handler := web.NewHandler(web.Provider{
		SQL:         agg.NewTable(agg.DefaultMaxKeys),
		StartRun:    core.startResetRun,
		FinishRun:   core.finishResetRun,
		CompleteRun: core.completeResetRun,
		Sections:    core.latestSections,
	})

	runID := postReset(t, handler)
	if runID == "" {
		t.Fatal("POST /reset opened no run")
	}
	// The dashboard, hit the way a bench operator hits it: repeatedly, while
	// the run is in flight.
	for i := 0; i < 10; i++ {
		for _, path := range []string{"/", "/live", "/json", "/snapshot.html"} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d", path, rec.Code)
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("POST /collect = %d", rec.Code)
		}
	}
	if got := inspects.Load(); got != 0 {
		t.Fatalf("%d EXPLAIN sessions were opened by GETs and flushes, want none", got)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /finish = %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := core.ctrl.Await(context.Background(), runID); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if got := inspects.Load(); got != 1 {
		t.Fatalf("enrich opened %d EXPLAIN sessions, want exactly one per target per run", got)
	}

	snapshot, err := core.ctrl.SnapshotOf(runID)
	if err != nil {
		t.Fatalf("SnapshotOf: %v", err)
	}
	section, ok := snapshot.Sections[queryplan.Name].(*queryplan.Section)
	if !ok || section == nil {
		t.Fatalf("run snapshot has no query-plan section; it holds %v", sectionNames(snapshot.Sections))
	}
	if len(section.Targets) != 1 || section.Targets[0].Code != queryplan.CodePurposeUnregistered {
		t.Fatalf("section = %+v, want the target skipped for a missing credential", section.Targets)
	}
	if len(section.Health) == 0 {
		t.Error("a skipped target must leave a health note")
	}

	// The report renders the cached result, and renders it without going near
	// the database again.
	for i := 0; i < 5; i++ {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /json after the run = %d", rec.Code)
		}
	}
	if !strings.Contains(rec.Body.String(), `"queryplan"`) {
		t.Error("the captured section must reach the report")
	}
	if got := inspects.Load(); got != 1 {
		t.Errorf("rendering the report opened %d more EXPLAIN sessions, want none", got-1)
	}
}

// TestExplainEnrichWithoutAnIntervalPublishesNothing keeps the hook silent on
// a run sqlrows produced nothing for: without the interval there is no digest
// ranking and no database clock, and re-reading either here would produce a
// second opinion that could disagree with the numbers next to the plans.
func TestExplainEnrichWithoutAnIntervalPublishesNothing(t *testing.T) {
	inspects := 0
	capture := &explainCapture{
		getenv:  envMap(map[string]string{}),
		targets: func() []sqlstats.TargetInfo { return nil },
		register: func(string, sqlstats.Purpose, string, string) error {
			t.Error("no credential may be registered without a DSN")
			return nil
		},
		inspect: func(context.Context, string, sqlstats.Purpose, func(context.Context, sqlstats.Querier) error) error {
			inspects++
			return nil
		},
	}
	for _, snapshot := range []*runctl.Snapshot{
		nil,
		{},
		{Sections: map[string]any{}},
		{Sections: map[string]any{sqlrows.Name: "not a section"}},
		{Sections: map[string]any{sqlrows.Name: (*sqlrows.Section)(nil)}},
	} {
		if err := capture.enrich(context.Background(), snapshot); err != nil {
			t.Errorf("enrich(%+v) = %v, want enrichment never to fail a run", snapshot, err)
		}
		if snapshot != nil && snapshot.Sections != nil {
			if _, published := snapshot.Sections[queryplan.Name]; published {
				t.Errorf("enrich published a section for %+v", snapshot)
			}
		}
	}
	if inspects != 0 {
		t.Errorf("%d EXPLAIN sessions were opened without an interval, want none", inspects)
	}
}
