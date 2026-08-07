package pprofanalyze

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	pprofprofile "github.com/google/pprof/profile"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

type IsolationProof struct {
	seal *isolationProofSeal
}

type isolationProofSeal struct{}

var verifiedIsolationSeal = &isolationProofSeal{}

// newVerifiedIsolationProof is intentionally package-private. The worker
// bootstrap is the only production path allowed to mint it after checking a
// hard OS limit and observing the child in SIGSTOP. Callers outside this
// package can only construct the zero value, which Decode rejects.
func newVerifiedIsolationProof() IsolationProof {
	return IsolationProof{seal: verifiedIsolationSeal}
}

type Limits struct {
	CompressedBytes    int64
	ExpandedBytes      int64
	Samples            int
	Locations          int
	Functions          int
	Mappings           int
	SampleTypes        int
	Comments           int
	LocationRefs       uint64
	LabelValues        uint64
	LabelKeysPerSample int
	LabelValuesPerKey  int
}

func DefaultLimits() Limits {
	return Limits{
		CompressedBytes: 32 << 20, ExpandedBytes: 64 << 20,
		Samples: 500_000, Locations: 250_000, Functions: 250_000,
		Mappings: 65_536, SampleTypes: 16, Comments: 128,
		LocationRefs: 5_000_000, LabelValues: 1_000_000,
		LabelKeysPerSample: 16, LabelValuesPerKey: 32,
	}
}

type DecodedProfile struct {
	Profile         *pprofprofile.Profile
	AbsoluteBudgets []int64
}

// Decode must only run after the worker bootstrap has proved a hard memory
// primitive. It bounds compressed and expanded bytes before protobuf decode,
// calls CheckValid, then applies object/string and checked-value preflights
// before returning a profile usable by aggregation.
func Decode(reader io.Reader, limits Limits, isolation IsolationProof) (*DecodedProfile, error) {
	if isolation.seal != verifiedIsolationSeal {
		return nil, errors.New("pprofanalyze: hard isolation and stopped bootstrap are required")
	}
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	compressed, err := readBounded(reader, limits.CompressedBytes, "compressed profile")
	if err != nil {
		return nil, err
	}
	expanded := compressed
	if len(compressed) >= 2 && compressed[0] == 0x1f && compressed[1] == 0x8b {
		gzipReader, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, fmt.Errorf("pprofanalyze: open gzip profile: %w", err)
		}
		expanded, err = readBounded(gzipReader, limits.ExpandedBytes, "expanded profile")
		closeErr := gzipReader.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, fmt.Errorf("pprofanalyze: close gzip profile: %w", closeErr)
		}
	} else if int64(len(expanded)) > limits.ExpandedBytes {
		return nil, fmt.Errorf("pprofanalyze: expanded profile exceeds %d bytes", limits.ExpandedBytes)
	}
	profile, err := pprofprofile.ParseUncompressed(expanded)
	if err != nil {
		return nil, fmt.Errorf("pprofanalyze: parse uncompressed profile: %w", err)
	}
	if err := profile.CheckValid(); err != nil {
		return nil, fmt.Errorf("pprofanalyze: profile is not valid: %w", err)
	}
	if err := validateObjects(profile, limits); err != nil {
		return nil, err
	}
	budgets, err := absoluteBudgets(profile)
	if err != nil {
		return nil, err
	}
	return &DecodedProfile{Profile: profile, AbsoluteBudgets: budgets}, nil
}

func readBounded(reader io.Reader, limit int64, name string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("pprofanalyze: read %s: %w", name, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("pprofanalyze: %s exceeds %d bytes", name, limit)
	}
	return body, nil
}

func validateLimits(limits Limits) error {
	if limits.CompressedBytes < 0 || limits.ExpandedBytes < 0 || limits.CompressedBytes > 1<<30 || limits.ExpandedBytes > 1<<30 ||
		limits.Samples < 0 || limits.Locations < 0 || limits.Functions < 0 || limits.Mappings < 0 || limits.SampleTypes < 0 ||
		limits.Comments < 0 || limits.LabelKeysPerSample < 0 || limits.LabelValuesPerKey < 0 {
		return errors.New("pprofanalyze: invalid decode limits")
	}
	return nil
}

func validateObjects(profile *pprofprofile.Profile, limits Limits) error {
	if len(profile.Sample) > limits.Samples {
		return fmt.Errorf("pprofanalyze: samples %d exceed %d", len(profile.Sample), limits.Samples)
	}
	if len(profile.Location) > limits.Locations {
		return fmt.Errorf("pprofanalyze: locations %d exceed %d", len(profile.Location), limits.Locations)
	}
	if len(profile.Function) > limits.Functions {
		return fmt.Errorf("pprofanalyze: functions %d exceed %d", len(profile.Function), limits.Functions)
	}
	if len(profile.Mapping) > limits.Mappings {
		return fmt.Errorf("pprofanalyze: mappings %d exceed %d", len(profile.Mapping), limits.Mappings)
	}
	if len(profile.SampleType) > limits.SampleTypes {
		return fmt.Errorf("pprofanalyze: sample types %d exceed %d", len(profile.SampleType), limits.SampleTypes)
	}
	if len(profile.Comments) > limits.Comments {
		return fmt.Errorf("pprofanalyze: comments %d exceed %d", len(profile.Comments), limits.Comments)
	}
	for i, valueType := range profile.SampleType {
		if valueType == nil || !boundedString(valueType.Type, 64) || !boundedString(valueType.Unit, 32) {
			return fmt.Errorf("pprofanalyze: sample type[%d] type/unit exceeds bounds", i)
		}
	}
	if profile.PeriodType != nil && (profile.PeriodType.Type != "" || profile.PeriodType.Unit != "") {
		if !boundedString(profile.PeriodType.Type, 64) || !boundedString(profile.PeriodType.Unit, 32) {
			return errors.New("pprofanalyze: period sample type exceeds bounds")
		}
	}
	for i, function := range profile.Function {
		if function == nil || !boundedString(function.Name, 512) || !boundedOptionalString(function.SystemName, 512) || !boundedOptionalString(function.Filename, 512) {
			return fmt.Errorf("pprofanalyze: function[%d] strings exceed bounds", i)
		}
	}
	for i, mapping := range profile.Mapping {
		if mapping == nil || !boundedOptionalString(mapping.File, 512) || !boundedOptionalString(mapping.BuildID, 512) {
			return fmt.Errorf("pprofanalyze: mapping[%d] strings exceed bounds", i)
		}
	}
	for i, comment := range profile.Comments {
		if !boundedString(comment, 1024) {
			return fmt.Errorf("pprofanalyze: comment[%d] exceeds bounds", i)
		}
	}
	if !boundedOptionalString(profile.DropFrames, 1024) || !boundedOptionalString(profile.KeepFrames, 1024) || !boundedOptionalString(profile.DefaultSampleType, 64) {
		return errors.New("pprofanalyze: profile selector string exceeds bounds")
	}

	var locationRefs, labelValues uint64
	for i, sample := range profile.Sample {
		if sample == nil {
			return fmt.Errorf("pprofanalyze: sample[%d] is nil", i)
		}
		var ok bool
		locationRefs, ok = profilemodel.CheckedAddUint64(locationRefs, uint64(len(sample.Location)))
		if !ok || locationRefs > limits.LocationRefs {
			return errors.New("pprofanalyze: location references exceed limit")
		}
		keys := make(map[string]struct{}, len(sample.Label)+len(sample.NumLabel))
		for key, values := range sample.Label {
			keys[key] = struct{}{}
			if !boundedString(key, 64) || len(values) > limits.LabelValuesPerKey {
				return fmt.Errorf("pprofanalyze: sample[%d] string label exceeds bounds", i)
			}
			for _, value := range values {
				if !boundedString(value, 128) {
					return fmt.Errorf("pprofanalyze: sample[%d] label value exceeds bounds", i)
				}
			}
			labelValues, ok = profilemodel.CheckedAddUint64(labelValues, uint64(len(values)))
			if !ok || labelValues > limits.LabelValues {
				return errors.New("pprofanalyze: label values exceed limit")
			}
		}
		for key, values := range sample.NumLabel {
			keys[key] = struct{}{}
			if !boundedString(key, 64) || len(values) > limits.LabelValuesPerKey {
				return fmt.Errorf("pprofanalyze: sample[%d] numeric label exceeds bounds", i)
			}
			units := sample.NumUnit[key]
			if len(units) != 0 && len(units) != len(values) {
				return fmt.Errorf("pprofanalyze: sample[%d] numeric label units mismatch", i)
			}
			for _, unit := range units {
				if !boundedString(unit, 32) {
					return fmt.Errorf("pprofanalyze: sample[%d] numeric label unit exceeds bounds", i)
				}
			}
			labelValues, ok = profilemodel.CheckedAddUint64(labelValues, uint64(len(values)))
			if !ok || labelValues > limits.LabelValues {
				return errors.New("pprofanalyze: label values exceed limit")
			}
		}
		if len(keys) > limits.LabelKeysPerSample {
			return fmt.Errorf("pprofanalyze: sample[%d] label keys exceed limit", i)
		}
	}
	return nil
}

func absoluteBudgets(profile *pprofprofile.Profile) ([]int64, error) {
	budgets := make([]int64, len(profile.SampleType))
	for sampleIndex, sample := range profile.Sample {
		for valueIndex, value := range sample.Value {
			magnitude, ok := profilemodel.CheckedAbs(value)
			if !ok {
				return nil, fmt.Errorf("pprofanalyze: sample-value-overflow at sample %d value %d", sampleIndex, valueIndex)
			}
			budgets[valueIndex], ok = profilemodel.CheckedAdd(budgets[valueIndex], magnitude)
			if !ok {
				return nil, fmt.Errorf("pprofanalyze: sample-value-overflow at sample %d value %d", sampleIndex, valueIndex)
			}
		}
	}
	return budgets, nil
}

func boundedString(value string, max int) bool {
	return value != "" && boundedOptionalString(value, max)
}

func boundedOptionalString(value string, max int) bool {
	return utf8.ValidString(value) && len(value) <= max
}
