package slowlog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	ReportSchema         = "isutools.mysql-slowlog/v1"
	DefaultMaxInputBytes = int64(256 << 20)
	DefaultMaxLineBytes  = 1 << 20
	DefaultMaxEvents     = int64(1_000_000)
	DefaultMaxClasses    = 10_000
	DefaultMaxQueryBytes = 1 << 20
	HardMaxInputBytes    = int64(1 << 30)
	HardMaxLineBytes     = 8 << 20
	HardMaxEvents        = int64(5_000_000)
	HardMaxClasses       = 100_000
	HardMaxQueryBytes    = 8 << 20
)

var queryMetricsPattern = regexp.MustCompile(`^# Query_time:\s+([0-9.]+)\s+Lock_time:\s+([0-9.]+)\s+Rows_sent:\s+([0-9]+)\s+Rows_examined:\s+([0-9]+)(?:\s+Rows_affected:\s+([0-9]+))?`)

type LimitError struct{ Code string }

func (e *LimitError) Error() string { return "slowlog: " + e.Code }

type Options struct {
	MaxInputBytes int64
	MaxLineBytes  int
	MaxEvents     int64
	MaxClasses    int
	MaxQueryBytes int
	Coverage      Coverage
}

type Health struct {
	Lines         int64 `json:"lines"`
	InputBytes    int64 `json:"input_bytes"`
	Events        int64 `json:"events"`
	Classes       int   `json:"classes"`
	Malformed     int64 `json:"malformed"`
	PartialEvents int64 `json:"partial_events"`
	Overflow      int64 `json:"overflow_classes"`
}

type DurationStats struct {
	Count  int64         `json:"count"`
	Min    time.Duration `json:"min_ns"`
	Max    time.Duration `json:"max_ns"`
	Sum    time.Duration `json:"sum_ns"`
	Avg    time.Duration `json:"avg_ns"`
	Median time.Duration `json:"median_ns"`
	P95    time.Duration `json:"p95_ns"`
	Stddev time.Duration `json:"stddev_ns"`
}

type IntegerStats struct {
	Available bool   `json:"available"`
	Count     int64  `json:"count"`
	Min       uint64 `json:"min"`
	Max       uint64 `json:"max"`
	Sum       uint64 `json:"sum"`
	Avg       uint64 `json:"avg"`
}

type QueryClass struct {
	FingerprintSHA256 string        `json:"fingerprint_sha256"`
	Operation         string        `json:"operation"`
	Count             int64         `json:"count"`
	QueryTime         DurationStats `json:"query_time"`
	LockTime          DurationStats `json:"lock_time"`
	RowsExamined      IntegerStats  `json:"rows_examined"`
	RowsSent          IntegerStats  `json:"rows_sent"`
	RowsAffected      IntegerStats  `json:"rows_affected"`
	FirstEvent        time.Time     `json:"first_event,omitzero"`
	LastEvent         time.Time     `json:"last_event,omitzero"`
	OutlierCount      int64         `json:"outlier_count"`
	SampleAvailable   bool          `json:"sample_available"`
}

type Report struct {
	Schema     string       `json:"schema"`
	Coverage   Coverage     `json:"coverage"`
	Health     Health       `json:"health"`
	Classes    []QueryClass `json:"classes"`
	Capability string       `json:"capability"`
}

type pendingEvent struct {
	at            time.Time
	queryTime     time.Duration
	lockTime      time.Duration
	rowsExamined  uint64
	rowsSent      uint64
	rowsAffected  uint64
	affectedKnown bool
	query         strings.Builder
	metrics       bool
}

type durationAccumulator struct {
	count int64
	min   int64
	max   int64
	sum   int64
	mean  float64
	m2    float64
	all   []int64
}

func (a *durationAccumulator) add(value time.Duration) error {
	ns := int64(value)
	if ns < 0 || ns > 0 && a.sum > math.MaxInt64-ns {
		return &LimitError{Code: "duration-overflow"}
	}
	a.count++
	a.sum += ns
	if a.count == 1 || ns < a.min {
		a.min = ns
	}
	if ns > a.max {
		a.max = ns
	}
	delta := float64(ns) - a.mean
	a.mean += delta / float64(a.count)
	a.m2 += delta * (float64(ns) - a.mean)
	a.all = append(a.all, ns)
	return nil
}

func (a *durationAccumulator) finish() DurationStats {
	if a.count == 0 {
		return DurationStats{}
	}
	all := append([]int64(nil), a.all...)
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	return DurationStats{
		Count: a.count, Min: time.Duration(a.min), Max: time.Duration(a.max), Sum: time.Duration(a.sum), Avg: time.Duration(a.sum / a.count),
		Median: time.Duration(nearestRank(all, 50)), P95: time.Duration(nearestRank(all, 95)), Stddev: time.Duration(math.Sqrt(a.m2 / float64(a.count))),
	}
}

func nearestRank(values []int64, percentile float64) int64 {
	index := int(math.Ceil(percentile/100*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	return values[index]
}

type integerAccumulator struct {
	known bool
	count int64
	min   uint64
	max   uint64
	sum   uint64
}

func (a *integerAccumulator) add(value uint64) error {
	if math.MaxUint64-a.sum < value {
		return &LimitError{Code: "rows-overflow"}
	}
	a.known = true
	a.count++
	a.sum += value
	if a.count == 1 || value < a.min {
		a.min = value
	}
	if value > a.max {
		a.max = value
	}
	return nil
}

func (a integerAccumulator) finish() IntegerStats {
	result := IntegerStats{Available: a.known, Count: a.count, Min: a.min, Max: a.max, Sum: a.sum}
	if a.count > 0 {
		result.Avg = a.sum / uint64(a.count)
	}
	return result
}

type classAccumulator struct {
	hash, operation string
	count           int64
	queryTime       durationAccumulator
	lockTime        durationAccumulator
	rowsExamined    integerAccumulator
	rowsSent        integerAccumulator
	rowsAffected    integerAccumulator
	first, last     time.Time
}

func (a *classAccumulator) add(event pendingEvent) error {
	a.count++
	if err := a.queryTime.add(event.queryTime); err != nil {
		return err
	}
	if err := a.lockTime.add(event.lockTime); err != nil {
		return err
	}
	if err := a.rowsExamined.add(event.rowsExamined); err != nil {
		return err
	}
	if err := a.rowsSent.add(event.rowsSent); err != nil {
		return err
	}
	if event.affectedKnown {
		if err := a.rowsAffected.add(event.rowsAffected); err != nil {
			return err
		}
	}
	if !event.at.IsZero() && (a.first.IsZero() || event.at.Before(a.first)) {
		a.first = event.at
	}
	if event.at.After(a.last) {
		a.last = event.at
	}
	return nil
}

// Parse produces a portable aggregate containing hashes and operations only.
// User/host/database names and SQL text are intentionally discarded.
func Parse(input io.Reader, options Options) (Report, error) {
	options = defaultOptions(options)
	if err := validateOptions(options); err != nil {
		return Report{}, err
	}
	report := Report{Schema: ReportSchema, Coverage: options.Coverage, Capability: "mysql-slowlog-events-no-raw-sql"}
	if report.Coverage.Reason == "" {
		report.Coverage.Complete = true
	}
	classes := make(map[string]*classAccumulator)
	reader := bufio.NewReaderSize(input, options.MaxLineBytes+1)
	pending := pendingEvent{}
	var headerTime time.Time
	finalize := func(complete bool) error {
		if !pending.metrics || pending.query.Len() == 0 {
			pending = pendingEvent{}
			return nil
		}
		report.Health.Events++
		if report.Health.Events > options.MaxEvents {
			return &LimitError{Code: "events-limit"}
		}
		if !complete {
			report.Health.PartialEvents++
			report.Coverage.Complete = false
			report.Coverage.Reason = "partial-event"
		}
		fp := fingerprint(pending.query.String())
		row := classes[fp.hash]
		if row == nil {
			if len(classes) >= options.MaxClasses {
				report.Health.Overflow++
				fp = fingerprintResult{hash: strings.Repeat("f", 64), operation: "OTHER"}
				row = classes[fp.hash]
			}
			if row == nil {
				row = &classAccumulator{hash: fp.hash, operation: fp.operation}
				classes[fp.hash] = row
			}
		}
		if err := row.add(pending); err != nil {
			return err
		}
		pending = pendingEvent{}
		return nil
	}
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return Report{}, &LimitError{Code: "line-too-large"}
		}
		if len(line) > 0 {
			report.Health.InputBytes += int64(len(line))
			if report.Health.InputBytes > options.MaxInputBytes {
				return Report{}, &LimitError{Code: "input-too-large"}
			}
			report.Health.Lines++
			text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			switch {
			case strings.HasPrefix(text, "# Time:"):
				if finalizeErr := finalize(strings.HasSuffix(strings.TrimSpace(pending.query.String()), ";")); finalizeErr != nil {
					return Report{}, finalizeErr
				}
				headerTime = parseSlowTime(strings.TrimSpace(strings.TrimPrefix(text, "# Time:")))
			case strings.HasPrefix(text, "# Query_time:"):
				if pending.metrics {
					if finalizeErr := finalize(strings.HasSuffix(strings.TrimSpace(pending.query.String()), ";")); finalizeErr != nil {
						return Report{}, finalizeErr
					}
				}
				values := queryMetricsPattern.FindStringSubmatch(text)
				if len(values) == 0 {
					report.Health.Malformed++
					break
				}
				pending = pendingEvent{at: headerTime, metrics: true}
				pending.queryTime, _ = secondsDuration(values[1])
				pending.lockTime, _ = secondsDuration(values[2])
				pending.rowsSent, _ = strconv.ParseUint(values[3], 10, 64)
				pending.rowsExamined, _ = strconv.ParseUint(values[4], 10, 64)
				if values[5] != "" {
					pending.rowsAffected, _ = strconv.ParseUint(values[5], 10, 64)
					pending.affectedKnown = true
				}
			case strings.HasPrefix(text, "SET timestamp="):
				if pending.at.IsZero() {
					value := strings.TrimSuffix(strings.TrimPrefix(text, "SET timestamp="), ";")
					if unixSeconds, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil {
						pending.at = time.Unix(unixSeconds, 0).UTC()
					}
				}
			case strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "use "):
				// Database names are not retained in portable output.
			case strings.HasPrefix(text, "#"):
				// User@Host and other potentially sensitive headers are discarded.
			case pending.metrics:
				if pending.query.Len()+len(text)+1 > options.MaxQueryBytes {
					return Report{}, &LimitError{Code: "query-too-large"}
				}
				if pending.query.Len() > 0 {
					pending.query.WriteByte('\n')
				}
				pending.query.WriteString(text)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Report{}, &LimitError{Code: "input-read-failed"}
		}
	}
	if err := finalize(strings.HasSuffix(strings.TrimSpace(pending.query.String()), ";")); err != nil {
		return Report{}, err
	}
	for _, value := range classes {
		queryTime := value.queryTime.finish()
		outliers := int64(0)
		for _, sample := range value.queryTime.all {
			if sample > int64(queryTime.P95) {
				outliers++
			}
		}
		report.Classes = append(report.Classes, QueryClass{
			FingerprintSHA256: value.hash, Operation: value.operation, Count: value.count,
			QueryTime: queryTime, LockTime: value.lockTime.finish(), RowsExamined: value.rowsExamined.finish(),
			RowsSent: value.rowsSent.finish(), RowsAffected: value.rowsAffected.finish(), FirstEvent: value.first.UTC(), LastEvent: value.last.UTC(),
			OutlierCount: outliers, SampleAvailable: false,
		})
	}
	sort.Slice(report.Classes, func(i, j int) bool {
		if report.Classes[i].QueryTime.Sum != report.Classes[j].QueryTime.Sum {
			return report.Classes[i].QueryTime.Sum > report.Classes[j].QueryTime.Sum
		}
		return report.Classes[i].FingerprintSHA256 < report.Classes[j].FingerprintSHA256
	})
	report.Health.Classes = len(report.Classes)
	return report, nil
}

func defaultOptions(options Options) Options {
	if options.MaxInputBytes == 0 {
		options.MaxInputBytes = DefaultMaxInputBytes
	}
	if options.MaxLineBytes == 0 {
		options.MaxLineBytes = DefaultMaxLineBytes
	}
	if options.MaxEvents == 0 {
		options.MaxEvents = DefaultMaxEvents
	}
	if options.MaxClasses == 0 {
		options.MaxClasses = DefaultMaxClasses
	}
	if options.MaxQueryBytes == 0 {
		options.MaxQueryBytes = DefaultMaxQueryBytes
	}
	return options
}

func validateOptions(options Options) error {
	if options.MaxInputBytes < 1 || options.MaxInputBytes > HardMaxInputBytes {
		return &LimitError{Code: "invalid-input-limit"}
	}
	if options.MaxLineBytes < 1 || options.MaxLineBytes > HardMaxLineBytes {
		return &LimitError{Code: "invalid-line-limit"}
	}
	if options.MaxEvents < 1 || options.MaxEvents > HardMaxEvents {
		return &LimitError{Code: "invalid-events-limit"}
	}
	if options.MaxClasses < 1 || options.MaxClasses > HardMaxClasses {
		return &LimitError{Code: "invalid-classes-limit"}
	}
	if options.MaxQueryBytes < 1 || options.MaxQueryBytes > HardMaxQueryBytes {
		return &LimitError{Code: "invalid-query-limit"}
	}
	return nil
}

func secondsDuration(value string) (time.Duration, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds < 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return 0, errors.New("invalid duration")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func parseSlowTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "20060102 15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

type fingerprintResult struct{ hash, operation string }

func fingerprint(query string) fingerprintResult {
	var normalized strings.Builder
	space := false
	for pos := 0; pos < len(query); {
		b := query[pos]
		if unicode.IsSpace(rune(b)) {
			space = true
			pos++
			continue
		}
		if space && normalized.Len() > 0 {
			normalized.WriteByte(' ')
			space = false
		}
		if b == '\'' || b == '"' {
			quote := b
			pos++
			for pos < len(query) {
				if query[pos] == '\\' && pos+1 < len(query) {
					pos += 2
					continue
				}
				if query[pos] == quote {
					pos++
					break
				}
				pos++
			}
			normalized.WriteByte('?')
			continue
		}
		if b >= '0' && b <= '9' {
			for pos < len(query) && ((query[pos] >= '0' && query[pos] <= '9') || strings.ContainsRune(".xXeE+-", rune(query[pos]))) {
				pos++
			}
			normalized.WriteByte('?')
			continue
		}
		if b == '-' && pos+1 < len(query) && query[pos+1] == '-' {
			for pos < len(query) && query[pos] != '\n' {
				pos++
			}
			space = true
			continue
		}
		if b == '/' && pos+1 < len(query) && query[pos+1] == '*' {
			pos += 2
			for pos+1 < len(query) && (query[pos] != '*' || query[pos+1] != '/') {
				pos++
			}
			if pos+1 < len(query) {
				pos += 2
			}
			space = true
			continue
		}
		if b >= 'a' && b <= 'z' {
			normalized.WriteByte(b - ('a' - 'A'))
		} else {
			normalized.WriteByte(b)
		}
		pos++
	}
	canonical := strings.TrimSpace(strings.TrimSuffix(normalized.String(), ";"))
	sum := sha256.Sum256([]byte(canonical))
	operation := "OTHER"
	if fields := strings.Fields(canonical); len(fields) > 0 {
		candidate := strings.Trim(fields[0], "(`")
		switch candidate {
		case "SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "CALL", "COMMIT", "ROLLBACK", "BEGIN":
			operation = candidate
		}
	}
	return fingerprintResult{hash: hex.EncodeToString(sum[:]), operation: operation}
}
