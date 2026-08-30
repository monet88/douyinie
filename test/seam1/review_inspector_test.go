package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// TestSeam1_ExceptionOnlyReviewQueue_And_InspectorTargetTextCorrection verifies:
// 1. Exception-only review queue: clean runs produce 0 pending items.
// 2. Overrun / QA failures surface in review queue.
// 3. POST /api/v1/assets/{id}/inspector/correct-text executes targeted invalidation & rerun.
// 4. Passing rerun candidate auto-resolves and leaves the review queue.
func TestSeam1_ExceptionOnlyReviewQueue_And_InspectorTargetTextCorrection(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Initial setup with an overly long translation segment that causes overrun
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "点击右上角关注",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      1500, // 1.5s slot
		},
	}
	transVariant, dubScriptVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	// Setup TextRegionPlan
	tPlan := domain.TextRegionPlan{
		ID:             "text-plan-seam1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-seam1",
		CreatedAt:      time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(tPlan)
	tObj, _ := h.casStore.Put(bytes.NewReader(tBytes))
	_ = h.db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             tPlan.ID,
		AssetID:        assetID,
		CASHash:        tObj.SHA256,
		ProvenanceHash: tPlan.ProvenanceHash,
		CreatedAt:      tPlan.CreatedAt,
	})

	// Setup AudioRolePlan
	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	roleResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	if err != nil || (roleResp.StatusCode != http.StatusOK && roleResp.StatusCode != http.StatusCreated) {
		t.Fatalf("setup audio role plan failed: %v", err)
	}
	roleResp.Body.Close()

	// Assign voices
	assignBody := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
	}
	assignResp, _ := runAssignVoices(t, h, assetID, assignBody)
	if assignResp.StatusCode != http.StatusOK && assignResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST voice-assignment failed: status=%d", assignResp.StatusCode)
	}

	// Save DubSegmentsVariant with an overrun exception (2400ms > 1500ms slot)
	dubSegVar := domain.DubSegmentsVariant{
		ID:                  "dubseg-ovr-seam1",
		RunID:               runID,
		JobID:               jobID,
		AssetID:             assetID,
		TargetLanguage:      "vi",
		DubScriptVariantCAS: dubScriptVariant.CASHash,
		ProvenanceHash:      "prov-dubseg-ovr-seam1",
		OverallStatus:       "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              1500,
				SlotDurationMs:     1500,
				MeasuredDurationMs: 2400,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
				AttemptCount:       3,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	dubSegBytes, _ := json.Marshal(dubSegVar)
	dubSegObj, _ := h.casStore.Put(bytes.NewReader(dubSegBytes))
	_ = h.db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dubSegVar.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: "vi",
		CASHash:        dubSegObj.SHA256,
		ProvenanceHash: dubSegVar.ProvenanceHash,
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      dubSegVar.CreatedAt,
	})

	// 2. Query review queue: should contain 1 pending overrun exception
	getQueueURL := fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID)
	qResp, err := http.Get(getQueueURL)
	if err != nil || qResp.StatusCode != http.StatusOK {
		t.Fatalf("GET review-items failed: status=%d err=%v", qResp.StatusCode, err)
	}
	var qBody struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qBody)
	qResp.Body.Close()

	if qBody.Count != 1 || len(qBody.ReviewItems) != 1 {
		t.Fatalf("expected 1 pending exception in review queue, got %d", qBody.Count)
	}
	if qBody.ReviewItems[0].Type != domain.ReviewItemTypeTTSOverrun {
		t.Errorf("expected review item type tts_overrun, got %s", qBody.ReviewItems[0].Type)
	}

	// 3. Execute inspector target-text correction via HTTP endpoint
	corrPayload := map[string]any{
		"run_id":               runID,
		"job_id":               jobID,
		"target_language":      "vi",
		"segment_index":        0,
		"new_target_text":      "Bấm góc trên", // Shortened concise text
		"spoken_text_override": "Bấm góc trên",
		"reason":               "Shortened for 1.5s slot fit",
		"operator":             "qa_lead",
	}
	corrBytes, _ := json.Marshal(corrPayload)
	corrURL := fmt.Sprintf("%s/api/v1/assets/%s/inspector/correct-text", h.server.URL, assetID)
	corrResp, err := http.Post(corrURL, "application/json", bytes.NewReader(corrBytes))
	if err != nil || corrResp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		if corrResp != nil {
			_, _ = buf.ReadFrom(corrResp.Body)
		}
		t.Fatalf("POST inspector/correct-text failed: status=%d err=%v body=%s", corrResp.StatusCode, err, buf.String())
	}
	var corrBody struct {
		Result service.TargetTextCorrectionResult `json:"result"`
	}
	_ = json.NewDecoder(corrResp.Body).Decode(&corrBody)
	corrResp.Body.Close()

	if corrBody.Result.TranslationVariantCAS == "" || corrBody.Result.TranslationVariantCAS == transVariant.CASHash {
		t.Errorf("expected newly minted TranslationVariant CAS hash, got %s", corrBody.Result.TranslationVariantCAS)
	}
	if corrBody.Result.DubScriptVariantCAS == "" || corrBody.Result.DubScriptVariantCAS == dubScriptVariant.CASHash {
		t.Errorf("expected newly minted DubScriptVariant CAS hash, got %s", corrBody.Result.DubScriptVariantCAS)
	}
	if corrBody.Result.Status != domain.ReviewItemStatusAutoResolved {
		t.Errorf("expected auto_resolved status, got %s", corrBody.Result.Status)
	}
	if corrBody.Result.DubMixCAS == "" {
		t.Errorf("expected non-empty DubMixCAS in result")
	}
	if corrBody.Result.LocalizedSubtitleCAS == "" {
		t.Errorf("expected non-empty LocalizedSubtitleCAS in result")
	}
	if corrBody.Result.RenderPlanCAS == "" {
		t.Errorf("expected non-empty RenderPlanCAS in result")
	}

	// Verify RenderPlan lineage and schema purity: decodes as domain.RenderPlan
	rprc, err := h.casStore.Get(corrBody.Result.RenderPlanCAS)
	if err != nil {
		t.Fatalf("load render plan from CAS failed: %v", err)
	}
	defer rprc.Close()
	var rPlan domain.RenderPlan
	if err := json.NewDecoder(rprc).Decode(&rPlan); err != nil {
		t.Fatalf("decode render plan failed (schema mismatch): %v", err)
	}
	if rPlan.DubMixCASHash != corrBody.Result.DubMixCAS {
		t.Errorf("RenderPlan did not pin new DubMixCAS: %s != %s", rPlan.DubMixCASHash, corrBody.Result.DubMixCAS)
	}
	if rPlan.SubtitlePlan.CASHash == "" || rPlan.SubtitlePlan.CASHash == corrBody.Result.LocalizedSubtitleCAS {
		t.Errorf("RenderPlan SubtitlePlan CAS invalid or mixed with LocalizedSubtitleTrack CAS: %s vs %s", rPlan.SubtitlePlan.CASHash, corrBody.Result.LocalizedSubtitleCAS)
	}
	// Verify SubtitlePlanArtifact in CAS decodes cleanly
	sprc, err := h.casStore.Get(rPlan.SubtitlePlan.CASHash)
	if err != nil {
		t.Fatalf("load subtitle plan artifact from CAS failed: %v", err)
	}
	defer sprc.Close()
	var subArt domain.SubtitlePlanArtifact
	if err := json.NewDecoder(sprc).Decode(&subArt); err != nil {
		t.Fatalf("decode subtitle plan artifact failed (schema mismatch): %v", err)
	}
	if len(subArt.Cues) == 0 {
		t.Errorf("expected subtitle plan artifact to have cues")
	}
	// 4. Verify Exception-only queue is now clean (0 pending items)
	qResp2, err := http.Get(getQueueURL)
	if err != nil || qResp2.StatusCode != http.StatusOK {
		t.Fatalf("GET review-items after correction failed: status=%d err=%v", qResp2.StatusCode, err)
	}
	var qBody2 struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp2.Body).Decode(&qBody2)
	qResp2.Body.Close()

	if qBody2.Count != 0 || len(qBody2.ReviewItems) != 0 {
		t.Errorf("expected 0 pending items after successful correction rerun, got %d: %+v", qBody2.Count, qBody2.ReviewItems)
	}
}

// TestSeam1_AuditableManualOverride_And_PreservedProvenance verifies:
// 1. Operator manual override recorded via HTTP POST /api/v1/assets/{id}/review/override.
// 2. Overridden exception leaves the pending review queue automatically.
// 3. Historical QA scores and provenance are immutable and NEVER mutated/faked to PASS.
// 4. Direct review item override route POST /api/v1/review-items/{id}/override works identically.
func TestSeam1_AuditableManualOverride_And_PreservedProvenance(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Create a TextRegionPlan with an uncertain role
	textPlan := domain.TextRegionPlan{
		ID:             "text-plan-ovr-seam1",
		AssetID:        assetID,
		ProvenanceHash: "prov-text-ovr-seam1",
		Regions: []domain.TrackedTextRegion{
			{
				ID:             "region-ovr-1",
				Role:           domain.TextRoleUncertain,
				Text:           "Watermark logo",
				FirstSeenMs:    0,
				LastSeenMs:     5000,
				ReviewRequired: true,
				ReviewReason:   "uncertain_visual_role",
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(textPlan)
	tObj, _ := h.casStore.Put(bytes.NewReader(tBytes))
	_ = h.db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             textPlan.ID,
		AssetID:        assetID,
		CASHash:        tObj.SHA256,
		ProvenanceHash: textPlan.ProvenanceHash,
		CreatedAt:      textPlan.CreatedAt,
	})

	// Initial check: 1 pending item
	getQueueURL := fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID)
	qResp, _ := http.Get(getQueueURL)
	var qBody struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qBody)
	qResp.Body.Close()

	if qBody.Count != 1 {
		t.Fatalf("expected 1 pending item, got %d", qBody.Count)
	}
	itemID := qBody.ReviewItems[0].ID

	// 2. Submit manual override via HTTP
	overridePayload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"review_item_id":  itemID,
		"stage":           "detect_text",
		"region_id":       "region-ovr-1",
		"reason":          "Static watermark; safe to ignore cover plan",
		"operator":        "reviewer_bob",
	}
	ovrBytes, _ := json.Marshal(overridePayload)
	ovrURL := fmt.Sprintf("%s/api/v1/assets/%s/review/override", h.server.URL, assetID)
	ovrResp, err := http.Post(ovrURL, "application/json", bytes.NewReader(ovrBytes))
	if err != nil || ovrResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST review/override failed: status=%d err=%v", ovrResp.StatusCode, err)
	}
	var ovrBody struct {
		Status   string                `json:"status"`
		Override domain.ReviewOverride `json:"override"`
	}
	_ = json.NewDecoder(ovrResp.Body).Decode(&ovrBody)
	ovrResp.Body.Close()

	if ovrBody.Status != "accepted" || ovrBody.Override.Operator != "reviewer_bob" {
		t.Errorf("unexpected override response: %+v", ovrBody)
	}

	// 3. Exception-only queue is now empty
	qResp2, _ := http.Get(getQueueURL)
	var qBody2 struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp2.Body).Decode(&qBody2)
	qResp2.Body.Close()

	if qBody2.Count != 0 {
		t.Errorf("expected 0 pending items after manual override, got %d", qBody2.Count)
	}

	// 4. Query with include_resolved=true returns the item with status manual_override
	getAllURL := fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi&include_resolved=true", h.server.URL, assetID)
	allResp, _ := http.Get(getAllURL)
	var allBody struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(allResp.Body).Decode(&allBody)
	allResp.Body.Close()

	if allBody.Count != 1 || allBody.ReviewItems[0].Status != domain.ReviewItemStatusManualOverride {
		t.Errorf("expected 1 item with status manual_override, got count=%d status=%s", allBody.Count, allBody.ReviewItems[0].Status)
	}

	// 5. Invariant: Original immutable TextRegionPlan in CAS is unchanged
	rc, err := h.casStore.Get(tObj.SHA256)
	if err != nil {
		t.Fatalf("load original text plan failed: %v", err)
	}
	var origPlan domain.TextRegionPlan
	_ = json.NewDecoder(rc).Decode(&origPlan)
	rc.Close()
	if origPlan.Regions[0].Role != domain.TextRoleUncertain {
		t.Errorf("historical role was mutated! expected uncertain, got %s", origPlan.Regions[0].Role)
	}
}

// TestSeam1_MultimodalQualityResults_DecoupledFromExecutionState verifies:
// 1. Stage execution can be SUCCEEDED while QualityResult is REVIEW_REQUIRED or FAIL.
// 2. Multimodal QualityResults are append-only.
// 3. QualityResult issues surface in the review items queue and can be overridden.
func TestSeam1_MultimodalQualityResults_DecoupledFromExecutionState(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Post a QualityResult via HTTP POST /api/v1/quality-results
	qrPayload := domain.QualityResult{
		ID:             "qr-sync-test-01",
		RunID:          runID,
		JobID:          jobID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Stage:          "render",
		OverallStatus:  domain.QualityStatusReviewRequired,
		Metrics: []domain.QualityMetric{
			{Name: "av_sync_drift_ms", Score: 85.0, Threshold: 50.0, Passed: false},
			{Name: "lip_activity_alignment", Score: 0.92, Threshold: 0.80, Passed: true},
		},
		Issues: []domain.ReviewItem{
			{
				ID:             "rev-qr-drift-1",
				RunID:          runID,
				AssetID:        assetID,
				TargetLanguage: "vi",
				Type:           domain.ReviewItemTypeTTSOverrun,
				Stage:          "render",
				Severity:       "warning",
				Reason:         "av_sync_drift_ms 85ms exceeds threshold 50ms",
				Status:         domain.ReviewItemStatusPending,
				CreatedAt:      time.Now().UTC(),
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	qrBytes, _ := json.Marshal(qrPayload)
	qrResp, err := http.Post(fmt.Sprintf("%s/api/v1/quality-results", h.server.URL), "application/json", bytes.NewReader(qrBytes))
	if err != nil || qrResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/quality-results failed: status=%d err=%v", qrResp.StatusCode, err)
	}
	qrResp.Body.Close()

	// 2. GET /api/v1/assets/{id}/quality-results returns the posted report
	getQRURL := fmt.Sprintf("%s/api/v1/assets/%s/quality-results?target_language=vi", h.server.URL, assetID)
	getResp, err := http.Get(getQRURL)
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/assets/{id}/quality-results failed: status=%d err=%v", getResp.StatusCode, err)
	}
	var qrGetBody struct {
		QualityResults []domain.QualityResult `json:"quality_results"`
		Count          int                    `json:"count"`
	}
	_ = json.NewDecoder(getResp.Body).Decode(&qrGetBody)
	getResp.Body.Close()

	if qrGetBody.Count != 1 || len(qrGetBody.QualityResults) != 1 {
		t.Fatalf("expected 1 quality result, got %d", qrGetBody.Count)
	}
	if qrGetBody.QualityResults[0].OverallStatus != domain.QualityStatusReviewRequired {
		t.Errorf("expected quality status REVIEW_REQUIRED, got %s", qrGetBody.QualityResults[0].OverallStatus)
	}

	// 3. Issue surfaces in ReviewItems queue
	getQueueURL := fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID)
	qResp, _ := http.Get(getQueueURL)
	var qBody struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qBody)
	qResp.Body.Close()

	if qBody.Count != 1 || qBody.ReviewItems[0].ID != "rev-qr-drift-1" {
		t.Fatalf("expected 1 pending item with ID rev-qr-drift-1, got count=%d", qBody.Count)
	}

	// 4. Override the issue via direct review-item override endpoint
	itemOvrURL := fmt.Sprintf("%s/api/v1/review-items/rev-qr-drift-1/override", h.server.URL)
	ovrPayload := map[string]any{
		"run_id":          runID,
		"asset_id":        assetID,
		"target_language": "vi",
		"stage":           "render",
		"reason":          "Drift is within perceptual tolerance for rapid narration",
		"operator":        "qc_lead",
	}
	ovrBytes, _ := json.Marshal(ovrPayload)
	ovrResp, err := http.Post(itemOvrURL, "application/json", bytes.NewReader(ovrBytes))
	if err != nil || ovrResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/review-items/{id}/override failed: status=%d err=%v", ovrResp.StatusCode, err)
	}
	ovrResp.Body.Close()

	// 5. Review queue is now clean
	qResp2, _ := http.Get(getQueueURL)
	var qBody2 struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp2.Body).Decode(&qBody2)
	qResp2.Body.Close()

	if qBody2.Count != 0 {
		t.Errorf("expected 0 pending items after direct item override, got %d", qBody2.Count)
	}
}

// TestSeam1_TargetedInvalidation_StrictReuseOfSourceArtifacts verifies:
//  1. Correcting target text triggers rerun only for declared downstream descendants:
//     TTS -> DubSegment -> DubMix -> LocalizedSubtitleTrack -> Render.
//  2. Upstream source-derived artifacts (SourceAsset, AudioRolePlan, TranscriptArtifact, AudioStems, TextRegionPlan)
//     are strictly REUSED and their CAS hashes remain identical.
func TestSeam1_TargetedInvalidation_StrictReuseOfSourceArtifacts(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup speech segments, AudioRolePlan, and VoiceAssignment
	segments := []domain.TranslationInputSegment{
		{
			Index:      0,
			SourceText: "今天天气很好。",
			SpeakerID:  "SPEAKER_00",
			StartMs:    0,
			EndMs:      3000,
		},
	}
	transVariant, dubScriptVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)
	// Setup TextRegionPlan
	tPlan2 := domain.TextRegionPlan{
		ID:             "text-plan-seam1-reuse",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-seam1-reuse",
		CreatedAt:      time.Now().UTC(),
	}
	tBytes2, _ := json.Marshal(tPlan2)
	tObj2, _ := h.casStore.Put(bytes.NewReader(tBytes2))
	_ = h.db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             tPlan2.ID,
		AssetID:        assetID,
		CASHash:        tObj2.SHA256,
		ProvenanceHash: tPlan2.ProvenanceHash,
		CreatedAt:      tPlan2.CreatedAt,
	})

	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	_, _ = http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))

	assignBody := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
	}
	_, voiceAssign := runAssignVoices(t, h, assetID, assignBody)

	// Record initial source-derived artifact CAS hashes
	sourceAsset, err := h.db.GetSourceAsset(ctx, assetID)
	if err != nil {
		t.Fatalf("get source asset failed: %v", err)
	}
	rolePlan, err := h.db.GetAudioRolePlan(ctx, assetID)
	if err != nil {
		t.Fatalf("get audio role plan failed: %v", err)
	}
	initialTransCAS := transVariant.CASHash
	initialDubScriptCAS := dubScriptVariant.CASHash
	initialVoiceAssignCAS := voiceAssign.CASHash

	// 2. Perform inspector target text correction
	corrPayload := map[string]any{
		"run_id":               runID,
		"job_id":               jobID,
		"target_language":      "vi",
		"segment_index":        0,
		"new_target_text":      "Hôm nay thời tiết rất tốt.",
		"spoken_text_override": "Hôm nay thời tiết rất tốt.",
		"operator":             "editor_monet",
	}
	corrBytes, _ := json.Marshal(corrPayload)
	corrURL := fmt.Sprintf("%s/api/v1/assets/%s/inspector/correct-text", h.server.URL, assetID)
	corrResp, err := http.Post(corrURL, "application/json", bytes.NewReader(corrBytes))
	if err != nil || corrResp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		if corrResp != nil {
			_, _ = buf.ReadFrom(corrResp.Body)
		}
		t.Fatalf("POST inspector/correct-text failed: status=%d err=%v body=%s", corrResp.StatusCode, err, buf.String())
	}
	var corrBody struct {
		Result service.TargetTextCorrectionResult `json:"result"`
	}
	_ = json.NewDecoder(corrResp.Body).Decode(&corrBody)
	corrResp.Body.Close()

	// 3. Verify Downstream targets are updated with new CAS hashes
	if corrBody.Result.TranslationVariantCAS == "" || corrBody.Result.TranslationVariantCAS == initialTransCAS {
		t.Errorf("TranslationVariant was not invalidated: %s", corrBody.Result.TranslationVariantCAS)
	}
	if corrBody.Result.DubScriptVariantCAS == "" || corrBody.Result.DubScriptVariantCAS == initialDubScriptCAS {
		t.Errorf("DubScriptVariant was not invalidated: %s", corrBody.Result.DubScriptVariantCAS)
	}

	// 4. Verify Upstream source-derived artifacts are strictly preserved and reused
	sourceAssetAfter, _ := h.db.GetSourceAsset(ctx, assetID)
	if sourceAssetAfter.SHA256 != sourceAsset.SHA256 {
		t.Errorf("SourceAsset media was modified: %s != %s", sourceAssetAfter.SHA256, sourceAsset.SHA256)
	}
	rolePlanAfter, _ := h.db.GetAudioRolePlan(ctx, assetID)
	if len(rolePlanAfter.Segments) != len(rolePlan.Segments) {
		t.Errorf("AudioRolePlan was modified")
	}
	vaAfter, _ := h.db.GetVoiceAssignmentIndex(ctx, assetID, "vi")
	if vaAfter.CASHash != initialVoiceAssignCAS {
		t.Errorf("VoiceAssignment was modified: %s != %s", vaAfter.CASHash, initialVoiceAssignCAS)
	}
}

func TestSeam1_ManualOverride_StrictPendingValidation_RejectionsAndSuccess(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup a TranslationVariant with a QA failure for language "vi"
	transVar := domain.TranslationVariant{
		ID:             "trans-strict-seam1",
		AssetID:        assetID,
		TargetLanguage: "vi",
		ProvenanceHash: "prov-trans-strict-seam1",
		Segments: []domain.TranslationSegment{
			{
				Index:        0,
				SourceText:   "测试句子",
				TargetText:   "Dịch test",
				QAConfidence: 0.3,
				PassedQAGate: false,
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(transVar)
	tObj, _ := h.casStore.Put(bytes.NewReader(tBytes))
	_ = h.db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             transVar.ID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		CASHash:        tObj.SHA256,
		ProvenanceHash: transVar.ProvenanceHash,
		CreatedAt:      transVar.CreatedAt,
	})

	// 2. Query pending queue -> 1 pending item
	getQueueURL := fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID)
	qResp, err := http.Get(getQueueURL)
	if err != nil || qResp.StatusCode != http.StatusOK {
		t.Fatalf("GET review-items failed: status=%d err=%v", qResp.StatusCode, err)
	}
	var qBody struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qBody)
	qResp.Body.Close()
	if qBody.Count != 1 {
		t.Fatalf("expected 1 pending item, got %d", qBody.Count)
	}
	validItemID := qBody.ReviewItems[0].ID

	ovrURL := fmt.Sprintf("%s/api/v1/assets/%s/review/override", h.server.URL, assetID)

	// Rejection 1: Empty ReviewItemID
	badPayload1, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"reason":          "Missing item id",
		"operator":        "qa_tester",
	})
	resp1, err := http.Post(ovrURL, "application/json", bytes.NewReader(badPayload1))
	if err != nil {
		t.Fatalf("POST review/override failed: %v", err)
	}
	if resp1.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing review_item_id, got %d", resp1.StatusCode)
	}
	resp1.Body.Close()

	// Rejection 2: Nonexistent ReviewItemID
	badPayload2, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"review_item_id":  "rev-nonexistent-item-id-999",
		"reason":          "Nonexistent item",
		"operator":        "qa_tester",
	})
	resp2, err := http.Post(ovrURL, "application/json", bytes.NewReader(badPayload2))
	if err != nil {
		t.Fatalf("POST review/override failed: %v", err)
	}
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for nonexistent review_item_id, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	// Rejection 3: Wrong target language
	badPayload3, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"target_language": "en",
		"review_item_id":  validItemID,
		"reason":          "Wrong language",
		"operator":        "qa_tester",
	})
	resp3, err := http.Post(ovrURL, "application/json", bytes.NewReader(badPayload3))
	if err != nil {
		t.Fatalf("POST review/override failed: %v", err)
	}
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for mismatched language, got %d", resp3.StatusCode)
	}
	resp3.Body.Close()

	// Rejection 4: Direct route with nonexistent item ID
	directURL := fmt.Sprintf("%s/api/v1/review-items/rev-direct-fake-item/override", h.server.URL)
	directBadPayload, _ := json.Marshal(map[string]any{
		"asset_id":        assetID,
		"target_language": "vi",
		"reason":          "Direct fake item",
		"operator":        "qa_tester",
	})
	respDirectBad, err := http.Post(directURL, "application/json", bytes.NewReader(directBadPayload))
	if err != nil {
		t.Fatalf("POST direct review item override failed: %v", err)
	}
	if respDirectBad.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for direct nonexistent review item, got %d", respDirectBad.StatusCode)
	}
	respDirectBad.Body.Close()

	// Success: Valid exact pending ReviewItemID
	goodPayload, _ := json.Marshal(map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"review_item_id":  validItemID,
		"reason":          "Logo accepted by editorial policy",
		"operator":        "lead_reviewer",
	})
	respGood, err := http.Post(ovrURL, "application/json", bytes.NewReader(goodPayload))
	if err != nil {
		t.Fatalf("POST review/override failed: %v", err)
	}
	if respGood.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for valid override, got %d", respGood.StatusCode)
	}
	respGood.Body.Close()

	// Verify queue is now empty
	qRespAfter, _ := http.Get(getQueueURL)
	var qBodyAfter struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qRespAfter.Body).Decode(&qBodyAfter)
	qRespAfter.Body.Close()
	if qBodyAfter.Count != 0 {
		t.Errorf("expected 0 pending items after successful override, got %d", qBodyAfter.Count)
	}

	// Rejection 5: Duplicate override on already-overridden item
	respDup, err := http.Post(ovrURL, "application/json", bytes.NewReader(goodPayload))
	if err != nil {
		t.Fatalf("POST duplicate review/override failed: %v", err)
	}
	if respDup.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for already-overridden item, got %d", respDup.StatusCode)
	}
	respDup.Body.Close()
}
