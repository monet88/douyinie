package benchmark

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SessionStatus represents the lifecycle state of a benchmark session.
type SessionStatus string

const (
	SessionStatusInitialized       SessionStatus = "INITIALIZED"
	SessionStatusRunning           SessionStatus = "RUNNING"
	SessionStatusPaused            SessionStatus = "PAUSED"
	SessionStatusCompleted         SessionStatus = "COMPLETED"
	SessionStatusFailed            SessionStatus = "FAILED"
	SessionStatusInvalidated       SessionStatus = "INVALIDATED"
	SessionStatusCorpusInvalidated SessionStatus = "CORPUS_INVALIDATED"
)

// Session represents a resumable benchmark execution envelope.
// Invariant: Native LocalizationJob and LocalizationRun identities are preserved;
// the benchmark session is an envelope over product executions rather than a replacement ID scheme.
type Session struct {
	ID                    string                               `json:"id"` // "bms_<identity_sha256>"
	IdentityDigest        string                               `json:"identity_digest"`
	IdentityInput         SessionIdentityInput                 `json:"identity_input"`
	Status                SessionStatus                        `json:"status"`
	CreatedAt             time.Time                            `json:"created_at"`
	UpdatedAt             time.Time                            `json:"updated_at"`
	AcquisitionEntries    map[string]*AcquisitionEntryEvidence `json:"acquisition_entries"`
	QualityCases          map[string]*QualityCaseEvidence      `json:"quality_cases"`
	QualityCorpusManifest *QualityCorpusManifest               `json:"quality_corpus_manifest,omitempty"`
	AuditPlan             *StratifiedAuditPlan                 `json:"audit_plan,omitempty"`
	AcquisitionManifest   *AcquisitionCorpusManifest           `json:"acquisition_manifest,omitempty"`
	AcquisitionSummary    *AcquisitionBenchmarkSummary         `json:"acquisition_summary,omitempty"`
	QualitySummary        *QualityBenchmarkSummary             `json:"quality_summary,omitempty"`
	mu                    sync.RWMutex
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

// RecordQualityCorpusManifest binds the quality corpus manifest to the session.
// Invariant (Issue #71 Criterion 10): Validation/audit-plan evidence is bound to the
// benchmark-session identity from #65 and remains benchmark tooling rather than a third product seam.
// RecordQualityCorpusManifest binds the quality corpus manifest to the session.
// Invariant (Issue #71 Criterion 10): Strict session binding. Manifest digest is verified,
// and SessionIdentityInput must explicitly contain a matching quality-corpus digest; absence is an error.
func (s *Session) RecordQualityCorpusManifest(m *QualityCorpusManifest) error {
	if m == nil {
		return errors.New("nil quality corpus manifest")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Verify manifest's internal integrity
	if err := m.VerifyManifestDigest(); err != nil {
		return fmt.Errorf("manifest integrity verification failed: %w", err)
	}

	// 2. Require a matching quality-corpus digest in SessionIdentityInput
	matched := false
	for _, cd := range s.IdentityInput.CorpusDigests {
		cdName := strings.ToLower(strings.TrimSpace(cd.Name))
		if strings.Contains(cdName, "quality") || strings.EqualFold(cdName, m.CorpusName) {
			matched = true
			if !strings.EqualFold(cd.Digest, m.ManifestDigest) {
				return fmt.Errorf("%w: session bound corpus digest %s != manifest digest %s",
					ErrManifestDigestMismatch, cd.Digest, m.ManifestDigest)
			}
		}
	}
	if !matched {
		return fmt.Errorf("%w: session identity input does not contain a quality corpus digest entry for %s",
			ErrManifestDigestMismatch, m.CorpusName)
	}

	s.QualityCorpusManifest = m
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// GetQualityCorpusManifest returns the bound quality corpus manifest if present.
func (s *Session) GetQualityCorpusManifest() (*QualityCorpusManifest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.QualityCorpusManifest == nil {
		return nil, false
	}
	cp := *s.QualityCorpusManifest
	return &cp, true
}

// RecordAuditPlan binds the deterministic PASS audit plan to the session.
// Invariant (Issue #71): Requires a bound verified manifest, verifies plan digest/integrity,
// and rejects identity/manifest mismatch.
func (s *Session) RecordAuditPlan(plan *StratifiedAuditPlan) error {
	if plan == nil {
		return errors.New("nil audit plan")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Must require a bound verified manifest
	if s.QualityCorpusManifest == nil {
		return fmt.Errorf("%w: cannot record audit plan without previously bound verified quality corpus manifest", ErrCorpusValidationFailed)
	}
	if err := s.QualityCorpusManifest.VerifyManifestDigest(); err != nil {
		return fmt.Errorf("bound manifest integrity invalid: %w", err)
	}

	// 2. Verify plan digest/integrity
	if err := plan.VerifyPlanDigest(); err != nil {
		return fmt.Errorf("audit plan integrity verification failed: %w", err)
	}

	// 3. Reject identity/manifest mismatch
	if !strings.EqualFold(plan.ManifestDigest, s.QualityCorpusManifest.ManifestDigest) {
		return fmt.Errorf("%w: audit plan manifest digest %s != bound corpus manifest digest %s",
			ErrManifestDigestMismatch, plan.ManifestDigest, s.QualityCorpusManifest.ManifestDigest)
	}

	s.AuditPlan = plan
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// GetAuditPlan returns the bound stratified audit plan if present.
func (s *Session) GetAuditPlan() (*StratifiedAuditPlan, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.AuditPlan == nil {
		return nil, false
	}
	cp := *s.AuditPlan
	return &cp, true
}

// RecordAcquisitionManifest binds a frozen 100-URL acquisition manifest to the session.
// Invariant (Issue #70 / #63): Manifest digest must match one of the CorpusDigests in SessionIdentityInput.
func (s *Session) RecordAcquisitionManifest(m *AcquisitionCorpusManifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status == SessionStatusCorpusInvalidated {
		return errors.New("cannot record acquisition manifest on CORPUS_INVALIDATED session")
	}
	if m == nil {
		return errors.New("acquisition corpus manifest is nil")
	}
	if err := ValidateAcquisitionCorpusManifest(m); err != nil {
		return fmt.Errorf("validate acquisition manifest: %w", err)
	}
	if err := m.VerifyManifestDigest(); err != nil {
		return fmt.Errorf("verify acquisition manifest digest: %w", err)
	}

	// Verify binding to SessionIdentityInput.CorpusDigests
	matched := false
	for _, cd := range s.IdentityInput.CorpusDigests {
		if strings.EqualFold(cd.Digest, m.ManifestDigest) {
			matched = true
			break
		}
	}
	if !matched {
		return fmt.Errorf("acquisition manifest digest %s does not match any corpus digest in session identity", m.ManifestDigest)
	}

	s.AcquisitionManifest = m
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// GetAcquisitionManifest returns a copy of the bound acquisition manifest if present.
func (s *Session) GetAcquisitionManifest() (*AcquisitionCorpusManifest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.AcquisitionManifest == nil {
		return nil, false
	}
	cp := *s.AcquisitionManifest
	return &cp, true
}

// RecordAcquisitionSummary records aggregate metrics for the 100-URL acquisition benchmark.
// Invariant (Issue #70 / #63): Denominator must be exactly 100. Denominator shrinkage is forbidden.
func (s *Session) RecordAcquisitionSummary(summary *AcquisitionBenchmarkSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status == SessionStatusCorpusInvalidated {
		return errors.New("cannot record summary on CORPUS_INVALIDATED session")
	}
	if summary == nil {
		return errors.New("acquisition summary is nil")
	}
	if s.AcquisitionManifest == nil {
		return errors.New("cannot record acquisition summary before binding acquisition manifest")
	}
	if !strings.EqualFold(summary.ManifestDigest, s.AcquisitionManifest.ManifestDigest) {
		return fmt.Errorf("summary manifest digest %s does not match session acquisition manifest %s",
			summary.ManifestDigest, s.AcquisitionManifest.ManifestDigest)
	}
	if summary.TotalEntries != 100 {
		return fmt.Errorf("acquisition benchmark denominator must be exactly 100, got %d", summary.TotalEntries)
	}
	if err := summary.VerifySummaryDigest(); err != nil {
		return fmt.Errorf("verify summary digest: %w", err)
	}

	s.AcquisitionSummary = summary
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// GetAcquisitionSummary returns a copy of the acquisition summary if present.
func (s *Session) GetAcquisitionSummary() (*AcquisitionBenchmarkSummary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.AcquisitionSummary == nil {
		return nil, false
	}
	cp := *s.AcquisitionSummary
	return &cp, true
}

// RecordQualitySummary records aggregate metrics for the 96-execution quality benchmark.
func (s *Session) RecordQualitySummary(summary *QualityBenchmarkSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status == SessionStatusCorpusInvalidated {
		return errors.New("cannot record summary on CORPUS_INVALIDATED session")
	}
	if summary == nil {
		return errors.New("quality summary is nil")
	}
	if s.QualityCorpusManifest == nil {
		return errors.New("cannot record quality summary before binding quality corpus manifest")
	}
	if !strings.EqualFold(summary.ManifestDigest, s.QualityCorpusManifest.ManifestDigest) {
		return fmt.Errorf("summary manifest digest %s does not match session quality corpus manifest %s",
			summary.ManifestDigest, s.QualityCorpusManifest.ManifestDigest)
	}
	if err := summary.VerifySummaryDigest(); err != nil {
		return fmt.Errorf("verify quality summary digest: %w", err)
	}

	s.QualitySummary = summary
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// GetQualitySummary returns a copy of the quality summary if present.
func (s *Session) GetQualitySummary() (*QualityBenchmarkSummary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.QualitySummary == nil {
		return nil, false
	}
	cp := *s.QualitySummary
	return &cp, true
}

// InvalidateCorpus records independent oracle proof of post-freeze source disappearance,
// transitions the session to CORPUS_INVALIDATED, and records the disappeared entry.
// Invariant (Issue #70 / #63): Post-freeze disappearance requires audited oracle proof,
// marks session CORPUS_INVALIDATED, and forces a full fresh session restart without denominator shrinkage.
func (s *Session) InvalidateCorpus(entryID string, proof OracleDisappearanceProof) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ValidateDisappearanceProof(proof); err != nil {
		return fmt.Errorf("invalid oracle disappearance proof: %w", err)
	}
	if proof.EntryID != entryID {
		return fmt.Errorf("proof entry_id %q does not match target entry_id %q", proof.EntryID, entryID)
	}

	s.Status = SessionStatusCorpusInvalidated
	now := time.Now().UTC()
	s.UpdatedAt = now

	existing, ok := s.AcquisitionEntries[entryID]
	if ok && existing != nil {
		existing.Status = "DISAPPEARED"
		existing.HTTPStatusCode = proof.HTTPStatus
		existing.DisappearanceProof = &proof
		existing.ErrorMessage = fmt.Sprintf("post-freeze disappearance verified by oracle (%s)", proof.OracleMethod)
		existing.Timestamp = now
	} else {
		s.AcquisitionEntries[entryID] = &AcquisitionEntryEvidence{
			EntryID:            entryID,
			CanonicalURL:       proof.CanonicalURL,
			ExpectedAwemeID:    proof.AwemeID,
			HTTPStatusCode:     proof.HTTPStatus,
			Status:             "DISAPPEARED",
			ErrorMessage:       fmt.Sprintf("post-freeze disappearance verified by oracle (%s)", proof.OracleMethod),
			DisappearanceProof: &proof,
			Timestamp:          now,
		}
	}

	return nil
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

	var corpusCopy *QualityCorpusManifest
	if s.QualityCorpusManifest != nil {
		cp := *s.QualityCorpusManifest
		corpusCopy = &cp
	}

	var planCopy *StratifiedAuditPlan
	if s.AuditPlan != nil {
		cp := *s.AuditPlan
		planCopy = &cp
	}

	var acqManifestCopy *AcquisitionCorpusManifest
	if s.AcquisitionManifest != nil {
		cp := *s.AcquisitionManifest
		acqManifestCopy = &cp
	}

	var acqSummaryCopy *AcquisitionBenchmarkSummary
	if s.AcquisitionSummary != nil {
		cp := *s.AcquisitionSummary
		acqSummaryCopy = &cp
	}

	var qualitySummaryCopy *QualityBenchmarkSummary
	if s.QualitySummary != nil {
		cp := *s.QualitySummary
		qualitySummaryCopy = &cp
	}
	return &Session{
		ID:                    s.ID,
		IdentityDigest:        s.IdentityDigest,
		IdentityInput:         s.IdentityInput,
		Status:                s.Status,
		CreatedAt:             s.CreatedAt,
		UpdatedAt:             s.UpdatedAt,
		AcquisitionEntries:    acqCopy,
		QualityCases:          qcCopy,
		QualityCorpusManifest: corpusCopy,
		AuditPlan:             planCopy,
		AcquisitionManifest:   acqManifestCopy,
		AcquisitionSummary:    acqSummaryCopy,
		QualitySummary:        qualitySummaryCopy,
	}
}
