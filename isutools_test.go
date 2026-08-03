package isutools

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Bind the admin server to an ephemeral port for the whole test binary
	// so tests never collide with a real 19191 listener.
	os.Setenv("ISUTOOLS_ADDR", "127.0.0.1:0")
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
	defer resp.Body.Close()
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
