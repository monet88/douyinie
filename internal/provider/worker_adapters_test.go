package provider_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
	"github.com/monet88/douyinie/internal/worker"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestProductionSpeechRegistry_ContainsAllRequiredCapabilities verifies Finding 1:
// the production speech registry is populated with concrete worker-backed ASR 1.7B,
// ASR 0.6B fallback, forced aligner, and diarization capability. No fake providers exist.
func TestProductionSpeechRegistry_ContainsAllRequiredCapabilities(t *testing.T) {
	t.Setenv("DOUYINIE_GATEWAY_URL", "")
	t.Setenv("DOUYINIE_GATEWAY_ENDPOINT", "")
	t.Setenv("DOUYINIE_SERVICE_BASELINE_GEMINI", "")
	t.Setenv("DOUYINIE_SERVICE_BASELINE_DEEPSEEK", "")

	reg, err := provider.NewProductionSpeechRegistry()
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry failed: %v", err)
	}

	// 1. ASR providers
	asrs := reg.ListByType(provider.TypeASR)
	if len(asrs) != 2 {
		t.Fatalf("expected 2 ASR providers, got %d", len(asrs))
	}

	// 2. Forced aligner
	aligners := reg.ListByType(provider.TypeAligner)
	if len(aligners) != 1 {
		t.Fatalf("expected 1 aligner provider, got %d", len(aligners))
	}

	// 3. Diarizer
	diarizers := reg.ListByType(provider.TypeDiarizer)
	if len(diarizers) != 1 {
		t.Fatalf("expected 1 diarizer provider, got %d", len(diarizers))
	}

	// Check model identity (Finding 2)
	asr17b, ok := reg.Get("qwen3_asr_1_7b")
	if !ok {
		t.Fatal("missing qwen3_asr_1_7b provider")
	}
	mName, mVer := asr17b.ModelInfo()
	if mName != "qwen3-asr" || mVer != "1.7b" {
		t.Fatalf("unexpected 1.7B model info: %s:%s", mName, mVer)
	}

	asr06b, ok := reg.Get("qwen3_asr_0_6b")
	if !ok {
		t.Fatal("missing qwen3_asr_0_6b provider")
	}
	mName, mVer = asr06b.ModelInfo()
	if mName != "qwen3-asr" || mVer != "0.6b" {
		t.Fatalf("unexpected 0.6B model info: %s:%s", mName, mVer)
	}

	// Forced aligner baseline identity (Finding 5)
	aligner, ok := reg.Get("qwen3_forced_aligner")
	if !ok {
		t.Fatal("missing qwen3_forced_aligner provider")
	}
	mName, mVer = aligner.ModelInfo()
	if mName != "Qwen3-ForcedAligner-0.6B" || mVer != "0.6b" {
		t.Fatalf("unexpected forced aligner model info: %s:%s", mName, mVer)
	}

	// 3D-Speaker / CAM++ diarizer identity
	diarizer, ok := reg.Get("campplus_diarizer")
	if !ok {
		t.Fatal("missing campplus_diarizer provider")
	}
	mName, mVer = diarizer.ModelInfo()
	if mName != "iic/speech_campplus_sv_zh_en_16k-common_advanced" || mVer != "v1.0.0" {
		t.Fatalf("unexpected diarizer model info: %s:%s", mName, mVer)
	}
	if vip, ok := diarizer.(interface{ VADModelInfo() (string, string) }); ok {
		vName, vVer := vip.VADModelInfo()
		if vName != "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch" || vVer != "v2.0.4" {
			t.Fatalf("unexpected diarizer VAD model info: %s:%s", vName, vVer)
		}
	} else {
		t.Fatal("diarizer does not implement VADModelInfo")
	}

	// Production translation is API-first. Without gateway configuration there
	// must be no translation provider, especially no local Qwen fallback.
	if translators := reg.ListByType(provider.TypeTranslation); len(translators) != 0 {
		t.Fatalf("expected no production translation providers without gateway config, got %d", len(translators))
	}

	// Verify Finding 1 & Issue #66: Every production local model-backed provider requires
	// a snapshot by default; remote gateway providers are never represented as pinned checkpoints.
	for _, p := range reg.ListAll() {
		rsp, ok := p.(interface{ RequiresSnapshot() bool })
		if !ok {
			t.Fatalf("production provider %s does not implement RequiresSnapshot()", p.ID())
		}
		if strings.EqualFold(p.Capability().ExecutionTier, "local") {
			if !rsp.RequiresSnapshot() {
				t.Fatalf("expected local production provider %s to require snapshot by default", p.ID())
			}
		} else {
			if rsp.RequiresSnapshot() {
				t.Fatalf("expected remote gateway provider %s to NOT require snapshot", p.ID())
			}
		}
	}
}

func TestProductionSpeechRegistry_ZeroTTSVietnameseLane(t *testing.T) {
	reg, err := provider.NewProductionSpeechRegistry(false)
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}

	p, ok := reg.Get(provider.ZeroTTSProviderID)
	if !ok {
		t.Fatalf("missing %s provider", provider.ZeroTTSProviderID)
	}
	modelName, modelVersion := p.ModelInfo()
	if modelName != provider.ZeroTTSModelID || modelVersion != provider.ZeroTTSModelVersion {
		t.Fatalf("unexpected ZeroTTS model identity: %s:%s", modelName, modelVersion)
	}
	for _, feature := range p.Capability().Features {
		if feature == "measured_duration_speed_fit" {
			t.Fatal("ZeroTTS must not advertise measured_duration_speed_fit")
		}
	}

	tts, ok := p.(provider.TTSProvider)
	if !ok {
		t.Fatalf("%s does not implement TTSProvider", p.ID())
	}
	catalog := tts.VoiceCatalog()
	if len(catalog) != 8 {
		t.Fatalf("expected all 8 verified ZeroTTS preset voices, got %d", len(catalog))
	}
	if catalog[0].VoiceID != "quangminh" || catalog[1].VoiceID != "maichi" {
		t.Fatalf("unexpected ZeroTTS catalog priority: %q, %q", catalog[0].VoiceID, catalog[1].VoiceID)
	}
	for _, voice := range catalog {
		if voice.ProviderID != provider.ZeroTTSProviderID || voice.IsClone {
			t.Fatalf("invalid ZeroTTS preset profile: %+v", voice)
		}
	}

	vieneu, ok := reg.Get(provider.VieNeuProviderID)
	if !ok {
		t.Fatalf("historical VieNeu provider %s missing", provider.VieNeuProviderID)
	}
	vieneuTTS := vieneu.(provider.TTSProvider)
	if got := vieneuTTS.VoiceCatalog(); len(got) != len(provider.OrderedVieNeuVoices()) || got[0].ProviderID != provider.VieNeuProviderID {
		t.Fatalf("VieNeu compatibility catalog changed: %+v", got)
	}
}

func TestProductionSpeechRegistry_TranslationProvidersAreGatewayOnly(t *testing.T) {
	reg, err := provider.NewProductionSpeechRegistry(provider.GatewayTranslationConfig{
		Endpoint:           "https://gateway.example.test/v1",
		GeminiBaselineID:   "test-gemini-baseline",
		DeepSeekBaselineID: "test-deepseek-baseline",
	})
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry failed: %v", err)
	}

	translators := reg.ListByType(provider.TypeTranslation)
	if len(translators) != 2 {
		t.Fatalf("expected exactly 2 gateway translation providers, got %d", len(translators))
	}
	want := map[string]bool{
		provider.GatewayGeminiTranslationProviderID:   false,
		provider.GatewayDeepSeekTranslationProviderID: false,
	}
	for _, p := range translators {
		if p.Capability().ExecutionTier == "local" {
			t.Fatalf("production registry must not contain local translation provider %s", p.ID())
		}
		if _, ok := want[p.ID()]; !ok {
			t.Fatalf("unexpected production translation provider %s", p.ID())
		}
		want[p.ID()] = true
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("missing production gateway translation provider %s", id)
		}
	}
}

func TestProductionSpeechRegistry_LoadsGatewayTranslationFromEnvironment(t *testing.T) {
	t.Setenv("DOUYINIE_GATEWAY_URL", "https://gateway.example.test/v1")
	t.Setenv("DOUYINIE_GATEWAY_ENDPOINT", "")
	t.Setenv("DOUYINIE_SERVICE_BASELINE_GEMINI", "env-gemini-baseline")
	t.Setenv("DOUYINIE_SERVICE_BASELINE_DEEPSEEK", "env-deepseek-baseline")

	reg, err := provider.NewProductionSpeechRegistry()
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry failed: %v", err)
	}
	translators := reg.ListByType(provider.TypeTranslation)
	if len(translators) != 2 {
		t.Fatalf("expected runtime environment to register 2 gateway translators, got %d", len(translators))
	}
	for _, p := range translators {
		if p.Capability().ExecutionTier == "local" {
			t.Fatalf("runtime environment must not register local translator %s", p.ID())
		}
	}
}

// 1.7B is selected as primary due to higher quality score; on quality failure,
// Router.ExecuteWithRetry falls back to 0.6B, with model identity observable.
func TestRouter_ASRQualityFallback_17BTo06BObservable(t *testing.T) {
	db, err := storage.Open(t.TempDir() + "/router_test.db")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg, err := provider.NewProductionSpeechRegistry(false) // disable snapshots for quality-fallback unit test
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	ctx := context.Background()
	// Register verified manifests for all providers
	for _, p := range reg.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName + "_" + mVer,
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
					_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
						DependencyName: dep.Name,
						Version:        dep.Version,
						SHA256:         "sha256_mock_" + dep.Name + "_" + dep.Version,
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

	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db)

	// 1. Check initial route picks 1.7B
	routeRes, err := router.Route(ctx, provider.RouteRequest{
		Stage:            provider.TypeASR,
		Language:         "zh",
		ExecutionProfile: domain.ExecutionProfileHybrid,
	})
	if err != nil {
		t.Fatalf("route ASR failed: %v", err)
	}
	if routeRes.SelectedProvider.ID() != "qwen3_asr_1_7b" {
		t.Fatalf("expected qwen3_asr_1_7b as primary, got %s", routeRes.SelectedProvider.ID())
	}
	if len(routeRes.FallbackOrdered) == 0 || routeRes.FallbackOrdered[0].ID() != "qwen3_asr_0_6b" {
		t.Fatalf("expected qwen3_asr_0_6b as fallback, got %v", routeRes.FallbackOrdered)
	}

	// 2. Execute with retry: simulate quality failure on 1.7B -> fallback to 0.6B
	var executedProviders []string
	var executedModels []string

	err = router.ExecuteWithRetry(ctx, provider.RouteRequest{
		Stage:            provider.TypeASR,
		Language:         "zh",
		ExecutionProfile: domain.ExecutionProfileHybrid,
	}, "input-hash-1", 1, func(p provider.Provider, attemptNumber int) error {
		mName, mVer := p.ModelInfo()
		executedProviders = append(executedProviders, p.ID())
		executedModels = append(executedModels, mName+":"+mVer)

		if p.ID() == "qwen3_asr_1_7b" {
			return domain.ErrQualityRejected
		}
		return nil
	})

	if err != nil {
		t.Fatalf("ExecuteWithRetry failed: %v", err)
	}

	if len(executedProviders) != 2 {
		t.Fatalf("expected 2 execution attempts, got %d (%v)", len(executedProviders), executedProviders)
	}
	if executedProviders[0] != "qwen3_asr_1_7b" || executedProviders[1] != "qwen3_asr_0_6b" {
		t.Fatalf("expected fallback order [qwen3_asr_1_7b, qwen3_asr_0_6b], got %v", executedProviders)
	}
	if executedModels[0] != "qwen3-asr:1.7b" || executedModels[1] != "qwen3-asr:0.6b" {
		t.Fatalf("expected model identities [qwen3-asr:1.7b, qwen3-asr:0.6b], got %v", executedModels)
	}
}

func TestWorkerTranslationProvider_SnapshotRootToEntrypointFileResolution(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	// Helper to create snapshot root with files and register license
	createSnapshotRoot := func(modelID, modelVer string, files map[string]string) (string, domain.SnapshotManifest) {
		root := t.TempDir()
		manifest := domain.SnapshotManifest{
			SchemaVersion: "1.0",
			ModelID:       modelID,
			ModelVersion:  modelVer,
		}
		for rel, content := range files {
			full := filepath.Join(root, filepath.FromSlash(rel))
			_ = os.MkdirAll(filepath.Dir(full), 0755)
			_ = os.WriteFile(full, []byte(content), 0644)
			h := sha256.Sum256([]byte(content))
			manifest.Files = append(manifest.Files, domain.SnapshotFileEntry{
				RelativePath: rel,
				SHA256:       hex.EncodeToString(h[:]),
				SizeBytes:    int64(len(content)),
			})
		}
		cSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
		if err != nil {
			t.Fatalf("ComputeSnapshotManifestSHA256: %v", err)
		}
		manifest.SnapshotManifestSHA256 = cSHA

		// Register 4-layer license manifest
		_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
			ID:             uuid.NewString(),
			DependencyName: modelID,
			Version:        modelVer,
			SHA256:         cSHA,
			CodeLicense:    "Apache-2.0",
			ModelLicense:   "Qwen-Research",
			DataLicense:    "Common-Voice",
			ServiceTerms:   "Local-Offline",
			Verified:       true,
			CreatedAt:      time.Now().UTC(),
		})

		return root, manifest
	}

	// 1. Snapshot with verified GGUF file + config in root directory
	rootDir, manifestValid := createSnapshotRoot("qwen3_4b_translator", "Qwen3-4B-Q4_K_M", map[string]string{
		"qwen3-4b-q4_k_m.gguf": "dummy binary gguf weights content",
		"config.json":          `{"model_type": "qwen3"}`,
	})

	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifestValid, rootDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot: %v", err)
	}

	// Prove binding.LocalPath is the snapshot root DIRECTORY
	info, err := os.Stat(binding.LocalPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("expected binding.LocalPath to be a directory, got: %s (isDir: %v)", binding.LocalPath, info.IsDir())
	}

	// Build real stageworker binary
	stageWorkerExe := filepath.Join(tmpDir, "stageworker-trans")
	if runtime.GOOS == "windows" {
		stageWorkerExe += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", stageWorkerExe, "github.com/monet88/douyinie/cmd/stageworker")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build stageworker: %v: %s", err, string(out))
	}
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", stageWorkerExe)

	// Create adapter script that records the received model_path
	recordedModelPathFile := filepath.Join(tmpDir, "received_model_path.txt")
	adapterScript := filepath.Join(tmpDir, "mock_adapter.py")
	scriptContent := fmt.Sprintf(`import sys, json, os
req = json.loads(sys.stdin.read())
m_path = req.get("model_path", "")
with open(%q, "w") as f:
    f.write(m_path)
out = {
    "segments": [
        {
            "index": 0,
            "source_text": req["segments"][0]["source_text"],
            "target_text": "Xin chao the gioi",
            "qa_confidence": 0.99,
            "passed_qa_gate": True
        }
    ],
    "model_name": req.get("model_name", ""),
    "model_version": req.get("model_version", "")
}
print(json.dumps(out))
`, recordedModelPathFile)
	if err := os.WriteFile(adapterScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write adapter script: %v", err)
	}
	t.Setenv("DOUYINIE_TRANSLATION_ADAPTER", adapterScript)

	// Setup WorkerTranslationProvider with snapshot service
	p, err := provider.NewWorkerTranslationProvider("qwen3_4b_translator", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M", 0.85)
	if err != nil {
		t.Fatalf("NewWorkerTranslationProvider: %v", err)
	}
	p.SetSnapshotService(snapSvc)

	// Execute TranslateText through real workerBridge
	req := provider.TranslationRequest{
		RunID:          "run-trans-test",
		SourceLanguage: "zh",
		TargetLanguage: "vi",
		Segments: []domain.TranslationInputSegment{
			{Index: 0, SourceText: "你好世界"},
		},
	}
	res, err := p.TranslateText(ctx, req)
	if err != nil {
		t.Fatalf("TranslateText through real workerBridge failed: %v", err)
	}
	if res == nil || len(res.Segments) != 1 {
		t.Fatalf("unexpected translation result: %+v", res)
	}

	// PROVE translation received the FILE path, NOT the directory!
	recordedBytes, err := os.ReadFile(recordedModelPathFile)
	if err != nil {
		t.Fatalf("read recorded model path: %v", err)
	}
	receivedPath := string(recordedBytes)
	expectedFilePath := filepath.Join(rootDir, "qwen3-4b-q4_k_m.gguf")
	if receivedPath != expectedFilePath {
		t.Fatalf("translation received %q, want FILE path %q", receivedPath, expectedFilePath)
	}

	receivedInfo, err := os.Stat(receivedPath)
	if err != nil {
		t.Fatalf("stat received path: %v", err)
	}
	if receivedInfo.IsDir() {
		t.Fatalf("translation received a directory (%s), must receive a regular file!", receivedPath)
	}

	// 2. Absent entrypoint fail-closed
	rootNoGGUF, manifestNoGGUF := createSnapshotRoot("qwen3_no_gguf", "Qwen3-4B-Q4_K_M", map[string]string{
		"config.json": `{"model_type": "qwen3"}`,
	})
	_, err = snapSvc.RegisterAndVerifySnapshot(ctx, manifestNoGGUF, rootNoGGUF)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot no gguf: %v", err)
	}

	pNoGGUF, _ := provider.NewWorkerTranslationProvider("qwen3_no_gguf", "qwen3_no_gguf", "Qwen3-4B-Q4_K_M", 0.85)
	pNoGGUF.SetSnapshotService(snapSvc)
	_, err = pNoGGUF.TranslateText(ctx, req)
	if err == nil {
		t.Fatal("expected absent entrypoint to fail closed, got nil error")
	}
	if !errors.Is(err, domain.ErrSnapshotEntrypointAbsent) {
		t.Fatalf("expected ErrSnapshotEntrypointAbsent, got: %v", err)
	}

	// 3. Ambiguous entrypoint fail-closed
	rootAmbig, manifestAmbig := createSnapshotRoot("qwen3_ambig", "Qwen3-4B-Q4_K_M", map[string]string{
		"qwen3-4b-q4_k_m-01.gguf": "part 1 gguf",
		"qwen3-4b-q4_k_m-02.gguf": "part 2 gguf",
	})
	_, err = snapSvc.RegisterAndVerifySnapshot(ctx, manifestAmbig, rootAmbig)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot ambig: %v", err)
	}

	pAmbig, _ := provider.NewWorkerTranslationProvider("qwen3_ambig", "qwen3_ambig", "Qwen3-4B-Q4_K_M", 0.85)
	pAmbig.SetSnapshotService(snapSvc)
	_, err = pAmbig.TranslateText(ctx, req)
	if err == nil {
		t.Fatal("expected ambiguous entrypoint to fail closed, got nil error")
	}
	if !errors.Is(err, domain.ErrSnapshotEntrypointAmbiguous) {
		t.Fatalf("expected ErrSnapshotEntrypointAmbiguous, got: %v", err)
	}
}

func TestWorkerOCRProvider_MultiDependencySnapshotResolution(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test_ocr.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	createSnapshotRoot := func(modelID, modelVer string, files map[string]string) (string, domain.SnapshotManifest) {
		root := t.TempDir()
		manifest := domain.SnapshotManifest{
			SchemaVersion: "1.0",
			ModelID:       modelID,
			ModelVersion:  modelVer,
		}
		for rel, content := range files {
			full := filepath.Join(root, filepath.FromSlash(rel))
			_ = os.MkdirAll(filepath.Dir(full), 0755)
			_ = os.WriteFile(full, []byte(content), 0644)
			h := sha256.Sum256([]byte(content))
			manifest.Files = append(manifest.Files, domain.SnapshotFileEntry{
				RelativePath: rel,
				SHA256:       hex.EncodeToString(h[:]),
				SizeBytes:    int64(len(content)),
			})
		}
		cSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
		if err != nil {
			t.Fatalf("ComputeSnapshotManifestSHA256: %v", err)
		}
		manifest.SnapshotManifestSHA256 = cSHA

		_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
			ID:             uuid.NewString(),
			DependencyName: modelID,
			Version:        modelVer,
			SHA256:         cSHA,
			CodeLicense:    "Apache-2.0",
			ModelLicense:   "Community-License",
			DataLicense:    "Open-Data",
			ServiceTerms:   "Self-Hosted",
			Verified:       true,
			CreatedAt:      time.Now().UTC(),
		})

		return root, manifest
	}

	// 1. Create and verify all 3 OCR snapshots
	detDir, detManifest := createSnapshotRoot("PP-OCRv6_medium_det", "v6", map[string]string{
		"inference.pdmodel":   "det model binary weights",
		"inference.pdiparams": "det params binary weights",
	})
	detBinding, err := snapSvc.RegisterAndVerifySnapshot(ctx, detManifest, detDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot det: %v", err)
	}

	recDir, recManifest := createSnapshotRoot("PP-OCRv6_medium_rec", "v6", map[string]string{
		"inference.pdmodel":   "rec model binary weights",
		"inference.pdiparams": "rec params binary weights",
		"ppocrv6_dict.txt":    "line1\nline2\n",
	})
	recBinding, err := snapSvc.RegisterAndVerifySnapshot(ctx, recManifest, recDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot rec: %v", err)
	}

	oriDir, oriManifest := createSnapshotRoot("PP-LCNet_x1_0_textline_ori", "v1", map[string]string{
		"inference.pdmodel":   "ori model binary weights",
		"inference.pdiparams": "ori params binary weights",
	})
	oriBinding, err := snapSvc.RegisterAndVerifySnapshot(ctx, oriManifest, oriDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot ori: %v", err)
	}

	// Build stageworker binary
	stageWorkerExe := filepath.Join(tmpDir, "stageworker-ocr")
	if runtime.GOOS == "windows" {
		stageWorkerExe += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", stageWorkerExe, "github.com/monet88/douyinie/cmd/stageworker")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build stageworker: %v: %s", err, string(out))
	}
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", stageWorkerExe)

	// Mock OCR adapter script that records det_model_dir, rec_model_dir, and cls_model_dir
	recordedDirsFile := filepath.Join(tmpDir, "received_ocr_dirs.json")
	adapterScript := filepath.Join(tmpDir, "mock_ocr_adapter.py")
	scriptContent := fmt.Sprintf(`import sys, json
req = json.loads(sys.stdin.read())
with open(%q, "w") as f:
    json.dump({
        "det_model_dir": req.get("det_model_dir", ""),
        "rec_model_dir": req.get("rec_model_dir", ""),
        "cls_model_dir": req.get("cls_model_dir", ""),
        "model_name": req.get("model_name", ""),
        "model_version": req.get("model_version", "")
    }, f)
out = {
    "frame_width": 640,
    "frame_height": 480,
    "frame_sample_step_ms": 500,
    "detections": [
        {
            "frame_index": 0,
            "timestamp_ms": 0,
            "text": "SUPOR",
            "confidence": 0.985,
            "box": {"x": 20, "y": 30, "width": 160, "height": 45}
        }
    ],
    "model_name": req.get("model_name", ""),
    "model_version": req.get("model_version", "")
}
print(json.dumps(out))
`, recordedDirsFile)
	if err := os.WriteFile(adapterScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write mock adapter script: %v", err)
	}
	t.Setenv("DOUYINIE_OCR_ADAPTER", adapterScript)

	dummyMedia := filepath.Join(tmpDir, "dummy_media.png")
	if err := os.WriteFile(dummyMedia, []byte("fake image content"), 0644); err != nil {
		t.Fatalf("write dummy media: %v", err)
	}

	// 1. Production regression: WorkerOCRProvider with non-empty model_name ("paddleocr-v6")
	// proves that the OCR branch runs (NOT skipped by mName != "") and resolves all 3 snapshot bindings.
	p, err := provider.NewWorkerOCRProvider("worker_ocr_prod", "paddleocr-v6", "v6", 0.92)
	if err != nil {
		t.Fatalf("NewWorkerOCRProvider: %v", err)
	}
	p.SetSnapshotService(snapSvc)

	req := provider.OCRRequest{
		AssetID:           "asset-ocr-1",
		SourceVideo:       worker.ArtifactRef{Path: dummyMedia},
		FrameSampleStepMs: 500,
	}
	res, err := p.DetectRegions(ctx, req)
	if err != nil {
		t.Fatalf("DetectRegions failed with non-empty model_name: %v", err)
	}
	if res == nil || len(res.Detections) != 1 {
		t.Fatalf("unexpected OCR result: %+v", res)
	}
	if res.Detections[0].Text != "SUPOR" {
		t.Fatalf("unexpected detection text: %s", res.Detections[0].Text)
	}

	// PROVE all three verified bindings reached StageWorker and the adapter!
	data, err := os.ReadFile(recordedDirsFile)
	if err != nil {
		t.Fatalf("read recorded dirs file: %v", err)
	}
	var recorded map[string]string
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatalf("unmarshal recorded dirs: %v", err)
	}

	if recorded["det_model_dir"] != detBinding.LocalPath {
		t.Fatalf("det_model_dir mismatch: got %q, want %q", recorded["det_model_dir"], detBinding.LocalPath)
	}
	if recorded["rec_model_dir"] != recBinding.LocalPath {
		t.Fatalf("rec_model_dir mismatch: got %q, want %q", recorded["rec_model_dir"], recBinding.LocalPath)
	}
	if recorded["cls_model_dir"] != oriBinding.LocalPath {
		t.Fatalf("cls_model_dir mismatch: got %q, want %q", recorded["cls_model_dir"], oriBinding.LocalPath)
	}

	// 2. Missing dependency fails closed: snapshot service without ori binding
	emptySnapSvc := governance.NewSnapshotService(db, licSvc)
	_, _ = emptySnapSvc.RegisterAndVerifySnapshot(ctx, detManifest, detDir)
	_, _ = emptySnapSvc.RegisterAndVerifySnapshot(ctx, recManifest, recDir)

	pMissing, _ := provider.NewWorkerOCRProvider("worker_ocr_no_ori", "paddleocr-v6", "v6", 0.92)
	pMissing.SetSnapshotService(emptySnapSvc)
	_, err = pMissing.DetectRegions(ctx, req)
	if err == nil {
		t.Fatal("expected missing ori dependency to fail closed, got nil error")
	}
	if !errors.Is(err, domain.ErrSnapshotUnverified) {
		t.Fatalf("expected ErrSnapshotUnverified on missing ori, got: %v", err)
	}

	// 3. Mutated dependency snapshot fails closed: tamper with rec model file
	tamperedRecFile := filepath.Join(recDir, "inference.pdmodel")
	if err := os.WriteFile(tamperedRecFile, []byte("mutated bytes corrupted"), 0644); err != nil {
		t.Fatalf("tamper rec file: %v", err)
	}
	_, err = p.DetectRegions(ctx, req)
	if err == nil {
		t.Fatal("expected mutated rec dependency to fail closed, got nil error")
	}
	if !strings.Contains(err.Error(), "snapshot verification failed") {
		t.Fatalf("expected snapshot verification failure on mutated dependency, got: %v", err)
	}
}

func TestWorkerSeparatorProvider_VerifiedRuntimeIdentityAndMetadata(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	uvrDir := filepath.Join(tmpDir, "uvr_snap")
	if err := os.MkdirAll(uvrDir, 0755); err != nil {
		t.Fatal(err)
	}
	onnxFile := filepath.Join(uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
	if err := os.WriteFile(onnxFile, []byte("valid_onnx_bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	metaFile := filepath.Join(uvrDir, "mdx_model_data.json")
	if err := os.WriteFile(metaFile, []byte(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	hOnnx := sha256.Sum256([]byte("valid_onnx_bytes"))
	hMeta := sha256.Sum256([]byte(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`))

	uvrManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       provider.UVRModelID,
		ModelVersion:  provider.UVRModelVersion,
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "UVR-MDX-NET-Inst_HQ_4.onnx",
				SHA256:       hex.EncodeToString(hOnnx[:]),
				SizeBytes:    int64(len("valid_onnx_bytes")),
			},
			{
				RelativePath: "mdx_model_data.json",
				SHA256:       hex.EncodeToString(hMeta[:]),
				SizeBytes:    int64(len(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`)),
			},
		},
	}
	cSHA, err := domain.ComputeSnapshotManifestSHA256(&uvrManifest)
	if err != nil {
		t.Fatal(err)
	}
	uvrManifest.SnapshotManifestSHA256 = cSHA
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: provider.UVRModelID,
		Version:        provider.UVRModelVersion,
		SHA256:         cSHA,
		CodeLicense:    "MIT",
		ModelLicense:   "MIT",
		DataLicense:    "Common-Voice",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, uvrManifest, uvrDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot failed: %v", err)
	}

	// Requirement 1 & 7: RegisterAndVerifySnapshot alone does NOT create runtime evidence
	if binding.RuntimeIdentity != nil {
		t.Fatalf("expected nil RuntimeIdentity after RegisterAndVerifySnapshot, got: %+v", binding.RuntimeIdentity)
	}

	sepProvider, err := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
	if err != nil {
		t.Fatal(err)
	}
	sepProvider.SetSnapshotService(snapSvc)
	sepProvider.SetRequiresSnapshot(true)

	// Requirement 7: Separator provider has empty baseline and fails closed when runtime evidence is missing
	if baselineID := sepProvider.ServiceBaselineID(); baselineID != "" {
		t.Fatalf("expected empty baseline ID when runtime identity is unset, got: %s", baselineID)
	}
	_, sepErr := sepProvider.SeparateStems(ctx, provider.SeparationRequest{
		SourceAudio: worker.ArtifactRef{Path: "dummy.wav"},
	})
	if sepErr == nil {
		t.Fatal("expected SeparateStems to fail closed with missing runtime identity")
	}
	if !errors.Is(sepErr, domain.ErrSnapshotUnverified) {
		t.Fatalf("expected ErrSnapshotUnverified, got: %v", sepErr)
	}

	// Requirement 2 & 7: Explicitly validated runtime evidence succeeds and attaches to binding
	validRT := domain.NewUVRRuntimeIdentity(binding.SnapshotManifestSHA256, nil)
	if err := snapSvc.SetRuntimeIdentity(provider.UVRModelID, provider.UVRModelVersion, validRT); err != nil {
		t.Fatalf("SetRuntimeIdentity failed: %v", err)
	}
	if binding.RuntimeIdentity == nil {
		t.Fatal("expected binding to have non-nil RuntimeIdentity after explicit SetRuntimeIdentity")
	}
	if binding.RuntimeIdentity.SourceRevision != domain.PinnedUVRSourceRevision {
		t.Fatalf("expected source revision %s, got %s", domain.PinnedUVRSourceRevision, binding.RuntimeIdentity.SourceRevision)
	}

	baselineID := sepProvider.ServiceBaselineID()
	if !strings.HasPrefix(baselineID, "baseline-worker_uvr-") {
		t.Fatalf("expected evidence-backed baseline ID after explicit runtime attachment, got %s", baselineID)
	}
}
func TestWorkerSeparatorProvider_ProductionRuntimeBootstrap(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	// Build real stageworker binary
	stageWorkerExe := filepath.Join(tmpDir, "stageworker-sep")
	if runtime.GOOS == "windows" {
		stageWorkerExe += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", stageWorkerExe, "github.com/monet88/douyinie/cmd/stageworker")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build stageworker: %v: %s", err, string(out))
	}
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", stageWorkerExe)

	// Setup mock Python runner for separator
	configFile := filepath.Join(tmpDir, "mock_separator_config.json")
	probeCountFile := filepath.Join(tmpDir, "mock_probe_count.txt")
	pyInterpScript := filepath.Join(tmpDir, "mock_separator_interp.py")
	scriptContent := fmt.Sprintf(`import sys, json, os

input_data = sys.stdin.read()
req = json.loads(input_data)
mode = req.get("mode")

config_file = %q
cfg = {}
if config_file and os.path.exists(config_file):
    with open(config_file, "r") as f:
        cfg = json.loads(f.read())

probe_count_file = %q
if mode == "probe" and probe_count_file:
    count = 0
    if os.path.exists(probe_count_file):
        with open(probe_count_file, "r") as f:
            try:
                count = int(f.read().strip())
            except Exception:
                pass
    with open(probe_count_file, "w") as f:
        f.write(str(count + 1))

if mode == "probe":
    probe_err = cfg.get("probe_error")
    if probe_err:
        sys.stderr.write(f"Separator error: RUNTIME_IDENTITY_PROBE_FAILED: {probe_err}\n")
        sys.exit(1)
    out = {
        "status": "ok",
        "package_name": cfg.get("package_name", "audio-separator"),
        "package_version": cfg.get("package_version", "0.47.0"),
        "source_revision": cfg.get("source_revision", "bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab"),
        "runtime_versions": cfg.get("runtime_versions", {"audio-separator": "0.47.0", "onnxruntime": "1.19.0"}),
        "adapter_revision": "cmd/stageworker/adapters/separator.py@v0.47.0"
    }
    print(json.dumps(out))
    sys.exit(0)

out = {
    "vocals_data": "dm9jYWxzX3BjbV9kYXRh",
    "background_data": "YmFja2dyb3VuZF9wY21fZGF0YQ==",
    "vocals_sha256": "3c4b5b9b05090fdf238f38ba5046813982d50e2a652e9cb3324ea79720c3c9c8",
    "background_sha256": "8726e21a993978c7ba086d3872e7608d7d5bfca646ca4aca459ffda844faa8b4",
    "duration_ms": 3000,
    "sample_rate": 16000,
    "channels": 1,
    "model_name": req.get("model_name", "UVR-MDX-NET-Inst_HQ_4.onnx"),
    "model_version": req.get("model_version", "v3"),
    "runtime_identity": "python-audio-separator 0.47.0"
}
print(json.dumps(out))
sys.exit(0)
`, configFile, probeCountFile)

	if err := os.WriteFile(pyInterpScript, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	var pyLauncher string
	if runtime.GOOS == "windows" {
		pyLauncher = filepath.Join(tmpDir, "mock_python_runner.bat")
		batContent := fmt.Sprintf("@echo off\npython -u %q %%*\n", pyInterpScript)
		if err := os.WriteFile(pyLauncher, []byte(batContent), 0755); err != nil {
			t.Fatal(err)
		}
	} else {
		pyLauncher = filepath.Join(tmpDir, "mock_python_runner.sh")
		shContent := fmt.Sprintf("#!/bin/sh\npython3 -u %q \"$@\"\n", pyInterpScript)
		if err := os.WriteFile(pyLauncher, []byte(shContent), 0755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("DOUYINIE_SEPARATOR_PYTHON_BIN", pyLauncher)
	t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", "")
	t.Setenv("DOUYINIE_SEPARATOR_BIN", "")
	setupUVRBinding := func(subDirName string) (string, *governance.SnapshotVerificationBinding) {
		uvrDir := filepath.Join(tmpDir, subDirName)
		if err := os.MkdirAll(uvrDir, 0755); err != nil {
			t.Fatal(err)
		}
		onnxFile := filepath.Join(uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
		if err := os.WriteFile(onnxFile, []byte("valid_onnx_bytes"), 0644); err != nil {
			t.Fatal(err)
		}
		metaFile := filepath.Join(uvrDir, "mdx_model_data.json")
		if err := os.WriteFile(metaFile, []byte(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`), 0644); err != nil {
			t.Fatal(err)
		}

		hOnnx := sha256.Sum256([]byte("valid_onnx_bytes"))
		hMeta := sha256.Sum256([]byte(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`))

		uvrManifest := domain.SnapshotManifest{
			SchemaVersion: "1.0",
			ModelID:       provider.UVRModelID,
			ModelVersion:  provider.UVRModelVersion,
			Files: []domain.SnapshotFileEntry{
				{
					RelativePath: "UVR-MDX-NET-Inst_HQ_4.onnx",
					SHA256:       hex.EncodeToString(hOnnx[:]),
					SizeBytes:    int64(len("valid_onnx_bytes")),
				},
				{
					RelativePath: "mdx_model_data.json",
					SHA256:       hex.EncodeToString(hMeta[:]),
					SizeBytes:    int64(len(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`)),
				},
			},
		}
		cSHA, err := domain.ComputeSnapshotManifestSHA256(&uvrManifest)
		if err != nil {
			t.Fatal(err)
		}
		uvrManifest.SnapshotManifestSHA256 = cSHA

		_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
			ID:             uuid.NewString(),
			DependencyName: provider.UVRModelID,
			Version:        provider.UVRModelVersion,
			SHA256:         cSHA,
			CodeLicense:    "MIT",
			ModelLicense:   "MIT",
			DataLicense:    "Common-Voice",
			ServiceTerms:   "Local-Offline",
			Verified:       true,
			CreatedAt:      time.Now().UTC(),
		})

		binding, err := snapSvc.RegisterAndVerifySnapshot(ctx, uvrManifest, uvrDir)
		if err != nil {
			t.Fatalf("RegisterAndVerifySnapshot failed: %v", err)
		}
		return onnxFile, binding
	}

	t.Run("ValidProbe_AutoBootstrapsWithoutManualSetRuntimeIdentity", func(t *testing.T) {
		onnxFile, binding := setupUVRBinding("uvr_valid")
		_ = onnxFile
		if binding.RuntimeIdentity != nil {
			t.Fatalf("expected nil RuntimeIdentity before probe, got: %+v", binding.RuntimeIdentity)
		}

		// Configure mock probe to return valid evidence
		_ = os.Remove(probeCountFile)
		validCfg := map[string]any{
			"package_name":     "audio-separator",
			"package_version":  "0.47.0",
			"source_revision":  domain.PinnedUVRSourceRevision,
			"runtime_versions": map[string]string{"audio-separator": "0.47.0", "onnxruntime": "1.19.0"},
		}
		cfgBytes, _ := json.Marshal(validCfg)
		if err := os.WriteFile(configFile, cfgBytes, 0644); err != nil {
			t.Fatal(err)
		}

		sepProvider, err := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
		if err != nil {
			t.Fatal(err)
		}
		sepProvider.SetSnapshotService(snapSvc)
		sepProvider.SetRequiresSnapshot(true)

		// Calling ServiceBaselineID or EnsureRuntimeIdentity triggers runtime probe and binding automatically
		baselineID := sepProvider.ServiceBaselineID()
		if !strings.HasPrefix(baselineID, "baseline-worker_uvr-") {
			t.Fatalf("expected auto-bootstrapped baseline ID, got %q", baselineID)
		}
		if binding.RuntimeIdentity == nil {
			t.Fatal("expected binding to have non-nil RuntimeIdentity after auto-bootstrap")
		}
		if binding.RuntimeIdentity.SourceRevision != domain.PinnedUVRSourceRevision {
			t.Fatalf("expected source revision %s, got %s", domain.PinnedUVRSourceRevision, binding.RuntimeIdentity.SourceRevision)
		}

		// Verify repeated call reuses validated evidence without re-probing
		_ = sepProvider.ServiceBaselineID()
		countBytes, _ := os.ReadFile(probeCountFile)
		if string(countBytes) != "1" {
			t.Fatalf("expected exactly 1 probe execution (cached), got: %s", string(countBytes))
		}

		// SeparateStems proceeds past the runtime identity gate without test-only manual SetRuntimeIdentity
		dummyWav := filepath.Join(tmpDir, "dummy_in.wav")
		_ = os.WriteFile(dummyWav, []byte("fake_pcm_wav"), 0644)
		_, err = sepProvider.SeparateStems(ctx, provider.SeparationRequest{
			SourceAudio: worker.ArtifactRef{Path: dummyWav},
		})
		if err != nil && errors.Is(err, domain.ErrSnapshotDigestMismatch) {
			// Expected: runtime identity passed gate; failed at snapshot weights verification due to dummy onnx bytes
		} else if err != nil {
			t.Fatalf("unexpected SeparateStems error: %v", err)
		}
	})

	t.Run("SnapshotMutation_RevokesEligibility", func(t *testing.T) {
		onnxFile, binding := setupUVRBinding("uvr_mutation")
		_ = binding

		validCfg := map[string]any{
			"package_name":     "audio-separator",
			"package_version":  "0.47.0",
			"source_revision":  domain.PinnedUVRSourceRevision,
			"runtime_versions": map[string]string{"audio-separator": "0.47.0"},
		}
		cfgBytes, _ := json.Marshal(validCfg)
		_ = os.WriteFile(configFile, cfgBytes, 0644)

		sepProvider, _ := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
		sepProvider.SetSnapshotService(snapSvc)
		sepProvider.SetRequiresSnapshot(true)

		// First bootstrap succeeds
		if err := sepProvider.EnsureRuntimeIdentity(ctx); err != nil {
			t.Fatalf("expected EnsureRuntimeIdentity to succeed, got: %v", err)
		}

		// Mutate snapshot file on disk
		if err := os.WriteFile(onnxFile, []byte("corrupted_onnx_bytes_after_verification"), 0644); err != nil {
			t.Fatal(err)
		}

		// Subsequent call detects mutation and fails closed
		mutErr := sepProvider.EnsureRuntimeIdentity(ctx)
		if mutErr == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail on snapshot mutation")
		}
		if !errors.Is(mutErr, domain.ErrSnapshotMutatedRehashRequired) {
			t.Fatalf("expected ErrSnapshotMutatedRehashRequired, got: %v", mutErr)
		}
		if baseID := sepProvider.ServiceBaselineID(); baseID != "" {
			t.Fatalf("expected empty baseline ID after mutation revocation, got: %s", baseID)
		}
	})

	t.Run("WrongPackageVersion_FailsClosed", func(t *testing.T) {
		_, _ = setupUVRBinding("uvr_bad_pkg")
		badCfg := map[string]any{
			"package_name":     "audio-separator",
			"package_version":  "0.46.0",
			"source_revision":  domain.PinnedUVRSourceRevision,
			"runtime_versions": map[string]string{"audio-separator": "0.46.0"},
		}
		cfgBytes, _ := json.Marshal(badCfg)
		_ = os.WriteFile(configFile, cfgBytes, 0644)

		sepProvider, _ := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
		sepProvider.SetSnapshotService(snapSvc)
		sepProvider.SetRequiresSnapshot(true)

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail closed on package version mismatch")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
	})

	t.Run("WrongVCSCommit_FailsClosed", func(t *testing.T) {
		_, _ = setupUVRBinding("uvr_bad_commit")
		badCfg := map[string]any{
			"package_name":     "audio-separator",
			"package_version":  "0.47.0",
			"source_revision":  "0000000000000000000000000000000000000000",
			"runtime_versions": map[string]string{"audio-separator": "0.47.0"},
		}
		cfgBytes, _ := json.Marshal(badCfg)
		_ = os.WriteFile(configFile, cfgBytes, 0644)

		sepProvider, _ := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
		sepProvider.SetSnapshotService(snapSvc)
		sepProvider.SetRequiresSnapshot(true)

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail closed on source revision mismatch")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
	})

	t.Run("MissingPEP610DirectURL_FailsClosed", func(t *testing.T) {
		_, _ = setupUVRBinding("uvr_missing_direct_url")
		badCfg := map[string]any{
			"probe_error": "package 'audio-separator' (0.47.0) has no PEP 610 direct_url.json; cannot verify exact VCS commit (hardcoded claims rejected)",
		}
		cfgBytes, _ := json.Marshal(badCfg)
		_ = os.WriteFile(configFile, cfgBytes, 0644)

		sepProvider, _ := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
		sepProvider.SetSnapshotService(snapSvc)
		sepProvider.SetRequiresSnapshot(true)

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail closed when direct_url.json is missing")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
	})

	t.Run("InvalidPythonRuntime_FailsClosed", func(t *testing.T) {
		_, _ = setupUVRBinding("uvr_bad_python")
		t.Setenv("DOUYINIE_SEPARATOR_PYTHON_BIN", filepath.Join(tmpDir, "non_existent_python_binary"))

		sepProvider, _ := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
		sepProvider.SetSnapshotService(snapSvc)
		sepProvider.SetRequiresSnapshot(true)

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail closed on invalid python runtime")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
	})

	t.Run("ArbitraryRunnerOverride_Rejected_EvenIfTrustedHashEnvSet", func(t *testing.T) {
		_, _ = setupUVRBinding("uvr_arbitrary_override")
		// Even if an operator or test sets a would-be trusted runner SHA256, arbitrary adapter/binary overrides are rejected
		t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", filepath.Join(tmpDir, "arbitrary_adapter.py"))
		_ = os.WriteFile(filepath.Join(tmpDir, "arbitrary_adapter.py"), []byte("import sys; sys.exit(0)\n"), 0755)
		t.Setenv("DOUYINIE_TRUSTED_SEPARATOR_RUNNER_SHA256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

		sepProvider, _ := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
		sepProvider.SetSnapshotService(snapSvc)
		sepProvider.SetRequiresSnapshot(true)

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail closed on arbitrary adapter override even with trusted hash")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
		if !strings.Contains(err.Error(), "SEPARATOR_RUNNER_OVERRIDE_REJECTED") {
			t.Fatalf("expected SEPARATOR_RUNNER_OVERRIDE_REJECTED in error, got: %v", err)
		}
	})

	t.Run("ExactPackageVersion_SuffixRejection_FailsClosed", func(t *testing.T) {
		_, _ = setupUVRBinding("uvr_suffix_version")
		t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", "")
		t.Setenv("DOUYINIE_SEPARATOR_BIN", "")
		t.Setenv("DOUYINIE_SEPARATOR_PYTHON_BIN", pyLauncher)
		badCfg := map[string]any{
			"package_version": "0.47.0+modified",
			"runtime_versions": map[string]string{
				"audio-separator": "0.47.0+modified",
			},
		}
		cfgBytes, _ := json.Marshal(badCfg)
		_ = os.WriteFile(configFile, cfgBytes, 0644)

		sepProvider, _ := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
		sepProvider.SetSnapshotService(snapSvc)
		sepProvider.SetRequiresSnapshot(true)

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail closed on package version suffix")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
	})
}

func TestWorkerSeparatorProvider_ArbitraryRunnerOverride_RejectedOnRCHardRoute(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "sep_runner_override.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	// Build real stageworker binary
	stageWorkerExe := filepath.Join(tmpDir, "stageworker-sep-override")
	if runtime.GOOS == "windows" {
		stageWorkerExe += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", stageWorkerExe, "github.com/monet88/douyinie/cmd/stageworker")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build stageworker: %v: %s", err, string(out))
	}
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", stageWorkerExe)
	uvrDir := filepath.Join(tmpDir, "uvr_override")
	_ = os.MkdirAll(uvrDir, 0755)
	onnxFile := filepath.Join(uvrDir, "UVR-MDX-NET-Inst_HQ_4.onnx")
	_ = os.WriteFile(onnxFile, []byte("onnx_bytes"), 0644)
	metaFile := filepath.Join(uvrDir, "mdx_model_data.json")
	_ = os.WriteFile(metaFile, []byte(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`), 0644)

	hOnnx := sha256.Sum256([]byte("onnx_bytes"))
	hMeta := sha256.Sum256([]byte(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`))

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       provider.UVRModelID,
		ModelVersion:  provider.UVRModelVersion,
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "UVR-MDX-NET-Inst_HQ_4.onnx", SHA256: hex.EncodeToString(hOnnx[:]), SizeBytes: int64(len("onnx_bytes"))},
			{RelativePath: "mdx_model_data.json", SHA256: hex.EncodeToString(hMeta[:]), SizeBytes: int64(len(`{"0ddfc0eb5792638ad5dc27850236c246": {"primary_stem": "Vocals"}}`))},
		},
	}
	cSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	manifest.SnapshotManifestSHA256 = cSHA
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: manifest.ModelID,
		Version:        manifest.ModelVersion,
		SHA256:         cSHA,
		CodeLicense:    "MIT",
		ModelLicense:   "MIT",
		DataLicense:    "Common-Voice",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	_, err = snapSvc.RegisterAndVerifySnapshot(ctx, manifest, uvrDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot: %v", err)
	}

	fakeScript := filepath.Join(tmpDir, "fake_self_attest_separator.py")
	fakeCode := `import sys, json
out = {
    "status": "ok",
    "package_name": "audio-separator",
    "package_version": "0.47.0",
    "source_revision": "bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab",
    "runtime_versions": {"audio-separator": "0.47.0"},
    "adapter_revision": "cmd/stageworker/adapters/separator.py@v0.47.0"
}
print(json.dumps(out))
sys.exit(0)
`
	_ = os.WriteFile(fakeScript, []byte(fakeCode), 0755)

	sepProvider, _ := provider.NewWorkerSeparatorProvider("worker_uvr", provider.UVRModelID, provider.UVRModelVersion, 0.95)
	sepProvider.SetSnapshotService(snapSvc)
	sepProvider.SetRequiresSnapshot(true)

	t.Run("CustomAdapterScript_Rejected_EvenIfTrustedHashEnvSet", func(t *testing.T) {
		t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", fakeScript)
		t.Setenv("DOUYINIE_TRUSTED_SEPARATOR_RUNNER_SHA256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail closed on arbitrary adapter override")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
		if !strings.Contains(err.Error(), "SEPARATOR_RUNNER_OVERRIDE_REJECTED") {
			t.Fatalf("expected error to cite SEPARATOR_RUNNER_OVERRIDE_REJECTED, got: %v", err)
		}
	})

	t.Run("ArbitraryBinary_Rejected_EvenIfTrustedHashEnvSet", func(t *testing.T) {
		t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", "")
		t.Setenv("DOUYINIE_SEPARATOR_BIN", "echo")
		t.Setenv("DOUYINIE_TRUSTED_SEPARATOR_RUNNER_SHA256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err == nil {
			t.Fatal("expected EnsureRuntimeIdentity to fail closed on arbitrary binary override")
		}
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected ErrSnapshotUnverified, got: %v", err)
		}
		if !strings.Contains(err.Error(), "SEPARATOR_RUNNER_OVERRIDE_REJECTED") {
			t.Fatalf("expected error to cite SEPARATOR_RUNNER_OVERRIDE_REJECTED, got: %v", err)
		}
	})

	t.Run("RepoAdapter_WithConfiguredPythonPath_Succeeds", func(t *testing.T) {
		t.Setenv("DOUYINIE_SEPARATOR_ADAPTER", "")
		t.Setenv("DOUYINIE_SEPARATOR_BIN", "")
		t.Setenv("DOUYINIE_TRUSTED_SEPARATOR_RUNNER_SHA256", "")

		pyInterp := filepath.Join(tmpDir, "valid_override_interp.py")
		script := fmt.Sprintf(`import sys, json
out = {
    "status": "ok",
    "package_name": "audio-separator",
    "package_version": "0.47.0",
    "source_revision": "bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab",
    "runtime_versions": {"audio-separator": "0.47.0", "onnxruntime": "1.19.0"},
    "adapter_revision": "cmd/stageworker/adapters/separator.py@v0.47.0"
}
print(json.dumps(out))
sys.exit(0)
`)
		_ = os.WriteFile(pyInterp, []byte(script), 0755)

		var pyLauncher string
		if runtime.GOOS == "windows" {
			pyLauncher = filepath.Join(tmpDir, "valid_python_runner.bat")
			_ = os.WriteFile(pyLauncher, []byte(fmt.Sprintf("@echo off\npython -u %q %%*\n", pyInterp)), 0755)
		} else {
			pyLauncher = filepath.Join(tmpDir, "valid_python_runner.sh")
			_ = os.WriteFile(pyLauncher, []byte(fmt.Sprintf("#!/bin/sh\npython3 -u %q \"$@\"\n", pyInterp)), 0755)
		}
		t.Setenv("DOUYINIE_SEPARATOR_PYTHON_BIN", pyLauncher)

		err := sepProvider.EnsureRuntimeIdentity(ctx)
		if err != nil {
			t.Fatalf("expected EnsureRuntimeIdentity to succeed with repo adapter and configured python runner, got: %v", err)
		}
	})
}

// Item 12: Worker bridge must fail closed when audio_role class map is missing from snapshot manifest
func TestWorkerAudioRoleProvider_MissingClassMapFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	yamnetDir := filepath.Join(tmpDir, "yamnet_incomplete")
	if err := os.MkdirAll(yamnetDir, 0755); err != nil {
		t.Fatalf("mkdir yamnet dir: %v", err)
	}
	tflitePath := filepath.Join(yamnetDir, "yamnet.tflite")
	if err := os.WriteFile(tflitePath, []byte("fake_tflite"), 0644); err != nil {
		t.Fatalf("write fake tflite: %v", err)
	}

	// Manifest with tflite but MISSING class map!
	manifest := domain.SnapshotManifest{
		SchemaVersion: "1",
		ModelID:       provider.YAMNetModelID,
		ModelVersion:  provider.YAMNetModelVersion,
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "yamnet.tflite",
				SizeBytes:    int64(len("fake_tflite")),
				SHA256:       domain.PinnedYAMNetArtifactSHA256,
			},
		},
	}
	snapSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		t.Fatalf("compute snapshot manifest sha: %v", err)
	}
	manifest.SnapshotManifestSHA256 = snapSHA

	_, err = domain.ResolveAudioRoleClassMapPath(manifest, yamnetDir)
	if err == nil {
		t.Fatal("expected ResolveAudioRoleClassMapPath to fail when class map missing from manifest")
	}
	if !errors.Is(err, domain.ErrAudioRoleModelAssetMissing) {
		t.Fatalf("expected ErrAudioRoleModelAssetMissing, got %v", err)
	}
}
