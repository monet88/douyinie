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

const (
	ExpectedTotalStrataCount = 28 // 2 profiles x 7 categories x 2 languages
)

var (
	AuditProfiles        = []string{"local", "hybrid"}
	AuditTargetLanguages = []string{"vi", "en"}
)

// AuditStratum uniquely identifies a (profile, primary_category, target_language) stratum.
// Invariant (Issue #57 / #71): Nominal plan size is 28 audits.
type AuditStratum struct {
	Profile        string          `json:"profile"`         // "local", "hybrid"
	Category       QualityCategory `json:"category"`        // e.g. "clean_single_speaker"
	TargetLanguage string          `json:"target_language"` // "vi", "en"
}

// Key returns a canonical string representation for maps and sorting.
func (s AuditStratum) Key() string {
	return fmt.Sprintf("%s:%s:%s",
		strings.ToLower(strings.TrimSpace(s.Profile)),
		strings.ToLower(strings.TrimSpace(string(s.Category))),
		strings.ToLower(strings.TrimSpace(s.TargetLanguage)),
	)
}

// CandidateAssetRef carries both the benchmark-local and immutable source asset IDs.
type CandidateAssetRef struct {
	AssetID       string `json:"asset_id"`
	SourceAssetID string `json:"source_asset_id"`
}

// CandidateRanking records the deterministic SHA-256 rank of an asset within a stratum.
type CandidateRanking struct {
	AssetID       string `json:"asset_id"`
	SourceAssetID string `json:"source_asset_id"`
	RankHex       string `json:"rank_hex"` // 64-char lowercase hex SHA-256
	Rank          int    `json:"rank"`     // 1-based rank (1 is selected)
}

// StratumAuditSelection records the chosen candidate for one stratum.
type StratumAuditSelection struct {
	Stratum               AuditStratum       `json:"stratum"`
	SelectedAssetID       string             `json:"selected_asset_id"`
	SelectedSourceAssetID string             `json:"selected_source_asset_id"`
	SelectionRankHex      string             `json:"selection_rank_hex"`
	CandidateCount        int                `json:"candidate_count"`
	AllRankings           []CandidateRanking `json:"all_rankings,omitempty"`
}

// StratifiedAuditPlan represents the reproducible 28-stratum automated PASS audit plan.
// Invariant (Issue #71 Criteria 8 & 9):
// - Selects one automated PASS for each (profile, primary_category, target_language) stratum.
// - Uses SHA-256 ranking over corpus-manifest identity + stratum + source-asset identity.
// - Algorithm does not require a secret/HMAC and is byte-for-byte reproducible.
type StratifiedAuditPlan struct {
	ManifestDigest      string                           `json:"manifest_digest"`
	CreatedAt           time.Time                        `json:"created_at"`
	TotalStrata         int                              `json:"total_strata"` // 28
	Selections          []StratumAuditSelection          `json:"selections"`
	SelectionsByStratum map[string]StratumAuditSelection `json:"selections_by_stratum"`
	PlanDigest          string                           `json:"plan_digest,omitempty"`
}

// ComputeRankHash computes the deterministic SHA-256 hash for ranking an asset in a stratum.
// Formula: sha256(manifest_digest + ":" + profile + ":" + category + ":" + target_language + ":" + source_asset_id)
// Invariant (Issue #71 Criterion 8 & 9): Ranks by immutable source-asset identity, no HMAC secret, byte-for-byte reproducible.
func ComputeRankHash(manifestDigest, profile, category, targetLang, sourceAssetID string) string {
	input := fmt.Sprintf("%s:%s:%s:%s:%s",
		strings.ToLower(strings.TrimSpace(manifestDigest)),
		strings.ToLower(strings.TrimSpace(profile)),
		strings.ToLower(strings.TrimSpace(category)),
		strings.ToLower(strings.TrimSpace(targetLang)),
		strings.TrimSpace(sourceAssetID),
	)
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])
}

// RankCandidates ranks candidate assets deterministically by their SHA-256 score using immutable SourceAssetID.
func RankCandidates(manifestDigest string, stratum AuditStratum, candidates []CandidateAssetRef) []CandidateRanking {
	uniqueSources := make(map[string]CandidateAssetRef)
	for _, c := range candidates {
		srcID := strings.TrimSpace(c.SourceAssetID)
		if srcID != "" {
			if _, exists := uniqueSources[srcID]; !exists {
				uniqueSources[srcID] = CandidateAssetRef{
					AssetID:       strings.TrimSpace(c.AssetID),
					SourceAssetID: srcID,
				}
			}
		}
	}

	rankings := make([]CandidateRanking, 0, len(uniqueSources))
	for srcID, ref := range uniqueSources {
		rankHex := ComputeRankHash(manifestDigest, stratum.Profile, string(stratum.Category), stratum.TargetLanguage, srcID)
		rankings = append(rankings, CandidateRanking{
			AssetID:       ref.AssetID,
			SourceAssetID: ref.SourceAssetID,
			RankHex:       rankHex,
		})
	}

	// Sort ascending by RankHex; break ties by SourceAssetID
	sort.Slice(rankings, func(i, j int) bool {
		if rankings[i].RankHex != rankings[j].RankHex {
			return rankings[i].RankHex < rankings[j].RankHex
		}
		return rankings[i].SourceAssetID < rankings[j].SourceAssetID
	})

	for i := range rankings {
		rankings[i].Rank = i + 1
	}

	return rankings
}

// GenerateStratifiedAuditPlan creates the nominal 28-stratum audit plan from a frozen QualityCorpusManifest.
// Invariant (Issue #71): Byte-for-byte reproducible from the same frozen inputs. Derives CreatedAt from manifest.FrozenAt.
func GenerateStratifiedAuditPlan(manifest *QualityCorpusManifest) (*StratifiedAuditPlan, error) {
	if manifest == nil {
		return nil, errors.New("nil quality corpus manifest")
	}
	if manifest.FrozenAt.IsZero() {
		return nil, errors.New("manifest frozen_at timestamp is required for deterministic audit plan generation")
	}
	manifestDigest := strings.ToLower(strings.TrimSpace(manifest.ManifestDigest))
	if manifestDigest == "" {
		computed, err := manifest.ComputeManifestDigest()
		if err != nil {
			return nil, fmt.Errorf("compute manifest digest for audit plan: %w", err)
		}
		manifestDigest = computed
	}

	// Group primary assets by category with both AssetID and SourceAssetID
	candidatesByCategory := make(map[QualityCategory][]CandidateAssetRef)
	for _, a := range manifest.PrimaryAssets {
		normCat, err := NormalizeCategory(string(a.PrimaryCategory))
		if err != nil {
			return nil, fmt.Errorf("normalize category for asset %s: %w", a.AssetID, err)
		}
		candidatesByCategory[normCat] = append(candidatesByCategory[normCat], CandidateAssetRef{
			AssetID:       a.AssetID,
			SourceAssetID: a.SourceAssetID,
		})
	}
	selections := make([]StratumAuditSelection, 0, ExpectedTotalStrataCount)
	selectionsByStratum := make(map[string]StratumAuditSelection, ExpectedTotalStrataCount)

	// Enumerate all 28 strata deterministically in ordered loops
	for _, profile := range AuditProfiles {
		for _, cat := range AllQualityCategories {
			for _, lang := range AuditTargetLanguages {
				stratum := AuditStratum{
					Profile:        profile,
					Category:       cat,
					TargetLanguage: lang,
				}
				candidateAssets := candidatesByCategory[cat]
				if len(candidateAssets) == 0 {
					return nil, fmt.Errorf("stratum %s has 0 candidate assets in manifest", stratum.Key())
				}

				rankings := RankCandidates(manifestDigest, stratum, candidateAssets)
				topCandidate := rankings[0]

				selection := StratumAuditSelection{
					Stratum:               stratum,
					SelectedAssetID:       topCandidate.AssetID,
					SelectedSourceAssetID: topCandidate.SourceAssetID,
					SelectionRankHex:      topCandidate.RankHex,
					CandidateCount:        len(rankings),
					AllRankings:           rankings,
				}
				selections = append(selections, selection)
				selectionsByStratum[stratum.Key()] = selection
			}
		}
	}

	plan := &StratifiedAuditPlan{
		ManifestDigest:      manifestDigest,
		CreatedAt:           manifest.FrozenAt.UTC().Truncate(time.Millisecond),
		TotalStrata:         len(selections),
		Selections:          selections,
		SelectionsByStratum: selectionsByStratum,
	}

	planDigest, err := plan.ComputePlanDigest()
	if err != nil {
		return nil, fmt.Errorf("compute plan digest: %w", err)
	}
	plan.PlanDigest = planDigest

	return plan, nil
}

// PassAuditCandidate captures execution evidence for an automated PASS case candidate.
type PassAuditCandidate struct {
	Profile        string          `json:"profile"`
	Category       QualityCategory `json:"category"`
	TargetLanguage string          `json:"target_language"`
	AssetID        string          `json:"asset_id"`
	SourceAssetID  string          `json:"source_asset_id"`
	CaseID         string          `json:"case_id,omitempty"`
	AutomatedQC    string          `json:"automated_qc"` // "PASS", "REVIEW_REQUIRED", "FAIL"
}

// SelectPassAudits selects one PASS audit for each of the 28 strata from actual candidate executions.
// Invariant (Issue #57 / #71): Filter to automated PASS executions, then apply deterministic SHA-256 ranking over SourceAssetID.
func SelectPassAudits(manifestDigest string, candidates []PassAuditCandidate, planTimestamp time.Time) (*StratifiedAuditPlan, error) {
	if planTimestamp.IsZero() {
		return nil, errors.New("plan_timestamp is required for deterministic pass audit selection")
	}
	manifestDigest = strings.ToLower(strings.TrimSpace(manifestDigest))
	if manifestDigest == "" {
		return nil, errors.New("manifest digest is required for pass audit selection")
	}

	// Filter and group PASS candidates by stratum
	candidatesByStratum := make(map[string][]CandidateAssetRef)
	for _, c := range candidates {
		if strings.ToUpper(strings.TrimSpace(c.AutomatedQC)) != "PASS" {
			continue
		}
		normCat, err := NormalizeCategory(string(c.Category))
		if err != nil {
			continue
		}
		stratum := AuditStratum{
			Profile:        strings.ToLower(strings.TrimSpace(c.Profile)),
			Category:       normCat,
			TargetLanguage: strings.ToLower(strings.TrimSpace(c.TargetLanguage)),
		}
		candidatesByStratum[stratum.Key()] = append(candidatesByStratum[stratum.Key()], CandidateAssetRef{
			AssetID:       c.AssetID,
			SourceAssetID: c.SourceAssetID,
		})
	}

	selections := make([]StratumAuditSelection, 0, ExpectedTotalStrataCount)
	selectionsByStratum := make(map[string]StratumAuditSelection, ExpectedTotalStrataCount)

	for _, profile := range AuditProfiles {
		for _, cat := range AllQualityCategories {
			for _, lang := range AuditTargetLanguages {
				stratum := AuditStratum{
					Profile:        profile,
					Category:       cat,
					TargetLanguage: lang,
				}
				passAssets := candidatesByStratum[stratum.Key()]
				if len(passAssets) == 0 {
					// No automated PASS in this stratum
					continue
				}

				rankings := RankCandidates(manifestDigest, stratum, passAssets)
				topCandidate := rankings[0]

				selection := StratumAuditSelection{
					Stratum:               stratum,
					SelectedAssetID:       topCandidate.AssetID,
					SelectedSourceAssetID: topCandidate.SourceAssetID,
					SelectionRankHex:      topCandidate.RankHex,
					CandidateCount:        len(rankings),
					AllRankings:           rankings,
				}

				selections = append(selections, selection)
				selectionsByStratum[stratum.Key()] = selection
			}
		}
	}

	plan := &StratifiedAuditPlan{
		ManifestDigest:      manifestDigest,
		CreatedAt:           planTimestamp.UTC().Truncate(time.Millisecond),
		TotalStrata:         len(selections),
		Selections:          selections,
		SelectionsByStratum: selectionsByStratum,
	}

	planDigest, err := plan.ComputePlanDigest()
	if err != nil {
		return nil, fmt.Errorf("compute plan digest: %w", err)
	}
	plan.PlanDigest = planDigest

	return plan, nil
}

// Canonicalize produces deterministic JSON bytes of the plan according to RFC 8785 principles.
func (p *StratifiedAuditPlan) Canonicalize() ([]byte, error) {
	if p == nil {
		return nil, errors.New("nil stratified audit plan")
	}

	normSelections := make([]StratumAuditSelection, len(p.Selections))
	copy(normSelections, p.Selections)
	sort.Slice(normSelections, func(i, j int) bool {
		return normSelections[i].Stratum.Key() < normSelections[j].Stratum.Key()
	})

	payload := struct {
		ManifestDigest string                  `json:"manifest_digest"`
		CreatedAt      time.Time               `json:"created_at"`
		TotalStrata    int                     `json:"total_strata"`
		Selections     []StratumAuditSelection `json:"selections"`
	}{
		ManifestDigest: strings.ToLower(strings.TrimSpace(p.ManifestDigest)),
		CreatedAt:      p.CreatedAt.UTC().Truncate(time.Millisecond),
		TotalStrata:    p.TotalStrata,
		Selections:     normSelections,
	}

	return json.Marshal(payload)
}

// ComputePlanDigest computes the SHA-256 hash of the canonical plan JSON.
func (p *StratifiedAuditPlan) ComputePlanDigest() (string, error) {
	canonicalBytes, err := p.Canonicalize()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonicalBytes)
	return hex.EncodeToString(hash[:]), nil
}

// VerifyPlanDigest verifies that PlanDigest matches the computed canonical digest.
func (p *StratifiedAuditPlan) VerifyPlanDigest() error {
	if strings.TrimSpace(p.PlanDigest) == "" {
		return errors.New("empty plan digest")
	}
	expected, err := p.ComputePlanDigest()
	if err != nil {
		return fmt.Errorf("compute plan digest: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(p.PlanDigest), expected) {
		return fmt.Errorf("%w: recorded %s != computed %s", ErrManifestDigestMismatch, p.PlanDigest, expected)
	}
	return nil
}
