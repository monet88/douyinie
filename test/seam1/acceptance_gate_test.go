package seam1_test

// Issue #43 — Phase 1 acceptance/regression gate.
//
// This gate models the 5 canonical E2E acceptance fixtures
// (.ref/_live-tests/e2e-acceptance-20260823/) as minimized synthetic equivalents over the
// deterministic Seam 1 fake providers, then asserts ONLY the normative cross-video
// invariants locked by #18 §5-§6 + the final revalidation amendment:
//
//  1. measured media truth                      — candidates are fit-gated on PROBED waveform
//     duration, never on predicted/WPM planning evidence.
//  2. tts_finish <= immutable_source_window_end — zero overrun inside the immutable slot.
//  3. no adjacent collision / run-on            — dub spans never collide on the timeline.
//  4. perceptible turn gaps                     — the inter-turn breathing room equals the
//     source-derived relation next_turn_start − dub_finish (clamped ≥ 0), never a hard-coded
//     per-fixture constant.
//  5. source-relative pacing/cadence AV-QC                — the source rate is derived from the
//     immutable source text + slot, the cadence ratio is the target÷source quotient, the word
//     budget is the production source-relative derivation, and the pacing gate PASSES (no
//     CADENCE_* exception on the artifact or in the public review projection).
//  6. soundtrack preservation                   — dialogue-only suppression on dubbed branches;
//     bitstream passthrough where the plan allows (no-dub fixture); music-only outro windows
//     never carry a suppress_dialogue action (Video 4).
//  7. TextRegionPlan roles + compact overlay + non-occlusion, per-fixture: each fixture carries
//     its own synthetic OCR detections, frame geometry, expected role set, overlay count,
//     protected-region count, and subtitle-cue count matching its documented visual strategy.
//
// Fixture-specific measured numbers (per-video duration, QA confidence) are documented as
// EVIDENCE in `fixtureEvidence` and reconciled with
// docs/research/phase1-e2e-acceptance-2026-08-23.md. They are NEVER encoded as assertions or as
// global thresholds (#18 final review explicitly rejected converting Video-4 observed
// fit/gap numbers into universal constants).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"unicode"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
)

// fixtureEvidence records the canonical per-video measurements from the live acceptance run,
// reconciled with docs/research/phase1-e2e-acceptance-2026-08-23.md (Video 4 PASS 0.72 /
// 33.14s). EVIDENCE ONLY — never asserted.
var fixtureEvidence = map[string]string{
	"video1": "n=0 speech; visual-only localization; 1080x1920 23.99s; original AAC bitstream passthrough; top disclaimer/step badges/title hashtag covered in-place; PASS 0.78",
	"video2": "2-speaker lifestyle narration dub + vocal separation; 1080x1920 42.80s; 7 floating in-place callouts; PASS 0.72",
	"video3": "~15 micro dub slots; 1080x1920 17.07s; top step badges + packaging caution badge + final sticker; cartoon SFX/BGM intact; PASS 0.78",
	"video4": "fast tutorial narration; 1080x1920 33.14s; V3 compact subtitle box; CapCut UI controls localized ('Xuất'); music-only outro 27.2s-33.11s preserved; PASS 0.72",
	"video5": "fast-cut lifestyle narration dub; 1080x1440 3:4 27.17s; bottom subtitle overlay; SUPOR/Samyang brand KEEP; Foley SFX (water plop 24-26s, frying, milk pour) preserved; PASS 0.72",
}

// fixtureGate describes the normative structure of one canonical fixture as a minimized
// synthetic equivalent. Timing relations are asserted as source-derived relations; every
// documented measured number lives in fixtureEvidence.
type fixtureGate struct {
	id string
	// name is the canonical fixture label.
	name string
	// segments are the immutable source speech windows. Empty means "no dub-eligible speech"
	// (no-dub branch). StartMs/EndMs are immutable source anchors.
	segments []domain.TranslationInputSegment
	// rolePlan classifies the source audio track into narration → suppress; non-speech → preserve.
	rolePlan []domain.AudioSegment
	// ocr is the fixture's own synthetic OCR detection set matching its documented visual
	// strategy (floating callouts, step badges, CapCut UI, brand KEEP, etc.).
	ocr []provider.RawTextDetection
	// frameW/frameH is the fixture's frame geometry (Video 5 is 3:4 1080x1440).
	frameW, frameH int
	// ttsDurations force the fake TTS provider's measured waveform duration per segment index.
	ttsDurations map[int]int64
	// ttsDuration is the fallback measured duration for any segment not in ttsDurations.
	ttsDuration int64
	// predictedMs forces a deliberately WRONG predicted duration (planning evidence) so the
	// gate proves selection is driven by measured truth, not predicted planning evidence.
	predictedMs int64
	// expectDub selects the dubbed branch (voice assignment + TTS + fit gate).
	expectDub bool
	// expectPassthrough selects the no-dub bitstream-passthrough branch.
	expectPassthrough bool
	// wantRoles are the TextRegionPlan roles this fixture's visual strategy MUST produce.
	wantRoles []domain.TextRegionRole
	// absentRoles are roles that MUST NOT appear (classification must not over-assign).
	absentRoles []domain.TextRegionRole
	// wantOverlays is the exact number of in-place overlays (semantic + instructional UI).
	wantOverlays int
	// wantProtected is the exact number of protected regions (brand/UI → non-occlusion).
	wantProtected int
	// wantCues is the exact number of localized subtitle cues (0 on the no-dub fixture).
	wantCues int
	// wantSpeakers is the number of distinct speakers to verify as distinct assigned voices
	// (0 skips the check on single-speaker fixtures).
	wantSpeakers int
	// outroNoDub marks a music-only outro window that must never carry a suppress_dialogue
	// action (Video 4: 27.2s-33.11s scaled to the synthetic timeline).
	outroNoDubStartMs, outroNoDubEndMs int64
	// evidence cross-references fixtureEvidence.
	evidence string
}

// det is a compact constructor for a single-frame OCR detection (high confidence so the
// deterministic classifier never flags review for confidence reasons).
func det(text string, x, y, w, h int) provider.RawTextDetection {
	return provider.RawTextDetection{
		FrameIndex:  0,
		TimestampMs: 0,
		Text:        text,
		Confidence:  0.95,
		Box:         domain.BoundingBox{X: x, Y: y, Width: w, Height: h},
	}
}

var acceptanceGateFixtures = []fixtureGate{
	{
		id:   "video1_no_dub_visual",
		name: "Video 1 — no-dub visual localization + soundtrack preservation",
		// No dub-eligible speech: instrumental BGM + ambience/SFX only.
		segments: []domain.TranslationInputSegment{},
		rolePlan: []domain.AudioSegment{
			{StartMs: 0, EndMs: 9500, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 1500, EndMs: 8000, Role: domain.AudioRoleAmbienceSFX},
		},
		// Doc: top disclaimer, step badges, title hashtag — all semantic; zero spoken lines
		// so NO speech_subtitle regions exist.
		ocr: []provider.RawTextDetection{
			det("抹茶蜜瓜冰食谱", 180, 120, 500, 60),   // title hashtag
			det("本食谱仅供娱乐参考", 180, 300, 460, 50), // top disclaimer
			det("步骤1：准备抹茶粉", 120, 700, 380, 50), // step badge
		},
		frameW:            1080,
		frameH:            1920,
		expectDub:         false,
		expectPassthrough: true,
		wantRoles:         []domain.TextRegionRole{domain.TextRoleSemanticText},
		absentRoles: []domain.TextRegionRole{
			domain.TextRoleSpeechSubtitle,
			domain.TextRoleInstructionalUIText,
			domain.TextRoleBrandKeep,
			domain.TextRoleIgnoreNoise,
		},
		wantOverlays:  3,
		wantProtected: 0,
		wantCues:      0,
		evidence:      fixtureEvidence["video1"],
	},
	{
		id:   "video2_narration_dub_floating_text",
		name: "Video 2 — 2-speaker narration dub + floating text localization",
		segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2500},
			{Index: 1, SourceText: "我们去公园散步吧。", SpeakerID: "SPEAKER_01", StartMs: 3000, EndMs: 5500},
			{Index: 2, SourceText: "明天再继续工作。", SpeakerID: "SPEAKER_00", StartMs: 6000, EndMs: 8500},
		},
		rolePlan: []domain.AudioSegment{
			{StartMs: 0, EndMs: 9500, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 0, EndMs: 2500, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 3000, EndMs: 5500, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 6000, EndMs: 8500, Role: domain.AudioRoleNarrationDialogue},
		},
		// Doc: floating in-place callouts (7 live, minimized to 2) — semantic text only,
		// no bottom subtitles, no UI, no brand.
		ocr: []provider.RawTextDetection{
			det("浇水正确方法", 380, 400, 320, 50), // floating callout 1
			det("剪掉枯萎叶片", 420, 900, 340, 50), // floating callout 2
		},
		frameW:       1080,
		frameH:       1920,
		ttsDurations: map[int]int64{0: 1500, 1: 1800, 2: 1500},
		ttsDuration:  1500,
		predictedMs:  9000, // wrong planning evidence; selection must use measured truth.
		expectDub:    true,
		wantRoles:    []domain.TextRegionRole{domain.TextRoleSemanticText},
		absentRoles: []domain.TextRegionRole{
			domain.TextRoleSpeechSubtitle,
			domain.TextRoleInstructionalUIText,
			domain.TextRoleBrandKeep,
			domain.TextRoleIgnoreNoise,
		},
		wantOverlays:  2,
		wantProtected: 0,
		wantCues:      3, // one cue per dub-script segment
		wantSpeakers:  2, // alternating speakers must get distinct voices
		evidence:      fixtureEvidence["video2"],
	},
	{
		id:   "video3_micro_timed_dub_dense_text",
		name: "Video 3 — micro-timed narration dub + dense short visual text",
		segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1200},
			{Index: 1, SourceText: "我们去公园散步吧。", SpeakerID: "SPEAKER_00", StartMs: 1450, EndMs: 2600},
			{Index: 2, SourceText: "明天再继续工作。", SpeakerID: "SPEAKER_00", StartMs: 2850, EndMs: 4000},
			{Index: 3, SourceText: "请将温度调至25度，张伟说不要打开窗户。", SpeakerID: "SPEAKER_00", StartMs: 4250, EndMs: 5600},
			{Index: 4, SourceText: "SUPOR电饭煲拥有3升容量，煮饭不粘锅。", SpeakerID: "SPEAKER_00", StartMs: 5850, EndMs: 7200},
			{Index: 5, SourceText: "步骤1：准备抹茶粉20克，不要加糖。", SpeakerID: "SPEAKER_00", StartMs: 7450, EndMs: 8800},
		},
		rolePlan: []domain.AudioSegment{
			{StartMs: 0, EndMs: 9500, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 0, EndMs: 1200, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 1450, EndMs: 2600, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 2850, EndMs: 4000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 4250, EndMs: 5600, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 5850, EndMs: 7200, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 7450, EndMs: 8800, Role: domain.AudioRoleNarrationDialogue},
		},
		// Doc: top step badges + packaging caution badge + final sticker (semantic) AND
		// bottom dialogue — dense short text; no UI, no brand KEEP (SUPOR appears only in
		// spoken narration here, not as on-screen brand text).
		ocr: []provider.RawTextDetection{
			det("欢迎来到频道", 180, 1550, 720, 65),   // bottom speech subtitle
			det("步骤1：准备原料", 120, 300, 380, 50),  // top step badge
			det("包装警示：小心跌落", 120, 500, 400, 45), // packaging caution badge
			det("小星星装饰贴画", 700, 600, 260, 45),   // final sticker
		},
		frameW:       1080,
		frameH:       1920,
		ttsDurations: map[int]int64{0: 800, 1: 900, 2: 900, 3: 1000, 4: 1000, 5: 1000},
		ttsDuration:  900,
		expectDub:    true,
		wantRoles:    []domain.TextRegionRole{domain.TextRoleSpeechSubtitle, domain.TextRoleSemanticText},
		absentRoles: []domain.TextRegionRole{
			domain.TextRoleInstructionalUIText,
			domain.TextRoleBrandKeep,
			domain.TextRoleIgnoreNoise,
		},
		wantOverlays:  3, // badges/sticker only; bottom subtitle covered by cues, not overlays
		wantProtected: 0,
		wantCues:      6,
		evidence:      fixtureEvidence["video3"],
	},
	{
		id:   "video4_fast_tutorial_instructional_ui_outro",
		name: "Video 4 — fast tutorial narration + CapCut instructional UI + music-only outro",
		segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 1500},
			{Index: 1, SourceText: "我们去公园散步吧。", SpeakerID: "SPEAKER_00", StartMs: 1800, EndMs: 3300},
			{Index: 2, SourceText: "明天再继续工作。", SpeakerID: "SPEAKER_00", StartMs: 3700, EndMs: 5200},
		},
		rolePlan: []domain.AudioSegment{
			{StartMs: 0, EndMs: 6000, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 6000, EndMs: 8500, Role: domain.AudioRoleInstrumentalBgm}, // music-only outro
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 1800, EndMs: 3300, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 3700, EndMs: 5200, Role: domain.AudioRoleNarrationDialogue},
		},
		// Doc: V3 compact subtitle + CapCut UI controls ('Xuất' normalized from 导出) —
		// instructional UI text is protected (non-occlusion), narration has no bottom OCR text.
		ocr: []provider.RawTextDetection{
			det("导出", 960, 80, 70, 35),            // CapCut export button → instructional UI
			det("本内容为个人观点", 200, 250, 380, 45),    // disclaimer → semantic
			det("第3步：添加转场效果", 240, 1500, 480, 55), // on-screen tutorial caption → speech subtitle
		},
		frameW:       1080,
		frameH:       1920,
		ttsDurations: map[int]int64{0: 1100, 1: 1100, 2: 1100},
		ttsDuration:  1100,
		expectDub:    true,
		wantRoles: []domain.TextRegionRole{
			domain.TextRoleInstructionalUIText,
			domain.TextRoleSemanticText,
			domain.TextRoleSpeechSubtitle,
		},
		absentRoles: []domain.TextRegionRole{
			domain.TextRoleBrandKeep,
			domain.TextRoleIgnoreNoise,
		},
		wantOverlays:  2, // UI control + disclaimer
		wantProtected: 1,
		wantCues:      3,
		// Doc: music-only outro 27.2s-33.11s (scaled here to the synthetic 6.0s-8.5s BGM tail).
		outroNoDubStartMs: 6000,
		outroNoDubEndMs:   8500,
		evidence:          fixtureEvidence["video4"],
	},
	{
		id:   "video5_narration_dub_brand_keep",
		name: "Video 5 — narration dub + bottom subtitle + brand/object KEEP on 3:4 frame",
		segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "今天天气很好。", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2300},
			{Index: 1, SourceText: "请将温度调至25度，张伟说不要打开窗户。", SpeakerID: "SPEAKER_00", StartMs: 2800, EndMs: 5200},
		},
		rolePlan: []domain.AudioSegment{
			{StartMs: 0, EndMs: 8500, Role: domain.AudioRoleInstrumentalBgm},
			{StartMs: 0, EndMs: 2300, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 2800, EndMs: 5200, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 0, EndMs: 8500, Role: domain.AudioRoleAmbienceSFX}, // Foley throughout
		},
		// Doc: SUPOR/Samyang brand KEEP (protected, untouched) + bottom subtitle overlay —
		// brand regions generate ZERO overlays and act as non-occlusion obstacles. The 3:4
		// 1080x1440 frame exercises scale-aware geometry distinct from the 9:16 fixtures.
		ocr: []provider.RawTextDetection{
			det("SUPOR", 60, 200, 200, 55),      // brand KEEP (product label)
			det("Samyang", 760, 600, 220, 50),   // brand KEEP (packaging)
			det("创意相机角度教学", 240, 1150, 600, 60), // bottom subtitle
		},
		frameW:       1080,
		frameH:       1440,
		ttsDurations: map[int]int64{0: 1500, 1: 2000},
		ttsDuration:  1500,
		expectDub:    true,
		wantRoles:    []domain.TextRegionRole{domain.TextRoleBrandKeep, domain.TextRoleSpeechSubtitle},
		absentRoles: []domain.TextRegionRole{
			domain.TextRoleSemanticText,
			domain.TextRoleInstructionalUIText,
			domain.TextRoleIgnoreNoise,
		},
		wantOverlays:  0, // brand_keep must remain untouched
		wantProtected: 2,
		wantCues:      2,
		evidence:      fixtureEvidence["video5"],
	},
}

// TestSeam1_AcceptanceGate_NormativeInvariants drives each canonical fixture through the
// Seam 1 RuntimeHost API and asserts the normative cross-video invariant set.
func TestSeam1_AcceptanceGate_NormativeInvariants(t *testing.T) {
	for _, fg := range acceptanceGateFixtures {
		fg := fg
		t.Run(fg.id, func(t *testing.T) {
			h := setupHarness(t)
			configureFixtureProviders(t, h, fg)

			jobID, runID := createJobAndRun(t, h)
			job := getJobViaAPI(t, h, jobID)
			assetID := job.SourceAssetID
			targetLang := domain.TargetLanguageVI

			saveAudioRolePlan(t, h, assetID, fg.rolePlan)

			// --- Dubbed branch: translation → dub script → voice → TTS fit gate ---
			var dubSegments *domain.DubSegmentsVariant
			var dubScript *domain.DubScriptVariant
			if fg.expectDub {
				transVariant := runFixtureTranslation(t, h, assetID, runID, fg.segments, fg.evidence)
				dubScript = runFixtureDubScript(t, h, assetID, runID, transVariant.CASHash)

				_, voiceAssign := runAssignVoices(t, h, assetID, map[string]any{
					"run_id":          runID,
					"job_id":          jobID,
					"target_language": targetLang,
				})
				if voiceAssign == nil {
					t.Fatalf("voice assignment failed")
				}
				if fg.wantSpeakers > 0 {
					if len(voiceAssign.Assignments) != fg.wantSpeakers {
						t.Fatalf("expected %d speaker assignments, got %d", fg.wantSpeakers, len(voiceAssign.Assignments))
					}
					seen := map[string]bool{}
					for spk, v := range voiceAssign.Assignments {
						if seen[v.ID] {
							t.Errorf("speaker %s shares voice %q with another speaker (1 stable voice per speaker)", spk, v.ID)
						}
						seen[v.ID] = true
					}
				}

				resp, segs := runDubSynthesize(t, h, assetID, map[string]any{
					"run_id":                 runID,
					"job_id":                 jobID,
					"target_language":        targetLang,
					"dub_script_variant_cas": dubScript.CASHash,
					"voice_assignment_cas":   voiceAssign.CASHash,
				})
				if resp.StatusCode != http.StatusCreated || segs == nil {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("dub-synthesize failed: status=%d body=%s", resp.StatusCode, string(b))
				}
				dubSegments = segs

				assertDubInvariants(t, h, fg, assetID, dubSegments, dubScript)
			}

			// --- Visual: detect-text → localized visual track (always runs, even no-dub) ---
			plan := runDetectText(t, h, assetID, runID)
			track := runLocalizedVisualTrack(t, h, assetID, runID, jobID)
			assertVisualInvariants(t, fg, plan, track)

			// --- Audio compose: passthrough (no-dub) or dialogue-suppression + preservation (dub) ---
			mixPayload := map[string]any{
				"run_id":          runID,
				"job_id":          jobID,
				"target_language": targetLang,
			}
			if fg.expectDub && dubSegments != nil {
				mixPayload["dub_segments_cas"] = dubSegments.CASHash
			}
			respMix, mix := runAudioMix(t, h, assetID, mixPayload)
			if respMix.StatusCode != http.StatusCreated {
				t.Fatalf("audio-mix failed: status=%d", respMix.StatusCode)
			}
			if fg.expectDub && dubSegments != nil && mix.DubSegmentsCAS != dubSegments.CASHash {
				t.Errorf("audio mix DubSegmentsCAS %q != synthesized dub segments CAS %q (lineage breach)",
					mix.DubSegmentsCAS, dubSegments.CASHash)
			}
			assertAudioMixInvariants(t, fg, mix, assetID)

			// --- Deterministic composition: freeze render plan ---
			renderPlan := runFreezeRenderPlanForGate(t, h, assetID, runID, jobID, mix)
			if renderPlan.DubMixCASHash != mix.CASHash {
				t.Errorf("render plan DubMixCASHash %q != audio mix CAS %q (lineage breach)", renderPlan.DubMixCASHash, mix.CASHash)
			}
			assertRenderPlanInvariants(t, fg, renderPlan)
		})
	}
}

// configureFixtureProviders overrides the deterministic fake TTS/OCR providers so each
// fixture's measured durations, predicted (planning) evidence, frame geometry, and OCR
// detection set are fixture-specific.
// ocrObservationFrames is how many sampled frames a fixture label stays on screen. One frame is
// not a readable label: the out-of-band classification gate requires an observation on at least
// two samples, which is what a real 500ms cadence gives any caption a viewer could actually read.
const ocrObservationFrames = 2

// persistentDetections expands single-shot fixture detections into a short observation run.
func persistentDetections(dets []provider.RawTextDetection) []provider.RawTextDetection {
	out := make([]provider.RawTextDetection, 0, len(dets)*ocrObservationFrames)
	for _, d := range dets {
		for i := range ocrObservationFrames {
			obs := d
			obs.FrameIndex = d.FrameIndex + i
			obs.TimestampMs = d.TimestampMs + int64(i)*500
			out = append(out, obs)
		}
	}
	return out
}

func configureFixtureProviders(t *testing.T, h *testHarness, fg fixtureGate) {
	t.Helper()
	// TTS: per-segment measured durations + a deliberately wrong predicted duration.
	fakeTTS := defaultVITTSFake(t, h)
	fakeTTS.CustomDurations = fg.ttsDurations
	if fg.ttsDuration > 0 {
		fakeTTS.DurationMs = fg.ttsDuration
	}
	fakeTTS.CustomPredictedMs = fg.predictedMs
	fakeTTS.Invocations = 0
	// OCR: fixture-specific detections + frame geometry (Video 5 is 3:4).
	if p, ok := h.registry.Get("fake_paddle_ocr"); ok {
		fake := p.(*provider.FakeOCRProvider)
		fake.CustomDetections = persistentDetections(fg.ocr)
		fake.FrameWidth = fg.frameW
		fake.FrameHeight = fg.frameH
	}
}

// runFixtureTranslation drives the meaning-first translation branch over explicit source
// segments (the fixture's immutable source speech windows).
func runFixtureTranslation(t *testing.T, h *testHarness, assetID, runID string, segments []domain.TranslationInputSegment, evidence string) *domain.TranslationVariant {
	t.Helper()
	resp, v := runTranslation(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"segments":        segments,
	})
	if resp.StatusCode != http.StatusCreated || v == nil {
		t.Fatalf("translation failed: %d (fixture evidence: %s)", resp.StatusCode, evidence)
	}
	return v
}

// runFixtureDubScript drives the spoken duration/cadence adaptation branch.
func runFixtureDubScript(t *testing.T, h *testHarness, assetID, runID, translationCAS string) *domain.DubScriptVariant {
	t.Helper()
	resp, v := runDubScript(t, h, assetID, map[string]any{
		"run_id":                  runID,
		"target_language":         domain.TargetLanguageVI,
		"translation_variant_cas": translationCAS,
	})
	if resp.StatusCode != http.StatusCreated || v == nil {
		t.Fatalf("dub-script adaptation failed: %d", resp.StatusCode)
	}
	return v
}

// saveAudioRolePlan persists the fixture's audio role plan through the Seam 1 API.
func saveAudioRolePlan(t *testing.T, h *testHarness, assetID string, segs []domain.AudioSegment) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"segments": segs})
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save audio role plan: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("save audio role plan failed: %d body=%s", resp.StatusCode, string(b))
	}
}

// runDetectText triggers OCR detection + multi-role TextRegionPlan generation.
func runDetectText(t *testing.T, h *testHarness, assetID, runID string) *domain.TextRegionPlan {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"run_id": runID})
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/detect-text", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("detect-text: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("detect-text failed: %d body=%s", resp.StatusCode, string(b))
	}
	var res struct {
		Plan domain.TextRegionPlan `json:"text_region_plan"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode text-region-plan: %v", err)
	}
	if res.Plan.ID == "" || res.Plan.ProvenanceHash == "" {
		t.Fatalf("detect-text returned incomplete plan")
	}
	return &res.Plan
}

// runLocalizedVisualTrack generates the compact-fit + in-place overlay visual track.
func runLocalizedVisualTrack(t *testing.T, h *testHarness, assetID, runID, jobID string) *domain.LocalizedVisualTrack {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
	})
	resp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/localized-visual-track", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("localized-visual-track: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("localized-visual-track failed: %d body=%s", resp.StatusCode, string(b))
	}
	var res struct {
		Track domain.LocalizedVisualTrack `json:"localized_visual_track"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode localized-visual-track: %v", err)
	}
	if res.Track.CASHash == "" || res.Track.SubtitleTrackCAS == "" {
		t.Fatalf("localized-visual-track incomplete (cas=%q subtitle=%q)", res.Track.CASHash, res.Track.SubtitleTrackCAS)
	}
	return &res.Track
}

// runFreezeRenderPlanForGate freezes the deterministic composition recipe.
func runFreezeRenderPlanForGate(t *testing.T, h *testHarness, assetID, runID, jobID string, mix *domain.DubMixArtifact) *domain.RenderPlan {
	t.Helper()
	body := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"dub_mix_cas":     mix.CASHash,
	}
	resp, plan := runFreezeRenderPlan(t, h, assetID, body)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("render-plan failed: %d body=%s", resp.StatusCode, string(b))
	}
	return plan
}

// assertDubInvariants asserts the cross-video dubbing invariants over the accepted segments.
func assertDubInvariants(t *testing.T, h *testHarness, fg fixtureGate, assetID string, segs *domain.DubSegmentsVariant, script *domain.DubScriptVariant) {
	t.Helper()

	// Expected accepted segments must all fit (this fixture is a clean run).
	if segs.OverallStatus != "PASS" {
		t.Fatalf("expected PASS on clean fixture run, got %s", segs.OverallStatus)
	}
	got := len(segs.Segments)
	want := len(fg.segments)
	if got != want {
		t.Fatalf("expected %d accepted segments, got %d", want, got)
	}
	if len(segs.ReviewSegments) != 0 {
		t.Fatalf("expected no review segments on clean run, got %d", len(segs.ReviewSegments))
	}

	// Invariant 5: source-relative pacing/cadence AV-QC outcome (relations + production gate).
	if len(script.Segments) != want {
		t.Fatalf("expected %d dub-script segments, got %d", want, len(script.Segments))
	}
	assertCadenceAVQC(t, h, assetID, script)

	for i, seg := range segs.Segments {
		if seg.Index != i {
			t.Errorf("segment %d: unexpected segment index %d", i, seg.Index)
		}
		// Invariant 2: tts_finish <= immutable source window end (zero overrun).
		finish := seg.StartMs + seg.MeasuredDurationMs
		if finish > seg.EndMs {
			t.Errorf("segment %d zero-overrun breach: finish %dms > immutable end %dms", i, finish, seg.EndMs)
		}
		if seg.MeasuredDurationMs > seg.SlotDurationMs {
			t.Errorf("segment %d measured %dms > slot %dms", i, seg.MeasuredDurationMs, seg.SlotDurationMs)
		}
		// Invariant 1: measured media truth — MeasuredDurationMs equals the probed waveform duration.
		probed := probeSegmentDuration(t, h, seg.AudioSHA256)
		if probed < 0 {
			t.Fatalf("segment %d: failed to probe audio from CAS", i)
		}
		if seg.MeasuredDurationMs != probed {
			t.Errorf("segment %d: measured %dms != probed waveform %dms (measured-media truth)", i, seg.MeasuredDurationMs, probed)
		}
		// Invariant 3/4: no adjacent collision, and the inter-turn gap equals the exact
		// source-derived relation next_turn_start − dub_finish (clamped ≥ 0) — never a
		// hard-coded floor.
		if i+1 < len(segs.Segments) {
			next := segs.Segments[i+1]
			if finish > next.StartMs {
				t.Errorf("segment %d collision/run-on: finish %dms > next start %dms", i, finish, next.StartMs)
			}
			wantGap := next.StartMs - finish
			if wantGap < 0 {
				wantGap = 0
			}
			if seg.NaturalGapAfterMs != wantGap {
				t.Errorf("segment %d natural gap %dms != source-derived relation %dms (next start %d - finish %d)",
					i, seg.NaturalGapAfterMs, wantGap, next.StartMs, finish)
			}
		} else if seg.NaturalGapAfterMs < 0 {
			t.Errorf("last segment %d: negative gap %dms", i, seg.NaturalGapAfterMs)
		}
	}
}

// assertCadenceAVQC proves the ticket-required source-relative pacing/cadence AV-QC
// OUTCOME as relations plus production's own gate verdict — never a positivity check and
// never a threshold re-encoded in the test:
//   - the artifact's source rate equals the production source-rate derivation from the
//     immutable source text + slot (cadence evidence is source-relative, not fabricated);
//   - the target rate equals the derivation from the adapted spoken text, and CadenceRatio
//     is exactly that target÷source quotient;
//   - TargetWordBudget is load-bearing on source cadence: re-running the production adapter
//     with the source rate scaled yields a monotonic budget, and at least one segment's
//     budget strictly tracks the source rate (cadence drives adaptation, not metadata-only);
//   - the pacing gate PASSES: production's adapter verdict on the final spoken copy raises
//     no CADENCE_* reason, no artifact segment carries one, and the public Seam 1 review
//     projection carries no pending cadence/pacing exception. Planning-stage duration
//     overrun is separate evidence resolved downstream by the measured fit controller
//     (invariants 1/2), so it is not re-asserted here.
func assertCadenceAVQC(t *testing.T, h *testHarness, assetID string, script *domain.DubScriptVariant) {
	t.Helper()
	adapter := provider.NewDefaultSpokenScriptAdapter()
	cadenceDriven := false
	for _, s := range script.Segments {
		if s.SourceSpeakingRateCPS <= 0 || s.TargetSpeakingRateCPS <= 0 || s.CadenceRatio <= 0 {
			t.Errorf("segment %d: cadence evidence must be positive, got src=%f tgt=%f ratio=%f",
				s.Index, s.SourceSpeakingRateCPS, s.TargetSpeakingRateCPS, s.CadenceRatio)
			continue
		}
		// Relation 1: the source rate is derived from the immutable source text + slot.
		// Recomputed independently here (Han char/digit = one speech unit, one per Latin
		// token, punctuation excluded) so a fabricated or text-independent source rate
		// cannot satisfy the relation by matching the production estimator on both sides.
		src := sourceSpeechRate(s.SourceText, s.SlotDurationMs)
		if math.Abs(src-s.SourceSpeakingRateCPS) > 1e-9 {
			t.Errorf("segment %d: source rate %f != source-derived %f (cadence must be measured against source)",
				s.Index, s.SourceSpeakingRateCPS, src)
		}
		// Relation 2: target rate derives from the adapted spoken text.
		tgt := provider.EstimateSpokenRateCPS(s.SpokenText, script.TargetLanguage)
		if math.Abs(tgt-s.TargetSpeakingRateCPS) > 1e-9 {
			t.Errorf("segment %d: target rate %f != spoken-text-derived %f", s.Index, s.TargetSpeakingRateCPS, tgt)
		}
		// Relation 3: the ratio is the target÷source quotient.
		if math.Abs(tgt/src-s.CadenceRatio) > 1e-9 {
			t.Errorf("segment %d: cadence ratio %f != target÷source %f", s.Index, s.CadenceRatio, tgt/src)
		}
		// Relation 4: the word budget is a function of source cadence (production
		// adapter probe with scaled source rates), never a slot-only constant.
		req := provider.SpokenScriptAdaptationRequest{
			SourceText:     s.SourceText,
			SourceLanguage: "zh",
			MeaningText:    s.MeaningText,
			TargetLanguage: script.TargetLanguage,
			SlotDurationMs: s.SlotDurationMs,
		}
		base := adaptBudgetProbe(t, adapter, req, src)
		slow := adaptBudgetProbe(t, adapter, req, src*0.5)
		fast := adaptBudgetProbe(t, adapter, req, src*2)
		if slow > base || base > fast {
			t.Errorf("segment %d: word budget must be monotonic in source cadence: slow=%d base=%d fast=%d",
				s.Index, slow, base, fast)
		}
		if slow < base || base < fast {
			cadenceDriven = true
		}
		if base != s.TargetWordBudget {
			t.Errorf("segment %d: artifact budget %d != production source-relative budget %d",
				s.Index, s.TargetWordBudget, base)
		}
		// Gate outcome: the artifact's ReviewReason is production's verdict computed on
		// the final spoken copy — a clean fixture must raise no CADENCE_* reason.
		if strings.HasPrefix(s.ReviewReason, "CADENCE_") {
			t.Errorf("segment %d: cadence AV-QC failed: %s (ratio %f vs source)", s.Index, s.ReviewReason, s.CadenceRatio)
		}
	}
	if !cadenceDriven {
		t.Errorf("no segment's word budget tracks source cadence — cadence evidence is metadata-only")
	}
	if script.RequiresReview {
		for _, s := range script.Segments {
			if strings.HasPrefix(s.ReviewReason, "CADENCE_") {
				t.Errorf("variant review driven by cadence gate failure: segment %d reason %s", s.Index, s.ReviewReason)
			}
		}
	}
	// Public Seam 1 review projection: no pending pacing/cadence exception.
	for _, it := range fetchReviewItems(t, h, assetID) {
		if strings.HasPrefix(it.Reason, "CADENCE_") || strings.Contains(strings.ToLower(it.Reason), "pacing") {
			t.Errorf("review projection carries pending cadence AV-QC exception: %+v", it)
		}
	}
}

// sourceSpeechRate independently derives the source cadence (speech units/sec) from the
// immutable source text and slot: one unit per Han character or digit, one per contiguous
// Latin letter run, punctuation/whitespace excluded. Deliberately an independent
// re-derivation so Relation 1 is a real cross-check, not estimator == estimator.
func sourceSpeechRate(text string, slotDurationMs int64) float64 {
	if slotDurationMs <= 0 {
		return 0
	}
	var units float64
	inLatin := false
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			units++
			inLatin = false
		case unicode.IsDigit(r):
			units++
			inLatin = false
		case unicode.IsLetter(r):
			if !inLatin {
				units++
				inLatin = true
			}
		default:
			inLatin = false
		}
	}
	if units <= 0 {
		return 0
	}
	return units / (float64(slotDurationMs) / 1000.0)
}

// adaptBudgetProbe re-runs the production adapter with an overridden source rate and
// returns the resulting source-relative word budget.
func adaptBudgetProbe(t *testing.T, adapter provider.SpokenScriptAdapter, req provider.SpokenScriptAdaptationRequest, sourceRate float64) int {
	t.Helper()
	req.SourceSpeakingRateCPS = sourceRate
	res, err := adapter.AdaptSpokenScript(context.Background(), req)
	if err != nil {
		t.Fatalf("adapter budget probe: %v", err)
	}
	return res.TargetWordBudget
}

// fetchReviewItems reads the pending review queue through the public Seam 1 API.
func fetchReviewItems(t *testing.T, h *testHarness, assetID string) []domain.ReviewItem {
	t.Helper()
	resp, err := http.Get(h.server.URL + "/api/v1/assets/" + assetID + "/review-items?target_language=vi")
	if err != nil {
		t.Fatalf("GET review-items: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET review-items: status=%d body=%s", resp.StatusCode, string(b))
	}
	var body struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode review-items: %v", err)
	}
	return body.ReviewItems
}

// probeSegmentDuration reads a segment's synthesized WAV from CAS and returns its probed
// duration in ms, or -1 on failure.
func probeSegmentDuration(t *testing.T, h *testHarness, sha string) int64 {
	t.Helper()
	if sha == "" {
		return -1
	}
	r, err := h.casStore.Get(sha)
	if err != nil {
		t.Logf("CAS read %s failed: %v", sha, err)
		return -1
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Logf("read %s failed: %v", sha, err)
		return -1
	}
	dur, err := media.ProbeWAVBytes(data)
	if err != nil {
		t.Logf("ProbeWAVBytes %s failed: %v", sha, err)
		return -1
	}
	return dur
}

// assertVisualInvariants asserts the TextRegionPlan roles + compact overlay + non-occlusion
// invariants per fixture: roles are first-class domain meaning, the fixture's documented
// visual strategy defines which roles must (and must not) appear, deterministic in-place
// cover is the default (inpainting is non-default), and overlay/protected/cue counts match
// the fixture's minimized scenario exactly.
func assertVisualInvariants(t *testing.T, fg fixtureGate, plan *domain.TextRegionPlan, track *domain.LocalizedVisualTrack) {
	t.Helper()

	// Invariant: the plan carries exactly the fixture's expected role classification.
	roles := make(map[domain.TextRegionRole]bool)
	for _, reg := range plan.Regions {
		if !domain.IsValidTextRegionRole(reg.Role) {
			t.Errorf("region %q has invalid role %q", reg.ID, reg.Role)
		}
		roles[reg.Role] = true
	}
	for _, wantRole := range fg.wantRoles {
		if !roles[wantRole] {
			t.Errorf("expected role %q in TextRegionPlan (fixture %s visual strategy), got %+v", wantRole, fg.id, roles)
		}
	}
	for _, absentRole := range fg.absentRoles {
		if roles[absentRole] {
			t.Errorf("role %q must not appear on fixture %s (classification over-assigned)", absentRole, fg.id)
		}
	}

	// Invariant: frame geometry is fixture-specific and flows into the plan.
	if plan.FrameWidth != fg.frameW || plan.FrameHeight != fg.frameH {
		t.Fatalf("plan frame %dx%d != fixture frame %dx%d", plan.FrameWidth, plan.FrameHeight, fg.frameW, fg.frameH)
	}
	frameW, frameH := fg.frameW, fg.frameH

	// Invariant: deterministic in-place cover (IsCoverDefault), never full-frame rectangles,
	// brand_keep preserved untouched (no overlay), non-occlusion against protected regions.
	sawBrandKeepOverlay := false
	sawInstructional := false
	for _, ov := range track.Overlays {
		if !ov.IsCoverDefault {
			t.Errorf("overlay region %q: expected deterministic in-place cover (IsCoverDefault=true)", ov.RegionID)
		}
		if ov.Inpainting {
			t.Errorf("overlay region %q: inpainting is a non-default fallback and must not be selected on this fixture", ov.RegionID)
		}
		if ov.Role == domain.TextRoleBrandKeep {
			sawBrandKeepOverlay = true
		}
		if ov.Role == domain.TextRoleInstructionalUIText {
			sawInstructional = true
		}
		// Compact: the cover box hugs rendered text — strictly narrower than the frame.
		if ov.Box.Width <= 0 || ov.Box.Width >= frameW {
			t.Errorf("overlay region %q: box width %d is not compact fit-content (< frame width %d)", ov.RegionID, ov.Box.Width, frameW)
		}
		if ov.Box.Height <= 0 || ov.Box.Height >= frameH {
			t.Errorf("overlay region %q: box height %d is not compact (< frame height %d)", ov.RegionID, ov.Box.Height, frameH)
		}
	}
	if sawBrandKeepOverlay {
		t.Errorf("brand_keep region must remain untouched — no overlay generated")
	}
	if len(track.Overlays) != fg.wantOverlays {
		t.Errorf("expected exactly %d overlays on fixture %s, got %d", fg.wantOverlays, fg.id, len(track.Overlays))
	}
	if !sawInstructional && hasRole(plan, domain.TextRoleInstructionalUIText) {
		t.Errorf("instructional_ui_text region present but no overlay generated")
	}
	if hasRole(plan, domain.TextRoleSemanticText) && countOverlaysForRole(track, domain.TextRoleSemanticText) == 0 && fg.wantOverlays > 0 {
		t.Errorf("semantic_text regions present but no semantic overlay generated")
	}

	// Invariant: protected regions match the fixture's protected text (brand/UI) count.
	if len(track.ProtectedRegions) != fg.wantProtected {
		t.Errorf("expected exactly %d protected regions on fixture %s, got %d", fg.wantProtected, fg.id, len(track.ProtectedRegions))
	}

	// Invariant: subtitle cue count matches the fixture's documented subtitle strategy
	// (bottom-band OCR → cue fallback; dubbed → one cue per dub-script segment).
	if len(track.SubtitleCues) != fg.wantCues {
		t.Fatalf("expected exactly %d subtitle cues on fixture %s, got %d", fg.wantCues, fg.id, len(track.SubtitleCues))
	}
	for idx, cue := range track.SubtitleCues {
		if cue.Width <= 0 || cue.Width >= frameW {
			t.Errorf("cue[%d]: width %d not compact (< frame width %d)", idx, cue.Width, frameW)
		}
		if cue.Height <= 0 || cue.Height >= frameH {
			t.Errorf("cue[%d]: height %d not compact (< frame height %d)", idx, cue.Height, frameH)
		}
		// Scale-aware padding is fixture-class evidence (frame geometry), not asserted as a
		// universal constant beyond the fit-content requirement.
		if cue.PaddingX <= 0 {
			t.Errorf("cue[%d]: expected positive fit-content padding_x", idx)
		}
		if cue.PaddingY <= 0 {
			t.Errorf("cue[%d]: expected positive fit-content padding_y", idx)
		}
		cueBox := domain.BoundingBox{X: cue.X, Y: cue.Y, Width: cue.Width, Height: cue.Height}
		for pIdx, prot := range track.ProtectedRegions {
			if domain.BoxesOverlap(cueBox, prot) {
				t.Errorf("cue[%d] occludes protected region[%d]: cue=(%d,%d,w=%d,h=%d) prot=(%d,%d,w=%d,h=%d)",
					idx, pIdx, cue.X, cue.Y, cue.Width, cue.Height, prot.X, prot.Y, prot.Width, prot.Height)
			}
		}
	}
}

// countOverlaysForRole counts overlays carrying the given role.
func countOverlaysForRole(track *domain.LocalizedVisualTrack, role domain.TextRegionRole) int {
	n := 0
	for _, ov := range track.Overlays {
		if ov.Role == role {
			n++
		}
	}
	return n
}

// hasRole reports whether the plan contains a region of the given role.
func hasRole(plan *domain.TextRegionPlan, role domain.TextRegionRole) bool {
	for _, reg := range plan.Regions {
		if reg.Role == role {
			return true
		}
	}
	return false
}

// assertAudioMixInvariants asserts soundtrack preservation: dialogue-only suppression on the
// dubbed branch, bitstream passthrough where the plan allows (no-dub fixture), and — on the
// music-only-outro fixture — no suppress_dialogue action over the outro window.
func assertAudioMixInvariants(t *testing.T, fg fixtureGate, mix *domain.DubMixArtifact, assetID string) {
	t.Helper()
	if mix == nil || mix.CASHash == "" {
		t.Fatalf("audio-mix returned no artifact")
	}
	if mix.OverallStatus != "PASS" {
		t.Fatalf("expected PASS audio mix, got %s (reason: %s)", mix.OverallStatus, mix.RefusalReason)
	}
	if !mix.SoundtrackPreserved {
		t.Errorf("expected SoundtrackPreserved=true (perceptual preservation invariant)")
	}

	if fg.expectPassthrough {
		// No-dub: bitstream-exact passthrough of the normalized source audio.
		if mix.DialogueSuppressed {
			t.Errorf("no-dub fixture must not suppress dialogue (no dubbing applied)")
		}
		if mix.DubSegmentsCAS != "" {
			t.Errorf("no-dub fixture must carry no dub segments, got %q", mix.DubSegmentsCAS)
		}
		if mix.AudioCASHash == "" {
			t.Errorf("no-dub passthrough missing normalized audio reference")
		}
		if len(mix.PreservationPlan.SpeechWindows) != 0 {
			t.Errorf("no-dub passthrough must have zero speech windows, got %d", len(mix.PreservationPlan.SpeechWindows))
		}
		return
	}

	// Dubbed branch: dialogue-only suppression inside the immutable narration windows.
	if !mix.DialogueSuppressed {
		t.Errorf("expected DialogueSuppressed=true on dubbed branch")
	}
	if !mix.PreservationPlan.PreserveSFX {
		t.Errorf("expected PreserveSFX=true")
	}
	if !mix.PreservationPlan.PreserveAmbience {
		t.Errorf("expected PreserveAmbience=true")
	}

	// Every narration window must be a suppress_dialogue speech window matching the source anchors.
	var narration []domain.AudioSegment
	for _, seg := range fg.rolePlan {
		if seg.Role == domain.AudioRoleNarrationDialogue {
			narration = append(narration, seg)
		}
	}
	if len(mix.PreservationPlan.SpeechWindows) != len(narration) {
		t.Fatalf("expected %d speech windows, got %d", len(narration), len(mix.PreservationPlan.SpeechWindows))
	}
	for i, win := range mix.PreservationPlan.SpeechWindows {
		if win.Action != "suppress_dialogue" {
			t.Errorf("speech window %d: expected suppress_dialogue, got %s", i, win.Action)
		}
		if win.StartMs != narration[i].StartMs || win.EndMs != narration[i].EndMs {
			t.Errorf("speech window %d bounds [%d,%d] != source anchors [%d,%d]",
				i, win.StartMs, win.EndMs, narration[i].StartMs, narration[i].EndMs)
		}
		// Music-only outro: a non-speech BGM tail must never be suppressed (Video 4).
		if fg.outroNoDubEndMs > 0 && win.EndMs > fg.outroNoDubStartMs && win.StartMs < fg.outroNoDubEndMs {
			t.Errorf("speech window %d [%d,%d] overlaps music-only outro [%d,%d] — outro must be preserved untouched",
				i, win.StartMs, win.EndMs, fg.outroNoDubStartMs, fg.outroNoDubEndMs)
		}
	}
}

// assertRenderPlanInvariants asserts the frozen render plan deterministically consumes the
// mix + subtitle track (no arbitrary latest-row fallback), keeping a stable composition
// recipe with the fixture's exact cue count.
func assertRenderPlanInvariants(t *testing.T, fg fixtureGate, plan *domain.RenderPlan) {
	t.Helper()
	if plan == nil {
		t.Fatalf("render plan not returned")
	}
	if plan.ProvenanceHash == "" || plan.CASHash == "" {
		t.Fatalf("render plan missing provenance/cas hash")
	}
	if plan.SubtitlePlan.CueCount != fg.wantCues {
		t.Errorf("render plan subtitle cue count %d != fixture expectation %d", plan.SubtitlePlan.CueCount, fg.wantCues)
	}
	if len(plan.SubtitleCues) != fg.wantCues {
		t.Errorf("render plan pinned %d subtitle cues, expected %d", len(plan.SubtitleCues), fg.wantCues)
	}
}
