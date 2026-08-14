package accesslog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// Format names an access-log wire contract. Explicit formats are preferred:
// they make product-specific duration units impossible to confuse.
type Format string

const (
	FormatAuto         Format = "auto"
	FormatIsutoolsLTSV Format = "isutools-ltsv"
	FormatIsutoolsJSON Format = "isutools-json-v1"
	FormatCaddyJSON    Format = "caddy-json"
	FormatTraefikJSON  Format = "traefik-json"
	FormatIISW3C       Format = "iis-w3c"
)

// ErrSkipLine is returned for comments and format headers that are not
// request records. Collector treats it as neither malformed nor dropped.
var ErrSkipLine = errors.New("accesslog: non-record line")

// ParseFormat validates a configured decoder name. A few compatibility
// aliases are accepted, and ParseFormat returns the canonical name.
func ParseFormat(value string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return FormatAuto, nil
	case "ltsv", "nginx-ltsv", string(FormatIsutoolsLTSV):
		return FormatIsutoolsLTSV, nil
	case "json", "isutools-json", "canonical-json", string(FormatIsutoolsJSON):
		return FormatIsutoolsJSON, nil
	case "caddy", string(FormatCaddyJSON):
		return FormatCaddyJSON, nil
	case "traefik", string(FormatTraefikJSON):
		return FormatTraefikJSON, nil
	case "iis", "w3c", string(FormatIISW3C):
		return FormatIISW3C, nil
	default:
		return "", fmt.Errorf("accesslog: unsupported format %q", value)
	}
}

func (f Format) String() string { return string(f) }

// ParseLineFormat decodes one record using an explicit wire contract.
func ParseLineFormat(format Format, line string) (Record, error) {
	switch format {
	case "", FormatAuto:
		return ParseLine(line)
	case FormatIsutoolsLTSV:
		return ParseNginxLTSV(line)
	case FormatIsutoolsJSON:
		return parseIsutoolsJSON(line)
	case FormatCaddyJSON:
		return parseCaddyJSON(line)
	case FormatTraefikJSON:
		return parseTraefikJSON(line)
	case FormatIISW3C:
		return parseIISW3C(line)
	default:
		return Record{}, fmt.Errorf("accesslog: unsupported format %q", format)
	}
}

func decodeJSONObject(line string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(line))
	decoder.UseNumber()
	raw := map[string]any{}
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("accesslog: invalid JSON line: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("accesslog: JSON line contains trailing values")
		}
		return nil, fmt.Errorf("accesslog: invalid JSON trailer: %w", err)
	}
	return raw, nil
}

func parseIsutoolsJSON(line string) (Record, error) {
	raw, err := decodeJSONObject(line)
	if err != nil {
		return Record{}, err
	}
	if schema, ok := scalarString(raw["schema"]); ok && schema != "isutools.http-access.v1" {
		return Record{}, fmt.Errorf("accesslog: unsupported schema %q", schema)
	}
	fields := make(map[string]string, len(raw))
	copyScalar(fields, "method", raw["method"])
	copyScalar(fields, "uri", raw["uri"])
	copyScalar(fields, "proto", firstValue(raw, "protocol", "proto", "request_protocol"))
	copyScalar(fields, "status", raw["status"])
	copyScalar(fields, "bytes", firstValue(raw, "bytes", "body_bytes"))
	copyScalar(fields, "cache", firstValue(raw, "cache_status", "cache"))
	copyScalar(fields, "ctype", firstValue(raw, "content_type", "ctype"))
	copyScalar(fields, "sess", raw["sess"])
	copyScalar(fields, "scenario", raw["scenario"])

	duration, err := explicitDuration(raw, []durationField{
		{name: "duration_ns", unit: time.Nanosecond},
		{name: "request_time_ns", unit: time.Nanosecond},
		{name: "duration_us", unit: time.Microsecond},
		{name: "reqtime_us", unit: time.Microsecond},
		{name: "duration_ms", unit: time.Millisecond},
		{name: "duration_sec", unit: time.Second},
		// Backward compatibility for the original isutools/alp JSON contract.
		{name: "reqtime", unit: time.Second},
		{name: "response_time", unit: time.Second},
	})
	if err != nil {
		return Record{}, fmt.Errorf("accesslog: request duration: %w", err)
	}
	fields["reqtime"] = durationSeconds(duration)

	upstream, present, err := optionalExplicitDuration(raw, []durationField{
		{name: "upstream_duration_ns", unit: time.Nanosecond},
		{name: "upstream_duration_us", unit: time.Microsecond},
		{name: "upstream_duration_ms", unit: time.Millisecond},
		{name: "upstream_duration_sec", unit: time.Second},
	})
	if err != nil {
		return Record{}, fmt.Errorf("accesslog: upstream duration: %w", err)
	}
	if legacy, ok := scalarString(raw["upstime"]); ok {
		if present {
			return Record{}, errors.New("accesslog: upstream duration uses conflicting fields")
		}
		fields["upstime"] = legacy
	} else if present {
		fields["upstime"] = durationSeconds(upstream)
	} else {
		fields["upstime"] = "-"
	}
	record, err := requiredRecord(fields)
	if err != nil {
		return Record{}, err
	}
	record.RequestTime = duration
	if present {
		record.UpstreamTotal = upstream
		record.UpstreamAttempts = 1
		record.UpstreamComplete = true
		record.UpstreamValid = true
		record.NoUpstreamTiming = false
	}
	return record, nil
}

func parseCaddyJSON(line string) (Record, error) {
	raw, err := decodeJSONObject(line)
	if err != nil {
		return Record{}, err
	}
	request, ok := raw["request"].(map[string]any)
	if !ok {
		return Record{}, errors.New("accesslog: Caddy request object is missing")
	}
	fields := map[string]string{"upstime": "-", "cache": ""}
	copyScalar(fields, "method", request["method"])
	copyScalar(fields, "uri", request["uri"])
	copyScalar(fields, "proto", request["proto"])
	copyScalar(fields, "status", raw["status"])
	copyScalar(fields, "bytes", raw["size"])
	duration, err := explicitDuration(raw, []durationField{{name: "duration", unit: time.Second}})
	if err != nil {
		return Record{}, fmt.Errorf("accesslog: Caddy duration: %w", err)
	}
	fields["reqtime"] = durationSeconds(duration)
	if headers, ok := raw["resp_headers"].(map[string]any); ok {
		copyHeader(fields, "ctype", headers, "Content-Type")
		// Only application response headers are trusted. Caddy request headers
		// are client-controlled and must never become flow identities.
		copyHeader(fields, "sess", headers, "X-Isutools-Session")
		copyHeader(fields, "scenario", headers, "X-Isutools-Scenario")
	}
	record, err := requiredRecord(fields)
	if err == nil {
		record.RequestTime = duration
	}
	return record, err
}

func parseTraefikJSON(line string) (Record, error) {
	raw, err := decodeJSONObject(line)
	if err != nil {
		return Record{}, err
	}
	fields := map[string]string{"upstime": "-", "cache": "", "ctype": ""}
	copyScalar(fields, "method", raw["RequestMethod"])
	copyScalar(fields, "uri", raw["RequestPath"])
	copyScalar(fields, "proto", raw["RequestProtocol"])
	copyScalar(fields, "status", raw["DownstreamStatus"])
	copyScalar(fields, "bytes", raw["DownstreamContentSize"])
	duration, err := explicitDuration(raw, []durationField{{name: "Duration", unit: time.Nanosecond}})
	if err != nil {
		return Record{}, fmt.Errorf("accesslog: Traefik duration: %w", err)
	}
	fields["reqtime"] = durationSeconds(duration)
	origin, originPresent, err := optionalExplicitDuration(raw, []durationField{{name: "OriginDuration", unit: time.Nanosecond}})
	if err != nil {
		return Record{}, fmt.Errorf("accesslog: Traefik origin duration: %w", err)
	} else if originPresent {
		fields["upstime"] = durationSeconds(origin)
	}
	// Traefik's header customization records public request headers. Flow
	// identity therefore stays in application middleware; no header-shaped
	// field in this native record is trusted.
	record, err := requiredRecord(fields)
	if err != nil {
		return Record{}, err
	}
	record.RequestTime = duration
	if originPresent {
		record.UpstreamTotal = origin
		record.UpstreamAttempts = 1
		record.UpstreamComplete = true
		record.UpstreamValid = true
		record.NoUpstreamTiming = false
	}
	return record, nil
}

// parseIISW3C consumes the fixed field order emitted by examples/proxies/iis-logging.ps1.
// W3C directives/comments are deliberately skipped. cs-uri-query is omitted
// from the configured log so sensitive query values never enter the report.
func parseIISW3C(line string) (Record, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return Record{}, ErrSkipLine
	}
	parts := strings.Fields(line)
	// date time cs-method cs-uri-stem sc-status sc-bytes time-taken cs-version
	if len(parts) != 8 {
		return Record{}, fmt.Errorf("accesslog: IIS W3C record has %d fields, want 8", len(parts))
	}
	fields := map[string]string{
		"method": parts[2], "uri": parts[3], "status": parts[4],
		"bytes": parts[5], "proto": parts[7], "upstime": "-",
		"cache": "", "ctype": "",
	}
	millis, err := strconv.ParseInt(parts[6], 10, 64)
	if err != nil || millis < 0 || millis > math.MaxInt64/int64(time.Millisecond) {
		return Record{}, fmt.Errorf("accesslog: invalid IIS time-taken %q", parts[6])
	}
	fields["reqtime"] = durationSeconds(time.Duration(millis) * time.Millisecond)
	record, err := requiredRecord(fields)
	if err == nil {
		record.RequestTime = time.Duration(millis) * time.Millisecond
	}
	return record, err
}

type durationField struct {
	name string
	unit time.Duration
}

func explicitDuration(raw map[string]any, candidates []durationField) (time.Duration, error) {
	d, present, err := optionalExplicitDuration(raw, candidates)
	if err != nil {
		return 0, err
	}
	if !present {
		names := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			names = append(names, candidate.name)
		}
		return 0, fmt.Errorf("one of %s is required", strings.Join(names, ", "))
	}
	return d, nil
}

func optionalExplicitDuration(raw map[string]any, candidates []durationField) (time.Duration, bool, error) {
	var selected *durationField
	var value any
	for i := range candidates {
		if candidateValue, ok := raw[candidates[i].name]; ok && candidateValue != nil && candidateValue != "" && candidateValue != "-" {
			if selected != nil {
				return 0, false, fmt.Errorf("conflicting fields %s and %s", selected.name, candidates[i].name)
			}
			selected = &candidates[i]
			value = candidateValue
		}
	}
	if selected == nil {
		return 0, false, nil
	}
	number, ok := scalarString(value)
	if !ok {
		return 0, false, fmt.Errorf("%s is not numeric", selected.name)
	}
	if !strings.ContainsAny(number, ".eE") {
		integer, err := strconv.ParseInt(number, 10, 64)
		if err != nil || integer < 0 {
			return 0, false, fmt.Errorf("%s is not a non-negative finite number", selected.name)
		}
		if integer > math.MaxInt64/int64(selected.unit) {
			return 0, false, fmt.Errorf("%s overflows time.Duration", selected.name)
		}
		return time.Duration(integer) * selected.unit, true, nil
	}
	f, err := strconv.ParseFloat(number, 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Errorf("%s is not a non-negative finite number", selected.name)
	}
	if f > float64(math.MaxInt64)/float64(selected.unit) {
		return 0, false, fmt.Errorf("%s overflows time.Duration", selected.name)
	}
	return time.Duration(f * float64(selected.unit)), true, nil
}

func requiredRecord(fields map[string]string) (Record, error) {
	for _, name := range []string{"method", "uri", "status", "reqtime"} {
		if fields[name] == "" {
			return Record{}, fmt.Errorf("accesslog: required field %q is missing", name)
		}
	}
	if fields["bytes"] == "" || fields["bytes"] == "-" {
		fields["bytes"] = "0"
	}
	if fields["upstime"] == "" {
		fields["upstime"] = "-"
	}
	return recordFromFields(fields)
}

func durationSeconds(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Second), 'f', 9, 64)
}

func firstValue(raw map[string]any, names ...string) any {
	for _, name := range names {
		if value, ok := raw[name]; ok {
			return value
		}
	}
	return nil
}

func scalarString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		return "", false
	}
}

func copyScalar(fields map[string]string, key string, value any) {
	if scalar, ok := scalarString(value); ok {
		fields[key] = scalar
	}
}

func copyHeader(fields map[string]string, key string, headers map[string]any, name string) {
	for headerName, raw := range headers {
		if !strings.EqualFold(headerName, name) {
			continue
		}
		switch values := raw.(type) {
		case []any:
			if len(values) > 0 {
				copyScalar(fields, key, values[0])
			}
		case []string:
			if len(values) > 0 {
				fields[key] = values[0]
			}
		default:
			copyScalar(fields, key, raw)
		}
		return
	}
}
