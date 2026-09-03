package provider_test

import (
	"context"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

// TestProductionSpeechRegistry_ContainsAllRequiredCapabilities verifies Finding 1:
// the production speech registry is populated with concrete worker-backed ASR 1.7B,
// ASR 0.6B fallback, forced aligner, and diarization capability. No fake providers exist.
func TestProductionSpeechRegistry_ContainsAllRequiredCapabilities(t *testing.T) {
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

	// Verify Finding 1: Every production model-backed provider requires a snapshot by default
	for _, p := range reg.ListAll() {
		rsp, ok := p.(interface{ RequiresSnapshot() bool })
		if !ok {
			t.Fatalf("production provider %s does not implement RequiresSnapshot()", p.ID())
		}
		if !rsp.RequiresSnapshot() {
			t.Fatalf("expected production provider %s to require snapshot by default", p.ID())
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
