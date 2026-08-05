package dbpool

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/sqlstats"
)

// TestWatchRejectsUnregisteredID pins the acceptance rule: only IDs that are
// already in the registry are watched, compared byte for byte. The first case
// is the exact string an earlier revision of the design put in its example —
// it is the alias half of an auto-derived ID with the hash suffix missing, so
// it can never resolve, and watching it would have measured nothing while
// looking correct.
func TestWatchRejectsUnregisteredID(t *testing.T) {
	registerTarget(t, "db1", appDSN)
	db := newFakeDB(t)

	tests := []struct {
		name     string
		targetID string
	}{
		{name: "alias without hash suffix", targetID: "mysql-db1_3306-isuconp"},
		{name: "alias of the registered target without hash", targetID: fakeDriverName + "-127.0.0.1_3306-isuconp"},
		{name: "uppercased id", targetID: "DB1"},
		{name: "leading space", targetID: " db1"},
		{name: "trailing space", targetID: "db1 "},
		{name: "empty", targetID: ""},
		{name: "never registered", targetID: "nosuchtarget"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			err := c.Watch(tc.targetID, db)
			if !errors.Is(err, sqlstats.ErrUnknownTarget) {
				t.Fatalf("Watch(%q) = %v, want ErrUnknownTarget", tc.targetID, err)
			}
			if watched := c.Watched(); len(watched) != 0 {
				t.Fatalf("watch set = %v, want empty", watched)
			}
		})
	}
}

// TestWatchAcceptsRegisteredID checks the recommended integration: name the
// target, then hand the same id to Watch.
func TestWatchAcceptsRegisteredID(t *testing.T) {
	registerTarget(t, "db1", appDSN)
	c := New()

	if err := c.Watch("db1", newFakeDB(t)); err != nil {
		t.Fatalf("Watch(\"db1\") = %v, want nil", err)
	}
	if got := c.Watched(); len(got) != 1 || got[0] != "db1" {
		t.Fatalf("watch set = %v, want [db1]", got)
	}

	entries := collectEntries(t, c, "run-1", 1)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly one", entries)
	}
	if entries[0].TargetID != "db1" {
		t.Fatalf("TargetID = %q, want %q", entries[0].TargetID, "db1")
	}
	if entries[0].Display != appDisplay {
		t.Fatalf("Display = %q, want %q", entries[0].Display, appDisplay)
	}
}

// TestWatchAcceptsTargetIDForDSN covers the lookup form of the integration:
// the id the registry hands back is always acceptable, which is what callers
// who never named a target explicitly have to rely on.
func TestWatchAcceptsTargetIDForDSN(t *testing.T) {
	registerTarget(t, "db1", appDSN)

	id, ok := sqlstats.TargetIDForDSN(fakeDriverName, appDSN)
	if !ok {
		t.Fatalf("TargetIDForDSN = _, false; want the registered id")
	}
	c := New()
	if err := c.Watch(id, newFakeDB(t)); err != nil {
		t.Fatalf("Watch(%q) = %v, want nil", id, err)
	}
}

func TestWatchNilDB(t *testing.T) {
	registerTarget(t, "db1", appDSN)
	c := New()

	err := c.Watch("db1", nil)
	if !errors.Is(err, ErrNilDB) {
		t.Fatalf("Watch(\"db1\", nil) = %v, want ErrNilDB", err)
	}
	if watched := c.Watched(); len(watched) != 0 {
		t.Fatalf("watch set = %v, want empty", watched)
	}
	// A nil handle is an argument bug, so it is reported even when the id
	// itself would not have resolved either.
	if err := c.Watch("nosuchtarget", nil); !errors.Is(err, ErrNilDB) {
		t.Fatalf("Watch(unknown, nil) = %v, want ErrNilDB", err)
	}
}

func TestWatchDuplicate(t *testing.T) {
	registerTarget(t, "db1", appDSN)
	c := New()
	db := newFakeDB(t)

	if err := c.Watch("db1", db); err != nil {
		t.Fatalf("first Watch = %v, want nil", err)
	}
	err := c.Watch("db1", newFakeDB(t))
	if !errors.Is(err, ErrDuplicatePool) {
		t.Fatalf("second Watch = %v, want ErrDuplicatePool", err)
	}
	if got := c.Watched(); len(got) != 1 {
		t.Fatalf("watch set = %v, want one entry", got)
	}
	// Unwatch then Watch is the supported way to swap a pool.
	if err := c.Unwatch("db1"); err != nil {
		t.Fatalf("Unwatch = %v, want nil", err)
	}
	if err := c.Watch("db1", db); err != nil {
		t.Fatalf("re-Watch = %v, want nil", err)
	}
}

func TestWatchTooManyPools(t *testing.T) {
	c := New()
	// The registry caps targets at the same number, so the cap is exercised
	// through the registry-free hook rather than by registering 17 databases.
	for i := 0; i < MaxPools; i++ {
		id := fmt.Sprintf("pool-%02d", i)
		if err := c.watchStats(id, "display", newScript().stats); err != nil {
			t.Fatalf("watchStats(%q) = %v, want nil", id, err)
		}
	}
	err := c.watchStats("pool-overflow", "display", newScript().stats)
	if !errors.Is(err, ErrTooManyPools) {
		t.Fatalf("watchStats beyond the cap = %v, want ErrTooManyPools", err)
	}
	if got := len(c.Watched()); got != MaxPools {
		t.Fatalf("watch set size = %d, want %d", got, MaxPools)
	}
}

func TestWatchStatsRejectsNilSampler(t *testing.T) {
	c := New()
	if err := c.watchStats("db1", "display", nil); !errors.Is(err, ErrNilDB) {
		t.Fatalf("watchStats(nil sampler) = %v, want ErrNilDB", err)
	}
}

func TestUnwatchIdempotent(t *testing.T) {
	registerTarget(t, "db1", appDSN)
	c := New()

	// Registered but never watched: undoing nothing is not an error.
	if err := c.Unwatch("db1"); err != nil {
		t.Fatalf("Unwatch of an unwatched target = %v, want nil", err)
	}
	if err := c.Watch("db1", newFakeDB(t)); err != nil {
		t.Fatalf("Watch = %v, want nil", err)
	}
	if err := c.Unwatch("db1"); err != nil {
		t.Fatalf("Unwatch = %v, want nil", err)
	}
	if err := c.Unwatch("db1"); err != nil {
		t.Fatalf("second Unwatch = %v, want nil", err)
	}
	if got := c.Watched(); len(got) != 0 {
		t.Fatalf("watch set = %v, want empty", got)
	}
	// An unregistered id is still an error: the caller is naming something
	// that does not exist rather than undoing something it did.
	if err := c.Unwatch("nosuchtarget"); !errors.Is(err, sqlstats.ErrUnknownTarget) {
		t.Fatalf("Unwatch(unknown) = %v, want ErrUnknownTarget", err)
	}
}

func TestWatchedIsSorted(t *testing.T) {
	c := New()
	for _, id := range []string{"zeta", "alpha", "mu"} {
		if err := c.watchStats(id, "display", newScript().stats); err != nil {
			t.Fatalf("watchStats(%q) = %v, want nil", id, err)
		}
	}
	got := c.Watched()
	want := []string{"alpha", "mu", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Watched() = %v, want %v", got, want)
	}
}

func TestCollectorName(t *testing.T) {
	if got := New().Name(); got != Name {
		t.Fatalf("Name() = %q, want %q", got, Name)
	}
	if Default == nil {
		t.Fatal("Default is nil")
	}
}

// TestWatchRealDBStats checks that a real *sql.DB flows through the boundary
// path, so the exported API is not only exercised through the test hook.
func TestWatchRealDBStats(t *testing.T) {
	registerTarget(t, "db1", appDSN)
	db := newFakeDB(t)
	db.SetMaxOpenConns(8)

	c := New()
	if err := c.Watch("db1", db); err != nil {
		t.Fatalf("Watch = %v, want nil", err)
	}
	entries := collectEntries(t, c, "run-1", 1)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one", entries)
	}
	if entries[0].MaxOpen != 8 {
		t.Fatalf("MaxOpen = %d, want 8", entries[0].MaxOpen)
	}
	if entries[0].Partial {
		t.Fatalf("Partial = true, want false for a quiet pool: %+v", entries[0])
	}
	_ = db.Stats()
}
