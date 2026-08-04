package advisor

import (
	"context"
	"strings"
	"testing"
)

func TestECHSkipWithoutProxyConf(t *testing.T) {
	m := byID(Collect(context.Background(), Options{}))
	for _, id := range []string{"ech-config", "ech-key-rotation", "ech-logging"} {
		if m[id].Status != StatusSkip {
			t.Errorf("%s = %q, want skip without proxy conf", id, m[id].Status)
		}
	}
}

func TestECHSkipsForNonNginxProxy(t *testing.T) {
	m := byID(Collect(context.Background(), Options{Protocol: ProtocolOptions{
		ProxyKind:   "caddy",
		ProxyConfig: []byte("example.com {\n\treverse_proxy app:8080\n}"),
	}}))
	if got := m["ech-config"].Status; got != StatusSkip {
		t.Errorf("caddy conf = %q, want skip (Caddy manages ECH itself)", got)
	}
}

func TestECHNotConfigured(t *testing.T) {
	conf := "http { server { listen 443 ssl; } }"
	m := byID(Collect(context.Background(), Options{Protocol: ProtocolOptions{
		ProxyKind: "nginx", ProxyConfig: []byte(conf),
	}}))
	c := m["ech-config"]
	if c.Status != StatusInfo {
		t.Errorf("nginx without ssl_ech_file = %q, want info (ECH is optional)", c.Status)
	}
	if !strings.Contains(c.Recommendation, "ssl_ech_file") {
		t.Errorf("recommendation = %q", c.Recommendation)
	}
	if m["ech-key-rotation"].Status != StatusSkip {
		t.Errorf("rotation check must skip without ECH, got %q", m["ech-key-rotation"].Status)
	}
	if m["ech-logging"].Status != StatusSkip {
		t.Errorf("logging check must skip without ECH, got %q", m["ech-logging"].Status)
	}
}

func TestECHSingleKeyRecommendsRotationWindow(t *testing.T) {
	conf := `
http {
  ssl_ech_file /usr/local/nginx/conf/ech/current.pem.ech;
  log_format main '$remote_addr ech:$ssl_ech_status:$ssl_ech_outer_server_name';
  server { listen 443 ssl; }
}`
	m := byID(Collect(context.Background(), Options{Protocol: ProtocolOptions{
		ProxyKind: "nginx", ProxyConfig: []byte(conf),
	}}))
	if got := m["ech-config"].Status; got != StatusOK {
		t.Errorf("ssl_ech_file present = %q, want ok", got)
	}
	c := m["ech-key-rotation"]
	if c.Status != StatusInfo {
		t.Errorf("single key file = %q, want info (no old-key window)", c.Status)
	}
	if !strings.Contains(c.Recommendation, "retry_configs") {
		t.Errorf("recommendation = %q", c.Recommendation)
	}
	if got := m["ech-logging"].Status; got != StatusOK {
		t.Errorf("$ssl_ech_status in log_format = %q, want ok", got)
	}
}

func TestECHMultipleKeysWithoutLogging(t *testing.T) {
	conf := `
http {
  ssl_ech_file /usr/local/nginx/conf/ech/current.pem.ech;
  ssl_ech_file /usr/local/nginx/conf/ech/previous.pem.ech;
  server { listen 443 ssl; }
}`
	m := byID(Collect(context.Background(), Options{Protocol: ProtocolOptions{
		ProxyKind: "nginx", ProxyConfig: []byte(conf),
	}}))
	if got := m["ech-key-rotation"].Status; got != StatusOK {
		t.Errorf("current+old key files = %q, want ok", got)
	}
	c := m["ech-logging"]
	if c.Status != StatusInfo {
		t.Errorf("no $ssl_ech_status = %q, want info", c.Status)
	}
	if !strings.Contains(c.Recommendation, "$ssl_ech_status") {
		t.Errorf("recommendation = %q", c.Recommendation)
	}
}
