package sqlrows

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

func TestDBClockOrdering(t *testing.T) {
	t0 := baseTime

	cases := []struct {
		name                                           string
		baseBefore, baseAfter, finalBefore, finalAfter time.Time
		wantMonotonic                                  bool
		wantAnomaly                                    string
	}{
		{
			name:       "monotonic",
			baseBefore: t0, baseAfter: t0.Add(time.Millisecond),
			finalBefore: t0.Add(time.Minute), finalAfter: t0.Add(time.Minute + time.Millisecond),
			wantMonotonic: true,
		},
		{
			name:       "equal readings are allowed",
			baseBefore: t0, baseAfter: t0, finalBefore: t0, finalAfter: t0,
			wantMonotonic: true,
		},
		{
			name:       "a reading was never taken",
			baseBefore: t0, baseAfter: t0.Add(time.Millisecond),
			finalBefore: time.Time{}, finalAfter: t0.Add(time.Minute),
			wantAnomaly: AnomalyMissing,
		},
		{
			name:       "the opening boundary stepped backwards",
			baseBefore: t0.Add(time.Second), baseAfter: t0,
			finalBefore: t0.Add(time.Minute), finalAfter: t0.Add(time.Minute + time.Millisecond),
			wantAnomaly: AnomalyBackwardsBaseline,
		},
		{
			name:       "the closing boundary stepped backwards",
			baseBefore: t0, baseAfter: t0.Add(time.Millisecond),
			finalBefore: t0.Add(time.Minute), finalAfter: t0.Add(time.Second),
			wantAnomaly: AnomalyBackwardsFinal,
		},
		{
			name:       "the interval itself is inverted",
			baseBefore: t0, baseAfter: t0.Add(time.Minute),
			finalBefore: t0.Add(58 * time.Second), finalAfter: t0.Add(59 * time.Second),
			wantAnomaly: AnomalyBackwardsInterval,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := &TargetSample{UTCBefore: tc.baseBefore, UTCAfter: tc.baseAfter}
			final := &TargetSample{UTCBefore: tc.finalBefore, UTCAfter: tc.finalAfter}

			clock := newDBClock(base, final)
			if clock.Monotonic != tc.wantMonotonic {
				t.Fatalf("monotonic = %v, want %v", clock.Monotonic, tc.wantMonotonic)
			}
			if clock.Anomaly != tc.wantAnomaly {
				t.Fatalf("anomaly = %q, want %q", clock.Anomaly, tc.wantAnomaly)
			}
			if clock.Monotonic && clock.skew() != 0 {
				t.Fatalf("a monotonic clock reported a skew of %v", clock.skew())
			}
			if !clock.Monotonic && tc.wantAnomaly != AnomalyMissing && clock.skew() >= 0 {
				t.Fatalf("skew = %v, want a negative step", clock.skew())
			}
		})
	}
}

// TestDBClockOrderingComesFromTheDatabase drives the four readings through the
// scripted connection, so the ordering rule is verified on values that took
// the same path a real UTC_TIMESTAMP(6) would.
func TestDBClockOrderingComesFromTheDatabase(t *testing.T) {
	baseServer := newServer()
	baseServer.before = baseTime
	baseServer.after = baseTime.Add(time.Minute)
	baseServer.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}

	q := baseServer.querier()
	infos := targetInfos("isuconp", "db1")
	c := testCollector(infos, map[string]*fakeQuerier{"db1": q})

	base, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// The database clock steps backwards between the boundaries: the closing
	// boundary starts before the opening one finished.
	q.answer(metaPFS, []any{baseServer.uuid, baseServer.uptime, baseTime.Add(30 * time.Second)})
	q.answer(clockAfter, []any{baseTime.Add(31 * time.Second)})
	q.answer(digestRows, digestRow("isuconp", "aaa", DigestRow{CountStar: 9, TimerWait: 90}))

	final, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}

	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	section := value.(*Section)
	target := findTarget(t, section, "db1")
	if target.DBClock.Monotonic {
		t.Fatalf("clock = %+v, want the backwards step reported", target.DBClock)
	}
	if target.DBClock.Anomaly != AnomalyBackwardsInterval {
		t.Fatalf("anomaly = %q, want %q", target.DBClock.Anomaly, AnomalyBackwardsInterval)
	}
	if target.DBClock.BaselineBefore != baseTime || target.DBClock.FinalAfter != baseTime.Add(31*time.Second) {
		t.Fatalf("clock readings did not survive the boundary: %+v", target.DBClock)
	}
}

func TestDBClockAnomalyKeepsDeltaAndMarksPartial(t *testing.T) {
	base := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 10, TimerWait: 100}})
	base.UTCBefore = baseTime
	base.UTCAfter = baseTime.Add(time.Minute)

	final := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 25, TimerWait: 400}})
	final.UTCBefore = baseTime.Add(30 * time.Second)
	final.UTCAfter = baseTime.Add(31 * time.Second)
	final.Texts = map[string]string{"aaa": "SELECT * FROM `posts`"}

	section := buildSection(sampleWith(base), sampleWith(final))
	if section.Validity != runctl.ValidityPartial {
		t.Fatalf("validity = %q, want %q", section.Validity, runctl.ValidityPartial)
	}
	target := findTarget(t, section, "db1")
	if !target.Usable {
		t.Fatal("a clock anomaly discarded counters that do not depend on the clock")
	}
	stat := findStat(t, target, "aaa")
	if stat.Count != 15 || stat.TimerWaitPicos != 300 {
		t.Fatalf("interval = %+v, want the counter difference kept", stat)
	}
	message, ok := hasHealth(section, HealthClockAnomaly)
	if !ok {
		t.Fatalf("health %q is missing from %+v", HealthClockAnomaly, section.Health)
	}
	if !strings.Contains(message, AnomalyBackwardsInterval) || !strings.Contains(message, "-30s") {
		t.Fatalf("health message = %q, want the anomaly and the size of the step", message)
	}
}
