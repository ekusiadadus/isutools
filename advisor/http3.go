package advisor

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Evidence is an explicit yes/no/unknown fact for conditions that this
// process cannot safely infer, such as an Internet-to-edge UDP path.
type Evidence string

const (
	EvidenceUnknown Evidence = ""
	EvidenceYes     Evidence = "yes"
	EvidenceNo      Evidence = "no"
)

// ProtocolOptions supplies HTTP/3/QUIC configuration and external evidence.
// ProxyKind accepts nginx, caddy, or envoy. An empty kind is auto-detected
// only when the configuration contains an unambiguous signature.
type ProtocolOptions struct {
	ProxyKind       string
	ProxyConfig     []byte
	UDP443Reachable Evidence
	EdgeName        string
	EdgeHTTP3       Evidence
	QUIC            *QUICTelemetry
	QUICError       string
}

// QUICTelemetry is an interval-aligned proxy transport snapshot. Values are
// normally supplied by Envoy QUIC/UDP stats or an equivalent server metric;
// HTTP middleware cannot observe packet retransmission or kernel UDP drops.
type QUICTelemetry struct {
	PacketsSent          uint64 `json:"packets_sent"`
	PacketsRetransmitted uint64 `json:"packets_retransmitted"`
	UDPDatagramsDropped  uint64 `json:"udp_datagrams_dropped"`
}

// ProtocolSample is one measured client-facing protocol aggregate.
type ProtocolSample struct {
	Protocol string
	Count    int64
	Errors   int64
	P95      time.Duration
}

type proxyReadiness struct {
	kind                  string
	known                 bool
	http3, tls, advertise bool
	fallback              bool
	http3Detail           string
	tlsDetail             string
	advertiseDetail       string
	fallbackDetail        string
}

var (
	nginxQUICListen = regexp.MustCompile(`(?im)\blisten\s+[^;\n]*\bquic\b[^;\n]*;`)
	nginxTLSListen  = regexp.MustCompile(`(?im)\blisten\s+[^;\n]*\b443\b[^;\n]*\bssl\b[^;\n]*;`)
	altSvcH3        = regexp.MustCompile(`(?is)alt-svc.{0,256}?h3\s*=`)
	caddyProtocols  = regexp.MustCompile(`(?im)\bprotocols\s+([^{}\n]+)`)
	caddyTLS        = regexp.MustCompile(`(?im)^\s*tls(?:\s|\{)`)
)

func checkHTTP3(opts Options) []Check {
	r := analyzeProxy(opts.Protocol.ProxyKind, opts.Protocol.ProxyConfig)
	checks := []Check{
		{ID: "http3-server", Title: "HTTP/3: server / QUIC capability"},
		{ID: "http3-tls", Title: "HTTP/3: TLS 1.3 and certificate"},
		{ID: "http3-advertisement", Title: "HTTP/3: Alt-Svc advertisement"},
		{ID: "http3-fallback", Title: "HTTP/3: HTTP/2・HTTP/1.1 fallback"},
		{ID: "http3-udp-listener", Title: "HTTP/3: local UDP/443 listener"},
		{ID: "http3-network-path", Title: "HTTP/3: client path to UDP/443"},
		{ID: "http3-edge", Title: "HTTP/3: load balancer / CDN termination"},
		{ID: "http3-quic-health", Title: "HTTP/3: QUIC retransmission / UDP drops"},
	}

	if !r.known {
		for i := 0; i < 4; i++ {
			checks[i].Status = StatusSkip
			checks[i].Detail = "proxy 設定を特定できない(ISUTOOLS_PROXY_KIND/ISUTOOLS_PROXY_CONF を設定)"
		}
	} else {
		setReadiness(&checks[0], r.http3, r.http3Detail,
			proxyRecommendation(r.kind, "HTTP/3/QUIC listener を有効化"))
		setReadiness(&checks[1], r.tls, r.tlsDetail,
			"TLS 1.3 と有効な証明書/秘密鍵を設定し、実接続で証明書検証する")
		setReadiness(&checks[2], r.advertise, r.advertiseDetail,
			proxyRecommendation(r.kind, "HTTP/3 endpoint を Alt-Svc: h3=... で通知"))
		setReadiness(&checks[3], r.fallback, r.fallbackDetail,
			"同じ公開endpointに TCP/TLS の HTTP/2 または HTTP/1.1 fallback を残す")
	}

	if opts.Protocol.EdgeName != "" && opts.Protocol.EdgeHTTP3 == EvidenceYes {
		checks[4].Status = StatusSkip
		checks[4].Detail = "HTTP/3 は edge で終端; origin の UDP listener は不要"
	} else if listening, err := hasUDPListener(opts.FS, 443); err != nil {
		checks[4].Status = StatusSkip
		checks[4].Detail = "ローカル UDP socket を確認できない: " + err.Error()
	} else if listening {
		checks[4].Status = StatusOK
		checks[4].Detail = "ローカル UDP/443 listener を /proc/net/udp{,6} で確認"
	} else if r.http3 {
		checks[4].Status = StatusWarn
		checks[4].Detail = "設定上はHTTP/3有効だが、ローカル UDP/443 listener が見つからない"
		checks[4].Recommendation = "proxy の起動状態、bind失敗、containerのUDP publishを確認"
	} else {
		checks[4].Status = StatusInfo
		checks[4].Detail = "ローカル UDP/443 listener なし"
	}

	switch opts.Protocol.UDP443Reachable {
	case EvidenceYes:
		checks[5].Status = StatusOK
		checks[5].Detail = "外部から UDP/443 到達確認済み(明示 evidence)"
	case EvidenceNo:
		checks[5].Status = StatusWarn
		checks[5].Detail = "外部から UDP/443 に到達できない(明示 evidence)"
		checks[5].Recommendation = "firewall/security group/NAT/LB/containerで UDP/443 をend-to-endに許可"
	default:
		checks[5].Status = StatusSkip
		checks[5].Detail = "プロセス内から firewall/NAT の外部経路は確定できない"
		checks[5].Recommendation = "HTTP/3対応clientを別hostから接続し、ISUTOOLS_HTTP3_UDP443=reachable|blocked で結果を記録"
	}

	if opts.Protocol.EdgeName == "" {
		checks[6].Status = StatusSkip
		checks[6].Detail = "LB/CDN 未宣言(direct origin と edge termination を自動判別できない)"
	} else {
		checks[6].Detail = "edge=" + opts.Protocol.EdgeName
		switch opts.Protocol.EdgeHTTP3 {
		case EvidenceYes:
			checks[6].Status = StatusOK
			checks[6].Detail += " / HTTP/3 enabled(明示 evidence)"
		case EvidenceNo:
			checks[6].Status = StatusWarn
			checks[6].Detail += " / HTTP/3 disabled(明示 evidence)"
			checks[6].Recommendation = "LB/CDN側でHTTP/3を有効化し、origin protocolとは別に検証"
		default:
			checks[6].Status = StatusInfo
			checks[6].Detail += " / HTTP/3 state unknown"
			checks[6].Recommendation = "edge管理画面と外部client実測でHTTP/3終端を確認"
		}
	}
	checks[7] = quicHealthCheck(opts.Protocol.QUIC, opts.Protocol.QUICError)
	return checks
}

func quicHealthCheck(telemetry *QUICTelemetry, telemetryError string) Check {
	c := Check{ID: "http3-quic-health", Title: "HTTP/3: QUIC retransmission / UDP drops"}
	if telemetryError != "" {
		c.Status = StatusSkip
		c.Detail = "invalid QUIC telemetry: " + telemetryError
		c.Recommendation = "metrics JSONのcounterを非負整数で揃える"
		return c
	}
	if telemetry == nil {
		c.Status = StatusSkip
		c.Detail = "QUIC packet telemetryなし(HTTP middlewareからは観測不可)"
		c.Recommendation = "Envoy QUIC stats等から sent/retransmitted と UDP dropped を同一ベンチ区間で入力"
		return c
	}
	rate := 0.0
	if telemetry.PacketsSent > 0 {
		rate = float64(telemetry.PacketsRetransmitted) * 100 / float64(telemetry.PacketsSent)
	}
	c.Detail = fmt.Sprintf("sent=%d / retransmitted=%d(%.2f%%) / udp_dropped=%d",
		telemetry.PacketsSent, telemetry.PacketsRetransmitted, rate, telemetry.UDPDatagramsDropped)
	switch {
	case telemetry.PacketsRetransmitted > telemetry.PacketsSent:
		c.Status = StatusWarn
		c.Recommendation = "telemetryのcounter範囲とreset時点を揃える"
	case telemetry.PacketsSent == 0 && telemetry.UDPDatagramsDropped == 0:
		c.Status = StatusSkip
		c.Recommendation = "QUIC packetがないため再送率を評価できない。HTTP/3 trafficのある区間を入力"
	case rate >= 2 || telemetry.UDPDatagramsDropped > 0:
		c.Status = StatusWarn
		c.Recommendation = "UDP receive buffer、NIC/GSO、network loss、QUIC error codeを確認"
	default:
		c.Status = StatusOK
	}
	return c
}

func setReadiness(c *Check, ok bool, detail, recommendation string) {
	c.Detail = detail
	if ok {
		c.Status = StatusOK
		return
	}
	c.Status = StatusMissing
	c.Recommendation = recommendation
}

func analyzeProxy(kind string, config []byte) proxyReadiness {
	conf := stripComments(string(config))
	if strings.TrimSpace(conf) == "" {
		return proxyReadiness{}
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = detectProxyKind(conf)
	}
	switch kind {
	case "nginx":
		return analyzeNginxHTTP3(conf)
	case "caddy":
		return analyzeCaddyHTTP3(conf)
	case "envoy":
		return analyzeEnvoyHTTP3(conf)
	default:
		return proxyReadiness{}
	}
}

func detectProxyKind(conf string) string {
	lower := strings.ToLower(conf)
	switch {
	case strings.Contains(lower, "quicdownstreamtransport") || strings.Contains(lower, "static_resources:"):
		return "envoy"
	case strings.Contains(lower, "worker_processes") || strings.Contains(lower, "proxy_pass ") || strings.Contains(lower, "listen 443"):
		return "nginx"
	case strings.Contains(lower, "reverse_proxy ") || strings.Contains(lower, "servers {"):
		return "caddy"
	default:
		return ""
	}
}

func analyzeNginxHTTP3(conf string) proxyReadiness {
	lower := strings.ToLower(conf)
	h3 := nginxQUICListen.MatchString(conf)
	cert := strings.Contains(lower, "ssl_certificate ") && strings.Contains(lower, "ssl_certificate_key ")
	tls13 := true
	if i := strings.Index(lower, "ssl_protocols"); i >= 0 {
		line := lower[i:]
		if end := strings.IndexAny(line, ";\n"); end >= 0 {
			line = line[:end]
		}
		tls13 = strings.Contains(line, "tlsv1.3")
	}
	advertise := altSvcH3.MatchString(conf)
	fallback := nginxTLSListen.MatchString(conf)
	return proxyReadiness{
		kind: "nginx", known: true, http3: h3, tls: cert && tls13,
		advertise: advertise, fallback: fallback,
		http3Detail:     boolDetail(h3, "listen ... quic を検出", "listen ... quic がない"),
		tlsDetail:       boolDetail(cert && tls13, "証明書設定とTLS 1.3を検出(証明書の実有効性は未検証)", "証明書設定またはTLS 1.3が不足"),
		advertiseDetail: boolDetail(advertise, "Alt-Svc h3 advertisement を検出", "Alt-Svc h3 advertisement がない"),
		fallbackDetail:  boolDetail(fallback, "TCP/TLS listener を検出", "TCP/TLS fallback listener がない"),
	}
}

func analyzeCaddyHTTP3(conf string) proxyReadiness {
	lower := strings.ToLower(conf)
	protocols := caddyProtocols.FindStringSubmatch(lower)
	h3, fallback := true, true // Documented Caddy defaults are h1+h2+h3.
	if len(protocols) == 2 {
		fields := strings.Fields(protocols[1])
		h3 = contains(fields, "h3")
		fallback = contains(fields, "h1") || contains(fields, "h2")
	}
	automaticCertificates := !strings.Contains(lower, "auto_https off") &&
		!strings.Contains(lower, "auto_https disable_certs")
	https := !onlyHTTPAddresses(lower) && (automaticCertificates || caddyTLS.MatchString(conf))
	advertise := h3 && !strings.Contains(lower, "header -alt-svc")
	return proxyReadiness{
		kind: "caddy", known: true, http3: h3, tls: https,
		advertise: advertise, fallback: fallback,
		http3Detail:     boolDetail(h3, "Caddy protocols にh3(default)を確認", "Caddy protocols でh3が無効"),
		tlsDetail:       boolDetail(https, "Caddy certificate pathを検出(実証明書は未検証)", "certificate automation/明示TLSがない、またはHTTP-only"),
		advertiseDetail: boolDetail(advertise, "Caddy h3 advertisement が有効", "h3無効またはAlt-Svc削除"),
		fallbackDetail:  boolDetail(fallback, "h1/h2 fallback が有効", "protocols がh3-only"),
	}
}

func analyzeEnvoyHTTP3(conf string) proxyReadiness {
	lower := strings.ToLower(conf)
	h3 := strings.Contains(lower, "protocol: udp") && strings.Contains(lower, "quic_options") && strings.Contains(lower, "quicdownstreamtransport")
	tls := strings.Contains(lower, "downstream_tls_context") && strings.Contains(lower, "tls_certificates")
	advertise := altSvcH3.MatchString(conf)
	fallback := strings.Contains(lower, "codec_type: auto") || strings.Contains(lower, `codec_type: "auto"`)
	return proxyReadiness{
		kind: "envoy", known: true, http3: h3, tls: tls, advertise: advertise, fallback: fallback,
		http3Detail:     boolDetail(h3, "UDP listener + quic_options + QUIC transportを検出", "Envoy downstream QUIC listenerが不完全"),
		tlsDetail:       boolDetail(tls, "QUIC downstream TLS contextを検出(証明書の実有効性は未検証)", "QUIC downstream TLS/certificate設定が不足"),
		advertiseDetail: boolDetail(advertise, "Alt-Svc h3 response headerを検出", "TCP listenerのAlt-Svc h3 response headerがない"),
		fallbackDetail:  boolDetail(fallback, "TCP listener codec AUTOを検出", "TCP HTTP/2・HTTP/1.1 fallbackを確認できない"),
	}
}

func proxyRecommendation(kind, action string) string {
	if kind == "" {
		return action
	}
	return kind + ": " + action
}

func boolDetail(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func onlyHTTPAddresses(conf string) bool {
	hasHTTP := false
	for _, line := range strings.Split(conf, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") {
			hasHTTP = true
		}
		if strings.HasPrefix(line, "https://") {
			return false
		}
	}
	return hasHTTP
}

func hasUDPListener(fsys fs.FS, port uint16) (bool, error) {
	if fsys == nil {
		return false, fmt.Errorf("filesystem unavailable")
	}
	want := fmt.Sprintf("%04X", port)
	readable := false
	for _, path := range []string{"proc/net/udp", "proc/net/udp6"} {
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			continue
		}
		readable = true
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if ok && strings.EqualFold(portHex, want) {
				return true, nil
			}
		}
	}
	if !readable {
		return false, fmt.Errorf("/proc/net/udp{,6} unavailable")
	}
	return false, nil
}

// WithProtocolTraffic replaces the dynamic HTTP/3 traffic check using one
// measurement source. Proxy access-log samples should be preferred because a
// reverse proxy hides the client protocol from application middleware.
func WithProtocolTraffic(checks []Check, source string, samples []ProtocolSample) []Check {
	return WithProtocolTrafficEvidence(checks, source, true, samples)
}

// WithProtocolTrafficEvidence replaces the traffic check and records whether
// the source observes the client-facing hop. Origin logs behind an edge must
// never be interpreted as client-to-edge HTTP/3 evidence.
func WithProtocolTrafficEvidence(checks []Check, source string, clientFacing bool, samples []ProtocolSample) []Check {
	result := make([]Check, 0, len(checks)+1)
	for _, check := range checks {
		if check.ID != "http3-traffic" {
			result = append(result, check)
		}
	}
	c := Check{ID: "http3-traffic", Title: "HTTP/3: measured traffic and migration signal"}
	totals := map[string]ProtocolSample{}
	for _, sample := range samples {
		protocol := canonicalHTTPProtocol(sample.Protocol)
		if protocol == "" || sample.Count <= 0 {
			continue
		}
		current := totals[protocol]
		current.Protocol = protocol
		current.Count += sample.Count
		current.Errors += sample.Errors
		if sample.P95 > current.P95 {
			current.P95 = sample.P95
		}
		totals[protocol] = current
	}
	if len(totals) == 0 {
		c.Status = StatusSkip
		c.Detail = "protocol付きtraffic sampleなし"
		c.Recommendation = "proxy access logにprotoを追加(直接HTTP/3終端ならapplication middlewareでも可)"
		result = append(result, c)
		return sortChecks(result)
	}
	order := []string{"HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/3.0", "(other)"}
	parts := make([]string, 0, len(totals))
	for _, protocol := range order {
		if sample, ok := totals[protocol]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d(5xx %s, p95<=%s)", protocol, sample.Count,
				formatErrorRate(sample.Errors, sample.Count), sample.P95))
		}
	}
	c.Detail = strings.Join(parts, " / ")
	if source != "" {
		c.Detail += " / source=" + source
	}
	if !clientFacing {
		c.Status = StatusInfo
		c.Recommendation = "このprotocolはedge→origin。client→edgeのHTTP/3比率はedge access log/analyticsで確認"
		result = append(result, c)
		return sortChecks(result)
	}
	h3 := totals["HTTP/3.0"]
	fallbackCount, fallbackErrors := int64(0), int64(0)
	for _, protocol := range []string{"HTTP/1.0", "HTTP/1.1", "HTTP/2.0"} {
		fallbackCount += totals[protocol].Count
		fallbackErrors += totals[protocol].Errors
	}
	if h3.Count == 0 {
		c.Status = StatusInfo
		c.Recommendation = "同一workloadでfallbackとHTTP/3のA/Bを行い、score・p95・error rateを比較"
	} else if h3.Count >= 20 && percent(h3.Errors, h3.Count) >= percent(fallbackErrors, fallbackCount)+1.0 && percent(h3.Errors, h3.Count) >= 2.0 {
		c.Status = StatusWarn
		c.Recommendation = "HTTP/3の5xx率がfallbackより高い。QUIC error、UDP drop、証明書、fallback挙動を確認してから移行"
	} else {
		c.Status = StatusOK
		c.Recommendation = "HTTP/3利用を確認。効果判定は同一workload A/Bのscore・p95・error rateで行う"
	}
	result = append(result, c)
	return sortChecks(result)
}

// WithQUICTelemetry replaces the dynamic QUIC transport check at snapshot
// time. Reading telemetry late avoids freezing start-of-generation counters.
func WithQUICTelemetry(checks []Check, telemetry *QUICTelemetry, err error) []Check {
	result := make([]Check, 0, len(checks)+1)
	for _, check := range checks {
		if check.ID != "http3-quic-health" {
			result = append(result, check)
		}
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	result = append(result, quicHealthCheck(telemetry, errText))
	return sortChecks(result)
}

func canonicalHTTPProtocol(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "HTTP/1", "HTTP/1.0", "H1":
		return "HTTP/1.0"
	case "HTTP/1.1", "H1.1":
		return "HTTP/1.1"
	case "HTTP/2", "HTTP/2.0", "H2":
		return "HTTP/2.0"
	case "HTTP/3", "HTTP/3.0", "H3":
		return "HTTP/3.0"
	case "":
		return ""
	default:
		return "(other)"
	}
}

func percent(n, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}

// formatErrorRate keeps rare real errors visible. The numerator is always
// present, while the percentage uses extra precision below 0.01% and a bound
// when even four decimals would round a non-zero value back to zero.
func formatErrorRate(errors, total int64) string {
	if errors <= 0 || total <= 0 {
		return fmt.Sprintf("%d/%d, 0%%", errors, total)
	}
	rate := percent(errors, total)
	switch {
	case rate < 0.0001:
		return fmt.Sprintf("%d/%d, <0.0001%%", errors, total)
	case rate < 0.01:
		return fmt.Sprintf("%d/%d, %.4f%%", errors, total, rate)
	default:
		return fmt.Sprintf("%d/%d, %.2f%%", errors, total, rate)
	}
}

func sortChecks(checks []Check) []Check {
	sort.SliceStable(checks, func(i, j int) bool {
		return statusRank[checks[i].Status] < statusRank[checks[j].Status]
	})
	return checks
}

// ParseEvidence parses environment-style readiness declarations.
func ParseEvidence(value string) Evidence {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "enabled", "reachable":
		return EvidenceYes
	case "0", "false", "no", "disabled", "blocked":
		return EvidenceNo
	default:
		return EvidenceUnknown
	}
}
