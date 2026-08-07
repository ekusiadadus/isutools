// Package timeline records bounded, run-aligned measurements and derives
// transparent correlation signals. It never claims that a suspect caused an
// outcome; every row carries the exact window, metric, formula and limitation
// that produced it.
package timeline

import "time"

const (
	SchemaV1 = "isutools.timeline/v1"

	DefaultInterval      = time.Second
	DefaultMaxBuckets    = 180
	DefaultMaxOperations = 32
	MaxSerializedBytes   = 2 << 20

	ReasonInsufficientBuckets = "insufficient-time-buckets"

	PhaseTrafficGrowth       = "traffic-growth"
	PhaseTailEscalation      = "tail-latency-escalation"
	PhaseSaturation          = "saturation"
	PhaseErrorOnset          = "error-onset"
	PhaseRecovery            = "recovery"
	PhaseBottleneckMigration = "bottleneck-migration"
)

type Config struct {
	Interval      time.Duration
	MaxBuckets    int
	MaxOperations int
}

type RunStart struct {
	RunID string
	Epoch uint64
	At    time.Time
}

type RunTermination struct {
	RunID    string
	Epoch    uint64
	At       time.Time
	Reason   string
	Validity string
}

type Section struct {
	Schema           string    `json:"schema"`
	RunID            string    `json:"run_id"`
	Epoch            uint64    `json:"epoch"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at,omitzero"`
	IntervalNs       int64     `json:"interval_ns"`
	MaxBuckets       int       `json:"max_buckets"`
	MaxOperations    int       `json:"max_operations"`
	Truncated        bool      `json:"truncated,omitempty"`
	OverflowedEvents uint64    `json:"overflowed_events,omitempty"`
	Validity         string    `json:"validity,omitempty"`
	StopReason       string    `json:"stop_reason,omitempty"`
	Outcome          Outcome   `json:"outcome,omitzero"`
	Buckets          []Bucket  `json:"buckets"`
	Analysis         Analysis  `json:"analysis"`
}

// Outcome records only values supplied by the benchmark integration. Pass is
// a pointer so "not supplied" remains distinct from a failing result.
type Outcome struct {
	Score    string `json:"score,omitempty"`
	Pass     *bool  `json:"pass,omitempty"`
	Validity string `json:"measurement_validity,omitempty"`
}

type Bucket struct {
	Index           int            `json:"index"`
	Start           time.Time      `json:"start"`
	End             time.Time      `json:"end"`
	HTTPInFlightMax int64          `json:"http_in_flight_max"`
	HTTP            []Operation    `json:"http,omitempty"`
	SQL             []Operation    `json:"sql,omitempty"`
	DBPools         []PoolBucket   `json:"dbpool,omitempty"`
	Process         *ProcessBucket `json:"process,omitempty"`
	Host            *HostBucket    `json:"host,omitempty"`
}

type Operation struct {
	Key       string `json:"key"`
	Count     int64  `json:"count"`
	Successes int64  `json:"successes"`
	Errors    int64  `json:"errors"`
	TotalNs   int64  `json:"total_ns"`
	MaxNs     int64  `json:"max_ns"`
	P95Ns     int64  `json:"p95_ns"`
}

type PoolPoint struct {
	TargetID     string
	MaxOpen      int
	Open         int
	InUse        int
	Idle         int
	WaitCount    int64
	WaitDuration time.Duration
}

type ProcessPoint struct {
	TotalJiffies   uint64
	BusyJiffies    uint64
	IOWaitJiffies  uint64
	ProcessJiffies uint64
	CPUs           int
	RSSBytes       uint64
}

type HostPoint struct {
	ReadBytes  uint64
	WriteBytes uint64
	IOTicks    time.Duration
	WeightedIO time.Duration
}

type ResourceSample struct {
	At      time.Time
	Pools   []PoolPoint
	Process *ProcessPoint
	Host    *HostPoint
}

type PoolBucket struct {
	TargetID       string `json:"target_id"`
	MaxOpen        int    `json:"max_open"`
	Open           int    `json:"open"`
	InUse          int    `json:"in_use"`
	Idle           int    `json:"idle"`
	WaitCount      int64  `json:"wait_count"`
	WaitDurationNs int64  `json:"wait_duration_ns"`
}

type ProcessBucket struct {
	BusyPercent       float64 `json:"busy_percent"`
	IOWaitPercent     float64 `json:"iowait_percent"`
	ProcessCPUPercent float64 `json:"process_cpu_percent"`
	RSSBytes          uint64  `json:"rss_bytes"`
}

type HostBucket struct {
	ReadBytes          uint64  `json:"read_bytes"`
	WriteBytes         uint64  `json:"write_bytes"`
	DiskBusyPercentSum float64 `json:"disk_busy_percent_sum"`
	WeightedIOPercent  float64 `json:"weighted_io_percent"`
}

type Analysis struct {
	Available bool      `json:"available"`
	Reason    string    `json:"reason,omitempty"`
	Rules     []Rule    `json:"rules,omitempty"`
	Phases    []Phase   `json:"phases,omitempty"`
	Suspects  []Suspect `json:"suspects,omitempty"`
}

type Rule struct {
	ID         string `json:"id"`
	Formula    string `json:"formula"`
	Limitation string `json:"limitation"`
}

type Phase struct {
	Kind        string        `json:"kind"`
	WindowStart time.Time     `json:"window_start"`
	WindowEnd   time.Time     `json:"window_end"`
	RuleID      string        `json:"rule_id"`
	Evidence    []EvidenceRef `json:"evidence"`
}

type Suspect struct {
	Signal   string        `json:"signal"`
	Key      string        `json:"key"`
	Kind     string        `json:"kind"`
	Label    string        `json:"label"`
	Score    int           `json:"score"`
	Evidence []EvidenceRef `json:"evidence"`
}

type EvidenceRef struct {
	BucketIndex int       `json:"bucket_index"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Signal      string    `json:"signal"`
	Metric      string    `json:"metric"`
	Value       float64   `json:"value"`
	Formula     string    `json:"formula"`
	Limitation  string    `json:"limitation"`
}
