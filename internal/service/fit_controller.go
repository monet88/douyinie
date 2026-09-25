package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
)

// FitControllerConfig holds parameters for the measured-duration fit controller.
type FitControllerConfig = domain.FitControllerConfig

// DefaultFitControllerConfig returns the standard fit controller configuration.
func DefaultFitControllerConfig() FitControllerConfig {
	return domain.DefaultFitControllerConfig()
}

// FitController evaluates synthesized audio candidates against immutable source timing windows.
type FitController struct {
	config FitControllerConfig
}

// NewFitController creates a new FitController instance.
func NewFitController(cfg ...FitControllerConfig) *FitController {
	c := DefaultFitControllerConfig()
	if len(cfg) > 0 {
		c = cfg[0]
	}
	return &FitController{config: c}
}

// FitEvaluationInput defines the inputs for evaluating a candidate's fit.
type FitEvaluationInput struct {
	SegmentIndex       int     `json:"segment_index"`
	SpeakerID          string  `json:"speaker_id"`
	StartMs            int64   `json:"start_ms"`
	EndMs              int64   `json:"end_ms"`
	NextTurnStartMs    int64   `json:"next_turn_start_ms"`   // Start of next speech turn (0 if none)
	NextTurnSpeakerID  string  `json:"next_turn_speaker_id"` // Speaker of next speech turn
	SourceGapAfterMs   int64   `json:"source_gap_after_ms"`  // Immutable source silence until next turn
	MeasuredDurationMs int64   `json:"measured_duration_ms"` // Probed waveform duration
	AttemptNumber      int     `json:"attempt_number"`       // Current synthesis attempt (1, 2, ...)
	CurrentSpeed       float64 `json:"current_speed"`        // Current speed multiplier
	CanShortenText     bool    `json:"can_shorten_text"`     // Whether text can be further condensed
	SupportsSpeedFit   bool    `json:"supports_speed_fit"`   // Whether provider supports native speed adjustment
	FixedRateVoice     bool    `json:"fixed_rate_voice"`     // Whether a non-1.0 speed request fails closed (preset-voice lanes)
	DubPlaybackEndMs   int64   `json:"dub_playback_end_ms"`  // Accepted playback ceiling; source EndMs when borrowing is unavailable.
	EffectiveReserveMs int64   `json:"effective_reserve_ms"`
	// OutputSampleRate is the sample rate of the mix this candidate lands in (the background stem
	// rate the mixer resamples to); 0 = unknown, and the fit then falls back to the millisecond
	// window instead of the frame-exact one.
	OutputSampleRate int `json:"output_sample_rate,omitempty"`
	// MeasuredFrames is the exact decoded frame count (per channel) of the candidate waveform;
	// 0 = unknown.
	MeasuredFrames int64 `json:"measured_frames,omitempty"`
	// MeasuredSampleRate is the sample rate of that decoded waveform; 0 = unknown.
	MeasuredSampleRate int `json:"measured_sample_rate,omitempty"`
}

// FitEvaluationResult contains the controller's decision and fit metrics.
type FitEvaluationResult struct {
	Decision           domain.FitAction `json:"decision"`
	UsableSlotMs       int64            `json:"usable_slot_ms"`
	SlotDurationMs     int64            `json:"slot_duration_ms"`
	MeasuredDurationMs int64            `json:"measured_duration_ms"`
	DurationDeltaMs    int64            `json:"duration_delta_ms"` // Measured - Usable (positive = overrun)
	RecommendedSpeed   float64          `json:"recommended_speed"` // Recommended speed for RESYNTH
	NaturalGapMs       int64            `json:"natural_gap_ms"`
	Reason             string           `json:"reason"`
	RequiresReview     bool             `json:"requires_review"`
	ReviewReason       string           `json:"review_reason,omitempty"`
	DubPlaybackEndMs   int64            `json:"dub_playback_end_ms"`
	EffectiveReserveMs int64            `json:"effective_reserve_ms"`
	FitPolicyID        string           `json:"fit_policy_id"`
}

func (fc *FitController) policyID() string {
	cfg := fc.config
	if cfg.PolicyVersion == "" || !isFinitePositive(cfg.ReserveRatio) || cfg.ReserveRatio > 1 || cfg.MinNaturalGapMs < 0 || cfg.MaxNaturalGapMs < cfg.MinNaturalGapMs {
		return ""
	}
	// Identity covers the whole effective config: freezing and hashing "the effective
	// reserve configuration" (#153) must not let a changed field (speed ceiling,
	// regroup allowance, default gap) keep the previous identity.
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return cfg.PolicyVersion + ":" + hex.EncodeToString(h[:])
}

func isFinitePositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// ResolvePlaybackWindow computes the only allowed silence borrowing: a genuine
// gap before the next canonical speech/vocal boundary, minus the frozen reserve.
func (fc *FitController) ResolvePlaybackWindow(sourceEndMs, nextVocalStartMs int64) (playbackEndMs, reserveMs int64, policyID string) {
	policyID = fc.policyID()
	if sourceEndMs <= 0 || nextVocalStartMs <= sourceEndMs || policyID == "" {
		return sourceEndMs, 0, policyID
	}
	gap := nextVocalStartMs - sourceEndMs
	reserve := int64(float64(gap) * fc.config.ReserveRatio)
	if reserve < fc.config.MinNaturalGapMs {
		reserve = fc.config.MinNaturalGapMs
	}
	if reserve > fc.config.MaxNaturalGapMs {
		reserve = fc.config.MaxNaturalGapMs
	}
	if reserve > gap {
		reserve = gap
	}
	end := nextVocalStartMs - reserve
	if end < sourceEndMs {
		end = sourceEndMs
	}
	return end, reserve, policyID
}

// EvaluateCandidate evaluates a measured synthesized audio candidate against immutable source constraints.
func (fc *FitController) EvaluateCandidate(ctx context.Context, in FitEvaluationInput) FitEvaluationResult {
	// A controller whose config cannot resolve a frozen policy identity cannot classify anything:
	// nothing downstream (mixer lineage check, artifact evidence) can validate a verdict it stamps,
	// so fail closed instead of resolving an ACCEPT the mix stage will refuse.
	policyID := fc.policyID()
	if policyID == "" {
		return FitEvaluationResult{Decision: domain.FitActionReview, SlotDurationMs: 0, MeasuredDurationMs: in.MeasuredDurationMs, RequiresReview: true, ReviewReason: "INVALID_FIT_POLICY", Reason: "fit controller policy identity is not resolvable", DubPlaybackEndMs: in.EndMs, FitPolicyID: policyID}
	}
	if in.StartMs < 0 || in.EndMs <= in.StartMs || in.MeasuredDurationMs <= 0 {
		return FitEvaluationResult{Decision: domain.FitActionReview, SlotDurationMs: 0, MeasuredDurationMs: in.MeasuredDurationMs, RequiresReview: true, ReviewReason: "INVALID_TIMING", Reason: "invalid source timing or measured duration", DubPlaybackEndMs: in.EndMs, FitPolicyID: fc.policyID()}
	}
	playbackEndMs := in.DubPlaybackEndMs
	if playbackEndMs < in.EndMs {
		playbackEndMs = in.EndMs
	}
	if playbackEndMs == 0 {
		playbackEndMs = in.EndMs
	}
	slotDurationMs := playbackEndMs - in.StartMs
	if slotDurationMs <= 0 {
		return FitEvaluationResult{Decision: domain.FitActionReview, SlotDurationMs: slotDurationMs, MeasuredDurationMs: in.MeasuredDurationMs, RequiresReview: true, ReviewReason: "INVALID_TIMING", Reason: "invalid playback window", DubPlaybackEndMs: playbackEndMs, FitPolicyID: fc.policyID()}
	}

	// 1. Calculate natural inter-turn breathing gap
	naturalGapMs := in.EffectiveReserveMs

	// The usable slot keeps the frozen natural reserve free, so Case 1 accepts only a candidate
	// that also preserves that pause while Case 2 stays reachable for one that still fits the
	// wider accepted window and merely consumes part of the reserve. A non-positive reserve
	// leaves the usable slot equal to the accepted window, preserving the previous behavior.
	usableSlotMs := slotDurationMs - in.EffectiveReserveMs
	if usableSlotMs < 1 {
		usableSlotMs = 1
	}
	if usableSlotMs > slotDurationMs {
		usableSlotMs = slotDurationMs
	}

	deltaMs := in.MeasuredDurationMs - usableSlotMs
	hardOverrunMs := in.MeasuredDurationMs - slotDurationMs

	// The mixer enforces the accepted playback window frame-exactly. The fit proves the same
	// window from the strongest evidence it was given, in one rule with three tiers:
	//  1. exact waveform geometry: the frames the mixer will actually place, after resampling;
	//  2. output rate only: a probe of D whole milliseconds can hide up to
	//     floor((D+1)*rate/1000) frames (N*1000/rate < D+1), so that bound must fit the window;
	//  3. no rate: the millisecond window alone, which cannot prove frame-exact placement.
	fitsPlaybackWindow := in.MeasuredDurationMs <= slotDurationMs
	if in.OutputSampleRate > 0 {
		windowFrames := media.PlaybackWindowFrames(in.StartMs, playbackEndMs, in.OutputSampleRate)
		if in.MeasuredFrames > 0 && in.MeasuredSampleRate > 0 {
			fitsPlaybackWindow = media.ResampledPCM16Frames(in.MeasuredFrames, in.MeasuredSampleRate, in.OutputSampleRate) <= windowFrames
		} else {
			fitsPlaybackWindow = ((in.MeasuredDurationMs+1)*int64(in.OutputSampleRate))/1000 <= windowFrames
		}
	}

	// Case 1: Fits comfortably within usable slot (including natural breathing room)
	if in.MeasuredDurationMs <= usableSlotMs && fitsPlaybackWindow {
		return FitEvaluationResult{
			Decision:           domain.FitActionAccept,
			UsableSlotMs:       usableSlotMs,
			SlotDurationMs:     slotDurationMs,
			MeasuredDurationMs: in.MeasuredDurationMs,
			DurationDeltaMs:    deltaMs,
			RecommendedSpeed:   1.0,
			NaturalGapMs:       naturalGapMs,
			Reason:             "fits within usable slot preserving natural breathing gap",
			RequiresReview:     false,
			DubPlaybackEndMs:   playbackEndMs, EffectiveReserveMs: naturalGapMs, FitPolicyID: fc.policyID(),
		}
	}

	// Case 2: Compatibility branch for a candidate that still fits the accepted playback window.
	if in.MeasuredDurationMs <= slotDurationMs && fitsPlaybackWindow {
		// Preserve the remaining playback-window margin as perceptual gap evidence.
		remainingGap := slotDurationMs - in.MeasuredDurationMs
		return FitEvaluationResult{
			Decision:           domain.FitActionAccept,
			UsableSlotMs:       usableSlotMs,
			SlotDurationMs:     slotDurationMs,
			MeasuredDurationMs: in.MeasuredDurationMs,
			DurationDeltaMs:    deltaMs,
			RecommendedSpeed:   1.0,
			NaturalGapMs:       remainingGap,
			Reason:             fmt.Sprintf("fits within accepted playback window (remaining pause %dms)", remainingGap),
			RequiresReview:     false,
			DubPlaybackEndMs:   playbackEndMs, EffectiveReserveMs: naturalGapMs, FitPolicyID: fc.policyID(),
		}
	}

	// Case 3: Overruns the accepted playback window, or fits it in milliseconds while the
	// frame-exact placement inside it cannot be proven from the floored probe. The thresholds
	// stay keyed to slotDurationMs, the accepted window itself. Must not accept without
	// remediation!
	overrunLabel := fmt.Sprintf("overrun by %dms", hardOverrunMs)
	if hardOverrunMs <= 0 {
		overrunLabel = "floored duration leaves no provable frame margin in the accepted window"
	}

	// Strategy A: RESYNTH via measured speed-fit
	// Needed speed factor to fit inside usable slot
	speedFactor := float64(in.MeasuredDurationMs) / float64(usableSlotMs)
	if in.CurrentSpeed > 0 {
		speedFactor = in.CurrentSpeed * (float64(in.MeasuredDurationMs) / float64(usableSlotMs))
	}

	// A fixed-rate preset-voice lane rejects any non-1.0 speed request, so a
	// speed resynthesis is not a remedy for it: overrun must go through
	// rewrite/regroup/review instead of a request the provider cannot honor.
	if !in.FixedRateVoice && in.AttemptNumber < 2 && speedFactor <= fc.config.MaxSpeedMultiplier && (in.SupportsSpeedFit || speedFactor <= 1.15) {
		return FitEvaluationResult{
			Decision:           domain.FitActionResynth,
			UsableSlotMs:       usableSlotMs,
			SlotDurationMs:     slotDurationMs,
			MeasuredDurationMs: in.MeasuredDurationMs,
			DurationDeltaMs:    deltaMs,
			RecommendedSpeed:   speedFactor,
			NaturalGapMs:       naturalGapMs,
			Reason:             fmt.Sprintf("%s: requesting resynth with calibrated speed %.2fx", overrunLabel, speedFactor),
			RequiresReview:     false,
			DubPlaybackEndMs:   playbackEndMs, EffectiveReserveMs: naturalGapMs, FitPolicyID: fc.policyID(),
		}
	}

	// Strategy B: REWRITE (shorten-first text adaptation)
	if in.CanShortenText && in.AttemptNumber < 3 {
		return FitEvaluationResult{
			Decision:           domain.FitActionRewrite,
			UsableSlotMs:       usableSlotMs,
			SlotDurationMs:     slotDurationMs,
			MeasuredDurationMs: in.MeasuredDurationMs,
			DurationDeltaMs:    deltaMs,
			RecommendedSpeed:   1.0,
			NaturalGapMs:       naturalGapMs,
			Reason:             fmt.Sprintf("%s: speed-fit limit reached, requesting shorten-first rewrite", overrunLabel),
			RequiresReview:     false,
			DubPlaybackEndMs:   playbackEndMs, EffectiveReserveMs: naturalGapMs, FitPolicyID: fc.policyID(),
		}
	}

	// Strategy C: REGROUP within same speaker turn if adjacent segment belongs to same speaker
	if fc.config.AllowRegroupSameTurn && in.NextTurnSpeakerID != "" && in.NextTurnSpeakerID == in.SpeakerID && in.SourceGapAfterMs > 0 && in.SourceGapAfterMs < 600 {
		return FitEvaluationResult{
			Decision:           domain.FitActionRegroup,
			UsableSlotMs:       usableSlotMs,
			SlotDurationMs:     slotDurationMs,
			MeasuredDurationMs: in.MeasuredDurationMs,
			DurationDeltaMs:    deltaMs,
			RecommendedSpeed:   1.0,
			NaturalGapMs:       naturalGapMs,
			Reason:             fmt.Sprintf("%s: requesting same-speaker turn regrouping across %dms gap", overrunLabel, in.SourceGapAfterMs),
			RequiresReview:     false,
			DubPlaybackEndMs:   playbackEndMs, EffectiveReserveMs: naturalGapMs, FitPolicyID: fc.policyID(),
		}
	}

	// Strategy D: REVIEW (Automated remedies exhausted, flag for operator review; overlong candidate cannot reach mixer)
	return FitEvaluationResult{
		Decision:           domain.FitActionReview,
		UsableSlotMs:       usableSlotMs,
		SlotDurationMs:     slotDurationMs,
		MeasuredDurationMs: in.MeasuredDurationMs,
		DurationDeltaMs:    deltaMs,
		RecommendedSpeed:   speedFactor,
		NaturalGapMs:       0,
		Reason:             fmt.Sprintf("unresolvable duration overrun: %s (measured %dms, slot %dms)", overrunLabel, in.MeasuredDurationMs, slotDurationMs),
		RequiresReview:     true,
		ReviewReason:       "DURATION_OVERRUN",
		DubPlaybackEndMs:   playbackEndMs, EffectiveReserveMs: naturalGapMs, FitPolicyID: fc.policyID(),
	}
}
