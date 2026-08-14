// Package advisor detects well-known ISUCON-critical settings that are NOT
// configured (prepared-statement round trips, nginx gzip/keepalive, kernel
// limits, GOMAXPROCS vs CPU quota, MySQL sizing) and reports them so the
// dashboard always shows what standard lever has not been pulled yet.
// Every check is fail-open: inspection problems degrade to StatusSkip.
package advisor

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
)

// Status classifies one check result.
type Status string

const (
	// StatusOK means the recommended setting is in place.
	StatusOK Status = "ok"
	// StatusMissing means a standard, high-impact setting is absent.
	StatusMissing Status = "missing"
	// StatusWarn means the current value is likely to hurt under load.
	StatusWarn Status = "warn"
	// StatusInfo is advisory context, not a defect.
	StatusInfo Status = "info"
	// StatusSkip means the check could not run (input unavailable).
	StatusSkip Status = "skip"
)

// Check is one advisor finding.
type Check struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Status         Status     `json:"status"`
	Detail         string     `json:"detail,omitempty"`
	Recommendation string     `json:"recommendation,omitempty"`
	Provenance     Provenance `json:"provenance"`
}

// Provenance explains the deterministic rule without copying raw config,
// SQL, driver errors, DSNs or credentials into the report.
type Provenance struct {
	RuleVersion string `json:"rule_version"`
	Category    string `json:"category"`
	Source      string `json:"source"`
	Freshness   string `json:"freshness"`
	Scope       string `json:"scope"`
	Formula     string `json:"formula"`
	Actual      string `json:"actual"`
	Unit        string `json:"unit"`
	Limitation  string `json:"limitation"`
	Docs        string `json:"docs"`
}

// Options supplies the inspectable inputs. Zero values skip the related checks.
type Options struct {
	DriverName string
	DSN        string
	// DB is an open raw (unproxied) connection for MySQL variable checks.
	// The caller owns closing it.
	DB *sql.DB
	// NginxConf is the concatenated nginx configuration content.
	NginxConf []byte
	// FS is the root filesystem ("/" in production, a fixture in tests).
	FS fs.FS
	// GOMAXPROCS is runtime.GOMAXPROCS(0); 0 skips the check.
	GOMAXPROCS int
	// Protocol supplies HTTP/3/QUIC readiness evidence. Configuration content
	// is inspected locally; off-host network and edge facts must be explicit.
	Protocol ProtocolOptions
	// Cache supplies application-side cache telemetry (memcached/redis
	// stats); nil skips the check. CacheError records why it could not be
	// read.
	Cache      *CacheTelemetry
	CacheError string
}

var statusRank = map[Status]int{
	StatusMissing: 0, StatusWarn: 1, StatusInfo: 2, StatusOK: 3, StatusSkip: 4,
}

var nginxUpstreamKeepalive = regexp.MustCompile(`(?m)(?:^|[;{}])\s*keepalive\s+[0-9]+\s*;`)

// Collect runs every check and returns findings sorted most-severe first.
func Collect(ctx context.Context, opts Options) []Check {
	checks := []Check{}
	checks = append(checks, checkDSN(opts)...)
	checks = append(checks, checkMySQL(ctx, opts)...)
	checks = append(checks, checkNginx(opts)...)
	checks = append(checks, checkOS(opts)...)
	checks = append(checks, checkGOMAXPROCS(opts))
	checks = append(checks, checkHTTP3(opts)...)
	checks = append(checks, checkResponseCache(opts)...)
	checks = append(checks, cacheHealthCheck(opts.Cache, opts.CacheError))
	checks = append(checks, queryPlanChecks(nil, "")...)
	checks = append(checks, checkECH(opts)...)
	checks = append(checks, checkTransport(opts)...)
	return sortChecks(checks)
}

func ensureProvenance(check Check) Check {
	if check.Provenance.RuleVersion != "" {
		return check
	}
	category, source, freshness, scope, limitation := provenanceClass(check.ID)
	check.Provenance = Provenance{
		RuleVersion: "advisor-v1", Category: category, Source: source,
		Freshness: freshness, Scope: scope,
		Formula: "deterministic predicate for rule " + check.ID + "; no generated score",
		Actual:  "status=" + string(check.Status), Unit: "state",
		Limitation: limitation,
		Docs:       "docs/ADVISOR_RULES.md#" + check.ID,
	}
	return check
}

func provenanceClass(id string) (category, source, freshness, scope, limitation string) {
	switch {
	case strings.HasPrefix(id, "dsn-"):
		return "config-inspection", "registered driver metadata", "connection-open", "process", "static DSN option inspection is not server runtime proof"
	case strings.HasPrefix(id, "mysql-"):
		return "measured-metric", "MySQL variables and information_schema", "generation-start", "registered-target", "requires a reachable MySQL-compatible inspector"
	case strings.HasPrefix(id, "nginx-"):
		return "config-inspection", "bounded proxy configuration", "generation-start", "configured-proxy", "static configuration does not prove the active process loaded it"
	case strings.HasPrefix(id, "os-"):
		return "measured-metric", "local procfs and cgroup files", "generation-start", "local-host", "off-host kernel state is unavailable"
	case strings.HasPrefix(id, "go-"):
		return "config-inspection", "Go runtime or executable metadata", "generation-start", "process", "build and runtime evidence do not establish workload impact"
	case strings.HasPrefix(id, "plan-"):
		return "measured-metric", "bounded query-plan capture", "run-finish", "registered-target", "plan evidence is sampled and correlation is not causation"
	case strings.HasPrefix(id, "cache-"):
		return "measured-metric", "application cache telemetry", "run-finish", "configured-cache", "counters require the same measured interval"
	case strings.HasPrefix(id, "http3-"):
		return "measured-metric", "protocol and QUIC evidence", "run-finish", "declared-hop", "origin traffic cannot prove client-to-edge protocol use"
	case strings.HasPrefix(id, "ech-"):
		return "external-evidence", "operator-declared edge evidence", "startup", "declared-edge", "declaration is not an active external probe"
	default:
		return "config-inspection", "bounded local inspection", "generation-start", "local-process", "recommendation requires workload-specific validation"
	}
}

func checkDSN(opts Options) []Check {
	c := Check{
		ID:    "dsn-interpolate-params",
		Title: "MySQL DSN: interpolateParams(プリペアドステートメント往復の削減)",
	}
	switch {
	case opts.DSN == "" || !strings.Contains(strings.ToLower(opts.DriverName), "mysql"):
		c.Status = StatusSkip
		c.Detail = "MySQL DSN が未観測"
	case strings.Contains(opts.DSN, "interpolateParams=true"):
		c.Status = StatusOK
	default:
		c.Status = StatusMissing
		c.Detail = "DSN に interpolateParams=true がない(クエリ毎に Prepare/Execute の2往復)"
		c.Recommendation = "DSN へ interpolateParams=true を追加(go-sql-driver はクライアント側プレースホルダ展開になり往復半減)"
	}
	return []Check{c}
}

func checkMySQL(ctx context.Context, opts Options) []Check {
	base := func(id, title string) Check { return Check{ID: id, Title: title} }
	maxConn := base("mysql-max-connections", "MySQL: max_connections")
	bufPool := base("mysql-buffer-pool", "MySQL: innodb_buffer_pool_size vs データ量")
	slowLog := base("mysql-slow-log", "MySQL: slow_query_log の常時 ON")
	if opts.DB == nil {
		maxConn.Status, bufPool.Status, slowLog.Status = StatusSkip, StatusSkip, StatusSkip
		return []Check{maxConn, bufPool, slowLog}
	}

	var maxConnections, bufferPool, slowOn, dataBytes int64
	row := opts.DB.QueryRowContext(ctx,
		"SELECT @@max_connections, @@innodb_buffer_pool_size, @@slow_query_log")
	if err := row.Scan(&maxConnections, &bufferPool, &slowOn); err != nil {
		msg := "MySQL 変数の取得に失敗: " + err.Error()
		maxConn.Status, bufPool.Status, slowLog.Status = StatusSkip, StatusSkip, StatusSkip
		maxConn.Detail = msg
		return []Check{maxConn, bufPool, slowLog}
	}
	_ = opts.DB.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.tables WHERE table_schema = DATABASE()").
		Scan(&dataBytes)

	maxConn.Detail = fmt.Sprintf("max_connections=%d", maxConnections)
	if maxConnections < 512 {
		maxConn.Status = StatusInfo
		maxConn.Recommendation = "アプリのコネクションプール上限×台数より十分大きいか確認(既定151は詰まりやすい)"
	} else {
		maxConn.Status = StatusOK
	}

	bufPool.Detail = fmt.Sprintf("buffer_pool=%dMB / データ+索引=%dMB", bufferPool>>20, dataBytes>>20)
	if dataBytes > 0 && bufferPool < dataBytes {
		bufPool.Status = StatusWarn
		bufPool.Recommendation = "innodb_buffer_pool_size をデータ+索引サイズ以上に(ディスク読みを排除)"
	} else {
		bufPool.Status = StatusOK
	}

	if slowOn == 1 {
		slowLog.Status = StatusInfo
		slowLog.Detail = "slow_query_log=ON"
		slowLog.Recommendation = "計測が終わったら本計測ベンチでは OFF に(書き込みオーバーヘッド)"
	} else {
		slowLog.Status = StatusOK
	}
	return []Check{maxConn, bufPool, slowLog}
}

func checkNginx(opts Options) []Check {
	mk := func(id, title string) Check { return Check{ID: id, Title: title} }
	checks := []Check{
		mk("nginx-gzip", "nginx: gzip 圧縮"),
		mk("nginx-keepalive", "nginx: upstream keepalive"),
		mk("nginx-worker-connections", "nginx: worker_connections"),
		mk("nginx-sendfile", "nginx: sendfile"),
		mk("nginx-expires", "nginx: 静的配信の expires/Cache-Control"),
	}
	if len(opts.NginxConf) == 0 {
		for i := range checks {
			checks[i].Status = StatusSkip
			checks[i].Detail = "ISUTOOLS_NGINX_CONF 未設定(conf を読めるようにすると検査できます)"
		}
		return checks
	}
	conf := stripComments(string(opts.NginxConf))

	set := func(i int, ok bool, missDetail, rec string) {
		if ok {
			checks[i].Status = StatusOK
		} else {
			checks[i].Status = StatusMissing
			checks[i].Detail = missDetail
			checks[i].Recommendation = rec
		}
	}
	set(0, strings.Contains(conf, "gzip on"),
		"gzip 未設定(HTML/JSON が非圧縮で転送)",
		"http コンテキストに gzip on; gzip_types text/css application/json application/javascript; を追加")
	set(1, nginxUpstreamKeepalive.MatchString(conf),
		"upstream keepalive なし(リクエスト毎に TCP 再接続 → 高RPSで502)",
		"upstream ブロックに keepalive 64; + proxy_http_version 1.1 + Connection \"\"")
	wc := 0
	if m := firstUintAfter(conf, "worker_connections"); m > 0 {
		wc = int(m)
	}
	if wc >= 4096 {
		checks[2].Status = StatusOK
		checks[2].Detail = fmt.Sprintf("worker_connections=%d", wc)
	} else {
		checks[2].Status = StatusWarn
		checks[2].Detail = fmt.Sprintf("worker_connections=%d", wc)
		checks[2].Recommendation = "4096 以上に(既定1024は高RPSで枯渇)"
	}
	set(3, strings.Contains(conf, "sendfile on"),
		"sendfile 未設定",
		"http コンテキストに sendfile on;(静的ファイルのカーネル内転送)")
	set(4, strings.Contains(conf, "expires") || strings.Contains(conf, "Cache-Control"),
		"静的配信に expires なし(毎回フル転送)",
		"静的 location に expires 1d; 等(304/クライアントキャッシュ活用)")
	return checks
}

func checkOS(opts Options) []Check {
	mk := func(id, title string) Check { return Check{ID: id, Title: title} }
	somax := mk("os-somaxconn", "OS: net.core.somaxconn(accept キュー)")
	ports := mk("os-port-range", "OS: ip_local_port_range(エフェメラルポート)")
	nofile := mk("os-nofile", "OS: open files 上限(nofile)")
	if opts.FS == nil {
		somax.Status, ports.Status, nofile.Status = StatusSkip, StatusSkip, StatusSkip
		return []Check{somax, ports, nofile}
	}

	if v, err := readUintFile(opts.FS, "proc/sys/net/core/somaxconn"); err != nil {
		somax.Status = StatusSkip
	} else {
		somax.Detail = fmt.Sprintf("somaxconn=%d(既定値は Linux 5.4 以降 4096、それ以前は 128)", v)
		if v < 1024 {
			somax.Status = StatusWarn
			somax.Recommendation = "net.core.somaxconn=4096 以上(書籍の実例は 8192)に。接続要求の取りこぼし防止。" +
				"nginx の listen backlog は既定 511 のままなので、上げるなら listen ... backlog= も合わせて明示する"
		} else {
			somax.Status = StatusOK
		}
	}

	if data, err := fs.ReadFile(opts.FS, "proc/sys/net/ipv4/ip_local_port_range"); err != nil {
		ports.Status = StatusSkip
	} else {
		fields := strings.Fields(string(data))
		if len(fields) == 2 {
			lo, _ := strconv.Atoi(fields[0])
			hi, _ := strconv.Atoi(fields[1])
			ports.Detail = fmt.Sprintf("%d-%d(幅%d)", lo, hi, hi-lo)
			if hi-lo < 20000 {
				ports.Status = StatusWarn
				ports.Recommendation = "net.ipv4.ip_local_port_range=\"10000 65535\" 等に拡大(接続枯渇防止)"
			} else {
				ports.Status = StatusOK
			}
		} else {
			ports.Status = StatusSkip
		}
	}

	if soft, err := readNofileSoftLimit(opts.FS); err != nil {
		nofile.Status = StatusSkip
	} else {
		nofile.Detail = fmt.Sprintf("soft=%d", soft)
		if soft < 8192 {
			nofile.Status = StatusWarn
			nofile.Recommendation = "nofile を 65536 以上に(ソケット/ファイル記述子の枯渇防止)"
		} else {
			nofile.Status = StatusOK
		}
	}
	return []Check{somax, ports, nofile}
}

func checkGOMAXPROCS(opts Options) Check {
	c := Check{ID: "go-gomaxprocs", Title: "Go: GOMAXPROCS vs cgroup CPU クォータ"}
	if opts.GOMAXPROCS <= 0 || opts.FS == nil {
		c.Status = StatusSkip
		return c
	}
	data, err := fs.ReadFile(opts.FS, "sys/fs/cgroup/cpu.max")
	if err != nil {
		c.Status = StatusSkip
		return c
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] == "max" {
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("CPU クォータなし / GOMAXPROCS=%d", opts.GOMAXPROCS)
		return c
	}
	quota, err1 := strconv.ParseFloat(fields[0], 64)
	period, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || period == 0 {
		c.Status = StatusSkip
		return c
	}
	cores := quota / period
	c.Detail = fmt.Sprintf("クォータ=%.1fコア / GOMAXPROCS=%d", cores, opts.GOMAXPROCS)
	if float64(opts.GOMAXPROCS) > cores*2 {
		c.Status = StatusWarn
		c.Recommendation = fmt.Sprintf("GOMAXPROCS=%d 程度に(クォータ超のスレッドは CFS スロットリングで空転し sys%% を浪費)", int(cores)+1)
	} else {
		c.Status = StatusOK
	}
	return c
}

func stripComments(conf string) string {
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(conf))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func firstUintAfter(conf, directive string) uint64 {
	i := strings.Index(conf, directive)
	if i < 0 {
		return 0
	}
	rest := strings.Fields(conf[i+len(directive):])
	if len(rest) == 0 {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSuffix(rest[0], ";"), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func readUintFile(fsys fs.FS, path string) (uint64, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func readNofileSoftLimit(fsys fs.FS) (uint64, error) {
	data, err := fs.ReadFile(fsys, "proc/self/limits")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Max open files") {
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				return strconv.ParseUint(fields[3], 10, 64)
			}
		}
	}
	return 0, fmt.Errorf("max open files not found")
}
