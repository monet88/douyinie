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

var (
	ErrInvalidAnnotationPack = errors.New("invalid reference annotation pack")
	ErrMissingAdjudication   = errors.New("reference annotation pack missing secondary adjudication")
	ErrIncompleteTranscript  = errors.New("reference annotation pack transcript incomplete or invalid")
	ErrIncompleteSemanticRef = errors.New("reference annotation pack missing semantic references")
	ErrIncompleteCritical    = errors.New("reference annotation pack critical items incomplete")
	ErrIncompleteSubtitles   = errors.New("reference annotation pack subtitle regions incomplete")
)

// SecondaryAdjudication records verification by an independent secondary adjudicator.
// Invariant (Issue #57 / #71): Ground truth requires secondary adjudication rather
// than relying on a single annotator.
type SecondaryAdjudication struct {
	AdjudicatedBy string    `json:"adjudicated_by"`
	AdjudicatedAt time.Time `json:"adjudicated_at"`
	Status        string    `json:"status"` // "APPROVED", "VERIFIED"
	Notes         string    `json:"notes,omitempty"`
}

// ReferenceWordAlignment captures phoneme/word-level time alignment.
type ReferenceWordAlignment struct {
	Word    string `json:"word"`
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
}

// ReferenceTranscriptSegment captures one gold speech segment with speaker attribution.
type ReferenceTranscriptSegment struct {
	SegmentID   string                   `json:"segment_id"`
	SpeakerID   string                   `json:"speaker_id"`
	ChineseText string                   `json:"chinese_text"`
	StartMs     int64                    `json:"start_ms"`
	EndMs       int64                    `json:"end_ms"`
	Words       []ReferenceWordAlignment `json:"words,omitempty"`
}

// ReferenceCriticalItem represents a domain-critical entity (names, numbers, negation, facts).
// Invariant (Issue #57 / #71): zero critical meaning errors gate requires explicit ground-truth items.
type ReferenceCriticalItem struct {
	Type        string `json:"type"` // "name", "number", "negation", "fact", "entity"
	SourceText  string `json:"source_text"`
	TargetRefVI string `json:"target_ref_vi"`
	TargetRefEN string `json:"target_ref_en"`
	SegmentID   string `json:"segment_id,omitempty"`
	Notes       string `json:"notes,omitempty"`
}

// SemanticReference captures VI and EN gold/reference translations for a transcript segment.
type SemanticReference struct {
	SegmentID                string   `json:"segment_id"`
	ReferenceVI              string   `json:"reference_vi"`
	ReferenceEN              string   `json:"reference_en"`
	AcceptableAlternativesVI []string `json:"acceptable_alternatives_vi,omitempty"`
	AcceptableAlternativesEN []string `json:"acceptable_alternatives_en,omitempty"`
}

// SubtitleBoundingBox captures normalized [0.0, 1.0] bounding box coordinates.
type SubtitleBoundingBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// ReferenceSubtitleRegion tracks burned-in dialogue subtitle regions.
type ReferenceSubtitleRegion struct {
	RegionID    string              `json:"region_id"`
	StartMs     int64               `json:"start_ms"`
	EndMs       int64               `json:"end_ms"`
	Box         SubtitleBoundingBox `json:"box"`
	ChineseText string              `json:"chinese_text"`
}

// ReferenceAnnotationPack provides complete secondary-adjudicated ground truth for a quality asset.
type ReferenceAnnotationPack struct {
	PackID               string                       `json:"pack_id"`
	AssetID              string                       `json:"asset_id"`
	Adjudication         SecondaryAdjudication        `json:"adjudication"`
	ChineseTranscript    []ReferenceTranscriptSegment `json:"chinese_transcript"`
	CriticalItems        []ReferenceCriticalItem      `json:"critical_items"`
	SemanticReferences   []SemanticReference          `json:"semantic_references"`
	HasBurnedInSubtitles bool                         `json:"has_burned_in_subtitles"`
	SubtitleRegions      []ReferenceSubtitleRegion    `json:"subtitle_regions,omitempty"`
	PackDigest           string                       `json:"pack_digest,omitempty"`
}

// UniqueSpeakerCount returns the number of distinct speakers in the Chinese transcript.
func (p *ReferenceAnnotationPack) UniqueSpeakerCount() int {
	if p == nil {
		return 0
	}
	speakers := make(map[string]struct{})
	for _, seg := range p.ChineseTranscript {
		spk := strings.TrimSpace(seg.SpeakerID)
		if spk != "" {
			speakers[spk] = struct{}{}
		}
	}
	return len(speakers)
}

// Canonicalize produces deterministic JSON bytes of the annotation pack according to RFC 8785 principles.
func (p *ReferenceAnnotationPack) Canonicalize() ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil annotation pack", ErrInvalidAnnotationPack)
	}

	// Normalize transcript segments and sort by StartMs, then EndMs, then SegmentID
	normTranscript := make([]ReferenceTranscriptSegment, len(p.ChineseTranscript))
	copy(normTranscript, p.ChineseTranscript)
	for i := range normTranscript {
		normTranscript[i].SegmentID = strings.TrimSpace(normTranscript[i].SegmentID)
		normTranscript[i].SpeakerID = strings.TrimSpace(normTranscript[i].SpeakerID)
		normTranscript[i].ChineseText = strings.TrimSpace(normTranscript[i].ChineseText)
		if len(normTranscript[i].Words) > 0 {
			words := make([]ReferenceWordAlignment, len(normTranscript[i].Words))
			copy(words, normTranscript[i].Words)
			for wi := range words {
				words[wi].Word = strings.TrimSpace(words[wi].Word)
			}
			sort.Slice(words, func(a, b int) bool {
				if words[a].StartMs != words[b].StartMs {
					return words[a].StartMs < words[b].StartMs
				}
				if words[a].EndMs != words[b].EndMs {
					return words[a].EndMs < words[b].EndMs
				}
				return words[a].Word < words[b].Word
			})
			normTranscript[i].Words = words
		}
	}
	sort.Slice(normTranscript, func(i, j int) bool {
		if normTranscript[i].StartMs != normTranscript[j].StartMs {
			return normTranscript[i].StartMs < normTranscript[j].StartMs
		}
		if normTranscript[i].EndMs != normTranscript[j].EndMs {
			return normTranscript[i].EndMs < normTranscript[j].EndMs
		}
		return normTranscript[i].SegmentID < normTranscript[j].SegmentID
	})

	// Normalize critical items and sort
	normCritical := make([]ReferenceCriticalItem, len(p.CriticalItems))
	copy(normCritical, p.CriticalItems)
	for i := range normCritical {
		normCritical[i].Type = strings.ToLower(strings.TrimSpace(normCritical[i].Type))
		normCritical[i].SourceText = strings.TrimSpace(normCritical[i].SourceText)
		normCritical[i].TargetRefVI = strings.TrimSpace(normCritical[i].TargetRefVI)
		normCritical[i].TargetRefEN = strings.TrimSpace(normCritical[i].TargetRefEN)
		normCritical[i].SegmentID = strings.TrimSpace(normCritical[i].SegmentID)
		normCritical[i].Notes = strings.TrimSpace(normCritical[i].Notes)
	}
	sort.Slice(normCritical, func(i, j int) bool {
		if normCritical[i].Type != normCritical[j].Type {
			return normCritical[i].Type < normCritical[j].Type
		}
		if normCritical[i].SourceText != normCritical[j].SourceText {
			return normCritical[i].SourceText < normCritical[j].SourceText
		}
		return normCritical[i].SegmentID < normCritical[j].SegmentID
	})

	// Normalize semantic references and sort
	normSemantic := make([]SemanticReference, len(p.SemanticReferences))
	copy(normSemantic, p.SemanticReferences)
	for i := range normSemantic {
		normSemantic[i].SegmentID = strings.TrimSpace(normSemantic[i].SegmentID)
		normSemantic[i].ReferenceVI = strings.TrimSpace(normSemantic[i].ReferenceVI)
		normSemantic[i].ReferenceEN = strings.TrimSpace(normSemantic[i].ReferenceEN)
		if len(normSemantic[i].AcceptableAlternativesVI) > 0 {
			altsVI := make([]string, len(normSemantic[i].AcceptableAlternativesVI))
			copy(altsVI, normSemantic[i].AcceptableAlternativesVI)
			sort.Strings(altsVI)
			normSemantic[i].AcceptableAlternativesVI = altsVI
		}
		if len(normSemantic[i].AcceptableAlternativesEN) > 0 {
			altsEN := make([]string, len(normSemantic[i].AcceptableAlternativesEN))
			copy(altsEN, normSemantic[i].AcceptableAlternativesEN)
			sort.Strings(altsEN)
			normSemantic[i].AcceptableAlternativesEN = altsEN
		}
	}
	sort.Slice(normSemantic, func(i, j int) bool {
		return normSemantic[i].SegmentID < normSemantic[j].SegmentID
	})

	// Normalize subtitle regions and sort
	normSubtitles := make([]ReferenceSubtitleRegion, len(p.SubtitleRegions))
	copy(normSubtitles, p.SubtitleRegions)
	for i := range normSubtitles {
		normSubtitles[i].RegionID = strings.TrimSpace(normSubtitles[i].RegionID)
		normSubtitles[i].ChineseText = strings.TrimSpace(normSubtitles[i].ChineseText)
	}
	sort.Slice(normSubtitles, func(i, j int) bool {
		if normSubtitles[i].StartMs != normSubtitles[j].StartMs {
			return normSubtitles[i].StartMs < normSubtitles[j].StartMs
		}
		if normSubtitles[i].EndMs != normSubtitles[j].EndMs {
			return normSubtitles[i].EndMs < normSubtitles[j].EndMs
		}
		return normSubtitles[i].RegionID < normSubtitles[j].RegionID
	})

	// Canonical payload excludes PackDigest to avoid circular hash dependency
	payload := struct {
		PackID               string                       `json:"pack_id"`
		AssetID              string                       `json:"asset_id"`
		Adjudication         SecondaryAdjudication        `json:"adjudication"`
		ChineseTranscript    []ReferenceTranscriptSegment `json:"chinese_transcript"`
		CriticalItems        []ReferenceCriticalItem      `json:"critical_items"`
		SemanticReferences   []SemanticReference          `json:"semantic_references"`
		HasBurnedInSubtitles bool                         `json:"has_burned_in_subtitles"`
		SubtitleRegions      []ReferenceSubtitleRegion    `json:"subtitle_regions"`
	}{
		PackID:  strings.TrimSpace(p.PackID),
		AssetID: strings.TrimSpace(p.AssetID),
		Adjudication: SecondaryAdjudication{
			AdjudicatedBy: strings.TrimSpace(p.Adjudication.AdjudicatedBy),
			AdjudicatedAt: p.Adjudication.AdjudicatedAt.UTC().Truncate(time.Millisecond),
			Status:        strings.ToUpper(strings.TrimSpace(p.Adjudication.Status)),
			Notes:         strings.TrimSpace(p.Adjudication.Notes),
		},
		ChineseTranscript:    normTranscript,
		CriticalItems:        normCritical,
		SemanticReferences:   normSemantic,
		HasBurnedInSubtitles: p.HasBurnedInSubtitles,
		SubtitleRegions:      normSubtitles,
	}

	return json.Marshal(payload)
}

// ComputePackDigest computes the SHA-256 hash of the canonical JSON bytes.
func (p *ReferenceAnnotationPack) ComputePackDigest() (string, error) {
	canonicalBytes, err := p.Canonicalize()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonicalBytes)
	return hex.EncodeToString(hash[:]), nil
}

// VerifyPackDigest verifies that PackDigest matches the computed canonical digest.
func (p *ReferenceAnnotationPack) VerifyPackDigest() error {
	if strings.TrimSpace(p.PackDigest) == "" {
		return errors.New("empty pack digest")
	}
	expected, err := p.ComputePackDigest()
	if err != nil {
		return fmt.Errorf("compute pack digest: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(p.PackDigest), expected) {
		return fmt.Errorf("%w: recorded %s != computed %s", ErrInvalidAnnotationPack, p.PackDigest, expected)
	}
	return nil
}

// ValidateReferenceAnnotationPack enforces all contract requirements from Issue #71.
func ValidateReferenceAnnotationPack(pack *ReferenceAnnotationPack, isNoDub bool) error {
	if pack == nil {
		return fmt.Errorf("%w: pack is nil", ErrInvalidAnnotationPack)
	}
	if strings.TrimSpace(pack.PackID) == "" {
		return fmt.Errorf("%w: missing pack_id", ErrInvalidAnnotationPack)
	}
	if strings.TrimSpace(pack.AssetID) == "" {
		return fmt.Errorf("%w: missing asset_id", ErrInvalidAnnotationPack)
	}

	// 1. Secondary Adjudication validation
	if strings.TrimSpace(pack.Adjudication.AdjudicatedBy) == "" {
		return fmt.Errorf("%w: adjudicated_by is required", ErrMissingAdjudication)
	}
	if pack.Adjudication.AdjudicatedAt.IsZero() {
		return fmt.Errorf("%w: adjudicated_at timestamp is required", ErrMissingAdjudication)
	}
	status := strings.ToUpper(strings.TrimSpace(pack.Adjudication.Status))
	if status != "APPROVED" && status != "VERIFIED" {
		return fmt.Errorf("%w: adjudication status must be APPROVED or VERIFIED, got %q", ErrMissingAdjudication, pack.Adjudication.Status)
	}

	return validateAnnotationPackContent(pack, isNoDub)
}

// ValidateDraftAnnotationPack enforces all contract requirements on draft content
// without self-certifying secondary adjudication approval (Task A contract).
func ValidateDraftAnnotationPack(pack *ReferenceAnnotationPack, isNoDub bool) error {
	if pack == nil {
		return fmt.Errorf("%w: pack is nil", ErrInvalidAnnotationPack)
	}
	if strings.TrimSpace(pack.PackID) == "" {
		return fmt.Errorf("%w: missing pack_id", ErrInvalidAnnotationPack)
	}
	if strings.TrimSpace(pack.AssetID) == "" {
		return fmt.Errorf("%w: missing asset_id", ErrInvalidAnnotationPack)
	}

	status := strings.ToUpper(strings.TrimSpace(pack.Adjudication.Status))
	if status == "" {
		return fmt.Errorf("%w: adjudication status is required", ErrMissingAdjudication)
	}

	return validateAnnotationPackContent(pack, isNoDub)
}

func validateAnnotationPackContent(pack *ReferenceAnnotationPack, isNoDub bool) error {
	// 2. Transcript validation
	if len(pack.ChineseTranscript) == 0 && !isNoDub {
		return fmt.Errorf("%w: non-no-dub video must have at least one speech segment", ErrIncompleteTranscript)
	}

	var lastStart int64 = -1
	segmentMap := make(map[string]struct{})
	for idx, seg := range pack.ChineseTranscript {
		if strings.TrimSpace(seg.SegmentID) == "" {
			return fmt.Errorf("%w: segment %d has empty segment_id", ErrIncompleteTranscript, idx)
		}
		if _, exists := segmentMap[seg.SegmentID]; exists {
			return fmt.Errorf("%w: duplicate segment_id %q", ErrIncompleteTranscript, seg.SegmentID)
		}
		segmentMap[seg.SegmentID] = struct{}{}

		if strings.TrimSpace(seg.SpeakerID) == "" {
			return fmt.Errorf("%w: segment %s has empty speaker_id", ErrIncompleteTranscript, seg.SegmentID)
		}
		if strings.TrimSpace(seg.ChineseText) == "" {
			return fmt.Errorf("%w: segment %s has empty chinese_text", ErrIncompleteTranscript, seg.SegmentID)
		}
		if seg.StartMs < 0 {
			return fmt.Errorf("%w: segment %s start_ms cannot be negative (%d)", ErrIncompleteTranscript, seg.SegmentID, seg.StartMs)
		}
		if seg.EndMs <= seg.StartMs {
			return fmt.Errorf("%w: segment %s end_ms (%d) must be strictly greater than start_ms (%d)", ErrIncompleteTranscript, seg.SegmentID, seg.EndMs, seg.StartMs)
		}
		if seg.StartMs < lastStart {
			return fmt.Errorf("%w: segment %s starts before previous segment (out of chronological order)", ErrIncompleteTranscript, seg.SegmentID)
		}
		lastStart = seg.StartMs

		// Invariant (Issue #71): enforce gold timing/alignment contract for dubbed speech
		if !isNoDub {
			if len(seg.Words) == 0 {
				return fmt.Errorf("%w: segment %s missing required word alignments", ErrIncompleteTranscript, seg.SegmentID)
			}
			var lastWordStart int64 = -1
			for wi, w := range seg.Words {
				if strings.TrimSpace(w.Word) == "" {
					return fmt.Errorf("%w: segment %s word %d has empty word text", ErrIncompleteTranscript, seg.SegmentID, wi)
				}
				if w.StartMs < seg.StartMs || w.EndMs > seg.EndMs {
					return fmt.Errorf("%w: segment %s word %d [%d, %d] outside segment bounds [%d, %d]",
						ErrIncompleteTranscript, seg.SegmentID, wi, w.StartMs, w.EndMs, seg.StartMs, seg.EndMs)
				}
				if w.EndMs < w.StartMs {
					return fmt.Errorf("%w: segment %s word %d has end_ms %d < start_ms %d",
						ErrIncompleteTranscript, seg.SegmentID, wi, w.EndMs, w.StartMs)
				}
				if w.StartMs < lastWordStart {
					return fmt.Errorf("%w: segment %s word %d start_ms %d out of chronological order (precedes %d)",
						ErrIncompleteTranscript, seg.SegmentID, wi, w.StartMs, lastWordStart)
				}
				lastWordStart = w.StartMs
			}
		}
	}

	// 3. Critical items validation
	validCriticalTypes := map[string]struct{}{
		"name":     {},
		"number":   {},
		"negation": {},
		"fact":     {},
		"entity":   {},
	}
	for idx, item := range pack.CriticalItems {
		itemType := strings.ToLower(strings.TrimSpace(item.Type))
		if _, ok := validCriticalTypes[itemType]; !ok {
			return fmt.Errorf("%w: item %d has invalid type %q (must be name, number, negation, fact, or entity)", ErrIncompleteCritical, idx, item.Type)
		}
		if strings.TrimSpace(item.SourceText) == "" {
			return fmt.Errorf("%w: item %d has empty source_text", ErrIncompleteCritical, idx)
		}
		if strings.TrimSpace(item.TargetRefVI) == "" {
			return fmt.Errorf("%w: item %d has empty target_ref_vi", ErrIncompleteCritical, idx)
		}
		if strings.TrimSpace(item.TargetRefEN) == "" {
			return fmt.Errorf("%w: item %d has empty target_ref_en", ErrIncompleteCritical, idx)
		}
		if item.SegmentID != "" {
			if _, ok := segmentMap[item.SegmentID]; !ok {
				return fmt.Errorf("%w: item %d references non-existent segment_id %q", ErrIncompleteCritical, idx, item.SegmentID)
			}
		}
	}

	// 4. Semantic references validation
	if !isNoDub {
		semanticMap := make(map[string]struct{})
		for idx, sref := range pack.SemanticReferences {
			segID := strings.TrimSpace(sref.SegmentID)
			if segID == "" {
				return fmt.Errorf("%w: reference %d has empty segment_id", ErrIncompleteSemanticRef, idx)
			}
			if _, exists := semanticMap[segID]; exists {
				return fmt.Errorf("%w: duplicate semantic reference for segment_id %q", ErrIncompleteSemanticRef, segID)
			}
			semanticMap[segID] = struct{}{}

			if strings.TrimSpace(sref.ReferenceVI) == "" {
				return fmt.Errorf("%w: segment %s has empty reference_vi", ErrIncompleteSemanticRef, segID)
			}
			if strings.TrimSpace(sref.ReferenceEN) == "" {
				return fmt.Errorf("%w: segment %s has empty reference_en", ErrIncompleteSemanticRef, segID)
			}
			if _, ok := segmentMap[segID]; !ok {
				return fmt.Errorf("%w: semantic reference for non-existent segment %q", ErrIncompleteSemanticRef, segID)
			}
		}

		// Ensure all transcript segments have a semantic reference
		for segID := range segmentMap {
			if _, ok := semanticMap[segID]; !ok {
				return fmt.Errorf("%w: segment %s missing semantic reference", ErrIncompleteSemanticRef, segID)
			}
		}
	}

	// 5. Burned-in dialogue subtitle regions validation
	if pack.HasBurnedInSubtitles {
		if len(pack.SubtitleRegions) == 0 {
			return fmt.Errorf("%w: has_burned_in_subtitles is true but subtitle_regions is empty", ErrIncompleteSubtitles)
		}
		for idx, sub := range pack.SubtitleRegions {
			if strings.TrimSpace(sub.RegionID) == "" {
				return fmt.Errorf("%w: subtitle region %d has empty region_id", ErrIncompleteSubtitles, idx)
			}
			if strings.TrimSpace(sub.ChineseText) == "" {
				return fmt.Errorf("%w: subtitle region %s has empty chinese_text", ErrIncompleteSubtitles, sub.RegionID)
			}
			if sub.StartMs < 0 || sub.EndMs <= sub.StartMs {
				return fmt.Errorf("%w: subtitle region %s invalid time window [%d, %d]", ErrIncompleteSubtitles, sub.RegionID, sub.StartMs, sub.EndMs)
			}
			if sub.Box.Width <= 0 || sub.Box.Height <= 0 || sub.Box.X < 0 || sub.Box.Y < 0 || sub.Box.X+sub.Box.Width > 1.05 || sub.Box.Y+sub.Box.Height > 1.05 {
				return fmt.Errorf("%w: subtitle region %s invalid box coordinates [%.2f, %.2f, %.2f, %.2f]", ErrIncompleteSubtitles, sub.RegionID, sub.Box.X, sub.Box.Y, sub.Box.Width, sub.Box.Height)
			}
		}
	}

	// 6. Cryptographic integrity: if PackDigest is supplied, verify it matches the canonical content
	if strings.TrimSpace(pack.PackDigest) != "" {
		if err := pack.VerifyPackDigest(); err != nil {
			return fmt.Errorf("reference annotation pack digest verification failed: %w", err)
		}
	}

	return nil
}
