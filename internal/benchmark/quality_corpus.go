package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/monet88/douyinie/internal/domain"
)

// QualityCategory represents one of the 7 frozen primary challenge categories.
// Invariant (Issue #57 / #71): Exactly 7 primary categories with quotas 3/4/4/3/3/4/3.
type QualityCategory string

const (
	CategoryCleanSingleSpeaker    QualityCategory = "clean_single_speaker"
	CategoryMultiSpeaker          QualityCategory = "multi_speaker"
	CategoryFastDense             QualityCategory = "fast_dense"
	CategoryLoudBGMAmbience       QualityCategory = "loud_bgm_ambience"
	CategoryAccentEntity          QualityCategory = "accent_entity"
	CategoryDenseBurnedInSubtitle QualityCategory = "dense_burned_in_subtitle"
	CategoryDurationPauseStress   QualityCategory = "duration_pause_stress"
)

var AllQualityCategories = []QualityCategory{
	CategoryCleanSingleSpeaker,
	CategoryMultiSpeaker,
	CategoryFastDense,
	CategoryLoudBGMAmbience,
	CategoryAccentEntity,
	CategoryDenseBurnedInSubtitle,
	CategoryDurationPauseStress,
}

var CategoryPrimaryQuotas = map[QualityCategory]int{
	CategoryCleanSingleSpeaker:    3,
	CategoryMultiSpeaker:          4,
	CategoryFastDense:             4,
	CategoryLoudBGMAmbience:       3,
	CategoryAccentEntity:          3,
	CategoryDenseBurnedInSubtitle: 4,
	CategoryDurationPauseStress:   3,
}

const (
	ExpectedPrimaryAssetCount = 24
	ExpectedReserveAssetCount = 7
	MinMultiSpeakerCount      = 6
	MinBurnedInSubtitleCount  = 16
	MaxNoDubVisualCount       = 1
	Min9x16MajorityCount      = 13 // > 24 / 2
	Min3x4PortraitCount       = 2
	MinDuration90sCount       = 2
	MinDurationBucketsCount   = 3
)

var (
	ErrCorpusValidationFailed  = errors.New("quality corpus validation failed")
	ErrInvalidCategory         = errors.New("invalid quality category")
	ErrQuotaMismatch           = errors.New("primary category quota mismatch")
	ErrCorpusMinimaViolated    = errors.New("corpus-wide minima requirement violated")
	ErrPreflightEnvelopeFailed = errors.New("preflight envelope check failed")
	ErrReserveCountMismatch    = errors.New("reserve category count mismatch")
	ErrManifestDigestMismatch  = errors.New("manifest digest mismatch")
)

// NormalizeCategory normalizes slug, kebab, title, or display representations of a category.
func NormalizeCategory(cat string) (QualityCategory, error) {
	c := strings.ToLower(strings.TrimSpace(cat))
	c = strings.ReplaceAll(c, "-", "_")
	c = strings.ReplaceAll(c, " ", "_")

	switch c {
	case "clean_single_speaker", "cleansinglespeaker", "clean_single":
		return CategoryCleanSingleSpeaker, nil
	case "multi_speaker", "multispeaker", "multi":
		return CategoryMultiSpeaker, nil
	case "fast_dense", "fastdense", "fast_dense_speech":
		return CategoryFastDense, nil
	case "loud_bgm_ambience", "loud_bgm", "loudbgmambience", "loud_bgm_noise_ambience":
		return CategoryLoudBGMAmbience, nil
	case "accent_entity", "accententity", "accent_dialect_entity":
		return CategoryAccentEntity, nil
	case "dense_burned_in_subtitle", "denseburnedinsubtitle", "dense_burned_in", "burned_in_subtitle", "burned_in_dialogue_subtitle":
		return CategoryDenseBurnedInSubtitle, nil
	case "duration_pause_stress", "duration_pause", "durationpausestress", "durationpause":
		return CategoryDurationPauseStress, nil
	default:
		return "", fmt.Errorf("%w: unknown category %q", ErrInvalidCategory, cat)
	}
}

// AspectRatioClassification returns the identified aspect ratio for dimensions.
func Is9x16(width, height int) bool {
	if width <= 0 || height <= 0 || height <= width {
		return false
	}
	ratio := float64(width) / float64(height)
	return math.Abs(ratio-(9.0/16.0)) <= 0.025
}

func Is3x4(width, height int) bool {
	if width <= 0 || height <= 0 || height <= width {
		return false
	}
	ratio := float64(width) / float64(height)
	return math.Abs(ratio-(3.0/4.0)) <= 0.025
}

// QualityCorpusAsset represents an in-memory primary or reserve candidate for corpus freeze.
type QualityCorpusAsset struct {
	AssetID         string                   `json:"asset_id"`
	SourceVideoID   string                   `json:"source_video_id"`
	SourceAssetID   string                   `json:"source_asset_id"`
	SHA256          string                   `json:"sha256"`
	PrimaryCategory QualityCategory          `json:"primary_category"`
	Tags            []string                 `json:"tags,omitempty"`
	Preflight       domain.PreflightReport   `json:"preflight"`
	ReferencePack   *ReferenceAnnotationPack `json:"reference_pack"`
	IsReserve       bool                     `json:"is_reserve"`
	NoDub           bool                     `json:"no_dub,omitempty"`
	SpeakerCount    int                      `json:"speaker_count,omitempty"`
}

// HasTag checks if the asset contains a specific secondary tag.
func (a *QualityCorpusAsset) HasTag(tag string) bool {
	target := strings.ToLower(strings.TrimSpace(tag))
	for _, t := range a.Tags {
		if strings.ToLower(strings.TrimSpace(t)) == target {
			return true
		}
	}
	return false
}

// IsMultiSpeaker determines whether this asset counts toward the multi-speaker minimum.
func (a *QualityCorpusAsset) IsMultiSpeaker() bool {
	if a.PrimaryCategory == CategoryMultiSpeaker || a.HasTag("multi_speaker") || a.SpeakerCount >= 2 {
		return true
	}
	if a.ReferencePack != nil && a.ReferencePack.UniqueSpeakerCount() >= 2 {
		return true
	}
	return false
}

// HasBurnedInSubtitles determines whether this asset counts toward the burned-in subtitle minimum.
func (a *QualityCorpusAsset) HasBurnedInSubtitles() bool {
	if a.PrimaryCategory == CategoryDenseBurnedInSubtitle || a.HasTag("burned_in_dialogue_subtitle") || a.HasTag("burned_in_subtitle") {
		return true
	}
	if a.ReferencePack != nil && (a.ReferencePack.HasBurnedInSubtitles || len(a.ReferencePack.SubtitleRegions) > 0) {
		return true
	}
	return false
}

// IsNoDub determines whether this asset is a no-dub visual case.
func (a *QualityCorpusAsset) IsNoDub() bool {
	return a.NoDub || a.HasTag("no_dub") || a.HasTag("no_dub_visual")
}

// ValidatePreflightEnvelope enforces the real product preflight requirements.
// Invariant (Issue #71 Criterion 3): Require every primary asset to pass the real product
// supported-input/preflight envelope; historical five-video fixtures are candidates only
// and receive no grandfathered slots.
func ValidatePreflightEnvelope(report *domain.PreflightReport, isNoDub bool) error {
	if report == nil {
		return fmt.Errorf("%w: preflight report is nil", ErrPreflightEnvelopeFailed)
	}
	if !report.ContainerValid {
		return fmt.Errorf("%w: invalid container: %s", ErrPreflightEnvelopeFailed, strings.Join(report.Errors, "; "))
	}
	if !report.FingerprintMatch {
		return fmt.Errorf("%w: fingerprint match failure", ErrPreflightEnvelopeFailed)
	}
	if report.DurationMs <= 0 && report.DurationSec <= 0 {
		return fmt.Errorf("%w: duration must be positive", ErrPreflightEnvelopeFailed)
	}
	if report.Width <= 0 || report.Height <= 0 {
		return fmt.Errorf("%w: video dimensions invalid (%dx%d)", ErrPreflightEnvelopeFailed, report.Width, report.Height)
	}
	if strings.TrimSpace(report.VideoCodec) == "" {
		return fmt.Errorf("%w: video codec missing", ErrPreflightEnvelopeFailed)
	}
	if !isNoDub && strings.TrimSpace(report.AudioCodec) == "" {
		return fmt.Errorf("%w: audio codec missing for dubbed asset", ErrPreflightEnvelopeFailed)
	}
	if !isNoDub && strings.TrimSpace(report.NormalizedAudioSHA256) == "" {
		return fmt.Errorf("%w: normalized 16kHz audio SHA-256 missing", ErrPreflightEnvelopeFailed)
	}
	return nil
}

// CorpusAssetEntry represents an immutable entry in the QualityCorpusManifest.
type CorpusAssetEntry struct {
	AssetID             string          `json:"asset_id"`
	SourceVideoID       string          `json:"source_video_id"`
	SourceAssetID       string          `json:"source_asset_id"`
	SHA256              string          `json:"sha256"`
	PrimaryCategory     QualityCategory `json:"primary_category"`
	Tags                []string        `json:"tags,omitempty"`
	ReferencePackID     string          `json:"reference_pack_id"`
	ReferencePackDigest string          `json:"reference_pack_digest"`
	DurationMs          int64           `json:"duration_ms"`
	Width               int             `json:"width"`
	Height              int             `json:"height"`
	IsReserve           bool            `json:"is_reserve"`
	NoDub               bool            `json:"no_dub,omitempty"`
}

// QualityCorpusManifest represents the frozen immutable quality corpus definition.
type QualityCorpusManifest struct {
	CorpusName         string                     `json:"corpus_name"`
	Version            string                     `json:"version"`
	FrozenAt           time.Time                  `json:"frozen_at"`
	PrimaryAssets      []CorpusAssetEntry         `json:"primary_assets"`
	ReserveAssets      []CorpusAssetEntry         `json:"reserve_assets"`
	ReplacementHistory []ReserveReplacementRecord `json:"replacement_history,omitempty"`
	ManifestDigest     string                     `json:"manifest_digest"`
}

// ValidateCorpusInputs runs complete validation against the 24 primary and 7 reserve assets.
func ValidateCorpusInputs(primaries []QualityCorpusAsset, reserves []QualityCorpusAsset) error {
	return validateCorpusInputsInternal(primaries, reserves, false)
}

// ValidateDraftCorpusInputs runs complete validation against the 24 primary and 7 reserve assets for draft Task A assets.
func ValidateDraftCorpusInputs(primaries []QualityCorpusAsset, reserves []QualityCorpusAsset) error {
	return validateCorpusInputsInternal(primaries, reserves, true)
}

func validateCorpusInputsInternal(primaries []QualityCorpusAsset, reserves []QualityCorpusAsset, isDraft bool) error {
	var errs []string
	// 1. Primary count
	if len(primaries) != ExpectedPrimaryAssetCount {
		errs = append(errs, fmt.Sprintf("expected exactly %d primary assets, got %d", ExpectedPrimaryAssetCount, len(primaries)))
	}

	// 2. Reserve count
	if len(reserves) != ExpectedReserveAssetCount {
		errs = append(errs, fmt.Sprintf("expected exactly %d reserve assets, got %d", ExpectedReserveAssetCount, len(reserves)))
	}

	// 3. Category quotas on primary assets
	primaryCounts := make(map[QualityCategory]int)
	seenPrimaryIDs := make(map[string]struct{})
	seenPrimarySourceIDs := make(map[string]struct{})
	multiSpeakerCount := 0
	burnedInCount := 0
	noDubCount := 0
	count9x16 := 0
	count3x4 := 0
	countGTE90s := 0
	durationBuckets := make(map[string]int)

	for idx, a := range primaries {
		if strings.TrimSpace(a.AssetID) == "" {
			errs = append(errs, fmt.Sprintf("primary asset #%d has empty asset_id", idx))
		} else {
			if _, exists := seenPrimaryIDs[a.AssetID]; exists {
				errs = append(errs, fmt.Sprintf("duplicate primary asset_id %q", a.AssetID))
			}
			seenPrimaryIDs[a.AssetID] = struct{}{}
		}

		// Invariant (Issue #71): require non-empty immutable source asset identity
		if strings.TrimSpace(a.SourceAssetID) == "" {
			errs = append(errs, fmt.Sprintf("primary asset %s has empty source_asset_id", a.AssetID))
		} else {
			if _, exists := seenPrimarySourceIDs[a.SourceAssetID]; exists {
				errs = append(errs, fmt.Sprintf("duplicate primary source_asset_id %q", a.SourceAssetID))
			}
			seenPrimarySourceIDs[a.SourceAssetID] = struct{}{}
		}
		if strings.TrimSpace(a.SourceVideoID) == "" {
			errs = append(errs, fmt.Sprintf("primary asset %s has empty source_video_id", a.AssetID))
		}

		// Invariant (Issue #71): require valid 64-character lowercase hex SHA-256
		trimmedSHA := strings.ToLower(strings.TrimSpace(a.SHA256))
		if len(trimmedSHA) != 64 {
			errs = append(errs, fmt.Sprintf("primary asset %s has invalid sha256: must be 64-char hex, got len %d", a.AssetID, len(trimmedSHA)))
		} else if _, err := hex.DecodeString(trimmedSHA); err != nil {
			errs = append(errs, fmt.Sprintf("primary asset %s has invalid sha256 hex: %v", a.AssetID, err))
		}

		normCat, err := NormalizeCategory(string(a.PrimaryCategory))
		if err != nil {
			errs = append(errs, fmt.Sprintf("primary asset %s: %v", a.AssetID, err))
		} else {
			primaryCounts[normCat]++
		}

		// Preflight envelope
		isNoDub := a.IsNoDub()
		if isNoDub {
			noDubCount++
		}
		if err := ValidatePreflightEnvelope(&a.Preflight, isNoDub); err != nil {
			errs = append(errs, fmt.Sprintf("primary asset %s preflight: %v", a.AssetID, err))
		}

		// Reference annotation pack
		if a.ReferencePack == nil {
			errs = append(errs, fmt.Sprintf("primary asset %s missing reference annotation pack", a.AssetID))
		} else {
			if strings.TrimSpace(a.ReferencePack.AssetID) != strings.TrimSpace(a.AssetID) {
				errs = append(errs, fmt.Sprintf("primary asset %s reference pack asset_id %q does not match corpus asset_id", a.AssetID, a.ReferencePack.AssetID))
			}
			var packErr error
			if isDraft {
				packErr = ValidateDraftAnnotationPack(a.ReferencePack, isNoDub)
			} else {
				packErr = ValidateReferenceAnnotationPack(a.ReferencePack, isNoDub)
			}
			if packErr != nil {
				errs = append(errs, fmt.Sprintf("primary asset %s reference pack: %v", a.AssetID, packErr))
			}
		}

		// Minima statistics
		if a.IsMultiSpeaker() {
			multiSpeakerCount++
		}
		if a.HasBurnedInSubtitles() {
			burnedInCount++
		}
		if Is9x16(a.Preflight.Width, a.Preflight.Height) {
			count9x16++
		}
		if Is3x4(a.Preflight.Width, a.Preflight.Height) {
			count3x4++
		}

		durMs := a.Preflight.DurationMs
		if durMs == 0 && a.Preflight.DurationSec > 0 {
			durMs = int64(a.Preflight.DurationSec * 1000)
		}
		if durMs >= 90_000 {
			countGTE90s++
		}

		// Duration buckets: <30s, 30s-60s, 60s-90s, >=90s
		switch {
		case durMs < 30_000:
			durationBuckets["<30s"]++
		case durMs < 60_000:
			durationBuckets["30s-60s"]++
		case durMs < 90_000:
			durationBuckets["60s-90s"]++
		default:
			durationBuckets[">=90s"]++
		}
	}

	for cat, expectedQuota := range CategoryPrimaryQuotas {
		actual := primaryCounts[cat]
		if actual != expectedQuota {
			errs = append(errs, fmt.Sprintf("category %s primary quota mismatch: expected %d, got %d", cat, expectedQuota, actual))
		}
	}

	// Corpus-wide minima checks
	if multiSpeakerCount < MinMultiSpeakerCount {
		errs = append(errs, fmt.Sprintf("multi-speaker count (%d) below minimum %d", multiSpeakerCount, MinMultiSpeakerCount))
	}
	if burnedInCount < MinBurnedInSubtitleCount {
		errs = append(errs, fmt.Sprintf("burned-in dialogue subtitle count (%d) below minimum %d", burnedInCount, MinBurnedInSubtitleCount))
	}
	if noDubCount > MaxNoDubVisualCount {
		errs = append(errs, fmt.Sprintf("no-dub visual count (%d) exceeds maximum %d", noDubCount, MaxNoDubVisualCount))
	}
	if count9x16 < Min9x16MajorityCount {
		errs = append(errs, fmt.Sprintf("9:16 aspect ratio count (%d) is not majority (expected at least %d)", count9x16, Min9x16MajorityCount))
	}
	if count3x4 < Min3x4PortraitCount {
		errs = append(errs, fmt.Sprintf("3:4 portrait count (%d) below minimum %d", count3x4, Min3x4PortraitCount))
	}
	if countGTE90s < MinDuration90sCount {
		errs = append(errs, fmt.Sprintf("videos >=90s count (%d) below minimum %d", countGTE90s, MinDuration90sCount))
	}
	if len(durationBuckets) < MinDurationBucketsCount {
		errs = append(errs, fmt.Sprintf("varied duration coverage below minimum buckets: got %d distinct buckets, expected at least %d", len(durationBuckets), MinDurationBucketsCount))
	}

	// 4. Reserves validation: exactly 1 per category
	reserveCounts := make(map[QualityCategory]int)
	seenReserveIDs := make(map[string]struct{})
	seenReserveSourceIDs := make(map[string]struct{})
	for idx, r := range reserves {
		if strings.TrimSpace(r.AssetID) == "" {
			errs = append(errs, fmt.Sprintf("reserve asset #%d has empty asset_id", idx))
		} else {
			if _, exists := seenPrimaryIDs[r.AssetID]; exists {
				errs = append(errs, fmt.Sprintf("reserve asset_id %q collides with primary asset_id", r.AssetID))
			}
			if _, exists := seenReserveIDs[r.AssetID]; exists {
				errs = append(errs, fmt.Sprintf("duplicate reserve asset_id %q", r.AssetID))
			}
			seenReserveIDs[r.AssetID] = struct{}{}
		}

		// Invariant (Issue #71): require non-empty immutable source asset identity
		if strings.TrimSpace(r.SourceAssetID) == "" {
			errs = append(errs, fmt.Sprintf("reserve asset %s has empty source_asset_id", r.AssetID))
		} else {
			if _, exists := seenPrimarySourceIDs[r.SourceAssetID]; exists {
				errs = append(errs, fmt.Sprintf("reserve source_asset_id %q collides with primary source_asset_id", r.SourceAssetID))
			}
			if _, exists := seenReserveSourceIDs[r.SourceAssetID]; exists {
				errs = append(errs, fmt.Sprintf("duplicate reserve source_asset_id %q", r.SourceAssetID))
			}
			seenReserveSourceIDs[r.SourceAssetID] = struct{}{}
		}
		if strings.TrimSpace(r.SourceVideoID) == "" {
			errs = append(errs, fmt.Sprintf("reserve asset %s has empty source_video_id", r.AssetID))
		}

		// Invariant (Issue #71): require valid 64-character lowercase hex SHA-256
		trimmedSHA := strings.ToLower(strings.TrimSpace(r.SHA256))
		if len(trimmedSHA) != 64 {
			errs = append(errs, fmt.Sprintf("reserve asset %s has invalid sha256: must be 64-char hex, got len %d", r.AssetID, len(trimmedSHA)))
		} else if _, err := hex.DecodeString(trimmedSHA); err != nil {
			errs = append(errs, fmt.Sprintf("reserve asset %s has invalid sha256 hex: %v", r.AssetID, err))
		}

		normCat, err := NormalizeCategory(string(r.PrimaryCategory))
		if err != nil {
			errs = append(errs, fmt.Sprintf("reserve asset %s: %v", r.AssetID, err))
		} else {
			reserveCounts[normCat]++
		}

		// Preflight envelope for reserve
		isNoDub := r.IsNoDub()
		if err := ValidatePreflightEnvelope(&r.Preflight, isNoDub); err != nil {
			errs = append(errs, fmt.Sprintf("reserve asset %s preflight: %v", r.AssetID, err))
		}

		// Reference pack for reserve
		if r.ReferencePack == nil {
			errs = append(errs, fmt.Sprintf("reserve asset %s missing reference annotation pack", r.AssetID))
		} else {
			if strings.TrimSpace(r.ReferencePack.AssetID) != strings.TrimSpace(r.AssetID) {
				errs = append(errs, fmt.Sprintf("reserve asset %s reference pack asset_id %q does not match corpus asset_id", r.AssetID, r.ReferencePack.AssetID))
			}
			var packErr error
			if isDraft {
				packErr = ValidateDraftAnnotationPack(r.ReferencePack, isNoDub)
			} else {
				packErr = ValidateReferenceAnnotationPack(r.ReferencePack, isNoDub)
			}
			if packErr != nil {
				errs = append(errs, fmt.Sprintf("reserve asset %s reference pack: %v", r.AssetID, packErr))
			}
		}
	}

	for _, cat := range AllQualityCategories {
		if reserveCounts[cat] != 1 {
			errs = append(errs, fmt.Sprintf("category %s must have exactly 1 reserve asset, got %d", cat, reserveCounts[cat]))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w:\n  - %s", ErrCorpusValidationFailed, strings.Join(errs, "\n  - "))
	}
	return nil
}

// CanonicalizeManifest produces deterministic JSON bytes according to RFC 8785 principles.
func (m *QualityCorpusManifest) Canonicalize() ([]byte, error) {
	if m == nil {
		return nil, errors.New("nil quality corpus manifest")
	}

	// Normalize and sort primary assets by AssetID
	normPrimaries := make([]CorpusAssetEntry, len(m.PrimaryAssets))
	copy(normPrimaries, m.PrimaryAssets)
	for i := range normPrimaries {
		normPrimaries[i].AssetID = strings.TrimSpace(normPrimaries[i].AssetID)
		normPrimaries[i].SourceVideoID = strings.TrimSpace(normPrimaries[i].SourceVideoID)
		normPrimaries[i].SourceAssetID = strings.TrimSpace(normPrimaries[i].SourceAssetID)
		normPrimaries[i].SHA256 = strings.ToLower(strings.TrimSpace(normPrimaries[i].SHA256))
		normPrimaries[i].ReferencePackID = strings.TrimSpace(normPrimaries[i].ReferencePackID)
		normPrimaries[i].ReferencePackDigest = strings.ToLower(strings.TrimSpace(normPrimaries[i].ReferencePackDigest))
		if len(normPrimaries[i].Tags) > 0 {
			tags := make([]string, len(normPrimaries[i].Tags))
			for ti, t := range normPrimaries[i].Tags {
				tags[ti] = strings.ToLower(strings.TrimSpace(t))
			}
			sort.Strings(tags)
			normPrimaries[i].Tags = tags
		}
	}
	sort.Slice(normPrimaries, func(i, j int) bool {
		return normPrimaries[i].AssetID < normPrimaries[j].AssetID
	})

	// Normalize and sort reserve assets by AssetID
	normReserves := make([]CorpusAssetEntry, len(m.ReserveAssets))
	copy(normReserves, m.ReserveAssets)
	for i := range normReserves {
		normReserves[i].AssetID = strings.TrimSpace(normReserves[i].AssetID)
		normReserves[i].SourceVideoID = strings.TrimSpace(normReserves[i].SourceVideoID)
		normReserves[i].SourceAssetID = strings.TrimSpace(normReserves[i].SourceAssetID)
		normReserves[i].SHA256 = strings.ToLower(strings.TrimSpace(normReserves[i].SHA256))
		normReserves[i].ReferencePackID = strings.TrimSpace(normReserves[i].ReferencePackID)
		normReserves[i].ReferencePackDigest = strings.ToLower(strings.TrimSpace(normReserves[i].ReferencePackDigest))
		if len(normReserves[i].Tags) > 0 {
			tags := make([]string, len(normReserves[i].Tags))
			for ti, t := range normReserves[i].Tags {
				tags[ti] = strings.ToLower(strings.TrimSpace(t))
			}
			sort.Strings(tags)
			normReserves[i].Tags = tags
		}
	}
	sort.Slice(normReserves, func(i, j int) bool {
		return normReserves[i].AssetID < normReserves[j].AssetID
	})

	// Normalize replacement history without circular NewManifestDigest
	type canonicalReplacementRecord struct {
		ReplacementID       string          `json:"replacement_id"`
		OldAssetID          string          `json:"old_asset_id"`
		NewAssetID          string          `json:"new_asset_id"`
		Category            QualityCategory `json:"category"`
		Reason              string          `json:"reason"`
		OperatorID          string          `json:"operator_id"`
		Timestamp           time.Time       `json:"timestamp"`
		PriorManifestDigest string          `json:"prior_manifest_digest"`
		BenchmarkSessionID  string          `json:"benchmark_session_id,omitempty"`
	}

	normHistory := make([]canonicalReplacementRecord, len(m.ReplacementHistory))
	for i, h := range m.ReplacementHistory {
		normHistory[i] = canonicalReplacementRecord{
			ReplacementID:       strings.TrimSpace(h.ReplacementID),
			OldAssetID:          strings.TrimSpace(h.OldAssetID),
			NewAssetID:          strings.TrimSpace(h.NewAssetID),
			Category:            h.Category,
			Reason:              strings.TrimSpace(h.Reason),
			OperatorID:          strings.TrimSpace(h.OperatorID),
			PriorManifestDigest: strings.ToLower(strings.TrimSpace(h.PriorManifestDigest)),
			BenchmarkSessionID:  strings.TrimSpace(h.BenchmarkSessionID),
			Timestamp:           h.Timestamp.UTC().Truncate(time.Millisecond),
		}
	}
	sort.Slice(normHistory, func(i, j int) bool {
		if !normHistory[i].Timestamp.Equal(normHistory[j].Timestamp) {
			return normHistory[i].Timestamp.Before(normHistory[j].Timestamp)
		}
		return normHistory[i].ReplacementID < normHistory[j].ReplacementID
	})

	payload := struct {
		CorpusName         string                       `json:"corpus_name"`
		Version            string                       `json:"version"`
		FrozenAt           time.Time                    `json:"frozen_at"`
		PrimaryAssets      []CorpusAssetEntry           `json:"primary_assets"`
		ReserveAssets      []CorpusAssetEntry           `json:"reserve_assets"`
		ReplacementHistory []canonicalReplacementRecord `json:"replacement_history"`
	}{
		CorpusName:         strings.TrimSpace(m.CorpusName),
		Version:            strings.TrimSpace(m.Version),
		FrozenAt:           m.FrozenAt.UTC().Truncate(time.Millisecond),
		PrimaryAssets:      normPrimaries,
		ReserveAssets:      normReserves,
		ReplacementHistory: normHistory,
	}

	return json.Marshal(payload)
}

// ComputeManifestDigest returns the SHA-256 digest of the canonical manifest JSON.
func (m *QualityCorpusManifest) ComputeManifestDigest() (string, error) {
	canonicalBytes, err := m.Canonicalize()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonicalBytes)
	return hex.EncodeToString(hash[:]), nil
}

// VerifyManifestDigest verifies that ManifestDigest matches the computed canonical digest.
func (m *QualityCorpusManifest) VerifyManifestDigest() error {
	expected, err := m.ComputeManifestDigest()
	if err != nil {
		return fmt.Errorf("compute manifest digest: %w", err)
	}
	if !strings.EqualFold(m.ManifestDigest, expected) {
		return fmt.Errorf("%w: recorded %s != computed %s", ErrManifestDigestMismatch, m.ManifestDigest, expected)
	}
	return nil
}

// FreezeQualityCorpus validates all assets and returns a frozen QualityCorpusManifest.
// Invariant (Issue #71): Fail-closed determinism: frozenAt, corpusName, and version are required.
func FreezeQualityCorpus(corpusName, version string, frozenAt time.Time, primaries, reserves []QualityCorpusAsset) (*QualityCorpusManifest, error) {
	if strings.TrimSpace(corpusName) == "" {
		return nil, errors.New("corpus_name is required for deterministic freeze")
	}
	if strings.TrimSpace(version) == "" {
		return nil, errors.New("version is required for deterministic freeze")
	}
	if frozenAt.IsZero() {
		return nil, errors.New("frozen_at timestamp is required for deterministic freeze")
	}
	if err := ValidateCorpusInputs(primaries, reserves); err != nil {
		return nil, fmt.Errorf("validate quality corpus inputs: %w", err)
	}
	primaryEntries := make([]CorpusAssetEntry, len(primaries))
	for i, p := range primaries {
		packDigest := p.ReferencePack.PackDigest
		if packDigest == "" {
			computed, err := p.ReferencePack.ComputePackDigest()
			if err != nil {
				return nil, fmt.Errorf("compute pack digest for primary %s: %w", p.AssetID, err)
			}
			packDigest = computed
		}

		durMs := p.Preflight.DurationMs
		if durMs == 0 && p.Preflight.DurationSec > 0 {
			durMs = int64(p.Preflight.DurationSec * 1000)
		}

		primaryEntries[i] = CorpusAssetEntry{
			AssetID:             p.AssetID,
			SourceVideoID:       p.SourceVideoID,
			SourceAssetID:       p.SourceAssetID,
			SHA256:              p.SHA256,
			PrimaryCategory:     p.PrimaryCategory,
			Tags:                p.Tags,
			ReferencePackID:     p.ReferencePack.PackID,
			ReferencePackDigest: packDigest,
			DurationMs:          durMs,
			Width:               p.Preflight.Width,
			Height:              p.Preflight.Height,
			IsReserve:           false,
			NoDub:               p.IsNoDub(),
		}
	}

	reserveEntries := make([]CorpusAssetEntry, len(reserves))
	for i, r := range reserves {
		packDigest := r.ReferencePack.PackDigest
		if packDigest == "" {
			computed, err := r.ReferencePack.ComputePackDigest()
			if err != nil {
				return nil, fmt.Errorf("compute pack digest for reserve %s: %w", r.AssetID, err)
			}
			packDigest = computed
		}

		durMs := r.Preflight.DurationMs
		if durMs == 0 && r.Preflight.DurationSec > 0 {
			durMs = int64(r.Preflight.DurationSec * 1000)
		}

		reserveEntries[i] = CorpusAssetEntry{
			AssetID:             r.AssetID,
			SourceVideoID:       r.SourceVideoID,
			SourceAssetID:       r.SourceAssetID,
			SHA256:              r.SHA256,
			PrimaryCategory:     r.PrimaryCategory,
			Tags:                r.Tags,
			ReferencePackID:     r.ReferencePack.PackID,
			ReferencePackDigest: packDigest,
			DurationMs:          durMs,
			Width:               r.Preflight.Width,
			Height:              r.Preflight.Height,
			IsReserve:           true,
			NoDub:               r.IsNoDub(),
		}
	}

	manifest := &QualityCorpusManifest{
		CorpusName:         corpusName,
		Version:            version,
		FrozenAt:           frozenAt,
		PrimaryAssets:      primaryEntries,
		ReserveAssets:      reserveEntries,
		ReplacementHistory: nil,
	}

	digest, err := manifest.ComputeManifestDigest()
	if err != nil {
		return nil, fmt.Errorf("compute frozen manifest digest: %w", err)
	}
	manifest.ManifestDigest = digest

	return manifest, nil
}
