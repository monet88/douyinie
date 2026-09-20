package seam1_test

import (
	"bytes"
	"context"
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
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/queue"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/server"
	"github.com/monet88/douyinie/internal/service"
	"github.com/monet88/douyinie/internal/storage"
)

const seam1PageHelperEnv = "DOUYINIE_SEAM1_PAGE_HELPER"

// TestMain lets the Seam 1 test binary double as the external page-backed
// helper. Production still crosses the real subprocess boundary; only the
// remote Douyin page itself is replaced by a deterministic test executable.
func TestMain(m *testing.M) {
	if os.Getenv(seam1PageHelperEnv) == "1" {
		os.Exit(runSeam1PageHelper())
	}
	os.Exit(m.Run())
}

func runSeam1PageHelper() int {
	var dest, credentialFile string
	for i, arg := range os.Args {
		switch arg {
		case "--dest":
			if i+1 < len(os.Args) {
				dest = os.Args[i+1]
			}
		case "--credential-file":
			if i+1 < len(os.Args) {
				credentialFile = os.Args[i+1]
			}
		}
	}
	if dest == "" || credentialFile == "" {
		return 2
	}
	if _, err := os.Stat(credentialFile); err != nil {
		return 3
	}
	if callsPath := os.Getenv("DOUYINIE_SEAM1_PAGE_HELPER_CALLS"); callsPath != "" {
		f, err := os.OpenFile(callsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return 4
		}
		_, _ = f.WriteString("call\n")
		_ = f.Close()
	}
	if err := os.WriteFile(filepath.Join(dest, "page-backed.mp4"), []byte("SEAM1_PAGE_BACKED_MEDIA"), 0600); err != nil {
		return 5
	}
	return 0
}

func setupArgusAcquisitionHarness(t *testing.T, directJiji, pageBacked bool) *acquisitionHarness {
	t.Helper()
	tmpDir := t.TempDir()
	casStore, err := cas.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("setup CAS store: %v", err)
	}
	db, err := storage.Open(filepath.Join(tmpDir, "argus_acquisition_test.db"))
	if err != nil {
		t.Fatalf("setup SQLite: %v", err)
	}
	prober := &media.MockProber{CustomReport: &domain.PreflightReport{
		ID:                     uuid.NewString(),
		DurationMs:             1000,
		DurationSec:            1,
		VideoCodec:             "h264",
		AudioCodec:             "aac",
		Width:                  1080,
		Height:                 1920,
		FrameRate:              30,
		ContainerFormat:        "mp4",
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  "argus-seam1-normalized-audio-sha",
		NormalizedAudioCASPath: "argus-seam1-normalized-audio-cas",
		CreatedAt:              time.Now().UTC(),
	}}
	ingestSvc := service.NewIngestService(db, casStore, prober)
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	registry := provider.NewRegistry()
	if directJiji {
		if err := registry.Register(provider.NewJijiAdapter("vtest", os.Args[0], "unused-script", credSvc.MaterializeSecret)); err != nil {
			t.Fatalf("register direct Jiji: %v", err)
		}
	}
	if pageBacked {
		if err := registry.Register(provider.NewPageBackedJijiAdapter("vtest", os.Args[0], credSvc.MaterializeSecret)); err != nil {
			t.Fatalf("register page-backed Jiji: %v", err)
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
		_ = srv.Shutdown(context.Background())
		ts.Close()
		_ = db.Close()
	})
	return &acquisitionHarness{server: ts, db: db, registry: registry, dir: tmpDir}
}

func canonicalArgusPayload(credID string) map[string]any {
	return map[string]any{
		"locator": map[string]string{
			"type":     "douyin_url",
			"location": "https://www.douyin.com/video/7300000000000000125",
		},
		"authorized_credentials": []string{credID},
		"attestation": map[string]any{
			"attestation_type": "OPERATOR_EXPLICIT_CONFIRMATION",
			"declared_by":      "seam1-argus-tester",
			"terms_accepted":   true,
		},
	}
}

func TestSeam1_Acquisition_DirectJijiArgusGateFailsClosedBeforeExecution(t *testing.T) {
	h := setupArgusAcquisitionHarness(t, true, false)
	credID := h.registerSessionCredential(t, "argus_direct_session", "DOUYINIE_TEST_ARGUS_DIRECT", "session=direct-secret")

	body, _ := json.Marshal(canonicalArgusPayload(credID))
	resp, err := http.Post(h.server.URL+"/api/v1/sources/acquire", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/sources/acquire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected direct Jiji Argus gate to fail closed with 422, got %d: %s", resp.StatusCode, b)
	}
	var result struct {
		Error            string `json:"error"`
		AcquisitionState string `json:"acquisition_state"`
		ProviderID       string `json:"provider_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode acquisition failure: %v", err)
	}
	if result.ProviderID != "jiji_douyin" || result.AcquisitionState != string(domain.AcquisitionAntiBotOrEmpty) {
		t.Fatalf("unexpected direct-Jiji failure: %+v", result)
	}
	if !strings.Contains(result.Error, "page-backed") || strings.Contains(result.Error, "direct-secret") {
		t.Fatalf("expected safe page-backed requirement without secret echo, got %q", result.Error)
	}
	if n := countRows(t, h.db, "source_assets"); n != 0 {
		t.Fatalf("fail-closed direct Jiji request must create no SourceAsset, found %d", n)
	}
}

func TestSeam1_Acquisition_PageBackedHelperCreatesAndReusesSourceAsset(t *testing.T) {
	t.Setenv(seam1PageHelperEnv, "1")
	callsPath := filepath.Join(t.TempDir(), "page-helper-calls.txt")
	t.Setenv("DOUYINIE_SEAM1_PAGE_HELPER_CALLS", callsPath)
	h := setupArgusAcquisitionHarness(t, false, true)
	credID := h.registerSessionCredential(t, "argus_page_session", "DOUYINIE_TEST_ARGUS_PAGE", "session=page-backed-secret")

	resp1, res1 := h.acquire(t, canonicalArgusPayload(credID))
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("page-backed acquisition status=%d", resp1.StatusCode)
	}
	if res1.Asset == nil || res1.Asset.SHA256 == "" {
		t.Fatalf("page-backed acquisition must create a validated SourceAsset, got %+v", res1.Asset)
	}
	if res1.Provenance == nil || res1.Provenance.Adapter != "douyin_browser_assist" || res1.Provenance.Method != "browser" || !res1.Provenance.Authenticated {
		t.Fatalf("unexpected page-backed provenance: %+v", res1.Provenance)
	}
	if res1.Provenance.SourceID != "douyin:aweme:7300000000000000125" {
		t.Fatalf("unexpected canonical source identity: %q", res1.Provenance.SourceID)
	}

	resp2, res2 := h.acquire(t, canonicalArgusPayload(credID))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated || !res2.Reused || res2.Asset == nil || res2.Asset.ID != res1.Asset.ID {
		t.Fatalf("repeat page-backed acquisition must reuse the canonical SourceAsset, status=%d reused=%v asset=%+v", resp2.StatusCode, res2.Reused, res2.Asset)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read page-helper call evidence: %v", err)
	}
	if got := strings.Count(string(calls), "call\n"); got != 1 {
		t.Fatalf("identity dedup must skip the helper on repeat acquisition; helper calls=%d", got)
	}
	if n := countRows(t, h.db, "source_assets"); n != 1 {
		t.Fatalf("page-backed acquisition and reuse must leave one SourceAsset, found %d", n)
	}
}
