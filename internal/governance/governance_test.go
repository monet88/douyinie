package governance_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/storage"
)

func TestPolicyService_StateTransitionsAndEvaluation(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	ps := governance.NewPolicyService(db)
	ctx := context.Background()

	// Default state
	defState := ps.GetPolicy(ctx, "prov_1", domain.PolicyAllowed)
	if defState != domain.PolicyAllowed {
		t.Errorf("expected default ALLOWED, got %s", defState)
	}

	// Transition to BLOCKED
	if err := ps.SetPolicy(ctx, "prov_1", domain.PolicyBlocked, "license violation"); err != nil {
		t.Fatalf("SetPolicy failed: %v", err)
	}

	ok, state, err := ps.EvaluateEligibility(ctx, "prov_1", domain.PolicyAllowed, false, false)
	if ok || state != domain.PolicyBlocked || !errors.Is(err, domain.ErrPolicyBlocked) {
		t.Errorf("expected blocked error, got ok=%v, state=%s, err=%v", ok, state, err)
	}

	// Transition to REQUIRES_EXPLICIT_CONSENT
	_ = ps.SetPolicy(ctx, "prov_1", domain.PolicyRequiresExplicitConsent, "third party cloud")
	ok, _, err = ps.EvaluateEligibility(ctx, "prov_1", domain.PolicyAllowed, false, false)
	if ok || !errors.Is(err, domain.ErrConsentRequired) {
		t.Errorf("expected consent required error, got ok=%v, err=%v", ok, err)
	}
	ok, _, err = ps.EvaluateEligibility(ctx, "prov_1", domain.PolicyAllowed, true, false)
	if !ok || err != nil {
		t.Errorf("expected eligible with consent, got ok=%v, err=%v", ok, err)
	}
}

func TestLicenseService_FourObligationLayersAndFailClosed(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	ls := governance.NewLicenseService(db)
	ctx := context.Background()

	// Missing obligation layer
	err = ls.RegisterManifest(ctx, domain.LicenseManifestEntry{
		DependencyName: "checkpoint_a",
		SHA256:         "sha256_hash",
		CodeLicense:    "MIT",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "", // Missing
		ServiceTerms:   "Standard",
	})
	if err == nil {
		t.Errorf("expected error when DataLicense missing")
	}

	// Complete manifest v1
	err = ls.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "checkpoint_a",
		Version:        "1.0.0",
		SHA256:         "sha256_hash_123",
		SourceRepo:     "github.com/monet88/models",
		CodeLicense:    "MIT",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RegisterManifest failed: %v", err)
	}

	// Register manifest v2 for same dependency (versioned immutable history)
	err = ls.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "checkpoint_a",
		Version:        "2.0.0",
		SHA256:         "sha256_hash_456",
		SourceRepo:     "github.com/monet88/models",
		CodeLicense:    "MIT",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatalf("RegisterManifest v2 failed: %v", err)
	}

	// Checkpoint verification success for v1
	if err := ls.VerifyCheckpoint(ctx, "checkpoint_a", "1.0.0", "sha256_hash_123"); err != nil {
		t.Errorf("VerifyCheckpoint v1 failed: %v", err)
	}

	// Checkpoint verification success for v2
	if err := ls.VerifyCheckpoint(ctx, "checkpoint_a", "2.0.0", "sha256_hash_456"); err != nil {
		t.Errorf("VerifyCheckpoint v2 failed: %v", err)
	}

	// Hash mismatch fail-closed
	if err := ls.VerifyCheckpoint(ctx, "checkpoint_a", "1.0.0", "wrong_hash"); err == nil {
		t.Errorf("expected hash mismatch failure")
	}

	// Unregistered checkpoint fail-closed
	if err := ls.VerifyCheckpoint(ctx, "unregistered_checkpoint", "", ""); !errors.Is(err, domain.ErrLicenseManifestMissing) {
		t.Errorf("expected ErrLicenseManifestMissing, got %v", err)
	}

	// Exact-version lookup must fail-closed if version not registered (never fallback to v2 or latest)
	_, err = ls.GetManifest(ctx, "checkpoint_a", "3.0.0")
	if !errors.Is(err, domain.ErrLicenseManifestMissing) {
		t.Errorf("expected exact-version lookup for missing version 3.0.0 to fail-closed with ErrLicenseManifestMissing, got %v", err)
	}
	if err := ls.VerifyCheckpoint(ctx, "checkpoint_a", "3.0.0", "sha256_hash_123"); !errors.Is(err, domain.ErrLicenseManifestMissing) {
		t.Errorf("expected VerifyCheckpoint for missing version 3.0.0 to fail-closed, got %v", err)
	}

	// List manifests for checkpoint_a preserves both versions
	allVersions, err := ls.ListManifests(ctx, "checkpoint_a")
	if err != nil || len(allVersions) != 2 {
		t.Fatalf("expected 2 versions in manifest history, got %d (err: %v)", len(allVersions), err)
	}
}

func TestCredentialService_RejectsRawSecretsAndValidatesAuth(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	cs := governance.NewCredentialService(db)
	ctx := context.Background()

	// Safe reference with key_ref "douyin_cookie_ref_01"
	safeRef := domain.CredentialRef{
		ID:          uuid.NewString(),
		Name:        "douyin_auth",
		ProviderID:  "fake_auth_provider",
		StorageType: "env_ref",
		KeyRef:      "douyin_cookie_ref_01",
	}
	if err := cs.RegisterCredentialRef(ctx, safeRef); err != nil {
		t.Fatalf("RegisterCredentialRef safe failed: %v", err)
	}

	// 1. Default resolver fails-closed when backing credential is not in environment
	ok, err := cs.ValidateCredentialRef(ctx, safeRef.ID, "fake_auth_provider")
	if ok || !errors.Is(err, domain.ErrAuthRequired) {
		t.Errorf("expected fail-closed ErrAuthRequired when backing key is not in environment, got ok=%v, err=%v", ok, err)
	}

	// 2. Setting backing environment variable makes credential verifiable in default resolver
	t.Setenv("douyin_cookie_ref_01", "authenticated_cookie_session_token")
	ok, err = cs.ValidateCredentialRef(ctx, safeRef.ID, "fake_auth_provider")
	if !ok || err != nil {
		t.Errorf("expected valid auth resolution with backed env var, got ok=%v, err=%v", ok, err)
	}

	// 3. Provider mismatch fails closed even if backing key exists
	ok, err = cs.ValidateCredentialRef(ctx, safeRef.ID, "different_provider")
	if ok || !errors.Is(err, domain.ErrAuthRequired) {
		t.Errorf("expected provider mismatch auth error, got ok=%v, err=%v", ok, err)
	}

	// 4. Raw secret in name/key_ref rejected
	rawRef1 := domain.CredentialRef{
		Name:        "leaked_key",
		StorageType: "env_ref",
		KeyRef:      "sk-1234567890abcdef1234567890",
	}
	if err := cs.RegisterCredentialRef(ctx, rawRef1); !errors.Is(err, domain.ErrRawSecretForbidden) {
		t.Errorf("expected ErrRawSecretForbidden for sk- key, got %v", err)
	}

	rawRef2 := domain.CredentialRef{
		Name:        "bearer_token",
		StorageType: "env_ref",
		KeyRef:      "Bearer eyJhbGciOi...",
	}
	if err := cs.RegisterCredentialRef(ctx, rawRef2); !errors.Is(err, domain.ErrRawSecretForbidden) {
		t.Errorf("expected ErrRawSecretForbidden for Bearer token, got %v", err)
	}

	// 5. Passing raw secret string to ValidateCredentialRef fails closed
	ok, err = cs.ValidateCredentialRef(ctx, "sk-raw-secret-attempt", "fake_auth_provider")
	if ok || !errors.Is(err, domain.ErrRawSecretForbidden) {
		t.Errorf("expected ErrRawSecretForbidden when validating raw secret string, got %v", err)
	}

	// 6. Persisted CredentialRef alone must NOT prove authorization if backing reference fails in resolver seam
	fakeResolver := governance.NewMapCredentialResolver(map[string]bool{
		"authorized_key_01": true,
	})
	cs.SetResolver(fakeResolver)

	// safeRef has "douyin_cookie_ref_01" which is not authorized in fakeResolver -> fails closed
	ok, err = cs.ValidateCredentialRef(ctx, safeRef.ID, "fake_auth_provider")
	if ok || !errors.Is(err, domain.ErrAuthRequired) {
		t.Errorf("expected ErrAuthRequired when backing resolver does not authorize key, got ok=%v, err=%v", ok, err)
	}

	// Authorize key in fakeResolver -> succeeds
	fakeResolver.Authorize("douyin_cookie_ref_01")
	ok, err = cs.ValidateCredentialRef(ctx, safeRef.ID, "fake_auth_provider")
	if !ok || err != nil {
		t.Errorf("expected success after fakeResolver authorized key, got ok=%v, err=%v", ok, err)
	}
}

func TestPolicyService_UnknownPolicyFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	ps := governance.NewPolicyService(db)
	ctx := context.Background()

	// Unknown provider with empty default policy -> fails closed (BLOCKED)
	state := ps.GetPolicy(ctx, "completely_unknown_provider", "")
	if state != domain.PolicyBlocked {
		t.Errorf("expected unknown provider policy without default to fail closed to BLOCKED, got %s", state)
	}

	ok, effState, err := ps.EvaluateEligibility(ctx, "completely_unknown_provider", "", true, true)
	if ok || effState != domain.PolicyBlocked || !errors.Is(err, domain.ErrPolicyBlocked) {
		t.Errorf("expected EvaluateEligibility for unknown provider to fail closed, got ok=%v, state=%s, err=%v", ok, effState, err)
	}
}

func TestLicenseService_SameVersionShadowingRejected(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	ls := governance.NewLicenseService(db)
	ctx := context.Background()

	entry1 := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "model_weights_v1",
		Version:        "1.0.0",
		SHA256:         "sha256_orig_111",
		SourceRepo:     "github.com/monet88/weights",
		CodeLicense:    "MIT",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := ls.RegisterManifest(ctx, entry1); err != nil {
		t.Fatalf("RegisterManifest entry1 failed: %v", err)
	}

	// Attempt to register same (DependencyName, Version) again with different SHA / metadata -> must fail
	entry2 := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "model_weights_v1",
		Version:        "1.0.0",
		SHA256:         "sha256_shadow_222",
		SourceRepo:     "github.com/monet88/weights",
		CodeLicense:    "MIT",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC().Add(time.Hour),
	}
	err = ls.RegisterManifest(ctx, entry2)
	if err == nil {
		t.Fatalf("expected error when attempting to overwrite/shadow existing version 1.0.0, got nil")
	}

	// Verify original manifest is preserved
	fetched, err := ls.GetManifest(ctx, "model_weights_v1", "1.0.0")
	if err != nil {
		t.Fatalf("GetManifest failed: %v", err)
	}
	if fetched.SHA256 != "sha256_orig_111" {
		t.Errorf("manifest was corrupted/shadowed: got SHA %s", fetched.SHA256)
	}
}

func TestCredentialService_StorageTypeTruthfulAndNoSecretEcho(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	cs := governance.NewCredentialService(db)
	ctx := context.Background()

	// 1. Unsupported storage type is rejected at registration (fails closed)
	unsupportedRef := domain.CredentialRef{
		ID:          uuid.NewString(),
		Name:        "unsupported_ref",
		ProviderID:  "prov_x",
		StorageType: "unsupported_vault_storage",
		KeyRef:      "my_secret_key_ref_999",
	}
	err = cs.RegisterCredentialRef(ctx, unsupportedRef)
	if err == nil || !errors.Is(err, domain.ErrUnsupportedStorageType) {
		t.Fatalf("expected ErrUnsupportedStorageType at registration, got %v", err)
	}

	// 2. Verify error message does not echo key_ref
	if err != nil && (containsFoldStr(err.Error(), "my_secret_key_ref_999")) {
		t.Errorf("error message leaked key_ref: %v", err)
	}

	// 3. Raw secret rejection does not echo secret
	secretAttempt := "sk-super-secret-key-that-should-never-be-in-logs-12345"
	rawRef := domain.CredentialRef{
		Name:        "leak_test",
		StorageType: "env_ref",
		KeyRef:      secretAttempt,
	}
	regErr := cs.RegisterCredentialRef(ctx, rawRef)
	if regErr == nil {
		t.Fatalf("expected raw secret rejection")
	}
	if containsFoldStr(regErr.Error(), secretAttempt) {
		t.Errorf("error message leaked raw secret: %v", regErr)
	}
}

func TestPolicyService_RejectsInvalidPolicyStates(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	ps := governance.NewPolicyService(db)
	ctx := context.Background()

	// 1. Initial baseline: set ALLOWED
	if err := ps.SetPolicy(ctx, "prov_canonical", domain.PolicyAllowed, "baseline"); err != nil {
		t.Fatalf("SetPolicy ALLOWED failed: %v", err)
	}
	if ps.GetPolicy(ctx, "prov_canonical", "") != domain.PolicyAllowed {
		t.Errorf("expected ALLOWED")
	}

	// 2. Reject non-canonical values and do not mutate in-memory state
	invalidStates := []domain.PolicyState{
		"INVALID_STATE",
		"allowed", // lowercase non-canonical
		"ENABLED",
		"DISABLED",
		"ALLOW",
		"",
	}
	for _, inv := range invalidStates {
		err := ps.SetPolicy(ctx, "prov_canonical", inv, "invalid attempt")
		if err == nil || !errors.Is(err, domain.ErrInvalidPolicyState) {
			t.Errorf("expected ErrInvalidPolicyState for %q, got %v", inv, err)
		}
		// In-memory state must remain ALLOWED
		if current := ps.GetPolicy(ctx, "prov_canonical", ""); current != domain.PolicyAllowed {
			t.Errorf("in-memory state corrupted by invalid attempt %q: got %s", inv, current)
		}
	}

	// 3. All four canonical states succeed
	canonicalStates := []domain.PolicyState{
		domain.PolicyAllowed,
		domain.PolicyRequiresExplicitConsent,
		domain.PolicyRequiresAuthorization,
		domain.PolicyBlocked,
	}
	for _, state := range canonicalStates {
		if err := ps.SetPolicy(ctx, "prov_canonical", state, "valid transition"); err != nil {
			t.Errorf("SetPolicy for canonical state %s failed: %v", state, err)
		}
		if current := ps.GetPolicy(ctx, "prov_canonical", ""); current != state {
			t.Errorf("expected state %s, got %s", state, current)
		}
	}
}

func TestPolicyService_InMemoryUntouchedOnDBSaveError(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ps := governance.NewPolicyService(db)
	ctx := context.Background()

	// Set initial valid policy
	if err := ps.SetPolicy(ctx, "prov_fail_test", domain.PolicyAllowed, "init"); err != nil {
		t.Fatalf("SetPolicy init failed: %v", err)
	}

	// Close database to force persistence failure on subsequent SetPolicy calls
	_ = db.Close()

	// Attempt transition to BLOCKED while DB is closed
	err = ps.SetPolicy(ctx, "prov_fail_test", domain.PolicyBlocked, "should fail db persist")
	if err == nil {
		t.Fatalf("expected error when setting policy on closed DB, got nil")
	}

	// In-memory state must NOT be mutated to BLOCKED (must remain ALLOWED)
	if current := ps.GetPolicy(ctx, "prov_fail_test", ""); current != domain.PolicyAllowed {
		t.Errorf("in-memory policy was mutated despite DB persistence failure: got %s, expected %s", current, domain.PolicyAllowed)
	}
}

func TestCredentialService_ValidatesStorageTypeAtRegistration(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	cs := governance.NewCredentialService(db)
	ctx := context.Background()

	// 1. Supported storage types succeed
	validTypes := []string{"env_ref", "os_credential_store", "ENV_REF", " OS_Credential_Store "}
	for i, st := range validTypes {
		ref := domain.CredentialRef{
			ID:          uuid.NewString(),
			Name:        fmt.Sprintf("valid_ref_%d", i),
			StorageType: st,
			KeyRef:      fmt.Sprintf("valid_key_ref_%d", i),
		}
		if err := cs.RegisterCredentialRef(ctx, ref); err != nil {
			t.Errorf("expected success for supported storage type %q, got: %v", st, err)
		}
	}

	// 2. Unsupported storage types fail registration and do not update in-memory state
	invalidTypes := []string{"aws_secrets_manager", "vault", "plaintext", "file_store"}
	for _, it := range invalidTypes {
		ref := domain.CredentialRef{
			ID:          uuid.NewString(),
			Name:        "invalid_ref_" + it,
			StorageType: it,
			KeyRef:      "key_ref_dummy",
		}
		err := cs.RegisterCredentialRef(ctx, ref)
		if err == nil || !errors.Is(err, domain.ErrUnsupportedStorageType) {
			t.Errorf("expected ErrUnsupportedStorageType for %q, got: %v", it, err)
		}
		// Must not exist in memory
		if _, getErr := cs.GetCredentialRef(ctx, ref.ID); getErr == nil {
			t.Errorf("unsupported credential was stored in memory despite validation failure")
		}
	}
}

func TestCredentialService_InMemoryUntouchedOnDBSaveError(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	cs := governance.NewCredentialService(db)
	ctx := context.Background()

	// Close database to force persistence failure
	_ = db.Close()

	ref := domain.CredentialRef{
		ID:          uuid.NewString(),
		Name:        "fail_persist_ref",
		StorageType: "env_ref",
		KeyRef:      "my_env_var_key",
	}

	err = cs.RegisterCredentialRef(ctx, ref)
	if err == nil {
		t.Fatalf("expected persistence failure on closed DB, got nil")
	}

	// Must not be present in in-memory state
	if _, getErr := cs.GetCredentialRef(ctx, ref.ID); getErr == nil {
		t.Errorf("credential reference was added to in-memory state despite SQLite persistence error")
	}
}

func containsFoldStr(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
