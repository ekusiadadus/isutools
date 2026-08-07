package hoststats

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// TimelinePoint is a cumulative, whole-device disk reading for bucket deltas.
type TimelinePoint struct {
	ReadBytes  uint64
	WriteBytes uint64
	IOTicks    time.Duration
	WeightedIO time.Duration
}

// Point reads only diskstats for the optional high-frequency timeline. It does
// not invoke statfs, PSI, identity or cgroup reads from the boundary collector.
func (c *Collector) Point(ctx context.Context) (TimelinePoint, error) {
	if c == nil {
		return TimelinePoint{}, fmt.Errorf("hoststats: nil collector")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return TimelinePoint{}, err
	}
	data, err := readFile(c.opt.ProcFS, pathDiskstats)
	if err != nil {
		return TimelinePoint{}, err
	}
	disks, err := parseDiskstats(data, wholeDeviceFilter(c.opt.SysFS))
	if err != nil {
		return TimelinePoint{}, err
	}
	var point TimelinePoint
	for _, disk := range disks {
		readBytes, ok := sectorBytes(disk.ReadSectors)
		if !ok {
			return TimelinePoint{}, fmt.Errorf("hoststats: disk read byte counter overflows")
		}
		writeBytes, ok := sectorBytes(disk.WriteSectors)
		if !ok {
			return TimelinePoint{}, fmt.Errorf("hoststats: disk write byte counter overflows")
		}
		ioTicks, ok := millisecondsDuration(disk.IOTicksMS)
		if !ok {
			return TimelinePoint{}, fmt.Errorf("hoststats: disk io time counter overflows")
		}
		weighted, ok := millisecondsDuration(disk.WeightedMS)
		if !ok {
			return TimelinePoint{}, fmt.Errorf("hoststats: disk weighted time counter overflows")
		}
		if point.ReadBytes > math.MaxUint64-readBytes || point.WriteBytes > math.MaxUint64-writeBytes ||
			point.IOTicks > time.Duration(math.MaxInt64)-ioTicks ||
			point.WeightedIO > time.Duration(math.MaxInt64)-weighted {
			return TimelinePoint{}, fmt.Errorf("hoststats: aggregate disk counter overflows")
		}
		point.ReadBytes += readBytes
		point.WriteBytes += writeBytes
		point.IOTicks += ioTicks
		point.WeightedIO += weighted
	}
	return point, nil
}

func millisecondsDuration(milliseconds uint64) (time.Duration, bool) {
	if milliseconds > uint64(math.MaxInt64/int64(time.Millisecond)) {
		return 0, false
	}
	return time.Duration(milliseconds) * time.Millisecond, true
}

func sectorBytes(sectors uint64) (uint64, bool) {
	if sectors > math.MaxUint64/512 {
		return 0, false
	}
	return sectors * 512, true
}

// A Collector is registered through runctl.RegisterBaseline, so the contract
// is asserted here rather than discovered at the call site.
var _ runctl.BaselineCollector = (*Collector)(nil)

// CaptureBaseline samples the opening boundary.
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.capture(ctx, runID, ep, runctl.PhaseStartBaseline)
}

// CaptureFinal samples the closing boundary.
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.capture(ctx, runID, ep, runctl.PhaseFinishFinal)
}

// capture takes one boundary sample.
//
// Committed answers "is a sample fixed for this run", not "did this call take
// one", so a retry replays the first answer including its timestamp. Only two
// things prevent a sample: an already-expired budget, and an unreadable
// /proc/meminfo. Everything else degrades into per-source codes, because a
// host section missing its disks is still worth far more than no host section.
//
// The budget is checked before the lock and the lock is never held across a
// read. Sampling reaches statfs(2), which on a wedged NFS or fuse mount blocks
// uninterruptibly for as long as the mount does: holding c.mu across it would
// park every other boundary behind a syscall that no deadline can cut short.
// Idempotency is enforced at commit time instead — the first sample stored
// under a key is the one every caller gets, timestamp included.
func (c *Collector) capture(ctx context.Context, runID string, ep runctl.Epoch, phase runctl.Phase) (runctl.SampleResult, error) {
	if err := ctx.Err(); err != nil {
		return runctl.SampleResult{At: c.opt.Now()}, fmt.Errorf("hoststats: %s: %w", phase, err)
	}

	key := runKey{RunID: runID, Epoch: ep, Phase: phase}
	fixed, done, err := c.begin(key)
	if err != nil || done {
		return fixed, err
	}

	sample, err := c.readSample(ctx, phase)
	if err != nil {
		return runctl.SampleResult{At: c.opt.Now()}, err
	}
	return c.commit(key, sample)
}

// begin fences the epoch and reports a sample that is already fixed for this
// boundary. It holds the lock for the map work only.
func (c *Collector) begin(key runKey) (result runctl.SampleResult, done bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if key.Epoch < c.epoch {
		return runctl.SampleResult{At: c.opt.Now()}, true,
			fmt.Errorf("%w: hoststats: epoch %d, current %d", runctl.ErrStaleEpoch, key.Epoch, c.epoch)
	}
	if key.Epoch > c.epoch {
		c.epoch = key.Epoch
		c.pruneLocked(key.Epoch)
	}
	fixed, ok := c.results[key]
	return fixed, ok, nil
}

// commit publishes a freshly read sample, or hands back the one another caller
// committed first.
//
// The loser of that race drops its own reading rather than replacing the
// stored one: a boundary must be a single instant, and a second sample would
// silently move the point the whole run is measured against. A newer epoch
// arriving mid-read fences the sample for the same reason it fences a late
// call — the controller has abandoned that run.
func (c *Collector) commit(key runKey, sample *Sample) (runctl.SampleResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if key.Epoch < c.epoch {
		return runctl.SampleResult{At: sample.At},
			fmt.Errorf("%w: hoststats: epoch %d, current %d", runctl.ErrStaleEpoch, key.Epoch, c.epoch)
	}
	if fixed, ok := c.results[key]; ok {
		return fixed, nil
	}
	result := runctl.SampleResult{
		Handle:    runctl.NewBaselineHandle(key.RunID, key.Epoch, CollectorName, key.Phase, sample.At, sample.clone()),
		At:        sample.At,
		Committed: true,
	}
	c.results[key] = result
	c.samples[key] = sample
	return result, nil
}

// Release drops the sample a handle pins. It is idempotent, and it is safe to
// call before Collect: the handle carries its own deep copy, so releasing the
// collector's bookkeeping cannot take data away from an interval.
func (c *Collector) Release(h runctl.BaselineHandle) {
	if h.Zero() || h.Collector != CollectorName {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := runKey{RunID: h.RunID, Epoch: h.Epoch, Phase: h.Phase}
	delete(c.samples, key)
	delete(c.results, key)
}

// pruneLocked forgets superseded epochs. A preempted run's samples can never
// be asked for again — the epoch that would name them is fenced — so keeping
// them would be a leak with a run's worth of data in it.
func (c *Collector) pruneLocked(current runctl.Epoch) {
	for key := range c.results {
		if key.Epoch < current {
			delete(c.results, key)
			delete(c.samples, key)
		}
	}
}

// readSample reads every source for one boundary.
//
// /proc/meminfo comes first and is required: it is the cheapest proof that
// procfs is readable at all, and the timestamp taken just before it defines
// the boundary as a single instant rather than the span of every read that
// follows. Each later source is preceded by a budget check, so an exhausted
// context truncates the sample instead of failing it.
func (c *Collector) readSample(ctx context.Context, phase runctl.Phase) (*Sample, error) {
	sample := &Sample{Phase: phase, At: c.opt.Now()}

	data, err := readFile(c.opt.ProcFS, pathMeminfo)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoSource, err)
	}
	mem, err := parseMeminfo(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoSource, err)
	}
	sample.Mem = mem

	// Identity is read unconditionally. It is six tiny reads, and without it a
	// sample cannot be compared with its partner at the other boundary or with
	// any other host's.
	sample.Identity = c.readIdentity()

	sample.CGroupSkip = c.cgroup.skip
	c.readSource(ctx, sample, SourceVMStat, func() error {
		data, err := readFile(c.opt.ProcFS, pathVMStat)
		if err != nil {
			return err
		}
		faults, err := parseVMStat(data)
		if err != nil {
			return err
		}
		sample.MajFault = faults
		sample.HasMajFault = true
		return nil
	})
	c.readSource(ctx, sample, SourceDiskstats, func() error {
		data, err := readFile(c.opt.ProcFS, pathDiskstats)
		if err != nil {
			return err
		}
		disks, err := parseDiskstats(data, wholeDeviceFilter(c.opt.SysFS))
		if err != nil {
			return err
		}
		sample.Disks = disks
		return nil
	})
	c.readSource(ctx, sample, SourcePSI, func() error {
		psi, err := readPSI(c.opt.ProcFS)
		if err != nil {
			return err
		}
		sample.PSI = psi
		return nil
	})
	c.readSource(ctx, sample, SourceCGroup, func() error {
		raw, err := readCGroupRaw(c.cgroup)
		if err != nil {
			return err
		}
		sample.CGroup = raw
		return nil
	})
	// statfs is read last, and with a deadline of its own making. It is the
	// only source that can block on something other than the kernel's own
	// memory: a wedged NFS or fuse mount makes statfs(2) hang for as long as
	// the mount is wedged. Reading it after the cheap procfs sources means such
	// a mount costs exactly one code — its own — instead of also taking down
	// every source that would have been read after it.
	c.readSource(ctx, sample, SourceStatfs, func() error {
		filesystems, err := readFilesystemsWithin(ctx, c.opt.Statfs, c.opt.DataDir)
		if err != nil {
			return err
		}
		sample.FS = filesystems
		return nil
	})
	return sample, nil
}

// readSource runs one optional source and records a skip code when the budget
// is gone or the source is unreadable. The two are recorded identically on
// purpose: from the snapshot's point of view "we ran out of time" and "this
// kernel has no PSI" are the same statement — this number is not here.
func (c *Collector) readSource(ctx context.Context, sample *Sample, source string, read func() error) {
	if err := ctx.Err(); err != nil {
		sample.skip(source)
		return
	}
	if err := read(); err != nil {
		sample.skip(source)
	}
}
