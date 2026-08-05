package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	netpprof "net/http/pprof"
	"os"
	"path/filepath"
	rpprof "runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekusiadadus/isutools/internal/health"
)

// pprofHandler exposes the process profiles under /pprof/ on the admin
// server (the admin server runs inside the instrumented process, so these
// are the application's profiles).
func pprofHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/pprof/")
	switch name {
	case "profile":
		netpprof.Profile(w, r)
	case "trace":
		netpprof.Trace(w, r)
	case "symbol":
		netpprof.Symbol(w, r)
	case "cmdline":
		netpprof.Cmdline(w, r)
	default:
		// Index serves "" and named runtime profiles (heap, goroutine, ...)
		// but expects the /debug/pprof/ path prefix.
		r.URL.Path = "/debug/pprof/" + name
		netpprof.Index(w, r)
	}
}

// cpuCaptureActive guards the process-wide CPU profiler (only one capture
// can run at a time).
var cpuCaptureActive atomic.Bool

// captureCPUProfile records a CPU profile for the given duration into
// DataDir, named after the generation it measures. Failures are logged and
// otherwise ignored: profiling must never break measurement or the app.
func (h *handler) captureCPUProfile(generation int64) {
	if h.p.PprofDuration <= 0 || h.p.DataDir == "" {
		return
	}
	if !cpuCaptureActive.CompareAndSwap(false, true) {
		log.Print("isutools: CPU capture already running; skipping")
		return
	}
	name := fmt.Sprintf("%s_gen%d_cpu.pprof",
		time.Now().In(reportTZ).Format("20060102-150405"), generation)
	path := filepath.Join(h.p.DataDir, name)
	f, err := os.Create(path)
	if err != nil {
		cpuCaptureActive.Store(false)
		log.Printf("isutools: CPU profile create failed: %v", err)
		return
	}
	if err := rpprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		cpuCaptureActive.Store(false)
		log.Printf("isutools: CPU profile start failed: %v", err)
		return
	}
	go func() {
		defer cpuCaptureActive.Store(false)
		time.Sleep(h.p.PprofDuration)
		rpprof.StopCPUProfile()
		if err := f.Close(); err != nil {
			log.Printf("isutools: CPU profile close failed: %v", err)
			return
		}
		log.Printf("isutools: CPU profile saved: %s", name)
	}()
}

// ProfilePoint names the run boundary a runtime profile was captured at. A
// mutex, block or heap profile is process-wide and cumulative, so a single
// file says nothing about a run: only the difference between the two
// boundaries does, and the point is what tells them apart.
type ProfilePoint string

const (
	// ProfilePointOpen is the capture taken as the run opens.
	ProfilePointOpen ProfilePoint = "open"
	// ProfilePointClose is the capture taken as the run's boundary is frozen.
	ProfilePointClose ProfilePoint = "close"
)

// profileCaptureLease bounds a whole boundary capture. runtime/pprof's WriteTo
// takes no context and cannot be interrupted, so the lease is enforced as a
// gate between profile kinds rather than as a timeout: whatever has started
// runs to completion, and the kinds after it are skipped.
const profileCaptureLease = 3 * time.Second

const (
	// profileArtifactExt is what /files/ serves; profileTempExt is what it
	// refuses, which is why an unfinished artifact wears it.
	profileArtifactExt = ".pprof"
	// profileSidecarExt ends in ".json" so /files/ serves the record next to
	// the profile it describes.
	profileSidecarExt = ".meta.json"
	profileTempExt    = ".tmp"
)

// A pair is only worth as much as the moments its two halves were taken at,
// so every capture records which gate it passed through on the way out.
const (
	// openGatePostStartReturn is an opening capture taken as soon as the run
	// coordinator returned the boundary. It is the tightest gate available and
	// the one POST /reset uses.
	openGatePostStartReturn = "post-start-return"
	// openGatePostPrevDrain is an opening capture deliberately held until the
	// previous run's drain finished. It trades a larger head loss for a
	// difference that does not contain the previous run's teardown, and the
	// trade is cheap: nothing is loading the application in that window, so the
	// lost head carries no benchmark traffic.
	openGatePostPrevDrain = "post-prev-drain"
)

// Reference phases name the coordinator step a half is measured from. The
// values are the coordinator's own phase names, so an artifact can be lined up
// with the run's boundary record without a translation table.
const (
	profileRefPhaseStart  = "start-boundary"
	profileRefPhaseFinish = "finish-freeze"
)

// Capture outcomes. A capture that was not taken is recorded rather than
// forgotten: "there is no mutex profile for this run" is only readable if the
// reason it is missing was written down.
const (
	profileStatusOK      = "ok"
	profileStatusFailed  = "failed"
	profileStatusSkipped = "skipped"
)

const (
	profileCodeLeaseExceeded = "capture-lease-exceeded"
	profileCodeAborted       = "aborted"
	profileCodeWriteFailed   = "write-failed"
	profileCodeUnknownKind   = "unknown-kind"
)

// The four health keys this feature owns. "profile" (the effective rates) is
// set once at startup by the process that configures them; the three below are
// per-capture verdicts.
const (
	healthProfileLag        = "profile-capture-lag"
	healthProfileFailed     = "profile-capture-failed"
	healthProfileIncomplete = "profile-pair-incomplete"
)

// profileLagLimit is the absolute lag beyond which a capture no longer
// brackets the boundary it names. The closing half is also judged against the
// run's own length, because a 900ms tail is negligible on a 60s run and
// alarming on a 5s one.
const profileLagLimit = time.Second

// Retention. Profile artifacts are large and accumulate one set per run, so
// the directory keeps the most recent runs and a total size, and drops the
// least useful first: orphans (no closing half, hence no difference to take),
// then runs the coordinator declared invalid, then simply the oldest.
const (
	profileRetentionRuns  = 20
	profileRetentionBytes = 512 << 20
	// profileLedgerRuns caps the in-memory pair bookkeeping. It is deliberately
	// larger than the on-disk retention so a run whose artifacts still exist
	// still has its record.
	profileLedgerRuns = 64
	// profileTempMaxAge is how long an unpublished temporary file may exist
	// before retention treats it as debris from a process that died mid-write.
	//
	// Every in-process failure path removes its own temporary file, so the only
	// way one survives is a crash or a kill between create and rename — and
	// nothing else can ever reclaim it, because a ".tmp" belongs to no run
	// group. Age is the only safe discriminator, so the window is far longer
	// than any single profile write can take (the whole capture is bounded by
	// profileCaptureLease) and a capture in flight is never touched.
	profileTempMaxAge = time.Hour
)

// ProfileCapture is one profile file's record: what was captured, when, and
// how far after the boundary it claims to bracket. It is the content of the
// ".meta.json" sidecar written next to every artifact.
//
// The sidecar is the durable primary record. An opening capture is written at
// reset, long before any snapshot exists to hold a manifest, so the sidecar is
// the only place the opening moment can survive a process that never finishes
// the run.
type ProfileCapture struct {
	RunID string       `json:"run_id"`
	Epoch uint64       `json:"epoch"`
	Point ProfilePoint `json:"point"`
	Kind  string       `json:"kind"`
	// File is the published ".pprof" name, empty when nothing was published.
	File string `json:"file,omitempty"`
	// Sidecar is this record's own file name, so a manifest entry can be
	// followed back to the record on disk.
	Sidecar  string `json:"sidecar"`
	OpenGate string `json:"open_gate,omitempty"`
	// Orphan marks an artifact whose run never reached a closing boundary. It
	// is written into the record rather than left to inference, because a lone
	// opening profile is otherwise indistinguishable from half of a pair whose
	// other half has not been taken yet.
	Orphan bool `json:"orphan,omitempty"`

	// The reference point this capture is measured from, copied from the
	// coordinator's boundary record. A boundary is an interval rather than an
	// instant, so the measured spreads travel with it and form the uncertainty
	// floor under every residual below. Legacy providers leave them zero, which
	// means "not reported", not "measured as zero".
	RefPhase         string    `json:"ref_phase"`
	RefAt            time.Time `json:"ref_at,omitzero"`
	RefSpreadNs      int64     `json:"ref_spread_ns"`
	BoundaryAt       time.Time `json:"boundary_at,omitzero"`
	BoundarySpreadNs int64     `json:"boundary_spread_ns"`

	// The measured capture instants. LagFromRefNs is the distance from the
	// boundary to the moment the write actually began, which is the number the
	// residual error is built out of.
	StartedAt    time.Time `json:"started_at,omitzero"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
	LagFromRefNs int64     `json:"lag_from_ref_ns"`
	DurationNs   int64     `json:"duration_ns"`
	Bytes        int64     `json:"bytes"`

	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Err    string `json:"err,omitempty"`
}

// ProfilePair is one kind's difference over a run: the two artifacts plus the
// measured error in treating their difference as the run.
//
// It is an approximation and says so. The difference starts a little after the
// run does (HeadLossNs, not included) and ends a little after the run ends
// (TailExcessNs, included), because both halves are taken by the caller after
// the coordinator has already fixed the boundary.
type ProfilePair struct {
	Kind      string `json:"kind"`
	OpenFile  string `json:"open_file"`
	CloseFile string `json:"close_file"`
	OpenGate  string `json:"open_gate,omitempty"`
	// RunSpanNs is the distance between the two boundaries themselves.
	RunSpanNs int64 `json:"run_span_ns"`
	// HeadLossNs is the run's beginning that the difference does not contain.
	HeadLossNs int64 `json:"head_loss_ns"`
	// TailExcessNs is the post-boundary tail that the difference does contain.
	TailExcessNs int64 `json:"tail_excess_ns"`
	// ApproxErrorNs is HeadLossNs + TailExcessNs: the total by which the
	// difference is not the run.
	ApproxErrorNs int64  `json:"approx_error_ns"`
	DiffCommand   string `json:"diff_command"`
}

// ProfileManifest is a run's whole profile record: every capture attempted at
// either boundary, and the pairs that can actually be differenced.
type ProfileManifest struct {
	RunID    string           `json:"run_id"`
	Epoch    uint64           `json:"epoch"`
	Validity string           `json:"validity,omitempty"`
	Captures []ProfileCapture `json:"captures,omitempty"`
	// Pairs holds only the kinds whose two halves both exist. A kind with one
	// half is deliberately absent: a difference cannot be taken from it, and
	// listing it would invite the reader to try.
	Pairs []ProfilePair `json:"pairs,omitempty"`
}

// Lagging reports whether either half of the pair was taken too far from the
// boundary it names. The Runs detail page and the health verdict both call
// this, so the badge on the page and the entry in health can never disagree.
func (p ProfilePair) Lagging() bool {
	return profileTailLagging(p.TailExcessNs, p.RunSpanNs) ||
		profileHeadLagging(p.HeadLossNs, p.OpenGate)
}

// profileTailLagging judges the closing half. A tail is always a defect: the
// closing capture is taken right after the freeze returns, so anything beyond
// the limit means something held the caller up.
func profileTailLagging(tailNs, runSpanNs int64) bool {
	return tailNs > int64(profileLagLimit) || (runSpanNs > 0 && tailNs > runSpanNs/100)
}

// profileHeadLagging judges the opening half, and only when the capture was
// meant to be immediate. A capture that waited for the previous run's drain
// was told to wait, and reporting the wait as a defect would teach the reader
// to ignore the warning.
func profileHeadLagging(headNs int64, gate string) bool {
	return gate != openGatePostPrevDrain && headNs > int64(profileLagLimit)
}

// ResidualText renders the pair's residual error for the Runs detail page.
func (p ProfilePair) ResidualText() string {
	text := fmt.Sprintf("欠落 %s・余剰 %s(合計 %s",
		profileLagText(p.HeadLossNs), profileLagText(p.TailExcessNs),
		profileLagText(p.ApproxErrorNs))
	if p.RunSpanNs > 0 {
		text += fmt.Sprintf(" = run 長の %.2f%%",
			float64(p.ApproxErrorNs)*100/float64(p.RunSpanNs))
	}
	return text + ")"
}

// Notes returns the sentences the Runs detail page prints under a pair.
//
// The approximation notice is unconditional. A small residual is still a
// residual, and a reader who sees the notice only on bad runs will read its
// absence as "this one is exact", which no pair ever is.
func (p ProfilePair) Notes() []string {
	notes := []string{
		"この pair はプロセス累積プロファイルの差分であり、run 単位のプロファイルではありません。",
		fmt.Sprintf("run 冒頭の %s を含まず、finish freeze 後の %s を含みます。",
			profileLagText(p.HeadLossNs), profileLagText(p.TailExcessNs)),
	}
	if p.OpenGate == openGatePostPrevDrain {
		notes = append(notes,
			"前 run の Drain 完了を待ってから採取しているため、この欠落区間にベンチ負荷はありません。")
	}
	return notes
}

// profileLagText renders a duration the way the page reads it: milliseconds
// while the number stays small enough to mean "negligible", seconds once it
// does not.
func profileLagText(ns int64) string {
	d := time.Duration(ns)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.3fs", d.Seconds())
}

// profileRef is the boundary instant a capture is measured against, together
// with the width that instant actually has: a boundary is an interval, so the
// spread is the floor under any error the residuals report.
//
// The instant is the moment the coordinator reported for the boundary — the
// opening run's StartedAt, the closing run's AcceptedAt. Both are taken at or
// after the generation window closed, so a residual measured from them is a
// lower bound on the distance from the generation boundary itself, never an
// overstatement of how tightly the pair brackets the run.
type profileRef struct {
	phase          string
	at             time.Time
	spread         time.Duration
	boundaryAt     time.Time
	boundarySpread time.Duration
}

// profileCaptureSpec is one end of a run as the capture path sees it.
type profileCaptureSpec struct {
	runID      string
	epoch      uint64
	point      ProfilePoint
	generation int64
	// stamp names the artifacts. Both halves use the run's opening moment, so
	// the closing capture takes it from the ledger rather than from the clock.
	stamp    time.Time
	validity string
	openGate string
	ref      profileRef
}

// profileRunKey is the identity a capture is idempotent under: one artifact
// per run, per point, per profile kind.
//
// The directory is part of the key because the artifacts are, and two handlers
// writing into one directory must agree about what is already there. Epoch is
// part of it because a run id alone is not a fencing token: a coordinator that
// re-opens under a new epoch is measuring a different run.
type profileRunKey struct {
	dir   string
	runID string
	epoch uint64
}

// profileRun is one run's capture bookkeeping.
type profileRun struct {
	mu     sync.Mutex
	key    profileRunKey
	prefix string
	// stamp and generation are fixed by the opening capture and reused by the
	// closing one: the two halves must share a file name prefix or the pair
	// cannot be reassembled from a directory listing.
	stamp      time.Time
	generation int64
	validity   string
	openRef    profileRef
	closeRef   profileRef
	captures   []ProfileCapture
	// taken marks a point as claimed, which is what makes a replayed finish a
	// no-op instead of a second artifact.
	taken     map[ProfilePoint]bool
	published map[ProfilePoint][]string
	orphan    bool
}

// profileLedger records the runs whose artifacts are on disk.
//
// It is process-wide rather than per-handler because the artifacts are: the
// pair, its idempotency and its retention are properties of the directory, and
// a second handler pointed at the same directory has to see the same record or
// it will publish a duplicate half.
type profileLedger struct {
	mu    sync.Mutex
	runs  map[profileRunKey]*profileRun
	order []profileRunKey
}

var boundaryProfiles = &profileLedger{runs: make(map[profileRunKey]*profileRun)}

// open returns the run's record, creating it for an opening capture.
//
// A closing capture with no record returns nil, and that is the point: without
// an opening half there is no pair, and a lone closing artifact would sit in
// the directory looking like one. A restarted process is the same case — its
// cumulative counters began again, so its artifacts cannot be differenced
// against the previous process's.
func (l *profileLedger) open(dir string, spec profileCaptureSpec) *profileRun {
	key := profileRunKey{dir: dir, runID: spec.runID, epoch: spec.epoch}
	l.mu.Lock()
	defer l.mu.Unlock()
	if run, ok := l.runs[key]; ok {
		return run
	}
	if spec.point != ProfilePointOpen {
		return nil
	}
	stamp := spec.stamp
	if stamp.IsZero() {
		stamp = time.Now()
	}
	run := &profileRun{
		key:        key,
		prefix:     profilePrefix(stamp, spec.generation, spec.runID),
		stamp:      stamp,
		generation: spec.generation,
		validity:   spec.validity,
		taken:      make(map[ProfilePoint]bool),
		published:  make(map[ProfilePoint][]string),
	}
	l.runs[key] = run
	l.order = append(l.order, key)
	for len(l.order) > profileLedgerRuns {
		delete(l.runs, l.order[0])
		l.order = l.order[1:]
	}
	return run
}

// find returns a known run's record, or nil.
func (l *profileLedger) find(dir, runID string, epoch uint64) *profileRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	if run := l.runs[profileRunKey{dir: dir, runID: runID, epoch: epoch}]; run != nil {
		return run
	}
	// Callers that only have the stable run id (manifest lookup and abort)
	// deliberately pass epoch zero. Run ids are unique within a coordinator;
	// search newest-first so compatibility callers can still find an epochful
	// record without weakening the exact key used by boundary captures.
	if epoch == 0 {
		for i := len(l.order) - 1; i >= 0; i-- {
			key := l.order[i]
			if key.dir == dir && key.runID == runID {
				return l.runs[key]
			}
		}
	}
	return nil
}

// unfinished returns the runs in one directory that opened a pair and never
// closed it, excluding the one given.
//
// Every such run is over: the handler serialises runs, so opening a new one
// means the previous was preempted or aborted. Their artifacts are orphans.
func (l *profileLedger) unfinished(dir string, except *profileRun) []*profileRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	var runs []*profileRun
	for _, key := range l.order {
		run := l.runs[key]
		if run == nil || run == except || key.dir != dir {
			continue
		}
		if run.needsOrphaning() {
			runs = append(runs, run)
		}
	}
	return runs
}

// invalidPrefixes returns the file name prefixes of runs the coordinator
// declared invalid, so retention can drop them before it drops good ones.
func (l *profileLedger) invalidPrefixes(dir string) map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	prefixes := make(map[string]bool)
	for key, run := range l.runs {
		if key.dir == dir && run.validity == "invalid" {
			prefixes[run.prefix] = true
		}
	}
	return prefixes
}

// pairable reports whether the run published an opening half, which is the
// only thing a closing half can be differenced against.
func (r *profileRun) pairable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.published[ProfilePointOpen]) > 0
}

// needsOrphaning reports whether the run published an opening half, never
// published a closing one, and has not been marked yet.
func (r *profileRun) needsOrphaning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.orphan && len(r.published[ProfilePointOpen]) > 0 && !r.taken[ProfilePointClose]
}

// claim takes the point for this caller, returning the artifacts an earlier
// caller already published there.
//
// This is the whole of the idempotency contract: POST /finish and POST /save
// both close the same run — the second call replays the first's accepted
// boundary — and a naive capture path would answer that replay with a second
// artifact taken minutes later, after the drain and the snapshot build, which
// is exactly the tail this capture point exists to avoid.
func (r *profileRun) claim(spec profileCaptureSpec) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.taken[spec.point] {
		return append([]string(nil), r.published[spec.point]...), false
	}
	r.taken[spec.point] = true
	if spec.point == ProfilePointClose {
		r.closeRef = spec.ref
		if spec.validity != "" {
			r.validity = spec.validity
		}
		return nil, true
	}
	r.openRef = spec.ref
	return nil, true
}

// settle records what a claimed point produced and returns the published
// names.
//
// A point that produced nothing and could still succeed releases its claim: an
// unwritten artifact is not an artifact anyone can duplicate, so a retry can
// only help. A point that was skipped because the run died stays claimed —
// that decision is final.
func (r *profileRun) settle(point ProfilePoint, captures []ProfileCapture) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var written []string
	final := false
	for _, capture := range captures {
		if capture.Status == profileStatusOK {
			written = append(written, capture.File)
		}
		if capture.Code == profileCodeAborted {
			final = true
		}
	}
	r.captures = append(r.captures, captures...)
	if len(written) == 0 && !final {
		delete(r.taken, point)
		return nil
	}
	r.published[point] = written
	return written
}

// newCapture starts the record for one kind at one point, pre-filled with
// everything that is known before the write is attempted. It starts out
// "skipped": a capture that never reaches the write path has, in fact, been
// skipped, and the default must not be the optimistic one.
func (r *profileRun) newCapture(spec profileCaptureSpec, kind string) ProfileCapture {
	capture := ProfileCapture{
		RunID:            r.key.runID,
		Epoch:            r.key.epoch,
		Point:            spec.point,
		Kind:             kind,
		Sidecar:          profileBaseName(r.prefix, kind, spec.point) + profileSidecarExt,
		RefPhase:         spec.ref.phase,
		RefAt:            spec.ref.at,
		RefSpreadNs:      spec.ref.spread.Nanoseconds(),
		BoundaryAt:       spec.ref.boundaryAt,
		BoundarySpreadNs: spec.ref.boundarySpread.Nanoseconds(),
		Status:           profileStatusSkipped,
	}
	if spec.point == ProfilePointOpen {
		capture.OpenGate = spec.openGate
	}
	return capture
}

// captureRuntimeProfiles writes the opening half of a run's profile pair into
// DataDir and returns the artifact names it published.
//
// It runs synchronously in the caller's goroutine. Capturing asynchronously
// would make the captured moment depend on the scheduler, which is precisely
// the thing a boundary artifact is supposed to pin down.
//
// Nothing here is fatal: a failed capture is logged and recorded, the
// remaining kinds are still attempted, and the caller is never told, because
// losing a profile must never cost the run.
func (h *handler) captureRuntimeProfiles(run RunStart, point ProfilePoint, generation int64) []string {
	refAt := run.GenerationWindow.Max
	if refAt.IsZero() {
		refAt = run.StartedAt
	}
	boundaryAt := run.BoundaryWindow.Max
	if boundaryAt.IsZero() {
		boundaryAt = run.StartedAt
	}
	return h.captureBoundary(profileCaptureSpec{
		runID:      run.RunID,
		epoch:      run.Epoch,
		point:      point,
		generation: generation,
		stamp:      run.StartedAt,
		validity:   run.Validity,
		// POST /reset opens the run and captures as soon as the coordinator
		// returns, without waiting for the previous run's drain: the response
		// is what releases the benchmarker, so a capture taken later would
		// already contain the load it is supposed to precede.
		openGate: openGatePostStartReturn,
		ref: profileRef{
			phase:          profileRefPhaseStart,
			at:             refAt,
			spread:         run.GenerationWindow.Spread,
			boundaryAt:     boundaryAt,
			boundarySpread: run.BoundaryWindow.Spread,
		},
	})
}

// captureCloseProfiles writes the closing half of the run's profile pair and
// returns the artifact names it published.
//
// Call it the moment the closing boundary comes back and before anything waits
// on the drain. The coordinator returns the accepted boundary without waiting
// for draining, collecting or snapshot building, so a capture taken after that
// wait would contain up to the whole background budget of isutools' own work —
// and mutex and block profiles would attribute it to lock contention, which is
// the one thing they are read for. What lands between the freeze and this call
// is measured instead of assumed, and reported as TailExcessNs.
//
// A run with no opening half captures nothing: see profileLedger.open.
func (h *handler) captureCloseProfiles(run RunFinish) []string {
	if run.RunID == "" {
		return nil
	}
	refAt := run.GenerationWindow.Max
	if refAt.IsZero() {
		refAt = run.AcceptedAt
	}
	boundaryAt := run.BoundaryWindow.Max
	if boundaryAt.IsZero() {
		boundaryAt = run.AcceptedAt
	}
	return h.captureBoundary(profileCaptureSpec{
		runID:    run.RunID,
		epoch:    run.Epoch,
		point:    ProfilePointClose,
		validity: run.Validity,
		ref: profileRef{
			phase:          profileRefPhaseFinish,
			at:             refAt,
			spread:         run.GenerationWindow.Spread,
			boundaryAt:     boundaryAt,
			boundarySpread: run.BoundaryWindow.Spread,
		},
	})
}

// abortRunProfiles marks a run that will never be closed — aborted, or
// preempted by the next reset — so its opening artifacts stop looking like
// half of a pair that is still coming.
func (h *handler) abortRunProfiles(runID string) {
	if h.p.DataDir == "" || runID == "" {
		return
	}
	run := boundaryProfiles.find(h.p.DataDir, runID, 0)
	if run == nil {
		return
	}
	h.orphan(run)
}

// profileManifest returns a run's profile record, or nil when the run captured
// nothing. It is what a snapshot embeds so a saved report can be read back
// with its profiles.
func (h *handler) profileManifest(runID string) *ProfileManifest {
	if h.p.DataDir == "" || runID == "" {
		return nil
	}
	run := boundaryProfiles.find(h.p.DataDir, runID, 0)
	if run == nil {
		return nil
	}
	return run.manifest()
}

// captureBoundary is the common path of both halves.
func (h *handler) captureBoundary(spec profileCaptureSpec) []string {
	if len(h.p.RuntimeProfiles) == 0 || h.p.DataDir == "" {
		return nil
	}
	return h.captureBoundaryBefore(spec, time.Now().Add(profileCaptureLease))
}

// captureBoundaryBefore captures every enabled kind, stopping at the lease.
//
// The deadline is a parameter so the lease can be exercised without waiting
// three seconds for it.
func (h *handler) captureBoundaryBefore(spec profileCaptureSpec, deadline time.Time) []string {
	kinds := orderProfileKinds(h.p.RuntimeProfiles)
	// Without a run id there is no pair to assemble and nothing to be
	// idempotent about, so the artifact is written as what it is: a lone
	// cumulative snapshot of a process that has no run coordinator.
	if spec.runID == "" {
		return h.captureAnonymous(spec, kinds, deadline)
	}
	run := boundaryProfiles.open(h.p.DataDir, spec)
	// A closing half is only worth writing when there is an opening half to
	// difference it against. Without one it is a lone cumulative profile that
	// looks exactly like half of a pair, which is the shape a reader would
	// take at face value.
	if run == nil || (spec.point == ProfilePointClose && !run.pairable()) {
		log.Printf("isutools: %s profile capture skipped: run %s has no opening capture to pair with",
			spec.point, spec.runID)
		return nil
	}
	names, claimed := run.claim(spec)
	if !claimed {
		return names
	}
	// Every name below is built from the run's prefix, which the opening
	// capture fixed: the closing half inherits the opening timestamp and
	// generation instead of reading the clock again.
	captures := make([]ProfileCapture, 0, len(kinds))
	for _, kind := range kinds {
		captures = append(captures, h.captureKind(run, spec, kind, deadline))
	}
	written := run.settle(spec.point, captures)
	for _, capture := range captures {
		h.writeProfileSidecar(capture)
	}
	if spec.point == ProfilePointOpen {
		// Opening a run ends every run before it. Any of those still waiting
		// for a closing half will wait forever.
		for _, stale := range boundaryProfiles.unfinished(h.p.DataDir, run) {
			h.orphan(stale)
		}
	}
	h.applyProfileHealth(run)
	h.pruneProfiles(run.prefix)
	return written
}

// captureAnonymous is the legacy path: profiles enabled on a process whose
// resets do not open runs. There is no pair, no sidecar and no manifest —
// only the artifact.
func (h *handler) captureAnonymous(spec profileCaptureSpec, kinds []string, deadline time.Time) []string {
	stamp := spec.stamp
	if stamp.IsZero() {
		stamp = time.Now()
	}
	prefix := profilePrefix(stamp, spec.generation, spec.runID)
	var written []string
	for _, kind := range kinds {
		if !time.Now().Before(deadline) {
			log.Printf("isutools: %s %s profile skipped: capture lease exceeded", kind, spec.point)
			break
		}
		name := profileBaseName(prefix, kind, spec.point) + profileArtifactExt
		profile := rpprof.Lookup(kind)
		if profile == nil {
			log.Printf("isutools: %s %s profile capture failed: unknown runtime profile", kind, spec.point)
			continue
		}
		if _, err := h.publishProfile(name, profile); err != nil {
			log.Printf("isutools: %s %s profile capture failed: %v", kind, spec.point, err)
			continue
		}
		written = append(written, name)
	}
	return written
}

// captureKind writes one profile and returns its record.
func (h *handler) captureKind(run *profileRun, spec profileCaptureSpec, kind string, deadline time.Time) ProfileCapture {
	capture := run.newCapture(spec, kind)
	if !time.Now().Before(deadline) {
		// The lease is a gate, not a timeout: WriteTo cannot be interrupted, so
		// the only thing that can be bounded is whether the next kind starts.
		capture.Code = profileCodeLeaseExceeded
		log.Printf("isutools: %s %s profile skipped: capture lease exceeded", kind, spec.point)
		return capture
	}
	profile := rpprof.Lookup(kind)
	if profile == nil {
		capture.Status, capture.Code = profileStatusFailed, profileCodeUnknownKind
		capture.Err = fmt.Sprintf("unknown runtime profile %q", kind)
		log.Printf("isutools: %s %s profile capture failed: %s", kind, spec.point, capture.Err)
		return capture
	}
	name := profileBaseName(run.prefix, kind, spec.point) + profileArtifactExt
	capture.StartedAt = time.Now()
	size, err := h.publishProfile(name, profile)
	capture.FinishedAt = time.Now()
	capture.LagFromRefNs = elapsedNs(spec.ref.at, capture.StartedAt)
	capture.DurationNs = capture.FinishedAt.Sub(capture.StartedAt).Nanoseconds()
	if err != nil {
		capture.Status, capture.Code, capture.Err = profileStatusFailed, profileCodeWriteFailed, err.Error()
		log.Printf("isutools: %s %s profile capture failed: %v", kind, spec.point, err)
		return capture
	}
	capture.Status, capture.File, capture.Bytes = profileStatusOK, name, size
	return capture
}

// orphan marks a run that will never be closed.
//
// The closing half is written as a record with no artifact, so a reader of the
// directory finds the reason the pair is missing instead of inferring it, and
// the opening records are rewritten as orphans. Retention drops them first.
func (h *handler) orphan(run *profileRun) {
	captures, prefix := run.markOrphan()
	for _, capture := range captures {
		h.writeProfileSidecar(capture)
	}
	if len(captures) == 0 {
		return
	}
	h.applyProfileHealth(run)
	h.pruneProfiles(prefix)
}

// markOrphan claims the closing point for good and returns every sidecar that
// has to be rewritten.
func (r *profileRun) markOrphan() ([]ProfileCapture, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.orphan || r.taken[ProfilePointClose] {
		return nil, r.prefix
	}
	r.orphan = true
	r.taken[ProfilePointClose] = true
	var changed []ProfileCapture
	for i := range r.captures {
		if r.captures[i].Point != ProfilePointOpen || r.captures[i].Status != profileStatusOK {
			continue
		}
		r.captures[i].Orphan = true
		changed = append(changed, r.captures[i])
		closing := ProfileCapture{
			RunID:    r.key.runID,
			Epoch:    r.key.epoch,
			Point:    ProfilePointClose,
			Kind:     r.captures[i].Kind,
			Sidecar:  profileBaseName(r.prefix, r.captures[i].Kind, ProfilePointClose) + profileSidecarExt,
			RefPhase: profileRefPhaseFinish,
			Status:   profileStatusSkipped,
			Code:     profileCodeAborted,
			Orphan:   true,
		}
		r.captures = append(r.captures, closing)
		changed = append(changed, closing)
	}
	return changed, r.prefix
}

// manifest assembles the run's record and the pairs that can be differenced.
func (r *profileRun) manifest() *ProfileManifest {
	r.mu.Lock()
	defer r.mu.Unlock()
	manifest := &ProfileManifest{
		RunID:    r.key.runID,
		Epoch:    r.key.epoch,
		Validity: r.validity,
		Captures: append([]ProfileCapture(nil), r.captures...),
	}
	span := elapsedNs(r.openRef.at, r.closeRef.at)
	for _, kind := range r.kindsLocked() {
		open := r.captureLocked(ProfilePointOpen, kind)
		closing := r.captureLocked(ProfilePointClose, kind)
		if open == nil || closing == nil {
			continue
		}
		pair := ProfilePair{
			Kind:          kind,
			OpenFile:      open.File,
			CloseFile:     closing.File,
			OpenGate:      open.OpenGate,
			RunSpanNs:     span,
			HeadLossNs:    open.LagFromRefNs,
			TailExcessNs:  closing.LagFromRefNs,
			ApproxErrorNs: open.LagFromRefNs + closing.LagFromRefNs,
			DiffCommand:   "go tool pprof -diff_base " + open.File + " " + closing.File,
		}
		manifest.Pairs = append(manifest.Pairs, pair)
	}
	return manifest
}

// incompleteKinds returns the kinds that captured one half and not the other.
//
// It answers nothing until the closing point has been decided: before then
// every kind is missing its second half, and that is not a defect but the
// normal state of a run in flight.
func (r *profileRun) incompleteKinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.taken[ProfilePointClose] {
		return nil
	}
	var kinds []string
	for _, kind := range r.kindsLocked() {
		if r.captureLocked(ProfilePointOpen, kind) == nil || r.captureLocked(ProfilePointClose, kind) == nil {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// kindsLocked lists the kinds this run touched, in capture order.
func (r *profileRun) kindsLocked() []string {
	var kinds []string
	seen := make(map[string]bool, len(r.captures))
	for _, capture := range r.captures {
		if !seen[capture.Kind] {
			seen[capture.Kind] = true
			kinds = append(kinds, capture.Kind)
		}
	}
	return kinds
}

// captureLocked returns the published capture for one kind at one point.
func (r *profileRun) captureLocked(point ProfilePoint, kind string) *ProfileCapture {
	for i := range r.captures {
		capture := &r.captures[i]
		if capture.Point == point && capture.Kind == kind && capture.Status == profileStatusOK {
			return capture
		}
	}
	return nil
}

// applyProfileHealth records this feature's three per-capture verdicts.
//
// Each is set on every capture, including when it is fine: a degrade left
// standing from an earlier run would describe conditions that no longer hold.
func (h *handler) applyProfileHealth(run *profileRun) {
	if h.p.Health == nil {
		return
	}
	manifest := run.manifest()
	var lagging, failed []string
	span := elapsedNs(run.openRef.at, run.closeRef.at)
	for _, capture := range manifest.Captures {
		switch {
		case capture.Status == profileStatusFailed:
			failed = append(failed, fmt.Sprintf("%s %s: %s", capture.Kind, capture.Point, capture.Err))
		case capture.Status != profileStatusOK:
			continue
		case capture.Point == ProfilePointClose && profileTailLagging(capture.LagFromRefNs, span),
			capture.Point == ProfilePointOpen && profileHeadLagging(capture.LagFromRefNs, capture.OpenGate):
			lagging = append(lagging, fmt.Sprintf("%s %s +%s",
				capture.Kind, capture.Point, profileLagText(capture.LagFromRefNs)))
		}
	}
	setProfileHealth(h.p.Health, healthProfileLag, lagging,
		"captured too far from the run boundary: ")
	setProfileHealth(h.p.Health, healthProfileFailed, failed, "capture failed: ")
	if incomplete := run.incompleteKinds(); len(incomplete) > 0 {
		h.p.Health.Set(healthProfileIncomplete, health.StatusDegraded,
			"no pair to difference for: "+strings.Join(incomplete, ", "))
	} else if manifest.Pairs != nil {
		h.p.Health.Set(healthProfileIncomplete, health.StatusOK, "")
	}
}

// setProfileHealth writes one verdict, degraded when there is anything to say.
func setProfileHealth(reg *health.Registry, key string, notes []string, prefix string) {
	if len(notes) == 0 {
		reg.Set(key, health.StatusOK, "")
		return
	}
	reg.Set(key, health.StatusDegraded, prefix+strings.Join(notes, "; "))
}

// writeProfileSidecar publishes one capture's record next to its artifact,
// through the same temporary-then-rename dance the artifact itself uses.
func (h *handler) writeProfileSidecar(capture ProfileCapture) {
	if h.p.DataDir == "" || capture.Sidecar == "" {
		return
	}
	body, err := json.MarshalIndent(capture, "", " ")
	if err != nil {
		log.Printf("isutools: profile sidecar encode failed: %v", err)
		return
	}
	final := filepath.Join(h.p.DataDir, capture.Sidecar)
	tmp := final + profileTempExt
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		// The write is create-truncate-write-close, so a failure anywhere after
		// the create leaves the file behind. Reap it here exactly as the rename
		// path below does: nothing else ever will. A ".tmp" is not an artifact,
		// so retention never groups it with a run, and it would otherwise sit in
		// the data directory for the life of the host.
		_ = os.Remove(tmp)
		log.Printf("isutools: profile sidecar write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		log.Printf("isutools: profile sidecar publish failed: %v", err)
	}
}

// writeRuntimeProfile publishes one profile atomically under the run's name.
func (h *handler) writeRuntimeProfile(kind string, run RunStart, point ProfilePoint, generation int64) (string, error) {
	profile := rpprof.Lookup(kind)
	if profile == nil {
		return "", fmt.Errorf("unknown runtime profile %q", kind)
	}
	name := profileArtifactName(run, point, generation, kind)
	if _, err := h.publishProfile(name, profile); err != nil {
		return "", err
	}
	return name, nil
}

// publishProfile writes one profile atomically and returns its size.
//
// The profile is written 0600 to a ".pprof.tmp" file and renamed only after a
// successful Close. /files/ serves neither that suffix nor a partial file, so
// a half-written profile is structurally impossible to download, and every
// failure path removes the temporary file rather than leaving it to be
// mistaken for one.
func (h *handler) publishProfile(name string, profile *rpprof.Profile) (int64, error) {
	final := filepath.Join(h.p.DataDir, name)
	tmp := final + profileTempExt
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", tmp, err)
	}
	if err := profile.WriteTo(f, 0); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("write %s: %w", tmp, err)
	}
	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("publish %s: %w", name, err)
	}
	return size, nil
}

// orderProfileKinds fixes the capture order at mutex, block, heap and drops
// duplicates.
//
// The order is cheapest first, so a large heap write cannot push the moment
// the mutex profile is taken further from the boundary. Kinds this package
// does not know are kept, after the known ones, in the order given.
func orderProfileKinds(kinds []string) []string {
	known := []string{"mutex", "block", "heap"}
	seen := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		seen[kind] = true
	}
	ordered := make([]string, 0, len(kinds))
	taken := make(map[string]bool, len(kinds))
	for _, kind := range known {
		if seen[kind] {
			ordered, taken[kind] = append(ordered, kind), true
		}
	}
	for _, kind := range kinds {
		if !taken[kind] {
			ordered, taken[kind] = append(ordered, kind), true
		}
	}
	return ordered
}

// profileArtifactName builds "<ts>_gen<N>_<runid8>_<kind>_<point>.pprof".
//
// The timestamp is the run's opening boundary at both points, never the
// moment of the write: an opening and a closing artifact of the same run must
// share a filename prefix, or the pair that the difference is taken over
// cannot be reassembled from a directory listing.
func profileArtifactName(run RunStart, point ProfilePoint, generation int64, kind string) string {
	stamp := run.StartedAt
	if stamp.IsZero() {
		stamp = time.Now()
	}
	return profileBaseName(profilePrefix(stamp, generation, run.RunID), kind, point) + profileArtifactExt
}

// profilePrefix builds the part of the name both halves share.
func profilePrefix(stamp time.Time, generation int64, runID string) string {
	return fmt.Sprintf("%s_gen%d_%s",
		stamp.In(reportTZ).Format("20060102-150405"), generation, runIDPrefix(runID))
}

// profileBaseName adds the kind and the point, which is all that distinguishes
// the files of one run.
func profileBaseName(prefix, kind string, point ProfilePoint) string {
	return prefix + "_" + sanitizeName(kind) + "_" + string(point)
}

// profileArtifactPrefix returns the run prefix of a boundary artifact or its
// sidecar, and whether the name is one at all.
//
// Everything else in the directory is left alone, which is the point: CPU
// profiles ("..._cpu.pprof"), saved snapshots, and the "_reset"/"_save"
// artifacts of the superseded naming all fail the test. The old names in
// particular must never be paired — they were captured at a different point,
// with a different meaning — and a directory holding both generations must
// still produce only correct pairs.
func profileArtifactPrefix(name string) (string, bool) {
	var base string
	switch {
	case strings.HasSuffix(name, profileSidecarExt):
		base = strings.TrimSuffix(name, profileSidecarExt)
	case strings.HasSuffix(name, profileArtifactExt):
		base = strings.TrimSuffix(name, profileArtifactExt)
	default:
		return "", false
	}
	cut := strings.LastIndex(base, "_")
	if cut < 0 {
		return "", false
	}
	switch ProfilePoint(base[cut+1:]) {
	case ProfilePointOpen, ProfilePointClose:
	default:
		return "", false
	}
	rest := base[:cut]
	cut = strings.LastIndex(rest, "_")
	if cut < 0 || !strings.Contains(rest[:cut], "_gen") {
		return "", false
	}
	return rest[:cut], true
}

// runIDPrefix shortens a run id to the leading 8 characters used in artifact
// names. Uniqueness comes from the whole prefix (timestamp, generation and
// id together), so the short form only has to make two runs of one second
// distinguishable.
func runIDPrefix(runID string) string {
	name := sanitizeName(runID)
	if name == "" {
		return "norun"
	}
	if len(name) > 8 {
		name = name[:8]
	}
	return name
}

// elapsedNs returns b-a in nanoseconds, and zero when either instant is
// unknown or the two are out of order.
//
// Residuals are distances from a boundary that has already happened, so a
// negative one is not a small number: it means the reference point is not what
// the record says it is, and reporting zero keeps a broken clock from
// presenting itself as a suspiciously good measurement.
func elapsedNs(a, b time.Time) int64 {
	if a.IsZero() || b.IsZero() || !b.After(a) {
		return 0
	}
	return b.Sub(a).Nanoseconds()
}

// pruneProfiles applies the retention contract to this handler's directory,
// never dropping the run that is being captured right now.
func (h *handler) pruneProfiles(protect string) {
	pruneProfileArtifacts(h.p.DataDir, profileRetentionRuns, profileRetentionBytes,
		protect, boundaryProfiles.invalidPrefixes(h.p.DataDir))
}

// profileGroup is one run's artifacts as they exist on disk.
type profileGroup struct {
	files    []string
	bytes    int64
	hasClose bool
}

// pruneProfileArtifacts enforces "the most recent runs, up to a total size".
//
// Files are dropped by run, artifact and sidecar together, so a record can
// never outlive the profile it describes. The order is by usefulness rather
// than only by age: an orphan can never be differenced, an invalid run's
// interval cannot be trusted, and only then does age decide.
func pruneProfileArtifacts(dir string, keepRuns int, keepBytes int64, protect string, invalid map[string]bool) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	groups := make(map[string]*profileGroup)
	var prefixes []string
	var total int64
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Stale temporary files are swept before the retention arithmetic and
		// regardless of its verdict: they are debris rather than a run's
		// artifacts, so keeping them until the directory happens to overflow
		// would leave a crash's leftovers behind forever.
		if strings.HasSuffix(entry.Name(), profileTempExt) {
			sweepStaleProfileTemp(dir, entry, now)
			continue
		}
		prefix, ok := profileArtifactPrefix(entry.Name())
		if !ok {
			continue
		}
		group := groups[prefix]
		if group == nil {
			group = &profileGroup{}
			groups[prefix] = group
			prefixes = append(prefixes, prefix)
		}
		group.files = append(group.files, entry.Name())
		if info, err := entry.Info(); err == nil {
			group.bytes += info.Size()
			total += info.Size()
		}
		if strings.HasSuffix(entry.Name(), "_"+string(ProfilePointClose)+profileArtifactExt) {
			group.hasClose = true
		}
	}
	// The prefix begins with a sortable timestamp, so this is oldest first.
	sort.Strings(prefixes)
	count := len(prefixes)
	if count <= keepRuns && total <= keepBytes {
		return
	}

	queued := make(map[string]bool, count)
	var queue []string
	for _, wanted := range []func(*profileGroup, string) bool{
		func(g *profileGroup, _ string) bool { return !g.hasClose },
		func(_ *profileGroup, prefix string) bool { return invalid[prefix] },
		func(*profileGroup, string) bool { return true },
	} {
		for _, prefix := range prefixes {
			if prefix == protect || queued[prefix] || !wanted(groups[prefix], prefix) {
				continue
			}
			queued[prefix] = true
			queue = append(queue, prefix)
		}
	}
	for _, prefix := range queue {
		if count <= keepRuns && total <= keepBytes {
			return
		}
		for _, name := range groups[prefix].files {
			_ = os.Remove(filepath.Join(dir, name))
		}
		count--
		total -= groups[prefix].bytes
	}
}

// sweepStaleProfileTemp removes one unpublished temporary file whose writer is
// gone.
//
// Only this feature's own temporaries are considered: the name is required to
// be a boundary artifact's or a sidecar's with the temporary suffix on top, so
// another component's half-written file in the same directory is left to its
// own owner. profileArtifactPrefix deliberately refuses a ".tmp" name — the
// pairing and the retention grouping must never see one — so the suffix is
// stripped before it is asked.
func sweepStaleProfileTemp(dir string, entry os.DirEntry, now time.Time) {
	if _, ok := profileArtifactPrefix(strings.TrimSuffix(entry.Name(), profileTempExt)); !ok {
		return
	}
	info, err := entry.Info()
	if err != nil || now.Sub(info.ModTime()) < profileTempMaxAge {
		return
	}
	_ = os.Remove(filepath.Join(dir, entry.Name()))
}

// BoundaryProfiler captures the opening half of a profile pair for a run that
// was opened outside this package's HTTP surface.
//
// It exists because POST /reset is not the only way a run begins. An
// application that follows the documented initialize contract opens its runs
// from its own handler through isutools.ResetNow and may never send a reset at
// all; without this entry point such a process publishes no opening artifact,
// and every closing capture finds nothing to pair with and writes nothing.
//
// Both entry points share one process-wide ledger keyed by the data directory,
// so a run opened here is closed by POST /finish or POST /save exactly as a run
// opened by POST /reset is, and the two halves are filed under one prefix.
type BoundaryProfiler struct{ h *handler }

// NewBoundaryProfiler returns the capturer for a provider's profile
// configuration, or nil when the configuration captures nothing — no data
// directory to publish into, or no profile kinds enabled.
//
// Only DataDir, RuntimeProfiles and Health are read; the capture path touches
// no collector and serves no request. The kinds are copied, so a caller that
// later rewrites its own slice cannot change what an opened run captures at its
// closing boundary.
func NewBoundaryProfiler(p Provider) *BoundaryProfiler {
	if p.DataDir == "" || len(p.RuntimeProfiles) == 0 {
		return nil
	}
	return &BoundaryProfiler{h: newHandler(Provider{
		DataDir:         p.DataDir,
		RuntimeProfiles: append([]string(nil), p.RuntimeProfiles...),
		Health:          p.Health,
	})}
}

// CaptureOpen writes the opening half of the run's profile pair and returns the
// artifacts it published.
//
// Call it the moment the coordinator returns the boundary and before the
// response that releases the benchmarker: the capture is synchronous for the
// same reason the reset path's is, because a moment chosen by the scheduler is
// not a boundary.
//
// A nil receiver captures nothing, so a caller never has to branch on whether
// profiling was configured. Nothing here is fatal.
func (b *BoundaryProfiler) CaptureOpen(run RunStart, generation int64) []string {
	if b == nil {
		return nil
	}
	return b.h.captureRuntimeProfiles(run, ProfilePointOpen, generation)
}

// listProfiles returns captured .pprof names, newest first.
func (h *handler) listProfiles() []string {
	if h.p.DataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(h.p.DataDir)
	if err != nil {
		return nil
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), profileArtifactExt) {
			names = append(names, e.Name())
		}
	}
	for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
		names[i], names[j] = names[j], names[i]
	}
	return names
}
