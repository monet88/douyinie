package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// Issue #156: Run-scoped inspector review items projection and dub candidate media playback endpoint.

func TestServer_Issue156_RunReviewItemsAndDubMediaPlayback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	casStore, err := cas.NewStore(dir)
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	assetID := "asset-tempo-srv"
	runID := "run-tempo-srv"
	jobID := "job-tempo-srv"
	targetLang := "vi"
	now := time.Now().UTC()

	_ = db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              "att-" + assetID,
		AttestationType: "OWNER_DIRECT",
		DeclaredBy:      "tester",
		TermsAccepted:   true,
		ConfirmedAt:     now,
	})
	_ = db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		SHA256:              strings.Repeat("1", 64),
		ByteSize:            1024,
		MimeType:            "video/mp4",
		RightsAttestationID: "att-" + assetID,
		CASPath:             "/source.mp4",
		CreatedAt:           now,
	})
	_ = db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: targetLang,
		Status:         "running",
		CreatedAt:      now,
	})
	_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "running",
		ConfigSnapshotJSON: "{}",
		CreatedAt:          now,
	}, jobID)

	// Natural WAV (1200ms) and transformed WAV (1000ms)
	natBytes := media.GeneratePCM16WAV(16000, 1, 1200)
	natObj, _ := casStore.Put(bytes.NewReader(natBytes))

	transBytes := media.GeneratePCM16WAV(16000, 1, 1000)
	transObj, _ := casStore.Put(bytes.NewReader(transBytes))

	tempoCandidate := &domain.DubTempoCandidate{
		NaturalAudioSHA256:     natObj.SHA256,
		TransformedAudioSHA256: transObj.SHA256,
		Factor:                 1.20,
		NaturalDurationMs:      1200,
		TransformedDurationMs:  1000,
		PlaybackDurationMs:     1000,
		Selectable:             true,
		Reason:                 domain.TempoReasonFits,
		ToolID:                 domain.TempoToolID,
		Filter:                 "atempo=1.200000",
		PolicyVersion:          "playback-window-v2",
	}

	dsv := domain.DubSegmentsVariant{
		ID:             uuid.NewString(),
		SchemaVersion:  domain.DubSegmentsSchemaVersion,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: targetLang,
		FitPolicyID:    "playback-window-v2",
		OverallStatus:  "REVIEW_REQUIRED",
		ReviewSegments: []domain.DubSegmentReview{
			{
				Index:              0,
				SpeechBlockIndices: []int{0},
				SpeakerID:          "SPEAKER_00",
				StartMs:            0,
				EndMs:              1000,
				SlotDurationMs:     1000,
				SourceText:         "测试",
				SpokenText:         "Thử nghiệm",
				AudioCASPath:       natObj.Path,
				AudioSHA256:        natObj.SHA256,
				MeasuredDurationMs: 1200,
				FitDecision:        domain.FitActionReview,
				ReviewReason:       "DURATION_OVERRUN",
				AttemptCount:       1,
				DubPlaybackEndMs:   1000,
				TempoCandidate:     tempoCandidate,
			},
		},
		CreatedAt: now,
	}

	dsvData, _ := json.Marshal(dsv)
	dsvObj, _ := casStore.Put(bytes.NewReader(dsvData))
	_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
		ID:             dsv.ID,
		AssetID:        assetID,
		RunID:          runID,
		JobID:          jobID,
		TargetLanguage: targetLang,
		CASHash:        dsvObj.SHA256,
		ProvenanceHash: "prov-tempo-server",
		OverallStatus:  "REVIEW_REQUIRED",
		CreatedAt:      now,
	})

	reviewSvc := service.NewReviewService(db, casStore)
	srv := server.New(server.Config{
		Addr:      "127.0.0.1:0",
		DB:        db,
		CASStore:  casStore,
		ReviewSvc: reviewSvc,
	})

	// 1. Test GET /api/v1/runs/{id}/review-items contains tempo details
	t.Run("review items exposes tempo candidate evidence without audio bytes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/runs/"+runID+"/review-items", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			ReviewItems []domain.ReviewItem `json:"review_items"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode review items response: %v", err)
		}
		if len(resp.ReviewItems) != 1 {
			t.Fatalf("expected 1 review item, got %d", len(resp.ReviewItems))
		}
		item := resp.ReviewItems[0]
		if item.Status != domain.ReviewItemStatusPending {
			t.Fatalf("expected status pending, got %s", item.Status)
		}
		tcRaw, ok := item.Details["tempo_candidate"]
		if !ok || tcRaw == nil {
			t.Fatal("expected tempo_candidate map in review item details")
		}
		tcMap, ok := tcRaw.(map[string]any)
		if !ok {
			t.Fatalf("expected map[string]any for tempo_candidate, got %T", tcRaw)
		}
		if tcMap["factor"] != 1.20 {
			t.Fatalf("expected factor 1.2, got %v", tcMap["factor"])
		}
		if tcMap["transformed_audio_sha256"] != transObj.SHA256 {
			t.Fatalf("expected transformed audio sha %s, got %v", transObj.SHA256, tcMap["transformed_audio_sha256"])
		}
		if tcMap["natural_audio_sha256"] != natObj.SHA256 {
			t.Fatalf("expected natural audio sha %s, got %v", natObj.SHA256, tcMap["natural_audio_sha256"])
		}
		if tcMap["selectable"] != true {
			t.Fatalf("expected selectable true, got %v", tcMap["selectable"])
		}
	})

	// 2. Test GET /api/v1/runs/{id}/dub-media/{hash} streams natural audio
	t.Run("streams natural audio from CAS", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/runs/"+runID+"/dub-media/"+natObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Type") != "audio/wav" {
			t.Fatalf("expected Content-Type audio/wav, got %s", rec.Header().Get("Content-Type"))
		}
		if !bytes.Equal(rec.Body.Bytes(), natBytes) {
			t.Fatal("streamed natural audio does not match original bytes")
		}
	})

	// 3. Test GET /api/v1/runs/{id}/dub-media/{hash} streams transformed audio
	t.Run("streams transformed audio from CAS", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/runs/"+runID+"/dub-media/"+transObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Type") != "audio/wav" {
			t.Fatalf("expected Content-Type audio/wav, got %s", rec.Header().Get("Content-Type"))
		}
		if !bytes.Equal(rec.Body.Bytes(), transBytes) {
			t.Fatal("streamed transformed audio does not match original bytes")
		}
	})

	// 4. Test rejecting unowned / foreign / cross-run hash
	t.Run("rejects foreign or unowned hash", func(t *testing.T) {
		foreignHash := strings.Repeat("f", 64)
		req := httptest.NewRequest("GET", "/api/v1/runs/"+runID+"/dub-media/"+foreignHash, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected status 403 Forbidden for foreign hash, got %d", rec.Code)
		}
	})

	// 5. Test rejecting malformed or path traversal hash
	t.Run("rejects malformed hash", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/runs/"+runID+"/dub-media/..%2F..%2Fetc%2Fpasswd", nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400 Bad Request for path traversal, got %d", rec.Code)
		}
	})

	// 6. Test override cannot create implicit selection
	t.Run("override cannot select or approve candidate", func(t *testing.T) {
		// A reason is required for the audit trail and is validated before the review item is
		// resolved, so supplying one is what makes this reach the hard timing blocker below
		// instead of an unrelated "reason is required" refusal.
		body := `{"review_item_id": "rev-dubseg-rev-` + dsvObj.SHA256 + `-0", "action": "override", "reason": "attempted operator override of a hard timing blocker"}`
		req := httptest.NewRequest("POST", "/api/v1/runs/"+runID+"/review/override", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		// TTS overruns are hard blockers and cannot be overridden; the service returns an
		// error so the handler responds with 400 Bad Request.
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request refusal for TTS overrun, got status %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "hard timing blocker") {
			t.Fatalf("expected the refusal to be the hard timing blocker, got body=%s", rec.Body.String())
		}
	})

	// 7. Test an owned artifact past the accepted artifact ceiling returns 413 Request Entity Too Large
	t.Run("rejects oversized owned audio with 413 before streaming", func(t *testing.T) {
		// Create a synthetic oversized variant where one owned segment references an object past
		// the playback bound. We do not allocate that many bytes: CAS Put takes an io.Reader, so
		// an io.LimitReader on a zero reader works.
		oversizedBytes := service.MaxDubMediaPlaybackBytes + 1024
		oversizedObj, err := casStore.Put(io.LimitReader(zeroReader{}, oversizedBytes))
		if err != nil {
			t.Fatalf("put oversized object in CAS: %v", err)
		}

		// Build variant containing this hash as an owned review segment
		overRunID := "run-oversized-playback"
		_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 overRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID)

		overVariant := domain.DubSegmentsVariant{
			ID:             uuid.NewString(),
			SchemaVersion:  domain.DubSegmentsSchemaVersion,
			AssetID:        assetID,
			RunID:          overRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			FitPolicyID:    "playback-window-v2",
			ReviewSegments: []domain.DubSegmentReview{
				{
					Index:              0,
					SpeakerID:          "SPEAKER_00",
					AudioSHA256:        oversizedObj.SHA256,
					MeasuredDurationMs: 1200,
					FitDecision:        domain.FitActionReview,
					ReviewReason:       "DURATION_OVERRUN",
				},
			},
			CreatedAt: now,
		}
		vBytes, _ := json.Marshal(&overVariant)
		vObj, _ := casStore.Put(bytes.NewReader(vBytes))
		_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
			ID:             overVariant.ID,
			AssetID:        assetID,
			RunID:          overRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			CASHash:        vObj.SHA256,
			ProvenanceHash: "prov-oversized-playback",
			CreatedAt:      now,
		})

		req := httptest.NewRequest("GET", "/api/v1/runs/"+overRunID+"/dub-media/"+oversizedObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413 StatusRequestEntityTooLarge, got %d body=%s", rec.Code, rec.Body.String())
		}
		// Nothing but the refusal json is written: no audio content type, no audio payload.
		if rec.Header().Get("Content-Type") == "audio/wav" {
			t.Fatal("oversized audio must not write audio/wav content type")
		}
		if errBody := rec.Body.String(); !strings.Contains(errBody, "exceeds playback size limit") || len(errBody) > 512 {
			t.Fatalf("expected only the refusal json past the bound, got %d bytes: %s", len(errBody), errBody)
		}
	})

	// 8. Test the dub_synthesize stage-artifact fallback when the variant index is missing.
	// The fallback trusts only this run's own variant: an artifact that decodes to another
	// run's variant body is refused before any of its audio is read.
	t.Run("resolves the run's own variant from dub_synthesize stage artifact when index missing", func(t *testing.T) {
		fallbackRunID := "run-stage-fallback"
		_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 fallbackRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID)

		// The run's own variant, recorded as its dub_synthesize artifact with no index row.
		ownVariant := domain.DubSegmentsVariant{
			ID:             uuid.NewString(),
			SchemaVersion:  domain.DubSegmentsSchemaVersion,
			AssetID:        assetID,
			RunID:          fallbackRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			FitPolicyID:    "playback-window-v2",
			ReviewSegments: []domain.DubSegmentReview{
				{
					Index:              0,
					SpeakerID:          "SPEAKER_00",
					AudioSHA256:        natObj.SHA256,
					MeasuredDurationMs: 1200,
					FitDecision:        domain.FitActionReview,
					ReviewReason:       "DURATION_OVERRUN",
				},
			},
			CreatedAt: now,
		}
		ownBytes, _ := json.Marshal(&ownVariant)
		ownObj, _ := casStore.Put(bytes.NewReader(ownBytes))
		_ = db.CreateStageExecution(ctx, domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          fallbackRunID,
			Stage:          "dub_synthesize",
			Status:         "succeeded",
			ArtifactSHA256: ownObj.SHA256,
			CreatedAt:      now,
		})

		req := httptest.NewRequest("GET", "/api/v1/runs/"+fallbackRunID+"/dub-media/"+natObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 via stage-artifact fallback, got %d body=%s", rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), natBytes) {
			t.Fatal("fallback streamed bytes mismatch")
		}
	})

	// 8b. A replay run binds the artifact it consumed through its own stage execution, so a body
	// an older run produced still plays: the run's stage binding, not a re-derived run claim on
	// the immutable body, is what makes the artifact this run's.
	t.Run("streams a stage artifact bound by the run's own stage execution", func(t *testing.T) {
		foreignRunID := "run-stage-fallback-foreign"
		_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 foreignRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID)

		// dsvObj is the first variant, produced by runID. foreignRunID replayed it: its own
		// dub_synthesize stage execution recorded that artifact.
		_ = db.CreateStageExecution(ctx, domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          foreignRunID,
			Stage:          "dub_synthesize",
			Status:         "succeeded",
			ArtifactSHA256: dsvObj.SHA256,
			CreatedAt:      now,
		})

		req := httptest.NewRequest("GET", "/api/v1/runs/"+foreignRunID+"/dub-media/"+natObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for a run-bound replay artifact, got %d body=%s", rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), natBytes) {
			t.Fatal("replay streamed bytes mismatch")
		}
	})

	// 8c. A real dub_segments_variants index row fetched by run_id claims that run, so a body
	// that decodes to another run's variant - same asset and language - is refused, exposing none
	// of that run's audio or hashes.
	t.Run("refuses a variant index row whose body claims another run", func(t *testing.T) {
		claimRunID := "run-row-claim"
		_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 claimRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID)
		// The row is keyed to claimRunID while its CAS body names runID.
		_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
			ID:             uuid.NewString(),
			AssetID:        assetID,
			RunID:          claimRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			CASHash:        dsvObj.SHA256,
			ProvenanceHash: "prov-tempo-row-claim",
			OverallStatus:  "REVIEW_REQUIRED",
			CreatedAt:      now,
		})

		req := httptest.NewRequest("GET", "/api/v1/runs/"+claimRunID+"/dub-media/"+natObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for a row whose body claims another run, got %d body=%s", rec.Code, rec.Body.String())
		}
		if bytes.Contains(rec.Body.Bytes(), natBytes) || strings.Contains(rec.Body.String(), natObj.SHA256) {
			t.Fatalf("refusal leaked audio or its hash: %s", rec.Body.String())
		}
		if rec.Header().Get("Content-Type") == "audio/wav" {
			t.Fatal("refused fallback must not write audio/wav content type")
		}
	})

	// 9. Test owned-but-missing CAS object returns 404
	t.Run("returns 404 for owned hash whose CAS object is missing", func(t *testing.T) {
		missingHash := strings.Repeat("e", 64)
		missingRunID := "run-missing-cas-obj"
		_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 missingRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID)

		mVariant := domain.DubSegmentsVariant{
			ID:             uuid.NewString(),
			SchemaVersion:  domain.DubSegmentsSchemaVersion,
			AssetID:        assetID,
			RunID:          missingRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			FitPolicyID:    "playback-window-v2",
			ReviewSegments: []domain.DubSegmentReview{
				{
					Index:              0,
					SpeakerID:          "SPEAKER_00",
					AudioSHA256:        missingHash, // owned in variant, but never put in CAS!
					MeasuredDurationMs: 1200,
					FitDecision:        domain.FitActionReview,
					ReviewReason:       "DURATION_OVERRUN",
				},
			},
			CreatedAt: now,
		}
		vBytes, _ := json.Marshal(&mVariant)
		vObj, _ := casStore.Put(bytes.NewReader(vBytes))
		_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
			ID:             mVariant.ID,
			AssetID:        assetID,
			RunID:          missingRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			CASHash:        vObj.SHA256,
			ProvenanceHash: "prov-missing-cas-object",
			CreatedAt:      now,
		})

		req := httptest.NewRequest("GET", "/api/v1/runs/"+missingRunID+"/dub-media/"+missingHash, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for owned-but-missing CAS object, got %d body=%s", rec.Code, rec.Body.String())
		}
		// The 404 must come from the owned-but-absent artifact, not from a missing variant row.
		if body := rec.Body.String(); !strings.Contains(body, "audio media artifact not found in store") {
			t.Fatalf("expected the missing-artifact refusal, got body=%s", body)
		}
	})

	// 10. Test an owned artifact at exactly the playback bound is still served: the bound is a
	// strict ceiling, so the largest legitimate waveform must not be refused.
	t.Run("serves an owned artifact at exactly the playback bound", func(t *testing.T) {
		boundObj, err := casStore.Put(io.LimitReader(zeroReader{}, service.MaxDubMediaPlaybackBytes))
		if err != nil {
			t.Fatalf("put bound-sized object in CAS: %v", err)
		}

		boundRunID := "run-exact-bound"
		_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 boundRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID)

		boundVariant := domain.DubSegmentsVariant{
			ID:             uuid.NewString(),
			SchemaVersion:  domain.DubSegmentsSchemaVersion,
			AssetID:        assetID,
			RunID:          boundRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			FitPolicyID:    "playback-window-v2",
			ReviewSegments: []domain.DubSegmentReview{
				{
					Index:              0,
					SpeakerID:          "SPEAKER_00",
					AudioSHA256:        boundObj.SHA256,
					MeasuredDurationMs: 1200,
					FitDecision:        domain.FitActionReview,
					ReviewReason:       "DURATION_OVERRUN",
				},
			},
			CreatedAt: now,
		}
		bBytes, _ := json.Marshal(&boundVariant)
		bObj, _ := casStore.Put(bytes.NewReader(bBytes))
		_ = db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
			ID:             boundVariant.ID,
			AssetID:        assetID,
			RunID:          boundRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			CASHash:        bObj.SHA256,
			ProvenanceHash: "prov-exact-bound",
			CreatedAt:      now,
		})

		req := httptest.NewRequest("GET", "/api/v1/runs/"+boundRunID+"/dub-media/"+boundObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 at exactly the playback bound, got %d body=%s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.FormatInt(service.MaxDubMediaPlaybackBytes, 10) {
			t.Fatalf("expected Content-Length %d, got %q", service.MaxDubMediaPlaybackBytes, got)
		}
		if int64(rec.Body.Len()) != service.MaxDubMediaPlaybackBytes {
			t.Fatalf("expected %d streamed bytes, got %d", service.MaxDubMediaPlaybackBytes, rec.Body.Len())
		}
	})

	// 11. Test a run whose index row is bound to another asset is refused, not served.
	t.Run("refuses a variant index bound to another asset", func(t *testing.T) {
		foreignRunID := "run-foreign-binding"
		_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 foreignRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID)

		// A second real asset that this run's job does not own.
		otherAssetID := "asset-other-owner"
		if err := db.CreateSourceAsset(ctx, domain.SourceAsset{
			ID:                  otherAssetID,
			SHA256:              strings.Repeat("2", 64),
			ByteSize:            2048,
			MimeType:            "video/mp4",
			RightsAttestationID: "att-" + assetID,
			CASPath:             "/other.mp4",
			CreatedAt:           now,
		}); err != nil {
			t.Fatalf("create foreign asset: %v", err)
		}
		if err := db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
			ID:             uuid.NewString(),
			AssetID:        otherAssetID,
			RunID:          foreignRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			CASHash:        dsvObj.SHA256,
			ProvenanceHash: "prov-foreign-binding",
			CreatedAt:      now,
		}); err != nil {
			t.Fatalf("save foreign-bound index: %v", err)
		}

		req := httptest.NewRequest("GET", "/api/v1/runs/"+foreignRunID+"/dub-media/"+natObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for a variant bound to another asset, got %d body=%s", rec.Code, rec.Body.String())
		}
	})

	// 12. Test unknown runs and runs that own no dubbing variant are 404, never 500.
	t.Run("returns 404 for unknown run and for a run without a dubbing variant", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/runs/run-absent/dub-media/"+natObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for unknown run, got %d body=%s", rec.Code, rec.Body.String())
		}

		bareRunID := "run-without-variant"
		_, _ = db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 bareRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID)

		bareReq := httptest.NewRequest("GET", "/api/v1/runs/"+bareRunID+"/dub-media/"+natObj.SHA256, nil)
		bareRec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(bareRec, bareReq)
		if bareRec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for a run without a dubbing variant, got %d body=%s", bareRec.Code, bareRec.Body.String())
		}
	})

	// 13. A run's own variant recorded under a superseded voice assignment must not be served:
	// the reassignment is the run's current voice lineage, so the stale pass is refused even
	// though the run's dub-variant row still resolves to it (Issue #156 stale-artifact ownership),
	// and the exception queue must not surface its evidence either.
	t.Run("refuses audio from a superseded voice assignment", func(t *testing.T) {
		staleAudioObj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1200)))
		if err != nil {
			t.Fatalf("put stale retained waveform: %v", err)
		}
		if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
			ID:              "va-current-" + runID,
			AssetID:         assetID,
			RunID:           runID,
			JobID:           jobID,
			TargetLanguage:  targetLang,
			CASHash:         "assign-current",
			ProvenanceHash:  "prov-va-current-server",
			AssignmentsJSON: "{}",
			CreatedAt:       now,
		}); err != nil {
			t.Fatalf("save current voice assignment: %v", err)
		}

		staleDSV := dsv
		staleDSV.ID = uuid.NewString()
		staleDSV.VoiceAssignmentCAS = "assign-superseded"
		staleDSV.ReviewSegments = append([]domain.DubSegmentReview(nil), dsv.ReviewSegments...)
		staleDSV.ReviewSegments[0].AudioSHA256 = staleAudioObj.SHA256
		staleDSV.CreatedAt = now.Add(time.Minute)
		staleData, err := json.Marshal(staleDSV)
		if err != nil {
			t.Fatalf("marshal stale variant: %v", err)
		}
		staleObj, err := casStore.Put(bytes.NewReader(staleData))
		if err != nil {
			t.Fatalf("put stale variant: %v", err)
		}
		// The superseded pass still holds the run's row - it was claimed before the reassignment -
		// and it is the newer row, so run-scoped resolution reads it.
		if err := db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
			ID:             staleDSV.ID,
			AssetID:        assetID,
			RunID:          runID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			CASHash:        staleObj.SHA256,
			ProvenanceHash: "prov-stale-assignment",
			OverallStatus:  "REVIEW_REQUIRED",
			CreatedAt:      now.Add(time.Minute),
		}); err != nil {
			t.Fatalf("save stale variant index: %v", err)
		}

		req := httptest.NewRequest("GET", "/api/v1/runs/"+runID+"/dub-media/"+staleAudioObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for audio from a superseded voice assignment, got %d body=%s", rec.Code, rec.Body.String())
		}

		itemsReq := httptest.NewRequest("GET", "/api/v1/runs/"+runID+"/review-items", nil)
		itemsRec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(itemsRec, itemsReq)
		if itemsRec.Code != http.StatusForbidden {
			t.Fatalf("the exception queue must refuse a superseded assignment's evidence with 403, got %d body=%s", itemsRec.Code, itemsRec.Body.String())
		}
	})

	// 14. A pass that never claimed the run's row survives only in the run's dub_synthesize stage
	// execution - which is what executeRun records for the pass a refused claim returned after a
	// reassignment landed mid-flight. With another assignment in force that stage binding must be
	// refused rather than served as the run's media.
	t.Run("refuses a stale stage-bound variant when the run owns no row", func(t *testing.T) {
		boundRunID := "run-stage-bound-stale"
		if _, err := db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 boundRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID); err != nil {
			t.Fatalf("create stage-bound run: %v", err)
		}
		if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
			ID:              "va-current-" + boundRunID,
			AssetID:         assetID,
			RunID:           boundRunID,
			JobID:           jobID,
			TargetLanguage:  targetLang,
			CASHash:         "assign-current-bound",
			ProvenanceHash:  "prov-va-bound",
			AssignmentsJSON: "{}",
			CreatedAt:       now,
		}); err != nil {
			t.Fatalf("save stage-bound current assignment: %v", err)
		}

		boundAudioObj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1100)))
		if err != nil {
			t.Fatalf("put stage-bound waveform: %v", err)
		}
		boundDSV := dsv
		boundDSV.ID = uuid.NewString()
		boundDSV.RunID = boundRunID
		boundDSV.VoiceAssignmentCAS = "assign-superseded-bound"
		boundDSV.ReviewSegments = append([]domain.DubSegmentReview(nil), dsv.ReviewSegments...)
		boundDSV.ReviewSegments[0].AudioSHA256 = boundAudioObj.SHA256
		boundData, err := json.Marshal(boundDSV)
		if err != nil {
			t.Fatalf("marshal stage-bound variant: %v", err)
		}
		boundObj, err := casStore.Put(bytes.NewReader(boundData))
		if err != nil {
			t.Fatalf("put stage-bound variant: %v", err)
		}
		if err := db.CreateStageExecution(ctx, domain.StageExecution{
			ID:             uuid.NewString(),
			RunID:          boundRunID,
			Stage:          "dub_synthesize",
			Status:         domain.StageStatusSucceeded,
			ArtifactSHA256: boundObj.SHA256,
			CreatedAt:      now,
		}); err != nil {
			t.Fatalf("bind stage artifact: %v", err)
		}

		req := httptest.NewRequest("GET", "/api/v1/runs/"+boundRunID+"/dub-media/"+boundAudioObj.SHA256, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for a stale stage-bound variant, got %d body=%s", rec.Code, rec.Body.String())
		}
	})

	// 15. Every entry point that projects a run's review items must classify the stale
	// voice-assignment refusal as 403 (not 400 for a malformed request, not 500 for a server
	// fault), so a superseded pass can be neither overridden nor handed off to a final render.
	t.Run("classifies a stale voice assignment as 403 on override and handoff", func(t *testing.T) {
		guardRunID := "run-stale-guard"
		if _, err := db.CreateRunEnqueued(ctx, domain.LocalizationRun{
			ID:                 guardRunID,
			JobID:              jobID,
			Status:             "running",
			ConfigSnapshotJSON: "{}",
			CreatedAt:          now,
		}, jobID); err != nil {
			t.Fatalf("create guarded run: %v", err)
		}
		if err := db.SaveVoiceAssignmentIndex(ctx, storage.VoiceAssignmentIndex{
			ID:              "va-current-" + guardRunID,
			AssetID:         assetID,
			RunID:           guardRunID,
			JobID:           jobID,
			TargetLanguage:  targetLang,
			CASHash:         "assign-current-guard",
			ProvenanceHash:  "prov-va-guard",
			AssignmentsJSON: "{}",
			CreatedAt:       now,
		}); err != nil {
			t.Fatalf("save guarded current assignment: %v", err)
		}
		guardAudioObj, err := casStore.Put(bytes.NewReader(media.GeneratePCM16WAV(16000, 1, 1300)))
		if err != nil {
			t.Fatalf("put guarded waveform: %v", err)
		}
		guardDSV := dsv
		guardDSV.ID = uuid.NewString()
		guardDSV.RunID = guardRunID
		guardDSV.VoiceAssignmentCAS = "assign-superseded-guard"
		guardDSV.ReviewSegments = append([]domain.DubSegmentReview(nil), dsv.ReviewSegments...)
		guardDSV.ReviewSegments[0].AudioSHA256 = guardAudioObj.SHA256
		guardData, err := json.Marshal(guardDSV)
		if err != nil {
			t.Fatalf("marshal guarded variant: %v", err)
		}
		guardObj, err := casStore.Put(bytes.NewReader(guardData))
		if err != nil {
			t.Fatalf("put guarded variant: %v", err)
		}
		if err := db.SaveDubSegmentsVariantIndex(ctx, storage.DubSegmentsVariantIndex{
			ID:             guardDSV.ID,
			AssetID:        assetID,
			RunID:          guardRunID,
			JobID:          jobID,
			TargetLanguage: targetLang,
			CASHash:        guardObj.SHA256,
			ProvenanceHash: "prov-stale-guard",
			OverallStatus:  "REVIEW_REQUIRED",
			CreatedAt:      now,
		}); err != nil {
			t.Fatalf("save guarded variant index: %v", err)
		}

		overrideBody := `{"review_item_id": "rev-x", "action": "override", "reason": "operator attempt"}`
		runOverrideBody := `{"run_id": "` + guardRunID + `", "review_item_id": "rev-x", "action": "override", "reason": "operator attempt"}`
		directOverrideBody := `{"run_id": "` + guardRunID + `", "asset_id": "` + assetID + `", "action": "override", "reason": "operator attempt"}`
		guardCases := []struct {
			name string
			path string
			body string
		}{
			{"run-scoped override", "/api/v1/runs/" + guardRunID + "/review/override", overrideBody},
			{"asset-scoped override", "/api/v1/assets/" + assetID + "/review/override", runOverrideBody},
			{"direct item override", "/api/v1/review-items/rev-x/override", directOverrideBody},
			{"run-scoped handoff", "/api/v1/runs/" + guardRunID + "/render/handoff", `{}`},
			{"asset-scoped handoff", "/api/v1/assets/" + assetID + "/render/handoff?run_id=" + guardRunID, `{}`},
		}
		for _, tc := range guardCases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("expected 403 for a superseded voice assignment, got %d body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	})
}

// zeroReader returns infinite zero bytes for testing large stream bounds without memory allocation.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
