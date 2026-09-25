package web

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/multihost"
	"github.com/ekusiadadus/isutools/procstats"
)

func peerWithProc(t *testing.T, name, role string, proc procstats.Snapshot) multihost.PeerResult {
	t.Helper()
	raw, err := json.Marshal(proc)
	if err != nil {
		t.Fatal(err)
	}
	return multihost.PeerResult{
		Name: name, Info: multihost.PeerInfoDTO{Role: role},
		Local: &multihost.LocalSnapshot{Validity: "valid", Sections: map[string]json.RawMessage{"proc": raw}},
	}
}

func TestHostEvidenceRowsKeepOneCoreProcessSeparateFromWholeHostIdle(t *testing.T) {
	start := time.Date(2026, time.September, 25, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	snapshot := Snapshot{
		Host: &hoststats.Section{Identity: hoststats.Identity{Hostname: "benchmark-1", Role: "benchmark"}},
		Proc: &procstats.Snapshot{
			StartedAt: start, EndedAt: end,
			CPUs:     8,
			CPUTotal: &procstats.CPUTotal{BusyPercent: 27, IdlePercent: 73},
			TopCPU:   []procstats.Process{{PID: 42, Command: "bench", CPUPercent: 100}},
		},
		Peers: []multihost.PeerResult{
			peerWithProc(t, "app-a", "app", procstats.Snapshot{
				StartedAt: start, EndedAt: end,
				CPUs: 4, CPUTotal: &procstats.CPUTotal{BusyPercent: 80, IdlePercent: 20},
				TopCPU: []procstats.Process{{PID: 7, Command: "server", CPUPercent: 190}},
			}),
			peerWithProc(t, "db-a", "db", procstats.Snapshot{
				CPUs: 2, CPUTotal: &procstats.CPUTotal{BusyPercent: 0, IdlePercent: 100},
				TopCPU: []procstats.Process{{PID: 8, Command: "mysqld", CPUPercent: 0}},
			}),
		},
	}
	rows := hostEvidenceRows(snapshot)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	if got := rows[0]; got.Name != "benchmark-1" || got.Role != "benchmark" || got.Status != "measured" || got.HostBusy != "27.0%" || got.HostIdle != "73.0%" || got.TopCPU != "100.0%" || got.CoreCapacity != "8" || got.Note != "proc interval 2026-09-25T10:00:00Z → 2026-09-25T10:01:00Z" {
		t.Errorf("benchmark row = %+v", got)
	}
	if got := rows[1]; got.Name != "app-a" || got.Role != "app" || got.HostBusy != "80.0%" || got.HostIdle != "20.0%" || got.TopCPU != "190.0%" || got.TopProcess != "server (PID 7)" || got.CoreCapacity != "4" || got.Note != rows[0].Note {
		t.Errorf("app row = %+v", got)
	}
	if got := rows[2]; got.Name != "db-a" || got.Role != "db" || got.HostBusy != "0.0%" || got.HostIdle != "100.0%" || got.TopCPU != "0.0%" || got.Status != "measured" {
		t.Errorf("idle db row = %+v", got)
	}
}

func TestHostEvidenceRowsKeepMissingPeerSeparateFromObservedIdle(t *testing.T) {
	snapshot := Snapshot{Peers: []multihost.PeerResult{
		{Name: "spare-a", Info: multihost.PeerInfoDTO{Role: "app"}, Failure: &multihost.ParticipantFailureDTO{Phase: "preflight", Code: "connection-refused"}},
		{Name: "spare-b", Info: multihost.PeerInfoDTO{Role: "db"}, Local: &multihost.LocalSnapshot{Validity: "partial", Sections: map[string]json.RawMessage{}}},
		peerWithProc(t, "spare-c", "app", procstats.Snapshot{CPUs: 4, CPUTotal: &procstats.CPUTotal{BusyPercent: 0, IdlePercent: 100}}),
	}}
	rows := hostEvidenceRows(snapshot)
	if len(rows) != 4 {
		t.Fatalf("rows = %+v", rows)
	}
	if got := rows[0]; got.Name != "local" || got.Status != "missing" || got.HostIdle != "-" {
		t.Errorf("local without proc = %+v", got)
	}
	if got := rows[1]; got.Name != "spare-a" || got.Status != "error" || got.HostBusy != "-" || got.HostIdle != "-" || !strings.Contains(got.Note, "connection-refused") {
		t.Errorf("failed peer = %+v", got)
	}
	if got := rows[2]; got.Name != "spare-b" || got.Status != "missing" || got.Validity != "partial" || got.HostBusy != "-" || got.HostIdle != "-" || got.Note != "interval unavailable; proc section missing" {
		t.Errorf("partial peer = %+v", got)
	}
	if got := rows[3]; got.Name != "spare-c" || got.Status != "measured" || got.HostBusy != "0.0%" || got.HostIdle != "100.0%" || got.TopCPU != "-" {
		t.Errorf("observed idle peer = %+v", got)
	}
}

func TestHostEvidenceRowsRejectInvalidCPUValues(t *testing.T) {
	var snapshot Snapshot
	snapshot.Proc = &procstats.Snapshot{
		CPUTotal: &procstats.CPUTotal{BusyPercent: 140, IdlePercent: 0},
		TopCPU:   []procstats.Process{{PID: 1, CPUPercent: math.NaN()}},
	}
	snapshot.Peers = []multihost.PeerResult{
		{Name: "bad-host", Local: &multihost.LocalSnapshot{Sections: map[string]json.RawMessage{"proc": []byte(`{"cpuTotal":{"busyPercent":-1,"idlePercent":100},"topCPU":[{"pid":4,"cpuPercent":-2}]}`)}}},
		peerWithProc(t, "no-counters", "app", procstats.Snapshot{Health: procstats.Health{Status: procstats.StatusUnavailable}}),
	}
	rows := hostEvidenceRows(snapshot)
	if got := rows[0]; got.Status != "error" || got.HostBusy != "-" || got.TopCPU != "-" || !strings.Contains(got.Note, "invalid host CPU percent") || !strings.Contains(got.Note, "invalid process CPU percent") {
		t.Errorf("invalid local row = %+v", got)
	}
	if got := rows[1]; got.Status != "error" || got.HostBusy != "-" || got.TopCPU != "-" || !strings.Contains(got.Note, "invalid host CPU percent") || !strings.Contains(got.Note, "invalid process CPU percent") {
		t.Errorf("invalid peer row = %+v", got)
	}
	if got := rows[2]; got.Status == "measured" || got.HostBusy != "-" || got.TopCPU != "-" || !strings.Contains(got.Note, "proc health unavailable") {
		t.Errorf("unavailable proc row = %+v", got)
	}
}

func TestHostEvidenceRowsBoundPeerCountWithExplicitSummary(t *testing.T) {
	snapshot := Snapshot{Peers: make([]multihost.PeerResult, multihost.MaxPeers+3)}
	for i := range snapshot.Peers {
		snapshot.Peers[i].Name = "peer"
	}
	rows := hostEvidenceRows(snapshot)
	if len(rows) != 1+multihost.MaxPeers+1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if got := rows[len(rows)-1]; got.Status != "truncated" || got.Name != "3 additional peers" || !strings.Contains(got.Note, "protocol limit") {
		t.Errorf("truncation summary = %+v", got)
	}
}

func TestHostEvidenceRowsLocalNameFallsBackToMetaHost(t *testing.T) {
	var snapshot Snapshot
	snapshot.Meta.Host.Hostname = "hub-1"
	snapshot.Meta.Host.NumCPU = 6
	got := hostEvidenceRows(snapshot)[0]
	if got.Name != "hub-1" || got.CoreCapacity != "6" || got.HostBusy != "-" {
		t.Errorf("local fallback = %+v", got)
	}
}

func TestHostEvidenceRowsProcHealthPartialDoesNotHideValues(t *testing.T) {
	snapshot := Snapshot{Peers: []multihost.PeerResult{
		peerWithProc(t, "partial-proc", "app", procstats.Snapshot{
			CPUTotal: &procstats.CPUTotal{BusyPercent: 0, IdlePercent: 100},
			Health:   procstats.Health{Status: procstats.StatusPartial},
		}),
	}}
	got := hostEvidenceRows(snapshot)[1]
	if got.Status != "partial" || got.Validity != "valid" || got.HostBusy != "0.0%" || got.HostIdle != "100.0%" {
		t.Errorf("partial collector row = %+v", got)
	}
}

func TestHostEvidenceRowsMalformedAndOversizePeerSections(t *testing.T) {
	snapshot := Snapshot{Peers: []multihost.PeerResult{
		{Name: "bad-json", Local: &multihost.LocalSnapshot{Validity: "partial", Sections: map[string]json.RawMessage{"proc": []byte(`{"cpuTotal":`)}}},
		{Name: "null-json", Local: &multihost.LocalSnapshot{Sections: map[string]json.RawMessage{"proc": []byte(`null`)}}},
		{Name: "too-big", Local: &multihost.LocalSnapshot{Sections: map[string]json.RawMessage{"proc": []byte(`"` + strings.Repeat("x", maxHostEvidenceSectionBytes) + `"`)}}},
		{Name: "field-absent", Local: &multihost.LocalSnapshot{Sections: map[string]json.RawMessage{"proc": []byte(`{"cpuTotal":{"idlePercent":0},"cpus":4}`)}}},
	}}
	rows := hostEvidenceRows(snapshot)
	if len(rows) != 5 {
		t.Fatalf("rows = %d", len(rows))
	}
	for _, row := range rows[1:4] {
		if row.Status != "error" || row.HostBusy != "-" || row.HostIdle != "-" {
			t.Errorf("malformed/oversize row = %+v", row)
		}
	}
	if got := rows[4]; got.Status != "measured" || got.HostBusy != "-" || got.HostIdle != "0.0%" || got.CoreCapacity != "4" {
		t.Errorf("presence-aware row = %+v", got)
	}
}
