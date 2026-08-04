package isutools

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/advisor"
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
