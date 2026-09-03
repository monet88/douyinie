package benchmark

import (
	"fmt"
	"sync"
	"time"
)

// SessionStatus represents the lifecycle state of a benchmark session.
type SessionStatus string

const (
	SessionStatusInitialized SessionStatus = "INITIALIZED"
	SessionStatusRunning     SessionStatus = "RUNNING"
	SessionStatusPaused      SessionStatus = "PAUSED"
	SessionStatusCompleted   SessionStatus = "COMPLETED"
	SessionStatusFailed      SessionStatus = "FAILED"
	SessionStatusInvalidated SessionStatus = "INVALIDATED"
)

// Session represents a resumable benchmark execution envelope.
// Invariant: Native LocalizationJob and LocalizationRun identities are preserved;
// the benchmark session is an envelope over product executions rather than a replacement ID scheme.
type Session struct {
	ID                 string                               `json:"id"` // "bms_<identity_sha256>"
	IdentityDigest     string                               `json:"identity_digest"`
	IdentityInput      SessionIdentityInput                 `json:"identity_input"`
	Status             SessionStatus                        `json:"status"`
	CreatedAt          time.Time                            `json:"created_at"`
	UpdatedAt          time.Time                            `json:"updated_at"`
	AcquisitionEntries map[string]*AcquisitionEntryEvidence `json:"acquisition_entries"`
	QualityCases       map[string]*QualityCaseEvidence      `json:"quality_cases"`

	mu sync.RWMutex
}

// NewSession creates a new benchmark session with deterministically computed session identity.
func NewSession(input SessionIdentityInput) (*Session, error) {
	sessionID, digest, err := ComputeSessionID(input)
	if err != nil {
		return nil, fmt.Errorf("new benchmark session: %w", err)
	}

	now := time.Now().UTC()
	return &Session{
		ID:                 sessionID,
		IdentityDigest:     digest,
		IdentityInput:      input,
		Status:             SessionStatusInitialized,
		CreatedAt:          now,
		UpdatedAt:          now,
		AcquisitionEntries: make(map[string]*AcquisitionEntryEvidence),
		QualityCases:       make(map[string]*QualityCaseEvidence),
	}, nil
}

// SetStatus updates session status in a thread-safe manner.
func (s *Session) SetStatus(status SessionStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = status
	s.UpdatedAt = time.Now().UTC()
}

// RecordAcquisition records or updates evidence for one acquisition entry.
func (s *Session) RecordAcquisition(entry AcquisitionEntryEvidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AcquisitionEntries[entry.EntryID] = &entry
	s.UpdatedAt = time.Now().UTC()
}

// GetAcquisition returns a copy of the acquisition evidence if present.
func (s *Session) GetAcquisition(entryID string) (*AcquisitionEntryEvidence, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.AcquisitionEntries[entryID]
	if !ok || entry == nil {
		return nil, false
	}
	cp := *entry
	return &cp, true
}

// IsAcquisitionComplete checks whether the acquisition entry has completed.
func (s *Session) IsAcquisitionComplete(entryID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.AcquisitionEntries[entryID]
	if !ok || entry == nil {
		return false
	}
	return entry.Status == "PASS" || entry.Status == "FAIL" || entry.Status == "CONTENT_UNAVAILABLE" || entry.Status == "DISAPPEARED"
}

// RecordQualityCase records or updates an entire quality case evidence record.
func (s *Session) RecordQualityCase(qc QualityCaseEvidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if qc.Stages == nil {
		qc.Stages = make(map[string]StageExecutionEvidence)
	}
	s.QualityCases[qc.CaseID] = &qc
	s.UpdatedAt = time.Now().UTC()
}

// GetQualityCase returns a copy of the quality case evidence if present.
func (s *Session) GetQualityCase(caseID string) (*QualityCaseEvidence, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	qc, ok := s.QualityCases[caseID]
	if !ok || qc == nil {
		return nil, false
	}
	cp := *qc
	return &cp, true
}

// IsQualityCaseComplete checks whether the quality case is marked COMPLETED.
func (s *Session) IsQualityCaseComplete(caseID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	qc, ok := s.QualityCases[caseID]
	if !ok || qc == nil {
		return false
	}
	return qc.Status == "COMPLETED"
}

// UpdateQualityStage records completion or failure of a single pipeline stage for a case.
func (s *Session) UpdateQualityStage(caseID string, stage StageExecutionEvidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	qc, ok := s.QualityCases[caseID]
	if !ok {
		qc = &QualityCaseEvidence{
			CaseID:    caseID,
			Stages:    make(map[string]StageExecutionEvidence),
			Status:    "IN_PROGRESS",
			CreatedAt: time.Now().UTC(),
		}
		s.QualityCases[caseID] = qc
	}
	if qc.Stages == nil {
		qc.Stages = make(map[string]StageExecutionEvidence)
	}
	qc.Stages[stage.Stage] = stage
	s.UpdatedAt = time.Now().UTC()
}

// RecordRelationalQC records SQLite relational QC results and review items for a case.
func (s *Session) RecordRelationalQC(caseID string, qc RelationalQCEvidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.QualityCases[caseID]
	if !ok {
		return
	}
	existing.RelationalQC = qc
	s.UpdatedAt = time.Now().UTC()
}

// RecordTelemetry appends a resource telemetry sample to a quality case.
func (s *Session) RecordTelemetry(caseID string, sample ResourceTelemetrySample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.QualityCases[caseID]
	if !ok {
		return
	}
	existing.Telemetry = append(existing.Telemetry, sample)
	s.UpdatedAt = time.Now().UTC()
}

// Snapshot returns a shallow copy of Session for serialization without holding locks during I/O.
func (s *Session) Snapshot() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	acqCopy := make(map[string]*AcquisitionEntryEvidence, len(s.AcquisitionEntries))
	for k, v := range s.AcquisitionEntries {
		if v != nil {
			cp := *v
			acqCopy[k] = &cp
		}
	}

	qcCopy := make(map[string]*QualityCaseEvidence, len(s.QualityCases))
	for k, v := range s.QualityCases {
		if v != nil {
			cp := *v
			qcCopy[k] = &cp
		}
	}

	return &Session{
		ID:                 s.ID,
		IdentityDigest:     s.IdentityDigest,
		IdentityInput:      s.IdentityInput,
		Status:             s.Status,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
		AcquisitionEntries: acqCopy,
		QualityCases:       qcCopy,
	}
}
