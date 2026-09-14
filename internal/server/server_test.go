package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
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
				".speaker-card.is-unassigned",
			},
		},
		{
			path:        "/ui/app.js",
			contentType: "javascript",
			contains: []string{
				"/api/v1/health", "/api/v1/jobs", "/api/v1/assets/upload",
				"/api/v1/runs/", "/render/handoff", "computeStepState", "speakerColor",
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
