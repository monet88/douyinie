package seam1_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

// setupJobWithStages creates a fully populated LocalizationJob with source asset,
// preflight, runs, stage execution, provider attempt, decision, and registered license manifests.
func setupJobWithStages(t *testing.T, h *testHarness) (string, string) {
	t.Helper()

	mediaPath := createSyntheticMedia(t, h.dir, "bundle_test_source.mp4")

	// 1. Ingest asset with Rights Attestation via Seam 1 API
	ingestPayload := map[string]any{
		"file_path": mediaPath,
		"attestation": map[string]any{
			"attestation_type": "OPERATOR_EXPLICIT_CONFIRMATION",
			"declared_by":      "bundle-operator",
			"terms_accepted":   true,
			"notes":            "Attestation for bundle integrity test",
		},
	}
	body, _ := json.Marshal(ingestPayload)
	resp, err := http.Post(h.server.URL+"/api/v1/assets/ingest", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/assets/ingest failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("ingest failed with status %d: %s", resp.StatusCode, string(b))
	}

	var ir service.IngestResult
	if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil {
		t.Fatalf("decode ingest response: %v", err)
	}
	assetID := ir.Asset.ID

	// 2. Set audio role plan via Seam 1 API
	rolePlanPayload := map[string]any{
		"segments": []map[string]any{
			{"start_ms": 0, "end_ms": 1500, "role": "narration/dialogue"},
		},
	}
	rpBody, _ := json.Marshal(rolePlanPayload)
	rpResp, err := http.Post(h.server.URL+"/api/v1/assets/"+assetID+"/audio-role-plan", "application/json", bytes.NewReader(rpBody))
	if err != nil {
		t.Fatalf("POST audio-role-plan failed: %v", err)
	}
	rpResp.Body.Close()

	// 3. Create LocalizationJob via Seam 1 API
	jobPayload := map[string]string{
		"source_asset_id": assetID,
		"target_language": "vi",
	}
	jBody, _ := json.Marshal(jobPayload)
	jResp, err := http.Post(h.server.URL+"/api/v1/jobs", "application/json", bytes.NewReader(jBody))
	if err != nil {
		t.Fatalf("POST /api/v1/jobs failed: %v", err)
	}
	defer jResp.Body.Close()

	var jRes struct {
		Job domain.LocalizationJob `json:"job"`
	}
	if err := json.NewDecoder(jResp.Body).Decode(&jRes); err != nil {
		t.Fatalf("decode job response: %v", err)
	}
	jobID := jRes.Job.ID

	// 4. Create Run via Seam 1 API
	runPayload := map[string]any{
		"config_snapshot_json": `{"profile": "hybrid", "tts_model": "fake_vieneu_tts_vi"}`,
	}
	rBody, _ := json.Marshal(runPayload)
	rResp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/runs", "application/json", bytes.NewReader(rBody))
	if err != nil {
		t.Fatalf("POST /api/v1/jobs/{id}/runs failed: %v", err)
	}
	defer rResp.Body.Close()

	var rRes struct {
		Run domain.LocalizationRun `json:"run"`
	}
	if err := json.NewDecoder(rResp.Body).Decode(&rRes); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	runID := rRes.Run.ID

	// 5. Execute Provider Routing via Seam 1 API to generate SelectionDecision & ProviderAttempt
	routePayload := map[string]any{
		"run_id":   runID,
		"stage":    "asr",
		"language": "zh",
	}
	rtBody, _ := json.Marshal(routePayload)
	rtResp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(rtBody))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/decide failed: %v", err)
	}
	rtResp.Body.Close()

	// Execute routed attempt
	execPayload := map[string]any{
		"run_id":      runID,
		"stage":       "asr",
		"provider_id": "fake_qwen3_asr",
		"input_hash":  ir.Asset.SHA256,
	}
	exBody, _ := json.Marshal(execPayload)
	exResp, err := http.Post(h.server.URL+"/api/v1/routing/execute", "application/json", bytes.NewReader(exBody))
	if err != nil {
		t.Fatalf("POST /api/v1/routing/execute failed: %v", err)
	}
	exResp.Body.Close()

	// 6. Record a stage execution
	now := time.Now().UTC()
	se := domain.StageExecution{
		ID:          uuid.NewString(),
		RunID:       runID,
		Stage:       "asr",
		Status:      domain.StageStatusSucceeded,
		StartedAt:   &now,
		CompletedAt: &now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_ = h.db.CreateStageExecution(context.Background(), se)

	return jobID, assetID
}

func TestSeam1_BundleExport_SelfContained_SecretFree_NoMachineLocalPaths(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := setupJobWithStages(t, h)

	// 1. Export job bundle via Seam 1 API: POST /api/v1/jobs/{id}/export
	resp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/v1/jobs/{id}/export failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("export failed with status %d: %s", resp.StatusCode, string(b))
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/zip") {
		t.Errorf("expected Content-Type application/zip, got %q", contentType)
	}

	zipBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read zip stream: %v", err)
	}
	if len(zipBytes) == 0 {
		t.Fatalf("empty zip bundle returned")
	}

	// 2. Inspect zip archive contents
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("open exported zip: %v", err)
	}

	var manifestBytes []byte
	artifactFiles := make(map[string][]byte)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open file %s in zip: %v", f.Name, err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()

		if f.Name == "manifest.json" {
			manifestBytes = data
		} else if strings.HasPrefix(f.Name, "artifacts/") {
			artifactFiles[f.Name] = data
		}
	}

	if manifestBytes == nil {
		t.Fatalf("manifest.json missing in exported zip bundle")
	}

	// 3. Verify Manifest carries 4 obligation layers and NO secrets / NO machine-local paths
	var manifest domain.JobBundleManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}

	if manifest.Job.ID != jobID {
		t.Errorf("expected job ID %s, got %s", jobID, manifest.Job.ID)
	}

	// Invariant: no machine-local absolute paths
	manifestStr := string(manifestBytes)
	if strings.Contains(manifestStr, `F:\CodeBase`) || strings.Contains(manifestStr, `C:\Users`) {
		t.Errorf("manifest contains machine-local path: %s", manifestStr)
	}
	if filepath.IsAbs(manifest.SourceAsset.OriginalFilename) {
		t.Errorf("OriginalFilename is absolute path: %s", manifest.SourceAsset.OriginalFilename)
	}

	// Invariant: no secret credentials in manifest
	if strings.Contains(manifestStr, "sk-") || strings.Contains(manifestStr, "ghp_") {
		t.Errorf("manifest contains secret tokens!")
	}

	// Invariant: 4 license obligation layers
	if len(manifest.LicenseManifests) == 0 {
		t.Fatalf("manifest has no license manifest entries")
	}
	for _, lm := range manifest.LicenseManifests {
		if lm.CodeLicense == "" || lm.ModelLicense == "" || lm.DataLicense == "" || lm.ServiceTerms == "" {
			t.Errorf("license entry %s missing required obligation layers: %+v", lm.DependencyName, lm)
		}
	}

	// 4. Verify reachable artifacts are self-contained in zip
	if len(manifest.Artifacts) == 0 {
		t.Fatalf("manifest declared 0 artifacts")
	}
	for _, art := range manifest.Artifacts {
		data, exists := artifactFiles[art.BundlePath]
		if !exists {
			t.Errorf("artifact file %s declared in manifest but missing from zip", art.BundlePath)
		}
		if int64(len(data)) != art.ByteSize {
			t.Errorf("artifact %s size mismatch: %d != %d", art.BundlePath, len(data), art.ByteSize)
		}
	}
}

func TestSeam1_BundleImport_TamperRejection(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := setupJobWithStages(t, h)

	// 1. Export valid bundle
	resp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	defer resp.Body.Close()

	origZip, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	// 2. Tamper with an artifact in the zip (corrupt 1 byte)
	zr, err := zip.NewReader(bytes.NewReader(origZip), int64(len(origZip)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}

	var tamperedZip bytes.Buffer
	zw := zip.NewWriter(&tamperedZip)

	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()

		if strings.HasPrefix(f.Name, "artifacts/") && len(data) > 0 {
			// Flip a byte to tamper
			data[0] ^= 0xAA
		}

		w, _ := zw.Create(f.Name)
		_, _ = w.Write(data)
	}
	zw.Close()

	// 3. Attempt import on fresh second harness
	h2 := setupHarness(t)

	importResp, err := http.Post(h2.server.URL+"/api/v1/jobs/import", "application/zip", bytes.NewReader(tamperedZip.Bytes()))
	if err != nil {
		t.Fatalf("POST /api/v1/jobs/import failed: %v", err)
	}
	defer importResp.Body.Close()

	// Invariant: Tampered artifact MUST be rejected with HTTP 422 Unprocessable Entity
	if importResp.StatusCode != http.StatusUnprocessableEntity {
		bodyBytes, _ := io.ReadAll(importResp.Body)
		t.Fatalf("expected HTTP 422 Unprocessable Entity for tampered artifact, got %d: %s", importResp.StatusCode, string(bodyBytes))
	}

	var errRes struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(importResp.Body).Decode(&errRes)
	if !strings.Contains(strings.ToLower(errRes.Error), "tamper") && !strings.Contains(strings.ToLower(errRes.Error), "hash") {
		t.Errorf("expected tamper / hash mismatch in error message, got %q", errRes.Error)
	}
}

func TestSeam1_BundleImport_SecretRejection(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := setupJobWithStages(t, h)

	resp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	defer resp.Body.Close()

	origZip, _ := io.ReadAll(resp.Body)

	// Tamper: inject raw API key in manifest.json
	zr, _ := zip.NewReader(bytes.NewReader(origZip), int64(len(origZip)))
	var tamperedZip bytes.Buffer
	zw := zip.NewWriter(&tamperedZip)

	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()

		if f.Name == "manifest.json" {
			data = bytes.Replace(data, []byte(`"target_language": "vi"`), []byte(`"target_language": "vi", "api_key": "sk-secretToken1234567890abcdef"`), 1)
		}

		w, _ := zw.Create(f.Name)
		_, _ = w.Write(data)
	}
	zw.Close()

	h2 := setupHarness(t)
	importResp, err := http.Post(h2.server.URL+"/api/v1/jobs/import", "application/zip", bytes.NewReader(tamperedZip.Bytes()))
	if err != nil {
		t.Fatalf("POST /api/v1/jobs/import failed: %v", err)
	}
	defer importResp.Body.Close()

	// Invariant: Secret presence MUST be rejected with HTTP 422 Unprocessable Entity
	if importResp.StatusCode != http.StatusUnprocessableEntity {
		bodyBytes, _ := io.ReadAll(importResp.Body)
		t.Fatalf("expected HTTP 422 for secret detected, got %d: %s", importResp.StatusCode, string(bodyBytes))
	}
}

func TestSeam1_BundleImport_SecretWithSpecialCharactersRejection(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := setupJobWithStages(t, h)

	resp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	defer resp.Body.Close()

	origZip, _ := io.ReadAll(resp.Body)

	// Tamper: inject API key with special characters in manifest.json
	zr, _ := zip.NewReader(bytes.NewReader(origZip), int64(len(origZip)))
	var tamperedZip bytes.Buffer
	zw := zip.NewWriter(&tamperedZip)

	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()

		if f.Name == "manifest.json" {
			data = bytes.Replace(data, []byte(`"target_language": "vi"`), []byte(`"target_language": "vi", "api_key": "sec!@#$%^&*ret12345"`), 1)
		}

		w, _ := zw.Create(f.Name)
		_, _ = w.Write(data)
	}
	zw.Close()

	h2 := setupHarness(t)
	importResp, err := http.Post(h2.server.URL+"/api/v1/jobs/import", "application/zip", bytes.NewReader(tamperedZip.Bytes()))
	if err != nil {
		t.Fatalf("POST /api/v1/jobs/import failed: %v", err)
	}
	defer importResp.Body.Close()

	// Invariant: Secret with special characters MUST be rejected with HTTP 422 Unprocessable Entity
	if importResp.StatusCode != http.StatusUnprocessableEntity {
		bodyBytes, _ := io.ReadAll(importResp.Body)
		t.Fatalf("expected HTTP 422 for secret with special chars detected, got %d: %s", importResp.StatusCode, string(bodyBytes))
	}
}

func TestSeam1_BundleImport_MachineLocalPathRejection(t *testing.T) {
	h := setupHarness(t)
	jobID, _ := setupJobWithStages(t, h)

	resp, err := http.Post(h.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	defer resp.Body.Close()

	origZip, _ := io.ReadAll(resp.Body)

	// Tamper: inject machine-local absolute path in manifest.json
	zr, _ := zip.NewReader(bytes.NewReader(origZip), int64(len(origZip)))
	var tamperedZip bytes.Buffer
	zw := zip.NewWriter(&tamperedZip)

	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()

		if f.Name == "manifest.json" {
			data = bytes.Replace(data, []byte(`"target_language": "vi"`), []byte(`"target_language": "vi", "abs_path": "C:\\malicious\\path\\secret.dll"`), 1)
		}

		w, _ := zw.Create(f.Name)
		_, _ = w.Write(data)
	}
	zw.Close()

	h2 := setupHarness(t)
	importResp, err := http.Post(h2.server.URL+"/api/v1/jobs/import", "application/zip", bytes.NewReader(tamperedZip.Bytes()))
	if err != nil {
		t.Fatalf("POST /api/v1/jobs/import failed: %v", err)
	}
	defer importResp.Body.Close()

	// Invariant: Machine-local absolute path MUST be rejected with HTTP 422 Unprocessable Entity
	if importResp.StatusCode != http.StatusUnprocessableEntity {
		bodyBytes, _ := io.ReadAll(importResp.Body)
		t.Fatalf("expected HTTP 422 for machine-local path detected, got %d: %s", importResp.StatusCode, string(bodyBytes))
	}
}

func TestSeam1_BundleRoundTrip_ReproducesJobAcrossHosts(t *testing.T) {
	// Host A: original host
	hA := setupHarness(t)
	jobID, assetID := setupJobWithStages(t, hA)

	// Export via HTTP API
	exportResp, err := http.Post(hA.server.URL+"/api/v1/jobs/"+jobID+"/export", "application/json", nil)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	defer exportResp.Body.Close()

	if exportResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(exportResp.Body)
		t.Fatalf("export failed: %s", string(b))
	}
	bundleZip, err := io.ReadAll(exportResp.Body)
	if err != nil {
		t.Fatalf("read exported bundle: %v", err)
	}

	// Host B: separate clean host environment
	hB := setupHarness(t)

	// Ingest bundle via HTTP API on Host B
	importResp, err := http.Post(hB.server.URL+"/api/v1/jobs/import", "application/zip", bytes.NewReader(bundleZip))
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	defer importResp.Body.Close()

	if importResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(importResp.Body)
		t.Fatalf("import failed with status %d: %s", importResp.StatusCode, string(b))
	}

	// Assert Job reproduced on Host B via Seam 1 API
	getJobResp, err := http.Get(hB.server.URL + "/api/v1/jobs/" + jobID)
	if err != nil {
		t.Fatalf("GET /api/v1/jobs failed: %v", err)
	}
	defer getJobResp.Body.Close()

	if getJobResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(getJobResp.Body)
		t.Fatalf("reproduced job not found on host B: %s", string(b))
	}

	var jobData struct {
		Job domain.LocalizationJob `json:"job"`
	}
	_ = json.NewDecoder(getJobResp.Body).Decode(&jobData)
	if jobData.Job.ID != jobID || jobData.Job.TargetLanguage != "vi" {
		t.Errorf("unexpected job reproduced: %+v", jobData.Job)
	}

	// Assert SourceAsset reproduced on Host B via Seam 1 API
	getAssetResp, err := http.Get(hB.server.URL + "/api/v1/assets/" + assetID)
	if err != nil {
		t.Fatalf("GET /api/v1/assets failed: %v", err)
	}
	defer getAssetResp.Body.Close()

	if getAssetResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(getAssetResp.Body)
		t.Fatalf("reproduced asset not found on host B: %s", string(b))
	}

	var assetData struct {
		Asset domain.SourceAsset `json:"asset"`
	}
	_ = json.NewDecoder(getAssetResp.Body).Decode(&assetData)
	if assetData.Asset.ID != assetID || assetData.Asset.SHA256 == "" {
		t.Errorf("unexpected asset reproduced: %+v", assetData.Asset)
	}

	// Assert media file is present and verified in Host B's CAS store
	if !hB.casStore.Exists(assetData.Asset.SHA256) {
		t.Fatalf("media artifact not found in Host B CAS store!")
	}
	if err := hB.casStore.VerifyIntegrity(assetData.Asset.SHA256); err != nil {
		t.Fatalf("integrity verification failed on Host B CAS store: %v", err)
	}

	// Assert preflight report reproduced on Host B
	getPfResp, err := http.Get(hB.server.URL + "/api/v1/assets/" + assetID + "/preflight")
	if err != nil {
		t.Fatalf("GET preflight failed: %v", err)
	}
	defer getPfResp.Body.Close()

	if getPfResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for preflight report on Host B, got %d", getPfResp.StatusCode)
	}

	// Assert audio role plan reproduced on Host B
	getArpResp, err := http.Get(hB.server.URL + "/api/v1/assets/" + assetID + "/audio-role-plan")
	if err != nil {
		t.Fatalf("GET audio-role-plan failed: %v", err)
	}
	defer getArpResp.Body.Close()

	if getArpResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for audio-role-plan on Host B, got %d", getArpResp.StatusCode)
	}

	// Assert licenses reproduced on Host B
	getLicResp, err := http.Get(hB.server.URL + "/api/v1/licenses")
	if err != nil {
		t.Fatalf("GET licenses failed: %v", err)
	}
	defer getLicResp.Body.Close()

	var licData struct {
		Licenses []domain.LicenseManifestEntry `json:"licenses"`
	}
	_ = json.NewDecoder(getLicResp.Body).Decode(&licData)
	if len(licData.Licenses) == 0 {
		t.Errorf("expected reproduced licenses on Host B, got none")
	}
}
