package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

type testHarness struct {
	server    *httptest.Server
	srv       *server.Server
	db        *storage.DB
	casStore  *cas.Store
	registry  *provider.Registry
	router    *provider.Router
	queueSvc  *queue.Service
	scheduler *scheduler.Scheduler
	dir       string
}

func (h *testHarness) SetExecutor(exec server.Executor) {
	h.srv.SetExecutor(exec)
}

func setupHarness(t *testing.T) *testHarness {
	t.Helper()
	tmpDir := t.TempDir()

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "douyinie_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}

	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()
	srv, registry, router := newRuntimeHost(t, db, casStore, queueSvc, resScheduler)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = db.Close()
	})

	return &testHarness{
		server:    ts,
		srv:       srv,
		db:        db,
		casStore:  casStore,
		registry:  registry,
		router:    router,
		queueSvc:  queueSvc,
		scheduler: resScheduler,
		dir:       tmpDir,
	}
}

// newRuntimeHost constructs a Seam-1 RuntimeHost server over the given open DB and
// CAS store, returning the server plus its registry and router. It is used both for
// the initial harness and to reconstruct a fresh RuntimeHost over reopened persisted
// state (restart simulation), so the surviving queue/recovery result is observed
// through the same public HTTP API surface.
func newRuntimeHost(t *testing.T, db *storage.DB, casStore *cas.Store, queueSvc *queue.Service, resScheduler *scheduler.Scheduler) (*server.Server, *provider.Registry, *provider.Router) {
	t.Helper()

	var prober media.Prober
	if _, err := exec.LookPath("ffprobe"); err == nil {
		prober = media.NewFFprobeProber()
	} else {
		prober = &media.MockProber{}
	}

	ingestSvc := service.NewIngestService(db, casStore, prober)
	fakeRegistry := provider.NewSeam1FakeRegistry()
	// Seam 1 models production translation as remote gateway execution. Keep all
	// voice/audio/CV fakes local; only the legacy translation fixtures are remote.
	for _, id := range []string{"fake_llm_translator", "fake_local_translator_fallback"} {
		if p, ok := fakeRegistry.Get(id); ok {
			p.(*provider.FakeTranslationProvider).Cap.ExecutionTier = "cloud"
		}
	}
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	initCtx := context.Background()
	for _, p := range fakeRegistry.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName,
				SourceRepo:     "github.com/monet88/douyinie/models/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
		if dmp, ok := p.(provider.DependentModelProvider); ok {
			for _, dep := range dmp.ModelDependencies() {
				if dep.Name != "" {
					_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
						DependencyName: dep.Name,
						Version:        dep.Version,
						SHA256:         "sha256_mock_" + dep.Name,
						SourceRepo:     "github.com/monet88/douyinie/models/" + dep.Name,
						CodeLicense:    "Apache-2.0",
						ModelLicense:   "Apache-2.0",
						DataLicense:    "OpenData",
						ServiceTerms:   "Standard",
						Verified:       true,
						CreatedAt:      time.Now().UTC(),
					})
				}
			}
		}
	}
	router := provider.NewRouter(fakeRegistry, polSvc, licSvc, credSvc, nil, db)

	audioMixSvc := service.NewAudioMixService(db, casStore)
	audioRoleSvc := service.NewAudioRoleServiceWithAnalyzer(db, casStore, audioMixSvc, service.NewDeterministicTestAudioRoleAnalyzer())
	return server.New(server.Config{
		Addr:           "127.0.0.1:0",
		DB:             db,
		CASStore:       casStore,
		Ingest:         ingestSvc,
		Registry:       fakeRegistry,
		PolicySvc:      polSvc,
		LicenseSvc:     licSvc,
		CredSvc:        credSvc,
		Router:         router,
		QueueSvc:       queueSvc,
		Scheduler:      resScheduler,
		SpeechSvc:      service.NewSpeechService(db, casStore),
		TranslationSvc: service.NewTranslationService(db, casStore),
		DubbingSvc:     service.NewDubbingService(db, casStore),
		AudioMixSvc:    audioMixSvc,
		AudioRoleSvc:   audioRoleSvc,
		VisualTextSvc:  service.NewVisualTextService(db, casStore),
		RenderSvc:      service.NewRenderService(db, casStore),
		ReviewSvc:      service.NewReviewService(db, casStore),
	}), fakeRegistry, router
}

// createSyntheticMedia creates a valid MP4 file if ffmpeg is available, or a fallback synthetic file.
func createSyntheticMedia(t *testing.T, dir string, name string) string {
	return createSyntheticMediaWithDuration(t, dir, name, 1.5)
}

// createSyntheticMediaWithDuration creates a valid MP4 file with the specified duration in seconds.
func createSyntheticMediaWithDuration(t *testing.T, dir string, name string, durationSec float64) string {
	t.Helper()
	targetPath := filepath.Join(dir, name)

	if durationSec <= 0 {
		durationSec = 1.5
	}
	durStr := fmt.Sprintf("%.1f", durationSec)

	if _, err := exec.LookPath("ffmpeg"); err == nil {
		cmd := exec.Command("ffmpeg",
			"-y",
			"-f", "lavfi", "-i", fmt.Sprintf("testsrc=duration=%s:size=640x360:rate=30", durStr),
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=880:duration=%s", durStr),
			"-c:v", "libx264",
			"-c:a", "aac",
			targetPath,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("ffmpeg creation failed (%v): %s, using raw bytes", err, string(out))
			_ = os.WriteFile(targetPath, []byte("SYNTHETIC_MEDIA_PAYLOAD_FOR_TESTING_12345"), 0644)
		}
	} else {
		_ = os.WriteFile(targetPath, []byte("SYNTHETIC_MEDIA_PAYLOAD_FOR_TESTING_12345"), 0644)
	}

	return targetPath
}

func TestSeam1_HealthAndProviders(t *testing.T) {
	h := setupHarness(t)

	// 1. Health check
	resp, err := http.Get(h.server.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var healthRes map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&healthRes); err != nil {
		t.Fatalf("decode health json: %v", err)
	}
	if healthRes["status"] != "ok" {
		t.Errorf("expected status ok, got %v", healthRes["status"])
	}

	// 2. Providers list
	resp2, err := http.Get(h.server.URL + "/api/v1/providers")
	if err != nil {
		t.Fatalf("GET /api/v1/providers failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp2.StatusCode)
	}

	var provRes struct {
		Providers []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Policy  string `json:"policy"`
			Healthy bool   `json:"healthy"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&provRes); err != nil {
		t.Fatalf("decode providers json: %v", err)
	}

	if len(provRes.Providers) < 6 {
		t.Errorf("expected at least 6 registered fake providers, got %d", len(provRes.Providers))
	}
}

func TestSeam1_LocalFileIngestEndToEnd(t *testing.T) {
	h := setupHarness(t)
	mediaPath := createSyntheticMedia(t, h.dir, "source_douyin.mp4")

	// 1. Ingest without rights attestation should be rejected (400 Bad Request)
	badBody, _ := json.Marshal(map[string]string{
		"file_path": mediaPath,
	})
	resp, err := http.Post(h.server.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(badBody))
	if err != nil {
		t.Fatalf("POST /api/v1/assets/ingest failed: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing attestation, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Valid Ingest with Rights Attestation
	validPayload := map[string]any{
		"file_path": mediaPath,
		"attestation": map[string]any{
			"attestation_type": "OPERATOR_EXPLICIT_CONFIRMATION",
			"declared_by":      "seam1-acceptance-tester",
			"terms_accepted":   true,
			"notes":            "Canonical acceptance test video",
		},
	}
	validBody, _ := json.Marshal(validPayload)

	resp, err = http.Post(h.server.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(validBody))
	if err != nil {
		t.Fatalf("POST /api/v1/assets/ingest failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 Created, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var ingestRes service.IngestResult
	if err := json.NewDecoder(resp.Body).Decode(&ingestRes); err != nil {
		t.Fatalf("decode ingest json: %v", err)
	}

	// Verify domain invariants
	if ingestRes.Asset == nil || ingestRes.Asset.ID == "" {
		t.Fatalf("missing Asset in response")
	}
	if ingestRes.Asset.SHA256 == "" {
		t.Errorf("empty SHA-256 fingerprint on asset")
	}
	if ingestRes.PreflightReport == nil {
		t.Fatalf("missing PreflightReport in response")
	}
	if !ingestRes.PreflightReport.ContainerValid {
		t.Errorf("container validation failed: %v", ingestRes.PreflightReport.Errors)
	}
	if !ingestRes.PreflightReport.FingerprintMatch {
		t.Errorf("fingerprint match failed")
	}
	if ingestRes.PreflightReport.DurationMs <= 0 {
		t.Errorf("expected duration > 0, got %d ms", ingestRes.PreflightReport.DurationMs)
	}
	if ingestRes.Attestation == nil || !ingestRes.Attestation.TermsAccepted {
		t.Errorf("rights attestation not properly linked/accepted")
	}

	// Verify CAS existence and integrity
	if !h.casStore.Exists(ingestRes.Asset.SHA256) {
		t.Errorf("asset does not exist in CAS store: %s", ingestRes.Asset.SHA256)
	}
	if err := h.casStore.VerifyIntegrity(ingestRes.Asset.SHA256); err != nil {
		t.Errorf("CAS VerifyIntegrity failed: %v", err)
	}

	// Verify retrieval via HTTP GET /api/v1/assets/{id}
	getAssetResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s", h.server.URL, ingestRes.Asset.ID))
	if err != nil {
		t.Fatalf("GET asset failed: %v", err)
	}
	defer getAssetResp.Body.Close()
	if getAssetResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for GET asset, got %d", getAssetResp.StatusCode)
	}

	// Verify retrieval via HTTP GET /api/v1/assets/{id}/preflight
	getPreflightResp, err := http.Get(fmt.Sprintf("%s/api/v1/assets/%s/preflight", h.server.URL, ingestRes.Asset.ID))
	if err != nil {
		t.Fatalf("GET preflight failed: %v", err)
	}
	defer getPreflightResp.Body.Close()
	if getPreflightResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for GET preflight, got %d", getPreflightResp.StatusCode)
	}

	// 3. Create LocalizationJob for VI and EN
	jobPayload := map[string]string{
		"source_asset_id": ingestRes.Asset.ID,
		"target_language": domain.TargetLanguageVI,
	}
	jobBody, _ := json.Marshal(jobPayload)

	jobResp, err := http.Post(h.server.URL+"/api/v1/jobs", "application/json", bytes.NewReader(jobBody))
	if err != nil {
		t.Fatalf("POST /api/v1/jobs failed: %v", err)
	}
	defer jobResp.Body.Close()

	if jobResp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201 Created for job, got %d", jobResp.StatusCode)
	}

	var jobResult struct {
		Job domain.LocalizationJob `json:"job"`
	}
	if err := json.NewDecoder(jobResp.Body).Decode(&jobResult); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	if jobResult.Job.TargetLanguage != "vi" || jobResult.Job.SourceAssetID != ingestRes.Asset.ID {
		t.Errorf("unexpected job: %+v", jobResult.Job)
	}

	// 4. Create LocalizationRun
	runPayload := map[string]string{
		"config_snapshot_json": `{"preset":"canonical_vi","zero_overrun":true}`,
	}
	runBody, _ := json.Marshal(runPayload)

	runResp, err := http.Post(fmt.Sprintf("%s/api/v1/jobs/%s/runs", h.server.URL, jobResult.Job.ID), "application/json", bytes.NewReader(runBody))
	if err != nil {
		t.Fatalf("POST run failed: %v", err)
	}
	defer runResp.Body.Close()

	if runResp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201 Created for run, got %d", runResp.StatusCode)
	}

	var runResult struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if err := json.NewDecoder(runResp.Body).Decode(&runResult); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	if runResult.Run.JobID != jobResult.Job.ID || runResult.Run.Status != "queued" {
		t.Errorf("unexpected run: %+v", runResult.Run)
	}
}

func TestSeam1_InvalidTargetLanguage(t *testing.T) {
	h := setupHarness(t)
	mediaPath := createSyntheticMedia(t, h.dir, "source_lang_test.mp4")

	// Ingest valid asset
	ingestRes, err := h.server.Client().Post(h.server.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader([]byte(fmt.Sprintf(`{
		"file_path": %q,
		"attestation": {
			"attestation_type": "OWNER_DIRECT",
			"declared_by": "owner",
			"terms_accepted": true
		}
	}`, mediaPath))))
	if err != nil {
		t.Fatalf("ingest failed: %v", err)
	}
	defer ingestRes.Body.Close()

	var ir service.IngestResult
	_ = json.NewDecoder(ingestRes.Body).Decode(&ir)

	// Attempt job creation with invalid language "fr"
	badJobBody, _ := json.Marshal(map[string]string{
		"source_asset_id": ir.Asset.ID,
		"target_language": "fr",
	})
	badResp, err := http.Post(h.server.URL+"/api/v1/jobs", "application/json", bytes.NewReader(badJobBody))
	if err != nil {
		t.Fatalf("POST job failed: %v", err)
	}
	defer badResp.Body.Close()

	if badResp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for unsupported language, got %d", badResp.StatusCode)
	}
}

func TestSeam1_CorruptMedia_Returns422_LeavesNoSourceAsset(t *testing.T) {
	h := setupHarness(t)
	corruptPath := filepath.Join(h.dir, "corrupt_payload.mp4")
	_ = os.WriteFile(corruptPath, []byte("NOT_A_VALID_MEDIA_CONTAINER"), 0644)

	payload := map[string]any{
		"file_path": corruptPath,
		"attestation": map[string]any{
			"declared_by":    "operator",
			"terms_accepted": true,
		},
	}
	bodyBytes, _ := json.Marshal(payload)

	resp, err := http.Post(h.server.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST ingest failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected HTTP 422 for corrupt media container, got %d", resp.StatusCode)
	}

	// Verify no source_assets or rights_attestations created in SQLite
	var assetCount, attestationCount int
	_ = h.db.QueryRow(context.Background(), "SELECT COUNT(*) FROM source_assets").Scan(&assetCount)
	_ = h.db.QueryRow(context.Background(), "SELECT COUNT(*) FROM rights_attestations").Scan(&attestationCount)

	if assetCount != 0 {
		t.Errorf("expected 0 source_assets in DB after failed preflight, found %d", assetCount)
	}
	if attestationCount != 0 {
		t.Errorf("expected 0 rights_attestations in DB after failed preflight, found %d", attestationCount)
	}
}

func TestSeam1_FingerprintMismatch_Returns422(t *testing.T) {
	tmpDir := t.TempDir()
	casStore, _ := cas.NewStore(tmpDir)
	db, _ := storage.Open(filepath.Join(tmpDir, "test.db"))
	defer db.Close()

	// Prober reports FingerprintMatch: false
	mockProber := &media.MockProber{
		CustomReport: &domain.PreflightReport{
			ContainerValid:   true,
			FingerprintMatch: false,
			Errors:           []string{"hash mismatch"},
		},
	}

	ingestSvc := service.NewIngestService(db, casStore, mockProber)
	srv := server.New(server.Config{
		Addr:     "127.0.0.1:0",
		DB:       db,
		CASStore: casStore,
		Ingest:   ingestSvc,
		Registry: provider.NewSeam1FakeRegistry(),
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	dummyFile := filepath.Join(tmpDir, "test.mp4")
	_ = os.WriteFile(dummyFile, []byte("dummy bytes"), 0644)

	payload := map[string]any{
		"file_path": dummyFile,
		"attestation": map[string]any{
			"declared_by":    "operator",
			"terms_accepted": true,
		},
	}
	bodyBytes, _ := json.Marshal(payload)

	resp, err := http.Post(ts.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("POST ingest failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("expected HTTP 422 for fingerprint mismatch, got %d", resp.StatusCode)
	}
}
