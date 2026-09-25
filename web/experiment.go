package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ExperimentMetadata records operator-supplied conditions, not inferred facts.
// Never include credentials, session tokens, or private benchmark inputs.
type ExperimentMetadata struct {
	Label          string            `json:"label,omitempty"`
	Change         string            `json:"change,omitempty"`
	Conditions     string            `json:"conditions,omitempty"`
	LoadParameters map[string]string `json:"load_parameters,omitempty"`
	Penalty        *float64          `json:"penalty,omitempty"`
	Throughput     *float64          `json:"throughput,omitempty"`
	P95MS          *float64          `json:"p95_ms,omitempty"`
	ErrorRate      *float64          `json:"error_rate,omitempty"`
	Restarted      *bool             `json:"restarted,omitempty"`
	Hosts          []ExperimentHost  `json:"hosts,omitempty"`
}

// ExperimentHost declares the intended topology and participation for a run.
// Names should match the local hostname or configured peer names.
type ExperimentHost struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	Participation string `json:"participation"`
}

func parseExperiment(w http.ResponseWriter, r *http.Request) (*ExperimentMetadata, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, nil
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, errors.New("experiment body requires application/json")
	}
	var body struct {
		Experiment *ExperimentMetadata `json:"experiment"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return nil, errors.New("invalid experiment JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing experiment JSON")
	}
	if body.Experiment != nil {
		if err := body.Experiment.validate(); err != nil {
			return nil, err
		}
	}
	return body.Experiment, nil
}

func (e *ExperimentMetadata) validate() error {
	if len(e.Label) > 128 || len(e.Change) > 512 || len(e.Conditions) > 256 || len(e.LoadParameters) > 24 || len(e.Hosts) > 16 {
		return errors.New("experiment metadata exceeds limit")
	}
	for _, value := range []*float64{e.Penalty, e.Throughput, e.P95MS, e.ErrorRate} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
			return errors.New("invalid experiment metric")
		}
	}
	if e.ErrorRate != nil && *e.ErrorRate > 1 {
		return errors.New("invalid error rate")
	}
	for key, value := range e.LoadParameters {
		if strings.TrimSpace(key) == "" || len(key) > 64 || len(value) > 256 {
			return errors.New("invalid load parameter")
		}
	}
	names := map[string]bool{}
	for _, host := range e.Hosts {
		if strings.TrimSpace(host.Name) == "" || len(host.Name) > 128 || names[host.Name] {
			return errors.New("invalid host name")
		}
		names[host.Name] = true
		switch host.Role {
		case "app", "db", "benchmark", "proxy", "other":
		default:
			return errors.New("invalid host role")
		}
		switch host.Participation {
		case "active", "standby", "unused", "unknown":
		default:
			return errors.New("invalid host participation")
		}
	}
	return nil
}

type experimentEvidenceRow struct{ Metric, Value string }
type experimentComparisonRow struct{ Metric, A, B string }

func experimentEvidenceRows(s Snapshot) []experimentEvidenceRow {
	unknown := func(v string) string {
		if v == "" {
			return "未記録"
		}
		return v
	}
	pass := "未記録"
	if s.Meta.BenchmarkPass != nil {
		pass = strconv.FormatBool(*s.Meta.BenchmarkPass)
	}
	rows := []experimentEvidenceRow{
		{"score", unknown(s.Meta.Score)}, {"pass", pass}, {"revision", unknown(s.Meta.Revision)},
		{"記録時刻", unknown(s.Meta.Time)}, {"partial snapshot", strconv.FormatBool(s.Meta.Partial)},
	}
	validity := "未記録"
	if s.Meta.Run != nil {
		validity = unknown(s.Meta.Run.Validity)
	}
	rows = append(rows, experimentEvidenceRow{"run validity", validity})
	e := s.Meta.Experiment
	if e == nil || e.validate() != nil {
		e = &ExperimentMetadata{}
	}
	penalty, restarted := "未記録", "未確認"
	if e.Penalty != nil {
		penalty = strconv.FormatFloat(*e.Penalty, 'f', -1, 64)
	}
	if e.Restarted != nil {
		restarted = strconv.FormatBool(*e.Restarted)
	}
	rows = append(rows, experimentEvidenceRow{"penalty (申告)", penalty}, experimentEvidenceRow{"再起動後の走行 (申告)", restarted},
		experimentEvidenceRow{"実験名", unknown(e.Label)}, experimentEvidenceRow{"変更内容", unknown(e.Change)},
		experimentEvidenceRow{"比較条件ID", unknown(e.Conditions)})
	for _, metric := range []struct {
		name  string
		value *float64
	}{{"throughput (req/s・申告)", e.Throughput}, {"p95 (ms・申告)", e.P95MS}, {"error rate (申告)", e.ErrorRate}} {
		value := "未記録"
		if metric.value != nil {
			value = strconv.FormatFloat(*metric.value, 'f', -1, 64)
		}
		rows = append(rows, experimentEvidenceRow{metric.name, value})
	}
	keys := make([]string, 0, len(e.LoadParameters))
	for key := range e.LoadParameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		rows = append(rows, experimentEvidenceRow{"負荷パラメータ", "未記録"})
	}
	for _, key := range keys {
		rows = append(rows, experimentEvidenceRow{"負荷: " + key, e.LoadParameters[key]})
	}
	var sql, requests, attributed, tracked int64
	for _, entry := range s.SQL {
		sql += entry.Count
	}
	for _, entry := range s.HTTP {
		requests += entry.Count
		attributed += entry.SQLCount
		tracked += entry.SQLTrackedRequests
	}
	sqlText, httpText, attributedText := "未計測", "未計測", "未計測"
	if s.SQL != nil {
		sqlText = strconv.FormatInt(sql, 10)
	}
	if s.HTTP != nil {
		httpText = fmt.Sprintf("%d / %d", requests, tracked)
	}
	if tracked > 0 {
		attributedText = strconv.FormatInt(attributed, 10)
	}
	rows = append(rows, experimentEvidenceRow{"SQL発行回数 (SQL集計)", sqlText},
		experimentEvidenceRow{"HTTP件数 / Context計測対象HTTP", httpText},
		experimentEvidenceRow{"HTTPに紐付いたSQL回数", attributedText})
	declared := map[string]ExperimentHost{}
	for _, host := range e.Hosts {
		declared[host.Name] = host
	}
	for _, host := range hostEvidenceRows(s) {
		role, participation := host.Role, "未申告"
		if d, ok := declared[host.Name]; ok {
			role, participation = d.Role+" (申告)", d.Participation+" (申告)"
			delete(declared, host.Name)
		}
		rows = append(rows, experimentEvidenceRow{"host: " + host.Name,
			fmt.Sprintf("role %s; participation %s; host busy %s / idle %s; process %s %s; cores %s; %s / %s; %s", role, participation, host.HostBusy, host.HostIdle, host.TopProcess, host.TopCPU, host.CoreCapacity, host.Status, host.Validity, host.Note)})
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		host := declared[name]
		rows = append(rows, experimentEvidenceRow{"host: " + name, fmt.Sprintf("role %s; participation %s (申告); CPU未計測", host.Role, host.Participation)})
	}
	return rows
}

func experimentComparison(a, b Snapshot) []experimentComparisonRow {
	var rows []experimentComparisonRow
	positions := map[string]int{}
	for _, row := range experimentEvidenceRows(a) {
		positions[row.Metric] = len(rows)
		rows = append(rows, experimentComparisonRow{row.Metric, row.Value, "未記録"})
	}
	for _, row := range experimentEvidenceRows(b) {
		if pos, ok := positions[row.Metric]; ok {
			rows[pos].B = row.Value
		} else {
			rows = append(rows, experimentComparisonRow{row.Metric, "未記録", row.Value})
		}
	}
	return rows
}

func experimentComparisonWarning(a, b *ExperimentMetadata) string {
	if a == nil || b == nil || a.validate() != nil || b.validate() != nil || strings.TrimSpace(a.Conditions) == "" || strings.TrimSpace(b.Conditions) == "" {
		return "比較条件が未記録です。同条件の比較か確認できません。"
	}
	hosts := func(e *ExperimentMetadata) map[string]ExperimentHost {
		out := map[string]ExperimentHost{}
		for _, host := range e.Hosts {
			out[host.Name] = host
		}
		return out
	}
	if a.Conditions != b.Conditions || !reflect.DeepEqual(a.LoadParameters, b.LoadParameters) || !reflect.DeepEqual(hosts(a), hosts(b)) || !reflect.DeepEqual(a.Restarted, b.Restarted) {
		return "記録された負荷条件・ホスト構成・再起動状態が異なります。変更単独の効果とは判定できません。"
	}
	return "記録された比較条件は一致しています。入力データ・実行環境の同一性や効果は未検証です。反復走行で確認してください。"
}
