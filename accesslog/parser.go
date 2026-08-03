// Package accesslog parses and aggregates explicitly configured nginx access
// logs. Collection is pull based, so it adds no work to an application's HTTP
// request path.
package accesslog

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Record is one parsed nginx isutools LTSV record.
type Record struct {
	Method string `json:"method"`
	URI    string `json:"uri"`
	Status int    `json:"status"`

	RequestTime time.Duration `json:"request_time_ns"`
	Bytes       int64         `json:"bytes"`
	CacheStatus string        `json:"cache_status"`
	ContentType string        `json:"content_type"`

	UpstreamRaw      string        `json:"upstream_raw"`
	UpstreamTotal    time.Duration `json:"upstream_total_ns"`
	UpstreamAttempts int           `json:"upstream_attempts"`
	UpstreamValid    bool          `json:"upstream_valid"`
	UpstreamComplete bool          `json:"upstream_complete"`
	NoUpstreamTiming bool          `json:"no_upstream_timing"`

	QueryStripped bool   `json:"query_stripped,omitempty"`
	Partial       bool   `json:"partial,omitempty"`
	Issue         string `json:"issue,omitempty"`
}

// ParseNginxLTSV parses the explicit nginx "isutools" LTSV format documented
// in DESIGN.md. Invalid optional upstream timing preserves the useful request
// fields and marks the record partial; invalid required fields return an error.
func ParseNginxLTSV(line string) (Record, error) {
	fields := make(map[string]string, 9)
	for _, part := range strings.Split(strings.TrimSuffix(line, "\r"), "\t") {
		name, value, ok := strings.Cut(part, ":")
		if !ok || name == "" {
			return Record{}, fmt.Errorf("accesslog: malformed LTSV field %q", part)
		}
		if _, duplicate := fields[name]; duplicate {
			return Record{}, fmt.Errorf("accesslog: duplicate LTSV field %q", name)
		}
		decoded, err := decodeJSONEscaped(value)
		if err != nil {
			return Record{}, fmt.Errorf("accesslog: decode %s: %w", name, err)
		}
		fields[name] = decoded
	}

	for _, name := range []string{"method", "uri", "status", "reqtime", "upstime", "bytes", "cache", "ctype"} {
		if _, ok := fields[name]; !ok {
			return Record{}, fmt.Errorf("accesslog: required LTSV field %q is missing", name)
		}
	}
	if fields["method"] == "" || fields["method"] == "-" {
		return Record{}, fmt.Errorf("accesslog: invalid method %q", fields["method"])
	}
	if fields["uri"] == "" || fields["uri"] == "-" {
		return Record{}, fmt.Errorf("accesslog: invalid uri %q", fields["uri"])
	}

	status, err := strconv.Atoi(fields["status"])
	if err != nil || status < 100 || status > 599 {
		return Record{}, fmt.Errorf("accesslog: invalid status %q", fields["status"])
	}
	reqtime, err := parseSeconds(fields["reqtime"])
	if err != nil {
		return Record{}, fmt.Errorf("accesslog: invalid reqtime %q: %w", fields["reqtime"], err)
	}
	bytes, err := strconv.ParseInt(fields["bytes"], 10, 64)
	if err != nil || bytes < 0 {
		return Record{}, fmt.Errorf("accesslog: invalid bytes %q", fields["bytes"])
	}

	uri := fields["uri"]
	cleanURI, _, stripped := strings.Cut(uri, "?")
	rec := Record{
		Method:        fields["method"],
		URI:           cleanURI,
		Status:        status,
		RequestTime:   reqtime,
		Bytes:         bytes,
		CacheStatus:   fields["cache"],
		ContentType:   fields["ctype"],
		UpstreamRaw:   fields["upstime"],
		QueryStripped: stripped,
	}
	if stripped {
		rec.Partial = true
		rec.Issue = "uri contained a query string; query was stripped"
	}

	total, attempts, complete, noTiming, err := parseUpstream(fields["upstime"])
	rec.UpstreamTotal = total
	rec.UpstreamAttempts = attempts
	rec.UpstreamComplete = complete
	rec.NoUpstreamTiming = noTiming
	rec.UpstreamValid = err == nil
	if err != nil {
		rec.Partial = true
		if rec.Issue == "" {
			rec.Issue = err.Error()
		} else {
			rec.Issue += "; " + err.Error()
		}
	}
	return rec, nil
}

func parseUpstream(raw string) (total time.Duration, attempts int, complete, noTiming bool, err error) {
	if strings.TrimSpace(raw) == "-" {
		return 0, 0, false, true, nil
	}
	complete = true
	missing := 0
	for _, token := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ':' }) {
		value := strings.TrimSpace(token)
		if value == "" {
			continue
		}
		if value == "-" {
			complete = false
			missing++
			continue
		}
		d, parseErr := parseSeconds(value)
		if parseErr != nil {
			return total, attempts, false, false, fmt.Errorf("invalid upstream timing %q", value)
		}
		total += d
		attempts++
	}
	if attempts == 0 && missing > 0 {
		return 0, 0, false, true, nil
	}
	if attempts == 0 {
		return 0, 0, false, false, fmt.Errorf("upstream timing %q has no values", raw)
	}
	return total, attempts, complete, false, nil
}

func parseSeconds(value string) (time.Duration, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, fmt.Errorf("must be a non-negative number of seconds")
	}
	// Duration(float64) truncates sub-nanosecond values, which matches nginx's
	// millisecond precision while avoiding a second parse implementation.
	return time.Duration(seconds * float64(time.Second)), nil
}

func decodeJSONEscaped(value string) (string, error) {
	// nginx escape=json produces JSON string escapes but not surrounding quotes.
	quoted := `"` + value + `"`
	decoded, err := strconv.Unquote(quoted)
	if err != nil {
		return "", err
	}
	return decoded, nil
}
