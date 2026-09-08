package seam1_test

import (
	"bytes"
	"context"
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

// TestSeam1_LocalizedVisualTrack_CompactSubtitles_And_Overrides verifies:
// 1. Deterministic in-place cover/overlay for semantic_text & instructional_ui_text
// 2. Instructional UI uses standard target-language terminology
// 3. Compact fit-content subtitle box hugs text and avoids protected UI areas
// 4. Direct-manipulation override invalidation and LocalizedSubtitleTrack persistence
func TestSeam1_LocalizedVisualTrack_CompactSubtitles_And_Overrides(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Detect text first
	detectBody, _ := json.Marshal(map[string]any{"run_id": runID})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID), "application/json", bytes.NewReader(detectBody))
	if err != nil {
		t.Fatalf("POST detect-text failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from detect-text, got %d", resp.StatusCode)
	}

	// 2. Generate LocalizedVisualTrack for VI
	visPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
	}
	visBody, _ := json.Marshal(visPayload)
	visResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(visBody))
	if err != nil {
		t.Fatalf("POST visual-track failed: %v", err)
	}
	defer visResp.Body.Close()
	if visResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from visual-track, got %d", visResp.StatusCode)
	}

	var visRes struct {
		Track domain.LocalizedVisualTrack `json:"localized_visual_track"`
	}
	if err := json.NewDecoder(visResp.Body).Decode(&visRes); err != nil {
		t.Fatalf("decode visual-track response failed: %v", err)
	}
	track1 := visRes.Track
	if track1.CASHash == "" {
		t.Error("expected non-empty CASHash for LocalizedVisualTrack")
	}
	if track1.SubtitleTrackCAS == "" {
		t.Error("expected non-empty SubtitleTrackCAS for LocalizedSubtitleTrack")
	}

	// Verify instructional_ui_text uses standard target terminology ("Xuất" for "导出")
	foundInstructionalUI := false
	foundSemantic := false
	for _, ov := range track1.Overlays {
		if ov.Role == domain.TextRoleInstructionalUIText {
			foundInstructionalUI = true
			if ov.LocalizedText != "Xuất" {
				t.Errorf("got instructional UI text %q, want 'Xuất'", ov.LocalizedText)
			}
			if !ov.IsCoverDefault {
				t.Errorf("expected IsCoverDefault=true (in-place cover)")
			}
		}
		if ov.Role == domain.TextRoleSemanticText {
			foundSemantic = true
			if ov.LocalizedText == "" {
				t.Errorf("expected localized text for semantic_text overlay")
			}
			if !ov.IsCoverDefault {
				t.Errorf("expected IsCoverDefault=true for semantic_text overlay")
			}
		}
	}
	if !foundInstructionalUI {
		t.Errorf("expected instructional_ui_text overlay in LocalizedVisualTrack")
	}
	if !foundSemantic {
		t.Errorf("expected semantic_text overlay in LocalizedVisualTrack")
	}

	// Verify computed subtitle cues and non-occlusion against protected regions
	if len(track1.SubtitleCues) == 0 {
		t.Fatalf("expected subtitle cues in LocalizedVisualTrack")
	}
	if len(track1.ProtectedRegions) == 0 {
		t.Fatalf("expected protected regions in LocalizedVisualTrack")
	}
	for idx, cue := range track1.SubtitleCues {
		if cue.Width <= 0 || cue.Height <= 0 {
			t.Errorf("cue[%d] missing computed fit-content dimensions: width=%d, height=%d", idx, cue.Width, cue.Height)
		}
		cueBox := domain.BoundingBox{
			X:      cue.X,
			Y:      cue.Y,
			Width:  cue.Width,
			Height: cue.Height,
		}
		for pIdx, prot := range track1.ProtectedRegions {
			if domain.BoxesOverlap(cueBox, prot) {
				t.Errorf("cue[%d] overlaps protected region[%d]: cue=(%d,%d,w=%d,h=%d), prot=(%d,%d,w=%d,h=%d)",
					idx, pIdx, cue.X, cue.Y, cue.Width, cue.Height, prot.X, prot.Y, prot.Width, prot.Height)
			}
		}
		if cue.PaddingX < 18 || cue.PaddingX > 28 {
			t.Errorf("cue[%d] padding_x %d out of scale-aware bounds [18, 28]", idx, cue.PaddingX)
		}
		if cue.PaddingY < 10 || cue.PaddingY > 16 {
			t.Errorf("cue[%d] padding_y %d out of scale-aware bounds [10, 16]", idx, cue.PaddingY)
		}
	}
	// Verify GET endpoints for LocalizedVisualTrack and LocalizedSubtitleTrack
	getTrackResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/visual-track?target_language=vi", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("GET visual-track failed: %v", err)
	}
	defer getTrackResp.Body.Close()
	if getTrackResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from GET visual-track, got %d", getTrackResp.StatusCode)
	}

	getSubResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/subtitle-track?target_language=vi", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("GET subtitle-track failed: %v", err)
	}
	defer getSubResp.Body.Close()
	if getSubResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from GET subtitle-track, got %d", getSubResp.StatusCode)
	}

	var subRes struct {
		Track domain.LocalizedSubtitleTrack `json:"localized_subtitle_track"`
	}
	if err := json.NewDecoder(getSubResp.Body).Decode(&subRes); err != nil {
		t.Fatalf("decode subtitle-track response failed: %v", err)
	}
	if subRes.Track.CASHash != track1.SubtitleTrackCAS {
		t.Errorf("GET subtitle-track CASHash mismatch: %s vs %s", subRes.Track.CASHash, track1.SubtitleTrackCAS)
	}

	// 3. Test Direct Manipulation Overrides invalidating provenance
	if len(track1.Overlays) == 0 {
		t.Fatalf("expected at least one overlay in track1 to test override")
	}
	targetRegID := track1.Overlays[0].RegionID
	newRole := domain.TextRoleSemanticText
	newText := "Nút Mới Đã Chỉnh"
	overridePayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"overrides": []map[string]any{
			{
				"region_id":   targetRegID,
				"new_role":    &newRole,
				"new_text":    &newText,
				"box_delta_x": 50,
				"box_delta_y": 100,
			},
		},
	}
	overrideBody, _ := json.Marshal(overridePayload)
	overResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(overrideBody))
	if err != nil {
		t.Fatalf("POST visual-track with overrides failed: %v", err)
	}
	defer overResp.Body.Close()
	if overResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from visual-track with overrides, got %d", overResp.StatusCode)
	}

	var overRes struct {
		Track domain.LocalizedVisualTrack `json:"localized_visual_track"`
	}
	_ = json.NewDecoder(overResp.Body).Decode(&overRes)
	if overRes.Track.ProvenanceHash == track1.ProvenanceHash {
		t.Errorf("expected overrides to produce a distinct provenance hash")
	}
	// 4. Test unknown RegionOverride ID returns 400 Bad Request
	badOverridePayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"overrides": []map[string]any{
			{
				"region_id": "non_existent_region_id_999",
			},
		},
	}
	badOverrideBody, _ := json.Marshal(badOverridePayload)
	badOverResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(badOverrideBody))
	if err != nil {
		t.Fatalf("POST visual-track with bad overrides failed: %v", err)
	}
	defer badOverResp.Body.Close()
	if badOverResp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for unknown region_id override, got %d", badOverResp.StatusCode)
	}
}

// TestSeam1_LocalizedVisualTrack_NonOcclusion_And_SceneEvidence tests that the Seam 1 API:
// 1. Accepts optional scene-protected rectangle evidence (e.g. face/tap target) and avoids occlusion.
// 2. Fails closed with 422 Unprocessable Entity when no safe placement exists.
// 3. Fails closed with 422 Unprocessable Entity when an overlay occludes a protected area.
func TestSeam1_LocalizedVisualTrack_NonOcclusion_And_SceneEvidence(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Detect text
	detectBody, _ := json.Marshal(map[string]any{"run_id": runID})
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID), "application/json", bytes.NewReader(detectBody))
	if err != nil {
		t.Fatalf("POST detect-text failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from detect-text, got %d", resp.StatusCode)
	}

	// 2. Localize with scene-protected face evidence at subtitle location (Y=1440)
	faceBox := domain.BoundingBox{X: 100, Y: 1400, Width: 880, Height: 200}
	visPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"scene_protected_regions": []domain.SceneProtectedRegion{
			{
				Reason:  "face",
				StartMs: 0,
				EndMs:   5000,
				Box:     faceBox,
			},
		},
	}
	visBody, _ := json.Marshal(visPayload)
	visResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(visBody))
	if err != nil {
		t.Fatalf("POST visual-track failed: %v", err)
	}
	defer visResp.Body.Close()
	if visResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from visual-track with scene evidence, got %d", visResp.StatusCode)
	}

	var visRes struct {
		Track domain.LocalizedVisualTrack `json:"localized_visual_track"`
	}
	if err := json.NewDecoder(visResp.Body).Decode(&visRes); err != nil {
		t.Fatalf("decode visual-track response failed: %v", err)
	}
	track := visRes.Track
	for idx, cue := range track.SubtitleCues {
		cueBox := domain.BoundingBox{X: cue.X, Y: cue.Y, Width: cue.Width, Height: cue.Height}
		if domain.BoxesOverlap(cueBox, faceBox) {
			t.Errorf("cue[%d] overlaps scene-protected face: cue=%+v, face=%+v", idx, cueBox, faceBox)
		}
	}

	// 3. Test Full-Screen Protection -> 422 Unprocessable Entity
	blockAllPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"scene_protected_regions": []domain.SceneProtectedRegion{
			{
				Reason:  "full_screen_block",
				StartMs: 0,
				EndMs:   5000,
				Box:     domain.BoundingBox{X: 0, Y: 0, Width: 1080, Height: 1920},
			},
		},
	}
	blockAllBody, _ := json.Marshal(blockAllPayload)
	blockAllResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(blockAllBody))
	if err != nil {
		t.Fatalf("POST visual-track with block all failed: %v", err)
	}
	defer blockAllResp.Body.Close()
	if blockAllResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity when no safe placement exists, got %d", blockAllResp.StatusCode)
	}

	// 4. Test Overlay Occlusion Collision -> 422 Unprocessable Entity
	// Place protected area right over upper semantic text
	overlayBlockPayload := map[string]any{
		"run_id":          runID,
		"target_language": "vi",
		"scene_protected_regions": []domain.SceneProtectedRegion{
			{
				Reason:  "face",
				StartMs: 0,
				EndMs:   5000,
				Box:     domain.BoundingBox{X: 50, Y: 250, Width: 800, Height: 200},
			},
		},
	}
	overlayBlockBody, _ := json.Marshal(overlayBlockPayload)
	overlayBlockResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/visual-track", h.server.URL, assetID), "application/json", bytes.NewReader(overlayBlockBody))
	if err != nil {
		t.Fatalf("POST visual-track with overlay occlusion failed: %v", err)
	}
	defer overlayBlockResp.Body.Close()
	if overlayBlockResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity when overlay occludes protected region, got %d", overlayBlockResp.StatusCode)
	}
}

type mockSnapshotOCRProvider struct {
	*provider.FakeOCRProvider
	detSHA    string
	recSHA    string
	oriSHA    string
	runtimeID string
}

func (p *mockSnapshotOCRProvider) OCRSnapshotDigests() (detSHA, recSHA, oriSHA, runtimeIdentity string) {
	return p.detSHA, p.recSHA, p.oriSHA, p.runtimeID
}
func (p *mockSnapshotOCRProvider) DetectRegions(ctx context.Context, req provider.OCRRequest) (*provider.OCRResult, error) {
	res, err := p.FakeOCRProvider.DetectRegions(ctx, req)
	if err != nil {
		return nil, err
	}
	res.DetSnapshotDigest = p.detSHA
	res.RecSnapshotDigest = p.recSHA
	res.OriSnapshotDigest = p.oriSHA
	res.RuntimeIdentity = p.runtimeID
	return res, nil
}

func TestSeam1_TextRegionPlan_SnapshotProvenanceBindingAndCacheIsolation(t *testing.T) {
	h := setupHarness(t)

	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// Wrap fake OCR provider with mock snapshot digests
	mockProv := &mockSnapshotOCRProvider{
		FakeOCRProvider: provider.NewFakeOCRProvider("fake_paddle_ocr"),
		detSHA:          "det_sha_256_mock_11223344",
		recSHA:          "rec_sha_256_mock_55667788",
		oriSHA:          "ori_sha_256_mock_99aabbcc",
		runtimeID:       "paddleocr:3.7.0@b03f46425e8ff4442b268ce449e3eef758146cd4",
	}
	h.registry.Register(mockProv)

	// 1. First execution creates plan and binds snapshot provenance
	detectPayload := map[string]any{
		"run_id":               runID,
		"frame_sample_step_ms": 500,
	}
	body, _ := json.Marshal(detectPayload)
	resp, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST detect-text failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
	}

	var res struct {
		Plan domain.TextRegionPlan `json:"text_region_plan"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	plan := res.Plan
	if plan.DetSnapshotDigest != mockProv.detSHA {
		t.Errorf("got det digest %q, want %q", plan.DetSnapshotDigest, mockProv.detSHA)
	}
	if plan.RecSnapshotDigest != mockProv.recSHA {
		t.Errorf("got rec digest %q, want %q", plan.RecSnapshotDigest, mockProv.recSHA)
	}
	if plan.OriSnapshotDigest != mockProv.oriSHA {
		t.Errorf("got ori digest %q, want %q", plan.OriSnapshotDigest, mockProv.oriSHA)
	}
	if plan.RuntimeIdentity != mockProv.runtimeID {
		t.Errorf("got runtime identity %q, want %q", plan.RuntimeIdentity, mockProv.runtimeID)
	}
	if plan.ProvenanceHash == "" {
		t.Error("expected non-empty provenance hash")
	}

	// 2. Second execution hits CAS cache directly via provenance hash
	resp2, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST detect-text 2 failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from cached call, got %d", resp2.StatusCode)
	}

	var res2 struct {
		Plan domain.TextRegionPlan `json:"text_region_plan"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&res2); err != nil {
		t.Fatalf("decode cached response: %v", err)
	}

	if res2.Plan.CASHash != plan.CASHash {
		t.Fatalf("cached call did not return matching CAS hash: got %s, want %s", res2.Plan.CASHash, plan.CASHash)
	}
	if res2.Plan.ProvenanceHash != plan.ProvenanceHash {
		t.Fatalf("cached call did not return matching provenance hash: got %s, want %s", res2.Plan.ProvenanceHash, plan.ProvenanceHash)
	}

	// 3. Verify stage execution record in DB
	execs, err := h.db.ListStageExecutions(context.Background(), runID)
	if err != nil {
		t.Fatalf("list stage executions: %v", err)
	}
	var foundVisualText bool
	for _, e := range execs {
		if e.Stage == "visual_text" && e.Status == "succeeded" {
			foundVisualText = true
			if e.ArtifactSHA256 != plan.CASHash {
				t.Errorf("stage execution artifact %s != plan CAS hash %s", e.ArtifactSHA256, plan.CASHash)
			}
		}
	}
	if !foundVisualText {
		t.Errorf("visual_text stage execution not found in DB")
	}

	// 4. Mutation test: provider weights change -> cache must NOT hit old CAS
	mockProvChanged := &mockSnapshotOCRProvider{
		FakeOCRProvider: provider.NewFakeOCRProvider("fake_paddle_ocr"),
		detSHA:          "det_sha_256_mock_CHANGED_99999",
		recSHA:          mockProv.recSHA,
		oriSHA:          mockProv.oriSHA,
		runtimeID:       mockProv.runtimeID,
	}
	h.registry.Register(mockProvChanged)

	resp3, err := http.Post(
		fmt.Sprintf("%s/api/v1/assets/%s/detect-text", h.server.URL, assetID),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST detect-text 3 failed: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created from changed snapshot call, got %d", resp3.StatusCode)
	}
	var res3 struct {
		Plan domain.TextRegionPlan `json:"text_region_plan"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&res3); err != nil {
		t.Fatalf("decode changed response: %v", err)
	}
	if res3.Plan.ProvenanceHash == plan.ProvenanceHash {
		t.Fatalf("changed snapshot digest must produce different provenance hash, got identical %s", plan.ProvenanceHash)
	}
	if res3.Plan.DetSnapshotDigest != "det_sha_256_mock_CHANGED_99999" {
		t.Fatalf("expected changed det digest, got %s", res3.Plan.DetSnapshotDigest)
	}
}
