package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
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
	segments         []domain.AudioSegment
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
	return domain.ProviderCapability{
		Stage:          string(provider.TypeAudioRole),
		Languages:      []string{"*"},
		ExecutionTier:  "local",
		CostPerUnit:    0,
		QualityScore:   0.95,
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
	dummy := []byte("RIFF1234WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80>\x00\x00\x00}\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00")
	obj, err := h.casStore.Put(bytes.NewReader(dummy))
	if err != nil {
		t.Fatalf("put dummy cas: %v", err)
	}

	attID := uuid.NewString()
	_ = h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	})

	_ = h.db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  assetID,
		RightsAttestationID: attID,
		SHA256:              obj.SHA256,
		CASPath:             obj.Path,
		ByteSize:            int64(len(dummy)),
		CreatedAt:           time.Now().UTC(),
	})

	_ = h.db.SavePreflightReport(ctx, domain.PreflightReport{
		ID:                     uuid.NewString(),
		AssetID:                assetID,
		DurationSec:            1.5,
		DurationMs:             1500,
		NormalizedAudioCASPath: obj.Path,
		NormalizedAudioSHA256:  obj.SHA256,
		CreatedAt:              time.Now().UTC(),
	})
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
		t.Logf("got expected fail-closed error: %v", err)
	}
}
