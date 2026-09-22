package service_test

import (
	"context"
	"math"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
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
