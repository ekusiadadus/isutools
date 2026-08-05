package netstats

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// collectorName is the section this collector fills. The package is netstats
// but the collector is "network": the run coordinator's registry names
// sections after what they describe, and that list is the authority.
const collectorName = "network"

// Default is the collector isutools registers. It reads the real /proc and
// /sys; on a host where either is absent, capture fails and the section is
// simply missing from the run.
var Default = New(os.DirFS("/proc"), os.DirFS("/sys"))

// sample is one boundary's frozen observation. Everything in it is built at
// capture time and never written again: handles are copied and shared, so a
// later mutation would silently change an interval that was supposed to be
// fixed.
type sample struct {
	TCP   TCPSummary
	Devs  map[string]devCounters
	Link  map[string]linkAttrs
	Notes []HealthNote
}

// cacheKey makes a capture idempotent per (run, epoch, phase), so a retried
// boundary replays the first answer instead of sampling twice.
type cacheKey struct {
	runID string
	epoch runctl.Epoch
	phase runctl.Phase
}

// Collector implements runctl.BaselineCollector for network observations.
//
// procFS and sysFS are separate filesystems on purpose: /proc/net/dev and
// /sys/class/net live on different mounts, and a single injected root cannot
// reach both.
type Collector struct {
	procFS fs.FS
	sysFS  fs.FS
	now    func() time.Time

	mu      sync.Mutex
	epoch   runctl.Epoch
	samples map[cacheKey]runctl.SampleResult
}

// New returns a collector reading network state from the given filesystems.
// Pass os.DirFS("/proc") and os.DirFS("/sys") in production and fstest.MapFS
// in tests. A nil filesystem is tolerated and surfaces as a degraded section
// rather than a panic, because measurement may not break the application.
func New(procFS, sysFS fs.FS) *Collector {
	return &Collector{
		procFS:  procFS,
		sysFS:   sysFS,
		now:     time.Now,
		samples: make(map[cacheKey]runctl.SampleResult),
	}
}

// Name identifies the snapshot section this collector fills.
func (c *Collector) Name() string { return collectorName }

// CaptureBaseline samples the opening boundary.
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.capture(ctx, runID, ep, runctl.PhaseStartBaseline)
}

// CaptureFinal samples the closing boundary.
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.capture(ctx, runID, ep, runctl.PhaseFinishFinal)
}

// Release drops the collector's reference to a handle's sample. It is
// idempotent: releasing twice, or releasing a handle this collector never
// issued, is a no-op.
func (c *Collector) Release(h runctl.BaselineHandle) {
	if h.Zero() {
		return
	}
	c.mu.Lock()
	delete(c.samples, cacheKey{runID: h.RunID, epoch: h.Epoch, phase: h.Phase})
	c.mu.Unlock()
}

// capture samples one boundary. It never returns a zero SampleResult: even on
// failure the caller needs the measured moment and an explicit Committed, so
// that a retry and the run record agree on what happened.
func (c *Collector) capture(ctx context.Context, runID string, ep runctl.Epoch, phase runctl.Phase) (runctl.SampleResult, error) {
	if err := ctx.Err(); err != nil {
		return runctl.SampleResult{At: c.now()}, fmt.Errorf("netstats: %s of run %s: %w", phase, runID, err)
	}
	key := cacheKey{runID: runID, epoch: ep, phase: phase}
	if cached, done, err := c.lookup(key, ep); done {
		return cached, err
	}

	// The moment is taken before the reads so that At marks the start of the
	// observation window rather than a point somewhere inside it.
	at := c.now()
	s, err := c.readSample()
	if err != nil {
		return runctl.SampleResult{At: at}, err
	}
	result := runctl.SampleResult{
		Handle:    runctl.NewBaselineHandle(runID, ep, collectorName, phase, at, s),
		At:        at,
		Committed: true,
	}
	return c.store(key, ep, result)
}

// lookup replays a cached sample and fences stale epochs. done reports whether
// the caller must return immediately.
func (c *Collector) lookup(key cacheKey, ep runctl.Epoch) (runctl.SampleResult, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ep < c.epoch {
		return runctl.SampleResult{At: c.now()}, true,
			fmt.Errorf("netstats: epoch %d is older than %d: %w", ep, c.epoch, runctl.ErrStaleEpoch)
	}
	if cached, ok := c.samples[key]; ok {
		return cached, true, nil
	}
	return runctl.SampleResult{}, false, nil
}

// store publishes a freshly taken sample, re-checking the epoch because the
// read happened outside the lock.
func (c *Collector) store(key cacheKey, ep runctl.Epoch, result runctl.SampleResult) (runctl.SampleResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ep < c.epoch {
		return runctl.SampleResult{At: result.At},
			fmt.Errorf("netstats: epoch %d is older than %d: %w", ep, c.epoch, runctl.ErrStaleEpoch)
	}
	if cached, ok := c.samples[key]; ok {
		// Another caller won the race for this boundary. Replaying its answer
		// keeps (runID, epoch, phase) idempotent down to the timestamp.
		return cached, nil
	}
	if ep > c.epoch {
		c.epoch = ep
		c.evictOlderThan(ep)
	}
	c.samples[key] = result
	return result, nil
}

// evictOlderThan drops samples belonging to superseded epochs so a long
// session of aborted runs cannot accumulate frozen samples.
func (c *Collector) evictOlderThan(ep runctl.Epoch) {
	for key := range c.samples {
		if key.epoch < ep {
			delete(c.samples, key)
		}
	}
}

// readSample reads both /proc/net files and the sysfs attributes of every
// interface it found.
//
// Only an unreadable /proc/net/dev is fatal: without it there is no interface
// table and nothing to display. A broken sockstat or an unreadable sysfs
// degrades into a health note and the rest of the section is still produced.
func (c *Collector) readSample() (*sample, error) {
	s := &sample{
		Devs: make(map[string]devCounters),
		Link: make(map[string]linkAttrs),
	}
	if c.procFS == nil {
		return nil, errors.New("netstats: procfs is not injected")
	}
	devData, err := fs.ReadFile(c.procFS, "net/dev")
	if err != nil {
		return nil, fmt.Errorf("netstats: read net/dev: %w", err)
	}
	devices, problems := parseNetDev(devData)
	s.Devs = devices
	for _, problem := range problems {
		s.note(HealthProcUnreadable, problem)
	}

	c.readSockstat(s)
	c.readLinks(s)
	return s, nil
}

// readSockstat fills the point observation. A missing sockstat6 is silent:
// hosts with IPv6 disabled simply do not have the file, and reporting that
// every run would make the health list useless.
func (c *Collector) readSockstat(s *sample) {
	if data, err := fs.ReadFile(c.procFS, "net/sockstat"); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.note(HealthProcUnreadable, "net/sockstat="+truncateErr(err.Error()))
		}
	} else if tcp, parseErr := parseSockstat(data); parseErr != nil {
		s.note(HealthProcUnreadable, "net/sockstat="+truncateErr(parseErr.Error()))
	} else {
		s.TCP = tcp
	}

	if data, err := fs.ReadFile(c.procFS, "net/sockstat6"); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.note(HealthProcUnreadable, "net/sockstat6="+truncateErr(err.Error()))
		}
	} else if inUse6, parseErr := parseSockstat6(data); parseErr != nil {
		s.note(HealthProcUnreadable, "net/sockstat6="+truncateErr(parseErr.Error()))
	} else {
		s.TCP.InUse6 = inUse6
	}
}

// readLinks reads speed and MTU for every interface in the sample. They are
// read here, at capture time, and not later during Collect: Collect is
// required to be pure, so an attribute not read now can never be read at all.
func (c *Collector) readLinks(s *sample) {
	names := make([]string, 0, len(s.Devs))
	for name := range s.Devs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		attrs, err := readLinkAttrs(c.sysFS, name)
		if err != nil {
			// Missing sysfs is a wiring fault, identical for every interface;
			// reporting it once is enough.
			s.note(HealthSysfsUnreadable, "sysfs="+truncateErr(err.Error()))
			return
		}
		for _, note := range attrs.Notes {
			s.note(HealthSysfsUnreadable, note)
		}
		s.Link[name] = linkAttrs{SpeedMbit: attrs.SpeedMbit, MTU: attrs.MTU}
	}
}

// note appends a degradation to the sample under construction. It is only
// called before the sample is handed to a handle.
func (s *sample) note(key, detail string) {
	s.Notes = append(s.Notes, HealthNote{Key: key, Detail: detail})
}
