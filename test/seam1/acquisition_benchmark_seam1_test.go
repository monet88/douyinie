package seam1_test

import (
	"context"
	"encoding/json"
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

func makeSeam1Acquisition100Entries() []benchmark.AcquisitionCorpusEntry {
	entries := make([]benchmark.AcquisitionCorpusEntry, 100)
	baseTime := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		awemeID := fmt.Sprintf("7410000000000000%02d", i)
		entries[i] = benchmark.AcquisitionCorpusEntry{
			EntryID:             awemeID,
			AwemeID:             awemeID,
			CanonicalURL:        fmt.Sprintf("https://www.douyin.com/video/%s", awemeID),
			ExpectedDurationMs:  1000,
			DurationToleranceMs: 500,
			RequiresAudio:       true,
			Oracle: benchmark.OracleValidation{
				Method:       "independent_browser_audit",
				ValidatedAt:  baseTime,
				EvidenceHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Notes:        "verified Douyin public video",
			},
		}
	}
	return entries
}

func TestSeam1_AcquisitionBenchmark_EndToEnd(t *testing.T) {
	prober := &candidateSplitProber{}
	h := newAcquisitionHarness(t, prober)

	// 1. Freeze 100-URL manifest
	entries := makeSeam1Acquisition100Entries()
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeAcquisitionCorpus("phase1_100_seam1", "1.1", frozenAt, entries)
	if err != nil {
		t.Fatalf("freeze acquisition manifest: %v", err)
	}

	// 2. Initialize Benchmark Session bound to the manifest digest
	storeDir := filepath.Join(h.dir, "benchmark_sessions")
	store, err := benchmark.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("create benchmark store: %v", err)
	}
	sessionInput := benchmark.SessionIdentityInput{
		ExecutionProfile: "local",
		BuildIdentity:    "seam1-build-test",
		ConfigSnapshot:   map[string]any{"profile": "local"},
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "acquisition", Digest: manifest.ManifestDigest},
		},
	}
	sess, err := benchmark.NewSession(sessionInput)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := sess.RecordAcquisitionManifest(manifest); err != nil {
		t.Fatalf("record acquisition manifest: %v", err)
	}

	// 3. Create BenchmarkRunner connected to Seam 1 RuntimeHost
	client := benchmark.NewRuntimeHostClient(h.server.URL, h.server.Client())
	runner, err := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
		Client:  client,
		Store:   store,
		Session: sess,
	})
	// Register authorized credential in the harness
	credID := h.registerSessionCredential(t, "douyin_session_all", "DOUYINIE_TEST_SESSION_1", "msToken=secret-1; ttwid=secret-2")
	ctx := context.Background()

	// 4. Test Single Live Acquisition via Seam 1
	// Using fake adapter in harness: fake_jiji_douyin handles douyin_url
	targetEntry := entries[0]
	input := benchmark.AcquisitionInput{
		EntryID:             targetEntry.EntryID,
		CanonicalURL:        targetEntry.CanonicalURL,
		ExpectedAwemeID:     targetEntry.AwemeID,
		ExpectedDurationMs:  targetEntry.ExpectedDurationMs,
		DurationToleranceMs: targetEntry.DurationToleranceMs,
		AcquireRequest: &service.AcquireRequest{
			Locator: domain.SourceLocator{
				Type:     "douyin_url",
				Location: targetEntry.CanonicalURL,
			},
			AuthorizedCredentials: []string{credID},
			Attestation: &domain.RightsAttestation{
				AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
				DeclaredBy:      "seam1-acquisition-tester",
				TermsAccepted:   true,
			},
			ConsentGranted: true,
		},
	}

	ev, err := runner.ExecuteAcquisition(ctx, input)
	if err != nil {
		t.Fatalf("execute acquisition: %v", err)
	}

	if ev.Status != "PASS" {
		t.Fatalf("expected PASS status, got %s (err: %s)", ev.Status, ev.ErrorMessage)
	}
	if ev.SourceAssetID == "" || ev.SourceAssetCASHash == "" {
		t.Fatalf("expected committed SourceAsset and CAS hash, got asset=%s cas=%s", ev.SourceAssetID, ev.SourceAssetCASHash)
	}

	// Finding 1 & 6: Happy path proves identity came from real AcquireResult.Provenance, not copied expected input
	if ev.ObservedAwemeID != targetEntry.AwemeID {
		t.Fatalf("expected observed aweme_id from real provenance %s, got %s", targetEntry.AwemeID, ev.ObservedAwemeID)
	}
	if ev.ProviderID != "fake_jiji_douyin" {
		t.Fatalf("expected provider from provenance fake_jiji_douyin, got %s", ev.ProviderID)
	}

	// Prove identity was NOT synthesized from input.ExpectedAwemeID
	inputWrongExpected := input
	inputWrongExpected.EntryID = "entry_wrong_expected_test"
	inputWrongExpected.ExpectedAwemeID = "741999999999999999" // Deliberate mismatch
	evWrongExpected, err := runner.ExecuteAcquisition(ctx, inputWrongExpected)
	if err != nil {
		t.Fatalf("execute with wrong expected ID: %v", err)
	}
	if evWrongExpected.ObservedAwemeID != targetEntry.AwemeID {
		t.Fatalf("identity must come from real provenance %s, not copied expected input %s; got %s",
			targetEntry.AwemeID, inputWrongExpected.ExpectedAwemeID, evWrongExpected.ObservedAwemeID)
	}
	scoreStatus, passed, reason := benchmark.ScoreAcquisitionEntry(benchmark.AcquisitionCorpusEntry{
		EntryID:             inputWrongExpected.EntryID,
		AwemeID:             inputWrongExpected.ExpectedAwemeID,
		CanonicalURL:        inputWrongExpected.CanonicalURL,
		ExpectedDurationMs:  inputWrongExpected.ExpectedDurationMs,
		DurationToleranceMs: inputWrongExpected.DurationToleranceMs,
	}, evWrongExpected)
	if passed || scoreStatus != "FAIL" || !strings.Contains(reason, "aweme_id mismatch") {
		t.Fatalf("scorer must fail on aweme_id mismatch: status=%s, reason=%s", scoreStatus, reason)
	}

	// Finding 1 & 6: Wrong provenance canonical URL must fail closed
	inputMismatchedURL := input
	inputMismatchedURL.EntryID = "entry_mismatched_url_test"
	inputMismatchedURL.CanonicalURL = "https://www.douyin.com/video/741000000000000099" // Mismatched
	evMismatchedURL, err := runner.ExecuteAcquisition(ctx, inputMismatchedURL)
	if err != nil {
		t.Fatalf("execute mismatched URL: %v", err)
	}
	if evMismatchedURL.Status != "FAIL" || !strings.Contains(evMismatchedURL.ErrorMessage, "provenance canonical_url mismatch") {
		t.Fatalf("mismatched provenance canonical URL must fail closed: status=%s, err=%s", evMismatchedURL.Status, evMismatchedURL.ErrorMessage)
	}

	// Score the genuine happy-path entry against the manifest
	scoreStatus, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, ev)
	if !passed || scoreStatus != "PASS" {
		t.Fatalf("expected score PASS, got %s (%s)", scoreStatus, reason)
	}

	// Finding 4 & 6: Baseline hash mismatch regression
	entryWithBaseline := targetEntry
	entryWithBaseline.BaselineMediaSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mismatchEv := *ev
	mismatchEv.SourceAssetCASHash = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	scoreStatus, passed, reason = benchmark.ScoreAcquisitionEntry(entryWithBaseline, &mismatchEv)
	if passed || scoreStatus != "FAIL" || !strings.Contains(reason, "CAS hash mismatch") {
		t.Fatalf("baseline hash mismatch must fail scorer, got %s (%s)", scoreStatus, reason)
	}

	// Finding 2 & 6: Missing preflight regression
	missingPreflightEv := &benchmark.AcquisitionEntryEvidence{
		EntryID:            targetEntry.EntryID,
		CanonicalURL:       targetEntry.CanonicalURL,
		ExpectedAwemeID:    targetEntry.AwemeID,
		ObservedAwemeID:    targetEntry.AwemeID,
		SourceAssetID:      ev.SourceAssetID,
		SourceAssetCASHash: ev.SourceAssetCASHash,
		IntegrityPassed:    false,
		ErrorMessage:       "preflight report missing from acquisition result",
	}
	scoreStatus, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, missingPreflightEv)
	if passed || scoreStatus != "FAIL" {
		t.Fatalf("missing preflight must fail scorer, got %s (%s)", scoreStatus, reason)
	}

	// Finding 3 & 6: Anti-bot status without a real HTTP code cannot PASS
	antiBotNoHttpEv := &benchmark.AcquisitionEntryEvidence{
		EntryID:            targetEntry.EntryID,
		CanonicalURL:       targetEntry.CanonicalURL,
		ExpectedAwemeID:    targetEntry.AwemeID,
		ObservedAwemeID:    targetEntry.AwemeID,
		SourceAssetID:      ev.SourceAssetID,
		SourceAssetCASHash: ev.SourceAssetCASHash,
		IntegrityPassed:    true,
		HTTPStatusCode:     0,
		Status:             "ANTI_BOT",
		ErrorMessage:       "anti-bot verification required",
	}
	scoreStatus, passed, reason = benchmark.ScoreAcquisitionEntry(targetEntry, antiBotNoHttpEv)
	if passed || scoreStatus != "FAIL" {
		t.Fatalf("anti-bot without HTTP code must fail scorer, got %s (%s)", scoreStatus, reason)
	}
	// 5. Prohibit Local File Substitution in Scored Benchmark
	substInput := benchmark.AcquisitionInput{
		EntryID:             entries[1].EntryID,
		CanonicalURL:        entries[1].CanonicalURL,
		ExpectedAwemeID:     entries[1].AwemeID,
		ExpectedDurationMs:  entries[1].ExpectedDurationMs,
		DurationToleranceMs: entries[1].DurationToleranceMs,
		MediaFilePath:       filepath.Join(h.dir, "acq_test.db"), // Local file
	}
	substEv, err := runner.ExecuteAcquisition(ctx, substInput)
	if err != nil {
		t.Fatalf("execute local substitution: %v", err)
	}
	if !substEv.IsLocalSubstitution {
		t.Fatal("expected IsLocalSubstitution to be true for local media file")
	}
	if substEv.Status != "FAIL" {
		t.Fatalf("local substitution must produce terminal FAIL on evidence, got %s", substEv.Status)
	}
	if !strings.Contains(substEv.ErrorMessage, "local-file substitution is prohibited") {
		t.Fatalf("expected local substitution error message on evidence, got %q", substEv.ErrorMessage)
	}

	scoreStatus, passed, reason = benchmark.ScoreAcquisitionEntry(entries[1], substEv)
	if passed || scoreStatus != "FAIL" {
		t.Fatalf("local substitution must FAIL scored benchmark, got %s (%s)", scoreStatus, reason)
	}
	// 6. Test HTTP 403 / WAF Challenge Handling (Scored as FAIL, not CONTENT_UNAVAILABLE)
	wafEv := &benchmark.AcquisitionEntryEvidence{
		EntryID:         entries[2].EntryID,
		CanonicalURL:    entries[2].CanonicalURL,
		ExpectedAwemeID: entries[2].AwemeID,
		HTTPStatusCode:  403,
		Status:          "FAIL",
		ErrorMessage:    "HTTP 403 Forbidden: WAF challenge triggered",
		Timestamp:       time.Now().UTC(),
	}
	scoreStatus, passed, reason = benchmark.ScoreAcquisitionEntry(entries[2], wafEv)
	if passed || scoreStatus != "FAIL" {
		t.Fatalf("HTTP 403 must FAIL scored benchmark, got %s (%s)", scoreStatus, reason)
	}

	// 7. Verify Fixed Denominator (100) Summary Computation
	// Populate simulated evidence for all 100 entries: 98 pass, 2 fail (local subst + 403)
	evidences := make(map[string]*benchmark.AcquisitionEntryEvidence, 100)
	evidences[entries[0].EntryID] = ev
	evidences[entries[1].EntryID] = substEv
	evidences[entries[2].EntryID] = wafEv
	for i := 3; i < 100; i++ {
		e := entries[i]
		evidences[e.EntryID] = &benchmark.AcquisitionEntryEvidence{
			EntryID:              e.EntryID,
			CanonicalURL:         e.CanonicalURL,
			ExpectedAwemeID:      e.AwemeID,
			ObservedAwemeID:      e.AwemeID,
			SourceAssetID:        fmt.Sprintf("asset-mock-%d", i),
			SourceAssetCASHash:   fmt.Sprintf("%064x", i+10),
			IntegrityPassed:      true,
			AudioIntegrityPassed: true,
			ObservedDurationMs:   e.ExpectedDurationMs,
			Status:               "PASS",
		}
	}

	summary, err := benchmark.ComputeAcquisitionSummary(sess.ID, manifest, evidences, time.Now().UTC())
	if err != nil {
		t.Fatalf("compute acquisition summary: %v", err)
	}
	if summary.TotalEntries != 100 {
		t.Fatalf("denominator must be fixed at 100, got %d", summary.TotalEntries)
	}
	if summary.PassedEntries != 98 {
		t.Fatalf("expected exactly 98 passed entries, got %d", summary.PassedEntries)
	}
	if summary.FailedEntries != 2 {
		t.Fatalf("expected exactly 2 failed entries, got %d", summary.FailedEntries)
	}
	if summary.SuccessRate != 0.98 {
		t.Fatalf("expected success rate 0.98, got %f", summary.SuccessRate)
	}
	if !summary.GateSatisfied {
		t.Fatal("expected GateSatisfied = true for 98/100 passes")
	}

	if err := sess.RecordAcquisitionSummary(summary); err != nil {
		t.Fatalf("record summary to session: %v", err)
	}

	// 8. Test Post-Freeze Disappearance -> CORPUS_INVALIDATED
	proof := benchmark.OracleDisappearanceProof{
		EntryID:       entries[5].EntryID,
		AwemeID:       entries[5].AwemeID,
		CanonicalURL:  entries[5].CanonicalURL,
		HTTPStatus:    404,
		OracleMethod:  "independent_browser_audit",
		EvidenceHash:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		VerifiedAt:    time.Now().UTC(),
		ConfirmedDead: true,
		OperatorNotes: "Source video deleted by creator post-freeze",
	}

	if err := sess.InvalidateCorpus(entries[5].EntryID, proof); err != nil {
		t.Fatalf("invalidate corpus: %v", err)
	}
	if sess.Status != benchmark.SessionStatusCorpusInvalidated {
		t.Fatalf("expected session status %s, got %s", benchmark.SessionStatusCorpusInvalidated, sess.Status)
	}

	// Post-invalidation runner calls must fail closed
	_, err = runner.ExecuteAcquisition(ctx, input)
	if err == nil {
		t.Fatal("expected error executing acquisition on CORPUS_INVALIDATED session, got nil")
	}
}

type configurableProber struct {
	report domain.PreflightReport
}

func (c *configurableProber) Probe(ctx context.Context, filePath string, expectedHash string) (*domain.PreflightReport, error) {
	cp := c.report
	return &cp, nil
}

func TestSeam1_AcquisitionBenchmark_PreflightIntegrityFailures(t *testing.T) {
	// 1. Missing Video Codec
	t.Run("missing_video_codec", func(t *testing.T) {
		prober := &configurableProber{
			report: domain.PreflightReport{
				DurationMs:             10000,
				DurationSec:            10.0,
				VideoCodec:             "", // missing
				AudioCodec:             "aac",
				Width:                  1080,
				Height:                 1920,
				FrameRate:              30.0,
				ContainerFormat:        "mp4",
				ContainerValid:         true,
				FingerprintMatch:       true,
				NormalizedAudioSHA256:  "mock-sha",
				NormalizedAudioCASPath: "mock-cas",
				CreatedAt:              time.Now().UTC(),
			},
		}
		h := newAcquisitionHarness(t, prober)
		store, _ := benchmark.NewFileStore(filepath.Join(h.dir, "sessions"))
		sess, _ := benchmark.NewSession(benchmark.SessionIdentityInput{
			ExecutionProfile: "local",
			BuildIdentity:    "seam1-pf-test",
			ConfigSnapshot:   map[string]any{"profile": "local"},
		})
		client := benchmark.NewRuntimeHostClient(h.server.URL, h.server.Client())
		runner, _ := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
			Client:  client,
			Store:   store,
			Session: sess,
		})

		ctx := context.Background()
		ev, err := runner.ExecuteAcquisition(ctx, benchmark.AcquisitionInput{
			EntryID:             "pf_codec_test",
			CanonicalURL:        "https://www.douyin.com/video/740000000000000091",
			ExpectedAwemeID:     "740000000000000091",
			ExpectedDurationMs:  10000,
			DurationToleranceMs: 1000,
			MediaFilePath:       filepath.Join(h.dir, "acq_test.db"),
		})
		if err != nil {
			t.Fatalf("unexpected runner error: %v", err)
		}
		if ev.Status != "FAIL" {
			t.Fatalf("expected FAIL on missing video codec, got %s", ev.Status)
		}
		if ev.IntegrityPassed {
			t.Fatal("expected IntegrityPassed=false on missing video codec")
		}
		if !strings.Contains(ev.ErrorMessage, "preflight video stream metadata invalid") {
			t.Fatalf("expected error message to mention video stream metadata invalid, got %q", ev.ErrorMessage)
		}
	})

	// 2. Zero Video Dimensions
	t.Run("zero_video_dimensions", func(t *testing.T) {
		prober := &configurableProber{
			report: domain.PreflightReport{
				DurationMs:             10000,
				DurationSec:            10.0,
				VideoCodec:             "h264",
				AudioCodec:             "aac",
				Width:                  0, // zero
				Height:                 1920,
				FrameRate:              30.0,
				ContainerFormat:        "mp4",
				ContainerValid:         true,
				FingerprintMatch:       true,
				NormalizedAudioSHA256:  "mock-sha",
				NormalizedAudioCASPath: "mock-cas",
				CreatedAt:              time.Now().UTC(),
			},
		}
		h := newAcquisitionHarness(t, prober)
		store, _ := benchmark.NewFileStore(filepath.Join(h.dir, "sessions"))
		sess, _ := benchmark.NewSession(benchmark.SessionIdentityInput{
			ExecutionProfile: "local",
			BuildIdentity:    "seam1-pf-test",
			ConfigSnapshot:   map[string]any{"profile": "local"},
		})
		client := benchmark.NewRuntimeHostClient(h.server.URL, h.server.Client())
		runner, _ := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
			Client:  client,
			Store:   store,
			Session: sess,
		})

		ctx := context.Background()
		ev, err := runner.ExecuteAcquisition(ctx, benchmark.AcquisitionInput{
			EntryID:             "pf_dim_test",
			CanonicalURL:        "https://www.douyin.com/video/740000000000000092",
			ExpectedAwemeID:     "740000000000000092",
			ExpectedDurationMs:  10000,
			DurationToleranceMs: 1000,
			MediaFilePath:       filepath.Join(h.dir, "acq_test.db"),
		})
		if err != nil {
			t.Fatalf("unexpected runner error: %v", err)
		}
		if ev.Status != "FAIL" {
			t.Fatalf("expected FAIL on zero video dimension, got %s", ev.Status)
		}
		if ev.IntegrityPassed {
			t.Fatal("expected IntegrityPassed=false on zero video dimension")
		}
		if !strings.Contains(ev.ErrorMessage, "preflight video stream metadata invalid") {
			t.Fatalf("expected error message to mention video stream metadata invalid, got %q", ev.ErrorMessage)
		}
	})

	t.Run("preflight_asset_id_mismatch", func(t *testing.T) {
		prober := &candidateSplitProber{}
		h := newAcquisitionHarness(t, prober)
		credID := h.registerSessionCredential(t, "douyin_session_all", "DOUYINIE_TEST_SESSION_1", "msToken=secret-1; ttwid=secret-2")

		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/sources/acquire" {
				rec := httptest.NewRecorder()
				h.server.Config.Handler.ServeHTTP(rec, r)
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
					var res service.AcquireResult
					if err := json.Unmarshal(rec.Body.Bytes(), &res); err == nil && res.PreflightReport != nil {
						res.PreflightReport.AssetID = "mismatched-asset-id"
						_ = json.NewEncoder(w).Encode(res)
						return
					}
				}
				_, _ = w.Write(rec.Body.Bytes())
				return
			}
			h.server.Config.Handler.ServeHTTP(w, r)
		}))
		defer proxy.Close()

		store, _ := benchmark.NewFileStore(filepath.Join(h.dir, "sessions"))
		sess, _ := benchmark.NewSession(benchmark.SessionIdentityInput{
			ExecutionProfile: "local",
			BuildIdentity:    "seam1-pf-test",
			ConfigSnapshot:   map[string]any{"profile": "local"},
		})
		client := benchmark.NewRuntimeHostClient(proxy.URL, proxy.Client())
		runner, _ := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
			Client:  client,
			Store:   store,
			Session: sess,
		})

		ctx := context.Background()
		canonicalURL := "https://www.douyin.com/video/740000000000000093"
		ev, err := runner.ExecuteAcquisition(ctx, benchmark.AcquisitionInput{
			EntryID:             "pf_mismatch_test",
			CanonicalURL:        canonicalURL,
			ExpectedAwemeID:     "740000000000000093",
			ExpectedDurationMs:  1000,
			DurationToleranceMs: 500,
			AcquireRequest: &service.AcquireRequest{
				Locator: domain.SourceLocator{
					Type:     "douyin_url",
					Location: canonicalURL,
				},
				AuthorizedCredentials: []string{credID},
				Attestation: &domain.RightsAttestation{
					AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
					DeclaredBy:      "tester",
					TermsAccepted:   true,
				},
				ConsentGranted: true,
			},
		})
		if err != nil {
			t.Fatalf("unexpected runner error: %v", err)
		}
		if ev.Status != "FAIL" {
			t.Fatalf("expected FAIL on preflight asset_id mismatch, got %s", ev.Status)
		}
		if ev.IntegrityPassed {
			t.Fatal("expected IntegrityPassed=false on preflight asset_id mismatch")
		}
		if !strings.Contains(ev.ErrorMessage, "preflight asset_id mismatch") {
			t.Fatalf("expected error message to mention preflight asset_id mismatch, got %q", ev.ErrorMessage)
		}
	})
	t.Run("successful_local_ingest_terminal_fail", func(t *testing.T) {
		prober := &configurableProber{
			report: domain.PreflightReport{
				DurationMs:             10000,
				DurationSec:            10.0,
				VideoCodec:             "h264",
				AudioCodec:             "aac",
				Width:                  1080,
				Height:                 1920,
				FrameRate:              30.0,
				ContainerFormat:        "mp4",
				ContainerValid:         true,
				FingerprintMatch:       true,
				NormalizedAudioSHA256:  "mock-sha-valid",
				NormalizedAudioCASPath: "mock-cas-valid",
				CreatedAt:              time.Now().UTC(),
			},
		}
		h := newAcquisitionHarness(t, prober)
		store, _ := benchmark.NewFileStore(filepath.Join(h.dir, "sessions"))
		sess, _ := benchmark.NewSession(benchmark.SessionIdentityInput{
			ExecutionProfile: "local",
			BuildIdentity:    "seam1-pf-test",
			ConfigSnapshot:   map[string]any{"profile": "local"},
		})
		client := benchmark.NewRuntimeHostClient(h.server.URL, h.server.Client())
		runner, _ := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
			Client:  client,
			Store:   store,
			Session: sess,
		})

		ctx := context.Background()
		ev, err := runner.ExecuteAcquisition(ctx, benchmark.AcquisitionInput{
			EntryID:             "local_success_terminal_fail",
			CanonicalURL:        "https://www.douyin.com/video/740000000000000094",
			ExpectedAwemeID:     "740000000000000094",
			ExpectedDurationMs:  10000,
			DurationToleranceMs: 1000,
			MediaFilePath:       filepath.Join(h.dir, "acq_test.db"),
		})
		if err != nil {
			t.Fatalf("unexpected runner error: %v", err)
		}
		if ev.Status != "FAIL" {
			t.Fatalf("expected terminal FAIL status on local substitution evidence, got %s", ev.Status)
		}
		if !ev.IsLocalSubstitution {
			t.Fatal("expected IsLocalSubstitution=true")
		}
		if !strings.Contains(ev.ErrorMessage, "local-file substitution is prohibited") {
			t.Fatalf("expected local substitution prohibited error message, got %q", ev.ErrorMessage)
		}
		if ev.SourceAssetID == "" || ev.SourceAssetCASHash == "" {
			t.Fatalf("expected diagnostic SourceAssetID and CASHash to be preserved, got ID=%q, CAS=%q", ev.SourceAssetID, ev.SourceAssetCASHash)
		}
		if !ev.IntegrityPassed {
			t.Fatal("expected IntegrityPassed=true for valid local media")
		}
		if !ev.AudioIntegrityPassed {
			t.Fatal("expected AudioIntegrityPassed=true")
		}
	})
}
