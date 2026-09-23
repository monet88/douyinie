package domain

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Translation domain errors.
var (
	ErrMeaningPreservationFailed    = errors.New("meaning preservation validation failed")
	ErrFactCorrupted                = errors.New("fact or entity corrupted in translation")
	ErrNameCorrupted                = errors.New("named entity corrupted or dropped in translation")
	ErrNumberCorrupted              = errors.New("numerical value or quantity corrupted in translation")
	ErrNegationInverted             = errors.New("negation polarity inverted in translation")
	ErrTranslationVariantNotFound   = errors.New("translation variant not found")
	ErrEmptyTranslationInput        = errors.New("empty translation input")
	ErrDubScriptVariantNotFound     = errors.New("dub script variant not found")
	ErrTranslationOwnershipMismatch = errors.New("translation ownership mismatch")
	ErrGlossaryConflict             = errors.New("request glossary conflicts with frozen run snapshot")
)

const (
	// TranslationSchemaVersion is part of the translation stage cache identity, so it
	// moves whenever a TranslationVariant's persisted contract changes. Version 2 adds
	// ReviewReason and the rule that a segment failing the meaning gate is persisted
	// with passed_qa_gate=false for operator review instead of rejecting the candidate:
	// a variant cached under version 1 predates both.
	TranslationSchemaVersion = 3
	DubScriptSchemaVersion   = 2

	// ReviewReasonMeaningCorrupted marks a dub script segment whose spoken text failed
	// the meaning-first gate. The operator corrects it or records a manual override.
	ReviewReasonMeaningCorrupted = "meaning_corrupted"
)

// GlossaryEntry is one ordered request-local terminology rule. It is frozen with
// the run config and never becomes a shared mutable catalog.
type GlossaryEntry struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Note   string `json:"note,omitempty"`
}

// EffectiveGlossary is the resolved subset that actually matches the current
// translation input. Its order is semantic and therefore participates in cache identity.
type EffectiveGlossary struct {
	Entries          []GlossaryEntry `json:"entries,omitempty"`
	OmittedMatches   int             `json:"omitted_matches,omitempty"`
	OmittedConflicts int             `json:"omitted_conflicts,omitempty"`
	Hash             string          `json:"hash,omitempty"`
}

// NormalizeGlossarySource canonicalizes a glossary source or target term for matching:
// NFKC, trimmed, and lowercased.
func NormalizeGlossarySource(s string) string {
	return strings.ToLower(norm.NFKC.String(strings.TrimSpace(s)))
}

// HasCJK reports whether the string contains any CJK ideographs or kana/hangul runes.
func HasCJK(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}

// GlossaryWordRune reports whether r is a Latin word rune, digit, or underscore.
// Non-Latin scripts (such as CJK) return false so Latin boundaries do not block
// matching adjacent to CJK or non-Latin characters (e.g. "AI" in "AI模型").
func GlossaryWordRune(r rune) bool {
	if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
		return true
	}
	return unicode.In(r, unicode.Latin)
}

// GlossaryTermMatches reports whether term appears in text according to the
// request-local glossary matching contract: NFKC/lowercase normalization,
// Latin/digit/underscore boundaries, and CJK substring matching.
func GlossaryTermMatches(text, term string) bool {
	normText := []rune(NormalizeGlossarySource(text))
	needle := []rune(NormalizeGlossarySource(term))
	if len(needle) == 0 || len(normText) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(normText); i++ {
		match := true
		for j := range needle {
			if normText[i+j] != needle[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if HasCJK(string(needle)) {
			return true
		}
		leftOK := i == 0 || !GlossaryWordRune(normText[i-1])
		right := i + len(needle)
		rightOK := right == len(normText) || !GlossaryWordRune(normText[right])
		if leftOK && rightOK {
			return true
		}
	}
	return false
}

// TranslationSegment represents a single translated unit (typically mapped 1:1 to a SpeechBlock or visual text region).
type TranslationSegment struct {
	Index            int      `json:"index"`
	SourceText       string   `json:"source_text"`
	TargetText       string   `json:"target_text"`
	SpeakerID        string   `json:"speaker_id,omitempty"`
	StartMs          int64    `json:"start_ms,omitempty"`
	EndMs            int64    `json:"end_ms,omitempty"`
	KeyFacts         []string `json:"key_facts,omitempty"` // Extracted facts, entities, numbers
	NegationPolarity bool     `json:"negation_polarity"`   // true if negative statement
	QAConfidence     float64  `json:"qa_confidence"`       // Meaning preservation QA score (0.0 - 1.0)
	PassedQAGate     bool     `json:"passed_qa_gate"`
	// ReviewReason explains the meaning-gate violation that flagged this segment for
	// operator review. Set only when PassedQAGate is false; the operator either corrects
	// the target text or records a manual override.
	ReviewReason string `json:"review_reason,omitempty"`
}

// TranslationVariant is the immutable target-language meaning-preserving artifact.
// Consumed explicitly by downstream stages: T10 (visual text localization) and T13 (dubbing translation).
type TranslationVariant struct {
	ID                    string               `json:"id"`
	SchemaVersion         int                  `json:"schema_version"`
	AssetID               string               `json:"asset_id"`
	RunID                 string               `json:"run_id"`
	JobID                 string               `json:"job_id,omitempty"`
	SourceLanguage        string               `json:"source_language"` // e.g. "zh"
	TargetLanguage        string               `json:"target_language"` // "vi" or "en"
	ContractID            string               `json:"contract_id"`
	EffectiveGlossary     EffectiveGlossary    `json:"effective_glossary,omitempty"`
	TranscriptArtifactCAS string               `json:"transcript_artifact_cas,omitempty"`
	InputHash             string               `json:"input_hash"`
	Segments              []TranslationSegment `json:"segments"`
	ProviderID            string               `json:"provider_id"`
	ModelName             string               `json:"model_name"`
	ModelVersion          string               `json:"model_version"`
	ServiceBaselineID     string               `json:"service_baseline_id,omitempty"`
	ObservedModel         string               `json:"observed_model,omitempty"`
	SystemFingerprint     string               `json:"system_fingerprint,omitempty"`
	CASHash               string               `json:"cas_hash,omitempty"`
	ProvenanceHash        string               `json:"provenance_hash,omitempty"`
	OverallQAScore        float64              `json:"overall_qa_score"`
	CreatedAt             time.Time            `json:"created_at"`
}

// TranslationInputSegment is a text input segment for translation.
type TranslationInputSegment struct {
	Index      int    `json:"index"`
	SourceText string `json:"source_text"`
	SpeakerID  string `json:"speaker_id,omitempty"`
	StartMs    int64  `json:"start_ms,omitempty"`
	EndMs      int64  `json:"end_ms,omitempty"`
}

// TranslationJobInput encapsulates inputs required to generate a TranslationVariant.
type TranslationJobInput struct {
	RunID                 string                    `json:"run_id"`
	AssetID               string                    `json:"asset_id"`
	JobID                 string                    `json:"job_id,omitempty"`
	SourceLanguage        string                    `json:"source_language"`
	TargetLanguage        string                    `json:"target_language"`
	Glossary              []GlossaryEntry           `json:"glossary,omitempty"`
	EffectiveGlossary     EffectiveGlossary         `json:"effective_glossary,omitempty"`
	Segments              []TranslationInputSegment `json:"segments"`
	TranscriptArtifactCAS string                    `json:"transcript_artifact_cas,omitempty"`
	ExecutionProfile      ExecutionProfile          `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string                  `json:"authorized_credentials,omitempty"`
	ConsentGranted        bool                      `json:"consent_granted,omitempty"`
	// Ephemeral marks a translation whose result is consumed inline by the caller (overlay /
	// visual text) rather than published as the run's canonical translation. An ephemeral call
	// must not move the run-scoped TranslationVariant index: the canonical variant is the one
	// the speech stages pin (DubScriptVariant.TranslationVariantCAS), and republishing it from an
	// unrelated lane makes every later artifact validate against the wrong CAS.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

type DubScriptSegment struct {
	Index                 int      `json:"index"`
	SourceText            string   `json:"source_text"`
	MeaningText           string   `json:"meaning_text"` // Original translation from TranslationVariant
	SpokenText            string   `json:"spoken_text"`  // Duration-adapted, shorten-first phrasing
	SpeakerID             string   `json:"speaker_id,omitempty"`
	StartMs               int64    `json:"start_ms"`                  // Immutable source window start
	EndMs                 int64    `json:"end_ms"`                    // Immutable source window end
	SlotDurationMs        int64    `json:"slot_duration_ms"`          // EndMs - StartMs
	EstimatedDurationMs   int64    `json:"estimated_duration_ms"`     // Estimated spoken duration
	SourceSpeakingRateCPS float64  `json:"source_speaking_rate_cps"`  // Source syllable-like speech units per second (Han character ~= syllable)
	TargetSpeakingRateCPS float64  `json:"target_speaking_rate_cps"`  // Target syllables per second
	CadenceRatio          float64  `json:"cadence_ratio"`             // Target syllables/sec vs source syllables/sec (source-relative)
	SourceGapAfterMs      int64    `json:"source_gap_after_ms"`       // Immutable source silence until the next speech turn
	NaturalGapMs          int64    `json:"natural_gap_ms"`            // Predicted pause from estimated dub finish to the next source turn
	TargetWordBudget      int      `json:"target_word_budget"`        // Source-relative max target words for the immutable speech slot
	IsShortened           bool     `json:"is_shortened"`              // Whether shorten-first adaptation was applied
	RequiresReview        bool     `json:"requires_review,omitempty"` // Flagged if unresolvable duration overrun or QA fallback
	ReviewReason          string   `json:"review_reason,omitempty"`   // Reason code for operator review
	KeyFacts              []string `json:"key_facts,omitempty"`
	NegationPolarity      bool     `json:"negation_polarity"`
	QAConfidence          float64  `json:"qa_confidence"`
	PassedQAGate          bool     `json:"passed_qa_gate"`
}

// DubScriptVariant is the immutable target-language duration-adapted spoken script artifact.
// Produced by T13 and consumed explicitly by T14 (TTS & fit controller).
type DubScriptVariant struct {
	ID                    string             `json:"id"`
	SchemaVersion         int                `json:"schema_version"`
	AssetID               string             `json:"asset_id"`
	RunID                 string             `json:"run_id"`
	JobID                 string             `json:"job_id,omitempty"`
	SourceLanguage        string             `json:"source_language"` // e.g. "zh"
	TargetLanguage        string             `json:"target_language"` // "vi" or "en"
	TranslationVariantCAS string             `json:"translation_variant_cas,omitempty"`
	Segments              []DubScriptSegment `json:"segments"`
	ProviderID            string             `json:"provider_id"`
	ModelName             string             `json:"model_name"`
	ModelVersion          string             `json:"model_version"`
	CASHash               string             `json:"cas_hash,omitempty"`
	ProvenanceHash        string             `json:"provenance_hash,omitempty"`
	OverallQAScore        float64            `json:"overall_qa_score"`
	RequiresReview        bool               `json:"requires_review,omitempty"`
	CreatedAt             time.Time          `json:"created_at"`
}

// DubScriptJobInput encapsulates inputs required to generate a DubScriptVariant.
type DubScriptJobInput struct {
	RunID                 string           `json:"run_id"`
	AssetID               string           `json:"asset_id"`
	JobID                 string           `json:"job_id,omitempty"`
	SourceLanguage        string           `json:"source_language"`
	TargetLanguage        string           `json:"target_language"`
	TranslationVariantCAS string           `json:"translation_variant_cas,omitempty"`
	ExecutionProfile      ExecutionProfile `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string         `json:"authorized_credentials,omitempty"`
}
