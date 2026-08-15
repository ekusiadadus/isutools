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
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	runtimepprof "runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/counters"
	"github.com/ekusiadadus/isutools/dbcap"
	"github.com/ekusiadadus/isutools/dbinspect"
	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/flowstats"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/internal/safefs"
	"github.com/ekusiadadus/isutools/internal/timeline"
	"github.com/ekusiadadus/isutools/netstats"
	"github.com/ekusiadadus/isutools/procstats"
	"github.com/ekusiadadus/isutools/queryplan"
	"github.com/ekusiadadus/isutools/redisstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/ekusiadadus/isutools/sqlrows"
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

// Off reports the immutable process-start decision for ISUTOOLS. Accepted
// hard-off spellings are off, 0, false, no, and disabled (case-insensitive).
func Off() bool { return resolvedProcessConfig().Off }

// Feature flags for the optional collectors. Every one of them defaults to
// on; the flag exists so a run can be measured with the collector removed
// entirely, which is what makes a single-feature overhead comparison (ABBA)
// possible. hoststats and sqlrows own their own flag names, so those are
// taken from the collector packages rather than repeated here.
const (
	envNetStats = "ISUTOOLS_NETSTATS"
	envDBPool   = "ISUTOOLS_DBPOOL"
)

// envDataDir names the directory snapshots and profile artifacts are published
// into. Both the transport and the ResetNow capture path read it, and they have
// to read the same one: the two halves of a profile pair are matched by the
// directory they were written to.
const envDataDir = "ISUTOOLS_DATA_DIR"

const (
	envAccessLogPathRules = "ISUTOOLS_ACCESS_LOG_PATH_RULES"
	envAccessLogUnmatched = "ISUTOOLS_ACCESS_LOG_UNMATCHED"
	envAccessLogFormat    = "ISUTOOLS_ACCESS_LOG_FORMAT"
	envFlowSource         = "ISUTOOLS_FLOW_SOURCE"
)

// Runtime profile configuration. All three default to off: the default
// configuration must add zero overhead, and a profile rate is a process-wide
// runtime setting that this package refuses to turn on behind the operator's
// back.
const (
	envMutexFraction        = "ISUTOOLS_MUTEX_FRACTION"
	envBlockRateNS          = "ISUTOOLS_BLOCK_RATE_NS"
	envHeapProfile          = "ISUTOOLS_HEAP_PROFILE"
	envAllocsProfile        = "ISUTOOLS_ALLOCS_PROFILE"
	envGoroutineProfile     = "ISUTOOLS_GOROUTINE_PROFILE"
	envThreadcreateProfile  = "ISUTOOLS_THREADCREATE_PROFILE"
	envGoroutineLeakProfile = "ISUTOOLS_GOROUTINELEAK_PROFILE"
)

// EXPLAIN capture configuration. The master flag itself lives in the
// queryplan package (queryplan.EnvFlag, default off); the two variables below
// are the single-target convenience path for the credential it runs on.
const (
	envExplainDSN    = "ISUTOOLS_EXPLAIN_DSN"
	envExplainDriver = "ISUTOOLS_EXPLAIN_DRIVER"
	// defaultExplainDriver is what ISUTOOLS_EXPLAIN_DSN is opened with when
	// ISUTOOLS_EXPLAIN_DRIVER says nothing.
	defaultExplainDriver = "mysql"
)

// Health keys this file owns.
const (
	// healthProfiles records the resolved runtime profile configuration so a
	// snapshot proves which rates were in effect while it was measured.
	healthProfiles = "profile"
	// healthExplainCredential records how the ISUTOOLS_EXPLAIN_DSN shortcut
	// resolved. It is a separate key from the capture's own notes, which reach
	// health under the collector name "queryplan": a credential that was never
	// registered and a target that was skipped are different problems with
	// different fixes.
	healthExplainCredential = "queryplan-credential"
	// healthInitializeUnserialized reports an initialize-triggered reset taken
	// outside SerializeInitialize. Such a run can still succeed, but a
	// concurrent rebuild may have polluted it, and a silently trusted
	// contaminated run is the failure the guard exists to expose.
	healthInitializeUnserialized = "initialize-unserialized"
)

// reasonInitialize marks a run opened by an application initialize handler.
// It is the value ResetNow uses, which is what lets the unserialized-guard
// check tell an initialize apart from an operator-driven reset.
const reasonInitialize = "initialize"

// StartResult is the immutable record of an opening boundary returned by
// ResetNow. It is an alias rather than a distinct type because the run
// lifecycle lives in an internal package that applications cannot import:
// without the alias a caller could read the value but never name its type.
type StartResult = runctl.StartResult

// Validity is a run's data-quality verdict, orthogonal to its state. A run
// can be perfectly finished and completely untrustworthy, so an initialize
// handler must inspect this and not only the error.
type Validity = runctl.Validity

// Validity values, re-exported for the same reason as the types above.
const (
	// ValidityValid means every collector contributed a complete interval.
	ValidityValid = runctl.ValidityValid
	// ValidityPartial means optional sections are missing but the interval is
	// usable.
	ValidityPartial = runctl.ValidityPartial
	// ValidityInvalid means the interval cannot be trusted and must not be
	// compared with other runs.
	ValidityInvalid = runctl.ValidityInvalid
)

// ErrInitializeBusy reports that SerializeInitialize could not acquire the
// process-wide initialize guard in time.
var ErrInitializeBusy = runctl.ErrInitializeBusy

var (
	measurementOnce sync.Once
	measurementCore *measurement
)

// measurement is the process-wide measurement core: one run Controller, one
// process collector, and the runtime profile configuration.
//
// It is a singleton because a measurement run is a property of the process,
// not of an HTTP handler. Handler() used to build a fresh procstats collector
// on every call, so two handlers measured two unrelated baselines and neither
// could be reconciled with a run boundary. Ownership lives here instead, and
// both Handler() and ResetNow go through it.
type measurement struct {
	ctrl *runctl.Controller
	// cpu is the sole run-aligned CPU owner. cpuBridge and the runctl observer
	// both refer to it in run mode; in fixed mode cpuBridge instead holds the
	// shared timer owner and no lifecycle observer is installed.
	cpu         cpuCoordinator
	cpuBridge   *cpuWebBridge
	cpuRoot     *safefs.Root
	cpuMode     string
	cpuDuration time.Duration
	trace       traceCoordinator
	traceBridge *traceWebBridge
	traceRoot   *safefs.Root
	recovery    *runRecoveryLedger
	timeline    *timelineRuntime
	executable  *buildinfo.ExecutableIdentity
	// proc is nil where procfs does not exist.
	proc *procstats.Collector
	// procManaged means proc is registered as a coordinated baseline collector,
	// so both ResetNow and POST /reset use the same opening and closing boundary.
	procManaged bool
	// profiles lists the runtime profiles to capture at a run boundary, in
	// capture order. Empty means profiling is off, which is the default.
	profiles []string
	// explain is the query-plan capture wired into the Controller's enrich
	// phase, or nil when ISUTOOLS_EXPLAIN is off. It is kept here so the
	// wiring can be inspected without reaching into the Controller.
	explain *explainCapture
	// boundary captures the opening half of a run's profile pair for runs this
	// package opens itself. POST /reset has its own capture inside the
	// transport; ResetNow is the other documented way a run begins, and without
	// this it would publish no opening artifact for the closing capture to be
	// differenced against. Nil when no profile is configured.
	boundary *web.BoundaryProfiler
	// generation names the collector generation a boundary artifact is filed
	// under. It is the number the transport reports as Provider.SQLGeneration,
	// so an artifact opened by ResetNow is named exactly as one opened by POST
	// /reset would have been.
	generation func() int64

	// accessLogOnce guards the late registration of the access log's
	// generation collector, which cannot happen at construction time because
	// the log collector is built by Handler.
	accessLogOnce sync.Once
	// generationManaged records which legacy dashboard collectors were
	// successfully registered with the run coordinator. POST /reset uses it to
	// avoid rotating the same generation a second time.
	generationManaged generationRegistration

	mu sync.Mutex
	// lastRunID names the newest run this process opened. It is how the
	// report finds the sections of a completed run without the transport
	// layer having to track run identity of its own.
	lastRunID string
	// healthRunID names the run whose diagnostics were already copied into the
	// health registry, so a dashboard refreshing every second does not rewrite
	// the same immutable notes on every render.
	healthRunID string
	// healthForwarded names the collectors whose registry status was last
	// written by a run forward. Only those may be cleared when the next run is
	// forwarded: a status set outside a run — a failed driver registration, for
	// one — describes the process, not the run, and survives every run.
	healthForwarded map[string]struct{}
}

// defaultMeasurement returns the process-wide core, building it once.
func defaultMeasurement() *measurement {
	measurementOnce.Do(func() {
		measurementCore = newMeasurement(os.Getenv, runctl.Options{})
	})
	return measurementCore
}

// generationCollectors are the swappable-generation collectors a run measures:
// the ones that actually carry a run's numbers, as opposed to the baseline
// collectors that carry its deltas.
//
// They are a parameter rather than a set of package globals because a
// generation collector's boundary epoch lives in the collector, not in the
// Controller. Registering one process-wide collector with two Controllers
// would make the second Controller's first boundary look stale and silently
// drop the section. Production has exactly one Controller and passes the
// process-wide collectors; a test that builds its own Controller passes its
// own.
type generationCollectors struct {
	http     runctl.GenerationCollector
	sql      runctl.GenerationCollector
	counters runctl.GenerationCollector
	redis    runctl.GenerationCollector
	flow     runctl.GenerationCollector
}

// processGenerationCollectors names the collectors the instrumented
// application actually writes to: the middleware's HTTP table, the proxied
// driver's SQL store, and the user counter registry.
func processGenerationCollectors() generationCollectors {
	return generationCollectors{
		http:     httpstats.Default,
		sql:      sqlstats.NewGenerationCollector(nil),
		counters: counters.NewGenerationCollector(nil),
		redis:    redisstats.NewGenerationCollector(nil),
		flow:     flowstats.NewGenerationCollector(nil),
	}
}

// newMeasurement builds a core from an injected environment. Tests use it to
// exercise the wiring with their own Controller and without touching the
// process-wide singleton.
func newMeasurement(getenv func(string) string, opts runctl.Options) *measurement {
	return newMeasurementWith(getenv, opts, processGenerationCollectors())
}

// newMeasurementWith is newMeasurement with an explicit generation collector
// set, so a test can drive a private Controller without advancing the epoch of
// the process-wide collectors.
func newMeasurementWith(getenv func(string) string, opts runctl.Options, gens generationCollectors) *measurement {
	mode := resolveGlobalConfig(getenv)
	if mode.Off {
		return &measurement{}
	}
	if opts.Health == nil {
		opts.Health = collectorHealth
	}
	var (
		cpu       cpuCoordinator
		cpuBridge *cpuWebBridge
		cpuRoot   *safefs.Root
		cpuMode   string
	)
	cpu, cpuBridge, cpuRoot, cpuMode = newRunCPUCoordinator(getenv)
	profiles := resolveProfileSettings(getenv).apply(collectorHealth)
	traceOwner, traceBridge, traceRoot := newRunTraceCoordinator(getenv, cpuMode, profiles)
	recovery := newRunRecoveryLedger()
	timelineRuntime := newTimelineRuntime(getenv)
	observers := joinedRunObservers{}
	if opts.LifecycleObserver != nil {
		observers = append(observers, opts.LifecycleObserver)
	}
	observers = append(observers, recovery)
	if timelineRuntime != nil {
		observers = append(observers, timelineRuntime)
	}
	if cpu != nil {
		observers = append(observers, cpuRunObserver{owner: cpu})
	}
	if traceOwner != nil {
		observers = append(observers, traceRunObserver{owner: traceOwner})
	}
	opts.LifecycleObserver = observers
	// The enrich hook has to be decided before the Controller exists, because
	// a Controller's hook is fixed at construction. Preserve an Enrich supplied
	// by the caller and compose query-plan capture after it; enabling EXPLAIN
	// must not silently disable another snapshot enrichment.
	explain := newExplainCapture(getenv, collectorHealth)
	if explain != nil {
		opts.Enrich = composeEnrich(opts.Enrich, explain.enrich)
	}
	ctrl, err := runctl.New(opts)
	if err != nil {
		// The default budget table is a compile-time constant set, so this is
		// unreachable in practice. Falling back keeps the process measurable
		// instead of panicking in a library path.
		log.Printf("isutools: run controller falling back to package defaults: %v", err)
		ctrl = runctl.Default()
		// The fallback Controller does not carry the observer installed above.
		// Keeping a managed CPU owner would create exactly the split ownership
		// this integration forbids, so disable and release it fail-closed.
		if cpu != nil {
			cpu.Close()
		}
		if cpuRoot != nil {
			_ = cpuRoot.Close()
		}
		cpu, cpuBridge, cpuRoot, cpuMode = nil, nil, nil, ""
		if timelineRuntime != nil {
			timelineRuntime.close()
			timelineRuntime = nil
		}
		if traceOwner != nil {
			traceOwner.Close()
		}
		if traceRoot != nil {
			_ = traceRoot.Close()
		}
		traceOwner, traceBridge, traceRoot = nil, nil, nil
	}
	m := &measurement{
		ctrl:        ctrl,
		cpu:         cpu,
		cpuBridge:   cpuBridge,
		cpuRoot:     cpuRoot,
		cpuMode:     cpuMode,
		cpuDuration: pprofDuration(getenv),
		trace:       traceOwner,
		traceBridge: traceBridge,
		traceRoot:   traceRoot,
		recovery:    recovery,
		timeline:    timelineRuntime,
		proc:        newProcCollector(),
		explain:     explain,
		generation:  sqlstats.Default.CurrentGeneration,
	}
	baselines := registerCollectorsStatus(ctrl, getenv)
	m.procManaged = registerProcCollector(ctrl, m.proc)
	m.generationManaged = registerGenerationCollectorsStatus(ctrl, gens)
	if m.timeline != nil {
		var pools *dbpool.Collector
		if featureEnabled(getenv, envDBPool) {
			pools = dbpool.Default
		}
		m.timeline.setSources(m.proc, baselines.host, pools)
		if collector, ok := gens.http.(*httpstats.Collector); ok {
			rules, err := httpstats.ParseSafeProfileRouteRules(getenv(envTimelineSafeRoutes))
			if err != nil {
				collectorHealth.Set(healthTimeline, health.StatusDegraded, err.Error()+"; unmatched routes only")
				rules = nil
			}
			collector.SetEventRouteRules(rules)
			collector.SetEventObserver(m.timeline.collector)
		}
		if collector, ok := gens.sql.(interface{ SetEventObserver(sqlstats.EventObserver) }); ok {
			collector.SetEventObserver(m.timeline.collector)
		}
	}
	m.profiles = profiles
	if cpu != nil || traceOwner != nil || len(m.profiles) != 0 || getenv(envProfileAnalysis) == "1" {
		identity, err := buildinfo.CaptureExecutable()
		m.executable = &identity
		if err != nil {
			collectorHealth.Set("profile-provenance", health.StatusDegraded, err.Error())
		} else if identity.Source == buildinfo.SourceProcSelfExe {
			collectorHealth.Set("profile-provenance", health.StatusOK, "")
		} else {
			collectorHealth.Set("profile-provenance", health.StatusDegraded, "running image hash is path-unbound on this platform")
		}
	}
	// The capturer is built from the environment rather than from Handler(), so
	// an application that opens its runs through ResetNow still publishes the
	// opening half of every pair when the admin server is disabled.
	m.boundary = web.NewBoundaryProfiler(web.Provider{
		DataDir:         getenv(envDataDir),
		RuntimeProfiles: m.profiles,
		// The same registry the effective profile rates were recorded in, and
		// the one Handler() hands the transport: a capture verdict has to reach
		// the operator through one health section, not two.
		Health: collectorHealth,
	})
	return m
}

func registerProcCollector(ctrl *runctl.Controller, collector *procstats.Collector) bool {
	if collector == nil {
		collectorHealth.Set(procstats.CollectorName, health.StatusDisabled, "procfs is only available on Linux")
		return false
	}
	err := ctrl.RegisterBaseline(runctl.Registration{Name: procstats.CollectorName}, collector)
	if err != nil {
		collectorHealth.Set(procstats.CollectorName, health.StatusFailed, err.Error())
		log.Printf("isutools: %s collector not registered: %v", procstats.CollectorName, err)
		return false
	}
	collectorHealth.Set(procstats.CollectorName, health.StatusOK, "")
	return true
}

// composeEnrich combines independent snapshot enrichments. Both run even if
// the first fails so one optional feature cannot suppress another; errors.Join
// preserves every failure for the coordinator's validity and health verdict.
func composeEnrich(
	first, second func(context.Context, *runctl.Snapshot) error,
) func(context.Context, *runctl.Snapshot) error {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return func(ctx context.Context, snapshot *runctl.Snapshot) error {
		firstErr := first(ctx, snapshot)
		secondErr := second(ctx, snapshot)
		return errors.Join(firstErr, secondErr)
	}
}

// newProcCollector returns the process collector, or nil on a host without
// procfs. Returning nil rather than a collector that fails every read keeps
// the "disabled" and "broken" cases distinguishable in health.
func newProcCollector() *procstats.Collector {
	if runtime.GOOS != "linux" {
		return nil
	}
	return procstats.New(procstats.WithTrackedPIDs(os.Getpid()))
}

// registerCollectors wires the optional baseline collectors into the run
// Controller, each behind its own feature flag, and returns the names it
// registered in registration order.
//
// A disabled or unsupported collector is not registered at all rather than
// registered and skipped: an unregistered collector consumes no phase budget
// and appears in no boundary record, which is what makes "measure without
// this collector" a real comparison instead of an approximation.
func registerCollectors(ctrl *runctl.Controller, getenv func(string) string) []string {
	return registerCollectorsStatus(ctrl, getenv).names
}

type baselineRegistration struct {
	names []string
	host  *hoststats.Collector
}

func registerCollectorsStatus(ctrl *runctl.Controller, getenv func(string) string) baselineRegistration {
	var registered []string
	var host *hoststats.Collector
	add := func(reg runctl.Registration, coll runctl.BaselineCollector) {
		if err := ctrl.RegisterBaseline(reg, coll); err != nil {
			collectorHealth.Set(reg.Name, health.StatusFailed, err.Error())
			log.Printf("isutools: %s collector not registered: %v", reg.Name, err)
			return
		}
		collectorHealth.Set(reg.Name, health.StatusOK, "")
		registered = append(registered, reg.Name)
	}

	if hoststats.Enabled(getenv) {
		// New reports ErrUnsupportedOS where there is no procfs to read, so a
		// non-Linux host declines the registration instead of dragging every
		// run to partial with a collector that can never succeed.
		collector, err := hoststats.New(hoststats.Options{Getenv: getenv})
		if err != nil {
			collectorHealth.Set(hoststats.CollectorName, health.StatusDisabled, err.Error())
		} else {
			add(runctl.Registration{Name: hoststats.CollectorName}, collector)
			host = collector
		}
	} else {
		collectorHealth.Set(hoststats.CollectorName, health.StatusDisabled, hoststats.EnvEnable+" is off")
	}

	networkName, nameErr := safeCollectorName(fallbackNetworkName, netstats.Default.Name)
	switch {
	case nameErr != nil:
		collectorHealth.Set(networkName, health.StatusFailed, nameErr.Error())
		log.Printf("isutools: %s collector not registered: %v", networkName, nameErr)
	case !featureEnabled(getenv, envNetStats):
		collectorHealth.Set(networkName, health.StatusDisabled, envNetStats+" is off")
	case runtime.GOOS != "linux":
		collectorHealth.Set(networkName, health.StatusDisabled, "/proc/net is only available on Linux")
	default:
		add(runctl.Registration{Name: networkName}, netstats.Default)
	}

	if featureEnabled(getenv, sqlrows.EnvFlag) {
		add(sqlrows.Registration(), sqlrows.New())
	} else {
		collectorHealth.Set(sqlrows.Name, health.StatusDisabled, sqlrows.EnvFlag+" is off")
	}

	// dbpool is registered even with an empty watch set: WatchDBPool can be
	// called long after startup, and a pool watched later must land in a
	// collector the Controller already knows about. A run with nothing
	// watched simply reports no section.
	if featureEnabled(getenv, envDBPool) {
		add(runctl.Registration{Name: dbpool.Name}, dbpool.Default)
	} else {
		collectorHealth.Set(dbpool.Name, health.StatusDisabled, envDBPool+" is off")
	}
	return baselineRegistration{names: registered, host: host}
}

// registerGenerationCollectors wires the swappable-generation collectors into
// the Controller and returns the names it registered.
//
// Without them a run has a lifecycle but no content: the boundary records
// would show a clean start and finish while every number a user came for —
// HTTP latencies, SQL statistics, counters — stayed in a live collector nobody
// ever froze.
//
// None of them is Required. A failed boundary degrades the run to partial
// rather than invalidating it, because the sections that did succeed are still
// real measurements and throwing them away would help nobody.
func registerGenerationCollectors(ctrl *runctl.Controller, gens generationCollectors) []string {
	return registerGenerationCollectorsStatus(ctrl, gens).names
}

type generationRegistration struct {
	names                           []string
	http, sql, counter, redis, flow bool
}

func registerGenerationCollectorsStatus(ctrl *runctl.Controller, gens generationCollectors) generationRegistration {
	var registered []string
	add := func(fallback string, coll runctl.GenerationCollector) bool {
		if coll == nil {
			return false
		}
		name, err := safeCollectorName(fallback, coll.Name)
		if err == nil {
			err = ctrl.RegisterGeneration(runctl.Registration{Name: name}, coll)
		}
		if err != nil {
			// Health is set only on failure: a successful registration must not
			// overwrite a status another wiring step already recorded for the
			// same key, such as a failed SQL driver registration under "sql".
			collectorHealth.Set(name, health.StatusFailed, err.Error())
			log.Printf("isutools: %s generation collector not registered: %v", name, err)
			return false
		}
		registered = append(registered, name)
		return true
	}
	// The fallback is the name the wiring expects this slot to carry. It names
	// the health key when the collector cannot report a name of its own,
	// because a failure recorded under no key at all is a failure nobody sees.
	status := generationRegistration{}
	status.http = add(httpstats.CollectorName, gens.http)
	status.sql = add(sqlstats.SectionName, gens.sql)
	status.counter = add(counters.SectionName, gens.counters)
	status.redis = add(redisstats.SectionName, gens.redis)
	status.flow = add(flowstats.SectionName, gens.flow)
	status.names = registered
	return status
}

// watchAccessLogGeneration registers the access log's generation collector and
// reports whether this collector ended up under the coordinator's control.
//
// It cannot happen at construction time: the log collector is built by
// Handler from ISUTOOLS_ACCESS_LOG, and an adapter wrapped around no collector
// would fail every boundary and drag every run to partial for a feature the
// operator never turned on. Registering late is safe because the Controller
// reads its registration table once per phase.
//
// The return value is what the transport needs to know: a managed collector's
// aggregate is cut and sealed by BeginBoundary → Drain → Release, so POST
// /reset must not also re-baseline it. Registration happens at most once per
// process, so a second Handler() built around a second log collector is told
// "not managed" — which is exactly right, since nothing is rotating that
// collector's generations for it.
func (m *measurement) watchAccessLogGeneration(collector *accesslog.Collector) bool {
	if collector == nil || Off() {
		return false
	}
	managed := false
	m.accessLogOnce.Do(func() {
		gen := accesslog.NewGenerationCollector(collector)
		name, err := safeCollectorName(accesslog.SectionName, gen.Name)
		if err == nil {
			err = m.ctrl.RegisterGeneration(runctl.Registration{Name: name}, gen)
		}
		if err != nil {
			collectorHealth.Set(name, health.StatusDegraded, err.Error())
			log.Printf("isutools: accesslog generation collector not registered: %v", err)
			return
		}
		managed = true
	})
	return managed
}

// fallbackNetworkName is the health key the network collector's registration
// falls back to when the collector cannot report its own name. netstats
// exports no name constant, so the wiring's expectation is spelled here.
const fallbackNetworkName = "network"

// panicNameTextMax bounds the recorded rendering of a panic value. It is
// copied into a health message an operator reads, so it stays one short line
// and never carries a stack.
const panicNameTextMax = 160

// safeCollectorName resolves a collector's own name behind a panic barrier.
//
// Name is collector code like any other, and registration runs inside the
// measured application's startup: a collector that panics while introducing
// itself must fail its own registration, not the process. The Controller
// already puts Budget() behind runctl's safe-call barrier for exactly this
// reason; that helper is unexported, so the same shape is repeated here.
//
// The fallback is the name the wiring expected. It is returned alongside the
// error so the caller still has a health key to record the failure under —
// a collector that panicked in Name gave us nothing else to key on.
func safeCollectorName(fallback string, name func() string) (resolved string, err error) {
	defer func() {
		if r := recover(); r != nil {
			resolved = fallback
			err = fmt.Errorf("collector %s panicked in Name: %s", fallback, shortPanicText(r))
		}
	}()
	return name(), nil
}

// shortPanicText renders a recovered value as a single short line.
//
// Rendering runs under its own recover because the panic value may be a type
// whose String or Error method panics in turn, and a second panic inside the
// barrier would defeat the entire point of having one.
func shortPanicText(v any) (text string) {
	defer func() {
		if recover() != nil {
			text = "unprintable panic value"
		}
	}()
	text = strings.Join(strings.Fields(fmt.Sprint(v)), " ")
	if text == "" {
		return "unprintable panic value"
	}
	if r := []rune(text); len(r) > panicNameTextMax {
		return string(r[:panicNameTextMax]) + "..."
	}
	return text
}

// featureEnabled reports whether a default-on feature flag leaves its feature
// on. The accepted spellings match the collector packages' own parsers so one
// flag cannot mean two different things depending on who reads it.
func featureEnabled(getenv func(string) string, key string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(key))) {
	case "off", "0", "false", "no", "disabled":
		return false
	}
	return true
}

// profileSettings is the parsed runtime profile configuration. Each rate
// carries an explicit "was it configured at all" bit, because an unset
// variable and an explicit zero mean different things: unset must leave a
// rate the application set for itself completely alone, while an explicit
// zero is an operator asking for the profile to be off.
type profileSettings struct {
	mutexSet      bool
	mutex         int
	blockSet      bool
	block         int
	heap          bool
	allocs        bool
	goroutine     bool
	threadcreate  bool
	goroutineleak bool
	// invalid lists variables whose value could not be parsed. They are
	// reported as degraded health and otherwise ignored (fail-open).
	invalid []string
}

// resolveProfileSettings parses the three profile variables. It performs no
// runtime calls, so it is safe to evaluate anywhere.
func resolveProfileSettings(getenv func(string) string) profileSettings {
	var s profileSettings
	parse := func(key string) (int, bool) {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return 0, false
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			s.invalid = append(s.invalid, key+"="+raw)
			return 0, false
		}
		return value, true
	}
	s.mutex, s.mutexSet = parse(envMutexFraction)
	s.block, s.blockSet = parse(envBlockRateNS)
	switch strings.ToLower(strings.TrimSpace(getenv(envHeapProfile))) {
	case "1", "true", "on", "yes", "enabled":
		s.heap = true
	}
	for key, target := range map[string]*bool{
		envAllocsProfile: &s.allocs, envGoroutineProfile: &s.goroutine,
		envThreadcreateProfile: &s.threadcreate, envGoroutineLeakProfile: &s.goroutineleak,
	} {
		raw := strings.TrimSpace(getenv(key))
		switch strings.ToLower(raw) {
		case "":
		case "1", "true", "on", "yes", "enabled":
			*target = true
		case "0", "false", "off", "no", "disabled":
		default:
			s.invalid = append(s.invalid, key+"="+raw)
		}
	}
	return s
}

// apply installs the configured rates and returns the profile kinds worth
// capturing, in capture order (cheapest first, so a large heap write cannot
// delay the moment the mutex profile is taken).
//
// An unset variable never reaches a runtime setter: the application may have
// configured a rate for its own reasons, and silently overriding it would
// change the very behaviour the profile is meant to describe.
func (s profileSettings) apply(reg *health.Registry) []string {
	if s.mutexSet {
		runtime.SetMutexProfileFraction(s.mutex)
	}
	if s.blockSet {
		runtime.SetBlockProfileRate(s.block)
	}
	// A negative argument reads the rate without changing it, so this reports
	// the effective fraction whether it came from the environment or from the
	// application itself. The runtime exposes no equivalent reader for the
	// block rate, so that one is known only when we set it.
	effectiveMutex := runtime.SetMutexProfileFraction(-1)

	var kinds []string
	if effectiveMutex > 0 {
		kinds = append(kinds, "mutex")
	}
	if s.blockSet && s.block > 0 {
		kinds = append(kinds, "block")
	}
	if s.heap {
		kinds = append(kinds, "heap")
	}
	for _, configured := range []struct {
		kind    string
		enabled bool
	}{
		{"allocs", s.allocs}, {"goroutine", s.goroutine}, {"threadcreate", s.threadcreate}, {"goroutineleak", s.goroutineleak},
	} {
		if !configured.enabled {
			continue
		}
		if runtimepprof.Lookup(configured.kind) == nil {
			s.invalid = append(s.invalid, configured.kind+"=unsupported")
			continue
		}
		kinds = append(kinds, configured.kind)
	}
	if reg != nil {
		reg.Set(healthProfiles, s.status(kinds), s.message(effectiveMutex, kinds))
	}
	return kinds
}

// status is degraded when a variable was unparseable, disabled when nothing
// is captured, ok otherwise.
func (s profileSettings) status(kinds []string) health.Status {
	switch {
	case len(s.invalid) > 0:
		return health.StatusDegraded
	case len(kinds) == 0:
		return health.StatusDisabled
	default:
		return health.StatusOK
	}
}

// message describes the effective configuration in one line so the snapshot
// records the conditions the run was measured under.
func (s profileSettings) message(effectiveMutex int, kinds []string) string {
	parts := []string{
		"mutex=" + rateText(effectiveMutex > 0, effectiveMutex),
		"block=" + rateText(s.blockSet && s.block > 0, s.block),
		"heap=" + rateText(s.heap, 1),
		"allocs=" + rateText(s.allocs, 1),
		"goroutine=" + rateText(s.goroutine, 1),
		"threadcreate=" + rateText(s.threadcreate, 1),
		"goroutineleak=" + rateText(s.goroutineleak, 1),
	}
	if len(kinds) == 0 {
		parts = append(parts, "no runtime profile is captured")
	}
	if len(s.invalid) > 0 {
		parts = append(parts, "ignored invalid values: "+strings.Join(s.invalid, ", "))
	}
	return strings.Join(parts, " ")
}

// rateText renders a rate as "off" or its value.
func rateText(on bool, value int) string {
	if !on {
		return "off"
	}
	return strconv.Itoa(value)
}

// explainCapture is the query-plan capture wired into the run coordinator's
// enrich phase.
//
// The phase is the whole point. EXPLAIN issues statements against the measured
// database, so it runs exactly once per run — in the finishing worker, after
// the interval has been collected and before the snapshot is published, inside
// runctl.EnrichBudget. It is deliberately not reachable from a dashboard GET,
// which would put an EXPLAIN on the database every time somebody opened the
// report, nor from POST /collect, which is a non-terminal flush with no
// interval to rank digests by.
type explainCapture struct {
	getenv func(string) string
	// inspect overrides the registry entry point queryplan reaches targets
	// through. Nil means sqlstats.Inspect, which is what production uses; a
	// test supplies its own so a capture can be observed without a database.
	inspect queryplan.InspectFunc
	health  *health.Registry
	// targets and register reach the DB target registry. They are fields
	// rather than direct calls so the credential path can be exercised
	// without registering a process-wide target that every other test in the
	// binary would then see.
	targets  func() []sqlstats.TargetInfo
	register func(targetID string, purpose sqlstats.Purpose, driverName, dsn string) error
}

// newExplainCapture returns the enrich hook, or nil when EXPLAIN capture is
// off — which is the default, because the feature adds statements to the
// database being measured.
func newExplainCapture(getenv func(string) string, reg *health.Registry) *explainCapture {
	if !explainEnabled(getenv) {
		return nil
	}
	return &explainCapture{
		getenv:   getenv,
		health:   reg,
		targets:  sqlstats.Targets,
		register: sqlstats.RegisterDBInspector,
	}
}

// explainEnabled reports whether ISUTOOLS_EXPLAIN turns the feature on.
//
// It mirrors queryplan.Enabled, which reads the process environment directly,
// so that the wiring can be driven from an injected environment like every
// other flag here. TestExplainEnabledAgreesWithQueryplan pins the two to the
// same vocabulary.
func explainEnabled(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(queryplan.EnvFlag))) {
	case "1", "on", "true", "yes", "enabled":
		return true
	default:
		return false
	}
}

// enrich captures the run's query plans and attaches them to its snapshot.
//
// It never fails a run. A returned error would degrade the run's verdict, and
// EXPLAIN is an optional extra: a target that could not be explained comes
// back as a reason ID inside the section, which reaches the operator through
// health without claiming the interval itself is worth less.
func (e *explainCapture) enrich(ctx context.Context, snapshot *runctl.Snapshot) error {
	if snapshot == nil || snapshot.Sections == nil {
		return nil
	}
	rows, ok := snapshot.Sections[sqlrows.Name].(*sqlrows.Section)
	if !ok || rows == nil {
		// Without the interval there is nothing to rank digests by, and no
		// database clock to judge a sample's freshness against. Reading either
		// back here would produce a second opinion that could disagree with
		// the numbers shown next to the plans.
		return nil
	}
	e.registerCredential()
	section, err := queryplan.Capture(ctx, queryplan.Input{Rows: rows, Inspect: e.inspect})
	if err != nil || section == nil {
		return nil
	}
	snapshot.Sections[queryplan.Name] = section
	return nil
}

// registerCredential applies the ISUTOOLS_EXPLAIN_DSN shortcut.
//
// It is the convenience path for the common single-database setup only. With
// two targets registered there is no way to tell which database the DSN
// belongs to, and guessing would point a credential at the wrong server, so
// the variable is refused and the operator is told to call
// RegisterDBInspector(id, PurposeExplain, ...) per target instead.
//
// It runs here rather than at startup because a target only exists once it has
// been declared or observed, which for an application that never calls
// RegisterDBTarget is the first query it makes. Re-registering is harmless:
// the registry answers a repeat with ErrDuplicatePurpose, which is the
// steady state after the first run.
func (e *explainCapture) registerCredential() {
	dsn := strings.TrimSpace(e.getenv(envExplainDSN))
	if dsn == "" {
		return
	}
	targets := e.targets()
	if len(targets) != 1 {
		e.note(health.StatusDegraded, fmt.Sprintf(
			"%s は登録済み target がちょうど 1 つのときだけ有効です(現在 %d 個)。"+
				"target ごとに RegisterDBInspector(id, PurposeExplain, ...) を呼んでください",
			envExplainDSN, len(targets)))
		return
	}
	driverName := strings.TrimSpace(e.getenv(envExplainDriver))
	if driverName == "" {
		driverName = defaultExplainDriver
	}
	err := e.register(targets[0].ID, sqlstats.PurposeExplain, driverName, dsn)
	if err != nil && !errors.Is(err, sqlstats.ErrDuplicatePurpose) {
		e.note(health.StatusDegraded, fmt.Sprintf("%s を target %q に登録できませんでした: %s",
			envExplainDSN, targets[0].ID, explainRegisterReason(err)))
		return
	}
	e.note(health.StatusOK, fmt.Sprintf("%s を target %q の EXPLAIN 用 credential として登録しました",
		envExplainDSN, targets[0].ID))
}

// explainRegisterReason renders a registration failure from a fixed table.
//
// The driver's own error is never forwarded. A DSN carries a password, drivers
// routinely echo the DSN they were handed, and this string is published in a
// snapshot's health section.
func explainRegisterReason(err error) string {
	switch {
	case errors.Is(err, sqlstats.ErrUnknownDriver):
		return envExplainDriver + " が database/sql に登録されていないドライバ名です"
	case errors.Is(err, sqlstats.ErrUnparsedDSN):
		return "DSN を解釈できませんでした(接続衛生を適用できないため登録しません)"
	case errors.Is(err, sqlstats.ErrUnknownTarget):
		return "target が登録されていません"
	default:
		return "登録に失敗しました"
	}
}

// note records the credential verdict under this feature's own health key.
func (e *explainCapture) note(status health.Status, message string) {
	if e.health == nil {
		return
	}
	e.health.Set(healthExplainCredential, status, message)
}

// ResetNow opens a new measurement run immediately, preempting one already in
// flight so that the last initialize deterministically wins.
//
// It is the initialize contract, and both halves of it matter:
//
//   - Call it BEFORE sending the initialize response. The benchmarker starts
//     loading the moment it sees the response, and a boundary taken after that
//     silently drops the opening seconds of the run.
//   - Treat a failure as a failure. If it returns an error, or a Validity of
//     ValidityInvalid, the handler should answer 500 rather than measure a run
//     it already knows is contaminated: an authoritative-looking wrong number
//     is worse than a missing one.
//
// Taking the boundary is not by itself enough. It serializes only the instant
// of the switch, so a second initialize rebuilding the database afterwards
// still pollutes this run. Wrap the whole handler in SerializeInitialize; a
// run opened with Reason "initialize" outside that guard is recorded as
// degraded health rather than silently trusted.
//
// When measurement is disabled (ISUTOOLS=off) it reports a zero StartResult
// and no error, so an initialize handler needs no build tags or branches.
func ResetNow(ctx context.Context) (StartResult, error) {
	return ResetNowOpts(ctx, runctl.StartRunOptions{
		Preempt: true,
		Reason:  reasonInitialize,
		Trigger: "api",
	})
}

// ResetNowWithNonce is ResetNow with a caller-supplied idempotency key.
// Repeating a call with the same nonce replays the original StartResult
// instead of opening a second run, which is what makes a retried initialize
// request safe.
func ResetNowWithNonce(ctx context.Context, nonce string) (StartResult, error) {
	return ResetNowOpts(ctx, runctl.StartRunOptions{
		Nonce:   nonce,
		Preempt: true,
		Reason:  reasonInitialize,
		Trigger: "api",
	})
}

// ResetNowOpts is ResetNow with explicit options, for callers that need a
// non-preempting start or a different trigger. Note that the zero options
// value does NOT preempt, so a start that collides with a run already in
// flight reports runctl.ErrRunActive instead of winning.
func ResetNowOpts(ctx context.Context, o runctl.StartRunOptions) (StartResult, error) {
	if Off() {
		return StartResult{}, nil
	}
	return defaultMeasurement().resetNow(ctx, o)
}

// resetNow opens the run and captures the opening half of its profile pair.
//
// The capture belongs to this path and not to startRun, which POST /reset also
// goes through: the transport takes its own opening capture immediately before
// the 204, under the generation it just rotated, and a capture taken earlier in
// the shared path would claim the point first and file both halves of the pair
// under a generation the transport never used.
func (m *measurement) resetNow(ctx context.Context, o runctl.StartRunOptions) (StartResult, error) {
	result, err := m.startRun(ctx, o)
	if err != nil {
		return result, err
	}
	if result.State != runctl.StateStarted ||
		(result.Validity != runctl.ValidityValid && result.Validity != runctl.ValidityPartial) {
		return result, nil
	}
	m.captureOpenProfiles(result)
	if m.cpuBridge != nil && m.cpuMode == "run" {
		cpuStart := m.cpuBridge.StartRun(ctx, web.CPUStartRequest{
			RunID: result.RunID, Epoch: uint64(result.Epoch), State: string(result.State), Validity: string(result.Validity),
			BoundaryStart: cpuStartBoundary(result), GenerationWindow: webBoundaryWindow(result.GenerationWindow), BoundaryWindow: webBoundaryWindow(result.BoundaryWindow),
		})
		result.CPUProfileStart = &runctl.ProfileStartEvidence{CaptureID: cpuStart.CaptureID, State: cpuStart.State, Code: cpuStart.Code}
		noteCPUStartHealth(cpuStart)
	} else if m.cpuBridge != nil && m.cpuMode == "fixed" {
		cpuStart := m.cpuBridge.StartFixed(ctx, web.FixedCPUStartRequest{
			RunID: result.RunID, Epoch: uint64(result.Epoch), State: string(result.State), Validity: string(result.Validity),
			Generation: m.generation(), Duration: m.cpuDuration, RequestedAt: time.Now(),
			GenerationWindow: webBoundaryWindow(result.GenerationWindow), BoundaryWindow: webBoundaryWindow(result.BoundaryWindow),
		})
		result.CPUProfileStart = &runctl.ProfileStartEvidence{CaptureID: cpuStart.CaptureID, State: cpuStart.State, Code: cpuStart.Code}
		noteCPUStartHealth(cpuStart)
	}
	if m.traceBridge != nil {
		traceStart := m.traceBridge.StartRun(web.TraceStartRequest{
			RunID: result.RunID, Epoch: uint64(result.Epoch), State: string(result.State), Validity: string(result.Validity), BoundaryAt: cpuStartBoundary(result),
		})
		result.TraceStart = &runctl.ProfileStartEvidence{CaptureID: traceStart.CaptureID, State: traceStart.State, Code: traceStart.Code}
	}
	return result, nil
}

func noteCPUStartHealth(result web.CPUStartResult) {
	if result.State == "capturing" || result.State == "replayed" {
		collectorHealth.Set("profile-cpu", health.StatusOK, "")
		return
	}
	code := result.Code
	if code == "" {
		code = "start-unavailable"
	}
	collectorHealth.Set("profile-cpu", health.StatusDegraded, code)
}

func cpuStartBoundary(result runctl.StartResult) time.Time {
	if !result.GenerationWindow.Max.IsZero() {
		return result.GenerationWindow.Max
	}
	return result.StartedAt
}

// captureOpenProfiles writes the opening half of the pair for a run this
// package opened itself.
//
// It runs synchronously, before ResetNow returns and therefore before the
// initialize response releases the benchmarker: a capture taken after that
// would already contain the load it is supposed to precede. Nothing it does can
// fail the run — a lost profile must never cost a measurement.
func (m *measurement) captureOpenProfiles(result runctl.StartResult) {
	if result.RunID == "" {
		return
	}
	m.boundary.CaptureOpen(webRunStart(result), m.generation())
}

// startRun is the shared body of the ResetNow family.
func (m *measurement) startRun(ctx context.Context, o runctl.StartRunOptions) (StartResult, error) {
	if o.Reason == reasonInitialize && !runctl.HasInitializeGuard(ctx) {
		collectorHealth.Set(healthInitializeUnserialized, health.StatusDegraded,
			"initialize reset taken outside SerializeInitialize; a concurrent rebuild may have polluted this run")
	}
	result, err := m.ctrl.StartRun(ctx, o)
	if err != nil {
		return result, fmt.Errorf("isutools: starting a measurement run: %w", err)
	}
	m.mu.Lock()
	m.lastRunID = result.RunID
	m.mu.Unlock()
	m.recovery.RecordStart(result)
	if m.timeline != nil {
		m.timeline.start(result)
	}
	return result, nil
}

// currentRunID names the newest run this process opened.
func (m *measurement) currentRunID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRunID
}

// latestSections returns the collector sections of the newest run that has
// produced a snapshot, keyed by collector name. It reports nil while the run
// is still in flight, which is the normal case for a report rendered during
// the benchmark.
func (m *measurement) latestSections() map[string]any {
	run := m.latestRunSnapshot()
	if run == nil {
		return nil
	}
	return run.Sections
}

// latestRunSnapshot returns the newest completed run with the lifecycle
// evidence the web report needs to prove its sections belong to one boundary.
func (m *measurement) latestRunSnapshot() *web.RunSnapshot {
	runID := m.currentRunID()
	if runID == "" {
		return nil
	}
	snapshot, err := m.ctrl.SnapshotOf(runID)
	if err != nil || snapshot == nil {
		return nil
	}
	m.forwardRunHealth(runID, snapshot)
	return webRunSnapshot(snapshot)
}

func webRunSnapshot(snapshot *runctl.Snapshot) *web.RunSnapshot {
	if snapshot == nil {
		return nil
	}
	collectors := make([]web.RunCollectorBoundary, 0, len(snapshot.Collectors))
	for _, boundary := range snapshot.Collectors {
		collectors = append(collectors, web.RunCollectorBoundary{
			Name:      boundary.Name,
			Kind:      boundary.Kind,
			Required:  boundary.Required,
			Phase:     string(boundary.Phase),
			At:        boundary.At,
			Committed: boundary.Committed,
			Code:      boundary.Code,
			Err:       boundary.Err,
			Dropped:   boundary.Dropped,
		})
	}
	return &web.RunSnapshot{
		Info: web.RunInfo{
			RunID:            snapshot.RunID,
			Epoch:            uint64(snapshot.Epoch),
			Validity:         string(snapshot.Validity),
			Trigger:          snapshot.Trigger,
			Collectors:       collectors,
			GenerationWindow: webBoundaryWindow(snapshot.GenerationWindow),
			BoundaryWindow:   webBoundaryWindow(snapshot.BoundaryWindow),
			StartedAt:        snapshot.StartedAt,
			FinishedAt:       snapshot.FinishedAt,
		},
		Sections: snapshot.Sections,
	}
}

func webBoundaryWindow(window runctl.BoundaryWindow) web.BoundaryWindow {
	return web.BoundaryWindow{Min: window.Min, Max: window.Max, Spread: window.Spread}
}

// finishAwaitBudget bounds the wait for a finished run's snapshot. It is the
// Controller's finish lease plus a margin: past the lease the watchdog aborts
// the worker, so waiting any longer could only ever return "aborted".
const finishAwaitBudget = 25 * time.Second

// finishResetRun fixes the closing boundary of the run in flight. POST /finish
// calls it and answers as soon as the boundary exists, because a benchmark
// driver has to be released the instant measurement stops: making it wait for
// the snapshot would put snapshot-building time inside the measured window of
// whatever runs next.
func (m *measurement) finishResetRun(ctx context.Context) (web.RunFinish, error) {
	runID := m.currentRunID()
	if runID == "" {
		return web.RunFinish{}, fmt.Errorf("isutools: no measurement run is in flight: %w", runctl.ErrUnknownRun)
	}
	accepted, err := m.ctrl.FinishRun(ctx, runID)
	if err != nil {
		return web.RunFinish{}, fmt.Errorf("isutools: finishing run %s: %w", runID, err)
	}
	return web.RunFinish{
		RunID:            accepted.RunID,
		Epoch:            uint64(accepted.Epoch),
		Validity:         string(accepted.Validity),
		AcceptedAt:       accepted.AcceptedAt,
		GenerationWindow: webBoundaryWindow(accepted.GenerationWindow),
		BoundaryWindow:   webBoundaryWindow(accepted.BoundaryWindow),
	}, nil
}

// completeResetRun ends the run in flight, waits for its immutable snapshot
// and acknowledges it. POST /save calls it so the persisted report describes
// the interval the run measured.
//
// "No run" is not an error. The reset → bench → collect → save loop predates
// the run lifecycle, and a process that never opened a run still has a report
// worth saving; the same goes for a run that was already abandoned, whose
// remaining live measurements are real even though its bookkeeping is not.
func (m *measurement) completeResetRun(ctx context.Context) (web.RunFinish, error) {
	runID := m.currentRunID()
	if runID == "" {
		return web.RunFinish{}, nil
	}
	accepted, err := m.ctrl.FinishRun(ctx, runID)
	if err != nil {
		if errors.Is(err, runctl.ErrUnknownRun) || errors.Is(err, runctl.ErrRunAborted) {
			if recovered, ok := m.recovery.RecoverStartedTTL(runID); ok {
				return recovered, nil
			}
			return web.RunFinish{}, nil
		}
		return web.RunFinish{}, fmt.Errorf("isutools: finishing run %s: %w", runID, err)
	}

	awaitCtx, cancel := context.WithTimeout(ctx, finishAwaitBudget)
	defer cancel()
	status, err := m.ctrl.Await(awaitCtx, runID)
	if err != nil {
		return web.RunFinish{}, fmt.Errorf("isutools: waiting for the snapshot of run %s: %w", runID, err)
	}
	// The snapshot is handed over here, which is what "save" means. An abort
	// that beat us to it leaves nothing to acknowledge and is not a failure of
	// the save.
	if err := m.ctrl.AckBy(runID, runctl.AckedBySave); err != nil && !errors.Is(err, runctl.ErrRunAborted) {
		return web.RunFinish{}, fmt.Errorf("isutools: acknowledging run %s: %w", runID, err)
	}
	return web.RunFinish{
		RunID:            runID,
		Epoch:            uint64(accepted.Epoch),
		Validity:         string(status.Validity),
		AcceptedAt:       accepted.AcceptedAt,
		GenerationWindow: webBoundaryWindow(accepted.GenerationWindow),
		BoundaryWindow:   webBoundaryWindow(accepted.BoundaryWindow),
	}, nil
}

// abortResetRun abandons the newest run without producing a snapshot. It is
// idempotent: with no current run there is nothing left to abort.
func (m *measurement) abortResetRun(ctx context.Context) (web.RunAbort, error) {
	runID := m.currentRunID()
	if runID == "" {
		return web.RunAbort{}, nil
	}
	result, err := m.ctrl.AbortRun(ctx, runID, runctl.ReasonExplicit)
	if err != nil {
		return web.RunAbort{}, fmt.Errorf("isutools: aborting run %s: %w", runID, err)
	}
	m.mu.Lock()
	if m.lastRunID == runID {
		m.lastRunID = ""
	}
	m.mu.Unlock()
	return web.RunAbort{
		RunID:     result.RunID,
		Epoch:     uint64(result.Epoch),
		Reason:    result.Reason,
		Detached:  result.Detached,
		AbortedAt: result.AbortedAt,
		Partial:   append([]string(nil), result.Partial...),
	}, nil
}

// forwardRunHealth copies a finished run's diagnostics into the process-wide
// health registry: the boundary records say why a section is missing, and the
// collectors' own notes say why a section that is present is incomplete.
//
// Collectors carry those notes inside their section value because Collect has
// to be pure, so without this step every plan-mandated note is produced and
// then dropped, and the dashboard shows a hole with no explanation.
//
// It runs once per run. The notes belong to an immutable snapshot, so
// repeating them on every dashboard refresh would cost work and say nothing
// new.
func (m *measurement) forwardRunHealth(runID string, snapshot *runctl.Snapshot) {
	m.mu.Lock()
	if m.healthRunID == runID {
		m.mu.Unlock()
		return
	}
	m.healthRunID = runID
	previous := m.healthForwarded
	m.mu.Unlock()

	// A collector's status describes the newest immutable run, so a failure the
	// last run reported must not stay attached to this one. Only statuses a
	// previous forward wrote are cleared: a status set outside any run — a
	// driver registration that failed at startup, for one — describes the
	// process rather than the run, and clearing it would report a database
	// nothing is measuring as healthy.
	for name := range previous {
		collectorHealth.Set(name, health.StatusOK, "")
	}

	// written collects every collector this forward writes, so the next one
	// knows exactly what to clear.
	written := make(map[string]struct{}, len(snapshot.Collectors))
	for _, boundary := range snapshot.Collectors {
		if boundary.Code == "" {
			continue
		}
		status := health.StatusDegraded
		if boundary.Dropped {
			status = health.StatusFailed
		}
		written[boundary.Name] = struct{}{}
		collectorHealth.Set(boundary.Name, status, boundaryMessage(boundary))
	}
	// The pool collector's notes explain an absent section — "no pool was ever
	// watched" is the common one — and live on the collector rather than in
	// the section, because a section that does not exist cannot carry them.
	//
	// They must not overwrite a boundary this run already failed. The notes are
	// standing collector state, while the boundary describes the very run being
	// forwarded, so a drain timeout would otherwise be relabelled "info" by an
	// unrelated not-registered note and the run would look healthy.
	if _, boundaryFailed := written[dbpool.Name]; !boundaryFailed {
		if notes := dbpool.Default.Notes(); len(notes) > 0 {
			written[dbpool.Name] = struct{}{}
			collectorHealth.Set(dbpool.Name, dbpoolNotesStatus(notes), strings.Join(notes, "; "))
		}
	}
	for _, entry := range web.SectionHealth(snapshot.Sections) {
		written[entry.Collector] = struct{}{}
		collectorHealth.Set(entry.Collector, entry.Status, entry.Message)
	}

	m.mu.Lock()
	m.healthForwarded = written
	m.mu.Unlock()
}

// dbpoolNotesStatus keeps a missing WatchDBPool integration visible without
// claiming the measured interval is incomplete. Every other pool note
// describes a boundary anomaly and therefore degrades the run.
func dbpoolNotesStatus(notes []string) health.Status {
	for _, note := range notes {
		if !strings.HasPrefix(note, dbpool.HealthNotRegistered+":") {
			return health.StatusDegraded
		}
	}
	return health.StatusInfo
}

// boundaryMessage renders one boundary failure as a single readable line.
func boundaryMessage(boundary runctl.CollectorBoundary) string {
	message := string(boundary.Phase) + " " + boundary.Code
	if boundary.Err != "" {
		message += ": " + boundary.Err
	}
	if boundary.Dropped {
		message += " (section dropped)"
	}
	return message
}

// SerializeInitialize runs fn as the only initialize in this process.
//
// ResetNow fixes the boundary but cannot stop a second initialize from
// rebuilding the database into a run that has already started; only
// serializing the whole handler can. Wrap the entire initialize body — schema
// rebuild, fixture load, and the ResetNow call at its end — in this function.
//
// The context handed to fn carries a guard marker, so a run opened inside it
// is distinguishable from one opened outside. Waiting for the guard is
// abandoned after runctl.InitializeGuardBudget with ErrInitializeBusy: hanging
// forever on a stuck initialize would be worse than reporting it.
//
// The guard is process-local by construction. It cannot serialize initialize
// across processes or hosts.
//
// Unlike the rest of this package it keeps working when ISUTOOLS=off. An
// application that serializes its initialize through this function must not
// silently lose that serialization because a measurement flag flipped, and
// the cost of the guard is one channel send.
func SerializeInitialize(ctx context.Context, fn func(context.Context) error) error {
	return runctl.SerializeInitialize(ctx, fn)
}

// WatchDBPool reports one *sql.DB's connection pool under an already
// registered TargetID, so pool waits can be lined up with the SQL statistics
// of the same database.
//
// The pool joins the NEXT run, not the one in flight: giving it a baseline
// taken after the run started would report a fraction of the interval as if
// it were the whole of it.
//
// The ID must already exist in the registry, compared byte for byte. Watch
// never creates a target, because a typo that silently created a second one
// would split a single database across two rows of every report. Obtain the
// ID from RegisterDBTarget, or look it up with sqlstats.TargetIDForDSN.
func WatchDBPool(targetID string, db *sql.DB) error {
	if Off() {
		return nil
	}
	if db == nil {
		return fmt.Errorf("%w: target %q", dbpool.ErrNilDB, targetID)
	}
	if _, ok := sqlstats.Target(targetID); !ok {
		return fmt.Errorf("%w: %q — register it with RegisterDBTarget, or look the id up with sqlstats.TargetIDForDSN",
			sqlstats.ErrUnknownTarget, targetID)
	}
	// The argument checks above run even when measurement is off, so a wiring
	// bug surfaces in the configuration the application actually ships rather
	// than only in the one it benchmarks.
	if !featureEnabled(os.Getenv, envDBPool) {
		return nil
	}
	defaultMeasurement()
	if err := dbpool.Default.Watch(targetID, db); err != nil {
		return fmt.Errorf("isutools: watching pool %q: %w", targetID, err)
	}
	return nil
}

// UnwatchDBPool stops reporting a pool and takes a final farewell sample at
// the moment of the call, so a pool retired mid-run still reports the part of
// the run it was present for. Call it before closing the *sql.DB: it also
// drops this package's last reference to the handle.
func UnwatchDBPool(targetID string) error {
	if Off() || !featureEnabled(os.Getenv, envDBPool) {
		return nil
	}
	defaultMeasurement()
	if err := dbpool.Default.Unwatch(targetID); err != nil {
		return fmt.Errorf("isutools: unwatching pool %q: %w", targetID, err)
	}
	return nil
}

// RegisterDBTarget declares a logical database under a stable ID, so every
// collector that reports per-database numbers joins on the same key. Prefer
// it over an auto-derived ID whenever another API needs to name the target:
// derived IDs end in a hash and cannot be spelled out by hand.
//
// It is re-exported from sqlstats so an application configures isutools
// through one package.
func RegisterDBTarget(id, driverName, dsn string) error {
	if Off() {
		return nil
	}
	if err := sqlstats.RegisterDBTarget(id, driverName, dsn); err != nil {
		return fmt.Errorf("isutools: registering db target %q: %w", id, err)
	}
	return nil
}

// RegisterDBInspector attaches a second credential to an existing target: a
// stats user for SHOW STATUS and performance_schema, or a least-privilege
// EXPLAIN user. The purpose is explicit and never falls back to the
// application credential, because an implicit downgrade to a credential
// holding DML rights would defeat the point of a restricted inspector.
//
// For PurposeExplain it is the only registration path once a process has more
// than one target: with two databases registered, ISUTOOLS_EXPLAIN_DSN cannot
// say which one it belongs to, so it is refused and recorded in health rather
// than applied to a guess. A single-target process may use that variable (plus
// ISUTOOLS_EXPLAIN_DRIVER, default "mysql") instead of calling this function.
// Either way, EXPLAIN capture itself still requires ISUTOOLS_EXPLAIN=1.
//
// It is re-exported from sqlstats so an application configures isutools
// through one package.
func RegisterDBInspector(targetID string, purpose sqlstats.Purpose, driverName, dsn string) error {
	if Off() {
		return nil
	}
	if err := sqlstats.RegisterDBInspector(targetID, purpose, driverName, dsn); err != nil {
		return fmt.Errorf("isutools: registering %s inspector for %q: %w", purpose, targetID, err)
	}
	return nil
}

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
	if Off() {
		return
	}
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
	labels := os.Getenv(envCPUProfileLabels)
	if labels == "1" {
		core := defaultMeasurement()
		if core.cpu == nil || core.cpuMode != "run" {
			collectorHealth.Set("profile-labels", health.StatusDegraded, "pprof labels require managed run CPU profiling")
		} else {
			rules, err := httpstats.ParseSafeProfileRouteRules(os.Getenv(envSafeProfileRoutes))
			if err != nil {
				collectorHealth.Set("profile-labels", health.StatusDegraded, err.Error())
				rules = nil
			} else {
				collectorHealth.Set("profile-labels", health.StatusOK, "")
			}
			next = httpstats.ProfileLabelMiddleware(next, cpuHTTPLabeler{owner: core.cpu}, rules)
		}
	} else if labels != "" && labels != "0" {
		collectorHealth.Set("profile-labels", health.StatusDegraded, fmt.Sprintf("unknown %s=%q; labels disabled", envCPUProfileLabels, labels))
	}
	flowLabels := sessionlabel.FromEnv(os.Getenv)
	switch resolveFlowSource(os.Getenv) {
	case "auto", "middleware":
		flowLabels = flowLabels.WithObserver(flowstats.Default)
	case "proxy", "off":
	default:
		collectorHealth.Set("flow", health.StatusDegraded, "invalid-flow-source")
	}
	flowHealth := flowLabels.Health()
	switch {
	case flowHealth.Enabled && flowHealth.Reason == "scenario-invalid":
		collectorHealth.Set("flow-labels", health.StatusDegraded, flowHealth.Reason)
	case flowHealth.Enabled:
		collectorHealth.Set("flow-labels", health.StatusOK, flowHealth.Reason)
	case flowHealth.Reason == "auto-unconfigured" || flowHealth.Reason == "flow-labels-off" || flowHealth.Reason == "global-off":
		collectorHealth.Set("flow-labels", health.StatusDisabled, flowHealth.Reason)
	default:
		collectorHealth.Set("flow-labels", health.StatusDegraded, flowHealth.Reason)
	}
	next = flowLabels.Middleware(next)
	return httpstats.Middleware(next)
}

func resolveFlowSource(getenv func(string) string) string {
	if getenv == nil {
		return "auto"
	}
	source := strings.ToLower(strings.TrimSpace(getenv(envFlowSource)))
	if source == "" {
		return "auto"
	}
	return source
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

// ObserveRedis records one Redis-compatible command latency. Only the first
// command token is retained; keys, values, arguments, errors, and DSNs are
// discarded. It works with go-redis, redigo, rueidis, and historical clients.
func ObserveRedis(command string, duration time.Duration, err error) {
	if Off() {
		return
	}
	redisstats.Default.Observe(command, duration, err)
}

// MeasureRedis runs fn and records its duration under the sanitized command.
// The original error is returned unchanged.
func MeasureRedis(command string, fn func() error) error {
	if fn == nil {
		return nil
	}
	if Off() {
		return fn()
	}
	started := time.Now()
	err := fn()
	ObserveRedis(command, time.Since(started), err)
	return err
}

// Handler serves the report UI: GET / (dashboard with snapshot history),
// GET /snapshot.html (download), GET /json, GET /files/<name>,
// POST /reset, POST /collect, POST /finish, POST /abort, POST /save. /reset opens a
// measurement run and /finish or /save closes it; /collect stays a
// non-terminal flush of the buffered access log.
// Snapshot history persists to ISUTOOLS_DATA_DIR
// when set. The DB schema is inspected through the first DSN the
// application opened, using the raw driver so inspection queries never
// appear in the SQL statistics.
//
// Every handler shares the process-wide measurement core, so two calls
// observe one run lifecycle and one process baseline rather than two
// unrelated ones.
func Handler() http.Handler {
	if Off() {
		return http.NotFoundHandler()
	}
	core := defaultMeasurement()
	sqlCommentPolicy, sqlCommentReason := sqlstats.ResolveCommentTagPolicy(os.Getenv)
	if sqlCommentReason == "invalid-value" {
		collectorHealth.Set("sql-comment-tags", health.StatusDegraded, sqlCommentReason)
	} else {
		collectorHealth.Set("sql-comment-tags", health.StatusOK, string(sqlCommentPolicy))
	}
	provider := web.Provider{
		SQL:           sqlstats.Default,
		SQLGeneration: sqlstats.Default.CurrentGeneration,
		RotateSQL: func() (int64, []agg.Entry) {
			frozen := sqlstats.Default.Rotate()
			return frozen.Generation, frozen.Entries
		},
		SQLGenerationManaged:      core.generationManaged.sql,
		HTTPGenerationManaged:     core.generationManaged.http,
		CountersGenerationManaged: core.generationManaged.counter,
		RedisGenerationManaged:    core.generationManaged.redis,
		FlowGenerationManaged:     core.generationManaged.flow,
		ProcRunManaged:            core.procManaged,
		Health:                    collectorHealth,
		HTTP:                      httpstats.Default,
		DataDir:                   os.Getenv(envDataDir),
		ProfileAnalysis:           os.Getenv(envProfileAnalysis) == "1",
		Executable:                core.executable,
		PprofDuration:             pprofDuration(os.Getenv),
		DB: func(ctx context.Context) *dbinspect.Schema {
			name, dsn, ok := sqlstats.FirstConn()
			if !ok {
				return nil
			}
			return dbinspect.Collect(ctx, name, dsn)
		},
		DBCapabilities: func() []dbcap.Target { return dbcap.ForTargets(sqlstats.Targets(), nil) },
		Advisor:        collectAdvice,
		Counters:       counters.Default,
		Redis:          redisstats.Default,
		FlowSource:     resolveFlowSource(os.Getenv),
		// The reset boundary is the run boundary: POST /reset opens a run on
		// the shared Controller and answers with its id, so a bench script can
		// name the run it just started. POST /finish and POST /save close it
		// again — without a terminator the collectors would keep accumulating
		// into a generation nobody ever froze, and no section would ever reach
		// a snapshot.
		StartRun:        core.startResetRun,
		FinishRun:       core.finishResetRun,
		CompleteRun:     core.completeResetRun,
		AbortRun:        core.abortResetRun,
		Sections:        core.latestSections,
		RunSnapshot:     core.latestRunSnapshot,
		Timeline:        core.timelineSection,
		RuntimeProfiles: core.profiles,
	}
	attachTraceCapture(&provider, core.traceBridge)
	if source := provider.FlowSource; source == "auto" || source == "middleware" {
		provider.Flow = flowstats.Default
	}
	if core.cpuBridge != nil {
		provider.CPUProfiles = core.cpuBridge
		provider.CPUProfileMode = core.cpuMode
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
		unmatched := accesslog.UnmatchedKeep
		switch strings.ToLower(strings.TrimSpace(os.Getenv(envAccessLogUnmatched))) {
		case "", "keep":
		case "collapse":
			unmatched = accesslog.UnmatchedCollapse
		default:
			collectorHealth.Set("accesslog-path-rules", health.StatusDegraded, "invalid-unmatched-policy")
		}
		collector := accesslog.New(path,
			accesslog.WithFormatSpec(os.Getenv(envAccessLogFormat)),
			accesslog.WithPathRulesSpec(os.Getenv(envAccessLogPathRules), unmatched),
		)
		provider.AccessLog = collector
		provider.AccessLogGenerationManaged = core.watchAccessLogGeneration(collector)
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
	if collector := core.proc; collector != nil {
		provider.Proc = collector
		if core.procManaged {
			collectorHealth.Set(procstats.CollectorName, health.StatusOK, "")
		} else if err := collector.Reset(); err != nil {
			collectorHealth.Set("proc", health.StatusDegraded, err.Error())
		} else {
			collectorHealth.Set("proc", health.StatusOK, "")
		}
	} else {
		collectorHealth.Set("proc", health.StatusDisabled, "procfs is only available on Linux")
	}
	return web.NewHandler(provider)
}

func (m *measurement) timelineSection(runID string, epoch uint64) *timeline.Section {
	if m == nil || m.timeline == nil {
		return nil
	}
	return m.timeline.section(runID, epoch)
}

// startResetRun opens the run that POST /reset measures.
//
// It preempts a run already in flight because that is what the endpoint has
// always meant: every reset starts a fresh generation and abandons whatever
// was being measured. A run whose result matters is ended through POST /finish
// or POST /save, which keep its snapshot; a reset deliberately does not.
func (m *measurement) startResetRun(ctx context.Context) (web.RunStart, error) {
	previous := m.currentRunID()
	result, err := m.startRun(ctx, runctl.StartRunOptions{
		Preempt: true,
		Reason:  "http",
		Trigger: "reset",
	})
	if err != nil {
		return web.RunStart{}, err
	}
	m.reapPreviousRun(ctx, previous, result)
	return webRunStart(result), nil
}

// webRunStart renders a coordinator boundary as the transport's opening-run
// record. Both entry points build it through this function, so a run opened by
// ResetNow carries exactly the identity — id, epoch, boundary windows — that
// POST /reset would have given it, and its profile artifacts are filed and
// paired the same way.
func webRunStart(result runctl.StartResult) web.RunStart {
	return web.RunStart{
		RunID:            result.RunID,
		Epoch:            uint64(result.Epoch),
		State:            string(result.State),
		StartedAt:        result.StartedAt,
		Validity:         string(result.Validity),
		GenerationWindow: webBoundaryWindow(result.GenerationWindow),
		BoundaryWindow:   webBoundaryWindow(result.BoundaryWindow),
	}
}

// reapPreviousRun makes sure the run the previous reset opened is no longer
// active once a new one has started, so runs cannot pile up in "started".
//
// StartRun's own preempt normally does this and records the successor in the
// abort reason, which is why the abort is not taken up front: doing it here
// keeps that provenance. Preempt can only see the Controller's newest run
// though, so this covers the case the state machine structurally cannot — a
// previous run that is no longer the newest and would otherwise stay active
// forever, holding its collectors' generations with it.
func (m *measurement) reapPreviousRun(ctx context.Context, previous string, result runctl.StartResult) {
	if previous == "" || previous == result.RunID || previous == result.PreemptedRunID {
		return
	}
	status, ok := m.ctrl.Status(previous)
	if !ok {
		return
	}
	switch status.State {
	case runctl.StateStarting, runctl.StateStarted, runctl.StateFinishing:
		//nolint:errcheck // AbortRun is idempotent and never fails for a known run.
		_, _ = m.ctrl.AbortRun(ctx, previous, runctl.ReasonPreemptedBy+result.RunID)
	}
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

const (
	maxProxyConfBytes = 4 << 20
	maxProxyConfFiles = 256
	maxProxyConfDepth = 32
)

// readProxyConf reads a proxy configuration with bounded, best-effort nginx
// include expansion. A file is treated as the active entrypoint, so only its
// reachable include graph is loaded. Directory mode retains the historical
// *.conf discovery, but canonical-path de-duplication prevents a
// sites-available file and its sites-enabled symlink from being counted twice.
// Caddy/Envoy callers should continue to pass one file; their syntax has no
// nginx "include" directive and is returned unchanged.
func readProxyConf(path string) []byte {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	loader := proxyConfLoader{seen: make(map[string]struct{})}
	if !info.IsDir() {
		loader.load(path, 0)
		return loader.out
	}
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(p), ".conf") {
			loader.load(p, 0)
		}
		return nil
	})
	return loader.out
}

type proxyConfLoader struct {
	out   []byte
	seen  map[string]struct{}
	files int
	bytes int
}

func (l *proxyConfLoader) load(path string, depth int) {
	data := l.expand(path, depth)
	if len(data) == 0 || len(data) > maxProxyConfBytes-len(l.out) {
		return
	}
	l.out = append(l.out, data...)
	if data[len(data)-1] != '\n' && len(l.out) < maxProxyConfBytes {
		l.out = append(l.out, '\n')
	}
}

func (l *proxyConfLoader) expand(path string, depth int) []byte {
	if depth > maxProxyConfDepth || l.files >= maxProxyConfFiles || l.bytes >= maxProxyConfBytes {
		return nil
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil
	}
	canonical = filepath.Clean(canonical)
	if _, duplicate := l.seen[canonical]; duplicate {
		return nil
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}

	file, err := os.Open(canonical)
	if err != nil {
		return nil
	}
	remaining := maxProxyConfBytes - l.bytes
	data, readErr := io.ReadAll(io.LimitReader(file, int64(remaining)+1))
	_ = file.Close()
	if readErr != nil || len(data) > remaining {
		return nil
	}
	l.seen[canonical] = struct{}{}
	l.files++
	l.bytes += len(data)

	// nginx -T already emits the entrypoint and every included file in one
	// stream, prefixed by repeated "configuration file" markers. Expanding its
	// still-visible include directives would load the live fragments a second
	// time and recreate the duplicate-listener problem this loader prevents.
	if nginxConfigDump(data) {
		return data
	}
	directives := nginxIncludeDirectives(data)
	if len(directives) == 0 {
		return data
	}
	var expanded []byte
	cursor := 0
	for _, directive := range directives {
		expanded = append(expanded, data[cursor:directive.start]...)
		cursor = directive.end
		target := directive.target
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(canonical), target)
		}
		matches, globErr := filepath.Glob(target)
		if globErr != nil {
			continue
		}
		for _, match := range matches {
			expanded = append(expanded, l.expand(match, depth+1)...)
		}
	}
	expanded = append(expanded, data[cursor:]...)
	return expanded
}

func nginxConfigDump(data []byte) bool {
	const marker = "# configuration file "
	text := string(data)
	count := strings.Count(text, "\n"+marker)
	if strings.HasPrefix(text, marker) {
		count++
	}
	return count >= 2
}

type nginxIncludeDirective struct {
	start, end int
	target     string
}

// nginxIncludeDirectives returns byte spans and targets for include
// directives. Semicolons and braces inside quoted strings or comments are not
// syntax, so a quoted path remains intact and inline contexts are supported.
func nginxIncludeDirectives(data []byte) []nginxIncludeDirective {
	var directives []nginxIncludeDirective
	statementStart := 0
	var quote byte
	escaped := false
	comment := false
	for i, ch := range data {
		if comment {
			if ch == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '#':
			comment = true
		case '\'', '"':
			quote = ch
		case '{', '}':
			statementStart = i + 1
		case ';':
			statement := strings.TrimSpace(stripNginxComments(string(data[statementStart:i])))
			fields := strings.Fields(statement)
			if len(fields) == 0 || fields[0] != "include" {
				statementStart = i + 1
				continue
			}
			rest := strings.TrimSpace(statement[len("include"):])
			if rest == "" {
				statementStart = i + 1
				continue
			}
			if (rest[0] == '"' && rest[len(rest)-1] == '"') ||
				(rest[0] == '\'' && rest[len(rest)-1] == '\'') {
				rest = rest[1 : len(rest)-1]
			}
			if rest != "" {
				directives = append(directives, nginxIncludeDirective{
					start: statementStart, end: i + 1, target: rest,
				})
			}
			statementStart = i + 1
		}
	}
	return directives
}

func stripNginxComments(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	var quote byte
	escaped := false
	comment := false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if comment {
			if ch == '\n' {
				comment = false
				out.WriteByte(ch)
			}
			continue
		}
		if quote != 0 {
			out.WriteByte(ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '#':
			comment = true
		case '\'', '"':
			quote = ch
			out.WriteByte(ch)
		default:
			out.WriteByte(ch)
		}
	}
	return out.String()
}
