package slowlog

import (
	"testing"
	"time"
)

func TestEvaluateCoverageDetectsRotationTruncationAndClock(t *testing.T) {
	start := CapturePoint{Identity: FileIdentity{Device: 1, Inode: 2}, Offset: 100, DBClock: time.Unix(1000, 0)}
	tests := []struct {
		name string
		end  CapturePoint
		ok   bool
		code string
	}{
		{"complete", CapturePoint{Identity: start.Identity, Offset: 200, DBClock: time.Unix(1001, 0)}, true, ""},
		{"rotated", CapturePoint{Identity: FileIdentity{Device: 1, Inode: 3}, Offset: 200, DBClock: time.Unix(1001, 0)}, false, "log-rotated"},
		{"copytruncate", CapturePoint{Identity: start.Identity, Offset: 50, DBClock: time.Unix(1001, 0)}, false, "log-truncated"},
		{"clock", CapturePoint{Identity: start.Identity, Offset: 200, DBClock: time.Unix(999, 0)}, false, "db-clock-backwards"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateCoverage(start, tc.end)
			if got.Complete != tc.ok || got.Reason != tc.code || got.StartOffset != 100 || got.EndOffset != tc.end.Offset {
				t.Fatalf("coverage=%+v", got)
			}
		})
	}
}
