package seam1_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
func TestSeam1_Snapshot_OCR_RegistrationAndVerification(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup cas: %v", err)
	}

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)
	queueSvc := queue.NewService(db)
	resScheduler := scheduler.New()

	srv := server.New(server.Config{
		Addr:        "127.0.0.1:0",
		DB:          db,
		CASStore:    casStore,
		LicenseSvc:  licSvc,
		SnapshotSvc: snapSvc,
		QueueSvc:    queueSvc,
		Scheduler:   resScheduler,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// 1. Create 3 independent verified snapshot directories: det, rec, ori
	detDir, detManifest := createSnapshotDirWithFiles(t, map[string]string{
		"inference.pdmodel":   "det_model_bytes",
		"inference.pdiparams": "det_params_bytes",
	})
	detManifest.ModelID = "PP-OCRv6_medium_det"
	detManifest.ModelVersion = "v6"
	detSHA, err := domain.ComputeSnapshotManifestSHA256(&detManifest)
	if err != nil {
		t.Fatalf("compute det sha: %v", err)
	}
	detManifest.SnapshotManifestSHA256 = detSHA

	recDir, recManifest := createSnapshotDirWithFiles(t, map[string]string{
		"inference.pdmodel":   "rec_model_bytes",
		"inference.pdiparams": "rec_params_bytes",
	})
	recManifest.ModelID = "PP-OCRv6_medium_rec"
	recManifest.ModelVersion = "v6"
	recSHA, err := domain.ComputeSnapshotManifestSHA256(&recManifest)
	if err != nil {
		t.Fatalf("compute rec sha: %v", err)
	}
	recManifest.SnapshotManifestSHA256 = recSHA

	oriDir, oriManifest := createSnapshotDirWithFiles(t, map[string]string{
		"inference.pdmodel":   "ori_model_bytes",
		"inference.pdiparams": "ori_params_bytes",
	})
	oriManifest.ModelID = "PP-LCNet_x1_0_textline_ori"
	oriManifest.ModelVersion = "v1"
	oriSHA, err := domain.ComputeSnapshotManifestSHA256(&oriManifest)
	if err != nil {
		t.Fatalf("compute ori sha: %v", err)
	}
	oriManifest.SnapshotManifestSHA256 = oriSHA
	// Register 4-layer license manifests for all 3 OCR dependencies
	initCtx := context.Background()
	for _, m := range []struct {
		name, ver, sha string
	}{
		{"PP-OCRv6_medium_det", "v6", detSHA},
		{"PP-OCRv6_medium_rec", "v6", recSHA},
		{"PP-LCNet_x1_0_textline_ori", "v1", oriSHA},
	} {
		_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
			ID:             uuid.NewString(),
			DependencyName: m.name,
			Version:        m.ver,
			SHA256:         m.sha,
			CodeLicense:    "Apache-2.0",
			ModelLicense:   "Community-License",
			DataLicense:    "Open-Data",
			ServiceTerms:   "Self-Hosted",
			Verified:       true,
			CreatedAt:      time.Now().UTC(),
		})
	}

	// 2. Verify all 3 snapshots through public API
	for _, pair := range []struct {
		m domain.SnapshotManifest
		p string
	}{
		{detManifest, detDir},
		{recManifest, recDir},
		{oriManifest, oriDir},
	} {
		payload, _ := json.Marshal(map[string]any{
			"manifest":   pair.m,
			"local_path": pair.p,
		})
		vResp, err := http.Post(ts.URL+"/api/v1/snapshots/verify", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("POST verify %s failed: %v", pair.m.ModelID, err)
		}
		if vResp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(vResp.Body)
			vResp.Body.Close()
			t.Fatalf("expected 201 Created from verify %s, got %d: %s", pair.m.ModelID, vResp.StatusCode, string(b))
		}
		vResp.Body.Close()
	}

	// 3. WorkerOCRProvider binds verified snapshots and exposes full provenance
	ppocr, err := provider.NewWorkerOCRProvider("ppocr_v6", "paddleocr-v6", "v6", 0.92)
	if err != nil {
		t.Fatalf("new worker ocr provider: %v", err)
	}
	ppocr.SetSnapshotService(snapSvc)

	gotDetSHA, gotRecSHA, gotOriSHA, runtimeIdentity := ppocr.OCRSnapshotDigests()
	if gotDetSHA != detSHA {
		t.Fatalf("got det sha %s, want %s", gotDetSHA, detSHA)
	}
	if gotRecSHA != recSHA {
		t.Fatalf("got rec sha %s, want %s", gotRecSHA, recSHA)
	}
	if gotOriSHA != oriSHA {
		t.Fatalf("got ori sha %s, want %s", gotOriSHA, oriSHA)
	}
	if runtimeIdentity != "paddleocr:3.7.0@b03f46425e8ff4442b268ce449e3eef758146cd4" {
		t.Fatalf("unexpected runtime identity: %s", runtimeIdentity)
	}

	obs := ppocr.ObservedModel()
	if !strings.Contains(obs, detSHA[:8]) || !strings.Contains(obs, recSHA[:8]) || !strings.Contains(obs, oriSHA[:8]) {
		t.Fatalf("observed model does not contain snapshot prefixes: %s", obs)
	}
	if ppocr.ServiceBaselineID() != "paddleocr-3.7.0-b03f46425e8ff4442b268ce449e3eef758146cd4" {
		t.Fatalf("unexpected baseline id: %s", ppocr.ServiceBaselineID())
	}

	deps := ppocr.ModelDependencies()
	if len(deps) != 3 {
		t.Fatalf("expected 3 model dependencies, got %d", len(deps))
	}

	// 4. Mutating any file in a snapshot causes CheckBindingFingerprints to fail closed
	if err := os.WriteFile(filepath.Join(detDir, "inference.pdmodel"), []byte("mutated_det_bytes"), 0644); err != nil {
		t.Fatalf("write mutated file: %v", err)
	}
	if err := snapSvc.CheckBindingFingerprints("PP-OCRv6_medium_det", "v6"); err == nil {
		t.Fatalf("expected mutated snapshot to fail CheckBindingFingerprints, got nil")
	} else if !strings.Contains(err.Error(), "mutated") {
		t.Fatalf("expected mutated error, got: %v", err)
	}
}

func TestSeam1_Snapshot_OCR_ProvenanceAndCacheIsolation(t *testing.T) {
	h1, err := domain.ComputeTextRegionPlanProvenanceHash(
		"asset-1", "ppocr_v6", "paddleocr-v6", "v6", 500,
		"det_sha_1", "rec_sha_1", "ori_sha_1", "paddleocr:3.7.0@b03f46425e8ff4442b268ce449e3eef758146cd4",
	)
	if err != nil {
		t.Fatalf("compute h1: %v", err)
	}

	// Changing any snapshot digest changes the provenance hash
	h2, err := domain.ComputeTextRegionPlanProvenanceHash(
		"asset-1", "ppocr_v6", "paddleocr-v6", "v6", 500,
		"det_sha_2_different", "rec_sha_1", "ori_sha_1", "paddleocr:3.7.0@b03f46425e8ff4442b268ce449e3eef758146cd4",
	)
	if err != nil {
		t.Fatalf("compute h2: %v", err)
	}

	if h1 == h2 {
		t.Fatalf("expected distinct provenance hashes for different snapshot digests, got identical %s", h1)
	}

	// Backward compatibility: omitting extraIdentities produces hash matching empty extraIdentities
	hBase1, err := domain.ComputeTextRegionPlanProvenanceHash("asset-1", "fake_ocr", "paddleocr", "v4", 500)
	if err != nil {
		t.Fatalf("compute hBase1: %v", err)
	}
	hBase2, err := domain.ComputeTextRegionPlanProvenanceHash("asset-1", "fake_ocr", "paddleocr", "v4", 500)
	if err != nil {
		t.Fatalf("compute hBase2: %v", err)
	}
	if hBase1 != hBase2 {
		t.Fatalf("base provenance hash not deterministic")
	}
	if hBase1 == h1 {
		t.Fatalf("base hash unexpectedly collided with snapshot-enriched hash")
	}
}

func TestSeam1_Snapshot_TTS_RegistrationAndVerification(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_tts_snap.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	// 1. Verify default preset voice rotations (Issue #68 / #93)
	// Vietnamese unattended default is the approved ZeroTTS rotation, in order.
	viVoices := provider.DefaultPresetVoices("vi")
	if len(viVoices) != provider.DefaultVIUnattendedVoiceCount {
		t.Fatalf("expected exactly %d ZeroTTS preset voices in the unattended VI rotation, got %d",
			provider.DefaultVIUnattendedVoiceCount, len(viVoices))
	}
	expectedVI := []string{"quangminh", "maichi"}
	for i, exp := range expectedVI {
		if viVoices[i].VoiceID != exp {
			t.Errorf("vi voice [%d] expected %s, got %s", i, exp, viVoices[i].VoiceID)
		}
		if viVoices[i].ProviderID != provider.ZeroTTSProviderID {
			t.Errorf("vi voice [%d] provider expected %s, got %s", i, provider.ZeroTTSProviderID, viVoices[i].ProviderID)
		}
	}

	// Verified presets beyond the unattended rotation stay selectable/frozen-capable
	// without becoming defaults merely because they exist in the snapshot.
	allPresets := provider.ZeroTTSPresetVoices()
	if len(allPresets) <= provider.DefaultVIUnattendedVoiceCount {
		t.Fatalf("expected verified ZeroTTS presets beyond the unattended rotation, got %d", len(allPresets))
	}
	for _, v := range allPresets {
		if !provider.IsVerifiedTTSVoice(provider.ZeroTTSProviderID, v.VoiceID) {
			t.Errorf("ZeroTTS preset %s must remain selectable/auditable", v.VoiceID)
		}
	}
	// VieNeu stays the compatibility lane for historical frozen assignments.
	vieneuVoices := provider.VieNeuPresetVoices()
	if len(vieneuVoices) != 4 {
		t.Fatalf("expected exactly 4 VieNeu compatibility voices, got %d", len(vieneuVoices))
	}
	expectedVieNeu := []string{"Trúc Ly", "Phạm Tuyên", "Đoan Trang", "Xuân Vĩnh"}
	for i, exp := range expectedVieNeu {
		if vieneuVoices[i].VoiceID != exp {
			t.Errorf("vieneu voice [%d] expected %s, got %s", i, exp, vieneuVoices[i].VoiceID)
		}
		if vieneuVoices[i].ProviderID != provider.VieNeuProviderID {
			t.Errorf("vieneu voice [%d] provider expected %s, got %s", i, provider.VieNeuProviderID, vieneuVoices[i].ProviderID)
		}
	}

	enVoices := provider.DefaultPresetVoices("en")
	if len(enVoices) != 4 {
		t.Fatalf("expected exactly 4 Kokoro preset voices in default rotation, got %d", len(enVoices))
	}
	expectedEN := []string{"af_heart", "am_michael", "af_bella", "am_fenrir"}
	for i, exp := range expectedEN {
		if enVoices[i].VoiceID != exp {
			t.Errorf("en voice [%d] expected %s, got %s", i, exp, enVoices[i].VoiceID)
		}
		if enVoices[i].ProviderID != provider.KokoroProviderID {
			t.Errorf("en voice [%d] provider expected %s, got %s", i, provider.KokoroProviderID, enVoices[i].ProviderID)
		}
	}

	// 2. Fabricated VieNeu snapshot with generic config.json or voices/*.pt must fail validation
	fabDir, fabManifest := createSnapshotDirWithFiles(t, map[string]string{
		"config.json":          `{"model_type": "vieneu_v3_turbo"}`,
		"model.safetensors":    "fake_vieneu_weights_bytes_1278db00",
		"voices/Trúc Ly.pt":    "voice_truc_ly_data",
		"voices/Phạm Tuyên.pt": "voice_pham_tuyen_data",
	})
	fabManifest.ModelID = provider.VieNeuModelID
	fabManifest.ModelVersion = provider.VieNeuModelVersion
	if _, err := domain.ResolveTTSVoiceEntrypoint(fabManifest, fabDir, provider.VieNeuModelID, "Trúc Ly"); err == nil {
		t.Fatalf("expected fabricated voices/*.pt without voices_v3_turbo.json to fail validation")
	}

	// 2b. Register genuine VieNeu model snapshot using v3 Turbo catalog
	vieneuFiles := map[string]string{
		"src/vieneu/assets/voices_v3_turbo.json": `{"presets": {"Trúc Ly": {"id": "Trúc Ly"}, "Phạm Tuyên": {"id": "Phạm Tuyên"}, "Đoan Trang": {"id": "Đoan Trang"}, "Xuân Vĩnh": {"id": "Xuân Vĩnh"}}}`,
	}
	for _, asset := range domain.PinnedVieNeuAssets {
		if _, ok := vieneuFiles[asset.RelativePath]; !ok {
			vieneuFiles[asset.RelativePath] = "fixture:" + asset.RelativePath
		}
	}
	vieneuDir, vieneuManifest := createSnapshotDirWithFiles(t, vieneuFiles)
	vieneuManifest.ModelID = provider.VieNeuModelID
	vieneuManifest.ModelVersion = provider.VieNeuModelVersion
	vSHA, err := domain.ComputeSnapshotManifestSHA256(&vieneuManifest)
	if err != nil {
		t.Fatalf("compute vieneu manifest sha: %v", err)
	}
	vieneuManifest.SnapshotManifestSHA256 = vSHA

	_ = licSvc.RegisterManifest(context.Background(), domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: provider.VieNeuModelID,
		Version:        provider.VieNeuModelVersion,
		SHA256:         vSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})

	vBinding, err := snapSvc.RegisterAndVerifySnapshot(context.Background(), vieneuManifest, vieneuDir)
	if err != nil {
		t.Fatalf("RegisterAndVerifySnapshot for VieNeu failed: %v", err)
	}
	if vBinding.DependencyName != provider.VieNeuModelID {
		t.Fatalf("expected dependency name %s, got %s", provider.VieNeuModelID, vBinding.DependencyName)
	}

	// Registration above proves the declared-digest -> on-disk-bytes link for this fixture. The
	// resolver adds the byte pin: a manifest that is self-consistent but declares different bytes
	// for a load-bearing weight is rejected even though registration accepted it.
	if _, err := domain.ResolveTTSVoiceEntrypoint(vieneuManifest, vieneuDir, provider.VieNeuModelID, "Trúc Ly"); !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for fixture bytes that are not the pinned weights, got: %v", err)
	}

	// Resolver view of the same snapshot with the pinned digests declared, i.e. what the provisioned
	// snapshot declares (see domain.TestSnapshot_ResolveTTSVoiceEntrypoint_ZeroTTSPinnedWeights).
	vieneuPinned := vieneuManifest
	vieneuPinned.Files = append([]domain.SnapshotFileEntry(nil), vieneuManifest.Files...)
	for i := range vieneuPinned.Files {
		for _, asset := range domain.PinnedVieNeuAssets {
			if vieneuPinned.Files[i].RelativePath == asset.RelativePath {
				vieneuPinned.Files[i].SHA256 = asset.SHA256
			}
		}
	}

	// All 4 VieNeu compatibility voices resolve
	for _, vID := range expectedVieNeu {
		ep, epErr := domain.ResolveTTSVoiceEntrypoint(vieneuPinned, vieneuDir, provider.VieNeuModelID, vID)
		if epErr != nil {
			t.Errorf("expected voice %s to resolve, got err: %v", vID, epErr)
		}
		expectedCatalogPath := filepath.Join(vieneuDir, "src", "vieneu", "assets", "voices_v3_turbo.json")
		if ep != expectedCatalogPath {
			t.Errorf("expected resolved ep %s, got %s", expectedCatalogPath, ep)
		}
	}

	// Unverified voice fails closed
	if _, err := domain.ResolveTTSVoiceEntrypoint(vieneuPinned, vieneuDir, provider.VieNeuModelID, "unverified_voice"); err == nil {
		t.Fatalf("expected unverified VieNeu voice to fail closed")
	}
	// 3. Kokoro model snapshot: synthetic fake checkpoint bytes cannot pass the pinned RC SHA-256 (fail-closed proof)
	kokoroDir, kokoroManifest := createSnapshotDirWithFiles(t, map[string]string{
		"config.json":          `{"model_type": "kokoro"}`,
		"kokoro-v1_0.pth":      "fake_synthetic_kokoro_checkpoint_bytes",
		"voices/af_heart.pt":   "voice_af_heart_data",
		"voices/am_michael.pt": "voice_am_michael_data",
		"voices/af_bella.pt":   "voice_af_bella_data",
		"voices/am_fenrir.pt":  "voice_am_fenrir_data",
	})
	kokoroManifest.ModelID = provider.KokoroModelID
	kokoroManifest.ModelVersion = provider.KokoroModelVersion
	kSHA, err := domain.ComputeSnapshotManifestSHA256(&kokoroManifest)
	if err != nil {
		t.Fatalf("compute kokoro manifest sha: %v", err)
	}
	kokoroManifest.SnapshotManifestSHA256 = kSHA

	// Case A: If manifest truthfully records the fake bytes' hash, ResolveTTSVoiceEntrypoint fails closed
	// because the checkpoint SHA does not match the immutable pinned RC checkpoint pin.
	if _, err := domain.ResolveTTSVoiceEntrypoint(kokoroManifest, kokoroDir, provider.KokoroModelID, "af_heart"); !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected fake checkpoint bytes to fail closed with ErrSnapshotDigestMismatch, got: %v", err)
	}

	// Case B: If manifest claims the pinned RC SHA but on-disk bytes are fake, RegisterAndVerifySnapshot fails closed
	// with snapshot digest mismatch without mutating the pin.
	fraudulentManifest := kokoroManifest
	for i, f := range fraudulentManifest.Files {
		if f.RelativePath == "kokoro-v1_0.pth" {
			fraudulentManifest.Files[i].SHA256 = "496dba118d1a58f5f3db2efc88dbdc216e0483fc89fe6e47ee1f2c53f18ad1e4"
		}
	}
	fSHA, err := domain.ComputeSnapshotManifestSHA256(&fraudulentManifest)
	if err != nil {
		t.Fatalf("compute fraudulent manifest sha: %v", err)
	}
	fraudulentManifest.SnapshotManifestSHA256 = fSHA
	_ = licSvc.RegisterManifest(context.Background(), domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: provider.KokoroModelID,
		Version:        provider.KokoroModelVersion,
		SHA256:         fSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	if _, err := snapSvc.RegisterAndVerifySnapshot(context.Background(), fraudulentManifest, kokoroDir); !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected unverified fake checkpoint on disk to fail RegisterAndVerifySnapshot, got: %v", err)
	}

	// Unverified Kokoro voice fails closed
	if _, err := domain.ResolveTTSVoiceEntrypoint(kokoroManifest, kokoroDir, provider.KokoroModelID, "unverified_voice"); err == nil {
		t.Fatalf("expected unverified Kokoro voice to fail closed")
	}

	// Unmanifested Kokoro config.json fails closed
	noConfigKokoroManifest := kokoroManifest
	var filteredFiles []domain.SnapshotFileEntry
	for _, f := range kokoroManifest.Files {
		if f.RelativePath != "config.json" {
			filteredFiles = append(filteredFiles, f)
		}
	}
	noConfigKokoroManifest.Files = filteredFiles
	if _, err := domain.ResolveTTSVoiceEntrypoint(noConfigKokoroManifest, kokoroDir, provider.KokoroModelID, "af_heart"); err == nil {
		t.Fatalf("expected Kokoro without manifest-declared config.json to fail closed")
	}

	// Unmanifested VieNeu MOSS tokenizer fails closed
	noMossVieNeuManifest := vieneuManifest
	var filteredVieNeuFiles []domain.SnapshotFileEntry
	for _, f := range vieneuManifest.Files {
		if !strings.HasPrefix(f.RelativePath, "moss_tokenizer") {
			filteredVieNeuFiles = append(filteredVieNeuFiles, f)
		}
	}
	noMossVieNeuManifest.Files = filteredVieNeuFiles
	if _, err := domain.ResolveTTSVoiceEntrypoint(noMossVieNeuManifest, vieneuDir, provider.VieNeuModelID, "Trúc Ly"); err == nil {
		t.Fatalf("expected VieNeu without manifest-declared MOSS tokenizer to fail closed")
	}
}

func TestSeam1_Snapshot_Separator_RegistrationAndVerification(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_separator_snap.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("create sqlite db: %v", err)
	}
	defer db.Close()

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	// 1. UVR model snapshot: synthetic fake checkpoint bytes cannot pass the pinned RC SHA-256 (fail-closed proof)
	uvrDir, uvrManifest := createSnapshotDirWithFiles(t, map[string]string{
		provider.UVRModelID: "fake_synthetic_uvr_onnx_bytes",
	})
	uvrManifest.ModelID = provider.UVRModelID
	uvrManifest.ModelVersion = provider.UVRModelVersion
	uSHA, err := domain.ComputeSnapshotManifestSHA256(&uvrManifest)
	if err != nil {
		t.Fatalf("compute uvr manifest sha: %v", err)
	}
	uvrManifest.SnapshotManifestSHA256 = uSHA

	// Case A: If manifest truthfully records the fake bytes' hash, ResolveSeparatorEntrypoint fails closed
	// because the ONNX SHA does not match the immutable pinned RC checkpoint pin.
	if _, err := domain.ResolveSeparatorEntrypoint(uvrManifest, uvrDir, provider.UVRModelID); !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected fake ONNX bytes to fail closed with ErrSnapshotDigestMismatch, got: %v", err)
	}

	// Case B: If manifest claims the pinned RC SHA but on-disk bytes are fake, RegisterAndVerifySnapshot fails closed
	fraudulentUVRManifest := uvrManifest
	for i, f := range fraudulentUVRManifest.Files {
		if f.RelativePath == provider.UVRModelID {
			fraudulentUVRManifest.Files[i].SHA256 = domain.PinnedUVRArtifactSHA256
		}
	}
	fUVRSha, err := domain.ComputeSnapshotManifestSHA256(&fraudulentUVRManifest)
	if err != nil {
		t.Fatalf("compute fraudulent uvr manifest sha: %v", err)
	}
	fraudulentUVRManifest.SnapshotManifestSHA256 = fUVRSha
	_ = licSvc.RegisterManifest(context.Background(), domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: provider.UVRModelID,
		Version:        provider.UVRModelVersion,
		SHA256:         fUVRSha,
		CodeLicense:    "MIT",
		ModelLicense:   "MIT",
		DataLicense:    "OpenData",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	})
	if _, err := snapSvc.RegisterAndVerifySnapshot(context.Background(), fraudulentUVRManifest, uvrDir); !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected unverified fake UVR file on disk to fail RegisterAndVerifySnapshot, got: %v", err)
	}

	// Case C: Genuine UVR snapshot where manifest records the pinned RC SHA
	genuineUVRManifest := uvrManifest
	genuineUVRManifest.Files = []domain.SnapshotFileEntry{
		{
			RelativePath: provider.UVRModelID,
			SHA256:       domain.PinnedUVRArtifactSHA256,
			SizeBytes:    1024,
		},
	}
	ep, err := domain.ResolveSeparatorEntrypoint(genuineUVRManifest, uvrDir, provider.UVRModelID)
	if err != nil {
		t.Fatalf("expected genuine UVR entrypoint to resolve, got: %v", err)
	}
	if filepath.Base(ep) != provider.UVRModelID {
		t.Fatalf("expected entrypoint file %s, got %s", provider.UVRModelID, ep)
	}

	// 2. Demucs model snapshot:
	demucsDir, demucsManifest := createSnapshotDirWithFiles(t, map[string]string{
		"955717e8-8726e21a.th": "fake_synthetic_demucs_th_bytes",
	})
	demucsManifest.ModelID = provider.DemucsModelID
	demucsManifest.ModelVersion = provider.DemucsModelVersion
	dSHA, err := domain.ComputeSnapshotManifestSHA256(&demucsManifest)
	if err != nil {
		t.Fatalf("compute demucs manifest sha: %v", err)
	}
	demucsManifest.SnapshotManifestSHA256 = dSHA

	// Case A: Fake bytes' hash fails closed
	if _, err := domain.ResolveSeparatorEntrypoint(demucsManifest, demucsDir, provider.DemucsModelID); !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected fake Demucs checkpoint bytes to fail closed with ErrSnapshotDigestMismatch, got: %v", err)
	}

	// Case B: Genuine Demucs snapshot where manifest records the pinned RC SHA
	genuineDemucsManifest := demucsManifest
	genuineDemucsManifest.Files = []domain.SnapshotFileEntry{
		{
			RelativePath: "955717e8-8726e21a.th",
			SHA256:       domain.PinnedDemucsCheckpointSHA,
			SizeBytes:    2048,
		},
	}
	dep, err := domain.ResolveSeparatorEntrypoint(genuineDemucsManifest, demucsDir, provider.DemucsModelID)
	if err != nil {
		t.Fatalf("expected genuine Demucs entrypoint to resolve, got: %v", err)
	}
	if filepath.Base(dep) != "955717e8-8726e21a.th" {
		t.Fatalf("expected entrypoint file 955717e8-8726e21a.th, got %s", dep)
	}

	// Case C: Reject htdemucs_ft
	if _, err := domain.ResolveSeparatorEntrypoint(genuineDemucsManifest, demucsDir, "htdemucs_ft"); !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected htdemucs_ft rejection, got: %v", err)
	}

	// Case D: Unmanifested UVR file fails closed
	emptyUVRManifest := genuineUVRManifest
	emptyUVRManifest.Files = nil
	if _, err := domain.ResolveSeparatorEntrypoint(emptyUVRManifest, uvrDir, provider.UVRModelID); !errors.Is(err, domain.ErrSeparatorModelAssetMissing) {
		t.Fatalf("expected empty manifest to fail ResolveSeparatorEntrypoint, got: %v", err)
	}

	// Case E: Genuine complete Demucs local repo with htdemucs.yaml bag definition
	if err := os.WriteFile(filepath.Join(demucsDir, "htdemucs.yaml"), []byte("models: ['955717e8']\n"), 0644); err != nil {
		t.Fatal(err)
	}
	completeDemucsManifest := genuineDemucsManifest
	completeDemucsManifest.Files = append(completeDemucsManifest.Files, domain.SnapshotFileEntry{
		RelativePath: "htdemucs.yaml",
		SHA256:       domain.PinnedDemucsBagYAMLSHA256,
		SizeBytes:    21,
	})
	depComplete, err := domain.ResolveSeparatorEntrypoint(completeDemucsManifest, demucsDir, provider.DemucsModelID)
	if err != nil {
		t.Fatalf("expected genuine complete Demucs repo to resolve, got: %v", err)
	}
	if filepath.Base(depComplete) != "955717e8-8726e21a.th" {
		t.Fatalf("expected entrypoint file 955717e8-8726e21a.th, got %s", depComplete)
	}
}

func TestSeam1_Snapshot_AudioRole_YAMNet_RegistrationAndVerification(t *testing.T) {
	tmpDir := t.TempDir()
	yamnetDir := filepath.Join(tmpDir, "yamnet")
	if err := os.MkdirAll(yamnetDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Case A: Missing file fails closed
	genuineYAMNetManifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       provider.YAMNetModelID,
		ModelVersion:  provider.YAMNetModelVersion,
		Files: []domain.SnapshotFileEntry{
			{
				RelativePath: "yamnet.tflite",
				SHA256:       domain.PinnedYAMNetArtifactSHA256,
				SizeBytes:    3870000,
			},
		},
		SnapshotManifestSHA256: domain.PinnedYAMNetManifestSHA256,
	}

	if _, err := domain.ResolveAudioRoleEntrypoint(genuineYAMNetManifest, yamnetDir, provider.YAMNetModelID); !errors.Is(err, domain.ErrSnapshotFileCorrupted) {
		t.Fatalf("expected ErrSnapshotFileCorrupted when file does not exist on disk, got: %v", err)
	}

	// Case B1: Unmanifested yamnet.tflite fails closed
	emptyManifest := genuineYAMNetManifest
	emptyManifest.Files = nil
	if _, err := domain.ResolveAudioRoleEntrypoint(emptyManifest, yamnetDir, provider.YAMNetModelID); !errors.Is(err, domain.ErrAudioRoleModelAssetMissing) {
		t.Fatalf("expected ErrAudioRoleModelAssetMissing when unmanifested, got: %v", err)
	}
	// Case B: Corrupted digest in manifest fails closed
	corruptPath := filepath.Join(yamnetDir, "yamnet.tflite")
	if err := os.WriteFile(corruptPath, []byte("valid-yamnet-weights"), 0644); err != nil {
		t.Fatal(err)
	}
	corruptManifest := genuineYAMNetManifest
	corruptManifest.Files = []domain.SnapshotFileEntry{
		{
			RelativePath: "yamnet.tflite",
			SHA256:       "badbeefbadbeef",
			SizeBytes:    100,
		},
	}
	if _, err := domain.ResolveAudioRoleEntrypoint(corruptManifest, yamnetDir, provider.YAMNetModelID); !errors.Is(err, domain.ErrSnapshotDigestMismatch) {
		t.Fatalf("expected ErrSnapshotDigestMismatch for corrupt manifest digest, got: %v", err)
	}

	// Case C: Genuine model file resolves successfully
	ep, err := domain.ResolveAudioRoleEntrypoint(genuineYAMNetManifest, yamnetDir, provider.YAMNetModelID)
	if err != nil {
		t.Fatalf("expected valid entrypoint to resolve, got: %v", err)
	}
	if filepath.Base(ep) != "yamnet.tflite" {
		t.Fatalf("expected yamnet.tflite, got: %s", ep)
	}

	// Case D: Unknown model ID fails closed
	if _, err := domain.ResolveAudioRoleEntrypoint(genuineYAMNetManifest, yamnetDir, "unknown_audio_model"); !errors.Is(err, domain.ErrAudioRoleModelAssetMissing) {
		t.Fatalf("expected ErrAudioRoleModelAssetMissing for unknown model ID, got: %v", err)
	}
}
