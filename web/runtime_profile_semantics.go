package web

import rpprof "runtime/pprof"

const (
	profileModeCumulativeDelta = "cumulative-delta"
	profileModeInterval        = "interval"
)

// RuntimeProfileSemantic is the stable contract for each runtime profile.
// Interval profiles are close-boundary snapshots; cumulative profiles require
// both boundaries and include measured head/tail approximation.
type RuntimeProfileSemantic struct {
	Kind         string   `json:"kind"`
	Available    bool     `json:"available"`
	Mode         string   `json:"mode"`
	CapturePoint string   `json:"capture_point"`
	SampleTypes  []string `json:"sample_types"`
	Approximate  bool     `json:"approximate"`
	Interference string   `json:"interference,omitempty"`
}

// RuntimeProfileSemantics reports runtime availability instead of assuming a
// profile added by a newer Go release exists in the running binary.
func RuntimeProfileSemantics() []RuntimeProfileSemantic {
	semantics := []RuntimeProfileSemantic{
		{Kind: "mutex", Mode: profileModeCumulativeDelta, CapturePoint: "open+close", SampleTypes: []string{"contentions/count", "delay/nanoseconds"}, Approximate: true, Interference: "requires a non-zero mutex sampling fraction"},
		{Kind: "block", Mode: profileModeCumulativeDelta, CapturePoint: "open+close", SampleTypes: []string{"contentions/count", "delay/nanoseconds"}, Approximate: true, Interference: "block profiling can affect scheduler measurements"},
		{Kind: "threadcreate", Mode: profileModeCumulativeDelta, CapturePoint: "open+close", SampleTypes: []string{"threadcreate/count"}, Approximate: true},
		{Kind: "allocs", Mode: profileModeCumulativeDelta, CapturePoint: "open+close", SampleTypes: []string{"alloc_objects/count", "alloc_space/bytes", "inuse_objects/count", "inuse_space/bytes"}, Approximate: true, Interference: "memory profile sampling may affect CPU measurements"},
		{Kind: "heap", Mode: profileModeCumulativeDelta, CapturePoint: "open+close", SampleTypes: []string{"alloc_objects/count", "alloc_space/bytes", "inuse_objects/count", "inuse_space/bytes"}, Approximate: true, Interference: "memory profile sampling may affect CPU measurements"},
		{Kind: "goroutine", Mode: profileModeInterval, CapturePoint: "close", SampleTypes: []string{"goroutine/count"}, Approximate: true},
		{Kind: "goroutineleak", Mode: profileModeInterval, CapturePoint: "close", SampleTypes: []string{"goroutine/count"}, Approximate: true},
	}
	for index := range semantics {
		semantics[index].Available = rpprof.Lookup(semantics[index].Kind) != nil
	}
	return semantics
}

func runtimeProfileMode(kind string) string {
	for _, semantic := range RuntimeProfileSemantics() {
		if semantic.Kind == kind {
			return semantic.Mode
		}
	}
	return profileModeCumulativeDelta
}

func captureProfileAt(kind string, point ProfilePoint) bool {
	return runtimeProfileMode(kind) != profileModeInterval || point == ProfilePointClose
}
