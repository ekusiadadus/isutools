package accessinspect

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"sort"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
)

const (
	ReportSchema         = "isutools.accesslog-inspect/v1"
	DefaultMaxInputBytes = int64(1 << 30)
	DefaultMaxLineBytes  = 1 << 20
	DefaultMaxRecords    = int64(1_000_000)
	DefaultMaxKeys       = 10_000
	HardMaxInputBytes    = int64(4 << 30)
	HardMaxLineBytes     = 8 << 20
	HardMaxRecords       = int64(5_000_000)
	HardMaxKeys          = 100_000
	overflowPath         = "(other)"
)

type LimitError struct{ Code string }

func (e *LimitError) Error() string { return "accessinspect: " + e.Code }

type Options struct {
	Context       context.Context
	Format        accesslog.Format
	Filter        *Filter
	PathRules     *accesslog.PathRules
	Percentiles   []float64
	MaxInputBytes int64
	MaxLineBytes  int
	MaxRecords    int64
	MaxKeys       int
}

type Health struct {
	Lines         int64 `json:"lines"`
	Parsed        int64 `json:"parsed"`
	Filtered      int64 `json:"filtered"`
	Skipped       int64 `json:"skipped"`
	Malformed     int64 `json:"malformed"`
	Partial       int64 `json:"partial"`
	QueryStripped int64 `json:"query_stripped"`
	InputBytes    int64 `json:"input_bytes"`
	OverflowKeys  int64 `json:"overflow_keys"`
}

type Percentile struct {
	Percent float64       `json:"percent"`
	Value   time.Duration `json:"value_ns"`
}

type DurationStats struct {
	Count       int64         `json:"count"`
	Min         time.Duration `json:"min_ns"`
	Max         time.Duration `json:"max_ns"`
	Sum         time.Duration `json:"sum_ns"`
	Avg         time.Duration `json:"avg_ns"`
	Stddev      time.Duration `json:"stddev_ns"`
	Percentiles []Percentile  `json:"percentiles,omitempty"`
}

type ByteStats struct {
	Count int64 `json:"count"`
	Min   int64 `json:"min"`
	Max   int64 `json:"max"`
	Sum   int64 `json:"sum"`
	Avg   int64 `json:"avg"`
}

type Dimension struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

type Row struct {
	Method        string        `json:"method"`
	Path          string        `json:"path"`
	Count         int64         `json:"count"`
	StatusClasses [5]int64      `json:"status_classes"`
	Request       DurationStats `json:"request"`
	Body          ByteStats     `json:"body"`
	Upstream      DurationStats `json:"upstream"`
	Residual      DurationStats `json:"residual"`
	NoUpstream    int64         `json:"no_upstream"`
	CacheStatuses []Dimension   `json:"cache_statuses,omitempty"`
	ContentTypes  []Dimension   `json:"content_types,omitempty"`
	Protocols     []Dimension   `json:"protocols,omitempty"`
}

type Report struct {
	Schema      string    `json:"schema"`
	Format      string    `json:"format,omitempty"`
	Percentiles []float64 `json:"percentiles"`
	Health      Health    `json:"health"`
	Rows        []Row     `json:"rows"`
}

type durationAccumulator struct {
	count  int64
	min    int64
	max    int64
	sum    int64
	mean   float64
	m2     float64
	values []int64
}

func (a *durationAccumulator) add(value int64) error {
	if value < 0 || value > 0 && a.sum > math.MaxInt64-value {
		return &LimitError{Code: "duration-overflow"}
	}
	a.count++
	a.sum += value
	if a.count == 1 || value < a.min {
		a.min = value
	}
	if value > a.max {
		a.max = value
	}
	delta := float64(value) - a.mean
	a.mean += delta / float64(a.count)
	a.m2 += delta * (float64(value) - a.mean)
	a.values = append(a.values, value)
	return nil
}

func (a *durationAccumulator) finish(percentiles []float64) DurationStats {
	if a.count == 0 {
		return DurationStats{}
	}
	values := append([]int64(nil), a.values...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	result := DurationStats{
		Count: a.count, Min: time.Duration(a.min), Max: time.Duration(a.max), Sum: time.Duration(a.sum),
		Avg: time.Duration(a.sum / a.count), Stddev: time.Duration(math.Sqrt(a.m2 / float64(a.count))),
		Percentiles: make([]Percentile, 0, len(percentiles)),
	}
	for _, percentile := range percentiles {
		index := int(math.Ceil(percentile/100*float64(len(values)))) - 1
		if index < 0 {
			index = 0
		}
		if index >= len(values) {
			index = len(values) - 1
		}
		result.Percentiles = append(result.Percentiles, Percentile{Percent: percentile, Value: time.Duration(values[index])})
	}
	return result
}

type byteAccumulator struct {
	count int64
	min   int64
	max   int64
	sum   int64
}

func (a *byteAccumulator) add(value int64) error {
	if value < 0 || value > 0 && a.sum > math.MaxInt64-value {
		return &LimitError{Code: "bytes-overflow"}
	}
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

func (a byteAccumulator) finish() ByteStats {
	if a.count == 0 {
		return ByteStats{}
	}
	return ByteStats{Count: a.count, Min: a.min, Max: a.max, Sum: a.sum, Avg: a.sum / a.count}
}

type rowAccumulator struct {
	method, path string
	count        int64
	status       [5]int64
	request      durationAccumulator
	body         byteAccumulator
	upstream     durationAccumulator
	residual     durationAccumulator
	noUpstream   int64
	cache        map[string]int64
	contentType  map[string]int64
	protocol     map[string]int64
}

func (a *rowAccumulator) observe(rec accesslog.Record) error {
	a.count++
	if class := rec.Status / 100; class >= 1 && class <= 5 {
		a.status[class-1]++
	}
	if err := a.body.add(rec.Bytes); err != nil {
		return err
	}
	addDimension(a.cache, rec.CacheStatus)
	addDimension(a.contentType, rec.ContentType)
	addDimension(a.protocol, rec.Protocol)
	if rec.Status == 101 {
		return nil
	}
	if err := a.request.add(int64(rec.RequestTime)); err != nil {
		return err
	}
	if rec.NoUpstreamTiming {
		a.noUpstream++
	}
	if rec.UpstreamValid {
		if err := a.upstream.add(int64(rec.UpstreamTotal)); err != nil {
			return err
		}
	}
	if rec.UpstreamValid && rec.UpstreamComplete && rec.RequestTime >= rec.UpstreamTotal {
		if err := a.residual.add(int64(rec.RequestTime - rec.UpstreamTotal)); err != nil {
			return err
		}
	}
	return nil
}

// Inspect parses a file or stdin stream and returns an exact, bounded report.
func Inspect(input io.Reader, options Options) (Report, error) {
	options = defaultOptions(options)
	if err := validateOptions(options); err != nil {
		return Report{}, err
	}
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	percentiles := append([]float64(nil), options.Percentiles...)
	if len(percentiles) == 0 {
		percentiles = []float64{50, 90, 95, 99}
	}
	sort.Float64s(percentiles)
	report := Report{Schema: ReportSchema, Format: options.Format.String(), Percentiles: percentiles}
	rows := make(map[string]*rowAccumulator)
	reader := bufio.NewReaderSize(input, options.MaxLineBytes+1)
	for {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return Report{}, &LimitError{Code: "line-too-large"}
		}
		if len(line) > 0 {
			report.Health.InputBytes += int64(len(line))
			if report.Health.InputBytes > options.MaxInputBytes {
				return Report{}, &LimitError{Code: "input-too-large"}
			}
			if len(line) > options.MaxLineBytes && line[len(line)-1] != '\n' {
				return Report{}, &LimitError{Code: "line-too-large"}
			}
			report.Health.Lines++
			line = trimLineEnd(line)
			rec, parseErr := accesslog.ParseLineFormat(options.Format, string(line))
			switch {
			case errors.Is(parseErr, accesslog.ErrSkipLine):
				report.Health.Skipped++
			case parseErr != nil:
				report.Health.Malformed++
			default:
				report.Health.Parsed++
				if report.Health.Parsed > options.MaxRecords {
					return Report{}, &LimitError{Code: "records-limit"}
				}
				if rec.Partial {
					report.Health.Partial++
				}
				if rec.QueryStripped {
					report.Health.QueryStripped++
				}
				rec.URI = options.PathRules.Normalize(rec.URI)
				if !options.Filter.Match(rec) {
					break
				}
				report.Health.Filtered++
				key := rec.Method + "\x00" + rec.URI
				row := rows[key]
				if row == nil {
					// Reserve one bounded bucket for every key beyond the
					// explicit cardinality budget. MaxKeys is therefore a hard
					// total, not MaxKeys plus a hidden overflow row.
					if len(rows) >= options.MaxKeys-1 {
						key = "\x00" + overflowPath
						row = rows[key]
						report.Health.OverflowKeys++
					}
					if row == nil {
						method, path := rec.Method, rec.URI
						if key == "\x00"+overflowPath {
							method, path = "", overflowPath
						}
						row = &rowAccumulator{method: method, path: path, cache: map[string]int64{}, contentType: map[string]int64{}, protocol: map[string]int64{}}
						rows[key] = row
					}
				}
				if observeErr := row.observe(rec); observeErr != nil {
					return Report{}, observeErr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Report{}, &LimitError{Code: "input-read-failed"}
		}
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		row := rows[key]
		report.Rows = append(report.Rows, Row{
			Method: row.method, Path: row.path, Count: row.count, StatusClasses: row.status,
			Request: row.request.finish(percentiles), Body: row.body.finish(),
			Upstream: row.upstream.finish(percentiles), Residual: row.residual.finish(percentiles), NoUpstream: row.noUpstream,
			CacheStatuses: dimensions(row.cache), ContentTypes: dimensions(row.contentType), Protocols: dimensions(row.protocol),
		})
	}
	return report, nil
}

func defaultOptions(options Options) Options {
	if options.MaxInputBytes == 0 {
		options.MaxInputBytes = DefaultMaxInputBytes
	}
	if options.MaxLineBytes == 0 {
		options.MaxLineBytes = DefaultMaxLineBytes
	}
	if options.MaxRecords == 0 {
		options.MaxRecords = DefaultMaxRecords
	}
	if options.MaxKeys == 0 {
		options.MaxKeys = DefaultMaxKeys
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
	if options.MaxRecords < 1 || options.MaxRecords > HardMaxRecords {
		return &LimitError{Code: "invalid-record-limit"}
	}
	if options.MaxKeys < 1 || options.MaxKeys > HardMaxKeys {
		return &LimitError{Code: "invalid-key-limit"}
	}
	for _, percentile := range options.Percentiles {
		if percentile <= 0 || percentile > 100 || math.IsNaN(percentile) || math.IsInf(percentile, 0) {
			return &LimitError{Code: "invalid-percentile"}
		}
	}
	if len(options.Percentiles) > 16 {
		return &LimitError{Code: "too-many-percentiles"}
	}
	return nil
}

func trimLineEnd(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

func addDimension(values map[string]int64, value string) {
	if value != "" && value != "-" {
		values[value]++
	}
}

func dimensions(values map[string]int64) []Dimension {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	result := make([]Dimension, 0, len(keys))
	for _, value := range keys {
		result = append(result, Dimension{Value: value, Count: values[value]})
	}
	return result
}
