package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
)

func TestStorage_SQLiteFlow(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_douyinie.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 1. Create RightsAttestation
	attestationID := uuid.NewString()
	ra := domain.RightsAttestation{
		ID:              attestationID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "operator-tester",
		TermsAccepted:   true,
		Notes:           "Test rights attestation",
		ConfirmedAt:     time.Now().UTC(),
	}
	if err := db.CreateRightsAttestation(ctx, ra); err != nil {
		t.Fatalf("CreateRightsAttestation failed: %v", err)
	}

	fetchedRA, err := db.GetRightsAttestation(ctx, attestationID)
	if err != nil {
		t.Fatalf("GetRightsAttestation failed: %v", err)
	}
	if fetchedRA.ID != ra.ID || fetchedRA.DeclaredBy != ra.DeclaredBy || !fetchedRA.TermsAccepted {
		t.Errorf("attestation mismatch: got %+v", fetchedRA)
	}

	// 2. Create SourceAsset
	assetID := uuid.NewString()
	sa := domain.SourceAsset{
		ID:                  assetID,
		SHA256:              "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "test_video.mp4",
		RightsAttestationID: attestationID,
		CASPath:             "cas/e3/b0/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.CreateSourceAsset(ctx, sa); err != nil {
		t.Fatalf("CreateSourceAsset failed: %v", err)
	}

	fetchedSA, err := db.GetSourceAsset(ctx, assetID)
	if err != nil {
		t.Fatalf("GetSourceAsset failed: %v", err)
	}
	if fetchedSA.SHA256 != sa.SHA256 || fetchedSA.RightsAttestationID != attestationID {
		t.Errorf("source asset mismatch: got %+v", fetchedSA)
	}

	// Test foreign key constraint: attempting to insert an asset without valid attestation should fail
	badAsset := domain.SourceAsset{
		ID:                  uuid.NewString(),
		SHA256:              "1111111111111111111111111111111111111111111111111111111111111111",
		ByteSize:            1024,
		MimeType:            "video/mp4",
		OriginalFilename:    "bad.mp4",
		RightsAttestationID: "non-existent-attestation-id",
		CASPath:             "cas/11/11/1111111111111111111111111111111111111111111111111111111111111111",
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.CreateSourceAsset(ctx, badAsset); err == nil {
		t.Errorf("expected error due to foreign key violation on non-existent attestation ID, got nil")
	}

	// 3. Save PreflightReport
	reportID := uuid.NewString()
	pr := domain.PreflightReport{
		ID:               reportID,
		AssetID:          assetID,
		DurationSec:      15.5,
		DurationMs:       15500,
		VideoCodec:       "h264",
		AudioCodec:       "aac",
		Width:            1080,
		Height:           1920,
		FrameRate:        30.0,
		AudioChannels:    2,
		AudioSampleRate:  44100,
		AudioBitRate:     128000,
		VideoBitRate:     2500000,
		ContainerFormat:  "mov,mp4,m4a,3gp,3g2,mj2",
		ContainerValid:   true,
		FingerprintMatch: true,
		Errors:           nil,
		CreatedAt:        time.Now().UTC(),
	}
	if err := db.SavePreflightReport(ctx, pr); err != nil {
		t.Fatalf("SavePreflightReport failed: %v", err)
	}

	fetchedPR, err := db.GetPreflightReport(ctx, assetID)
	if err != nil {
		t.Fatalf("GetPreflightReport failed: %v", err)
	}
	if fetchedPR.AssetID != assetID || fetchedPR.DurationMs != 15500 || !fetchedPR.ContainerValid {
		t.Errorf("preflight report mismatch: got %+v", fetchedPR)
	}

	// 4. Create LocalizationJob
	jobID := uuid.NewString()
	job := domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: domain.TargetLanguageVI,
		Status:         "pending",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	fetchedJob, err := db.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if fetchedJob.TargetLanguage != domain.TargetLanguageVI || fetchedJob.SourceAssetID != assetID {
		t.Errorf("job mismatch: got %+v", fetchedJob)
	}

	// 5. Create LocalizationRun
	runID := uuid.NewString()
	run := domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "queued",
		ConfigSnapshotJSON: `{"preset":"vietnam_standard","model":"vienew"}`,
		CreatedAt:          time.Now().UTC(),
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	fetchedRun, err := db.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if fetchedRun.JobID != jobID || fetchedRun.Status != "queued" {
		t.Errorf("run mismatch: got %+v", fetchedRun)
	}

	// 6. License Manifests
	lm := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "qwen3_asr_1.7b",
		SHA256:         "sha256_mock_qwen3_asr_123",
		SourceRepo:     "Qwen/Qwen3-ASR",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Self-Hosted",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.SaveLicenseManifest(ctx, lm); err != nil {
		t.Fatalf("SaveLicenseManifest failed: %v", err)
	}
	fetchedLM, err := db.GetLicenseManifest(ctx, lm.DependencyName, "")
	if err != nil {
		t.Fatalf("GetLicenseManifest failed: %v", err)
	}
	if fetchedLM.SHA256 != lm.SHA256 || !fetchedLM.Verified {
		t.Errorf("license manifest mismatch: got %+v", fetchedLM)
	}

	// Save version 2 of the same dependency without overwriting version 1 (immutable versioning)
	lmV2 := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "qwen3_asr_1.7b",
		Version:        "2.0.0",
		SHA256:         "sha256_mock_qwen3_asr_v2_456",
		SourceRepo:     "Qwen/Qwen3-ASR",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Self-Hosted",
		Verified:       true,
		CreatedAt:      time.Now().UTC().Add(time.Second),
	}
	if err := db.SaveLicenseManifest(ctx, lmV2); err != nil {
		t.Fatalf("SaveLicenseManifest V2 failed: %v", err)
	}
	allVersions, err := db.ListLicenseManifests(ctx, "qwen3_asr_1.7b")
	if err != nil || len(allVersions) != 2 {
		t.Fatalf("expected 2 versioned manifest entries for qwen3_asr_1.7b, got %d (err: %v)", len(allVersions), err)
	}

	// 7. Credential References
	cr := domain.CredentialRef{
		ID:          uuid.NewString(),
		Name:        "douyin_session_token",
		StorageType: "os_credential_store",
		KeyRef:      "douyin_cookie_ref_01",
		CreatedAt:   time.Now().UTC(),
	}
	if err := db.SaveCredentialRef(ctx, cr); err != nil {
		t.Fatalf("SaveCredentialRef failed: %v", err)
	}
	fetchedCR, err := db.GetCredentialRef(ctx, cr.ID)
	if err != nil {
		t.Fatalf("GetCredentialRef failed: %v", err)
	}
	if fetchedCR.KeyRef != cr.KeyRef {
		t.Errorf("credential ref mismatch: got %+v", fetchedCR)
	}

	// 8. Provider Policies
	if err := db.SetProviderPolicy(ctx, "fake_vieneu_tts_vi", domain.PolicyAllowed, "baseline approved"); err != nil {
		t.Fatalf("SetProviderPolicy failed: %v", err)
	}
	policy, err := db.GetProviderPolicy(ctx, "fake_vieneu_tts_vi")
	if err != nil {
		t.Fatalf("GetProviderPolicy failed: %v", err)
	}
	if policy != domain.PolicyAllowed {
		t.Errorf("expected ALLOWED policy, got %s", policy)
	}

	// 9. Provider Attempts & Selection Decisions
	pa := domain.ProviderAttempt{
		ID:            uuid.NewString(),
		RunID:         runID,
		Stage:         "tts",
		ProviderID:    "fake_vieneu_tts_vi",
		ModelName:     "vieneu-v1",
		ModelVersion:  "1.0.0",
		InputHash:     "hash123",
		AttemptNumber: 1,
		Status:        "succeeded",
		LatencyMs:     120,
		CostUnits:     0.0,
		CreatedAt:     time.Now().UTC(),
	}
	if err := db.RecordProviderAttempt(ctx, pa); err != nil {
		t.Fatalf("RecordProviderAttempt failed: %v", err)
	}
	attempts, err := db.ListProviderAttempts(ctx, runID, "tts")
	if err != nil || len(attempts) != 1 {
		t.Fatalf("ListProviderAttempts failed: %v, got %d", err, len(attempts))
	}

	sd := domain.SelectionDecision{
		ID:                 uuid.NewString(),
		RunID:              runID,
		Stage:              "tts",
		SelectedProviderID: "fake_vieneu_tts_vi",
		CandidatesEvaluated: []domain.CandidateEvaluation{
			{ProviderID: "fake_vieneu_tts_vi", Eligible: true, PolicyState: "ALLOWED", HealthOK: true, CircuitClosed: true, Score: 0.95},
		},
		PolicyCheckResult: "ALLOWED",
		DecisionReason:    "highest scoring eligible candidate",
		CreatedAt:         time.Now().UTC(),
	}
	if err := db.RecordSelectionDecision(ctx, sd); err != nil {
		t.Fatalf("RecordSelectionDecision failed: %v", err)
	}
	decisions, err := db.ListSelectionDecisions(ctx, runID, "tts")
	if err != nil || len(decisions) != 1 {
		t.Fatalf("ListSelectionDecisions failed: %v, got %d", err, len(decisions))
	}
}

func TestStorage_MigrationV2ToV3_ForwardSafeAndPreservesData(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "migration_test.db")

	// 1. Manually initialize a database at migration version 2 (with old single-version UNIQUE dependency_name schema)
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}

	initSQL := `
	CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	);
	INSERT INTO schema_migrations (version, applied_at) VALUES (1, datetime('now'));
	INSERT INTO schema_migrations (version, applied_at) VALUES (2, datetime('now'));

	CREATE TABLE license_manifests (
		id TEXT PRIMARY KEY,
		dependency_name TEXT NOT NULL UNIQUE,
		sha256 TEXT NOT NULL,
		source_repo TEXT NOT NULL,
		code_license TEXT NOT NULL,
		model_license TEXT NOT NULL,
		data_license TEXT NOT NULL,
		service_terms TEXT NOT NULL,
		verified INTEGER NOT NULL,
		created_at TEXT NOT NULL
	);

	INSERT INTO license_manifests (id, dependency_name, sha256, source_repo, code_license, model_license, data_license, service_terms, verified, created_at)
	VALUES ('legacy_id_001', 'qwen_legacy_asr', 'sha256_legacy_111', 'Qwen/ASR', 'Apache-2.0', 'Apache-2.0', 'OpenData', 'Self-Hosted', 1, '2026-01-01T00:00:00Z');
	`
	if _, err := rawDB.Exec(initSQL); err != nil {
		rawDB.Close()
		t.Fatalf("init legacy v2 db failed: %v", err)
	}
	rawDB.Close()

	// 2. Open via storage.Open which executes migrate() forward to v3
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open failed on legacy v2 db: %v", err)
	}
	defer db.Close()

	// 3. Verify schema migration version is now 3
	var v3Count int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 3`).Scan(&v3Count); err != nil || v3Count != 1 {
		t.Fatalf("expected migration version 3 recorded, got count=%d, err=%v", v3Count, err)
	}

	// 4. Verify existing record was preserved with default version 'v1'
	legacyEntry, err := db.GetLicenseManifest(ctx, "qwen_legacy_asr", "")
	if err != nil {
		t.Fatalf("GetLicenseManifest for legacy record failed: %v", err)
	}
	if legacyEntry.ID != "legacy_id_001" || legacyEntry.Version != "v1" || legacyEntry.SHA256 != "sha256_legacy_111" {
		t.Errorf("legacy record data corrupted during migration: %+v", legacyEntry)
	}

	// 5. Verify forward-safe versioning: can now register version 2.0.0 for the same dependency without unique violation
	v2Entry := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "qwen_legacy_asr",
		Version:        "2.0.0",
		SHA256:         "sha256_legacy_v2_222",
		SourceRepo:     "Qwen/ASR",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Self-Hosted",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.SaveLicenseManifest(ctx, v2Entry); err != nil {
		t.Fatalf("SaveLicenseManifest for version 2.0.0 failed after migration: %v", err)
	}

	// 6. Verify listing manifests returns both versions in immutable history
	allVersions, err := db.ListLicenseManifests(ctx, "qwen_legacy_asr")
	if err != nil || len(allVersions) != 2 {
		t.Fatalf("expected 2 versions after adding v2, got %d (err: %v)", len(allVersions), err)
	}

	// 7. Verify duplicate of exact same (dependency_name, version) is still rejected (immutability preserved)
	dupEntry := v2Entry
	dupEntry.ID = uuid.NewString()
	dupEntry.SHA256 = "sha256_corrupted"
	if err := db.SaveLicenseManifest(ctx, dupEntry); err == nil {
		t.Fatalf("expected error when inserting duplicate (dependency_name, version), got nil")
	}
}
