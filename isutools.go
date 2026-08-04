// Package isutools is an all-in-one profiling module for ISUCON-style
// tuning: wrap your SQL driver, download sorted reports.
//
// Minimal integration (1 line):
//
//	db, _ := sqlx.Open(isutools.SQLDriverName("mysql"), dsn)
//
// SQLDriverName also starts a small admin server (default 127.0.0.1:19191,
// override with ISUTOOLS_ADDR, disable with ISUTOOLS_ADDR=off) serving the
// report UI, snapshot export, and POST /reset — the control channel for
// bench scripts. It intentionally runs on its own port so the application
// router and reverse proxy never expose it.
//
// ISUTOOLS=off disables everything: SQLDriverName then returns the raw
// driver name, so the application runs unproxied with zero overhead. The
// on/off decision is made once at startup; it is not dynamic.
package isutools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/counters"
	"github.com/ekusiadadus/isutools/dbinspect"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/procstats"
	"github.com/ekusiadadus/isutools/sqlstats"
	"github.com/ekusiadadus/isutools/web"
)

// defaultAdminAddr binds to loopback so the admin server is never reachable
// from outside the host unless ISUTOOLS_ADDR explicitly widens it.
const defaultAdminAddr = "127.0.0.1:19191"

var (
	adminOnce       sync.Once
	adminMu         sync.Mutex
	adminBind       string
	collectorHealth = health.NewRegistry()
)

// Off reports whether measurement is globally disabled via ISUTOOLS=off.
func Off() bool { return os.Getenv("ISUTOOLS") == "off" }

// SQLDriverName registers a measuring wrapper for the named driver and
// returns the driver name the application should open. When disabled — or
// if registration fails — it returns the raw name unchanged, so measurement
// can never break application startup (fail-open). On success it also
// starts the admin server once.
func SQLDriverName(name string) string {
	if Off() {
		return name
	}
	if err := sqlstats.Register(name); err != nil {
		collectorHealth.Set("sql", health.StatusFailed, err.Error())
		log.Printf("isutools: sql registration failed: %v", err)
		return name
	}
	collectorHealth.Set("sql", health.StatusOK, "")
	startAdmin()
	return name + sqlstats.DriverSuffix
}

// pprofDuration parses ISUTOOLS_PPROF_SECONDS (0 or unset = disabled).
func pprofDuration(getenv func(string) string) time.Duration {
	v := getenv("ISUTOOLS_PPROF_SECONDS")
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// resolveAdminAddr returns the bind address, or "" when disabled.
func resolveAdminAddr(getenv func(string) string) string {
	switch addr := getenv("ISUTOOLS_ADDR"); addr {
	case "off":
		return ""
	case "":
		return defaultAdminAddr
	default:
		return addr
	}
}

func resolveAccessLogPath(getenv func(string) string) string {
	if path := getenv("ISUTOOLS_ACCESS_LOG"); path != "" {
		return path
	}
	return getenv("ISUTOOLS_NGINX_LOG")
}

// startAdmin starts the admin HTTP server once. Failures are logged and
// otherwise ignored: the report becomes unavailable, the app is unaffected.
func startAdmin() {
	adminOnce.Do(func() {
		addr := resolveAdminAddr(os.Getenv)
		if addr == "" {
			collectorHealth.Set("admin", health.StatusDisabled, "disabled by ISUTOOLS_ADDR")
			return
		}
		allowUnauthenticated := os.Getenv("ISUTOOLS_ALLOW_UNAUTHENTICATED") == "1"
		if !isLoopbackAdminAddr(addr) && !allowUnauthenticated {
			err := errors.New("non-loopback admin bind requires explicit ISUTOOLS_ALLOW_UNAUTHENTICATED=1 and external SSH/firewall isolation")
			collectorHealth.Set("admin", health.StatusFailed, err.Error())
			log.Printf("isutools: admin server disabled: %v", err)
			return
		}
		unprotectedNonLoopback := !isLoopbackAdminAddr(addr) && allowUnauthenticated
		if unprotectedNonLoopback {
			message := "SECURITY WARNING: non-loopback admin bind enabled; restrict host publishing to 127.0.0.1 and use SSH"
			collectorHealth.Set("admin", health.StatusOK, message)
			log.Printf("isutools: warning: %s", message)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			collectorHealth.Set("admin", health.StatusFailed, err.Error())
			log.Printf("isutools: admin listen on %s failed: %v", addr, err)
			return
		}
		if !unprotectedNonLoopback {
			collectorHealth.Set("admin", health.StatusOK, "")
		}
		adminMu.Lock()
		adminBind = ln.Addr().String()
		adminMu.Unlock()
		log.Printf("isutools: admin server on http://%s", ln.Addr())
		handler, err := protectAdmin(addr, allowUnauthenticated, Handler())
		if err != nil {
			_ = ln.Close()
			collectorHealth.Set("admin", health.StatusFailed, err.Error())
			return
		}
		go func() {
			server := &http.Server{
				Handler:           handler,
				ReadHeaderTimeout: 5 * time.Second,
				IdleTimeout:       60 * time.Second,
			}
			_ = server.Serve(ln)
		}()
	})
}

// adminAddr returns the actual bound address of the admin server
// ("" if it is not running).
func adminAddr() string {
	adminMu.Lock()
	defer adminMu.Unlock()
	return adminBind
}

func isLoopbackAdminAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func protectAdmin(addr string, allowUnauthenticated bool, next http.Handler) (http.Handler, error) {
	if !isLoopbackAdminAddr(addr) && !allowUnauthenticated {
		return nil, errors.New("non-loopback admin bind requires explicit external-isolation opt-in")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequestHost(r.Host) || strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") ||
			!sameAdminOrigin(r.Header.Get("Origin"), r.Host) || !sameAdminOrigin(r.Header.Get("Referer"), r.Host) {
			http.Error(w, "forbidden by SSH-only admin policy", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}

func isLoopbackRequestHost(hostport string) bool {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameAdminOrigin(value, requestHost string) bool {
	if value == "" {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || !isLoopbackRequestHost(parsed.Host) {
		return false
	}
	return strings.EqualFold(parsed.Host, requestHost)
}

// RegisterSQL wraps the named drivers ("mysql", "pgx", ...) and registers
// measuring variants under "<name>:isutools". Prefer SQLDriverName, which
// also resolves the on/off decision. No-op when disabled.
func RegisterSQL(names ...string) error {
	if Off() {
		return nil
	}
	err := sqlstats.Register(names...)
	if err != nil {
		collectorHealth.Set("sql", health.StatusFailed, err.Error())
		return err
	}
	collectorHealth.Set("sql", health.StatusOK, "")
	return nil
}

// HTTP instruments inbound HTTP requests. When ISUTOOLS=off it returns next
// unchanged, avoiding request-path overhead. Path normalization rules can be
// injected via ISUTOOLS_PATH_RULES ("regex=replacement;..." — split on the
// last '=' of each pair).
func HTTP(next http.Handler) http.Handler {
	if Off() {
		return next
	}
	pathRulesOnce.Do(func() {
		spec := os.Getenv("ISUTOOLS_PATH_RULES")
		if spec == "" {
			return
		}
		rules, err := httpstats.ParseRules(spec)
		if err != nil {
			log.Printf("isutools: ISUTOOLS_PATH_RULES ignored: %v", err)
			return
		}
		httpstats.Default.SetRules(rules)
	})
	return httpstats.Middleware(next)
}

var pathRulesOnce sync.Once

// Count increments a named user counter by 1 (e.g. cache hit/miss). Shown
// in the report's Counters section, reset per generation. No-op when off.
func Count(name string) { AddCount(name, 1) }

// AddCount increments a named user counter by delta. No-op when off.
func AddCount(name string, delta int64) {
	if Off() {
		return
	}
	counters.Default.Add(name, delta)
}

// Handler serves the report UI: GET / (dashboard with snapshot history),
// GET /snapshot.html (download), GET /json, GET /files/<name>,
// POST /reset, POST /collect, POST /save. Snapshot history persists to ISUTOOLS_DATA_DIR
// when set. The DB schema is inspected through the first DSN the
// application opened, using the raw driver so inspection queries never
// appear in the SQL statistics.
func Handler() http.Handler {
	provider := web.Provider{
		SQL:           sqlstats.Default,
		SQLGeneration: sqlstats.Default.CurrentGeneration,
		RotateSQL: func() (int64, []agg.Entry) {
			frozen := sqlstats.Default.Rotate()
			return frozen.Generation, frozen.Entries
		},
		Health:        collectorHealth,
		HTTP:          httpstats.Default,
		DataDir:       os.Getenv("ISUTOOLS_DATA_DIR"),
		PprofDuration: pprofDuration(os.Getenv),
		DB: func(ctx context.Context) *dbinspect.Schema {
			name, dsn, ok := sqlstats.FirstConn()
			if !ok {
				return nil
			}
			return dbinspect.Collect(ctx, name, dsn)
		},
		Advisor:  collectAdvice,
		Counters: counters.Default,
	}
	trafficClientFacing := os.Getenv("ISUTOOLS_HTTP3_EDGE") == ""
	provider.ProtocolTrafficClientFacing = &trafficClientFacing
	if path := os.Getenv("ISUTOOLS_HTTP3_QUIC_METRICS"); path != "" {
		provider.QUICTelemetry = func() (*advisor.QUICTelemetry, error) {
			return readQUICTelemetryFile(path)
		}
	}
	if path := os.Getenv("ISUTOOLS_CACHE_METRICS"); path != "" {
		provider.CacheTelemetry = func() (*advisor.CacheTelemetry, error) {
			return readCacheTelemetryFile(path)
		}
	}
	collectorHealth.Set("http", health.StatusOK, "")
	if path := resolveAccessLogPath(os.Getenv); path != "" {
		collector := accesslog.New(path)
		provider.AccessLog = collector
		provider.AccessLogQuiet = 100 * time.Millisecond
		provider.AccessLogPoll = 25 * time.Millisecond
		provider.CollectTimeout = 2 * time.Second
		state := collector.Health()
		if state.Status == accesslog.StatusOK {
			collectorHealth.Set("accesslog", health.StatusOK, "")
		} else {
			collectorHealth.Set("accesslog", health.StatusDegraded, state.Message)
		}
	} else {
		collectorHealth.Set("accesslog", health.StatusDisabled, "ISUTOOLS_ACCESS_LOG is not configured")
	}
	if runtime.GOOS == "linux" {
		collector := procstats.New()
		provider.Proc = collector
		if err := collector.Reset(); err != nil {
			collectorHealth.Set("proc", health.StatusDegraded, err.Error())
		} else {
			collectorHealth.Set("proc", health.StatusOK, "")
		}
	} else {
		collectorHealth.Set("proc", health.StatusDisabled, "procfs is only available on Linux")
	}
	return web.NewHandler(provider)
}

// collectAdvice gathers the advisor inputs available to this process:
// the observed DSN, a raw DB connection, nginx conf (ISUTOOLS_NGINX_CONF:
// file or directory of *.conf), the root filesystem, and GOMAXPROCS.
func collectAdvice(ctx context.Context) []advisor.Check {
	opts := advisor.Options{
		FS:         os.DirFS("/"),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
	}
	if name, dsn, ok := sqlstats.FirstConn(); ok {
		opts.DriverName, opts.DSN = name, dsn
		if db, err := sql.Open(name, dsn); err == nil {
			defer func() { _ = db.Close() }()
			db.SetMaxOpenConns(1)
			opts.DB = db
			return collectAdviceWithConf(ctx, opts)
		}
	}
	return collectAdviceWithConf(ctx, opts)
}

func collectAdviceWithConf(ctx context.Context, opts advisor.Options) []advisor.Check {
	return collectAdviceWithEnv(ctx, opts, os.Getenv)
}

func collectAdviceWithEnv(ctx context.Context, opts advisor.Options, getenv func(string) string) []advisor.Check {
	path := getenv("ISUTOOLS_PROXY_CONF")
	kind := strings.ToLower(strings.TrimSpace(getenv("ISUTOOLS_PROXY_KIND")))
	legacyNginx := false
	if path == "" {
		path = getenv("ISUTOOLS_NGINX_CONF")
		legacyNginx = path != ""
	}
	if kind == "" {
		if legacyNginx {
			kind = "nginx"
		} else {
			kind = inferProxyKindFromPath(path)
		}
	}
	if path != "" {
		config := readProxyConf(path)
		opts.Protocol.ProxyConfig = config
		opts.Protocol.ProxyKind = kind
		if kind == "nginx" {
			opts.NginxConf = config
		}
	}
	opts.Protocol.UDP443Reachable = advisor.ParseEvidence(getenv("ISUTOOLS_HTTP3_UDP443"))
	opts.Protocol.EdgeName = strings.TrimSpace(getenv("ISUTOOLS_HTTP3_EDGE"))
	opts.Protocol.EdgeHTTP3 = advisor.ParseEvidence(getenv("ISUTOOLS_HTTP3_EDGE_ENABLED"))
	return advisor.Collect(ctx, opts)
}

func readQUICTelemetryFile(path string) (*advisor.QUICTelemetry, error) {
	var telemetry advisor.QUICTelemetry
	if err := readTelemetryJSON(path, &telemetry); err != nil {
		return nil, err
	}
	return &telemetry, nil
}

func readCacheTelemetryFile(path string) (*advisor.CacheTelemetry, error) {
	var telemetry advisor.CacheTelemetry
	if err := readTelemetryJSON(path, &telemetry); err != nil {
		return nil, err
	}
	return &telemetry, nil
}

func readTelemetryJSON(path string, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	const maxTelemetryBytes = 64 << 10
	data, err := io.ReadAll(io.LimitReader(file, maxTelemetryBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxTelemetryBytes {
		return errors.New("telemetry exceeds 64 KiB")
	}
	return json.Unmarshal(data, out)
}

func inferProxyKindFromPath(path string) string {
	name := strings.ToLower(filepath.Base(path))
	switch {
	case name == "caddyfile" || strings.Contains(name, "caddy"):
		return "caddy"
	case strings.Contains(name, "envoy"):
		return "envoy"
	case strings.Contains(name, "nginx"):
		return "nginx"
	default:
		return ""
	}
}

// readProxyConf reads a config file, or concatenates *.conf files when path is
// an nginx-style directory (best effort). Caddy/Envoy should pass a file.
func readProxyConf(path string) []byte {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		data, _ := os.ReadFile(path)
		return data
	}
	var out []byte
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".conf") {
			if data, rerr := os.ReadFile(p); rerr == nil {
				out = append(out, data...)
				out = append(out, '\n')
			}
		}
		return nil
	})
	return out
}
