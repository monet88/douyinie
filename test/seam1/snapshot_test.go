package seam1_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/storage"
)

func createSnapshotDirWithFiles(t *testing.T, files map[string]string) (string, domain.SnapshotManifest) {
	t.Helper()
	dir := t.TempDir()
	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "test-asr-snapshot",
		ModelVersion:  "1.0.0",
	}

	for relPath, content := range files {
		fullPath := filepath.Join(dir, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", relPath, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}
		hash := sha256.Sum256([]byte(content))
		manifest.Files = append(manifest.Files, domain.SnapshotFileEntry{
			RelativePath: relPath,
			SHA256:       hex.EncodeToString(hash[:]),
			SizeBytes:    int64(len(content)),
		})
	}

	computedSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		t.Fatalf("compute manifest sha: %v", err)
	}
	manifest.SnapshotManifestSHA256 = computedSHA

	return dir, manifest
}

func TestSeam1_Snapshot_VerifyAndList(t *testing.T) {
	h := setupHarness(t)

	dir, manifest := createSnapshotDirWithFiles(t, map[string]string{
		"weights.bin": "binary model weights data",
		"config.json": `{"model_name": "test-asr-snapshot"}`,
		"vocab.txt":   "tokenA\ntokenB\ntokenC",
	})

	verifyPayload := map[string]any{
		"manifest":   manifest,
		"local_path": dir,
	}
	body, _ := json.Marshal(verifyPayload)

	// 1. Missing license manifest fails closed (HTTP 422)
	resp, err := http.Post(h.server.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/snapshots/verify failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422 when license manifest missing, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// 2. Register required 4-layer license manifest
	licPayload := domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: manifest.ModelID,
		Version:        manifest.ModelVersion,
		SHA256:         manifest.SnapshotManifestSHA256,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Community-License",
		DataLicense:    "Open-Audio-Corpus",
		ServiceTerms:   "Self-Hosted",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}
	licBody, _ := json.Marshal(licPayload)
	licResp, err := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(licBody))
	if err != nil {
		t.Fatalf("POST /api/v1/licenses failed: %v", err)
	}
	licResp.Body.Close()
	if licResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for license register, got %d", licResp.StatusCode)
	}

	// 3. Digest mismatch fails closed (HTTP 422)
	badManifest := manifest
	badManifest.SnapshotManifestSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	badPayload, _ := json.Marshal(map[string]any{
		"manifest":   badManifest,
		"local_path": dir,
	})
	badResp, err := http.Post(h.server.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(badPayload))
	if err != nil {
		t.Fatalf("POST /api/v1/snapshots/verify bad digest failed: %v", err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for digest mismatch, got %d", badResp.StatusCode)
	}

	// 4. Missing/corrupted file fails closed (HTTP 422)
	corruptManifest := manifest
	corruptManifest.Files = append(corruptManifest.Files, domain.SnapshotFileEntry{
		RelativePath: "nonexistent.bin",
		SHA256:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes:    12345,
	})
	corruptManifest.SnapshotManifestSHA256, _ = domain.ComputeSnapshotManifestSHA256(&corruptManifest)
	corruptPayload, _ := json.Marshal(map[string]any{
		"manifest":   corruptManifest,
		"local_path": dir,
	})
	corruptResp, err := http.Post(h.server.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(corruptPayload))
	if err != nil {
		t.Fatalf("POST /api/v1/snapshots/verify corrupt failed: %v", err)
	}
	defer corruptResp.Body.Close()
	if corruptResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for corrupted snapshot, got %d", corruptResp.StatusCode)
	}

	// 5. Successful verification creates in-process binding (HTTP 201 Created)
	goodResp, err := http.Post(h.server.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/snapshots/verify success failed: %v", err)
	}
	defer goodResp.Body.Close()
	if goodResp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(goodResp.Body)
		t.Fatalf("expected 201 Created, got %d: %s", goodResp.StatusCode, string(bodyBytes))
	}

	// 6. List snapshots over GET /api/v1/snapshots
	listResp, err := http.Get(h.server.URL + "/api/v1/snapshots")
	if err != nil {
		t.Fatalf("GET /api/v1/snapshots failed: %v", err)
	}
	defer listResp.Body.Close()
	var listResult struct {
		Bindings []struct {
			DependencyName         string `json:"dependency_name"`
			Version                string `json:"version"`
			SnapshotManifestSHA256 string `json:"snapshot_manifest_sha256"`
			Active                 bool   `json:"active"`
		} `json:"bindings"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listResult); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResult.Bindings) == 0 {
		t.Fatalf("expected at least 1 snapshot binding, got 0")
	}

	// 7. GET /api/v1/snapshots/{dependency} returns portable evidence without weights or local paths
	evResp, err := http.Get(h.server.URL + "/api/v1/snapshots/" + manifest.ModelID + "?version=" + manifest.ModelVersion)
	if err != nil {
		t.Fatalf("GET /api/v1/snapshots/{dependency} failed: %v", err)
	}
	defer evResp.Body.Close()
	if evResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for snapshot evidence, got %d", evResp.StatusCode)
	}
	var evResult struct {
		Evidence domain.SnapshotPortableEvidence `json:"evidence"`
	}
	if err := json.NewDecoder(evResp.Body).Decode(&evResult); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	if evResult.Evidence.SnapshotManifestSHA256 != manifest.SnapshotManifestSHA256 {
		t.Fatalf("evidence sha mismatch: got %s, want %s", evResult.Evidence.SnapshotManifestSHA256, manifest.SnapshotManifestSHA256)
	}
	for _, f := range evResult.Evidence.Files {
		if filepath.IsAbs(f.RelativePath) || strings.Contains(f.RelativePath, dir) {
			t.Fatalf("portable evidence must not export machine-local paths: %s", f.RelativePath)
		}
	}

	// 8. GET /api/v1/snapshots/events lists historical audit events
	auditResp, err := http.Get(h.server.URL + "/api/v1/snapshots/events?dependency_name=" + manifest.ModelID)
	if err != nil {
		t.Fatalf("GET /api/v1/snapshots/events failed: %v", err)
	}
	defer auditResp.Body.Close()
	var auditResult struct {
		Events []domain.SnapshotVerificationEvent `json:"events"`
	}
	if err := json.NewDecoder(auditResp.Body).Decode(&auditResult); err != nil {
		t.Fatalf("decode audit events: %v", err)
	}
	if len(auditResult.Events) == 0 {
		t.Fatalf("expected historical verification audit events recorded")
	}
}

func TestSeam1_Snapshot_RouterFailClosedAndRehash(t *testing.T) {
	h := setupHarness(t)
	ctx := context.Background()

	// Register a fake provider that requires snapshot verification
	testProvider := provider.NewFakeASRProvider("fake_asr_snapshot_required")
	testProvider.ModelName = "qwen3-asr-snap-test"
	testProvider.ModelVersion = "1.7b"
	testProvider.SetRequiresSnapshot(true)
	_ = h.registry.Register(testProvider)

	// 1. Create real snapshot files and compute manifest SHA
	dir := t.TempDir()
	content := "real model weights binary"
	fullPath := filepath.Join(dir, "model.bin")
	_ = os.WriteFile(fullPath, []byte(content), 0644)
	hBytes := sha256.Sum256([]byte(content))
	fileSHA := hex.EncodeToString(hBytes[:])

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "qwen3-asr-snap-test",
		ModelVersion:  "1.7b",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: fileSHA, SizeBytes: int64(len(content))},
		},
	}
	realManifestSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	manifest.SnapshotManifestSHA256 = realManifestSHA

	// Register 4-layer license manifest for this model matching real manifest SHA
	licPayload, _ := json.Marshal(domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "qwen3-asr-snap-test",
		Version:        "1.7b",
		SHA256:         realManifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Research-Only",
		DataLicense:    "InternalData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	resp, _ := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(licPayload))
	resp.Body.Close()

	// 2. Before snapshot registration, candidate evaluation fails closed with SNAPSHOT_UNVERIFIED
	decidePayload, _ := json.Marshal(map[string]any{
		"run_id":                uuid.NewString(),
		"stage":                 "asr",
		"language":              "zh",
		"preferred_provider_id": "fake_asr_snapshot_required",
	})
	decideResp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decidePayload))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide failed: %v", err)
	}
	defer decideResp.Body.Close()

	// Since fake_asr_snapshot_required is unverified, routing should not pick it as eligible
	var decideResult struct {
		SelectedProviderID string `json:"selected_provider_id"`
		Decision           struct {
			Evaluations []domain.CandidateEvaluation `json:"evaluations"`
		} `json:"decision"`
	}
	_ = json.NewDecoder(decideResp.Body).Decode(&decideResult)
	for _, eval := range decideResult.Decision.Evaluations {
		if eval.ProviderID == "fake_asr_snapshot_required" {
			if eval.Eligible {
				t.Fatalf("expected unverified provider to be ineligible")
			}
			if eval.RejectionCode != "SNAPSHOT_UNVERIFIED" {
				t.Fatalf("expected rejection code SNAPSHOT_UNVERIFIED, got %s", eval.RejectionCode)
			}
		}
	}

	// 3. Register and verify snapshot via API
	verifyPayload, _ := json.Marshal(map[string]any{
		"manifest":   manifest,
		"local_path": dir,
	})
	vResp, err := http.Post(h.server.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(verifyPayload))
	if err != nil {
		t.Fatalf("POST /api/v1/snapshots/verify failed: %v", err)
	}
	defer vResp.Body.Close()
	if vResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(vResp.Body)
		t.Fatalf("expected 201 Created for snapshot verify, got %d: %s", vResp.StatusCode, string(b))
	}

	// 3. Now candidate evaluation succeeds and provider is eligible
	decideResp2, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decidePayload))
	if err != nil {
		t.Fatalf("POST decide 2 failed: %v", err)
	}
	defer decideResp2.Body.Close()
	var decideResult2 struct {
		SelectedProviderID string `json:"selected_provider_id"`
		Decision           struct {
			Evaluations []domain.CandidateEvaluation `json:"evaluations"`
		} `json:"decision"`
	}
	_ = json.NewDecoder(decideResp2.Body).Decode(&decideResult2)
	for _, eval := range decideResult2.Decision.Evaluations {
		if eval.ProviderID == "fake_asr_snapshot_required" {
			if !eval.Eligible {
				t.Fatalf("expected verified provider to be eligible, got reason: %s (%s)", eval.Reason, eval.RejectionCode)
			}
		}
	}

	// 4. Tamper with file on disk -> candidate evaluation fails closed with SNAPSHOT_MUTATED_REHASH_REQUIRED
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(fullPath, []byte("tampered content on disk!"), 0644)

	decideResp3, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decidePayload))
	if err != nil {
		t.Fatalf("POST decide 3 failed: %v", err)
	}
	defer decideResp3.Body.Close()
	var decideResult3 struct {
		Decision struct {
			Evaluations []domain.CandidateEvaluation `json:"evaluations"`
		} `json:"decision"`
	}
	_ = json.NewDecoder(decideResp3.Body).Decode(&decideResult3)
	for _, eval := range decideResult3.Decision.Evaluations {
		if eval.ProviderID == "fake_asr_snapshot_required" {
			if eval.Eligible {
				t.Fatalf("expected mutated provider to be ineligible")
			}
			if eval.RejectionCode != "SNAPSHOT_MUTATED_REHASH_REQUIRED" {
				t.Fatalf("expected SNAPSHOT_MUTATED_REHASH_REQUIRED, got %s", eval.RejectionCode)
			}
		}
	}

	// 5. Reverify via POST /api/v1/snapshots/{dependency}/reverify on tampered file fails (HTTP 422)
	reverifyResp, err := http.Post(h.server.URL+"/api/v1/snapshots/qwen3-asr-snap-test/reverify?version=1.7b", "application/json", nil)
	if err != nil {
		t.Fatalf("reverify post failed: %v", err)
	}
	defer reverifyResp.Body.Close()
	if reverifyResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on reverify tampered file, got %d", reverifyResp.StatusCode)
	}

	// 6. Restore file and reverify succeeds (HTTP 200)
	_ = os.WriteFile(fullPath, []byte(content), 0644)
	reverifyResp2, err := http.Post(h.server.URL+"/api/v1/snapshots/qwen3-asr-snap-test/reverify?version=1.7b", "application/json", nil)
	if err != nil {
		t.Fatalf("reverify post 2 failed: %v", err)
	}
	defer reverifyResp2.Body.Close()
	if reverifyResp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(reverifyResp2.Body)
		t.Fatalf("expected 200 on reverify restored file, got %d: %s", reverifyResp2.StatusCode, string(b))
	}

	// 7. Provider is eligible again
	_ = ctx
	decideResp4, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decidePayload))
	if err != nil {
		t.Fatalf("POST decide 4 failed: %v", err)
	}
	defer decideResp4.Body.Close()
	var decideResult4 struct {
		Decision struct {
			Evaluations []domain.CandidateEvaluation `json:"evaluations"`
		} `json:"decision"`
	}
	_ = json.NewDecoder(decideResp4.Body).Decode(&decideResult4)
	for _, eval := range decideResult4.Decision.Evaluations {
		if eval.ProviderID == "fake_asr_snapshot_required" {
			if !eval.Eligible {
				t.Fatalf("expected restored provider to be eligible again, got %s", eval.Reason)
			}
		}
	}
}

func TestSeam1_Snapshot_DependencySnapshotValidation(t *testing.T) {
	h := setupHarness(t)

	// Create a diarization provider with primary model and VAD dependency
	diarizer := provider.NewFakeDiarizationProvider("fake_diarizer_with_deps")
	diarizer.ModelName = "campplus-diarizer"
	diarizer.ModelVersion = "v1"
	diarizer.VADModelName = "fsmn-vad"
	diarizer.VADModelVersion = "v2"
	diarizer.SetRequiresSnapshot(true)
	_ = h.registry.Register(diarizer)

	// Verify primary snapshot for campplus-diarizer
	dirPrimary, manifestPrimary := createSnapshotDirWithFiles(t, map[string]string{
		"diarizer_weights.bin": "diarizer weight data",
	})
	manifestPrimary.ModelID = "campplus-diarizer"
	manifestPrimary.ModelVersion = "v1"
	manifestPrimary.SnapshotManifestSHA256, _ = domain.ComputeSnapshotManifestSHA256(&manifestPrimary)

	// Register license for primary
	lic1, _ := json.Marshal(domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "campplus-diarizer",
		Version:        "v1",
		SHA256:         manifestPrimary.SnapshotManifestSHA256,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	resp1, _ := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(lic1))
	resp1.Body.Close()

	// Register primary snapshot
	v1Payload, _ := json.Marshal(map[string]any{
		"manifest":   manifestPrimary,
		"local_path": dirPrimary,
	})
	vResp1, _ := http.Post(h.server.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(v1Payload))
	vResp1.Body.Close()

	// 1. Dependency VAD is unverified -> candidate evaluation fails closed with DEPENDENCY_SNAPSHOT_INVALID
	decidePayload, _ := json.Marshal(map[string]any{
		"run_id":                uuid.NewString(),
		"stage":                 "diarize",
		"language":              "zh",
		"preferred_provider_id": "fake_diarizer_with_deps",
	})
	decideResp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decidePayload))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide failed: %v", err)
	}
	defer decideResp.Body.Close()
	var decideResult struct {
		Decision struct {
			Evaluations []domain.CandidateEvaluation `json:"evaluations"`
		} `json:"decision"`
	}
	_ = json.NewDecoder(decideResp.Body).Decode(&decideResult)
	for _, eval := range decideResult.Decision.Evaluations {
		if eval.ProviderID == "fake_diarizer_with_deps" {
			if eval.Eligible {
				t.Fatalf("expected provider with unverified dependency to be ineligible")
			}
			if eval.RejectionCode != "DEPENDENCY_SNAPSHOT_INVALID" && eval.RejectionCode != "LICENSE_MANIFEST_MISSING" {
				t.Fatalf("expected DEPENDENCY_SNAPSHOT_INVALID, got %s", eval.RejectionCode)
			}
		}
	}

	// 2. Now verify dependency VAD snapshot
	dirVAD, manifestVAD := createSnapshotDirWithFiles(t, map[string]string{
		"vad_model.bin": "fsmn vad weights",
	})
	manifestVAD.ModelID = "fsmn-vad"
	manifestVAD.ModelVersion = "v2"
	manifestVAD.SnapshotManifestSHA256, _ = domain.ComputeSnapshotManifestSHA256(&manifestVAD)

	licVAD, _ := json.Marshal(domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "fsmn-vad",
		Version:        "v2",
		SHA256:         manifestVAD.SnapshotManifestSHA256,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	respVAD, _ := http.Post(h.server.URL+"/api/v1/licenses", "application/json", bytes.NewReader(licVAD))
	respVAD.Body.Close()

	v2Payload, _ := json.Marshal(map[string]any{
		"manifest":   manifestVAD,
		"local_path": dirVAD,
	})
	vResp2, _ := http.Post(h.server.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(v2Payload))
	vResp2.Body.Close()

	// 3. Both primary and dependency verified -> provider is now eligible!
	decideResp2, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decidePayload))
	if err != nil {
		t.Fatalf("POST decide 2 failed: %v", err)
	}
	defer decideResp2.Body.Close()
	var decideResult2 struct {
		Decision struct {
			Evaluations []domain.CandidateEvaluation `json:"evaluations"`
		} `json:"decision"`
	}
	_ = json.NewDecoder(decideResp2.Body).Decode(&decideResult2)
	for _, eval := range decideResult2.Decision.Evaluations {
		if eval.ProviderID == "fake_diarizer_with_deps" {
			if !eval.Eligible {
				t.Fatalf("expected provider with verified dependencies to be eligible, got %s", eval.Reason)
			}
		}
	}
}

// TestSeam1_ProductionRegistry_FailsClosedWhenSnapshotAbsent proves Finding 1:
// In the real production registry (NewProductionSpeechRegistry), model-backed
// candidate providers require verified snapshots by default and fail-closed
// (ineligible with SNAPSHOT_UNVERIFIED) when the required binding is absent,
// even when valid license manifests exist.
func TestSeam1_ProductionRegistry_FailsClosedWhenSnapshotAbsent(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "prod_reg_test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS: %v", err)
	}

	// Real production speech registry with default settings (requireSnapshots = true by default!)
	prodReg, err := provider.NewProductionSpeechRegistry()
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry failed: %v", err)
	}

	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()

	router := provider.NewRouter(prodReg, polSvc, licSvc, credSvc, nil, db)
	router.SetSnapshotService(snapSvc)

	// 1. Prepare valid snapshot files and compute real digest for qwen3-asr 1.7b
	modelDir := t.TempDir()
	modelContent := "production test weights binary"
	_ = os.WriteFile(filepath.Join(modelDir, "model.bin"), []byte(modelContent), 0644)
	hBytes := sha256.Sum256([]byte(modelContent))
	fileSHA := hex.EncodeToString(hBytes[:])

	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       "qwen3-asr",
		ModelVersion:  "1.7b",
		Files: []domain.SnapshotFileEntry{
			{RelativePath: "model.bin", SHA256: fileSHA, SizeBytes: int64(len(modelContent))},
		},
	}
	realManifestSHA, _ := domain.ComputeSnapshotManifestSHA256(&manifest)
	manifest.SnapshotManifestSHA256 = realManifestSHA

	// Register 4-obligation-layer license manifests for Qwen3-ASR models
	initCtx := context.Background()
	_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "qwen3-asr",
		Version:        "1.7b",
		SHA256:         realManifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Research-Only",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "qwen3-asr",
		Version:        "0.6b",
		SHA256:         "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Research-Only",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	srv := server.New(server.Config{
		Addr:        "127.0.0.1:0",
		DB:          db,
		CASStore:    casStore,
		Registry:    prodReg,
		PolicySvc:   polSvc,
		LicenseSvc:  licSvc,
		CredSvc:     credSvc,
		Router:      router,
		SnapshotSvc: snapSvc,
		QueueSvc:    queueSvc,
		Scheduler:   resScheduler,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close() })

	// 2. Call decide endpoint without registered snapshot binding
	runID := uuid.NewString()
	decidePayload, _ := json.Marshal(map[string]any{
		"run_id":   runID,
		"stage":    "asr",
		"language": "zh",
	})
	resp, err := http.Post(ts.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decidePayload))
	if err != nil {
		t.Fatalf("POST decide failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 UnprocessableEntity when all candidates lack snapshots, got %d", resp.StatusCode)
	}

	// Query decisions provenance to verify candidate rejection reasons
	dResp, err := http.Get(ts.URL + "/api/v1/routing/decisions?run_id=" + runID)
	if err != nil {
		t.Fatalf("GET decisions failed: %v", err)
	}
	defer dResp.Body.Close()
	var dResult struct {
		Decisions []domain.SelectionDecision `json:"decisions"`
	}
	_ = json.NewDecoder(dResp.Body).Decode(&dResult)
	if len(dResult.Decisions) == 0 {
		t.Fatalf("expected recorded selection decision provenance in DB")
	}

	dec := dResult.Decisions[0]
	if dec.SelectedProviderID != "" {
		t.Fatalf("expected no provider selected when snapshot absent, got %s", dec.SelectedProviderID)
	}

	found17B := false
	for _, eval := range dec.CandidatesEvaluated {
		if eval.ProviderID == "qwen3_asr_1_7b" {
			found17B = true
			if eval.Eligible {
				t.Fatalf("expected qwen3_asr_1_7b to be ineligible without snapshot binding")
			}
			if eval.RejectionCode != "SNAPSHOT_UNVERIFIED" {
				t.Fatalf("expected RejectionCode SNAPSHOT_UNVERIFIED, got %s (reason: %s)", eval.RejectionCode, eval.Reason)
			}
		}
	}
	if !found17B {
		t.Fatalf("expected qwen3_asr_1_7b in candidate evaluations")
	}

	// 3. Now register and verify the snapshot via API
	verifyPayload, _ := json.Marshal(map[string]any{
		"manifest":   manifest,
		"local_path": modelDir,
	})
	vResp, err := http.Post(ts.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(verifyPayload))
	if err != nil {
		t.Fatalf("POST verify failed: %v", err)
	}
	defer vResp.Body.Close()
	if vResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(vResp.Body)
		t.Fatalf("expected 201 Created from snapshot verify, got %d: %s", vResp.StatusCode, string(b))
	}

	// 4. Now candidate evaluation succeeds for qwen3_asr_1_7b!
	resp2, err := http.Post(ts.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decidePayload))
	if err != nil {
		t.Fatalf("POST decide 2 failed: %v", err)
	}
	defer resp2.Body.Close()

	var decideResult2 struct {
		SelectedProviderID string `json:"selected_provider_id"`
		Decision           struct {
			Evaluations []domain.CandidateEvaluation `json:"evaluations"`
		} `json:"decision"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&decideResult2)

	if decideResult2.SelectedProviderID != "qwen3_asr_1_7b" {
		t.Fatalf("expected qwen3_asr_1_7b to be selected after verification, got: %s", decideResult2.SelectedProviderID)
	}
}
