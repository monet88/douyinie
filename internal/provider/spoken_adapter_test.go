package provider

import (
	"context"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestSpokenScriptAdapter_UnseenSource_ProducesConciseCandidate(t *testing.T) {
	a := NewDefaultSpokenScriptAdapter()
	ctx := context.Background()

	viMeaning := "Vui lòng hãy mở cửa sổ và kiểm tra âm thanh."
	res, err := a.AdaptSpokenScript(ctx, SpokenScriptAdaptationRequest{
		SourceText:       "请打开窗户并检查声音。",
		SourceLanguage:   "zh",
		MeaningText:      viMeaning,
		TargetLanguage:   "vi",
		SlotDurationMs:   2300,
		SourceGapAfterMs: 120,
		HasNextTurn:      true,
	})
	if err != nil {
		t.Fatalf("AdaptSpokenScript VI failed: %v", err)
	}
	if !res.IsShortened {
		t.Errorf("expected unseen VI text to be shortened through generic filler removal")
	}
	if len(res.SpokenText) >= len(viMeaning) {
		t.Errorf("expected spoken text shorter than meaning: %q vs %q", res.SpokenText, viMeaning)
	}
	if res.TargetWordBudget <= 0 {
		t.Errorf("expected positive target word budget, got %d", res.TargetWordBudget)
	}
	if res.CadenceRatio <= 0 {
		t.Errorf("expected positive cadence ratio, got %f", res.CadenceRatio)
	}
	if res.EstimatedDurationMs > res.UsableSlotMs {
		t.Errorf("expected shortened VI text to fit source slot: %d ms vs %d ms", res.EstimatedDurationMs, res.UsableSlotMs)
	}
	if res.NaturalGapMs < 120 {
		t.Errorf("expected predicted pause to preserve 120ms source gap, got %dms", res.NaturalGapMs)
	}
	if res.RequiresReview {
		t.Errorf("expected unseen VI text to fit without review: %s", res.ReviewReason)
	}

	enMeaning := "Please open the window and do not close the door."
	resEn, err := a.AdaptSpokenScript(ctx, SpokenScriptAdaptationRequest{
		SourceText:       "请打开窗户并检查声音。",
		SourceLanguage:   "zh",
		MeaningText:      enMeaning,
		TargetLanguage:   "en",
		SlotDurationMs:   3200,
		SourceGapAfterMs: 120,
		HasNextTurn:      true,
	})
	if err != nil {
		t.Fatalf("AdaptSpokenScript EN failed: %v", err)
	}
	if !resEn.IsShortened {
		t.Errorf("expected unseen EN text to be shortened through generic politeness/contraction rules")
	}
	if len(resEn.SpokenText) >= len(enMeaning) {
		t.Errorf("expected EN spoken text shorter than meaning: %q vs %q", resEn.SpokenText, enMeaning)
	}
	if resEn.RequiresReview {
		t.Errorf("expected unseen EN text to fit without review: %s", resEn.ReviewReason)
	}
}

func TestSpokenScriptAdapter_SourceGapIsActualPredictedPause(t *testing.T) {
	a := NewDefaultSpokenScriptAdapter()
	res, err := a.AdaptSpokenScript(context.Background(), SpokenScriptAdaptationRequest{
		SourceText:       "打开窗户然后检查机器。",
		SourceLanguage:   "zh",
		MeaningText:      "Mở cửa sổ rồi kiểm tra máy.",
		TargetLanguage:   "vi",
		SlotDurationMs:   2000,
		SourceGapAfterMs: 150,
		HasNextTurn:      true,
	})
	if err != nil {
		t.Fatalf("AdaptSpokenScript failed: %v", err)
	}
	if res.UsableSlotMs != 2000 {
		t.Fatalf("source gap is outside immutable speech slot and must not reduce it: got usable %dms", res.UsableSlotMs)
	}
	if res.EstimatedDurationMs != 1970 {
		t.Fatalf("test fixture expected 1970ms estimate, got %dms", res.EstimatedDurationMs)
	}
	if res.NaturalGapMs != 180 {
		t.Fatalf("expected predicted pause 2000+150-1970=180ms, got %dms", res.NaturalGapMs)
	}
	if res.RequiresReview {
		t.Fatalf("fitting segment preserves source gap and should not require review: %s", res.ReviewReason)
	}
}

func TestSpokenScriptAdapter_FlagsSourceRelativeCadenceDeviation(t *testing.T) {
	a := NewDefaultSpokenScriptAdapter()
	res, err := a.AdaptSpokenScript(context.Background(), SpokenScriptAdaptationRequest{
		SourceText:            "这是一个非常快速而且连续不断的中文口播句子",
		SourceLanguage:        "zh",
		MeaningText:           "Mở cửa sổ ngay.",
		TargetLanguage:        "vi",
		SlotDurationMs:        2000,
		SourceSpeakingRateCPS: 9.0,
	})
	if err != nil {
		t.Fatalf("AdaptSpokenScript failed: %v", err)
	}
	if res.EstimatedDurationMs > 2000 {
		t.Fatalf("cadence regression must fit duration so review is caused by cadence, got %dms", res.EstimatedDurationMs)
	}
	if !res.RequiresReview {
		t.Fatal("expected source-relative cadence deviation to require review")
	}
	if res.ReviewReason != "CADENCE_TOO_SLOW" {
		t.Fatalf("expected CADENCE_TOO_SLOW, got %q", res.ReviewReason)
	}
	if res.CadenceRatio >= cadenceReviewMinRatio {
		t.Fatalf("expected cadence ratio below minimum %.2f, got %.3f", cadenceReviewMinRatio, res.CadenceRatio)
	}
}

func TestSpokenScriptAdapter_ProtectedTermsPreservedByGlossaryMatching(t *testing.T) {
	a := NewDefaultSpokenScriptAdapter()
	ctx := context.Background()

	// If shortening would drop a protected term (checked with word-boundary/CJK matching),
	// candidate reverts to meaningText.
	meaning := "Vui lòng mở ứng dụng OpenAI model ngay."
	res, err := a.AdaptSpokenScript(ctx, SpokenScriptAdaptationRequest{
		SourceText:     "请打开 OpenAI 模型。",
		SourceLanguage: "zh",
		MeaningText:    meaning,
		TargetLanguage: "vi",
		SlotDurationMs: 1500, // force shouldShorten = true
		ProtectedTerms: []domain.GlossaryEntry{
			{Source: "OpenAI", Target: "OpenAI"},
		},
	})
	if err != nil {
		t.Fatalf("AdaptSpokenScript failed: %v", err)
	}
	if !domain.GlossaryTermMatches(res.SpokenText, "OpenAI") {
		t.Fatalf("expected spoken text to preserve protected term 'OpenAI', got %q", res.SpokenText)
	}
}
