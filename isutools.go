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
	"log"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/ekusiadadus/isutools/dbinspect"
	"github.com/ekusiadadus/isutools/sqlstats"
	"github.com/ekusiadadus/isutools/web"
)

// defaultAdminAddr binds to loopback so the admin server is never reachable
// from outside the host unless ISUTOOLS_ADDR explicitly widens it.
const defaultAdminAddr = "127.0.0.1:19191"

var (
	adminOnce sync.Once
	adminMu   sync.Mutex
	adminBind string
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
		return name
	}
	startAdmin()
	return name + sqlstats.DriverSuffix
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
			return
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("isutools: admin listen on %s failed: %v", addr, err)
			return
		}
		adminMu.Lock()
		adminBind = ln.Addr().String()
		adminMu.Unlock()
		log.Printf("isutools: admin server on http://%s", ln.Addr())
		go func() {
			_ = http.Serve(ln, Handler())
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

// RegisterSQL wraps the named drivers ("mysql", "pgx", ...) and registers
// measuring variants under "<name>:isutools". Prefer SQLDriverName, which
// also resolves the on/off decision. No-op when disabled.
func RegisterSQL(names ...string) error {
	if Off() {
		return nil
	}
	return sqlstats.Register(names...)
}

// Handler serves the report UI: GET / (dashboard with snapshot history),
// GET /snapshot.html (download), GET /json, GET /files/<name>,
// POST /reset, POST /save. Snapshot history persists to ISUTOOLS_DATA_DIR
// when set. The DB schema is inspected through the first DSN the
// application opened, using the raw driver so inspection queries never
// appear in the SQL statistics.
func Handler() http.Handler {
	return web.NewHandler(web.Provider{
		SQL:     sqlstats.Default,
		DataDir: os.Getenv("ISUTOOLS_DATA_DIR"),
		DB: func(ctx context.Context) *dbinspect.Schema {
			name, dsn, ok := sqlstats.FirstConn()
			if !ok {
				return nil
			}
			return dbinspect.Collect(ctx, name, dsn)
		},
	})
}
