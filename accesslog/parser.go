// Package accesslog parses and aggregates explicitly configured nginx access
// logs. Collection is pull based, so it adds no work to an application's HTTP
// request path.
package accesslog

import (
	"encoding/json"
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
	return recordFromFields(fields)
}

// ParseLine auto-detects the record syntax: JSON object lines (nginx
// log_format with JSON output, isutools or alp-style keys) or isutools LTSV.
func ParseLine(line string) (Record, error) {
	if strings.HasPrefix(strings.TrimSpace(line), "{") {
		return ParseNginxJSON(line)
	}
	return ParseNginxLTSV(line)
}

// jsonAliases maps alp's default JSON key names to isutools field names.
var jsonAliases = map[string]string{
	"body_bytes":    "bytes",
	"response_time": "reqtime",
	"upstream_time": "upstime",
}

// ParseNginxJSON parses one JSON-object access-log line. Both isutools key
// names (method/uri/status/reqtime/upstime/bytes/cache/ctype) and alp's
// defaults (body_bytes/response_time) are accepted. Missing optional fields
// default: bytes 0, upstime "-", empty cache/ctype.
func ParseNginxJSON(line string) (Record, error) {
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return Record{}, fmt.Errorf("accesslog: invalid JSON line: %w", err)
	}
	fields := make(map[string]string, len(raw))
	for key, value := range raw {
		if alias, ok := jsonAliases[key]; ok {
			key = alias
		}
		switch v := value.(type) {
		case string:
			fields[key] = v
		case float64:
			fields[key] = strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			fields[key] = strconv.FormatBool(v)
		}
	}
	for _, name := range []string{"method", "uri", "status", "reqtime"} {
		if _, ok := fields[name]; !ok {
			return Record{}, fmt.Errorf("accesslog: required JSON field %q is missing", name)
		}
	}
	if _, ok := fields["bytes"]; !ok {
		fields["bytes"] = "0"
	}
	if _, ok := fields["upstime"]; !ok {
		fields["upstime"] = "-"
	}
	return recordFromFields(fields)
}

// recordFromFields builds a Record from normalized field values, shared by
// the LTSV and JSON parsers.
func recordFromFields(fields map[string]string) (Record, error) {
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
