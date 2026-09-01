package service_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

func setupBundleTestEnv(t *testing.T) (*storage.DB, *cas.Store, *governance.LicenseService, *service.BundleService) {
	t.Helper()
	dir := t.TempDir()

	dbPath := filepath.Join(dir, "test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	casStore, err := cas.NewStore(dir)
	if err != nil {
		t.Fatalf("open cas: %v", err)
	}

	licSvc := governance.NewLicenseService(db)
	bundleSvc := service.NewBundleService(db, casStore, licSvc)

	return db, casStore, licSvc, bundleSvc
}

func seedTestJob(t *testing.T, db *storage.DB, casStore *cas.Store, licSvc *governance.LicenseService) (string, string) {
	t.Helper()
	ctx := context.Background()

	// 1. Ingest media into CAS
	mediaContent := []byte("FAKE_MEDIA_CONTENT_FOR_TESTING_12345")
	mediaObj, err := casStore.Put(bytes.NewReader(mediaContent))
	if err != nil {
		t.Fatalf("put media into cas: %v", err)
	}

	// 2. Register License Manifest with all 4 obligation layers
	licEntry := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "qwen3-asr",
		Version:        "1.7b",
		SHA256:         "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		SourceRepo:     "https://github.com/QwenLM/Qwen3-ASR",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Qwen-Research",
		DataLicense:    "Common-Voice",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}
	if err := licSvc.RegisterManifest(ctx, licEntry); err != nil {
		t.Fatalf("register license manifest: %v", err)
	}

	// 3. Create RightsAttestation
	attID := uuid.NewString()
	att := domain.RightsAttestation{
		ID:              attID,
		AttestationType: "OWNER_DIRECT",
		DeclaredBy:      "test-operator",
		TermsAccepted:   true,
		Notes:           "Attestation notes",
		ConfirmedAt:     time.Now().UTC(),
	}
	if err := db.CreateRightsAttestation(ctx, att); err != nil {
		t.Fatalf("create attestation: %v", err)
	}

	// 4. Create SourceAsset
	assetID := uuid.NewString()
	asset := domain.SourceAsset{
		ID:                  assetID,
		SHA256:              mediaObj.SHA256,
		ByteSize:            mediaObj.ByteSize,
		MimeType:            "video/mp4",
		OriginalFilename:    `C:\test\local\video.mp4`,
		RightsAttestationID: attID,
		CASPath:             mediaObj.Path,
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.CreateSourceAsset(ctx, asset); err != nil {
		t.Fatalf("create source asset: %v", err)
	}

	// 5. Create PreflightReport
	preflight := domain.PreflightReport{
		ID:              uuid.NewString(),
		AssetID:         assetID,
		DurationSec:     10.5,
		DurationMs:      10500,
		ContainerFormat: "mp4",
		ContainerValid:  true,
		CreatedAt:       time.Now().UTC(),
	}
	if err := db.SavePreflightReport(ctx, preflight); err != nil {
		t.Fatalf("save preflight report: %v", err)
	}

	// 6. Create Job
	jobID := uuid.NewString()
	job := domain.LocalizationJob{
		ID:             jobID,
		SourceAssetID:  assetID,
		TargetLanguage: "vi",
		Status:         "completed",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	if err := db.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	// 7. Create Run
	runID := uuid.NewString()
	now := time.Now().UTC()
	run := domain.LocalizationRun{
		ID:                 runID,
		JobID:              jobID,
		Status:             "completed",
		ConfigSnapshotJSON: `{"profile": "hybrid", "model": "qwen3-asr"}`,
		CreatedAt:          now,
		CompletedAt:        &now,
	}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// 8. Create StageExecution
	se := domain.StageExecution{
		ID:          uuid.NewString(),
		RunID:       runID,
		Stage:       "asr",
		Status:      "succeeded",
		StartedAt:   &now,
		CompletedAt: &now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := db.CreateStageExecution(ctx, se); err != nil {
		t.Fatalf("create stage execution: %v", err)
	}

	// 9. Create ProviderAttempt
	attmpt := domain.ProviderAttempt{
		ID:            uuid.NewString(),
		RunID:         runID,
		Stage:         "asr",
		ProviderID:    "fake_qwen3_asr",
		ModelName:     "qwen3-asr",
		ModelVersion:  "1.7b",
		InputHash:     mediaObj.SHA256,
		AttemptNumber: 1,
		Status:        "succeeded",
		LatencyMs:     120,
		CostUnits:     0.0,
		CreatedAt:     now,
	}
	if err := db.RecordProviderAttempt(ctx, attmpt); err != nil {
		t.Fatalf("record attempt: %v", err)
	}

	// 10. Create SelectionDecision
	dec := domain.SelectionDecision{
		ID:                 uuid.NewString(),
		RunID:              runID,
		Stage:              "asr",
		SelectedProviderID: "fake_qwen3_asr",
		CandidatesEvaluated: []domain.CandidateEvaluation{
			{ProviderID: "fake_qwen3_asr", Eligible: true, PolicyState: "ALLOWED", HealthOK: true, CircuitClosed: true, Score: 0.95},
		},
		PolicyCheckResult: "ALLOWED",
		DecisionReason:    "optimal score",
		CreatedAt:         now,
	}
	if err := db.RecordSelectionDecision(ctx, dec); err != nil {
		t.Fatalf("record decision: %v", err)
	}

	return jobID, assetID
}

func TestBundleService_ExportImport_RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, assetID := seedTestJob(t, dbA, casA, licA)

	// 1. Export bundle to buffer
	var zipBuf bytes.Buffer
	manifest, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err != nil {
		t.Fatalf("export bundle failed: %v", err)
	}

	if manifest == nil {
		t.Fatalf("expected non-nil manifest")
	}

	// Invariant: no machine-local absolute paths in manifest
	if strings.Contains(manifest.SourceAsset.CASPath, `F:\CodeBase`) || strings.Contains(manifest.SourceAsset.CASPath, `C:\`) {
		t.Errorf("manifest contains machine-local path: %s", manifest.SourceAsset.CASPath)
	}
	if manifest.SourceAsset.OriginalFilename != "video.mp4" {
		t.Errorf("expected video.mp4, got %q", manifest.SourceAsset.OriginalFilename)
	}

	// Invariant: license metadata present with all 4 obligation layers
	if len(manifest.LicenseManifests) == 0 {
		t.Fatalf("expected license manifests in bundle, got none")
	}
	lm := manifest.LicenseManifests[0]
	if lm.CodeLicense == "" || lm.ModelLicense == "" || lm.DataLicense == "" || lm.ServiceTerms == "" {
		t.Errorf("license manifest missing obligation layers: %+v", lm)
	}

	// 2. Set up clean destination environment B
	dbB, casB, licB, bundleSvcB := setupBundleTestEnv(t)
	defer dbB.Close()

	zipBytes := zipBuf.Bytes()
	importedManifest, err := bundleSvcB.ImportBundle(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), service.ImportOptions{})
	if err != nil {
		t.Fatalf("import bundle failed: %v", err)
	}

	if importedManifest.Job.ID != jobID {
		t.Errorf("expected job ID %s, got %s", jobID, importedManifest.Job.ID)
	}

	// 3. Verify reproduction on destination B
	reproducedJob, err := dbB.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("lookup reproduced job: %v", err)
	}
	if reproducedJob.TargetLanguage != "vi" || reproducedJob.Status != "completed" {
		t.Errorf("unexpected reproduced job: %+v", reproducedJob)
	}

	reproducedAsset, err := dbB.GetSourceAsset(ctx, assetID)
	if err != nil {
		t.Fatalf("lookup reproduced asset: %v", err)
	}
	if reproducedAsset.SHA256 != manifest.SourceAsset.SHA256 {
		t.Errorf("reproduced asset SHA mismatch: %s != %s", reproducedAsset.SHA256, manifest.SourceAsset.SHA256)
	}

	// CAS file must exist on destination B and match content
	if !casB.Exists(reproducedAsset.SHA256) {
		t.Fatalf("media file does not exist in destination CAS store!")
	}
	rc, err := casB.Get(reproducedAsset.SHA256)
	if err != nil {
		t.Fatalf("get media from destination CAS: %v", err)
	}
	content, _ := io.ReadAll(rc)
	rc.Close()
	if string(content) != "FAKE_MEDIA_CONTENT_FOR_TESTING_12345" {
		t.Errorf("unexpected content in destination CAS: %s", string(content))
	}

	// License manifest must exist on destination B
	licBEntry, err := licB.GetManifest(ctx, "qwen3-asr", "1.7b")
	if err != nil {
		t.Fatalf("lookup reproduced license manifest: %v", err)
	}
	if licBEntry.CodeLicense != "Apache-2.0" || licBEntry.ServiceTerms != "Local-Offline" {
		t.Errorf("unexpected reproduced license entry: %+v", licBEntry)
	}
}

func TestBundleService_TamperRejection(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, _ := seedTestJob(t, dbA, casA, licA)

	var zipBuf bytes.Buffer
	_, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err != nil {
		t.Fatalf("export bundle failed: %v", err)
	}

	// Tamper with the zip: mutate 1 byte in an artifact file
	origZipBytes := zipBuf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(origZipBytes), int64(len(origZipBytes)))
	if err != nil {
		t.Fatalf("open zip reader: %v", err)
	}

	var tamperedZip bytes.Buffer
	zw := zip.NewWriter(&tamperedZip)

	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open file in zip: %v", err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()

		if strings.HasPrefix(f.Name, "artifacts/") {
			// Tamper! Flip the last byte
			if len(data) > 0 {
				data[len(data)-1] ^= 0xFF
			}
		}

		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatalf("create in tampered zip: %v", err)
		}
		_, _ = w.Write(data)
	}
	zw.Close()

	// Attempt to import tampered zip into clean DB
	dbB, _, _, bundleSvcB := setupBundleTestEnv(t)
	defer dbB.Close()

	tamperedBytes := tamperedZip.Bytes()
	_, err = bundleSvcB.ImportBundle(ctx, bytes.NewReader(tamperedBytes), int64(len(tamperedBytes)), service.ImportOptions{})
	if err == nil {
		t.Fatalf("expected tamper rejection error, but import succeeded!")
	}
	if !errors.Is(err, domain.ErrJobBundleTampered) {
		t.Errorf("expected ErrJobBundleTampered, got: %v", err)
	}
}

func TestBundleService_SecretInManifest_Rejected(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, _ := seedTestJob(t, dbA, casA, licA)

	var zipBuf bytes.Buffer
	_, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err != nil {
		t.Fatalf("export bundle failed: %v", err)
	}

	// Inject a raw secret into manifest.json inside the zip
	origZipBytes := zipBuf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(origZipBytes), int64(len(origZipBytes)))
	if err != nil {
		t.Fatalf("open zip reader: %v", err)
	}

	var tamperedZip bytes.Buffer
	zw := zip.NewWriter(&tamperedZip)

	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open file in zip: %v", err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()

		if f.Name == "manifest.json" {
			// Inject API key
			data = bytes.Replace(data, []byte(`"status": "completed"`), []byte(`"status": "completed", "api_key": "sk-1234567890abcdef1234567890abcdef"`), 1)
		}

		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatalf("create in tampered zip: %v", err)
		}
		_, _ = w.Write(data)
	}
	zw.Close()

	dbB, _, _, bundleSvcB := setupBundleTestEnv(t)
	defer dbB.Close()

	tamperedBytes := tamperedZip.Bytes()
	_, err = bundleSvcB.ImportBundle(ctx, bytes.NewReader(tamperedBytes), int64(len(tamperedBytes)), service.ImportOptions{})
	if err == nil {
		t.Fatalf("expected secret rejection error, but import succeeded!")
	}
	if !errors.Is(err, domain.ErrJobBundleSecretDetected) {
		t.Errorf("expected ErrJobBundleSecretDetected, got: %v", err)
	}
}

func TestBundleService_MachineLocalPath_Rejected(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, _ := seedTestJob(t, dbA, casA, licA)

	var zipBuf bytes.Buffer
	_, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err != nil {
		t.Fatalf("export bundle failed: %v", err)
	}

	// Inject a machine-local absolute path into manifest.json
	origZipBytes := zipBuf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(origZipBytes), int64(len(origZipBytes)))
	if err != nil {
		t.Fatalf("open zip reader: %v", err)
	}

	var tamperedZip bytes.Buffer
	zw := zip.NewWriter(&tamperedZip)

	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open file in zip: %v", err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()

		if f.Name == "manifest.json" {
			// Inject machine-local absolute path
			data = bytes.Replace(data, []byte(`"status": "completed"`), []byte(`"status": "completed", "local_path": "C:\\Windows\\System32\\bad.dll"`), 1)
		}

		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatalf("create in tampered zip: %v", err)
		}
		_, _ = w.Write(data)
	}
	zw.Close()

	dbB, _, _, bundleSvcB := setupBundleTestEnv(t)
	defer dbB.Close()

	tamperedBytes := tamperedZip.Bytes()
	_, err = bundleSvcB.ImportBundle(ctx, bytes.NewReader(tamperedBytes), int64(len(tamperedBytes)), service.ImportOptions{})
	if err == nil {
		t.Fatalf("expected machine local path rejection error, but import succeeded!")
	}
	if !errors.Is(err, domain.ErrJobBundleMachineLocalPath) {
		t.Errorf("expected ErrJobBundleMachineLocalPath, got: %v", err)
	}
}

func TestBundleService_FileHelpers(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, _ := seedTestJob(t, dbA, casA, licA)

	tmpDir := t.TempDir()
	exportPath := filepath.Join(tmpDir, "exported_bundle.zip")

	manifest, err := bundleSvcA.ExportBundleToFile(ctx, jobID, exportPath)
	if err != nil {
		t.Fatalf("export to file failed: %v", err)
	}
	if manifest == nil {
		t.Fatalf("manifest is nil")
	}

	info, err := os.Stat(exportPath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("export file is missing or empty: %v", err)
	}

	dbB, _, _, bundleSvcB := setupBundleTestEnv(t)
	defer dbB.Close()

	imported, err := bundleSvcB.ImportBundleFromFile(ctx, exportPath, service.ImportOptions{})
	if err != nil {
		t.Fatalf("import from file failed: %v", err)
	}
	if imported.Job.ID != jobID {
		t.Errorf("expected job ID %s, got %s", jobID, imported.Job.ID)
	}
}

func TestBundleService_OverwriteHandling(t *testing.T) {
	ctx := context.Background()
	dbA, casA, licA, bundleSvcA := setupBundleTestEnv(t)
	defer dbA.Close()

	jobID, _ := seedTestJob(t, dbA, casA, licA)

	var zipBuf bytes.Buffer
	_, err := bundleSvcA.ExportBundle(ctx, jobID, &zipBuf)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	zipBytes := zipBuf.Bytes()

	// 1. First import into destination dbB succeeds
	dbB, casB, licB, bundleSvcB := setupBundleTestEnv(t)
	defer dbB.Close()
	_ = casB
	_ = licB

	_, err = bundleSvcB.ImportBundle(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), service.ImportOptions{Overwrite: false})
	if err != nil {
		t.Fatalf("initial import failed: %v", err)
	}

	// 2. Second import with Overwrite: false MUST fail with ErrJobAlreadyExists
	_, err = bundleSvcB.ImportBundle(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), service.ImportOptions{Overwrite: false})
	if !errors.Is(err, domain.ErrJobAlreadyExists) {
		t.Fatalf("expected ErrJobAlreadyExists when overwrite is false, got %v", err)
	}

	// 3. Third import with Overwrite: true MUST succeed
	_, err = bundleSvcB.ImportBundle(ctx, bytes.NewReader(zipBytes), int64(len(zipBytes)), service.ImportOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("import with overwrite: true failed: %v", err)
	}
}
