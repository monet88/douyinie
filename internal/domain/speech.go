package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	// ErrNoDubEligibleSpeech is returned when the audio role plan has no narration/dialogue segments,
	// so speech understanding is skipped.
	ErrNoDubEligibleSpeech = errors.New("no dub-eligible speech in audio role plan")
	// ErrASRNoCandidates is returned when no ASR provider produced output.
	ErrASRNoCandidates = errors.New("no ASR results from any provider candidate")
	// ErrAlignmentFailed is returned when forced alignment produced no word timings.
	ErrAlignmentFailed = errors.New("forced alignment produced no word-level timings")
	// ErrDiarizationNoCandidates is returned when no diarization provider produced speaker regions.
	ErrDiarizationNoCandidates = errors.New("no diarization results from any provider candidate")
	// ErrAudioRolePlanRequired is returned when no audio role plan exists for the asset.
	ErrAudioRolePlanRequired = errors.New("audio role plan required before speech understanding")
)

// ASRRawSegment represents a raw ASR recognition result from one VAD turn.
// Recognizer boundaries are never canonical SpeechBlock boundaries on their own
// (AC: recognizer boundaries never canonical alone).
type ASRRawSegment struct {
	StartMs      int64   `json:"start_ms"`
	EndMs        int64   `json:"end_ms"`
	Text         string  `json:"text"`
	Confidence   float64 `json:"confidence"`
	LanguageCode string  `json:"language_code,omitempty"`
}

// WordTiming represents a single word-level alignment result from the forced aligner.
type WordTiming struct {
	Word       string  `json:"word"`
	StartMs    int64   `json:"start_ms"`
	EndMs      int64   `json:"end_ms"`
	Confidence float64 `json:"confidence"`
	SpeakerID  string  `json:"speaker_id,omitempty"` // assigned by conditional diarization before segmentation
}

// SpeakerAssignment maps a speaker to a time region.
// Produced by conditional diarization runs only when speaker evidence warrants it.
type SpeakerAssignment struct {
	SpeakerID  string  `json:"speaker_id"`
	Label      string  `json:"label,omitempty"` // e.g. "SPEAKER_00", "SPEAKER_01"
	StartMs    int64   `json:"start_ms"`
	EndMs      int64   `json:"end_ms"`
	Confidence float64 `json:"confidence"`
}

// DiarizationPlan is the output of conditional diarization (runs only when speaker evidence warrants).
type DiarizationPlan struct {
	ID              string              `json:"id"`
	RunID           string              `json:"run_id"`
	ProviderID      string              `json:"provider_id,omitempty"` // diarizer/provider identity (empty for injected test plans)
	ModelName       string              `json:"model_name,omitempty"`
	ModelVersion    string              `json:"model_version,omitempty"`
	VADModelName    string              `json:"vad_model_name,omitempty"`
	VADModelVersion string              `json:"vad_model_version,omitempty"`
	Assignments     []SpeakerAssignment `json:"assignments"`
	SpeakerEvidence *SpeakerEvidence    `json:"speaker_evidence,omitempty"`
	Confidence      float64             `json:"confidence"`
	CreatedAt       time.Time           `json:"created_at"`
}

// SpeechBlock is the canonical atomic speech unit derived after forced alignment
// and diarization. It defines an immutable source timing window.
// ASR boundaries alone are never canonical — only segmentation via semantic/punctuation/pause rules
// produces canonical SpeechBlocks.
type SpeechBlock struct {
	Index             int          `json:"index"`
	StartMs           int64        `json:"start_ms"`
	EndMs             int64        `json:"end_ms"`
	SpeakerID         string       `json:"speaker_id"`
	SourceText        string       `json:"source_text"`
	TokenTimings      []WordTiming `json:"token_timings"`
	SpeakerConfidence float64      `json:"speaker_confidence"`
	SegmentType       string       `json:"segment_type"` // "speech" | "silence"
}

// SpeechBlockSegmentType constants.
const (
	SpeechBlockTypeSpeech  = "speech"
	SpeechBlockTypeSilence = "silence"
	SpeechBlockTypeNoise   = "noise"
)

// IsPathologicalRepetitionNoise detects non-semantic repetitive ASR noise loops
// (such as Whisper looping on BGM/ambient sound with 4+ identical n-grams or
// single-character runaway loops). It protects production translation/dubbing
// pipelines from unrecoverable whole-run aborts while preserving genuine speech.
func IsPathologicalRepetitionNoise(text string) bool {
	s := strings.TrimSpace(text)
	runes := []rune(s)
	n := len(runes)
	if n < 4 {
		return false
	}

	// 1. Single rune repeated 5+ times consecutively (e.g. 啊啊啊啊啊, 啦啦啦啦啦)
	runCount := 1
	for i := 1; i < n; i++ {
		if runes[i] == runes[i-1] {
			runCount++
			if runCount >= 5 {
				return true
			}
		} else {
			runCount = 1
		}
	}

	// 2. Substrings of length L (1 to 8 runes) repeating consecutively 3+ times
	// Note: for l == 1 (single character), require reps >= 5 so intentional 3x/4x
	// emphasis or laughter (e.g. 哈哈哈, 哈哈哈哈) is not marked as noise.
	for l := 1; l <= 8 && l*3 <= n; l++ {
		for i := 0; i+l*3 <= n; i++ {
			gram := string(runes[i : i+l])
			reps := 1
			for j := i + l; j+l <= n; j += l {
				if string(runes[j:j+l]) == gram {
					reps++
				} else {
					break
				}
			}
			if l == 1 {
				if reps >= 5 {
					return true
				}
			} else if reps >= 3 {
				matchedLen := reps * l
				if matchedLen >= 10 || float64(matchedLen)/float64(n) >= 0.4 {
					return true
				}
			}
		}
	}

	// 3. Frequency of most common 2-gram, 3-gram, or 4-gram
	for l := 2; l <= 4 && l <= n; l++ {
		counts := make(map[string]int)
		for i := 0; i+l <= n; i++ {
			gram := string(runes[i : i+l])
			counts[gram]++
		}
		for _, count := range counts {
			if count >= 4 && float64(count*l)/float64(n) >= 0.45 {
				return true
			}
		}
	}

	// 4. Heavy Japanese Kana Hallucination on non-Japanese speech:
	// When Whisper loops on background music in non-Japanese speech, it typically produces
	// runaway Katakana onomatopoeia (15+ consecutive Katakana runes without spaces/kanji)
	// OR overwhelming Kana (>= 30 runes, >= 50% of text) with 3+ repeated 3-grams.
	maxKatakana := 0
	curKatakana := 0
	kanaCount := 0
	for _, r := range runes {
		if r >= 0x30A0 && r <= 0x30FF {
			curKatakana++
			if curKatakana > maxKatakana {
				maxKatakana = curKatakana
			}
			kanaCount++
		} else if r >= 0x3040 && r <= 0x309F {
			curKatakana = 0
			kanaCount++
		} else if r != ' ' {
			curKatakana = 0
		}
	}
	if maxKatakana >= 15 {
		return true
	}
	if kanaCount >= 30 && float64(kanaCount)/float64(n) >= 0.5 {
		for l := 3; l <= 8 && l <= n; l++ {
			counts := make(map[string]int)
			for i := 0; i+l <= n; i++ {
				counts[string(runes[i:i+l])]++
			}
			for _, count := range counts {
				if count >= 3 {
					return true
				}
			}
		}
	}
	return false
}

// TranscriptArtifact is the persisted output of the speech understanding pipeline:
// ASR → accepted transcript → forced alignment → conditional diarization → canonical SpeechBlock segmentation.
type TranscriptArtifact struct {
	ID                      string              `json:"id"`
	AssetID                 string              `json:"asset_id"`
	RunID                   string              `json:"run_id"`
	RawSegments             []ASRRawSegment     `json:"raw_segments"`
	WordTimings             []WordTiming        `json:"word_timings"`
	SpeakerAssignments      []SpeakerAssignment `json:"speaker_assignments,omitempty"`
	SpeakerEvidence         *SpeakerEvidence    `json:"speaker_evidence,omitempty"`
	SpeechBlocks            []SpeechBlock       `json:"speech_blocks"`
	ASRProviderID           string              `json:"asr_provider_id"`
	AlignerProviderID       string              `json:"aligner_provider_id"`
	DiarizationRan          bool                `json:"diarization_ran"`
	DiarizationProviderID   string              `json:"diarization_provider_id,omitempty"` // diarizer identity when evidence-gated diarization ran
	DiarizationModelName    string              `json:"diarization_model_name,omitempty"`
	DiarizationModelVersion string              `json:"diarization_model_version,omitempty"`
	DiarizationVADModel     string              `json:"diarization_vad_model,omitempty"`
	DiarizationVADVersion   string              `json:"diarization_vad_version,omitempty"`
	SourceLanguage          string              `json:"source_language"`
	CASHash                 string              `json:"cas_hash,omitempty"`        // content-addressed store object hash
	ProvenanceHash          string              `json:"provenance_hash,omitempty"` // deterministic identity over dependency/config/provider/model/schema inputs
	CreatedAt               time.Time           `json:"created_at"`
}

// AudioNormalizationConfig captures the deterministic audio normalization parameters
// (16 kHz, mono, 16-bit PCM WAV) applied during Acquisition/Preflight.
type AudioNormalizationConfig struct {
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Codec      string `json:"codec"`
	Format     string `json:"format"`
}

// DefaultAudioNormalizationConfig returns the standard 16 kHz mono WAV configuration.
func DefaultAudioNormalizationConfig() AudioNormalizationConfig {
	return AudioNormalizationConfig{
		SampleRate: 16000,
		Channels:   1,
		Codec:      "pcm_s16le",
		Format:     "wav",
	}
}

// TranscriptProvenance captures the deterministic inputs that determine a
// transcript artifact's cache identity: source asset, normalized audio identity,
// normalization config, providers, models, segment rules, diarization identity
// and semantic evidence config, and the pipeline schema version. Changing any of
// these yields a NEW artifact identity instead of a permanent write-once failure.
type TranscriptProvenance struct {
	AssetSHA256             string                    `json:"asset_sha256"`
	NormalizedAudioSHA256   string                    `json:"normalized_audio_sha256,omitempty"`
	NormalizationConfig     AudioNormalizationConfig  `json:"normalization_config"`
	ASRProviderID           string                    `json:"asr_provider_id"`
	ASRModelName            string                    `json:"asr_model_name"`
	ASRModelVersion         string                    `json:"asr_model_version"`
	AlignerProviderID       string                    `json:"aligner_provider_id"`
	AlignerModelName        string                    `json:"aligner_model_name"`
	AlignerModelVersion     string                    `json:"aligner_model_version"`
	SegmentConfig           SegmentRuleConfig         `json:"segment_config"`
	DiarizationProviderID   string                    `json:"diarization_provider_id,omitempty"`
	DiarizationModelName    string                    `json:"diarization_model_name,omitempty"`
	DiarizationModelVersion string                    `json:"diarization_model_version,omitempty"`
	DiarizationVADModel     string                    `json:"diarization_vad_model,omitempty"`
	DiarizationVADVersion   string                    `json:"diarization_vad_version,omitempty"`
	DiarizationRan          bool                      `json:"diarization_ran"`
	DiarizationEvidence     DiarizationEvidenceConfig `json:"diarization_evidence"`
	SpeakerEvidenceHash     string                    `json:"speaker_evidence_hash,omitempty"`
	SchemaVersion           int                       `json:"schema_version"`
}

// Hash returns a deterministic SHA-256 identity over the canonical JSON of
// the provenance inputs. Identical pipeline inputs always produce the same
// hash; changing any input produces a different hash.
func (p TranscriptProvenance) Hash() string {
	b, err := json.Marshal(p)
	if err != nil {
		// Canonical struct with only JSON-marshalable fields cannot fail.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TranscriptSchemaVersion is the canonical transcript pipeline schema version.
// It participates in the provenance hash so schema changes re-derive artifacts.
// v3: provenance carries normalized audio identity + normalization config + explicit embedding cosine threshold
const TranscriptSchemaVersion = 3

// SpeakerEvidence captures upstream pre-diarization evidence of multi-speaker content
// (e.g. acoustic change points, multi-character metadata, or audio role cues).
// Pauses alone are not multi-speaker proof without an upstream speaker-evidence signal.
type SpeakerEvidence struct {
	HasMultiSpeakerCues bool    `json:"has_multi_speaker_cues"`
	SpeakerChangeCount  int     `json:"speaker_change_count"`
	Confidence          float64 `json:"confidence"`
	Source              string  `json:"source,omitempty"`
}

// Hash returns a deterministic SHA-256 identity over the canonical JSON of
// the speaker evidence cues.
func (e *SpeakerEvidence) Hash() string {
	if e == nil {
		return ""
	}
	b, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DiarizationEvidenceConfig captures the deterministic multi-speaker evidence
// gate parameters used to decide whether conditional diarization runs. It
// combines pre-diarization timing cues (pause gaps) with upstream speaker evidence
// cues — never from SpeakerID labels the diarizer itself produces. It hashes
// into TranscriptProvenance so a changed evidence policy yields new artifacts.
type DiarizationEvidenceConfig struct {
	MinTurnGapMs             int64   `json:"min_turn_gap_ms"`
	MinDistinctTurns         int     `json:"min_distinct_turns"`
	RequireSpeakerCues       bool    `json:"require_speaker_cues"`
	MinEvidenceConfidence    float64 `json:"min_evidence_confidence"`
	EmbeddingCosineThreshold float64 `json:"embedding_cosine_threshold"`
}

// DefaultDiarizationEvidenceConfig returns the default multi-speaker evidence
// gate: at least MIN_DISTINCT_TURNS distinct speech turns separated by a pause
// of at least MinTurnGapMs milliseconds, requiring upstream speaker cues,
// and evaluated with an explicit embedding cosine change threshold of 0.65.
func DefaultDiarizationEvidenceConfig() DiarizationEvidenceConfig {
	return DiarizationEvidenceConfig{
		MinTurnGapMs:             300,
		MinDistinctTurns:         2,
		RequireSpeakerCues:       true,
		MinEvidenceConfidence:    0.0,
		EmbeddingCosineThreshold: 0.65,
	}
}

// SpeechPipelineInput captures the full input for the speech understanding pipeline run.
// It bundles all needed references so the pipeline can be invoked atomically.
type SpeechPipelineInput struct {
	RunID                 string
	AssetID               string
	JobID                 string
	AudioPath             string           // source-media CAS path or preflight normalized audio path
	AudioSHA256           string           // SHA-256 content identity
	NormalizedAudioPath   string           // optional explicit preflight normalized audio CAS path
	NormalizedAudioSHA256 string           // optional explicit preflight normalized audio SHA-256
	SpeakerEvidence       *SpeakerEvidence // optional upstream speaker-evidence cues
	AudioRolePlan         *AudioRolePlan   // for determining if dub-eligible speech exists
}

// SegmentRuleConfig defines the tunable parameters for canonical SpeechBlock segmentation.
type SegmentRuleConfig struct {
	MinSpeechBlockMs   int64 `json:"min_speech_block_ms"`
	MaxSpeechBlockMs   int64 `json:"max_speech_block_ms"`
	PauseSplitMs       int64 `json:"pause_split_ms"`        // gap >= this triggers a block split
	MinSilenceMs       int64 `json:"min_silence_ms"`        // gap >= this is a silence segment
	MinSpeakerChangeMs int64 `json:"min_speaker_change_ms"` // speaker-change gap threshold
	PunctuationSplit   bool  `json:"punctuation_split"`     // split on sentence-final punctuation
}

// DefaultSegmentRuleConfig returns the default segmentation parameters.
// These are empirically tunable (per fixture evidence, not universal hard-coded thresholds).
func DefaultSegmentRuleConfig() SegmentRuleConfig {
	return SegmentRuleConfig{
		MinSpeechBlockMs:   500,
		MaxSpeechBlockMs:   15000,
		PauseSplitMs:       400,
		MinSilenceMs:       300,
		MinSpeakerChangeMs: 100,
		PunctuationSplit:   true,
	}
}

// IsDubEligible returns true if the audio role plan contains at least one
// narration/dialogue segment.
func IsDubEligible(plan *AudioRolePlan) bool {
	if plan == nil {
		return false
	}
	for _, seg := range plan.Segments {
		if seg.Role == AudioRoleNarrationDialogue {
			return true
		}
	}
	return false
}
