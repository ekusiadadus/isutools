// Command isutools-agent serves a standalone, loopback-only multi-host peer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/dbcap"
	"github.com/ekusiadadus/isutools/dbinspect"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/internal/agentconfig"
	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/multihost"
	"github.com/ekusiadadus/isutools/netstats"
	"github.com/ekusiadadus/isutools/queryplan"
	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
	_ "github.com/go-sql-driver/mysql"
)

const (
	envTargets = "ISUTOOLS_AGENT_TARGETS_FILE"
	envDataDir = "ISUTOOLS_DATA_DIR"
	envToken   = "ISUTOOLS_PEER_TOKEN"
)

type options struct {
	addr, role, dataDir, targetsFile, accessLog string
}

func main() {
	var opt options
	flag.StringVar(&opt.addr, "addr", "127.0.0.1:19192", "literal loopback listen address")
	flag.StringVar(&opt.role, "role", "host", "peer role label")
	flag.StringVar(&opt.dataDir, "data-dir", defaultString(os.Getenv(envDataDir), "./isutools-agent-data"), "agent identity data directory")
	flag.StringVar(&opt.targetsFile, "targets", os.Getenv(envTargets), "owner-only targets JSON file")
	flag.StringVar(&opt.accessLog, "accesslog", "", "optional access log path")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, opt, os.Getenv(envToken)); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func run(ctx context.Context, opt options, token string) error {
	if !literalLoopback(opt.addr) {
		return errors.New("isutools-agent: address must use a literal loopback IP")
	}
	agentID, err := agentconfig.AgentID(opt.dataDir)
	if err != nil {
		return errors.New("isutools-agent: agent identity unavailable")
	}
	targets, err := loadAndRegisterTargets(opt.targetsFile)
	if err != nil {
		return err
	}

	var rowsCollector *sqlrows.Collector
	controller, err := runctl.New(runctl.Options{Enrich: func(ctx context.Context, snapshot *runctl.Snapshot) error {
		snapshot.Sections["db-capabilities"] = dbcap.ForTargets(sqlstats.Targets(), nil)
		schemas := make(map[string]*dbinspect.Schema)
		for _, target := range sqlstats.Targets() {
			schemas[target.ID] = dbinspect.CollectRegistered(ctx, target.ID)
		}
		snapshot.Sections["dbinspect"] = schemas
		snapshot.Sections["advisor-static"] = advisor.Collect(ctx, advisor.Options{FS: os.DirFS("/"), GOMAXPROCS: runtime.GOMAXPROCS(0)})
		if rows, ok := snapshot.Sections[sqlrows.Name].(*sqlrows.Section); ok && rowsCollector != nil {
			if plans, captureErr := queryplan.Capture(ctx, queryplan.Input{Rows: rows}); captureErr == nil {
				snapshot.Sections[queryplan.Name] = plans
			}
		}
		return nil
	}})
	if err != nil {
		return errors.New("isutools-agent: controller unavailable")
	}
	defer controller.Close()

	identity := hoststats.Identity{}
	if collector, collectorErr := hoststats.New(hoststats.Options{Getenv: func(key string) string {
		if key == hoststats.EnvRole {
			return opt.role
		}
		return os.Getenv(key)
	}}); collectorErr == nil {
		if err := controller.RegisterBaseline(runctl.Registration{Name: hoststats.CollectorName}, collector); err != nil {
			return errors.New("isutools-agent: host collector registration failed")
		}
		identity = collector.Identity()
	}
	if runtime.GOOS == "linux" {
		if err := controller.RegisterBaseline(runctl.Registration{Name: netstats.Default.Name()}, netstats.Default); err != nil {
			return errors.New("isutools-agent: network collector registration failed")
		}
	}
	rowsCollector = sqlrows.New()
	if err := controller.RegisterBaseline(sqlrows.Registration(), rowsCollector); err != nil {
		return errors.New("isutools-agent: SQL row collector registration failed")
	}
	if opt.accessLog != "" {
		unmatched := accesslog.UnmatchedKeep
		if strings.EqualFold(strings.TrimSpace(os.Getenv("ISUTOOLS_ACCESS_LOG_UNMATCHED")), string(accesslog.UnmatchedCollapse)) {
			unmatched = accesslog.UnmatchedCollapse
		}
		collector := accesslog.New(opt.accessLog, accesslog.WithPathRulesSpec(os.Getenv("ISUTOOLS_ACCESS_LOG_PATH_RULES"), unmatched))
		if err := controller.RegisterGeneration(runctl.Registration{Name: accesslog.SectionName}, accesslog.NewGenerationCollector(collector)); err != nil {
			return errors.New("isutools-agent: access log collector registration failed")
		}
	}

	sections := controller.RegisteredCollectors()
	sections = append(sections, "advisor-static", "db-capabilities", "dbinspect")
	if hasTargetPurpose(targets, string(sqlstats.PurposeExplain)) {
		sections = append(sections, queryplan.Name)
	}
	peer, err := multihost.NewPeer(multihost.PeerOptions{
		Enabled: true, Token: token, Role: opt.role, Form: "agent", AgentID: agentID,
		Sections: sections, Capabilities: []string{"run-v1", "strict-dto", "lease-v1", "bounded-snapshot"},
		Identity: identity, CgroupScope: os.Getenv(hoststats.EnvCGroupScope), Targets: targets,
		Controller: controller, Snapshot: func(snapshot *runctl.Snapshot) map[string]any { return snapshot.Sections },
	})
	if err != nil {
		return errors.New("isutools-agent: peer configuration invalid")
	}
	defer peer.Close()
	listener, err := net.Listen("tcp", opt.addr)
	if err != nil {
		return fmt.Errorf("isutools-agent: listen failed: %w", err)
	}
	server := &http.Server{Handler: peer, ReadHeaderTimeout: 2 * time.Second, MaxHeaderBytes: 8 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	log.Printf("isutools-agent: listening on %s role=%s agent_id=%s", listener.Addr(), opt.role, agentID)
	select {
	case serveErr := <-done:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		<-done
		return ctx.Err()
	}
}

func hasTargetPurpose(targets []multihost.TargetSummaryDTO, purpose string) bool {
	for _, target := range targets {
		for _, candidate := range target.Purposes {
			if candidate == purpose {
				return true
			}
		}
	}
	return false
}

func loadAndRegisterTargets(path string) ([]multihost.TargetSummaryDTO, error) {
	if path == "" {
		return nil, nil
	}
	items, err := agentconfig.LoadTargets(path)
	if err != nil {
		return nil, errors.New("isutools-agent: targets file rejected")
	}
	for i, item := range items {
		if err := sqlstats.RegisterDBTarget(item.ID, item.Driver, item.DSN); err != nil {
			return nil, fmt.Errorf("isutools-agent: target %d registration failed", i)
		}
		if item.ExplainDSN != "" {
			if err := sqlstats.RegisterDBInspector(item.ID, sqlstats.PurposeExplain, item.ExplainDriver, item.ExplainDSN); err != nil {
				return nil, fmt.Errorf("isutools-agent: target %d explain registration failed", i)
			}
		}
	}
	infos := sqlstats.Targets()
	result := make([]multihost.TargetSummaryDTO, 0, len(infos))
	for _, info := range infos {
		purposes := make([]string, len(info.Purposes))
		for i, purpose := range info.Purposes {
			purposes[i] = string(purpose)
		}
		result = append(result, multihost.TargetSummaryDTO{ID: info.ID, Driver: info.Driver, Display: info.Display, Schema: info.Schema, Purposes: purposes})
	}
	return result, nil
}

func literalLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
