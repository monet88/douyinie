package service

import (
	"context"
	"fmt"

	"github.com/monet88/douyinie/internal/domain"
)

// FitControllerConfig holds parameters for the measured-duration fit controller.
type FitControllerConfig struct {
	MaxSpeedMultiplier   float64 // Maximum allowable speed factor for RESYNTH (default 1.25)
	MinNaturalGapMs      int64   // Minimum natural pause margin to preserve (default 50ms)
	MaxNaturalGapMs      int64   // Maximum natural pause margin (default 400ms)
	DefaultNaturalGapMs  int64   // Default natural pause margin (default 150ms)
	AllowRegroupSameTurn bool    // Whether to attempt same-speaker regrouping on overrun
}

// DefaultFitControllerConfig returns the standard fit controller configuration.
func DefaultFitControllerConfig() FitControllerConfig {
	return FitControllerConfig{
		MaxSpeedMultiplier:   1.25,
		MinNaturalGapMs:      50,
		MaxNaturalGapMs:      400,
		DefaultNaturalGapMs:  150,
		AllowRegroupSameTurn: true,
	}
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
}

// EvaluateCandidate evaluates a measured synthesized audio candidate against immutable source constraints.
func (fc *FitController) EvaluateCandidate(ctx context.Context, in FitEvaluationInput) FitEvaluationResult {
	slotDurationMs := in.EndMs - in.StartMs
	if slotDurationMs <= 0 {
		slotDurationMs = 1000
	}

	// 1. Calculate natural inter-turn breathing gap
	var naturalGapMs int64
	if in.NextTurnStartMs > in.EndMs {
		gap := in.NextTurnStartMs - in.EndMs
		// Reserve roughly 30% of silence gap, clamped between MinNaturalGapMs and MaxNaturalGapMs
		reserved := int64(float64(gap) * 0.30)
		if reserved < fc.config.MinNaturalGapMs {
			reserved = fc.config.MinNaturalGapMs
		}
		if reserved > fc.config.MaxNaturalGapMs {
			reserved = fc.config.MaxNaturalGapMs
		}
		if gap < reserved {
			reserved = gap
		}
		naturalGapMs = reserved
	} else if in.SourceGapAfterMs > 0 {
		naturalGapMs = in.SourceGapAfterMs
		if naturalGapMs > fc.config.MaxNaturalGapMs {
			naturalGapMs = fc.config.MaxNaturalGapMs
		}
	} else {
		// Single turn or no gap
		naturalGapMs = 0
	}

	usableSlotMs := slotDurationMs - naturalGapMs
	if usableSlotMs <= 0 {
		usableSlotMs = slotDurationMs
	}

	deltaMs := in.MeasuredDurationMs - usableSlotMs
	hardOverrunMs := in.MeasuredDurationMs - slotDurationMs

	// Case 1: Fits comfortably within usable slot (including natural breathing room)
	if in.MeasuredDurationMs <= usableSlotMs {
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
		}
	}

	// Case 2: Fits within raw source slot (tts_finish <= source_end), but slightly pinches the natural gap
	if in.MeasuredDurationMs <= slotDurationMs {
		// If measured duration is strictly <= slotDurationMs, zero overrun is satisfied!
		// Preserves perceptible turn gap
		remainingGap := slotDurationMs - in.MeasuredDurationMs
		return FitEvaluationResult{
			Decision:           domain.FitActionAccept,
			UsableSlotMs:       usableSlotMs,
			SlotDurationMs:     slotDurationMs,
			MeasuredDurationMs: in.MeasuredDurationMs,
			DurationDeltaMs:    deltaMs,
			RecommendedSpeed:   1.0,
			NaturalGapMs:       remainingGap,
			Reason:             fmt.Sprintf("fits within immutable source slot (remaining pause %dms)", remainingGap),
			RequiresReview:     false,
		}
	}

	// Case 3: Overruns immutable source slot (hardOverrunMs > 0).
	// Must not accept without remediation!

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
			Reason:             fmt.Sprintf("overrun by %dms: requesting resynth with calibrated speed %.2fx", hardOverrunMs, speedFactor),
			RequiresReview:     false,
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
			Reason:             fmt.Sprintf("overrun by %dms: speed-fit limit reached, requesting shorten-first rewrite", hardOverrunMs),
			RequiresReview:     false,
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
			Reason:             fmt.Sprintf("overrun by %dms: requesting same-speaker turn regrouping across %dms gap", hardOverrunMs, in.SourceGapAfterMs),
			RequiresReview:     false,
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
		Reason:             fmt.Sprintf("unresolvable duration overrun: measured %dms exceeds slot %dms by %dms", in.MeasuredDurationMs, slotDurationMs, hardOverrunMs),
		RequiresReview:     true,
		ReviewReason:       "DURATION_OVERRUN",
	}
}
