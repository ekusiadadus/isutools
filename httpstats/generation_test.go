package httpstats

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// drainCancelGrace mirrors runctl.DrainCancelGrace: Drain must return within
// this long of ctx being done.
const drainCancelGrace = time.Second

// blockingHandler serves requests that park until release is closed, which is
// how the tests hold a generation open across a boundary.
type blockingHandler struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingHandler() *blockingHandler {
	return &blockingHandler{started: make(chan struct{}), release: make(chan struct{})}
}

func (h *blockingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	close(h.started)
	<-h.release
	w.WriteHeader(http.StatusNoContent)
}

func isOpen(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
		return true
	}
}

func currentGeneration(c *Collector) *generation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func mustFreeze(t *testing.T, c *Collector, runID string, ep runctl.Epoch) runctl.BoundaryResult {
	t.Helper()
	res, err := c.Freeze(context.Background(), runID, ep)
	if err != nil {
		t.Fatalf("Freeze(%s, %d) = %v", runID, ep, err)
	}
	if !res.Committed {
		t.Fatalf("Freeze(%s, %d) reported success without committing: %#v", runID, ep, res)
	}
	return res
}

func mustCollect(t *testing.T, c *Collector, h runctl.GenerationHandle) Result {
	t.Helper()
	got, err := c.Collect(h)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	result, ok := got.(Result)
	if !ok {
		t.Fatalf("Collect returned %T, want httpstats.Result", got)
	}
	return result
}

func TestCollectorImplementsGenerationContract(t *testing.T) {
	var coll runctl.GenerationCollector = New()
	if coll.Name() != CollectorName {
		t.Fatalf("Name() = %q, want %q", coll.Name(), CollectorName)
	}
}

// TestDrainReturnsOnContextCancelWithRequestInFlight is the Drain conformance
// test. A sync.Cond wait cannot be interrupted by a context, so this test
// deadlocks the moment the mechanism regresses to one.
func TestDrainReturnsOnContextCancelWithRequestInFlight(t *testing.T) {
	c := New()
	blocked := newBlockingHandler()
	h := c.Middleware(blocked)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	}()
	<-blocked.started

	frozen := mustFreeze(t, c, "run-1", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := c.Drain(ctx, frozen.Handle)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain with a cancelled context = %v, want context.Canceled", err)
	}
	if elapsed > drainCancelGrace {
		t.Fatalf("Drain took %v, want at most %v after cancel", elapsed, drainCancelGrace)
	}

	// The late request must land in its own sealed generation and nowhere else.
	close(blocked.release)
	<-requestDone
	if err := c.Drain(context.Background(), frozen.Handle); err != nil {
		t.Fatalf("Drain after the request finished = %v", err)
	}
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("late request leaked into the live generation: %#v", got)
	}
	result := mustCollect(t, c, frozen.Handle)
	if len(result.HTTP) != 1 || result.HTTP[0].Path != "/slow" {
		t.Fatalf("drained generation = %#v, want the late request", result.HTTP)
	}
}

func TestDrainReturnsNilWhenLastRequestFinishes(t *testing.T) {
	c := New()
	blocked := newBlockingHandler()
	h := c.Middleware(blocked)
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	<-blocked.started

	frozen := mustFreeze(t, c, "run-1", 1)
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- c.Drain(ctx, frozen.Handle)
	}()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned %v while a request was still in flight", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(blocked.release)
	if err := <-drained; err != nil {
		t.Fatalf("Drain = %v, want nil once the last request finished", err)
	}
	if err := c.Drain(context.Background(), frozen.Handle); err != nil {
		t.Fatalf("second Drain = %v, want nil", err)
	}
}

// TestLiveGenerationIsNeverClosed pins the rule that makes Drain trustworthy:
// only a sealed generation may close its done channel. A live generation
// reaches zero in-flight between requests constantly, and closing it there
// would either panic on a double close or make a later Drain return while the
// generation was still being written to.
func TestLiveGenerationIsNeverClosed(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	live := currentGeneration(c)
	for i := 0; i < 50; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
		if !isOpen(live.done) {
			t.Fatalf("live generation was settled after %d requests", i+1)
		}
		c.mu.Lock()
		inFlight, sealed := live.inFlight, live.sealed
		c.mu.Unlock()
		if inFlight != 0 || sealed {
			t.Fatalf("after request %d: inFlight = %d, sealed = %v; want 0, false", i+1, inFlight, sealed)
		}
	}

	// Once sealed, Drain of that same generation must still wait for work that
	// was pinned to it before the boundary.
	blocked := newBlockingHandler()
	blocking := c.Middleware(blocked)
	go blocking.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	<-blocked.started
	if currentGeneration(c) != live {
		t.Fatal("traffic moved the collector off the generation under test")
	}

	frozen := mustFreeze(t, c, "run-1", 1)
	if frozen.Handle.Token() != live {
		t.Fatal("Freeze returned a handle to a different generation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Drain(ctx, frozen.Handle); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain of a sealed generation with work in flight = %v, want DeadlineExceeded", err)
	}

	close(blocked.release)
	if err := c.Drain(context.Background(), frozen.Handle); err != nil {
		t.Fatalf("Drain after release = %v", err)
	}
	result := mustCollect(t, c, frozen.Handle)
	if len(result.HTTP) != 2 {
		t.Fatalf("drained generation = %#v, want the fast and the slow identity", result.HTTP)
	}
}

func TestBeginBoundaryReturnsPreviousGenerationAndSwapTime(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/before", nil))

	before := time.Now()
	previous := currentGeneration(c)
	res, err := c.BeginBoundary(context.Background(), "run-1", 1)
	after := time.Now()
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if !res.Committed {
		t.Fatalf("BeginBoundary reported success without committing: %#v", res)
	}
	if res.Handle.Token() != previous {
		t.Fatal("BeginBoundary did not return the generation it closed")
	}
	if res.At.Before(before) || res.At.After(after) {
		t.Fatalf("At = %v, want a real swap time in [%v, %v]", res.At, before, after)
	}
	if currentGeneration(c) == previous {
		t.Fatal("BeginBoundary did not swap in a fresh generation")
	}
	if err := c.Drain(context.Background(), res.Handle); err != nil {
		t.Fatalf("Drain of an idle generation = %v", err)
	}
	result := mustCollect(t, c, res.Handle)
	if len(result.HTTP) != 1 || result.HTTP[0].Path != "/before" {
		t.Fatalf("closed generation = %#v, want the pre-boundary request", result.HTTP)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/after", nil))
	again := mustCollect(t, c, res.Handle)
	if len(again.HTTP) != 1 || again.HTTP[0].Path != "/before" {
		t.Fatalf("post-boundary traffic changed a closed generation: %#v", again.HTTP)
	}
}

func TestBoundariesAreIdempotentPerRunAndEpoch(t *testing.T) {
	c := New()
	first, err := c.BeginBoundary(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	live := currentGeneration(c)
	second, err := c.BeginBoundary(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("retried BeginBoundary: %v", err)
	}
	if second != first {
		t.Fatalf("retry = %#v, want the first result %#v", second, first)
	}
	if !second.At.Equal(first.At) {
		t.Fatalf("retry At = %v, want %v", second.At, first.At)
	}
	if currentGeneration(c) != live {
		t.Fatal("retried BeginBoundary swapped a second time")
	}

	frozenFirst := mustFreeze(t, c, "run-1", 1)
	if frozenFirst.Handle.Token() == first.Handle.Token() {
		t.Fatal("Freeze reused the handle BeginBoundary closed")
	}
	frozenAgain := mustFreeze(t, c, "run-1", 1)
	if frozenAgain != frozenFirst {
		t.Fatalf("retried Freeze = %#v, want %#v", frozenAgain, frozenFirst)
	}
}

func TestBoundaryRejectsStaleEpochWithoutSwapping(t *testing.T) {
	c := New()
	if _, err := c.BeginBoundary(context.Background(), "run-2", 2); err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	live := currentGeneration(c)

	res, err := c.BeginBoundary(context.Background(), "run-1", 1)
	if !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("stale BeginBoundary = %v, want ErrStaleEpoch", err)
	}
	if res.Committed {
		t.Fatalf("stale BeginBoundary claimed a commit: %#v", res)
	}
	if res.At.IsZero() {
		t.Fatal("boundary result must carry a time even on error")
	}
	if _, err := c.Freeze(context.Background(), "run-1", 1); !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("stale Freeze = %v, want ErrStaleEpoch", err)
	}
	if currentGeneration(c) != live {
		t.Fatal("a stale boundary swapped the generation")
	}
}

func TestBoundaryOnDoneContextDoesNotSwap(t *testing.T) {
	c := New()
	live := currentGeneration(c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BeginBoundary with a cancelled context = %v, want context.Canceled", err)
	}
	if res.Committed {
		t.Fatalf("failed boundary claimed a commit: %#v", res)
	}
	if currentGeneration(c) != live {
		t.Fatal("a failed boundary must leave the process on the old generation")
	}
	if got, err := c.BeginBoundary(context.Background(), "run-1", 1); err != nil || !got.Committed {
		t.Fatalf("retry after a cancelled boundary = (%#v, %v), want a committed swap", got, err)
	}
}

func TestFreezeMovesLaterTrafficToTheNextGeneration(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/in-run", nil))
	frozen := mustFreeze(t, c, "run-1", 1)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/after-run", nil))

	if err := c.Drain(context.Background(), frozen.Handle); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	result := mustCollect(t, c, frozen.Handle)
	if len(result.HTTP) != 1 || result.HTTP[0].Path != "/in-run" {
		t.Fatalf("frozen generation = %#v, want only the in-run request", result.HTTP)
	}
	live := c.Snapshot()
	if len(live) != 1 || live[0].Path != "/after-run" {
		t.Fatalf("live generation = %#v, want only the post-freeze request", live)
	}
}

func TestCollectCarriesConnectionTotalsOfItsOwnWindow(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/events", nil))

	frozen := mustFreeze(t, c, "run-1", 1)
	if err := c.Drain(context.Background(), frozen.Handle); err != nil {
		t.Fatalf("Drain of a released SSE connection = %v", err)
	}
	result := mustCollect(t, c, frozen.Handle)
	if result.Connections.Total != 1 {
		t.Fatalf("frozen connections = %+v, want total 1", result.Connections)
	}
	if got := c.Connections(); got.Total != 0 {
		t.Fatalf("live connections = %+v, want the counters cleared with the generation", got)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	frozen := mustFreeze(t, c, "run-1", 1)
	if err := c.Drain(context.Background(), frozen.Handle); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	c.Release(frozen.Handle)
	c.Release(frozen.Handle)
	c.Release(runctl.GenerationHandle{})
	c.Release(runctl.NewGenerationHandle("run-1", 1, CollectorName, 0, "not a generation"))

	if _, err := c.Collect(frozen.Handle); !errors.Is(err, errReleasedHandle) {
		t.Fatalf("Collect after Release = %v, want errReleasedHandle", err)
	}
	// A released generation must still absorb a late write instead of panicking.
	g, err := c.generationOf(frozen.Handle)
	if err != nil {
		t.Fatalf("generationOf: %v", err)
	}
	g.table.observe(identity{method: "GET", path: "/late", protocol: "HTTP/1.1", status: 200}, time.Millisecond, 1)
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("a late write reached the live generation: %#v", got)
	}
}

func TestForeignAndZeroHandlesAreRejectedNotPanicked(t *testing.T) {
	first, second := New(), New()
	frozen := mustFreeze(t, first, "run-1", 1)

	if err := second.Drain(context.Background(), frozen.Handle); !errors.Is(err, errForeignHandle) {
		t.Fatalf("Drain of another collector's handle = %v, want errForeignHandle", err)
	}
	if _, err := second.Collect(frozen.Handle); !errors.Is(err, errForeignHandle) {
		t.Fatalf("Collect of another collector's handle = %v, want errForeignHandle", err)
	}
	second.Release(frozen.Handle)
	if _, err := second.Collect(frozen.Handle); !errors.Is(err, errForeignHandle) {
		t.Fatalf("a foreign Release freed the owner's generation: %v", err)
	}
	if err := first.Drain(context.Background(), runctl.GenerationHandle{}); !errors.Is(err, errForeignHandle) {
		t.Fatalf("Drain of a zero handle = %v, want errForeignHandle", err)
	}
	if _, err := first.Collect(runctl.GenerationHandle{}); !errors.Is(err, errForeignHandle) {
		t.Fatalf("Collect of a zero handle = %v, want errForeignHandle", err)
	}
}

// TestNilContextIsTolerated covers the caller mistake a measurement library
// must survive: a nil context degrades to "no deadline", never to a panic.
func TestNilContextIsTolerated(t *testing.T) {
	c := New()
	res, err := c.BeginBoundary(nil, "run-1", 1) //nolint:staticcheck // SA1012
	if err != nil || !res.Committed {
		t.Fatalf("BeginBoundary(nil ctx) = (%#v, %v), want a committed swap", res, err)
	}
	frozen := mustFreeze(t, c, "run-1", 1)
	if err := c.Drain(nil, frozen.Handle); err != nil { //nolint:staticcheck // SA1012
		t.Fatalf("Drain(nil ctx) = %v, want nil for a settled generation", err)
	}
}

func TestDrainWithNilContextWaitsForInFlight(t *testing.T) {
	c := New()
	blocked := newBlockingHandler()
	h := c.Middleware(blocked)
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	<-blocked.started
	frozen := mustFreeze(t, c, "run-1", 1)

	drained := make(chan error, 1)
	go func() { drained <- c.Drain(nil, frozen.Handle) }() //nolint:staticcheck // SA1012
	select {
	case err := <-drained:
		t.Fatalf("Drain(nil ctx) returned %v while a request was still in flight", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocked.release)
	if err := <-drained; err != nil {
		t.Fatalf("Drain(nil ctx) = %v, want nil once the request finished", err)
	}
}

// TestDrainDoesNotWaitForAConnectionConfirmedAfterFreeze covers the release
// path closing a sealed generation: a request that turns into a long-lived
// connection after the boundary leaves its generation, so Drain settles
// instead of waiting for a stream that may never end.
func TestDrainDoesNotWaitForAConnectionConfirmedAfterFreeze(t *testing.T) {
	c := New()
	entered := make(chan struct{})
	proceed := make(chan struct{})
	hold := make(chan struct{})
	handlerDone := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-proceed
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-hold
	}))
	go func() {
		defer close(handlerDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/events", nil))
	}()
	<-entered

	frozen := mustFreeze(t, c, "run-1", 1)
	close(proceed)
	ctx, cancel := context.WithTimeout(context.Background(), drainCancelGrace)
	defer cancel()
	if err := c.Drain(ctx, frozen.Handle); err != nil {
		t.Fatalf("Drain = %v, want nil: the open stream left the frozen generation", err)
	}
	result := mustCollect(t, c, frozen.Handle)
	if len(result.HTTP) != 0 {
		t.Fatalf("frozen latency table = %#v, want the stream excluded", result.HTTP)
	}

	close(hold)
	<-handlerDone
}

// TestWriterStaysTransparentAcrossABoundary keeps the middleware's writer
// contract fixed while a boundary swaps underneath an in-flight request.
func TestWriterStaysTransparentAcrossABoundary(t *testing.T) {
	c := New()
	underlying := &allFeaturesWriter{plainWriter: plainWriter{header: make(http.Header)}}
	boundaryTaken := make(chan runctl.BoundaryResult, 1)
	inHandler := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inHandler)
		<-boundaryTaken
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapped writer lost http.Flusher")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("wrapped writer lost http.Hijacker")
		}
		rf, ok := w.(io.ReaderFrom)
		if !ok {
			t.Fatal("wrapped writer lost io.ReaderFrom")
		}
		if uw, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok || uw.Unwrap() != underlying {
			t.Errorf("Unwrap() = (%v, %v), want the underlying writer", uw, ok)
		}
		w.(http.Flusher).Flush()
		_, _ = rf.ReadFrom(readerOnly{Reader: strings.NewReader("stream")})
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(underlying, httptest.NewRequest(http.MethodGet, "/stream", nil))
	}()
	<-inHandler
	frozen := mustFreeze(t, c, "run-1", 1)
	boundaryTaken <- frozen
	<-done

	if err := c.Drain(context.Background(), frozen.Handle); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	result := mustCollect(t, c, frozen.Handle)
	if len(result.HTTP) != 1 || result.HTTP[0].TotalBytes != int64(len("stream")) {
		t.Fatalf("frozen generation = %#v, want the ReaderFrom bytes", result.HTTP)
	}
	if !underlying.flushed {
		t.Error("Flush was not delegated across the boundary")
	}
}

// TestResetIsBoundedWhenARequestNeverReturns pins the bound on the compat
// shim. /reset calls Reset while it holds the process-wide reset lock and the
// operation slot, so an unbounded wait would let a single request that never
// returns wedge /reset and, through the same lock, /finish, /collect and
// /save. One wedged request may cost a budget; it may not cost the endpoint.
func TestResetIsBoundedWhenARequestNeverReturns(t *testing.T) {
	const budget = 50 * time.Millisecond
	// A small multiple of the injected bound: generous enough for a loaded
	// race-detector run, far short of the unbounded wait this test exists for.
	const limit = 20 * budget

	c := New()
	c.SetResetDrainBudget(budget)
	wedged := newBlockingHandler()
	wedgedDone := make(chan struct{})
	go func() {
		defer close(wedgedDone)
		c.Middleware(wedged).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/wedged", nil))
	}()
	<-wedged.started
	// Released from a cleanup so the goroutine is joined even when an
	// assertion below aborts the test.
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(wedged.release)
			<-wedgedDone
		})
	}
	t.Cleanup(release)

	// One request that does return, so the cut-short snapshot has to carry
	// real data rather than merely being non-nil.
	fast := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	fast.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/fast", nil))

	returned := make(chan Snapshot, 1)
	start := time.Now()
	go func() { returned <- c.Reset() }()
	var snap Snapshot
	select {
	case snap = <-returned:
	case <-time.After(limit):
		t.Fatalf("Reset did not return within %v of a %v budget: /reset is wedged by one request", limit, budget)
	}
	if elapsed := time.Since(start); elapsed > limit {
		t.Fatalf("Reset took %v, want at most %v for a %v budget", elapsed, limit, budget)
	}
	if len(snap) != 1 || snap[0].Path != "/fast" {
		t.Fatalf("cut-short snapshot = %#v, want the request that did finish", snap)
	}
	if got := c.ResetsCutShort(); got != 1 {
		t.Fatalf("ResetsCutShort() = %d, want 1 so the caller can mark the section partial", got)
	}

	// The request the budget gave up on still lands in its own sealed
	// generation, never in the live one the caller just started measuring.
	release()
	if live := c.Snapshot(); len(live) != 0 {
		t.Fatalf("the wedged request leaked into the live generation: %#v", live)
	}
}

// TestResetWaitsForASettledGenerationWithoutCountingACut keeps the bound from
// degenerating into "always give up": the ordinary path still waits for the
// requests it closed and reports no cut.
func TestResetWaitsForASettledGenerationWithoutCountingACut(t *testing.T) {
	c := New()
	c.SetResetDrainBudget(5 * time.Second)
	blocked := newBlockingHandler()
	served := make(chan struct{})
	go func() {
		defer close(served)
		c.Middleware(blocked).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	}()
	<-blocked.started

	returned := make(chan Snapshot, 1)
	go func() { returned <- c.Reset() }()
	select {
	case snap := <-returned:
		t.Fatalf("Reset returned %#v while a request was still in flight", snap)
	case <-time.After(20 * time.Millisecond):
	}

	close(blocked.release)
	<-served
	snap := <-returned
	if len(snap) != 1 || snap[0].Path != "/slow" {
		t.Fatalf("Reset = %#v, want the request it waited for", snap)
	}
	if got := c.ResetsCutShort(); got != 0 {
		t.Fatalf("ResetsCutShort() = %d after a complete wait, want 0", got)
	}
}

// TestSetResetDrainBudgetRestoresTheDefault pins the documented meaning of a
// non-positive budget: fall back to ResetDrainBudget rather than to zero, so a
// caller cannot accidentally turn the wait off entirely.
func TestSetResetDrainBudgetRestoresTheDefault(t *testing.T) {
	c := New()
	c.SetResetDrainBudget(time.Millisecond)
	c.SetResetDrainBudget(0)
	c.mu.Lock()
	got := c.gens.resetBudget
	c.mu.Unlock()
	if got != 0 {
		t.Fatalf("resetBudget = %v, want 0 to mean the default", got)
	}

	blocked := newBlockingHandler()
	served := make(chan struct{})
	go func() {
		defer close(served)
		c.Middleware(blocked).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	}()
	<-blocked.started
	returned := make(chan Snapshot, 1)
	go func() { returned <- c.Reset() }()
	select {
	case snap := <-returned:
		t.Fatalf("Reset returned %#v immediately, so the zero budget was used verbatim", snap)
	case <-time.After(20 * time.Millisecond):
	}
	close(blocked.release)
	<-served
	if snap := <-returned; len(snap) != 1 {
		t.Fatalf("Reset = %#v, want the request it waited for", snap)
	}
}

// TestBoundariesUnderConcurrentTraffic drives boundaries against live traffic
// so the race detector sees the settle condition evaluated from both the swap
// side and the last-request side, and checks the accounting invariant that
// makes generations worth having: every served request lands in exactly one
// generation, never zero and never two.
func TestBoundariesUnderConcurrentTraffic(t *testing.T) {
	const workers = 4
	c := New()
	var served atomic.Int64
	var flowing sync.Once
	traffic := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		flowing.Do(func() { close(traffic) })
		w.WriteHeader(http.StatusOK)
	}))
	stop := make(chan struct{})
	stopped := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer func() { stopped <- struct{}{} }()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
			}
		}()
	}
	// The boundaries must run against traffic, not against goroutines the
	// scheduler has not started yet: on a single P the loop below can otherwise
	// finish before any worker's first request.
	<-traffic

	drainAndCount := func(res runctl.BoundaryResult) int64 {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Drain(ctx, res.Handle); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		var count int64
		for _, entry := range mustCollect(t, c, res.Handle).HTTP {
			count += entry.Count
		}
		c.Release(res.Handle)
		return count
	}

	var total int64
	for epoch := runctl.Epoch(1); epoch <= 20; epoch++ {
		res, err := c.BeginBoundary(context.Background(), "run", epoch)
		if err != nil {
			t.Fatalf("BeginBoundary(%d): %v", epoch, err)
		}
		total += drainAndCount(res)
	}

	close(stop)
	for i := 0; i < workers; i++ {
		<-stopped
	}
	// Every worker has returned, so every request it served is recorded.
	total += drainAndCount(mustFreeze(t, c, "run", 21))

	if want := served.Load(); total != want {
		t.Fatalf("requests attributed to generations = %d, want the %d served", total, want)
	}
	if total == 0 {
		t.Fatal("the traffic goroutines served nothing")
	}
}
