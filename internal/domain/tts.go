package domain

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
	// ErrEngineHoppingForbidden is returned when different providers/engines are mixed across sentences for a single speaker.
	ErrEngineHoppingForbidden = errors.New("engine hopping across sentences for a single speaker is strictly prohibited")
	// ErrTTSDurationOverrun is returned when synthesized audio exceeds its accepted playback window.
	ErrTTSDurationOverrun = errors.New("tts synthesized duration overruns immutable source window")
	// ErrOverlongCandidateNotSelectable is returned when an overlong measured candidate is rejected from selection.
	ErrOverlongCandidateNotSelectable = errors.New("overlong measured candidate cannot be selected into final dub")
	// ErrVoiceAssignmentFrozen is returned when attempting to mutate an already frozen VoiceAssignment.
	ErrVoiceAssignmentFrozen = errors.New("voice assignment is frozen for this run")
	// ErrNoEligibleTTSProvider is returned when no policy-eligible TTS provider is available.
	ErrNoEligibleTTSProvider = errors.New("no eligible TTS provider found for voice assignment")
	// ErrTTSSpeedUnsupported is returned when a preset-only provider cannot safely alter speaking speed.
	ErrTTSSpeedUnsupported = errors.New("tts provider does not support requested speed")
	// ErrDubSegmentCollision is returned when adjacent dub segments would collide on the timeline.
	ErrDubSegmentCollision = errors.New("adjacent speech collision detected between dub segments")
	// ErrVoiceProfileNotFound is returned when a speaker has no assigned voice profile.
	ErrVoiceProfileNotFound = errors.New("voice profile not found for speaker")
	// ErrDubScriptRequiredForDubbing is returned when DubScriptVariant is missing.
	ErrDubScriptRequiredForDubbing = errors.New("dub script variant required before TTS synthesis")
	// ErrVoiceAssignmentRequired is returned when VoiceAssignment is missing.
	ErrVoiceAssignmentRequired = errors.New("voice assignment required before TTS synthesis")
	// ErrVoiceAssignmentNotFound is returned when VoiceAssignment is not found in storage.
	ErrVoiceAssignmentNotFound = errors.New("voice assignment not found")
	// ErrDubSegmentsVariantNotFound is returned when DubSegmentsVariant is not found in storage.
	ErrDubSegmentsVariantNotFound = errors.New("dub segments variant not found")
	// ErrNoDubbingRequired is returned when no dub-eligible dialogue exists for voice audition/assignment.
	ErrNoDubbingRequired = errors.New("no dubbing required")
)

const (
	VoiceAssignmentSchemaVersion = 1
	// DubSegmentsSchemaVersion is part of the TTS stage cache identity, so it must move
	// whenever a DubSegmentsVariant's persisted contract or synthesis semantics change.
	// Version 3 adds the pinned playback-window, canonical source-membership, transcript,
	// audio-role-plan, and fit-policy evidence required by the current mixing contract.
	DubSegmentsSchemaVersion = 3
)

// VoiceProfile represents a preset or cloned voice configuration.
type VoiceProfile struct {
	ID                string  `json:"id"`                            // Unique profile identifier (e.g. "vieneu_vi_female_1")
	ProviderID        string  `json:"provider_id"`                   // Provider owning this voice (e.g. "vieneu_tts_vi", "cosyvoice3_tts")
	VoiceID           string  `json:"voice_id"`                      // Engine internal voice identifier
	Name              string  `json:"name"`                          // Human-readable display name
	Language          string  `json:"language"`                      // "vi", "en", "zh"
	Gender            string  `json:"gender,omitempty"`              // "female", "male", "neutral"
	Pitch             float64 `json:"pitch,omitempty"`               // Pitch adjustment multiplier (1.0 default)
	Speed             float64 `json:"speed,omitempty"`               // Base speaking speed multiplier (1.0 default)
	Timbre            string  `json:"timbre,omitempty"`              // Timbre description (e.g. "warm", "crisp")
	IsClone           bool    `json:"is_clone,omitempty"`            // True if source-cloned voice
	ReferenceAudioCAS string  `json:"reference_audio_cas,omitempty"` // CAS hash of reference audio if cloned
}

// VoiceAssignment is the frozen, run-scoped mapping of speaker_id -> VoiceProfile.
// Engine hopping across sentences for a single speaker is strictly prohibited.
type VoiceAssignment struct {
	ID                    string                     `json:"id"`
	SchemaVersion         int                        `json:"schema_version"`
	AssetID               string                     `json:"asset_id"`
	RunID                 string                     `json:"run_id"`
	JobID                 string                     `json:"job_id,omitempty"`
	TargetLanguage        string                     `json:"target_language"` // "vi" or "en"
	Assignments           map[string]VoiceProfile    `json:"assignments"`     // speaker_id -> VoiceProfile
	UseSameVoiceForAll    bool                       `json:"use_same_voice_for_all"`
	SupersedesCAS         string                     `json:"supersedes_cas,omitempty"`
	InvalidatedSpeakers   []string                   `json:"invalidated_speakers,omitempty"`
	InvalidationScope     []string                   `json:"invalidation_scope,omitempty"`
	Distinguishability    *VoiceDistinguishabilityQC `json:"distinguishability,omitempty"`
	CASHash               string                     `json:"cas_hash,omitempty"`
	ProvenanceHash        string                     `json:"provenance_hash,omitempty"`
	DubScriptVariantCAS   string                     `json:"dub_script_variant_cas,omitempty"`
	TranscriptArtifactCAS string                     `json:"transcript_artifact_cas,omitempty"`
	FrozenAt              time.Time                  `json:"frozen_at"`
	CreatedAt             time.Time                  `json:"created_at"`
}

// VoiceDistinguishabilityQC captures quality-control evaluation of multi-speaker voice assignments.
type VoiceDistinguishabilityQC struct {
	MultiSpeaker     bool     `json:"multi_speaker"`
	Status           string   `json:"status"` // "PASS" | "REVIEW_REQUIRED"
	Issues           []string `json:"issues,omitempty"`
	DistinctVoiceIDs int      `json:"distinct_voice_ids"`
	SpeakerCount     int      `json:"speaker_count"`
}

// VoiceChangeInvalidationStages returns the declared descendant stage chain
// regenerated when an operator edits a speaker's voice assignment (Issue #40).
// RenderPlan sits between DubMix and FinalRender. Source-derived artifacts
// (transcript, translation, dub script, stems) remain reusable.
func VoiceChangeInvalidationStages() []string {
	return []string{"tts", "dub_segments", "dub_mix", "render_plan", "final_render"}
}

// EvaluateVoiceDistinguishability checks that multi-speaker assignments keep voices
// distinguishable. Distinct speakers sharing one identical voice profile without an
// explicit same-voice-for-all convenience flag is flagged REVIEW_REQUIRED.
func EvaluateVoiceDistinguishability(assignments map[string]VoiceProfile, useSameVoiceForAll bool) *VoiceDistinguishabilityQC {
	qc := &VoiceDistinguishabilityQC{
		MultiSpeaker: len(assignments) > 1,
		SpeakerCount: len(assignments),
	}
	voiceIDs := make(map[string]bool)
	for _, v := range assignments {
		voiceIDs[v.ID] = true
	}
	qc.DistinctVoiceIDs = len(voiceIDs)

	if !qc.MultiSpeaker {
		qc.Status = "PASS"
		return qc
	}
	if useSameVoiceForAll {
		qc.Status = "PASS"
		return qc
	}
	if qc.DistinctVoiceIDs < len(assignments) {
		byVoice := make(map[string][]string)
		for spk, v := range assignments {
			byVoice[v.ID] = append(byVoice[v.ID], spk)
		}
		for vid, spks := range byVoice {
			if len(spks) > 1 {
				sort.Strings(spks)
				qc.Issues = append(qc.Issues, fmt.Sprintf("speakers %s share voice %q without use_same_voice_for_all", strings.Join(spks, ","), vid))
			}
		}
		sort.Strings(qc.Issues)
		qc.Status = "REVIEW_REQUIRED"
		return qc
	}
	qc.Status = "PASS"
	return qc
}

// AffectedSpeakers returns the list of speaker IDs whose voice profile differs between old and new assignments.
func AffectedSpeakers(oldAssignments, newAssignments map[string]VoiceProfile) []string {
	affectedMap := make(map[string]bool)
	for spk, newProf := range newAssignments {
		oldProf, exists := oldAssignments[spk]
		if !exists || !VoiceProfileEquivalent(oldProf, newProf) {
			affectedMap[spk] = true
		}
	}
	for spk := range oldAssignments {
		if _, exists := newAssignments[spk]; !exists {
			affectedMap[spk] = true
		}
	}
	var res []string
	for spk := range affectedMap {
		res = append(res, spk)
	}
	sort.Strings(res)
	return res
}

// VoiceProfileEquivalent checks if two voice profiles are semantically identical in all audio-affecting fields.
func VoiceProfileEquivalent(a, b VoiceProfile) bool {
	return a.ID == b.ID &&
		a.ProviderID == b.ProviderID &&
		a.VoiceID == b.VoiceID &&
		a.Language == b.Language &&
		a.Gender == b.Gender &&
		a.Pitch == b.Pitch &&
		a.Speed == b.Speed &&
		a.Timbre == b.Timbre &&
		a.IsClone == b.IsClone &&
		a.ReferenceAudioCAS == b.ReferenceAudioCAS
}

// FitAction represents the outcome of evaluating a candidate synthesized media against the immutable source window.
type FitAction string

const (
	FitActionAccept  FitAction = "ACCEPT"
	FitActionResynth FitAction = "RESYNTH"
	FitActionRewrite FitAction = "REWRITE"
	FitActionRegroup FitAction = "REGROUP"
	FitActionReview  FitAction = "REVIEW"
)

// DubbingFitPlan captures the cadence and duration fit analysis for one segment.
type DubbingFitPlan struct {
	SegmentIndex       int       `json:"segment_index"`
	SpeakerID          string    `json:"speaker_id"`
	SlotDurationMs     int64     `json:"slot_duration_ms"`
	UsableSlotMs       int64     `json:"usable_slot_ms"`
	MeasuredDurationMs int64     `json:"measured_duration_ms"`
	DurationDeltaMs    int64     `json:"duration_delta_ms"` // MeasuredDurationMs - UsableSlotMs (>0 means overrun)
	SpeedFactor        float64   `json:"speed_factor"`      // Speed adjustment multiplier applied
	NaturalGapMs       int64     `json:"natural_gap_ms"`    // Inter-turn silence preserved
	DubPlaybackEndMs   int64     `json:"dub_playback_end_ms"`
	EffectiveReserveMs int64     `json:"effective_reserve_ms"`
	FitPolicyID        string    `json:"fit_policy_id"`
	SpeechBlockIndices []int     `json:"speech_block_indices,omitempty"`
	Decision           FitAction `json:"decision"` // ACCEPT | RESYNTH | REWRITE | REGROUP | REVIEW
	DecisionReason     string    `json:"decision_reason,omitempty"`
	AttemptCount       int       `json:"attempt_count"`
}

// TTSCandidate represents a synthesized candidate waveform.
type TTSCandidate struct {
	CandidateID         string       `json:"candidate_id"`
	SegmentIndex        int          `json:"segment_index"`
	SpeakerID           string       `json:"speaker_id"`
	Text                string       `json:"text"`
	Voice               VoiceProfile `json:"voice"`
	AudioCASPath        string       `json:"audio_cas_path"`
	AudioSHA256         string       `json:"audio_sha256"`
	PredictedDurationMs int64        `json:"predicted_duration_ms"`
	MeasuredDurationMs  int64        `json:"measured_duration_ms"`
	SpeedFactor         float64      `json:"speed_factor"`
	AttemptNumber       int          `json:"attempt_number"`
	ProbedAt            time.Time    `json:"probed_at"`
}

// DubSegment is the selected audio candidate for a speech segment.
// It covers 1..N same-speaker SpeechBlocks and is strictly fit-gated against its accepted playback window.
type DubSegment struct {
	Index              int          `json:"index"`
	SpeechBlockIndices []int        `json:"speech_block_indices,omitempty"`
	SpeakerID          string       `json:"speaker_id"`
	StartMs            int64        `json:"start_ms"`         // Immutable source window start
	EndMs              int64        `json:"end_ms"`           // Immutable source window end
	SlotDurationMs     int64        `json:"slot_duration_ms"` // EndMs - StartMs
	SourceText         string       `json:"source_text"`
	SpokenText         string       `json:"spoken_text"` // Synthesized target text
	AudioCASPath       string       `json:"audio_cas_path"`
	AudioSHA256        string       `json:"audio_sha256"`
	MeasuredDurationMs int64        `json:"measured_duration_ms"` // Probed true duration
	Voice              VoiceProfile `json:"voice"`
	FitDecision        FitAction    `json:"fit_decision"`
	ReviewReason       string       `json:"review_reason,omitempty"`
	RequiresReview     bool         `json:"requires_review,omitempty"`
	NaturalGapAfterMs  int64        `json:"natural_gap_after_ms"`
	DubPlaybackEndMs   int64        `json:"dub_playback_end_ms"`
	EffectiveReserveMs int64        `json:"effective_reserve_ms"`
}

// DubSegmentReview records an unselected candidate or segment flagged for operator review.
type DubSegmentReview struct {
	Index              int          `json:"index"`
	SpeechBlockIndices []int        `json:"speech_block_indices,omitempty"`
	SpeakerID          string       `json:"speaker_id"`
	StartMs            int64        `json:"start_ms"`
	EndMs              int64        `json:"end_ms"`
	SlotDurationMs     int64        `json:"slot_duration_ms"`
	SourceText         string       `json:"source_text"`
	SpokenText         string       `json:"spoken_text"`
	AudioCASPath       string       `json:"audio_cas_path"`
	AudioSHA256        string       `json:"audio_sha256"`
	MeasuredDurationMs int64        `json:"measured_duration_ms"`
	Voice              VoiceProfile `json:"voice"`
	FitDecision        FitAction    `json:"fit_decision"`
	ReviewReason       string       `json:"review_reason"`
	AttemptCount       int          `json:"attempt_count"`
	DubPlaybackEndMs   int64        `json:"dub_playback_end_ms"`
	EffectiveReserveMs int64        `json:"effective_reserve_ms"`
}

// VoiceEscalationReasonFixedRateOverrun is the deterministic reason recorded when a
// fixed-rate preset lane (ZeroTTS) could not fit its accepted playback window after the
// bounded natural-speed rewrite/regroup remedies were exhausted (Issue #94).
const VoiceEscalationReasonFixedRateOverrun = "UNRESOLVED_FIXED_RATE_DURATION_OVERRUN"

// VoiceEscalationReasonSharedVoiceScope is the deterministic reason recorded for a
// speaker carried onto the fallback lane because the run pins one voice for every
// speaker: the escalated speaker's fallback voice is theirs too. Such a speaker has no
// unresolved overrun trigger of its own.
const VoiceEscalationReasonSharedVoiceScope = "SHARED_VOICE_FOR_ALL_FOLLOWS_FALLBACK_LANE"

// VoiceProviderEscalation records one whole-speaker provider escalation caused by an
// unresolved timing failure on a fixed-rate preset lane. The provider change is applied
// by superseding the speaker's frozen VoiceAssignment, never by editing historical
// assignment evidence and never sentence-by-sentence within one speaker.
type VoiceProviderEscalation struct {
	SpeakerID                string `json:"speaker_id"`
	FromProviderID           string `json:"from_provider_id"`
	FromVoiceID              string `json:"from_voice_id"`
	ToProviderID             string `json:"to_provider_id"`
	ToVoiceID                string `json:"to_voice_id"`
	Reason                   string `json:"reason"`
	TriggerSegmentIndices    []int  `json:"trigger_segment_indices"`
	SupersededAssignmentCAS  string `json:"superseded_assignment_cas"`
	SupersedingAssignmentCAS string `json:"superseding_assignment_cas"`
	// Resolved reports whether the escalated regeneration cleared every unresolved
	// slot overrun for this speaker. False means the run projects REVIEW.
	Resolved bool `json:"resolved"`
}

// DubSegmentsVariant is the immutable target-language dubbing artifact containing all selected DubSegments.
type DubSegmentsVariant struct {
	ID                    string             `json:"id"`
	SchemaVersion         int                `json:"schema_version"`
	AssetID               string             `json:"asset_id"`
	RunID                 string             `json:"run_id"`
	JobID                 string             `json:"job_id,omitempty"`
	TargetLanguage        string             `json:"target_language"` // "vi" or "en"
	DubScriptVariantCAS   string             `json:"dub_script_variant_cas,omitempty"`
	VoiceAssignmentCAS    string             `json:"voice_assignment_cas,omitempty"`
	TranscriptArtifactCAS string             `json:"transcript_artifact_cas,omitempty"`
	AudioRolePlanCAS      string             `json:"audio_role_plan_cas,omitempty"`
	FitPolicyID           string             `json:"fit_policy_id"`
	Segments              []DubSegment       `json:"segments"`                  // Strictly ACCEPTED fit-gated segments (mixer inputs)
	ReviewSegments        []DubSegmentReview `json:"review_segments,omitempty"` // Flagged unselected candidates requiring review
	FitPlans              []DubbingFitPlan   `json:"fit_plans,omitempty"`
	// Escalations records whole-speaker provider escalations performed while
	// generating this variant (Issue #94): empty for the single-pass case.
	Escalations []VoiceProviderEscalation `json:"escalations,omitempty"`
	// FixedRateSpeakers lists the speakers whose candidates were produced by a lane
	// that fails closed on any non-1.0 speed request, i.e. lanes whose overrun cannot
	// be remediated by resynthesis and may therefore be escalated (Issue #94).
	FixedRateSpeakers []string  `json:"fixed_rate_speakers,omitempty"`
	CASHash           string    `json:"cas_hash,omitempty"`
	ProvenanceHash    string    `json:"provenance_hash,omitempty"`
	OverallStatus     string    `json:"overall_status"` // "PASS", "REVIEW_REQUIRED", "FAIL"
	CreatedAt         time.Time `json:"created_at"`
}

// VoiceAssignmentInput defines input parameters for generating/freezing a VoiceAssignment.
type VoiceAssignmentInput struct {
	RunID                 string                  `json:"run_id"`
	AssetID               string                  `json:"asset_id"`
	JobID                 string                  `json:"job_id,omitempty"`
	TargetLanguage        string                  `json:"target_language"` // "vi" or "en"
	CustomAssignments     map[string]VoiceProfile `json:"custom_assignments,omitempty"`
	UseSameVoiceForAll    bool                    `json:"use_same_voice_for_all"`
	ExecutionProfile      ExecutionProfile        `json:"execution_profile,omitempty"`
	DubScriptVariantCAS   string                  `json:"dub_script_variant_cas,omitempty"`
	TranscriptArtifactCAS string                  `json:"transcript_artifact_cas,omitempty"`
}

// VoiceAuditionInput defines input parameters for pre-dub voice audition.
type VoiceAuditionInput struct {
	RunID          string       `json:"run_id"`
	AssetID        string       `json:"asset_id"`
	TargetLanguage string       `json:"target_language"` // "vi" or "en"
	Voice          VoiceProfile `json:"voice"`
	SampleText     string       `json:"sample_text,omitempty"`
	IsContextual   bool         `json:"is_contextual"` // True for 10s contextual audition mixed with BGM
	SegmentIndex   int          `json:"segment_index,omitempty"`
}

// VoiceAuditionResult represents the result of a voice audition probe.
type VoiceAuditionResult struct {
	Voice              VoiceProfile `json:"voice"`
	AudioCASHash       string       `json:"audio_cas_hash"`
	AudioCASPath       string       `json:"audio_cas_path"`
	MeasuredDurationMs int64        `json:"measured_duration_ms"`
	IsContextual       bool         `json:"is_contextual"`
	ContextualMixed    bool         `json:"contextual_mixed"`
	SampleText         string       `json:"sample_text"`
	ProviderID         string       `json:"provider_id,omitempty"`
	ModelName          string       `json:"model_name,omitempty"`
	ModelVersion       string       `json:"model_version,omitempty"`
	// AudioBytes carries the bounded audition artifact for transport serialization.
	// It is never part of the JSON contract; transports encode it into a playable
	// data URL and browser clients never receive a CAS/local path.
	AudioBytes []byte `json:"-"`
}

// DubbingJobInput defines the inputs required to run the TTS synthesis & fit controller pipeline.
type DubbingJobInput struct {
	RunID                 string           `json:"run_id"`
	AssetID               string           `json:"asset_id"`
	JobID                 string           `json:"job_id,omitempty"`
	TargetLanguage        string           `json:"target_language"` // "vi" or "en"
	DubScriptVariantCAS   string           `json:"dub_script_variant_cas,omitempty"`
	VoiceAssignmentCAS    string           `json:"voice_assignment_cas,omitempty"`
	TranscriptArtifactCAS string           `json:"transcript_artifact_cas,omitempty"`
	AudioRolePlanCAS      string           `json:"audio_role_plan_cas,omitempty"`
	ExecutionProfile      ExecutionProfile `json:"execution_profile,omitempty"`
	AuthorizedCredentials []string         `json:"authorized_credentials,omitempty"`
}

// ComputeVoiceAssignmentInputHash creates a deterministic hash for VoiceAssignment input.
func ComputeVoiceAssignmentInputHash(assetID, targetLang string, assignments map[string]VoiceProfile, sameVoice bool) (string, error) {
	payload := struct {
		AssetID        string                  `json:"asset_id"`
		TargetLanguage string                  `json:"target_language"`
		Assignments    map[string]VoiceProfile `json:"assignments"`
		SameVoice      bool                    `json:"same_voice"`
	}{
		AssetID:        assetID,
		TargetLanguage: targetLang,
		Assignments:    assignments,
		SameVoice:      sameVoice,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}
