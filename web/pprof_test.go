package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
)

func TestPprofIndexIsMounted(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pprof index status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "goroutine") {
		t.Error("pprof index must list available profiles")
	}
}

func TestPprofNamedProfile(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pprof/goroutine", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("goroutine profile status = %d", rec.Code)
	}
}

func TestResetTriggersCPUCapture(t *testing.T) {
	dir := t.TempDir()
	tbl := agg.NewTable(agg.DefaultMaxKeys)
	h := NewHandler(Provider{
		SQL:           tbl,
		DataDir:       dir,
		PprofDuration: 150 * time.Millisecond,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof"))
		if len(matches) > 0 {
			info, err := os.Stat(matches[0])
			sidecar := strings.TrimSuffix(matches[0], profileArtifactExt) + profileSidecarExt
			if _, sidecarErr := os.Stat(sidecar); err == nil && info.Size() > 0 && sidecarErr == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("CPU profile was not captured after reset")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The captured profile must be listed on the index and downloadable.
	matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof"))
	name := filepath.Base(matches[0])
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	sidecarName := strings.TrimSuffix(name, profileArtifactExt) + profileSidecarExt
	sidecarBody, err := os.ReadFile(filepath.Join(dir, sidecarName))
	if err != nil {
		t.Fatalf("fixed CPU sidecar was not published: %v", err)
	}
	var fixed FixedCPUProfileRecord
	if err := json.Unmarshal(sidecarBody, &fixed); err != nil {
		t.Fatal(err)
	}
	if fixed.Schema != fixedCPUProfileSchema || fixed.Mode != "fixed" || fixed.Status != "published" ||
		fixed.File != name || fixed.SHA256 != hashBytes(body) || fixed.Bytes != int64(len(body)) || fixed.CaptureID == "" {
		t.Fatalf("fixed CPU sidecar = %#v", fixed)
	}
	for _, artifact := range []string{name, sidecarName} {
		info, err := os.Stat(filepath.Join(dir, artifact))
		if err != nil {
			t.Fatalf("fixed artifact %s stat: %v", artifact, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("fixed artifact %s mode=%v", artifact, info.Mode().Perm())
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), name) {
		t.Error("index must list captured profiles")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("profile download status = %d", rec.Code)
	}
}

func TestResetWithoutPprofDurationDoesNotCapture(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}
	time.Sleep(200 * time.Millisecond)
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof")); len(matches) != 0 {
		t.Errorf("no profile expected, got %v", matches)
	}
}

// profileHandler builds a handler that captures the named profile kinds into
// a fresh data directory, with a health registry to read the verdicts back
// out of.
func profileHandler(t *testing.T, kinds ...string) (*handler, string, *health.Registry) {
	t.Helper()
	dir := t.TempDir()
	registry := health.NewRegistry()
	return &handler{
		p:         Provider{DataDir: dir, RuntimeProfiles: kinds, Health: registry},
		operation: make(chan struct{}, 1),
	}, dir, registry
}

// readCapture decodes one sidecar, the durable record of a single capture.
func readCapture(t *testing.T, dir, name string) ProfileCapture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read sidecar %s: %v", name, err)
	}
	var capture ProfileCapture
	if err := json.Unmarshal(body, &capture); err != nil {
		t.Fatalf("decode sidecar %s: %v", name, err)
	}
	return capture
}

func globNames(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatalf("glob %s: %v", pattern, err)
	}
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, filepath.Base(match))
	}
	slices.Sort(names)
	return names
}

func healthStatusOf(t *testing.T, registry *health.Registry, key string) health.Entry {
	t.Helper()
	entries, _ := registry.Snapshot()
	for _, entry := range entries {
		if entry.Collector == key {
			return entry
		}
	}
	return health.Entry{}
}

// TestCloseCapturePublishesThePairedArtifact is the whole point of the
// feature: a cumulative profile is meaningless alone, so the closing boundary
// has to produce the second half, named so the two can be found together.
func TestCloseCapturePublishesThePairedArtifact(t *testing.T) {
	h, dir, _ := profileHandler(t, "heap")
	startedAt := time.Date(2026, 8, 5, 12, 0, 0, 0, reportTZ)
	run := RunStart{RunID: "run-a1b2c3d4e5f6", StartedAt: startedAt, Validity: "valid"}

	opened := h.captureRuntimeProfiles(run, ProfilePointOpen, 7)
	closed := h.captureCloseProfiles(RunFinish{
		RunID: run.RunID, Validity: "valid", AcceptedAt: startedAt.Add(time.Minute),
	})

	wantOpen := "20260805-120000_gen7_run-a1b2_heap_open.pprof"
	wantClose := "20260805-120000_gen7_run-a1b2_heap_close.pprof"
	if !slices.Equal(opened, []string{wantOpen}) {
		t.Errorf("opening artifacts = %v, want [%s]", opened, wantOpen)
	}
	// The closing name differs from the opening one only in the point, which
	// is what makes the pair reassemblable from a directory listing.
	if !slices.Equal(closed, []string{wantClose}) {
		t.Errorf("closing artifacts = %v, want [%s]", closed, wantClose)
	}
	for _, name := range []string{wantOpen, wantClose} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, mode)
		}
	}
	if leftovers := globNames(t, dir, "*.tmp"); len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}

	closing := readCapture(t, dir, "20260805-120000_gen7_run-a1b2_heap_close.meta.json")
	if closing.Status != profileStatusOK || closing.File != wantClose {
		t.Errorf("closing record = %+v, want an ok record naming %s", closing, wantClose)
	}
	if closing.Point != ProfilePointClose || closing.RefPhase != profileRefPhaseFinish {
		t.Errorf("closing record point/phase = %s/%s", closing.Point, closing.RefPhase)
	}
	if closing.FinishedAt.Before(closing.StartedAt) {
		t.Errorf("capture instants are out of order: %v → %v", closing.StartedAt, closing.FinishedAt)
	}

	// Both the artifact and its record must be downloadable.
	routes := h.routes()
	for _, name := range []string{wantClose, "20260805-120000_gen7_run-a1b2_heap_close.meta.json"} {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /files/%s = %d, want 200", name, rec.Code)
		}
	}
}

func TestProfileCaptureCarriesCoordinatorWindows(t *testing.T) {
	h, dir, _ := profileHandler(t, "heap")
	openRef := time.Date(2026, 8, 5, 12, 0, 0, 0, reportTZ)
	closeRef := openRef.Add(time.Minute)
	start := RunStart{
		RunID: "run-window-evidence", Epoch: 9, StartedAt: openRef, Validity: "valid",
		GenerationWindow: BoundaryWindow{Min: openRef.Add(-2 * time.Millisecond), Max: openRef, Spread: 2 * time.Millisecond},
		BoundaryWindow:   BoundaryWindow{Min: openRef.Add(-3 * time.Millisecond), Max: openRef, Spread: 3 * time.Millisecond},
	}
	finish := RunFinish{
		RunID: start.RunID, Epoch: start.Epoch, AcceptedAt: closeRef, Validity: "valid",
		GenerationWindow: BoundaryWindow{Min: closeRef.Add(-4 * time.Millisecond), Max: closeRef, Spread: 4 * time.Millisecond},
		BoundaryWindow:   BoundaryWindow{Min: closeRef.Add(-5 * time.Millisecond), Max: closeRef, Spread: 5 * time.Millisecond},
	}

	h.captureRuntimeProfiles(start, ProfilePointOpen, 4)
	h.captureCloseProfiles(finish)
	closing := readCapture(t, dir, "20260805-120000_gen4_run-wind_heap_close.meta.json")
	if closing.Epoch != 9 || !closing.RefAt.Equal(closeRef) || closing.RefSpreadNs != int64(4*time.Millisecond) {
		t.Fatalf("closing reference = %+v", closing)
	}
	if !closing.BoundaryAt.Equal(closeRef) || closing.BoundarySpreadNs != int64(5*time.Millisecond) {
		t.Fatalf("closing boundary = %+v", closing)
	}
	manifest := h.profileManifest(start.RunID)
	if manifest == nil || manifest.Epoch != 9 || len(manifest.Pairs) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if got := manifest.Pairs[0].RunSpanNs; got != int64(time.Minute) {
		t.Fatalf("run span = %s, want 1m", time.Duration(got))
	}
}

// TestCloseCaptureIsIdempotentAcrossFinishThenSave protects the capture point.
// POST /finish and POST /save both close the same run — the second replays the
// first's accepted boundary — and a second capture there would be taken after
// the drain and the snapshot build, which is the tail the close point exists
// to keep out of the difference.
func TestCloseCaptureIsIdempotentAcrossFinishThenSave(t *testing.T) {
	h, dir, _ := profileHandler(t, "heap")
	run := RunStart{RunID: "run-idem0123", StartedAt: time.Now(), Validity: "valid"}
	h.captureRuntimeProfiles(run, ProfilePointOpen, 4)
	finished := RunFinish{RunID: run.RunID, Validity: "valid", AcceptedAt: time.Now()}

	first := h.captureCloseProfiles(finished)
	if len(first) != 1 {
		t.Fatalf("first close = %v, want one artifact", first)
	}
	before := readCapture(t, dir, strings.TrimSuffix(first[0], profileArtifactExt)+profileSidecarExt)

	second := h.captureCloseProfiles(finished)
	if !slices.Equal(first, second) {
		t.Errorf("replayed close = %v, want the existing %v", second, first)
	}
	if names := globNames(t, dir, "*_close"+profileArtifactExt); len(names) != 1 {
		t.Errorf("closing artifacts = %v, want exactly one", names)
	}
	after := readCapture(t, dir, before.Sidecar)
	if !after.StartedAt.Equal(before.StartedAt) {
		t.Errorf("record was rewritten: %v → %v", before.StartedAt, after.StartedAt)
	}

	manifest := h.profileManifest(run.RunID)
	if manifest == nil {
		t.Fatal("run has no manifest")
	}
	closes := 0
	for _, capture := range manifest.Captures {
		if capture.Point == ProfilePointClose {
			closes++
		}
	}
	if closes != 1 || len(manifest.Pairs) != 1 {
		t.Errorf("manifest has %d closing captures and %d pairs, want 1 and 1",
			closes, len(manifest.Pairs))
	}
}

// TestCloseCaptureWithoutAnOpeningHalfWritesNothing keeps a lone artifact from
// masquerading as a pair. Nothing can be differenced against a boundary that
// was never captured — including the previous process's, whose cumulative
// counters started over.
func TestCloseCaptureWithoutAnOpeningHalfWritesNothing(t *testing.T) {
	h, dir, _ := profileHandler(t, "heap")
	if names := h.captureCloseProfiles(RunFinish{RunID: "run-unknown", AcceptedAt: time.Now()}); names != nil {
		t.Errorf("close artifacts = %v, want none", names)
	}
	if names := h.captureCloseProfiles(RunFinish{AcceptedAt: time.Now()}); names != nil {
		t.Errorf("close artifacts for an anonymous run = %v, want none", names)
	}
	if names := globNames(t, dir, "*"); len(names) > 0 {
		t.Errorf("files = %v, want an empty directory", names)
	}
}

// TestProfilePairMetadataRecordsTheResidual pins the numbers that say how
// badly the pair approximates the run. They are the reason the capture point
// was moved to the freeze: without them "close enough" is an assertion rather
// than a measurement.
func TestProfilePairMetadataRecordsTheResidual(t *testing.T) {
	h, dir, registry := profileHandler(t, "heap")
	openRef := time.Now()
	run := RunStart{RunID: "run-meta4567", StartedAt: openRef, Validity: "valid"}
	h.captureRuntimeProfiles(run, ProfilePointOpen, 9)
	closeRef := time.Now()
	h.captureCloseProfiles(RunFinish{RunID: run.RunID, Validity: "valid", AcceptedAt: closeRef})

	manifest := h.profileManifest(run.RunID)
	if manifest == nil || len(manifest.Pairs) != 1 {
		t.Fatalf("manifest = %+v, want one pair", manifest)
	}
	for _, capture := range manifest.Captures {
		if capture.Status == profileStatusOK && len(capture.SHA256) != 64 {
			t.Fatalf("successful capture has no full SHA-256: %#v", capture)
		}
	}
	pair := manifest.Pairs[0]
	if pair.HeadLossNs < 0 || pair.TailExcessNs < 0 {
		t.Errorf("residuals must be non-negative: %+v", pair)
	}
	if pair.ApproxErrorNs != pair.HeadLossNs+pair.TailExcessNs {
		t.Errorf("approx error = %d, want head %d + tail %d",
			pair.ApproxErrorNs, pair.HeadLossNs, pair.TailExcessNs)
	}
	if want := closeRef.Sub(openRef).Nanoseconds(); pair.RunSpanNs != want {
		t.Errorf("run span = %d, want %d (the distance between the two boundaries)",
			pair.RunSpanNs, want)
	}
	// The whole reason the closing capture happens before the drain: the tail
	// is milliseconds, not the background budget of drain plus snapshot build.
	if pair.TailExcessNs > (500 * time.Millisecond).Nanoseconds() {
		t.Errorf("tail excess = %v, want the capture to precede the background work",
			time.Duration(pair.TailExcessNs))
	}
	if pair.OpenGate != openGatePostStartReturn {
		t.Errorf("open gate = %q, want %q", pair.OpenGate, openGatePostStartReturn)
	}
	if !strings.Contains(pair.DiffCommand, "-diff_base") ||
		!strings.Contains(pair.DiffCommand, pair.OpenFile) ||
		!strings.Contains(pair.DiffCommand, pair.CloseFile) {
		t.Errorf("diff command = %q, want it to difference both halves", pair.DiffCommand)
	}
	if lag := healthStatusOf(t, registry, healthProfileLag); lag.Status != health.StatusOK {
		t.Errorf("%s = %+v, want ok for a prompt capture", healthProfileLag, lag)
	}
	if pairs := healthStatusOf(t, registry, healthProfileIncomplete); pairs.Status != health.StatusOK {
		t.Errorf("%s = %+v, want ok for a complete pair", healthProfileIncomplete, pairs)
	}
	// A superseded artifact from the earlier naming must never be adopted as a
	// half: it was captured at a different point and means something else.
	legacy := strings.TrimSuffix(pair.CloseFile, "_close"+profileArtifactExt) + "_save" + profileArtifactExt
	if err := os.WriteFile(filepath.Join(dir, legacy), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again := h.profileManifest(run.RunID); again.Pairs[0].CloseFile != pair.CloseFile {
		t.Errorf("pair close file = %q, want the _close artifact", again.Pairs[0].CloseFile)
	}
}

// TestProfilePairTextAlwaysDeclaresTheApproximation fixes the wording. The
// pair is a difference of process-wide cumulative profiles with a post-finish
// tail, and a reader who is not told that will read it as the run.
func TestProfilePairTextAlwaysDeclaresTheApproximation(t *testing.T) {
	tests := []struct {
		name  string
		pair  ProfilePair
		want  []string
		avoid string
	}{
		{
			name: "immediate capture",
			pair: ProfilePair{
				HeadLossNs: (41 * time.Millisecond).Nanoseconds(),
				//nolint:mnd // the residual values are the point of the fixture
				TailExcessNs:  (118 * time.Millisecond).Nanoseconds(),
				ApproxErrorNs: (159 * time.Millisecond).Nanoseconds(),
				RunSpanNs:     (59676 * time.Millisecond).Nanoseconds(),
				OpenGate:      openGatePostStartReturn,
			},
			want:  []string{"run 冒頭の 41ms を含まず", "finish freeze 後の 118ms を含みます", "run 単位のプロファイルではありません"},
			avoid: "ベンチ負荷はありません",
		},
		{
			name: "capture held for the previous drain",
			pair: ProfilePair{
				HeadLossNs:    (5 * time.Second).Nanoseconds(),
				TailExcessNs:  0,
				ApproxErrorNs: (5 * time.Second).Nanoseconds(),
				RunSpanNs:     time.Minute.Nanoseconds(),
				OpenGate:      openGatePostPrevDrain,
			},
			want: []string{"run 冒頭の 5.000s を含まず", "finish freeze 後の 0ms を含みます", "ベンチ負荷はありません"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notes := strings.Join(tt.pair.Notes(), "\n")
			for _, want := range tt.want {
				if !strings.Contains(notes, want) {
					t.Errorf("notes = %q, want it to contain %q", notes, want)
				}
			}
			if tt.avoid != "" && strings.Contains(notes, tt.avoid) {
				t.Errorf("notes = %q, must not contain %q", notes, tt.avoid)
			}
			if residual := tt.pair.ResidualText(); !strings.Contains(residual, "欠落") ||
				!strings.Contains(residual, "run 長の") {
				t.Errorf("residual text = %q", residual)
			}
		})
	}
}

// TestProfilePairLaggingUsesOneThreshold keeps the page badge and the health
// verdict on the same rule: a capture that waited for the previous run's drain
// was told to wait, and calling that a defect teaches the reader to ignore the
// warning that matters.
func TestProfilePairLaggingUsesOneThreshold(t *testing.T) {
	tests := []struct {
		name string
		pair ProfilePair
		want bool
	}{
		{
			name: "prompt capture",
			pair: ProfilePair{HeadLossNs: 5e6, TailExcessNs: 5e6, RunSpanNs: time.Minute.Nanoseconds()},
			want: false,
		},
		{
			name: "head loss behind the previous drain",
			pair: ProfilePair{HeadLossNs: (5 * time.Second).Nanoseconds(),
				RunSpanNs: time.Minute.Nanoseconds(), OpenGate: openGatePostPrevDrain},
			want: false,
		},
		{
			name: "head loss with nothing to wait for",
			pair: ProfilePair{HeadLossNs: (5 * time.Second).Nanoseconds(),
				RunSpanNs: time.Minute.Nanoseconds(), OpenGate: openGatePostStartReturn},
			want: true,
		},
		{
			name: "tail beyond the absolute limit",
			pair: ProfilePair{TailExcessNs: (2 * time.Second).Nanoseconds(),
				RunSpanNs: time.Hour.Nanoseconds()},
			want: true,
		},
		{
			name: "tail beyond one percent of a short run",
			pair: ProfilePair{TailExcessNs: (100 * time.Millisecond).Nanoseconds(),
				RunSpanNs: time.Second.Nanoseconds()},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pair.Lagging(); got != tt.want {
				t.Errorf("lagging = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAbortedRunLeavesNoOrphanPretendingToBeAPair covers both ways a run dies:
// an explicit abort, and being preempted by the next reset. Either way the
// opening artifact is left with no partner, and it must say so.
func TestAbortedRunLeavesNoOrphanPretendingToBeAPair(t *testing.T) {
	tests := []struct {
		name string
		kill func(h *handler, runID string)
	}{
		{
			name: "aborted",
			kill: func(h *handler, runID string) { h.abortRunProfiles(runID) },
		},
		{
			name: "preempted by the next run",
			kill: func(h *handler, _ string) {
				h.captureRuntimeProfiles(
					RunStart{RunID: "run-successor", StartedAt: time.Now(), Validity: "valid"},
					ProfilePointOpen, 2)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, dir, registry := profileHandler(t, "heap")
			run := RunStart{RunID: "run-dead" + tt.name[:4], StartedAt: time.Now(), Validity: "valid"}
			opened := h.captureRuntimeProfiles(run, ProfilePointOpen, 1)
			if len(opened) != 1 {
				t.Fatalf("opening artifacts = %v, want one", opened)
			}
			tt.kill(h, run.RunID)

			// A dead run's closing capture is refused for good: replaying it
			// must not resurrect the pair minutes later.
			if names := h.captureCloseProfiles(RunFinish{RunID: run.RunID, AcceptedAt: time.Now()}); names != nil {
				t.Errorf("close artifacts = %v, want none for a run that never froze", names)
			}
			base := strings.TrimSuffix(opened[0], "_open"+profileArtifactExt)
			if _, err := os.Stat(filepath.Join(dir, base+"_close"+profileArtifactExt)); !os.IsNotExist(err) {
				t.Errorf("closing artifact exists (err %v), want none", err)
			}
			if opening := readCapture(t, dir, base+"_open"+profileSidecarExt); !opening.Orphan {
				t.Errorf("opening record = %+v, want it marked as an orphan", opening)
			}
			closing := readCapture(t, dir, base+"_close"+profileSidecarExt)
			if closing.Status != profileStatusSkipped || closing.Code != profileCodeAborted || closing.File != "" {
				t.Errorf("closing record = %+v, want a skipped/aborted record with no file", closing)
			}
			manifest := h.profileManifest(run.RunID)
			if manifest == nil || len(manifest.Pairs) != 0 {
				t.Errorf("manifest = %+v, want no pair", manifest)
			}
			if entry := healthStatusOf(t, registry, healthProfileIncomplete); entry.Status != health.StatusDegraded {
				t.Errorf("%s = %+v, want degraded", healthProfileIncomplete, entry)
			}
		})
	}
}

// TestProfileRetentionDropsOrphansFirst enforces the deletion order. An orphan
// can never be differenced, an invalid run's interval cannot be trusted, and
// only then does age decide. Artifact and record always go together, and
// nothing that is not a boundary artifact is touched at all.
func TestProfileRetentionDropsOrphansFirst(t *testing.T) {
	dir := t.TempDir()
	write := func(names ...string) {
		t.Helper()
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	orphan := "20260805-115900_gen1_run-aaaa"
	older := "20260805-120000_gen2_run-bbbb"
	newer := "20260805-120100_gen3_run-cccc"
	write(orphan+"_heap_open.pprof", orphan+"_heap_open.meta.json")
	for _, prefix := range []string{older, newer} {
		write(prefix+"_heap_open.pprof", prefix+"_heap_open.meta.json",
			prefix+"_heap_close.pprof", prefix+"_heap_close.meta.json")
	}
	// None of these belong to a managed profile group and none may be deleted.
	bystanders := []string{
		older + "_heap_save.pprof",
		"20260805-120000-000001_gen2_abc1234.html",
		"20260805-120000-000001_gen2_abc1234.json",
	}
	write(bystanders...)

	// Two runs fit, so only the orphan goes.
	pruneProfileArtifacts(dir, 2, 1<<30, "", nil)
	if names := globNames(t, dir, orphan+"*"); len(names) != 0 {
		t.Errorf("orphan files = %v, want them dropped first", names)
	}
	if names := globNames(t, dir, older+"_heap_open*"); len(names) != 2 {
		t.Errorf("older run opening files = %v, want artifact and record kept", names)
	}
	if names := globNames(t, dir, older+"_heap_close*"); len(names) != 2 {
		t.Errorf("older run closing files = %v, want artifact and record kept", names)
	}
	for _, name := range bystanders {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}

	// One run fits, and the newer one is the untrustworthy one: validity beats
	// age.
	pruneProfileArtifacts(dir, 1, 1<<30, "", map[string]bool{newer: true})
	if names := globNames(t, dir, newer+"*"); len(names) != 0 {
		t.Errorf("invalid run files = %v, want them dropped before older valid ones", names)
	}
	if names := globNames(t, dir, older+"_heap_close*"); len(names) != 2 {
		t.Errorf("older run files = %v, want the valid run kept", names)
	}
}

func TestProfileRetentionTreatsFixedCPUAndSidecarAsOneCompleteGroup(t *testing.T) {
	dir := t.TempDir()
	older := "20260805-120000_gen1_cpu"
	newer := "20260805-120100_gen2_cpu"
	for _, base := range []string{older, newer} {
		for _, suffix := range []string{profileArtifactExt, profileSidecarExt} {
			if err := os.WriteFile(filepath.Join(dir, base+suffix), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	pruneProfileArtifacts(dir, 1, 1<<30, "", nil)
	if files := globNames(t, dir, older+"*"); len(files) != 0 {
		t.Fatalf("old fixed CPU group retained: %v", files)
	}
	if files := globNames(t, dir, newer+"*"); len(files) != 2 {
		t.Fatalf("new fixed CPU group = %v, want profile and sidecar", files)
	}
}

// TestProfileRetentionNeverDropsTheRunBeingCaptured guards the one group that
// is not safe to reclaim: the pair currently being assembled.
func TestProfileRetentionNeverDropsTheRunBeingCaptured(t *testing.T) {
	dir := t.TempDir()
	current := "20260805-120100_gen3_run-cccc"
	for _, name := range []string{current + "_heap_open.pprof", current + "_heap_open.meta.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneProfileArtifacts(dir, 0, 0, current, nil)
	if names := globNames(t, dir, current+"*"); len(names) != 2 {
		t.Errorf("files = %v, want the in-flight run kept", names)
	}
}

// TestProfileArtifactPrefixIgnoresEverythingElse is what keeps the superseded
// "_save"/"_reset" artifacts and saved snapshots out of both pairing and
// retention. Fixed CPU files are intentionally retention-only groups.
func TestProfileArtifactPrefixIgnoresEverythingElse(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		wantOK bool
	}{
		{name: "20260805-120000_gen7_run-a1b2_heap_open.pprof", want: "20260805-120000_gen7_run-a1b2", wantOK: true},
		{name: "20260805-120000_gen7_run-a1b2_heap_close.meta.json", want: "20260805-120000_gen7_run-a1b2", wantOK: true},
		{name: "20260805-120000_gen7_run-a1b2_heap_save.pprof"},
		{name: "20260805-120000_gen7_run-a1b2_heap_reset.pprof"},
		{name: "20260805-120000_gen2_cpu.pprof", want: "20260805-120000_gen2_cpu", wantOK: true},
		{name: "20260805-120000_gen2_cpu.meta.json", want: "20260805-120000_gen2_cpu", wantOK: true},
		{name: "cpu_019876543210aaaaaaaaaaaaaaaaaaaa.pprof", want: "cpu_019876543210aaaaaaaaaaaaaaaaaaaa", wantOK: true},
		{name: "cpu_019876543210aaaaaaaaaaaaaaaaaaaa.meta.json", want: "cpu_019876543210aaaaaaaaaaaaaaaaaaaa", wantOK: true},
		{name: "cpu_019876543210aaaaaaaaaaaaaaaaaaaa.coverage.000001." + strings.Repeat("b", 64) + ".json", want: "cpu_019876543210aaaaaaaaaaaaaaaaaaaa", wantOK: true},
		{name: "cpu_019876543210aaaaaaaaaaaaaaaaaaaa.coverage.1." + strings.Repeat("b", 64) + ".json"},
		{name: "20260805-120000-000001_gen2_abc1234.json"},
		{name: "20260805-120000-000001_gen2_abc1234.html"},
		{name: "20260805-120000_gen7_run-a1b2_heap_open.pprof.tmp"},
		{name: "open.pprof"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := profileArtifactPrefix(tt.name)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("prefix = %q,%v, want %q,%v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestProfileRetentionOrdersRunCPUAndTimestampGroupsChronologically(t *testing.T) {
	dir := t.TempDir()
	oldTime := time.Date(2026, 8, 5, 11, 59, 0, 0, reportTZ)
	oldID := fmt.Sprintf("%012x%s", oldTime.UnixMilli(), strings.Repeat("a", 20))
	oldCPU := "cpu_" + oldID
	newer := "20260805-120000_gen2_run-bbbb"
	for _, name := range []string{
		oldCPU + profileArtifactExt, oldCPU + profileSidecarExt,
		newer + "_heap_open.pprof", newer + "_heap_open.meta.json",
		newer + "_heap_close.pprof", newer + "_heap_close.meta.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneProfileArtifacts(dir, 1, 1<<30, "", nil)
	if files := globNames(t, dir, oldCPU+"*"); len(files) != 0 {
		t.Fatalf("older run CPU retained: %v", files)
	}
	if files := globNames(t, dir, newer+"*"); len(files) != 4 {
		t.Fatalf("newer cumulative group = %v", files)
	}
}

// TestCaptureSkipsProfilesWhoseRateIsZero pins the default: mutex and block
// only reach RuntimeProfiles when their rate is non-zero and heap only under
// ISUTOOLS_HEAP_PROFILE, so a kind that is not listed must not be written at
// either boundary even though the runtime can always look it up.
func TestCaptureSkipsProfilesWhoseRateIsZero(t *testing.T) {
	tests := []struct {
		name  string
		kinds []string
		want  []string
	}{
		{name: "all three off by default"},
		{name: "heap opted in", kinds: []string{"heap"}, want: []string{"heap"}},
		{name: "mutex rate set", kinds: []string{"mutex", "heap"}, want: []string{"mutex", "heap"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, dir, _ := profileHandler(t, tt.kinds...)
			run := RunStart{RunID: "run-rates01", StartedAt: time.Now(), Validity: "valid"}
			h.captureRuntimeProfiles(run, ProfilePointOpen, 1)
			h.captureCloseProfiles(RunFinish{RunID: run.RunID, AcceptedAt: time.Now()})

			for _, kind := range []string{"mutex", "block", "heap"} {
				want := 0
				if slices.Contains(tt.want, kind) {
					want = 2 // one artifact at each boundary
				}
				if names := globNames(t, dir, "*_"+kind+"_*"+profileArtifactExt); len(names) != want {
					t.Errorf("%s artifacts = %v, want %d", kind, names, want)
				}
			}
		})
	}
}

// TestCaptureLeaseExceededSkipsTheKind records what the lease does. WriteTo
// cannot be interrupted, so the lease can only decide whether the next kind
// starts — and a kind that never started is recorded rather than forgotten.
func TestCaptureLeaseExceededSkipsTheKind(t *testing.T) {
	h, dir, _ := profileHandler(t, "heap")
	spec := profileCaptureSpec{
		runID:      "run-lease001",
		point:      ProfilePointOpen,
		generation: 5,
		stamp:      time.Now(),
		openGate:   openGatePostStartReturn,
		ref:        profileRef{phase: profileRefPhaseStart, at: time.Now()},
	}
	if names := h.captureBoundaryBefore(spec, time.Now().Add(-time.Millisecond)); names != nil {
		t.Errorf("artifacts = %v, want none once the lease is spent", names)
	}
	if names := globNames(t, dir, "*"+profileArtifactExt); len(names) != 0 {
		t.Errorf("artifacts = %v, want none", names)
	}
	records := globNames(t, dir, "*"+profileSidecarExt)
	if len(records) != 1 {
		t.Fatalf("records = %v, want the skip written down", records)
	}
	skipped := readCapture(t, dir, records[0])
	if skipped.Status != profileStatusSkipped || skipped.Code != profileCodeLeaseExceeded {
		t.Errorf("record = %+v, want skipped/%s", skipped, profileCodeLeaseExceeded)
	}
	// Nothing was published, so the point is not spent: the next attempt at
	// the same boundary may still succeed.
	if names := h.captureBoundaryBefore(spec, time.Now().Add(profileCaptureLease)); len(names) != 1 {
		t.Errorf("retry = %v, want the artifact this time", names)
	}
}

// TestCaptureWriteFailureIsRecordedAndNonFatal covers the fail-open contract:
// a capture that cannot be published costs the caller nothing, leaves no
// temporary file to be mistaken for one, and says why in the record.
func TestCaptureWriteFailureIsRecordedAndNonFatal(t *testing.T) {
	h, dir, registry := profileHandler(t, "heap")
	run := RunStart{RunID: "run-fail0001", StartedAt: time.Now(), Validity: "valid"}
	// Publication renames the temporary file onto this name. A directory there
	// fails that rename after the profile has been written in full, which is
	// the only failure mode that can leave a temporary file behind.
	blocked := profileArtifactName(run, ProfilePointOpen, 3, "heap")
	if err := os.Mkdir(filepath.Join(dir, blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	if names := h.captureRuntimeProfiles(run, ProfilePointOpen, 3); names != nil {
		t.Errorf("artifacts = %v, want none published", names)
	}
	if leftovers := globNames(t, dir, "*.tmp"); len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
	record := readCapture(t, dir, strings.TrimSuffix(blocked, profileArtifactExt)+profileSidecarExt)
	if record.Status != profileStatusFailed || record.Code != profileCodeWriteFailed {
		t.Errorf("record = %+v, want failed/%s", record, profileCodeWriteFailed)
	}
	if record.File != "" {
		t.Errorf("record names %q, want no file for a failed capture", record.File)
	}
	if entry := healthStatusOf(t, registry, healthProfileFailed); entry.Status != health.StatusDegraded {
		t.Errorf("%s = %+v, want degraded", healthProfileFailed, entry)
	}
	// The failure is not the run's problem: a later close on a run that never
	// published an opening half simply captures nothing.
	if names := h.captureCloseProfiles(RunFinish{RunID: run.RunID, AcceptedAt: time.Now()}); names != nil {
		t.Errorf("close artifacts = %v, want none", names)
	}
	manifest := h.profileManifest(run.RunID)
	if manifest == nil || len(manifest.Expected) != 1 || len(manifest.Expected[0].Inputs) != 2 || manifest.Expected[0].Inputs[0].File == "" {
		t.Fatalf("failed capture lost expected inputs: %#v", manifest)
	}
}

// TestSidecarWriteFailureLeavesNoTemporaryFile covers the sidecar's half of the
// fail-open contract. The artifact path already reaps its temporary file on
// every failure; the record path has to as well, because a ".tmp" belongs to no
// run group and retention would therefore never reclaim it.
func TestSidecarWriteFailureLeavesNoTemporaryFile(t *testing.T) {
	capture := ProfileCapture{
		RunID:   "run-sidecar1",
		Point:   ProfilePointOpen,
		Kind:    "heap",
		Sidecar: "20260805-120000_gen1_run-side_heap_open" + profileSidecarExt,
		Status:  profileStatusOK,
	}
	tests := []struct {
		name  string
		block func(t *testing.T, tmp string)
	}{
		{
			// A directory at the temporary path fails the open for every user,
			// root included, so the failure is real on any machine.
			name: "temporary path is a directory",
			block: func(t *testing.T, tmp string) {
				t.Helper()
				if err := os.Mkdir(tmp, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The shape a crashed writer actually leaves behind: a temporary
			// file that cannot be rewritten. Reaping it is what lets the next
			// capture publish at all.
			name: "temporary path is an unwritable file",
			block: func(t *testing.T, tmp string) {
				t.Helper()
				if os.Geteuid() == 0 {
					t.Skip("root ignores the permission bits this case relies on")
				}
				if err := os.WriteFile(tmp, []byte("stale"), 0o400); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, dir, _ := profileHandler(t, "heap")
			tt.block(t, filepath.Join(dir, capture.Sidecar+profileTempExt))

			h.writeProfileSidecar(capture)

			if _, err := os.Stat(filepath.Join(dir, capture.Sidecar)); !os.IsNotExist(err) {
				t.Fatalf("stat %s = %v, want the blocked write to have published nothing",
					capture.Sidecar, err)
			}
			if leftovers := globNames(t, dir, "*"+profileTempExt); len(leftovers) > 0 {
				t.Errorf("temporary files left behind: %v", leftovers)
			}
		})
	}
}

// TestRetentionSweepsStaleTemporaryFiles pins the only reaper a temporary file
// has.
//
// A process killed between the write and the rename leaves one behind, and it
// is not an artifact: profileArtifactPrefix refuses the name, so no run group
// ever owns it and the by-run retention can never drop it. It has to be swept
// by age instead — and while the directory is still well inside its limits,
// which is where a crashed capture's debris actually sits.
func TestRetentionSweepsStaleTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return name
	}
	prefix := "20260805-120000_gen1_run-aaaa"
	// A complete pair, so the directory holds a run worth keeping and stays far
	// inside both retention limits.
	kept := []string{
		write(prefix+"_heap_open"+profileArtifactExt, 0),
		write(prefix+"_heap_open"+profileSidecarExt, 0),
		write(prefix+"_heap_close"+profileArtifactExt, 0),
		write(prefix+"_heap_close"+profileSidecarExt, 0),
		// A capture in flight owns its temporary file and must survive.
		write(prefix+"_block_open"+profileArtifactExt+profileTempExt, time.Minute),
		// Another component's temporary file in the same directory belongs to
		// its own owner, however old it is.
		write("20260805-120000-000001_gen1_abc1234.json"+profileTempExt, 48*time.Hour),
	}
	swept := []string{
		write(prefix+"_heap_open"+profileArtifactExt+profileTempExt, 2*profileTempMaxAge),
		write(prefix+"_heap_open"+profileSidecarExt+profileTempExt, 2*profileTempMaxAge),
	}

	pruneProfileArtifacts(dir, profileRetentionRuns, profileRetentionBytes, "", nil)

	for _, name := range swept {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("stat %s = %v, want the stale temporary file swept", name, err)
		}
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

// TestBoundaryProfilerOpensThePairForARunTheHandlerNeverStarted is the wiring
// test for the documented initialize contract: isutools.ResetNow opens the run,
// POST /save closes it, and the pair has to come out complete anyway.
//
// Without an opening capture on that path every closing capture finds no
// opening half to difference against and writes nothing at all, which is a
// feature that is present, configured, and permanently silent.
func TestBoundaryProfilerOpensThePairForARunTheHandlerNeverStarted(t *testing.T) {
	dir := t.TempDir()
	provider := Provider{DataDir: dir, RuntimeProfiles: []string{"heap"}, Health: health.NewRegistry()}
	profiler := NewBoundaryProfiler(provider)
	if profiler == nil {
		t.Fatal("a configured provider must produce a capturer")
	}
	startedAt := time.Date(2026, 8, 5, 12, 0, 0, 0, reportTZ)
	run := RunStart{RunID: "run-outside01", Epoch: 3, StartedAt: startedAt, Validity: "valid"}

	opened := profiler.CaptureOpen(run, 7)
	wantOpen := "20260805-120000_gen7_run-outs_heap_open" + profileArtifactExt
	if !slices.Equal(opened, []string{wantOpen}) {
		t.Fatalf("opening artifacts = %v, want [%s]", opened, wantOpen)
	}

	// The closing half comes from the transport, on the same data directory:
	// that is the real topology, since the run was never opened through it.
	h := &handler{p: provider, operation: make(chan struct{}, 1)}
	closed := h.captureCloseProfiles(RunFinish{
		RunID: run.RunID, Epoch: run.Epoch, Validity: "valid",
		AcceptedAt: startedAt.Add(time.Minute),
	})
	wantClose := "20260805-120000_gen7_run-outs_heap_close" + profileArtifactExt
	if !slices.Equal(closed, []string{wantClose}) {
		t.Fatalf("closing artifacts = %v, want [%s]", closed, wantClose)
	}
	manifest := h.profileManifest(run.RunID)
	if manifest == nil || len(manifest.Pairs) != 1 {
		t.Fatalf("manifest = %+v, want exactly one pair", manifest)
	}
	if pair := manifest.Pairs[0]; pair.OpenFile != wantOpen || pair.CloseFile != wantClose {
		t.Errorf("pair = %+v, want the two halves this run published", pair)
	}
	for _, name := range []string{wantOpen, wantClose} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Size() == 0 {
			t.Errorf("stat %s = (%v, %v), want a published artifact", name, info, err)
		}
	}
	if leftovers := globNames(t, dir, "*"+profileTempExt); len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// TestBoundaryProfilerWithoutAConfigurationCapturesNothing keeps the caller
// free of branches: profiling is off by default, so the entry point has to be
// safe to call unconditionally.
func TestBoundaryProfilerWithoutAConfigurationCapturesNothing(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		provider Provider
	}{
		{name: "no data directory", provider: Provider{RuntimeProfiles: []string{"heap"}}},
		{name: "no profile kinds", provider: Provider{DataDir: dir}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profiler := NewBoundaryProfiler(tt.provider)
			if profiler != nil {
				t.Fatalf("capturer = %+v, want none for %+v", profiler, tt.provider)
			}
			run := RunStart{RunID: "run-noconfig", StartedAt: time.Now()}
			if names := profiler.CaptureOpen(run, 1); names != nil {
				t.Errorf("artifacts = %v, want none", names)
			}
		})
	}
	if names := globNames(t, dir, "*"); len(names) > 0 {
		t.Errorf("files = %v, want an empty directory", names)
	}
}

// TestBoundaryProfilerCopiesItsKinds keeps a caller's later edit of its own
// slice from changing what an opened run captures at its closing boundary,
// which would leave a pair whose halves are not the same set of profiles.
func TestBoundaryProfilerCopiesItsKinds(t *testing.T) {
	dir := t.TempDir()
	kinds := []string{"heap"}
	profiler := NewBoundaryProfiler(Provider{DataDir: dir, RuntimeProfiles: kinds})
	kinds[0] = "mutex"
	run := RunStart{RunID: "run-copy0001", StartedAt: time.Now()}
	names := profiler.CaptureOpen(run, 1)
	if len(names) != 1 || !strings.Contains(names[0], "_heap_open") {
		t.Fatalf("artifacts = %v, want the heap profile the capturer was built with", names)
	}
}

// TestOrderProfileKindsPutsTheCheapOnesFirst keeps a large heap write from
// pushing the moment the mutex profile is taken away from the boundary.
func TestOrderProfileKindsPutsTheCheapOnesFirst(t *testing.T) {
	tests := []struct {
		name  string
		kinds []string
		want  []string
	}{
		{name: "already ordered", kinds: []string{"mutex", "block", "heap"}, want: []string{"mutex", "block", "heap"}},
		{name: "reordered", kinds: []string{"heap", "block", "mutex"}, want: []string{"mutex", "block", "heap"}},
		{name: "duplicates dropped", kinds: []string{"heap", "heap"}, want: []string{"heap"}},
		{name: "unknown kinds kept last", kinds: []string{"goroutine", "mutex"}, want: []string{"mutex", "goroutine"}},
		{name: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := orderProfileKinds(tt.kinds); !slices.Equal(got, tt.want) {
				t.Errorf("order = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCaptureWithoutARunCoordinatorWritesTheArtifactAlone covers the legacy
// wiring: profiles enabled on a process whose resets open no run. There is
// nothing to pair, so the artifact is published as what it is — a lone
// cumulative snapshot — with no record and no manifest to imply otherwise.
func TestCaptureWithoutARunCoordinatorWritesTheArtifactAlone(t *testing.T) {
	h, dir, _ := profileHandler(t, "heap")
	names := h.captureRuntimeProfiles(RunStart{StartedAt: time.Now()}, ProfilePointOpen, 2)
	if len(names) != 1 || !strings.Contains(names[0], "_norun_heap_open") {
		t.Fatalf("artifacts = %v, want one anonymous artifact", names)
	}
	if records := globNames(t, dir, "*"+profileSidecarExt); len(records) != 0 {
		t.Errorf("records = %v, want none without a run to record", records)
	}
	if manifest := h.profileManifest(""); manifest != nil {
		t.Errorf("manifest = %+v, want none", manifest)
	}
}

// TestAnonymousCaptureFailuresAreSilent keeps the legacy path fail-open too.
func TestAnonymousCaptureFailuresAreSilent(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		provider Provider
		deadline time.Duration
	}{
		{
			name:     "lease already spent",
			provider: Provider{DataDir: dir, RuntimeProfiles: []string{"heap"}},
			deadline: -time.Millisecond,
		},
		{
			name:     "unknown kind",
			provider: Provider{DataDir: dir, RuntimeProfiles: []string{"nonexistent"}},
			deadline: profileCaptureLease,
		},
		{
			name:     "unwritable directory",
			provider: Provider{DataDir: filepath.Join(dir, "missing"), RuntimeProfiles: []string{"heap"}},
			deadline: profileCaptureLease,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &handler{p: tt.provider, operation: make(chan struct{}, 1)}
			spec := profileCaptureSpec{point: ProfilePointOpen, generation: 1, stamp: time.Now()}
			if names := h.captureBoundaryBefore(spec, time.Now().Add(tt.deadline)); names != nil {
				t.Errorf("artifacts = %v, want none", names)
			}
			if leftovers := globNames(t, dir, "*.tmp"); len(leftovers) > 0 {
				t.Errorf("temporary files left behind: %v", leftovers)
			}
		})
	}
}

// TestCaptureIntoAMissingDirectoryIsRecordedNotFatal exercises the path where
// even the record cannot be written: the capture still costs the caller
// nothing.
func TestCaptureIntoAMissingDirectoryIsRecordedNotFatal(t *testing.T) {
	h := &handler{
		p: Provider{DataDir: filepath.Join(t.TempDir(), "missing"),
			RuntimeProfiles: []string{"heap"}, Health: health.NewRegistry()},
		operation: make(chan struct{}, 1),
	}
	run := RunStart{RunID: "run-nodir001", StartedAt: time.Now()}
	if names := h.captureRuntimeProfiles(run, ProfilePointOpen, 1); names != nil {
		t.Errorf("artifacts = %v, want none", names)
	}
	if entry := healthStatusOf(t, h.p.Health, healthProfileFailed); entry.Status != health.StatusDegraded {
		t.Errorf("%s = %+v, want degraded", healthProfileFailed, entry)
	}
}

// TestProfileBookkeepingIgnoresUnknownRuns keeps the accessors the transport
// calls from inventing state for a run that captured nothing.
func TestProfileBookkeepingIgnoresUnknownRuns(t *testing.T) {
	h, _, _ := profileHandler(t, "heap")
	h.abortRunProfiles("")
	h.abortRunProfiles("run-never-seen")
	if manifest := h.profileManifest("run-never-seen"); manifest != nil {
		t.Errorf("manifest = %+v, want none", manifest)
	}
	off := &handler{p: Provider{RuntimeProfiles: []string{"heap"}}, operation: make(chan struct{}, 1)}
	off.abortRunProfiles("run-any")
	if manifest := off.profileManifest("run-any"); manifest != nil {
		t.Errorf("manifest without a data directory = %+v, want none", manifest)
	}
}

// TestClosedRunSurvivesTheNextRunOpening keeps the orphan marking from eating
// good pairs: only a run that never closed is an orphan, and a run whose
// verdict is already settled is never rewritten by a late abort.
func TestClosedRunSurvivesTheNextRunOpening(t *testing.T) {
	h, dir, _ := profileHandler(t, "heap")
	first := RunStart{RunID: "run-first001", StartedAt: time.Now(), Validity: "valid"}
	opened := h.captureRuntimeProfiles(first, ProfilePointOpen, 1)
	if len(opened) != 1 {
		t.Fatalf("opening artifacts = %v, want one", opened)
	}
	h.captureCloseProfiles(RunFinish{RunID: first.RunID, Validity: "valid", AcceptedAt: time.Now()})
	h.captureRuntimeProfiles(
		RunStart{RunID: "run-second02", StartedAt: time.Now(), Validity: "valid"}, ProfilePointOpen, 2)

	base := strings.TrimSuffix(opened[0], "_open"+profileArtifactExt)
	if record := readCapture(t, dir, base+"_open"+profileSidecarExt); record.Orphan {
		t.Errorf("opening record = %+v, want a completed run left alone", record)
	}
	if manifest := h.profileManifest(first.RunID); manifest == nil || len(manifest.Pairs) != 1 {
		t.Errorf("manifest = %+v, want the pair intact", manifest)
	}
	h.abortRunProfiles(first.RunID)
	if record := readCapture(t, dir, base+"_close"+profileSidecarExt); record.Status != profileStatusOK {
		t.Errorf("closing record = %+v, want the published capture untouched", record)
	}
}
