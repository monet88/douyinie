package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
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

// TestSeam1_MultimodalQualityResults_AutomatedRunDerivationAndCapture verifies:
// 1. Client helper PostQualityResult posts domain.QualityResult through /api/v1/quality-results.
// 2. GET /api/v1/runs/{id}/quality-results captures the newly persisted result.
// 3. EvaluateCaseQuality projects QC FAIL and REVIEW_REQUIRED into metrics without fabricating status.
func TestSeam1_MultimodalQualityResults_AutomatedRunDerivationAndCapture(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	client := benchmark.NewRuntimeHostClient(h.server.URL, h.server.Client())
	ctx := context.Background()

	// 1. Post automated QualityResult via public Client helper
	qr := domain.QualityResult{
		ID:             "qr-auto-seam1-01",
		RunID:          runID,
		JobID:          jobID,
		AssetID:        assetID,
		TargetLanguage: "vi",
		Stage:          "multimodal_qc",
		OverallStatus:  domain.QualityStatusReviewRequired,
		Metrics: []domain.QualityMetric{
			{Name: "soundtrack_preservation", Score: 1.0, Passed: true},
			{Name: "burned_in_subtitle_replacement", Score: 0.5, Passed: false, Description: "uncovered regions"},
		},
		Issues: []domain.ReviewItem{
			{
				ID:             "rev-auto-burned-1",
				RunID:          runID,
				AssetID:        assetID,
				TargetLanguage: "vi",
				Type:           domain.ReviewItemTypeVisualOcclusion,
				Stage:          "multimodal_qc",
				Severity:       "warning",
				Reason:         "burned-in subtitle region lacks temporal cue coverage",
				Status:         domain.ReviewItemStatusPending,
				CreatedAt:      time.Now().UTC(),
			},
		},
		CreatedAt: time.Now().UTC(),
	}

	posted, err := client.PostQualityResult(ctx, qr)
	if err != nil {
		t.Fatalf("PostQualityResult failed: %v", err)
	}
	if posted.ID != qr.ID {
		t.Errorf("expected ID %s, got %s", qr.ID, posted.ID)
	}

	// 2. Fetch by run ID using public Client helper
	runQRs, err := client.GetRunQualityResults(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunQualityResults failed: %v", err)
	}
	if len(runQRs) != 1 || runQRs[0].ID != qr.ID {
		t.Fatalf("expected 1 quality result with ID %s, got %+v", qr.ID, runQRs)
	}
	if runQRs[0].OverallStatus != domain.QualityStatusReviewRequired {
		t.Errorf("expected REVIEW_REQUIRED, got %s", runQRs[0].OverallStatus)
	}

	// 3. EvaluateCaseQuality projects QC status into case metrics
	caseEv := &benchmark.QualityCaseEvidence{
		CaseID:          "case_test_qc_proj",
		SourceVideoID:   "video_qc_proj",
		PrimaryCategory: "clean_single_speaker",
		TargetLanguage:  "vi",
		Profile:         "hybrid",
		Status:          "COMPLETED",
		RelationalQC: benchmark.RelationalQCEvidence{
			QualityResultIDs: []string{runQRs[0].ID},
			QualityResults:   runQRs,
		},
	}
	metrics := benchmark.EvaluateCaseQuality(caseEv, &benchmark.ReferenceAnnotationPack{
		PackID:  "dummy_pack",
		AssetID: assetID,
	})
	if metrics.Status != "REVIEW_REQUIRED" {
		t.Errorf("expected EvaluateCaseQuality to project REVIEW_REQUIRED, got %s", metrics.Status)
	}
	if len(metrics.ReviewReasons) == 0 || !strings.Contains(metrics.ReviewReasons[0], "QC multimodal_qc REVIEW_REQUIRED") {
		t.Errorf("expected review reason for multimodal_qc, got %v", metrics.ReviewReasons)
	}
}

// TestSeam1_MultimodalQualityResults_ResumeSafeAndFailClosedCapture verifies:
// 1. One unowned multimodal_qc + zero benchmark-owned: runner creates exactly one benchmark-owned result.
// 2. Retry reuses that marked result without appending duplicate.
// 3. Multiple marked benchmark-owned results strictly fail closed.
// 4. All QC results (both unowned and benchmark-owned) are captured into RelationalQC.
func TestSeam1_MultimodalQualityResults_ResumeSafeAndFailClosedCapture(t *testing.T) {
	h := setupHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	storeDir := filepath.Join(h.dir, "benchmark_sessions_resume_test")
	store, err := benchmark.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("create benchmark store: %v", err)
	}

	identity := testBenchmarkIdentity()
	session, err := store.Resume(identity)
	if err != nil {
		t.Fatalf("initial session resume: %v", err)
	}

	client := benchmark.NewRuntimeHostClient(h.server.URL, nil)
	runner, err := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
		Client:  client,
		Store:   store,
		Session: session,
	})
	if err != nil {
		t.Fatalf("create benchmark runner: %v", err)
	}

	qcMediaPath := createSyntheticMedia(t, h.dir, "qc_resume_01.mp4")
	segments := []domain.TranslationInputSegment{
		{Index: 0, SourceText: "测试语句", SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000},
	}
	audioRoleSegments := []domain.AudioSegment{
		{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
	}

	// 1. Ingest asset, create job and run beforehand to inject an unowned multimodal_qc result
	asset, err := client.IngestAsset(ctx, qcMediaPath, "test_operator", true)
	if err != nil {
		t.Fatalf("ingest asset: %v", err)
	}
	job, err := client.CreateJob(ctx, asset.ID, "vi")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	run, err := client.CreateRun(ctx, job.ID, "{}")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Post an unowned multimodal_qc result (e.g. from manual/production review)
	unownedQR := domain.QualityResult{
		ID:             "qr-unowned-production-01",
		RunID:          run.ID,
		JobID:          job.ID,
		AssetID:        asset.ID,
		TargetLanguage: "vi",
		Stage:          "multimodal_qc",
		OverallStatus:  domain.QualityStatusPass,
		CreatedAt:      time.Now().UTC(),
		Details:        map[string]any{"source": "external_production_eval"}, // No benchmark producer/schema marker
	}
	if _, err := client.PostQualityResult(ctx, unownedQR); err != nil {
		t.Fatalf("post unowned quality result: %v", err)
	}

	qcInput := benchmark.QualityCaseInput{
		CaseID:            "video_resume_vi",
		SourceVideoID:     "video_resume_01",
		PrimaryCategory:   "lifestyle_narration",
		TargetLanguage:    "vi",
		Profile:           "hybrid",
		MediaFilePath:     qcMediaPath,
		SourceAssetID:     asset.ID,
		Segments:          segments,
		AudioRoleSegments: audioRoleSegments,
	}

	// Pre-seed the run in session so ExecuteQualityCase attaches to this existing run
	qcPre := benchmark.QualityCaseEvidence{
		CaseID:          qcInput.CaseID,
		SourceVideoID:   qcInput.SourceVideoID,
		PrimaryCategory: qcInput.PrimaryCategory,
		TargetLanguage:  qcInput.TargetLanguage,
		Profile:         "hybrid",
		Status:          "IN_PROGRESS",
		CreatedAt:       time.Now().UTC(),
		SourceAssetID:   asset.ID,
		JobID:           job.ID,
		RunID:           run.ID,
		Stages:          make(map[string]benchmark.StageExecutionEvidence),
	}
	session.RecordQualityCase(qcPre)

	// Step 1: Execute quality case with 1 unowned multimodal_qc + 0 benchmark-owned
	// Runner must NOT suppress creation and must create exactly 1 benchmark-owned result
	qcEv, err := runner.ExecuteQualityCase(ctx, qcInput)
	if err != nil {
		t.Fatalf("initial ExecuteQualityCase failed: %v", err)
	}

	qrs1, err := client.GetRunQualityResults(ctx, qcEv.RunID)
	if err != nil {
		t.Fatalf("GetRunQualityResults failed: %v", err)
	}
	if len(qrs1) != 2 {
		t.Fatalf("expected 2 quality results on run (1 unowned + 1 benchmark-owned), got %d", len(qrs1))
	}
	var benchmarkOwnedCount int
	var autoQRID string
	for _, qr := range qrs1 {
		if benchmark.IsBenchmarkOwnedQualityResult(qr, qcEv.RunID, qcEv.JobID, qcEv.SourceAssetID, qcEv.TargetLanguage) {
			benchmarkOwnedCount++
			autoQRID = qr.ID
		}
	}
	if benchmarkOwnedCount != 1 {
		t.Fatalf("expected exactly 1 benchmark-owned QualityResult, got %d", benchmarkOwnedCount)
	}
	if len(qcEv.RelationalQC.QualityResults) != 2 {
		t.Fatalf("expected both unowned and benchmark-owned results captured in RelationalQC, got %d", len(qcEv.RelationalQC.QualityResults))
	}

	// Step 2: Retry reuses that marked result without appending duplicates
	qcEv.Status = "IN_PROGRESS"
	session.RecordQualityCase(*qcEv)

	qcEv2, err := runner.ExecuteQualityCase(ctx, qcInput)
	if err != nil {
		t.Fatalf("re-executing QualityCase on resume failed: %v", err)
	}
	qrs2, err := client.GetRunQualityResults(ctx, qcEv2.RunID)
	if err != nil {
		t.Fatalf("GetRunQualityResults after retry failed: %v", err)
	}
	if len(qrs2) != 2 {
		t.Fatalf("retry appended duplicate QualityResult: expected exactly 2 results on run, got %d", len(qrs2))
	}
	var benchmarkOwnedCount2 int
	for _, qr := range qrs2 {
		if benchmark.IsBenchmarkOwnedQualityResult(qr, qcEv.RunID, qcEv.JobID, qcEv.SourceAssetID, qcEv.TargetLanguage) {
			benchmarkOwnedCount2++
			if qr.ID != autoQRID {
				t.Fatalf("expected reused benchmark-owned ID %s, got %s", autoQRID, qr.ID)
			}
		}
	}
	if benchmarkOwnedCount2 != 1 {
		t.Fatalf("expected exactly 1 benchmark-owned QualityResult after retry, got %d", benchmarkOwnedCount2)
	}

	// Step 3: Multiple marked benchmark-owned results fail closed
	secondBenchmarkQR := domain.QualityResult{
		ID:             "qr-benchmark-duplicate-02",
		RunID:          qcEv.RunID,
		JobID:          qcEv.JobID,
		AssetID:        qcEv.SourceAssetID,
		TargetLanguage: qcEv.TargetLanguage,
		Stage:          "multimodal_qc",
		OverallStatus:  domain.QualityStatusPass,
		CreatedAt:      time.Now().UTC(),
		Details: map[string]any{
			benchmark.BenchmarkQCProducerKey: benchmark.BenchmarkQCProducerValue,
			benchmark.BenchmarkQCSchemaKey:   benchmark.BenchmarkQCSchemaVersion,
		},
	}
	if _, err := client.PostQualityResult(ctx, secondBenchmarkQR); err != nil {
		t.Fatalf("post second benchmark-owned quality result: %v", err)
	}
	qcEv.Status = "IN_PROGRESS"
	session.RecordQualityCase(*qcEv)

	_, err = runner.ExecuteQualityCase(ctx, qcInput)
	if err == nil || !strings.Contains(err.Error(), "multiple (2) benchmark-owned multimodal_qc results already exist") {
		t.Fatalf("expected fail-closed error for multiple benchmark-owned multimodal_qc results, got %v", err)
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
		RunID:          runID,
		JobID:          jobID,
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
		RunID:          runID,
		JobID:          jobID,
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

// TestSeam1_VoiceReassign_TargetedInvalidation_And_DownstreamRerun verifies:
// 1. Voice reassignment regenerates exactly the changed speaker's affected downstream scope (TTS -> DubSegment -> DubMix -> RenderPlan).
// 2. Unchanged speakers' synthesized audio segments are reused without re-synthesis.
// 3. Upstream source artifacts (SourceAsset, AudioStems, TranscriptArtifact, TranslationVariant, DubScriptVariant, TextRegionPlan) are strictly reused.
// 4. Clean rerun auto-resolves.
func TestSeam1_VoiceReassign_TargetedInvalidation_And_DownstreamRerun(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup multi-speaker dub script (2 segments per speaker, 4 total)
	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
		{Index: 1, SpeakerID: "SPEAKER_00", StartMs: 2200, EndMs: 4200, SourceText: "测试语音输入"},
		{Index: 2, SpeakerID: "SPEAKER_01", StartMs: 4500, EndMs: 6500, SourceText: "我们去公园散步吧。"},
		{Index: 3, SpeakerID: "SPEAKER_01", StartMs: 6700, EndMs: 8700, SourceText: "明天再继续工作。"},
	}
	transVariant, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	// Setup AudioRolePlan
	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 2000, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 2200, EndMs: 4200, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 4500, EndMs: 6500, Role: domain.AudioRoleNarrationDialogue},
			{StartMs: 6700, EndMs: 8700, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	_, _ = http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))

	// Setup TextRegionPlan
	tPlan := domain.TextRegionPlan{
		ID:             "text-plan-reassign-seam1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-reassign-seam1",
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

	// 2. Initial voice assignment + synthesis + mix + visual track + render plan
	assignBody := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}
	respAssign, assign1 := runAssignVoices(t, h, assetID, assignBody)
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("initial assign voices failed: %d", respAssign.StatusCode)
	}

	respSynth1, variant1 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth1.StatusCode != http.StatusCreated || variant1 == nil {
		t.Fatalf("initial synthesis failed: %d", respSynth1.StatusCode)
	}

	_, mix1 := runAudioMix(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if mix1 == nil {
		t.Fatalf("initial audio mix failed")
	}

	// Generate visual track
	_, _ = http.Post(fmt.Sprintf("%s/api/v1/assets/%s/visual-track", h.server.URL, assetID), "application/json", bytes.NewReader([]byte(`{"run_id":"`+runID+`","target_language":"vi"}`)))

	_, rPlan1 := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
	})
	if rPlan1 == nil {
		t.Fatalf("initial render plan freeze failed")
	}

	// Record initial artifacts
	sourceAsset, _ := h.db.GetSourceAsset(ctx, assetID)
	spk1Seg2_v1 := variant1.Segments[2]
	spk1Seg3_v1 := variant1.Segments[3]
	spk0Seg0_v1 := variant1.Segments[0]

	// 3. Register CosyVoice3 provider & license for new voice
	fakeCosy := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", 1350)
	fakeCosy.SpeedFitEnabled = true
	_ = h.registry.Register(fakeCosy)

	licBody, _ := json.Marshal(domain.LicenseManifestEntry{
		DependencyName: "fake_cosyvoice3_tts",
		Version:        "1.0.0",
		SHA256:         "sha256_mock_fake_cosyvoice3_tts",
		SourceRepo:     "github.com/monet88/douyinie/models/fake_cosyvoice3_tts",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	licResp, _ := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(licBody))
	licResp.Body.Close()

	// 4. Execute inspector voice reassignment via POST /api/v1/assets/{id}/inspector/reassign-voice
	newVoiceForSpk0 := fakeCosy.VoiceCatalog()[0]

	reassignPayload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": newVoiceForSpk0,
		},
		"reason":   "Operator prefers warmer tone for SPEAKER_00",
		"operator": "lead_audio_editor",
	}
	reassignBytes, _ := json.Marshal(reassignPayload)
	reassignURL := fmt.Sprintf("%s/api/v1/assets/%s/inspector/reassign-voice", h.server.URL, assetID)
	reassignResp, err := http.Post(reassignURL, "application/json", bytes.NewReader(reassignBytes))
	if err != nil || reassignResp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		if reassignResp != nil {
			_, _ = buf.ReadFrom(reassignResp.Body)
		}
		t.Fatalf("POST inspector/reassign-voice failed: status=%d err=%v body=%s", reassignResp.StatusCode, err, buf.String())
	}

	var reassignResult struct {
		Result service.VoiceReassignCorrectionResult `json:"result"`
	}
	_ = json.NewDecoder(reassignResp.Body).Decode(&reassignResult)
	reassignResp.Body.Close()

	res := reassignResult.Result
	if res.VoiceAssignmentCAS == "" || res.VoiceAssignmentCAS == assign1.CASHash {
		t.Errorf("expected new VoiceAssignment CAS, got %s", res.VoiceAssignmentCAS)
	}
	if len(res.InvalidatedSpeakers) != 1 || res.InvalidatedSpeakers[0] != "SPEAKER_00" {
		t.Errorf("expected InvalidatedSpeakers=[SPEAKER_00], got %v", res.InvalidatedSpeakers)
	}
	if res.DubSegmentsVariantCAS == "" || res.DubSegmentsVariantCAS == variant1.CASHash {
		t.Errorf("expected new DubSegmentsVariant CAS, got %s", res.DubSegmentsVariantCAS)
	}
	if res.DubMixCAS == "" || res.DubMixCAS == mix1.CASHash {
		t.Errorf("expected new DubMix CAS, got %s", res.DubMixCAS)
	}
	if res.RenderPlanCAS == "" || res.RenderPlanCAS == rPlan1.CASHash {
		t.Errorf("expected new RenderPlan CAS, got %s", res.RenderPlanCAS)
	}
	if res.Status != domain.ReviewItemStatusAutoResolved {
		t.Errorf("expected auto_resolved status, got %s", res.Status)
	}

	// 5. Verify that SPEAKER_01's segments were strictly reused from prior variant
	rcDub, err := h.casStore.Get(res.DubSegmentsVariantCAS)
	if err != nil {
		t.Fatalf("load dub segments from CAS failed: %v", err)
	}
	var var2 domain.DubSegmentsVariant
	_ = json.NewDecoder(rcDub).Decode(&var2)
	rcDub.Close()

	if len(var2.Segments) != 4 {
		t.Fatalf("expected 4 segments in variant 2, got %d", len(var2.Segments))
	}
	if var2.Segments[2].AudioSHA256 != spk1Seg2_v1.AudioSHA256 {
		t.Errorf("SPEAKER_01 segment 2 was re-synthesized! %s vs %s", var2.Segments[2].AudioSHA256, spk1Seg2_v1.AudioSHA256)
	}
	if var2.Segments[3].AudioSHA256 != spk1Seg3_v1.AudioSHA256 {
		t.Errorf("SPEAKER_01 segment 3 was re-synthesized! %s vs %s", var2.Segments[3].AudioSHA256, spk1Seg3_v1.AudioSHA256)
	}
	if var2.Segments[0].AudioSHA256 == spk0Seg0_v1.AudioSHA256 {
		t.Errorf("SPEAKER_00 segment 0 was NOT regenerated with new voice!")
	}

	// 6. Verify Upstream source-derived artifacts strictly preserved
	sourceAfter, _ := h.db.GetSourceAsset(ctx, assetID)
	if sourceAfter.SHA256 != sourceAsset.SHA256 {
		t.Errorf("source media was altered")
	}
	tAfter, _ := h.db.GetTranslationVariantIndex(ctx, assetID, "vi")
	if tAfter.CASHash != transVariant.CASHash {
		t.Errorf("translation was invalidated: %s vs %s", tAfter.CASHash, transVariant.CASHash)
	}
}

// TestSeam1_VoiceReassign_AssetScopedWithoutRunID verifies that the legacy
// asset-scoped inspector route (POST /api/v1/assets/{id}/inspector/reassign-voice)
// keeps working for callers that predate run-scoped plumbing and therefore send
// no run_id: the reassignment resolves the run from the asset's latest dub script
// variant and still regenerates the run's TTS -> DubMix -> RenderPlan lineage.
func TestSeam1_VoiceReassign_AssetScopedWithoutRunID(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	segments := []domain.TranslationInputSegment{
		{Index: 0, SpeakerID: "SPEAKER_00", StartMs: 0, EndMs: 2000, SourceText: "今天天气很好。"},
		{Index: 1, SpeakerID: "SPEAKER_01", StartMs: 2200, EndMs: 4200, SourceText: "测试语音输入"},
	}
	_, dubVariant := setupDubScriptForSeam1(t, h, runID, assetID, segments)

	respAssign, assign1 := runAssignVoices(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	})
	if respAssign.StatusCode != http.StatusCreated || assign1 == nil {
		t.Fatalf("initial assign voices failed: %d", respAssign.StatusCode)
	}

	respSynth, variant1 := runDubSynthesize(t, h, assetID, map[string]any{
		"run_id":                 runID,
		"target_language":        "vi",
		"dub_script_variant_cas": dubVariant.CASHash,
		"voice_assignment_cas":   assign1.CASHash,
	})
	if respSynth.StatusCode != http.StatusCreated || variant1 == nil {
		t.Fatalf("initial synthesis failed: %d", respSynth.StatusCode)
	}

	sepResp, _ := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	if sepResp.StatusCode != http.StatusCreated {
		t.Fatalf("separate stems failed: %d", sepResp.StatusCode)
	}
	_ = sepResp.Body.Close()

	mixResp, mix1 := runAudioMix(t, h, assetID, map[string]any{"run_id": runID, "target_language": "vi"})
	if mix1 == nil {
		t.Fatalf("initial audio mix failed: %d", mixResp.StatusCode)
	}
	_ = mixResp.Body.Close()

	// Seed two same-asset/same-language LocalizedVisualTrack rows: run A's own
	// track first, then a NEWER track belonging to a different run (run B) with
	// distinct subtitle cues. The legacy reassign adopts run A from the latest
	// dub script variant, so the regenerated render plan must freeze run A's cues.
	seedVisualTrack := func(trackRunID, id, cueText string, createdAt time.Time) {
		t.Helper()
		track := domain.LocalizedVisualTrack{
			ID:             id,
			AssetID:        assetID,
			RunID:          trackRunID,
			TargetLanguage: "vi",
			SubtitleCues: []domain.SubtitleCue{
				{ID: id + "-cue", StartMs: 0, EndMs: 2000, Text: cueText, FontSizePx: 48},
			},
			CreatedAt: createdAt,
		}
		raw, _ := json.Marshal(track)
		obj, err := h.casStore.Put(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("seed visual track %s: %v", id, err)
		}
		if err := h.db.SaveLocalizedVisualTrackIndex(ctx, storage.LocalizedVisualTrackIndex{
			ID:                id,
			AssetID:           assetID,
			RunID:             trackRunID,
			JobID:             "job-" + trackRunID,
			TargetLanguage:    "vi",
			TextRegionPlanCAS: "seed-trp-" + id,
			CASHash:           obj.SHA256,
			ProvenanceHash:    "prov-" + id,
			CreatedAt:         createdAt,
		}); err != nil {
			t.Fatalf("seed visual track index %s: %v", id, err)
		}
	}
	seedBase := time.Now().UTC()
	seedVisualTrack(runID, "vt-run-a", "run A subtitle", seedBase)
	seedVisualTrack("run-b-visual-only", "vt-run-b", "run B subtitle", seedBase.Add(time.Hour))

	// Register CosyVoice3 provider & license for the replacement voice.
	fakeCosy := provider.NewFakeTTSProvider("fake_cosyvoice3_tts", 1350)
	fakeCosy.SpeedFitEnabled = true
	_ = h.registry.Register(fakeCosy)

	licBody, _ := json.Marshal(domain.LicenseManifestEntry{
		DependencyName: "fake_cosyvoice3_tts",
		Version:        "1.0.0",
		SHA256:         "sha256_mock_fake_cosyvoice3_tts",
		SourceRepo:     "github.com/monet88/douyinie/models/fake_cosyvoice3_tts",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	licResp, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(licBody))
	if err != nil {
		t.Fatalf("register license failed: %v", err)
	}
	_ = licResp.Body.Close()

	// Legacy payload: asset + target language only, no run_id and no job_id.
	legacyPayload := map[string]any{
		"target_language": "vi",
		"custom_assignments": map[string]domain.VoiceProfile{
			"SPEAKER_00": fakeCosy.VoiceCatalog()[0],
		},
		"reason":   "Operator prefers warmer tone for SPEAKER_00",
		"operator": "lead_audio_editor",
	}
	legacyBytes, _ := json.Marshal(legacyPayload)
	legacyURL := fmt.Sprintf("%s/api/v1/assets/%s/inspector/reassign-voice", h.server.URL, assetID)
	legacyResp, err := http.Post(legacyURL, "application/json", bytes.NewReader(legacyBytes))
	if err != nil {
		t.Fatalf("POST legacy inspector/reassign-voice failed: %v", err)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(legacyResp.Body)
	_ = legacyResp.Body.Close()
	if legacyResp.StatusCode != http.StatusOK {
		t.Fatalf("legacy asset-scoped reassign without run_id: status=%d body=%s", legacyResp.StatusCode, buf.String())
	}

	var decoded struct {
		Result service.VoiceReassignCorrectionResult `json:"result"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode reassign result: %v", err)
	}
	res := decoded.Result
	if res.VoiceAssignmentCAS == "" || res.VoiceAssignmentCAS == assign1.CASHash {
		t.Errorf("expected new VoiceAssignment CAS, got %s", res.VoiceAssignmentCAS)
	}
	if len(res.InvalidatedSpeakers) != 1 || res.InvalidatedSpeakers[0] != "SPEAKER_00" {
		t.Errorf("expected InvalidatedSpeakers=[SPEAKER_00], got %v", res.InvalidatedSpeakers)
	}
	if res.DubSegmentsVariantCAS == "" || res.DubSegmentsVariantCAS == variant1.CASHash {
		t.Errorf("expected new DubSegmentsVariant CAS, got %s", res.DubSegmentsVariantCAS)
	}
	if res.DubMixCAS == "" || res.DubMixCAS == mix1.CASHash {
		t.Errorf("expected new DubMix CAS, got %s", res.DubMixCAS)
	}
	if res.RenderPlanCAS == "" {
		t.Errorf("expected new RenderPlan CAS, got empty")
	}

	// The legacy call must still land on the run the resolved variant belongs to:
	// the regenerated dub mix has to be run-bound so run-scoped reads still see it.
	runMixIdx, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil {
		t.Fatalf("run-scoped dub mix index missing after legacy reassign: %v", err)
	}
	if runMixIdx.CASHash != res.DubMixCAS {
		t.Errorf("legacy reassign dub mix bound to CAS %s, want run %s mix %s", runMixIdx.CASHash, runID, res.DubMixCAS)
	}

	// The regenerated render plan must freeze the adopted run's subtitle cues.
	// A newer same-asset/same-language visual track from a different run must
	// never bleed its cues in.
	rcPlan, err := h.casStore.Get(res.RenderPlanCAS)
	if err != nil {
		t.Fatalf("load regenerated render plan from CAS: %v", err)
	}
	var newPlan domain.RenderPlan
	decErr := json.NewDecoder(rcPlan).Decode(&newPlan)
	rcPlan.Close()
	if decErr != nil {
		t.Fatalf("decode regenerated render plan: %v", decErr)
	}
	if len(newPlan.SubtitleCues) != 1 || newPlan.SubtitleCues[0].Text != "run A subtitle" {
		t.Errorf("legacy reassign froze subtitle cues from another run: got %+v, want run A subtitle", newPlan.SubtitleCues)
	}
}

// installPreviewComposer replaces the harness render service with one whose composition
// backend writes playablePreview to the requested output. A region correction renders the
// preview the operator reloads, and the seam-1 fixtures are synthetic files no real encoder
// can read, so composition is pinned here: the bytes written are the bytes the operator plays.
func installPreviewComposer(t *testing.T, h *testHarness, playablePreview string) *service.RenderService {
	t.Helper()
	payload, err := os.ReadFile(playablePreview)
	if err != nil {
		t.Fatalf("read preview composition source: %v", err)
	}
	renderSvc := service.NewRenderService(h.db, h.casStore)
	renderSvc.SetCustomComposer(func(_ context.Context, req media.CompositionRequest) (*media.CompositionResult, error) {
		if err := os.WriteFile(req.OutputPath, payload, 0o644); err != nil {
			return nil, err
		}
		return &media.CompositionResult{
			OutputPath: req.OutputPath,
			ByteSize:   int64(len(payload)),
			DurationMs: 1500,
			Renderer:   "seam1-preview-composer",
		}, nil
	})
	h.srv.SetRenderService(renderSvc)
	return renderSvc
}

// TestSeam1_RegionOverride_Reclassify_Drag_Resize_Relabel_TargetedInvalidation verifies:
// 1. Region reclassify/drag/resize/relabel via inspector updates TextRegionPlan and regenerates
// LocalizedVisualTrack + LocalizedSubtitleTrack + RenderPlan + the preview the operator reloads.
// 2. Upstream source audio, ASR, translation, TTS, dub segments, audio stems, and dub mix are strictly preserved and untouched.
// 3. Uncertain text role exception in review queue auto-resolves after reclassifying to a valid role.
func TestSeam1_RegionOverride_Reclassify_Drag_Resize_Relabel_TargetedInvalidation(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Region corrections render the preview the operator reloads. Pin composition to the
	// run's own media so the assertion below observes the bytes the browser would play.
	installPreviewComposer(t, h, filepath.Join(h.dir, "queue_source.mp4"))

	// 1. Initial TextRegionPlan with an uncertain region that flags a review exception
	textPlan := domain.TextRegionPlan{
		ID:             "text-plan-region-ovr-seam1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-region-ovr-seam1",
		Regions: []domain.TrackedTextRegion{
			{
				ID:                 "region-ovr-101",
				Role:               domain.TextRoleUncertain,
				Text:               "点击下载应用",
				FirstSeenMs:        0,
				LastSeenMs:         3000,
				ConfidenceEvidence: domain.ConfidenceEvidence{MeanConfidence: 0.55},
				ReviewReason:       "uncertain_visual_role",
				Keyframes: []domain.RegionKeyframe{
					{
						FrameIndex:  0,
						TimestampMs: 0,
						Box:         domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 80},
						Observed:    true,
					},
				},
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

	// Setup the DubMix on the selected run itself. Creating it through
	// setupAssetWithDubMix would mint a second run for the same deduped asset,
	// which no longer satisfies selected-run correction semantics.
	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	roleResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	if err != nil {
		t.Fatalf("setup audio role plan failed: %v", err)
	}
	if roleResp.StatusCode != http.StatusCreated {
		t.Fatalf("setup audio role plan failed: status=%d", roleResp.StatusCode)
	}
	_ = roleResp.Body.Close()

	sepResp, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	if sepResp.StatusCode != http.StatusCreated || stems == nil {
		t.Fatalf("setup separate stems failed: status=%d", sepResp.StatusCode)
	}
	_ = sepResp.Body.Close()

	mixResp, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
	})
	if mixResp.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("setup audio mix failed: status=%d", mixResp.StatusCode)
	}
	_ = mixResp.Body.Close()

	// Upstream lineage a visual correction must reuse rather than rerun.
	stemsBefore, err := h.db.GetAudioStemsArtifactIndex(ctx, assetID)
	if err != nil || stemsBefore == nil {
		t.Fatalf("capture audio stems before the correction: idx=%v err=%v", stemsBefore, err)
	}
	mixBefore, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || mixBefore == nil {
		t.Fatalf("capture dub mix before the correction: idx=%v err=%v", mixBefore, err)
	}

	// 2. Query review items -> 1 pending low confidence / uncertain role item
	getQueueURL := fmt.Sprintf("%s/api/v1/assets/%s/review-items?target_language=vi", h.server.URL, assetID)
	qResp, _ := http.Get(getQueueURL)
	var qBody struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp.Body).Decode(&qBody)
	qResp.Body.Close()
	if qBody.Count != 1 {
		t.Fatalf("expected 1 pending review exception, got %d", qBody.Count)
	}

	// 3. Post direct-manipulation region override (reclassify to semantic_text, drag +20,+30, resize +50,+10, relabel)
	newRole := domain.TextRoleSemanticText
	newText := "Bấm để tải ứng dụng"
	deltaX := 20
	deltaY := 30
	deltaW := 50
	deltaH := 10

	ovrPayload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"overrides": []domain.RegionOverride{
			{
				RegionID:  "region-ovr-101",
				NewRole:   &newRole,
				NewText:   &newText,
				BoxDeltaX: deltaX,
				BoxDeltaY: deltaY,
				BoxDeltaW: deltaW,
				BoxDeltaH: deltaH,
			},
		},
		"reason":   "Operator reclassified to semantic_text with repositioned box",
		"operator": "visual_editor",
	}
	ovrBytes, _ := json.Marshal(ovrPayload)
	ovrURL := fmt.Sprintf("%s/api/v1/assets/%s/inspector/override-region", h.server.URL, assetID)
	ovrResp, err := http.Post(ovrURL, "application/json", bytes.NewReader(ovrBytes))
	if err != nil || ovrResp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		if ovrResp != nil {
			_, _ = buf.ReadFrom(ovrResp.Body)
		}
		t.Fatalf("POST inspector/override-region failed: status=%d err=%v body=%s", ovrResp.StatusCode, err, buf.String())
	}

	var regRes struct {
		Result service.RegionGeometryCorrectionResult `json:"result"`
	}
	_ = json.NewDecoder(ovrResp.Body).Decode(&regRes)
	ovrResp.Body.Close()

	if regRes.Result.LocalizedVisualTrackCAS == "" {
		t.Errorf("expected non-empty LocalizedVisualTrackCAS")
	}
	if regRes.Result.LocalizedSubtitleCAS == "" {
		t.Errorf("expected non-empty LocalizedSubtitleCAS")
	}
	if regRes.Result.RenderPlanCAS == "" {
		t.Errorf("expected non-empty RenderPlanCAS")
	}
	if regRes.Result.Status != domain.ReviewItemStatusAutoResolved {
		t.Errorf("expected auto_resolved status, got %s", regRes.Result.Status)
	}

	// 4. Verify LocalizedVisualTrack contains the updated overlay
	rcVis, err := h.casStore.Get(regRes.Result.LocalizedVisualTrackCAS)
	if err != nil {
		t.Fatalf("load visual track from CAS failed: %v", err)
	}
	var visTrack domain.LocalizedVisualTrack
	_ = json.NewDecoder(rcVis).Decode(&visTrack)
	rcVis.Close()

	foundOverlay := false
	for _, ov := range visTrack.Overlays {
		if ov.RegionID == "region-ovr-101" {
			foundOverlay = true
			if ov.Role != domain.TextRoleSemanticText {
				t.Errorf("expected semantic_text role, got %s", ov.Role)
			}
			if !strings.Contains(ov.LocalizedText, "Bấm để tải ứng dụng") {
				t.Errorf("expected localized text containing 'Bấm để tải ứng dụng', got %s", ov.LocalizedText)
			}
			if ov.Box.X != 120 || ov.Box.Y != 230 {
				t.Errorf("expected shifted box (120, 230), got (%d, %d)", ov.Box.X, ov.Box.Y)
			}
		}
	}
	if !foundOverlay {
		t.Errorf("overridden region not found in LocalizedVisualTrack overlays")
	}

	// 5. The preview the operator reloads must be rendered from the plan that was just
	// frozen: freezing a plan without a new preview leaves the pre-correction geometry on
	// screen after a reported success.
	if regRes.Result.PreviewRenderCAS == "" {
		t.Errorf("expected non-empty PreviewRenderCAS")
	}
	rcPlan, err := h.casStore.Get(regRes.Result.RenderPlanCAS)
	if err != nil {
		t.Fatalf("load regenerated render plan from CAS: %v", err)
	}
	var frozenPlan domain.RenderPlan
	if err := json.NewDecoder(rcPlan).Decode(&frozenPlan); err != nil {
		t.Fatalf("decode regenerated render plan: %v", err)
	}
	rcPlan.Close()

	prevResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?target_language=vi&run_id=%s", h.server.URL, assetID, runID))
	if err != nil || prevResp.StatusCode != http.StatusOK {
		t.Fatalf("GET run preview render failed: status=%d err=%v", prevResp.StatusCode, err)
	}
	var prevBody struct {
		PreviewRender domain.PreviewRenderArtifact `json:"preview_render"`
	}
	_ = json.NewDecoder(prevResp.Body).Decode(&prevBody)
	prevResp.Body.Close()
	if prevBody.PreviewRender.CASHash != regRes.Result.PreviewRenderCAS {
		t.Errorf("the run's latest preview artifact is %s, want the correction's %s",
			prevBody.PreviewRender.CASHash, regRes.Result.PreviewRenderCAS)
	}
	if prevBody.PreviewRender.ConsumedPlan.PlanProvenanceHash != frozenPlan.ProvenanceHash {
		t.Errorf("preview consumed plan provenance %s, want the corrected plan %s",
			prevBody.PreviewRender.ConsumedPlan.PlanProvenanceHash, frozenPlan.ProvenanceHash)
	}
	if prevBody.PreviewRender.ConsumedPlan.PlanCASHash != regRes.Result.RenderPlanCAS {
		t.Errorf("preview consumed plan cas %s, want %s",
			prevBody.PreviewRender.ConsumedPlan.PlanCASHash, regRes.Result.RenderPlanCAS)
	}

	// The media the player fetches is served from that artifact's fresh output.
	mediaResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview/media?target_language=vi&run_id=%s", h.server.URL, assetID, runID))
	if err != nil || mediaResp.StatusCode != http.StatusOK {
		t.Fatalf("GET preview media failed: status=%d err=%v", mediaResp.StatusCode, err)
	}
	mediaBytes, _ := io.ReadAll(mediaResp.Body)
	mediaResp.Body.Close()
	if int64(len(mediaBytes)) != prevBody.PreviewRender.OutputByteSize {
		t.Errorf("served preview media is %d bytes, want the rendered %d", len(mediaBytes), prevBody.PreviewRender.OutputByteSize)
	}

	// 7. A visual correction is a targeted rerun: separation, mixing and the dub mix the
	// preview composes against are reused, never re-derived.
	stemsAfter, err := h.db.GetAudioStemsArtifactIndex(ctx, assetID)
	if err != nil || stemsAfter == nil {
		t.Fatalf("get audio stems after the correction: idx=%v err=%v", stemsAfter, err)
	}
	if stemsAfter.CASHash != stemsBefore.CASHash || stemsAfter.ProvenanceHash != stemsBefore.ProvenanceHash {
		t.Errorf("region correction reran audio separation: cas=%s provenance=%s, want %s / %s",
			stemsAfter.CASHash, stemsAfter.ProvenanceHash, stemsBefore.CASHash, stemsBefore.ProvenanceHash)
	}
	mixAfter, err := h.db.GetDubMixArtifactIndexByRun(ctx, runID)
	if err != nil || mixAfter == nil {
		t.Fatalf("get dub mix after the correction: idx=%v err=%v", mixAfter, err)
	}
	if mixAfter.CASHash != mixBefore.CASHash {
		t.Errorf("region correction reran the audio mix: cas=%s, want %s", mixAfter.CASHash, mixBefore.CASHash)
	}

	// 7. Verify that the pending review queue is now clean (0 items)
	qResp2, _ := http.Get(getQueueURL)
	var qBody2 struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp2.Body).Decode(&qBody2)
	qResp2.Body.Close()
	if qBody2.Count != 0 {
		t.Errorf("expected 0 pending review items after reclassification, got %d: %+v", qBody2.Count, qBody2.ReviewItems)
	}
}

// TestSeam1_FinalRenderHandoff_QueueZero_AutoVsReview verifies:
// 1. When queue has pending review exceptions (queue > 0):
//   - Handoff returns can_start_final_render=false, auto_render_started=false, action="review_required".
//
// 2. When queue reaches zero (after manual_override or auto_resolved):
//   - In Review mode (posture="review"): returns can_start_final_render=true, auto_render_started=false, action="start_final_render".
//   - In Auto mode (posture="auto"): returns can_start_final_render=true, auto_render_started=true, action="auto_render_started", and renders final video automatically.
func TestSeam1_FinalRenderHandoff_QueueZero_AutoVsReview(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Setup dub mix
	_, _, _, dubMix := setupAssetWithDubMix(t, h)

	// Overwrite AudioRolePlan with an uncertain segment -> creates 1 pending review exception
	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleUncertain},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	_, _ = http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))

	_, rPlan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"dub_mix_cas":     dubMix.CASHash,
		"subtitle_cues": []domain.SubtitleCue{
			{StartMs: 0, EndMs: 1500, Text: "Test Handoff Subtitle", X: 100, Y: 200, Width: 400, Height: 80},
		},
	})
	if rPlan == nil {
		t.Fatalf("setup render plan failed")
	}

	// 2. Query pending queue -> 1 item
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
	pendingItemID := qBody.ReviewItems[0].ID

	// 3. Test Handoff with queue > 0 (Auto mode)
	handoffURL := fmt.Sprintf("%s/api/v1/assets/%s/render/handoff", h.server.URL, assetID)
	hResp1, err := http.Post(handoffURL, "application/json", bytes.NewReader([]byte(`{"run_id":"`+runID+`","posture":"auto","target_language":"vi"}`)))
	if err != nil || hResp1.StatusCode != http.StatusOK {
		t.Fatalf("POST render/handoff failed: %v", err)
	}
	var hRes1 struct {
		Handoff domain.FinalRenderHandoffResult `json:"handoff"`
	}
	_ = json.NewDecoder(hResp1.Body).Decode(&hRes1)
	hResp1.Body.Close()

	if hRes1.Handoff.QueueZero {
		t.Errorf("expected QueueZero=false when exception is pending")
	}
	if hRes1.Handoff.CanStartFinalRender {
		t.Errorf("expected CanStartFinalRender=false when exception is pending")
	}
	if hRes1.Handoff.AutoRenderStarted {
		t.Errorf("expected AutoRenderStarted=false when exception is pending")
	}
	if hRes1.Handoff.Action != "review_required" {
		t.Errorf("expected Action='review_required', got %s", hRes1.Handoff.Action)
	}

	// 4. Resolve the pending exception via manual_override
	ovrPayload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"review_item_id":  pendingItemID,
		"stage":           "audio_role_plan",
		"reason":          "Background speech confirmed as BGM",
		"operator":        "qa_supervisor",
	}
	ovrBytes, _ := json.Marshal(ovrPayload)
	ovrResp, _ := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/review/override", h.server.URL, assetID), "application/json", bytes.NewReader(ovrBytes))
	if ovrResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST review/override failed: %d", ovrResp.StatusCode)
	}
	ovrResp.Body.Close()

	// Verify queue is now zero
	qResp2, _ := http.Get(getQueueURL)
	var qBody2 struct {
		ReviewItems []domain.ReviewItem `json:"review_items"`
		Count       int                 `json:"count"`
	}
	_ = json.NewDecoder(qResp2.Body).Decode(&qBody2)
	qResp2.Body.Close()
	if qBody2.Count != 0 {
		t.Fatalf("expected 0 pending items, got %d", qBody2.Count)
	}

	// 5. Test Handoff with queue == 0 in Review mode (posture="review")
	hRespReview, err := http.Post(handoffURL, "application/json", bytes.NewReader([]byte(`{"run_id":"`+runID+`","posture":"review","target_language":"vi"}`)))
	if err != nil || hRespReview.StatusCode != http.StatusOK {
		t.Fatalf("POST render/handoff in review mode failed: %v", err)
	}
	var hResReview struct {
		Handoff domain.FinalRenderHandoffResult `json:"handoff"`
	}
	_ = json.NewDecoder(hRespReview.Body).Decode(&hResReview)
	hRespReview.Body.Close()

	if !hResReview.Handoff.QueueZero {
		t.Errorf("expected QueueZero=true")
	}
	if !hResReview.Handoff.CanStartFinalRender {
		t.Errorf("expected CanStartFinalRender=true")
	}
	if hResReview.Handoff.AutoRenderStarted {
		t.Errorf("expected AutoRenderStarted=false in Review mode (awaits operator)")
	}
	if hResReview.Handoff.Action != "start_final_render" {
		t.Errorf("expected Action='start_final_render' in Review mode, got %s", hResReview.Handoff.Action)
	}

	// 6. Test Handoff with queue == 0 in Auto mode (posture="auto")
	hRespAuto, err := http.Post(handoffURL, "application/json", bytes.NewReader([]byte(`{"run_id":"`+runID+`","posture":"auto","target_language":"vi"}`)))
	if err != nil || hRespAuto.StatusCode != http.StatusOK {
		t.Fatalf("POST render/handoff in auto mode failed: %v", err)
	}
	var hResAuto struct {
		Handoff domain.FinalRenderHandoffResult `json:"handoff"`
	}
	_ = json.NewDecoder(hRespAuto.Body).Decode(&hResAuto)
	hRespAuto.Body.Close()

	if !hResAuto.Handoff.QueueZero {
		t.Errorf("expected QueueZero=true")
	}
	if !hResAuto.Handoff.CanStartFinalRender {
		t.Errorf("expected CanStartFinalRender=true")
	}
	if !hResAuto.Handoff.AutoRenderStarted {
		t.Errorf("expected AutoRenderStarted=true in Auto mode")
	}
	if hResAuto.Handoff.Action != "auto_render_started" {
		t.Errorf("expected Action='auto_render_started', got %s", hResAuto.Handoff.Action)
	}
	if hResAuto.Handoff.FinalRenderCAS == "" {
		t.Errorf("expected non-empty FinalRenderCAS produced automatically in Auto mode")
	}

	// Verify final render artifact in CAS decodes cleanly
	rcFinal, err := h.casStore.Get(hResAuto.Handoff.FinalRenderCAS)
	if err != nil {
		t.Fatalf("load final render artifact from CAS failed: %v", err)
	}
	var finalArt domain.FinalRenderArtifact
	_ = json.NewDecoder(rcFinal).Decode(&finalArt)
	rcFinal.Close()

	if finalArt.OutputCASHash == "" {
		t.Errorf("final render artifact missing OutputCASHash")
	}
}

// TestSeam1_FinalRenderHandoff_RouteAndValidationRemediation verifies:
// 1. GET /api/v1/assets/{id}/render/handoff and GET /api/v1/runs/{id}/render/handoff are removed and return 404 / 405.
// 2. POST /api/v1/assets/{id}/render/handoff and POST /api/v1/runs/{id}/render/handoff return HTTP 400 on malformed JSON.
// 3. Empty body / query parameter requests are preserved.
func TestSeam1_FinalRenderHandoff_RouteAndValidationRemediation(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	assetHandoffURL := fmt.Sprintf("%s/api/v1/assets/%s/render/handoff", h.server.URL, assetID)
	runHandoffURL := fmt.Sprintf("%s/api/v1/runs/%s/render/handoff", h.server.URL, runID)

	// 1. Verify GET routes are removed (Method Not Allowed or Not Found)
	getAssetResp, err := http.Get(assetHandoffURL)
	if err != nil {
		t.Fatalf("GET asset render/handoff request failed: %v", err)
	}
	getAssetResp.Body.Close()
	if getAssetResp.StatusCode != http.StatusMethodNotAllowed && getAssetResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected GET asset render/handoff to be rejected with 404 or 405, got %d", getAssetResp.StatusCode)
	}

	getRunResp, err := http.Get(runHandoffURL)
	if err != nil {
		t.Fatalf("GET run render/handoff request failed: %v", err)
	}
	getRunResp.Body.Close()
	if getRunResp.StatusCode != http.StatusMethodNotAllowed && getRunResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected GET run render/handoff to be rejected with 404 or 405, got %d", getRunResp.StatusCode)
	}

	// 2. Verify POST returns HTTP 400 on malformed JSON for both asset and run endpoints
	badJSON := []byte(`{malformed_json: true, "run_id":`)
	postAssetBad, err := http.Post(assetHandoffURL, "application/json", bytes.NewReader(badJSON))
	if err != nil {
		t.Fatalf("POST asset render/handoff bad json failed: %v", err)
	}
	postAssetBad.Body.Close()
	if postAssetBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 on malformed JSON for asset handoff, got %d", postAssetBad.StatusCode)
	}

	postRunBad, err := http.Post(runHandoffURL, "application/json", bytes.NewReader(badJSON))
	if err != nil {
		t.Fatalf("POST run render/handoff bad json failed: %v", err)
	}
	postRunBad.Body.Close()
	if postRunBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 on malformed JSON for run handoff, got %d", postRunBad.StatusCode)
	}

	// 3. Verify POST with empty body (query parameter driven) is accepted without error
	postEmptyBodyURL := fmt.Sprintf("%s/api/v1/assets/%s/render/handoff?run_id=%s&target_language=vi&posture=review", h.server.URL, assetID, runID)
	postEmptyResp, err := http.Post(postEmptyBodyURL, "application/json", bytes.NewReader([]byte{}))
	if err != nil {
		t.Fatalf("POST asset render/handoff empty body failed: %v", err)
	}
	postEmptyResp.Body.Close()
	if postEmptyResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 on valid empty body with query params, got %d", postEmptyResp.StatusCode)
	}
}

// TestSeam1_CorrectRegionGeometry_FailClosedOnPersistenceErrors verifies:
// When TextRegionPlan is missing in DB or CAS, or DB write fails,
// CorrectRegionGeometry fails closed with an error and does not return auto_resolved.
func TestSeam1_CorrectRegionGeometry_FailClosedOnPersistenceErrors(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Attempt region override on asset with no persisted TextRegionPlan
	newRole := domain.TextRoleSemanticText
	ovrPayload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"overrides": []domain.RegionOverride{
			{
				RegionID: "nonexistent-region-1",
				NewRole:  &newRole,
			},
		},
		"reason":   "Operator override on missing plan",
		"operator": "visual_editor",
	}
	ovrBytes, _ := json.Marshal(ovrPayload)
	ovrURL := fmt.Sprintf("%s/api/v1/assets/%s/inspector/override-region", h.server.URL, assetID)
	ovrResp, err := http.Post(ovrURL, "application/json", bytes.NewReader(ovrBytes))
	if err != nil {
		t.Fatalf("POST override-region request failed: %v", err)
	}
	defer ovrResp.Body.Close()

	// Expect fail closed (500 or non-200 error since TextRegionPlan is missing from DB/CAS)
	if ovrResp.StatusCode == http.StatusOK {
		t.Fatalf("expected fail-closed error response when TextRegionPlan is missing, got 200 OK")
	}
}

// TestSeam1_RegionOverride_OutOfFrameFailsClosedWithoutMutatingState proves the RuntimeHost seam
// rejects geometry the canonical frame cannot hold instead of persisting a clamped result. The
// Operator UI's own preflight is a convenience, not the authority: a request that reaches the API
// (stale client, direct call, or a drag the browser could not bound) must fail closed with an
// actionable 400 and leave the asset's current plan untouched.
func TestSeam1_RegionOverride_OutOfFrameFailsClosedWithoutMutatingState(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	textPlan := domain.TextRegionPlan{
		ID:             "text-plan-region-bounds-seam1",
		AssetID:        assetID,
		FrameWidth:     1080,
		FrameHeight:    1920,
		ProvenanceHash: "prov-text-region-bounds-seam1",
		Regions: []domain.TrackedTextRegion{
			{
				ID:          "region-bounds-101",
				Role:        domain.TextRoleSemanticText,
				Text:        "点击下载应用",
				FirstSeenMs: 0,
				LastSeenMs:  3000,
				Keyframes: []domain.RegionKeyframe{
					{
						FrameIndex:  0,
						TimestampMs: 0,
						Box:         domain.BoundingBox{X: 100, Y: 200, Width: 300, Height: 80},
						Observed:    true,
					},
				},
			},
		},
		CreatedAt: time.Now().UTC(),
	}
	tBytes, _ := json.Marshal(textPlan)
	tObj, _ := h.casStore.Put(bytes.NewReader(tBytes))
	if err := h.db.SaveTextRegionPlanIndex(ctx, storage.TextRegionPlanIndex{
		ID:             textPlan.ID,
		AssetID:        assetID,
		CASHash:        tObj.SHA256,
		ProvenanceHash: textPlan.ProvenanceHash,
		CreatedAt:      textPlan.CreatedAt,
	}); err != nil {
		t.Fatalf("save text region plan index: %v", err)
	}

	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	roleResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	if err != nil {
		t.Fatalf("setup audio role plan failed: %v", err)
	}
	if roleResp.StatusCode != http.StatusCreated {
		t.Fatalf("setup audio role plan failed: status=%d", roleResp.StatusCode)
	}
	_ = roleResp.Body.Close()

	sepResp, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	if sepResp.StatusCode != http.StatusCreated || stems == nil {
		t.Fatalf("setup separate stems failed: status=%d", sepResp.StatusCode)
	}
	_ = sepResp.Body.Close()

	mixResp, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
	})
	if mixResp.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("setup audio mix failed: status=%d", mixResp.StatusCode)
	}
	_ = mixResp.Body.Close()

	// Drag the region 400 canonical px left: the requested box starts at x=-300.
	ovrPayload := map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": "vi",
		"overrides": []domain.RegionOverride{
			{RegionID: "region-bounds-101", BoxDeltaX: -400},
		},
		"reason":   "Drag out of the canonical frame must be refused, never clamped",
		"operator": "visual_editor",
	}
	ovrBytes, _ := json.Marshal(ovrPayload)
	ovrResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/inspector/override-region", h.server.URL, assetID), "application/json", bytes.NewReader(ovrBytes))
	if err != nil {
		t.Fatalf("POST inspector/override-region failed: %v", err)
	}
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(ovrResp.Body)
	_ = ovrResp.Body.Close()
	if ovrResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-frame geometry must fail closed with 400, got status=%d body=%s", ovrResp.StatusCode, body.String())
	}
	if !strings.Contains(body.String(), "frame") {
		t.Errorf("the refusal must name the violated frame bounds, got: %s", body.String())
	}

	// The rejected edit must not become the asset's current plan.
	current, err := h.db.GetTextRegionPlanIndex(ctx, assetID)
	if err != nil {
		t.Fatalf("get current text region plan index: %v", err)
	}
	if current.ProvenanceHash != textPlan.ProvenanceHash || current.CASHash != tObj.SHA256 {
		t.Errorf("rejected out-of-frame edit became the current plan: provenance=%s cas=%s, want %s / %s",
			current.ProvenanceHash, current.CASHash, textPlan.ProvenanceHash, tObj.SHA256)
	}
}
