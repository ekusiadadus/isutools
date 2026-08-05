package hoststats

import (
	"strings"
	"testing"
)

func TestHostStatsHealthKeys(t *testing.T) {
	t.Parallel()
	keys := HealthKeys()
	want := []string{
		"hoststats-source-skipped",
		"hoststats-cgroup-path-rejected",
		"hoststats-cgroup-v1",
		"hoststats-counter-rewind",
		"hoststats-host-changed",
	}
	if len(keys) != len(want) {
		t.Fatalf("HealthKeys() = %v, want exactly %d keys", keys, len(want))
	}
	for i, key := range want {
		if keys[i] != key {
			t.Fatalf("HealthKeys() = %v, want %v", keys, want)
		}
	}
	// The namespace is separate from runctl's on purpose: a key collision
	// would let one collector's degradation overwrite another's.
	for _, key := range keys {
		if !strings.HasPrefix(key, "hoststats-") {
			t.Fatalf("key %q is outside this package's namespace", key)
		}
	}
}

func TestHealthNotes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		base       func(*Sample)
		final      func(*Sample)
		codes      []string
		wantKeys   []string
		wantDetail string
	}{
		{name: "clean run has no notes"},
		{
			name:       "skipped sources are reported once",
			codes:      []string{CodeNotCapturedPrefix + SourcePSI, CodeNotCapturedPrefix + SourceCGroup},
			wantKeys:   []string{HealthSourceSkipped},
			wantDetail: SourcePSI,
		},
		{
			name:       "rejected cgroup path",
			final:      func(s *Sample) { s.CGroupSkip = cgroupSkipRejectPrefix + rejectEscapesMount },
			codes:      []string{CodeNotCapturedPrefix + SourceCGroup},
			wantKeys:   []string{HealthSourceSkipped, HealthCGroupPathRejected},
			wantDetail: rejectEscapesMount,
		},
		{
			name:     "cgroup v1 falls back to the baseline reason",
			base:     func(s *Sample) { s.CGroupSkip = cgroupSkipV1 },
			wantKeys: []string{HealthCGroupV1},
		},
		{
			name:       "counter rewind names the devices",
			codes:      []string{CodeCounterRewindPrefix + "sda", CodeCounterRewindPrefix + SourceVMStat},
			wantKeys:   []string{HealthCounterRewind},
			wantDetail: "sda",
		},
		{
			name:       "host change",
			codes:      []string{CodeBootIDChanged, CodeMachineIDChanged},
			wantKeys:   []string{HealthHostChanged},
			wantDetail: CodeBootIDChanged,
		},
		{
			name:     "a no-mount skip is not a rejected configuration",
			final:    func(s *Sample) { s.CGroupSkip = cgroupSkipNoMount },
			wantKeys: nil,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := &Sample{}
			final := &Sample{}
			if tt.base != nil {
				tt.base(base)
			}
			if tt.final != nil {
				tt.final(final)
			}
			notes := healthNotes(base, final, tt.codes)
			if len(notes) != len(tt.wantKeys) {
				t.Fatalf("notes = %+v, want keys %v", notes, tt.wantKeys)
			}
			for i, key := range tt.wantKeys {
				if notes[i].Key != key {
					t.Fatalf("notes = %+v, want %v in order", notes, tt.wantKeys)
				}
				if notes[i].Message == "" {
					t.Fatalf("note %q has no message", key)
				}
			}
			if tt.wantDetail == "" {
				return
			}
			joined := ""
			for _, note := range notes {
				joined += note.Message
			}
			if !strings.Contains(joined, tt.wantDetail) {
				t.Fatalf("messages = %q, want them to name %q", joined, tt.wantDetail)
			}
		})
	}
}

func TestSectionHealthNotesCopies(t *testing.T) {
	t.Parallel()
	var nilSection *Section
	if nilSection.HealthNotes() != nil {
		t.Fatal("HealthNotes() on nothing must be nothing")
	}

	section := &Section{health: []HealthNote{{Key: HealthCGroupV1, Message: "x"}}}
	notes := section.HealthNotes()
	notes[0].Key = "mutated"
	if section.HealthNotes()[0].Key != HealthCGroupV1 {
		t.Fatal("HealthNotes() must hand out a copy")
	}
	if (&Section{}).HealthNotes() != nil {
		t.Fatal("a clean section has no notes")
	}
}

func TestDisplayNotesAreFixed(t *testing.T) {
	t.Parallel()
	// These two strings exist to prevent a specific misreading, so their
	// wording is part of the contract rather than template decoration.
	if !strings.Contains(DiskUtilNote, "multi-queue") {
		t.Fatalf("DiskUtilNote = %q, want the multi-queue caveat", DiskUtilNote)
	}
	if !strings.Contains(CGroupScopeNote, "scope") {
		t.Fatalf("CGroupScopeNote = %q, want the scope caveat", CGroupScopeNote)
	}
}
