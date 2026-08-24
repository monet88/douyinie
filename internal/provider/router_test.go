package provider_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

func setupTestRouter(t *testing.T) (*provider.Router, *storage.DB, *provider.Registry, *governance.PolicyService, *governance.LicenseService, *governance.CredentialService) {
	t.Helper()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("setup SQLite failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := provider.NewSeam1FakeRegistry()
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	ctx := context.Background()
	for _, p := range reg.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
				ID:             uuid.NewString(),
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_dummy_" + mName,
				SourceRepo:     "test/" + mName,
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Apache-2.0",
				DataLicense:    "OpenData",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
	}

	circuit := provider.NewCircuitBreaker(provider.CircuitBreakerConfig{
		FailureThreshold: 2,
		CooldownDuration: 100 * time.Millisecond,
		SuccessThreshold: 1,
	})

	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, circuit, db)
	return router, db, reg, polSvc, licSvc, credSvc
}

func TestRouter_PolicyBeforeHealth(t *testing.T) {
	router, _, _, polSvc, _, _ := setupTestRouter(t)
	ctx := context.Background()

	// "fake_indextts2_blocked" has QualityScore 0.99 and Healthy=true, but Policy=BLOCKED.
	// Router should NEVER select it.
	res, err := router.Route(ctx, provider.RouteRequest{
		RunID:    uuid.NewString(),
		Stage:    provider.TypeTTS,
		Language: "vi",
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	if res.SelectedProvider.ID() == "fake_indextts2_blocked" {
		t.Fatalf("policy invariant violation: healthy but policy-blocked provider was selected!")
	}
	if res.SelectedProvider.ID() != "fake_vieneu_tts_vi" {
		t.Errorf("expected fake_vieneu_tts_vi, got %s", res.SelectedProvider.ID())
	}

	// Dynamically block fake_vieneu_tts_vi via PolicyService
	_ = polSvc.SetPolicy(ctx, "fake_vieneu_tts_vi", domain.PolicyBlocked, "temporary restriction")

	// Now routing for VI should fail since the other candidate is also blocked
	_, err = router.Route(ctx, provider.RouteRequest{
		RunID:    uuid.NewString(),
		Stage:    provider.TypeTTS,
		Language: "vi",
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Errorf("expected ErrNoEligibleProvider when all candidates are blocked, got %v", err)
	}
}

func TestRouter_ConsentRequirement(t *testing.T) {
	router, _, _, polSvc, _, _ := setupTestRouter(t)
	ctx := context.Background()

	// Block local provider so only cloud consent provider remains
	_ = polSvc.SetPolicy(ctx, "fake_vieneu_tts_vi", domain.PolicyBlocked, "disabled")

	// 1. Without consent -> rejection
	_, err := router.Route(ctx, provider.RouteRequest{
		RunID:          uuid.NewString(),
		Stage:          provider.TypeTTS,
		Language:       "vi",
		ConsentGranted: false,
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Errorf("expected failure when consent is not granted, got %v", err)
	}

	// 2. With consent -> selected
	res, err := router.Route(ctx, provider.RouteRequest{
		RunID:          uuid.NewString(),
		Stage:          provider.TypeTTS,
		Language:       "vi",
		ConsentGranted: true,
	})
	if err != nil {
		t.Fatalf("expected success with consent, got %v", err)
	}
	if res.SelectedProvider.ID() != "fake_cloud_tts_consent" {
		t.Errorf("expected fake_cloud_tts_consent, got %s", res.SelectedProvider.ID())
	}
}

func TestRouter_RequiredFeaturesEnforcement(t *testing.T) {
	router, _, _, _, _, _ := setupTestRouter(t)
	ctx := context.Background()

	// 1. Request OCR with feature "fit_content_geometry" -> matches fake_paddle_ocr
	res, err := router.Route(ctx, provider.RouteRequest{
		RunID:            uuid.NewString(),
		Stage:            provider.TypeOCR,
		Language:         "zh",
		RequiredFeatures: []string{"fit_content_geometry"},
	})
	if err != nil {
		t.Fatalf("Route with supported feature failed: %v", err)
	}
	if res.SelectedProvider.ID() != "fake_paddle_ocr" {
		t.Errorf("expected fake_paddle_ocr, got %s", res.SelectedProvider.ID())
	}

	// 2. Request OCR with non-existent feature -> rejection
	_, err = router.Route(ctx, provider.RouteRequest{
		RunID:            uuid.NewString(),
		Stage:            provider.TypeOCR,
		Language:         "zh",
		RequiredFeatures: []string{"unsupported_exotic_feature"},
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Errorf("expected ErrNoEligibleProvider for unsupported feature, got %v", err)
	}
}

func TestRouter_CredentialBackedAuthorization(t *testing.T) {
	router, _, _, polSvc, _, credSvc := setupTestRouter(t)
	ctx := context.Background()

	// Block standard ASR so only authorization-required provider remains
	_ = polSvc.SetPolicy(ctx, "fake_qwen3_asr", domain.PolicyBlocked, "disabled")

	// 1. Passing provider ID directly without valid credential reference -> Fail-closed
	_, err := router.Route(ctx, provider.RouteRequest{
		RunID:                 uuid.NewString(),
		Stage:                 provider.TypeASR,
		Language:              "zh",
		AuthorizedCredentials: []string{"fake_auth_cloud_asr"}, // Caller self-authorizing attempt
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider when self-authorizing without valid CredentialRef, got %v", err)
	}

	// 2. Register valid CredentialRef for the provider, but backing credential does NOT exist yet
	credRef := domain.CredentialRef{
		ID:          uuid.NewString(),
		Name:        "enterprise_cloud_asr_ref",
		ProviderID:  "fake_auth_cloud_asr",
		StorageType: "env_ref",
		KeyRef:      "enterprise_token_vault_key",
		CreatedAt:   time.Now().UTC(),
	}
	if err := credSvc.RegisterCredentialRef(ctx, credRef); err != nil {
		t.Fatalf("RegisterCredentialRef failed: %v", err)
	}

	// 2a. Routing with unbacked CredentialRef fails closed (cannot self-authorize with unbacked persisted ref)
	_, err = router.Route(ctx, provider.RouteRequest{
		RunID:                 uuid.NewString(),
		Stage:                 provider.TypeASR,
		Language:              "zh",
		AuthorizedCredentials: []string{credRef.ID},
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider when backing key is not verifiable, got %v", err)
	}

	// 3. Set backing environment variable -> Routing with valid CredentialRef succeeds and selects provider
	t.Setenv("enterprise_token_vault_key", "secret_enterprise_token_value")
	res, err := router.Route(ctx, provider.RouteRequest{
		RunID:                 uuid.NewString(),
		Stage:                 provider.TypeASR,
		Language:              "zh",
		AuthorizedCredentials: []string{credRef.ID},
	})
	if err != nil {
		t.Fatalf("Route with valid credential reference failed: %v", err)
	}
	if res.SelectedProvider.ID() != "fake_auth_cloud_asr" {
		t.Errorf("expected fake_auth_cloud_asr, got %s", res.SelectedProvider.ID())
	}
}

func TestRouter_ExecuteWithRetry_QualityVsTransient(t *testing.T) {
	router, db, _, _, _, _ := setupTestRouter(t)
	ctx := context.Background()
	runID := uuid.NewString()

	req := provider.RouteRequest{
		RunID:    runID,
		Stage:    provider.TypeTTS,
		Language: "vi",
	}

	// 1. Transient Retry Test: attempt 1 fails transiently, attempt 2 succeeds on same provider
	callCount := 0
	err := router.ExecuteWithRetry(ctx, req, "input_sha_123", 3, func(p provider.Provider, attempt int) error {
		callCount++
		if attempt < 2 {
			return errors.New("transient timeout")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteWithRetry transient failed: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls for transient retry, got %d", callCount)
	}

	// 2. Quality Rejection Test: quality gate rejects candidate -> logged as quality_failed and advances to fallback candidate without retrying same candidate
	runIDQuality := uuid.NewString()
	reqQuality := provider.RouteRequest{
		RunID:          runIDQuality,
		Stage:          provider.TypeTTS,
		Language:       "vi",
		ConsentGranted: true, // Allows fallback to cloud TTS
	}
	qualityAttempts := 0
	var invokedProviders []string
	err = router.ExecuteWithRetry(ctx, reqQuality, "input_sha_456", 3, func(p provider.Provider, attempt int) error {
		qualityAttempts++
		invokedProviders = append(invokedProviders, p.ID())
		if p.ID() == "fake_vieneu_tts_vi" {
			// Quality evaluation gate rejection
			return domain.ErrQualityRejected
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteWithRetry quality fallback failed: %v", err)
	}
	if len(invokedProviders) < 2 || invokedProviders[0] != "fake_vieneu_tts_vi" || invokedProviders[1] != "fake_cloud_tts_consent" {
		t.Errorf("expected fallback to fake_cloud_tts_consent after quality failure on vieneu, got %v", invokedProviders)
	}

	// Verify attempt statuses in SQLite
	qAttempts, err := db.ListProviderAttempts(ctx, runIDQuality, "tts")
	if err != nil {
		t.Fatalf("ListProviderAttempts failed: %v", err)
	}
	if len(qAttempts) != 2 {
		t.Fatalf("expected 2 attempts recorded in DB, got %d", len(qAttempts))
	}
	if qAttempts[0].Status != "quality_failed" || qAttempts[1].Status != "succeeded" {
		t.Errorf("unexpected attempt statuses: [0]=%s, [1]=%s", qAttempts[0].Status, qAttempts[1].Status)
	}
}

func TestRouter_CircuitBreaker_SkipsAndRecordsProvenance(t *testing.T) {
	router, db, reg, _, licSvc, _ := setupTestRouter(t)
	ctx := context.Background()

	// Register third candidate for TTS and its license manifest to verify fallback continuation after circuit_broken
	thirdProv := provider.NewFakeTTSProvider("fake_z_fallback_tts_vi", 1600)
	thirdCap := thirdProv.Capability()
	thirdCap.QualityScore = 0.80
	thirdProv.Cap = thirdCap
	_ = reg.Register(thirdProv)
	mName, mVer := thirdProv.ModelInfo()
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		DependencyName: mName,
		Version:        mVer,
		SHA256:         "sha256_mock_" + mName,
		SourceRepo:     "test/" + mName,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	runID := uuid.NewString()
	req := provider.RouteRequest{
		RunID:            runID,
		Stage:            provider.TypeTTS,
		Language:         "vi",
		ExecutionProfile: domain.ExecutionProfileCloud,
		ConsentGranted:   true,
	}

	// In the execution with Cloud profile:
	// Candidate 1 is fake_cloud_tts_consent (highest cloud score)
	// Candidate 2 is fake_vieneu_tts_vi
	// Candidate 3 is fake_z_fallback_tts_vi
	// When candidate 1 runs, it fails with quality error, and trips candidate 2's circuit breaker.
	// When router falls back to candidate 2, it detects circuit open -> logs circuit_broken attempt -> falls back to candidate 3 -> succeeds!
	var executedProviders []string
	err := router.ExecuteWithRetry(ctx, req, "input_circuit_2", 2, func(p provider.Provider, attempt int) error {
		executedProviders = append(executedProviders, p.ID())
		if p.ID() == "fake_cloud_tts_consent" {
			// Trip circuit breaker on fallback candidate fake_vieneu_tts_vi
			router.Circuit().RecordFailure("fake_vieneu_tts_vi", true)
			router.Circuit().RecordFailure("fake_vieneu_tts_vi", true)
			return domain.ErrQualityRejected
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteWithRetry fallback after circuit open failed: %v", err)
	}

	if len(executedProviders) != 2 || executedProviders[0] != "fake_cloud_tts_consent" || executedProviders[1] != "fake_z_fallback_tts_vi" {
		t.Errorf("expected execution on candidate 1 then candidate 3 (skipping candidate 2), got: %v", executedProviders)
	}

	// Verify attempt records in DB for runID:
	// Attempt 1: fake_cloud_tts_consent with status="quality_failed"
	// Attempt 2: fake_vieneu_tts_vi with status="circuit_broken"
	// Attempt 3: fake_z_fallback_tts_vi with status="succeeded"
	attempts, err := db.ListProviderAttempts(ctx, runID, "tts")
	if err != nil {
		t.Fatalf("ListProviderAttempts failed: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts (quality_failed + circuit_broken + succeeded), got %d: %+v", len(attempts), attempts)
	}
	if attempts[0].Status != "quality_failed" || attempts[0].ProviderID != "fake_cloud_tts_consent" {
		t.Errorf("expected attempt 0 to be quality_failed on fake_cloud_tts_consent, got %+v", attempts[0])
	}
	if attempts[1].Status != "circuit_broken" || attempts[1].ProviderID != "fake_vieneu_tts_vi" {
		t.Errorf("expected attempt 1 to be circuit_broken on fake_vieneu_tts_vi, got %+v", attempts[1])
	}
	if attempts[2].Status != "succeeded" || attempts[2].ProviderID != "fake_z_fallback_tts_vi" {
		t.Errorf("expected attempt 2 to be succeeded on fake_z_fallback_tts_vi, got %+v", attempts[2])
	}
}

func TestRouter_UnmanifestedCheckpointFailClosed(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("setup SQLite failed: %v", err)
	}
	defer db.Close()

	reg := provider.NewRegistry()
	_ = reg.Register(&provider.FakeASRProvider{
		BaseFakeProvider: provider.BaseFakeProvider{
			ProviderID:   "unmanifested_asr",
			ProviderType: provider.TypeASR,
			Policy:       domain.PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:         string(provider.TypeASR),
				Languages:     []string{"zh"},
				ExecutionTier: "local",
				QualityScore:  0.95,
			},
			ModelName:    "unmanifested_model_checkpoint",
			ModelVersion: "1.0",
		},
	})

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db) // Empty manifest registry!
	credSvc := governance.NewCredentialService(db)
	circuit := provider.NewCircuitBreaker(provider.CircuitBreakerConfig{})

	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, circuit, db)

	// Routing should FAIL-CLOSED because "unmanifested_model_checkpoint" is not in license manifests
	_, err = router.Route(context.Background(), provider.RouteRequest{
		RunID:    uuid.NewString(),
		Stage:    provider.TypeASR,
		Language: "zh",
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected fail-closed ErrNoEligibleProvider for unmanifested checkpoint, got %v", err)
	}
}

func TestRouter_ExecuteWithRetry_InputHashAndExecutorValidation(t *testing.T) {
	router, _, _, _, _, _ := setupTestRouter(t)
	ctx := context.Background()
	req := provider.RouteRequest{
		RunID:    uuid.NewString(),
		Stage:    provider.TypeTTS,
		Language: "vi",
	}

	// 1. Missing inputHash -> returns error
	err := router.ExecuteWithRetry(ctx, req, "", 1, func(p provider.Provider, attempt int) error {
		return nil
	})
	if err == nil || err.Error() != "input_hash is required" {
		t.Fatalf("expected 'input_hash is required' error, got %v", err)
	}

	// 2. Whitespace inputHash -> returns error
	err = router.ExecuteWithRetry(ctx, req, "   ", 1, func(p provider.Provider, attempt int) error {
		return nil
	})
	if err == nil || err.Error() != "input_hash is required" {
		t.Fatalf("expected 'input_hash is required' error for whitespace hash, got %v", err)
	}

	// 3. Nil executeFn -> returns error
	err = router.ExecuteWithRetry(ctx, req, "valid_hash", 1, nil)
	if err == nil || err.Error() != "execute function is required" {
		t.Fatalf("expected 'execute function is required' error, got %v", err)
	}
}

func TestRouter_ExecuteWithRetry_HonorsExcludedProviders(t *testing.T) {
	router, _, _, _, _, _ := setupTestRouter(t)
	ctx := context.Background()
	req := provider.RouteRequest{
		RunID:             uuid.NewString(),
		Stage:             provider.TypeTTS,
		Language:          "vi",
		ConsentGranted:    true,
		ExcludedProviders: []string{"fake_vieneu_tts_vi"},
	}

	var executedProvider string
	err := router.ExecuteWithRetry(ctx, req, "hash_excluded_test", 1, func(p provider.Provider, attempt int) error {
		executedProvider = p.ID()
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteWithRetry failed: %v", err)
	}

	if executedProvider == "fake_vieneu_tts_vi" {
		t.Fatalf("CRITICAL INVARIANT VIOLATION: excluded provider was executed!")
	}
	if executedProvider != "fake_cloud_tts_consent" {
		t.Errorf("expected fake_cloud_tts_consent, got %s", executedProvider)
	}
}

func TestRouter_FallbackSelectionDecision_PersistsEffectivePolicyState(t *testing.T) {
	router, db, _, polSvc, _, _ := setupTestRouter(t)
	ctx := context.Background()
	runID := uuid.NewString()

	// "fake_cloud_tts_consent" has declared PolicyState = REQUIRES_EXPLICIT_CONSENT.
	// Operator overrides policy to ALLOWED via PolicyService.
	if err := polSvc.SetPolicy(ctx, "fake_cloud_tts_consent", domain.PolicyAllowed, "operator explicit override to allowed"); err != nil {
		t.Fatalf("SetPolicy failed: %v", err)
	}

	req := provider.RouteRequest{
		RunID:          runID,
		Stage:          provider.TypeTTS,
		Language:       "vi",
		ConsentGranted: false, // Consent is false, but PolicyService says ALLOWED!
	}

	// First candidate fake_vieneu_tts_vi fails quality; fallback selects fake_cloud_tts_consent.
	err := router.ExecuteWithRetry(ctx, req, "input_hash_fallback_provenance", 1, func(p provider.Provider, attempt int) error {
		if p.ID() == "fake_vieneu_tts_vi" {
			return domain.ErrQualityRejected
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteWithRetry failed: %v", err)
	}

	// Query recorded selection decisions from DB
	decisions, err := db.ListSelectionDecisions(ctx, runID, "tts")
	if err != nil {
		t.Fatalf("ListSelectionDecisions failed: %v", err)
	}
	if len(decisions) < 2 {
		t.Fatalf("expected at least 2 selection decisions (initial + fallback), got %d", len(decisions))
	}

	fallbackDec := decisions[1]
	if fallbackDec.SelectedProviderID != "fake_cloud_tts_consent" {
		t.Errorf("expected fallback provider fake_cloud_tts_consent, got %s", fallbackDec.SelectedProviderID)
	}

	// Must persist the effective policy state "ALLOWED" actually returned by PolicyService,
	// NOT the declared provider state "REQUIRES_EXPLICIT_CONSENT".
	if fallbackDec.PolicyCheckResult != "ALLOWED" {
		t.Errorf("expected PolicyCheckResult to be 'ALLOWED', got %s", fallbackDec.PolicyCheckResult)
	}
	if len(fallbackDec.CandidatesEvaluated) > 0 && fallbackDec.CandidatesEvaluated[0].PolicyState != "ALLOWED" {
		t.Errorf("expected candidate evaluation PolicyState to be 'ALLOWED', got %s", fallbackDec.CandidatesEvaluated[0].PolicyState)
	}
}

func TestRouter_PerProviderRetryPolicyOverride(t *testing.T) {
	router, db, reg, _, licSvc, _ := setupTestRouter(t)
	ctx := context.Background()

	// Register a custom provider with MaxRetries = 1 (no retries after first attempt)
	oneRetry := 1
	p1 := provider.NewFakeTTSProvider("fake_onetry_tts", 1600)
	cap1 := p1.Capability()
	cap1.MaxRetries = &oneRetry
	p1.Cap = cap1
	_ = reg.Register(p1)

	mName, mVer := p1.ModelInfo()
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		DependencyName: mName,
		Version:        mVer,
		SHA256:         "sha256_mock_" + mName,
		SourceRepo:     "test/" + mName,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	runID := uuid.NewString()
	req := provider.RouteRequest{
		RunID:             runID,
		Stage:             provider.TypeTTS,
		Language:          "vi",
		ExcludedProviders: []string{"fake_vieneu_tts_vi", "fake_cloud_tts_consent", "fake_indextts2_blocked"},
	}

	callCount := 0
	// Even though shared maxRetries is 5, provider p1 has MaxRetries=1 -> should execute only 1 time!
	err := router.ExecuteWithRetry(ctx, req, "hash_per_prov_retry", 5, func(p provider.Provider, attempt int) error {
		callCount++
		return errors.New("transient error")
	})

	if err == nil {
		t.Fatalf("expected error from all candidates failing")
	}
	if callCount != 1 {
		t.Errorf("expected per-provider retry override (MaxRetries=1) to limit attempts to 1, got %d", callCount)
	}

	// Verify attempt logged in DB
	attempts, err := db.ListProviderAttempts(ctx, runID, "tts")
	if err != nil {
		t.Fatalf("ListProviderAttempts failed: %v", err)
	}
	if len(attempts) != 1 {
		t.Errorf("expected exactly 1 attempt in DB, got %d", len(attempts))
	}
}

func TestRouter_UnknownPolicyFailsClosed(t *testing.T) {
	router, _, reg, _, licSvc, _ := setupTestRouter(t)
	ctx := context.Background()

	// Provider with empty PolicyState (undeclared / unknown policy)
	pUnknown := provider.NewFakeTTSProvider("fake_unknown_policy_tts", 1600)
	pUnknown.Policy = "" // Undeclared policy
	_ = reg.Register(pUnknown)

	mName, mVer := pUnknown.ModelInfo()
	_ = licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		DependencyName: mName,
		Version:        mVer,
		SHA256:         "sha256_mock_" + mName,
		SourceRepo:     "test/" + mName,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	req := provider.RouteRequest{
		RunID:             uuid.NewString(),
		Stage:             provider.TypeTTS,
		Language:          "vi",
		ExcludedProviders: []string{"fake_vieneu_tts_vi", "fake_cloud_tts_consent", "fake_indextts2_blocked"},
	}

	_, err := router.Route(ctx, req)
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Errorf("expected ErrNoEligibleProvider for provider with undeclared policy, got %v", err)
	}
}
