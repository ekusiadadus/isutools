package sqlrows

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

func TestCommittedMatrix(t *testing.T) {
	measurable := func() *fakeServer {
		server := newServer()
		server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
		return server
	}
	unsupported := func() *fakeServer {
		server := newServer()
		server.performance = int64(0)
		return server
	}

	cases := []struct {
		name          string
		targets       []string
		servers       map[string]*fakeServer
		expired       bool
		wantCommitted bool
		wantErr       error
	}{
		{
			name:    "one target measured, one unreadable",
			targets: []string{"db1", "db2"},
			servers: map[string]*fakeServer{
				"db1": measurable(),
				"db2": func() *fakeServer { s := newServer(); s.probeErr = errors.New("Access denied"); return s }(),
			},
			wantCommitted: true,
		},
		{
			name:          "every target skipped by its configuration",
			targets:       []string{"db1", "db2"},
			servers:       map[string]*fakeServer{"db1": unsupported(), "db2": unsupported()},
			wantCommitted: true,
		},
		{
			name:    "every target failed",
			targets: []string{"db1"},
			servers: map[string]*fakeServer{
				"db1": func() *fakeServer { s := newServer(); s.probeErr = errors.New("Access denied"); return s }(),
			},
			wantErr: ErrNoTargetCaptured,
		},
		{
			name:          "no targets registered at all",
			wantCommitted: true,
		},
		{
			name:    "the boundary budget was already gone",
			targets: []string{"db1"},
			servers: map[string]*fakeServer{"db1": measurable()},
			expired: true,
			wantErr: ErrNoTargetCaptured,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queriers := map[string]*fakeQuerier{}
			for id, server := range tc.servers {
				queriers[id] = server.querier()
			}
			c := testCollector(targetInfos("isuconp", tc.targets...), queriers)

			ctx := context.Background()
			if tc.expired {
				expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				ctx = expired
			}

			res, err := c.CaptureBaseline(ctx, "run-1", 1)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if res.Committed {
					t.Fatal("a failed boundary reported itself as committed")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Committed != tc.wantCommitted {
				t.Fatalf("committed = %v, want %v", res.Committed, tc.wantCommitted)
			}
			if res.Handle.Sample() == nil {
				t.Fatal("a committed boundary must carry a sample, even an empty one")
			}
			if !res.At.Equal(res.Handle.SampledAt) {
				t.Fatalf("At = %v but the handle says %v", res.At, res.Handle.SampledAt)
			}
		})
	}
}

// TestBoundaryReplayIsIdempotent fixes the contract that Committed is a
// statement about the world, not about the call: replaying a boundary returns
// the first answer instead of re-reading counters that have moved on.
func TestBoundaryReplayIsIdempotent(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	first, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	q.reset()
	// The counters move on between the two calls.
	q.answer(digestRows, digestRow("isuconp", "aaa", DigestRow{CountStar: 500, TimerWait: 500}))

	second, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("replayed CaptureBaseline: %v", err)
	}
	if stmts := q.statements(); len(stmts) != 0 {
		t.Fatalf("the replay issued %d statements: %v", len(stmts), stmts)
	}
	if !first.At.Equal(second.At) || first.Committed != second.Committed {
		t.Fatalf("replay returned %+v, want %+v", second, first)
	}
	if first.Handle.Sample() != second.Handle.Sample() {
		t.Fatal("the replay handed out a different sample")
	}
}

// TestFinalBoundaryReplayIsIdempotent covers the closing boundary separately,
// because it also carries the digest texts.
func TestFinalBoundaryReplayIsIdempotent(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	server.texts = [][]any{{"aaa", "SELECT ?"}}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	q.answer(digestRows, digestRow("isuconp", "aaa", DigestRow{CountStar: 4, TimerWait: 40}))
	first, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}
	q.reset()
	second, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("replayed CaptureFinal: %v", err)
	}
	if len(q.statements()) != 0 {
		t.Fatalf("the replay issued statements: %v", q.statements())
	}
	if first.Handle.Sample() != second.Handle.Sample() || !first.At.Equal(second.At) {
		t.Fatal("the closing boundary was re-sampled instead of replayed")
	}
}

// TestStaleEpochIsFenced keeps a displaced run from publishing.
func TestStaleEpochIsFenced(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	if _, err := c.CaptureBaseline(context.Background(), "run-2", 7); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	q.reset()

	res, err := c.CaptureFinal(context.Background(), "run-1", 3)
	if !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("err = %v, want %v", err, runctl.ErrStaleEpoch)
	}
	if res.Committed {
		t.Fatal("a fenced boundary reported itself as committed")
	}
	if stmts := q.statements(); len(stmts) != 0 {
		t.Fatalf("a fenced boundary touched the database: %v", stmts)
	}
}

// TestNewEpochEvictsTheDisplacedRun keeps a preempted run's baseline from
// living until the process exits.
func TestNewEpochEvictsTheDisplacedRun(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": server.querier()})

	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	if _, err := c.CaptureBaseline(context.Background(), "run-2", 2); err != nil {
		t.Fatalf("second CaptureBaseline: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pending[runKey{runID: "run-1", epoch: 1}]; ok {
		t.Fatal("the displaced run's baseline is still pinned")
	}
	if len(c.results) != 1 {
		t.Fatalf("results hold %d entries, want only the current run's", len(c.results))
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	c.Release(res.Handle)
	c.Release(res.Handle)
	c.Release(runctl.BaselineHandle{})

	c.mu.Lock()
	pending, results := len(c.pending), len(c.results)
	c.mu.Unlock()
	if pending != 0 || results != 0 {
		t.Fatalf("release left %d pending and %d results", pending, results)
	}

	// A released boundary is no longer cached, so the same key samples again.
	q.reset()
	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline after release: %v", err)
	}
	if len(q.statements()) == 0 {
		t.Fatal("the released boundary was replayed from a cache that should be gone")
	}
}

func TestCollectorIdentityAndBudget(t *testing.T) {
	c := New()
	if c.Name() != Name {
		t.Fatalf("name = %q, want %q", c.Name(), Name)
	}
	reg := Registration()
	if reg.Name != Name || reg.Required {
		t.Fatalf("registration = %+v, want an optional %q collector", reg, Name)
	}
	budget := c.Budget()
	if budget > runctl.PerCollectorBaselineBudget {
		t.Fatalf("budget %v exceeds the per-collector budget %v", budget, runctl.PerCollectorBaselineBudget)
	}
	if budget < runctl.PerTargetBudget {
		t.Fatalf("budget %v is below one target's budget %v", budget, runctl.PerTargetBudget)
	}
	// Two waves of BaselineConcurrency cover MaxTargets, which is what the
	// budget is derived from.
	if want := 2 * runctl.PerTargetBudget; budget != want {
		t.Fatalf("budget = %v, want %v", budget, want)
	}
}

func TestEnabled(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{value: "", want: true},
		{value: "on", want: true},
		{value: "1", want: true},
		{value: "off"},
		{value: "OFF"},
		{value: " off "},
		{value: "0"},
		{value: "false"},
		{value: "no"},
		{value: "disabled"},
	}
	for _, tc := range cases {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(EnvFlag, tc.value)
			if got := Enabled(); got != tc.want {
				t.Fatalf("Enabled() = %v with %s=%q, want %v", got, EnvFlag, tc.value, tc.want)
			}
		})
	}
}

// TestCollectorUsesTheStatsPurpose is the registry contract in unit form: the
// stats credential is what guarantees a connection without a default database.
func TestCollectorUsesTheStatsPurpose(t *testing.T) {
	var seen []sqlstats.Purpose
	c := New()
	c.targets = func() []sqlstats.TargetInfo { return targetInfos("isuconp", "db1") }
	c.inspect = func(_ context.Context, _ string, purpose sqlstats.Purpose, _ func(context.Context, sqlstats.Querier) error) error {
		seen = append(seen, purpose)
		return errors.New("not needed for this assertion")
	}
	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); !errors.Is(err, ErrNoTargetCaptured) {
		t.Fatalf("err = %v, want %v", err, ErrNoTargetCaptured)
	}
	if len(seen) != 1 || seen[0] != sqlstats.PurposeStats {
		t.Fatalf("inspected with %v, want [%q]", seen, sqlstats.PurposeStats)
	}
}

// TestUnknownTargetIsRecorded covers a target that disappeared from the
// registry between the two boundaries.
func TestUnknownTargetIsRecorded(t *testing.T) {
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{})
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if !errors.Is(err, ErrNoTargetCaptured) {
		t.Fatalf("err = %v, want %v", err, ErrNoTargetCaptured)
	}
	target := sampleOfResult(t, res).Targets["db1"]
	if target.Code != CodeQueryError || !strings.Contains(target.Err, "unknown target") {
		t.Fatalf("db1 = %+v, want the registry error recorded", target)
	}
}

// TestQuerySampleTextSupported hands the probe's finding to the query plan
// consumer without it having to probe again.
func TestQuerySampleTextSupported(t *testing.T) {
	modern := newServer()
	modern.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	old := newServer()
	old.columns = append([]string(nil), requiredColumns...)
	old.digests = modern.digests

	c := testCollector(targetInfos("isuconp", "db1", "db2"), map[string]*fakeQuerier{
		"db1": modern.querier(),
		"db2": old.querier(),
	})

	if supported, known := c.QuerySampleTextSupported("db1"); supported || known {
		t.Fatal("an unprobed target must report the answer as unknown")
	}
	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	if supported, known := c.QuerySampleTextSupported("db1"); !supported || !known {
		t.Fatalf("db1 = (%v, %v), want a known yes", supported, known)
	}
	if supported, known := c.QuerySampleTextSupported("db2"); supported || !known {
		t.Fatalf("db2 = (%v, %v), want a known no", supported, known)
	}
	if _, known := c.QuerySampleTextSupported("db-unregistered"); known {
		t.Fatal("an unknown target must not report a verdict")
	}

	// A target whose connection was refused for hygiene reasons never got as
	// far as the column list, so it has no verdict to hand on.
	dirty := newServer()
	dirty.defaultSchema = []byte("isuconp")
	d := testCollector(targetInfos("isuconp", "db3"), map[string]*fakeQuerier{"db3": dirty.querier()})
	if _, err := d.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	if supported, known := d.QuerySampleTextSupported("db3"); supported || known {
		t.Fatalf("db3 = (%v, %v), want no verdict from a connection that was never used", supported, known)
	}
}
