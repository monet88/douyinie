package provider_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

func TestRouter_TranslationLocalProfile_DisablesTranslationProviders(t *testing.T) {
	ctx := context.Background()
	reg := provider.NewRegistry()

	// Register remote gateway providers
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
		"baseline-deepseek-v4-2026-08",
		0.95,
		"http://127.0.0.1:8080",
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	// Register local Qwen provider
	qwen, _ := provider.NewWorkerTranslationProvider("qwen3_4b_translator", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M", 0.85)
	qwen.SetRequiresSnapshot(false) // bypass snapshot for pure router profile test
	_ = reg.Register(qwen)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)

	_, err := router.Route(ctx, provider.RouteRequest{
		RunID:            uuid.NewString(),
		Stage:            provider.TypeTranslation,
		Language:         "vi",
		ExecutionProfile: domain.ExecutionProfileLocal,
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected local translation routing to fail closed with ErrNoEligibleProvider, got: %v", err)
	}
}

func TestRouter_TranslationHybridProfile_DeterministicOrdering(t *testing.T) {
	ctx := context.Background()
	reg := provider.NewRegistry()

	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
		"baseline-deepseek-v4-2026-08",
		0.95,
		"http://127.0.0.1:8080",
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	qwen, _ := provider.NewWorkerTranslationProvider("qwen3_4b_translator", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M", 0.85)
	qwen.SetRequiresSnapshot(false)
	_ = reg.Register(qwen)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)

	res, err := router.Route(ctx, provider.RouteRequest{
		RunID:            uuid.NewString(),
		Stage:            provider.TypeTranslation,
		Language:         "vi",
		ExecutionProfile: domain.ExecutionProfileHybrid,
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	// 1. Primary must be Gateway Gemini (highest quality 0.99 in hybrid profile)
	if res.SelectedProvider.ID() != provider.GatewayGeminiTranslationProviderID {
		t.Fatalf("expected primary provider gateway_gemini_3_8_flash, got: %s", res.SelectedProvider.ID())
	}

	// 2. Fallbacks must contain DeepSeek only; local Qwen is not production-eligible for translation.
	if len(res.FallbackOrdered) != 1 {
		t.Fatalf("expected 1 fallback provider, got %d", len(res.FallbackOrdered))
	}
	if res.FallbackOrdered[0].ID() != provider.GatewayDeepSeekTranslationProviderID {
		t.Fatalf("expected first fallback gateway_deepseek_v4_1_flash, got: %s", res.FallbackOrdered[0].ID())
	}
	for _, cand := range res.Decision.CandidatesEvaluated {
		if cand.ProviderID == "qwen3_4b_translator" {
			if cand.Eligible {
				t.Fatal("local Qwen must be ineligible for Hybrid translation")
			}
			if cand.RejectionCode != "TRANSLATION_LOCAL_PROVIDER_EXCLUDED" {
				t.Fatalf("expected TRANSLATION_LOCAL_PROVIDER_EXCLUDED, got %s", cand.RejectionCode)
			}
		}
	}
}

func TestRouter_TranslationHybridProfile_FrozenOrderingRegardlessOfQuality(t *testing.T) {
	ctx := context.Background()
	reg := provider.NewRegistry()

	// Deliberately give DeepSeek a higher quality score than Gemini
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.80, // Lower quality score
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
		"baseline-deepseek-v4-2026-08",
		0.99, // Higher quality score
		"http://127.0.0.1:8080",
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	qwen, _ := provider.NewWorkerTranslationProvider("qwen3_4b_translator", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M", 0.99)
	qwen.SetRequiresSnapshot(false)
	_ = reg.Register(qwen)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)

	res, err := router.Route(ctx, provider.RouteRequest{
		RunID:            uuid.NewString(),
		Stage:            provider.TypeTranslation,
		Language:         "vi",
		ExecutionProfile: domain.ExecutionProfileHybrid,
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	// Frozen production ordering MUST still hold: Gemini > DeepSeek, with no local LLM fallback.
	if res.SelectedProvider.ID() != provider.GatewayGeminiTranslationProviderID {
		t.Fatalf("expected primary provider gateway_gemini_3_8_flash, got: %s", res.SelectedProvider.ID())
	}
	if len(res.FallbackOrdered) != 1 {
		t.Fatalf("expected 1 fallback provider, got %d", len(res.FallbackOrdered))
	}
	if res.FallbackOrdered[0].ID() != provider.GatewayDeepSeekTranslationProviderID {
		t.Fatalf("expected first fallback gateway_deepseek_v4_1_flash, got: %s", res.FallbackOrdered[0].ID())
	}
}

func TestRouter_TranslationHybridProfile_FrozenOrderingIgnoresPreferredProviderOverride(t *testing.T) {
	ctx := context.Background()
	reg := provider.NewRegistry()

	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.90,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	deepseek, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayDeepSeekTranslationProviderID,
		provider.GatewayDeepSeekModelAlias,
		"baseline-deepseek-v4-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	deepseek.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(deepseek)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)
	res, err := router.Route(ctx, provider.RouteRequest{
		RunID:               uuid.NewString(),
		Stage:               provider.TypeTranslation,
		Language:            "vi",
		ExecutionProfile:    domain.ExecutionProfileHybrid,
		PreferredProviderID: provider.GatewayDeepSeekTranslationProviderID,
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if res.SelectedProvider.ID() != provider.GatewayGeminiTranslationProviderID {
		t.Fatalf("translation ladder must ignore preferred-provider override; got primary %s", res.SelectedProvider.ID())
	}
	if len(res.FallbackOrdered) != 1 || res.FallbackOrdered[0].ID() != provider.GatewayDeepSeekTranslationProviderID {
		t.Fatalf("expected frozen fallback %s, got %+v", provider.GatewayDeepSeekTranslationProviderID, res.FallbackOrdered)
	}
}

func TestRouter_TranslationCloudProfile_RejectsLocalProvider(t *testing.T) {
	ctx := context.Background()
	reg := provider.NewRegistry()

	qwen, _ := provider.NewWorkerTranslationProvider("qwen3_4b_translator", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M", 0.99)
	qwen.SetRequiresSnapshot(false)
	_ = reg.Register(qwen)

	router := provider.NewRouter(reg, nil, nil, nil, nil, nil)
	_, err := router.Route(ctx, provider.RouteRequest{
		RunID:            uuid.NewString(),
		Stage:            provider.TypeTranslation,
		Language:         "en",
		ExecutionProfile: domain.ExecutionProfileCloud,
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected cloud translation to reject local provider and fail closed, got: %v", err)
	}
}

func TestRouter_TranslationLocalProfile_FailsClosedIfLocalQwenUnverified(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	reg := provider.NewRegistry()
	qwen, _ := provider.NewWorkerTranslationProvider("qwen3_4b_translator", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M", 0.85)
	qwen.SetRequiresSnapshot(true) // requires verified model snapshot (#64)
	_ = reg.Register(qwen)

	router := provider.NewRouter(reg, nil, licSvc, nil, nil, db)
	router.SetSnapshotService(snapSvc)

	// Since "qwen3_4b_translator" snapshot has NOT been verified, router must fail closed with ErrNoEligibleProvider
	_, err = router.Route(ctx, provider.RouteRequest{
		RunID:            uuid.NewString(),
		Stage:            provider.TypeTranslation,
		Language:         "vi",
		ExecutionProfile: domain.ExecutionProfileLocal,
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider for unverified snapshot, got: %v", err)
	}
}

func TestRouter_TranslationAttemptProvenance_RecordsObservedModelAndBaseline(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	reg := provider.NewRegistry()
	gemini, _ := provider.NewGatewayTranslationProvider(
		provider.GatewayGeminiTranslationProviderID,
		provider.GatewayGeminiModelAlias,
		"baseline-gemini-3.8-flash-2026-08",
		0.99,
		"http://127.0.0.1:8080",
	)
	gemini.SetPolicyState(domain.PolicyAllowed)
	_ = reg.Register(gemini)

	router := provider.NewRouter(reg, nil, nil, nil, nil, db)

	runID := "run-provenance-test"
	err = router.ExecuteWithRetry(ctx, provider.RouteRequest{
		RunID:            runID,
		Stage:            provider.TypeTranslation,
		Language:         "vi",
		ExecutionProfile: domain.ExecutionProfileCloud,
	}, "input-hash-123", 1, func(p provider.Provider, attemptNumber int) error {
		return nil // succeed
	})
	if err != nil {
		t.Fatalf("ExecuteWithRetry failed: %v", err)
	}

	attempts, err := db.ListProviderAttempts(ctx, runID, string(provider.TypeTranslation))
	if err != nil || len(attempts) != 1 {
		t.Fatalf("expected 1 attempt, got err=%v count=%d", err, len(attempts))
	}

	att := attempts[0]
	if att.ProviderID != provider.GatewayGeminiTranslationProviderID {
		t.Fatalf("unexpected provider id: %s", att.ProviderID)
	}
	if att.ObservedModel != provider.GatewayGeminiModelAlias {
		t.Fatalf("expected observed model 'gemini-3.8-flash', got %q", att.ObservedModel)
	}
	if att.ServiceBaselineID != "baseline-gemini-3.8-flash-2026-08" {
		t.Fatalf("expected service baseline 'baseline-gemini-3.8-flash-2026-08', got %q", att.ServiceBaselineID)
	}
	if att.ModelName != "" {
		t.Fatalf("remote gateway attempt model_name must be empty, got %q", att.ModelName)
	}
}
