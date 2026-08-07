package pprofanalyze

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	pprofprofile "github.com/google/pprof/profile"

	"github.com/ekusiadadus/isutools/internal/profilecapture"
	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

var logicalLabelKeys = []string{"http.method", "http.route", "isutools.scenario", "isutools.region"}

// AggregateTrustedLabels accepts only the two private physical labels and an
// exact, sealed capture dictionary. Application labels with logical names are
// ignored. foreign is true when a sample tried to use the private namespace
// but failed capture/tuple/dictionary validation.
func AggregateTrustedLabels(profile *pprofprofile.Profile, sampleType int, dictionary profilecapture.LabelDictionary) ([]profilemodel.LabelBreakdown, bool, error) {
	if profile == nil || sampleType < 0 || sampleType >= len(profile.SampleType) {
		return nil, false, errors.New("pprofanalyze: invalid label aggregation input")
	}
	tuples, err := validateLabelDictionary(dictionary)
	if err != nil {
		return nil, false, err
	}
	totals := make(map[string]map[string]int64, len(logicalLabelKeys))
	for _, key := range logicalLabelKeys {
		totals[key] = make(map[string]int64)
	}
	foreign := false
	for sampleIndex, sample := range profile.Sample {
		captureValues, hasCapture := sample.Label[profilecapture.PrivateCaptureLabel]
		tupleValues, hasTuple := sample.Label[profilecapture.PrivateTupleLabel]
		_, numericCapture := sample.NumLabel[profilecapture.PrivateCaptureLabel]
		_, numericTuple := sample.NumLabel[profilecapture.PrivateTupleLabel]
		if !hasCapture && !hasTuple && !numericCapture && !numericTuple {
			continue
		}
		if numericCapture || numericTuple || len(captureValues) != 1 || len(tupleValues) != 1 ||
			captureValues[0] != dictionary.CaptureID || !opaqueID(captureValues[0]) || !opaqueID(tupleValues[0]) {
			foreign = true
			continue
		}
		tuple, ok := tuples[tupleValues[0]]
		if !ok {
			foreign = true
			continue
		}
		if sampleType >= len(sample.Value) {
			return nil, foreign, fmt.Errorf("pprofanalyze: sample %d has no value type %d", sampleIndex, sampleType)
		}
		values := map[string]string{
			"http.method": tuple.Method, "http.route": tuple.Route,
			"isutools.scenario": tuple.Scenario, "isutools.region": tuple.Region,
		}
		for key, value := range values {
			if value == "" {
				continue
			}
			next, ok := profilemodel.CheckedAdd(totals[key][value], sample.Value[sampleType])
			if !ok {
				return nil, foreign, fmt.Errorf("pprofanalyze: %s aggregating label at sample %d", profilemodel.DiagnosticSampleValueOverflow, sampleIndex)
			}
			totals[key][value] = next
		}
	}
	breakdowns := make([]profilemodel.LabelBreakdown, 0, len(logicalLabelKeys))
	for _, key := range logicalLabelKeys {
		values := make([]profilemodel.LabelValue, 0, len(totals[key]))
		for value, total := range totals[key] {
			if total != 0 {
				values = append(values, profilemodel.LabelValue{Value: value, Total: total})
			}
		}
		sort.Slice(values, func(i, j int) bool {
			if values[i].Total != values[j].Total {
				return values[i].Total > values[j].Total
			}
			return values[i].Value < values[j].Value
		})
		if len(values) != 0 {
			breakdowns = append(breakdowns, profilemodel.LabelBreakdown{Key: key, Values: values})
		}
	}
	return breakdowns, foreign, nil
}

func validateLabelDictionary(dictionary profilecapture.LabelDictionary) (map[string]profilecapture.SafeLabelTuple, error) {
	if !dictionary.Sealed || dictionary.RunID == "" || dictionary.Epoch == 0 || !opaqueID(dictionary.CaptureID) ||
		len(dictionary.Tuples) > profilecapture.MaxLabelTuples || !fullHash(dictionary.SHA256) {
		return nil, errors.New("pprofanalyze: invalid CPU label dictionary envelope")
	}
	copy := dictionary
	copy.SHA256 = ""
	body, err := json.Marshal(copy)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != dictionary.SHA256 {
		return nil, errors.New("pprofanalyze: CPU label dictionary hash mismatch")
	}
	tuples := make(map[string]profilecapture.SafeLabelTuple, len(dictionary.Tuples))
	overflow := 0
	for _, tuple := range dictionary.Tuples {
		if !opaqueID(tuple.TupleID) || tuple.Method == "" || tuple.Route == "" || len(tuple.Method) > 16 || len(tuple.Route) > 128 || len(tuple.Scenario) > 64 || len(tuple.Region) > 64 ||
			strings.ContainsAny(tuple.Method+tuple.Route+tuple.Scenario+tuple.Region, "\x00\r\n") {
			return nil, errors.New("pprofanalyze: invalid CPU label dictionary tuple")
		}
		if _, duplicate := tuples[tuple.TupleID]; duplicate {
			return nil, errors.New("pprofanalyze: duplicate CPU label tuple ID")
		}
		if tuple.Overflow {
			overflow++
		}
		tuples[tuple.TupleID] = tuple
	}
	if overflow > 1 {
		return nil, errors.New("pprofanalyze: multiple CPU label overflow tuples")
	}
	return tuples, nil
}

func opaqueID(value string) bool {
	return len(value) == 32 && lowerHexString(value)
}

func fullHash(value string) bool {
	return len(value) == 64 && lowerHexString(value)
}

func lowerHexString(value string) bool {
	for i := range value {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}
