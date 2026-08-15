package flowviz

import (
	"math/bits"
	"sync"
	"time"
)

const latencyBuckets = 256

type Event struct {
	Session  string
	Scenario string
	Method   string
	Route    string
	Status   int
	Latency  time.Duration
	At       time.Time
}

type Snapshot struct {
	Status         string           `json:"status"`
	Reason         string           `json:"reason,omitempty"`
	Partial        bool             `json:"partial,omitempty"`
	SessionDropped int64            `json:"session_dropped,omitempty"`
	TimingMissing  int64            `json:"timing_missing,omitempty"`
	Funnels        []FunnelSnapshot `json:"funnels,omitempty"`
	Graph          GraphSnapshot    `json:"graph"`
}

type FunnelSnapshot struct {
	ID           string               `json:"id"`
	Scenario     string               `json:"scenario"`
	Mode         string               `json:"mode"`
	Within       string               `json:"within,omitempty"`
	Entered      int64                `json:"entered"`
	Completed    int64                `json:"completed"`
	Expired      int64                `json:"expired,omitempty"`
	ConversionBP int64                `json:"conversion_basis_points"`
	Steps        []FunnelStepSnapshot `json:"steps"`
}

type FunnelStepSnapshot struct {
	ID             string        `json:"id"`
	Route          string        `json:"route"`
	Sessions       int64         `json:"sessions"`
	Requests       int64         `json:"requests"`
	DropOff        int64         `json:"drop_off"`
	Retries        int64         `json:"retries"`
	Status4xx      int64         `json:"status_4xx"`
	Status5xx      int64         `json:"status_5xx"`
	RequestP95     time.Duration `json:"request_p95_ns"`
	FromStartBP    int64         `json:"from_start_basis_points"`
	FromPreviousBP int64         `json:"from_previous_basis_points"`
}

type compiledFunnel struct {
	definition FunnelDefinition
	within     time.Duration
	routeIndex map[string]int
	steps      []stepStat
	entered    int64
	completed  int64
	expired    int64
}

type stepStat struct {
	sessions  int64
	requests  int64
	retries   int64
	status4xx int64
	status5xx int64
	latencyN  int64
	maxNS     int64
	buckets   [latencyBuckets]int64
}

type sessionState struct {
	funnel    int
	next      int
	started   time.Time
	completed bool
	expired   bool
}

type Aggregator struct {
	mu             sync.Mutex
	enabled        bool
	maxNodes       int
	maxEdges       int
	maxSessions    int
	funnels        []compiledFunnel
	byScenario     map[string][]int
	sessions       map[string]*sessionState
	sessionDropped int64
	timingMissing  int64
	latestObserved time.Time
}

func New(options Options) (*Aggregator, error) {
	opts, err := options.normalized()
	if err != nil {
		return nil, err
	}
	a := &Aggregator{enabled: opts.Enabled}
	if !opts.Enabled {
		return a, nil
	}
	a.maxNodes, a.maxEdges, a.maxSessions = opts.MaxNodes, opts.MaxEdges, opts.MaxSessions
	a.byScenario = make(map[string][]int)
	a.sessions = make(map[string]*sessionState)
	for _, definition := range opts.Config.Funnels {
		within, _ := parseWindow(definition.Within)
		compiled := compiledFunnel{
			definition: definition, within: within,
			routeIndex: make(map[string]int, len(definition.Steps)),
			steps:      make([]stepStat, len(definition.Steps)),
		}
		for i, step := range definition.Steps {
			compiled.routeIndex[step.Route] = i
		}
		a.funnels = append(a.funnels, compiled)
		idx := len(a.funnels) - 1
		a.byScenario[definition.Scenario] = append(a.byScenario[definition.Scenario], idx)
	}
	return a, nil
}

func (a *Aggregator) Enabled() bool { return a != nil && a.enabled }

func (a *Aggregator) Observe(event Event) {
	if a == nil || !a.enabled || !safeSession(event.Session) || !safeIDPattern.MatchString(event.Scenario) {
		return
	}
	page := event.Method + " " + event.Route
	if len(page) > MaxNodeBytes || !routePattern.MatchString(page) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !event.At.IsZero() && event.At.After(a.latestObserved) {
		a.latestObserved = event.At
	}
	for _, funnelIndex := range a.byScenario[event.Scenario] {
		a.observeFunnel(funnelIndex, page, event)
	}
}

func (a *Aggregator) observeFunnel(funnelIndex int, page string, event Event) {
	funnel := &a.funnels[funnelIndex]
	stepIndex, isStep := funnel.routeIndex[page]
	if !isStep {
		return
	}
	key := funnel.definition.ID + "\x00" + event.Session
	state := a.sessions[key]
	if state == nil {
		if stepIndex != 0 {
			return
		}
		if len(a.sessions) >= a.maxSessions {
			a.sessionDropped++
			return
		}
		state = &sessionState{funnel: funnelIndex, next: 1, started: event.At}
		a.sessions[key] = state
		funnel.entered++
		a.acceptStep(&funnel.steps[0], event, false)
		if funnel.within > 0 && event.At.IsZero() {
			a.timingMissing++
		}
		return
	}
	if state.funnel != funnelIndex || state.expired {
		return
	}
	if funnel.within > 0 {
		if event.At.IsZero() || state.started.IsZero() {
			a.timingMissing++
		} else if event.At.Before(state.started) {
			a.timingMissing++
		} else if event.At.Sub(state.started) > funnel.within {
			state.expired = true
			if !state.completed {
				funnel.expired++
			}
			return
		}
	}
	switch {
	case stepIndex < state.next:
		a.acceptStep(&funnel.steps[stepIndex], event, true)
	case stepIndex == state.next:
		a.acceptStep(&funnel.steps[stepIndex], event, false)
		state.next++
		if state.next == len(funnel.steps) {
			state.completed = true
			funnel.completed++
		}
	default:
		// A later step before its predecessor is intentionally ignored. The
		// configured order, not route-name inference, defines conversion.
	}
}

func (a *Aggregator) acceptStep(step *stepStat, event Event, retry bool) {
	step.requests++
	if retry {
		step.retries++
	} else {
		step.sessions++
	}
	if event.Status >= 400 && event.Status < 500 {
		step.status4xx++
	} else if event.Status >= 500 {
		step.status5xx++
	}
	if event.Latency >= 0 {
		ns := event.Latency.Nanoseconds()
		step.latencyN++
		if ns > step.maxNS {
			step.maxNS = ns
		}
		step.buckets[latencyBucket(ns)]++
	}
}

func (a *Aggregator) Snapshot(transitions []Transition) Snapshot {
	if a == nil || !a.enabled {
		return Snapshot{Status: StatusDisabled, Reason: "flow-viz-off"}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireSilentSessions()
	result := Snapshot{
		Status: StatusReady, SessionDropped: a.sessionDropped, TimingMissing: a.timingMissing,
		Funnels: make([]FunnelSnapshot, 0, len(a.funnels)),
		Graph:   BuildGraph(transitions, a.maxNodes, a.maxEdges),
	}
	for _, funnel := range a.funnels {
		row := FunnelSnapshot{
			ID: funnel.definition.ID, Scenario: funnel.definition.Scenario, Mode: funnel.definition.Mode,
			Within: funnel.definition.Within, Entered: funnel.entered, Completed: funnel.completed,
			Expired: funnel.expired, ConversionBP: basisPoints(funnel.completed, funnel.entered),
			Steps: make([]FunnelStepSnapshot, len(funnel.steps)),
		}
		for i, stat := range funnel.steps {
			dropOff := int64(0)
			if i+1 < len(funnel.steps) {
				dropOff = stat.sessions - funnel.steps[i+1].sessions
				if dropOff < 0 {
					dropOff = 0
				}
			}
			previous := funnel.entered
			if i > 0 {
				previous = funnel.steps[i-1].sessions
			}
			row.Steps[i] = FunnelStepSnapshot{
				ID: funnel.definition.Steps[i].ID, Route: funnel.definition.Steps[i].Route,
				Sessions: stat.sessions, Requests: stat.requests, DropOff: dropOff, Retries: stat.retries,
				Status4xx: stat.status4xx, Status5xx: stat.status5xx,
				RequestP95:  time.Duration(stepPercentile(stat, 95)),
				FromStartBP: basisPoints(stat.sessions, funnel.entered), FromPreviousBP: basisPoints(stat.sessions, previous),
			}
		}
		result.Funnels = append(result.Funnels, row)
	}
	result.Partial = result.SessionDropped > 0 || result.TimingMissing > 0 || result.Graph.Partial
	if result.Partial {
		result.Status = StatusPartial
		result.Reason = "bounded-or-incomplete-flow-data"
	}
	return result
}

func (a *Aggregator) Reset() {
	if a == nil || !a.enabled {
		return
	}
	a.mu.Lock()
	for i := range a.funnels {
		a.funnels[i].steps = make([]stepStat, len(a.funnels[i].definition.Steps))
		a.funnels[i].entered, a.funnels[i].completed, a.funnels[i].expired = 0, 0, 0
	}
	a.sessions = make(map[string]*sessionState)
	a.sessionDropped, a.timingMissing = 0, 0
	a.latestObserved = time.Time{}
	a.mu.Unlock()
}

func (a *Aggregator) expireSilentSessions() {
	if a.latestObserved.IsZero() {
		return
	}
	for _, state := range a.sessions {
		if state.completed || state.expired || state.started.IsZero() || state.funnel < 0 || state.funnel >= len(a.funnels) {
			continue
		}
		funnel := &a.funnels[state.funnel]
		if funnel.within > 0 && a.latestObserved.After(state.started) && a.latestObserved.Sub(state.started) > funnel.within {
			state.expired = true
			funnel.expired++
		}
	}
}

// CloneSnapshot returns a deep copy suitable for immutable run handoff.
func CloneSnapshot(value *Snapshot) *Snapshot {
	if value == nil {
		return nil
	}
	result := *value
	result.Funnels = make([]FunnelSnapshot, len(value.Funnels))
	for i, funnel := range value.Funnels {
		result.Funnels[i] = funnel
		result.Funnels[i].Steps = append([]FunnelStepSnapshot(nil), funnel.Steps...)
	}
	result.Graph.Nodes = append([]GraphNode(nil), value.Graph.Nodes...)
	result.Graph.Edges = append([]GraphEdge(nil), value.Graph.Edges...)
	return &result
}

func safeSession(value string) bool {
	return value != "" && len(value) <= 128 && safeIDPattern.MatchString(value)
}

func basisPoints(numerator, denominator int64) int64 {
	if numerator <= 0 || denominator <= 0 {
		return 0
	}
	if numerator >= denominator {
		return 10000
	}
	return (numerator*10000 + denominator/2) / denominator
}

func latencyBucket(ns int64) int {
	if ns <= 0 {
		return 0
	}
	exponent := bits.Len64(uint64(ns)) - 1
	base := uint64(1) << uint(exponent)
	sub := int((uint64(ns) - base) * 4 / base)
	if sub > 3 {
		sub = 3
	}
	index := 1 + exponent*4 + sub
	if index >= latencyBuckets {
		return latencyBuckets - 1
	}
	return index
}

func latencyBucketUpper(index int) int64 {
	if index <= 0 {
		return 0
	}
	exponent := (index - 1) / 4
	sub := (index - 1) % 4
	if exponent >= 62 {
		return int64(^uint64(0) >> 1)
	}
	base := uint64(1) << uint(exponent)
	return int64(base + base*uint64(sub+1)/4)
}

func stepPercentile(step stepStat, percentile int64) int64 {
	if step.latencyN == 0 {
		return 0
	}
	target := (step.latencyN*percentile + 99) / 100
	seen := int64(0)
	for i, count := range step.buckets {
		seen += count
		if count > 0 && seen >= target {
			upper := latencyBucketUpper(i)
			if upper > step.maxNS {
				return step.maxNS
			}
			return upper
		}
	}
	return step.maxNS
}
