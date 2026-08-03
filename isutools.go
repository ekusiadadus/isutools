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
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/advisor"
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

// startAdmin starts the admin HTTP server once. Failures are logged and
// otherwise ignored: the report becomes unavailable, the app is unaffected.
func startAdmin() {
	adminOnce.Do(func() {
		addr := resolveAdminAddr(os.Getenv)
		if addr == "" {
			collectorHealth.Set("admin", health.StatusDisabled, "disabled by ISUTOOLS_ADDR")
			return
		}
		token := os.Getenv("ISUTOOLS_TOKEN")
		allowUnauthenticated := os.Getenv("ISUTOOLS_ALLOW_UNAUTHENTICATED") == "1"
		if !isLoopbackAdminAddr(addr) && token == "" && !allowUnauthenticated {
			err := errors.New("non-loopback admin bind requires ISUTOOLS_TOKEN or explicit ISUTOOLS_ALLOW_UNAUTHENTICATED=1")
			collectorHealth.Set("admin", health.StatusFailed, err.Error())
			log.Printf("isutools: admin server disabled: %v", err)
			return
		}
		unprotectedNonLoopback := !isLoopbackAdminAddr(addr) && allowUnauthenticated
		if unprotectedNonLoopback {
			message := "unauthenticated non-loopback admin bind explicitly enabled; restrict host publishing to 127.0.0.1"
			collectorHealth.Set("admin", health.StatusDegraded, message)
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
		handler, err := protectAdmin(addr, token, allowUnauthenticated, Handler())
		if err != nil {
			_ = ln.Close()
			collectorHealth.Set("admin", health.StatusFailed, err.Error())
			return
		}
		go func() {
			_ = http.Serve(ln, handler)
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

// adminCookieName carries browser sessions authenticated once via ?token=.
const adminCookieName = "isutools_token"

func protectAdmin(addr, token string, allowUnauthenticated bool, next http.Handler) (http.Handler, error) {
	if isLoopbackAdminAddr(addr) || allowUnauthenticated {
		return next, nil
	}
	if token == "" {
		return nil, errors.New("non-loopback admin bind requires authentication or explicit unauthenticated opt-in")
	}
	want := sha256.Sum256([]byte("Bearer " + token))
	matches := func(bearer string) bool {
		got := sha256.Sum256([]byte(bearer))
		return subtle.ConstantTimeCompare(got[:], want[:]) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized := matches(r.Header.Get("Authorization"))
		// Browsers cannot send Authorization headers to a plain URL, so a
		// one-time ?token= grants an HttpOnly session cookie for the UI.
		if !authorized {
			if q := r.URL.Query().Get("token"); q != "" && matches("Bearer "+q) {
				authorized = true
				http.SetCookie(w, &http.Cookie{
					Name: adminCookieName, Value: q, Path: "/", HttpOnly: true,
				})
			}
		}
		if !authorized {
			if c, err := r.Cookie(adminCookieName); err == nil && matches("Bearer "+c.Value) {
				authorized = true
			}
		}
		if !authorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="isutools"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
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
// unchanged, avoiding request-path overhead.
func HTTP(next http.Handler) http.Handler {
	if Off() {
		return next
	}
	return httpstats.Middleware(next)
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
		Advisor: collectAdvice,
	}
	collectorHealth.Set("http", health.StatusOK, "")
	if path := os.Getenv("ISUTOOLS_NGINX_LOG"); path != "" {
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
		collectorHealth.Set("accesslog", health.StatusDisabled, "ISUTOOLS_NGINX_LOG is not configured")
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
			defer db.Close()
			db.SetMaxOpenConns(1)
			opts.DB = db
			return collectAdviceWithConf(ctx, opts)
		}
	}
	return collectAdviceWithConf(ctx, opts)
}

func collectAdviceWithConf(ctx context.Context, opts advisor.Options) []advisor.Check {
	if path := os.Getenv("ISUTOOLS_NGINX_CONF"); path != "" {
		opts.NginxConf = readNginxConf(path)
	}
	return advisor.Collect(ctx, opts)
}

// readNginxConf reads a conf file, or concatenates nginx.conf and *.conf
// files when path is a directory (best effort).
func readNginxConf(path string) []byte {
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
