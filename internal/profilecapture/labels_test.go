package profilecapture

import (
	"context"
	"fmt"
	"regexp"
	"runtime/pprof"
	"testing"
	"time"
)

func TestLabelScopeUsesOpaquePrivateLabelsAndCanonicalDictionary(t *testing.T) {
	scope := newLabelScope("0123456789abcdef0123456789abcdef")
	logical := SafeLabelTuple{Method: "GET", Route: "/users/{id}", Scenario: "browse", Region: "tokyo"}
	called := false
	ok := scope.Do(context.Background(), logical, func(ctx context.Context) {
		called = true
		capture, captureOK := pprof.Label(ctx, PrivateCaptureLabel)
		tuple, tupleOK := pprof.Label(ctx, PrivateTupleLabel)
		if !captureOK || capture != "0123456789abcdef0123456789abcdef" {
			t.Fatalf("capture label = %q, %v", capture, captureOK)
		}
		if !tupleOK || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(tuple) {
			t.Fatalf("tuple label = %q, %v", tuple, tupleOK)
		}
		if _, ok := pprof.Label(ctx, "http.route"); ok {
			t.Fatal("logical route leaked into the physical pprof label set")
		}
		fromContext, ok := LogicalLabels(ctx)
		if !ok || fromContext != logical {
			t.Fatalf("logical context = %#v, %v", fromContext, ok)
		}
	})
	if !ok || !called {
		t.Fatal("scope did not execute labeled callback")
	}

	scope.Seal()
	dictionary := scope.Dictionary("run-1", 9)
	if !dictionary.Sealed || len(dictionary.Tuples) != 1 || dictionary.Tuples[0].Method != "GET" || dictionary.SHA256 == "" {
		t.Fatalf("dictionary = %#v", dictionary)
	}
	if dictionary.CaptureID != "0123456789abcdef0123456789abcdef" || dictionary.RunID != "run-1" || dictionary.Epoch != 9 {
		t.Fatalf("dictionary binding = %#v", dictionary)
	}
}

func TestLabelScopeCapsCaptureLifetimeAndSeals(t *testing.T) {
	scope := newLabelScope("0123456789abcdef0123456789abcdef")
	for i := 0; i < MaxConcreteLabelTuples; i++ {
		logical := SafeLabelTuple{Method: "GET", Route: fmt.Sprintf("/route/%03d", i)}
		if !scope.Do(context.Background(), logical, func(context.Context) {}) {
			t.Fatalf("tuple %d was not labeled", i)
		}
	}
	for i := 0; i < 10; i++ {
		logical := SafeLabelTuple{Method: "GET", Route: fmt.Sprintf("/overflow/%03d", i)}
		if !scope.Do(context.Background(), logical, func(context.Context) {}) {
			t.Fatalf("overflow tuple %d was not labeled", i)
		}
	}
	dictionary := scope.Dictionary("run", 1)
	if len(dictionary.Tuples) != MaxLabelTuples {
		t.Fatalf("dictionary tuples = %d, want %d", len(dictionary.Tuples), MaxLabelTuples)
	}
	overflow := 0
	for _, tuple := range dictionary.Tuples {
		if tuple.Overflow {
			overflow++
		}
	}
	if overflow != 1 {
		t.Fatalf("overflow entries = %d, want 1", overflow)
	}

	scope.Seal()
	called := false
	if scope.Do(context.Background(), SafeLabelTuple{Method: "POST", Route: "/late"}, func(context.Context) { called = true }) || !called {
		t.Fatal("sealed scope must run callback without adding pprof labels")
	}
	if got := len(scope.Dictionary("run", 1).Tuples); got != MaxLabelTuples {
		t.Fatalf("sealed dictionary grew to %d", got)
	}
}

func TestCoordinatorExposesLabelsOnlyWhileCapturingAndSealsOnStop(t *testing.T) {
	backend := &fakeBackend{stopWait: make(chan struct{})}
	factory := &fakeFactory{}
	coordinator := newTestCoordinator(t, backend, factory, nil)
	request := validStartRequest()
	request.RunID = "run"
	start := coordinator.StartRun(context.Background(), request)
	if start.State != StateCapturing {
		t.Fatalf("start = %#v", start)
	}
	scope := coordinator.ActiveLabelScope()
	if scope == nil || scope.CaptureID() != start.CaptureID {
		t.Fatalf("active scope = %#v", scope)
	}

	ticket := coordinator.RequestStop(StopRequest{
		RunID: "run", Epoch: 1, State: "finished", Validity: "valid", Reason: "finish", BoundaryAt: time.Now(),
	})
	if coordinator.ActiveLabelScope() != nil {
		t.Fatal("stopping capture remained available for new labels")
	}
	if scope.Do(context.Background(), SafeLabelTuple{Method: "GET", Route: "/late"}, func(context.Context) {}) {
		t.Fatal("stop did not seal previously acquired label scope")
	}
	close(backend.stopWait)
	status := coordinator.Await(ticket, context.Background())
	if status.State != StatePublished {
		t.Fatalf("status = %#v", status)
	}
	dictionary, ok := coordinator.LabelDictionary("run", 1)
	if !ok || !dictionary.Sealed || dictionary.CaptureID != start.CaptureID {
		t.Fatalf("dictionary = %#v, %v", dictionary, ok)
	}
}
