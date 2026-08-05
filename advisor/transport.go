package advisor

import (
	"fmt"
	"io/fs"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
)

// Transport checks cover the "same-host hop" levers of ISUCON tuning
// (ISUCON book 9-8) plus the Go equivalent of enabling a runtime JIT:
//
//   - nginx -> app over loopback TCP burns an ephemeral port and a full TCP
//     handshake per connection where a UNIX domain socket would not;
//   - raising net.core.somaxconn is only half a fix while nginx keeps its
//     built-in listen backlog of 511;
//   - a Go binary built without a profile leaves a few percent on the table.
//
// All three are opportunities, not defects, so they report StatusInfo and
// never StatusWarn/StatusMissing. Anything that cannot be resolved statically
// (variable interpolation, unusual listen forms, an unreadable /proc) degrades
// to StatusSkip instead of guessing: a wrong nginx recommendation costs more
// contest time than a missing one.

const (
	// nginxDefaultBacklog is nginx's built-in listen backlog (-1 => 511 on Linux).
	nginxDefaultBacklog = 511
	// somaxconnModernDefault is the net.core.somaxconn default since Linux 5.4
	// (128 before). At or above it, nginx's 511 becomes the binding limit.
	somaxconnModernDefault = 4096
	// maxNginxStatements bounds parser memory for pathological input.
	maxNginxStatements = 20000
	// maxNginxBlockDepth bounds the block stack for pathological input.
	maxNginxBlockDepth = 64
	// maxDetailItems bounds how many items a Detail string enumerates.
	maxDetailItems = 6
)

// concatCaveat records that the analysed input is a concatenation of conf
// fragments: include relationships are not resolved here, so a disabled file
// that still ends in .conf may be part of the input.
const concatCaveat = "解析対象: 連結された nginx conf(include 関係は未解決)"

// nginxBlock is one open block ("server {", "upstream app {") enclosing a
// directive. Args are needed to attribute upstream servers to their block.
type nginxBlock struct {
	name string
	args []string
}

// nginxStmt is one ";"-terminated directive together with the block stack it
// appeared in. The stack is what lets checks exclude stream/mail listens and
// tell an upstream "server" from a "server {" block.
type nginxStmt struct {
	words []string
	line  int
	stack []nginxBlock
}

// enclosedBy reports whether any enclosing block has the given name.
func (s nginxStmt) enclosedBy(name string) bool {
	for _, b := range s.stack {
		if b.name == name {
			return true
		}
	}
	return false
}

// innermost returns the directly enclosing block ("" when at top level).
func (s nginxStmt) innermost() nginxBlock {
	if len(s.stack) == 0 {
		return nginxBlock{}
	}
	return s.stack[len(s.stack)-1]
}

// parseNginxStmts is a deliberately small brace/semicolon scanner: it tracks
// enough structure (block stack, line numbers) for the transport checks
// without pretending to be an nginx parser. Comments must already be stripped.
func parseNginxStmts(conf string) []nginxStmt {
	stmts := make([]nginxStmt, 0, 64)
	stack := make([]nginxBlock, 0, 8)
	var cur strings.Builder
	line, start := 1, 1
	started := false
	quote := byte(0)

	take := func() []string {
		words := strings.Fields(cur.String())
		cur.Reset()
		started = false
		return words
	}
	for i := 0; i < len(conf); i++ {
		c := conf[i]
		if quote != 0 {
			if c == '\n' {
				line++
			}
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '\n':
			line++
			cur.WriteByte(' ')
		case ';':
			if words := take(); len(words) > 0 && len(stmts) < maxNginxStatements {
				stmts = append(stmts, nginxStmt{words: words, line: start, stack: cloneBlocks(stack)})
			}
		case '{':
			words := take()
			block := nginxBlock{}
			if len(words) > 0 {
				block = nginxBlock{name: words[0], args: words[1:]}
			}
			if len(stack) < maxNginxBlockDepth {
				stack = append(stack, block)
			}
		case '}':
			take()
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			if !started && c != ' ' && c != '\t' && c != '\r' {
				start, started = line, true
			}
			cur.WriteByte(c)
		}
	}
	return stmts
}

func cloneBlocks(stack []nginxBlock) []nginxBlock {
	out := make([]nginxBlock, len(stack))
	copy(out, stack)
	return out
}

// checkTransport runs the same-host transport opportunities and the PGO check.
func checkTransport(opts Options) []Check {
	return []Check{
		checkUpstreamUDS(opts),
		checkListenBacklog(opts),
		checkPGO(debug.ReadBuildInfo),
	}
}

// --- nginx-upstream-uds ------------------------------------------------------

// upstreamTarget is one statically resolved nginx upstream destination.
type upstreamTarget struct {
	raw    string // address as written
	origin string // "proxy_pass" or "upstream <name>"
}

// upstreamScan is the classification of every upstream destination in a conf.
type upstreamScan struct {
	loopback    []upstreamTarget // plain-HTTP loopback: the UDS opportunity
	sockets     int              // already unix:
	loopbackTLS int              // https to loopback: excluded, UDS would drop TLS
	remote      int              // another host / container service name
	unresolved  int              // variables etc.
}

func checkUpstreamUDS(opts Options) Check {
	c := Check{ID: "nginx-upstream-uds", Title: "nginx: 同一ホスト upstream の UNIX domain socket 化"}
	if len(opts.NginxConf) == 0 {
		c.Status = StatusSkip
		c.Detail = "ISUTOOLS_NGINX_CONF 未設定(conf を読めるようにすると検査できます)"
		return c
	}
	scan := scanUpstreams(parseNginxStmts(stripComments(string(opts.NginxConf))))

	switch {
	case len(scan.loopback) > 0:
		items := make([]string, 0, len(scan.loopback))
		for _, t := range scan.loopback {
			items = append(items, fmt.Sprintf("%s(%s)", t.raw, t.origin))
		}
		c.Status = StatusInfo
		c.Detail = fmt.Sprintf("同一ホスト宛の TCP upstream: %s。ephemeral port と TCP ハンドシェイクを毎接続で消費している / %s",
			joinLimited(items, maxDetailItems), concatCaveat)
		c.Recommendation = udsRecommendation
	case scan.sockets > 0:
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("upstream は既に unix: ソケット(%d 件)", scan.sockets)
	case scan.loopbackTLS > 0:
		c.Status = StatusSkip
		c.Detail = "対象外: 同一ホスト宛は https のみ(UDS 化すると TLS を失うため提案しない)"
	case scan.remote > 0:
		c.Status = StatusSkip
		c.Detail = "対象外: upstream が同一ホストではない(別ホスト/コンテナのサービス名)"
	case scan.unresolved > 0:
		c.Status = StatusSkip
		c.Detail = "対象外: upstream の宛先を静的に解決できない(変数展開・動的 resolver)"
	default:
		c.Status = StatusSkip
		c.Detail = "proxy_pass がなく対象外"
	}
	return c
}

// udsRecommendation intentionally names /run/<app>/ and never a world-writable
// shared temp directory: there a third party can pre-create the path before the
// app binds (EADDRINUSE denial of service, and stale-socket cleanup deleting
// someone else's file), and a proxy unit with PrivateTmp=true would not even
// see the same path.
const udsRecommendation = "同一ホストなら UDS へ。app を /run/<app>/app.sock で listen させ" +
	"(専用ディレクトリ 0750・socket 0660・owner を app、group を nginx worker と共有)、" +
	"nginx は server unix:/run/<app>/app.sock;。world-writable な共有一時ディレクトリは" +
	"第三者による事前生成と PrivateTmp による分離があるため使わない。" +
	"再起動時は socket であることを確認してから stale を unlink(systemd なら RuntimeDirectory= が停止時に自動削除)。" +
	"前提: (a) app を UDS listen に変更できる (b) nginx と app が同じ mount namespace で socket を共有できる " +
	"(c) 同一ホスト間で TLS が不要"

func scanUpstreams(stmts []nginxStmt) upstreamScan {
	scan := upstreamScan{}
	type upstreamServer struct {
		name string
		addr string
	}
	proxyPass := make([]string, 0, 8)
	servers := make([]upstreamServer, 0, 8)
	names := map[string]bool{}

	for _, s := range stmts {
		switch {
		case s.words[0] == "proxy_pass" && len(s.words) >= 2:
			proxyPass = append(proxyPass, s.words[1])
		case s.words[0] == "server" && len(s.words) >= 2 && s.innermost().name == "upstream":
			name := ""
			if args := s.innermost().args; len(args) > 0 {
				name = args[0]
			}
			if name != "" {
				names[name] = true
			}
			servers = append(servers, upstreamServer{name: name, addr: s.words[1]})
		}
	}

	// An upstream reached over https must not be proposed for UDS conversion:
	// the socket carries no TLS. Resolve the indirection before classifying.
	tlsUpstream := map[string]bool{}
	for _, raw := range proxyPass {
		scheme, authority, ok := splitProxyPassTarget(raw)
		if ok && scheme == "https" && names[hostOfAuthority(authority)] {
			tlsUpstream[hostOfAuthority(authority)] = true
		}
	}

	for _, raw := range proxyPass {
		if strings.Contains(raw, "$") {
			scan.unresolved++
			continue
		}
		scheme, authority, ok := splitProxyPassTarget(raw)
		if !ok {
			scan.unresolved++
			continue
		}
		if isUnixTarget(authority) {
			scan.sockets++
			continue
		}
		host := hostOfAuthority(authority)
		switch {
		case names[host]:
			// Classified through the upstream block below.
		case !isLoopbackHost(host):
			scan.remote++
		case scheme == "https":
			scan.loopbackTLS++
		default:
			scan.loopback = append(scan.loopback, upstreamTarget{raw: authority, origin: "proxy_pass"})
		}
	}

	for _, srv := range servers {
		if strings.Contains(srv.addr, "$") {
			scan.unresolved++
			continue
		}
		if isUnixTarget(srv.addr) {
			scan.sockets++
			continue
		}
		origin := "upstream"
		if srv.name != "" {
			origin = "upstream " + srv.name
		}
		switch {
		case !isLoopbackHost(hostOfAuthority(srv.addr)):
			scan.remote++
		case tlsUpstream[srv.name]:
			scan.loopbackTLS++
		default:
			scan.loopback = append(scan.loopback, upstreamTarget{raw: srv.addr, origin: origin})
		}
	}
	return scan
}

// splitProxyPassTarget splits "http://host:port/path" into scheme and
// authority. A missing/unknown scheme is reported as unresolvable rather than
// assumed, because proxy_pass without a scheme is not valid nginx.
func splitProxyPassTarget(raw string) (scheme, authority string, ok bool) {
	lower := strings.ToLower(raw)
	rest := ""
	switch {
	case strings.HasPrefix(lower, "http://"):
		scheme, rest = "http", raw[len("http://"):]
	case strings.HasPrefix(lower, "https://"):
		scheme, rest = "https", raw[len("https://"):]
	default:
		return "", "", false
	}
	if isUnixTarget(rest) {
		return scheme, rest, true
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "", "", false
	}
	return scheme, rest, true
}

func isUnixTarget(s string) bool {
	return strings.HasPrefix(strings.ToLower(s), "unix:")
}

// hostOfAuthority extracts the host from "host", "host:port" or "[v6]:port".
func hostOfAuthority(authority string) string {
	if strings.HasPrefix(authority, "[") {
		if i := strings.IndexByte(authority, ']'); i > 0 {
			return authority[1:i]
		}
		return ""
	}
	if i := strings.IndexByte(authority, ':'); i >= 0 {
		return authority[:i]
	}
	return authority
}

// isLoopbackHost reports whether traffic to host stays on this host. Only
// literal loopback addresses and localhost qualify; arbitrary names may resolve
// anywhere and are left alone.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// --- nginx-listen-backlog ----------------------------------------------------

// listenEndpoint is one listen socket (address:port). backlog is a socket
// parameter, so several server blocks sharing an address:port share one
// backlog and nginx accepts the parameter on only one of those listens.
type listenEndpoint struct {
	key       string
	backlog   int
	backlogAt []int // lines carrying backlog=; more than one cannot start nginx
	listens   int   // how many listen directives map to this endpoint
}

func checkListenBacklog(opts Options) Check {
	c := Check{ID: "nginx-listen-backlog", Title: "nginx: listen backlog(accept キュー)"}
	if len(opts.NginxConf) == 0 {
		c.Status = StatusSkip
		c.Detail = "ISUTOOLS_NGINX_CONF 未設定(conf を読めるようにすると検査できます)"
		return c
	}
	somaxconn, err := readSomaxconn(opts.FS)
	if err != nil {
		c.Status = StatusSkip
		c.Detail = "somaxconn を読めないため backlog との比較ができません"
		return c
	}
	endpoints, skipped := effectiveListenEndpoints(parseNginxStmts(stripComments(string(opts.NginxConf))))

	parts := make([]string, 0, len(endpoints))
	status := StatusOK
	for _, ep := range endpoints {
		if len(ep.backlogAt) >= 2 {
			skipped = append(skipped, fmt.Sprintf("%s に backlog= が %d 箇所(行 %s。nginx は duplicate listen options で起動に失敗するため、"+
				"読んだ conf が稼働中のものと異なる疑いがある)",
				ep.key, len(ep.backlogAt), joinInts(ep.backlogAt)))
			continue
		}
		if ep.backlog == 0 {
			parts = append(parts, fmt.Sprintf("%s backlog 未指定(既定 %d)", ep.key, nginxDefaultBacklog))
			if somaxconn >= somaxconnModernDefault {
				status = StatusInfo
			}
			continue
		}
		parts = append(parts, fmt.Sprintf("%s backlog=%d", ep.key, ep.backlog))
		if uint64(ep.backlog)*2 < somaxconn {
			status = StatusInfo
		}
	}
	if len(parts) == 0 {
		c.Status = StatusSkip
		c.Detail = "解析できる listen が見つかりませんでした"
		if len(skipped) > 0 {
			c.Detail += ": " + joinLimited(skipped, maxDetailItems)
		}
		return c
	}

	c.Status = status
	c.Detail = fmt.Sprintf("somaxconn=%d / %s", somaxconn, joinLimited(parts, maxDetailItems))
	if len(skipped) > 0 {
		c.Detail += fmt.Sprintf(" / 判定対象外: %d 件(%s)", len(skipped), joinLimited(skipped, 2))
	}
	c.Detail += " / " + concatCaveat
	if status == StatusInfo {
		c.Recommendation = fmt.Sprintf("listen 80 backlog=%d; のように somaxconn に見合う値を明示(nginx 既定は %d のままで、"+
			"カーネルの somaxconn=%d を上げても accept キューは %d に制限される)。"+
			"同一 address:port では 1 箇所の listen にのみ指定でき、重複すると nginx は起動に失敗する",
			somaxconn, nginxDefaultBacklog, somaxconn, nginxDefaultBacklog)
	}
	return c
}

// effectiveListenEndpoints groups listen directives by normalized address:port
// and returns the endpoints it could resolve plus a reason per listen it
// deliberately did not judge.
func effectiveListenEndpoints(stmts []nginxStmt) (endpoints []listenEndpoint, skipped []string) {
	index := map[string]int{}
	for _, s := range stmts {
		if s.words[0] != "listen" {
			continue
		}
		if s.enclosedBy("stream") || s.enclosedBy("mail") {
			skipped = append(skipped, fmt.Sprintf("stream/mail の listen(行 %d)", s.line))
			continue
		}
		if s.innermost().name != "server" {
			skipped = append(skipped, fmt.Sprintf("server ブロック外の listen(行 %d)", s.line))
			continue
		}
		if len(s.words) < 2 {
			skipped = append(skipped, fmt.Sprintf("引数のない listen(行 %d)", s.line))
			continue
		}
		key, ok := listenEndpointKey(s.words[1])
		if !ok {
			skipped = append(skipped, fmt.Sprintf("解釈できない listen %q(行 %d)", s.words[1], s.line))
			continue
		}
		backlog, ok := listenBacklog(s.words[2:])
		if !ok {
			skipped = append(skipped, fmt.Sprintf("解釈できない backlog= (行 %d)", s.line))
			continue
		}
		i, seen := index[key]
		if !seen {
			i = len(endpoints)
			index[key] = i
			endpoints = append(endpoints, listenEndpoint{key: key})
		}
		ep := endpoints[i]
		ep.listens++
		if backlog > 0 {
			ep.backlog = backlog
			ep.backlogAt = append(append([]int{}, ep.backlogAt...), s.line)
		}
		endpoints[i] = ep
	}
	return endpoints, skipped
}

// listenEndpointKey normalizes a listen address into the socket it opens.
// "80" and "*:80" are the IPv4 wildcard; "[::]:80" is a different socket
// because ipv6only is on by default. Forms whose socket cannot be determined
// (variables, a bare address with no port, a hostname) are rejected.
func listenEndpointKey(addr string) (string, bool) {
	if addr == "" || strings.Contains(addr, "$") {
		return "", false
	}
	if isUnixTarget(addr) {
		path := addr[len("unix:"):]
		if path == "" {
			return "", false
		}
		return "unix:" + path, true
	}
	if v, err := strconv.Atoi(addr); err == nil {
		if v <= 0 || v > 65535 {
			return "", false
		}
		return fmt.Sprintf("0.0.0.0:%d", v), true
	}
	host, port := "", ""
	if strings.HasPrefix(addr, "[") {
		i := strings.IndexByte(addr, ']')
		if i < 0 || !strings.HasPrefix(addr[i+1:], ":") {
			return "", false
		}
		host, port = addr[1:i], addr[i+2:]
	} else {
		i := strings.LastIndexByte(addr, ':')
		if i < 0 {
			// A bare address without a port: nginx applies its default port,
			// which depends on context and privileges. Do not guess.
			return "", false
		}
		host, port = addr[:i], addr[i+1:]
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return "", false
	}
	if host == "*" || host == "" {
		return fmt.Sprintf("0.0.0.0:%d", p), true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", false
	}
	if ip.To4() == nil {
		return fmt.Sprintf("[%s]:%d", ip.String(), p), true
	}
	return fmt.Sprintf("%s:%d", ip.String(), p), true
}

// listenBacklog returns the backlog= parameter (0 when absent). ok is false
// when the parameter is present but unreadable, so the caller can skip the
// endpoint instead of reporting a wrong value.
func listenBacklog(params []string) (int, bool) {
	for _, p := range params {
		if !strings.HasPrefix(p, "backlog=") {
			continue
		}
		v, err := strconv.Atoi(strings.TrimPrefix(p, "backlog="))
		if err != nil || v <= 0 {
			return 0, false
		}
		return v, true
	}
	return 0, true
}

func readSomaxconn(fsys fs.FS) (uint64, error) {
	if fsys == nil {
		return 0, fmt.Errorf("read somaxconn: no filesystem supplied")
	}
	v, err := readUintFile(fsys, "proc/sys/net/core/somaxconn")
	if err != nil {
		return 0, fmt.Errorf("read somaxconn: %w", err)
	}
	return v, nil
}

// --- go-pgo ------------------------------------------------------------------

// checkPGO reports whether this binary was built with a profile. The reader is
// injected so the classification can be tested with synthetic build settings.
func checkPGO(read func() (*debug.BuildInfo, bool)) Check {
	c := Check{ID: "go-pgo", Title: "Go: PGO(プロファイル誘導最適化)ビルド"}
	info, ok := read()
	if !ok || info == nil {
		c.Status = StatusSkip
		c.Detail = "buildinfo を読めないため PGO の適用状態を確認できません"
		return c
	}
	profile := ""
	for _, s := range info.Settings {
		if s.Key == "-pgo" {
			profile = strings.TrimSpace(s.Value)
			break
		}
	}
	if profile == "" || strings.EqualFold(profile, "off") {
		c.Status = StatusInfo
		c.Detail = "PGO なしのビルド(-pgo 設定なし)"
		c.Recommendation = "Go 1.21+ の PGO で数% の改善余地。isutools がベンチ中に採取した CPU プロファイル" +
			"(/files/ の *_cpu.pprof)を main パッケージに default.pgo として置いて再ビルドするだけで有効になる"
		return c
	}
	c.Status = StatusOK
	c.Detail = "PGO 適用済み: -pgo=" + profile
	return c
}

// joinInts renders line numbers for a Detail string.
func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ", ")
}

// joinLimited renders at most limit items and summarizes the remainder so a
// Detail string stays readable on the dashboard.
func joinLimited(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:limit], ", ") + fmt.Sprintf(" 他 %d 件", len(items)-limit)
}
