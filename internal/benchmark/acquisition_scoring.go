package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// OracleDisappearanceProof captures independent secondary verification that an asset
// has genuinely disappeared from Douyin (HTTP 404 or 410) post-freeze.
// Invariant (Issue #70 / #63): Post-freeze disappearance requires an audited oracle proof,
// marks the session CORPUS_INVALIDATED, and forces a full fresh session restart.
// Denominator shrinkage or mid-run substitution is strictly forbidden.
type OracleDisappearanceProof struct {
	EntryID       string    `json:"entry_id"`
	AwemeID       string    `json:"aweme_id"`
	CanonicalURL  string    `json:"canonical_url"`
	HTTPStatus    int       `json:"http_status"` // Must be 404 (Not Found) or 410 (Gone)
	OracleMethod  string    `json:"oracle_method"`
	EvidenceHash  string    `json:"evidence_hash"` // 64-char lowercase hex SHA-256
	VerifiedAt    time.Time `json:"verified_at"`
	ConfirmedDead bool      `json:"confirmed_dead"` // Must be true
	OperatorNotes string    `json:"operator_notes,omitempty"`
}

// ValidateDisappearanceProof validates that the proof meets all oracle requirements.
func ValidateDisappearanceProof(proof OracleDisappearanceProof) error {
	if strings.TrimSpace(proof.EntryID) == "" {
		return errors.New("proof entry_id cannot be empty")
	}
	if strings.TrimSpace(proof.AwemeID) == "" {
		return errors.New("proof aweme_id cannot be empty")
	}
	if strings.TrimSpace(proof.CanonicalURL) == "" {
		return errors.New("proof canonical_url cannot be empty")
	}
	if proof.HTTPStatus != 404 && proof.HTTPStatus != 410 {
		return fmt.Errorf("oracle proof http_status must be 404 or 410, got %d (403/429 are rate-limit/WAF challenges, not disappearance)", proof.HTTPStatus)
	}
	if strings.TrimSpace(proof.OracleMethod) == "" {
		return errors.New("proof oracle_method cannot be empty")
	}
	if proof.VerifiedAt.IsZero() {
		return errors.New("proof verified_at must not be zero")
	}
	if !proof.ConfirmedDead {
		return errors.New("proof confirmed_dead must be true")
	}
	if err := validateSHA256Hex(proof.EvidenceHash); err != nil {
		return fmt.Errorf("proof evidence_hash: %w", err)
	}
	return nil
}

// ScoreAcquisitionEntry deterministically evaluates observed acquisition evidence against
// the frozen manifest entry.
// Returns:
// - status: "PASS", "FAIL", or "DISAPPEARED"
// - passed: boolean indicating whether the entry passed the scoring criteria
// - reason: explanation of failure or scoring decision
func ScoreAcquisitionEntry(entry AcquisitionCorpusEntry, observed *AcquisitionEntryEvidence) (status string, passed bool, reason string) {
	if observed == nil {
		return "FAIL", false, "no observed acquisition evidence"
	}

	// 1. Prohibit local-file substitution in scored acquisition benchmark (Issue #70 invariant)
	if observed.IsLocalSubstitution {
		return "FAIL", false, "local-file substitution is prohibited for scored acquisition benchmark; must use authorized Douyin URL path"
	}

	// 2. Verified Content Disappearance post-freeze with independent oracle proof
	if observed.DisappearanceProof != nil && observed.DisappearanceProof.ConfirmedDead {
		if err := ValidateDisappearanceProof(*observed.DisappearanceProof); err == nil {
			return "DISAPPEARED", false, fmt.Sprintf("post-freeze source disappearance verified by oracle (%s, http %d)", observed.DisappearanceProof.OracleMethod, observed.DisappearanceProof.HTTPStatus)
		}
	}

	// 3. Observed failure or anti-bot status
	if observed.Status == "FAIL" || observed.Status == "ANTI_BOT" {
		reason := observed.ErrorMessage
		if reason == "" {
			reason = fmt.Sprintf("observed acquisition status is %s", observed.Status)
		}
		return "FAIL", false, reason
	}

	// Anti-bot status or challenge condition without a real HTTP code cannot PASS (Finding 6)
	lowerErr := strings.ToLower(observed.ErrorMessage)
	if strings.Contains(lowerErr, "anti-bot") || strings.Contains(lowerErr, "waf") || strings.Contains(lowerErr, "rate limit") || strings.Contains(lowerErr, "captcha") {
		return "FAIL", false, fmt.Sprintf("anti-bot/challenge condition scored as failure: %s", observed.ErrorMessage)
	}

	// 4. HTTP >= 400 or unexpected non-success status is scored as failure
	if observed.HTTPStatusCode >= 400 {
		return "FAIL", false, fmt.Sprintf("HTTP %d is scored as failure; not genuine disappearance", observed.HTTPStatusCode)
	}
	if observed.HTTPStatusCode != 0 && observed.HTTPStatusCode != 200 && observed.HTTPStatusCode != 201 {
		return "FAIL", false, fmt.Sprintf("HTTP status %d is scored as failure", observed.HTTPStatusCode)
	}

	// 5. Content Unavailable without oracle proof is a normal acquisition failure
	if observed.Status == "CONTENT_UNAVAILABLE" {
		return "FAIL", false, "content unavailable without independent oracle verification is scored as acquisition failure"
	}

	// 6. Must commit to an immutable SourceAsset in CAS
	if strings.TrimSpace(observed.SourceAssetID) == "" {
		return "FAIL", false, "missing committed SourceAssetID"
	}
	if strings.TrimSpace(observed.SourceAssetCASHash) == "" {
		return "FAIL", false, "missing immutable SourceAssetCASHash"
	}
	// Invariant (Issue #70 Finding 4): Validate observed CAS hash as real 64-char hex
	if err := validateSHA256Hex(observed.SourceAssetCASHash); err != nil {
		return "FAIL", false, fmt.Sprintf("invalid observed SourceAssetCASHash: %v", err)
	}
	// Invariant (Issue #70 Finding 4): If BaselineMediaSHA256 is frozen, observed CAS hash must match exactly
	if entry.BaselineMediaSHA256 != "" {
		if !strings.EqualFold(observed.SourceAssetCASHash, entry.BaselineMediaSHA256) {
			return "FAIL", false, fmt.Sprintf("CAS hash mismatch: expected baseline %s, observed %s",
				entry.BaselineMediaSHA256, observed.SourceAssetCASHash)
		}
	}

	// 7. Identity match: observed aweme_id must match expected aweme_id (derived from real provenance)
	if strings.TrimSpace(observed.ObservedAwemeID) != strings.TrimSpace(entry.AwemeID) {
		return "FAIL", false, fmt.Sprintf("aweme_id mismatch: expected %s, observed %s", entry.AwemeID, observed.ObservedAwemeID)
	}
	// Canonical URL match if present in evidence
	if observed.CanonicalURL != "" && observed.CanonicalURL != entry.CanonicalURL {
		return "FAIL", false, fmt.Sprintf("canonical_url mismatch: expected %s, observed %s", entry.CanonicalURL, observed.CanonicalURL)
	}
	// 7. Video container & fingerprint integrity check
	if !observed.IntegrityPassed {
		return "FAIL", false, "container/video integrity validation failed (corrupt or unreadable media)"
	}

	// 8. Audio integrity check if audio is required
	if entry.RequiresAudio && !observed.AudioIntegrityPassed {
		return "FAIL", false, "audio track missing or normalized audio integrity check failed"
	}

	// 9. Duration tolerance check
	if entry.ExpectedDurationMs > 0 && entry.DurationToleranceMs > 0 {
		delta := observed.ObservedDurationMs - entry.ExpectedDurationMs
		if delta < 0 {
			delta = -delta
		}
		if delta > entry.DurationToleranceMs {
			return "FAIL", false, fmt.Sprintf("duration tolerance exceeded: observed %dms, expected %dms (tolerance %dms, delta %dms)",
				observed.ObservedDurationMs, entry.ExpectedDurationMs, entry.DurationToleranceMs, delta)
		}
	}

	return "PASS", true, ""
}

// AcquisitionBenchmarkSummary captures aggregate metrics and scoring results
// for the complete 100-URL acquisition benchmark.
type AcquisitionBenchmarkSummary struct {
	SessionID          string                     `json:"session_id"`
	ManifestDigest     string                     `json:"manifest_digest"`
	TotalEntries       int                        `json:"total_entries"`       // Invariant: Fixed at 100
	PassedEntries      int                        `json:"passed_entries"`      // Count of PASS entries
	FailedEntries      int                        `json:"failed_entries"`      // Count of FAIL entries
	DisappearedEntries int                        `json:"disappeared_entries"` // Count of DISAPPEARED entries
	SuccessRate        float64                    `json:"success_rate"`        // PassedEntries / TotalEntries (denominator=100)
	GateSatisfied      bool                       `json:"gate_satisfied"`      // True if PassedEntries >= 98 && TotalEntries == 100 && DisappearedEntries == 0
	ComputedAt         time.Time                  `json:"computed_at"`
	SummaryDigest      string                     `json:"summary_digest"` // SHA-256 of canonical summary bytes
	Entries            []AcquisitionEntryEvidence `json:"entries"`
}

type canonicalAcquisitionSummary struct {
	DisappearedEntries int     `json:"disappeared_entries"`
	FailedEntries      int     `json:"failed_entries"`
	GateSatisfied      bool    `json:"gate_satisfied"`
	ManifestDigest     string  `json:"manifest_digest"`
	PassedEntries      int     `json:"passed_entries"`
	SessionID          string  `json:"session_id"`
	SuccessRate        float64 `json:"success_rate"`
	TotalEntries       int     `json:"total_entries"`
}

// Canonicalize produces deterministic RFC 8785 canonical JSON bytes of the summary.
func (s *AcquisitionBenchmarkSummary) Canonicalize() ([]byte, error) {
	if s == nil {
		return nil, errors.New("summary is nil")
	}
	canon := canonicalAcquisitionSummary{
		DisappearedEntries: s.DisappearedEntries,
		FailedEntries:      s.FailedEntries,
		GateSatisfied:      s.GateSatisfied,
		ManifestDigest:     strings.ToLower(strings.TrimSpace(s.ManifestDigest)),
		PassedEntries:      s.PassedEntries,
		SessionID:          strings.TrimSpace(s.SessionID),
		SuccessRate:        s.SuccessRate,
		TotalEntries:       s.TotalEntries,
	}
	return json.Marshal(canon)
}

// ComputeSummaryDigest calculates the SHA-256 hex digest of the canonical summary JSON.
func (s *AcquisitionBenchmarkSummary) ComputeSummaryDigest() (string, error) {
	bytes, err := s.Canonicalize()
	if err != nil {
		return "", fmt.Errorf("canonicalize summary: %w", err)
	}
	hash := sha256.Sum256(bytes)
	return hex.EncodeToString(hash[:]), nil
}

// VerifySummaryDigest verifies that the summary digest matches the computed digest.
func (s *AcquisitionBenchmarkSummary) VerifySummaryDigest() error {
	if s == nil {
		return errors.New("summary is nil")
	}
	if strings.TrimSpace(s.SummaryDigest) == "" {
		return errors.New("summary_digest is empty")
	}
	computed, err := s.ComputeSummaryDigest()
	if err != nil {
		return fmt.Errorf("compute summary digest: %w", err)
	}
	if !strings.EqualFold(s.SummaryDigest, computed) {
		return fmt.Errorf("summary digest mismatch: expected %s, computed %s", s.SummaryDigest, computed)
	}
	return nil
}

// ComputeAcquisitionSummary aggregates and scores results for a completed 100-URL benchmark.
// Invariant (Issue #70 / #63): Denominator is fixed at 100. Denominator shrinkage is forbidden.
func ComputeAcquisitionSummary(sessionID string, manifest *AcquisitionCorpusManifest, evidences map[string]*AcquisitionEntryEvidence, computedAt time.Time) (*AcquisitionBenchmarkSummary, error) {
	if manifest == nil {
		return nil, errors.New("manifest is nil")
	}
	if err := manifest.VerifyManifestDigest(); err != nil {
		return nil, fmt.Errorf("verify manifest digest: %w", err)
	}
	if manifest.TotalEntries != 100 || len(manifest.Entries) != 100 {
		return nil, fmt.Errorf("manifest must have exactly 100 entries, got %d", manifest.TotalEntries)
	}
	if computedAt.IsZero() {
		return nil, errors.New("computedAt must not be zero")
	}

	passed := 0
	failed := 0
	disappeared := 0
	scoredEntries := make([]AcquisitionEntryEvidence, 0, 100)

	for _, entry := range manifest.Entries {
		ev, exists := evidences[entry.EntryID]
		if !exists || ev == nil {
			// Missing entry is scored as FAIL to preserve denominator = 100
			failed++
			scoredEntries = append(scoredEntries, AcquisitionEntryEvidence{
				EntryID:             entry.EntryID,
				CanonicalURL:        entry.CanonicalURL,
				ExpectedAwemeID:     entry.AwemeID,
				ExpectedDurationMs:  entry.ExpectedDurationMs,
				DurationToleranceMs: entry.DurationToleranceMs,
				Status:              "FAIL",
				ErrorMessage:        "missing execution evidence in completed session",
				Timestamp:           computedAt,
			})
			continue
		}

		status, isPass, reason := ScoreAcquisitionEntry(entry, ev)
		scoredEv := *ev
		scoredEv.Status = status
		if !isPass && scoredEv.ErrorMessage == "" {
			scoredEv.ErrorMessage = reason
		}
		scoredEntries = append(scoredEntries, scoredEv)

		switch status {
		case "PASS":
			passed++
		case "DISAPPEARED":
			disappeared++
		default:
			failed++
		}
	}

	// Sort scored entries deterministically by EntryID
	sort.Slice(scoredEntries, func(i, j int) bool {
		return scoredEntries[i].EntryID < scoredEntries[j].EntryID
	})

	// Fixed denominator = 100
	total := 100
	successRate := float64(passed) / float64(total)
	gateSatisfied := (passed >= 98 && disappeared == 0)

	summary := &AcquisitionBenchmarkSummary{
		SessionID:          sessionID,
		ManifestDigest:     manifest.ManifestDigest,
		TotalEntries:       total,
		PassedEntries:      passed,
		FailedEntries:      failed,
		DisappearedEntries: disappeared,
		SuccessRate:        successRate,
		GateSatisfied:      gateSatisfied,
		ComputedAt:         computedAt.UTC(),
		Entries:            scoredEntries,
	}

	digest, err := summary.ComputeSummaryDigest()
	if err != nil {
		return nil, fmt.Errorf("compute summary digest: %w", err)
	}
	summary.SummaryDigest = digest

	return summary, nil
}
