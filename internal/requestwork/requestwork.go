// Package requestwork attributes completed SQL and outbound HTTP work to one request.
package requestwork

import (
	"context"
	"sort"
	"sync"
	"time"
)

const (
	MaxIntervals   = 256
	MaxLabels      = 32
	MaxLabelLength = 64
	OverflowLabel  = "(other external calls)"
)

type contextKey struct{}
type interval struct {
	start, end time.Time
	external   bool
}

// Call contains aggregate data for one static caller supplied label. Phase
// durations can overlap and must not be added to obtain Total.
type Call struct {
	Label   string        `json:"label"`
	Count   int64         `json:"count"`
	Errors  int64         `json:"errors"`
	Reused  int64         `json:"reused"`
	Total   time.Duration `json:"total_ns"`
	Acquire time.Duration `json:"acquire_ns"`
	DNS     time.Duration `json:"dns_ns"`
	Connect time.Duration `json:"connect_ns"`
	TLS     time.Duration `json:"tls_ns"`
	Wait    time.Duration `json:"wait_ns"`
	Headers time.Duration `json:"headers_ns"`
	Body    time.Duration `json:"body_ns"`
}

type Summary struct {
	SQLTotal       time.Duration `json:"sql_total_ns"`
	ExternalTotal  time.Duration `json:"external_total_ns"`
	SQLOnly        time.Duration `json:"sql_only_ns"`
	ExternalOnly   time.Duration `json:"external_only_ns"`
	Overlap        time.Duration `json:"overlap_ns"`
	Other          time.Duration `json:"other_ns"`
	ExternalCount  int64         `json:"external_count"`
	ExternalErrors int64         `json:"external_errors"`
	Truncated      bool          `json:"truncated"`
	Calls          []Call        `json:"calls,omitempty"`
}

// External is a completed transport attempt. Failed includes transport, body
// read or Close errors at completion, and HTTP status 400 or higher. Errors
// after the first completion (for example Close after EOF) are not counted.
type External struct {
	Label                                           string
	Start, End                                      time.Time
	Failed, Reused                                  bool
	Acquire, DNS, Connect, TLS, Wait, Headers, Body time.Duration
}

type Counter struct {
	mu        sync.Mutex
	closed    bool
	summary   Summary
	intervals []interval
	labels    map[string]int
}

func WithCounter(ctx context.Context) (context.Context, *Counter) {
	c := &Counter{}
	return context.WithValue(ctx, contextKey{}, c), c
}

func counterFrom(ctx context.Context) *Counter {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(contextKey{}).(*Counter)
	return c
}

// Enabled permits instrumentation to avoid trace and wrapper allocation when
// no request-local counter is attached.
func Enabled(ctx context.Context) bool { return counterFrom(ctx) != nil }

func CompletedSQL(ctx context.Context, start, end time.Time) {
	c := counterFrom(ctx)
	if c == nil || !end.After(start) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.summary.SQLTotal += end.Sub(start)
	c.addInterval(start, end, false)
}

func CompletedExternal(ctx context.Context, e External) {
	c := counterFrom(ctx)
	if c == nil || !e.End.After(e.Start) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	d := e.End.Sub(e.Start)
	s := &c.summary
	s.ExternalTotal += d
	s.ExternalCount++
	if e.Failed {
		s.ExternalErrors++
	}
	c.addInterval(e.Start, e.End, true)
	label := e.Label
	if len(label) > MaxLabelLength {
		label = label[:MaxLabelLength]
	}
	if c.labels == nil {
		c.labels = make(map[string]int)
	}
	i, found := c.labels[label]
	if !found {
		if len(c.labels) >= MaxLabels {
			label = OverflowLabel
			i, found = c.labels[label]
		}
		if !found {
			i = len(s.Calls)
			c.labels[label] = i
			s.Calls = append(s.Calls, Call{Label: label})
		}
	}
	call := &s.Calls[i]
	call.Count++
	if e.Failed {
		call.Errors++
	}
	if e.Reused {
		call.Reused++
	}
	call.Total += d
	call.Acquire += e.Acquire
	call.DNS += e.DNS
	call.Connect += e.Connect
	call.TLS += e.TLS
	call.Wait += e.Wait
	call.Headers += e.Headers
	call.Body += e.Body
}

func (c *Counter) addInterval(start, end time.Time, external bool) {
	if c.summary.Truncated {
		return
	}
	if len(c.intervals) >= MaxIntervals {
		c.summary.Truncated = true
		c.intervals = nil
		return
	}
	c.intervals = append(c.intervals, interval{start, end, external})
}

// Close freezes the counter. Wall-time fields partition [start,end); when the
// interval budget is exceeded all wall-time fields are zero and Truncated is
// true. A repeated Close returns the same snapshot.
func (c *Counter) Close(start, end time.Time) Summary {
	if c == nil {
		return Summary{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return clone(c.summary)
	}
	c.closed = true
	if !c.summary.Truncated && end.After(start) {
		type edge struct {
			t        time.Time
			sql, ext int
		}
		edges := make([]edge, 0, len(c.intervals)*2+2)
		edges = append(edges, edge{t: start}, edge{t: end})
		for _, v := range c.intervals {
			a, b := v.start, v.end
			if a.Before(start) {
				a = start
			}
			if b.After(end) {
				b = end
			}
			if !b.After(a) {
				continue
			}
			if v.external {
				edges = append(edges, edge{t: a, ext: 1}, edge{t: b, ext: -1})
			} else {
				edges = append(edges, edge{t: a, sql: 1}, edge{t: b, sql: -1})
			}
		}
		sort.Slice(edges, func(i, j int) bool { return edges[i].t.Before(edges[j].t) })
		var sql, ext int
		for i := 0; i < len(edges)-1; i++ {
			sql += edges[i].sql
			ext += edges[i].ext
			d := edges[i+1].t.Sub(edges[i].t)
			if d <= 0 {
				continue
			}
			switch {
			case sql > 0 && ext > 0:
				c.summary.Overlap += d
			case sql > 0:
				c.summary.SQLOnly += d
			case ext > 0:
				c.summary.ExternalOnly += d
			default:
				c.summary.Other += d
			}
		}
	}
	return clone(c.summary)
}

func clone(s Summary) Summary { s.Calls = append([]Call(nil), s.Calls...); return s }
