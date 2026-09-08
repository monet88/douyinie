package benchmark_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

func setupTestRunnerWithSessionInput(t *testing.T, handler http.Handler, sessionInput benchmark.SessionIdentityInput) (*benchmark.BenchmarkRunner, *benchmark.Session) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	storeDir := filepath.Join(t.TempDir(), "sessions")
	store, err := benchmark.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}

	sess, err := benchmark.NewSession(sessionInput)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	client := benchmark.NewRuntimeHostClient(server.URL, server.Client())
	runner, err := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
		Client:  client,
		Store:   store,
		Session: sess,
	})
	if err != nil {
		t.Fatalf("new benchmark runner: %v", err)
	}
	return runner, sess
}

func setupTestRunner(t *testing.T, handler http.Handler) (*benchmark.BenchmarkRunner, *benchmark.Session) {
	sessionInput := benchmark.SessionIdentityInput{
		ExecutionProfile: "local",
		BuildIdentity:    "runner-test-build",
		ConfigSnapshot:   map[string]any{"profile": "local"},
	}
	return setupTestRunnerWithSessionInput(t, handler, sessionInput)
}

func validAcquireResult(assetID, awemeID, canonicalURL string) service.AcquireResult {
	return service.AcquireResult{
		Asset: &domain.SourceAsset{
			ID:       assetID,
			SHA256:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			ByteSize: 1024,
			MimeType: "video/mp4",
		},
		Provenance: &domain.AcquisitionProvenance{
			Platform:     "douyin",
			SourceID:     "douyin:aweme:" + awemeID,
			CanonicalURL: canonicalURL,
			AssetID:      assetID,
			Adapter:      "fake_jiji_douyin",
			Method:       "json_api",
		},
		PreflightReport: &domain.PreflightReport{
			ID:                    "pf-" + assetID,
			AssetID:               assetID,
			DurationMs:            10000,
			DurationSec:           10.0,
			VideoCodec:            "h264",
			AudioCodec:            "aac",
			Width:                 1080,
			Height:                1920,
			FrameRate:             30.0,
			ContainerFormat:       "mp4",
			ContainerValid:        true,
			FingerprintMatch:      true,
			NormalizedAudioSHA256: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		},
	}
}

func TestExecuteAcquisition_PreflightAssetIDMismatch_FailsClosed(t *testing.T) {
	assetID := "asset-truth-001"
	awemeID := "740000000000000001"
	canonicalURL := "https://www.douyin.com/video/" + awemeID

	res := validAcquireResult(assetID, awemeID, canonicalURL)
	// Finding 1: PreflightReport.AssetID does not match SourceAsset.ID
	res.PreflightReport.AssetID = "asset-spoofed-999"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sources/acquire":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
		case "/api/v1/routing/attempts":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"attempts": []any{}})
		default:
			http.NotFound(w, r)
		}
	})

	runner, _ := setupTestRunner(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ev, err := runner.ExecuteAcquisition(ctx, benchmark.AcquisitionInput{
		EntryID:             awemeID,
		CanonicalURL:        canonicalURL,
		ExpectedAwemeID:     awemeID,
		ExpectedDurationMs:  10000,
		DurationToleranceMs: 1000,
		AcquireRequest: &service.AcquireRequest{
			Locator: domain.SourceLocator{
				Type:     "douyin_url",
				Location: canonicalURL,
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected runner error: %v", err)
	}
	if ev.Status != "FAIL" {
		t.Fatalf("expected FAIL status on preflight asset_id mismatch, got %s", ev.Status)
	}
	if ev.IntegrityPassed {
		t.Fatal("expected IntegrityPassed to be false on preflight asset_id mismatch")
	}
	if !strings.Contains(ev.ErrorMessage, "preflight asset_id mismatch") {
		t.Fatalf("expected error message to mention preflight asset_id mismatch, got %q", ev.ErrorMessage)
	}
}

func TestExecuteAcquisition_MissingVideoStreamMetadata_FailsClosed(t *testing.T) {
	assetID := "asset-truth-002"
	awemeID := "740000000000000002"
	canonicalURL := "https://www.douyin.com/video/" + awemeID

	tests := []struct {
		name       string
		modify     func(p *domain.PreflightReport)
		descSubstr string
	}{
		{
			name: "empty video codec",
			modify: func(p *domain.PreflightReport) {
				p.VideoCodec = ""
			},
			descSubstr: "preflight video stream metadata invalid",
		},
		{
			name: "zero width",
			modify: func(p *domain.PreflightReport) {
				p.Width = 0
			},
			descSubstr: "preflight video stream metadata invalid",
		},
		{
			name: "zero height",
			modify: func(p *domain.PreflightReport) {
				p.Height = 0
			},
			descSubstr: "preflight video stream metadata invalid",
		},
		{
			name: "zero frame rate",
			modify: func(p *domain.PreflightReport) {
				p.FrameRate = 0
			},
			descSubstr: "preflight video stream metadata invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := validAcquireResult(assetID, awemeID, canonicalURL)
			tt.modify(res.PreflightReport)

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/sources/acquire":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(res)
				case "/api/v1/routing/attempts":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"attempts": []any{}})
				default:
					http.NotFound(w, r)
				}
			})

			runner, _ := setupTestRunner(t, handler)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			ev, err := runner.ExecuteAcquisition(ctx, benchmark.AcquisitionInput{
				EntryID:             awemeID,
				CanonicalURL:        canonicalURL,
				ExpectedAwemeID:     awemeID,
				ExpectedDurationMs:  10000,
				DurationToleranceMs: 1000,
				AcquireRequest: &service.AcquireRequest{
					Locator: domain.SourceLocator{
						Type:     "douyin_url",
						Location: canonicalURL,
					},
				},
			})
			if err != nil {
				t.Fatalf("unexpected runner error: %v", err)
			}
			if ev.Status != "FAIL" {
				t.Fatalf("expected FAIL status, got %s", ev.Status)
			}
			if ev.IntegrityPassed {
				t.Fatal("expected IntegrityPassed to be false")
			}
			if !strings.Contains(ev.ErrorMessage, tt.descSubstr) {
				t.Fatalf("expected error message containing %q, got %q", tt.descSubstr, ev.ErrorMessage)
			}
		})
	}
}

func TestExecuteAcquisition_SuccessfulLocalIngest_TerminalFAIL(t *testing.T) {
	assetID := "asset-local-003"
	casHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/assets/ingest":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"asset": domain.SourceAsset{
					ID:       assetID,
					SHA256:   casHash,
					ByteSize: 2048,
					MimeType: "video/mp4",
				},
			})
		case "/api/v1/assets/" + assetID + "/preflight":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"preflight_report": domain.PreflightReport{
					ID:                    "pf-" + assetID,
					AssetID:               assetID,
					DurationMs:            10000,
					DurationSec:           10.0,
					VideoCodec:            "h264",
					AudioCodec:            "aac",
					Width:                 1080,
					Height:                1920,
					FrameRate:             30.0,
					ContainerFormat:       "mp4",
					ContainerValid:        true,
					FingerprintMatch:      true,
					NormalizedAudioSHA256: "norm-audio-sha",
				},
			})
		case "/api/v1/routing/attempts":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"attempts": []any{}})
		default:
			http.NotFound(w, r)
		}
	})

	runner, sess := setupTestRunner(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ev, err := runner.ExecuteAcquisition(ctx, benchmark.AcquisitionInput{
		EntryID:             "local_entry_003",
		CanonicalURL:        "https://www.douyin.com/video/740000000000000003",
		ExpectedAwemeID:     "740000000000000003",
		ExpectedDurationMs:  10000,
		DurationToleranceMs: 1000,
		MediaFilePath:       "/path/to/local/video.mp4",
	})
	if err != nil {
		t.Fatalf("unexpected runner error: %v", err)
	}

	// Finding 2: Evidence itself must have terminal FAIL status
	if ev.Status != "FAIL" {
		t.Fatalf("expected evidence.Status to be terminal FAIL, got %s", ev.Status)
	}
	if !ev.IsLocalSubstitution {
		t.Fatal("expected IsLocalSubstitution to be true")
	}
	if !strings.Contains(ev.ErrorMessage, "local-file substitution is prohibited") {
		t.Fatalf("expected local substitution error message, got %q", ev.ErrorMessage)
	}

	// Diagnostic provenance is preserved
	if ev.SourceAssetID != assetID {
		t.Fatalf("expected SourceAssetID %s, got %s", assetID, ev.SourceAssetID)
	}
	if ev.SourceAssetCASHash != casHash {
		t.Fatalf("expected SourceAssetCASHash %s, got %s", casHash, ev.SourceAssetCASHash)
	}
	if !ev.IntegrityPassed {
		t.Fatal("expected IntegrityPassed to be true for valid preflight")
	}
	if !ev.AudioIntegrityPassed {
		t.Fatal("expected AudioIntegrityPassed to be true for valid normalized audio")
	}
	if ev.ObservedDurationMs != 10000 {
		t.Fatalf("expected ObservedDurationMs 10000, got %d", ev.ObservedDurationMs)
	}

	// Resuming / re-executing must preserve terminal FAIL
	reused, err := runner.ExecuteAcquisition(ctx, benchmark.AcquisitionInput{
		EntryID:             "local_entry_003",
		CanonicalURL:        "https://www.douyin.com/video/740000000000000003",
		ExpectedAwemeID:     "740000000000000003",
		ExpectedDurationMs:  10000,
		DurationToleranceMs: 1000,
		MediaFilePath:       "/path/to/local/video.mp4",
	})
	if err != nil {
		t.Fatalf("re-executing acquisition: %v", err)
	}
	if reused.Status != "FAIL" {
		t.Fatalf("reused acquisition must remain terminal FAIL, got %s", reused.Status)
	}
	if reused.SourceAssetID != assetID {
		t.Fatalf("reused acquisition must preserve SourceAssetID, got %s", reused.SourceAssetID)
	}

	// Session recording must reflect terminal FAIL
	stored, ok := sess.GetAcquisition("local_entry_003")
	if !ok {
		t.Fatal("expected session to contain local_entry_003")
	}
	if stored.Status != "FAIL" {
		t.Fatalf("session stored evidence must be terminal FAIL, got %s", stored.Status)
	}
}

func TestClient_PostQualityResult_Roundtrip(t *testing.T) {
	var postedQR domain.QualityResult
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/quality-results":
			if err := json.NewDecoder(r.Body).Decode(&postedQR); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"quality_result": postedQR})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/test-run-001/quality-results":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"quality_results": []domain.QualityResult{postedQR},
				"count":           1,
			})
		default:
			http.NotFound(w, r)
		}
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := benchmark.NewRuntimeHostClient(server.URL, server.Client())
	ctx := context.Background()

	qr := domain.QualityResult{
		ID:             "qr-test-rt-01",
		RunID:          "test-run-001",
		AssetID:        "test-asset-001",
		TargetLanguage: "vi",
		Stage:          "multimodal_qc",
		OverallStatus:  domain.QualityStatusPass,
		Metrics: []domain.QualityMetric{
			{Name: "render_integrity", Score: 1.0, Passed: true},
		},
		CreatedAt: time.Now().UTC(),
	}

	res, err := client.PostQualityResult(ctx, qr)
	if err != nil {
		t.Fatalf("PostQualityResult failed: %v", err)
	}
	if res.ID != qr.ID {
		t.Errorf("expected ID %s, got %s", qr.ID, res.ID)
	}

	got, err := client.GetRunQualityResults(ctx, "test-run-001")
	if err != nil {
		t.Fatalf("GetRunQualityResults failed: %v", err)
	}
	if len(got) != 1 || got[0].ID != qr.ID {
		t.Errorf("expected 1 result with ID %s, got %+v", qr.ID, got)
	}
}

func TestBenchmarkRunner_ExecuteQualityCase_FailClosedOnMissingQCCapture(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/sources/probe"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"descriptor": domain.SourceDescriptor{DurationMs: 3000},
			})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/assets/ingest"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"asset": domain.SourceAsset{ID: "asset-qc-01", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/assets/") && strings.Contains(r.URL.Path, "/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"run": domain.LocalizationRun{ID: "run-qc-01", JobID: "job-qc-01"},
			})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/render/final"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"final_render": domain.FinalRenderArtifact{
					ID: "render-01", OverallStatus: "PASS", CASHash: "cas-render-01", OutputByteSize: 1024, OutputDurationMs: 3000,
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/quality-results":
			var qr domain.QualityResult
			_ = json.NewDecoder(r.Body).Decode(&qr)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"quality_result": qr})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/quality-results"):
			// Return empty list to simulate missing capture after POST
			_ = json.NewEncoder(w).Encode(map[string]any{"quality_results": []domain.QualityResult{}, "count": 0})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/review-items"):
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []domain.ReviewItem{}, "count": 0})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cas_hash": "cas-mock-hash",
				"id":       "mock-id",
				"cues":     []domain.SubtitleCue{},
			})
		}
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	storeDir := filepath.Join(t.TempDir(), "sessions")
	store, _ := benchmark.NewFileStore(storeDir)
	sess, _ := benchmark.NewSession(benchmark.SessionIdentityInput{ExecutionProfile: "local"})
	client := benchmark.NewRuntimeHostClient(server.URL, server.Client())
	runner, _ := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
		Client:  client,
		Store:   store,
		Session: sess,
	})
	input := benchmark.QualityCaseInput{
		CaseID:          "case-fail-capture",
		SourceVideoID:   "v-fail-capture",
		SourceAssetID:   "asset-qc-01",
		PrimaryCategory: "lifestyle_narration",
		TargetLanguage:  "vi",
		Profile:         "local",
		IsNoDub:         true,
	}
	_, err := runner.ExecuteQualityCase(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "not captured in run") {
		t.Fatalf("expected fail-closed error for missing QC capture, got %v", err)
	}
}

func TestIsBenchmarkOwnedQualityResult(t *testing.T) {
	validQR := domain.QualityResult{
		ID:             "qr-bench-1",
		RunID:          "run-1",
		JobID:          "job-1",
		AssetID:        "asset-1",
		TargetLanguage: "vi",
		Stage:          "multimodal_qc",
		Details: map[string]any{
			benchmark.BenchmarkQCProducerKey: benchmark.BenchmarkQCProducerValue,
			benchmark.BenchmarkQCSchemaKey:   benchmark.BenchmarkQCSchemaVersion,
		},
	}

	// 1. Valid exact match
	if !benchmark.IsBenchmarkOwnedQualityResult(validQR, "run-1", "job-1", "asset-1", "vi") {
		t.Errorf("expected valid benchmark QR to match")
	}

	// 2. Deserialized float64 schema (standard JSON unmarshal to any)
	floatSchemaQR := validQR
	floatSchemaQR.Details = map[string]any{
		benchmark.BenchmarkQCProducerKey: benchmark.BenchmarkQCProducerValue,
		benchmark.BenchmarkQCSchemaKey:   float64(1.0),
	}
	if !benchmark.IsBenchmarkOwnedQualityResult(floatSchemaQR, "run-1", "job-1", "asset-1", "vi") {
		t.Errorf("expected float64 schema version 1.0 to match")
	}

	// 3. Unowned (no details or wrong producer)
	unownedQR := validQR
	unownedQR.Details = map[string]any{"source": "production_human"}
	if benchmark.IsBenchmarkOwnedQualityResult(unownedQR, "run-1", "job-1", "asset-1", "vi") {
		t.Errorf("unowned QR must not match")
	}

	// 4. Missing details
	nilDetailsQR := validQR
	nilDetailsQR.Details = nil
	if benchmark.IsBenchmarkOwnedQualityResult(nilDetailsQR, "run-1", "job-1", "asset-1", "vi") {
		t.Errorf("nil details QR must not match")
	}

	// 5. Wrong stage
	wrongStageQR := validQR
	wrongStageQR.Stage = "other_stage"
	if benchmark.IsBenchmarkOwnedQualityResult(wrongStageQR, "run-1", "job-1", "asset-1", "vi") {
		t.Errorf("wrong stage must not match")
	}

	// 6. Scope mismatch (wrong run, job, asset, or lang)
	if benchmark.IsBenchmarkOwnedQualityResult(validQR, "run-2", "job-1", "asset-1", "vi") {
		t.Errorf("mismatched runID must not match")
	}
	if benchmark.IsBenchmarkOwnedQualityResult(validQR, "run-1", "job-2", "asset-1", "vi") {
		t.Errorf("mismatched jobID must not match")
	}
	if benchmark.IsBenchmarkOwnedQualityResult(validQR, "run-1", "job-1", "asset-2", "vi") {
		t.Errorf("mismatched assetID must not match")
	}
	if benchmark.IsBenchmarkOwnedQualityResult(validQR, "run-1", "job-1", "asset-1", "en") {
		t.Errorf("mismatched targetLanguage must not match")
	}
}

func TestExecuteAcquisitionCorpus_ContextCancellationAndPerEntryFailure(t *testing.T) {
	entries := makeValid100Entries()
	manifest, err := benchmark.FreezeAcquisitionCorpus("acq_test", "1.1", time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC), entries)
	if err != nil {
		t.Fatalf("freeze acquisition corpus: %v", err)
	}

	// 1. Context cancellation stops corpus execution immediately and returns context error
	t.Run("ContextCancellationStopsExecution", func(t *testing.T) {
		invocations := 0
		ctx, cancel := context.WithCancel(context.Background())
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v1/sources/acquire":
				invocations++
				if invocations >= 3 {
					cancel()
				}
				res := validAcquireResult(fmt.Sprintf("asset-%d", invocations), entries[invocations-1].AwemeID, entries[invocations-1].CanonicalURL)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(res)
			case r.URL.Path == "/api/v1/routing/attempts":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"attempts": []any{}})
			case r.URL.Path == "/api/v1/routing/decisions":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"decisions": []any{}})
			default:
				http.NotFound(w, r)
			}
		})

		sessionInput := benchmark.SessionIdentityInput{
			CorpusDigests: []benchmark.CorpusDigest{
				{Name: "acquisition", Digest: manifest.ManifestDigest, EntriesCount: 100},
			},
			ExecutionProfile: "local",
			BuildIdentity:    "runner-test-build",
			ConfigSnapshot:   map[string]any{"profile": "local"},
		}
		runner, _ := setupTestRunnerWithSessionInput(t, handler, sessionInput)
		summary, err := runner.ExecuteAcquisitionCorpus(ctx, manifest, "")
		if err == nil {
			t.Fatal("expected context cancellation error, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected errors.Is context.Canceled, got %v", err)
		}
		if summary != nil {
			t.Fatalf("expected nil summary on context cancellation, got %+v", summary)
		}
		if invocations >= 100 {
			t.Fatalf("expected execution to stop early, processed %d entries", invocations)
		}
	})

	// 2. Ordinary per-entry acquisition failure does not abort the corpus; completes all 100 entries
	t.Run("PerEntryFailureDoesNotAbortCorpus", func(t *testing.T) {
		invocations := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v1/sources/acquire":
				invocations++
				// Half fail with 403, half succeed
				if invocations%2 == 1 {
					w.WriteHeader(http.StatusForbidden)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"error": map[string]any{
							"code":    "ACQUISITION_FAILED",
							"message": "WAF blocked request",
						},
					})
					return
				}
				res := validAcquireResult(fmt.Sprintf("asset-%d", invocations), entries[invocations-1].AwemeID, entries[invocations-1].CanonicalURL)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(res)
			case r.URL.Path == "/api/v1/routing/attempts":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"attempts": []any{}})
			case r.URL.Path == "/api/v1/routing/decisions":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"decisions": []any{}})
			default:
				http.NotFound(w, r)
			}
		})

		sessionInput := benchmark.SessionIdentityInput{
			CorpusDigests: []benchmark.CorpusDigest{
				{Name: "acquisition", Digest: manifest.ManifestDigest, EntriesCount: 100},
			},
			ExecutionProfile: "local",
			BuildIdentity:    "runner-test-build",
			ConfigSnapshot:   map[string]any{"profile": "local"},
		}
		runner, _ := setupTestRunnerWithSessionInput(t, handler, sessionInput)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		summary, err := runner.ExecuteAcquisitionCorpus(ctx, manifest, "")
		if err != nil {
			t.Fatalf("expected nil error for corpus despite per-entry failures, got %v", err)
		}
		if summary == nil {
			t.Fatal("expected non-nil summary")
		}
		if invocations != 100 {
			t.Fatalf("expected all 100 entries to execute, got %d", invocations)
		}
		if summary.TotalEntries != 100 {
			t.Fatalf("expected total entries 100, got %d", summary.TotalEntries)
		}
		if summary.FailedEntries == 0 {
			t.Fatalf("expected failed entries to be recorded in summary, got %d", summary.FailedEntries)
		}
	})
}
