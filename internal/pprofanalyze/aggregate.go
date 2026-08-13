package pprofanalyze

import (
	"errors"
	"fmt"
	"sort"

	pprofprofile "github.com/google/pprof/profile"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

var reportGranularities = []string{
	profilemodel.GranularityFunctions,
	profilemodel.GranularityFileFunctions,
	profilemodel.GranularityFiles,
	profilemodel.GranularityLines,
}

// AnalyzeInterval produces flat and cumulative reports without mutating the
// decoded profile. Interval profiles cannot contain negative samples.
func AnalyzeInterval(decoded *DecodedProfile, topN int) ([]profilemodel.ProfileSummary, error) {
	profile, _, err := checkedProfile(decoded, topN)
	if err != nil {
		return nil, err
	}
	for sampleIndex, sample := range profile.Sample {
		for valueIndex, value := range sample.Value {
			if value < 0 {
				return nil, fmt.Errorf("pprofanalyze: %s at sample %d value %d", profilemodel.DiagnosticNegativeIntervalSample, sampleIndex, valueIndex)
			}
		}
	}
	return aggregate(profile, profilemodel.ProfileModeInterval, topN)
}

// AnalyzeDelta subtracts open from close using checked arithmetic and the
// google/pprof compatibility rules. Both input profiles remain untouched.
func AnalyzeDelta(open, closeProfile *DecodedProfile, topN int) ([]profilemodel.ProfileSummary, error) {
	merged, err := DeltaProfile(open, closeProfile, topN)
	if err != nil {
		return nil, err
	}
	return aggregate(merged, profilemodel.ProfileModeCumulativeDelta, topN)
}

// DeltaProfile returns close-open after the same compatibility and overflow
// checks used by AnalyzeDelta. Callers use it for signed flame layouts.
func DeltaProfile(open, closeProfile *DecodedProfile, topN int) (*pprofprofile.Profile, error) {
	openProfile, openBudgets, err := checkedProfile(open, topN)
	if err != nil {
		return nil, fmt.Errorf("open profile: %w", err)
	}
	closeChecked, closeBudgets, err := checkedProfile(closeProfile, topN)
	if err != nil {
		return nil, fmt.Errorf("close profile: %w", err)
	}
	if len(openBudgets) != len(closeBudgets) {
		return nil, errors.New("pprofanalyze: sample-type-incompatible: budget count differs")
	}
	for i := range openBudgets {
		if _, ok := profilemodel.CheckedAdd(openBudgets[i], closeBudgets[i]); !ok {
			return nil, fmt.Errorf("pprofanalyze: %s at sample type %d", profilemodel.DiagnosticSampleValueOverflow, i)
		}
	}

	negativeOpen := openProfile.Copy()
	for sampleIndex, sample := range negativeOpen.Sample {
		for valueIndex, value := range sample.Value {
			negated, ok := profilemodel.CheckedNegate(value)
			if !ok {
				return nil, fmt.Errorf("pprofanalyze: %s at open sample %d value %d", profilemodel.DiagnosticSampleValueOverflow, sampleIndex, valueIndex)
			}
			sample.Value[valueIndex] = negated
		}
	}
	merged, err := pprofprofile.Merge([]*pprofprofile.Profile{closeChecked.Copy(), negativeOpen})
	if err != nil {
		return nil, fmt.Errorf("pprofanalyze: %s: %w", profilemodel.DiagnosticSampleTypeIncompatible, err)
	}
	if err := merged.CheckValid(); err != nil {
		return nil, fmt.Errorf("pprofanalyze: merged profile is invalid: %w", err)
	}
	if _, err := absoluteBudgets(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func checkedProfile(decoded *DecodedProfile, topN int) (*pprofprofile.Profile, []int64, error) {
	if decoded == nil || decoded.Profile == nil {
		return nil, nil, errors.New("pprofanalyze: decoded profile is required")
	}
	if topN <= 0 || topN > profilemodel.MaxTopNodes {
		return nil, nil, fmt.Errorf("pprofanalyze: top limit %d is outside 1..%d", topN, profilemodel.MaxTopNodes)
	}
	if err := decoded.Profile.CheckValid(); err != nil {
		return nil, nil, fmt.Errorf("pprofanalyze: profile is not valid: %w", err)
	}
	budgets, err := absoluteBudgets(decoded.Profile)
	if err != nil {
		return nil, nil, err
	}
	return decoded.Profile, budgets, nil
}

func aggregate(profile *pprofprofile.Profile, mode string, topN int) ([]profilemodel.ProfileSummary, error) {
	type tables struct {
		flat map[nodeKey]int64
		cum  map[nodeKey]int64
	}
	perType := make([]map[string]*tables, len(profile.SampleType))
	positive := make([]int64, len(profile.SampleType))
	negative := make([]int64, len(profile.SampleType))
	for i := range perType {
		perType[i] = make(map[string]*tables, len(reportGranularities))
		for _, granularity := range reportGranularities {
			perType[i][granularity] = &tables{flat: make(map[nodeKey]int64), cum: make(map[nodeKey]int64)}
		}
	}

	for sampleIndex, sample := range profile.Sample {
		for valueIndex, value := range sample.Value {
			var ok bool
			if value >= 0 {
				positive[valueIndex], ok = profilemodel.CheckedAdd(positive[valueIndex], value)
			} else {
				magnitude, magnitudeOK := profilemodel.CheckedAbs(value)
				if !magnitudeOK {
					return nil, fmt.Errorf("pprofanalyze: %s at sample %d value %d", profilemodel.DiagnosticSampleValueOverflow, sampleIndex, valueIndex)
				}
				negative[valueIndex], ok = profilemodel.CheckedAdd(negative[valueIndex], magnitude)
			}
			if !ok {
				return nil, fmt.Errorf("pprofanalyze: %s at sample %d value %d", profilemodel.DiagnosticSampleValueOverflow, sampleIndex, valueIndex)
			}

			for _, granularity := range reportGranularities {
				if key, ok := flatKey(sample, granularity); ok {
					if err := addNode(perType[valueIndex][granularity].flat, key, value); err != nil {
						return nil, err
					}
				}
				seen := make(map[nodeKey]struct{})
				for _, location := range sample.Location {
					for _, line := range location.Line {
						key, ok := keyForLine(line, granularity)
						if !ok {
							continue
						}
						seen[key] = struct{}{}
					}
				}
				for key := range seen {
					if err := addNode(perType[valueIndex][granularity].cum, key, value); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	summaries := make([]profilemodel.ProfileSummary, len(profile.SampleType))
	for i, sampleType := range profile.SampleType {
		negated, ok := profilemodel.CheckedNegate(negative[i])
		if !ok {
			return nil, fmt.Errorf("pprofanalyze: %s computing net total", profilemodel.DiagnosticSampleValueOverflow)
		}
		net, ok := profilemodel.CheckedAdd(positive[i], negated)
		if !ok {
			return nil, fmt.Errorf("pprofanalyze: %s computing net total", profilemodel.DiagnosticSampleValueOverflow)
		}
		denominator := positive[i]
		denominatorMode := profilemodel.DenominatorNet
		if mode == profilemodel.ProfileModeCumulativeDelta {
			denominator, ok = profilemodel.CheckedAdd(positive[i], negative[i])
			if !ok {
				return nil, fmt.Errorf("pprofanalyze: %s computing percentage denominator", profilemodel.DiagnosticSampleValueOverflow)
			}
			denominatorMode = profilemodel.DenominatorAbsoluteAddress
		}
		reports := make([]profilemodel.ProfileReport, 0, len(reportGranularities))
		for _, granularity := range reportGranularities {
			table := perType[i][granularity]
			reports = append(reports, profilemodel.ProfileReport{
				Granularity: granularity,
				TopFlat:     topNodes(table.flat, topN, false), TopCumulative: topNodes(table.cum, topN, false),
				TopNegativeFlat: topNodes(table.flat, topN, true), TopNegativeCumulative: topNodes(table.cum, topN, true),
			})
		}
		summaries[i] = profilemodel.ProfileSummary{
			SampleType: sampleType.Type, Unit: sampleType.Unit,
			NetTotal: net, PositiveTotal: positive[i], NegativeMagnitude: negative[i],
			PercentDenominator: denominator, DenominatorMode: denominatorMode,
			Reports: reports,
		}
	}
	return summaries, nil
}

type nodeKey struct {
	function string
	file     string
	line     int64
}

func flatKey(sample *pprofprofile.Sample, granularity string) (nodeKey, bool) {
	if len(sample.Location) == 0 || len(sample.Location[0].Line) == 0 {
		return nodeKey{}, false
	}
	return keyForLine(sample.Location[0].Line[0], granularity)
}

func keyForLine(line pprofprofile.Line, granularity string) (nodeKey, bool) {
	if line.Function == nil || line.Function.Name == "" || line.Function.Filename == "" {
		return nodeKey{}, false
	}
	switch granularity {
	case profilemodel.GranularityFunctions:
		return nodeKey{function: line.Function.Name}, true
	case profilemodel.GranularityFileFunctions:
		return nodeKey{function: line.Function.Name, file: line.Function.Filename}, true
	case profilemodel.GranularityFiles:
		return nodeKey{file: line.Function.Filename}, true
	case profilemodel.GranularityLines:
		return nodeKey{function: line.Function.Name, file: line.Function.Filename, line: line.Line}, true
	default:
		return nodeKey{}, false
	}
}

func addNode(table map[nodeKey]int64, key nodeKey, value int64) error {
	next, ok := profilemodel.CheckedAdd(table[key], value)
	if !ok {
		return fmt.Errorf("pprofanalyze: %s aggregating node", profilemodel.DiagnosticSampleValueOverflow)
	}
	table[key] = next
	return nil
}

func topNodes(table map[nodeKey]int64, limit int, negative bool) []profilemodel.ProfileNode {
	nodes := make([]profilemodel.ProfileNode, 0, len(table))
	for key, value := range table {
		if value == 0 || negative != (value < 0) {
			continue
		}
		nodes = append(nodes, profilemodel.ProfileNode{Function: key.function, File: key.file, Line: key.line, Value: value})
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Value != nodes[j].Value {
			if negative {
				return nodes[i].Value < nodes[j].Value
			}
			return nodes[i].Value > nodes[j].Value
		}
		if nodes[i].Function != nodes[j].Function {
			return nodes[i].Function < nodes[j].Function
		}
		if nodes[i].File != nodes[j].File {
			return nodes[i].File < nodes[j].File
		}
		return nodes[i].Line < nodes[j].Line
	})
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes
}
