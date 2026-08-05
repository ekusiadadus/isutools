package sqlrows

import (
	"fmt"
	"testing"
)

// The digest table holds up to performance_schema_digests_size rows (10000 by
// default) and the registry allows up to sqlstats.MaxTargets targets, so these
// benchmarks measure the worst case a run can produce rather than a typical
// one. Run them with -benchmem; the numbers belong in the docs, because the
// whole point of a measurement tool is that its own footprint is known.
const (
	benchDigests = 10000
	benchTargets = 16
)

// benchSample builds one boundary's sample at full size.
func benchSample(offset uint64, withTexts bool) *Sample {
	sample := &Sample{Targets: make(map[string]*TargetSample, benchTargets)}
	for target := 0; target < benchTargets; target++ {
		digests := make(map[string]DigestRow, benchDigests)
		var texts map[string]string
		if withTexts {
			texts = make(map[string]string, DigestTextFetchLimit)
		}
		for i := 0; i < benchDigests; i++ {
			digest := fmt.Sprintf("%064x", i)
			digests[digest] = DigestRow{
				CountStar:    uint64(i) + offset,
				TimerWait:    uint64(i)*1000 + offset,
				RowsExamined: uint64(i) * 10,
				RowsSent:     uint64(i),
			}
			if withTexts && i < DigestTextFetchLimit {
				texts[digest] = "SELECT * FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT ?"
			}
		}
		id := fmt.Sprintf("db%02d", target)
		ts := capturedTargetForBench(id, digests)
		ts.Texts = texts
		sample.Targets[id] = ts
	}
	return sample
}

func capturedTargetForBench(id string, digests map[string]DigestRow) *TargetSample {
	return &TargetSample{
		TargetID:   id,
		Schema:     "isuconp",
		ServerUUID: "uuid-1",
		UptimeSec:  1000,
		UTCBefore:  baseTime,
		UTCAfter:   baseTime,
		Digests:    digests,
		Captured:   true,
	}
}

// BenchmarkSampleAlloc measures holding one boundary's readings.
func BenchmarkSampleAlloc(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if sample := benchSample(0, false); len(sample.Targets) != benchTargets {
			b.Fatal("unexpected sample size")
		}
	}
}

// BenchmarkSampleAllocWithTexts measures the closing boundary, which also
// carries up to DigestTextFetchLimit digest texts per target.
func BenchmarkSampleAllocWithTexts(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if sample := benchSample(1, true); len(sample.Targets) != benchTargets {
			b.Fatal("unexpected sample size")
		}
	}
}

// BenchmarkCollectAlloc measures deriving the interval from two full samples.
func BenchmarkCollectAlloc(b *testing.B) {
	base := benchSample(0, false)
	final := benchSample(1, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		section := buildSection(base, final)
		if len(section.Targets) != benchTargets {
			b.Fatal("unexpected section size")
		}
	}
}
