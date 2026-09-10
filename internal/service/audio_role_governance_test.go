package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// mockRouterAudioRoleProvider simulates an AudioRole provider (such as YAMNet)
// with controllable policy, license, snapshot, and invocation tracking.
type mockRouterAudioRoleProvider struct {
	id               string
	modelName        string
	modelVersion     string
	policy           domain.PolicyState
	healthy          bool
	requiresSnapshot bool
	invoked          *bool
	configHash       string
	snapshotSHA      string
	runtimeSHA       string
	qualityScore     float64
	errToReturn      error
	segments         []domain.AudioSegment
}

func (m *mockRouterAudioRoleProvider) SnapshotRuntimeIdentity() (snapshotSHA, runtimeSHA string, err error) {
	return m.snapshotSHA, m.runtimeSHA, nil
}
func (m *mockRouterAudioRoleProvider) ID() string                      { return m.id }
func (m *mockRouterAudioRoleProvider) Type() provider.ProviderType     { return provider.TypeAudioRole }
func (m *mockRouterAudioRoleProvider) PolicyState() domain.PolicyState { return m.policy }
func (m *mockRouterAudioRoleProvider) IsHealthy() bool                 { return m.healthy }
func (m *mockRouterAudioRoleProvider) RequiresSnapshot() bool          { return m.requiresSnapshot }
func (m *mockRouterAudioRoleProvider) ModelInfo() (string, string) {
	return m.modelName, m.modelVersion
}
func (m *mockRouterAudioRoleProvider) Capability() domain.ProviderCapability {
	qs := m.qualityScore
	if qs == 0 {
		qs = 0.95
	}
	return domain.ProviderCapability{
		Stage:          string(provider.TypeAudioRole),
		Languages:      []string{"*"},
		ExecutionTier:  "local",
		CostPerUnit:    0,
		QualityScore:   qs,
		MaxConcurrency: 1,
		Features:       []string{"yamnet_classification"},
	}
}

func (m *mockRouterAudioRoleProvider) AnalyzerInfo() (providerID, modelName, modelVersion, configHash string) {
	return m.id, m.modelName, m.modelVersion, m.configHash
}

func (m *mockRouterAudioRoleProvider) AnalyzeAudioRoles(ctx context.Context, req domain.AudioRoleAnalysisRequest) (*domain.AudioRoleAnalysisResult, error) {
	if m.invoked != nil {
		*m.invoked = true
	}
	if m.errToReturn != nil {
		return nil, m.errToReturn
	}
	segs := m.segments
	if len(segs) == 0 {
		segs = []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		}
	}
	return &domain.AudioRoleAnalysisResult{
		Segments:        segs,
		ProviderID:      m.id,
		ModelName:       m.modelName,
		ModelVersion:    m.modelVersion,
		RuntimeIdentity: "mock_runtime",
	}, nil
}

type govHarness struct {
	db          *storage.DB
	casStore    *cas.Store
	reg         *provider.Registry
	router      *provider.Router
	polSvc      *governance.PolicyService
	licSvc      *governance.LicenseService
	snapSvc     *governance.SnapshotService
	audioMixSvc *service.AudioMixService
	audioRole   *service.AudioRoleService
}

func setupGovHarness(t *testing.T) *govHarness {
	t.Helper()
	dir := t.TempDir()

	dbPath := filepath.Join(dir, "test_gov.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	casStore, err := cas.NewStore(dir)
	if err != nil {
		t.Fatalf("new cas: %v", err)
	}

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)
	reg := provider.NewRegistry()
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, nil, db)
	router.SetSnapshotService(snapSvc)

	audioMixSvc := service.NewAudioMixService(db, casStore)
	audioMixSvc.ConfigureRouter(router)

	// Register fake separator provider so SeparateAudio succeeds on routed path
	sep := provider.NewFakeSeparatorProvider("fake_uvr_separator")
	_ = reg.Register(sep)
	sepName, sepVer := sep.ModelInfo()
	_ = licSvc.RegisterManifest(context.Background(), domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: sepName,
		Version:        sepVer,
		SHA256:         "sha256_mock_sep",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	// Production AudioRoleService with NO deterministic analyzer fallback
	audioRole := service.NewAudioRoleService(db, casStore, audioMixSvc)
	audioRole.ConfigureRouter(router)

	t.Cleanup(func() {
		_ = db.Close()
	})

	return &govHarness{
		db:          db,
		casStore:    casStore,
		reg:         reg,
		router:      router,
		polSvc:      polSvc,
		licSvc:      licSvc,
		snapSvc:     snapSvc,
		audioMixSvc: audioMixSvc,
		audioRole:   audioRole,
	}
}

func createTestMediaAndPreflight(t *testing.T, h *govHarness, assetID string) {
	t.Helper()
	ctx := context.Background()
	samples := make([]int16, 24000)
	for i := range samples {
		samples[i] = int16(i % 1000)
	}
	dummy := media.EncodePCM16Samples(samples, 16000, 1)
	obj, err := h.casStore.Put(bytes.NewReader(dummy))
	if err != nil {
		t.Fatalf("put dummy cas: %v", err)
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

	if err := h.db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              obj.SHA256,
		CASPath:             obj.Path,
		ByteSize:            int64(len(dummy)),
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	if err := h.db.SavePreflightReport(ctx, domain.PreflightReport{
		ID:                     uuid.NewString(),
		AssetID:                assetID,
		DurationSec:            1.5,
		DurationMs:             1500,
		NormalizedAudioCASPath: obj.Path,
		NormalizedAudioSHA256:  obj.SHA256,
		CreatedAt:              time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}
}

// Case A: Policy-blocked YAMNet provider -> fails before StageWorker invocation.
func TestAudioRoleService_Governance_PolicyBlocked_FailsBeforeInvocation(t *testing.T) {
	h := setupGovHarness(t)
	ctx := context.Background()

	invoked := false
	prov := &mockRouterAudioRoleProvider{
		id:               "yamnet_blocked",
		modelName:        domain.PinnedYAMNetModelID,
		modelVersion:     domain.PinnedYAMNetModelVersion,
		policy:           domain.PolicyBlocked, // Policy blocked!
		healthy:          true,
		requiresSnapshot: false,
		invoked:          &invoked,
		configHash:       "conf_hash_1",
	}
	if err := h.reg.Register(prov); err != nil {
		t.Fatalf("register prov: %v", err)
	}

	assetID := "asset_blocked_" + uuid.NewString()[:8]
	createTestMediaAndPreflight(t, h, assetID)

	runID := "run_" + uuid.NewString()[:8]
	_, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
	})

	if err == nil {
		t.Fatal("expected policy-blocked provider to fail, got nil err")
	}
	if invoked {
		t.Fatal("FAIL: provider was invoked despite being policy-blocked!")
	}
}

// Case B: Missing/unverified license manifest -> fails before invocation.
func TestAudioRoleService_Governance_MissingLicenseManifest_FailsBeforeInvocation(t *testing.T) {
	h := setupGovHarness(t)
	ctx := context.Background()

	invoked := false
	prov := &mockRouterAudioRoleProvider{
		id:               "yamnet_unlicensed",
		modelName:        domain.PinnedYAMNetModelID,
		modelVersion:     domain.PinnedYAMNetModelVersion,
		policy:           domain.PolicyAllowed,
		healthy:          true,
		requiresSnapshot: false,
		invoked:          &invoked,
		configHash:       "conf_hash_1",
	}
	if err := h.reg.Register(prov); err != nil {
		t.Fatalf("register prov: %v", err)
	}
	// Note: We intentionally DO NOT register license manifest for YAMNet here!

	assetID := "asset_unlicensed_" + uuid.NewString()[:8]
	createTestMediaAndPreflight(t, h, assetID)

	runID := "run_" + uuid.NewString()[:8]
	_, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
	})

	if err == nil {
		t.Fatal("expected unmanifested provider to fail, got nil err")
	}
	if invoked {
		t.Fatal("FAIL: provider was invoked despite missing license manifest!")
	}
}

// Case C: Mutated snapshot -> fails before invocation.
func TestAudioRoleService_Governance_MutatedSnapshot_FailsBeforeInvocation(t *testing.T) {
	h := setupGovHarness(t)
	ctx := context.Background()

	// 1. Set up valid snapshot on disk
	snapDir := t.TempDir()
	tflitePath := filepath.Join(snapDir, "yamnet.tflite")
	initialBytes := []byte("VALID_YAMNET_TFLITE_MODEL_DATA")
	if err := os.WriteFile(tflitePath, initialBytes, 0644); err != nil {
		t.Fatalf("write tflite: %v", err)
	}
	sum := sha256.Sum256(initialBytes)
	fileHash := hex.EncodeToString(sum[:])

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       domain.PinnedYAMNetModelID,
		ModelVersion:  domain.PinnedYAMNetModelVersion,
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "yamnet.tflite",
				SHA256:       fileHash,
				SizeBytes:    int64(len(initialBytes)),
			},
		},
	}
	manifestSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		t.Fatalf("compute manifest sha: %v", err)
	}

	// Register matching license manifest
	_ = h.licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: domain.PinnedYAMNetModelID,
		Version:        domain.PinnedYAMNetModelVersion,
		SHA256:         manifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "CC-BY-4.0",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	// Register and verify snapshot initially
	_, err = h.snapSvc.RegisterAndVerifySnapshot(ctx, manifest, snapDir)
	if err != nil {
		t.Fatalf("register and verify snapshot: %v", err)
	}

	// 2. Mutate the snapshot on disk after verification!
	mutatedBytes := []byte("MUTATED_YAMNET_TFLITE_CORRUPTED")
	if err := os.WriteFile(tflitePath, mutatedBytes, 0644); err != nil {
		t.Fatalf("mutate tflite: %v", err)
	}

	invoked := false
	prov := &mockRouterAudioRoleProvider{
		id:               "yamnet_mutated_snap",
		modelName:        domain.PinnedYAMNetModelID,
		modelVersion:     domain.PinnedYAMNetModelVersion,
		policy:           domain.PolicyAllowed,
		healthy:          true,
		requiresSnapshot: true,
		invoked:          &invoked,
		configHash:       "conf_hash_1",
	}
	if err := h.reg.Register(prov); err != nil {
		t.Fatalf("register prov: %v", err)
	}

	assetID := "asset_mutated_" + uuid.NewString()[:8]
	createTestMediaAndPreflight(t, h, assetID)

	runID := "run_" + uuid.NewString()[:8]
	_, err = h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
	})

	if err == nil {
		t.Fatal("expected mutated/unverified snapshot to fail, got nil err")
	}
	if invoked {
		t.Fatal("FAIL: provider was invoked despite mutated/invalid snapshot!")
	}
}

// Case D: Allowed, verified license -> selection + attempt are recorded by canonical router path.
func TestAudioRoleService_Governance_AllowedVerified_RecordsCanonicalProvenance(t *testing.T) {
	h := setupGovHarness(t)
	ctx := context.Background()

	// 1. Register verified license manifest
	if err := provider.BootstrapYAMNetLicenseManifest(ctx, h.licSvc); err != nil {
		t.Fatalf("bootstrap license: %v", err)
	}

	invoked := false
	prov := &mockRouterAudioRoleProvider{
		id:               "yamnet_verified",
		modelName:        domain.PinnedYAMNetModelID,
		modelVersion:     domain.PinnedYAMNetModelVersion,
		policy:           domain.PolicyAllowed,
		healthy:          true,
		requiresSnapshot: false, // Snapshot bypassed for unit router test
		invoked:          &invoked,
		configHash:       "conf_hash_verified",
	}
	if err := h.reg.Register(prov); err != nil {
		t.Fatalf("register prov: %v", err)
	}

	assetID := "asset_verified_" + uuid.NewString()[:8]
	createTestMediaAndPreflight(t, h, assetID)

	runID := "run_verified_" + uuid.NewString()[:8]
	plan, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
	})
	if err != nil {
		t.Fatalf("generate audio role plan failed: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if !invoked {
		t.Fatal("expected provider to be invoked")
	}

	// Verify canonical governance records in SQLite
	decisions, err := h.db.ListSelectionDecisions(ctx, runID, "audio_role_plan")
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	if len(decisions) == 0 {
		t.Fatal("expected SelectionDecision recorded by Router")
	}
	if decisions[0].SelectedProviderID != "yamnet_verified" {
		t.Fatalf("expected selected provider yamnet_verified, got %s", decisions[0].SelectedProviderID)
	}
	if decisions[0].Stage != "audio_role_plan" {
		t.Fatalf("expected SelectionDecision stage audio_role_plan, got %s", decisions[0].Stage)
	}

	attempts, err := h.db.ListProviderAttempts(ctx, runID, "audio_role_plan")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) == 0 {
		t.Fatal("expected ProviderAttempt recorded by Router")
	}
	if attempts[0].Status != "succeeded" {
		t.Fatalf("expected attempt status succeeded, got %s", attempts[0].Status)
	}
	if attempts[0].Stage != "audio_role_plan" {
		t.Fatalf("expected ProviderAttempt stage audio_role_plan, got %s", attempts[0].Stage)
	}
	if attempts[0].InputHash != plan.ProvenanceHash {
		t.Fatalf("expected attempt InputHash to bind canonical audio role plan provenance, got %s, want %s",
			attempts[0].InputHash, plan.ProvenanceHash)
	}
}

// Case E: No production provider available -> fails closed without fallback to deterministic analyzer.
func TestAudioRoleService_Governance_NoProviderAvailable_FailsClosedWithoutFallback(t *testing.T) {
	h := setupGovHarness(t)
	ctx := context.Background()

	// Note: Registry has only separator, NO audio role provider!
	assetID := "asset_empty_reg_" + uuid.NewString()[:8]
	createTestMediaAndPreflight(t, h, assetID)

	runID := "run_empty_" + uuid.NewString()[:8]
	plan, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
	})

	if err == nil {
		t.Fatal("expected fail-closed when no provider is available, got nil error")
	}
	if plan != nil {
		t.Fatalf("expected nil plan, got %+v", plan)
	}
	if !errors.Is(err, domain.ErrNoEligibleProvider) && !errors.Is(err, domain.ErrAudioRoleAnalyzerUnavailable) {
		t.Fatalf("expected ErrNoEligibleProvider or ErrAudioRoleAnalyzerUnavailable, got unexpected error: %v", err)
	}
}

// Item 4: Verified runtime/snapshot identity changes invalidate AudioRolePlan cache
func TestAudioRoleService_Governance_RuntimeIdentityChange_InvalidatesCache(t *testing.T) {
	h := setupGovHarness(t)
	ctx := context.Background()

	invokedCount := 0
	p := &mockRouterAudioRoleProvider{
		id:               "mock_yamnet_runtime_test",
		modelName:        "yamnet",
		modelVersion:     "v1",
		policy:           domain.PolicyAllowed,
		healthy:          true,
		requiresSnapshot: false,
		snapshotSHA:      "snap_sha_initial_11111111111111111111111111111111",
		runtimeSHA:       "rt_sha_initial_22222222222222222222222222222222",
		segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1000, Role: domain.AudioRoleNarrationDialogue},
		},
	}
	if err := h.reg.Register(p); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	if err := h.licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "yamnet",
		Version:        "v1",
		SHA256:         "sha256_mock_yamnet",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register manifest: %v", err)
	}

	assetID := "asset_runtime_inval_" + uuid.NewString()[:8]
	createTestMediaAndPreflight(t, h, assetID)

	// Run 1: Fresh generation with runtime SHA 2222...
	p.invoked = new(bool)
	plan1, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   "run_initial_" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("run 1 failed: %v", err)
	}
	if !*p.invoked {
		t.Fatalf("expected provider invoked on run 1")
	}
	invokedCount++

	// Run 2: Exact same runtime -> cache HIT, provider NOT invoked
	p.invoked = new(bool)
	plan2, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   "run_second_" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("run 2 failed: %v", err)
	}
	if *p.invoked {
		t.Fatalf("expected cache hit on run 2, but provider was invoked")
	}
	if plan2.ID != plan1.ID {
		t.Fatalf("expected plan2 ID == plan1 ID, got %s vs %s", plan2.ID, plan1.ID)
	}

	// Run 3: Runtime changed (e.g. ai-edge-litert upgrade or adapter revision)
	// Provenance hash changes -> cache MUST miss and fresh plan generated!
	p.runtimeSHA = "rt_sha_upgraded_33333333333333333333333333333333"
	p.invoked = new(bool)
	plan3, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   "run_third_" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("run 3 failed: %v", err)
	}
	if !*p.invoked {
		t.Fatalf("expected provider invoked on run 3 due to runtime change, but got cache hit")
	}
	if plan3.ID == plan1.ID {
		t.Fatalf("expected new plan ID after runtime identity change, but got stale plan ID %s", plan3.ID)
	}
}

// Item 9: Router/AudioRole fallback ProviderAttempt.InputHash binds actual provider B, not selected A
func TestAudioRoleService_Governance_FallbackCandidate_BindsProviderBInputHash(t *testing.T) {
	h := setupGovHarness(t)
	ctx := context.Background()

	// Provider A: Primary, fails transiently
	pA := &mockRouterAudioRoleProvider{
		id:           "mock_primary_failing",
		modelName:    "yamnet_primary",
		modelVersion: "v1",
		policy:       domain.PolicyAllowed,
		healthy:      true,
		qualityScore: 0.99,
		configHash:   "cfg_hash_A",
		errToReturn:  errors.New("transient neural engine bus error"),
	}
	// Provider B: Fallback, succeeds
	pB := &mockRouterAudioRoleProvider{
		id:           "mock_fallback_succeeding",
		modelName:    "yamnet_fallback",
		modelVersion: "v2",
		policy:       domain.PolicyAllowed,
		healthy:      true,
		qualityScore: 0.85,
		configHash:   "cfg_hash_B",
		segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		},
	}

	if err := h.reg.Register(pA); err != nil {
		t.Fatalf("register provider pA: %v", err)
	}
	if err := h.reg.Register(pB); err != nil {
		t.Fatalf("register provider pB: %v", err)
	}
	for _, p := range []*mockRouterAudioRoleProvider{pA, pB} {
		mName, mVer := p.ModelInfo()
		if err := h.licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
			ID:             uuid.NewString(),
			DependencyName: mName,
			Version:        mVer,
			SHA256:         "sha256_" + mName,
			CodeLicense:    "Apache-2.0",
			ModelLicense:   "Apache-2.0",
			DataLicense:    "OpenData",
			ServiceTerms:   "Standard",
			Verified:       true,
			CreatedAt:      time.Now().UTC(),
		}); err != nil {
			t.Fatalf("register manifest for %s: %v", mName, err)
		}
	}

	assetID := "asset_fallback_hash_" + uuid.NewString()[:8]
	createTestMediaAndPreflight(t, h, assetID)
	runID := "run_fb_" + uuid.NewString()[:8]

	plan, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   runID,
	})
	if err != nil {
		t.Fatalf("expected fallback to succeed, got %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan from fallback")
	}

	attempts, err := h.db.ListProviderAttempts(ctx, runID, "audio_role_plan")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) < 2 {
		t.Fatalf("expected at least 2 attempts (failing primary + successful fallback), got %d: %+v", len(attempts), attempts)
	}

	var attemptA, attemptB *domain.ProviderAttempt
	for i := range attempts {
		if attempts[i].ProviderID == pA.ID() {
			attemptA = &attempts[i]
		} else if attempts[i].ProviderID == pB.ID() {
			attemptB = &attempts[i]
		}
	}

	if attemptA == nil || attemptB == nil {
		t.Fatalf("missing attempt records: A=%v, B=%v", attemptA, attemptB)
	}

	// Provenance hash for A and B must be DIFFERENT because modelName, modelVer, configHash differ!
	if attemptA.InputHash == attemptB.InputHash {
		t.Fatalf("Item 9 violation: attempt B reused attempt A input hash %s instead of binding provider B provenance", attemptB.InputHash)
	}

	preflight, err := h.db.GetPreflightReport(ctx, assetID)
	if err != nil {
		t.Fatalf("get preflight: %v", err)
	}
	stemsCASHash := ""
	if stemsIdx, err := h.db.GetAudioStemsArtifactIndex(ctx, assetID); err == nil && stemsIdx != nil {
		stemsCASHash = stemsIdx.CASHash
	}
	expectedHashB := domain.ComputeAudioRolePlanProvenanceHash(
		preflight.NormalizedAudioSHA256,
		stemsCASHash,
		pB.ID(),
		pB.modelName,
		pB.modelVersion,
		pB.configHash,
	)
	if attemptB.InputHash != expectedHashB {
		t.Fatalf("attempt B InputHash mismatch: expected %s, got %s", expectedHashB, attemptB.InputHash)
	}
}

// Item 10: Production WorkerAudioRoleProvider must fail closed when runtime identity
// cannot be verified, even if a stale empty-runtime cache artifact exists in SQLite/CAS.
func TestAudioRoleService_Governance_WorkerAudioRoleProvider_UnverifiedRuntimeFailsClosedBeforeCacheLookup(t *testing.T) {
	h := setupGovHarness(t)
	ctx := context.Background()

	assetID := "asset_unverified_rt_" + uuid.NewString()[:8]
	createTestMediaAndPreflight(t, h, assetID)

	workerProv, err := provider.NewWorkerAudioRoleProvider("worker_yamnet_prod_test", "yamnet", "v1", 0.95)
	if err != nil {
		t.Fatalf("new worker provider: %v", err)
	}
	workerProv.SetSnapshotService(h.snapSvc)
	if err := h.reg.Register(workerProv); err != nil {
		t.Fatalf("register worker provider: %v", err)
	}

	snapDir := t.TempDir()
	tflitePath := filepath.Join(snapDir, "yamnet.tflite")
	initialBytes := []byte("VALID_YAMNET_TFLITE_MODEL_DATA")
	if err := os.WriteFile(tflitePath, initialBytes, 0644); err != nil {
		t.Fatalf("write tflite: %v", err)
	}
	sum := sha256.Sum256(initialBytes)
	fileHash := hex.EncodeToString(sum[:])

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "yamnet",
		ModelVersion:  "v1",
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "yamnet.tflite",
				SHA256:       fileHash,
				SizeBytes:    int64(len(initialBytes)),
			},
		},
	}
	manifestSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		t.Fatalf("compute manifest sha: %v", err)
	}

	if err := h.licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "yamnet",
		Version:        "v1",
		SHA256:         manifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "CC-BY-4.0",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register manifest: %v", err)
	}

	binding, err := h.snapSvc.RegisterAndVerifySnapshot(ctx, manifest, snapDir)
	if err != nil {
		t.Fatalf("register and verify snapshot: %v", err)
	}
	snapSHA := binding.SnapshotManifestSHA256
	preflight, err := h.db.GetPreflightReport(ctx, assetID)
	if err != nil {
		t.Fatalf("get preflight: %v", err)
	}
	stems, err := h.audioMixSvc.SeparateAudio(ctx, service.AudioSeparationInput{
		AssetID: assetID,
		RunID:   "run_prep",
	})
	if err != nil {
		t.Fatalf("separate audio: %v", err)
	}
	stemsCASHash := stems.CASHash

	// Stale cache artifact stored under old-style empty-runtime provenance hash
	staleProvHash := domain.ComputeAudioRolePlanProvenanceHash(
		preflight.NormalizedAudioSHA256,
		stemsCASHash,
		workerProv.ID(),
		"yamnet",
		"v1",
		workerProv.Config().Hash(),
		snapSHA,
		"", // empty runtime SHA!
	)
	stalePlan := domain.AudioRolePlan{
		ID:             "stale-plan-" + uuid.NewString()[:8],
		AssetID:        assetID,
		ProviderID:     workerProv.ID(),
		ModelName:      "yamnet",
		ModelVersion:   "v1",
		ProvenanceHash: staleProvHash,
		Segments: []domain.AudioSegment{
			{StartMs: 0, EndMs: 1500, Role: domain.AudioRoleNarrationDialogue},
		},
		CreatedAt: time.Now().UTC(),
	}
	blob, err := json.Marshal(stalePlan)
	if err != nil {
		t.Fatalf("marshal stale plan: %v", err)
	}
	obj, err := h.casStore.Put(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("put stale plan to cas: %v", err)
	}
	stalePlan.CASHash = obj.SHA256
	if err := h.db.SaveAudioRolePlan(ctx, stalePlan); err != nil {
		t.Fatalf("save stale plan to db: %v", err)
	}

	// GenerateAudioRolePlan MUST fail closed because WorkerAudioRoleProvider's runtime identity
	// cannot be verified (no probe / invalid runtime). It MUST NOT return stalePlan!
	plan, err := h.audioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   "run_stale_lookup_test",
	})
	if err == nil {
		t.Fatalf("expected fail-closed error when runtime identity cannot be verified, but got cache hit plan: %+v", plan)
	}
	if plan != nil {
		t.Fatalf("expected nil plan, got %+v", plan)
	}
	if !errors.Is(err, domain.ErrAudioRoleAnalyzerUnavailable) && !errors.Is(err, domain.ErrSnapshotUnverified) {
		t.Fatalf("expected ErrAudioRoleAnalyzerUnavailable or ErrSnapshotUnverified, got: %v", err)
	}

	// Injected deterministic analyzer must continue working cleanly
	detAnalyzer := service.NewDeterministicTestAudioRoleAnalyzer()
	detAudioRole := service.NewAudioRoleServiceWithAnalyzer(h.db, h.casStore, h.audioMixSvc, detAnalyzer)
	detPlan, err := detAudioRole.GenerateAudioRolePlan(ctx, service.AudioRolePlanInput{
		AssetID: assetID,
		RunID:   "run_det_test",
	})
	if err != nil {
		t.Fatalf("deterministic injected analyzer must succeed, got: %v", err)
	}
	if detPlan == nil || len(detPlan.Segments) == 0 {
		t.Fatal("deterministic injected analyzer produced empty plan")
	}
}
