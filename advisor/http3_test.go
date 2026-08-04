package advisor

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func udpListenerFS(portHex string) fstest.MapFS {
	return fstest.MapFS{
		"proc/net/udp": {Data: []byte("  sl  local_address rem_address   st\n   0: 00000000:" + portHex + " 00000000:0000 07\n")},
	}
}

func TestHTTP3NginxReadiness(t *testing.T) {
	conf := `
server {
  listen 443 ssl;
  listen 443 quic reuseport;
  http2 on;
  ssl_protocols TLSv1.2 TLSv1.3;
  ssl_certificate /etc/ssl/example.crt;
  ssl_certificate_key /etc/ssl/example.key;
  add_header Alt-Svc 'h3=":443"' always;
}`
	checks := checkHTTP3(Options{
		FS: udpListenerFS("01BB"),
		Protocol: ProtocolOptions{
			ProxyKind:       "nginx",
			ProxyConfig:     []byte(conf),
			UDP443Reachable: EvidenceYes,
		},
	})
	m := byID(checks)
	for _, id := range []string{
		"http3-server", "http3-tls", "http3-advertisement",
		"http3-fallback", "http3-udp-listener", "http3-network-path",
	} {
		if got := m[id].Status; got != StatusOK {
			t.Errorf("%s = %q (%s), want ok", id, got, m[id].Detail)
		}
	}
	if got := m["http3-edge"].Status; got != StatusSkip {
		t.Errorf("http3-edge = %q, want skip when no edge is declared", got)
	}
}

func TestHTTP3NginxReportsMissingMigrationPieces(t *testing.T) {
	checks := checkHTTP3(Options{
		FS: udpListenerFS("1F90"),
		Protocol: ProtocolOptions{
			ProxyKind:   "nginx",
			ProxyConfig: []byte("server { listen 443 ssl; ssl_certificate cert.pem; ssl_certificate_key key.pem; }")},
	})
	m := byID(checks)
	for _, id := range []string{"http3-server", "http3-advertisement"} {
		if got := m[id].Status; got != StatusMissing {
			t.Errorf("%s = %q, want missing", id, got)
		}
	}
	if got := m["http3-network-path"].Status; got != StatusSkip {
		t.Errorf("network path without external evidence = %q, want skip", got)
	}
}

func TestHTTP3CaddyDefaultsAndExplicitDisable(t *testing.T) {
	ready := byID(checkHTTP3(Options{Protocol: ProtocolOptions{
		ProxyKind:   "caddy",
		ProxyConfig: []byte("example.com { reverse_proxy 127.0.0.1:8080 }")},
	}))
	for _, id := range []string{"http3-server", "http3-tls", "http3-advertisement", "http3-fallback"} {
		if got := ready[id].Status; got != StatusOK {
			t.Errorf("Caddy default %s = %q (%s), want ok", id, got, ready[id].Detail)
		}
	}

	disabled := byID(checkHTTP3(Options{Protocol: ProtocolOptions{
		ProxyKind:   "caddy",
		ProxyConfig: []byte("{ servers { protocols h1 h2 } }\nexample.com { reverse_proxy 127.0.0.1:8080 }")},
	}))
	if got := disabled["http3-server"].Status; got != StatusMissing {
		t.Errorf("explicitly disabled Caddy h3 = %q, want missing", got)
	}
}

func TestHTTP3CaddyAcceptsManualTLSWhenAutomationIsOff(t *testing.T) {
	checks := byID(checkHTTP3(Options{Protocol: ProtocolOptions{
		ProxyKind: "caddy",
		ProxyConfig: []byte(`{
  auto_https off
}
example.com {
  tls /etc/caddy/cert.pem /etc/caddy/key.pem
  reverse_proxy 127.0.0.1:8080
}`),
	}}))
	if got := checks["http3-tls"].Status; got != StatusOK {
		t.Fatalf("Caddy manual TLS with automation off = %q (%s), want ok", got, checks["http3-tls"].Detail)
	}
}

func TestHTTP3EnvoyRequiresQUICTransportAndAdvertisement(t *testing.T) {
	conf := `
static_resources:
  listeners:
  - name: tcp_listener
    address: { socket_address: { address: 0.0.0.0, port_value: 443 } }
    codec_type: AUTO
  - name: quic_listener
    address: { socket_address: { address: 0.0.0.0, port_value: 443, protocol: UDP } }
    udp_listener_config: { quic_options: {} }
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.transport_sockets.quic.v3.QuicDownstreamTransport
      downstream_tls_context: { common_tls_context: { tls_certificates: [{}] } }
  response_headers_to_add:
  - header:
      key: alt-svc
      value: 'h3=":443"; ma=86400'
`
	m := byID(checkHTTP3(Options{Protocol: ProtocolOptions{ProxyKind: "envoy", ProxyConfig: []byte(conf)}}))
	for _, id := range []string{"http3-server", "http3-tls", "http3-advertisement", "http3-fallback"} {
		if got := m[id].Status; got != StatusOK {
			t.Errorf("Envoy %s = %q (%s), want ok", id, got, m[id].Detail)
		}
	}
}

func TestHTTP3UnknownProxyFailsOpen(t *testing.T) {
	m := byID(checkHTTP3(Options{Protocol: ProtocolOptions{ProxyConfig: []byte("not a recognized config")}}))
	if got := m["http3-server"].Status; got != StatusSkip {
		t.Errorf("unknown proxy = %q, want skip", got)
	}
}

func TestHTTP3ExplicitKindWithoutConfigFailsOpen(t *testing.T) {
	m := byID(checkHTTP3(Options{Protocol: ProtocolOptions{ProxyKind: "caddy"}}))
	if got := m["http3-server"].Status; got != StatusSkip {
		t.Errorf("kind without readable config = %q, want skip", got)
	}
}

func TestHTTP3EdgeAndExternalNetworkEvidence(t *testing.T) {
	m := byID(checkHTTP3(Options{Protocol: ProtocolOptions{
		EdgeName:        "example-edge",
		EdgeHTTP3:       EvidenceNo,
		UDP443Reachable: EvidenceNo,
	}}))
	if got := m["http3-edge"].Status; got != StatusWarn {
		t.Errorf("edge disabled = %q, want warn", got)
	}
	if got := m["http3-network-path"].Status; got != StatusWarn {
		t.Errorf("UDP/443 blocked = %q, want warn", got)
	}
}

func TestHTTP3QUICTransportHealth(t *testing.T) {
	good := byID(checkHTTP3(Options{Protocol: ProtocolOptions{QUIC: &QUICTelemetry{
		PacketsSent: 10000, PacketsRetransmitted: 50,
	}}}))["http3-quic-health"]
	if good.Status != StatusOK || !strings.Contains(good.Detail, "0.50%") {
		t.Errorf("healthy QUIC telemetry = %#v", good)
	}

	bad := byID(checkHTTP3(Options{Protocol: ProtocolOptions{QUIC: &QUICTelemetry{
		PacketsSent: 100, PacketsRetransmitted: 8, UDPDatagramsDropped: 3,
	}}}))["http3-quic-health"]
	if bad.Status != StatusWarn {
		t.Errorf("lossy QUIC telemetry = %#v", bad)
	}

	missing := byID(checkHTTP3(Options{}))["http3-quic-health"]
	if missing.Status != StatusSkip || !strings.Contains(missing.Recommendation, "Envoy") {
		t.Errorf("missing QUIC telemetry = %#v", missing)
	}

	zero := byID(checkHTTP3(Options{Protocol: ProtocolOptions{QUIC: &QUICTelemetry{}}}))["http3-quic-health"]
	if zero.Status != StatusSkip {
		t.Errorf("zero-packet QUIC telemetry = %#v, want skip", zero)
	}
}

func TestWithProtocolTrafficUsesMeasuredProtocolAndFlagsErrors(t *testing.T) {
	checks := WithProtocolTraffic(nil, "proxy access log", []ProtocolSample{
		{Protocol: "HTTP/2.0", Count: 900, Errors: 1, P95: 80 * time.Millisecond},
		{Protocol: "HTTP/3.0", Count: 100, Errors: 10, P95: 120 * time.Millisecond},
	})
	c := byID(checks)["http3-traffic"]
	if c.Status != StatusWarn {
		t.Fatalf("traffic status = %q, want warn for elevated HTTP/3 errors", c.Status)
	}
	for _, want := range []string{"HTTP/3.0=100", "10.00%", "proxy access log"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not contain %q", c.Detail, want)
		}
	}

	noH3 := byID(WithProtocolTraffic(nil, "application middleware", []ProtocolSample{
		{Protocol: "HTTP/2.0", Count: 100, P95: 40 * time.Millisecond},
	}))["http3-traffic"]
	if noH3.Status != StatusInfo || !strings.Contains(noH3.Recommendation, "A/B") {
		t.Errorf("no-H3 traffic = %#v, want informational A/B recommendation", noH3)
	}

	unknown := byID(WithProtocolTraffic(nil, "", nil))["http3-traffic"]
	if unknown.Status != StatusSkip {
		t.Errorf("no traffic evidence = %q, want skip", unknown.Status)
	}
}

func TestProtocolTrafficBehindEdgeIsNotClientEvidence(t *testing.T) {
	checks := WithProtocolTrafficEvidence(nil, "origin proxy access log (edge declared)", false, []ProtocolSample{
		{Protocol: "HTTP/3.0", Count: 100, P95: 10 * time.Millisecond},
	})
	c := byID(checks)["http3-traffic"]
	if c.Status != StatusInfo || !strings.Contains(c.Recommendation, "edge") {
		t.Errorf("origin-side protocol evidence = %#v", c)
	}
}
