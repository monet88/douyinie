package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fmt"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

func seedSeam1UniqueAssetAndJob(t *testing.T, db *storage.DB) (string, string) {
	t.Helper()
	ctx := context.Background()
	attID := uuid.NewString()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "seam1-tester",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})
	assetID := uuid.NewString()
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              "sha256-seam1-asset-" + assetID,
		ByteSize:            2048,
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateSourceAsset: %v", err)
	}
	jobID := "job-" + uuid.NewString()
	if err := db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "pending",
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return assetID, jobID
}

func seedSeam1TranscriptCAS(t *testing.T, h *customTranslationHarness, assetID, runID, sourceText string) string {
	t.Helper()
	ctx := context.Background()
	trJSON := fmt.Sprintf(`{"id":%q,"asset_id":%q,"run_id":%q,"speech_blocks":[{"index":0,"start_ms":0,"end_ms":1000,"source_text":%q,"speaker_id":"SPEAKER_00","segment_type":"speech"}]}`, "tr-"+runID, assetID, runID, sourceText)
	trObj, err := h.casStore.Put(bytes.NewReader([]byte(trJSON)))
	if err != nil {
		t.Fatalf("put transcript CAS: %v", err)
	}
	_ = h.db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID:             "tr-" + runID,
		AssetID:        assetID,
		RunID:          runID,
		CASHash:        trObj.SHA256,
		ProvenanceHash: "prov-tr-" + runID,
		CreatedAt:      time.Now().UTC(),
	})
	_ = h.db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             "se-tr-" + runID,
		RunID:          runID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: trObj.SHA256,
		CreatedAt:      time.Now().UTC(),
	})
	return trObj.SHA256
}

func postSeam1Translate(t *testing.T, baseURL, assetID string, payload map[string]any) (int, *domain.TranslationVariant, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	resp, err := http.Post(baseURL+"/api/v1/assets/"+assetID+"/translate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /translate: %v", err)
	}
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		var v domain.TranslationVariant
		if rawVar, ok := raw["translation_variant"]; ok {
			if err := json.Unmarshal(rawVar, &v); err != nil {
				t.Fatalf("unmarshal translation_variant: %v", err)
			}
		}
		return resp.StatusCode, &v, ""
	}
	var errMsg string
	if rawErr, ok := raw["error"]; ok {
		_ = json.Unmarshal(rawErr, &errMsg)
	}
	return resp.StatusCode, nil, errMsg
}

func writeGatewayJSON(w http.ResponseWriter, model, fingerprint, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":                 "chatcmpl-" + uuid.NewString(),
		"model":              model,
		"system_fingerprint": fingerprint,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
	})
}

// assertEvidenceCountsOnce proves the bounded subrequest counts are merged into the existing attempt
// evidence exactly once: a row keeps its own error text and never gets a duplicated counts phrase.
func assertEvidenceCountsOnce(t *testing.T, row domain.ProviderAttempt) {
	t.Helper()
	if got := strings.Count(row.ErrorMessage, "gateway wire calls"); got != 1 {
		t.Fatalf("attempt %s/%s must carry bounded subrequest counts exactly once, got %d in %q", row.ProviderID, row.Status, got, row.ErrorMessage)
	}
}

// frozenGlossaryPresent reports whether a gateway user payload carries the frozen request glossary
// as structured data: an effective_glossary entry with both source and target equal to "SUPOR".
// It decodes the payload so the assertion cannot be satisfied by the segment source_text echo.
func frozenGlossaryPresent(userContent string) bool {
	var payload struct {
		EffectiveGlossary []struct {
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"effective_glossary"`
	}
	if json.Unmarshal([]byte(userContent), &payload) != nil {
		return false
	}
	for _, entry := range payload.EffectiveGlossary {
		if entry.Source == "SUPOR" && entry.Target == "SUPOR" {
			return true
		}
	}
	return false
}

func TestSeam1_Issue152_AnchorValidation_InitialShuffledDuplicateExtraShiftedMissing(t *testing.T) {
	var geminiCalls int32
	var deepseekCalls int32
	var targetedRepairs int32
	var mode string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Model == provider.GatewayDeepSeekModelAlias {
			atomic.AddInt32(&deepseekCalls, 1)
			writeGatewayJSON(w, "deepseek/deepseek-v4.1-flash-001", "fp_ds_1", `{"segments":[
				{"index":10,"source_text":"第一句原话","target_text":"Câu một từ DeepSeek","key_facts":[],"negation_polarity":false},
				{"index":20,"source_text":"第二句原话","target_text":"Câu hai từ DeepSeek","key_facts":[],"negation_polarity":false},
				{"index":30,"source_text":"第三句原话","target_text":"Câu ba từ DeepSeek","key_facts":[],"negation_polarity":false}
			]}`)
			return
		}

		c := atomic.AddInt32(&geminiCalls, 1)
		userContent := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				userContent = m.Content
			}
		}
		switch mode {
		case "initial_shuffled_success":
			// Out-of-order [30, 10, 20] with NFKC/punctuation/space differences in source_text echo
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_ok", `{"segments":[
				{"index":30,"source_text":"  第三句，原话！ ","target_text":"Câu ba chuẩn","key_facts":["ba"],"negation_polarity":false},
				{"index":10,"source_text":"第一句原话...","target_text":"Câu một chuẩn","key_facts":["một"],"negation_polarity":false},
				{"index":20,"source_text":"第二句 原话","target_text":"Câu hai chuẩn","key_facts":["hai"],"negation_polarity":false}
			]}`)
		case "duplicate_repaired_by_whole_batch":
			if c == 1 {
				// Duplicate index 10
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_dup", `{"segments":[
					{"index":10,"source_text":"第一句原话","target_text":"Bản 1"},
					{"index":10,"source_text":"第一句原话","target_text":"Bản 2"},
					{"index":20,"source_text":"第二句原话","target_text":"Câu hai"},
					{"index":30,"source_text":"第三句原话","target_text":"Câu ba"}
				]}`)
				return
			}
			// Whole-batch repair succeeds
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_dup", `{"segments":[
				{"index":10,"source_text":"第一句原话","target_text":"Câu một đã sửa"},
				{"index":20,"source_text":"第二句原话","target_text":"Câu hai đã sửa"},
				{"index":30,"source_text":"第三句原话","target_text":"Câu ba đã sửa"}
			]}`)
		case "extra_and_shifted_persist_fallback":
			if c == 1 {
				// Shifted echo (index 10 echoes segment 20's source)
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_shift", `{"segments":[
					{"index":10,"source_text":"第二句原话","target_text":"Lệch câu hai"},
					{"index":20,"source_text":"第三句原话","target_text":"Lệch câu ba"},
					{"index":30,"source_text":"第一句原话","target_text":"Lệch câu một"}
				]}`)
				return
			}
			// Whole-batch repair still has extra index 99 -> must reject Gemini immediately (no targeted calls!) and fallback to DeepSeek
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_shift", `{"segments":[
				{"index":10,"source_text":"第一句原话","target_text":"Câu một"},
				{"index":20,"source_text":"第二句原话","target_text":"Câu hai"},
				{"index":30,"source_text":"第三句原话","target_text":"Câu ba"},
				{"index":99,"source_text":"多余","target_text":"Thừa"}
			]}`)
		case "missing_index_repaired_by_targeted":
			if c == 1 {
				// Index 20 is omitted entirely: one missing entry out of three is exactly 1/3
				// (not > 1/3), so the batch must go straight to a targeted repair of index 20.
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_miss", `{"segments":[
					{"index":10,"source_text":"第一句原话","target_text":"Câu một"},
					{"index":30,"source_text":"第三句原话","target_text":"Câu ba"}
				]}`)
				return
			}
			if strings.Contains(userContent, "read_only_context_neighbors") {
				atomic.AddInt32(&targetedRepairs, 1)
			}
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_miss", `{"segments":[
				{"index":20,"source_text":"第二句原话","target_text":"Câu hai bù"}
			]}`)
		case "missing_index_repair_exhausted":
			if c == 1 {
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_miss", `{"segments":[
					{"index":10,"source_text":"第一句原话","target_text":"Câu một"},
					{"index":30,"source_text":"第三句原话","target_text":"Câu ba"}
				]}`)
				return
			}
			// Targeted repair never returns the missing index 20, so every attempt is rejected and
			// the entry stays unrepaired: bounded repair must report exhaustion, never backfill.
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_gem_miss", `{"segments":[
				{"index":10,"source_text":"第一句原话","target_text":"Câu một"}
			]}`)
		}
	}))
	defer ts.Close()

	reg := provider.NewRegistry()
	mockSec := func(context.Context, string, string) (string, error) { return "sec-token", nil }
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-152",
		0.99,
		mockSec,
		ts.Client(),
		ts.URL,
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
		"baseline-deepseek-152",
		0.95,
		mockSec,
		ts.Client(),
		ts.URL,
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	h := setupCustomTranslationHarness(t, reg)
	canonicalInput := []map[string]any{
		{"index": 10, "source_text": "第一句原话", "speaker_id": "SPK_A", "start_ms": 100, "end_ms": 500},
		{"index": 20, "source_text": "第二句原话", "speaker_id": "SPK_B", "start_ms": 600, "end_ms": 1100},
		{"index": 30, "source_text": "第三句原话", "speaker_id": "SPK_A", "start_ms": 1200, "end_ms": 1800},
	}

	// Case 1: Shuffled valid output restores canonical order and preserves canonical source/speaker/timings
	mode = "initial_shuffled_success"
	atomic.StoreInt32(&geminiCalls, 0)
	atomic.StoreInt32(&deepseekCalls, 0)
	asset1, job1 := seedSeam1UniqueAssetAndJob(t, h.db)
	run1 := "run-shuffled-" + uuid.NewString()
	seedSeam1Run(t, h.db, asset1, job1, run1)

	status, v1, errMsg := postSeam1Translate(t, h.ts.URL, asset1, map[string]any{
		"run_id":                 run1,
		"job_id":                 job1,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               canonicalInput,
	})
	if status != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", status, errMsg)
	}
	if atomic.LoadInt32(&geminiCalls) != 1 || atomic.LoadInt32(&deepseekCalls) != 0 {
		t.Fatalf("expected 1 gemini call and 0 deepseek calls, got gemini=%d deepseek=%d", geminiCalls, deepseekCalls)
	}
	// Verify canonical order [10, 20, 30] and canonical SourceText/SpeakerID/StartMs/EndMs (not echoed punctuation)
	if len(v1.Segments) != 3 ||
		v1.Segments[0].Index != 10 || v1.Segments[0].SourceText != "第一句原话" || v1.Segments[0].SpeakerID != "SPK_A" || v1.Segments[0].StartMs != 100 || v1.Segments[0].EndMs != 500 || v1.Segments[0].TargetText != "Câu một chuẩn" ||
		v1.Segments[1].Index != 20 || v1.Segments[1].SourceText != "第二句原话" || v1.Segments[1].SpeakerID != "SPK_B" || v1.Segments[1].StartMs != 600 || v1.Segments[1].EndMs != 1100 || v1.Segments[1].TargetText != "Câu hai chuẩn" ||
		v1.Segments[2].Index != 30 || v1.Segments[2].SourceText != "第三句原话" || v1.Segments[2].SpeakerID != "SPK_A" || v1.Segments[2].StartMs != 1200 || v1.Segments[2].EndMs != 1800 || v1.Segments[2].TargetText != "Câu ba chuẩn" {
		t.Fatalf("shuffled output did not restore canonical order/metadata: %+v", v1.Segments)
	}

	// Case 2: Duplicate index on initial response triggers 1 whole-batch repair; 1 ProviderAttempt row
	mode = "duplicate_repaired_by_whole_batch"
	atomic.StoreInt32(&geminiCalls, 0)
	h2 := setupCustomTranslationHarness(t, reg)
	asset2, job2 := seedSeam1UniqueAssetAndJob(t, h2.db)
	run2 := "run-dup-" + uuid.NewString()
	seedSeam1Run(t, h2.db, asset2, job2, run2)

	status, v2, errMsg := postSeam1Translate(t, h2.ts.URL, asset2, map[string]any{
		"run_id":                 run2,
		"job_id":                 job2,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               canonicalInput,
	})
	if status != http.StatusCreated {
		t.Fatalf("expected 201 after whole-batch repair, got %d (%s)", status, errMsg)
	}
	if atomic.LoadInt32(&geminiCalls) != 2 {
		t.Fatalf("expected 2 gemini calls (initial + 1 whole-batch repair), got %d", geminiCalls)
	}
	attempts2, _ := h2.db.ListProviderAttempts(context.Background(), run2, string(provider.TypeTranslation))
	if len(attempts2) != 1 || attempts2[0].Status != "succeeded" || attempts2[0].ObservedModel != "gemini-3.8-flash-001" {
		t.Fatalf("repair subrequest must not add extra ProviderAttempt rows; got %+v", attempts2)
	}
	if v2.Segments[0].TargetText != "Câu một đã sửa" {
		t.Fatalf("unexpected repaired segment: %+v", v2.Segments[0])
	}

	// Case 3: Shifted initial output + extra index remaining after whole-batch repair -> rejects Gemini (2 calls) and falls back to DeepSeek (1 call)
	mode = "extra_and_shifted_persist_fallback"
	atomic.StoreInt32(&geminiCalls, 0)
	atomic.StoreInt32(&deepseekCalls, 0)
	h3 := setupCustomTranslationHarness(t, reg)
	asset3, job3 := seedSeam1UniqueAssetAndJob(t, h3.db)
	run3 := "run-shift-" + uuid.NewString()
	seedSeam1Run(t, h3.db, asset3, job3, run3)

	status, v3, errMsg := postSeam1Translate(t, h3.ts.URL, asset3, map[string]any{
		"run_id":                 run3,
		"job_id":                 job3,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               canonicalInput,
	})
	if status != http.StatusCreated {
		t.Fatalf("expected 201 via fallback, got %d (%s)", status, errMsg)
	}
	if atomic.LoadInt32(&geminiCalls) != 2 || atomic.LoadInt32(&deepseekCalls) != 1 {
		t.Fatalf("expected 2 gemini calls and 1 deepseek call, got gemini=%d deepseek=%d", geminiCalls, deepseekCalls)
	}
	if v3.ProviderID != provider.GatewayDeepSeekTranslationProviderID {
		t.Fatalf("expected DeepSeek fallback provider, got %s", v3.ProviderID)
	}
	attempts3, _ := h3.db.ListProviderAttempts(context.Background(), run3, string(provider.TypeTranslation))
	if len(attempts3) != 2 || attempts3[0].Status != "quality_failed" || attempts3[1].Status != "succeeded" {
		t.Fatalf("expected [quality_failed, succeeded] ProviderAttempt rows, got %+v", attempts3)
	}

	// Case 4: A genuinely missing requested index is detected as a per-entry failure and repaired
	// by a targeted subrequest, restoring the entry in canonical order.
	mode = "missing_index_repaired_by_targeted"
	atomic.StoreInt32(&geminiCalls, 0)
	atomic.StoreInt32(&deepseekCalls, 0)
	atomic.StoreInt32(&targetedRepairs, 0)
	h4 := setupCustomTranslationHarness(t, reg)
	asset4, job4 := seedSeam1UniqueAssetAndJob(t, h4.db)
	run4 := "run-missing-" + uuid.NewString()
	seedSeam1Run(t, h4.db, asset4, job4, run4)

	status, v4, errMsg := postSeam1Translate(t, h4.ts.URL, asset4, map[string]any{
		"run_id":                 run4,
		"job_id":                 job4,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               canonicalInput,
	})
	if status != http.StatusCreated {
		t.Fatalf("expected 201 after targeted repair of the missing index, got %d (%s)", status, errMsg)
	}
	if atomic.LoadInt32(&geminiCalls) != 2 || atomic.LoadInt32(&targetedRepairs) != 1 || atomic.LoadInt32(&deepseekCalls) != 0 {
		t.Fatalf("expected 1 initial + 1 targeted gemini call and 0 deepseek calls, got gemini=%d targeted=%d deepseek=%d", geminiCalls, targetedRepairs, deepseekCalls)
	}
	if len(v4.Segments) != 3 || v4.Segments[1].Index != 20 || v4.Segments[1].SourceText != "第二句原话" || v4.Segments[1].TargetText != "Câu hai bù" {
		t.Fatalf("missing index 20 was not restored in canonical order: %+v", v4.Segments)
	}
	attempts4, _ := h4.db.ListProviderAttempts(context.Background(), run4, string(provider.TypeTranslation))
	if len(attempts4) != 1 || attempts4[0].Status != "succeeded" {
		t.Fatalf("expected a single succeeded ProviderAttempt, got %+v", attempts4)
	}

	// Case 5: An unrepaired missing index is refused as an addressable entry failure (never
	// silently backfilled), the quality-failed attempt carries the missing-index classification,
	// and routing falls back to DeepSeek.
	mode = "missing_index_repair_exhausted"
	atomic.StoreInt32(&geminiCalls, 0)
	atomic.StoreInt32(&deepseekCalls, 0)
	h5 := setupCustomTranslationHarness(t, reg)
	asset5, job5 := seedSeam1UniqueAssetAndJob(t, h5.db)
	run5 := "run-missing-exhaust-" + uuid.NewString()
	seedSeam1Run(t, h5.db, asset5, job5, run5)

	status, v5, errMsg := postSeam1Translate(t, h5.ts.URL, asset5, map[string]any{
		"run_id":                 run5,
		"job_id":                 job5,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               canonicalInput,
	})
	if status != http.StatusCreated {
		t.Fatalf("expected 201 via fallback after unrepaired missing index, got %d (%s)", status, errMsg)
	}
	if atomic.LoadInt32(&geminiCalls) != 4 || atomic.LoadInt32(&deepseekCalls) != 1 {
		t.Fatalf("expected 1 initial + 3 targeted gemini calls and 1 deepseek call, got gemini=%d deepseek=%d", geminiCalls, deepseekCalls)
	}
	if v5.ProviderID != provider.GatewayDeepSeekTranslationProviderID {
		t.Fatalf("expected DeepSeek fallback provider, got %s", v5.ProviderID)
	}
	attempts5, _ := h5.db.ListProviderAttempts(context.Background(), run5, string(provider.TypeTranslation))
	if len(attempts5) != 2 || attempts5[0].Status != "quality_failed" || attempts5[1].Status != "succeeded" {
		t.Fatalf("expected [quality_failed, succeeded] ProviderAttempt rows, got %+v", attempts5)
	}
	if !strings.Contains(attempts5[0].ErrorMessage, "missing index") {
		t.Fatalf("expected the missing-index classification in the quality-failed evidence, got %q", attempts5[0].ErrorMessage)
	}
}

func TestSeam1_Issue152_OneThirdThreshold_StrongWeakCopies_NeighborEdit_TermPreservation(t *testing.T) {
	var callCount int32
	var wholeBatchCount int32
	var targetedCount int32
	var glossaryOnInitial int32
	var glossaryOnWholeBatch int32
	var glossaryOnTargeted int32
	var mode string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&callCount, 1)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		sysPrompt := ""
		userContent := ""
		for _, m := range req.Messages {
			if m.Role == "system" {
				sysPrompt = m.Content
			}
			if m.Role == "user" {
				userContent = m.Content
			}
		}
		isWholeBatchRepair := strings.Contains(sysPrompt, "REPAIR INSTRUCTION")
		isTargetedRepair := strings.Contains(userContent, "read_only_context_neighbors")
		if isWholeBatchRepair {
			atomic.AddInt32(&wholeBatchCount, 1)
		}
		if isTargetedRepair {
			atomic.AddInt32(&targetedCount, 1)
		}
		// The frozen EffectiveGlossary must survive as a structured value on every request shape.
		// Counting raw source text here would be tautological: segment source_text contains "SUPOR".
		if frozenGlossaryPresent(userContent) {
			switch {
			case isTargetedRepair:
				atomic.AddInt32(&glossaryOnTargeted, 1)
			case isWholeBatchRepair:
				atomic.AddInt32(&glossaryOnWholeBatch, 1)
			default:
				atomic.AddInt32(&glossaryOnInitial, 1)
			}
		}

		switch mode {
		case "exact_one_third_targeted_only":
			// 3 segments: 1 missing (1*3 == 3, NOT > 1/3 -> must NOT do whole-batch repair, goes straight to targeted!)
			if c == 1 {
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_1", `{"segments":[
					{"index":0,"source_text":"SUPOR第一段内容","target_text":"Nội dung SUPOR một"},
					{"index":1,"source_text":"SUPOR第二段内容","target_text":""},
					{"index":2,"source_text":"SUPOR第三段内容","target_text":"Nội dung SUPOR ba"}
				]}`)
				return
			}
			// Targeted attempt 1: malformed JSON -> rejected
			if c == 2 {
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_1", `{broken_json`)
				return
			}
			// Targeted attempt 2: neighbor edit (includes neighbor index 0 alongside target index 1) -> rejected
			if c == 3 {
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_1", `{"segments":[
					{"index":0,"source_text":"SUPOR第一段内容","target_text":"Hàng xóm bị sửa"},
					{"index":1,"source_text":"SUPOR第二段内容","target_text":"Nội dung SUPOR hai"}
				]}`)
				return
			}
			// Targeted attempt 3: valid single segment
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_1", `{"segments":[
				{"index":1,"source_text":"SUPOR第二段内容","target_text":"Nội dung SUPOR hai chuẩn"}
			]}`)

		case "just_above_one_third_whole_batch":
			// 5 segments: 2 strong copies (2 >= 2 && 2*3 > 5) + 1 weak copy ("小王") + 1 authorized unchanged ("SUPOR") + 1 URL ("https://example.com").
			// Weak copy "小王" is promoted -> 3 flagged entries out of 5 (> 1/3) -> triggers 1 whole-batch repair!
			// Authorized unchanged "SUPOR" and URL "https://example.com" are NEVER promoted.
			if c == 1 {
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_2", `{"segments":[
					{"index":0,"source_text":"今天我们深入评测这款厨房神器","target_text":"今天我们深入评测这款厨房神器"},
					{"index":1,"source_text":"它的加热速度比传统锅快三倍以上","target_text":"它的加热速度比传统锅快三倍以上"},
					{"index":2,"source_text":"小王","target_text":"小王"},
					{"index":3,"source_text":"SUPOR","target_text":"SUPOR"},
					{"index":4,"source_text":"https://example.com","target_text":"https://example.com"}
				]}`)
				return
			}
			// Whole-batch repair fixes 0, 1, 2 while keeping 3 ("SUPOR") and 4 ("https://example.com") unchanged!
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_2", `{"segments":[
				{"index":0,"source_text":"今天我们深入评测这款厨房神器","target_text":"Hôm nay chúng tôi đánh giá sâu thiết bị bếp này"},
				{"index":1,"source_text":"它的加热速度比传统锅快三倍以上","target_text":"Tốc độ làm nóng nhanh hơn gấp ba lần nồi truyền thống"},
				{"index":2,"source_text":"小王","target_text":"Tiểu Vương"},
				{"index":3,"source_text":"SUPOR","target_text":"SUPOR"},
				{"index":4,"source_text":"https://example.com","target_text":"https://example.com"}
			]}`)
		}
	}))
	defer ts.Close()

	reg := provider.NewRegistry()
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-152",
		0.99,
		func(context.Context, string, string) (string, error) { return "sec-token", nil },
		ts.Client(),
		ts.URL,
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)
	h := setupCustomTranslationHarness(t, reg)

	// Subcase A: Exactly 1/3 (1 of 3) + malformed repair + neighbor edit + glossary term preservation
	mode = "exact_one_third_targeted_only"
	atomic.StoreInt32(&callCount, 0)
	atomic.StoreInt32(&wholeBatchCount, 0)
	atomic.StoreInt32(&targetedCount, 0)
	atomic.StoreInt32(&glossaryOnInitial, 0)
	atomic.StoreInt32(&glossaryOnWholeBatch, 0)
	atomic.StoreInt32(&glossaryOnTargeted, 0)

	assetA, jobA := seedSeam1UniqueAssetAndJob(t, h.db)
	runA := "run-1third-" + uuid.NewString()
	seedSeam1Run(t, h.db, assetA, jobA, runA)

	status, vA, errMsg := postSeam1Translate(t, h.ts.URL, assetA, map[string]any{
		"run_id":                 runA,
		"job_id":                 jobA,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"glossary": []map[string]any{
			{"source": "SUPOR", "target": "SUPOR"},
		},
		"segments": []map[string]any{
			{"index": 0, "source_text": "SUPOR第一段内容"},
			{"index": 1, "source_text": "SUPOR第二段内容"},
			{"index": 2, "source_text": "SUPOR第三段内容"},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", status, errMsg)
	}
	if atomic.LoadInt32(&wholeBatchCount) != 0 || atomic.LoadInt32(&targetedCount) != 3 || atomic.LoadInt32(&callCount) != 4 {
		t.Fatalf("expected 0 whole-batch, 3 targeted, 4 total calls; got whole=%d targeted=%d total=%d", wholeBatchCount, targetedCount, callCount)
	}
	if atomic.LoadInt32(&glossaryOnInitial) != 1 {
		t.Fatalf("expected the frozen EffectiveGlossary on the initial request, got %d", glossaryOnInitial)
	}
	if atomic.LoadInt32(&glossaryOnTargeted) != 3 {
		t.Fatalf("expected the frozen EffectiveGlossary on all 3 targeted repair requests, got %d", glossaryOnTargeted)
	}
	if atomic.LoadInt32(&glossaryOnWholeBatch) != 0 {
		t.Fatalf("expected no whole-batch repair request in this subcase, got %d", glossaryOnWholeBatch)
	}
	if vA.Segments[0].TargetText != "Nội dung SUPOR một" || vA.Segments[1].TargetText != "Nội dung SUPOR hai chuẩn" {
		t.Fatalf("neighbor 0 must not be edited and target 1 must be repaired: %+v", vA.Segments)
	}

	// Subcase B: Just above 1/3 via strong/weak copy promotion while preserving authorized unchanged & URL
	mode = "just_above_one_third_whole_batch"
	atomic.StoreInt32(&callCount, 0)
	atomic.StoreInt32(&wholeBatchCount, 0)
	atomic.StoreInt32(&targetedCount, 0)
	atomic.StoreInt32(&glossaryOnInitial, 0)
	atomic.StoreInt32(&glossaryOnWholeBatch, 0)
	atomic.StoreInt32(&glossaryOnTargeted, 0)

	assetB, jobB := seedSeam1UniqueAssetAndJob(t, h.db)
	runB := "run-above1third-" + uuid.NewString()
	seedSeam1Run(t, h.db, assetB, jobB, runB)

	status, vB, errMsg := postSeam1Translate(t, h.ts.URL, assetB, map[string]any{
		"run_id":                 runB,
		"job_id":                 jobB,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"glossary": []map[string]any{
			{"source": "SUPOR", "target": "SUPOR"},
		},
		"segments": []map[string]any{
			{"index": 0, "source_text": "今天我们深入评测这款厨房神器"},
			{"index": 1, "source_text": "它的加热速度比传统锅快三倍以上"},
			{"index": 2, "source_text": "小王"},
			{"index": 3, "source_text": "SUPOR"},
			{"index": 4, "source_text": "https://example.com"},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("expected 201 after whole-batch repair, got %d (%s)", status, errMsg)
	}
	if atomic.LoadInt32(&wholeBatchCount) != 1 || atomic.LoadInt32(&targetedCount) != 0 || atomic.LoadInt32(&callCount) != 2 {
		t.Fatalf("expected 1 whole-batch, 0 targeted, 2 total calls; got whole=%d targeted=%d total=%d", wholeBatchCount, targetedCount, callCount)
	}
	if atomic.LoadInt32(&glossaryOnInitial) != 1 {
		t.Fatalf("expected the frozen EffectiveGlossary on the initial request, got %d", glossaryOnInitial)
	}
	if atomic.LoadInt32(&glossaryOnWholeBatch) != 1 {
		t.Fatalf("expected the frozen EffectiveGlossary on the whole-batch repair request, got %d", glossaryOnWholeBatch)
	}
	if atomic.LoadInt32(&glossaryOnTargeted) != 0 {
		t.Fatalf("expected no targeted repair request in this subcase, got %d", glossaryOnTargeted)
	}
	if vB.Segments[2].TargetText != "Tiểu Vương" || vB.Segments[3].TargetText != "SUPOR" || vB.Segments[4].TargetText != "https://example.com" {
		t.Fatalf("unexpected segments after copy promotion + whole-batch repair: %+v", vB.Segments)
	}
}

// fixedRetryTranslationProvider is a test-only adapter that pins the Router-visible retry budget
// (Capability().MaxRetries) on an otherwise unmodified production GatewayTranslationProvider.
// Production code deliberately exposes no retry seam, so the regression drives it from the test.
type fixedRetryTranslationProvider struct {
	*provider.GatewayTranslationProvider
	maxRetries int
}

func (p *fixedRetryTranslationProvider) Capability() domain.ProviderCapability {
	cap := p.GatewayTranslationProvider.Capability()
	v := p.maxRetries
	cap.MaxRetries = &v
	return cap
}

func TestSeam1_Issue152_RetryReentry_CandidateLocalRepairLimits_And_CancellationAtEachPhase(t *testing.T) {
	mockSec := func(context.Context, string, string) (string, error) { return "sec-token", nil }

	// Repair state is candidate-local across Router retry re-entry (Issue #152): at most one
	// whole-batch repair, and at most three targeted attempts per canonical segment index, for the same
	// candidate. Subcase A proves the whole-batch limit; subcase B proves the per-entry targeted limit.
	// Both end with Gemini exhausted ("failed" then "quality_failed") and DeepSeek taking over with its
	// own fresh budget.
	retryReentryHarness := func(t *testing.T, ts *httptest.Server) *customTranslationHarness {
		t.Helper()
		reg := provider.NewRegistry()
		gemini, _ := provider.NewGatewayTranslationProvider(
			provider.GatewayGeminiTranslationProviderID,
			provider.GatewayGeminiModelAlias,
			"baseline-gemini-retry",
			0.99,
			mockSec,
			ts.Client(),
			ts.URL,
		)
		gemini.SetPolicyState(domain.PolicyAllowed)
		// Production code exposes no retry seam: this test-only adapter pins the Router-visible retry
		// budget while delegating every behavior to the real GatewayTranslationProvider.
		_ = reg.Register(&fixedRetryTranslationProvider{GatewayTranslationProvider: gemini, maxRetries: 2})

		deepseek, _ := provider.NewGatewayTranslationProvider(
			provider.GatewayDeepSeekTranslationProviderID,
			provider.GatewayDeepSeekModelAlias,
			"baseline-deepseek-retry",
			0.95,
			mockSec,
			ts.Client(),
			ts.URL,
		)
		deepseek.SetPolicyState(domain.PolicyAllowed)
		_ = reg.Register(deepseek)
		return setupCustomTranslationHarness(t, reg)
	}

	// assertRetryReentryFallback pins the outcome both candidate-local repair subcases share: the
	// request succeeds through the DeepSeek fallback, Gemini stops after exactly 5 wire calls (each
	// subcase names why, so the wire shape stays explicit), the fallback receives its own fresh budget,
	// and the run records 3 ProviderAttempt rows [gemini:failed, gemini:quality_failed,
	// deepseek:succeeded]. Subcases assert their own row error evidence on the returned rows.
	assertRetryReentryFallback := func(
		t *testing.T,
		h *customTranslationHarness,
		runID string,
		status int,
		v *domain.TranslationVariant,
		errMsg, stopReason string,
		geminiCalls, deepseekCalls *int32,
	) []domain.ProviderAttempt {
		t.Helper()
		if status != http.StatusCreated {
			t.Fatalf("expected 201 via DeepSeek fallback, got %d (%s)", status, errMsg)
		}
		if got := atomic.LoadInt32(geminiCalls); got != 5 {
			t.Fatalf("expected Gemini to stop after 5 wire calls %s, got %d", stopReason, got)
		}
		if got := atomic.LoadInt32(deepseekCalls); got != 1 {
			t.Fatalf("expected DeepSeek fallback to receive its own fresh budget (1 call), got %d", got)
		}
		if v.ProviderID != provider.GatewayDeepSeekTranslationProviderID {
			t.Fatalf("expected DeepSeek provider, got %s", v.ProviderID)
		}
		attempts, _ := h.db.ListProviderAttempts(context.Background(), runID, string(provider.TypeTranslation))
		if len(attempts) != 3 ||
			attempts[0].ProviderID != provider.GatewayGeminiTranslationProviderID || attempts[0].Status != "failed" ||
			attempts[1].ProviderID != provider.GatewayGeminiTranslationProviderID || attempts[1].Status != "quality_failed" ||
			attempts[2].ProviderID != provider.GatewayDeepSeekTranslationProviderID || attempts[2].Status != "succeeded" {
			t.Fatalf("expected 3 ProviderAttempt rows [gemini:failed, gemini:quality_failed, deepseek:succeeded], got %+v", attempts)
		}
		return attempts
	}

	t.Run("whole-batch repair is candidate-local across retry re-entry", func(t *testing.T) {
		var geminiCalls, deepseekCalls int32

		// Gemini (retry budget 2 through the test-only adapter):
		// - Call 1 (initial): 5 empty targets -> >1/3 flagged -> whole-batch repair
		// - Call 2 (whole-batch): 5 empty targets -> targeted repair for index 0
		// - Call 3 (targeted index 0, attempt 1): empty -> continue
		// - Call 4 (targeted index 0, attempt 2): HTTP 503 transport failure -> "failed" row, retry re-entry
		// - Call 5 (initial, attempt 2): 5 empty targets -> the candidate-local whole-batch allowance is
		//   already consumed, so the second whole-batch repair is refused without any wire call:
		//   "quality_failed" row -> DeepSeek fallback with its own fresh budget.
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Model == provider.GatewayDeepSeekModelAlias {
				atomic.AddInt32(&deepseekCalls, 1)
				writeGatewayJSON(w, "deepseek/deepseek-v4.1-flash", "fp_ds", `{"segments":[
					{"index":0,"source_text":"句0","target_text":"Câu 0"},
					{"index":1,"source_text":"句1","target_text":"Câu 1"},
					{"index":2,"source_text":"句2","target_text":"Câu 2"},
					{"index":3,"source_text":"句3","target_text":"Câu 3"},
					{"index":4,"source_text":"句4","target_text":"Câu 4"}
				]}`)
				return
			}
			c := atomic.AddInt32(&geminiCalls, 1)
			if c == 4 {
				http.Error(w, "upstream gateway 503", http.StatusServiceUnavailable)
				return
			}
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_g", `{"segments":[
				{"index":0,"source_text":"句0","target_text":""},
				{"index":1,"source_text":"句1","target_text":""},
				{"index":2,"source_text":"句2","target_text":""},
				{"index":3,"source_text":"句3","target_text":""},
				{"index":4,"source_text":"句4","target_text":""}
			]}`)
		}))
		defer ts.Close()

		h := retryReentryHarness(t, ts)
		assetID, jobID := seedSeam1AssetAndJob(t, h.db)
		runID := "run-retry-wholebatch-" + uuid.NewString()
		seedSeam1Run(t, h.db, assetID, jobID, runID)

		status, v, errMsg := postSeam1Translate(t, h.ts.URL, assetID, map[string]any{
			"run_id":                 runID,
			"job_id":                 jobID,
			"source_language":        "zh",
			"target_language":        "vi",
			"authorized_credentials": []string{"cred"},
			"segments": []map[string]any{
				{"index": 0, "source_text": "句0"},
				{"index": 1, "source_text": "句1"},
				{"index": 2, "source_text": "句2"},
				{"index": 3, "source_text": "句3"},
				{"index": 4, "source_text": "句4"},
			},
		})
		attempts := assertRetryReentryFallback(t, h, runID, status, v, errMsg,
			"(initial + whole-batch + 2 targeted + re-entry initial) instead of running a second whole-batch repair",
			&geminiCalls, &deepseekCalls)
		// Repair subrequests create no extra attempt rows: the transient row keeps its own transport
		// error and gains the exact bounded subrequest counts.
		if !strings.Contains(attempts[0].ErrorMessage, "gateway transport failure") ||
			!strings.Contains(attempts[0].ErrorMessage, "failed after 4 gateway wire calls (2 targeted)") {
			t.Fatalf("transient failure row must keep its error and carry its counts, got %q", attempts[0].ErrorMessage)
		}
		assertEvidenceCountsOnce(t, attempts[0])
		if !strings.Contains(attempts[1].ErrorMessage, "whole-batch repair already consumed for this candidate") ||
			!strings.Contains(attempts[1].ErrorMessage, "after 5 gateway wire calls (2 targeted)") {
			t.Fatalf("re-entry must fail closed on the consumed whole-batch allowance with counts, got %q", attempts[1].ErrorMessage)
		}
		assertEvidenceCountsOnce(t, attempts[1])
	})

	t.Run("targeted attempts are candidate-local per canonical segment index", func(t *testing.T) {
		var geminiCalls, deepseekCalls int32

		// 3 canonical segments with exactly 1 flagged entry stay below the >1/3 whole-batch gate, so
		// index 0 is repaired by targeted calls only:
		// - Call 1 (initial): index 0 empty -> targeted repair for index 0
		// - Call 2 (targeted index 0, attempt 1): empty -> continue
		// - Call 3 (targeted index 0, attempt 2): HTTP 503 transport failure -> "failed" row, retry re-entry
		// - Call 4 (initial, attempt 2): index 0 empty -> targeted repair for index 0
		// - Call 5 (targeted index 0, attempt 3): empty -> the fourth attempt for index 0 is refused
		//   without a wire call: "quality_failed" row -> DeepSeek fallback with a fresh budget.
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Model == provider.GatewayDeepSeekModelAlias {
				atomic.AddInt32(&deepseekCalls, 1)
				writeGatewayJSON(w, "deepseek/deepseek-v4.1-flash", "fp_ds", `{"segments":[
					{"index":0,"source_text":"句0","target_text":"Câu 0"},
					{"index":1,"source_text":"句1","target_text":"Câu 1"},
					{"index":2,"source_text":"句2","target_text":"Câu 2"}
				]}`)
				return
			}
			isTargeted := false
			for _, m := range req.Messages {
				if m.Role == "user" && strings.Contains(m.Content, "read_only_context_neighbors") {
					isTargeted = true
				}
			}
			c := atomic.AddInt32(&geminiCalls, 1)
			switch {
			case c == 3:
				http.Error(w, "upstream gateway 503", http.StatusServiceUnavailable)
			case isTargeted:
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_g", `{"segments":[{"index":0,"source_text":"句0","target_text":""}]}`)
			default:
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_g", `{"segments":[
					{"index":0,"source_text":"句0","target_text":""},
					{"index":1,"source_text":"句1","target_text":"Câu 1"},
					{"index":2,"source_text":"句2","target_text":"Câu 2"}
				]}`)
			}
		}))
		defer ts.Close()

		h := retryReentryHarness(t, ts)
		assetID, jobID := seedSeam1AssetAndJob(t, h.db)
		runID := "run-retry-targeted-" + uuid.NewString()
		seedSeam1Run(t, h.db, assetID, jobID, runID)

		status, v, errMsg := postSeam1Translate(t, h.ts.URL, assetID, map[string]any{
			"run_id":                 runID,
			"job_id":                 jobID,
			"source_language":        "zh",
			"target_language":        "vi",
			"authorized_credentials": []string{"cred"},
			"segments": []map[string]any{
				{"index": 0, "source_text": "句0"},
				{"index": 1, "source_text": "句1"},
				{"index": 2, "source_text": "句2"},
			},
		})
		attempts := assertRetryReentryFallback(t, h, runID, status, v, errMsg,
			"with only 3 targeted calls for index 0 across re-entry",
			&geminiCalls, &deepseekCalls)
		if !strings.Contains(attempts[0].ErrorMessage, "failed after 3 gateway wire calls (2 targeted)") {
			t.Fatalf("transient failure row must carry its counts across re-entry, got %q", attempts[0].ErrorMessage)
		}
		assertEvidenceCountsOnce(t, attempts[0])
		if !strings.Contains(attempts[1].ErrorMessage, "targeted repair attempts exhausted for segment 0") ||
			!strings.Contains(attempts[1].ErrorMessage, "after 5 gateway wire calls (3 targeted)") {
			t.Fatalf("the fourth targeted attempt for index 0 must be refused with the three-call limit, got %q", attempts[1].ErrorMessage)
		}
		assertEvidenceCountsOnce(t, attempts[1])
	})

	// Cancellation at each repair phase: phase 1 (initial), phase 2 (whole-batch), phase 3 (targeted).
	// Must stop immediately at the cancelled phase without calling DeepSeek fallback!
	for _, cancelAtCall := range []int32{1, 2, 3} {
		var phaseCalls int32
		var fallbackHit int32
		var cancelFunc context.CancelFunc

		tsCancel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Model == provider.GatewayDeepSeekModelAlias {
				atomic.AddInt32(&fallbackHit, 1)
				w.WriteHeader(http.StatusOK)
				return
			}
			n := atomic.AddInt32(&phaseCalls, 1)
			if n == cancelAtCall && cancelFunc != nil {
				cancelFunc()
				time.Sleep(20 * time.Millisecond)
			}
			// Return 2 empty segments out of 2 so call 1 -> whole-batch (call 2), call 2 returns 1 empty of 3 -> targeted (call 3)
			if n == 1 {
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_c", `{"segments":[
					{"index":0,"source_text":"句0","target_text":""},
					{"index":1,"source_text":"句1","target_text":""},
					{"index":2,"source_text":"句2","target_text":"Câu 2"}
				]}`)
				return
			}
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_c", `{"segments":[
				{"index":0,"source_text":"句0","target_text":""},
				{"index":1,"source_text":"句1","target_text":"Câu 1"},
				{"index":2,"source_text":"句2","target_text":"Câu 2"}
			]}`)
		}))

		regC := provider.NewRegistry()
		gemC, _ := provider.NewGatewayTranslationProvider(
			provider.GatewayGeminiTranslationProviderID,
			provider.GatewayGeminiModelAlias,
			"baseline-cancel",
			0.99,
			mockSec,
			tsCancel.Client(),
			tsCancel.URL,
		)
		gemC.SetPolicyState(domain.PolicyAllowed)
		_ = regC.Register(gemC)
		dsC, _ := provider.NewGatewayTranslationProvider(
			provider.GatewayDeepSeekTranslationProviderID,
			provider.GatewayDeepSeekModelAlias,
			"baseline-ds-cancel",
			0.95,
			mockSec,
			tsCancel.Client(),
			tsCancel.URL,
		)
		dsC.SetPolicyState(domain.PolicyAllowed)
		_ = regC.Register(dsC)

		hC := setupCustomTranslationHarness(t, regC)
		aC, jC := seedSeam1AssetAndJob(t, hC.db)
		rC := "run-cancel-" + uuid.NewString()
		seedSeam1Run(t, hC.db, aC, jC, rC)

		reqCtx, cancel := context.WithCancel(context.Background())
		cancelFunc = cancel
		bodyBytes, _ := json.Marshal(map[string]any{
			"run_id":                 rC,
			"job_id":                 jC,
			"source_language":        "zh",
			"target_language":        "vi",
			"authorized_credentials": []string{"cred"},
			"segments": []map[string]any{
				{"index": 0, "source_text": "句0"},
				{"index": 1, "source_text": "句1"},
				{"index": 2, "source_text": "句2"},
			},
		})
		httpReq, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, hC.ts.URL+"/api/v1/assets/"+aC+"/translate", bytes.NewReader(bodyBytes))
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(httpReq)
		if err == nil {
			resp.Body.Close()
		}
		cancel()
		tsCancel.Close()

		if atomic.LoadInt32(&phaseCalls) != cancelAtCall {
			t.Fatalf("cancelAtCall=%d: expected exactly %d wire calls before stopping, got %d", cancelAtCall, cancelAtCall, phaseCalls)
		}
		if atomic.LoadInt32(&fallbackHit) != 0 {
			t.Fatalf("cancelAtCall=%d: cancellation must never fall back to DeepSeek, got %d fallback calls", cancelAtCall, fallbackHit)
		}

		// A cancellation is evidence too: the canceled candidate's attempt row must be persisted with
		// its own error text and bounded subrequest counts, even though its request context is done.
		attemptsC, errC := hC.db.ListProviderAttempts(context.Background(), rC, string(provider.TypeTranslation))
		if errC != nil {
			t.Fatalf("cancelAtCall=%d: ListProviderAttempts failed: %v", cancelAtCall, errC)
		}
		if len(attemptsC) != 1 {
			t.Fatalf("cancelAtCall=%d: expected exactly 1 canceled attempt row, got %+v", cancelAtCall, attemptsC)
		}
		if attemptsC[0].ProviderID != provider.GatewayGeminiTranslationProviderID || attemptsC[0].Status != "failed" {
			t.Fatalf("cancelAtCall=%d: unexpected canceled attempt row %s/%s", cancelAtCall, attemptsC[0].ProviderID, attemptsC[0].Status)
		}
		if !strings.Contains(attemptsC[0].ErrorMessage, "canceled after ") {
			t.Fatalf("cancelAtCall=%d: canceled row must carry its bounded subrequest evidence, got %q", cancelAtCall, attemptsC[0].ErrorMessage)
		}
		assertEvidenceCountsOnce(t, attemptsC[0])
	}
}

func TestSeam1_Issue152_ConcurrentProvenance_MixedFingerprints_And_ContractReuse(t *testing.T) {
	var gatewayCalls int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&gatewayCalls, 1)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		userMsg := req.Messages[len(req.Messages)-1].Content

		if strings.Contains(userMsg, "并发A") {
			writeGatewayJSON(w, "gemini-3.8-flash-concurrent-A", "fp-concurrent-A", `{"segments":[
				{"index":0,"source_text":"并发A","target_text":"Song song A"}
			]}`)
			return
		}
		if strings.Contains(userMsg, "并发B") {
			writeGatewayJSON(w, "gemini-3.8-flash-concurrent-B", "fp-concurrent-B", `{"segments":[
				{"index":0,"source_text":"并发B","target_text":"Song song B"}
			]}`)
			return
		}
		if strings.Contains(userMsg, "混合指纹") {
			// First call returns empty target with fp_1; targeted repair returns valid target with fp_2 -> mixed fingerprints -> SystemFingerprint must be ""
			if c == 1 {
				writeGatewayJSON(w, "gemini-3.8-flash-mixed", "fp_1", `{"segments":[
					{"index":0,"source_text":"混合指纹","target_text":""}
				]}`)
				return
			}
			writeGatewayJSON(w, "gemini-3.8-flash-mixed", "fp_2", `{"segments":[
				{"index":0,"source_text":"混合指纹","target_text":"Vân tay hỗn hợp"}
			]}`)
			return
		}
		writeGatewayJSON(w, "gemini-3.8-flash-v4", "fp-v4", `{"segments":[
			{"index":0,"source_text":"契约测试","target_text":"Kiểm tra hợp đồng v4"}
		]}`)
	}))
	defer ts.Close()

	reg := provider.NewRegistry()
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-152-prov",
		0.99,
		func(context.Context, string, string) (string, error) { return "sec-token", nil },
		ts.Client(),
		ts.URL,
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)
	h := setupCustomTranslationHarness(t, reg)

	// 1. Two concurrent Seam1 calls with distinct models/fingerprints must not contaminate each other's TranslationVariant or ProviderAttempt
	assetA, jobA := seedSeam1UniqueAssetAndJob(t, h.db)
	runA := "run-conc-A-" + uuid.NewString()
	seedSeam1Run(t, h.db, assetA, jobA, runA)

	assetB, jobB := seedSeam1UniqueAssetAndJob(t, h.db)
	runB := "run-conc-B-" + uuid.NewString()
	seedSeam1Run(t, h.db, assetB, jobB, runB)

	var wg sync.WaitGroup
	wg.Add(2)
	var stA, stB int
	var vA, vB *domain.TranslationVariant
	go func() {
		defer wg.Done()
		stA, vA, _ = postSeam1Translate(t, h.ts.URL, assetA, map[string]any{
			"run_id":                 runA,
			"job_id":                 jobA,
			"source_language":        "zh",
			"target_language":        "vi",
			"authorized_credentials": []string{"cred"},
			"segments":               []map[string]any{{"index": 0, "source_text": "并发A"}},
		})
	}()
	go func() {
		defer wg.Done()
		stB, vB, _ = postSeam1Translate(t, h.ts.URL, assetB, map[string]any{
			"run_id":                 runB,
			"job_id":                 jobB,
			"source_language":        "zh",
			"target_language":        "vi",
			"authorized_credentials": []string{"cred"},
			"segments":               []map[string]any{{"index": 0, "source_text": "并发B"}},
		})
	}()
	wg.Wait()

	if stA != http.StatusCreated || stB != http.StatusCreated {
		t.Fatalf("concurrent calls failed: stA=%d stB=%d", stA, stB)
	}
	if vA.ObservedModel != "gemini-3.8-flash-concurrent-A" || vA.SystemFingerprint != "fp-concurrent-A" {
		t.Fatalf("variant A contaminated: %+v", vA)
	}
	if vB.ObservedModel != "gemini-3.8-flash-concurrent-B" || vB.SystemFingerprint != "fp-concurrent-B" {
		t.Fatalf("variant B contaminated: %+v", vB)
	}
	attA, _ := h.db.ListProviderAttempts(context.Background(), runA, string(provider.TypeTranslation))
	attB, _ := h.db.ListProviderAttempts(context.Background(), runB, string(provider.TypeTranslation))
	if len(attA) != 1 || attA[0].ObservedModel != "gemini-3.8-flash-concurrent-A" || attA[0].ServiceBaselineID != "baseline-gemini-152-prov" {
		t.Fatalf("attempt A contaminated: %+v", attA)
	}
	if len(attB) != 1 || attB[0].ObservedModel != "gemini-3.8-flash-concurrent-B" || attB[0].ServiceBaselineID != "baseline-gemini-152-prov" {
		t.Fatalf("attempt B contaminated: %+v", attB)
	}

	// 2. Mixed fingerprints across repair subrequests leave SystemFingerprint unset ("")
	atomic.StoreInt32(&gatewayCalls, 0)
	assetM, jobM := seedSeam1UniqueAssetAndJob(t, h.db)
	runM := "run-mixed-fp-" + uuid.NewString()
	seedSeam1Run(t, h.db, assetM, jobM, runM)

	stM, vM, errMsg := postSeam1Translate(t, h.ts.URL, assetM, map[string]any{
		"run_id":                 runM,
		"job_id":                 jobM,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               []map[string]any{{"index": 0, "source_text": "混合指纹"}},
	})
	if stM != http.StatusCreated {
		t.Fatalf("expected 201 for mixed fingerprint repair, got %d (%s)", stM, errMsg)
	}
	if vM.SystemFingerprint != "" {
		t.Fatalf("expected empty SystemFingerprint when repair responses disagree on fingerprint, got %q", vM.SystemFingerprint)
	}

	// 3. Contract identity advance & reuse:
	// Current contract must equal literal "facts_names_numbers_negation_v4+request_glossary_v1+anchored_repair_v1".
	expectedContractLiteral := "facts_names_numbers_negation_v4+request_glossary_v1+anchored_repair_v1"
	if vM.ContractID != expectedContractLiteral {
		t.Fatalf("expected ContractID %q, got %q", expectedContractLiteral, vM.ContractID)
	}

	// Seed a legacy #151 artifact ("facts_names_numbers_negation_v3+request_glossary_v1") and verify it is NOT reused (triggers 1 gateway call),
	// then repeat with the newly produced v4 artifact and verify it IS reused with 0 gateway calls!
	atomic.StoreInt32(&gatewayCalls, 0)
	assetC, jobC := seedSeam1UniqueAssetAndJob(t, h.db)
	runC1 := "run-contract-1-" + uuid.NewString()
	_ = h.db.CreateRun(context.Background(), domain.LocalizationRun{
		ID:        runC1,
		JobID:     jobC,
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})
	trCAS := seedSeam1TranscriptCAS(t, h, assetC, runC1, "契约测试")

	legacyVariant := domain.TranslationVariant{
		ID:                    "legacy-v3-" + uuid.NewString(),
		SchemaVersion:         3,
		ContractID:            "facts_names_numbers_negation_v3+request_glossary_v1",
		AssetID:               assetC,
		RunID:                 runC1,
		JobID:                 jobC,
		SourceLanguage:        "zh",
		TargetLanguage:        "vi",
		TranscriptArtifactCAS: trCAS,
		EffectiveGlossary:     domain.EffectiveGlossary{Hash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		ProviderID:            provider.GatewayGeminiTranslationProviderID,
		ModelVersion:          provider.GatewayGeminiModelAlias,
		ServiceBaselineID:     "baseline-gemini-152-prov",
		ObservedModel:         "gemini-3.8-flash-legacy",
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "契约测试", TargetText: "Bản cũ v3", PassedQAGate: true},
		},
		CreatedAt: time.Now().UTC(),
	}
	legacyBytes, _ := json.Marshal(legacyVariant)
	legacyObj, _ := h.casStore.Put(bytes.NewReader(legacyBytes))
	legacyVariant.CASHash = legacyObj.SHA256
	legacyVariant.ProvenanceHash = "prov-legacy-" + uuid.NewString()
	_ = h.db.SaveTranslationVariantIndex(context.Background(), storage.TranslationVariantIndex{
		ID:             legacyVariant.ID,
		AssetID:        assetC,
		RunID:          runC1,
		JobID:          jobC,
		TargetLanguage: "vi",
		CASHash:        legacyObj.SHA256,
		ProvenanceHash: legacyVariant.ProvenanceHash,
		ProviderID:     legacyVariant.ProviderID,
		ModelVersion:   legacyVariant.ModelVersion,
		CreatedAt:      legacyVariant.CreatedAt,
	})
	stC1, vC1, errMsg := postSeam1Translate(t, h.ts.URL, assetC, map[string]any{
		"run_id":                 runC1,
		"job_id":                 jobC,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               []map[string]any{{"index": 0, "source_text": "契约测试"}},
	})
	if stC1 != http.StatusCreated {
		t.Fatalf("expected 201 when regenerating over legacy v3 contract, got %d (%s)", stC1, errMsg)
	}
	if atomic.LoadInt32(&gatewayCalls) != 1 {
		t.Fatalf("expected legacy v3 artifact to be rejected and trigger 1 gateway call, got %d", gatewayCalls)
	}
	if vC1.ContractID != expectedContractLiteral || vC1.Segments[0].TargetText != "Kiểm tra hợp đồng v4" {
		t.Fatalf("unexpected regenerated v4 variant: %+v", vC1)
	}

	// Second run on the same inputs reuses the current v4 compatible artifact with 0 gateway calls!
	atomic.StoreInt32(&gatewayCalls, 0)
	runC2 := "run-contract-2-" + uuid.NewString()
	_ = h.db.CreateRun(context.Background(), domain.LocalizationRun{
		ID:        runC2,
		JobID:     jobC,
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	})
	_ = h.db.CreateStageExecution(context.Background(), domain.StageExecution{
		ID:             "se-" + runC2,
		RunID:          runC2,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: trCAS,
		CreatedAt:      time.Now().UTC(),
	})
	stC2, vC2, errMsg := postSeam1Translate(t, h.ts.URL, assetC, map[string]any{
		"run_id":                 runC2,
		"job_id":                 jobC,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               []map[string]any{{"index": 0, "source_text": "契约测试"}},
	})
	if stC2 != http.StatusCreated {
		t.Fatalf("expected 201 on current contract reuse, got %d (%s)", stC2, errMsg)
	}
	if atomic.LoadInt32(&gatewayCalls) != 0 {
		t.Fatalf("expected current v4 compatible artifact to reuse with 0 gateway calls, got %d", gatewayCalls)
	}
	if vC2.CASHash != vC1.CASHash {
		t.Fatalf("expected reused CASHash %s, got %s", vC1.CASHash, vC2.CASHash)
	}
}

func TestSeam1_Issue152_MeaningQADistinction_ModelDisagreement_MissingFingerprint_And_Correction(t *testing.T) {
	var mode string
	var deepseekCalls int32
	var step int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == provider.GatewayDeepSeekModelAlias {
			atomic.AddInt32(&deepseekCalls, 1)
		}
		n := atomic.AddInt32(&step, 1)

		switch mode {
		case "missing_fingerprint":
			// Call 1 has fp_1 and empty target; targeted repair has "" (missing fingerprint) -> final SystemFingerprint must be ""
			if n == 1 {
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_1", `{"segments":[{"index":0,"source_text":"缺失指纹","target_text":""}]}`)
				return
			}
			writeGatewayJSON(w, "gemini-3.8-flash-001", "", `{"segments":[{"index":0,"source_text":"缺失指纹","target_text":"Thiếu vân tay"}]}`)
		case "model_disagreement":
			// Call 1 reports model-1; targeted repair reports model-2 -> fails closed (ErrInconsistentProvenance, policy_rejected, 0 DeepSeek fallback calls)
			if n == 1 {
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_same", `{"segments":[{"index":0,"source_text":"模型冲突","target_text":""}]}`)
				return
			}
			writeGatewayJSON(w, "gemini-3.8-flash-002-drift", "fp_same", `{"segments":[{"index":0,"source_text":"模型冲突","target_text":"Xung đột model"}]}`)
		case "meaning_qa_review_distinct_from_structural":
			// Both Gemini and DeepSeek return structurally valid anchored output that omits the number "500" -> meaning QA flags it for review (201 Created, PassedQAGate=false)
			writeGatewayJSON(w, req.Model+"-obs", "fp_qa", `{"segments":[{"index":0,"source_text":"容量500毫升","target_text":"Dung tích lớn","key_facts":["500"],"negation_polarity":false}]}`)
		}
	}))
	defer ts.Close()

	reg := provider.NewRegistry()
	mockSec := func(context.Context, string, string) (string, error) { return "sec-token", nil }
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-152-qa",
		0.99,
		mockSec,
		ts.Client(),
		ts.URL,
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)
	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
		"baseline-deepseek-152-qa",
		0.95,
		mockSec,
		ts.Client(),
		ts.URL,
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	h := setupCustomTranslationHarness(t, reg)

	// 1. Missing fingerprint in one contributing response -> SystemFingerprint is ""
	mode = "missing_fingerprint"
	atomic.StoreInt32(&step, 0)
	asset1, job1 := seedSeam1UniqueAssetAndJob(t, h.db)
	run1 := "run-miss-fp-" + uuid.NewString()
	seedSeam1Run(t, h.db, asset1, job1, run1)
	st1, v1, errMsg := postSeam1Translate(t, h.ts.URL, asset1, map[string]any{
		"run_id":                 run1,
		"job_id":                 job1,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               []map[string]any{{"index": 0, "source_text": "缺失指纹"}},
	})
	if st1 != http.StatusCreated || v1.SystemFingerprint != "" {
		t.Fatalf("expected 201 with empty SystemFingerprint on missing contributor fingerprint, got st=%d fp=%q err=%s", st1, v1.SystemFingerprint, errMsg)
	}

	// 2. Model disagreement across repair subrequests -> fails closed (no fallback to DeepSeek)
	mode = "model_disagreement"
	atomic.StoreInt32(&step, 0)
	atomic.StoreInt32(&deepseekCalls, 0)
	asset2, job2 := seedSeam1UniqueAssetAndJob(t, h.db)
	run2 := "run-disagree-" + uuid.NewString()
	seedSeam1Run(t, h.db, asset2, job2, run2)
	st2, _, _ := postSeam1Translate(t, h.ts.URL, asset2, map[string]any{
		"run_id":                 run2,
		"job_id":                 job2,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               []map[string]any{{"index": 0, "source_text": "模型冲突"}},
	})
	if st2 == http.StatusCreated || st2 == http.StatusOK {
		t.Fatalf("expected model disagreement to fail closed, got %d", st2)
	}
	if atomic.LoadInt32(&deepseekCalls) != 0 {
		t.Fatalf("model disagreement must fail closed without falling back to DeepSeek, got %d DeepSeek calls", deepseekCalls)
	}
	att2, _ := h.db.ListProviderAttempts(context.Background(), run2, string(provider.TypeTranslation))
	if len(att2) != 1 || att2[0].Status != "policy_rejected" {
		t.Fatalf("expected single policy_rejected ProviderAttempt row on model disagreement, got %+v", att2)
	}

	// 3. Honest meaning-QA review artifact stays distinct from structural rejection
	mode = "meaning_qa_review_distinct_from_structural"
	atomic.StoreInt32(&step, 0)
	asset3, job3 := seedSeam1UniqueAssetAndJob(t, h.db)
	run3 := "run-meaning-qa-" + uuid.NewString()
	seedSeam1Run(t, h.db, asset3, job3, run3)
	st3, v3, errMsg := postSeam1Translate(t, h.ts.URL, asset3, map[string]any{
		"run_id":                 run3,
		"job_id":                 job3,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments":               []map[string]any{{"index": 0, "source_text": "容量500毫升"}},
	})
	if st3 != http.StatusCreated || v3 == nil {
		t.Fatalf("expected 201 Created with flagged meaning-QA variant, got %d (%s)", st3, errMsg)
	}
	if len(v3.Segments) != 1 || v3.Segments[0].PassedQAGate || !strings.Contains(v3.Segments[0].ReviewReason, "500") {
		t.Fatalf("expected segment 0 flagged for missing number 500, got %+v", v3.Segments)
	}
}

// TestSeam1_Issue152_InvalidCanonicalBatch_IngressBadRequest covers the typed canonical-batch
// ingress classification (A4 bounds): duplicate indices, the 512-segment ceiling and the 256 KiB
// aggregate source ceiling are caller contract violations answered with 400 before any provider
// routing or state mutation.
func TestSeam1_Issue152_InvalidCanonicalBatch_IngressBadRequest(t *testing.T) {
	reg := provider.NewRegistry()
	var gatewayCalls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gatewayCalls, 1)
		writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_ingress", `{"segments":[]}`)
	}))
	defer ts.Close()
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-152",
		0.99,
		func(context.Context, string, string) (string, error) { return "sec-token", nil },
		ts.Client(),
		ts.URL,
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)
	h := setupCustomTranslationHarness(t, reg)

	oversizeSegments := make([]map[string]any, 513)
	for i := range oversizeSegments {
		oversizeSegments[i] = map[string]any{"index": i, "source_text": "a"}
	}
	// Varied content: a repetitive filler is filtered as pathological repetition noise before the
	// bound check, so the aggregate byte ceiling must be crossed with non-repetitive source text.
	letters := []rune("天地玄黄宇宙洪荒日月盈昃辰宿列张寒来暑往秋收冬藏闰余成岁律吕调阳云腾致雨露结为霜0123456789")
	var byteHeavy strings.Builder
	for i := 0; byteHeavy.Len() <= (256 << 10); i++ {
		byteHeavy.WriteRune(letters[(i*7)%len(letters)])
		byteHeavy.WriteRune(letters[(i*13+5)%len(letters)])
		byteHeavy.WriteRune(letters[(i*29+11)%len(letters)])
		byteHeavy.WriteRune(rune('0' + (i % 10)))
	}
	byteHeavySegments := []map[string]any{
		{"index": 0, "source_text": byteHeavy.String()},
		{"index": 1, "source_text": "b"},
	}

	cases := []struct {
		name     string
		segments []map[string]any
		want     string
	}{
		{name: "duplicate_index", segments: []map[string]any{{"index": 7, "source_text": "a"}, {"index": 7, "source_text": "b"}}, want: "batch indices must be unique"},
		{name: "segment_count_bound", segments: oversizeSegments, want: "512-segment bound"},
		{name: "source_byte_bound", segments: byteHeavySegments, want: "262144-byte batch bound"},
	}

	for _, tc := range cases {
		assetID, jobID := seedSeam1UniqueAssetAndJob(t, h.db)
		runID := "run-ingress-" + uuid.NewString()
		seedSeam1Run(t, h.db, assetID, jobID, runID)
		status, variant, errMsg := postSeam1Translate(t, h.ts.URL, assetID, map[string]any{
			"run_id":                 runID,
			"job_id":                 jobID,
			"source_language":        "zh",
			"target_language":        "vi",
			"authorized_credentials": []string{"cred"},
			"segments":               tc.segments,
		})
		if status != http.StatusBadRequest || variant != nil {
			t.Fatalf("%s: expected 400 with no variant, got status=%d variant=%v (%s)", tc.name, status, variant, errMsg)
		}
		if !strings.Contains(errMsg, tc.want) {
			t.Fatalf("%s: expected error to mention %q, got %q", tc.name, tc.want, errMsg)
		}
	}
	if got := atomic.LoadInt32(&gatewayCalls); got != 0 {
		t.Fatalf("invalid canonical batches must stop before provider routing, got %d gateway calls", got)
	}
}

// TestSeam1_Issue152_RepairEvidence_SubrequestCountsOnSuccess covers Issue #150 A5: a successful
// bounded repair exposes the consumed total/targeted subrequest counts through the existing
// ProviderAttempt evidence, without creating extra attempt rows or a new attempts store.
func TestSeam1_Issue152_RepairEvidence_SubrequestCountsOnSuccess(t *testing.T) {
	var mode string
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		userContent := ""
		sysPrompt := ""
		for _, m := range body.Messages {
			switch m.Role {
			case "user":
				userContent = m.Content
			case "system":
				sysPrompt = m.Content
			}
		}
		isTargeted := strings.Contains(userContent, "read_only_context_neighbors")
		isWholeBatch := strings.Contains(sysPrompt, "REPAIR INSTRUCTION")
		c := atomic.AddInt32(&calls, 1)

		switch mode {
		case "whole_batch_repair_success":
			if c == 1 {
				// Duplicate index forces exactly one whole-batch repair.
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_ev_wb", `{"segments":[
					{"index":10,"source_text":"第一句原话","target_text":"Bản 1"},
					{"index":10,"source_text":"第一句原话","target_text":"Bản 2"},
					{"index":20,"source_text":"第二句原话","target_text":"Câu hai"},
					{"index":30,"source_text":"第三句原话","target_text":"Câu ba"}
				]}`)
				return
			}
			if !isWholeBatch {
				t.Errorf("whole-batch success case: expected a REPAIR INSTRUCTION subrequest, got user=%q", userContent)
			}
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_ev_wb", `{"segments":[
				{"index":10,"source_text":"第一句原话","target_text":"Câu một đã sửa"},
				{"index":20,"source_text":"第二句原话","target_text":"Câu hai đã sửa"},
				{"index":30,"source_text":"第三句原话","target_text":"Câu ba đã sửa"}
			]}`)
		case "targeted_repair_success":
			if c == 1 {
				// 1 empty target out of 3 stays at exactly 1/3 (not > 1/3): targeted repair only.
				writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_ev_tg", `{"segments":[
					{"index":40,"source_text":"第四句原话","target_text":""},
					{"index":50,"source_text":"第五句原话","target_text":"Câu năm"},
					{"index":60,"source_text":"第六句原话","target_text":"Câu sáu"}
				]}`)
				return
			}
			if !isTargeted {
				t.Errorf("targeted success case: expected a read_only_context_neighbors subrequest, got user=%q", userContent)
			}
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_ev_tg", `{"segments":[
				{"index":40,"source_text":"第四句原话","target_text":"Câu bốn đã sửa"}
			]}`)
		}
	}))
	defer ts.Close()

	reg := provider.NewRegistry()
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-152",
		0.99,
		func(context.Context, string, string) (string, error) { return "sec-token", nil },
		ts.Client(),
		ts.URL,
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)
	h := setupCustomTranslationHarness(t, reg)

	wholeBatchInput := []map[string]any{
		{"index": 10, "source_text": "第一句原话"},
		{"index": 20, "source_text": "第二句原话"},
		{"index": 30, "source_text": "第三句原话"},
	}
	// Distinct canonical input: contract-safe reuse must not serve the previous case's variant.
	targetedInput := []map[string]any{
		{"index": 40, "source_text": "第四句原话"},
		{"index": 50, "source_text": "第五句原话"},
		{"index": 60, "source_text": "第六句原话"},
	}

	cases := []struct {
		mode            string
		segments        []map[string]any
		wantCalls       int32
		wantEvidence    string
		wantFirstTarget string
	}{
		{mode: "whole_batch_repair_success", segments: wholeBatchInput, wantCalls: 2, wantEvidence: "succeeded after 2 gateway wire calls (0 targeted) with a 14-call candidate budget", wantFirstTarget: "Câu một đã sửa"},
		{mode: "targeted_repair_success", segments: targetedInput, wantCalls: 2, wantEvidence: "succeeded after 2 gateway wire calls (1 targeted) with a 14-call candidate budget", wantFirstTarget: "Câu bốn đã sửa"},
	}

	for _, tc := range cases {
		mode = tc.mode
		atomic.StoreInt32(&calls, 0)
		assetID, jobID := seedSeam1UniqueAssetAndJob(t, h.db)
		runID := "run-evidence-" + uuid.NewString()
		seedSeam1Run(t, h.db, assetID, jobID, runID)

		status, v, errMsg := postSeam1Translate(t, h.ts.URL, assetID, map[string]any{
			"run_id":                 runID,
			"job_id":                 jobID,
			"source_language":        "zh",
			"target_language":        "vi",
			"authorized_credentials": []string{"cred"},
			"segments":               tc.segments,
		})
		if status != http.StatusCreated {
			t.Fatalf("%s: expected 201, got %d (%s)", tc.mode, status, errMsg)
		}
		if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
			t.Fatalf("%s: expected %d wire calls, got %d", tc.mode, tc.wantCalls, got)
		}
		if v.Segments[0].TargetText != tc.wantFirstTarget {
			t.Fatalf("%s: unexpected repaired target: %+v", tc.mode, v.Segments[0])
		}
		attempts, _ := h.db.ListProviderAttempts(context.Background(), runID, string(provider.TypeTranslation))
		if len(attempts) != 1 {
			t.Fatalf("%s: repair subrequests must not add ProviderAttempt rows, got %+v", tc.mode, attempts)
		}
		if attempts[0].Status != "succeeded" {
			t.Fatalf("%s: expected succeeded attempt, got %+v", tc.mode, attempts[0])
		}
		if !strings.Contains(attempts[0].ErrorMessage, tc.wantEvidence) {
			t.Fatalf("%s: expected attempt evidence %q, got %q", tc.mode, tc.wantEvidence, attempts[0].ErrorMessage)
		}
	}
}

// TestSeam1_Issue152_ModelDisagreement_PublishesInvocationEvidenceBeforeFailClosed pins #150 A5:
// observed-model disagreement across the initial call and a repair subrequest fails closed with
// ErrInconsistentProvenance, but the single ProviderAttempt row still carries the exact
// invocation-local observed model, configured service baseline and bounded subrequest counts.
// No fallback candidate may be invoked, and no extra attempt row may be created for repair
// subrequests.
func TestSeam1_Issue152_ModelDisagreement_PublishesInvocationEvidenceBeforeFailClosed(t *testing.T) {
	var geminiCalls, deepseekCalls int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == provider.GatewayDeepSeekModelAlias {
			atomic.AddInt32(&deepseekCalls, 1)
			writeGatewayJSON(w, "deepseek/deepseek-v4.1-flash", "fp_ds", `{"segments":[{"index":0,"source_text":"句0","target_text":"Câu 0"}]}`)
			return
		}
		c := atomic.AddInt32(&geminiCalls, 1)
		if c == 1 {
			// Initial call: one empty target of one segment -> whole-batch repair.
			writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_g", `{"segments":[{"index":0,"source_text":"句0","target_text":""}]}`)
			return
		}
		// Whole-batch repair answers with a different observed model: provenance disagreement.
		writeGatewayJSON(w, "gemini-3.8-flash-002-different", "fp_g", `{"segments":[{"index":0,"source_text":"句0","target_text":"Câu 0"}]}`)
	}))
	defer ts.Close()

	reg := provider.NewRegistry()
	mockSec := func(context.Context, string, string) (string, error) { return "sec-token", nil }
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-disagree",
		0.99,
		mockSec,
		ts.Client(),
		ts.URL,
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
		"baseline-deepseek-disagree",
		0.95,
		mockSec,
		ts.Client(),
		ts.URL,
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	h := setupCustomTranslationHarness(t, reg)
	assetID, jobID := seedSeam1AssetAndJob(t, h.db)
	runID := "run-disagree-" + uuid.NewString()
	seedSeam1Run(t, h.db, assetID, jobID, runID)

	status, _, errMsg := postSeam1Translate(t, h.ts.URL, assetID, map[string]any{
		"run_id":                 runID,
		"job_id":                 jobID,
		"source_language":        "zh",
		"target_language":        "vi",
		"authorized_credentials": []string{"cred"},
		"segments": []map[string]any{
			{"index": 0, "source_text": "句0"},
		},
	})
	if status != http.StatusInternalServerError {
		t.Fatalf("expected 500 fail-closed on model disagreement, got %d (%s)", status, errMsg)
	}
	if atomic.LoadInt32(&geminiCalls) != 2 || atomic.LoadInt32(&deepseekCalls) != 0 {
		t.Fatalf("expected 2 gemini calls and no fallback invocation, got gemini=%d deepseek=%d", geminiCalls, deepseekCalls)
	}

	attempts, _ := h.db.ListProviderAttempts(context.Background(), runID, string(provider.TypeTranslation))
	if len(attempts) != 1 {
		t.Fatalf("expected exactly 1 ProviderAttempt row (no extra repair rows), got %+v", attempts)
	}
	row := attempts[0]
	if row.ProviderID != provider.GatewayGeminiTranslationProviderID || row.Status != "policy_rejected" {
		t.Fatalf("expected gemini policy_rejected row, got %s/%s", row.ProviderID, row.Status)
	}
	if row.ObservedModel != "gemini-3.8-flash-001" {
		t.Fatalf("expected the first invocation-observed model, got %q", row.ObservedModel)
	}
	if row.ServiceBaselineID != "baseline-gemini-disagree" {
		t.Fatalf("expected the configured service baseline, got %q", row.ServiceBaselineID)
	}
	if !strings.Contains(row.ErrorMessage, "observed model disagreement across repair responses in translation invocation") {
		t.Fatalf("row must keep the disagreement error text, got %q", row.ErrorMessage)
	}
	if !strings.Contains(row.ErrorMessage, "model disagreement after 2 gateway wire calls (0 targeted) with a 14-call candidate budget") {
		t.Fatalf("row must carry the exact disagreement invocation evidence, got %q", row.ErrorMessage)
	}
	assertEvidenceCountsOnce(t, row)
}

// TestSeam1_Issue152_ModelDisagreementWithInvalidMergedOutput_FailsClosedWithoutFallback pins the
// provenance precedence: when one invocation both observes two different models AND leaves the
// merged batch structurally invalid, the candidate must fail closed as inconsistent provenance
// instead of degrading into a retryable quality rejection that lets the Router advance to the
// fallback lane (Issue #150 A5). The structural rejection and the provenance failure coincide here,
// and provenance must win.
func TestSeam1_Issue152_ModelDisagreementWithInvalidMergedOutput_FailsClosedWithoutFallback(t *testing.T) {
	mockSec := func(context.Context, string, string) (string, error) { return "sec-token", nil }

	// Both cases observe two different models in one invocation and leave the merged batch invalid:
	// "batch_residual" is rejected inside the single whole-batch repair, "targeted_exhaustion" after
	// three targeted attempts. Either way the provenance failure must win over the structural
	// rejection, otherwise the Router would answer it with the DeepSeek fallback lane.
	cases := []struct {
		name         string
		mode         string
		wantGemini   int32
		wantEvidence string
	}{
		{
			name:         "whole-batch repair leaves residual structure",
			mode:         "batch_residual",
			wantGemini:   2,
			wantEvidence: "model disagreement after 2 gateway wire calls (0 targeted) with a 14-call candidate budget",
		},
		{
			name:         "targeted repair exhausted with invalid merged output",
			mode:         "targeted_exhaustion",
			wantGemini:   5,
			wantEvidence: "model disagreement after 5 gateway wire calls (3 targeted) with a 14-call candidate budget",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var geminiCalls, deepseekCalls int32

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Model == provider.GatewayDeepSeekModelAlias {
					// The fallback lane must never be reached: only its call counter is asserted.
					atomic.AddInt32(&deepseekCalls, 1)
					w.WriteHeader(http.StatusOK)
					return
				}
				c := atomic.AddInt32(&geminiCalls, 1)
				if c == 1 {
					// Initial call: one empty target of one segment -> whole-batch repair.
					writeGatewayJSON(w, "gemini-3.8-flash-001", "fp_g", `{"segments":[{"index":0,"source_text":"句0","target_text":""}]}`)
					return
				}
				if tc.mode == "batch_residual" {
					// The single whole-batch allowance reports the other model AND repeats the index,
					// so the batch is still structurally invalid when the repair is rejected.
					writeGatewayJSON(w, "gemini-3.8-flash-002-different", "fp_g", `{"segments":[
						{"index":0,"source_text":"句0","target_text":"Câu 0"},
						{"index":0,"source_text":"句0","target_text":"Câu 0"}
					]}`)
					return
				}
				// Targeted repair reports the other model AND keeps the echo mismatched, so every
				// targeted attempt is refused and the merged batch is never repaired.
				writeGatewayJSON(w, "gemini-3.8-flash-002-different", "fp_g", `{"segments":[{"index":0,"source_text":"回声不匹配","target_text":"Câu 0"}]}`)
			}))
			defer ts.Close()

			reg := provider.NewRegistry()
			gemini, _ := provider.NewGatewayTranslationProvider(
				provider.GatewayGeminiTranslationProviderID,
				provider.GatewayGeminiModelAlias,
				"baseline-gemini-disagree-invalid",
				0.99,
				mockSec,
				ts.Client(),
				ts.URL,
			)
			gemini.SetPolicyState(domain.PolicyAllowed)
			_ = reg.Register(gemini)

			deepseek, _ := provider.NewGatewayTranslationProvider(
				provider.GatewayDeepSeekTranslationProviderID,
				provider.GatewayDeepSeekModelAlias,
				"baseline-deepseek-disagree-invalid",
				0.95,
				mockSec,
				ts.Client(),
				ts.URL,
			)
			deepseek.SetPolicyState(domain.PolicyAllowed)
			_ = reg.Register(deepseek)

			h := setupCustomTranslationHarness(t, reg)
			assetID, jobID := seedSeam1AssetAndJob(t, h.db)
			runID := "run-disagree-invalid-" + uuid.NewString()
			seedSeam1Run(t, h.db, assetID, jobID, runID)

			status, _, errMsg := postSeam1Translate(t, h.ts.URL, assetID, map[string]any{
				"run_id":                 runID,
				"job_id":                 jobID,
				"source_language":        "zh",
				"target_language":        "vi",
				"authorized_credentials": []string{"cred"},
				"segments": []map[string]any{
					{"index": 0, "source_text": "句0"},
				},
			})
			if status != http.StatusInternalServerError {
				t.Fatalf("expected 500 fail-closed on provenance disagreement, got %d (%s)", status, errMsg)
			}
			if !strings.Contains(errMsg, "observed model disagreement across repair responses in translation invocation") {
				t.Fatalf("the provenance failure must be reported instead of the structural rejection, got %q", errMsg)
			}
			if got := atomic.LoadInt32(&geminiCalls); got != tc.wantGemini {
				t.Fatalf("expected %d gemini wire calls, got %d", tc.wantGemini, got)
			}
			if atomic.LoadInt32(&deepseekCalls) != 0 {
				t.Fatalf("provenance disagreement must never fall back to another candidate, got %d deepseek calls", deepseekCalls)
			}

			attempts, _ := h.db.ListProviderAttempts(context.Background(), runID, string(provider.TypeTranslation))
			if len(attempts) != 1 {
				t.Fatalf("expected exactly 1 ProviderAttempt row, got %+v", attempts)
			}
			row := attempts[0]
			if row.ProviderID != provider.GatewayGeminiTranslationProviderID || row.Status != "policy_rejected" {
				t.Fatalf("expected gemini policy_rejected row, got %s/%s", row.ProviderID, row.Status)
			}
			if row.ObservedModel != "gemini-3.8-flash-001" {
				t.Fatalf("expected the first invocation-observed model, got %q", row.ObservedModel)
			}
			if row.ServiceBaselineID != "baseline-gemini-disagree-invalid" {
				t.Fatalf("expected the configured service baseline, got %q", row.ServiceBaselineID)
			}
			if !strings.Contains(row.ErrorMessage, tc.wantEvidence) {
				t.Fatalf("row must carry the exact disagreement invocation evidence, got %q", row.ErrorMessage)
			}
			assertEvidenceCountsOnce(t, row)
		})
	}
}
