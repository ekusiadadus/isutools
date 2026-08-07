package pprofanalyze

import (
	"bytes"
	"compress/gzip"
	"math"
	"strings"
	"testing"

	pprofprofile "github.com/google/pprof/profile"
)

func encodedProfile(t *testing.T, profile *pprofprofile.Profile, compressed bool) []byte {
	t.Helper()
	var raw bytes.Buffer
	if err := profile.WriteUncompressed(&raw); err != nil {
		t.Fatalf("WriteUncompressed: %v", err)
	}
	if !compressed {
		return raw.Bytes()
	}
	var zipped bytes.Buffer
	writer := gzip.NewWriter(&zipped)
	if _, err := writer.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return zipped.Bytes()
}

func validProfile(values ...int64) *pprofprofile.Profile {
	return &pprofprofile.Profile{
		SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample:     []*pprofprofile.Sample{{Value: values}},
	}
}

func hardIsolation() IsolationProof {
	return newVerifiedIsolationProof()
}

func TestDecodeValidatesAndComputesCheckedAbsoluteBudget(t *testing.T) {
	t.Parallel()

	for _, compressed := range []bool{false, true} {
		compressed := compressed
		t.Run(map[bool]string{false: "protobuf", true: "gzip"}[compressed], func(t *testing.T) {
			t.Parallel()
			body := encodedProfile(t, validProfile(7), compressed)
			decoded, err := Decode(bytes.NewReader(body), DefaultLimits(), hardIsolation())
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(decoded.AbsoluteBudgets) != 1 || decoded.AbsoluteBudgets[0] != 7 {
				t.Fatalf("absolute budgets = %v", decoded.AbsoluteBudgets)
			}
		})
	}
}

func TestDecodeFailsClosedWithoutHardIsolation(t *testing.T) {
	t.Parallel()

	body := encodedProfile(t, validProfile(1), false)
	if _, err := Decode(bytes.NewReader(body), DefaultLimits(), IsolationProof{}); err == nil || !strings.Contains(err.Error(), "hard isolation") {
		t.Fatalf("Decode = %v, want hard-isolation failure", err)
	}
}

func TestDecodeRejectsExpansionAndCompressedCeilingsBeforeParse(t *testing.T) {
	t.Parallel()

	body := encodedProfile(t, validProfile(1), true)
	limits := DefaultLimits()
	limits.CompressedBytes = int64(len(body) - 1)
	if _, err := Decode(bytes.NewReader(body), limits, hardIsolation()); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("compressed limit = %v", err)
	}

	limits = DefaultLimits()
	limits.ExpandedBytes = 4
	if _, err := Decode(bytes.NewReader(body), limits, hardIsolation()); err == nil || !strings.Contains(err.Error(), "expanded") {
		t.Fatalf("expanded limit = %v", err)
	}
}

func TestDecodeRejectsInvalidProfileAndValueOverflow(t *testing.T) {
	t.Parallel()

	invalid := validProfile(1, 2)
	if _, err := Decode(bytes.NewReader(encodedProfile(t, invalid, false)), DefaultLimits(), hardIsolation()); err == nil || !strings.Contains(err.Error(), "valid") {
		t.Fatalf("invalid profile = %v", err)
	}
	overflow := validProfile(math.MinInt64)
	if _, err := Decode(bytes.NewReader(encodedProfile(t, overflow, false)), DefaultLimits(), hardIsolation()); err == nil || !strings.Contains(err.Error(), "sample-value-overflow") {
		t.Fatalf("overflow profile = %v", err)
	}
}

func TestDecodeRejectsObjectAndStringCaps(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.Samples = 0
	if _, err := Decode(bytes.NewReader(encodedProfile(t, validProfile(1), false)), limits, hardIsolation()); err == nil || !strings.Contains(err.Error(), "samples") {
		t.Fatalf("sample cap = %v", err)
	}

	longType := validProfile(1)
	longType.SampleType[0].Type = strings.Repeat("x", 65)
	if _, err := Decode(bytes.NewReader(encodedProfile(t, longType, false)), DefaultLimits(), hardIsolation()); err == nil || !strings.Contains(err.Error(), "sample type") {
		t.Fatalf("type cap = %v", err)
	}
}

func richProfile() *pprofprofile.Profile {
	mapping := &pprofprofile.Mapping{ID: 1, Start: 1, Limit: 2, File: "app", BuildID: "build"}
	function := &pprofprofile.Function{ID: 1, Name: "main.work", SystemName: "main.work", Filename: "main.go"}
	location := &pprofprofile.Location{ID: 1, Mapping: mapping, Address: 1, Line: []pprofprofile.Line{{Function: function, Line: 10}}}
	return &pprofprofile.Profile{
		SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample: []*pprofprofile.Sample{{
			Location: []*pprofprofile.Location{location}, Value: []int64{1},
			Label:    map[string][]string{"route": {"/items/{id}"}},
			NumLabel: map[string][]int64{"bytes": {1}}, NumUnit: map[string][]string{"bytes": {"bytes"}},
		}},
		Mapping: []*pprofprofile.Mapping{mapping}, Location: []*pprofprofile.Location{location}, Function: []*pprofprofile.Function{function},
		Comments: []string{"fixture"}, DropFrames: "drop", KeepFrames: "keep", DefaultSampleType: "cpu",
	}
}

func TestDecodeObjectCapMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		change  func(*Limits)
		wantErr string
	}{
		{name: "locations", change: func(l *Limits) { l.Locations = 0 }, wantErr: "locations"},
		{name: "functions", change: func(l *Limits) { l.Functions = 0 }, wantErr: "functions"},
		{name: "mappings", change: func(l *Limits) { l.Mappings = 0 }, wantErr: "mappings"},
		{name: "sample-types", change: func(l *Limits) { l.SampleTypes = 0 }, wantErr: "sample types"},
		{name: "comments", change: func(l *Limits) { l.Comments = 0 }, wantErr: "comments"},
		{name: "location-refs", change: func(l *Limits) { l.LocationRefs = 0 }, wantErr: "location references"},
		{name: "label-values", change: func(l *Limits) { l.LabelValues = 0 }, wantErr: "label values"},
		{name: "label-keys", change: func(l *Limits) { l.LabelKeysPerSample = 1 }, wantErr: "label keys"},
		{name: "label-values-per-key", change: func(l *Limits) { l.LabelValuesPerKey = 0 }, wantErr: "label"},
	}
	body := encodedProfile(t, richProfile(), false)
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limits := DefaultLimits()
			tt.change(&limits)
			if _, err := Decode(bytes.NewReader(body), limits, hardIsolation()); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Decode = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeStringCapMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		change  func(*pprofprofile.Profile)
		wantErr string
	}{
		{name: "function", change: func(p *pprofprofile.Profile) { p.Function[0].Name = strings.Repeat("f", 513) }, wantErr: "function"},
		{name: "mapping", change: func(p *pprofprofile.Profile) { p.Mapping[0].BuildID = strings.Repeat("b", 513) }, wantErr: "mapping"},
		{name: "comment", change: func(p *pprofprofile.Profile) { p.Comments[0] = strings.Repeat("c", 1025) }, wantErr: "comment"},
		{name: "selector", change: func(p *pprofprofile.Profile) { p.DropFrames = strings.Repeat("d", 1025) }, wantErr: "selector"},
		{name: "label-key", change: func(p *pprofprofile.Profile) { p.Sample[0].Label = map[string][]string{strings.Repeat("k", 65): {"v"}} }, wantErr: "string label"},
		{name: "label-value", change: func(p *pprofprofile.Profile) {
			p.Sample[0].Label = map[string][]string{"k": {strings.Repeat("v", 129)}}
		}, wantErr: "label value"},
		{name: "numeric-unit", change: func(p *pprofprofile.Profile) { p.Sample[0].NumUnit["bytes"] = []string{strings.Repeat("u", 33)} }, wantErr: "numeric label unit"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			profile := richProfile()
			tt.change(profile)
			if _, err := Decode(bytes.NewReader(encodedProfile(t, profile, false)), DefaultLimits(), hardIsolation()); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Decode = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeRejectsSummedSampleOverflowAndInvalidLimits(t *testing.T) {
	t.Parallel()

	profile := &pprofprofile.Profile{
		SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample:     []*pprofprofile.Sample{{Value: []int64{math.MaxInt64}}, {Value: []int64{1}}},
	}
	if _, err := Decode(bytes.NewReader(encodedProfile(t, profile, false)), DefaultLimits(), hardIsolation()); err == nil || !strings.Contains(err.Error(), "sample-value-overflow") {
		t.Fatalf("sum overflow = %v", err)
	}
	limits := DefaultLimits()
	limits.ExpandedBytes = -1
	if _, err := Decode(bytes.NewReader(nil), limits, hardIsolation()); err == nil || !strings.Contains(err.Error(), "invalid decode limits") {
		t.Fatalf("invalid limits = %v", err)
	}
}
