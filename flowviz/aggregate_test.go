package flowviz

import (
	"testing"
	"time"
)

func TestOrderedFunnelCountsSessionsDropoffRetriesLatencyAndErrors(t *testing.T) {
	agg := mustAggregator(t, Options{
		Enabled: true,
		Config: Config{Version: 1, Funnels: []FunnelDefinition{{
			ID: "checkout", Scenario: "buyer", Mode: ModeOrdered, Within: "2m",
			Steps: []StepDefinition{
				{ID: "list", Route: "GET /items"},
				{ID: "cart", Route: "POST /cart"},
				{ID: "done", Route: "POST /checkout"},
			},
		}}},
	})
	base := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	observe := func(session, page string, at time.Duration, latency time.Duration, status int) {
		method, route := splitPage(t, page)
		agg.Observe(Event{Session: session, Scenario: "buyer", Method: method, Route: route, At: base.Add(at), Latency: latency, Status: status})
	}

	observe("s1", "GET /items", 0, 10*time.Millisecond, 200)
	observe("s1", "GET /noise", time.Second, 2*time.Millisecond, 200) // ordered permits intervening routes
	observe("s1", "POST /cart", 2*time.Second, 20*time.Millisecond, 200)
	observe("s1", "POST /cart", 3*time.Second, 40*time.Millisecond, 500) // retry
	observe("s1", "POST /checkout", 4*time.Second, 80*time.Millisecond, 201)
	observe("s2", "GET /items", 0, 30*time.Millisecond, 200)
	observe("s2", "POST /cart", time.Second, 50*time.Millisecond, 429)
	observe("s3", "POST /cart", 0, 10*time.Millisecond, 200) // never entered

	snap := agg.Snapshot(nil)
	if snap.Status != StatusReady || snap.Partial || len(snap.Funnels) != 1 {
		t.Fatalf("snapshot = %#v", snap)
	}
	f := snap.Funnels[0]
	if f.Entered != 2 || f.Completed != 1 || f.ConversionBP != 5000 {
		t.Fatalf("funnel totals = %#v", f)
	}
	wants := []struct {
		sessions, requests, dropoff, retries, status4xx, status5xx int64
		fromStart, fromPrev                                        int64
	}{
		{2, 2, 0, 0, 0, 0, 10000, 10000},
		{2, 3, 1, 1, 1, 1, 10000, 10000},
		{1, 1, 0, 0, 0, 0, 5000, 5000},
	}
	for i, want := range wants {
		got := f.Steps[i]
		if got.Sessions != want.sessions || got.Requests != want.requests || got.DropOff != want.dropoff ||
			got.Retries != want.retries || got.Status4xx != want.status4xx || got.Status5xx != want.status5xx ||
			got.FromStartBP != want.fromStart || got.FromPreviousBP != want.fromPrev {
			t.Errorf("step %d = %#v, want %#v", i, got, want)
		}
	}
	if got := f.Steps[1].RequestP95; got < 40*time.Millisecond || got > 64*time.Millisecond {
		t.Errorf("cart p95 = %s", got)
	}
}

func TestFunnelWindowAndMissingClockAreExplicit(t *testing.T) {
	agg := mustAggregator(t, Options{Enabled: true, Config: Config{Version: 1, Funnels: []FunnelDefinition{{
		ID: "short", Scenario: "x", Mode: ModeOrdered, Within: "1s", Steps: validSteps(),
	}}}})
	base := time.Now()
	agg.Observe(Event{Session: "late", Scenario: "x", Method: "GET", Route: "/start", At: base})
	agg.Observe(Event{Session: "late", Scenario: "x", Method: "POST", Route: "/done", At: base.Add(2 * time.Second)})
	agg.Observe(Event{Session: "unknown-clock", Scenario: "x", Method: "GET", Route: "/start"})
	agg.Observe(Event{Session: "unknown-clock", Scenario: "x", Method: "POST", Route: "/done"})
	snap := agg.Snapshot(nil)
	if !snap.Partial || snap.TimingMissing != 2 {
		t.Fatalf("timing health = %#v", snap)
	}
	f := snap.Funnels[0]
	if f.Entered != 2 || f.Completed != 1 || f.Expired != 1 {
		t.Fatalf("window result = %#v", f)
	}
}

func TestCompletedFunnelIsNotLaterClassifiedAsExpired(t *testing.T) {
	agg := mustAggregator(t, Options{Enabled: true, Config: Config{Version: 1, Funnels: []FunnelDefinition{{
		ID: "short", Scenario: "x", Mode: ModeOrdered, Within: "1s", Steps: validSteps(),
	}}}})
	base := time.Now()
	agg.Observe(Event{Session: "complete", Scenario: "x", Method: "GET", Route: "/start", At: base})
	agg.Observe(Event{Session: "complete", Scenario: "x", Method: "POST", Route: "/done", At: base.Add(time.Second)})
	agg.Observe(Event{Session: "complete", Scenario: "x", Method: "POST", Route: "/done", At: base.Add(2 * time.Second)})

	funnel := agg.Snapshot(nil).Funnels[0]
	if funnel.Entered != 1 || funnel.Completed != 1 || funnel.Expired != 0 {
		t.Fatalf("completed funnel must not expire later: %#v", funnel)
	}
}

func TestSnapshotExpiresSilentIncompleteSessionAtLatestObservedTime(t *testing.T) {
	agg := mustAggregator(t, Options{Enabled: true, Config: Config{Version: 1, Funnels: []FunnelDefinition{{
		ID: "short", Scenario: "x", Mode: ModeOrdered, Within: "1s", Steps: validSteps(),
	}}}})
	base := time.Now()
	agg.Observe(Event{Session: "silent", Scenario: "x", Method: "GET", Route: "/start", At: base})
	agg.Observe(Event{Session: "clock", Scenario: "x", Method: "GET", Route: "/start", At: base.Add(2 * time.Second)})

	funnel := agg.Snapshot(nil).Funnels[0]
	if funnel.Entered != 2 || funnel.Completed != 0 || funnel.Expired != 1 {
		t.Fatalf("silent timeout must be explicit at snapshot: %#v", funnel)
	}
}

func TestFunnelSessionBoundIsReported(t *testing.T) {
	agg := mustAggregator(t, Options{Enabled: true, MaxSessions: 1, Config: Config{Version: 1, Funnels: []FunnelDefinition{{
		ID: "bounded", Scenario: "x", Mode: ModeOrdered, Steps: validSteps(),
	}}}})
	for _, session := range []string{"s1", "s2"} {
		agg.Observe(Event{Session: session, Scenario: "x", Method: "GET", Route: "/start"})
	}
	snap := agg.Snapshot(nil)
	if !snap.Partial || snap.SessionDropped != 1 || snap.Funnels[0].Entered != 1 {
		t.Fatalf("bounded snapshot = %#v", snap)
	}
}

func mustAggregator(t *testing.T, opts Options) *Aggregator {
	t.Helper()
	agg, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return agg
}

func splitPage(t *testing.T, page string) (string, string) {
	t.Helper()
	for i := range page {
		if page[i] == ' ' {
			return page[:i], page[i+1:]
		}
	}
	t.Fatalf("invalid page %q", page)
	return "", ""
}
