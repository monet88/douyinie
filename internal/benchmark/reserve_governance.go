package benchmark

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrMidRunReplacementForbidden = errors.New("mid-run replacement rejected: substitutions allowed only before scored run")
	ErrAssetNotFound              = errors.New("primary asset not found in manifest")
	ErrReserveNotFound            = errors.New("reserve asset not found in manifest")
	ErrCategoryMismatch           = errors.New("replacement must be 1-for-1 within the same primary category")
	ErrReasonRequired             = errors.New("replacement reason is required")
	ErrOperatorRequired           = errors.New("operator ID is required")
)

// ReserveReplacementRecord binds the audited transition of replacing a primary asset with a reserve.
// Invariant (Issue #57 / #71): Replacement is allowed only before a scored run, 1-for-1 within the
// same primary category, with an audit record binding old/new identities, reason, operator,
// timestamp, and manifest digests; mid-run substitution is rejected.
type ReserveReplacementRecord struct {
	ReplacementID       string          `json:"replacement_id"`
	OldAssetID          string          `json:"old_asset_id"`
	NewAssetID          string          `json:"new_asset_id"`
	Category            QualityCategory `json:"category"`
	Reason              string          `json:"reason"`
	OperatorID          string          `json:"operator_id"`
	Timestamp           time.Time       `json:"timestamp"`
	PriorManifestDigest string          `json:"prior_manifest_digest"`
	NewManifestDigest   string          `json:"new_manifest_digest"`
	BenchmarkSessionID  string          `json:"benchmark_session_id,omitempty"`
}

// ReplacePrimaryWithReserve executes governed 1-for-1 replacement of a primary asset with a reserve asset.
// Enforces:
// 1. isRunStarted == false (mid-run substitution is rejected fail-closed).
// 2. Both oldAssetID and reserveAssetID must exist in manifest.
// 3. 1-for-1 within the exact same primary category.
// 4. Non-empty operator and reason.
// 5. Immutable audit record generation binding old/new identities, reason, operator, timestamp, and digests.
func (m *QualityCorpusManifest) ReplacePrimaryWithReserve(
	oldAssetID string,
	reserveAssetID string,
	reason string,
	operatorID string,
	timestamp time.Time,
	isRunStarted bool,
	benchmarkSessionID string,
) (*ReserveReplacementRecord, *QualityCorpusManifest, error) {
	if m == nil {
		return nil, nil, errors.New("nil quality corpus manifest")
	}

	// 1. Mid-run substitution check
	if isRunStarted {
		return nil, nil, fmt.Errorf("%w: cannot replace asset %s with %s after scored run has started", ErrMidRunReplacementForbidden, oldAssetID, reserveAssetID)
	}

	// 2. Operator & reason validation
	trimmedReason := strings.TrimSpace(reason)
	if trimmedReason == "" {
		return nil, nil, ErrReasonRequired
	}
	trimmedOperator := strings.TrimSpace(operatorID)
	if trimmedOperator == "" {
		return nil, nil, ErrOperatorRequired
	}

	if timestamp.IsZero() {
		return nil, nil, errors.New("replacement timestamp is required for deterministic governance audit")
	}

	// 3. Locate old primary asset
	oldIndex := -1
	var oldEntry CorpusAssetEntry
	for i, a := range m.PrimaryAssets {
		if a.AssetID == oldAssetID {
			oldIndex = i
			oldEntry = a
			break
		}
	}
	if oldIndex == -1 {
		return nil, nil, fmt.Errorf("%w: asset_id %q", ErrAssetNotFound, oldAssetID)
	}

	// 4. Locate reserve asset
	reserveIndex := -1
	var reserveEntry CorpusAssetEntry
	for i, r := range m.ReserveAssets {
		if r.AssetID == reserveAssetID {
			reserveIndex = i
			reserveEntry = r
			break
		}
	}
	if reserveIndex == -1 {
		return nil, nil, fmt.Errorf("%w: asset_id %q", ErrReserveNotFound, reserveAssetID)
	}

	// 5. Enforce 1-for-1 within the same primary category
	if oldEntry.PrimaryCategory != reserveEntry.PrimaryCategory {
		return nil, nil, fmt.Errorf("%w: old asset %s is in category %s, reserve asset %s is in category %s",
			ErrCategoryMismatch, oldAssetID, oldEntry.PrimaryCategory, reserveAssetID, reserveEntry.PrimaryCategory)
	}

	priorDigest := m.ManifestDigest
	if priorDigest == "" {
		computed, err := m.ComputeManifestDigest()
		if err != nil {
			return nil, nil, fmt.Errorf("compute prior manifest digest: %w", err)
		}
		priorDigest = computed
	}

	// 6. Build new manifest with replacement
	newPrimaries := make([]CorpusAssetEntry, len(m.PrimaryAssets))
	copy(newPrimaries, m.PrimaryAssets)
	promotedReserve := reserveEntry
	promotedReserve.IsReserve = false
	newPrimaries[oldIndex] = promotedReserve

	// Remove reserve from available reserve list
	newReserves := make([]CorpusAssetEntry, 0, len(m.ReserveAssets)-1)
	for i, r := range m.ReserveAssets {
		if i != reserveIndex {
			newReserves = append(newReserves, r)
		}
	}

	newHistory := make([]ReserveReplacementRecord, len(m.ReplacementHistory))
	copy(newHistory, m.ReplacementHistory)

	replacementID := "rep_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	record := ReserveReplacementRecord{
		ReplacementID:       replacementID,
		OldAssetID:          oldAssetID,
		NewAssetID:          reserveAssetID,
		Category:            oldEntry.PrimaryCategory,
		Reason:              trimmedReason,
		OperatorID:          trimmedOperator,
		Timestamp:           timestamp.UTC().Truncate(time.Millisecond),
		PriorManifestDigest: priorDigest,
		BenchmarkSessionID:  strings.TrimSpace(benchmarkSessionID),
	}

	newManifest := &QualityCorpusManifest{
		CorpusName:         m.CorpusName,
		Version:            m.Version,
		FrozenAt:           m.FrozenAt,
		PrimaryAssets:      newPrimaries,
		ReserveAssets:      newReserves,
		ReplacementHistory: append(newHistory, record),
	}

	newDigest, err := newManifest.ComputeManifestDigest()
	if err != nil {
		return nil, nil, fmt.Errorf("compute new manifest digest after replacement: %w", err)
	}

	// Update record and manifest with final digest
	record.NewManifestDigest = newDigest
	newManifest.ManifestDigest = newDigest
	newManifest.ReplacementHistory[len(newManifest.ReplacementHistory)-1].NewManifestDigest = newDigest

	return &record, newManifest, nil
}
