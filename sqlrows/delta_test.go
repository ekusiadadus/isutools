package sqlrows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// findTarget returns one target section by ID.
func findTarget(t *testing.T, section *Section, id string) TargetSection {
	t.Helper()
	for _, target := range section.Targets {
		if target.TargetID == id {
			return target
		}
	}
	t.Fatalf("section has no target %q", id)
	return TargetSection{}
}

// findStat returns one digest row by digest.
func findStat(t *testing.T, target TargetSection, digest string) DigestStat {
	t.Helper()
	for _, stat := range target.Digests {
		if stat.Digest == digest {
			return stat
		}
	}
	t.Fatalf("target %q has no digest %q", target.TargetID, digest)
	return DigestStat{}
}

// hasHealth reports whether a health key is present, returning its message.
func hasHealth(section *Section, key string) (string, bool) {
	for _, note := range section.Health {
		if note.Key == key {
			return note.Message, true
		}
	}
	return "", false
}

func TestBuildSectionDelta(t *testing.T) {
	base := sampleWith(capturedTarget("db1", map[string]DigestRow{
		"aaa": {CountStar: 10, TimerWait: 1000, RowsExamined: 100, RowsSent: 10},
	}))
	final := sampleWith(capturedTarget("db1", map[string]DigestRow{
		// Seen at both boundaries: only the difference belongs to the run.
		"aaa": {CountStar: 15, TimerWait: 4000, RowsExamined: 700, RowsSent: 60},
		// First seen inside the interval: the whole counter is the interval.
		"bbb": {CountStar: 3, TimerWait: 9000, RowsExamined: 30, RowsSent: 3},
	}))
	final.Targets["db1"].Texts = map[string]string{
		"aaa": "SELECT * FROM `posts` WHERE `id` = ?",
		"bbb": "INSERT INTO `comments` (`body`) VALUES (?)",
	}

	section := buildSection(base, final)
	if section.Validity != runctl.ValidityValid {
		t.Fatalf("validity = %q, want %q", section.Validity, runctl.ValidityValid)
	}
	target := findTarget(t, section, "db1")
	if !target.Usable {
		t.Fatalf("target is not usable: %+v", target)
	}
	if target.Total != 2 || target.Shown != 2 || target.Dropped != 0 {
		t.Fatalf("shown/total/dropped = %d/%d/%d, want 2/2/0", target.Shown, target.Total, target.Dropped)
	}
	// Ordered by interval time, not by cumulative time: bbb is new but heavier.
	if target.Digests[0].Digest != "bbb" {
		t.Fatalf("digests are not ordered by interval time: %+v", target.Digests)
	}

	aaa := findStat(t, target, "aaa")
	want := DigestStat{
		Digest: "aaa", Count: 5, TimerWaitPicos: 3000, RowsExamined: 600, RowsSent: 50,
	}
	if aaa.Count != want.Count || aaa.TimerWaitPicos != want.TimerWaitPicos ||
		aaa.RowsExamined != want.RowsExamined || aaa.RowsSent != want.RowsSent {
		t.Fatalf("aaa delta = %+v, want %+v", aaa, want)
	}
	if aaa.Kind != KindSelect || !aaa.HasRatio || aaa.ExaminedPerSent != 12 {
		t.Fatalf("aaa ratio = %v/%v/%v, want select/true/12", aaa.Kind, aaa.HasRatio, aaa.ExaminedPerSent)
	}
	if aaa.TotalTime != 3*time.Nanosecond {
		t.Fatalf("aaa total time = %v, want 3ns", aaa.TotalTime)
	}

	bbb := findStat(t, target, "bbb")
	if bbb.Count != 3 || bbb.Kind != KindDML || bbb.HasRatio {
		t.Fatalf("bbb = %+v, want the full counter classified as dml without a ratio", bbb)
	}
}

func TestBuildSectionIntervalValidity(t *testing.T) {
	digests := map[string]DigestRow{"aaa": {CountStar: 10, TimerWait: 10}}

	cases := []struct {
		name     string
		mutate   func(base, final *TargetSample)
		wantCode string
		wantKey  string
		// keepNumbers marks the cases where the counters survive because the
		// anomaly does not affect them.
		keepNumbers bool
	}{
		{
			name: "server uuid changed",
			mutate: func(_, final *TargetSample) {
				final.ServerUUID = "uuid-2"
			},
			wantCode: CodeDBRestart,
			wantKey:  HealthDBRestart,
		},
		{
			name: "uptime decreased without a clock anomaly",
			mutate: func(_, final *TargetSample) {
				final.UptimeSec = 5
			},
			wantCode: CodeDBRestart,
			wantKey:  HealthDBRestart,
		},
		{
			name: "uptime decreased together with a clock anomaly",
			mutate: func(base, final *TargetSample) {
				final.UptimeSec = 5
				final.UTCBefore = base.UTCAfter.Add(-2 * time.Second)
				final.UTCAfter = final.UTCBefore.Add(time.Millisecond)
			},
			wantKey:     HealthClockAnomaly,
			keepNumbers: true,
		},
		{
			name: "baseline digest disappeared",
			mutate: func(_, final *TargetSample) {
				final.Digests = map[string]DigestRow{"zzz": {CountStar: 1, TimerWait: 1}}
			},
			wantCode: CodeCounterReset,
			wantKey:  HealthCounterReset,
		},
		{
			name: "counter rewound",
			mutate: func(_, final *TargetSample) {
				final.Digests = map[string]DigestRow{"aaa": {CountStar: 4, TimerWait: 4}}
			},
			wantCode: CodeCounterReset,
			wantKey:  HealthCounterReset,
		},
		{
			name: "rows examined rewound while the call count grew",
			mutate: func(base, final *TargetSample) {
				base.Digests = map[string]DigestRow{"aaa": {CountStar: 10, TimerWait: 10, RowsExamined: 500}}
				final.Digests = map[string]DigestRow{"aaa": {CountStar: 12, TimerWait: 12, RowsExamined: 3}}
			},
			wantCode: CodeCounterReset,
			wantKey:  HealthCounterReset,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseTarget := capturedTarget("db1", map[string]DigestRow{"aaa": digests["aaa"]})
			finalTarget := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 20, TimerWait: 40}})
			tc.mutate(baseTarget, finalTarget)

			section := buildSection(sampleWith(baseTarget), sampleWith(finalTarget))
			if section.Validity != runctl.ValidityPartial {
				t.Fatalf("validity = %q, want %q", section.Validity, runctl.ValidityPartial)
			}
			target := findTarget(t, section, "db1")
			if target.Usable != tc.keepNumbers {
				t.Fatalf("usable = %v, want %v (%+v)", target.Usable, tc.keepNumbers, target)
			}
			if target.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", target.Code, tc.wantCode)
			}
			if _, ok := hasHealth(section, tc.wantKey); !ok {
				t.Fatalf("health %q is missing from %+v", tc.wantKey, section.Health)
			}
			if tc.keepNumbers && len(target.Digests) == 0 {
				t.Fatal("counters were dropped even though only the clock was anomalous")
			}
		})
	}
}

func TestTopDigestTruncationIsRecorded(t *testing.T) {
	baseDigests := map[string]DigestRow{}
	finalDigests := map[string]DigestRow{}
	texts := map[string]string{}
	const total = 250
	for i := 0; i < total; i++ {
		digest := fmt.Sprintf("d%03d", i)
		baseDigests[digest] = DigestRow{CountStar: 1, TimerWait: 1}
		finalDigests[digest] = DigestRow{CountStar: 2, TimerWait: uint64(1 + i*10)}
		texts[digest] = "SELECT ? FROM `t`"
	}
	base := sampleWith(capturedTarget("db1", baseDigests))
	finalTarget := capturedTarget("db1", finalDigests)
	finalTarget.Texts = texts

	section := buildSection(base, sampleWith(finalTarget))
	target := findTarget(t, section, "db1")
	if target.Total != total {
		t.Fatalf("total = %d, want %d", target.Total, total)
	}
	if target.Shown != DigestTextFetchLimit || len(target.Digests) != DigestTextFetchLimit {
		t.Fatalf("shown = %d (%d rows), want %d", target.Shown, len(target.Digests), DigestTextFetchLimit)
	}
	if target.Dropped != total-DigestTextFetchLimit {
		t.Fatalf("dropped = %d, want %d", target.Dropped, total-DigestTextFetchLimit)
	}
	// The heaviest digest must survive truncation, the lightest must not.
	if target.Digests[0].Digest != "d249" {
		t.Fatalf("first row = %q, want d249", target.Digests[0].Digest)
	}
	for _, stat := range target.Digests {
		if stat.Digest == "d000" {
			t.Fatal("the lightest digest survived a truncation it should not have")
		}
	}
	if section.Validity != runctl.ValidityValid {
		t.Fatalf("truncation must not degrade the run: %q", section.Validity)
	}
}

func TestRatioIsNotApplicable(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		row      DigestRow
		wantKind StatementKind
		wantOK   bool
	}{
		{
			name:     "select that sent no rows",
			text:     "SELECT * FROM `users` WHERE `id` = ?",
			row:      DigestRow{CountStar: 4, RowsExamined: 9000, RowsSent: 0},
			wantKind: KindSelect,
		},
		{
			name:     "update never has a ratio",
			text:     "UPDATE `users` SET `name` = ? WHERE `id` = ?",
			row:      DigestRow{CountStar: 4, RowsExamined: 9000, RowsSent: 3},
			wantKind: KindDML,
		},
		{
			name:     "digest without text is other",
			row:      DigestRow{CountStar: 4, RowsExamined: 10, RowsSent: 5},
			wantKind: KindOther,
		},
		{
			name:     "select with rows sent has a ratio",
			text:     "SELECT * FROM `users`",
			row:      DigestRow{CountStar: 4, RowsExamined: 10, RowsSent: 5},
			wantKind: KindSelect,
			wantOK:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finalTarget := capturedTarget("db1", map[string]DigestRow{"aaa": tc.row})
			if tc.text != "" {
				finalTarget.Texts = map[string]string{"aaa": tc.text}
			}
			section := buildSection(sampleWith(capturedTarget("db1", nil)), sampleWith(finalTarget))
			stat := findStat(t, findTarget(t, section, "db1"), "aaa")
			if stat.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", stat.Kind, tc.wantKind)
			}
			if stat.HasRatio != tc.wantOK {
				t.Fatalf("has ratio = %v, want %v", stat.HasRatio, tc.wantOK)
			}
			if !tc.wantOK && stat.ExaminedPerSent != 0 {
				t.Fatalf("ratio = %v, want it left unset", stat.ExaminedPerSent)
			}
			if tc.text == "" && stat.Query != MissingQueryText {
				t.Fatalf("query = %q, want %q", stat.Query, MissingQueryText)
			}
		})
	}
}

func TestOverflowIsDeduplicatedPerServer(t *testing.T) {
	newPair := func(id string, uuid string, baseCount, finalCount uint64) (*TargetSample, *TargetSample) {
		b := capturedTarget(id, map[string]DigestRow{"aaa": {CountStar: 1, TimerWait: 1}})
		f := capturedTarget(id, map[string]DigestRow{"aaa": {CountStar: 2, TimerWait: 2}})
		b.ServerUUID, f.ServerUUID = uuid, uuid
		b.Overflow, b.HasOverflow = DigestRow{CountStar: baseCount}, true
		f.Overflow, f.HasOverflow = DigestRow{CountStar: finalCount}, true
		return b, f
	}
	b1, f1 := newPair("db1", "uuid-shared", 10, 40)
	b2, f2 := newPair("db2", "uuid-shared", 10, 40)

	section := buildSection(sampleWith(b1, b2), sampleWith(f1, f2))
	first := findTarget(t, section, "db1")
	second := findTarget(t, section, "db2")
	if !first.Overflow.Detected || first.Overflow.CountStar != 30 {
		t.Fatalf("db1 overflow = %+v, want detected with 30", first.Overflow)
	}
	if second.Overflow.Detected {
		t.Fatal("the instance-global overflow row was reported twice")
	}
	if second.Overflow.ReportedBy != "db1" {
		t.Fatalf("db2 points at %q, want db1", second.Overflow.ReportedBy)
	}
	message, ok := hasHealth(section, HealthOverflow)
	if !ok || strings.Count(message, "db") != 1 {
		t.Fatalf("overflow health = %q (present=%v), want exactly one target", message, ok)
	}
	if section.Validity != runctl.ValidityValid {
		t.Fatalf("overflow alone must not degrade the run, got %q", section.Validity)
	}
}

func TestUnpairedTargetDropped(t *testing.T) {
	base := sampleWith(
		capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 1, TimerWait: 1}}),
		capturedTarget("db2", map[string]DigestRow{"aaa": {CountStar: 1, TimerWait: 1}}),
	)
	final := sampleWith(capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 3, TimerWait: 3}}))

	section := buildSection(base, final)
	if section.Validity != runctl.ValidityPartial {
		t.Fatalf("validity = %q, want %q", section.Validity, runctl.ValidityPartial)
	}
	dropped := findTarget(t, section, "db2")
	if dropped.Usable || dropped.Code != CodeUnpairedBoundary {
		t.Fatalf("db2 = %+v, want an unusable unpaired-boundary target", dropped)
	}
	if len(dropped.Digests) != 0 {
		t.Fatal("an unpaired target published numbers")
	}
	if message, ok := hasHealth(section, HealthTargetDropped); !ok || !strings.Contains(message, "db2") {
		t.Fatalf("dropped health = %q (present=%v)", message, ok)
	}
	if kept := findTarget(t, section, "db1"); !kept.Usable {
		t.Fatal("the paired target was dropped along with the unpaired one")
	}
}

func TestDroppedTargetsAreRecordedWithTheirReason(t *testing.T) {
	base := sampleWith(
		capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 1}}),
		skipped(sqlstats.TargetInfo{ID: "db2", Schema: "isuconp"}, CodeBudgetExhausted, "budget"),
		skipped(sqlstats.TargetInfo{ID: "db3", Schema: "isuconp"}, CodeBudgetExhausted, "budget"),
		skipped(sqlstats.TargetInfo{ID: "db4"}, CodeNoSchema, "no schema"),
		skipped(sqlstats.TargetInfo{ID: "db5", Schema: "isuconp"}, CodeProbeSkip, "performance_schema is OFF"),
	)
	final := sampleWith(
		capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 2}}),
		skipped(sqlstats.TargetInfo{ID: "db2", Schema: "isuconp"}, CodeBudgetExhausted, "budget"),
		skipped(sqlstats.TargetInfo{ID: "db3", Schema: "isuconp"}, CodeBudgetExhausted, "budget"),
		skipped(sqlstats.TargetInfo{ID: "db4"}, CodeNoSchema, "no schema"),
		skipped(sqlstats.TargetInfo{ID: "db5", Schema: "isuconp"}, CodeProbeSkip, "performance_schema is OFF"),
	)

	section := buildSection(base, final)
	if section.Validity != runctl.ValidityPartial {
		t.Fatalf("validity = %q, want %q", section.Validity, runctl.ValidityPartial)
	}
	if len(section.Targets) != 5 {
		t.Fatalf("section has %d targets, want every registered target recorded", len(section.Targets))
	}
	message, ok := hasHealth(section, HealthTargetDropped)
	if !ok || message != "db2, db3 ("+CodeBudgetExhausted+")" {
		t.Fatalf("dropped health = %q (present=%v), want the two budget-dropped targets on one line", message, ok)
	}
	if message, ok := hasHealth(section, HealthNoSchema); !ok || !strings.Contains(message, "db4") {
		t.Fatalf("no-schema health = %q (present=%v)", message, ok)
	}
	if message, ok := hasHealth(section, HealthSkip); !ok || !strings.Contains(message, "performance_schema is OFF") {
		t.Fatalf("skip health = %q (present=%v)", message, ok)
	}
}

func TestCollectUsesFrozenSamplesOnly(t *testing.T) {
	c := New()
	c.targets = func() []sqlstats.TargetInfo {
		t.Error("Collect read the target registry")
		return nil
	}
	c.inspect = func(_ context.Context, _ string, _ sqlstats.Purpose, _ func(context.Context, sqlstats.Querier) error) error {
		t.Error("Collect opened a database connection")
		return nil
	}
	// A pending baseline that differs from the handles proves Collect reads
	// the handles rather than the collector's own state.
	c.pending[runKey{runID: "run-1", epoch: 1}] = sampleWith(
		capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 999999, TimerWait: 999999}}),
	)

	base := runctl.NewBaselineHandle("run-1", 1, Name, runctl.PhaseStartBaseline, baseTime,
		sampleWith(capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 1, TimerWait: 1}})))
	final := runctl.NewBaselineHandle("run-1", 1, Name, runctl.PhaseFinishFinal, baseTime,
		sampleWith(capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 4, TimerWait: 7}})))

	value, err := c.Collect(base, final)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	section, ok := value.(*Section)
	if !ok {
		t.Fatalf("Collect returned %T, want *Section", value)
	}
	stat := findStat(t, findTarget(t, section, "db1"), "aaa")
	if stat.Count != 3 || stat.TimerWaitPicos != 6 {
		t.Fatalf("interval = %+v, want the handles' difference", stat)
	}
}

func TestCollectRejectsForeignSample(t *testing.T) {
	c := New()
	good := runctl.NewBaselineHandle("run-1", 1, Name, runctl.PhaseStartBaseline, baseTime, sampleWith())

	cases := []struct {
		name        string
		base, final runctl.BaselineHandle
	}{
		{
			name:  "final carries another collector's sample",
			base:  good,
			final: runctl.NewBaselineHandle("run-1", 1, Name, runctl.PhaseFinishFinal, baseTime, struct{ N int }{1}),
		},
		{
			name:  "baseline carries nothing",
			base:  runctl.BaselineHandle{},
			final: good,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, err := c.Collect(tc.base, tc.final)
			if !errors.Is(err, ErrSampleType) {
				t.Fatalf("err = %v, want %v", err, ErrSampleType)
			}
			if value != nil {
				t.Fatalf("value = %v, want nil", value)
			}
		})
	}
}

// TestTargetPresentOnlyAtTheClosingBoundary covers a target registered while
// the run was already open.
func TestTargetPresentOnlyAtTheClosingBoundary(t *testing.T) {
	base := sampleWith(capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 1}}))
	final := sampleWith(
		capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 2}}),
		capturedTarget("db2", map[string]DigestRow{"aaa": {CountStar: 900}}),
	)

	section := buildSection(base, final)
	late := findTarget(t, section, "db2")
	if late.Usable || late.Code != CodeUnpairedBoundary {
		t.Fatalf("db2 = %+v, want an unusable unpaired-boundary target", late)
	}
	if !strings.Contains(late.Reason, "closing boundary") {
		t.Fatalf("reason = %q, want it to name the boundary that saw the target", late.Reason)
	}
	if section.Validity != runctl.ValidityPartial {
		t.Fatalf("validity = %q, want %q", section.Validity, runctl.ValidityPartial)
	}
}

// TestCounterResetNamesTheDigest keeps the health message actionable while
// staying short: a digest is 64 hex characters, and its prefix identifies it.
func TestCounterResetNamesTheDigest(t *testing.T) {
	long := strings.Repeat("a", 64)
	base := capturedTarget("db1", map[string]DigestRow{long: {CountStar: 10}})
	final := capturedTarget("db1", map[string]DigestRow{"other": {CountStar: 1}})

	section := buildSection(sampleWith(base), sampleWith(final))
	target := findTarget(t, section, "db1")
	if target.Code != CodeCounterReset {
		t.Fatalf("code = %q, want %q", target.Code, CodeCounterReset)
	}
	if !strings.Contains(target.Reason, long[:16]) || strings.Contains(target.Reason, long) {
		t.Fatalf("reason = %q, want the shortened digest", target.Reason)
	}
}

// TestMissingClockReadingIsReported covers the boundary that never managed to
// read UTC_TIMESTAMP: consumers must be told to abstain rather than shown an
// interval starting at the zero time.
func TestMissingClockReadingIsReported(t *testing.T) {
	base := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 1}})
	final := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 5}})
	final.UTCAfter = time.Time{}

	section := buildSection(sampleWith(base), sampleWith(final))
	target := findTarget(t, section, "db1")
	if target.DBClock.Monotonic || target.DBClock.Anomaly != AnomalyMissing {
		t.Fatalf("clock = %+v, want %q", target.DBClock, AnomalyMissing)
	}
	if !target.Usable {
		t.Fatal("a missing clock reading discarded counters that do not depend on it")
	}
	message, ok := hasHealth(section, HealthClockAnomaly)
	if !ok || message != "db1 ("+AnomalyMissing+")" {
		t.Fatalf("health = %q (present=%v)", message, ok)
	}
	if section.Validity != runctl.ValidityPartial {
		t.Fatalf("validity = %q, want %q", section.Validity, runctl.ValidityPartial)
	}
}

// TestOverflowWithoutServerIdentityIsStillReported: deduplication needs an
// identity, and losing the warning is worse than repeating it.
func TestOverflowWithoutServerIdentityIsStillReported(t *testing.T) {
	base := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 1}})
	final := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 2}})
	base.ServerUUID, final.ServerUUID = "", ""
	base.Overflow, base.HasOverflow = DigestRow{CountStar: 1}, true
	final.Overflow, final.HasOverflow = DigestRow{CountStar: 6}, true

	section := buildSection(sampleWith(base), sampleWith(final))
	target := findTarget(t, section, "db1")
	if !target.Overflow.Detected || target.Overflow.CountStar != 5 {
		t.Fatalf("overflow = %+v, want it reported without an identity", target.Overflow)
	}
}

// TestOverflowRowAppearingDuringTheRunCountsWhole: a row absent at the opening
// boundary has no earlier value to subtract.
func TestOverflowRowAppearingDuringTheRunCountsWhole(t *testing.T) {
	base := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 1}})
	final := capturedTarget("db1", map[string]DigestRow{"aaa": {CountStar: 2}})
	final.Overflow, final.HasOverflow = DigestRow{CountStar: 12}, true

	section := buildSection(sampleWith(base), sampleWith(final))
	target := findTarget(t, section, "db1")
	if !target.Overflow.Detected || target.Overflow.CountStar != 12 {
		t.Fatalf("overflow = %+v, want the whole counter", target.Overflow)
	}
}
