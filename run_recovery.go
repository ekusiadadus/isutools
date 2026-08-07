package isutools

import (
	"sync"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/web"
)

const maxRunRecoveryRecords = 8

// runRecoveryLedger keeps the bounded lifecycle evidence needed to preserve a
// live report when /save arrives after StartedTTL. The Controller intentionally
// evicts tombstones; recovery provenance must therefore not depend on the
// tombstone still being present an hour later.
type runRecoveryLedger struct {
	mu      sync.Mutex
	records map[string]*runRecoveryRecord
	order   []string
}

type runRecoveryRecord struct {
	start       runctl.StartResult
	hasStart    bool
	termination runctl.RunTerminationEvent
	hasTerminal bool
}

func newRunRecoveryLedger() *runRecoveryLedger {
	return &runRecoveryLedger{records: make(map[string]*runRecoveryRecord)}
}

func (l *runRecoveryLedger) RecordStart(start runctl.StartResult) {
	if l == nil || start.RunID == "" || start.Epoch == 0 {
		return
	}
	l.mu.Lock()
	record := l.recordLocked(start.RunID)
	record.start, record.hasStart = start, true
	l.mu.Unlock()
}

func (l *runRecoveryLedger) OnRunTermination(event runctl.RunTerminationEvent) {
	if l == nil || event.RunID == "" || event.Epoch == 0 || event.Reason == "" || event.BoundaryAt.IsZero() {
		return
	}
	l.mu.Lock()
	record := l.recordLocked(event.RunID)
	// LifecycleObserver is exactly-once, but retaining the first event also
	// makes this ledger safe if a custom Controller violates that contract.
	if !record.hasTerminal {
		record.termination, record.hasTerminal = event, true
	}
	l.mu.Unlock()
}

func (l *runRecoveryLedger) RecoverStartedTTL(runID string) (web.RunFinish, bool) {
	if l == nil || runID == "" {
		return web.RunFinish{}, false
	}
	l.mu.Lock()
	record := l.records[runID]
	if record == nil || !record.hasStart || !record.hasTerminal {
		l.mu.Unlock()
		return web.RunFinish{}, false
	}
	start, terminal := record.start, record.termination
	l.mu.Unlock()
	if terminal.Reason != runctl.ReasonStartedTTL || terminal.Epoch != start.Epoch {
		return web.RunFinish{}, false
	}
	return web.RunFinish{
		RunID: runID, Epoch: uint64(start.Epoch), State: string(runctl.StateAborted),
		Validity: string(runctl.ValidityInvalid), StartedAt: start.StartedAt,
		AcceptedAt: terminal.BoundaryAt, Recovered: true, RecoveryReason: terminal.Reason,
		GenerationWindow: webBoundaryWindow(start.GenerationWindow),
	}, true
}

func (l *runRecoveryLedger) recordLocked(runID string) *runRecoveryRecord {
	if record := l.records[runID]; record != nil {
		return record
	}
	record := &runRecoveryRecord{}
	l.records[runID] = record
	l.order = append(l.order, runID)
	for len(l.order) > maxRunRecoveryRecords {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.records, oldest)
	}
	return record
}
