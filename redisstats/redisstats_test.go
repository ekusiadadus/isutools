package redisstats

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestObserveRedactsArgumentsAndCountsErrors(t *testing.T) {
	r := NewRegistry(10)
	r.Observe("get private:user:42", 2*time.Millisecond, nil)
	r.Observe("GET another-secret", 4*time.Millisecond, errors.New("redis down"))
	entries := r.Snapshot()
	if len(entries) != 1 || entries[0].Command != "GET" || entries[0].Count != 2 || entries[0].ErrorCount != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	if strings.Contains(strings.Join([]string{entries[0].Command}, ""), "secret") {
		t.Fatal("Redis key leaked into snapshot")
	}
}

func TestBoundsAndReset(t *testing.T) {
	r := NewRegistry(1)
	r.Observe("GET", time.Millisecond, nil)
	r.Observe("SET", time.Millisecond, nil)
	r.Observe("bad/command", time.Millisecond, nil)
	if r.Dropped() != 2 {
		t.Fatalf("dropped = %d", r.Dropped())
	}
	r.Reset()
	if len(r.Snapshot()) != 0 || r.Dropped() != 0 {
		t.Fatal("reset did not clear generation")
	}
}
