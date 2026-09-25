package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProductionWriteTimeoutCoversLongOperations pins the production HTTP
// write timeout: localhost synchronous ML endpoints legitimately run for
// multiple minutes before the first response byte, so the timeout must equal
// the named long-operation budget and stay finite/non-zero. No sleeping.
func TestProductionWriteTimeoutCoversLongOperations(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if s.server == nil {
		t.Fatal("expected production http.Server to be configured")
	}
	if got := s.server.WriteTimeout; got != runtimeHostWriteTimeout {
		t.Fatalf("WriteTimeout = %v, want named runtimeHostWriteTimeout %v", got, runtimeHostWriteTimeout)
	}
	if s.server.WriteTimeout <= 0 {
		t.Fatal("WriteTimeout must remain finite/non-zero")
	}
	if want := 30 * time.Minute; s.server.WriteTimeout != want {
		t.Fatalf("WriteTimeout = %v, want %v", s.server.WriteTimeout, want)
	}
	if got := s.server.ReadTimeout; got != 30*time.Second {
		t.Fatalf("ReadTimeout = %v, want 30s (must stay unchanged)", got)
	}
}

func TestOperatorUIShellRoutes(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})

	tests := []struct {
		path        string
		contentType string
		contains    []string
		notContains []string
	}{
		{
			path:        "/",
			contentType: "text/html",
			contains: []string{
				"Douyinie", "Operator", "rel=\"icon\"", "/ui/app.css", "/ui/app.js",
				"workflow-stepper", "review-workspace", "observational-timeline",
				"review_posture", "data-step=\"source\"", "data-step=\"export\"",
				"zone-context", "zone-center", "zone-inspector",
				"data-view-target=\"discover\"", "id=\"discovery-search-form\"", "id=\"discovery-download\"",
			},
			notContains: []string{
				"kokoro_tts_en", "af_heart",
			},
		},
		{
			path:        "/ui/app.css",
			contentType: "text/css",
			contains: []string{
				"--surface", ".app-shell", ".workflow-stepper", ".review-workspace",
				".observational-timeline-panel", ".stepper-step.status-queued",
				".speaker-card.is-unassigned", ".discovery-layout", ".discovery-bulk",
			},
		},
		{
			path:        "/ui/app.js",
			contentType: "javascript",
			contains: []string{
				"/api/v1/health", "/api/v1/jobs", "/api/v1/assets/upload",
				"/api/v1/runs/", "/render/handoff", "computeStepState", "speakerColor",
				"/api/v1/douyin/search", "/api/v1/douyin/lookup/video", "/api/v1/douyin/lookup/creator",
				"downloadSelectedDiscovery", "discoverySelected",
				"\"queued\"", "unassigned-badge",
				`const finalStage = state.stages.find((st) => st.stage === "render_final")`,
				`getRunPosture(state.selectedRun) === "review"`,
				`posture !== "review" || !state.preview || state.final`,
				"state.handoff = null",
				"function emptyReviewQueueState()", `runStatus === "completed"`,
				"Chưa thể xem là PASS.",
			},
			notContains: []string{
				"\"Default voice\"", "voiceId: \"default\"",
				`st.stage === "render_final" || st.stage === "final_render_handoff"`,
				"Tất cả quality gates đã PASS.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != 200 {
				t.Fatalf("GET %s status = %d, want 200; body=%q", tt.path, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tt.contentType) {
				t.Fatalf("GET %s Content-Type = %q, want to contain %q", tt.path, got, tt.contentType)
			}
			body := rec.Body.String()
			for _, want := range tt.contains {
				if !strings.Contains(body, want) {
					t.Fatalf("GET %s body missing %q", tt.path, want)
				}
			}
			for _, prohibited := range tt.notContains {
				if strings.Contains(body, prohibited) {
					t.Fatalf("GET %s body unexpectedly contains %q", tt.path, prohibited)
				}
			}
		})
	}
}

// newAuditionTestServer builds a Server whose fake TTS returns synthWAV, returning
// the server, the audition asset ID and the harness root for path-leak assertions.
func newAuditionTestServer(t *testing.T, synthWAV []byte) (*Server, string, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "audition.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	now := time.Now().UTC()
	att := domain.RightsAttestation{ID: "att-audition", AttestationType: "OWNER_DIRECT", DeclaredBy: "tester", TermsAccepted: true, ConfirmedAt: now}
	if err := db.CreateRightsAttestation(ctx, att); err != nil {
		t.Fatalf("create attestation: %v", err)
	}
	asset := domain.SourceAsset{ID: "asset-audition", SHA256: strings.Repeat("a", 64), ByteSize: 1024, MimeType: "video/mp4", RightsAttestationID: att.ID, CASPath: filepath.Join(root, "secret-source.mp4"), CreatedAt: now}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("create asset: %v", err)
	}
	if err := db.SaveAudioRolePlan(ctx, domain.AudioRolePlan{ID: "role-audition", AssetID: asset.ID, CreatedAt: now, Segments: []domain.AudioSegment{{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue}}}); err != nil {
		t.Fatalf("save audio role plan: %v", err)
	}

	dubbingSvc := service.NewDubbingService(db, casStore)
	dubbingSvc.TTSInvoke = func(context.Context, provider.Provider, provider.TTSSynthesisRequest) (*provider.TTSSynthesisResult, error) {
		return &provider.TTSSynthesisResult{AudioData: synthWAV, Format: "wav", SampleRate: 16000, Channels: 1, MeasuredDurationMs: 500, ProviderID: "fake-tts", ModelName: "fake-model", ModelVersion: "1"}, nil
	}
	return New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore, DubbingSvc: dubbingSvc}), asset.ID, root
}

func TestVoiceAuditionRejectsOversizeArtifactFailClosed(t *testing.T) {
	t.Parallel()
	// Valid, probeable WAV padded to exactly one byte past the audition artifact
	// limit: the request must fail closed instead of buffering and base64-encoding
	// an unbounded artifact into the HTTP response.
	oversize := media.GeneratePCM16WAV(16000, 1, 1000)
	oversize = append(oversize, make([]byte, int(service.MaxAuditionAudioBytes)+1-len(oversize))...)
	s, assetID, root := newAuditionTestServer(t, oversize)

	body, err := json.Marshal(map[string]any{
		"run_id": "run-audition-oversize", "target_language": "vi", "sample_text": "Xin chao",
		"voice": domain.VoiceProfile{ID: "voice-audition", ProviderID: "fake-tts", VoiceID: "voice-1", Name: "Voice 1", Language: "vi"},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/"+assetID+"/voice-audition", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("oversize audition status=%d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "audio_data_url") {
		t.Fatalf("oversize audition must not return a partial audio payload: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exceeds") {
		t.Fatalf("oversize audition error must name the artifact limit: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), root) {
		t.Fatalf("response leaked a machine-local filesystem path: %s", rec.Body.String())
	}
}

func TestVoiceAuditionReturnsBrowserPlayableAudioWithoutLocalPath(t *testing.T) {
	t.Parallel()
	wav := media.GeneratePCM16WAV(16000, 1, 500)
	s, assetID, root := newAuditionTestServer(t, wav)

	body, err := json.Marshal(map[string]any{
		"run_id": "run-audition", "target_language": "vi", "sample_text": "Xin chao",
		"voice": domain.VoiceProfile{ID: "voice-audition", ProviderID: "fake-tts", VoiceID: "voice-1", Name: "Voice 1", Language: "vi"},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/"+assetID+"/voice-audition", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("audition status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		AuditionResult domain.VoiceAuditionResult `json:"audition_result"`
		AudioDataURL   string                     `json:"audio_data_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	const prefix = "data:audio/wav;base64,"
	if !strings.HasPrefix(payload.AudioDataURL, prefix) {
		t.Fatalf("audio_data_url missing playable WAV prefix")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(payload.AudioDataURL, prefix))
	if err != nil || !bytes.Equal(decoded, wav) {
		t.Fatalf("decoded audition audio did not match synthesized WAV: %v", err)
	}
	if payload.AuditionResult.AudioCASPath != "" {
		t.Fatalf("audio_cas_path leaked to browser: %q", payload.AuditionResult.AudioCASPath)
	}
	if strings.Contains(rec.Body.String(), root) || strings.Contains(rec.Body.String(), "secret-source.mp4") {
		t.Fatalf("response leaked a machine-local filesystem path: %s", rec.Body.String())
	}
}

func TestVoiceAuditionContextualRequiresRunID(t *testing.T) {
	t.Parallel()
	wav := media.GeneratePCM16WAV(16000, 1, 500)
	s, assetID, _ := newAuditionTestServer(t, wav)
	body, err := json.Marshal(map[string]any{
		"target_language": "vi", "is_contextual": true, "segment_index": 0,
		"voice": domain.VoiceProfile{ID: "voice-audition", ProviderID: "fake-tts", VoiceID: "voice-1", Name: "Voice 1", Language: "vi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/"+assetID+"/voice-audition", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "run_id is required") {
		t.Fatalf("contextual audition without run_id status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOperatorUIUploadsLocalMediaWithoutExposingFilesystemPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}
	ingest := service.NewIngestService(db, casStore, &media.MockProber{CustomReport: &domain.PreflightReport{
		ContainerValid:         true,
		FingerprintMatch:       true,
		ContainerFormat:        "mp4",
		NormalizedAudioSHA256:  "normalized-audio-sha",
		NormalizedAudioCASPath: "cas/normalized-audio",
	}})
	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore, Ingest: ingest})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "sample.mp4")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write([]byte("fake mp4 payload for browser upload")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	for key, value := range map[string]string{
		"declared_by":    "ui-test-operator",
		"terms_accepted": "true",
		"notes":          "browser upload acceptance",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write field %s: %v", key, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"sample.mp4", "ui-test-operator", "browser upload acceptance"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response missing %q: %s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), root) {
		t.Fatalf("response leaked server filesystem path %q: %s", root, rec.Body.String())
	}
}

func TestRenderMediaRoutesStreamOnlyIndexedOutputCAS(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(root)
	if err != nil {
		t.Fatalf("new cas store: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()
	attestation := domain.RightsAttestation{
		ID:              "att-ui-media",
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "server-test",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, attestation); err != nil {
		t.Fatalf("create attestation: %v", err)
	}
	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  "asset-ui-media",
		SHA256:              strings.Repeat("a", 64),
		ByteSize:            16,
		MimeType:            "video/mp4",
		OriginalFilename:    "source.mp4",
		RightsAttestationID: attestation.ID,
		CASPath:             filepath.Join(root, "source.mp4"),
		CreatedAt:           now,
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	mediaBytes := []byte("0123456789abcdef")
	mediaObject, err := casStore.Put(bytes.NewReader(mediaBytes))
	if err != nil {
		t.Fatalf("put media object: %v", err)
	}

	for i, kind := range []string{domain.RenderKindPreview, domain.RenderKindFinal} {
		marker := byte('b' + i)
		if err := db.SaveRenderArtifactIndex(ctx, storage.RenderArtifactIndex{
			ID:             "idx-" + kind,
			AssetID:        "asset-ui-media",
			RunID:          "run-ui-media",
			JobID:          "job-ui-media",
			TargetLanguage: "vi",
			Kind:           kind,
			PlanProvenance: strings.Repeat(string(marker), 64),
			PlanCASHash:    strings.Repeat("d", 64),
			OutputCASHash:  mediaObject.SHA256,
			CASHash:        strings.Repeat("e", 64),
			ProvenanceHash: strings.Repeat(string(marker+2), 64),
			OverallStatus:  "PASS",
			CreatedAt:      now.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("save %s render index: %v", kind, err)
		}
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore})

	t.Run("preview supports byte ranges", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/asset-ui-media/render/preview/media?target_language=vi", nil)
		req.Header.Set("Range", "bytes=2-5")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != 206 {
			t.Fatalf("status = %d, want 206; body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
			t.Fatalf("Content-Type = %q, want video/mp4", got)
		}
		if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/16" {
			t.Fatalf("Content-Range = %q, want bytes 2-5/16", got)
		}
		if got, want := rec.Body.String(), string(mediaBytes[2:6]); got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
	})

	t.Run("final streams full indexed media", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/asset-ui-media/render/final/media?target_language=vi", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != 200 {
			t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
		}
		if got := rec.Body.Bytes(); !bytes.Equal(got, mediaBytes) {
			t.Fatalf("body = %q, want %q", got, mediaBytes)
		}
	})

	t.Run("unindexed asset stays inaccessible", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/other/render/preview/media?target_language=vi", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestRenderMetadataAndMediaRunBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	casDir := filepath.Join(tmpDir, "cas")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(casDir)
	if err != nil {
		t.Fatalf("new cas store: %v", err)
	}

	now := time.Now().UTC()
	ra := domain.RightsAttestation{
		ID:              "att-binding",
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		Notes:           "test",
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}

	if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  "asset-binding",
		SHA256:              strings.Repeat("1", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "clip.mp4",
		RightsAttestationID: ra.ID,
		CASPath:             "clip.mp4",
		CreatedAt:           now,
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	mediaRun1Bytes := []byte("MEDIA_RUN_1_CONTENT")
	objRun1, err := casStore.Put(bytes.NewReader(mediaRun1Bytes))
	if err != nil {
		t.Fatalf("put run 1 media: %v", err)
	}
	mediaRun2Bytes := []byte("MEDIA_RUN_2_CONTENT")
	objRun2, err := casStore.Put(bytes.NewReader(mediaRun2Bytes))
	if err != nil {
		t.Fatalf("put run 2 media: %v", err)
	}

	// Seed artifacts in CAS
	prev1 := domain.PreviewRenderArtifact{
		ID:             "preview-art-run1",
		AssetID:        "asset-binding",
		RunID:          "run-1",
		TargetLanguage: "vi",
		OutputCASHash:  objRun1.SHA256,
		OverallStatus:  "PASS",
		CreatedAt:      now.Add(1 * time.Second),
	}
	p1Bytes, _ := json.Marshal(prev1)
	casP1, err := casStore.Put(bytes.NewReader(p1Bytes))
	if err != nil {
		t.Fatalf("put p1: %v", err)
	}

	prev2 := domain.PreviewRenderArtifact{
		ID:             "preview-art-run2",
		AssetID:        "asset-binding",
		RunID:          "run-2",
		TargetLanguage: "vi",
		OutputCASHash:  objRun2.SHA256,
		OverallStatus:  "PASS",
		CreatedAt:      now.Add(2 * time.Second),
	}
	p2Bytes, _ := json.Marshal(prev2)
	casP2, err := casStore.Put(bytes.NewReader(p2Bytes))
	if err != nil {
		t.Fatalf("put p2: %v", err)
	}

	fin1 := domain.FinalRenderArtifact{
		ID:             "final-art-run1",
		AssetID:        "asset-binding",
		RunID:          "run-1",
		TargetLanguage: "vi",
		OutputCASHash:  objRun1.SHA256,
		OverallStatus:  "PASS",
		CreatedAt:      now.Add(1 * time.Second),
	}
	f1Bytes, _ := json.Marshal(fin1)
	casF1, err := casStore.Put(bytes.NewReader(f1Bytes))
	if err != nil {
		t.Fatalf("put f1: %v", err)
	}

	fin2 := domain.FinalRenderArtifact{
		ID:             "final-art-run2",
		AssetID:        "asset-binding",
		RunID:          "run-2",
		TargetLanguage: "vi",
		OutputCASHash:  objRun2.SHA256,
		OverallStatus:  "PASS",
		CreatedAt:      now.Add(2 * time.Second),
	}
	f2Bytes, _ := json.Marshal(fin2)
	casF2, err := casStore.Put(bytes.NewReader(f2Bytes))
	if err != nil {
		t.Fatalf("put f2: %v", err)
	}

	// Save DB indices: Run 1 older, Run 2 newer
	if err := db.SaveRenderArtifactIndex(ctx, storage.RenderArtifactIndex{
		ID:             "idx-p-run1",
		AssetID:        "asset-binding",
		RunID:          "run-1",
		JobID:          "job-1",
		TargetLanguage: "vi",
		Kind:           domain.RenderKindPreview,
		OutputCASHash:  objRun1.SHA256,
		CASHash:        casP1.SHA256,
		ProvenanceHash: strings.Repeat("a", 64),
		OverallStatus:  "PASS",
		CreatedAt:      now.Add(1 * time.Second),
	}); err != nil {
		t.Fatalf("save idx p1: %v", err)
	}
	if err := db.SaveRenderArtifactIndex(ctx, storage.RenderArtifactIndex{
		ID:             "idx-f-run1",
		AssetID:        "asset-binding",
		RunID:          "run-1",
		JobID:          "job-1",
		TargetLanguage: "vi",
		Kind:           domain.RenderKindFinal,
		OutputCASHash:  objRun1.SHA256,
		CASHash:        casF1.SHA256,
		ProvenanceHash: strings.Repeat("b", 64),
		OverallStatus:  "PASS",
		CreatedAt:      now.Add(1 * time.Second),
	}); err != nil {
		t.Fatalf("save idx f1: %v", err)
	}
	if err := db.SaveRenderArtifactIndex(ctx, storage.RenderArtifactIndex{
		ID:             "idx-p-run2",
		AssetID:        "asset-binding",
		RunID:          "run-2",
		JobID:          "job-1",
		TargetLanguage: "vi",
		Kind:           domain.RenderKindPreview,
		OutputCASHash:  objRun2.SHA256,
		CASHash:        casP2.SHA256,
		ProvenanceHash: strings.Repeat("c", 64),
		OverallStatus:  "PASS",
		CreatedAt:      now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("save idx p2: %v", err)
	}
	if err := db.SaveRenderArtifactIndex(ctx, storage.RenderArtifactIndex{
		ID:             "idx-f-run2",
		AssetID:        "asset-binding",
		RunID:          "run-2",
		JobID:          "job-1",
		TargetLanguage: "vi",
		Kind:           domain.RenderKindFinal,
		OutputCASHash:  objRun2.SHA256,
		CASHash:        casF2.SHA256,
		ProvenanceHash: strings.Repeat("d", 64),
		OverallStatus:  "PASS",
		CreatedAt:      now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("save idx f2: %v", err)
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore})

	t.Run("omitted run_id falls back to latest (run 2)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/asset-binding/render/preview?target_language=vi", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d", rec.Code)
		}
		var resp map[string]domain.PreviewRenderArtifact
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal preview: %v", err)
		}
		if resp["preview_render"].RunID != "run-2" {
			t.Fatalf("got preview run_id %q, want run-2", resp["preview_render"].RunID)
		}
	})

	t.Run("explicit run_id returns run 1 metadata", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/asset-binding/render/preview?target_language=vi&run_id=run-1", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d", rec.Code)
		}
		var resp map[string]domain.PreviewRenderArtifact
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal preview: %v", err)
		}
		if resp["preview_render"].RunID != "run-1" {
			t.Fatalf("got preview run_id %q, want run-1", resp["preview_render"].RunID)
		}

		reqFinal := httptest.NewRequest("GET", "/api/v1/assets/asset-binding/render/final?target_language=vi&run_id=run-1", nil)
		recFinal := httptest.NewRecorder()
		s.Handler().ServeHTTP(recFinal, reqFinal)
		if recFinal.Code != http.StatusOK {
			t.Fatalf("final status = %d", recFinal.Code)
		}
		var respFinal map[string]domain.FinalRenderArtifact
		if err := json.Unmarshal(recFinal.Body.Bytes(), &respFinal); err != nil {
			t.Fatalf("unmarshal final: %v", err)
		}
		if respFinal["final_render"].RunID != "run-1" {
			t.Fatalf("got final run_id %q, want run-1", respFinal["final_render"].RunID)
		}
	})

	t.Run("explicit run_id streams bound media content", func(t *testing.T) {
		reqMedia := httptest.NewRequest("GET", "/api/v1/assets/asset-binding/render/preview/media?target_language=vi&run_id=run-1", nil)
		recMedia := httptest.NewRecorder()
		s.Handler().ServeHTTP(recMedia, reqMedia)
		if recMedia.Code != http.StatusOK {
			t.Fatalf("preview media status = %d", recMedia.Code)
		}
		if !bytes.Equal(recMedia.Body.Bytes(), mediaRun1Bytes) {
			t.Fatalf("preview media got %q, want %q", recMedia.Body.String(), string(mediaRun1Bytes))
		}

		reqMediaFinal := httptest.NewRequest("GET", "/api/v1/assets/asset-binding/render/final/media?target_language=vi&run_id=run-1", nil)
		recMediaFinal := httptest.NewRecorder()
		s.Handler().ServeHTTP(recMediaFinal, reqMediaFinal)
		if recMediaFinal.Code != http.StatusOK {
			t.Fatalf("final media status = %d", recMediaFinal.Code)
		}
		if !bytes.Equal(recMediaFinal.Body.Bytes(), mediaRun1Bytes) {
			t.Fatalf("final media got %q, want %q", recMediaFinal.Body.String(), string(mediaRun1Bytes))
		}
	})

	t.Run("nonexistent run_id returns 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/assets/asset-binding/render/preview?target_language=vi&run_id=run-nonexistent", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestUploadAssetFilenameSanitization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "upload_test.db")
	casDir := filepath.Join(tmpDir, "cas")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(casDir)
	if err != nil {
		t.Fatalf("new cas store: %v", err)
	}

	ingestSvc := service.NewIngestService(db, casStore, &media.MockProber{CustomReport: &domain.PreflightReport{
		ContainerValid:         true,
		FingerprintMatch:       true,
		ContainerFormat:        "mp4",
		NormalizedAudioSHA256:  "normalized-audio-sha",
		NormalizedAudioCASPath: "cas/normalized-audio",
	}})
	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore, Ingest: ingestSvc})

	// Create sample MP4 payload
	var mp4Buf bytes.Buffer
	mp4Buf.WriteString("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2mp41\x00\x00\x00\x08free")

	// Test Windows backslash path
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("terms_accepted", "true")
	_ = writer.WriteField("declared_by", "Alice")
	part, err := writer.CreateFormFile("file", `C:\Users\Alice\video.mp4`)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = part.Write(mp4Buf.Bytes())
	_ = writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/assets/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%q", rec.Code, rec.Body.String())
	}

	var resp struct {
		Asset struct {
			ID               string `json:"id"`
			OriginalFilename string `json:"original_filename"`
		} `json:"asset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if resp.Asset.OriginalFilename != "video.mp4" {
		t.Fatalf("got original_filename %q, want video.mp4", resp.Asset.OriginalFilename)
	}

	// Verify in DB as well
	storedAsset, err := db.GetSourceAsset(ctx, resp.Asset.ID)
	if err != nil {
		t.Fatalf("get source asset: %v", err)
	}
	if storedAsset.OriginalFilename != "video.mp4" {
		t.Fatalf("db original_filename %q, want video.mp4", storedAsset.OriginalFilename)
	}
}

func TestExecuteRun_StageFailureRecordsEvidenceAndInterrupts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "autorun_err_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	now := time.Now().UTC()
	ra := domain.RightsAttestation{
		ID:              "att-err",
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		Notes:           "test",
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}

	asset := domain.SourceAsset{
		ID:                  "test-asset-err",
		SHA256:              strings.Repeat("1", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "dummy.mp4",
		RightsAttestationID: ra.ID,
		CASPath:             "dummy.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("save asset: %v", err)
	}

	job := domain.LocalizationJob{
		ID:             "test-job-err",
		SourceAssetID:  asset.ID,
		TargetLanguage: domain.TargetLanguageVI,
		Status:         "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := domain.LocalizationRun{
		ID:        "test-run-err",
		JobID:     job.ID,
		Status:    domain.RunStatusRunning,
		CreatedAt: now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	qSvc := queue.NewService(db)
	if _, err := qSvc.Enqueue(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, run.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Seed a zero-dialogue AudioRolePlan so audio_role_plan succeeds,
	// but leave AudioMixSvc nil in server Config so audio_mix stage fails.
	rolePlan := &domain.AudioRolePlan{
		ID:      "plan-1",
		AssetID: asset.ID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleInstrumentalBgm},
		},
		CreatedAt: now,
	}
	if err := db.SaveAudioRolePlan(ctx, *rolePlan); err != nil {
		t.Fatalf("save audio role plan: %v", err)
	}

	s := New(Config{
		Addr:     "127.0.0.1:0",
		DB:       db,
		CASStore: casStore,
		QueueSvc: qSvc,
	})

	// executeRun should return nil because the service failure was successfully and durably
	// recorded as failed stage evidence and the queue status updated to interrupted.
	if err := s.executeRun(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("expected executeRun to return nil on durably recorded failure, got: %v", err)
	}

	// Verify queue entry was interrupted
	entry, err := db.GetQueueEntryByRunID(ctx, run.ID)
	if err != nil {
		t.Fatalf("get queue entry: %v", err)
	}
	if entry.Status != domain.RunStatusInterrupted {
		t.Fatalf("expected queue entry %s, got %s", domain.RunStatusInterrupted, entry.Status)
	}

	// Verify failed diagnostic stage execution exists
	stages, err := db.ListStageExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("expected 1 stage execution, got %d", len(stages))
	}
	if stages[0].Stage != "audio_mix" || stages[0].Status != domain.StageStatusFailed {
		t.Fatalf("expected failed audio_mix stage, got %+v", stages[0])
	}
}

func TestExecuteRun_RunScopedAudioRolePlan_NeverAdoptsNewerNoDubPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "execute_run_rolediv.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(tmpDir, "cas"))
	if err != nil {
		t.Fatalf("new cas: %v", err)
	}

	qSvc := queue.NewService(db)
	now := time.Now().UTC()

	att := domain.RightsAttestation{
		ID:              "att-1",
		AttestationType: "USER_VERIFIED",
		DeclaredBy:      "tester",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, att); err != nil {
		t.Fatalf("create attestation: %v", err)
	}

	rawBytes := []byte("fake audio content for normalization")
	cObj, err := casStore.Put(bytes.NewReader(rawBytes))
	if err != nil {
		t.Fatalf("put raw bytes: %v", err)
	}

	asset := domain.SourceAsset{
		ID:                  "asset-rolediv",
		SHA256:              cObj.SHA256,
		CASPath:             cObj.Path,
		RightsAttestationID: att.ID,
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	job := domain.LocalizationJob{
		ID:             "job-rolediv",
		SourceAssetID:  asset.ID,
		TargetLanguage: "vi",
		Status:         "queued",
		CreatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := domain.LocalizationRun{
		ID:                 "run-rolediv",
		JobID:              job.ID,
		Status:             domain.RunStatusQueued,
		ConfigSnapshotJSON: "{}",
		CreatedAt:          now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	if _, err := qSvc.Enqueue(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, run.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// 1. Pinned AudioRolePlan A with dub-eligible dialogue
	planA := domain.AudioRolePlan{
		ID:      "planA",
		AssetID: asset.ID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 5000, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: now,
	}
	planABytes, _ := json.Marshal(planA)
	planAObj, err := casStore.Put(bytes.NewReader(planABytes))
	if err != nil {
		t.Fatalf("put planA: %v", err)
	}
	planA.CASHash = planAObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, planA); err != nil {
		t.Fatalf("save planA: %v", err)
	}
	// Pin Plan A to Run A via stage_executions
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             "se-audio-role-pin",
		RunID:          run.ID,
		Stage:          "audio_role_plan",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: planAObj.SHA256,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("record audio_role_plan stage: %v", err)
	}

	// 2. Save newer AudioRolePlan B for asset with NO dialogue
	planB := domain.AudioRolePlan{
		ID:      "planB",
		AssetID: asset.ID,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 35000, Role: domain.AudioRoleInstrumentalBgm},
		},
		CreatedAt: now.Add(time.Minute),
	}
	planBBytes, _ := json.Marshal(planB)
	planBObj, err := casStore.Put(bytes.NewReader(planBBytes))
	if err != nil {
		t.Fatalf("put planB: %v", err)
	}
	planB.CASHash = planBObj.SHA256
	if err := db.SaveAudioRolePlan(ctx, planB); err != nil {
		t.Fatalf("save planB: %v", err)
	}

	// 3. executeRun for Run A
	// Without SpeechSvc configured:
	// - If Run A uses Plan A (dub-eligible), it attempts speech_understand and fails with "SpeechService is not configured"
	// - If Run A adopted Plan B (no dialogue), it would bypass speech_understand, translation, and dub_synthesize, jumping to audio_mix
	s := New(Config{
		Addr:     "127.0.0.1:0",
		DB:       db,
		CASStore: casStore,
		QueueSvc: qSvc,
	})

	_ = s.executeRun(ctx, run.ID, job.ID)

	stages, err := db.ListStageExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}

	foundSpeechStage := false
	for _, st := range stages {
		if st.Stage == "speech_understand" {
			foundSpeechStage = true
			if st.Status != domain.StageStatusFailed {
				t.Errorf("expected speech_understand stage to fail closed, got status: %s", st.Status)
			}
		}
		if st.Stage == "audio_mix" {
			t.Fatalf("executeRun incorrectly skipped speech understanding and jumped to audio_mix (it adopted no-dub Plan B!)")
		}
	}
	if !foundSpeechStage {
		t.Fatalf("expected speech_understand stage to be attempted under Plan A, but it was not found in stages: %+v", stages)
	}
}

// A run leaves `review_required` behind when a blocked final render handoff interrupts it.
// Once the operator clears the queue and the handoff passes, the job must follow its run.
func TestCompleteRunSafely_CompletesTheJobItFinished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "complete_run_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	ra := domain.RightsAttestation{
		ID:              "att-complete",
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	asset := domain.SourceAsset{
		ID:                  "test-asset-complete",
		SHA256:              strings.Repeat("2", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "dummy.mp4",
		RightsAttestationID: ra.ID,
		CASPath:             "dummy.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("save asset: %v", err)
	}

	job := domain.LocalizationJob{
		ID:             "test-job-complete",
		SourceAssetID:  asset.ID,
		TargetLanguage: domain.TargetLanguageVI,
		Status:         "review_required",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := domain.LocalizationRun{ID: "test-run-complete", JobID: job.ID, Status: domain.RunStatusRunning, CreatedAt: now}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	qSvc := queue.NewService(db)
	if _, err := qSvc.Enqueue(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, run.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, QueueSvc: qSvc})
	if err := s.completeRunSafely(ctx, run.ID); err != nil {
		t.Fatalf("completeRunSafely: %v", err)
	}

	entry, err := db.GetQueueEntryByRunID(ctx, run.ID)
	if err != nil {
		t.Fatalf("get queue entry: %v", err)
	}
	if entry.Status != domain.RunStatusCompleted {
		t.Fatalf("expected queue entry %s, got %s", domain.RunStatusCompleted, entry.Status)
	}

	got, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != "completed" {
		t.Errorf("job status after a completed run = %q, want completed", got.Status)
	}
}

// In Review posture, the handoff only exposes 'start_final_render' and does not execute
// a render. completeRunSafely must complete the run without marking the job 'completed',
// leaving it open until the explicit final render succeeds.
func TestCompleteRunSafely_ReviewPostureLeavesJobOpenUntilExplicitRender(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "review_posture_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	ra := domain.RightsAttestation{
		ID:              "att-review",
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	asset := domain.SourceAsset{
		ID:                  "asset-review",
		SHA256:              strings.Repeat("3", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "dummy.mp4",
		RightsAttestationID: ra.ID,
		CASPath:             "dummy.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("save asset: %v", err)
	}

	job := domain.LocalizationJob{
		ID:             "job-review",
		SourceAssetID:  asset.ID,
		TargetLanguage: domain.TargetLanguageVI,
		Status:         "review_required",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := domain.LocalizationRun{
		ID:                 "run-review",
		JobID:              job.ID,
		Status:             domain.RunStatusRunning,
		ConfigSnapshotJSON: `{"posture":"review"}`,
		CreatedAt:          now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	qSvc := queue.NewService(db)
	if _, err := qSvc.Enqueue(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, run.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, QueueSvc: qSvc})
	if err := s.completeRunSafely(ctx, run.ID); err != nil {
		t.Fatalf("completeRunSafely: %v", err)
	}

	// Queue entry must be completed
	entry, err := db.GetQueueEntryByRunID(ctx, run.ID)
	if err != nil {
		t.Fatalf("get queue entry: %v", err)
	}
	if entry.Status != domain.RunStatusCompleted {
		t.Fatalf("expected queue entry %s, got %s", domain.RunStatusCompleted, entry.Status)
	}

	// Job status must NOT be completed while explicit render is pending
	got, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status == "completed" {
		t.Fatalf("job status was marked completed before explicit final render")
	}
	if got.Status != "review_required" {
		t.Errorf("job status = %q, want review_required", got.Status)
	}

}

// If UpdateJobStatus fails, completeRunSafely must surface the error and NOT mark the
// run or queue as completed.
func TestCompleteRunSafely_UpdateJobStatusFailureSurfacesError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "job_failure_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	ra := domain.RightsAttestation{
		ID:              "att-fail",
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	asset := domain.SourceAsset{
		ID:                  "asset-fail",
		SHA256:              strings.Repeat("4", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "dummy.mp4",
		RightsAttestationID: ra.ID,
		CASPath:             "dummy.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("save asset: %v", err)
	}

	job := domain.LocalizationJob{
		ID:             "job-fail",
		SourceAssetID:  asset.ID,
		TargetLanguage: domain.TargetLanguageVI,
		Status:         "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	run := domain.LocalizationRun{
		ID:                 "run-fail",
		JobID:              job.ID,
		Status:             domain.RunStatusRunning,
		ConfigSnapshotJSON: `{"posture":"auto"}`,
		CreatedAt:          now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	qSvc := queue.NewService(db)
	if _, err := qSvc.Enqueue(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, run.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Inject failure via SQLite trigger on localization_jobs UPDATE
	rawDB, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(5000)", dbPath))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer rawDB.Close()

	_, err = rawDB.ExecContext(ctx, `CREATE TRIGGER fail_job_update BEFORE UPDATE ON localization_jobs BEGIN SELECT RAISE(ABORT, 'injected job update failure'); END;`)
	if err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, QueueSvc: qSvc})
	err = s.completeRunSafely(ctx, run.ID)
	if err == nil {
		t.Fatal("expected completeRunSafely to fail when UpdateJobStatus fails, got nil")
	}
	if !strings.Contains(err.Error(), "injected job update failure") {
		t.Errorf("expected error to contain trigger message, got: %v", err)
	}

	// Queue entry must NOT be completed
	entry, err := db.GetQueueEntryByRunID(ctx, run.ID)
	if err != nil {
		t.Fatalf("get queue entry: %v", err)
	}
	if entry.Status == domain.RunStatusCompleted {
		t.Errorf("queue entry must not be marked completed when job update fails, got %s", entry.Status)
	}
}

func TestOperatorUIRuntimeLifecycleAndPostureContracts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}
	ingest := service.NewIngestService(db, casStore, &media.MockProber{CustomReport: &domain.PreflightReport{
		ContainerValid:         true,
		FingerprintMatch:       true,
		ContainerFormat:        "mp4",
		NormalizedAudioSHA256:  "normalized-audio-sha",
		NormalizedAudioCASPath: "cas/normalized-audio",
	}})
	qSvc := queue.NewService(db)
	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore, Ingest: ingest, QueueSvc: qSvc})

	ctx := context.Background()
	now := time.Now().UTC()

	attestation := domain.RightsAttestation{
		ID:              "attestation-ui-1",
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "ui-operator",
		TermsAccepted:   true,
		Notes:           "operator ui contract test",
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, attestation); err != nil {
		t.Fatalf("create attestation: %v", err)
	}

	asset := domain.SourceAsset{
		ID:                  "asset-ui-1",
		SHA256:              "asset-sha-ui-1",
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "source.mp4",
		RightsAttestationID: attestation.ID,
		CASPath:             "source.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	// 2. Create Job
	job := domain.LocalizationJob{
		ID:             "job-ui-1",
		SourceAssetID:  asset.ID,
		TargetLanguage: "vi",
		Status:         "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	// 3. POST /api/v1/jobs/{id}/runs with posture="review"
	run1ReqBody := `{"posture":"review","review_posture":"review","config_snapshot_json":"{\"profile\":\"hybrid\",\"source\":\"operator-ui\"}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+job.ID+"/runs", strings.NewReader(run1ReqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create run 1 status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var run1Resp struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&run1Resp); err != nil {
		t.Fatalf("decode run 1: %v", err)
	}
	if !strings.Contains(run1Resp.Run.ConfigSnapshotJSON, `"posture":"review"`) {
		t.Fatalf("expected run 1 config snapshot to contain posture:review, got %s", run1Resp.Run.ConfigSnapshotJSON)
	}

	// 4. POST /api/v1/jobs/{id}/runs with posture="auto"
	run2ReqBody := `{"posture":"auto","config_snapshot_json":"{\"profile\":\"hybrid\"}"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+job.ID+"/runs", strings.NewReader(run2ReqBody))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusCreated {
		t.Fatalf("create run 2 status = %d, want 201; body=%s", rec2.Code, rec2.Body.String())
	}
	var run2Resp struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&run2Resp); err != nil {
		t.Fatalf("decode run 2: %v", err)
	}
	if !strings.Contains(run2Resp.Run.ConfigSnapshotJSON, `"posture":"auto"`) {
		t.Fatalf("expected run 2 config snapshot to contain posture:auto, got %s", run2Resp.Run.ConfigSnapshotJSON)
	}

	// 5. GET /api/v1/queue -> verify positions and 2 entries
	qReq := httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil)
	qRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(qRec, qReq)
	if qRec.Code != http.StatusOK {
		t.Fatalf("get queue status = %d, want 200", qRec.Code)
	}
	var qData struct {
		Queue []domain.QueueEntry `json:"queue"`
	}
	if err := json.NewDecoder(qRec.Body).Decode(&qData); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	if len(qData.Queue) != 2 {
		t.Fatalf("expected 2 queue entries, got %d", len(qData.Queue))
	}
	if qData.Queue[0].RunID != run1Resp.Run.ID || qData.Queue[0].Position != 1 {
		t.Fatalf("unexpected entry 0: %+v", qData.Queue[0])
	}
	if qData.Queue[1].RunID != run2Resp.Run.ID || qData.Queue[1].Position != 2 {
		t.Fatalf("unexpected entry 1: %+v", qData.Queue[1])
	}

	// 6. Test Pause on run 1
	pauseReq := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run1Resp.Run.ID+"/pause", strings.NewReader("{}"))
	pauseRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(pauseRec, pauseReq)
	if pauseRec.Code != http.StatusOK {
		t.Fatalf("pause status = %d, want 200; body=%s", pauseRec.Code, pauseRec.Body.String())
	}

	// Verify run status is paused
	rReq := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run1Resp.Run.ID, nil)
	rRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rRec, rReq)
	var rData struct {
		Run domain.LocalizationRun `json:"run"`
	}
	_ = json.NewDecoder(rRec.Body).Decode(&rData)
	if rData.Run.Status != domain.RunStatusPaused {
		t.Fatalf("expected status %s, got %s", domain.RunStatusPaused, rData.Run.Status)
	}

	// 7. Test Resume on run 1
	resumeReq := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run1Resp.Run.ID+"/resume", strings.NewReader("{}"))
	resumeRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(resumeRec, resumeReq)
	if resumeRec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200; body=%s", resumeRec.Code, resumeRec.Body.String())
	}

	// 8. Test Cancel on run 2
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run2Resp.Run.ID+"/cancel", strings.NewReader("{}"))
	cancelRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200; body=%s", cancelRec.Code, cancelRec.Body.String())
	}

	// 9. Test render handoff endpoint for posture evaluation
	handoffReq := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run1Resp.Run.ID+"/render/handoff", strings.NewReader(`{"posture":"review"}`))
	handoffReq.Header.Set("Content-Type", "application/json")
	handoffRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(handoffRec, handoffReq)
	if handoffRec.Code != http.StatusOK {
		t.Fatalf("handoff status = %d, want 200; body=%s", handoffRec.Code, handoffRec.Body.String())
	}
	var handoffData struct {
		Handoff domain.FinalRenderHandoffResult `json:"handoff"`
	}
	_ = json.NewDecoder(handoffRec.Body).Decode(&handoffData)
	if handoffData.Handoff.Posture != domain.ReviewPostureReview {
		t.Fatalf("expected posture review, got %s", handoffData.Handoff.Posture)
	}

	// 10. Verify no filesystem path leakage across endpoints
	for _, b := range []string{rec.Body.String(), rec2.Body.String(), qRec.Body.String(), pauseRec.Body.String(), handoffRec.Body.String()} {
		if strings.Contains(b, root) {
			t.Fatalf("endpoint leaked root directory path %q: %s", root, b)
		}
	}
}

// #108: the console offers Cancel for an interrupted run, so the endpoint the operator's control
// calls must accept that state instead of answering 409 for the very run they are abandoning.
func TestCancelInterruptedRunOverHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	attestation := domain.RightsAttestation{
		ID:              "att-cancel-interrupted",
		AttestationType: "OPERATOR_CONFIRMED",
		DeclaredBy:      "operator",
		TermsAccepted:   true,
		Notes:           "test",
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, attestation); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	asset := domain.SourceAsset{
		ID:                  "asset-cancel-interrupted",
		SHA256:              strings.Repeat("9", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "interrupted.mp4",
		RightsAttestationID: attestation.ID,
		CASPath:             "interrupted.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("save asset: %v", err)
	}
	job := domain.LocalizationJob{
		ID:             "job-cancel-interrupted",
		SourceAssetID:  asset.ID,
		TargetLanguage: domain.TargetLanguageVI,
		Status:         "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	run := domain.LocalizationRun{
		ID:        "run-cancel-interrupted",
		JobID:     job.ID,
		Status:    domain.RunStatusRunning,
		CreatedAt: now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	qSvc := queue.NewService(db)
	if _, err := qSvc.Enqueue(ctx, run.ID, job.ID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := qSvc.MarkRunning(ctx, run.ID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	// A fail-closed run (the final-render handoff is the normal path into this state) leaves its
	// queue entry interrupted while the run row keeps its own interrupted status.
	if err := db.UpdateQueueStatus(ctx, run.ID, domain.RunStatusInterrupted, domain.RunStatusInterrupted); err != nil {
		t.Fatalf("interrupt run: %v", err)
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, QueueSvc: qSvc})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run.ID+"/cancel", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel interrupted run status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	entry, err := db.GetQueueEntryByRunID(ctx, run.ID)
	if err != nil {
		t.Fatalf("get queue entry: %v", err)
	}
	if entry.Status != domain.RunStatusCancelled {
		t.Fatalf("expected queue entry %s after cancel, got %s", domain.RunStatusCancelled, entry.Status)
	}
	cancelled, err := db.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if cancelled.Status != domain.RunStatusCancelled {
		t.Fatalf("expected run %s after cancel, got %s", domain.RunStatusCancelled, cancelled.Status)
	}

	// Cancelled is terminal: a second cancel must stay refused rather than silently succeeding.
	again := httptest.NewRecorder()
	s.Handler().ServeHTTP(again, httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+run.ID+"/cancel", strings.NewReader("{}")))
	if again.Code != http.StatusConflict {
		t.Fatalf("re-cancel of a cancelled run status = %d, want %d", again.Code, http.StatusConflict)
	}
}

func TestTranslationAndVoiceAssignmentRunIsolation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore})
	ctx := context.Background()
	now := time.Now().UTC()

	attestation := domain.RightsAttestation{
		ID:              "attestation-iso-1",
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "tester",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	if err := db.CreateRightsAttestation(ctx, attestation); err != nil {
		t.Fatalf("create attestation: %v", err)
	}

	asset := domain.SourceAsset{
		ID:                  "asset-iso-1",
		SHA256:              "sha-iso-1",
		ByteSize:            100,
		MimeType:            "video/mp4",
		OriginalFilename:    "sample.mp4",
		RightsAttestationID: attestation.ID,
		CASPath:             "sample.mp4",
		CreatedAt:           now,
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("create asset: %v", err)
	}

	job := domain.LocalizationJob{
		ID:             "job-iso-1",
		SourceAssetID:  asset.ID,
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	run1 := domain.LocalizationRun{
		ID:        "run-iso-1",
		JobID:     job.ID,
		Status:    "completed",
		CreatedAt: now,
	}
	if err := db.CreateRun(ctx, run1); err != nil {
		t.Fatalf("create run1: %v", err)
	}

	run2 := domain.LocalizationRun{
		ID:        "run-iso-2",
		JobID:     job.ID,
		Status:    "running",
		CreatedAt: now.Add(time.Minute),
	}
	if err := db.CreateRun(ctx, run2); err != nil {
		t.Fatalf("create run2: %v", err)
	}

	// Seed TranscriptArtifact for both runs so selected-run inspector reads can
	// prove they do not fall back to the latest same-asset transcript.
	transcript1 := domain.TranscriptArtifact{
		ID:             "transcript-1",
		AssetID:        asset.ID,
		RunID:          run1.ID,
		SourceLanguage: "zh",
		ASRProviderID:  "asr-run-1",
		CreatedAt:      now,
	}
	transcript1Bytes, _ := json.Marshal(transcript1)
	casTranscript1, err := casStore.Put(bytes.NewReader(transcript1Bytes))
	if err != nil {
		t.Fatalf("put transcript1: %v", err)
	}
	if err := db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID:             transcript1.ID,
		AssetID:        asset.ID,
		RunID:          run1.ID,
		CASHash:        casTranscript1.SHA256,
		ProvenanceHash: "prov-transcript-1",
		ASRProviderID:  "asr-run-1",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("save transcript1 index: %v", err)
	}

	transcript2 := domain.TranscriptArtifact{
		ID:             "transcript-2",
		AssetID:        asset.ID,
		RunID:          run2.ID,
		SourceLanguage: "zh",
		ASRProviderID:  "asr-run-2",
		CreatedAt:      now.Add(time.Minute),
	}
	transcript2Bytes, _ := json.Marshal(transcript2)
	casTranscript2, err := casStore.Put(bytes.NewReader(transcript2Bytes))
	if err != nil {
		t.Fatalf("put transcript2: %v", err)
	}
	if err := db.SaveTranscriptArtifactIndex(ctx, storage.TranscriptArtifactIndex{
		ID:             transcript2.ID,
		AssetID:        asset.ID,
		RunID:          run2.ID,
		CASHash:        casTranscript2.SHA256,
		ProvenanceHash: "prov-transcript-2",
		ASRProviderID:  "asr-run-2",
		CreatedAt:      now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("save transcript2 index: %v", err)
	}

	// Seed TranslationVariant for run 1
	trans1 := domain.TranslationVariant{
		ID:             "trans-var-1",
		AssetID:        asset.ID,
		RunID:          run1.ID,
		JobID:          job.ID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "你好", TargetText: "Xin chào từ Run 1", StartMs: 0, EndMs: 1000},
		},
	}
	t1Bytes, _ := json.Marshal(trans1)
	casT1, err := casStore.Put(bytes.NewReader(t1Bytes))
	if err != nil {
		t.Fatalf("put trans1: %v", err)
	}
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             trans1.ID,
		AssetID:        asset.ID,
		RunID:          run1.ID,
		JobID:          job.ID,
		TargetLanguage: "vi",
		CASHash:        casT1.SHA256,
		ProvenanceHash: "prov-trans-1",
		ProviderID:     "gemini",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("save trans1 index: %v", err)
	}

	// Seed TranslationVariant for run 2
	trans2 := domain.TranslationVariant{
		ID:             "trans-var-2",
		AssetID:        asset.ID,
		RunID:          run2.ID,
		JobID:          job.ID,
		TargetLanguage: "vi",
		SourceLanguage: "zh",
		Segments: []domain.TranslationSegment{
			{Index: 0, SourceText: "你好", TargetText: "Xin chào từ Run 2", StartMs: 0, EndMs: 1000},
		},
	}
	t2Bytes, _ := json.Marshal(trans2)
	casT2, err := casStore.Put(bytes.NewReader(t2Bytes))
	if err != nil {
		t.Fatalf("put trans2: %v", err)
	}
	if err := db.SaveTranslationVariantIndex(ctx, storage.TranslationVariantIndex{
		ID:             trans2.ID,
		AssetID:        asset.ID,
		RunID:          run2.ID,
		JobID:          job.ID,
		TargetLanguage: "vi",
		CASHash:        casT2.SHA256,
		ProvenanceHash: "prov-trans-2",
		ProviderID:     "gemini",
		CreatedAt:      now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("save trans2 index: %v", err)
	}

	// Seed VoiceAssignment for run 1
	va1 := domain.VoiceAssignment{
		ID:             "va-1",
		AssetID:        asset.ID,
		RunID:          run1.ID,
		JobID:          job.ID,
		TargetLanguage: "vi",
		Assignments: map[string]domain.VoiceProfile{
			"spk_0": {ID: "profile-1", ProviderID: "vieneu", VoiceID: "voice-run-1", Name: "Voice Run 1", Language: "vi"},
		},
	}
	va1Bytes, _ := json.Marshal(va1)
	casVA1, err := casStore.Put(bytes.NewReader(va1Bytes))
	if err != nil {
		t.Fatalf("put va1: %v", err)
	}
	if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID:             va1.ID,
		AssetID:        asset.ID,
		RunID:          run1.ID,
		JobID:          job.ID,
		TargetLanguage: "vi",
		CASHash:        casVA1.SHA256,
		ProvenanceHash: "prov-va-1",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("save va1 index: %v", err)
	}

	// Seed VoiceAssignment for run 2
	va2 := domain.VoiceAssignment{
		ID:             "va-2",
		AssetID:        asset.ID,
		RunID:          run2.ID,
		JobID:          job.ID,
		TargetLanguage: "vi",
		Assignments: map[string]domain.VoiceProfile{
			"spk_0": {ID: "profile-2", ProviderID: "cosyvoice", VoiceID: "voice-run-2", Name: "Voice Run 2", Language: "vi"},
		},
	}
	va2Bytes, _ := json.Marshal(va2)
	casVA2, err := casStore.Put(bytes.NewReader(va2Bytes))
	if err != nil {
		t.Fatalf("put va2: %v", err)
	}
	if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
		ID:             va2.ID,
		AssetID:        asset.ID,
		RunID:          run2.ID,
		JobID:          job.ID,
		TargetLanguage: "vi",
		CASHash:        casVA2.SHA256,
		ProvenanceHash: "prov-va-2",
		CreatedAt:      now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("save va2 index: %v", err)
	}

	// --- 1. Transcript Isolation ---
	reqTranscript1 := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/transcript?run_id="+run1.ID, nil)
	recTranscript1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(recTranscript1, reqTranscript1)
	if recTranscript1.Code != http.StatusOK {
		t.Fatalf("get run 1 transcript status = %d, want 200; body=%s", recTranscript1.Code, recTranscript1.Body.String())
	}
	if !strings.Contains(recTranscript1.Body.String(), "asr-run-1") || strings.Contains(recTranscript1.Body.String(), "asr-run-2") {
		t.Fatalf("run 1 transcript unexpected body: %s", recTranscript1.Body.String())
	}

	reqTranscript2 := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/transcript?run_id="+run2.ID, nil)
	recTranscript2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(recTranscript2, reqTranscript2)
	if recTranscript2.Code != http.StatusOK {
		t.Fatalf("get run 2 transcript status = %d, want 200; body=%s", recTranscript2.Code, recTranscript2.Body.String())
	}
	if !strings.Contains(recTranscript2.Body.String(), "asr-run-2") || strings.Contains(recTranscript2.Body.String(), "asr-run-1") {
		t.Fatalf("run 2 transcript unexpected body: %s", recTranscript2.Body.String())
	}

	reqTranscriptMissing := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/transcript?run_id=missing-run", nil)
	recTranscriptMissing := httptest.NewRecorder()
	s.Handler().ServeHTTP(recTranscriptMissing, reqTranscriptMissing)
	if recTranscriptMissing.Code != http.StatusNotFound {
		t.Fatalf("missing run transcript status = %d, want 404; body=%s", recTranscriptMissing.Code, recTranscriptMissing.Body.String())
	}

	reqTranscriptWrongAsset := httptest.NewRequest(http.MethodGet, "/api/v1/assets/wrong-asset/transcript?run_id="+run1.ID, nil)
	recTranscriptWrongAsset := httptest.NewRecorder()
	s.Handler().ServeHTTP(recTranscriptWrongAsset, reqTranscriptWrongAsset)
	if recTranscriptWrongAsset.Code != http.StatusNotFound {
		t.Fatalf("wrong asset transcript status = %d, want 404; body=%s", recTranscriptWrongAsset.Code, recTranscriptWrongAsset.Body.String())
	}

	reqTranscriptLatest := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/transcript", nil)
	recTranscriptLatest := httptest.NewRecorder()
	s.Handler().ServeHTTP(recTranscriptLatest, reqTranscriptLatest)
	if recTranscriptLatest.Code != http.StatusOK || !strings.Contains(recTranscriptLatest.Body.String(), "asr-run-2") {
		t.Fatalf("omitted run_id should return latest transcript: status=%d body=%s", recTranscriptLatest.Code, recTranscriptLatest.Body.String())
	}

	// --- 2. Translation Variant Isolation ---
	// Run 1 specific query
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/translation-variant?target_language=vi&run_id="+run1.ID, nil)
	rec1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("get run 1 translation status = %d, want 200; body=%s", rec1.Code, rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), "Xin chào từ Run 1") || strings.Contains(rec1.Body.String(), "Xin chào từ Run 2") {
		t.Fatalf("run 1 translation unexpected body: %s", rec1.Body.String())
	}

	// Run 2 specific query
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/translation-variant?target_language=vi&run_id="+run2.ID, nil)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("get run 2 translation status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "Xin chào từ Run 2") || strings.Contains(rec2.Body.String(), "Xin chào từ Run 1") {
		t.Fatalf("run 2 translation unexpected body: %s", rec2.Body.String())
	}

	// Nonexistent run_id -> 404
	reqNonexistent := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/translation-variant?target_language=vi&run_id=nonexistent", nil)
	recNonexistent := httptest.NewRecorder()
	s.Handler().ServeHTTP(recNonexistent, reqNonexistent)
	if recNonexistent.Code != http.StatusNotFound {
		t.Fatalf("nonexistent run translation status = %d, want 404; body=%s", recNonexistent.Code, recNonexistent.Body.String())
	}

	// Mismatched route asset -> 404
	reqWrongAsset := httptest.NewRequest(http.MethodGet, "/api/v1/assets/wrong-asset/translation-variant?target_language=vi&run_id="+run1.ID, nil)
	recWrongAsset := httptest.NewRecorder()
	s.Handler().ServeHTTP(recWrongAsset, reqWrongAsset)
	if recWrongAsset.Code != http.StatusNotFound {
		t.Fatalf("wrong asset translation status = %d, want 404; body=%s", recWrongAsset.Code, recWrongAsset.Body.String())
	}

	// Mismatched target language -> 404
	reqWrongLang := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/translation-variant?target_language=en&run_id="+run1.ID, nil)
	recWrongLang := httptest.NewRecorder()
	s.Handler().ServeHTTP(recWrongLang, reqWrongLang)
	if recWrongLang.Code != http.StatusNotFound {
		t.Fatalf("wrong lang translation status = %d, want 404; body=%s", recWrongLang.Code, recWrongLang.Body.String())
	}

	// Omitted run_id -> backward compatible, returns latest (run 2)
	reqOmitted := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/translation-variant?target_language=vi", nil)
	recOmitted := httptest.NewRecorder()
	s.Handler().ServeHTTP(recOmitted, reqOmitted)
	if recOmitted.Code != http.StatusOK {
		t.Fatalf("omitted run translation status = %d, want 200; body=%s", recOmitted.Code, recOmitted.Body.String())
	}
	if !strings.Contains(recOmitted.Body.String(), "Xin chào từ Run 2") {
		t.Fatalf("omitted run_id should return latest translation variant: %s", recOmitted.Body.String())
	}

	// --- 3. Voice Assignment Isolation ---
	// Run 1 specific query
	reqVA1 := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/voice-assignment?target_language=vi&run_id="+run1.ID, nil)
	recVA1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(recVA1, reqVA1)
	if recVA1.Code != http.StatusOK {
		t.Fatalf("get run 1 voice assignment status = %d, want 200; body=%s", recVA1.Code, recVA1.Body.String())
	}
	if !strings.Contains(recVA1.Body.String(), "voice-run-1") || strings.Contains(recVA1.Body.String(), "voice-run-2") {
		t.Fatalf("run 1 voice assignment unexpected body: %s", recVA1.Body.String())
	}

	// Run 2 specific query
	reqVA2 := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/voice-assignment?target_language=vi&run_id="+run2.ID, nil)
	recVA2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(recVA2, reqVA2)
	if recVA2.Code != http.StatusOK {
		t.Fatalf("get run 2 voice assignment status = %d, want 200; body=%s", recVA2.Code, recVA2.Body.String())
	}
	if !strings.Contains(recVA2.Body.String(), "voice-run-2") || strings.Contains(recVA2.Body.String(), "voice-run-1") {
		t.Fatalf("run 2 voice assignment unexpected body: %s", recVA2.Body.String())
	}

	// Nonexistent run_id -> 404
	reqVANonexistent := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/voice-assignment?target_language=vi&run_id=nonexistent", nil)
	recVANonexistent := httptest.NewRecorder()
	s.Handler().ServeHTTP(recVANonexistent, reqVANonexistent)
	if recVANonexistent.Code != http.StatusNotFound {
		t.Fatalf("nonexistent run voice assignment status = %d, want 404; body=%s", recVANonexistent.Code, recVANonexistent.Body.String())
	}

	// Mismatched route asset -> 404
	reqVAWrongAsset := httptest.NewRequest(http.MethodGet, "/api/v1/assets/wrong-asset/voice-assignment?target_language=vi&run_id="+run1.ID, nil)
	recVAWrongAsset := httptest.NewRecorder()
	s.Handler().ServeHTTP(recVAWrongAsset, reqVAWrongAsset)
	if recVAWrongAsset.Code != http.StatusNotFound {
		t.Fatalf("wrong asset voice assignment status = %d, want 404; body=%s", recVAWrongAsset.Code, recVAWrongAsset.Body.String())
	}

	// Mismatched target language -> 404
	reqVAWrongLang := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/voice-assignment?target_language=en&run_id="+run1.ID, nil)
	recVAWrongLang := httptest.NewRecorder()
	s.Handler().ServeHTTP(recVAWrongLang, reqVAWrongLang)
	if recVAWrongLang.Code != http.StatusNotFound {
		t.Fatalf("wrong lang voice assignment status = %d, want 404; body=%s", recVAWrongLang.Code, recVAWrongLang.Body.String())
	}

	// Omitted run_id -> backward compatible, returns latest (run 2)
	reqVAOmitted := httptest.NewRequest(http.MethodGet, "/api/v1/assets/"+asset.ID+"/voice-assignment?target_language=vi", nil)
	recVAOmitted := httptest.NewRecorder()
	s.Handler().ServeHTTP(recVAOmitted, reqVAOmitted)
	if recVAOmitted.Code != http.StatusOK {
		t.Fatalf("omitted run voice assignment status = %d, want 200; body=%s", recVAOmitted.Code, recVAOmitted.Body.String())
	}
	if !strings.Contains(recVAOmitted.Body.String(), "voice-run-2") {
		t.Fatalf("omitted run_id should return latest voice assignment: %s", recVAOmitted.Body.String())
	}

	// --- 4. Review-item Isolation ---
	// Both translation variants are intentionally below the QA gate. The
	// run-scoped endpoint must only surface the exception frozen for that run.
	reqReview1 := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run1.ID+"/review-items", nil)
	recReview1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(recReview1, reqReview1)
	if recReview1.Code != http.StatusOK {
		t.Fatalf("get run 1 review items status = %d, want 200; body=%s", recReview1.Code, recReview1.Body.String())
	}
	if !strings.Contains(recReview1.Body.String(), "Xin chào từ Run 1") || strings.Contains(recReview1.Body.String(), "Xin chào từ Run 2") {
		t.Fatalf("run 1 review projection crossed run boundary: %s", recReview1.Body.String())
	}

	reqReview2 := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run2.ID+"/review-items", nil)
	recReview2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(recReview2, reqReview2)
	if recReview2.Code != http.StatusOK {
		t.Fatalf("get run 2 review items status = %d, want 200; body=%s", recReview2.Code, recReview2.Body.String())
	}
	if !strings.Contains(recReview2.Body.String(), "Xin chào từ Run 2") || strings.Contains(recReview2.Body.String(), "Xin chào từ Run 1") {
		t.Fatalf("run 2 review projection crossed run boundary: %s", recReview2.Body.String())
	}
}

func TestHandleRunTranslation_AssetRunBindingAndFrozenGlossary(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	transSvc := service.NewTranslationService(db, casStore)
	invocations := 0
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		invocations++
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "fake_model",
			ModelVersion: "1",
			Segments: []domain.TranslationSegment{
				{Index: 0, SourceText: req.Segments[0].SourceText, TargetText: "Chào Nồi Supor"},
			},
		}, nil
	}

	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore, TranslationSvc: transSvc})
	ctx := context.Background()
	now := time.Now().UTC()

	attestation := domain.RightsAttestation{
		ID:              "attestation-bind-1",
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "tester",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	_ = db.CreateRightsAttestation(ctx, attestation)

	assetA := domain.SourceAsset{
		ID:                  "asset-A",
		SHA256:              "sha-A",
		ByteSize:            100,
		RightsAttestationID: attestation.ID,
		CreatedAt:           now,
	}
	_ = db.CreateSourceAsset(ctx, assetA)

	assetB := domain.SourceAsset{
		ID:                  "asset-B",
		SHA256:              "sha-B",
		ByteSize:            100,
		RightsAttestationID: attestation.ID,
		CreatedAt:           now,
	}
	_ = db.CreateSourceAsset(ctx, assetB)

	jobA := domain.LocalizationJob{
		ID:             "job-A",
		SourceAssetID:  assetA.ID,
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      now,
	}
	_ = db.CreateJob(ctx, jobA)

	jobB := domain.LocalizationJob{
		ID:             "job-B",
		SourceAssetID:  assetB.ID,
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      now,
	}
	_ = db.CreateJob(ctx, jobB)

	runA := domain.LocalizationRun{
		ID:                 "run-A",
		JobID:              jobA.ID,
		Status:             "running",
		ConfigSnapshotJSON: `{"glossary":[{"source":"SUPOR","target":"Nồi Supor","note":"Brand"}]}`,
		CreatedAt:          now,
	}
	_ = db.CreateRun(ctx, runA)
	tObjA, _ := casStore.Put(bytes.NewReader([]byte(`{"asset_id":"asset-A"}`)))
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             "se-stage-runA",
		RunID:          runA.ID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObjA.SHA256,
		CreatedAt:      now,
	})

	runB := domain.LocalizationRun{
		ID:                 "run-B",
		JobID:              jobB.ID,
		Status:             "running",
		ConfigSnapshotJSON: `{"glossary":[{"source":"SUPOR","target":"Nồi Supor","note":"Brand"}]}`,
		CreatedAt:          now,
	}
	_ = db.CreateRun(ctx, runB)

	// 1. Cross-asset run_id must fail closed with 400 (runB passed to assetA URL)
	reqCrossRun := httptest.NewRequest(http.MethodPost, "/api/v1/assets/asset-A/translate", strings.NewReader(`{
		"run_id": "run-B",
		"target_language": "vi",
		"segments": [{"index": 0, "source_text": "SUPOR 你好"}]
	}`))
	recCrossRun := httptest.NewRecorder()
	s.Handler().ServeHTTP(recCrossRun, reqCrossRun)
	if recCrossRun.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for cross-asset run_id, got %d: %s", recCrossRun.Code, recCrossRun.Body.String())
	}
	if !strings.Contains(recCrossRun.Body.String(), "belongs to asset asset-B, not asset-A") {
		t.Fatalf("expected error message to name asset mismatch, got: %s", recCrossRun.Body.String())
	}
	if invocations != 0 {
		t.Fatalf("expected 0 provider invocations on cross-asset run, got %d", invocations)
	}

	// 2. Mismatched job_id must fail closed with 400
	reqMismatchedJob := httptest.NewRequest(http.MethodPost, "/api/v1/assets/asset-A/translate", strings.NewReader(`{
		"run_id": "run-A",
		"job_id": "job-B",
		"target_language": "vi",
		"segments": [{"index": 0, "source_text": "SUPOR 你好"}]
	}`))
	recMismatchedJob := httptest.NewRecorder()
	s.Handler().ServeHTTP(recMismatchedJob, reqMismatchedJob)
	if recMismatchedJob.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for mismatched job_id, got %d: %s", recMismatchedJob.Code, recMismatchedJob.Body.String())
	}
	if invocations != 0 {
		t.Fatalf("expected 0 provider invocations on mismatched job_id, got %d", invocations)
	}

	// 3. Conflicting glossary must fail closed with 400 before provider routing or mutation
	reqConflictGlossary := httptest.NewRequest(http.MethodPost, "/api/v1/assets/asset-A/translate", strings.NewReader(`{
		"run_id": "run-A",
		"job_id": "job-A",
		"target_language": "vi",
		"segments": [{"index": 0, "source_text": "SUPOR 你好"}],
		"glossary": [{"source": "SUPOR", "target": "Khác", "note": "Override attempt"}]
	}`))
	recConflictGlossary := httptest.NewRecorder()
	s.Handler().ServeHTTP(recConflictGlossary, reqConflictGlossary)
	if recConflictGlossary.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for conflicting glossary, got %d: %s", recConflictGlossary.Code, recConflictGlossary.Body.String())
	}
	if !strings.Contains(recConflictGlossary.Body.String(), "request glossary conflicts with frozen run snapshot") {
		t.Fatalf("expected error message to explain glossary conflict, got: %s", recConflictGlossary.Body.String())
	}
	if invocations != 0 {
		t.Fatalf("expected 0 provider invocations on conflicting glossary, got %d", invocations)
	}
	if _, err := db.GetTranslationVariantIndexByRun(ctx, runA.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected zero mutation in storage on failure, got err=%v", err)
	}

	// 4. Inheriting frozen glossary with empty request glossary succeeds (201 Created)
	reqInherit := httptest.NewRequest(http.MethodPost, "/api/v1/assets/asset-A/translate", strings.NewReader(`{
		"run_id": "run-A",
		"job_id": "job-A",
		"target_language": "vi",
		"segments": [{"index": 0, "source_text": "SUPOR 你好"}]
	}`))
	recInherit := httptest.NewRecorder()
	s.Handler().ServeHTTP(recInherit, reqInherit)
	if recInherit.Code != http.StatusCreated {
		t.Fatalf("expected 201 for inheriting frozen glossary, got %d: %s", recInherit.Code, recInherit.Body.String())
	}
	if invocations != 1 {
		t.Fatalf("expected 1 provider invocation, got %d", invocations)
	}
	var res1 struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	if err := json.Unmarshal(recInherit.Body.Bytes(), &res1); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(res1.Variant.EffectiveGlossary.Entries) != 1 || res1.Variant.EffectiveGlossary.Entries[0].Target != "Nồi Supor" {
		t.Fatalf("expected effective glossary target 'Nồi Supor', got %+v", res1.Variant.EffectiveGlossary)
	}

	// 5. Canonically equivalent glossary succeeds (201 Created)
	runA2 := domain.LocalizationRun{
		ID:                 "run-A2",
		JobID:              jobA.ID,
		Status:             "running",
		ConfigSnapshotJSON: `{"glossary":[{"source":"SUPOR","target":"Nồi Supor","note":"Brand"}]}`,
		CreatedAt:          now.Add(time.Second),
	}
	_ = db.CreateRun(ctx, runA2)
	_ = db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             "se-stage-runA2",
		RunID:          runA2.ID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObjA.SHA256,
		CreatedAt:      now,
	})

	reqCanon := httptest.NewRequest(http.MethodPost, "/api/v1/assets/asset-A/translate", strings.NewReader(`{
		"run_id": "run-A2",
		"job_id": "job-A",
		"target_language": "vi",
		"segments": [{"index": 0, "source_text": "SUPOR 你好"}],
		"glossary": [{"source": " ｓｕｐｏｒ ", "target": "Nồi Supor", "note": "Brand"}]
	}`))
	recCanon := httptest.NewRecorder()
	s.Handler().ServeHTTP(recCanon, reqCanon)
	if recCanon.Code != http.StatusCreated {
		t.Fatalf("expected 201 for canonically equivalent glossary, got %d: %s", recCanon.Code, recCanon.Body.String())
	}
	if invocations != 2 {
		t.Fatalf("expected 2 provider invocations, got %d", invocations)
	}
}

// TestReviewItemOverrideRequiresRunID pins finding 3: a review item id alone cannot prove which
// run's pending queue it belongs to, so the direct override endpoint must reject a missing run_id
// instead of silently falling back to the asset-latest queue (which could accept a stale item from
// a superseded run). The guard must also reject a whitespace-only run_id.
func TestReviewItemOverrideRequiresRunID(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	// ReviewSvc is auto-constructed from DB + CASStore, mirroring production wiring.
	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore})

	for name, body := range map[string]string{
		"missing run_id": `{}`,
		"blank run_id":   `{"run_id":"   "}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/review-items/item-1/override", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "run_id is required") {
			t.Fatalf("%s: expected the run_id requirement, got %s", name, rec.Body.String())
		}
	}
}

// TestSpeechUnderstandRejectsUnprovenRunLineage pins finding 4: the speech_understand stage
// execution and the transcript artifact index are both pinned to the client-supplied run_id, so a
// run that does not exist, or that belongs to a different asset, must be rejected before any
// lineage is written - otherwise the run's transcript lineage could be poisoned with another
// asset's media.
func TestSpeechUnderstandRejectsUnprovenRunLineage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC()

	attestation := domain.RightsAttestation{
		ID:              "attestation-speech-own-1",
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "tester",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	}
	_ = db.CreateRightsAttestation(ctx, attestation)

	for _, id := range []string{"asset-owned", "asset-other"} {
		_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
			ID:                  id,
			SHA256:              "sha-" + id,
			ByteSize:            100,
			RightsAttestationID: attestation.ID,
			CreatedAt:           now,
		})
	}
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             "job-other",
		SourceAssetID:  "asset-other",
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      now,
	})
	_ = db.CreateRun(ctx, domain.LocalizationRun{
		ID:        "run-other",
		JobID:     "job-other",
		Status:    "running",
		CreatedAt: now,
	})

	speechSvc := service.NewSpeechService(db, casStore)
	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore, SpeechSvc: speechSvc})

	cases := []struct {
		name         string
		body         string
		wantContains string
	}{
		{"missing run_id", `{}`, "run_id is required"},
		{"unknown run", `{"run_id":"run-absent"}`, "not found"},
		{"run owned by another asset", `{"run_id":"run-other"}`, "belongs to asset asset-other"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/asset-owned/speech-understand", strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.wantContains) {
			t.Fatalf("%s: expected response to mention %q, got %s", tc.name, tc.wantContains, rec.Body.String())
		}
	}

	// No lineage may be written by a rejected request: not for the foreign run, not for the asset.
	stages, err := db.ListStageExecutions(ctx, "run-other")
	if err != nil {
		t.Fatalf("list stage executions: %v", err)
	}
	if len(stages) != 0 {
		t.Fatalf("rejected speech-understand wrote lineage for run-other: %+v", stages)
	}
	if idx, err := db.GetTranscriptArtifactIndex(ctx, "asset-owned"); err == nil && idx != nil {
		t.Fatalf("rejected speech-understand persisted a transcript index for asset-owned: %+v", idx)
	}
}

// TestRunGlossaryConflictCountSurvivesIngress pins finding 11 end to end: a run glossary frozen at
// create-run keeps conflicting duplicates, so the variant produced for that run reports the
// omitted-conflict count the operator UI turns into feedback. Persisting only the deduped list would
// make EffectiveGlossary report zero conflicts for the frozen run and the feedback would never fire.
// The same ingress must reject an invisible format rune before any mutation.
func TestRunGlossaryConflictCountSurvivesIngress(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	db, err := storage.Open(filepath.Join(root, "douyinie.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	transSvc := service.NewTranslationService(db, casStore)
	transSvc.TranslateInvoke = func(ctx context.Context, p provider.Provider, req domain.TranslationJobInput) (*provider.TranslationResult, error) {
		target := req.Segments[0].SourceText
		if len(req.Glossary) > 0 {
			target = req.Glossary[0].Target
		}
		return &provider.TranslationResult{
			ProviderID:   "fake_trans",
			ModelName:    "fake_model",
			ModelVersion: "1",
			Segments: []domain.TranslationSegment{
				{Index: 0, SourceText: req.Segments[0].SourceText, TargetText: target},
			},
		}, nil
	}
	s := New(Config{Addr: "127.0.0.1:0", DB: db, CASStore: casStore, TranslationSvc: transSvc})

	ctx := context.Background()
	now := time.Now().UTC()
	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              "attestation-glossary-conflict",
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "tester",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  "asset-glossary",
		SHA256:              "sha-glossary",
		ByteSize:            100,
		RightsAttestationID: "attestation-glossary-conflict",
		CreatedAt:           now,
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             "job-glossary",
		SourceAssetID:  "asset-glossary",
		TargetLanguage: "vi",
		Status:         "running",
		CreatedAt:      now,
	})

	// Invisible format rune: the request must be rejected before any run is created.
	badBody, _ := json.Marshal(map[string]any{
		"config_snapshot_json": `{"glossary":[{"source":"ke\u200byword","target":"x"}]}`,
	})
	recBad := httptest.NewRecorder()
	s.Handler().ServeHTTP(recBad, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-glossary/runs", bytes.NewReader(badBody)))
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a glossary source with an invisible format rune, got %d: %s", recBad.Code, recBad.Body.String())
	}
	if !strings.Contains(recBad.Body.String(), "format character") {
		t.Fatalf("expected the format-character rejection to be reported, got %s", recBad.Body.String())
	}

	// Conflicting duplicates: same source, different targets. First entry wins.
	runBody, _ := json.Marshal(map[string]any{
		"config_snapshot_json": `{"glossary":[{"source":"SUPOR","target":"Nồi Supor"},{"source":"ｓｕｐｏｒ","target":"Supor"}]}`,
	})
	recRun := httptest.NewRecorder()
	s.Handler().ServeHTTP(recRun, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job-glossary/runs", bytes.NewReader(runBody)))
	if recRun.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating the run, got %d: %s", recRun.Code, recRun.Body.String())
	}
	var created struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if err := json.NewDecoder(recRun.Body).Decode(&created); err != nil {
		t.Fatalf("decode created run: %v", err)
	}
	if created.Run.ID == "" {
		t.Fatalf("expected a run id in the create response: %s", recRun.Body.String())
	}

	// The frozen snapshot must still carry both conflicting entries, otherwise the count is unrecoverable.
	if !strings.Contains(created.Run.ConfigSnapshotJSON, "Supor") {
		t.Fatalf("frozen run snapshot dropped the conflicting duplicate: %s", created.Run.ConfigSnapshotJSON)
	}

	// Translation requires pinned speech_understand transcript lineage for the run.
	tObj, err := casStore.Put(bytes.NewReader([]byte(`{"asset_id":"asset-glossary"}`)))
	if err != nil {
		t.Fatalf("put transcript artifact: %v", err)
	}
	if err := db.CreateStageExecution(ctx, domain.StageExecution{
		ID:             "se-glossary-transcript",
		RunID:          created.Run.ID,
		Stage:          "speech_understand",
		Status:         domain.StageStatusSucceeded,
		ArtifactSHA256: tObj.SHA256,
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("pin transcript lineage: %v", err)
	}

	translateBody, _ := json.Marshal(map[string]any{
		"run_id":          created.Run.ID,
		"job_id":          "job-glossary",
		"target_language": "vi",
		"segments":        []map[string]any{{"index": 0, "source_text": "SUPOR 你好"}},
	})
	recTrans := httptest.NewRecorder()
	s.Handler().ServeHTTP(recTrans, httptest.NewRequest(http.MethodPost, "/api/v1/assets/asset-glossary/translate", bytes.NewReader(translateBody)))
	if recTrans.Code != http.StatusCreated {
		t.Fatalf("expected 201 translating with the frozen run glossary, got %d: %s", recTrans.Code, recTrans.Body.String())
	}
	var translated struct {
		Variant domain.TranslationVariant `json:"translation_variant"`
	}
	if err := json.NewDecoder(recTrans.Body).Decode(&translated); err != nil {
		t.Fatalf("decode translation variant: %v", err)
	}
	if got := translated.Variant.EffectiveGlossary.OmittedConflicts; got != 1 {
		t.Fatalf("expected the operator-visible conflict count 1 from the frozen run glossary, got %d (effective glossary %+v)",
			got, translated.Variant.EffectiveGlossary)
	}
	entries := translated.Variant.EffectiveGlossary.Entries
	if len(entries) != 1 || entries[0].Target != "Nồi Supor" {
		t.Fatalf("expected first-wins to keep the first conflicting entry, got %+v", entries)
	}
}
