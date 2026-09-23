package benchmark_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
	"github.com/monet88/douyinie/internal/domain"
)

func TestQualityScoring_LevenshteinAndCER(t *testing.T) {
	// 1. Identical strings
	cer := benchmark.ComputeCER("欢迎大家来到现场", "欢迎大家来到现场")
	if cer != 0.0 {
		t.Errorf("expected CER 0.0 for identical strings, got %f", cer)
	}

	// 2. 2 deletions ("大家") out of 8 characters = 2/8 = 0.25
	cer = benchmark.ComputeCER("欢迎来到现场", "欢迎大家来到现场")
	if cer != 0.25 {
		t.Errorf("expected CER 0.25 for 2 deletions in 8 chars, got %f", cer)
	}

	// 3. 1 substitution out of 4 characters = 1/4 = 0.25
	cer = benchmark.ComputeCER("四十九元", "五十元")
	if cer <= 0.0 {
		t.Errorf("expected positive CER, got %f", cer)
	}

	// 4. Empty strings
	if benchmark.ComputeCER("", "") != 0.0 {
		t.Errorf("expected CER 0.0 for empty strings")
	}
	if benchmark.ComputeCER("something", "") != 1.0 {
		t.Errorf("expected CER 1.0 for non-empty hypothesis with empty reference")
	}
}

func TestQualityScoring_CaseEvaluationAndCategoryFailureVisibility(t *testing.T) {
	primaries, reserves := buildValidTestCorpus()
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_qc_test", "1.1", time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC), primaries, reserves)
	if err != nil {
		t.Fatalf("freeze corpus failed: %v", err)
	}

	// Build reference packs map
	packs := make(map[string]*benchmark.ReferenceAnnotationPack, len(primaries))
	for _, p := range primaries {
		packs[p.AssetID] = p.ReferencePack
	}

	// Build 48 local + 48 hybrid cases = 96 cases
	cases := make(map[string]*benchmark.QualityCaseEvidence)
	profiles := []string{"local", "hybrid"}
	languages := []string{"vi", "en"}

	for _, prof := range profiles {
		for _, asset := range manifest.PrimaryAssets {
			for _, lang := range languages {
				caseID := prof + "_" + asset.AssetID + "_" + lang
				now := time.Now().UTC()
				pack := packs[asset.AssetID]
				var speechBlocks []domain.SpeechBlock
				var wordTimings []domain.WordTiming
				for idx, seg := range pack.ChineseTranscript {
					speechBlocks = append(speechBlocks, domain.SpeechBlock{
						Index:      idx,
						StartMs:    seg.StartMs,
						EndMs:      seg.EndMs,
						SpeakerID:  seg.SpeakerID,
						SourceText: seg.ChineseText,
					})
					for _, w := range seg.Words {
						wordTimings = append(wordTimings, domain.WordTiming{
							Word:       w.Word,
							StartMs:    w.StartMs,
							EndMs:      w.EndMs,
							Confidence: 0.95,
						})
					}
				}
				var dubSegs []domain.DubSegment
				for idx, seg := range pack.ChineseTranscript {
					dubSegs = append(dubSegs, domain.DubSegment{
						Index:              idx,
						StartMs:            seg.StartMs,
						EndMs:              seg.EndMs,
						SlotDurationMs:     seg.EndMs - seg.StartMs,
						MeasuredDurationMs: seg.EndMs - seg.StartMs,
					})
				}
				var transSegs []domain.TranslationSegment
				for idx, seg := range pack.ChineseTranscript {
					targetText := "bản dịch tiếng Việt"
					if lang == "en" {
						targetText = "English translation"
					}
					for _, ci := range pack.CriticalItems {
						if ci.SegmentID == seg.SegmentID {
							if lang == "vi" {
								targetText += " " + ci.TargetRefVI
							} else {
								targetText += " " + ci.TargetRefEN
							}
						}
					}
					transSegs = append(transSegs, domain.TranslationSegment{
						Index:            idx,
						SourceText:       seg.ChineseText,
						TargetText:       targetText,
						PassedQAGate:     true,
						QAConfidence:     0.95,
						NegationPolarity: true,
					})
				}

				cases[caseID] = &benchmark.QualityCaseEvidence{
					CaseID:          caseID,
					SourceVideoID:   asset.SourceVideoID,
					PrimaryCategory: string(asset.PrimaryCategory),
					TargetLanguage:  lang,
					Profile:         prof,
					SourceAssetID:   asset.SourceAssetID,
					Status:          "COMPLETED",
					Stages: map[string]benchmark.StageExecutionEvidence{
						"speech_understand": {Stage: "speech_understand", Status: "COMPLETED"},
						"translate":         {Stage: "translate", Status: "COMPLETED"},
						"dub_synthesize":    {Stage: "dub_synthesize", Status: "COMPLETED"},
						"render_final":      {Stage: "render_final", Status: "COMPLETED"},
					},
					StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
						Transcript: &domain.TranscriptArtifact{
							SpeechBlocks: speechBlocks,
							WordTimings:  wordTimings,
						},
						Translation: &domain.TranslationVariant{
							Segments:       transSegs,
							OverallQAScore: 0.95,
						},
						DubSegments: &domain.DubSegmentsVariant{
							Segments: dubSegs,
						},
					},
					Telemetry: []benchmark.ResourceTelemetrySample{
						{Stage: "translate", RTF: 0.8, DeviceBaselineVRAMBytes: 2000, DevicePeakVRAMBytes: 4000},
					},
					CreatedAt:   now,
					CompletedAt: &now,
				}
			}
		}
	}

	if len(cases) != 96 {
		t.Fatalf("expected 96 cases, got %d", len(cases))
	}

	// 1. Initial healthy baseline summary
	summary, err := benchmark.ComputeQualitySummary("session_001", manifest, nil, cases, packs, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute quality summary: %v", err)
	}

	if summary.TotalCases != 96 {
		t.Errorf("expected 96 total cases, got %d", summary.TotalCases)
	}
	if summary.TotalPassCount != 96 {
		t.Errorf("expected 96 passes, got %d", summary.TotalPassCount)
	}
	if summary.TotalFailCount != 0 {
		t.Errorf("expected 0 fails, got %d", summary.TotalFailCount)
	}
	if summary.OverallVerdict != "PASS" {
		t.Errorf("expected PASS overall verdict, got %s", summary.OverallVerdict)
	}

	// 1b. A benchmark session is bound to exactly one hard execution profile (#65).
	localOnly := make(map[string]*benchmark.QualityCaseEvidence, 48)
	for caseID, ev := range cases {
		if ev.Profile == "local" {
			localOnly[caseID] = ev
		}
	}
	localSummary, err := benchmark.ComputeQualitySummary("session_local", manifest, nil, localOnly, packs, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute local-only quality summary: %v", err)
	}
	if localSummary.TotalCases != 48 {
		t.Fatalf("expected 48 local-only cases, got %d", localSummary.TotalCases)
	}
	if localSummary.LocalRollup == nil || localSummary.LocalRollup.TotalCases != 48 {
		t.Fatalf("expected populated 48-case Local rollup, got %+v", localSummary.LocalRollup)
	}
	if localSummary.HybridRollup != nil {
		t.Fatalf("absent Hybrid profile must stay nil in Local session summary, got %+v", localSummary.HybridRollup)
	}
	if localSummary.OverallVerdict != "PASS" {
		t.Fatalf("expected PASS for healthy Local profile summary, got %s", localSummary.OverallVerdict)
	}

	// 2. Category Failure Invariant (Spec #63 / Issue #72):
	// Global averages cannot mask a systematic category failure!
	// Introduce a failure in ONE category ("accent_entity") on Local profile.
	failedCaseID := "local_" + manifest.PrimaryAssets[14].AssetID + "_vi" // AccentEntity asset
	cases[failedCaseID].Stages["dub_synthesize"] = benchmark.StageExecutionEvidence{
		Stage:        "dub_synthesize",
		Status:       "FAILED",
		ErrorMessage: "probed waveform zero-overrun violation",
	}

	summaryFailed, err := benchmark.ComputeQualitySummary("session_001", manifest, nil, cases, packs, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute summary with category failure: %v", err)
	}

	// Profile Rollup must detect failure
	if summaryFailed.LocalRollup.FailCount != 1 {
		t.Errorf("expected LocalRollup.FailCount 1, got %d", summaryFailed.LocalRollup.FailCount)
	}
	if summaryFailed.LocalRollup.GateSatisfied {
		t.Errorf("expected LocalRollup.GateSatisfied to be false after case failure")
	}

	// Category Rollup must explicitly expose the failure
	catRollup, ok := summaryFailed.LocalRollup.CategoryRollups[manifest.PrimaryAssets[14].PrimaryCategory]
	if !ok || catRollup == nil {
		t.Fatalf("missing category rollup for failed category")
	}
	if catRollup.FailCount != 1 {
		t.Errorf("expected category rollup FailCount 1, got %d", catRollup.FailCount)
	}

	// Overall verdict must be strictly FAIL (precedence FAIL > REVIEW_REQUIRED > PASS)
	if summaryFailed.OverallVerdict != "FAIL" {
		t.Errorf("expected overall verdict FAIL, got %s", summaryFailed.OverallVerdict)
	}

	// 3. Local RTF Gate Enforcement (RTF <= 5.0)
	cases[failedCaseID].Stages["dub_synthesize"] = benchmark.StageExecutionEvidence{
		Stage:  "dub_synthesize",
		Status: "COMPLETED",
	}
	// Inject RTF violation (5.5x > 5.0x) on Local case
	cases[failedCaseID].Telemetry = []benchmark.ResourceTelemetrySample{
		{Stage: "translate", RTF: 5.5, DeviceBaselineVRAMBytes: 2000, DevicePeakVRAMBytes: 4000},
	}
	summaryRTFFail, err := benchmark.ComputeQualitySummary("session_001", manifest, nil, cases, packs, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute summary with RTF violation: %v", err)
	}
	if summaryRTFFail.LocalRollup.GateSatisfied {
		t.Errorf("expected LocalRollup to fail when RTF > 5.0")
	}
}

func TestEvaluateCaseQuality_WrongTranscriptYieldsNonZeroCER(t *testing.T) {
	// Regression test for fake-pass bug: hypothesis must NOT be replaced with gold!
	pack := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_test_cer",
		AssetID: "asset_test_cer",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{
				SegmentID:   "seg_01",
				ChineseText: "欢迎大家来到现场",
				StartMs:     0,
				EndMs:       3000,
			},
		},
	}

	// Case where speech_understand COMPLETED but transcript text differs
	caseEv := &benchmark.QualityCaseEvidence{
		CaseID:          "test_case_wrong_transcript",
		SourceVideoID:   "v_cer",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		Stages: map[string]benchmark.StageExecutionEvidence{
			"speech_understand": {Stage: "speech_understand", Status: "COMPLETED"},
		},
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{
					{SourceText: "完全不同的识别结果"}, // completely different
				},
			},
		},
		Telemetry: []benchmark.ResourceTelemetrySample{
			{Stage: "translate", RTF: 1.0, DevicePeakVRAMBytes: 1000},
		},
	}

	metrics := benchmark.EvaluateCaseQuality(caseEv, pack)
	if metrics.ASRCER == 0.0 {
		t.Fatalf("expected non-zero CER for wrong transcript, got 0.0 (fake-pass regression!)")
	}
	if metrics.Status == "PASS" {
		t.Fatalf("expected case to fail due to high CER, got PASS")
	}
}

func TestComputeQualitySummary_MissingReferencePackFailsClosed(t *testing.T) {
	primaries, reserves := buildValidTestCorpus()
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_qc_missing_pack", "1.1", time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC), primaries, reserves)
	if err != nil {
		t.Fatalf("freeze corpus failed: %v", err)
	}

	cases := make(map[string]*benchmark.QualityCaseEvidence)
	// Call with nil packs: must fail closed!
	_, err = benchmark.ComputeQualitySummary("session_missing_pack", manifest, nil, cases, nil, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected fail-closed error for nil reference packs")
	}
	if !strings.Contains(err.Error(), "missing reference annotation packs") {
		t.Errorf("expected error containing 'missing reference annotation packs', got: %v", err)
	}

	// Call with incomplete packs (missing one primary asset)
	packs := make(map[string]*benchmark.ReferenceAnnotationPack)
	for i := 1; i < len(primaries); i++ { // omit primaries[0]
		packs[primaries[i].AssetID] = primaries[i].ReferencePack
	}
	_, err = benchmark.ComputeQualitySummary("session_incomplete_pack", manifest, nil, cases, packs, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected fail-closed error for incomplete reference packs")
	}
	if !strings.Contains(err.Error(), "missing reference annotation pack") {
		t.Errorf("expected error containing 'missing reference annotation pack', got: %v", err)
	}
}

func TestEvaluateCaseQuality_WordTimingBoundaryErrorsMeasuredP95(t *testing.T) {
	pack := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_test_align",
		AssetID: "asset_test_align",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{
				SegmentID:   "seg_01",
				ChineseText: "测试对齐时间",
				StartMs:     0,
				EndMs:       3000,
				Words: []benchmark.ReferenceWordAlignment{
					{Word: "测试", StartMs: 0, EndMs: 1000},
					{Word: "对齐", StartMs: 1000, EndMs: 2000},
					{Word: "时间", StartMs: 2000, EndMs: 3000},
				},
			},
		},
	}

	// 1. Missing alignment must NOT become 120ms default-pass
	caseEvMissing := &benchmark.QualityCaseEvidence{
		CaseID:          "case_missing_align",
		SourceVideoID:   "v_align",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{
					{SourceText: "测试对齐时间"},
				},
				// WordTimings is empty!
			},
		},
	}
	mMissing := benchmark.EvaluateCaseQuality(caseEvMissing, pack)
	if mMissing.ForcedAlignmentP95Ms == 120 {
		t.Fatalf("missing alignment must not become 120ms default pass!")
	}
	if mMissing.ForcedAlignmentP95Ms <= 250 {
		t.Errorf("expected missing alignment P95 > 250ms, got %d", mMissing.ForcedAlignmentP95Ms)
	}

	// 2. Alignment with measured 300ms error must produce P95 >= 300ms, not 120ms
	caseEvError := &benchmark.QualityCaseEvidence{
		CaseID:          "case_err_align",
		SourceVideoID:   "v_align",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{
					{SourceText: "测试对齐时间"},
				},
				WordTimings: []domain.WordTiming{
					{Word: "测试", StartMs: 300, EndMs: 1300, Confidence: 0.9},  // 300ms error
					{Word: "对齐", StartMs: 1300, EndMs: 2300, Confidence: 0.9}, // 300ms error
					{Word: "时间", StartMs: 2300, EndMs: 3300, Confidence: 0.9}, // 300ms error
				},
			},
		},
	}
	mErr := benchmark.EvaluateCaseQuality(caseEvError, pack)
	if mErr.ForcedAlignmentP95Ms < 300 {
		t.Errorf("expected alignment P95 >= 300ms, got %d", mErr.ForcedAlignmentP95Ms)
	}
	if mErr.Status == "PASS" {
		t.Errorf("expected failure when P95 %d exceeds 250ms gate", mErr.ForcedAlignmentP95Ms)
	}
}

func TestEvaluateCaseQuality_DubbingTimingWithin200MsAnd500MsThreshold(t *testing.T) {
	pack := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_test_dub",
		AssetID: "asset_test_dub",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{
				SegmentID:   "seg_01",
				ChineseText: "一段用于配音评测的长对话",
				StartMs:     0,
				EndMs:       10000,
			},
		},
	}

	// Create 10 dub blocks: 9 blocks with 0ms error (within 200ms), 1 block with 250ms error (>200ms)
	// Expected percentage = 9 / 10 = 0.90 (90%), NOT 0.98!
	dubSegs := make([]domain.DubSegment, 10)
	for i := 0; i < 9; i++ {
		st := int64(i * 1000)
		en := int64((i + 1) * 1000)
		dubSegs[i] = domain.DubSegment{
			Index:              i,
			StartMs:            st,
			EndMs:              en,
			SlotDurationMs:     1000,
			MeasuredDurationMs: 1000, // 0ms error
		}
	}
	// 10th block: 250ms overrun
	dubSegs[9] = domain.DubSegment{
		Index:              9,
		StartMs:            9000,
		EndMs:              10000,
		SlotDurationMs:     1000,
		MeasuredDurationMs: 1250, // 250ms error -> abs(finish - EndMs) = 250 > 200
	}

	caseEv := &benchmark.QualityCaseEvidence{
		CaseID:          "case_dub_10_blocks",
		SourceVideoID:   "v_dub",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			DubSegments: &domain.DubSegmentsVariant{
				Segments: dubSegs,
			},
		},
		Telemetry: []benchmark.ResourceTelemetrySample{
			{Stage: "translate", RTF: 1.0, DevicePeakVRAMBytes: 1000},
		},
	}

	metrics := benchmark.EvaluateCaseQuality(caseEv, pack)
	if metrics.DubbingTimingWithin200Ms != 0.90 {
		t.Fatalf("expected exactly 0.90 within 200ms (9/10), got %f (synthetic 0.98 regression!)", metrics.DubbingTimingWithin200Ms)
	}

	// Now test >500ms block error: must project REVIEW_REQUIRED or FAIL
	dubSegs[9].MeasuredDurationMs = 1600 // 600ms error > 500ms
	caseEv500 := &benchmark.QualityCaseEvidence{
		CaseID:          "case_dub_500ms",
		SourceVideoID:   "v_dub",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			DubSegments: &domain.DubSegmentsVariant{
				Segments: dubSegs,
			},
		},
		Telemetry: []benchmark.ResourceTelemetrySample{
			{Stage: "translate", RTF: 1.0, DevicePeakVRAMBytes: 1000},
		},
	}
	m500 := benchmark.EvaluateCaseQuality(caseEv500, pack)
	if m500.Status == "PASS" {
		t.Fatalf("expected >500ms timing error to prevent PASS, got PASS")
	}
}

func TestEvaluateCaseQuality_MissingLocalTelemetryCannotPassResourceGate(t *testing.T) {
	pack := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_test_telem",
		AssetID: "asset_test_telem",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{SegmentID: "seg_01", ChineseText: "标准文本", StartMs: 0, EndMs: 2000},
		},
	}

	// Local case with empty Telemetry: must NOT pass resource gate by default zero!
	caseEv := &benchmark.QualityCaseEvidence{
		CaseID:          "local_case_no_telemetry",
		SourceVideoID:   "v_telem",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "local",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{
					{SourceText: "标准文本"},
				},
				WordTimings: []domain.WordTiming{
					{Word: "标准文本", StartMs: 0, EndMs: 2000},
				},
			},
			DubSegments: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{
					{StartMs: 0, EndMs: 2000, MeasuredDurationMs: 2000},
				},
			},
		},
		Telemetry: nil, // MISSING telemetry!
	}

	metrics := benchmark.EvaluateCaseQuality(caseEv, pack)
	if metrics.Status == "PASS" {
		t.Fatalf("Local case with missing telemetry must NOT PASS by zero!")
	}
	if metrics.Status != "REVIEW_REQUIRED" && metrics.Status != "FAIL" {
		t.Errorf("expected REVIEW_REQUIRED or FAIL, got %s", metrics.Status)
	}
	foundReviewReason := false
	for _, r := range metrics.ReviewReasons {
		if strings.Contains(r, "telemetry") {
			foundReviewReason = true
			break
		}
	}
	if !foundReviewReason {
		t.Errorf("expected review reason mentioning missing telemetry, got %v", metrics.ReviewReasons)
	}
}

func TestWDDMGPUCollector_SamplingAndStageBinding(t *testing.T) {
	collector := benchmark.NewWDDMGPUCollector(50 * time.Millisecond)

	// Test with mocked queryFn
	mockVRAM := uint64(2147483648) // 2 GB
	collector.RecordSample(time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC), mockVRAM)
	collector.RecordSample(time.Date(2026, 9, 4, 10, 0, 0, 100000000, time.UTC), mockVRAM+1073741824) // 3 GB peak
	collector.RecordSample(time.Date(2026, 9, 4, 10, 0, 0, 200000000, time.UTC), mockVRAM)

	stageStart := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	sample, ok := collector.SampleStage("translate", stageStart, 200)
	if !ok {
		t.Fatalf("expected SampleStage to succeed")
	}

	if sample.DeviceBaselineVRAMBytes != mockVRAM {
		t.Errorf("expected baseline %d, got %d", mockVRAM, sample.DeviceBaselineVRAMBytes)
	}
	if sample.DevicePeakVRAMBytes != mockVRAM+1073741824 {
		t.Errorf("expected peak %d, got %d", mockVRAM+1073741824, sample.DevicePeakVRAMBytes)
	}
	if sample.DevicePeakAboveBaselineBytes != 1073741824 {
		t.Errorf("expected peak above baseline 1073741824, got %d", sample.DevicePeakAboveBaselineBytes)
	}
	if sample.Stage != "translate" {
		t.Errorf("expected stage 'translate', got %s", sample.Stage)
	}
}

func TestQualityScoring_DenominatorCorrectAggregation(t *testing.T) {
	// 2 cases with different denominator sizes:
	// Case 1: Short video, ref = 10 chars, edit dist = 2 -> case CER = 2/10 = 0.20
	// Case 2: Long video, ref = 100 chars, edit dist = 2 -> case CER = 2/100 = 0.02
	// Denominator-correct CER = (2 + 2) / (10 + 100) = 4 / 110 = 0.03636...
	// Naive average of per-case CER = (0.20 + 0.02) / 2 = 0.110 (11.0% -> which would erroneously fail the 5% gate!)
	m1 := benchmark.CaseQualityMetrics{
		CaseID:                    "case_1",
		PrimaryCategory:           "clean_single_speaker",
		TargetLanguage:            "vi",
		Profile:                   "hybrid",
		Status:                    "PASS",
		ASRCER:                    0.20,
		RefCharCount:              10,
		EditDistance:              2,
		RefSpeechBlockCount:       2,
		CorruptedSpeechBlockCount: 0,
		TotalDubbingBlocks:        10,
		CompliantDubbingBlocks:    9,
		AlignmentBoundaryErrors:   []int64{50, 60, 70},
	}
	m2 := benchmark.CaseQualityMetrics{
		CaseID:                    "case_2",
		PrimaryCategory:           "clean_single_speaker",
		TargetLanguage:            "vi",
		Profile:                   "hybrid",
		Status:                    "PASS",
		ASRCER:                    0.02,
		RefCharCount:              100,
		EditDistance:              2,
		RefSpeechBlockCount:       20,
		CorruptedSpeechBlockCount: 0,
		TotalDubbingBlocks:        90,
		CompliantDubbingBlocks:    90,
		AlignmentBoundaryErrors:   []int64{30, 40, 50},
	}

	rollup := benchmark.BuildProfileRollupForTest("hybrid", []benchmark.CaseQualityMetrics{m1, m2})
	expectedCER := 4.0 / 110.0
	if math.Abs(rollup.OverallCER-expectedCER) > 1e-6 {
		t.Fatalf("expected denominator-correct CER %f, got %f (naive average error!)", expectedCER, rollup.OverallCER)
	}
	// Dubbing timing: (9 + 90) / (10 + 90) = 99 / 100 = 0.99
	expectedTiming := 99.0 / 100.0
	if math.Abs(rollup.DubbingTimingWithin200Ms-expectedTiming) > 1e-6 {
		t.Fatalf("expected denominator-correct dubbing timing %f, got %f", expectedTiming, rollup.DubbingTimingWithin200Ms)
	}
}

func TestEvaluateCaseQuality_SegmentScopedCriticalItemAndUncertainty(t *testing.T) {
	pack := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_crit",
		AssetID: "asset_crit",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{
				SegmentID:   "seg_01",
				ChineseText: "价格是五十元",
				StartMs:     0,
				EndMs:       2000,
			},
		},
		CriticalItems: []benchmark.ReferenceCriticalItem{
			{
				Type:        "number",
				SourceText:  "五十",
				SegmentID:   "seg_01",
				TargetRefVI: "50",
			},
		},
	}

	// 1. Target has wrong number (40 instead of 50) -> must FAIL critical meaning error
	caseEvFail := &benchmark.QualityCaseEvidence{
		CaseID:          "case_num_err",
		SourceVideoID:   "v_crit",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{{SourceText: "价格是五十元"}},
			},
			Translation: &domain.TranslationVariant{
				Segments: []domain.TranslationSegment{
					{SourceText: "价格是五十元", TargetText: "Gia la 40 dong", PassedQAGate: true},
				},
				OverallQAScore: 0.95,
			},
		},
		Telemetry: []benchmark.ResourceTelemetrySample{
			{Stage: "translate", RTF: 1.0, DevicePeakVRAMBytes: 1000},
		},
	}

	mFail := benchmark.EvaluateCaseQuality(caseEvFail, pack)
	if mFail.CriticalMeaningErrors != 1 {
		t.Errorf("expected 1 critical meaning error for missing number, got %d", mFail.CriticalMeaningErrors)
	}
	if mFail.Status != "FAIL" {
		t.Errorf("expected FAIL for corrupted critical item, got %s", mFail.Status)
	}

	// 2. Displaced critical item in speech understanding:
	// ASR has the critical item, but in a completely different segment -> must project REVIEW_REQUIRED
	pack2 := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_crit_disp",
		AssetID: "asset_crit_disp",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{SegmentID: "seg_01", ChineseText: "第一句包含重要名字张三", StartMs: 0, EndMs: 2000},
			{SegmentID: "seg_02", ChineseText: "第二句普通聊天", StartMs: 3000, EndMs: 5000},
		},
		CriticalItems: []benchmark.ReferenceCriticalItem{
			{Type: "name", SourceText: "张三", SegmentID: "seg_01"},
		},
	}

	caseEvDisp := &benchmark.QualityCaseEvidence{
		CaseID:          "case_disp",
		SourceVideoID:   "v_disp",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{
					{SourceText: "第一句丢失了名字", StartMs: 0, EndMs: 2000},
					{SourceText: "第二句张三移到了这里", StartMs: 3000, EndMs: 5000},
				},
			},
		},
		Telemetry: []benchmark.ResourceTelemetrySample{
			{Stage: "translate", RTF: 1.0, DevicePeakVRAMBytes: 1000},
		},
	}

	mDisp := benchmark.EvaluateCaseQuality(caseEvDisp, pack2)
	if mDisp.Status != "REVIEW_REQUIRED" && mDisp.Status != "FAIL" {
		t.Errorf("expected REVIEW_REQUIRED for displaced critical item, got %s", mDisp.Status)
	}
}

func TestEvaluateCaseQuality_CumulativeDriftNoFabricated150Ms(t *testing.T) {
	pack := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_drift",
		AssetID: "asset_drift",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{SegmentID: "seg_01", ChineseText: "第一句", StartMs: 0, EndMs: 2000},
			{SegmentID: "seg_02", ChineseText: "第二句", StartMs: 2000, EndMs: 4000},
		},
	}

	// Segment 2 ends at 4045ms while source end is 4000ms -> measured drift is 45ms.
	// Segment 1 has anchor_drift review flag.
	// The scorer must record measured drift 45ms (NOT fabricated 150ms) and project REVIEW_REQUIRED.
	caseEv := &benchmark.QualityCaseEvidence{
		CaseID:          "case_drift_check",
		SourceVideoID:   "v_drift",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{
					{SourceText: "第一句", StartMs: 0, EndMs: 2000},
					{SourceText: "第二句", StartMs: 2000, EndMs: 4000},
				},
				WordTimings: []domain.WordTiming{
					{Word: "第一句", StartMs: 0, EndMs: 2000},
					{Word: "第二句", StartMs: 2000, EndMs: 4000},
				},
			},
			DubSegments: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{
					{
						Index:              0,
						StartMs:            0,
						EndMs:              2000,
						MeasuredDurationMs: 2000,
						RequiresReview:     true,
						ReviewReason:       "anchor_drift: slot boundary shift detected",
					},
					{
						Index:              1,
						StartMs:            2000,
						EndMs:              4000,
						MeasuredDurationMs: 2045, // finish = 4045, sourceEnd = 4000 -> drift = 45ms
					},
				},
			},
		},
		Telemetry: []benchmark.ResourceTelemetrySample{
			{Stage: "translate", RTF: 1.0, DevicePeakVRAMBytes: 1000},
		},
	}

	metrics := benchmark.EvaluateCaseQuality(caseEv, pack)
	if metrics.CumulativeDriftMs == 150 {
		t.Fatalf("fabricated 150ms cumulative drift detected!")
	}
	if metrics.CumulativeDriftMs != 45 {
		t.Errorf("expected measured cumulative drift 45ms, got %d ms", metrics.CumulativeDriftMs)
	}
	if metrics.Status != "REVIEW_REQUIRED" {
		t.Errorf("expected REVIEW_REQUIRED due to anchor_drift review flag, got %s", metrics.Status)
	}
}

func TestBuildProfileRollup_EnforcesOverallSeriousErrorRate(t *testing.T) {
	// 48 cases where 1 case has a corrupted critical item out of 50 total blocks across the profile:
	// 1 / 50 = 0.02 (2.0% > 1.0% MaxSeriousErrorRate)
	metrics := make([]benchmark.CaseQualityMetrics, 48)
	for i := 0; i < 48; i++ {
		metrics[i] = benchmark.CaseQualityMetrics{
			CaseID:                 fmt.Sprintf("case_%d", i),
			PrimaryCategory:        "clean_single_speaker",
			TargetLanguage:         "vi",
			Profile:                "local",
			Status:                 "PASS",
			RefCharCount:           100,
			EditDistance:           2,
			RefSpeechBlockCount:    1,
			TotalDubbingBlocks:     10,
			CompliantDubbingBlocks: 10,
		}
	}
	// Inject 1 corrupted block into case 0 (total blocks = 48, corrupted = 1 -> rate = 1/48 = 0.0208 > 0.01)
	metrics[0].CorruptedSpeechBlockCount = 1

	rollup := benchmark.BuildProfileRollupForTest("local", metrics)
	if rollup.GateSatisfied {
		t.Fatalf("expected GateSatisfied to be false when OverallSeriousErrorRate (%f) > 0.01", rollup.OverallSeriousErrorRate)
	}
	foundReason := false
	for _, r := range rollup.FailReasons {
		if strings.Contains(r, "serious ASR error rate") {
			foundReason = true
			break
		}
	}
	if !foundReason {
		t.Errorf("expected fail reason mentioning serious ASR error rate, got %v", rollup.FailReasons)
	}
}

func TestBuildProfileRollup_EnforcesHardProfileDenominator48(t *testing.T) {
	// 1. 47 cases (all PASS): must fail the hard denominator 48 gate
	cases47 := make([]benchmark.CaseQualityMetrics, 47)
	for i := 0; i < 47; i++ {
		cases47[i] = benchmark.CaseQualityMetrics{
			CaseID:                 fmt.Sprintf("case_%d", i),
			PrimaryCategory:        "clean_single_speaker",
			TargetLanguage:         "vi",
			Profile:                "local",
			Status:                 "PASS",
			RefCharCount:           100,
			EditDistance:           2,
			RefSpeechBlockCount:    2,
			TotalDubbingBlocks:     10,
			CompliantDubbingBlocks: 10,
		}
	}
	r47 := benchmark.BuildProfileRollupForTest("local", cases47)
	if r47.GateSatisfied {
		t.Fatalf("expected 47-case profile to fail hard denominator 48 gate")
	}

	// 2. 48 cases with 43 PASS and 5 REVIEW_REQUIRED: must fail gate (requires >= 44 PASS, <= 4 REVIEW)
	cases43Pass := make([]benchmark.CaseQualityMetrics, 48)
	for i := 0; i < 48; i++ {
		status := "PASS"
		if i >= 43 {
			status = "REVIEW_REQUIRED"
		}
		cases43Pass[i] = benchmark.CaseQualityMetrics{
			CaseID:                 fmt.Sprintf("case_%d", i),
			PrimaryCategory:        "clean_single_speaker",
			TargetLanguage:         "vi",
			Profile:                "local",
			Status:                 status,
			RefCharCount:           100,
			EditDistance:           2,
			RefSpeechBlockCount:    2,
			TotalDubbingBlocks:     10,
			CompliantDubbingBlocks: 10,
		}
	}
	r43 := benchmark.BuildProfileRollupForTest("local", cases43Pass)
	if r43.GateSatisfied {
		t.Fatalf("expected 43-pass / 5-review profile to fail gate")
	}

	// 3. 48 cases with exactly 44 PASS and 4 REVIEW_REQUIRED (0 FAIL): must PASS gate
	cases44Pass := make([]benchmark.CaseQualityMetrics, 48)
	for i := 0; i < 48; i++ {
		status := "PASS"
		if i >= 44 {
			status = "REVIEW_REQUIRED"
		}
		cases44Pass[i] = benchmark.CaseQualityMetrics{
			CaseID:                 fmt.Sprintf("case_%d", i),
			PrimaryCategory:        "clean_single_speaker",
			TargetLanguage:         "vi",
			Profile:                "local",
			Status:                 status,
			RefCharCount:           100,
			EditDistance:           2,
			RefSpeechBlockCount:    2,
			TotalDubbingBlocks:     10,
			CompliantDubbingBlocks: 10,
		}
	}
	r44 := benchmark.BuildProfileRollupForTest("local", cases44Pass)
	if !r44.GateSatisfied {
		t.Fatalf("expected 44-pass / 4-review profile to satisfy gate, fail reasons: %v", r44.FailReasons)
	}
}

func TestEvaluateCaseQuality_SegmentScopedTranslation_NoCrossSegmentMasking(t *testing.T) {
	pack := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_scope_test",
		AssetID: "asset_scope_test",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{
				SegmentID:   "seg_00",
				ChineseText: "价格是五十元",
				StartMs:     0,
				EndMs:       2000,
			},
			{
				SegmentID:   "seg_01",
				ChineseText: "这是其他内容",
				StartMs:     2000,
				EndMs:       4000,
			},
		},
		CriticalItems: []benchmark.ReferenceCriticalItem{
			{
				Type:        "number",
				SourceText:  "五十",
				SegmentID:   "seg_00",
				TargetRefVI: "50",
			},
		},
	}

	// Segment 0 translation has NO number ("Giá rất rẻ").
	// Segment 1 translation has the number 50 ("Nội dung khác có 50").
	// In global concatenation, "50" in Segment 1 would mask the missing number in Segment 0.
	// With segment-scoped evaluation, Segment 0 MUST fail with critical meaning error!
	caseEv := &benchmark.QualityCaseEvidence{
		CaseID:          "case_scope_cross_masking",
		SourceVideoID:   "v_scope",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{
					{SourceText: "价格是五十元", StartMs: 0, EndMs: 2000},
					{SourceText: "这是其他内容", StartMs: 2000, EndMs: 4000},
				},
			},
			Translation: &domain.TranslationVariant{
				Segments: []domain.TranslationSegment{
					{
						Index:        0,
						SourceText:   "价格是五十元",
						TargetText:   "Giá rất rẻ", // missing 50!
						PassedQAGate: true,
					},
					{
						Index:        1,
						SourceText:   "这是其他内容",
						TargetText:   "Nội dung khác có 50", // 50 leaked into segment 1!
						PassedQAGate: true,
					},
				},
				OverallQAScore: 0.95,
			},
		},
		Telemetry: []benchmark.ResourceTelemetrySample{
			{Stage: "translate", RTF: 1.0, DevicePeakVRAMBytes: 1000},
		},
	}

	m := benchmark.EvaluateCaseQuality(caseEv, pack)
	if m.CriticalMeaningErrors != 1 {
		t.Fatalf("expected 1 critical error because segment 0 lacks '50' (cross-segment masking failed!), got %d", m.CriticalMeaningErrors)
	}
	if m.Status != "FAIL" {
		t.Errorf("expected FAIL for segment-scoped number omission, got %s", m.Status)
	}

	// Also test ambiguous segment correspondence: unknown SegmentID
	packAmbiguous := &benchmark.ReferenceAnnotationPack{
		PackID:  "pack_ambig",
		AssetID: "asset_ambig",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{SegmentID: "seg_known", ChineseText: "已知段落", StartMs: 0, EndMs: 2000},
		},
		CriticalItems: []benchmark.ReferenceCriticalItem{
			{
				Type:        "entity",
				SourceText:  "未知实体",
				SegmentID:   "seg_unknown_id",
				TargetRefVI: "thực thể",
			},
		},
	}
	caseEvAmbig := &benchmark.QualityCaseEvidence{
		CaseID:          "case_ambig",
		SourceVideoID:   "v_ambig",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Translation: &domain.TranslationVariant{
				Segments: []domain.TranslationSegment{
					{Index: 0, SourceText: "已知段落", TargetText: "Doan van biet", PassedQAGate: true},
				},
				OverallQAScore: 0.95,
			},
		},
		Telemetry: []benchmark.ResourceTelemetrySample{
			{Stage: "translate", RTF: 1.0, DevicePeakVRAMBytes: 1000},
		},
	}
	mAmbig := benchmark.EvaluateCaseQuality(caseEvAmbig, packAmbiguous)
	if mAmbig.Status != "REVIEW_REQUIRED" && mAmbig.Status != "FAIL" {
		t.Errorf("expected REVIEW_REQUIRED for ambiguous segment correspondence, got %s", mAmbig.Status)
	}
}

func TestDeriveAutomatedQualityResult_HonestSignalsAndFailClosed(t *testing.T) {
	// 1. Missing artifacts must fail closed (FAIL status, not pass-by-default)
	qcEmpty := &benchmark.QualityCaseEvidence{
		CaseID:         "empty_case",
		RunID:          "run_empty",
		JobID:          "job_empty",
		SourceAssetID:  "asset_empty",
		TargetLanguage: "vi",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{},
	}
	qrEmpty := benchmark.DeriveAutomatedQualityResultForTest(qcEmpty, nil, false)
	if qrEmpty.OverallStatus != domain.QualityStatusFail {
		t.Errorf("expected FAIL for empty stage artifacts, got %s", qrEmpty.OverallStatus)
	}
	if len(qrEmpty.Issues) == 0 {
		t.Errorf("expected review issues logged for missing artifacts")
	}

	// 2. No-dub case: voice and dialogue suppression are not applicable (PASS)
	qcNoDub := &benchmark.QualityCaseEvidence{
		CaseID:         "nodub_case",
		RunID:          "run_nodub",
		JobID:          "job_nodub",
		SourceAssetID:  "asset_nodub",
		TargetLanguage: "vi",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			DubMix: &domain.DubMixArtifact{
				SoundtrackPreserved: true,
				OverallStatus:       "PASS",
			},
			FinalRender: &domain.FinalRenderArtifact{
				OverallStatus:    "PASS",
				OutputCASHash:    "sha256_render_nodub",
				OutputByteSize:   2048,
				OutputDurationMs: 5000,
			},
		},
	}
	qrNoDub := benchmark.DeriveAutomatedQualityResultForTest(qcNoDub, nil, true)
	if qrNoDub.OverallStatus != domain.QualityStatusPass {
		t.Errorf("expected PASS for valid no-dub case, got %s (issues: %+v)", qrNoDub.OverallStatus, qrNoDub.Issues)
	}
	// 2b. No-dub visual-only case without DubMix artifact MUST PASS without requiring DubMix
	qcNoDubVisualOnly := &benchmark.QualityCaseEvidence{
		CaseID:         "nodub_visual_only_case",
		RunID:          "run_nodub_vo",
		JobID:          "job_nodub_vo",
		SourceAssetID:  "asset_nodub_vo",
		TargetLanguage: "vi",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			DubMix: nil, // Visual-only asset has no dub mix
			FinalRender: &domain.FinalRenderArtifact{
				OverallStatus:    "PASS",
				OutputCASHash:    "sha256_render_nodub_vo",
				OutputByteSize:   4096,
				OutputDurationMs: 8000,
			},
		},
	}
	qrNoDubVO := benchmark.DeriveAutomatedQualityResultForTest(qcNoDubVisualOnly, nil, true)
	if qrNoDubVO.OverallStatus != domain.QualityStatusPass {
		t.Fatalf("expected PASS for no-dub visual-only asset without DubMix, got %s (issues: %+v)", qrNoDubVO.OverallStatus, qrNoDubVO.Issues)
	}
	if len(qrNoDubVO.Issues) != 0 {
		t.Fatalf("expected 0 issues for no-dub visual-only asset, got %+v", qrNoDubVO.Issues)
	}

	// Verify metrics for no-dub visual-only
	foundSoundtrack := false
	foundDialogue := false
	for _, m := range qrNoDubVO.Metrics {
		if m.Name == "soundtrack_preservation" {
			foundSoundtrack = true
			if !m.Passed || m.Score != 1.0 {
				t.Errorf("expected soundtrack_preservation to pass with 1.0, got passed=%v score=%f", m.Passed, m.Score)
			}
		}
		if m.Name == "dialogue_suppression" {
			foundDialogue = true
			if !m.Passed || m.Score != 1.0 {
				t.Errorf("expected dialogue_suppression to pass with 1.0, got passed=%v score=%f", m.Passed, m.Score)
			}
		}
	}
	if !foundSoundtrack || !foundDialogue {
		t.Fatalf("missing required metrics in no-dub evaluation: soundtrack=%v, dialogue=%v", foundSoundtrack, foundDialogue)
	}

	// Dub-eligible case without DubMix artifact MUST fail closed with FAIL
	qrDubEligibleMissingMix := benchmark.DeriveAutomatedQualityResultForTest(qcNoDubVisualOnly, nil, false)
	if qrDubEligibleMissingMix.OverallStatus != domain.QualityStatusFail {
		t.Fatalf("expected FAIL for dub-eligible asset without DubMix, got %s", qrDubEligibleMissingMix.OverallStatus)
	}
	if len(qrDubEligibleMissingMix.Issues) == 0 || qrDubEligibleMissingMix.Issues[0].Reason != "missing dub mix artifact" {
		t.Fatalf("expected blocker issue 'missing dub mix artifact', got %+v", qrDubEligibleMissingMix.Issues)
	}

	// 3. Multi-speaker voice distinguishability: REVIEW_REQUIRED if QC status != PASS
	qcVoiceReview := &benchmark.QualityCaseEvidence{
		CaseID:         "voice_review_case",
		RunID:          "run_voice",
		JobID:          "job_voice",
		SourceAssetID:  "asset_voice",
		TargetLanguage: "vi",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			VoiceAssignment: &domain.VoiceAssignment{
				Distinguishability: &domain.VoiceDistinguishabilityQC{
					MultiSpeaker:     true,
					Status:           "REVIEW_REQUIRED",
					Issues:           []string{"duplicate voices assigned to distinct speakers"},
					DistinctVoiceIDs: 1,
					SpeakerCount:     2,
				},
			},
			DubMix: &domain.DubMixArtifact{
				SoundtrackPreserved: true,
				DialogueSuppressed:  true,
				OverallStatus:       "PASS",
			},
			FinalRender: &domain.FinalRenderArtifact{
				OverallStatus:    "PASS",
				OutputCASHash:    "sha256_render_voice",
				OutputByteSize:   2048,
				OutputDurationMs: 5000,
			},
		},
	}
	qrVoice := benchmark.DeriveAutomatedQualityResultForTest(qcVoiceReview, nil, false)
	if qrVoice.OverallStatus != domain.QualityStatusReviewRequired {
		t.Errorf("expected REVIEW_REQUIRED for distinguishability issue, got %s", qrVoice.OverallStatus)
	}

	// 4. Burned-in subtitle reference regions without temporal cue coverage: must be REVIEW_REQUIRED
	packBurnedIn := &benchmark.ReferenceAnnotationPack{
		PackID:               "pack_burned",
		AssetID:              "asset_burned",
		HasBurnedInSubtitles: true,
		SubtitleRegions: []benchmark.ReferenceSubtitleRegion{
			{
				RegionID:    "sub_1",
				StartMs:     1000,
				EndMs:       3000,
				ChineseText: "屏幕字幕",
			},
		},
	}
	qcBurnedUncovered := &benchmark.QualityCaseEvidence{
		CaseID:         "burned_case",
		RunID:          "run_burned",
		JobID:          "job_burned",
		SourceAssetID:  "asset_burned",
		TargetLanguage: "vi",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			VoiceAssignment: &domain.VoiceAssignment{
				Distinguishability: &domain.VoiceDistinguishabilityQC{
					Status: "PASS",
				},
			},
			DubMix: &domain.DubMixArtifact{
				SoundtrackPreserved: true,
				DialogueSuppressed:  true,
				OverallStatus:       "PASS",
			},
			FinalRender: &domain.FinalRenderArtifact{
				OverallStatus:    "PASS",
				OutputCASHash:    "sha256_render_burned",
				OutputByteSize:   2048,
				OutputDurationMs: 5000,
			},
			LocalizedSubtitleTrack: &domain.LocalizedSubtitleTrack{
				Cues: []domain.SubtitleCue{
					// Cue timing does not overlap 1000-3000ms
					{StartMs: 5000, EndMs: 7000, Text: "Phụ đề trễ"},
				},
			},
		},
	}
	qrBurned := benchmark.DeriveAutomatedQualityResultForTest(qcBurnedUncovered, packBurnedIn, false)
	if qrBurned.OverallStatus != domain.QualityStatusReviewRequired {
		t.Errorf("expected REVIEW_REQUIRED for uncovered burned-in subtitle regions, got %s", qrBurned.OverallStatus)
	}

	// 5. Temporal-only cue coverage cannot prove source-text elimination and must NOT PASS (Defect 1)
	qcBurnedTemporalOnly := &benchmark.QualityCaseEvidence{
		CaseID:         "burned_temporal_case",
		RunID:          "run_burned_temp",
		JobID:          "job_burned_temp",
		SourceAssetID:  "asset_burned",
		TargetLanguage: "vi",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			VoiceAssignment: &domain.VoiceAssignment{
				Distinguishability: &domain.VoiceDistinguishabilityQC{
					Status: "PASS",
				},
			},
			DubMix: &domain.DubMixArtifact{
				SoundtrackPreserved: true,
				DialogueSuppressed:  true,
				OverallStatus:       "PASS",
			},
			FinalRender: &domain.FinalRenderArtifact{
				OverallStatus:    "PASS",
				OutputCASHash:    "sha256_render_temp",
				OutputByteSize:   2048,
				OutputDurationMs: 5000,
			},
			LocalizedSubtitleTrack: &domain.LocalizedSubtitleTrack{
				Cues: []domain.SubtitleCue{
					{StartMs: 1000, EndMs: 3000, Text: "Phụ đề chỉ khớp thời gian"},
				},
			},
		},
	}
	qrBurnedTemp := benchmark.DeriveAutomatedQualityResultForTest(qcBurnedTemporalOnly, packBurnedIn, false)
	if qrBurnedTemp.OverallStatus != domain.QualityStatusReviewRequired {
		t.Errorf("expected REVIEW_REQUIRED for temporal-only subtitle cue coverage, got %s", qrBurnedTemp.OverallStatus)
	}
	for _, m := range qrBurnedTemp.Metrics {
		if m.Name == "burned_in_subtitle_replacement" && m.Passed {
			t.Errorf("burned_in_subtitle_replacement must not PASS on temporal-only evidence")
		}
	}

	// 6. A temporally overlapping unrelated overlay cannot PASS burned_in_subtitle_replacement.
	// ReferenceSubtitleRegion is specifically a burned-in dialogue subtitle region.
	// Retained LocalizedOverlayItem artifacts only represent semantic_text/instructional_ui_text,
	// and cannot prove the dialogue subtitle was eliminated, so evaluation must remain REVIEW_REQUIRED.
	qcBurnedOverlay := &benchmark.QualityCaseEvidence{
		CaseID:         "burned_overlay_case",
		RunID:          "run_burned_ov",
		JobID:          "job_burned_ov",
		SourceAssetID:  "asset_burned",
		TargetLanguage: "vi",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			VoiceAssignment: &domain.VoiceAssignment{
				Distinguishability: &domain.VoiceDistinguishabilityQC{
					Status: "PASS",
				},
			},
			DubMix: &domain.DubMixArtifact{
				SoundtrackPreserved: true,
				DialogueSuppressed:  true,
				OverallStatus:       "PASS",
			},
			FinalRender: &domain.FinalRenderArtifact{
				OverallStatus:    "PASS",
				OutputCASHash:    "sha256_render_ov",
				OutputByteSize:   2048,
				OutputDurationMs: 5000,
			},
			LocalizedVisualTrack: &domain.LocalizedVisualTrack{
				Overlays: []domain.LocalizedOverlayItem{
					{
						StartMs:        1000,
						EndMs:          3000,
						IsCoverDefault: true,
						Box:            domain.BoundingBox{X: 100, Y: 800, Width: 800, Height: 100},
						LocalizedText:  "Giao diện/tiêu đề tiếng Việt",
					},
				},
			},
		},
	}
	qrOverlay := benchmark.DeriveAutomatedQualityResultForTest(qcBurnedOverlay, packBurnedIn, false)
	if qrOverlay.OverallStatus != domain.QualityStatusReviewRequired {
		t.Errorf("expected REVIEW_REQUIRED when unrelated overlay overlaps dialogue subtitle, got %s", qrOverlay.OverallStatus)
	}
	for _, m := range qrOverlay.Metrics {
		if m.Name == "burned_in_subtitle_replacement" && m.Passed {
			t.Errorf("burned_in_subtitle_replacement must not PASS on unrelated overlay evidence")
		}
	}
}

func TestQualityScoring_LegitimateBorrowedSilencePlayback_Passes(t *testing.T) {
	pack := &benchmark.ReferenceAnnotationPack{
		AssetID: "asset-borrowed-silence",
		ChineseTranscript: []benchmark.ReferenceTranscriptSegment{
			{SegmentID: "s1", StartMs: 1000, EndMs: 2000, ChineseText: "测试"},
		},
	}
	// Segment has source EndMs: 2000, but accepted DubPlaybackEndMs: 2500 (borrowed silence).
	// TTS finish: 1000 + 1450 = 2450ms.
	// Compared to source EndMs (2000), diff would be 450ms (> 200ms -> fail).
	// Compared to accepted DubPlaybackEndMs (2500), diff is 50ms (<= 200ms -> PASS).
	caseEv := &benchmark.QualityCaseEvidence{
		CaseID:          "case-borrowed-silence",
		SourceVideoID:   "video-1",
		PrimaryCategory: string(benchmark.CategoryCleanSingleSpeaker),
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		StageArtifacts: &benchmark.CaseMeasuredStageArtifacts{
			Transcript: &domain.TranscriptArtifact{
				SpeechBlocks: []domain.SpeechBlock{
					{Index: 0, StartMs: 1000, EndMs: 2000, SourceText: "测试", SegmentType: domain.SpeechBlockTypeSpeech},
				},
			},
			DubSegments: &domain.DubSegmentsVariant{
				Segments: []domain.DubSegment{
					{
						Index:              0,
						StartMs:            1000,
						EndMs:              2000,
						DubPlaybackEndMs:   2500,
						MeasuredDurationMs: 1450,
						FitDecision:        domain.FitActionAccept,
					},
				},
			},
		},
	}

	metrics := benchmark.EvaluateCaseQuality(caseEv, pack)
	if metrics.DubbingTimingWithin200Ms != 1.0 {
		t.Fatalf("expected DubbingTimingWithin200Ms 1.0, got %f (status: %s, fails: %v)", metrics.DubbingTimingWithin200Ms, metrics.Status, metrics.FailReasons)
	}
	if metrics.CumulativeDriftMs != 50 {
		t.Fatalf("expected CumulativeDriftMs 50, got %d", metrics.CumulativeDriftMs)
	}
	if metrics.Status != "PASS" {
		t.Fatalf("expected PASS status, got %s: %v", metrics.Status, metrics.FailReasons)
	}
}
