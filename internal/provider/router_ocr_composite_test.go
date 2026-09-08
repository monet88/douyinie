package provider_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/storage"
)

type ocrCompositeDep struct {
	name string
	ver  string
	role string
}

func ocrCompositeDeps() []ocrCompositeDep {
	return []ocrCompositeDep{
		{name: "PP-OCRv6_medium_det", ver: "v6", role: "det"},
		{name: "PP-OCRv6_medium_rec", ver: "v6", role: "rec"},
		{name: "PP-LCNet_x1_0_textline_ori", ver: "v1", role: "ori"},
	}
}

func registerOCRCompositeLicense(t *testing.T, ctx context.Context, licSvc *governance.LicenseService, skip string) {
	t.Helper()
	for _, d := range ocrCompositeDeps() {
		if d.name == skip {
			continue
		}
		if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
			ID:             uuid.NewString(),
			DependencyName: d.name,
			Version:        d.ver,
			SHA256:         "sha256_dummy_" + d.name,
			SourceRepo:     "test/" + d.name,
			CodeLicense:    "Apache-2.0",
			ModelLicense:   "Apache-2.0",
			DataLicense:    "OpenData",
			ServiceTerms:   "Standard",
			Verified:       true,
			CreatedAt:      time.Now().UTC(),
		}); err != nil {
			t.Fatalf("register license %s:%s: %v", d.name, d.ver, err)
		}
	}
}

func provisionOCRCompositeWithSnapshots(t *testing.T, ctx context.Context, licSvc *governance.LicenseService, snapSvc *governance.SnapshotService, skipLicense, skipSnapshot string) map[string]string {
	t.Helper()
	roots := map[string]string{}
	for _, d := range ocrCompositeDeps() {
		if d.name == skipLicense {
			continue
		}
		root := t.TempDir()
		content := "weights-" + d.name + "-" + d.ver
		rel := "model.bin"
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0644); err != nil {
			t.Fatalf("write snapshot file: %v", err)
		}
		h := sha256.Sum256([]byte(content))
		manifest := domain.SnapshotManifest{
			SchemaVersion: "1.0",
			ModelID:       d.name,
			ModelVersion:  d.ver,
			Files: []domain.SnapshotFileEntry{
				{RelativePath: rel, SHA256: hex.EncodeToString(h[:]), SizeBytes: int64(len(content))},
			},
		}
		cSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
		if err != nil {
			t.Fatalf("compute manifest sha: %v", err)
		}
		manifest.SnapshotManifestSHA256 = cSHA
		if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
			ID:             uuid.NewString(),
			DependencyName: d.name,
			Version:        d.ver,
			SHA256:         cSHA,
			SourceRepo:     "test/" + d.name,
			CodeLicense:    "Apache-2.0",
			ModelLicense:   "Apache-2.0",
			DataLicense:    "OpenData",
			ServiceTerms:   "Standard",
			Verified:       true,
			CreatedAt:      time.Now().UTC(),
		}); err != nil {
			t.Fatalf("register license %s:%s: %v", d.name, d.ver, err)
		}
		if d.name == skipSnapshot {
			continue
		}
		if _, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, root); err != nil {
			t.Fatalf("register snapshot %s:%s: %v", d.name, d.ver, err)
		}
		roots[d.name] = root
	}
	return roots
}

func setupOCRCompositeRouter(t *testing.T, skipLicense, skipSnapshot string, withSnapshots bool) (*provider.Router, *provider.WorkerOCRProvider) {
	t.Helper()
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ocr, err := provider.NewWorkerOCRProvider("ppocr_v6", "paddleocr-v6", "v6", 0.92)
	if err != nil {
		t.Fatalf("NewWorkerOCRProvider: %v", err)
	}
	reg := provider.NewRegistry()
	if err := reg.Register(ocr); err != nil {
		t.Fatalf("register ocr: %v", err)
	}
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	circuit := provider.NewCircuitBreaker(provider.CircuitBreakerConfig{})
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, circuit, db)

	if withSnapshots {
		snapSvc := governance.NewSnapshotService(db, licSvc)
		provisionOCRCompositeWithSnapshots(t, ctx, licSvc, snapSvc, skipLicense, skipSnapshot)
		router.SetSnapshotService(snapSvc)
		ocr.SetSnapshotService(snapSvc)
	} else {
		registerOCRCompositeLicense(t, ctx, licSvc, skipLicense)
	}
	return router, ocr
}

func TestRouter_OCRComposite_NoTopLevelRequired(t *testing.T) {
	router, ocr := setupOCRCompositeRouter(t, "", "", true)
	if got, want := func() string { n, v := ocr.ModelInfo(); return n + ":" + v }(), "paddleocr-v6:v6"; got != want {
		t.Fatalf("ModelInfo must remain logical identity, got %q want %q", got, want)
	}
	if ocr.PrimaryCheckpointRequired() {
		t.Fatalf("WorkerOCRProvider must opt out of primary checkpoint governance")
	}
	if !ocr.RequiresSnapshot() {
		t.Fatalf("WorkerOCRProvider must keep requiresSnapshot=true for det/rec/ori envelope validation")
	}
	routeRes, err := router.Route(context.Background(), provider.RouteRequest{Stage: provider.TypeOCR, Language: "zh"})
	if err != nil {
		t.Fatalf("OCR composite routing must succeed without top-level license/snapshot, got: %v", err)
	}
	if routeRes.SelectedProvider.ID() != "ppocr_v6" {
		t.Fatalf("expected ppocr_v6 selected, got %s", routeRes.SelectedProvider.ID())
	}
}

func TestRouter_OCRComposite_MissingDepLicenseFailClosed(t *testing.T) {
	router, _ := setupOCRCompositeRouter(t, "PP-OCRv6_medium_rec", "", false)
	_, err := router.Route(context.Background(), provider.RouteRequest{Stage: provider.TypeOCR, Language: "zh"})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider on missing dep license, got %v", err)
	}
}

func TestRouter_OCRComposite_MissingDepSnapshotFailClosed(t *testing.T) {
	router, _ := setupOCRCompositeRouter(t, "", "PP-LCNet_x1_0_textline_ori", true)
	_, err := router.Route(context.Background(), provider.RouteRequest{Stage: provider.TypeOCR, Language: "zh"})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider on missing dep snapshot, got %v", err)
	}
}

func TestRouter_OCRComposite_MutatedDepSnapshotFailClosed(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ocr, err := provider.NewWorkerOCRProvider("ppocr_v6", "paddleocr-v6", "v6", 0.92)
	if err != nil {
		t.Fatalf("NewWorkerOCRProvider: %v", err)
	}
	reg := provider.NewRegistry()
	if err := reg.Register(ocr); err != nil {
		t.Fatalf("register: %v", err)
	}
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, provider.NewCircuitBreaker(provider.CircuitBreakerConfig{}), db)
	snapSvc := governance.NewSnapshotService(db, licSvc)
	roots := provisionOCRCompositeWithSnapshots(t, ctx, licSvc, snapSvc, "", "")
	router.SetSnapshotService(snapSvc)
	ocr.SetSnapshotService(snapSvc)

	if _, err := router.Route(ctx, provider.RouteRequest{Stage: provider.TypeOCR, Language: "zh"}); err != nil {
		t.Fatalf("precondition routing must succeed, got: %v", err)
	}
	recRoot := roots["PP-OCRv6_medium_rec"]
	if err := os.WriteFile(filepath.Join(recRoot, "model.bin"), []byte("mutated-bytes-corrupted-longer"), 0644); err != nil {
		t.Fatalf("mutate rec file: %v", err)
	}
	_, err = router.Route(ctx, provider.RouteRequest{Stage: provider.TypeOCR, Language: "zh"})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider on mutated dep snapshot, got %v", err)
	}
}

func TestRouter_DiarizerPrimaryStillRequired(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	diar, err := provider.NewWorkerDiarizationProvider("campplus_diarizer", "iic/speech_campplus_sv_zh_en_16k-common_advanced", "v1.0.0")
	if err != nil {
		t.Fatalf("NewWorkerDiarizationProvider: %v", err)
	}
	diar.SetRequiresSnapshot(false)
	if pc, ok := any(diar).(provider.PrimaryCheckpointProvider); ok && !pc.PrimaryCheckpointRequired() {
		t.Fatalf("diarizer must not opt out of primary checkpoint governance")
	}
	reg := provider.NewRegistry()
	if err := reg.Register(diar); err != nil {
		t.Fatalf("register: %v", err)
	}
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, provider.NewCircuitBreaker(provider.CircuitBreakerConfig{}), db)

	vName, vVer := diar.VADModelInfo()
	if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID: uuid.NewString(), DependencyName: vName, Version: vVer, SHA256: "sha256_dummy_" + vName,
		SourceRepo: "test/" + vName, CodeLicense: "Apache-2.0", ModelLicense: "Apache-2.0",
		DataLicense: "OpenData", ServiceTerms: "Standard", Verified: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register vad license: %v", err)
	}
	_, err = router.Route(ctx, provider.RouteRequest{Stage: provider.TypeDiarizer, Language: "zh"})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("diarizer without primary license must fail closed, got %v", err)
	}
	mName, mVer := diar.ModelInfo()
	if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID: uuid.NewString(), DependencyName: mName, Version: mVer, SHA256: "sha256_dummy_" + mName,
		SourceRepo: "test/" + mName, CodeLicense: "Apache-2.0", ModelLicense: "Apache-2.0",
		DataLicense: "OpenData", ServiceTerms: "Standard", Verified: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register primary license: %v", err)
	}
	routeRes, err := router.Route(ctx, provider.RouteRequest{Stage: provider.TypeDiarizer, Language: "zh"})
	if err != nil {
		t.Fatalf("diarizer with primary+vad licenses must route, got: %v", err)
	}
	if routeRes.SelectedProvider.ID() != "campplus_diarizer" {
		t.Fatalf("expected campplus_diarizer, got %s", routeRes.SelectedProvider.ID())
	}
}

func TestRouter_ExecuteRouted_OCRNoLogicalSnapshot(t *testing.T) {
	router, _ := setupOCRCompositeRouter(t, "", "", true)
	ctx := context.Background()
	routeRes, err := router.Route(ctx, provider.RouteRequest{Stage: provider.TypeOCR, Language: "zh"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	called := false
	err = router.ExecuteRoutedWithRetry(ctx, provider.RouteRequest{Stage: provider.TypeOCR, Language: "zh"}, routeRes, "test-input-hash", 1, func(p provider.Provider, attempt int) error {
		called = true
		if n, v := p.ModelInfo(); n != "paddleocr-v6" || v != "v6" {
			t.Errorf("ModelInfo must stay paddleocr-v6:v6, got %s:%s", n, v)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteRoutedWithRetry must not demand logical paddleocr-v6:v6 snapshot, got: %v", err)
	}
	if !called {
		t.Fatalf("executeFn was not invoked")
	}
}

func TestRouter_OCRComposite_MissingDetSnapshotFailClosed(t *testing.T) {
	router, _ := setupOCRCompositeRouter(t, "", "PP-OCRv6_medium_det", true)
	_, err := router.Route(context.Background(), provider.RouteRequest{Stage: provider.TypeOCR, Language: "zh"})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider on missing det snapshot, got %v", err)
	}
}

func TestRouter_LanguageWildcard_RequestSideSelectsConcreteProvider(t *testing.T) {
	router, _ := setupOCRCompositeRouter(t, "", "", true)
	ctx := context.Background()

	// 1. Exact "*" wildcard skips language restriction and selects ppocr_v6 (which declares zh, en)
	res, err := router.Route(ctx, provider.RouteRequest{Stage: provider.TypeOCR, Language: "*"})
	if err != nil {
		t.Fatalf("expected RouteRequest Language='*' to select concrete provider, got error: %v", err)
	}
	if res.SelectedProvider.ID() != "ppocr_v6" {
		t.Fatalf("expected ppocr_v6 selected, got %s", res.SelectedProvider.ID())
	}

	// 2. Whitespace-padded " * " wildcard also normalized and selects ppocr_v6
	resPadded, err := router.Route(ctx, provider.RouteRequest{Stage: provider.TypeOCR, Language: " * "})
	if err != nil {
		t.Fatalf("expected RouteRequest Language=' * ' to select concrete provider, got error: %v", err)
	}
	if resPadded.SelectedProvider.ID() != "ppocr_v6" {
		t.Fatalf("expected ppocr_v6 selected, got %s", resPadded.SelectedProvider.ID())
	}
}

func TestRouter_LanguageWildcard_ConcreteUnsupportedLanguageRejects(t *testing.T) {
	router, _ := setupOCRCompositeRouter(t, "", "", true)
	ctx := context.Background()

	// Concrete unsupported language must fail closed with ErrNoEligibleProvider
	_, err := router.Route(ctx, provider.RouteRequest{Stage: provider.TypeOCR, Language: "fr"})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider for unsupported language 'fr', got: %v", err)
	}
}

func TestRouter_LanguageWildcard_ProviderSideWildcardMatchesConcreteLanguage(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := storage.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fakeWildcard := &provider.BaseFakeProvider{
		ProviderID:   "fake_wildcard_ocr",
		ProviderType: provider.TypeOCR,
		Policy:       provider.PolicyAllowed,
		Healthy:      true,
		Cap: domain.ProviderCapability{
			Stage:         string(provider.TypeOCR),
			Languages:     []string{"*"},
			ExecutionTier: "local",
			QualityScore:  0.90,
		},
		ModelName:    "mock-ocr",
		ModelVersion: "v1",
	}

	reg := provider.NewRegistry()
	if err := reg.Register(fakeWildcard); err != nil {
		t.Fatalf("register: %v", err)
	}
	polSvc := governance.NewPolicyService(db)
	licSvc := governance.NewLicenseService(db)
	credSvc := governance.NewCredentialService(db)
	circuit := provider.NewCircuitBreaker(provider.CircuitBreakerConfig{})
	router := provider.NewRouter(reg, polSvc, licSvc, credSvc, circuit, db)

	if err := licSvc.RegisterManifest(context.Background(), domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: "mock-ocr",
		Version:        "v1",
		SHA256:         "sha256_dummy_mock-ocr",
		SourceRepo:     "test/mock-ocr",
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "OpenData",
		ServiceTerms:   "Standard",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("register license: %v", err)
	}

	// Provider declaring "*" must match concrete request language "vi"
	res, err := router.Route(context.Background(), provider.RouteRequest{
		Stage:    provider.TypeOCR,
		Language: "vi",
	})
	if err != nil {
		t.Fatalf("expected provider-side wildcard to match concrete language 'vi', got error: %v", err)
	}
	if res.SelectedProvider.ID() != "fake_wildcard_ocr" {
		t.Fatalf("expected fake_wildcard_ocr selected, got %s", res.SelectedProvider.ID())
	}
}

func TestRouter_LanguageWildcard_RequiredFeaturesStarDoesNotWildcard(t *testing.T) {
	router, _ := setupOCRCompositeRouter(t, "", "", true)
	ctx := context.Background()

	// Requesting feature "*" must NOT act as a wildcard and must reject ppocr_v6
	_, err := router.Route(ctx, provider.RouteRequest{
		Stage:            provider.TypeOCR,
		Language:         "*",
		RequiredFeatures: []string{"*"},
	})
	if !errors.Is(err, domain.ErrNoEligibleProvider) {
		t.Fatalf("expected ErrNoEligibleProvider when requesting feature '*', got: %v", err)
	}
}
