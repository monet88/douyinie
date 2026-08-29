package seam1_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
)

// TestSeam1_TextRegionPlan_DeterministicClassification verifies that the RuntimeHost
// orchestrates OCR detection, multi-role classification, tracking, and confidence
// evidence persistence through the public HTTP API.
func TestSeam1_TextRegionPlan_DeterministicClassification(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Trigger text detection & tracking through the public API
	detectPayload := map[string]any{
		"run_id":               runID,
		"frame_sample_step_ms": 500,
	}
	body, _ := json.Marshal(detectPayload)
	detectResp, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST detect-text failed: %v", err)
	}
	defer detectResp.Body.Close()

	if detectResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from detect-text, got %d", detectResp.StatusCode)
	}

	var detectRes struct {
		Plan domain.TextRegionPlan `json:"text_region_plan"`
	}
	if err := json.NewDecoder(detectResp.Body).Decode(&detectRes); err != nil {
		t.Fatalf("decode detect response failed: %v", err)
	}

	plan := detectRes.Plan
	if plan.AssetID != assetID {
		t.Errorf("got asset_id %q, want %q", plan.AssetID, assetID)
	}
	if plan.ProvenanceHash == "" {
		t.Error("expected non-empty provenance hash")
	}
	if plan.CASHash == "" {
		t.Error("expected non-empty CAS hash")
	}

	// Verify all 5 first-class text roles are classified per TextRegionPlan
	rolesFound := make(map[domain.TextRegionRole]bool)
	for _, reg := range plan.Regions {
		rolesFound[reg.Role] = true
	}

	if !rolesFound[domain.TextRoleBrandKeep] {
		t.Errorf("expected brand_keep role in plan, found: %+v", rolesFound)
	}
	if !rolesFound[domain.TextRoleInstructionalUIText] {
		t.Errorf("expected instructional_ui_text role in plan, found: %+v", rolesFound)
	}
	if !rolesFound[domain.TextRoleSpeechSubtitle] {
		t.Errorf("expected speech_subtitle role in plan, found: %+v", rolesFound)
	}
	if !rolesFound[domain.TextRoleSemanticText] {
		t.Errorf("expected semantic_text role in plan, found: %+v", rolesFound)
	}
	if !rolesFound[domain.TextRoleIgnoreNoise] {
		t.Errorf("expected ignore/noise role in plan, found: %+v", rolesFound)
	}

	// Verify protected metadata for instructional UI
	for _, reg := range plan.Regions {
		if reg.Role == domain.TextRoleInstructionalUIText {
			if !reg.ProtectedMetadata.IsProtected {
				t.Errorf("expected instructional_ui_text to have is_protected=true")
			}
			if reg.ProtectedMetadata.Reason == "" {
				t.Errorf("expected protected reason for instructional UI control")
			}
		}
	}

	// Verify tracking and linear interpolation of missing frame geometry
	for _, reg := range plan.Regions {
		if reg.Role == domain.TextRoleSpeechSubtitle {
			if len(reg.Keyframes) < 4 {
				t.Fatalf("expected at least 4 keyframes for subtitle, got %d", len(reg.Keyframes))
			}
			var frame2 *domain.RegionKeyframe
			for _, kf := range reg.Keyframes {
				if kf.FrameIndex == 2 {
					frame2 = &kf
					break
				}
			}
			if frame2 == nil {
				t.Fatalf("expected keyframe for frame 2 (missing in raw OCR)")
			}
			if frame2.Observed {
				t.Errorf("expected frame 2 keyframe to be marked Observed=false (interpolated)")
			}
			if reg.ConfidenceEvidence.InterpolatedFrames != 1 {
				t.Errorf("got %d interpolated frames, want 1", reg.ConfidenceEvidence.InterpolatedFrames)
			}
		}
	}

	// 2. Retrieve the plan through GET endpoint
	getResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/text-region-plan", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("GET text-region-plan failed: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from GET text-region-plan, got %d", getResp.StatusCode)
	}

	var getRes struct {
		Plan domain.TextRegionPlan `json:"text_region_plan"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&getRes); err != nil {
		t.Fatalf("decode get response failed: %v", err)
	}

	if getRes.Plan.ID != plan.ID {
		t.Errorf("GET returned plan ID %q, want %q", getRes.Plan.ID, plan.ID)
	}
	if getRes.Plan.ProvenanceHash != plan.ProvenanceHash {
		t.Errorf("GET returned provenance %q, want %q", getRes.Plan.ProvenanceHash, plan.ProvenanceHash)
	}
}

// TestSeam1_TextRegionPlan_LanguageReusability verifies that source-derived
// TextRegionPlan artifacts are reusable across VI and EN jobs without re-computing OCR.
func TestSeam1_TextRegionPlan_LanguageReusability(t *testing.T) {
	h := setupHarness(t)

	// Create VI job & run
	jobIDVI, runIDVI := createJobAndRun(t, h)
	jobVI := getJobViaAPI(t, h, jobIDVI)
	assetID := jobVI.SourceAssetID

	// First execution (e.g. for VI job)
	bodyVI, _ := json.Marshal(map[string]any{"run_id": runIDVI})
	resp1, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(bodyVI),
	)
	if err != nil {
		t.Fatalf("POST detect-text (VI) failed: %v", err)
	}
	defer resp1.Body.Close()

	var res1 struct {
		Plan domain.TextRegionPlan `json:"text_region_plan"`
	}
	_ = json.NewDecoder(resp1.Body).Decode(&res1)

	// Second execution (e.g. for EN job for same asset)
	runIDEN := "run_en_" + runIDVI
	bodyEN, _ := json.Marshal(map[string]any{"run_id": runIDEN})
	resp2, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(bodyEN),
	)
	if err != nil {
		t.Fatalf("POST detect-text (EN) failed: %v", err)
	}
	defer resp2.Body.Close()

	var res2 struct {
		Plan domain.TextRegionPlan `json:"text_region_plan"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&res2)

	// Provenance hash and CAS artifact must match exactly
	if res1.Plan.ProvenanceHash != res2.Plan.ProvenanceHash {
		t.Errorf("provenance hash mismatch between VI and EN runs: %s vs %s",
			res1.Plan.ProvenanceHash, res2.Plan.ProvenanceHash)
	}
	if res1.Plan.CASHash != res2.Plan.CASHash {
		t.Errorf("CAS hash mismatch between VI and EN runs: %s vs %s",
			res1.Plan.CASHash, res2.Plan.CASHash)
	}
}

// TestSeam1_TextRegionPlan_PolicyBlockedOCRNotRouted verifies that a policy-blocked
// OCR provider fails closed and cannot be routed.
func TestSeam1_TextRegionPlan_PolicyBlockedOCRNotRouted(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	policyReq := map[string]string{
		"policy_state": string(provider.PolicyBlocked),
		"reason":       "testing policy-before-health for OCR",
	}
	pBody, _ := json.Marshal(policyReq)
	pReq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/v1/policies/fake_paddle_ocr", h.server.URL), bytes.NewReader(pBody))
	pReq.Header.Set("Content-Type", "application/json")
	pResp, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatalf("update policy failed: %v", err)
	}
	defer pResp.Body.Close()
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from policy update, got %d", pResp.StatusCode)
	}

	// Attempt text detection: must fail closed because no allowed OCR provider exists
	detectPayload := map[string]any{
		"run_id": runID,
	}
	body, _ := json.Marshal(detectPayload)
	detectResp, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST detect-text failed: %v", err)
	}
	defer detectResp.Body.Close()

	if detectResp.StatusCode == http.StatusCreated {
		t.Fatalf("expected fail-closed error for blocked OCR provider, got 201 Created")
	}
}
