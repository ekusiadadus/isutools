package isutools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/internal/timeline"
	"github.com/ekusiadadus/isutools/procstats"
)

const (
	envTimeline              = "ISUTOOLS_TIMELINE"
	envTimelineInterval      = "ISUTOOLS_TIMELINE_INTERVAL"
	envTimelineBuckets       = "ISUTOOLS_TIMELINE_BUCKETS"
	envTimelineMaxOperations = "ISUTOOLS_TIMELINE_MAX_OPERATIONS"
	envTimelineSafeRoutes    = "ISUTOOLS_TIMELINE_SAFE_ROUTE_RULES"
	healthTimeline           = "timeline"
)

// timelineRuntime binds exact request/query events and low-frequency resource
// sampling to one run epoch. Its lifecycle callback only cancels a context and
// freezes in-memory data; it performs no procfs IO on the controller path.
type timelineRuntime struct {
	collector *timeline.Collector
	interval  time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	runID  string
	epoch  uint64
	proc   *procstats.Collector
	host   *hoststats.Collector
	pools  *dbpool.Collector
}

var _ runctl.LifecycleObserver = (*timelineRuntime)(nil)

func newTimelineRuntime(getenv func(string) string) *timelineRuntime {
	if getenv == nil {
		return nil
	}
	value := strings.ToLower(strings.TrimSpace(getenv(envTimeline)))
	if value == "" || value == "0" || value == "off" || value == "false" || value == "no" {
		collectorHealth.Set(healthTimeline, health.StatusDisabled, envTimeline+" is off")
		return nil
	}
	if value != "1" && value != "on" && value != "true" && value != "yes" {
		collectorHealth.Set(healthTimeline, health.StatusDegraded, fmt.Sprintf("unknown %s=%q; timeline disabled", envTimeline, value))
		return nil
	}
	cfg := timeline.Config{}
	var err error
	if value := strings.TrimSpace(getenv(envTimelineInterval)); value != "" {
		cfg.Interval, err = time.ParseDuration(value)
		if err != nil {
			collectorHealth.Set(healthTimeline, health.StatusDegraded, fmt.Sprintf("invalid %s; timeline disabled", envTimelineInterval))
			return nil
		}
	}
	if cfg.MaxBuckets, err = parseOptionalPositiveInt(getenv(envTimelineBuckets)); err != nil {
		collectorHealth.Set(healthTimeline, health.StatusDegraded, fmt.Sprintf("invalid %s; timeline disabled", envTimelineBuckets))
		return nil
	}
	if cfg.MaxOperations, err = parseOptionalPositiveInt(getenv(envTimelineMaxOperations)); err != nil {
		collectorHealth.Set(healthTimeline, health.StatusDegraded, fmt.Sprintf("invalid %s; timeline disabled", envTimelineMaxOperations))
		return nil
	}
	collector, err := timeline.New(cfg)
	if err != nil {
		collectorHealth.Set(healthTimeline, health.StatusDegraded, err.Error())
		return nil
	}
	if cfg.Interval == 0 {
		cfg.Interval = timeline.DefaultInterval
	}
	collectorHealth.Set(healthTimeline, health.StatusOK, "")
	return &timelineRuntime{collector: collector, interval: cfg.Interval}
}

func parseOptionalPositiveInt(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("must be a positive integer")
	}
	return parsed, nil
}

func (r *timelineRuntime) setSources(proc *procstats.Collector, host *hoststats.Collector, pools *dbpool.Collector) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.proc, r.host, r.pools = proc, host, pools
	r.mu.Unlock()
}

func (r *timelineRuntime) start(result runctl.StartResult) {
	if r == nil || r.collector == nil || result.RunID == "" || result.Epoch == 0 {
		return
	}
	startedAt := result.StartedAt
	if startedAt.IsZero() {
		startedAt = cpuStartBoundary(result)
	}
	accepted := r.collector.Start(timeline.RunStart{RunID: result.RunID, Epoch: uint64(result.Epoch), At: startedAt})
	if result.State != runctl.StateStarted ||
		(result.Validity != runctl.ValidityValid && result.Validity != runctl.ValidityPartial) || !accepted {
		return
	}
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.runID, r.epoch = cancel, result.RunID, uint64(result.Epoch)
	r.mu.Unlock()
	go r.sampleLoop(ctx, result.RunID, uint64(result.Epoch), startedAt)
}

func (r *timelineRuntime) sampleLoop(ctx context.Context, runID string, epoch uint64, startedAt time.Time) {
	r.sample(ctx, startedAt)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			r.mu.Lock()
			current := r.runID == runID && r.epoch == epoch
			r.mu.Unlock()
			if !current {
				return
			}
			r.sample(ctx, at)
		}
	}
}

func (r *timelineRuntime) sample(parent context.Context, at time.Time) {
	r.mu.Lock()
	proc, host, pools := r.proc, r.host, r.pools
	r.mu.Unlock()
	sample := timeline.ResourceSample{At: at}
	if pools != nil {
		for _, point := range pools.Points() {
			sample.Pools = append(sample.Pools, timeline.PoolPoint{
				TargetID: point.TargetID, MaxOpen: point.MaxOpen, Open: point.Open,
				InUse: point.InUse, Idle: point.Idle, WaitCount: point.WaitCount, WaitDuration: point.WaitDuration,
			})
		}
	}
	if proc != nil {
		if point, err := proc.TimelinePoint(); err == nil {
			sample.Process = &timeline.ProcessPoint{
				TotalJiffies: point.TotalJiffies, BusyJiffies: point.BusyJiffies,
				IOWaitJiffies: point.IOWaitJiffies, ProcessJiffies: point.ProcessJiffies,
				CPUs: point.CPUs, RSSBytes: point.RSSBytes,
			}
		}
	}
	if host != nil {
		budget := r.interval / 2
		if budget > 100*time.Millisecond {
			budget = 100 * time.Millisecond
		}
		if budget <= 0 {
			budget = 50 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(parent, budget)
		point, err := host.Point(ctx)
		cancel()
		if err == nil {
			sample.Host = &timeline.HostPoint{
				ReadBytes: point.ReadBytes, WriteBytes: point.WriteBytes,
				IOTicks: point.IOTicks, WeightedIO: point.WeightedIO,
			}
		}
	}
	r.collector.Tick(at)
	r.collector.Sample(sample)
}

func (r *timelineRuntime) OnRunTermination(event runctl.RunTerminationEvent) {
	if r == nil || r.collector == nil {
		return
	}
	r.mu.Lock()
	if r.runID == event.RunID && r.epoch == uint64(event.Epoch) {
		if r.cancel != nil {
			r.cancel()
		}
		r.cancel, r.runID, r.epoch = nil, "", 0
	}
	r.mu.Unlock()
	r.collector.Terminate(timeline.RunTermination{
		RunID: event.RunID, Epoch: uint64(event.Epoch), At: event.BoundaryAt,
		Reason: event.Reason, Validity: string(event.Validity),
	})
}

func (r *timelineRuntime) section(runID string, epoch uint64) *timeline.Section {
	if r == nil || r.collector == nil || runID == "" || epoch == 0 {
		return nil
	}
	section, ok := r.collector.Section(runID, epoch)
	if !ok {
		return nil
	}
	return &section
}

func (r *timelineRuntime) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.cancel, r.runID, r.epoch = nil, "", 0
	r.mu.Unlock()
}
