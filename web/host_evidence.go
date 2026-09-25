package web

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/multihost"
	"github.com/ekusiadadus/isutools/procstats"
)

// hostEvidenceRow keeps each machine's CPU evidence separate. HostBusy and
// HostIdle are percentages of all logical CPUs; TopCPU is a process percentage
// where 100% means one fully occupied core. A dash means no observation, not
// an observed zero.
type hostEvidenceRow struct {
	Name         string
	Role         string
	Status       string
	Validity     string
	HostBusy     string
	HostIdle     string
	TopProcess   string
	TopCPU       string
	CoreCapacity string
	Note         string
}

const maxHostEvidenceSectionBytes = 1 << 20

// hostEvidenceRows returns the hub's own machine and every configured peer,
// including peers that failed before a snapshot could be fetched. It does not
// infer a machine's workload from its role, name, or idle percentage.
func hostEvidenceRows(snapshot Snapshot) []hostEvidenceRow {
	peers := snapshot.Peers
	if len(peers) > multihost.MaxPeers {
		peers = peers[:multihost.MaxPeers]
	}
	rows := make([]hostEvidenceRow, 0, 2+len(peers))
	local := emptyHostEvidenceRow()
	local.Name = "local"
	if snapshot.Meta.Host.Hostname != "" && snapshot.Meta.Host.Hostname != "unknown" {
		local.Name = snapshot.Meta.Host.Hostname
	}
	if snapshot.Host != nil {
		if snapshot.Host.Identity.Hostname != "" {
			local.Name = snapshot.Host.Identity.Hostname
		}
		local.Role = displayHostEvidence(snapshot.Host.Identity.Role)
	}
	if snapshot.Meta.Run != nil {
		local.Validity = displayHostEvidence(snapshot.Meta.Run.Validity)
	}
	if snapshot.Meta.Host.NumCPU > 0 {
		local.CoreCapacity = fmt.Sprint(snapshot.Meta.Host.NumCPU)
	}
	if snapshot.Proc != nil {
		local.applyProc(snapshot.Proc)
	} else {
		local.Note = appendHostEvidenceNote(local.Note, "proc section missing")
	}
	rows = append(rows, local)

	for _, peer := range peers {
		row := emptyHostEvidenceRow()
		row.Name = displayHostEvidence(peer.Name)
		row.Role = displayHostEvidence(peer.Info.Role)
		if peer.Local != nil {
			row.Validity = displayHostEvidence(peer.Local.Validity)
		} else if peer.Status != nil {
			row.Validity = displayHostEvidence(peer.Status.Validity)
		}
		if peer.Local == nil {
			row.Note = appendHostEvidenceNote(row.Note, "peer snapshot missing")
		} else {
			row.applyPeerProc(peer.Local)
		}
		if peer.Failure != nil {
			row.Status = "error"
			row.Note = appendHostEvidenceNote(row.Note, "peer "+peer.Failure.Phase+" / "+peer.Failure.Code)
		}
		rows = append(rows, row)
	}
	if omitted := len(snapshot.Peers) - len(peers); omitted > 0 {
		row := emptyHostEvidenceRow()
		row.Name = fmt.Sprintf("%d additional peers", omitted)
		row.Status = "truncated"
		row.Note = fmt.Sprintf("peer rows omitted beyond protocol limit of %d", multihost.MaxPeers)
		rows = append(rows, row)
	}
	return rows
}

func emptyHostEvidenceRow() hostEvidenceRow {
	return hostEvidenceRow{Role: "-", Status: "missing", Validity: "-", HostBusy: "-", HostIdle: "-", TopProcess: "-", TopCPU: "-", CoreCapacity: "-", Note: "interval unavailable"}
}

func displayHostEvidence(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func appendHostEvidenceNote(note, addition string) string {
	if note == "" {
		return addition
	}
	return note + "; " + addition
}

func procIntervalNote(start, end time.Time) string {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return "interval unavailable"
	}
	return "proc interval " + start.Format(time.RFC3339Nano) + " → " + end.Format(time.RFC3339Nano)
}

func validHostPercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}

func validProcessPercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func (row *hostEvidenceRow) applyProcHealth(health procstats.Health) {
	if health.Status == procstats.StatusPartial || health.Partial {
		if row.Status != "error" {
			row.Status = "partial"
		}
	} else if health.Status != "" && health.Status != procstats.StatusOK {
		row.Status = "error"
		row.Note = appendHostEvidenceNote(row.Note, "proc health unavailable")
	}
}

func (row *hostEvidenceRow) applyProc(proc *procstats.Snapshot) {
	row.Note = procIntervalNote(proc.StartedAt, proc.EndedAt)
	invalid := false
	if proc.CPUs > 0 {
		row.CoreCapacity = fmt.Sprint(proc.CPUs)
	}
	if proc.CPUTotal != nil {
		if validHostPercent(proc.CPUTotal.BusyPercent) && validHostPercent(proc.CPUTotal.IdlePercent) {
			row.HostBusy = fmt.Sprintf("%.1f%%", proc.CPUTotal.BusyPercent)
			row.HostIdle = fmt.Sprintf("%.1f%%", proc.CPUTotal.IdlePercent)
			row.Status = "measured"
		} else {
			invalid = true
			row.Note = appendHostEvidenceNote(row.Note, "invalid host CPU percent")
		}
	}
	if len(proc.TopCPU) > 0 {
		top := proc.TopCPU[0]
		valid := true
		for _, candidate := range proc.TopCPU {
			if !validProcessPercent(candidate.CPUPercent) {
				valid = false
			}
			if candidate.CPUPercent > top.CPUPercent {
				top = candidate
			}
		}
		if valid {
			row.TopProcess = hostEvidenceProcess(top.Command, top.PID)
			row.TopCPU = fmt.Sprintf("%.1f%%", top.CPUPercent)
			row.Status = "measured"
		} else {
			invalid = true
			row.Note = appendHostEvidenceNote(row.Note, "invalid process CPU percent")
		}
	}
	if row.Status == "missing" {
		row.Note = appendHostEvidenceNote(row.Note, "CPU measurements missing")
	}
	if invalid {
		row.Status = "error"
	}
	row.applyProcHealth(proc.Health)
}

func hostEvidenceProcess(command string, pid int) string {
	// Process names originate on the host and can be arbitrarily long in a
	// peer JSON snapshot. Keep a single dashboard cell bounded.
	if len(command) > 96 {
		command = strings.ToValidUTF8(command[:96], "") + "…"
	}
	if command == "" {
		return fmt.Sprintf("PID %d", pid)
	}
	return fmt.Sprintf("%s (PID %d)", command, pid)
}

// Decode only the fields used by the table. In particular, pointer fields
// preserve the difference between an absent JSON value and an observed zero.
type peerHostProcEvidence struct {
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt"`
	CPUs      *int      `json:"cpus"`
	CPUTotal  *struct {
		Busy *float64 `json:"busyPercent"`
		Idle *float64 `json:"idlePercent"`
	} `json:"cpuTotal"`
	TopCPU []struct {
		PID     int      `json:"pid"`
		Command string   `json:"command"`
		CPU     *float64 `json:"cpuPercent"`
	} `json:"topCPU"`
	Health procstats.Health `json:"health"`
}

func (row *hostEvidenceRow) applyPeerProc(local *multihost.LocalSnapshot) {
	raw, exists := local.Sections[procstats.CollectorName]
	if !exists || len(raw) == 0 {
		row.Note = appendHostEvidenceNote(row.Note, "proc section missing")
		return
	}
	if len(raw) > maxHostEvidenceSectionBytes {
		row.Status = "error"
		row.Note = appendHostEvidenceNote(row.Note, "proc section exceeds display limit")
		return
	}
	var proc peerHostProcEvidence
	if err := json.Unmarshal(raw, &proc); err != nil || string(raw) == "null" {
		row.Status = "error"
		row.Note = appendHostEvidenceNote(row.Note, "proc section malformed")
		return
	}
	row.Note = procIntervalNote(proc.StartedAt, proc.EndedAt)
	invalid := false
	if proc.CPUs != nil && *proc.CPUs > 0 {
		row.CoreCapacity = fmt.Sprint(*proc.CPUs)
	}
	if proc.CPUTotal != nil {
		if proc.CPUTotal.Busy != nil {
			if validHostPercent(*proc.CPUTotal.Busy) {
				row.HostBusy = fmt.Sprintf("%.1f%%", *proc.CPUTotal.Busy)
				row.Status = "measured"
			} else {
				invalid = true
			}
		}
		if proc.CPUTotal.Idle != nil {
			if validHostPercent(*proc.CPUTotal.Idle) {
				row.HostIdle = fmt.Sprintf("%.1f%%", *proc.CPUTotal.Idle)
				row.Status = "measured"
			} else {
				invalid = true
			}
		}
		if invalid {
			row.Note = appendHostEvidenceNote(row.Note, "invalid host CPU percent")
		}
	}
	var top *struct {
		PID     int      `json:"pid"`
		Command string   `json:"command"`
		CPU     *float64 `json:"cpuPercent"`
	}
	processInvalid := false
	for i := range proc.TopCPU {
		candidate := &proc.TopCPU[i]
		if candidate.CPU != nil && !validProcessPercent(*candidate.CPU) {
			processInvalid = true
		}
		if candidate.CPU != nil && validProcessPercent(*candidate.CPU) && (top == nil || *candidate.CPU > *top.CPU) {
			top = candidate
		}
	}
	if processInvalid {
		invalid = true
		row.Note = appendHostEvidenceNote(row.Note, "invalid process CPU percent")
	} else if top != nil {
		row.TopProcess = hostEvidenceProcess(top.Command, top.PID)
		row.TopCPU = fmt.Sprintf("%.1f%%", *top.CPU)
		row.Status = "measured"
	}
	if row.Status == "missing" {
		row.Note = appendHostEvidenceNote(row.Note, "CPU measurements missing")
	}
	if invalid {
		row.Status = "error"
	}
	row.applyProcHealth(proc.Health)
}
