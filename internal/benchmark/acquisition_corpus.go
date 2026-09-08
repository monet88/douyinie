package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OracleValidation records independent verification that an asset is genuine,
// accessible, and public prior to corpus freeze.
type OracleValidation struct {
	Method       string    `json:"method"`        // e.g. "oracle_curl_probe", "independent_browser_audit"
	ValidatedAt  time.Time `json:"validated_at"`  // Timestamp of oracle verification
	EvidenceHash string    `json:"evidence_hash"` // 64-char lowercase hex SHA-256 of probe payload/response
	Notes        string    `json:"notes,omitempty"`
}

// AcquisitionCorpusEntry represents one oracle-validated Douyin URL in the frozen acquisition benchmark.
func validateSHA256Hex(s string) error {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	if len(trimmed) != 64 {
		return fmt.Errorf("sha256 must be 64 hex characters, got len %d", len(trimmed))
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return fmt.Errorf("invalid sha256 hex encoding: %w", err)
	}
	return nil
}

// ValidateCanonicalURL enforces the exact frozen canonical form:
// https://www.douyin.com/video/<aweme_id>
// Rejects hostile lookalike hosts, wrong path or aweme_id, queries, fragments, or non-HTTPS schemes.
func ValidateCanonicalURL(rawURL string, expectedAwemeID string) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return errors.New("canonical_url cannot be empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("parse canonical_url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("canonical_url scheme must be https, got %q", u.Scheme)
	}
	if u.Host != "www.douyin.com" {
		return fmt.Errorf("canonical_url host must be exactly www.douyin.com, got %q", u.Host)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("canonical_url must not contain query parameters, got query %q", u.RawQuery)
	}
	if u.Fragment != "" {
		return fmt.Errorf("canonical_url must not contain fragment, got fragment %q", u.Fragment)
	}
	expectedPath := "/video/" + expectedAwemeID
	if u.Path != expectedPath {
		return fmt.Errorf("canonical_url path must be exactly %q, got %q", expectedPath, u.Path)
	}
	expectedURL := "https://www.douyin.com/video/" + expectedAwemeID
	if trimmed != expectedURL {
		return fmt.Errorf("canonical_url must match exact frozen form %q, got %q", expectedURL, trimmed)
	}
	return nil
}

// AcquisitionCorpusEntry represents one oracle-validated Douyin URL in the frozen acquisition benchmark.
type AcquisitionCorpusEntry struct {
	EntryID             string           `json:"entry_id"`                        // Stable ID (matches AwemeID)
	AwemeID             string           `json:"aweme_id"`                        // Numeric Douyin item ID
	CanonicalURL        string           `json:"canonical_url"`                   // https://www.douyin.com/video/<aweme_id>
	ExpectedDurationMs  int64            `json:"expected_duration_ms"`            // Ground truth duration in ms
	DurationToleranceMs int64            `json:"duration_tolerance_ms"`           // Max acceptable duration delta
	RequiresAudio       bool             `json:"requires_audio"`                  // Whether soundtrack/audio is expected
	Oracle              OracleValidation `json:"oracle"`                          // Independent oracle attestation
	BaselineMediaSHA256 string           `json:"baseline_media_sha256,omitempty"` // Known media hash if available
}

// AcquisitionCorpusManifest is the frozen, immutable manifest of exactly 100 oracle-validated Douyin URLs.
// Invariant (Issue #70 / #63): The acquisition benchmark must measure real authorized production acquisition
// with no denominator shrinkage, no local file substitution, and no hidden benchmark retries.
type AcquisitionCorpusManifest struct {
	CorpusName     string                   `json:"corpus_name"`     // e.g. "douyin_phase1_acquisition_100"
	Version        string                   `json:"version"`         // e.g. "1.1"
	FrozenAt       time.Time                `json:"frozen_at"`       // Timestamp when manifest was cryptographically frozen
	TotalEntries   int                      `json:"total_entries"`   // MUST be exactly 100
	Entries        []AcquisitionCorpusEntry `json:"entries"`         // Exactly 100 entries sorted by AwemeID
	ManifestDigest string                   `json:"manifest_digest"` // 64-char lowercase hex SHA-256 of canonical bytes
}

// ValidateAcquisitionCorpusManifest validates that the manifest adheres to all Issue #70 invariants:
// 1. TotalEntries == 100 and len(Entries) == 100.
// 2. No duplicate EntryID, AwemeID, or CanonicalURL.
// 3. Numeric AwemeID and canonical URL format.
// 4. Positive duration and tolerance.
// 5. Complete Oracle validation (non-zero time, valid 64-char hex evidence hash).
func ValidateAcquisitionCorpusManifest(m *AcquisitionCorpusManifest) error {
	if m == nil {
		return errors.New("acquisition corpus manifest is nil")
	}
	if strings.TrimSpace(m.CorpusName) == "" {
		return errors.New("corpus_name cannot be empty")
	}
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("version cannot be empty")
	}
	if m.FrozenAt.IsZero() {
		return errors.New("frozen_at must not be zero timestamp")
	}
	if m.TotalEntries != 100 {
		return fmt.Errorf("total_entries must be exactly 100, got %d", m.TotalEntries)
	}
	if len(m.Entries) != 100 {
		return fmt.Errorf("manifest must contain exactly 100 entries, got %d", len(m.Entries))
	}

	seenEntryIDs := make(map[string]struct{}, 100)
	seenAwemeIDs := make(map[string]struct{}, 100)
	seenURLs := make(map[string]struct{}, 100)

	for i, entry := range m.Entries {
		prefix := fmt.Sprintf("entry[%d] (id=%s)", i, entry.EntryID)
		if strings.TrimSpace(entry.EntryID) == "" {
			return fmt.Errorf("%s: entry_id cannot be empty", prefix)
		}
		if _, exists := seenEntryIDs[entry.EntryID]; exists {
			return fmt.Errorf("%s: duplicate entry_id %q", prefix, entry.EntryID)
		}
		seenEntryIDs[entry.EntryID] = struct{}{}

		awemeID := strings.TrimSpace(entry.AwemeID)
		if awemeID == "" {
			return fmt.Errorf("%s: aweme_id cannot be empty", prefix)
		}
		if _, err := strconv.ParseUint(awemeID, 10, 64); err != nil {
			return fmt.Errorf("%s: aweme_id must be numeric: %w", prefix, err)
		}
		if _, exists := seenAwemeIDs[awemeID]; exists {
			return fmt.Errorf("%s: duplicate aweme_id %q", prefix, awemeID)
		}
		seenAwemeIDs[awemeID] = struct{}{}

		rawURL := strings.TrimSpace(entry.CanonicalURL)
		if err := ValidateCanonicalURL(rawURL, awemeID); err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		if _, exists := seenURLs[rawURL]; exists {
			return fmt.Errorf("%s: duplicate canonical_url %q", prefix, rawURL)
		}
		seenURLs[rawURL] = struct{}{}

		if entry.ExpectedDurationMs <= 0 {
			return fmt.Errorf("%s: expected_duration_ms must be positive, got %d", prefix, entry.ExpectedDurationMs)
		}
		if entry.DurationToleranceMs <= 0 {
			return fmt.Errorf("%s: duration_tolerance_ms must be positive, got %d", prefix, entry.DurationToleranceMs)
		}

		// Validate Oracle
		if strings.TrimSpace(entry.Oracle.Method) == "" {
			return fmt.Errorf("%s: oracle method cannot be empty", prefix)
		}
		if entry.Oracle.ValidatedAt.IsZero() {
			return fmt.Errorf("%s: oracle validated_at must not be zero", prefix)
		}
		if err := validateSHA256Hex(entry.Oracle.EvidenceHash); err != nil {
			return fmt.Errorf("%s: oracle evidence_hash: %w", prefix, err)
		}

		if entry.BaselineMediaSHA256 != "" {
			if err := validateSHA256Hex(entry.BaselineMediaSHA256); err != nil {
				return fmt.Errorf("%s: baseline_media_sha256: %w", prefix, err)
			}
		}
	}

	return nil
}

// canonicalAcquisitionEntry represents entry fields for RFC 8785 canonical JSON serialization.
type canonicalAcquisitionEntry struct {
	AwemeID             string `json:"aweme_id"`
	BaselineMediaSHA256 string `json:"baseline_media_sha256,omitempty"`
	CanonicalURL        string `json:"canonical_url"`
	DurationToleranceMs int64  `json:"duration_tolerance_ms"`
	EntryID             string `json:"entry_id"`
	ExpectedDurationMs  int64  `json:"expected_duration_ms"`
	OracleEvidenceHash  string `json:"oracle_evidence_hash"`
	OracleMethod        string `json:"oracle_method"`
	OracleValidatedAt   string `json:"oracle_validated_at"`
	RequiresAudio       bool   `json:"requires_audio"`
}

type canonicalAcquisitionManifest struct {
	CorpusName   string                      `json:"corpus_name"`
	Entries      []canonicalAcquisitionEntry `json:"entries"`
	FrozenAt     string                      `json:"frozen_at"`
	TotalEntries int                         `json:"total_entries"`
	Version      string                      `json:"version"`
}

// Canonicalize produces deterministic RFC 8785 canonical JSON bytes representing the manifest.
// Entries are deterministically sorted by EntryID.
// The ManifestDigest field is excluded from canonicalization to prevent circular hash dependency.
func (m *AcquisitionCorpusManifest) Canonicalize() ([]byte, error) {
	if m == nil {
		return nil, errors.New("manifest is nil")
	}

	canonEntries := make([]canonicalAcquisitionEntry, len(m.Entries))
	for i, e := range m.Entries {
		canonEntries[i] = canonicalAcquisitionEntry{
			AwemeID:             e.AwemeID,
			BaselineMediaSHA256: e.BaselineMediaSHA256,
			CanonicalURL:        e.CanonicalURL,
			DurationToleranceMs: e.DurationToleranceMs,
			EntryID:             e.EntryID,
			ExpectedDurationMs:  e.ExpectedDurationMs,
			OracleEvidenceHash:  strings.ToLower(strings.TrimSpace(e.Oracle.EvidenceHash)),
			OracleMethod:        strings.TrimSpace(e.Oracle.Method),
			OracleValidatedAt:   e.Oracle.ValidatedAt.UTC().Format(time.RFC3339Nano),
			RequiresAudio:       e.RequiresAudio,
		}
	}

	// Sort entries deterministically by EntryID
	sort.Slice(canonEntries, func(i, j int) bool {
		return canonEntries[i].EntryID < canonEntries[j].EntryID
	})

	canon := canonicalAcquisitionManifest{
		CorpusName:   strings.TrimSpace(m.CorpusName),
		Entries:      canonEntries,
		FrozenAt:     m.FrozenAt.UTC().Format(time.RFC3339Nano),
		TotalEntries: m.TotalEntries,
		Version:      strings.TrimSpace(m.Version),
	}

	return json.Marshal(canon)
}

// ComputeManifestDigest calculates the SHA-256 hex digest of the canonical manifest JSON.
func (m *AcquisitionCorpusManifest) ComputeManifestDigest() (string, error) {
	bytes, err := m.Canonicalize()
	if err != nil {
		return "", fmt.Errorf("canonicalize acquisition manifest: %w", err)
	}
	hash := sha256.Sum256(bytes)
	return hex.EncodeToString(hash[:]), nil
}

// VerifyManifestDigest checks that the stored ManifestDigest matches the computed digest.
func (m *AcquisitionCorpusManifest) VerifyManifestDigest() error {
	if m == nil {
		return errors.New("manifest is nil")
	}
	if strings.TrimSpace(m.ManifestDigest) == "" {
		return errors.New("manifest_digest is empty")
	}
	computed, err := m.ComputeManifestDigest()
	if err != nil {
		return fmt.Errorf("compute manifest digest: %w", err)
	}
	if !strings.EqualFold(m.ManifestDigest, computed) {
		return fmt.Errorf("manifest digest mismatch: expected %s, computed %s", m.ManifestDigest, computed)
	}
	return nil
}

// FreezeAcquisitionCorpus creates an immutable AcquisitionCorpusManifest from raw entries.
// It enforces all Issue #70 validation rules, sorts entries, and calculates the ManifestDigest.
func FreezeAcquisitionCorpus(corpusName, version string, frozenAt time.Time, entries []AcquisitionCorpusEntry) (*AcquisitionCorpusManifest, error) {
	if frozenAt.IsZero() {
		return nil, errors.New("frozenAt must be non-zero timestamp")
	}

	// Sort entries by AwemeID deterministically
	sortedEntries := make([]AcquisitionCorpusEntry, len(entries))
	copy(sortedEntries, entries)
	sort.Slice(sortedEntries, func(i, j int) bool {
		return sortedEntries[i].AwemeID < sortedEntries[j].AwemeID
	})

	m := &AcquisitionCorpusManifest{
		CorpusName:   strings.TrimSpace(corpusName),
		Version:      strings.TrimSpace(version),
		FrozenAt:     frozenAt.UTC(),
		TotalEntries: len(sortedEntries),
		Entries:      sortedEntries,
	}

	if err := ValidateAcquisitionCorpusManifest(m); err != nil {
		return nil, fmt.Errorf("validate acquisition manifest: %w", err)
	}

	digest, err := m.ComputeManifestDigest()
	if err != nil {
		return nil, fmt.Errorf("compute manifest digest: %w", err)
	}
	m.ManifestDigest = digest

	return m, nil
}
