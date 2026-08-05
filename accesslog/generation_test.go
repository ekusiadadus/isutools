package accesslog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// genLine builds one LTSV record for the given URI. Distinct URIs make it
// obvious which generation a line landed in.
func genLine(uri string) string {
	return "time:2026-08-05T10:00:00+09:00\tmethod:GET\turi:" + uri +
		"\tstatus:200\treqtime:0.010\tupstime:0.005\tbytes:100\tcache:MISS\tctype:text/html\n"
}

func genWriteLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func genAppendLog(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("append %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func newLogGenTest(t *testing.T) (*GenerationCollector, *Collector, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access.log")
	genWriteLog(t, path, "")
	collector := New(path)
	t.Cleanup(collector.Close)
	return NewGenerationCollector(collector), collector, path
}

func genDrainCollect(t *testing.T, c *GenerationCollector, h runctl.GenerationHandle) Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Drain(ctx, h); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	value, err := c.Collect(h)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	snap, ok := value.(Snapshot)
	if !ok {
		t.Fatalf("Collect returned %T, want Snapshot", value)
	}
	return snap
}

func uris(snap Snapshot) []string {
	out := make([]string, 0, len(snap.Entries))
	for _, entry := range snap.Entries {
		out = append(out, entry.URI)
	}
	return out
}

func TestLogGenerationBoundaryFreezeDrainCollect(t *testing.T) {
	c, _, path := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, path, genLine("/before"))
	start, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if !start.Committed || start.At.IsZero() {
		t.Fatalf("BeginBoundary = %+v, want a committed, timed boundary", start)
	}

	genAppendLog(t, path, genLine("/during")+genLine("/during"))
	final, err := c.Freeze(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !final.Committed {
		t.Fatal("Freeze must commit the boundary")
	}

	before := genDrainCollect(t, c, start.Handle)
	if got := uris(before); len(got) != 1 || got[0] != "/before" {
		t.Fatalf("pre-run generation = %v, want [/before]", got)
	}
	run := genDrainCollect(t, c, final.Handle)
	if got := uris(run); len(got) != 1 || got[0] != "/during" {
		t.Fatalf("run generation = %v, want [/during]", got)
	}
	if run.Lines != 2 {
		t.Fatalf("run generation counted %d lines, want 2", run.Lines)
	}
}

// TestLogGenerationExcludesLinesWrittenAfterTheFreezePoint is the reason the
// boundary records an offset instead of letting the drain read to end of file:
// a Finish must not swallow requests logged after it.
func TestLogGenerationExcludesLinesWrittenAfterTheFreezePoint(t *testing.T) {
	c, _, path := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, path, genLine("/inside"))
	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}

	// Written after the freeze point but before the catch-up runs: the exact
	// window a "drain to EOF" implementation would get wrong.
	genAppendLog(t, path, genLine("/after")+genLine("/after"))

	snap := genDrainCollect(t, c, res.Handle)
	if got := uris(snap); len(got) != 1 || got[0] != "/inside" {
		t.Fatalf("frozen generation = %v, want only [/inside]", got)
	}
	if snap.Lines != 1 {
		t.Fatalf("frozen generation counted %d lines, want 1", snap.Lines)
	}

	// The lines written after the freeze point are not lost either: they are
	// the next generation's, and the next generation starts exactly there.
	next, err := c.Freeze(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	following := genDrainCollect(t, c, next.Handle)
	if got := uris(following); len(got) != 1 || got[0] != "/after" {
		t.Fatalf("next generation = %v, want [/after]", got)
	}
	if following.Lines != 2 {
		t.Fatalf("next generation counted %d lines, want 2", following.Lines)
	}
}

// TestLogGenerationFreezePointHoldsAcrossAPartialLine checks that a record
// still being written when the boundary is taken is not chopped in half.
func TestLogGenerationFreezePointHoldsAcrossAPartialLine(t *testing.T) {
	c, _, path := newLogGenTest(t)
	ctx := context.Background()

	complete := genLine("/complete")
	partial := genLine("/partial")
	genAppendLog(t, path, complete+partial[:len(partial)/2])

	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	genAppendLog(t, path, partial[len(partial)/2:])

	snap := genDrainCollect(t, c, res.Handle)
	if got := uris(snap); len(got) != 1 || got[0] != "/complete" {
		t.Fatalf("frozen generation = %v, want only [/complete]", got)
	}
	next, err := c.Freeze(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if got := uris(genDrainCollect(t, c, next.Handle)); len(got) != 1 || got[0] != "/partial" {
		t.Fatalf("next generation = %v, want [/partial]; the split line must survive", got)
	}
}

func TestLogGenerationDrainHonoursContextCancellation(t *testing.T) {
	c, _, path := newLogGenTest(t)

	genAppendLog(t, path, strings.Repeat(genLine("/bulk"), 64))
	res, err := c.BeginBoundary(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err = c.Drain(ctx, res.Handle)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain error = %v, want a wrapped context.Canceled", err)
	}
	if elapsed > runctl.DrainCancelGrace {
		t.Fatalf("Drain took %v, want at most DrainCancelGrace (%v)", elapsed, runctl.DrainCancelGrace)
	}

	// A cut-short drain keeps what it managed to read rather than failing the
	// whole section.
	value, err := c.Collect(res.Handle)
	if err != nil {
		t.Fatalf("Collect after a cancelled Drain: %v", err)
	}
	partial, ok := value.(Snapshot)
	if !ok {
		t.Fatalf("Collect returned %T, want Snapshot", value)
	}
	if partial.Lines >= 64 {
		t.Fatalf("cancelled drain collected %d lines, want fewer than 64", partial.Lines)
	}

	// The same handle resumes and completes when it is given a live context.
	if snap := genDrainCollect(t, c, res.Handle); snap.Lines != 64 {
		t.Fatalf("resumed drain collected %d lines, want 64", snap.Lines)
	}
}

func TestLogGenerationReplaysSameRunAndEpoch(t *testing.T) {
	c, _, path := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, path, genLine("/first"))
	first, err := c.BeginBoundary(ctx, "run-1", 3)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	genAppendLog(t, path, genLine("/second"))

	second, err := c.BeginBoundary(ctx, "run-1", 3)
	if err != nil {
		t.Fatalf("replayed BeginBoundary: %v", err)
	}
	if second.Handle != first.Handle || !second.At.Equal(first.At) || second.Committed != first.Committed {
		t.Fatalf("replay returned %+v, want %+v", second, first)
	}
	// A replay that moved the freeze point would have pulled /second into the
	// generation that was already closed.
	if got := uris(genDrainCollect(t, c, first.Handle)); len(got) != 1 || got[0] != "/first" {
		t.Fatalf("replayed boundary froze %v, want [/first]", got)
	}

	final, err := c.Freeze(ctx, "run-1", 3)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if final.Handle == first.Handle {
		t.Fatal("Freeze must close its own generation")
	}
	replayFinal, err := c.Freeze(ctx, "run-1", 3)
	if err != nil {
		t.Fatalf("replayed Freeze: %v", err)
	}
	if replayFinal.Handle != final.Handle || !replayFinal.At.Equal(final.At) {
		t.Fatalf("replayed Freeze returned %+v, want %+v", replayFinal, final)
	}
	if got := uris(genDrainCollect(t, c, final.Handle)); len(got) != 1 || got[0] != "/second" {
		t.Fatalf("run generation = %v, want [/second]", got)
	}
}

func TestLogGenerationRejectsStaleEpoch(t *testing.T) {
	c, _, _ := newLogGenTest(t)
	ctx := context.Background()

	if _, err := c.BeginBoundary(ctx, "run-2", 5); err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	res, err := c.BeginBoundary(ctx, "run-1", 4)
	if !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("stale epoch error = %v, want ErrStaleEpoch", err)
	}
	if res.Committed {
		t.Fatal("a rejected boundary must not report a commit")
	}
	if _, err := c.Freeze(ctx, "run-1", 4); !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("stale epoch Freeze error = %v, want ErrStaleEpoch", err)
	}
}

func TestLogGenerationReleaseIsIdempotent(t *testing.T) {
	c, _, path := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, path, genLine("/one"))
	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if got := uris(genDrainCollect(t, c, res.Handle)); len(got) != 1 {
		t.Fatalf("frozen generation = %v, want one entry", got)
	}

	c.Release(res.Handle)
	c.Release(res.Handle)
	c.Release(res.Handle)

	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrHandleReleased) {
		t.Fatalf("Collect after Release = %v, want ErrHandleReleased", err)
	}
	if err := c.Drain(ctx, res.Handle); err != nil {
		t.Fatalf("Drain after Release: %v", err)
	}

	// Releasing a generation must not strand its successor.
	genAppendLog(t, path, genLine("/two"))
	next, err := c.Freeze(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if got := uris(genDrainCollect(t, c, next.Handle)); len(got) != 1 || got[0] != "/two" {
		t.Fatalf("successor generation = %v, want [/two]", got)
	}
}

// TestLogGenerationReleaseWithoutDrainStartsTheNextGenerationClean checks the
// abort path: a generation nobody collected must not leave its lines in the
// aggregate for the next run to inherit.
func TestLogGenerationReleaseWithoutDrainStartsTheNextGenerationClean(t *testing.T) {
	c, collector, path := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, path, genLine("/abandoned"))
	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if err := collector.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	c.Release(res.Handle)

	next, err := c.BeginBoundary(ctx, "run-2", 2)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if got := uris(genDrainCollect(t, c, next.Handle)); len(got) != 0 {
		t.Fatalf("generation after an abandoned one = %v, want none", got)
	}
}

// TestLogGenerationRefusesAFreezeWithoutAnOpeningBoundary covers the
// late-registration hazard. The adapter is registered by the handler the first
// time an access log is configured, which can land after a run has already
// taken its opening boundary. The closing Freeze would then mint the
// collector's first generation, whose lower edge is the collector's start of
// life — so the run's section would report every line logged since the process
// started as that run's interval. The section is skipped instead.
func TestLogGenerationRefusesAFreezeWithoutAnOpeningBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	genWriteLog(t, path, "")
	collector := New(path)
	t.Cleanup(collector.Close)
	ctx := context.Background()

	// Traffic that predates the registration, already pulled into the
	// aggregate the way a live bare collector would have pulled it.
	genAppendLog(t, path, genLine("/pre-registration")+genLine("/pre-registration"))
	if err := collector.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// The run opened before this line: the adapter never saw BeginBoundary.
	c := NewGenerationCollector(collector)
	genAppendLog(t, path, genLine("/mid-run"))

	res, err := c.Freeze(ctx, "run-1", 1)
	if !errors.Is(err, ErrFreezeWithoutBoundary) {
		t.Fatalf("Freeze without an opening boundary = %v, want ErrFreezeWithoutBoundary", err)
	}
	if res.Committed {
		t.Fatalf("a refused boundary reported a commit: %+v", res)
	}
	if res.At.IsZero() {
		t.Fatal("a boundary result must carry a time even on error")
	}
	if !res.Handle.Zero() {
		t.Fatalf("a refused boundary handed out %+v, want no handle to collect", res.Handle)
	}
	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrForeignHandle) {
		t.Fatalf("Collect of a refused boundary = %v, want no collectable section", err)
	}
	state := collector.Health()
	if state.Status == StatusOK {
		t.Fatalf("health = %+v, want a degraded status naming the skipped section", state)
	}
	if state.Message != SkippedLateRegistration {
		t.Fatalf("health message = %q, want %q", state.Message, SkippedLateRegistration)
	}
	// The refusal is replayable like every other boundary outcome.
	if _, again := c.Freeze(ctx, "run-1", 1); !errors.Is(again, ErrFreezeWithoutBoundary) {
		t.Fatalf("replayed Freeze = %v, want ErrFreezeWithoutBoundary", again)
	}

	// The refusal minted nothing, so the next run — which does take both
	// boundaries — measures its own interval and only its own interval.
	start, err := c.BeginBoundary(ctx, "run-2", 2)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	genAppendLog(t, path, genLine("/measured"))
	final, err := c.Freeze(ctx, "run-2", 2)
	if err != nil {
		t.Fatalf("Freeze after a proper opening boundary: %v", err)
	}
	if got := uris(genDrainCollect(t, c, start.Handle)); len(got) != 2 ||
		got[0] != "/pre-registration" || got[1] != "/mid-run" {
		t.Fatalf("pre-run generation = %v, want [/pre-registration /mid-run]", got)
	}
	run := genDrainCollect(t, c, final.Handle)
	if got := uris(run); len(got) != 1 || got[0] != "/measured" {
		t.Fatalf("run generation = %v, want only [/measured]: the pre-registration lines are not this run's", got)
	}
	if run.Lines != 1 {
		t.Fatalf("run generation counted %d lines, want 1", run.Lines)
	}
}

// TestLogGenerationFreezeIsAcceptedAfterAnyEarlierBoundary keeps the refusal
// narrow: once any generation exists, its freeze point bounds the next one
// from below, so a Freeze is a legitimate closing boundary again.
func TestLogGenerationFreezeIsAcceptedAfterAnyEarlierBoundary(t *testing.T) {
	c, _, path := newLogGenTest(t)
	ctx := context.Background()

	start, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if got := uris(genDrainCollect(t, c, start.Handle)); len(got) != 0 {
		t.Fatalf("pre-run generation = %v, want none", got)
	}
	genAppendLog(t, path, genLine("/in-run"))
	// A later run whose opening boundary this collector missed still has a
	// predecessor generation, so its interval is bounded and it is measured.
	final, err := c.Freeze(ctx, "run-2", 2)
	if err != nil {
		t.Fatalf("Freeze after an earlier run's boundary: %v", err)
	}
	if !final.Committed {
		t.Fatalf("Freeze = %+v, want a committed boundary", final)
	}
	if got := uris(genDrainCollect(t, c, final.Handle)); len(got) != 1 || got[0] != "/in-run" {
		t.Fatalf("run generation = %v, want [/in-run]", got)
	}
}

func TestLogGenerationRejectsForeignHandle(t *testing.T) {
	c, _, _ := newLogGenTest(t)
	other, _, otherPath := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, otherPath, genLine("/other"))
	res, err := other.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if err := c.Drain(ctx, res.Handle); !errors.Is(err, ErrForeignHandle) {
		t.Fatalf("Drain of a foreign handle = %v, want ErrForeignHandle", err)
	}
	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrForeignHandle) {
		t.Fatalf("Collect of a foreign handle = %v, want ErrForeignHandle", err)
	}
	// Release has no error channel, so the only requirement is that a foreign
	// handle cannot make it panic or free another collector's data.
	c.Release(res.Handle)
	c.Release(runctl.GenerationHandle{})
	if got := uris(genDrainCollect(t, other, res.Handle)); len(got) != 1 || got[0] != "/other" {
		t.Fatalf("foreign Release freed another collector's generation: %v", got)
	}
}

func TestLogGenerationSurvivesAnUnconfiguredCollector(t *testing.T) {
	c := NewGenerationCollector(nil)
	if c.Name() != SectionName {
		t.Fatalf("Name() = %q, want %q", c.Name(), SectionName)
	}
	res, err := c.BeginBoundary(context.Background(), "run-1", 1)
	if err == nil {
		t.Fatal("a boundary without a log collector must fail rather than pretend")
	}
	if res.Committed {
		t.Fatal("a failed boundary must not report a commit")
	}
	c.Release(res.Handle)
}

// TestLogGenerationSurvivesAClosedCollector covers the fail-open path: a
// boundary taken after Close must still produce a usable, empty generation
// rather than an error the run cannot recover from.
func TestLogGenerationSurvivesAClosedCollector(t *testing.T) {
	c, collector, path := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, path, genLine("/before-close"))
	collector.Close()

	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary on a closed collector: %v", err)
	}
	if !res.Committed {
		t.Fatal("the boundary still fixes a (broken) freeze point, so it commits")
	}
	snap := genDrainCollect(t, c, res.Handle)
	if len(snap.Entries) != 0 {
		t.Fatalf("closed collector produced %v, want an empty generation", uris(snap))
	}
}

// TestLogGenerationRecoversFromAMissingLogFile covers the other fail-open
// path: the log did not exist when the collector was built.
func TestLogGenerationRecoversFromAMissingLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	collector := New(path) // the file does not exist yet
	t.Cleanup(collector.Close)
	c := NewGenerationCollector(collector)
	ctx := context.Background()

	first, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary without a log file: %v", err)
	}
	if got := uris(genDrainCollect(t, c, first.Handle)); len(got) != 0 {
		t.Fatalf("generation without a log file = %v, want none", got)
	}

	genWriteLog(t, path, "")
	second, err := c.Freeze(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("Freeze after the log appeared: %v", err)
	}
	if got := uris(genDrainCollect(t, c, second.Handle)); len(got) != 0 {
		t.Fatalf("generation spanning the reopen = %v, want none", got)
	}

	genAppendLog(t, path, genLine("/recovered"))
	third, err := c.BeginBoundary(ctx, "run-2", 2)
	if err != nil {
		t.Fatalf("BeginBoundary after recovery: %v", err)
	}
	if got := uris(genDrainCollect(t, c, third.Handle)); len(got) != 1 || got[0] != "/recovered" {
		t.Fatalf("generation after recovery = %v, want [/recovered]", got)
	}
}

// TestLogGenerationMarksARotationBetweenBoundaryAndDrain checks that a freeze
// point measured in a file that has since been replaced is reported as
// incomplete instead of being read against the wrong file.
func TestLogGenerationMarksARotationBetweenBoundaryAndDrain(t *testing.T) {
	c, collector, path := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, path, genLine("/pre-rotation"))
	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	genWriteLog(t, path, genLine("/post-rotation"))
	// The bare collector is what notices the rotation and re-opens; the
	// generation's freeze point now names an offset in a file that is gone.
	if err := collector.Collect(); err != nil {
		t.Fatalf("Collect across the rotation: %v", err)
	}

	snap := genDrainCollect(t, c, res.Handle)
	if snap.Health.Rotations == 0 {
		t.Fatalf("health = %+v, want the rotation recorded", snap.Health)
	}
	if snap.Health.Status == StatusOK {
		t.Fatalf("health = %+v, want a degraded status after a rotation", snap.Health)
	}
	// Whatever the drain could attribute is kept; nothing crashes and no
	// second generation's bytes are pulled in twice.
	if next, err := c.Freeze(ctx, "run-1", 1); err != nil {
		t.Fatalf("Freeze after the rotation: %v", err)
	} else {
		genDrainCollect(t, c, next.Handle)
	}
}

// TestPeekBetweenBoundaryAndDrainKeepsTheClosingGenerationClosed is the
// dashboard read the transport performs on GET /, /live, /json and
// /snapshot.html. Those handlers hold no boundary lock, and POST /finish
// answers as soon as the closing boundary exists and drains in the background,
// so a refresh lands in that window routinely. Snapshot's flush there advances
// the offset past the freeze point, and the drain — which stops when the
// remaining distance to the freeze point is not positive — then seals traffic
// the run was measured to exclude. Peek is the read that cannot do that.
func TestPeekBetweenBoundaryAndDrainKeepsTheClosingGenerationClosed(t *testing.T) {
	c, collector, path := newLogGenTest(t)
	ctx := context.Background()

	opening, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	// The coordinator settles the generation an opening boundary closed before
	// the run's own generation can be cut; generations are drained in order.
	genDrainCollect(t, c, opening.Handle)
	c.Release(opening.Handle)
	genAppendLog(t, path, genLine("/inside-the-run"))

	closing, err := c.Freeze(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	// Traffic the benchmarker produced after measurement stopped. It belongs to
	// the next generation, and the drain has not run yet.
	genAppendLog(t, path, genLine("/after-the-boundary"))

	peeked := collector.Peek()
	if peeked.Lines != 0 {
		t.Fatalf("peek between the boundary and the drain read %d lines, want none: %v",
			peeked.Lines, uris(peeked))
	}

	snap := genDrainCollect(t, c, closing.Handle)
	if got := uris(snap); len(got) != 1 || got[0] != "/inside-the-run" {
		t.Fatalf("the closing generation = %v, want only the traffic inside the run", got)
	}
}

// TestPeekWithNoBoundaryOutstandingStaysLive is the other half of the
// contract. Refusing to read is only safe while a freeze point is waiting for
// its drain; the rest of the time a report that never refreshed would be a
// dashboard showing nothing, which is how a "safe" read turns into a useless
// one.
func TestPeekWithNoBoundaryOutstandingStaysLive(t *testing.T) {
	c, collector, path := newLogGenTest(t)
	ctx := context.Background()

	genAppendLog(t, path, genLine("/live-traffic"))
	if got := uris(collector.Peek()); len(got) != 1 || got[0] != "/live-traffic" {
		t.Fatalf("peek before any boundary = %v, want the traffic already in the log", got)
	}

	// A drained generation owes nothing, so freshness comes back with it.
	start, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	genDrainCollect(t, c, start.Handle)
	c.Release(start.Handle)
	genAppendLog(t, path, genLine("/next-generation"))
	if got := uris(collector.Peek()); len(got) != 1 || got[0] != "/next-generation" {
		t.Fatalf("peek after the drain = %v, want the new generation's traffic", got)
	}
}
