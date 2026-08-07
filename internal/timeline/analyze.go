package timeline

import (
	"math"
	"sort"
	"time"
)

const evidenceLimitation = "time alignment shows correlation and ordering, not causality; unobserved queues or external services may explain the same change"

var analysisRules = []Rule{
	{ID: PhaseTrafficGrowth, Formula: "current HTTP count >= 1.4 * previous count and delta >= 10", Limitation: evidenceLimitation},
	{ID: PhaseTailEscalation, Formula: "current max operation p95 >= 2 * previous p95 and current p95 >= 50ms", Limitation: "p95 is computed per operation and the phase uses the largest operation p95; it is not a merged request distribution"},
	{ID: PhaseSaturation, Formula: "pool in_use/max_open >= 0.90, process busy >= 90%, or disk busy sum >= 90%", Limitation: evidenceLimitation},
	{ID: PhaseErrorOnset, Formula: "previous observed HTTP errors == 0 and current errors > 0", Limitation: "only responses observed by the configured HTTP collector are counted"},
	{ID: PhaseRecovery, Formula: "observed errors return to zero or the prior saturation signal clears", Limitation: evidenceLimitation},
	{ID: PhaseBottleneckMigration, Formula: "the leading measured saturation resource changes between adjacent buckets", Limitation: evidenceLimitation},
}

type bucketSummary struct {
	count, success, failures int64
	p95                      int64
	saturation               string
	saturationValue          float64
}

func Analyze(section Section) Analysis {
	analysis := Analysis{Rules: append([]Rule(nil), analysisRules...)}
	if len(section.Buckets) < 2 {
		analysis.Reason = ReasonInsufficientBuckets
		return analysis
	}
	summaries := make([]bucketSummary, len(section.Buckets))
	hasEvidence := false
	for index, bucket := range section.Buckets {
		summaries[index] = summarizeBucket(bucket)
		if summaries[index].count > 0 || len(bucket.SQL) > 0 || summaries[index].saturation != "" || bucket.Process != nil || bucket.Host != nil || len(bucket.DBPools) > 0 {
			hasEvidence = true
		}
	}
	if !hasEvidence {
		analysis.Reason = "insufficient-signal-evidence"
		return analysis
	}
	analysis.Available = true

	for index := 1; index < len(section.Buckets); index++ {
		previous, current := summaries[index-1], summaries[index]
		bucket := section.Buckets[index]
		if previous.count > 0 && current.count-previous.count >= 10 && float64(current.count) >= 1.4*float64(previous.count) {
			analysis.Phases = append(analysis.Phases, phase(bucket, PhaseTrafficGrowth, "http", "request_count", float64(current.count), analysisRules[0]))
		}
		if previous.p95 > 0 && current.p95 >= int64(50*time.Millisecond) && current.p95/2 >= previous.p95 {
			analysis.Phases = append(analysis.Phases, phase(bucket, PhaseTailEscalation, "http", "max_operation_p95_ns", float64(current.p95), analysisRules[1]))
		}
		if current.saturation != "" && previous.saturation != current.saturation {
			analysis.Phases = append(analysis.Phases, phase(bucket, PhaseSaturation, current.saturation, "utilization_percent", current.saturationValue, analysisRules[2]))
		}
		if previous.failures == 0 && current.failures > 0 {
			analysis.Phases = append(analysis.Phases, phase(bucket, PhaseErrorOnset, "http", "error_count", float64(current.failures), analysisRules[3]))
		}
		if previous.failures == 0 && current.failures > 0 || current.success < previous.success {
			analysis.Suspects = append(analysis.Suspects, lowVolumeGateSuspects(section.Buckets, index)...)
		}
		analysis.Suspects = append(analysis.Suspects, pollingSuspects(section.Buckets[index-1], bucket)...)
		if (previous.failures > 0 && current.failures == 0) || (previous.saturation != "" && current.saturation == "") {
			analysis.Phases = append(analysis.Phases, phase(bucket, PhaseRecovery, "http/resource", "error_or_saturation_recovery", 1, analysisRules[4]))
		}
		if previous.saturation != "" && current.saturation != "" && previous.saturation != current.saturation {
			p := phase(bucket, PhaseBottleneckMigration, current.saturation, "leading_saturation_resource_changed", 1, analysisRules[5])
			p.Evidence = append(p.Evidence, evidence(section.Buckets[index-1], previous.saturation, "previous_utilization_percent", previous.saturationValue, analysisRules[5]))
			analysis.Phases = append(analysis.Phases, p)
		}
	}

	analysis.Suspects = dedupeSuspects(analysis.Suspects)
	sort.SliceStable(analysis.Suspects, func(i, j int) bool {
		if analysis.Suspects[i].Score != analysis.Suspects[j].Score {
			return analysis.Suspects[i].Score > analysis.Suspects[j].Score
		}
		return analysis.Suspects[i].Key < analysis.Suspects[j].Key
	})
	if len(analysis.Suspects) > 20 {
		analysis.Suspects = analysis.Suspects[:20]
	}
	return analysis
}

func summarizeBucket(bucket Bucket) bucketSummary {
	var summary bucketSummary
	for _, operation := range bucket.HTTP {
		summary.count = saturatingNonnegativeAdd(summary.count, operation.Count)
		summary.success = saturatingNonnegativeAdd(summary.success, operation.Successes)
		summary.failures = saturatingNonnegativeAdd(summary.failures, operation.Errors)
		if operation.P95Ns > summary.p95 {
			summary.p95 = operation.P95Ns
		}
	}
	for _, pool := range bucket.DBPools {
		if pool.MaxOpen > 0 {
			value := float64(pool.InUse) * 100 / float64(pool.MaxOpen)
			if value >= 90 && value > summary.saturationValue {
				summary.saturation, summary.saturationValue = "dbpool:"+pool.TargetID, value
			}
		}
	}
	if bucket.Process != nil && bucket.Process.BusyPercent >= 90 && bucket.Process.BusyPercent > summary.saturationValue {
		summary.saturation, summary.saturationValue = "process-cpu", bucket.Process.BusyPercent
	}
	if bucket.Host != nil && bucket.Host.DiskBusyPercentSum >= 90 && bucket.Host.DiskBusyPercentSum > summary.saturationValue {
		summary.saturation, summary.saturationValue = "host-disk", bucket.Host.DiskBusyPercentSum
	}
	return summary
}

func phase(bucket Bucket, kind, signal, metric string, value float64, rule Rule) Phase {
	return Phase{
		Kind: kind, WindowStart: bucket.Start, WindowEnd: bucket.End, RuleID: rule.ID,
		Evidence: []EvidenceRef{evidence(bucket, signal, metric, value, rule)},
	}
}

func evidence(bucket Bucket, signal, metric string, value float64, rule Rule) EvidenceRef {
	return EvidenceRef{
		BucketIndex: bucket.Index, WindowStart: bucket.Start, WindowEnd: bucket.End,
		Signal: signal, Metric: metric, Value: value, Formula: rule.Formula, Limitation: rule.Limitation,
	}
}

func lowVolumeGateSuspects(buckets []Bucket, onset int) []Suspect {
	if onset <= 0 || onset >= len(buckets) {
		return nil
	}
	previous := buckets[onset-1]
	previousByKey := operationMap(previous.HTTP)
	var maxVolume int64
	for _, operation := range previous.HTTP {
		if operation.Count > maxVolume {
			maxVolume = operation.Count
		}
	}
	var suspects []Suspect
	for key, operation := range previousByKey {
		if operation.Count > 0 && maxVolume > 0 && operation.Count <= maxVolume/5 && operation.P95Ns >= int64(100*time.Millisecond) {
			earlierP95 := int64(0)
			if onset >= 2 {
				earlierP95 = operationMap(buckets[onset-2].HTTP)[key].P95Ns
			}
			if earlierP95 == 0 || float64(operation.P95Ns) >= 1.5*float64(earlierP95) {
				rule := Rule{
					ID: "low-volume-precedes-outcome-shift", Formula: "count <= 20% of busiest operation, p95 >= 100ms, and latency is elevated before error onset or successful-throughput regression",
					Limitation: evidenceLimitation,
				}
				suspects = append(suspects, Suspect{
					Signal: "low-volume critical-path candidate", Key: key, Kind: "http", Label: "correlation-suspect", Score: 100 + int(math.Min(float64(operation.P95Ns/int64(time.Millisecond)), 100)),
					Evidence: []EvidenceRef{evidence(previous, "http:"+key, "p95_ns", float64(operation.P95Ns), rule)},
				})
			}
		}
	}
	return suspects
}

func pollingSuspects(previous, current Bucket) []Suspect {
	previousByKey := operationMap(previous.HTTP)
	currentByKey := operationMap(current.HTTP)
	previousSummary, currentSummary := summarizeBucket(previous), summarizeBucket(current)
	var suspects []Suspect
	for key, operation := range currentByKey {
		base := previousByKey[key]
		if base.Count <= 0 || float64(operation.Count) < 1.2*float64(base.Count) || float64(operation.Count) < 0.5*float64(currentSummary.count) {
			continue
		}
		otherBefore := previousSummary.success - base.Successes
		otherCurrent := currentSummary.success - operation.Successes
		if otherCurrent <= otherBefore {
			rule := Rule{
				ID: "high-rate-flat-other-throughput", Formula: "operation rate grows >= 20% while successful HTTP completions excluding that operation do not grow",
				Limitation: "successful HTTP responses are only a generic useful-work proxy; polling may itself be useful work",
			}
			suspects = append(suspects, Suspect{
				Signal: "backpressure/polling symptom candidate", Key: key, Kind: "http", Label: "correlation-suspect", Score: 60,
				Evidence: []EvidenceRef{evidence(current, "http:"+key, "request_count", float64(operation.Count), rule)},
			})
		}
	}
	return suspects
}

func dedupeSuspects(suspects []Suspect) []Suspect {
	const maxEvidencePerSuspect = 4
	index := make(map[string]int, len(suspects))
	out := make([]Suspect, 0, len(suspects))
	for _, suspect := range suspects {
		key := suspect.Kind + "\x00" + suspect.Signal + "\x00" + suspect.Key
		if existing, ok := index[key]; ok {
			if suspect.Score > out[existing].Score {
				out[existing].Score = suspect.Score
			}
			remaining := maxEvidencePerSuspect - len(out[existing].Evidence)
			if remaining > len(suspect.Evidence) {
				remaining = len(suspect.Evidence)
			}
			if remaining > 0 {
				out[existing].Evidence = append(out[existing].Evidence, suspect.Evidence[:remaining]...)
			}
			continue
		}
		index[key] = len(out)
		if len(suspect.Evidence) > maxEvidencePerSuspect {
			suspect.Evidence = suspect.Evidence[:maxEvidencePerSuspect]
		}
		out = append(out, suspect)
	}
	return out
}

func saturatingNonnegativeAdd(current, value int64) int64 {
	if value <= 0 {
		return current
	}
	if current > math.MaxInt64-value {
		return math.MaxInt64
	}
	return current + value
}

func operationMap(operations []Operation) map[string]Operation {
	out := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		out[operation.Key] = operation
	}
	return out
}
