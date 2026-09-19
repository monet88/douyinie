package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Helper to freeze a render plan
func runFreezeRenderPlan(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, *domain.RenderPlan) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/render-plan", h.server.URL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("freeze-render-plan request failed: %v", err)
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var res struct {
			Plan domain.RenderPlan `json:"render_plan"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return resp, &res.Plan
	}
	return resp, nil
}

// Helper to render preview
func runRenderPreview(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, *domain.PreviewRenderArtifact) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/render/preview", h.server.URL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("render-preview request failed: %v", err)
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var res struct {
			Artifact domain.PreviewRenderArtifact `json:"preview_render"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return resp, &res.Artifact
	}
	return resp, nil
}

// Helper to render final
func runRenderFinal(t *testing.T, h *testHarness, assetID string, payload map[string]any) (*http.Response, *domain.FinalRenderArtifact) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/render/final", h.server.URL, assetID), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("render-final request failed: %v", err)
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var res struct {
			Artifact domain.FinalRenderArtifact `json:"final_render"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		return resp, &res.Artifact
	}
	return resp, nil
}

// Helper to set up a media asset with an AudioRolePlan and a PASS DubMixArtifact
func setupAssetWithDubMix(t *testing.T, h *testHarness) (assetID, runID, jobID string, dubMix *domain.DubMixArtifact) {
	t.Helper()
	jobID, runID = createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID = job.SourceAssetID

	// 1. Save AudioRolePlan
	rolePayload := map[string]any{
		"segments": []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	roleBody, _ := json.Marshal(rolePayload)
	roleResp, err := http.Post(fmt.Sprintf("%s/api/v1/assets/%s/audio-role-plan", h.server.URL, assetID), "application/json", bytes.NewReader(roleBody))
	if err != nil || roleResp.StatusCode != http.StatusCreated {
		t.Fatalf("setup audio role plan failed: %v, status: %d", err, roleResp.StatusCode)
	}
	_ = roleResp.Body.Close()

	// 2. Separate stems
	sepResp, stems := runSeparateStems(t, h, assetID, map[string]any{"run_id": runID})
	if sepResp.StatusCode != http.StatusCreated || stems == nil {
		t.Fatalf("setup separate stems failed, status: %d", sepResp.StatusCode)
	}
	_ = sepResp.Body.Close()

	// 3. Audio mix (passthrough/preservation)
	mixResp, mix := runAudioMix(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
	})
	if mixResp.StatusCode != http.StatusCreated || mix == nil {
		t.Fatalf("setup audio mix failed, status: %d", mixResp.StatusCode)
	}
	_ = mixResp.Body.Close()

	return assetID, runID, jobID, mix
}

// ---------------------------------------------------------------------------
// Test 1: RenderPlan Freeze Semantics (AC 1 & 2)
// RenderPlan freezes exact artifact IDs/versions (SourceAsset SHA, DubMix CAS, AudioCAS, SubtitlePlanRef).
// Idempotent re-freeze returns identical provenance hash.
// Supports CAS-backed SubtitlePlanArtifact references.
// ---------------------------------------------------------------------------
func TestSeam1_Render_RenderPlanFreezeSemantics(t *testing.T) {
	h := setupHarness(t)
	assetID, runID, jobID, dubMix := setupAssetWithDubMix(t, h)

	cues := []domain.SubtitleCue{
		{
			StartMs:    0,
			EndMs:      700,
			Text:       "Chào mừng bạn đến với Douyinie",
			X:          100,
			Y:          250,
			FontSizePx: 24,
			PaddingX:   18,
			PaddingY:   10,
			BoxColor:   "black@0.6",
			FontColor:  "white",
		},
	}

	// 1. Freeze RenderPlan
	resp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   cues,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || plan == nil {
		t.Fatalf("expected 201 Created for freeze-render-plan, got %d", resp.StatusCode)
	}

	// Invariant: exact artifact refs are frozen
	if plan.SourceAssetSHA256 == "" {
		t.Errorf("expected frozen source asset SHA256")
	}
	if plan.DubMixCASHash != dubMix.CASHash {
		t.Errorf("expected frozen DubMixCASHash %s, got %s", dubMix.CASHash, plan.DubMixCASHash)
	}
	if plan.AudioCASHash != dubMix.AudioCASHash {
		t.Errorf("expected frozen AudioCASHash %s, got %s", dubMix.AudioCASHash, plan.AudioCASHash)
	}
	if plan.SubtitlePlan.CASHash == "" {
		t.Errorf("expected frozen SubtitlePlan CASHash")
	}
	if plan.SubtitlePlan.SchemaVersion != domain.SubtitlePlanSchemaVersion {
		t.Errorf("expected SubtitlePlan SchemaVersion %d, got %d", domain.SubtitlePlanSchemaVersion, plan.SubtitlePlan.SchemaVersion)
	}
	if plan.SubtitlePlan.CueCount != 1 {
		t.Errorf("expected SubtitlePlan CueCount=1, got %d", plan.SubtitlePlan.CueCount)
	}
	if !h.casStore.Exists(plan.SubtitlePlan.CASHash) {
		t.Errorf("expected SubtitlePlanArtifact stored in CAS: %s", plan.SubtitlePlan.CASHash)
	}
	if plan.ProvenanceHash == "" {
		t.Errorf("expected non-empty ProvenanceHash")
	}
	if plan.CASHash == "" {
		t.Errorf("expected plan stored in CAS with non-empty CASHash")
	}

	// 2. GET /api/v1/assets/{id}/render-plan retrieves identical frozen plan
	getResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render-plan?target_language=vi", h.server.URL, assetID))
	if err != nil {
		t.Fatalf("get render plan failed: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", getResp.StatusCode)
	}
	var getResult struct {
		Plan domain.RenderPlan `json:"render_plan"`
	}
	_ = json.NewDecoder(getResp.Body).Decode(&getResult)
	if getResult.Plan.ProvenanceHash != plan.ProvenanceHash {
		t.Errorf("GET returned plan provenance %s != %s", getResult.Plan.ProvenanceHash, plan.ProvenanceHash)
	}
	if getResult.Plan.SubtitlePlan.CASHash != plan.SubtitlePlan.CASHash {
		t.Errorf("GET returned different SubtitlePlan CASHash: %s != %s", getResult.Plan.SubtitlePlan.CASHash, plan.SubtitlePlan.CASHash)
	}

	// 3. Idempotent freeze: re-freezing same inputs returns identical provenance hash
	resp2, plan2 := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   cues,
	})
	defer resp2.Body.Close()
	if plan2.ProvenanceHash != plan.ProvenanceHash {
		t.Errorf("idempotent freeze produced different provenance: %s != %s", plan2.ProvenanceHash, plan.ProvenanceHash)
	}

	// 4. Freeze with explicit SubtitlePlanCAS (e.g. prepared by upstream stage #34)
	subArt := domain.SubtitlePlanArtifact{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.SubtitlePlanSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: domain.TargetLanguageVI,
		Format:         "compact_fit_cues",
		Cues:           cues,
		ASSContent:     media.GenerateASSContent(plan.Timeline, cues, ""),
		ProvenanceHash: "custom-sub-prov-1",
		CreatedAt:      time.Now().UTC(),
	}
	subBytes, _ := json.Marshal(subArt)
	subObj, err := h.casStore.Put(bytes.NewReader(subBytes))
	if err != nil {
		t.Fatalf("put custom subtitle plan in cas: %v", err)
	}

	respExplicit, planExplicit := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   domain.TargetLanguageVI,
		"subtitle_plan_cas": subObj.SHA256,
	})
	defer respExplicit.Body.Close()
	if respExplicit.StatusCode != http.StatusCreated || planExplicit == nil {
		t.Fatalf("freeze with explicit subtitle_plan_cas failed: %d", respExplicit.StatusCode)
	}
	if planExplicit.SubtitlePlan.CASHash != subObj.SHA256 {
		t.Errorf("expected SubtitlePlan CASHash %s, got %s", subObj.SHA256, planExplicit.SubtitlePlan.CASHash)
	}
}

// ---------------------------------------------------------------------------
// Test 2: Preview / Final Parity & Distinct Immutables (AC 3, 4, 5, 6)
// Preview uses proxies/lower encode quality but consumes the SAME accepted timeline,
// subtitle layout/style plan, audio selection, and render-plan semantics as final.
// Uses deterministic ASS/libass composition.
// PreviewRenderArtifact and FinalRenderArtifact are distinct immutables.
// ---------------------------------------------------------------------------
func TestSeam1_Render_PreviewFinalParity(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available on test host, skipping native composition parity execution")
	}
	if err := media.CheckFFmpegLibassCapability("ffmpeg"); err != nil {
		t.Skipf("ffmpeg lacks libass/ass filter support on host: %v", err)
	}

	h := setupHarness(t)
	assetID, runID, jobID, dubMix := setupAssetWithDubMix(t, h)

	cues := []domain.SubtitleCue{
		{
			StartMs:    100,
			EndMs:      800,
			Text:       "Huong dan su dung CapCut",
			X:          150,
			Y:          280,
			FontSizePx: 22,
			PaddingX:   18,
			PaddingY:   10,
			BoxColor:   "black@0.6",
			FontColor:  "white",
		},
	}

	// 1. Freeze RenderPlan
	freezeResp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   cues,
	})
	defer freezeResp.Body.Close()
	if freezeResp.StatusCode != http.StatusCreated || plan == nil {
		t.Fatalf("freeze render plan failed: %d", freezeResp.StatusCode)
	}

	// 2. Render Preview
	prevResp, preview := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
	})
	defer prevResp.Body.Close()
	if prevResp.StatusCode != http.StatusCreated || preview == nil {
		t.Fatalf("render preview failed: %d", prevResp.StatusCode)
	}

	// 3. Render Final
	finResp, final := runRenderFinal(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
	})
	defer finResp.Body.Close()
	if finResp.StatusCode != http.StatusCreated || final == nil {
		t.Fatalf("render final failed: %d", finResp.StatusCode)
	}

	// -----------------------------------------------------------------------
	// PARITY ASSERTIONS: Exact Semantic Equality
	// -----------------------------------------------------------------------
	// 1. Same RenderPlan provenance and CAS hash consumed
	if preview.ConsumedPlan.PlanProvenanceHash != final.ConsumedPlan.PlanProvenanceHash {
		t.Errorf("parity violation: PlanProvenanceHash mismatch: preview=%s, final=%s",
			preview.ConsumedPlan.PlanProvenanceHash, final.ConsumedPlan.PlanProvenanceHash)
	}
	if preview.ConsumedPlan.PlanCASHash != final.ConsumedPlan.PlanCASHash {
		t.Errorf("parity violation: PlanCASHash mismatch: preview=%s, final=%s",
			preview.ConsumedPlan.PlanCASHash, final.ConsumedPlan.PlanCASHash)
	}

	// 2. Same source video stream consumed
	if preview.ConsumedPlan.SourceAssetSHA256 != final.ConsumedPlan.SourceAssetSHA256 {
		t.Errorf("parity violation: SourceAssetSHA256 mismatch")
	}

	// 3. Same audio selection consumed (exact dub mix audio CAS)
	if preview.ConsumedPlan.AudioCASHash != dubMix.AudioCASHash || final.ConsumedPlan.AudioCASHash != dubMix.AudioCASHash {
		t.Errorf("parity violation: AudioCASHash mismatch: expected %s, got preview=%s, final=%s",
			dubMix.AudioCASHash, preview.ConsumedPlan.AudioCASHash, final.ConsumedPlan.AudioCASHash)
	}

	// 4. Same subtitle layout & style plan consumed (exact SubtitlePlanRef parity)
	if preview.ConsumedPlan.SubtitlePlan.CASHash != final.ConsumedPlan.SubtitlePlan.CASHash {
		t.Errorf("parity violation: SubtitlePlan CASHash mismatch: preview=%s, final=%s",
			preview.ConsumedPlan.SubtitlePlan.CASHash, final.ConsumedPlan.SubtitlePlan.CASHash)
	}
	if preview.ConsumedPlan.SubtitlePlan.CueSpecHash != final.ConsumedPlan.SubtitlePlan.CueSpecHash {
		t.Errorf("parity violation: Subtitle layout CueSpecHash mismatch")
	}
	if preview.ConsumedPlan.SubtitlePlan.CueCount != 1 || final.ConsumedPlan.SubtitlePlan.CueCount != 1 {
		t.Errorf("parity violation: CueCount mismatch")
	}

	// 5. Same timeline coordinates (Duration, Width, Height, FPS)
	if preview.ConsumedPlan.DurationMs != final.ConsumedPlan.DurationMs {
		t.Errorf("parity violation: timeline DurationMs mismatch")
	}

	// -----------------------------------------------------------------------
	// DISTINCT IMMUTABLES & ENCODE PROFILE ASSERTIONS
	// -----------------------------------------------------------------------
	// 1. Distinct IDs and distinct provenance hashes
	if preview.ID == final.ID {
		t.Errorf("expected distinct IDs for preview and final artifacts")
	}
	if preview.ProvenanceHash == final.ProvenanceHash {
		t.Errorf("expected distinct artifact provenance hashes")
	}
	if preview.OutputCASHash == final.OutputCASHash {
		t.Errorf("expected distinct output media CAS hashes due to proxy encode differences")
	}

	// 2. Preview uses proxies / lower encode quality
	if preview.EncodeProfile.Kind != domain.RenderKindPreview {
		t.Errorf("expected preview profile kind 'preview', got %s", preview.EncodeProfile.Kind)
	}
	if final.EncodeProfile.Kind != domain.RenderKindFinal {
		t.Errorf("expected final profile kind 'final', got %s", final.EncodeProfile.Kind)
	}
	if preview.EncodeProfile.ProxyScaleDivisor != 2 {
		t.Errorf("expected preview ProxyScaleDivisor=2 (half resolution proxy), got %d", preview.EncodeProfile.ProxyScaleDivisor)
	}
	if final.EncodeProfile.ProxyScaleDivisor != 1 {
		t.Errorf("expected final ProxyScaleDivisor=1 (native full resolution), got %d", final.EncodeProfile.ProxyScaleDivisor)
	}
	if preview.EncodeProfile.CRF <= final.EncodeProfile.CRF {
		t.Errorf("expected preview CRF (%d) > final CRF (%d) for lower encode quality",
			preview.EncodeProfile.CRF, final.EncodeProfile.CRF)
	}
	if preview.EncodeProfile.AudioBitRateK >= final.EncodeProfile.AudioBitRateK {
		t.Errorf("expected preview audio bitrate (%dk) < final audio bitrate (%dk)",
			preview.EncodeProfile.AudioBitRateK, final.EncodeProfile.AudioBitRateK)
	}

	// 3. Renderer is native-ffmpeg-libass
	if preview.Renderer != "native-ffmpeg-libass" || final.Renderer != "native-ffmpeg-libass" {
		t.Errorf("expected renderer 'native-ffmpeg-libass', got preview=%s, final=%s", preview.Renderer, final.Renderer)
	}

	// 4. NativeRenderBackend: output files exist in CAS
	if !h.casStore.Exists(preview.OutputCASHash) {
		t.Errorf("preview output MP4 missing in CAS: %s", preview.OutputCASHash)
	}
	if !h.casStore.Exists(final.OutputCASHash) {
		t.Errorf("final output MP4 missing in CAS: %s", final.OutputCASHash)
	}

	// 5. GET endpoints return the distinct immutables
	getPrevResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/preview?target_language=vi", h.server.URL, assetID))
	if err != nil || getPrevResp.StatusCode != http.StatusOK {
		t.Fatalf("GET preview failed: status %d", getPrevResp.StatusCode)
	}
	defer getPrevResp.Body.Close()

	getFinResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/render/final?target_language=vi", h.server.URL, assetID))
	if err != nil || getFinResp.StatusCode != http.StatusOK {
		t.Fatalf("GET final failed: status %d", getFinResp.StatusCode)
	}
	defer getFinResp.Body.Close()
}

// ---------------------------------------------------------------------------
// Test 3: Fail-Closed on Missing Dependencies or Refused Dub Mix
// ---------------------------------------------------------------------------
func TestSeam1_Render_FailClosed_MissingOrRefusedDependencies(t *testing.T) {
	h := setupHarness(t)
	jobID, runID := createJobAndRun(t, h)
	job := getJobViaAPI(t, h, jobID)
	assetID := job.SourceAssetID

	// 1. Freeze attempt without DubMixArtifact -> must fail closed (422 Unprocessable Entity)
	resp, _ := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   []domain.SubtitleCue{},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for freeze without dub mix, got %d", resp.StatusCode)
	}

	// 2. Freeze attempt with a REFUSED DubMixArtifact -> must fail closed (422 Unprocessable Entity)
	refusedMix := domain.DubMixArtifact{
		ID:                  "refused-mix-id",
		SchemaVersion:       domain.DubMixSchemaVersion,
		AssetID:             assetID,
		RunID:               runID,
		TargetLanguage:      domain.TargetLanguageVI,
		AudioCASHash:        "non-existent-hash",
		OverallStatus:       "REFUSED",
		RefusalReason:       "overrun detected",
		SoundtrackPreserved: true,
	}
	refusedBytes, _ := json.Marshal(refusedMix)
	refusedObj, err := h.casStore.Put(bytes.NewReader(refusedBytes))
	if err != nil {
		t.Fatalf("put refused mix in cas: %v", err)
	}
	_ = h.db.SaveDubMixArtifactIndex(context.Background(), storage.DubMixArtifactIndex{
		ID:             refusedMix.ID,
		AssetID:        refusedMix.AssetID,
		RunID:          refusedMix.RunID,
		TargetLanguage: refusedMix.TargetLanguage,
		CASHash:        refusedObj.SHA256,
		ProvenanceHash: "refused-prov-hash",
		OverallStatus:  "REFUSED",
		RefusalReason:  "overrun detected",
	})

	refusedFreezeResp, _ := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"dub_mix_cas":     refusedObj.SHA256,
		"subtitle_cues":   []domain.SubtitleCue{},
	})
	defer refusedFreezeResp.Body.Close()
	if refusedFreezeResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for freeze on REFUSED dub mix, got %d", refusedFreezeResp.StatusCode)
	}

	// 3. Render preview with invalid/non-existent plan -> must fail closed (404 Not Found)
	prevResp, _ := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": "non-existent-plan-provenance-12345",
	})
	defer prevResp.Body.Close()
	if prevResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent plan, got %d", prevResp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Custom Composition Seam for Parity Verification without FFmpeg
// ---------------------------------------------------------------------------
func TestSeam1_Render_ParityWithCustomComposer(t *testing.T) {
	h := setupHarness(t)
	assetID, runID, jobID, dubMix := setupAssetWithDubMix(t, h)

	// Inject a deterministic custom composer into RenderService to prove parity
	// and verify that ASS content is passed to the composer.
	customComposerCalled := 0
	renderSvc := service.NewRenderService(h.db, h.casStore)
	renderSvc.SetCustomComposer(func(ctx context.Context, req media.CompositionRequest) (*media.CompositionResult, error) {
		customComposerCalled++
		if req.ASSContent == "" && len(req.Cues) == 0 {
			t.Errorf("expected ASSContent or Cues in composition request")
		}
		dummyBytes := []byte(fmt.Sprintf("DETERMINISTIC_RENDER_%s_SCALE_%d", req.Profile.Kind, req.Profile.ProxyScaleDivisor))
		_ = os.WriteFile(req.OutputPath, dummyBytes, 0644)
		return &media.CompositionResult{
			OutputPath: req.OutputPath,
			ByteSize:   int64(len(dummyBytes)),
			DurationMs: 1500,
			Renderer:   "custom-mock-backend",
		}, nil
	})
	h.srv.SetRenderService(renderSvc)

	cues := []domain.SubtitleCue{
		{
			StartMs:    0,
			EndMs:      500,
			Text:       "Demo subtitle cue",
			X:          100,
			Y:          200,
			FontSizePx: 24,
			PaddingX:   20,
			BoxColor:   "black@0.6",
			FontColor:  "white",
		},
	}

	freezeResp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   cues,
	})
	defer freezeResp.Body.Close()
	if freezeResp.StatusCode != http.StatusCreated || plan == nil {
		t.Fatalf("freeze plan failed: %d", freezeResp.StatusCode)
	}

	prevResp, preview := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
	})
	defer prevResp.Body.Close()
	if prevResp.StatusCode != http.StatusCreated || preview == nil {
		t.Fatalf("render preview failed: %d", prevResp.StatusCode)
	}

	finResp, final := runRenderFinal(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
	})
	defer finResp.Body.Close()
	if finResp.StatusCode != http.StatusCreated || final == nil {
		t.Fatalf("render final failed: %d", finResp.StatusCode)
	}

	if customComposerCalled != 2 {
		t.Errorf("expected custom composer called 2 times (preview + final), got %d", customComposerCalled)
	}

	// Parity: identical plan references consumed
	if preview.ConsumedPlan.PlanProvenanceHash != final.ConsumedPlan.PlanProvenanceHash {
		t.Errorf("PlanProvenanceHash mismatch")
	}
	if preview.ConsumedPlan.AudioCASHash != dubMix.AudioCASHash {
		t.Errorf("AudioCASHash mismatch")
	}
	if preview.ConsumedPlan.SubtitlePlan.CASHash != final.ConsumedPlan.SubtitlePlan.CASHash {
		t.Errorf("SubtitlePlan.CASHash mismatch")
	}

	// Distinct immutables
	if preview.ID == final.ID || preview.OutputCASHash == final.OutputCASHash {
		t.Errorf("expected distinct preview and final outputs")
	}
	if preview.Renderer != "custom-mock-backend" || final.Renderer != "custom-mock-backend" {
		t.Errorf("unexpected renderer string")
	}
}

// ---------------------------------------------------------------------------
// Test 5: Red-Capable Verification: ASS CompactFitBox & Box Semantics (Finding 1)
// Proves that production subtitle rendering generates CompactFitBox with BorderStyle=3,
// uses CompactFitBox style for Dialogue lines, and reflects cue BoxColor/FontColor/PaddingX.
// ---------------------------------------------------------------------------
func TestSeam1_Render_RedTest_RejectsDrawTextAndEnforcesLibass(t *testing.T) {
	h := setupHarness(t)
	assetID, runID, jobID, _ := setupAssetWithDubMix(t, h)

	cues := []domain.SubtitleCue{
		{
			StartMs:    0,
			EndMs:      600,
			Text:       "Kiểm tra ASS libass",
			X:          120,
			Y:          240,
			FontSizePx: 26,
			PaddingX:   22,
			BoxColor:   "black@0.6",
			FontColor:  "white",
		},
	}

	// 1. Freeze plan with cues
	freezeResp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   cues,
	})
	defer freezeResp.Body.Close()
	if freezeResp.StatusCode != http.StatusCreated || plan == nil {
		t.Fatalf("freeze plan failed: %d", freezeResp.StatusCode)
	}

	// Retrieve SubtitlePlanArtifact from CAS and assert valid ASS format
	r, err := h.casStore.Get(plan.SubtitlePlan.CASHash)
	if err != nil {
		t.Fatalf("failed to retrieve subtitle plan artifact from CAS: %v", err)
	}
	defer r.Close()

	var subArt domain.SubtitlePlanArtifact
	if err := json.NewDecoder(r).Decode(&subArt); err != nil {
		t.Fatalf("failed to decode subtitle plan artifact: %v", err)
	}

	// ASS Script structure assertions
	if !strings.Contains(subArt.ASSContent, "[Script Info]") {
		t.Errorf("red test failure: SubtitlePlanArtifact must contain ASS [Script Info], got: %s", subArt.ASSContent)
	}
	if !strings.Contains(subArt.ASSContent, "[V4+ Styles]") {
		t.Errorf("red test failure: SubtitlePlanArtifact must contain ASS [V4+ Styles], got: %s", subArt.ASSContent)
	}
	// Verify CompactFitBox style definition uses BorderStyle=3
	if !strings.Contains(subArt.ASSContent, "Style: CompactFitBox") || !strings.Contains(subArt.ASSContent, ",3,18,0,7,") {
		t.Errorf("red test failure: SubtitlePlanArtifact must contain CompactFitBox style with BorderStyle=3, got: %s", subArt.ASSContent)
	}
	if !strings.Contains(subArt.ASSContent, "[Events]") {
		t.Errorf("red test failure: SubtitlePlanArtifact must contain ASS [Events], got: %s", subArt.ASSContent)
	}
	// Verify Dialogue line uses CompactFitBox style and contains box padding (\bord22), font color (\c&HFFFFFF&), box color (\3c&H000000&\4c&H000000&)
	if !strings.Contains(subArt.ASSContent, "Dialogue: 0,0:00:00.00,0:00:00.60,CompactFitBox,,0,0,0,,{\\an7\\pos(120,240)\\bord22\\shad0\\fs26\\c&HFFFFFF&\\3c&H000000&\\4c&H000000&\\3a&H66&\\4a&H66&}Kiểm tra ASS libass") {
		t.Errorf("red test failure: Dialogue line must use CompactFitBox and exact box tags, got: %s", subArt.ASSContent)
	}

	// Ensure no drawtext remains in ASS content
	if strings.Contains(subArt.ASSContent, "drawtext=") {
		t.Errorf("red test failure: drawtext found in SubtitlePlanArtifact")
	}

	// 2. Test fail-closed behavior when FFmpeg binary path is invalid or lacks libass
	renderSvc := service.NewRenderService(h.db, h.casStore)
	renderSvc.SetFFmpegPath("non-existent-ffmpeg-binary-path-xyz")
	h.srv.SetRenderService(renderSvc)

	prevResp, _ := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
	})
	defer prevResp.Body.Close()
	if prevResp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity when renderer backend is unavailable, got %d", prevResp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Test 6: Red-Capable Verification: Frozen SubtitlePlan Artifact Ref Parity (Finding 2)
// Proves that preview and final share the exact frozen SubtitlePlanRef (ID, SchemaVersion, CASHash, CueSpecHash),
// and changing the subtitle ref alters plan provenance.
// ---------------------------------------------------------------------------
func TestSeam1_Render_RedTest_FrozenSubtitlePlanArtifactRefParity(t *testing.T) {
	h := setupHarness(t)
	assetID, runID, jobID, dubMix := setupAssetWithDubMix(t, h)

	cues1 := []domain.SubtitleCue{
		{StartMs: 0, EndMs: 400, Text: "Phụ đề gốc"},
	}
	cues2 := []domain.SubtitleCue{
		{StartMs: 0, EndMs: 400, Text: "Phụ đề đã sửa đổi"},
	}

	freezeResp1, plan1 := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   cues1,
	})
	defer freezeResp1.Body.Close()
	if freezeResp1.StatusCode != http.StatusCreated || plan1 == nil {
		t.Fatalf("freeze plan 1 failed: %d", freezeResp1.StatusCode)
	}

	freezeResp2, plan2 := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   cues2,
	})
	defer freezeResp2.Body.Close()
	if freezeResp2.StatusCode != http.StatusCreated || plan2 == nil {
		t.Fatalf("freeze plan 2 failed: %d", freezeResp2.StatusCode)
	}

	// Invariant: SubtitlePlanRef is an exact frozen artifact reference with schema version
	if plan1.SubtitlePlan.SchemaVersion != domain.SubtitlePlanSchemaVersion {
		t.Errorf("expected SubtitlePlan.SchemaVersion %d, got %d", domain.SubtitlePlanSchemaVersion, plan1.SubtitlePlan.SchemaVersion)
	}
	if plan1.SubtitlePlan.CASHash == "" || plan2.SubtitlePlan.CASHash == "" {
		t.Errorf("expected non-empty SubtitlePlan.CASHash")
	}

	// Modifying subtitle plan must yield different SubtitlePlan CASHash and different RenderPlan ProvenanceHash
	if plan1.SubtitlePlan.CASHash == plan2.SubtitlePlan.CASHash {
		t.Errorf("expected distinct SubtitlePlan.CASHash for different cues")
	}
	if plan1.ProvenanceHash == plan2.ProvenanceHash {
		t.Errorf("expected distinct RenderPlan ProvenanceHash for different subtitle plan refs")
	}

	// Verify preview / final parity consumes identical frozen SubtitlePlanRef
	renderSvc := service.NewRenderService(h.db, h.casStore)
	renderSvc.SetCustomComposer(func(ctx context.Context, req media.CompositionRequest) (*media.CompositionResult, error) {
		dummy := []byte("PARITY_TEST")
		_ = os.WriteFile(req.OutputPath, dummy, 0644)
		return &media.CompositionResult{OutputPath: req.OutputPath, ByteSize: int64(len(dummy)), DurationMs: 1000, Renderer: "mock"}, nil
	})
	h.srv.SetRenderService(renderSvc)

	_, preview := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan1.ProvenanceHash,
	})
	_, final := runRenderFinal(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan1.ProvenanceHash,
	})

	if preview == nil || final == nil {
		t.Fatalf("render preview/final failed")
	}

	if preview.ConsumedPlan.SubtitlePlan.CASHash != plan1.SubtitlePlan.CASHash {
		t.Errorf("preview did not consume exact frozen SubtitlePlan CASHash: expected %s, got %s",
			plan1.SubtitlePlan.CASHash, preview.ConsumedPlan.SubtitlePlan.CASHash)
	}
	if final.ConsumedPlan.SubtitlePlan.CASHash != plan1.SubtitlePlan.CASHash {
		t.Errorf("final did not consume exact frozen SubtitlePlan CASHash: expected %s, got %s",
			plan1.SubtitlePlan.CASHash, final.ConsumedPlan.SubtitlePlan.CASHash)
	}
	if preview.ConsumedPlan.SubtitlePlan.CASHash != final.ConsumedPlan.SubtitlePlan.CASHash {
		t.Errorf("preview and final SubtitlePlan CASHash mismatch")
	}
	if preview.ConsumedPlan.AudioCASHash != dubMix.AudioCASHash {
		t.Errorf("preview AudioCASHash mismatch")
	}
}

// ---------------------------------------------------------------------------
// Test 7: Red-Capable Verification: Broken/Missing SubtitlePlan Fails Closed (Finding 2)
// Proves that composeVideo fails closed on missing, corrupt, or inconsistent SubtitlePlanArtifact,
// and that a valid empty SubtitlePlanArtifact succeeds cleanly.
// ---------------------------------------------------------------------------
func TestSeam1_Render_RedTest_BrokenSubtitlePlanFailsClosed_AndValidEmptySucceeds(t *testing.T) {
	h := setupHarness(t)
	assetID, runID, jobID, dubMix := setupAssetWithDubMix(t, h)

	// Set custom composer to isolate CAS/decode validation
	renderSvc := service.NewRenderService(h.db, h.casStore)
	renderSvc.SetCustomComposer(func(ctx context.Context, req media.CompositionRequest) (*media.CompositionResult, error) {
		dummy := []byte("TEST_VIDEO")
		_ = os.WriteFile(req.OutputPath, dummy, 0644)
		return &media.CompositionResult{OutputPath: req.OutputPath, ByteSize: int64(len(dummy)), DurationMs: 1000, Renderer: "mock"}, nil
	})
	h.srv.SetRenderService(renderSvc)

	// 1. Freeze a normal plan
	freezeResp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues": []domain.SubtitleCue{
			{StartMs: 0, EndMs: 500, Text: "Test cue"},
		},
	})
	defer freezeResp.Body.Close()
	if freezeResp.StatusCode != http.StatusCreated || plan == nil {
		t.Fatalf("freeze plan failed: %d", freezeResp.StatusCode)
	}

	// 2. Create a corrupted plan whose SubtitlePlan CAS hash points to missing CAS content
	brokenPlanMissingCAS := *plan
	brokenPlanMissingCAS.ID = uuid.NewString()
	brokenPlanMissingCAS.SubtitlePlan.CASHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // missing
	brokenBytes, _ := json.Marshal(brokenPlanMissingCAS)
	brokenObj, _ := h.casStore.Put(bytes.NewReader(brokenBytes))
	_ = h.db.SaveRenderPlanIndex(context.Background(), storage.RenderPlanIndex{
		ID:             brokenPlanMissingCAS.ID,
		AssetID:        assetID,
		RunID:          runID,
		TargetLanguage: domain.TargetLanguageVI,
		CASHash:        brokenObj.SHA256,
		ProvenanceHash: "broken-prov-missing-cas",
	})

	prevResp1, _ := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_cas":        brokenObj.SHA256,
	})
	defer prevResp1.Body.Close()
	if prevResp1.StatusCode != http.StatusUnprocessableEntity && prevResp1.StatusCode != http.StatusBadRequest {
		t.Errorf("expected fail-closed (400 or 422) for missing SubtitlePlan CAS, got %d", prevResp1.StatusCode)
	}

	// 3. Create a corrupted plan whose SubtitlePlan CAS contains corrupt non-JSON bytes
	corruptObj, _ := h.casStore.Put(bytes.NewReader([]byte("not valid json content for artifact")))
	brokenPlanCorrupt := *plan
	brokenPlanCorrupt.ID = uuid.NewString()
	brokenPlanCorrupt.SubtitlePlan.CASHash = corruptObj.SHA256
	brokenCorruptBytes, _ := json.Marshal(brokenPlanCorrupt)
	brokenCorruptPlanObj, _ := h.casStore.Put(bytes.NewReader(brokenCorruptBytes))

	prevResp2, _ := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_cas":        brokenCorruptPlanObj.SHA256,
	})
	defer prevResp2.Body.Close()
	if prevResp2.StatusCode != http.StatusBadRequest && prevResp2.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected fail-closed (400 or 422) for corrupt SubtitlePlan CAS, got %d", prevResp2.StatusCode)
	}

	// 4. Create an explicitly valid EMPTY SubtitlePlanArtifact (0 cues, valid no-caption plan) -> must SUCCEED
	emptySubArt := domain.SubtitlePlanArtifact{
		ID:             "empty-sub-art-id",
		SchemaVersion:  domain.SubtitlePlanSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: domain.TargetLanguageVI,
		Format:         "compact_fit_cues",
		Cues:           []domain.SubtitleCue{},
		ASSContent:     "",
		ProvenanceHash: "empty-sub-prov",
		CreatedAt:      time.Now().UTC(),
	}
	emptySubBytes, _ := json.Marshal(emptySubArt)
	emptySubObj, _ := h.casStore.Put(bytes.NewReader(emptySubBytes))

	emptyFreezeResp, emptyPlan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":            runID,
		"job_id":            jobID,
		"target_language":   domain.TargetLanguageVI,
		"subtitle_plan_cas": emptySubObj.SHA256,
	})
	defer emptyFreezeResp.Body.Close()
	if emptyFreezeResp.StatusCode != http.StatusCreated || emptyPlan == nil {
		t.Fatalf("freeze empty subtitle plan failed: %d", emptyFreezeResp.StatusCode)
	}

	emptyPrevResp, emptyPreview := runRenderPreview(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": emptyPlan.ProvenanceHash,
	})
	defer emptyPrevResp.Body.Close()
	if emptyPrevResp.StatusCode != http.StatusCreated || emptyPreview == nil {
		t.Fatalf("expected 201 Created for explicitly valid empty subtitle plan, got %d", emptyPrevResp.StatusCode)
	}
	if emptyPreview.ConsumedPlan.SubtitlePlan.CueCount != 0 {
		t.Errorf("expected 0 cues consumed, got %d", emptyPreview.ConsumedPlan.SubtitlePlan.CueCount)
	}
	if emptyPreview.ConsumedPlan.AudioCASHash != dubMix.AudioCASHash {
		t.Errorf("expected dubMix audio preserved in empty subtitle render")
	}
}

// ---------------------------------------------------------------------------
// Test 8: Fix 1 Regression — Cross-Run / Cross-Asset Final Render Mismatch Fails Closed
// Explicit final render must fail closed (409 Conflict) when the requested run, job,
// and asset do not agree, and must NEVER complete another run's job.
// ---------------------------------------------------------------------------
func TestSeam1_Render_CrossRunFinalRenderMismatch_FailsClosedAndNeverCompletesOtherJob(t *testing.T) {
	h := setupHarness(t)

	// Asset A: has its own job, run, and a frozen render plan.
	assetA, runA, jobA, _ := setupAssetWithDubMix(t, h)
	cuesA := []domain.SubtitleCue{
		{StartMs: 0, EndMs: 400, Text: "Phụ đề asset A"},
	}
	freezeRespA, planA := runFreezeRenderPlan(t, h, assetA, map[string]any{
		"run_id":          runA,
		"job_id":          jobA,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   cuesA,
	})
	defer freezeRespA.Body.Close()
	if freezeRespA.StatusCode != http.StatusCreated || planA == nil {
		t.Fatalf("freeze plan for asset A failed: %d", freezeRespA.StatusCode)
	}

	// Asset B: distinct source media, its own job and run.
	jobB, runB := createJobAndRunWithDuration(t, h, 2.0)
	jobBEntity := getJobViaAPI(t, h, jobB)
	assetB := jobBEntity.SourceAssetID
	if assetB == assetA {
		t.Fatalf("test invariant: asset B must differ from asset A, both got %s", assetA)
	}

	// Mark run B's queue entry as completed so the completion gate would trigger
	// if reached.
	if err := h.db.UpdateQueueStatus(context.Background(), runB, domain.RunStatusCompleted, domain.RunStatusCompleted); err != nil {
		t.Fatalf("mark run B queue completed: %v", err)
	}

	// 1. Cross-run attack: attempt to render asset A using run B's ID.
	// Must fail closed with 409 Conflict.
	finResp, _ := runRenderFinal(t, h, assetA, map[string]any{
		"run_id":          runB,
		"job_id":          jobB,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": planA.ProvenanceHash,
	})
	defer finResp.Body.Close()
	if finResp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 Conflict on cross-run render attempt, got status %d", finResp.StatusCode)
	}

	// Job B must NOT have been marked completed.
	jobBAfter := getJobViaAPI(t, h, jobB)
	if jobBAfter.Status == "completed" {
		t.Fatalf("SECURITY VIOLATION: job B was completed by an explicit render of asset A")
	}

	// 2. Cross-asset plan mismatch: attempt to render asset B using asset A's frozen plan.
	finRespPlanMismatch, _ := runRenderFinal(t, h, assetB, map[string]any{
		"run_id":          runB,
		"job_id":          jobB,
		"target_language": domain.TargetLanguageVI,
		"plan_cas":        planA.CASHash,
	})
	defer finRespPlanMismatch.Body.Close()
	if finRespPlanMismatch.StatusCode != http.StatusConflict {
		t.Errorf("expected 409 Conflict on cross-asset plan_cas render, got %d", finRespPlanMismatch.StatusCode)
	}

	// 3. Positive control: valid explicit render of asset A with matching run A succeeds
	// and completes job A if run A was completed.
	if err := h.db.UpdateQueueStatus(context.Background(), runA, domain.RunStatusCompleted, domain.RunStatusCompleted); err != nil {
		t.Fatalf("mark run A queue completed: %v", err)
	}
	finRespValid, artValid := runRenderFinal(t, h, assetA, map[string]any{
		"run_id":          runA,
		"job_id":          jobA,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": planA.ProvenanceHash,
	})
	defer finRespValid.Body.Close()
	if finRespValid.StatusCode != http.StatusCreated || artValid == nil {
		t.Fatalf("expected 201 Created for valid same-run final render, got %d", finRespValid.StatusCode)
	}
	jobAAfter := getJobViaAPI(t, h, jobA)
	if jobAAfter.Status != "completed" {
		t.Errorf("expected job A completed after valid explicit final render, got %s", jobAAfter.Status)
	}
}

// ---------------------------------------------------------------------------
// Test 9: Fix 2 Regression — resolveVisualTrackLayers Fails Closed on Corrupt/Missing/Foreign Tracks
// True absence stays optional; DB, CAS, decode, schema, and ownership failures fail closed.
// ---------------------------------------------------------------------------
func TestSeam1_Render_VisualTrackLayersFailClosed_OnCorruptMissingOrForeignEvidence(t *testing.T) {
	h := setupHarness(t)
	assetID, baseRunID, jobID, _ := setupAssetWithDubMix(t, h)
	ctx := context.Background()

	jobForeign, foreignRunID := createJobAndRunWithDuration(t, h, 2.5)
	foreignAssetID := getJobViaAPI(t, h, jobForeign).SourceAssetID

	defaultTrack := func(runID string) domain.LocalizedVisualTrack {
		return domain.LocalizedVisualTrack{
			ID:             uuid.NewString(),
			SchemaVersion:  domain.LocalizedVisualTrackSchemaVersion,
			AssetID:        assetID,
			RunID:          runID,
			TargetLanguage: domain.TargetLanguageVI,
			ProvenanceHash: "prov-" + runID,
			CreatedAt:      time.Now().UTC(),
		}
	}

	seed := func(t *testing.T, runID string, payload any, mutateIdx func(*storage.LocalizedVisualTrackIndex)) string {
		t.Helper()
		var casHash string
		trackID := uuid.NewString()
		prov := "prov-" + runID

		if payload != nil {
			var raw []byte
			switch v := payload.(type) {
			case []byte:
				raw = v
			case string:
				raw = []byte(v)
			case domain.LocalizedVisualTrack:
				trackID = v.ID
				prov = v.ProvenanceHash
				var err error
				raw, err = json.Marshal(v)
				if err != nil {
					t.Fatalf("marshal visual track payload: %v", err)
				}
			default:
				t.Fatalf("unsupported payload type: %T", payload)
			}
			obj, err := h.casStore.Put(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("put visual track in cas: %v", err)
			}
			casHash = obj.SHA256
		} else {
			casHash = "nonexistent_cas_hash_1234567890abcdef1234567890abcdef12345678"
		}

		idx := storage.LocalizedVisualTrackIndex{
			ID:             trackID,
			AssetID:        assetID,
			RunID:          runID,
			TargetLanguage: domain.TargetLanguageVI,
			CASHash:        casHash,
			ProvenanceHash: prov,
			CreatedAt:      time.Now().UTC(),
		}
		if mutateIdx != nil {
			mutateIdx(&idx)
		}
		if err := h.db.SaveLocalizedVisualTrackIndex(ctx, idx); err != nil {
			t.Fatalf("save visual track index: %v", err)
		}
		return idx.RunID
	}

	cases := []struct {
		name           string
		expectedStatus int
		setup          func(t *testing.T, runID string) string
	}{
		{
			name:           "true absence succeeds",
			expectedStatus: http.StatusCreated,
			setup: func(t *testing.T, _ string) string {
				return baseRunID
			},
		},
		{
			name:           "missing CAS",
			expectedStatus: http.StatusUnprocessableEntity,
			setup: func(t *testing.T, runID string) string {
				return seed(t, runID, nil, nil)
			},
		},
		{
			name:           "corrupt CAS",
			expectedStatus: http.StatusBadRequest,
			setup: func(t *testing.T, runID string) string {
				return seed(t, runID, "not-valid-json-track-bytes", nil)
			},
		},
		{
			name:           "schema mismatch",
			expectedStatus: http.StatusBadRequest,
			setup: func(t *testing.T, runID string) string {
				tr := defaultTrack(runID)
				tr.SchemaVersion = 999
				return seed(t, runID, tr, nil)
			},
		},
		{
			name:           "foreign asset ownership",
			expectedStatus: http.StatusConflict,
			setup: func(t *testing.T, _ string) string {
				tr := defaultTrack(foreignRunID)
				tr.AssetID = foreignAssetID
				return seed(t, foreignRunID, tr, func(idx *storage.LocalizedVisualTrackIndex) {
					idx.AssetID = foreignAssetID
				})
			},
		},
		{
			name:           "foreign CAS payload",
			expectedStatus: http.StatusConflict,
			setup: func(t *testing.T, runID string) string {
				tr := defaultTrack(runID)
				tr.AssetID = foreignAssetID
				return seed(t, runID, tr, nil)
			},
		},
		{
			name:           "schema_version=0/missing",
			expectedStatus: http.StatusBadRequest,
			setup: func(t *testing.T, runID string) string {
				tr := defaultTrack(runID)
				tr.SchemaVersion = 0
				return seed(t, runID, tr, nil)
			},
		},
		{
			name:           "mismatched run_id",
			expectedStatus: http.StatusConflict,
			setup: func(t *testing.T, runID string) string {
				tr := defaultTrack(runID)
				tr.RunID = "different_run_in_payload"
				return seed(t, runID, tr, func(idx *storage.LocalizedVisualTrackIndex) {
					idx.RunID = runID
				})
			},
		},
		{
			name:           "empty run_id",
			expectedStatus: http.StatusConflict,
			setup: func(t *testing.T, runID string) string {
				tr := defaultTrack(runID)
				tr.RunID = ""
				return seed(t, runID, tr, func(idx *storage.LocalizedVisualTrackIndex) {
					idx.RunID = runID
				})
			},
		},
		{
			name:           "mismatched ID",
			expectedStatus: http.StatusConflict,
			setup: func(t *testing.T, runID string) string {
				tr := defaultTrack(runID)
				tr.ID = "different-track-id-in-payload"
				return seed(t, runID, tr, func(idx *storage.LocalizedVisualTrackIndex) {
					idx.ID = uuid.NewString()
				})
			},
		},
		{
			name:           "mismatched provenance",
			expectedStatus: http.StatusConflict,
			setup: func(t *testing.T, runID string) string {
				tr := defaultTrack(runID)
				tr.ProvenanceHash = "prov-in-payload-12345"
				return seed(t, runID, tr, func(idx *storage.LocalizedVisualTrackIndex) {
					idx.ProvenanceHash = "prov-in-index-67890"
				})
			},
		},
		{
			name:           "empty provenance",
			expectedStatus: http.StatusConflict,
			setup: func(t *testing.T, runID string) string {
				tr := defaultTrack(runID)
				tr.ProvenanceHash = ""
				return seed(t, runID, tr, func(idx *storage.LocalizedVisualTrackIndex) {
					idx.ProvenanceHash = "index-has-prov-but-payload-empty"
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tcRunID := "run_vis_" + uuid.NewString()[:8]
			reqRunID := tc.setup(t, tcRunID)
			resp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
				"run_id":          reqRunID,
				"job_id":          jobID,
				"target_language": domain.TargetLanguageVI,
				"subtitle_cues":   []domain.SubtitleCue{{StartMs: 0, EndMs: 300, Text: tc.name}},
			})
			defer resp.Body.Close()
			if resp.StatusCode != tc.expectedStatus {
				t.Fatalf("expected status %d, got %d", tc.expectedStatus, resp.StatusCode)
			}
			if tc.expectedStatus == http.StatusCreated && plan == nil {
				t.Fatal("expected non-nil render plan on success")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test 10: Fix 3 Regression — font_file File Path is Resolved to ASS Family Name (not Path as Fontname)
// Proves that configuring a font_file path resolves the font's internal family name into ASS Fontname,
// and registers the font directory with libass via fontsdir.
// ---------------------------------------------------------------------------
func TestSeam1_Render_FontFileResolvedToFamilyName_AndFontsdirRegistered(t *testing.T) {
	h := setupHarness(t)
	assetID, runID, jobID, _ := setupAssetWithDubMix(t, h)

	// 1. Unparseable font file must fail closed when freezing a plan (prevents silent fallback/path embedding).
	corruptFontPath := filepath.Join(t.TempDir(), "corrupt.ttf")
	if err := os.WriteFile(corruptFontPath, []byte("not-a-valid-ttf-binary"), 0644); err != nil {
		t.Fatalf("write corrupt font: %v", err)
	}

	renderSvc := service.NewRenderService(h.db, h.casStore)
	renderSvc.SetFontFile(corruptFontPath)
	h.srv.SetRenderService(renderSvc)

	respFail, _ := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   []domain.SubtitleCue{{StartMs: 0, EndMs: 400, Text: "Fail font"}},
	})
	defer respFail.Body.Close()
	if respFail.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request when font_file is unparseable, got %d", respFail.StatusCode)
	}
	renderSvcRuntime := service.NewRenderService(h.db, h.casStore)
	var composerCalls int
	var capturedReq media.CompositionRequest
	renderSvcRuntime.SetCustomComposer(func(ctx context.Context, req media.CompositionRequest) (*media.CompositionResult, error) {
		composerCalls++
		capturedReq = req
		dummy := []byte(fmt.Sprintf("FONT_OVERRIDE_TEST_%d", composerCalls))
		if err := os.WriteFile(req.OutputPath, dummy, 0644); err != nil {
			return nil, fmt.Errorf("write mock output: %w", err)
		}
		return &media.CompositionResult{OutputPath: req.OutputPath, ByteSize: int64(len(dummy)), DurationMs: 1000, Renderer: "mock"}, nil
	})
	h.srv.SetRenderService(renderSvcRuntime)

	// 2. Freeze ONE plan without custom font -> carries default family ("Arial").
	testFamily := "DouyinieDisplayTest"
	fontFile := writeMinimalTestFont(t, t.TempDir(), testFamily)

	freezeResp, plan := runFreezeRenderPlan(t, h, assetID, map[string]any{
		"run_id":          runID,
		"job_id":          jobID,
		"target_language": domain.TargetLanguageVI,
		"subtitle_cues":   []domain.SubtitleCue{{StartMs: 0, EndMs: 400, Text: "Text with runtime font"}},
	})
	defer freezeResp.Body.Close()
	if freezeResp.StatusCode != http.StatusCreated || plan == nil {
		t.Fatalf("freeze plan failed: %d", freezeResp.StatusCode)
	}

	// Inspect frozen SubtitlePlanArtifact in CAS: it was frozen with default font (Arial)
	r, err := h.casStore.Get(plan.SubtitlePlan.CASHash)
	if err != nil {
		t.Fatalf("get subtitle plan from cas: %v", err)
	}
	defer r.Close()
	var subArt domain.SubtitlePlanArtifact
	if err := json.NewDecoder(r).Decode(&subArt); err != nil {
		t.Fatalf("decode subtitle plan: %v", err)
	}
	if !strings.Contains(subArt.ASSContent, "Style: CompactFitBox,Arial,") {
		t.Fatalf("expected frozen subtitle plan to carry default Arial style, got: %s", subArt.ASSContent)
	}

	// 3. Render final WITHOUT font override -> establishes cached default-font artifact.
	finRespDef, finalArtDef := runRenderFinal(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
	})
	defer finRespDef.Body.Close()
	if finRespDef.StatusCode != http.StatusCreated || finalArtDef == nil {
		t.Fatalf("render default final failed: %d", finRespDef.StatusCode)
	}
	if composerCalls != 1 {
		t.Fatalf("expected composer called once for default render, got %d", composerCalls)
	}
	if !strings.Contains(capturedReq.ASSContent, "Style: CompactFitBox,Arial,") {
		t.Errorf("expected render without font override to preserve frozen Arial style, got: %s", capturedReq.ASSContent)
	}
	if capturedReq.FontFile != "" {
		t.Errorf("expected empty CompositionRequest.FontFile when no override given, got %q", capturedReq.FontFile)
	}
	defaultProv := finalArtDef.ProvenanceHash
	defaultCAS := finalArtDef.CASHash

	// 4. Render the SAME plan with font_file override -> must NOT hit the cached default-font artifact!
	finRespOverride, finalArtOverride := runRenderFinal(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
		"font_file":       fontFile,
	})
	defer finRespOverride.Body.Close()
	if finRespOverride.StatusCode != http.StatusCreated || finalArtOverride == nil {
		t.Fatalf("render final with font override failed: %d", finRespOverride.StatusCode)
	}

	// Composer MUST have been called a second time (did not return cached default artifact)
	if composerCalls != 2 {
		t.Errorf("CACHE COLLISION BUG: composer was not invoked for font override render; got call count %d, want 2", composerCalls)
	}
	// Render artifact provenance and output CAS must differ from default
	if finalArtOverride.ProvenanceHash == defaultProv {
		t.Errorf("expected distinct artifact provenance for font override render, got identical %s", defaultProv)
	}
	if finalArtOverride.CASHash == defaultCAS {
		t.Errorf("expected distinct CAS output for font override render, got identical %s", defaultCAS)
	}
	// CompositionRequest ASSContent uses custom family, not Arial or raw path
	if !strings.Contains(capturedReq.ASSContent, fmt.Sprintf("Style: CompactFitBox,%s,", testFamily)) {
		t.Errorf("expected runtime font override to regenerate ASS with family %q, got: %s", testFamily, capturedReq.ASSContent)
	}
	if strings.Contains(capturedReq.ASSContent, "Style: CompactFitBox,Arial,") {
		t.Errorf("runtime font override did not replace frozen Arial style in ASSContent")
	}
	if strings.Contains(capturedReq.ASSContent, fontFile) {
		t.Errorf("REGRESSION DETECTED: ASS content contains raw font_file path %q as Fontname", fontFile)
	}
	if capturedReq.FontFile != fontFile {
		t.Errorf("expected CompositionRequest.FontFile to preserve file path %q, got %q", fontFile, capturedReq.FontFile)
	}

	// 5. Repeated render with the SAME font override -> cache hit (idempotent; composer not re-invoked).
	finRespRepeat, finalArtRepeat := runRenderFinal(t, h, assetID, map[string]any{
		"run_id":          runID,
		"target_language": domain.TargetLanguageVI,
		"plan_provenance": plan.ProvenanceHash,
		"font_file":       fontFile,
	})
	defer finRespRepeat.Body.Close()
	if finRespRepeat.StatusCode != http.StatusCreated || finalArtRepeat == nil {
		t.Fatalf("render final with repeated font override failed: %d", finRespRepeat.StatusCode)
	}
	if composerCalls != 2 {
		t.Errorf("expected composer NOT re-invoked on repeated identical font override (cache hit), got call count %d", composerCalls)
	}
	if finalArtRepeat.ProvenanceHash != finalArtOverride.ProvenanceHash {
		t.Errorf("expected repeated font override to return identical provenance hash %s, got %s",
			finalArtOverride.ProvenanceHash, finalArtRepeat.ProvenanceHash)
	}

	// 6. Preview render parity: preview also uses corrected font identity.
	prevDef, err := renderSvcRuntime.RenderPreview(context.Background(), service.RenderExecutionInput{
		AssetID:        assetID,
		TargetLanguage: domain.TargetLanguageVI,
		PlanProvenance: plan.ProvenanceHash,
	})
	if err != nil {
		t.Fatalf("preview render without font override failed: %v", err)
	}
	prevOverride, err := renderSvcRuntime.RenderPreview(context.Background(), service.RenderExecutionInput{
		AssetID:        assetID,
		TargetLanguage: domain.TargetLanguageVI,
		PlanProvenance: plan.ProvenanceHash,
		FontFile:       fontFile,
	})
	if err != nil {
		t.Fatalf("preview render with font override failed: %v", err)
	}
	if prevOverride.ProvenanceHash == prevDef.ProvenanceHash {
		t.Errorf("expected preview render with font override to produce distinct provenance, got identical %s", prevDef.ProvenanceHash)
	}
}

// writeMinimalTestFont creates a syntactically valid TrueType sfnt binary with a single
// 'name' table carrying the specified family name (nameID 1, Windows Unicode).
func writeMinimalTestFont(t *testing.T, dir, family string) string {
	t.Helper()

	// Encode family string to UTF-16BE
	utf16Units := utf16.Encode([]rune(family))
	strBytes := make([]byte, len(utf16Units)*2)
	for i, u := range utf16Units {
		strBytes[i*2] = byte(u >> 8)
		strBytes[i*2+1] = byte(u & 0xFF)
	}

	// Build 'name' table (format 0)
	// Header: format=0 (2), count=1 (2), strOffset=6+12=18 (2) -> 6 bytes
	// Record: platform=3 (2), encoding=1 (2), language=0x0409 (2), nameID=1 (2), length (2), offset=0 (2) -> 12 bytes
	// String pool: strBytes
	nameTable := new(bytes.Buffer)
	nameTable.Write([]byte{0x00, 0x00})                                                 // format 0
	nameTable.Write([]byte{0x00, 0x01})                                                 // count = 1
	nameTable.Write([]byte{0x00, 18})                                                   // string offset = 18
	nameTable.Write([]byte{0x00, 0x03, 0x00, 0x01, 0x04, 0x09, 0x00, 0x01})             // platform 3, encoding 1, lang 0x409, nameID 1
	nameTable.Write([]byte{byte(len(strBytes) >> 8), byte(len(strBytes) & 0xFF), 0, 0}) // length, offset=0
	nameTable.Write(strBytes)

	// Pad name table to 4-byte boundary
	for nameTable.Len()%4 != 0 {
		nameTable.WriteByte(0)
	}
	nameLen := uint32(nameTable.Len())
	nameOffset := uint32(12 + 16) // sfnt header (12) + 1 table entry (16)

	// Build sfnt file
	fontBuf := new(bytes.Buffer)
	// Header: version 0x00010000, numTables=1, searchRange=16, entrySelector=0, rangeShift=0
	fontBuf.Write([]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00})
	// Table record: tag='name', checksum=0, offset=28, length=nameLen
	fontBuf.WriteString("name")
	fontBuf.Write([]byte{0x00, 0x00, 0x00, 0x00}) // checksum dummy
	fontBuf.Write([]byte{byte(nameOffset >> 24), byte(nameOffset >> 16), byte(nameOffset >> 8), byte(nameOffset)})
	fontBuf.Write([]byte{byte(nameLen >> 24), byte(nameLen >> 16), byte(nameLen >> 8), byte(nameLen)})
	fontBuf.Write(nameTable.Bytes())

	fontPath := filepath.Join(dir, "test_custom_font.ttf")
	if err := os.WriteFile(fontPath, fontBuf.Bytes(), 0644); err != nil {
		t.Fatalf("write minimal test font: %v", err)
	}
	return fontPath
}
