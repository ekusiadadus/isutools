// Package multihost implements the evidence-bounded hub/peer protocol.
package multihost

import (
	"encoding/json"
	"time"

	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/internal/runctl"
)

const (
	ProtocolVersion     = 1
	SchemaVersion       = 1
	MaxPeers            = 8
	RetainedRuns        = 2
	NonceHistoryMax     = 64
	MaxRequestBytes     = 64 << 10
	PeerSelfCapBytes    = 8 << 20
	TotalSnapshotCap    = 32 << 20
	HubSelfReserve      = 16 << 20
	PerPeerDefaultBytes = 4 << 20
	MinPeerBytes        = 512 << 10
	BudgetReserve       = 4 << 10
	PeerStartedLease    = 45 * time.Second
	PeerStartedLeaseMax = 90 * time.Second
	PeerAckLease        = 90 * time.Second
)

type ErrorDTO struct {
	Code             string `json:"code"`
	Message          string `json:"message,omitempty"`
	RunID            string `json:"run_id,omitempty"`
	ActiveRunID      string `json:"active_run_id,omitempty"`
	ActiveState      string `json:"active_state,omitempty"`
	LeaseExpiresInMS int64  `json:"lease_expires_in_ms,omitempty"`
}

type TargetSummaryDTO struct {
	ID       string   `json:"id"`
	Driver   string   `json:"driver"`
	Display  string   `json:"display"`
	Schema   string   `json:"schema"`
	Purposes []string `json:"purposes"`
}

type PeerInfoDTO struct {
	ProtocolVersion int                `json:"protocol_version"`
	SchemaVersion   int                `json:"schema_version"`
	LibraryVersion  string             `json:"library_version"`
	AgentID         string             `json:"agent_id"`
	Form            string             `json:"form"`
	Role            string             `json:"role"`
	Sections        []string           `json:"sections"`
	Capabilities    []string           `json:"capabilities"`
	Identity        hoststats.Identity `json:"identity"`
	CgroupScope     string             `json:"cgroup_scope"`
	Targets         []TargetSummaryDTO `json:"targets,omitempty"`
	ActiveRunID     string             `json:"active_run_id,omitempty"`
	StartedAt       time.Time          `json:"started_at"`
}

type StartRunRequest struct {
	RunID   *string `json:"run_id"`
	Nonce   *string `json:"nonce"`
	Preempt *bool   `json:"preempt"`
	Trigger string  `json:"trigger,omitempty"`
	LeaseMS int64   `json:"lease_ms,omitempty"`
}

type StartResultDTO struct {
	RunID            string                     `json:"run_id"`
	LocalRunID       string                     `json:"local_run_id"`
	Nonce            string                     `json:"nonce"`
	Epoch            uint64                     `json:"epoch"`
	State            string                     `json:"state"`
	Validity         string                     `json:"validity"`
	Collectors       []runctl.CollectorBoundary `json:"collectors"`
	GenerationWindow runctl.BoundaryWindow      `json:"generation_window"`
	BoundaryWindow   runctl.BoundaryWindow      `json:"boundary_window"`
	PreemptedRunID   string                     `json:"preempted_run_id,omitempty"`
	LeaseExpiresAt   time.Time                  `json:"lease_expires_at"`
	LeaseMS          int64                      `json:"lease_ms"`
	StartedAt        time.Time                  `json:"started_at"`
}

type FinishRunRequest struct{}

type FinishAcceptedDTO struct {
	RunID                string                     `json:"run_id"`
	Epoch                uint64                     `json:"epoch"`
	State                string                     `json:"state"`
	Validity             string                     `json:"validity"`
	Collectors           []runctl.CollectorBoundary `json:"collectors"`
	GenerationWindow     runctl.BoundaryWindow      `json:"generation_window"`
	BoundaryWindow       runctl.BoundaryWindow      `json:"boundary_window"`
	FinishLeaseExpiresAt time.Time                  `json:"finish_lease_expires_at"`
	AcceptedAt           time.Time                  `json:"accepted_at"`
}

type SectionIssueDTO struct {
	Section string `json:"section"`
	Code    string `json:"code"`
	Detail  string `json:"detail,omitempty"`
}

type RunStatusDTO struct {
	RunID          string            `json:"run_id"`
	LocalRunID     string            `json:"local_run_id"`
	Epoch          uint64            `json:"epoch"`
	State          string            `json:"state"`
	Validity       string            `json:"validity"`
	Reason         string            `json:"reason,omitempty"`
	AckedBy        string            `json:"acked_by,omitempty"`
	Detached       bool              `json:"detached,omitempty"`
	Since          time.Time         `json:"since"`
	Origin         string            `json:"origin"`
	LeaseExpiresAt time.Time         `json:"lease_expires_at,omitzero"`
	ExpiryReason   string            `json:"expiry_reason,omitempty"`
	SnapshotReady  bool              `json:"snapshot_ready"`
	SnapshotBytes  int64             `json:"snapshot_bytes,omitempty"`
	Issues         []SectionIssueDTO `json:"issues,omitempty"`
}

type AbortRequest struct {
	Reason string `json:"reason,omitempty"`
}

type AbortResultDTO struct {
	RunID             string    `json:"run_id"`
	Epoch             uint64    `json:"epoch"`
	State             string    `json:"state"`
	Reason            string    `json:"reason"`
	PeerReason        string    `json:"peer_reason"`
	Detached          bool      `json:"detached"`
	Partial           []string  `json:"partial,omitempty"`
	SnapshotDiscarded bool      `json:"snapshot_discarded"`
	AbortedAt         time.Time `json:"aborted_at"`
}

type LeaseDTO struct {
	RunID          string    `json:"run_id"`
	State          string    `json:"state"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	LeaseMS        int64     `json:"lease_ms"`
}

type SnapshotMetaDTO struct {
	Capabilities     []string              `json:"capabilities"`
	BoundaryWindow   runctl.BoundaryWindow `json:"boundary_window"`
	GenerationWindow runctl.BoundaryWindow `json:"generation_window"`
	Identity         hoststats.Identity    `json:"identity"`
	CgroupScope      string                `json:"cgroup_scope"`
}

type SnapshotBudgetDTO struct {
	MaxBytes        int64    `json:"max_bytes"`
	EncodedBytes    int64    `json:"encoded_bytes"`
	ShrunkSections  []string `json:"shrunk_sections,omitempty"`
	DroppedSections []string `json:"dropped_sections,omitempty"`
}

type LocalSnapshot struct {
	SchemaVersion int                        `json:"schema_version"`
	RunID         string                     `json:"run_id"`
	LocalRunID    string                     `json:"local_run_id"`
	Epoch         uint64                     `json:"epoch"`
	Validity      string                     `json:"validity"`
	Meta          SnapshotMetaDTO            `json:"meta"`
	Sections      map[string]json.RawMessage `json:"sections"`
	Issues        []SectionIssueDTO          `json:"issues,omitempty"`
	Budget        SnapshotBudgetDTO          `json:"budget"`
}

type ParticipantFailureDTO struct {
	Phase  string    `json:"phase"`
	Code   string    `json:"code"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

type PeerResult struct {
	Name          string                 `json:"name"`
	Info          PeerInfoDTO            `json:"info"`
	Required      bool                   `json:"required"`
	Form          string                 `json:"form"`
	Start         *StartResultDTO        `json:"start,omitempty"`
	Finish        *FinishAcceptedDTO     `json:"finish,omitempty"`
	Status        *RunStatusDTO          `json:"status,omitempty"`
	Aborted       *AbortResultDTO        `json:"aborted,omitempty"`
	Failure       *ParticipantFailureDTO `json:"failure,omitempty"`
	Sealed        string                 `json:"sealed"`
	StartSendAck  [2]time.Time           `json:"start_send_ack"`
	FinishSendAck [2]time.Time           `json:"finish_send_ack"`
	Issues        []SectionIssueDTO      `json:"issues,omitempty"`
	Local         *LocalSnapshot         `json:"local,omitempty"`
}
