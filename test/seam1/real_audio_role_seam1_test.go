package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
	"github.com/monet88/douyinie/internal/worker"
)

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found; cannot locate repo root from %s", dir)
		}
		dir = parent
	}
}

func TestSeam1_AudioRole_RealProductionSeam_SmokeEvidence(t *testing.T) {
	// 1. Resolve prerequisite paths from environment with sensible fallbacks
	root := findRepoRoot(t)

	refDir := os.Getenv("DOUYINIE_REF_DIR")
	if refDir == "" {
		refDir = filepath.Join(root, ".ref")
	}
	yamnetModelPath := os.Getenv("DOUYINIE_YAMNET_MODEL_PATH")
	if yamnetModelPath == "" {
		yamnetModelPath = filepath.Join(refDir, "phase1.1-runtime", "staging", "yamnet_v1", "yamnet.tflite")
	}
	yamnetClassMapPath := os.Getenv("DOUYINIE_YAMNET_CLASS_MAP_PATH")
	if yamnetClassMapPath == "" {
		yamnetClassMapPath = filepath.Join(refDir, "phase1.1-runtime", "staging", "yamnet_v1", "yamnet_class_map.csv")
	}
	pythonBin := os.Getenv("DOUYINIE_AUDIO_ROLE_PYTHON_BIN")
	if pythonBin == "" {
		candidates := []string{
			filepath.Join(refDir, "phase1.1-runtime", "venvs", "audio_role", "Scripts", "python.exe"),
			filepath.Join(refDir, "phase1.1-runtime", "venvs", "audio_role", "bin", "python"),
		}
		for _, cand := range candidates {
			if _, err := os.Stat(cand); err == nil {
				pythonBin = cand
				break
			}
		}
		if pythonBin == "" {
			pythonBin = candidates[0]
		}
	}
	fixtureAudio := os.Getenv("DOUYINIE_SMOKE_FIXTURE_AUDIO")
	if fixtureAudio == "" {
		fixtureAudio = filepath.Join(refDir, "smoke_evidence", "fixture1_16k.wav")
	}

	if _, err := os.Stat(yamnetModelPath); err != nil {
		t.Skipf("YAMNet model not found at %s: %v", yamnetModelPath, err)
	}
	if _, err := os.Stat(yamnetClassMapPath); err != nil {
		t.Skipf("YAMNet class map not found at %s: %v", yamnetClassMapPath, err)
	}
	if _, err := os.Stat(pythonBin); err != nil {
		t.Skipf("YAMNet python runtime not found at %s: %v", pythonBin, err)
	}
	if _, err := os.Stat(fixtureAudio); err != nil {
		t.Skipf("fixture audio not found at %s: %v", fixtureAudio, err)
	}
	t.Setenv("DOUYINIE_AUDIO_ROLE_PYTHON_BIN", pythonBin)
	adapterPath := os.Getenv("DOUYINIE_AUDIO_ROLE_ADAPTER")
	if adapterPath == "" {
		adapterPath = filepath.Join(root, "cmd", "stageworker", "adapters", "audio_role_yamnet.py")
	}
	if _, err := os.Stat(adapterPath); err != nil {
		t.Skipf("YAMNet adapter script not found at %s: %v", adapterPath, err)
	}
	t.Setenv("DOUYINIE_AUDIO_ROLE_ADAPTER", adapterPath)
	// 2. Build StageWorker binary
	binDir := t.TempDir()
	workerExe := buildStageWorkerForSeam1(t, binDir)
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", workerExe)
	// 3. Harness setup
	h := setupHarness(t)
	ctx := context.Background()
	licSvc := governance.NewLicenseService(h.db)
	snapshotSvc := governance.NewSnapshotService(h.db, licSvc)
	policySvc := governance.NewPolicyService(h.db)

	// 4. Exact license manifest bootstrap
	if err := provider.BootstrapYAMNetLicenseManifest(ctx, licSvc); err != nil {
		t.Fatalf("bootstrap YAMNet license manifest: %v", err)
	}
	licEntry, err := licSvc.GetManifest(ctx, domain.PinnedYAMNetModelID, domain.PinnedYAMNetModelVersion)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	if licEntry.SHA256 != domain.PinnedYAMNetManifestSHA256 {
		t.Fatalf("manifest sha mismatch: got %s, want %s", licEntry.SHA256, domain.PinnedYAMNetManifestSHA256)
	}

	// 5. Register YAMNet snapshot
	yamnetDir := filepath.Dir(yamnetModelPath)
	snapManifest := domain.SnapshotManifest{
		SchemaVersion: "v1",
		ModelID:       domain.PinnedYAMNetModelID,
		ModelVersion:  domain.PinnedYAMNetModelVersion,
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "yamnet.tflite",
				SHA256:       domain.PinnedYAMNetArtifactSHA256,
				SizeBytes:    4126810,
			},
			{
				RelativePath: "yamnet_class_map.csv",
				SHA256:       domain.PinnedYAMNetClassMapSHA256,
				SizeBytes:    14096,
			},
		},
		SnapshotManifestSHA256: domain.PinnedYAMNetManifestSHA256,
	}
	if _, err := snapshotSvc.RegisterAndVerifySnapshot(ctx, snapManifest, yamnetDir); err != nil {
		t.Fatalf("register and verify snapshot: %v", err)
	}
	bCheck, errCheck := snapshotSvc.GetBinding(domain.PinnedYAMNetModelID, domain.PinnedYAMNetModelVersion)
	if errCheck != nil || bCheck == nil {
		t.Fatalf("snapshot binding check failed right after registration: %v", errCheck)
	}
	t.Logf("Snapshot binding verified: %s, active: %v", bCheck.ID, bCheck.Active)
	// 6. Setup WorkerAudioRoleProvider
	leaseMgr := worker.NewGPULeaseManager(h.scheduler)
	yamnetWorker, err := provider.NewWorkerAudioRoleProvider(
		provider.YAMNetProviderID,
		domain.PinnedYAMNetModelID,
		domain.PinnedYAMNetModelVersion,
		0.95,
	)
	if err != nil {
		t.Fatalf("new worker audio role provider: %v", err)
	}
	yamnetWorker.SetRequiresSnapshot(true)
	yamnetWorker.SetSnapshotService(snapshotSvc)
	yamnetWorker.SetLeaseManager(leaseMgr)

	if err := h.registry.Register(yamnetWorker); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if err := policySvc.SetPolicy(ctx, provider.YAMNetProviderID, domain.PolicyAllowed, "production verified"); err != nil {
		t.Fatalf("set policy allowed: %v", err)
	}

	// Reconfigure router on AudioRoleService and server
	h.router.SetSnapshotService(snapshotSvc)
	audioMixSvc := service.NewAudioMixService(h.db, h.casStore)
	audioRoleSvc := service.NewAudioRoleService(h.db, h.casStore, audioMixSvc)
	h.srv.SetAudioRoleService(audioRoleSvc)

	// 7. Ingest real media fixture, preflight, and pre-isolated stems
	fixtureBytes, err := os.ReadFile(fixtureAudio)
	if err != nil {
		t.Fatalf("read fixture audio: %v", err)
	}
	casObj, err := h.casStore.Put(bytes.NewReader(fixtureBytes))
	if err != nil {
		t.Fatalf("store fixture in cas: %v", err)
	}

	attID := uuid.NewString()
	if err := h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	assetID := "asset_real_yamnet_" + uuid.NewString()[:8]
	if err := h.db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              casObj.SHA256,
		CASPath:             casObj.Path,
		ByteSize:            int64(len(fixtureBytes)),
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	if err := h.db.SavePreflightReport(ctx, domain.PreflightReport{
		ID:                     uuid.NewString(),
		AssetID:                assetID,
		ContainerValid:         true,
		DurationSec:            8.0,
		NormalizedAudioCASPath: casObj.Path,
		NormalizedAudioSHA256:  casObj.SHA256,
		CreatedAt:              time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}

	// Generate distinct vocals and background stem media for realistic CAS storage
	fixtureSamples, info, err := media.ExtractPCM16Samples(fixtureBytes)
	if err != nil {
		t.Fatalf("extract fixture samples: %v", err)
	}
	sampleRate := 16000
	if info != nil && info.SampleRate > 0 {
		sampleRate = int(info.SampleRate)
	}
	vocalsSamples := make([]int16, len(fixtureSamples))
	bgSamples := make([]int16, len(fixtureSamples))
	midPoint := 4 * sampleRate
	if midPoint > len(fixtureSamples) {
		midPoint = len(fixtureSamples) / 2
	}
	copy(vocalsSamples[:midPoint], fixtureSamples[:midPoint])
	copy(bgSamples[midPoint:], fixtureSamples[midPoint:])

	vocalsWAV := media.EncodePCM16Samples(vocalsSamples, sampleRate, 1)
	bgWAV := media.EncodePCM16Samples(bgSamples, sampleRate, 1)

	vocalsCAS, err := h.casStore.Put(bytes.NewReader(vocalsWAV))
	if err != nil {
		t.Fatalf("store vocals in cas: %v", err)
	}
	bgCAS, err := h.casStore.Put(bytes.NewReader(bgWAV))
	if err != nil {
		t.Fatalf("store background in cas: %v", err)
	}
	if vocalsCAS.SHA256 == bgCAS.SHA256 {
		t.Fatalf("expected distinct vocals and background CAS hashes")
	}

	stems := domain.AudioStemArtifacts{
		ID:            uuid.NewString(),
		AssetID:       assetID,
		SchemaVersion: 1,
		ProviderID:    "worker_separator",
		ModelName:     "uvr_mdx",
		ModelVersion:  "v3",
		CreatedAt:     time.Now().UTC(),
		Stems: []domain.AudioStem{
			{Type: domain.StemTypeVocals, AudioCASHash: vocalsCAS.SHA256, AudioCASPath: vocalsCAS.Path, DurationMs: 8000, SampleRate: sampleRate, Channels: 1, Format: "wav"},
			{Type: domain.StemTypeBackground, AudioCASHash: bgCAS.SHA256, AudioCASPath: bgCAS.Path, DurationMs: 8000, SampleRate: sampleRate, Channels: 1, Format: "wav"},
		},
	}
	stemsBytes, err := json.Marshal(stems)
	if err != nil {
		t.Fatalf("marshal stems artifact: %v", err)
	}
	stemsCAS, err := h.casStore.Put(bytes.NewReader(stemsBytes))
	if err != nil {
		t.Fatalf("store stems in cas: %v", err)
	}
	if err := h.db.SaveAudioStemsArtifactIndex(ctx, storage.AudioStemsArtifactIndex{
		ID:             uuid.NewString(),
		AssetID:        assetID,
		CASHash:        stemsCAS.SHA256,
		ProvenanceHash: "prov_stems_real",
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save audio stems artifact index: %v", err)
	}
	jobID := "job_real_yamnet_" + uuid.NewString()[:8]
	if err := h.db.CreateJob(ctx, domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "queued",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	runID := "run_real_yamnet_" + uuid.NewString()[:8]
	if err := h.db.CreateRun(ctx, domain.LocalizationRun{
		ID:        runID,
		JobID:     jobID,
		Status:    "queued",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// 8. Generate AudioRolePlan through production AudioRoleService
	plan, err := audioRoleSvc.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
		JobID:   jobID,
	})
	if err != nil {
		decisions, diagErr := h.db.ListSelectionDecisions(ctx, runID, "audio_role_plan")
		if diagErr == nil && len(decisions) > 0 {
			t.Fatalf("generate audio role plan failed: %v, evals: %+v", err, decisions[0].CandidatesEvaluated)
		}
		t.Fatalf("generate audio role plan via production seam failed: %v", err)
	}
	// 9. Governance and Provenance assertions
	decisions, err := h.db.ListSelectionDecisions(ctx, runID, "audio_role_plan")
	if err != nil || len(decisions) == 0 {
		t.Fatalf("expected SelectionDecision for audio_role_plan, err=%v, count=%d", err, len(decisions))
	}
	if decisions[0].SelectedProviderID != provider.YAMNetProviderID {
		t.Fatalf("expected provider %s, got %s", provider.YAMNetProviderID, decisions[0].SelectedProviderID)
	}
	if decisions[0].Stage != "audio_role_plan" {
		t.Fatalf("expected SelectionDecision stage audio_role_plan, got %s", decisions[0].Stage)
	}

	attempts, err := h.db.ListProviderAttempts(ctx, runID, "audio_role_plan")
	if err != nil || len(attempts) == 0 {
		t.Fatalf("expected ProviderAttempt for audio_role_plan, err=%v, count=%d", err, len(attempts))
	}
	if attempts[0].Status != "succeeded" {
		t.Fatalf("expected attempt status succeeded, got %s", attempts[0].Status)
	}
	if attempts[0].Stage != "audio_role_plan" {
		t.Fatalf("expected ProviderAttempt stage audio_role_plan, got %s", attempts[0].Stage)
	}
	if attempts[0].InputHash != plan.ProvenanceHash {
		t.Fatalf("expected attempt InputHash to match plan ProvenanceHash, got %s, want %s", attempts[0].InputHash, plan.ProvenanceHash)
	}

	// 10. Review Item projection assertion: uncertain segments MUST project review item
	hasUncertain := false
	for _, seg := range plan.Segments {
		if seg.Role == domain.AudioRoleUncertain {
			hasUncertain = true
			break
		}
	}
	if !hasUncertain {
		t.Fatal("expected real fixture to contain uncertain segments")
	}

	reviewSvc := service.NewReviewService(h.db, h.casStore)
	projected, err := reviewSvc.ProjectReviewItems(ctx, assetID, "vi")
	if err != nil {
		t.Fatalf("project review items: %v", err)
	}
	foundUncertainReview := false
	for _, item := range projected {
		if item.Type == domain.ReviewItemTypeAudioRole {
			foundUncertainReview = true
			if item.Status != domain.ReviewItemStatusPending {
				t.Fatalf("expected pending status on uncertain review item, got %s", item.Status)
			}
		}
	}
	if !foundUncertainReview {
		t.Fatal("expected uncertain intervals to project ReviewItemTypeAudioUncertain, but none found")
	}

	// 11. Write durable evidence JSON outside Git
	evidenceDir := os.Getenv("DOUYINIE_SMOKE_EVIDENCE_DIR")
	if evidenceDir == "" {
		evidenceDir = filepath.Join(root, ".ref", "smoke_evidence", "audio_role")
	}
	if err := os.MkdirAll(evidenceDir, 0755); err != nil {
		t.Fatalf("mkdir evidence dir: %v", err)
	}
	evidenceFile := filepath.Join(evidenceDir, "production_seam_evidence.json")
	evidenceData := map[string]any{
		"timestamp":          time.Now().UTC().Format(time.RFC3339),
		"stage":              "audio_role_plan",
		"provider_id":        provider.YAMNetProviderID,
		"model_id":           domain.PinnedYAMNetModelID,
		"model_version":      domain.PinnedYAMNetModelVersion,
		"manifest_sha256":    domain.PinnedYAMNetManifestSHA256,
		"run_id":             runID,
		"asset_id":           assetID,
		"provenance_hash":    plan.ProvenanceHash,
		"segments_count":     len(plan.Segments),
		"has_uncertain":      hasUncertain,
		"review_projected":   foundUncertainReview,
		"review_status":      "REVIEW_REQUIRED",
		"no_dub_autonomous":  false,
		"decisions_recorded": len(decisions),
		"attempts_recorded":  len(attempts),
		"stageworker_binary": workerExe,
		"python_binary":      pythonBin,
	}
	raw, err := json.MarshalIndent(evidenceData, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence json: %v", err)
	}
	if err := os.WriteFile(evidenceFile, raw, 0644); err != nil {
		t.Fatalf("write evidence file: %v", err)
	}
}
