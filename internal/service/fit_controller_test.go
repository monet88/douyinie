package service_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/service"
)

func TestFitController_Accept_WhenWithinUsableSlot(t *testing.T) {
	fc := service.NewFitController()

	res := fc.EvaluateCandidate(context.Background(), service.FitEvaluationInput{
		SegmentIndex:       0,
		SpeakerID:          "SPEAKER_00",
		StartMs:            0,
		EndMs:              2000,
		NextTurnStartMs:    2500,
		NextTurnSpeakerID:  "SPEAKER_01",
		MeasuredDurationMs: 1600,
		AttemptNumber:      1,
	})

	if res.Decision != domain.FitActionAccept {
		t.Fatalf("expected ACCEPT, got %s: %s", res.Decision, res.Reason)
	}
	if res.RequiresReview {
		t.Errorf("expected RequiresReview=false, got true")
	}
}

func TestFitController_Resynth_WhenOverrunWithinSpeedLimit(t *testing.T) {
	fc := service.NewFitController()

	res := fc.EvaluateCandidate(context.Background(), service.FitEvaluationInput{
		SegmentIndex:       0,
		SpeakerID:          "SPEAKER_00",
		StartMs:            0,
		EndMs:              2000,
		NextTurnStartMs:    2200,
		NextTurnSpeakerID:  "SPEAKER_01",
		MeasuredDurationMs: 2200, // exceeds slot 2000ms by 200ms (1.10x needed speed)
		AttemptNumber:      1,
		SupportsSpeedFit:   true,
	})

	if res.Decision != domain.FitActionResynth {
		t.Fatalf("expected RESYNTH, got %s: %s", res.Decision, res.Reason)
	}
	if res.RecommendedSpeed <= 1.0 {
		t.Errorf("expected recommended speed > 1.0, got %f", res.RecommendedSpeed)
	}
	if res.RequiresReview {
		t.Errorf("expected RequiresReview=false for automated resynth, got true")
	}
}

func TestFitController_Rewrite_WhenSpeedFitExhaustedAndCanShorten(t *testing.T) {
	fc := service.NewFitController()

	res := fc.EvaluateCandidate(context.Background(), service.FitEvaluationInput{
		SegmentIndex:       0,
		SpeakerID:          "SPEAKER_00",
		StartMs:            0,
		EndMs:              1000,
		NextTurnStartMs:    1100,
		NextTurnSpeakerID:  "SPEAKER_01",
		MeasuredDurationMs: 1800, // 1.8x needed speed, exceeds 1.25x max
		AttemptNumber:      1,
		CanShortenText:     true,
	})

	if res.Decision != domain.FitActionRewrite {
		t.Fatalf("expected REWRITE, got %s: %s", res.Decision, res.Reason)
	}
	if res.RequiresReview {
		t.Errorf("expected RequiresReview=false for automated rewrite, got true")
	}
}

func TestFitController_Regroup_WhenSameSpeakerTurnAdjacent(t *testing.T) {
	fc := service.NewFitController()

	res := fc.EvaluateCandidate(context.Background(), service.FitEvaluationInput{
		SegmentIndex:       0,
		SpeakerID:          "SPEAKER_00",
		StartMs:            0,
		EndMs:              1000,
		NextTurnStartMs:    1300,
		NextTurnSpeakerID:  "SPEAKER_00", // same speaker!
		SourceGapAfterMs:   300,
		MeasuredDurationMs: 1800,
		AttemptNumber:      2,
		CanShortenText:     false,
	})

	if res.Decision != domain.FitActionRegroup {
		t.Fatalf("expected REGROUP, got %s: %s", res.Decision, res.Reason)
	}
}

func TestFitController_Review_WhenUnresolvableOverrun(t *testing.T) {
	fc := service.NewFitController()

	res := fc.EvaluateCandidate(context.Background(), service.FitEvaluationInput{
		SegmentIndex:       0,
		SpeakerID:          "SPEAKER_00",
		StartMs:            0,
		EndMs:              1000,
		NextTurnStartMs:    1100,
		NextTurnSpeakerID:  "SPEAKER_01", // different speaker
		MeasuredDurationMs: 2500,         // extreme 2.5x overrun
		AttemptNumber:      3,            // exhausted attempts
		CanShortenText:     false,
	})

	if res.Decision != domain.FitActionReview {
		t.Fatalf("expected REVIEW, got %s: %s", res.Decision, res.Reason)
	}
	if !res.RequiresReview {
		t.Errorf("expected RequiresReview=true for unresolvable overrun")
	}
	if res.ReviewReason != "DURATION_OVERRUN" {
		t.Errorf("expected ReviewReason=DURATION_OVERRUN, got %s", res.ReviewReason)
	}
}

func TestFitController_PlaybackWindowBorrowsOnlyReservedGap(t *testing.T) {
	fc := service.NewFitController()
	playbackEnd, reserve, policyID := fc.ResolvePlaybackWindow(1000, 2000)
	if playbackEnd != 1700 || reserve != 300 || policyID == "" {
		t.Fatalf("playback window = end %d reserve %d policy %q, want 1700/300/non-empty", playbackEnd, reserve, policyID)
	}
	res := fc.EvaluateCandidate(context.Background(), service.FitEvaluationInput{
		SegmentIndex: 0, StartMs: 0, EndMs: 1000, MeasuredDurationMs: 1500,
		DubPlaybackEndMs: playbackEnd, EffectiveReserveMs: reserve, AttemptNumber: 1,
	})
	if res.Decision != domain.FitActionAccept || res.DubPlaybackEndMs != 1700 {
		t.Fatalf("borrowed-window candidate should ACCEPT, got %+v", res)
	}
}

func TestFitController_PolicyIdentityCoversEveryEffectiveField(t *testing.T) {
	baseCfg := service.DefaultFitControllerConfig()
	_, _, baseID := service.NewFitController(baseCfg).ResolvePlaybackWindow(1000, 2000)
	if baseID == "" {
		t.Fatal("default fit config must produce a policy identity")
	}
	if _, _, again := service.NewFitController(baseCfg).ResolvePlaybackWindow(1000, 2000); again != baseID {
		t.Fatalf("policy identity is not deterministic: %q != %q", again, baseID)
	}

	variants := map[string]func(*service.FitControllerConfig){
		"max_speed_multiplier":    func(c *service.FitControllerConfig) { c.MaxSpeedMultiplier = 1.30 },
		"allow_regroup_same_turn": func(c *service.FitControllerConfig) { c.AllowRegroupSameTurn = false },
		"default_natural_gap_ms":  func(c *service.FitControllerConfig) { c.DefaultNaturalGapMs = 200 },
		"min_natural_gap_ms":      func(c *service.FitControllerConfig) { c.MinNaturalGapMs = 60 },
		"max_natural_gap_ms":      func(c *service.FitControllerConfig) { c.MaxNaturalGapMs = 500 },
		"reserve_ratio":           func(c *service.FitControllerConfig) { c.ReserveRatio = 0.25 },
		"policy_version":          func(c *service.FitControllerConfig) { c.PolicyVersion = "playback-window-v2" },
	}
	for name, mutate := range variants {
		cfg := baseCfg
		mutate(&cfg)
		_, _, changedID := service.NewFitController(cfg).ResolvePlaybackWindow(1000, 2000)
		if changedID == baseID {
			t.Errorf("changing %s must change the frozen fit policy identity", name)
		}
	}
}

func TestFitController_NoBoundaryAndInvalidTimingFailClosed(t *testing.T) {
	fc := service.NewFitController()
	playbackEnd, reserve, _ := fc.ResolvePlaybackWindow(1000, 0)
	if playbackEnd != 1000 || reserve != 0 {
		t.Fatalf("media tail must not be borrowed: end=%d reserve=%d", playbackEnd, reserve)
	}
	res := fc.EvaluateCandidate(context.Background(), service.FitEvaluationInput{StartMs: 1000, EndMs: 1000, MeasuredDurationMs: 500})
	if res.Decision != domain.FitActionReview || !res.RequiresReview || res.SlotDurationMs != 0 {
		t.Fatalf("invalid zero slot must fail closed without fallback: %+v", res)
	}

	for _, tc := range []service.FitEvaluationInput{
		{StartMs: -1, EndMs: 1000, MeasuredDurationMs: 500},
		{StartMs: math.MaxInt64, EndMs: math.MinInt64, MeasuredDurationMs: 500},
	} {
		res = fc.EvaluateCandidate(context.Background(), tc)
		if res.Decision != domain.FitActionReview || !res.RequiresReview || res.SlotDurationMs != 0 || res.ReviewReason != "INVALID_TIMING" {
			t.Fatalf("negative/overflowing source timing must fail closed without arithmetic fallback: input=%+v result=%+v", tc, res)
		}
	}
}

// Finding A: the reserve-preserving usable slot must stay distinct from the wider accepted
// playback window, otherwise Case 2 is subsumed and unreachable.
func TestFitController_UsableSlotPreservesReserveAndPlaybackWindow(t *testing.T) {
	ctx := context.Background()
	fc := service.NewFitController()

	// start=1000 end=2000 with an accepted window end of 2400 and a 400ms frozen reserve:
	// accepted window = 1400ms, reserve-preserving usable slot = 1000ms.
	base := service.FitEvaluationInput{
		SegmentIndex: 0, SpeakerID: "SPEAKER_00",
		StartMs: 1000, EndMs: 2000, DubPlaybackEndMs: 2400, EffectiveReserveMs: 400,
		AttemptNumber: 1, CanShortenText: true, SupportsSpeedFit: true,
	}

	// (1) No reserve: usable slot collapses onto the accepted window, exactly as before.
	noReserve := base
	noReserve.EffectiveReserveMs = 0
	noReserve.DubPlaybackEndMs = 2000
	noReserve.MeasuredDurationMs = 1000
	res := fc.EvaluateCandidate(ctx, noReserve)
	if res.Decision != domain.FitActionAccept || res.UsableSlotMs != 1000 || res.SlotDurationMs != 1000 || res.NaturalGapMs != 0 || res.DurationDeltaMs != 0 {
		t.Fatalf("reserve-free full fit must ACCEPT exactly as before: %+v", res)
	}

	// (2) Candidate between the usable slot and the accepted window: ACCEPT, consuming part of
	// the reserve, with the remaining window margin reported as the natural gap.
	in := base
	in.MeasuredDurationMs = 1200
	res = fc.EvaluateCandidate(ctx, in)
	if res.Decision != domain.FitActionAccept || res.UsableSlotMs != 1000 || res.SlotDurationMs != 1400 {
		t.Fatalf("candidate inside the accepted window must ACCEPT: %+v", res)
	}
	if res.NaturalGapMs != 200 || res.DurationDeltaMs != 200 || res.EffectiveReserveMs != 400 || res.RecommendedSpeed != 1.0 {
		t.Fatalf("accepted-window fit must report the remaining pause as gap: %+v", res)
	}
	if !strings.Contains(res.Reason, "remaining pause 200ms") {
		t.Fatalf("accepted-window fit must name the remaining pause, got %q", res.Reason)
	}

	// (2b) The two branches split exactly at the usable slot, one millisecond apart.
	atUsable := base
	atUsable.MeasuredDurationMs = 1000
	res = fc.EvaluateCandidate(ctx, atUsable)
	if res.Decision != domain.FitActionAccept || res.NaturalGapMs != 400 || res.UsableSlotMs != 1000 {
		t.Fatalf("candidate at the usable slot boundary must keep the full reserve: %+v", res)
	}
	if !strings.Contains(res.Reason, "fits within usable slot") {
		t.Fatalf("candidate at the usable slot must stay in the reserve-preserving branch, got %q", res.Reason)
	}
	aboveUsable := base
	aboveUsable.MeasuredDurationMs = 1001
	res = fc.EvaluateCandidate(ctx, aboveUsable)
	if res.Decision != domain.FitActionAccept || res.NaturalGapMs != 399 || res.UsableSlotMs != 1000 || res.DurationDeltaMs != 1 {
		t.Fatalf("one millisecond past the usable slot must reach the accepted-window branch: %+v", res)
	}

	// (3) Candidate inside the reserve-preserving usable slot: ACCEPT with the reserve intact.
	in.MeasuredDurationMs = 900
	res = fc.EvaluateCandidate(ctx, in)
	if res.Decision != domain.FitActionAccept || res.UsableSlotMs != 1000 || res.NaturalGapMs != 400 || res.DurationDeltaMs != -100 {
		t.Fatalf("candidate inside the usable slot must ACCEPT preserving the reserve: %+v", res)
	}

	// (4) Candidate past the accepted window: remediation, thresholded on the accepted window.
	in.MeasuredDurationMs = 1750
	res = fc.EvaluateCandidate(ctx, in)
	if res.Decision == domain.FitActionAccept {
		t.Fatalf("candidate past the accepted window must not ACCEPT: %+v", res)
	}
	if !strings.Contains(res.Reason, "overrun by 350ms") {
		t.Fatalf("remediation must be keyed to the accepted window (1750-1400), got %q", res.Reason)
	}

	// (4b) The same window overrun with every remedy exhausted parks the candidate for review
	// rather than letting it reach the mixer.
	exhausted := in
	exhausted.CanShortenText = false
	exhausted.AttemptNumber = 3
	res = fc.EvaluateCandidate(ctx, exhausted)
	if res.Decision != domain.FitActionReview || !res.RequiresReview || res.ReviewReason != "DURATION_OVERRUN" {
		t.Fatalf("unremedied window overrun must park for review: %+v", res)
	}
	if res.UsableSlotMs != 1000 || res.SlotDurationMs != 1400 {
		t.Fatalf("review must still report the reserve-preserving slot and the accepted window: %+v", res)
	}

	// (5) The resynth speed fits the reserve-preserving usable slot (1500/1000), not the window.
	cfg := service.DefaultFitControllerConfig()
	cfg.MaxSpeedMultiplier = 1.6
	fast := service.NewFitController(cfg)
	in.MeasuredDurationMs = 1500
	res = fast.EvaluateCandidate(ctx, in)
	if res.Decision != domain.FitActionResynth || res.RecommendedSpeed != 1.5 || res.DurationDeltaMs != 500 {
		t.Fatalf("resynth must target the usable slot: %+v", res)
	}

	// (6) A reserve larger than the accepted window still leaves a usable slot of at least one
	// millisecond instead of dividing by zero.
	in = base
	in.EffectiveReserveMs = 5000
	in.MeasuredDurationMs = 1750
	res = fc.EvaluateCandidate(ctx, in)
	if res.UsableSlotMs != 1 {
		t.Fatalf("usable slot must clamp to 1ms, got %d", res.UsableSlotMs)
	}
	if res.Decision != domain.FitActionRewrite {
		t.Fatalf("extreme reserve must fall through to remediation, got %s: %s", res.Decision, res.Reason)
	}
}

// Finding B: the fit must prove the accepted playback window from the strongest evidence it has,
// so an ACCEPT can never be refused by the frame-exact mixer check.
func TestFitController_PlaybackWindowFrameEvidencePrecedence(t *testing.T) {
	ctx := context.Background()
	fc := service.NewFitController()

	// Accepted window [0,1000] at 1000ms: 44100 output frames of room at 44.1kHz.
	const outputRate = 44100
	const windowFrames = 44100
	base := service.FitEvaluationInput{
		SegmentIndex: 0, SpeakerID: "SPEAKER_00",
		StartMs: 0, EndMs: 1000, DubPlaybackEndMs: 1000, OutputSampleRate: outputRate,
		AttemptNumber: 3, FixedRateVoice: true,
	}
	refuse := func(res service.FitEvaluationResult, want string) {
		t.Helper()
		if res.Decision == domain.FitActionAccept {
			t.Fatalf("%s: must not ACCEPT: %+v", want, res)
		}
	}

	// Tier 1: exact waveform geometry. The floored probe is identical for both candidates, so
	// only the frame count can tell them apart.
	exact := base
	exact.MeasuredDurationMs = 1000
	exact.MeasuredFrames = windowFrames
	exact.MeasuredSampleRate = outputRate
	res := fc.EvaluateCandidate(ctx, exact)
	if res.Decision != domain.FitActionAccept {
		t.Fatalf("candidate with exactly %d output frames must ACCEPT: %+v", windowFrames, res)
	}

	probe, err := media.ProbeWAVBytes(media.GeneratePCM16WAV(outputRate, 1, 1000))
	if err != nil || probe != exact.MeasuredDurationMs {
		t.Fatalf("fixture probe must read %dms, got %d (%v)", exact.MeasuredDurationMs, probe, err)
	}
	// Both candidates floor to the same 1000ms probe, so the pre-fix fit - whose whole ACCEPT
	// condition was MeasuredDurationMs <= SlotDurationMs - accepted the overflowing one too.
	for _, frames := range []int64{windowFrames, windowFrames + 1} {
		clipProbe, err := media.ProbeWAVBytes(media.EncodePCM16Samples(make([]int16, frames), outputRate, 1))
		if err != nil {
			t.Fatalf("probe %d-frame fixture: %v", frames, err)
		}
		if clipProbe != base.EndMs-base.StartMs {
			t.Fatalf("%d frames at %dHz must floor to the %dms window probe, got %dms", frames, outputRate, base.EndMs-base.StartMs, clipProbe)
		}
	}
	over := exact
	over.MeasuredFrames = windowFrames + 1
	refuse(fc.EvaluateCandidate(ctx, over), fmt.Sprintf("%d output frames overflow the window", windowFrames+1))

	// Same flip across a non-integer resample ratio (16kHz source into a 44.1kHz mix): 16000
	// frames resample to exactly 44100, one frame more overflows.
	resampled := base
	resampled.MeasuredDurationMs = 1000
	resampled.MeasuredSampleRate = 16000
	resampled.MeasuredFrames = 16000
	if res := fc.EvaluateCandidate(ctx, resampled); res.Decision != domain.FitActionAccept {
		t.Fatalf("16000 frames at 16kHz must resample into the window and ACCEPT: %+v", res)
	}
	resampled.MeasuredFrames = 16001
	refuse(fc.EvaluateCandidate(ctx, resampled), "16001 frames at 16kHz resample past the window")

	// Tier 2: output rate only. A floored 1000ms probe can hide up to floor(1001*44100/1000)
	// frames, so it cannot be accepted; a probe one millisecond short proves placement.
	rateOnly := base
	rateOnly.MeasuredDurationMs = 1000
	res = fc.EvaluateCandidate(ctx, rateOnly)
	refuse(res, "floored probe equal to the window cannot prove placement")
	if res.ReviewReason != "DURATION_OVERRUN" {
		t.Fatalf("unprovable placement must reach overrun remediation, got %+v", res)
	}
	rateOnly.MeasuredDurationMs = 999
	if res := fc.EvaluateCandidate(ctx, rateOnly); res.Decision != domain.FitActionAccept {
		t.Fatalf("probe with a full millisecond of margin must ACCEPT: %+v", res)
	}

	// Tier 3: no output rate at all degrades to the millisecond window it always used.
	noRate := base
	noRate.OutputSampleRate = 0
	noRate.MeasuredDurationMs = 1000
	if res := fc.EvaluateCandidate(ctx, noRate); res.Decision != domain.FitActionAccept {
		t.Fatalf("unknown output rate must keep the millisecond window: %+v", res)
	}
}

// F21: a controller whose policy identity cannot be resolved must fail closed deterministically
// instead of stamping an ACCEPT that no downstream lineage check can validate.
func TestFitController_UnresolvablePolicyFailsClosed(t *testing.T) {
	ctx := context.Background()
	in := service.FitEvaluationInput{
		SegmentIndex: 0, SpeakerID: "SPEAKER_00",
		StartMs: 0, EndMs: 1000, DubPlaybackEndMs: 1000,
		MeasuredDurationMs: 500, AttemptNumber: 1, CanShortenText: true,
	}
	for name, cfg := range map[string]service.FitControllerConfig{
		"zero_value":                  {},
		"missing_policy_version":      {ReserveRatio: 0.3, MinNaturalGapMs: 50, MaxNaturalGapMs: 400},
		"reserve_ratio_out_of_range":  {PolicyVersion: "v1", ReserveRatio: 1.5, MinNaturalGapMs: 50, MaxNaturalGapMs: 400},
		"inverted_natural_gap_bounds": {PolicyVersion: "v1", ReserveRatio: 0.3, MinNaturalGapMs: 400, MaxNaturalGapMs: 50},
	} {
		fc := service.NewFitController(cfg)
		playbackEnd, reserve, policyID := fc.ResolvePlaybackWindow(1000, 2000)
		if policyID != "" || playbackEnd != 1000 || reserve != 0 {
			t.Fatalf("%s: unresolvable policy must not borrow silence: end=%d reserve=%d policy=%q", name, playbackEnd, reserve, policyID)
		}

		res := fc.EvaluateCandidate(ctx, in)
		if res.Decision != domain.FitActionReview || !res.RequiresReview || res.ReviewReason != "INVALID_FIT_POLICY" {
			t.Fatalf("%s: invalid policy must fail closed with INVALID_FIT_POLICY, got %+v", name, res)
		}
		if res.SlotDurationMs != 0 || res.FitPolicyID != "" || res.NaturalGapMs != 0 {
			t.Fatalf("%s: fail-closed result must carry no slot or policy identity, got %+v", name, res)
		}

		if again := service.NewFitController(cfg).EvaluateCandidate(ctx, in); again != res {
			t.Fatalf("%s: invalid policy verdict must be deterministic: %+v != %+v", name, again, res)
		}
	}
}
