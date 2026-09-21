package seam1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/cas"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

// candidateSplitProber rejects only the CAS object whose bytes were produced
// by the jiji fake adapter (its payload embeds the adapter id), so the ladder
// head fails integrity while the fallback candidate's bytes pass. Reports
// carry the normalized-audio identity, so no ffmpeg is required.
type candidateSplitProber struct {
	jijiCorrupt bool
}

func (p *candidateSplitProber) Probe(ctx context.Context, filePath string, expectedHash string) (*domain.PreflightReport, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	report := &domain.PreflightReport{
		ID:                     uuid.NewString(),
		DurationMs:             1000,
		DurationSec:            1.0,
		VideoCodec:             "h264",
		AudioCodec:             "aac",
		Width:                  1080,
		Height:                 1920,
		FrameRate:              30.0,
		ContainerFormat:        "mp4",
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  "mock-normalized-audio-sha",
		NormalizedAudioCASPath: "mock-normalized-audio-cas",
		CreatedAt:              time.Now().UTC(),
	}
	if p.jijiCorrupt && bytes.Contains(data, []byte("fake_jiji_douyin")) {
		report.ContainerValid = false
		report.Errors = []string{"moov atom not found"}
	}
	return report, nil
}

type acquisitionHarness struct {
	server   *httptest.Server
	db       *storage.DB
	registry *provider.Registry
	acq      *service.AcquisitionService
	cred     *governance.CredentialService
	dir      string
}

func newAcquisitionHarness(t *testing.T, prober media.Prober) *acquisitionHarness {
	t.Helper()
	tmpDir := t.TempDir()

	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}
	db, err := storage.Open(filepath.Join(tmpDir, "acq_test.db"))
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}

	ingestSvc := service.NewIngestService(db, casStore, prober)
	registry := provider.NewSeam1FakeRegistry()
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)

	initCtx := context.Background()
	for _, p := range registry.ListAll() {
		mName, mVer := p.ModelInfo()
		if mName != "" {
			_ = licSvc.RegisterManifest(initCtx, domain.LicenseManifestEntry{
				DependencyName: mName,
				Version:        mVer,
				SHA256:         "sha256_mock_" + mName,
				SourceRepo:     "github.com/monet88/douyinie/providers/" + mName,
				CodeLicense:    "MIT",
				ModelLicense:   "N/A",
				DataLicense:    "N/A",
				ServiceTerms:   "Standard",
				Verified:       true,
				CreatedAt:      time.Now().UTC(),
			})
		}
	}
	router := provider.NewRouter(registry, polSvc, licSvc, credSvc, nil, db)
	acqSvc := service.NewAcquisitionService(db, ingestSvc, router, tmpDir)

	srv := server.New(server.Config{
		Addr:        "127.0.0.1:0",
		DB:          db,
		CASStore:    casStore,
		Ingest:      ingestSvc,
		Acquisition: acqSvc,
		Registry:    registry,
		PolicySvc:   polSvc,
		LicenseSvc:  licSvc,
		CredSvc:     credSvc,
		Router:      router,
		QueueSvc:    queue.NewService(db),
		Scheduler:   scheduler.New(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = db.Close()
	})

	return &acquisitionHarness{server: ts, db: db, registry: registry, acq: acqSvc, cred: credSvc, dir: tmpDir}
}

func setupAcquisitionHarness(t *testing.T) *acquisitionHarness {
	t.Helper()
	return newAcquisitionHarness(t, &media.MockProber{
		CustomReport: &domain.PreflightReport{
			DurationMs:             1000,
			DurationSec:            1.0,
			ContainerFormat:        "mp4",
			ContainerValid:         true,
			FingerprintMatch:       true,
			NormalizedAudioSHA256:  "mock-normalized-audio-sha",
			NormalizedAudioCASPath: "mock-normalized-audio-cas",
			CreatedAt:              time.Now().UTC(),
		},
	})
}

func (h *acquisitionHarness) acquisitionProvider(t *testing.T, id string) *provider.FakeAcquisitionProvider {
	t.Helper()
	p, ok := h.registry.Get(id)
	if !ok {
		t.Fatalf("provider %s not registered", id)
	}
	ap, ok := p.(*provider.FakeAcquisitionProvider)
	if !ok {
		t.Fatalf("provider %s is not a FakeAcquisitionProvider", id)
	}
	return ap
}

// registerSessionCredential registers a safe env-backed CredentialRef and
// sets the backing env var. The secret value stays in the environment only;
// tests assert it never leaks into persisted provenance or API responses.
func (h *acquisitionHarness) registerSessionCredential(t *testing.T, name, envKey, secret string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":         name,
		"storage_type": "env_ref",
		"key_ref":      envKey,
	})
	resp, err := http.Post(h.server.URL+"/api/v1/credentials", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/credentials failed: status=%d err=%v", resp.StatusCode, err)
	}
	var res struct {
		CredentialRef domain.CredentialRef `json:"credential_ref"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res.CredentialRef.ID == "" {
		t.Fatal("credential ref id missing")
	}
	t.Setenv(envKey, secret)
	return res.CredentialRef.ID
}

func (h *acquisitionHarness) acquire(t *testing.T, payload map[string]any) (*http.Response, service.AcquireResult) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/sources/acquire", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/sources/acquire failed: %v", err)
	}
	var res service.AcquireResult
	if resp.StatusCode == http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			resp.Body.Close()
			t.Fatalf("decode acquire result: %v", err)
		}
	}
	return resp, res
}

func douyinPayload(credIDs ...string) map[string]any {
	p := map[string]any{
		"locator": map[string]string{
			"type":     "douyin_url",
			"location": "https://v.douyin.com/t6U4nCrLIYc/",
		},
		"attestation": map[string]any{
			"attestation_type": "OPERATOR_EXPLICIT_CONFIRMATION",
			"declared_by":      "seam1-acquisition-tester",
			"terms_accepted":   true,
		},
	}
	if len(credIDs) > 0 {
		p["authorized_credentials"] = credIDs
	}
	return p
}

func countRows(t *testing.T, db *storage.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestSeam1_Acquisition_PolicyBeforeHealth proves a healthy acquisition
// adapter with the highest quality score is never selected while policy-
// blocked, and that REQUIRES_AUTHORIZATION adapters stay unselectable until
// a validated credential reference backs them (Issue #28 Seam 1 coverage).
func TestSeam1_Acquisition_PolicyBeforeHealth(t *testing.T) {
	h := setupAcquisitionHarness(t)

	// 1. No credentials: every acquisition adapter is gated (BLOCKED or
	// REQUIRES_AUTHORIZATION) -> routing fails closed, nothing acquires.
	resp, _ := h.acquire(t, douyinPayload())
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422 when no authorized adapter exists, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	if n := countRows(t, h.db, "source_assets"); n != 0 {
		t.Fatalf("blocked acquisition must create no SourceAsset, found %d", n)
	}

	// 2. fake_scrapling_blocked is Healthy=true with the top quality score
	// (0.99) but PolicyState=BLOCKED. With a valid authorized credential,
	// routing must still prefer the Jiji ladder head and NEVER select it.
	credID := h.registerSessionCredential(t, "douyin_session_all", "DOUYINIE_TEST_SESSION_1", "msToken=secret-value-1; ttwid=secret-value-2")

	decBody, _ := json.Marshal(map[string]any{
		"stage":                  "acquisition",
		"authorized_credentials": []string{credID},
	})
	decResp, err := http.Post(h.server.URL+"/api/v1/routing/decide", "application/json", bytes.NewReader(decBody))
	if err != nil {
		t.Fatalf("POST routing/decide failed: %v", err)
	}
	defer decResp.Body.Close()
	if decResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(decResp.Body)
		t.Fatalf("expected 200 with authorized credential, got %d: %s", decResp.StatusCode, body)
	}
	var decision struct {
		SelectedProviderID string                   `json:"selected_provider_id"`
		Decision           domain.SelectionDecision `json:"decision"`
	}
	_ = json.NewDecoder(decResp.Body).Decode(&decision)
	if decision.SelectedProviderID == "fake_scrapling_blocked" {
		t.Fatal("CRITICAL INVARIANT VIOLATION: healthy but policy-blocked acquisition adapter was selected")
	}
	if decision.SelectedProviderID != "fake_jiji_douyin" {
		t.Errorf("expected fake_jiji_douyin (preferred ladder head), got %s", decision.SelectedProviderID)
	}
	var blockedEvaluated, blockedEligible bool
	for _, c := range decision.Decision.CandidatesEvaluated {
		if c.ProviderID == "fake_scrapling_blocked" {
			blockedEvaluated = true
			blockedEligible = c.Eligible
		}
	}
	if !blockedEvaluated || blockedEligible {
		t.Errorf("blocked adapter must be evaluated as ineligible, evaluated=%v eligible=%v", blockedEvaluated, blockedEligible)
	}
}

// TestSeam1_Acquisition_LadderFallback proves the Jiji -> F2 -> browser
// ladder: an ANTI_BOT_OR_EMPTY_RESPONSE on Jiji falls through to the F2
// parser fallback, with immutable ProviderAttempt + append-only
// SelectionDecision provenance for both adapters.
func TestSeam1_Acquisition_LadderFallback(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	jiji.AcquireErr = &domain.AcquisitionError{State: domain.AcquisitionAntiBotOrEmpty, ProviderID: jiji.ProviderID, Detail: "empty http 200 response"}

	credID := h.registerSessionCredential(t, "douyin_session_ladder", "DOUYINIE_TEST_SESSION_LADDER", "session-secret-ladder-value")

	resp, res := h.acquire(t, douyinPayload(credID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 after ladder fallback, got %d: %s", resp.StatusCode, body)
	}

	if res.Asset == nil || res.Asset.SHA256 == "" {
		t.Fatal("acquire must produce a fingerprinted SourceAsset")
	}
	if res.Provenance == nil || res.Provenance.Adapter != "fake_f2_douyin" {
		t.Fatalf("expected provenance adapter fake_f2_douyin, got %+v", res.Provenance)
	}
	// Each ladder candidate probes before acquiring: jiji probe+acquire
	// (acquire failed), f2 probe+acquire (success).
	if strings.Join(jiji.Calls, ",") != "probe,acquire" {
		t.Errorf("expected jiji probe+acquire, got %v", jiji.Calls)
	}
	if strings.Join(f2.Calls, ",") != "probe,acquire" {
		t.Errorf("expected f2 probe+acquire, got %v", f2.Calls)
	}

	attempts, err := h.db.ListProviderAttempts(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	var jijiFailed, f2Succeeded bool
	for _, a := range attempts {
		switch {
		case a.ProviderID == "fake_jiji_douyin" && a.Status == "failed":
			jijiFailed = true
		case a.ProviderID == "fake_f2_douyin" && a.Status == "succeeded":
			f2Succeeded = true
		}
	}
	if !jijiFailed || !f2Succeeded {
		t.Errorf("expected jiji failed + f2 succeeded attempts, got %+v", attempts)
	}

	decisions, err := h.db.ListSelectionDecisions(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	if len(decisions) < 2 {
		t.Fatalf("expected append-only initial + alternate decisions, got %d", len(decisions))
	}
	// Stage-only listing is newest-first; the alternate-candidate decision
	// must be the latest append.
	if decisions[0].SelectedProviderID != "fake_f2_douyin" {
		t.Errorf("latest decision must record the alternate adapter, got %s", decisions[0].SelectedProviderID)
	}
}

// TestSeam1_Acquisition_ContentFingerprintDedupAcrossSourceIDs proves the
// Issue #28 criterion "equivalent reacquisition reuses source-derived
// artifacts via content fingerprint": two distinct canonical source ids whose
// adapters produce byte-identical media resolve to the SAME immutable
// SourceAsset (CAS SHA-256 dedup), not a duplicate asset.
func TestSeam1_Acquisition_ContentFingerprintDedupAcrossSourceIDs(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")

	const sharedBytes = "IDENTICAL_MEDIA_BYTES_ACROSS_SOURCES"
	jiji.MediaBytes = []byte(sharedBytes)
	f2.MediaBytes = []byte(sharedBytes)

	credID := h.registerSessionCredential(t, "douyin_session_fp", "DOUYINIE_TEST_SESSION_FP", "fp-dedup-secret")

	respA, resA := h.acquire(t, douyinPayload(credID))
	if respA.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(respA.Body)
		t.Fatalf("acquire A failed: %d %s", respA.StatusCode, body)
	}
	respA.Body.Close()

	// Different locator -> different canonical source id, so the identity
	// fast-path cannot hit; the ladder runs again for source B.
	payloadB := map[string]any{
		"locator": map[string]string{
			"type":     "douyin_url",
			"location": "https://www.douyin.com/video/7777888899990000111",
		},
		"attestation": map[string]any{
			"attestation_type": "OPERATOR_EXPLICIT_CONFIRMATION",
			"declared_by":      "seam1-acquisition-tester",
			"terms_accepted":   true,
		},
		"authorized_credentials": []string{credID},
	}
	// Route B through the F2 adapter (Jiji fails structurally at acquire) so
	// the second acquisition is a genuinely separate ladder run.
	jiji.AcquireErr = &domain.AcquisitionError{State: domain.AcquisitionAntiBotOrEmpty, ProviderID: jiji.ProviderID, Detail: "empty response"}

	respB, resB := h.acquire(t, payloadB)
	if respB.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(respB.Body)
		t.Fatalf("acquire B failed: %d %s", respB.StatusCode, body)
	}
	respB.Body.Close()

	if resA.Asset == nil || resB.Asset == nil {
		t.Fatal("both acquisitions must produce assets")
	}
	if resA.Asset.SHA256 != resB.Asset.SHA256 {
		t.Fatalf("identical bytes must share a content fingerprint: %s vs %s", resA.Asset.SHA256, resB.Asset.SHA256)
	}
	// Content-fingerprint dedup: byte-identical media from a different source
	// id resolves to the SAME immutable SourceAsset row.
	if resB.Asset.ID != resA.Asset.ID {
		t.Errorf("content-fingerprint dedup must reuse the existing SourceAsset, got %s vs %s", resA.Asset.ID, resB.Asset.ID)
	}
	if n := countRows(t, h.db, "source_assets"); n != 1 {
		t.Errorf("expected exactly 1 SourceAsset after fingerprint dedup, found %d", n)
	}
	// The ladder really re-ran for source B (identity fast-path missed):
	// f2 probed then acquired.
	if strings.Join(f2.Calls, ",") != "probe,acquire" {
		t.Errorf("expected f2 probe+acquire for source B, got %v", f2.Calls)
	}
}

// TestSeam1_Acquisition_ContentUnavailableFailsClosed proves CONTENT_UNAVAILABLE
// is a hard block: the ladder never falls through to another adapter and the
// state is surfaced structurally.
func TestSeam1_Acquisition_ContentUnavailableFailsClosed(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	browser := h.acquisitionProvider(t, "fake_browser_assist")
	jiji.AcquireErr = &domain.AcquisitionError{State: domain.AcquisitionContentUnavailable, ProviderID: jiji.ProviderID, Detail: "video removed"}

	credID := h.registerSessionCredential(t, "douyin_session_hard", "DOUYINIE_TEST_SESSION_HARD", "session-secret-hard-value")

	resp, _ := h.acquire(t, douyinPayload(credID))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 fail-closed, got %d: %s", resp.StatusCode, body)
	}
	var errRes map[string]any
	_ = json.Unmarshal(body, &errRes)
	if errRes["acquisition_state"] != "CONTENT_UNAVAILABLE" {
		t.Errorf("expected structural CONTENT_UNAVAILABLE state, got %v", errRes["acquisition_state"])
	}

	if len(f2.Calls) != 0 || len(browser.Calls) != 0 {
		t.Errorf("CONTENT_UNAVAILABLE must not fall through the ladder, f2=%v browser=%v", f2.Calls, browser.Calls)
	}
	if n := countRows(t, h.db, "source_assets"); n != 0 {
		t.Errorf("failed acquisition must create no SourceAsset, found %d", n)
	}

	// Fail-closed provenance: the rejection is recorded as policy_rejected,
	// never retried past policy.
	attempts, err := h.db.ListProviderAttempts(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	for _, a := range attempts {
		if a.ProviderID == "fake_jiji_douyin" && a.Status != "policy_rejected" {
			t.Errorf("CONTENT_UNAVAILABLE attempt must be policy_rejected, got %s", a.Status)
		}
	}
}

// TestSeam1_Acquisition_IntegrityFallsBackToHealthyProvider is the Issue #4
// regression: corrupt bytes from the ladder head (Jiji) must NOT surface
// INTEGRITY_FAILED immediately. The candidate-integrity check runs inside the
// ladder step, so the bad candidate fails as quality_failed and the Router
// advances to the next independent media provider, which succeeds.
func TestSeam1_Acquisition_IntegrityFallsBackToHealthyProvider(t *testing.T) {
	// A prober that rejects only the bytes produced under the jiji candidate
	// directory (the ladder writes one directory per candidate) and accepts
	// everything else, with the normalized-audio identity pre-set so no
	// ffmpeg is required.
	h := newAcquisitionHarness(t, &candidateSplitProber{jijiCorrupt: true})

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")

	credID := h.registerSessionCredential(t, "douyin_session_integrity_fb", "DOUYINIE_TEST_SESSION_INTEGRITY_FB", "integrity-fallback-secret")

	resp, res := h.acquire(t, douyinPayload(credID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("corrupt jiji media must fall back to f2, got %d: %s", resp.StatusCode, body)
	}
	if res.Provenance == nil || res.Provenance.Adapter != "fake_f2_douyin" {
		t.Fatalf("expected provenance adapter fake_f2_douyin after integrity fallback, got %+v", res.Provenance)
	}
	if strings.Join(jiji.Calls, ",") != "probe,acquire" || strings.Join(f2.Calls, ",") != "probe,acquire" {
		t.Errorf("expected jiji probe+acquire then f2 probe+acquire, got jiji=%v f2=%v", jiji.Calls, f2.Calls)
	}

	attempts, err := h.db.ListProviderAttempts(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	var jijiQualityFailed, f2Succeeded bool
	for _, a := range attempts {
		switch {
		case a.ProviderID == "fake_jiji_douyin" && a.Status == "quality_failed":
			jijiQualityFailed = true
		case a.ProviderID == "fake_f2_douyin" && a.Status == "succeeded":
			f2Succeeded = true
		}
	}
	if !jijiQualityFailed || !f2Succeeded {
		t.Errorf("expected jiji quality_failed + f2 succeeded attempts, got %+v", attempts)
	}
	if n := countRows(t, h.db, "source_assets"); n != 1 {
		t.Errorf("fallback success must create exactly one SourceAsset, found %d", n)
	}
}

// TestSeam1_Acquisition_IntegrityFailedOnlyAfterLadderExhaustion proves
// INTEGRITY_FAILED is terminal only after every independent provider produced
// bad bytes: with a globally corrupt prober the whole Jiji -> F2 -> browser
// ladder runs, each candidate fails integrity, and only then does the
// structural state surface.
func TestSeam1_Acquisition_IntegrityFailedOnlyAfterLadderExhaustion(t *testing.T) {
	h := newAcquisitionHarness(t, &media.MockProber{
		CustomReport: &domain.PreflightReport{
			ContainerValid:   false,
			FingerprintMatch: true,
			Errors:           []string{"moov atom not found"},
		},
	})

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	browser := h.acquisitionProvider(t, "fake_browser_assist")

	credID := h.registerSessionCredential(t, "douyin_session_integrity", "DOUYINIE_TEST_SESSION_INTEGRITY", "session-secret-integrity")

	resp, _ := h.acquire(t, douyinPayload(credID))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 INTEGRITY_FAILED, got %d: %s", resp.StatusCode, body)
	}
	var errRes map[string]any
	_ = json.Unmarshal(body, &errRes)
	if errRes["acquisition_state"] != "INTEGRITY_FAILED" {
		t.Errorf("expected structural INTEGRITY_FAILED state, got %v (body: %s)", errRes["acquisition_state"], body)
	}
	// Issue #4: the terminal state is only allowed after every independent
	// media option was tried — each candidate probes then acquires.
	if strings.Join(jiji.Calls, ",") != "probe,acquire" || strings.Join(f2.Calls, ",") != "probe,acquire" || strings.Join(browser.Calls, ",") != "probe,acquire" {
		t.Errorf("INTEGRITY_FAILED must exhaust the ladder, got jiji=%v f2=%v browser=%v", jiji.Calls, f2.Calls, browser.Calls)
	}
	if n := countRows(t, h.db, "source_assets"); n != 0 {
		t.Errorf("INTEGRITY_FAILED must create no SourceAsset, found %d", n)
	}
}

// TestSeam1_Acquisition_DownloadFailedMapsTo502 proves a transient
// DOWNLOAD_FAILED that exhausts the ladder surfaces structurally and maps to
// the transport's 502 bad-gateway class (not a generic 500).
func TestSeam1_Acquisition_DownloadFailedMapsTo502(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	browser := h.acquisitionProvider(t, "fake_browser_assist")
	fail := func(p *provider.FakeAcquisitionProvider) {
		p.AcquireErr = &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: p.ProviderID, Detail: "network reset"}
	}
	fail(jiji)
	fail(f2)
	fail(browser)

	credID := h.registerSessionCredential(t, "douyin_session_dl502", "DOUYINIE_TEST_SESSION_DL502", "download-failed-secret")

	resp, _ := h.acquire(t, douyinPayload(credID))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 for exhausted DOWNLOAD_FAILED ladder, got %d: %s", resp.StatusCode, body)
	}
	var errRes map[string]any
	_ = json.Unmarshal(body, &errRes)
	if errRes["acquisition_state"] != "DOWNLOAD_FAILED" {
		t.Errorf("expected structural DOWNLOAD_FAILED state, got %v", errRes["acquisition_state"])
	}
	if n := countRows(t, h.db, "source_assets"); n != 0 {
		t.Errorf("failed downloads must create no SourceAsset, found %d", n)
	}
}

// TestSeam1_Acquisition_FingerprintDedup proves an equivalent reacquisition
// of the same canonical source resolves to the existing content-fingerprinted
// SourceAsset without re-running the adapter ladder.
func TestSeam1_Acquisition_FingerprintDedup(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	credID := h.registerSessionCredential(t, "douyin_session_dedup", "DOUYINIE_TEST_SESSION_DEDUP", "session-secret-dedup")

	resp1, res1 := h.acquire(t, douyinPayload(credID))
	if resp1.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("first acquire failed: %d %s", resp1.StatusCode, body)
	}
	resp1.Body.Close()
	if res1.Reused {
		t.Fatal("first acquisition must not report reuse")
	}

	resp2, res2 := h.acquire(t, douyinPayload(credID))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("second acquire status %d", resp2.StatusCode)
	}
	if !res2.Reused {
		t.Error("equivalent reacquisition must report Reused=true")
	}
	if res2.Asset == nil || res1.Asset == nil || res2.Asset.ID != res1.Asset.ID {
		t.Fatalf("reacquisition must resolve to the same immutable SourceAsset, got %v vs %v", res1.Asset, res2.Asset)
	}
	// First call: probe+acquire. Second call: probe only (dedup hit, no
	// re-download).
	if strings.Join(jiji.Calls, ",") != "probe,acquire,probe" {
		t.Errorf("expected second acquisition to skip the ladder, got %v", jiji.Calls)
	}
	if n := countRows(t, h.db, "source_assets"); n != 1 {
		t.Errorf("dedup must create exactly one SourceAsset, found %d", n)
	}
}

// TestSeam1_Acquisition_SessionSecretsLocalOnly proves raw session material
// never reaches persisted provenance, attempts, decisions, or API responses —
// only safe credential references are recorded.
func TestSeam1_Acquisition_SessionSecretsLocalOnly(t *testing.T) {
	h := setupAcquisitionHarness(t)

	const secret = "msToken=SUP3RSECR3TVALUE; ttwid=AN0THERSECR3T"
	credID := h.registerSessionCredential(t, "douyin_session_priv", "DOUYINIE_TEST_SESSION_PRIV", secret)

	resp, res := h.acquire(t, douyinPayload(credID))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("acquire failed: %d %s", resp.StatusCode, body)
	}
	respBytes, _ := json.Marshal(res)
	resp.Body.Close()
	if strings.Contains(string(respBytes), secret) {
		t.Error("acquire response leaked raw session secret")
	}
	if res.Provenance == nil || res.Provenance.AuthRefID != credID {
		t.Errorf("provenance must carry only the safe credential reference, got %+v", res.Provenance)
	}
	if !res.Provenance.Authenticated {
		t.Error("authenticated boolean must be recorded")
	}

	// Scan every persisted provenance surface for the secret.
	prov, err := h.db.GetSourceAcquisitionBySourceID(context.Background(), res.Provenance.SourceID)
	if err != nil {
		t.Fatalf("get source acquisition: %v", err)
	}
	provBytes, _ := json.Marshal(prov)
	if strings.Contains(string(provBytes), secret) {
		t.Error("raw session secret leaked into source_acquisitions provenance")
	}

	attempts, err := h.db.ListProviderAttempts(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	attemptBytes, _ := json.Marshal(attempts)
	if strings.Contains(string(attemptBytes), secret) {
		t.Error("raw session secret leaked into provider_attempts")
	}

	decisions, err := h.db.ListSelectionDecisions(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	decisionBytes, _ := json.Marshal(decisions)
	if strings.Contains(string(decisionBytes), secret) {
		t.Error("raw session secret leaked into selection_decisions")
	}
}

// TestSeam1_Acquisition_ProbeCreatesNoAsset proves probe resolves the
// canonical descriptor through a policy-gated adapter without creating a
// durable SourceAsset (Issue #17: probe is not acquisition).
func TestSeam1_Acquisition_ProbeCreatesNoAsset(t *testing.T) {
	h := setupAcquisitionHarness(t)
	credID := h.registerSessionCredential(t, "douyin_session_probe", "DOUYINIE_TEST_SESSION_PROBE", "probe-session-secret")

	body, _ := json.Marshal(douyinPayload(credID))
	resp, err := http.Post(h.server.URL+"/api/v1/sources/probe", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/sources/probe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}
	var res struct {
		Descriptor domain.SourceDescriptor `json:"descriptor"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if !strings.HasPrefix(res.Descriptor.SourceID, "douyin:aweme:") {
		t.Errorf("probe must return a canonical source id, got %q", res.Descriptor.SourceID)
	}
	if n := countRows(t, h.db, "source_assets"); n != 0 {
		t.Errorf("probe must not create SourceAssets, found %d", n)
	}
	if n := countRows(t, h.db, "source_acquisitions"); n != 0 {
		t.Errorf("probe must not persist acquisition provenance, found %d", n)
	}
}

// TestSeam1_Acquisition_AuthLadderBrowserLast proves AUTH_REQUIRED and
// SESSION_EXPIRED advance the ladder to the browser-assisted adapter, which
// runs last and records the browser method.
func TestSeam1_Acquisition_AuthLadderBrowserLast(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	browser := h.acquisitionProvider(t, "fake_browser_assist")
	// Whole ladder auth-blocked until the browser-assisted path: Jiji and F2
	// report AUTH_REQUIRED / SESSION_EXPIRED, browser-assist succeeds last.
	jiji.AcquireErr = &domain.AcquisitionError{State: domain.AcquisitionAuthRequired, ProviderID: jiji.ProviderID}
	f2.AcquireErr = &domain.AcquisitionError{State: domain.AcquisitionSessionExpired, ProviderID: f2.ProviderID}
	browser.Method = "browser"

	credID := h.registerSessionCredential(t, "douyin_session_auth", "DOUYINIE_TEST_SESSION_AUTH", "auth-ladder-secret")

	resp, res := h.acquire(t, douyinPayload(credID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("browser-assisted last-resort must complete the ladder, got %d: %s", resp.StatusCode, body)
	}
	if res.Provenance == nil || res.Provenance.Adapter != "fake_browser_assist" {
		t.Fatalf("expected browser-assist to win the auth ladder last, got %+v", res.Provenance)
	}
	if res.Provenance.Method != "browser" || !res.Provenance.Authenticated {
		t.Errorf("browser method + authenticated flag must be recorded, got %+v", res.Provenance)
	}
	if strings.Join(browser.Calls, ",") != "probe,acquire" {
		t.Errorf("browser adapter must run exactly once, got %v", browser.Calls)
	}
}

// TestSeam1_Acquisition_ProbeLevelFallback proves the probe participates in
// the provider ladder (Issue #4 routing rules): a fallback-eligible probe
// failure (ANTI_BOT_OR_EMPTY_RESPONSE) on the Jiji head advances to the F2
// candidate, whose probe+acquire complete the run and whose descriptor feeds
// dedup/provenance.
func TestSeam1_Acquisition_ProbeLevelFallback(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	jiji.ProbeErr = &domain.AcquisitionError{State: domain.AcquisitionAntiBotOrEmpty, ProviderID: jiji.ProviderID, Detail: "empty http 200 on probe"}

	credID := h.registerSessionCredential(t, "douyin_session_probefb", "DOUYINIE_TEST_SESSION_PROBEFB", "probe-fallback-secret")

	resp, res := h.acquire(t, douyinPayload(credID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("fallback-eligible probe failure must advance the ladder, got %d: %s", resp.StatusCode, body)
	}
	if res.Provenance == nil || res.Provenance.Adapter != "fake_f2_douyin" {
		t.Fatalf("expected f2 to win after jiji probe failure, got %+v", res.Provenance)
	}
	// Jiji failed at probe (never acquired); f2 probed then acquired.
	if strings.Join(jiji.Calls, ",") != "probe" {
		t.Errorf("jiji must stop at probe, got %v", jiji.Calls)
	}
	if strings.Join(f2.Calls, ",") != "probe,acquire" {
		t.Errorf("expected f2 probe+acquire, got %v", f2.Calls)
	}

	attempts, err := h.db.ListProviderAttempts(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	var jijiProbeFailed, f2Succeeded bool
	for _, a := range attempts {
		switch {
		case a.ProviderID == "fake_jiji_douyin" && a.Status == "failed":
			jijiProbeFailed = true
		case a.ProviderID == "fake_f2_douyin" && a.Status == "succeeded":
			f2Succeeded = true
		}
	}
	if !jijiProbeFailed || !f2Succeeded {
		t.Errorf("expected jiji probe failed + f2 succeeded attempts, got %+v", attempts)
	}
}

// TestSeam1_Acquisition_ProbeHardStopsFailClosed proves probe-level hard
// blocks never fall through: CONTENT_UNAVAILABLE stays a hard block, and a
// gallery/live classification surfaces distinctly as UNSUPPORTED_MEDIA_TYPE
// (resolved Issue #4 V1 media policy) — not silently as CONTENT_UNAVAILABLE —
// before any expensive acquire.
func TestSeam1_Acquisition_ProbeHardStopsFailClosed(t *testing.T) {
	t.Run("content-unavailable", func(t *testing.T) {
		h := setupAcquisitionHarness(t)
		jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
		f2 := h.acquisitionProvider(t, "fake_f2_douyin")
		jiji.ProbeErr = &domain.AcquisitionError{State: domain.AcquisitionContentUnavailable, ProviderID: jiji.ProviderID, Detail: "url returned http 404"}

		credID := h.registerSessionCredential(t, "douyin_session_probehard", "DOUYINIE_TEST_SESSION_PROBEHARD", "probe-hard-secret")
		resp, _ := h.acquire(t, douyinPayload(credID))
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 fail-closed, got %d: %s", resp.StatusCode, body)
		}
		var errRes map[string]any
		_ = json.Unmarshal(body, &errRes)
		if errRes["acquisition_state"] != "CONTENT_UNAVAILABLE" {
			t.Errorf("expected CONTENT_UNAVAILABLE, got %v", errRes["acquisition_state"])
		}
		if len(f2.Calls) != 0 {
			t.Errorf("probe CONTENT_UNAVAILABLE must not fall through, f2=%v", f2.Calls)
		}
	})

	t.Run("gallery-rejected-distinctly", func(t *testing.T) {
		h := setupAcquisitionHarness(t)
		jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
		f2 := h.acquisitionProvider(t, "fake_f2_douyin")
		jiji.Descriptor = &domain.SourceDescriptor{
			SourceID: "douyin:aweme:7300000000000000001", Platform: "douyin",
			CanonicalURL: "https://www.douyin.com/note/7300000000000000001", MediaType: "gallery",
		}

		credID := h.registerSessionCredential(t, "douyin_session_gallery", "DOUYINIE_TEST_SESSION_GALLERY", "gallery-secret")
		resp, _ := h.acquire(t, douyinPayload(credID))
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("gallery must be rejected with 422, got %d: %s", resp.StatusCode, body)
		}
		var errRes map[string]any
		_ = json.Unmarshal(body, &errRes)
		if errRes["acquisition_state"] != "UNSUPPORTED_MEDIA_TYPE" {
			t.Errorf("gallery must surface UNSUPPORTED_MEDIA_TYPE, got %v (body: %s)", errRes["acquisition_state"], body)
		}
		// Rejected before expensive work: no acquire ran, ladder never advanced.
		if strings.Join(jiji.Calls, ",") != "probe" {
			t.Errorf("gallery must be rejected before acquire, got %v", jiji.Calls)
		}
		if len(f2.Calls) != 0 {
			t.Errorf("UNSUPPORTED_MEDIA_TYPE must not fall through, f2=%v", f2.Calls)
		}
		if n := countRows(t, h.db, "source_assets"); n != 0 {
			t.Errorf("rejected gallery must create no SourceAsset, found %d", n)
		}
	})
}

// TestSeam1_Acquisition_InvalidLocatorMapsTo400 proves invalid client input
// (missing or malformed locator) maps to a stable 400 on both endpoints —
// never a 500 and never mislabeled CONTENT_UNAVAILABLE.
func TestSeam1_Acquisition_InvalidLocatorMapsTo400(t *testing.T) {
	h := setupAcquisitionHarness(t)

	for _, tc := range []struct {
		name     string
		location string
	}{
		{"missing", ""},
		{"malformed", "not-a-url"},
	} {
		for _, ep := range []string{"/api/v1/sources/probe", "/api/v1/sources/acquire"} {
			payload := douyinPayload()
			payload["locator"] = map[string]string{"type": "douyin_url", "location": tc.location}
			body, _ := json.Marshal(payload)
			resp, err := http.Post(h.server.URL+ep, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST %s: %v", ep, err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s/%s: expected 400, got %d: %s", ep, tc.name, resp.StatusCode, raw)
			}
			if strings.Contains(string(raw), "CONTENT_UNAVAILABLE") {
				t.Errorf("%s/%s: invalid input must not be labeled CONTENT_UNAVAILABLE: %s", ep, tc.name, raw)
			}
		}
	}
	// Unsupported locator type is also invalid input, not content gone.
	payload := douyinPayload()
	payload["locator"] = map[string]string{"type": "local_file", "location": "https://www.douyin.com/video/7300000000000000000"}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(h.server.URL+"/api/v1/sources/acquire", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST acquire: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("local_file locator must be 400, got %d: %s", resp.StatusCode, raw)
	}
	if n := countRows(t, h.db, "source_assets"); n != 0 {
		t.Errorf("invalid input must create no SourceAsset, found %d", n)
	}
}

// TestSeam1_Acquisition_MimeTypeReflectsProducedFile proves the acquired
// asset's mime type follows the container the winning adapter actually
// produced (.webm here), not a hardcoded video/mp4.
func TestSeam1_Acquisition_MimeTypeReflectsProducedFile(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	jiji.AcquireErr = &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: jiji.ProviderID, Detail: "network reset"}
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	f2.MediaExt = ".webm"

	credID := h.registerSessionCredential(t, "douyin_session_mime", "DOUYINIE_TEST_SESSION_MIME", "mime-secret")
	resp, res := h.acquire(t, douyinPayload(credID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("acquire failed: %d %s", resp.StatusCode, body)
	}
	if res.Asset == nil {
		t.Fatal("expected asset")
	}
	if res.Asset.MimeType == "video/mp4" {
		t.Errorf("asset mime must reflect the produced .webm file, got hardcoded %s", res.Asset.MimeType)
	}
}

// TestSeam1_Acquisition_CredentialMisconfigFailsClosedNoFallback is the
// final-review regression: a permanent governance/auth sentinel
// (domain.ErrAuthRequired wrapped by credential/config validation, e.g.
// CredentialService.MaterializeSecret) must NOT be relabeled as the
// fallback-eligible structural AUTH_REQUIRED. It stops fail-closed at the
// first provider — no F2, no browser-assist — and errors.Is(ErrAuthRequired)
// still matches through the transport mapping.
func TestSeam1_Acquisition_CredentialMisconfigFailsClosedNoFallback(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	browser := h.acquisitionProvider(t, "fake_browser_assist")
	// Permanent misconfiguration, not a provider-observed auth challenge.
	jiji.SecretErr = fmt.Errorf("%w: backing credential validation failed", domain.ErrAuthRequired)

	credID := h.registerSessionCredential(t, "douyin_session_misconfig", "DOUYINIE_TEST_SESSION_MISCONFIG", "misconfig-secret")

	body, _ := json.Marshal(douyinPayload(credID))
	resp, err := http.Post(h.server.URL+"/api/v1/sources/acquire", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST acquire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422 fail-closed, got %d: %s", resp.StatusCode, b)
	}
	var errRes map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&errRes)
	if _, structural := errRes["acquisition_state"]; structural {
		t.Errorf("permanent governance sentinel must not surface as a structural acquisition state, got %v", errRes["acquisition_state"])
	}
	if !strings.Contains(fmt.Sprint(errRes["error"]), domain.ErrAuthRequired.Error()) {
		t.Errorf("errors.Is(ErrAuthRequired) sentinel text must survive to the transport, got %v", errRes["error"])
	}

	// No ladder fallthrough: only the head adapter ran, and it stopped at acquire.
	if strings.Join(jiji.Calls, ",") != "probe,acquire" {
		t.Errorf("jiji must run probe+acquire then stop, got %v", jiji.Calls)
	}
	if len(f2.Calls) != 0 || len(browser.Calls) != 0 {
		t.Errorf("credential misconfiguration must not fall back to F2/browser, f2=%v browser=%v", f2.Calls, browser.Calls)
	}

	attempts, err := h.db.ListProviderAttempts(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	for _, a := range attempts {
		if a.ProviderID == "fake_jiji_douyin" && a.Status != "policy_rejected" {
			t.Errorf("permanent auth sentinel attempt must be policy_rejected, got %s", a.Status)
		}
	}
}

// TestSeam1_Acquisition_ForceRefreshPreservesJobBoundAsset proves force_refresh
// may repoint the canonical source_acquisitions binding to a newer asset but
// never rewrites or deletes an immutable SourceAsset already referenced by a
// LocalizationJob (Issue #28 provenance immutability).
func TestSeam1_Acquisition_ForceRefreshPreservesJobBoundAsset(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	credID := h.registerSessionCredential(t, "douyin_session_refresh", "DOUYINIE_TEST_SESSION_REFRESH", "refresh-secret")

	resp1, res1 := h.acquire(t, douyinPayload(credID))
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated || res1.Asset == nil {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("first acquire failed: %d %s", resp1.StatusCode, body)
	}
	oldAssetID := res1.Asset.ID
	oldSHA := res1.Asset.SHA256
	sourceID := res1.Provenance.SourceID

	// Bind a LocalizationJob to the acquired asset (existing API seam).
	jobBody, _ := json.Marshal(map[string]string{"source_asset_id": oldAssetID, "target_language": domain.TargetLanguageVI})
	jobResp, err := http.Post(h.server.URL+"/api/v1/jobs", "application/json", bytes.NewReader(jobBody))
	if err != nil || jobResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/jobs failed: status=%d err=%v", jobResp.StatusCode, err)
	}
	var jobResult struct {
		Job domain.LocalizationJob `json:"job"`
	}
	_ = json.NewDecoder(jobResp.Body).Decode(&jobResult)
	jobResp.Body.Close()
	if jobResult.Job.SourceAssetID != oldAssetID {
		t.Fatalf("job must bind the acquired asset, got %s", jobResult.Job.SourceAssetID)
	}

	// force_refresh with different bytes: the canonical binding may move...
	jiji.MediaBytes = []byte("FAKE_DOUYIN_MEDIA_PAYLOAD_refreshed_v2")
	payload := douyinPayload(credID)
	payload["force_refresh"] = true
	resp2, res2 := h.acquire(t, payload)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated || res2.Asset == nil {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("force_refresh acquire failed: %d %s", resp2.StatusCode, body)
	}
	if res2.Asset.ID == oldAssetID {
		t.Fatal("force_refresh with new bytes must produce a new asset")
	}

	// ...but the job's binding and the old immutable asset are untouched.
	job, err := h.db.GetJob(context.Background(), jobResult.Job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.SourceAssetID != oldAssetID {
		t.Errorf("force_refresh must not alter LocalizationJob.SourceAssetID, got %s", job.SourceAssetID)
	}
	oldAsset, err := h.db.GetSourceAsset(context.Background(), oldAssetID)
	if err != nil {
		t.Fatalf("job-bound SourceAsset must survive force_refresh: %v", err)
	}
	if oldAsset.SHA256 != oldSHA {
		t.Errorf("immutable SourceAsset bytes overwritten: %s -> %s", oldSHA, oldAsset.SHA256)
	}

	// The canonical binding now points at the refreshed asset.
	prior, err := h.db.GetSourceAcquisitionBySourceID(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("get provenance: %v", err)
	}
	if prior.AssetID != res2.Asset.ID {
		t.Errorf("force_refresh must update the canonical binding, got %s", prior.AssetID)
	}
}

// TestSeam1_Acquisition_CanonicalReuseFailureFailsClosed proves that when
// canonical reuse resolution fails (missing DB asset, missing preflight report,
// missing rights attestation, or missing CAS bytes for a recorded provenance
// row), the acquisition stage fails closed immediately (CODING_STANDARDS §10).
// It must NOT fall through to alternate providers (F2, browser assist) or retry
// transiently, and must record a policy_rejected attempt rather than treating
// persistence/CAS inconsistency as a provider transient fault.
func TestSeam1_Acquisition_CanonicalReuseFailureFailsClosed(t *testing.T) {
	h := setupAcquisitionHarness(t)

	jiji := h.acquisitionProvider(t, "fake_jiji_douyin")
	f2 := h.acquisitionProvider(t, "fake_f2_douyin")
	browser := h.acquisitionProvider(t, "fake_browser_assist")
	credID := h.registerSessionCredential(t, "douyin_session_inconsistency", "DOUYINIE_TEST_SESSION_INCONSISTENCY", "secret-inconsistency")

	// 1. First acquisition succeeds through the ladder head.
	resp1, res1 := h.acquire(t, douyinPayload(credID))
	if resp1.StatusCode != http.StatusCreated || res1.Asset == nil {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("initial acquire failed: %d %s", resp1.StatusCode, body)
	}
	resp1.Body.Close()

	// Remove the underlying media file in CAS to force a canonical reuse failure.
	if err := os.Remove(res1.Asset.CASPath); err != nil {
		t.Fatalf("failed to remove CAS file %s: %v", res1.Asset.CASPath, err)
	}

	// Reset call counters on all providers.
	jiji.Calls = nil
	f2.Calls = nil
	browser.Calls = nil

	// 2. Re-acquire the same source URL.
	resp2, _ := h.acquire(t, douyinPayload(credID))
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	// Must fail closed with 422 Unprocessable Entity (not 500, not 502 bad gateway).
	if resp2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 fail-closed on canonical reuse inconsistency, got %d: %s", resp2.StatusCode, body2)
	}

	// Alternate providers MUST NOT have run (no fallback to F2 or browser assist).
	if len(f2.Calls) != 0 {
		t.Errorf("expected no calls to f2 on canonical reuse failure, got %v", f2.Calls)
	}
	if len(browser.Calls) != 0 {
		t.Errorf("expected no calls to browser assist on canonical reuse failure, got %v", browser.Calls)
	}
	// Jiji probed to identify sourceID, then failed closed without calling acquire.
	if strings.Join(jiji.Calls, ",") != "probe" {
		t.Errorf("expected jiji to only probe before failing closed, got %v", jiji.Calls)
	}

	// Prove ProviderAttempt status is recorded as policy_rejected (fail-closed, no retries).
	attempts, err := h.db.ListProviderAttempts(context.Background(), "", "acquisition")
	if err != nil {
		t.Fatalf("list provider attempts: %v", err)
	}
	if len(attempts) < 2 {
		t.Fatalf("expected at least 2 provider attempts recorded, got %d", len(attempts))
	}
	// Initial acquire succeeded (attempt 1); second acquire failed closed (attempt 2).
	var foundRejected bool
	for _, a := range attempts {
		if a.ProviderID == "fake_jiji_douyin" && a.Status == "policy_rejected" {
			foundRejected = true
			if !strings.Contains(a.ErrorMessage, domain.ErrInconsistentProvenance.Error()) {
				t.Errorf("expected attempt error message to contain ErrInconsistentProvenance, got: %s", a.ErrorMessage)
			}
		}
	}
	if !foundRejected {
		t.Error("expected at least one attempt with status policy_rejected for fake_jiji_douyin")
	}
}
