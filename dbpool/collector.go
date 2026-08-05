package dbpool

import (
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// Name is the collector name and the key of its snapshot section.
const Name = "dbpool"

// Health keys this package reports through Notes. They are stable strings:
// the admin UI and the multi-host agent match on them.
const (
	// HealthNotRegistered reports that no pool was ever watched, which is why
	// the snapshot has no DB Pool section.
	HealthNotRegistered = "dbpool-not-registered"
	// HealthRegisteredMidRun reports a pool watched after the run's opening
	// boundary. It is measured from the next run onwards.
	HealthRegisteredMidRun = "dbpool-registered-mid-run"
	// HealthUnwatchedMidRun reports a pool unwatched before the run's closing
	// boundary. Its entry survives with a shortened interval.
	HealthUnwatchedMidRun = "dbpool-unwatched-mid-run"
	// HealthSampleFailed reports a pool whose Stats implementation panicked,
	// which drops it from that boundary rather than from the process.
	HealthSampleFailed = "dbpool-sample-failed"
)

// maxNotes bounds the note buffer. Notes exist to explain a degraded section,
// not to be a log; an unbounded slice fed by a misbehaving caller would be a
// leak in the measured process.
const maxNotes = 32

var _ runctl.BaselineCollector = (*Collector)(nil)

// Default is the process-wide collector that isutools registers with the run
// Controller and that WatchDBPool feeds. Tests build their own with New.
var Default = New()

// watched is one entry of the watch set. The pool is held as a bound Stats
// method value rather than as a *sql.DB: it is the only operation this package
// is ever allowed to perform on the application's handle, and deleting the map
// entry drops the collector's last reference to a pool the application may be
// about to close.
type watched struct {
	display string
	stats   func() sql.DBStats
}

// runSamples caches one run's boundary samples so that repeating a capture for
// the same (runID, epoch) returns the identical SampleResult, timestamp
// included.
type runSamples struct {
	runID string
	epoch runctl.Epoch
	// active is the run's watch set, frozen at the opening boundary. A pool
	// watched later is deliberately absent from it: giving it a baseline taken
	// after the run started would report a fraction of the interval as if it
	// were the whole of it.
	active map[string]struct{}

	base      *runctl.SampleResult
	baseTaken bool

	final      *runctl.SampleResult
	finalTaken bool
}

// Collector watches connection pools and turns two boundary samples into a
// per-pool interval report.
//
// One mutex covers the watch set, the farewell samples and the run cache, so
// that a WatchDBPool racing an opening boundary produces a watch set that is
// either wholly before or wholly after the boundary — never half of each.
type Collector struct {
	mu       sync.Mutex
	watch    map[string]watched
	farewell Sample
	run      *runSamples
	notes    []string
	noteSeen map[string]struct{}
	// everWatched distinguishes a missing WatchDBPool integration from an
	// intentional empty watch set after every pool was unwatched.
	everWatched bool

	// now is the clock, injectable so tests can assert that a farewell sample
	// is stamped earlier than the closing boundary.
	now func() time.Time
}

// New returns an empty collector.
func New() *Collector {
	return &Collector{
		watch:    map[string]watched{},
		farewell: Sample{},
		noteSeen: map[string]struct{}{},
		now:      time.Now,
	}
}

// Name identifies the snapshot section this collector fills.
func (c *Collector) Name() string { return Name }

// Watch adds a pool to the watch set under an existing TargetID.
//
// Only IDs already present in the registry are accepted, compared byte for
// byte: no case folding, no trimming, no Unicode normalization. Watch never
// creates a target, because a typo that silently created a second target would
// split one database across two rows of every report. Auto-derived IDs end in
// a 26 character hash and cannot be spelled out by hand — obtain them from
// sqlstats.TargetIDForDSN, or name the target explicitly with RegisterDBTarget
// first.
//
// The pool becomes part of the *next* run: see CaptureBaseline.
func (c *Collector) Watch(targetID string, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("%w: target %q", ErrNilDB, targetID)
	}
	info, ok := sqlstats.Target(targetID)
	if !ok {
		return fmt.Errorf("%w: %q — register it with RegisterDBTarget, or look the id up with TargetIDForDSN",
			sqlstats.ErrUnknownTarget, targetID)
	}
	return c.watchStats(targetID, info.Display, db.Stats)
}

// watchStats is the registry-free core of Watch. Watch resolves the ID and
// Display and hands over db.Stats, which keeps the public API restricted to
// *sql.DB — an arbitrary interface would let a typed nil, a blocking
// implementation or a panicking one into the boundary path — while still
// letting tests drive the delta arithmetic with a scripted sampler.
func (c *Collector) watchStats(targetID, display string, stats func() sql.DBStats) error {
	if stats == nil {
		return fmt.Errorf("%w: target %q", ErrNilDB, targetID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.watch[targetID]; dup {
		return fmt.Errorf("%w: %q — unwatch it before watching a replacement pool", ErrDuplicatePool, targetID)
	}
	if len(c.watch) >= MaxPools {
		return fmt.Errorf("%w: %q was not added, the limit is %d", ErrTooManyPools, targetID, MaxPools)
	}
	c.watch[targetID] = watched{display: display, stats: stats}
	c.everWatched = true
	if c.midRunLocked() {
		if _, active := c.run.active[targetID]; !active {
			c.noteLocked(fmt.Sprintf("%s: %q joined after the run started; it is measured from the next reset onwards",
				HealthRegisteredMidRun, targetID))
		}
	}
	return nil
}

// Unwatch removes a pool from the watch set.
//
// It is idempotent: a registered target that is not watched is a no-op. An
// unregistered ID is still an error, because it means the caller is naming
// something that does not exist rather than undoing something it did.
//
// If the run is in progress and this pool is part of it, Unwatch takes a
// farewell sample first. Dropping the entry instead would make the pool vanish
// from a report it genuinely contributed to; keeping it without a final sample
// would require reading a *sql.DB the application is about to close.
func (c *Collector) Unwatch(targetID string) error {
	if _, ok := sqlstats.Target(targetID); !ok {
		return fmt.Errorf("%w: %q", sqlstats.ErrUnknownTarget, targetID)
	}
	c.unwatchStats(targetID)
	return nil
}

// unwatchStats is the registry-free core of Unwatch. It reports whether the
// target was watched.
func (c *Collector) unwatchStats(targetID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.watch[targetID]
	if !ok {
		return false
	}
	delete(c.watch, targetID)
	if !c.midRunLocked() {
		return true
	}
	if _, active := c.run.active[targetID]; !active {
		return true
	}
	at := c.now()
	stats, sampled := safeStats(w.stats)
	if !sampled {
		c.noteLocked(fmt.Sprintf("%s: %q panicked while taking its farewell sample; it is dropped from this run",
			HealthSampleFailed, targetID))
		return true
	}
	c.farewell[targetID] = PoolSample{Stats: stats, At: at, Display: w.display, Unwatched: true}
	c.noteLocked(fmt.Sprintf("%s: %q left the watch set at %s; its interval is shorter than the run",
		HealthUnwatchedMidRun, targetID, at.UTC().Format(time.RFC3339Nano)))
	return true
}

// Watched returns the currently watched TargetIDs in ascending order. isutools
// uses it to decide whether the DB Pool section exists at all.
func (c *Collector) Watched() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.watch))
	for id := range c.watch {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Notes returns the degradation notes recorded so far. Each note starts with
// the health key it belongs to, so the caller can forward it verbatim.
func (c *Collector) Notes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.notes))
	copy(out, c.notes)
	return out
}

// midRunLocked reports whether a run has an opening boundary but no closing
// one yet. The caller holds c.mu.
func (c *Collector) midRunLocked() bool {
	return c.run != nil && c.run.baseTaken && !c.run.finalTaken
}

// noteLocked records a note once. The caller holds c.mu.
func (c *Collector) noteLocked(message string) {
	if _, seen := c.noteSeen[message]; seen {
		return
	}
	if len(c.notes) >= maxNotes {
		return
	}
	c.noteSeen[message] = struct{}{}
	c.notes = append(c.notes, message)
}
