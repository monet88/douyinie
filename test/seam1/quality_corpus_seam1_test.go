package seam1_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
)

// makeSeam1AnnotationPack builds a fully secondary-adjudicated ReferenceAnnotationPack fixture.
func makeSeam1AnnotationPack(assetID string, isMultiSpeaker, hasBurnedIn, isNoDub bool) *benchmark.ReferenceAnnotationPack {
	adjudicatedAt := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	adjudication := benchmark.SecondaryAdjudication{
		AdjudicatedBy: "lead_linguist_adjudicator_seam1",
		AdjudicatedAt: adjudicatedAt,
		Status:        "APPROVED",
		Notes:         "Secondary adjudication verified against Seam 1 audio and visual streams.",
	}

	if isNoDub {
		return &benchmark.ReferenceAnnotationPack{
			PackID:               "pack_" + assetID,
			AssetID:              assetID,
			Adjudication:         adjudication,
			ChineseTranscript:    nil,
			CriticalItems:        nil,
			SemanticReferences:   nil,
			HasBurnedInSubtitles: false,
			SubtitleRegions:      nil,
		}
	}

	speaker1 := "spk_01"
	speaker2 := "spk_01"
	if isMultiSpeaker {
		speaker2 = "spk_02"
	}

	transcript := []benchmark.ReferenceTranscriptSegment{
		{
			SegmentID:   "seg_01",
			SpeakerID:   speaker1,
			ChineseText: "欢迎来到本期产品发布现场",
			StartMs:     0,
			EndMs:       3000,
			Words: []benchmark.ReferenceWordAlignment{
				{Word: "欢迎", StartMs: 0, EndMs: 800},
				{Word: "来到", StartMs: 800, EndMs: 1400},
				{Word: "本期", StartMs: 1400, EndMs: 2000},
				{Word: "产品", StartMs: 2000, EndMs: 2500},
				{Word: "发布现场", StartMs: 2500, EndMs: 3000},
			},
		},
		{
			SegmentID:   "seg_02",
			SpeakerID:   speaker2,
			ChineseText: "首批现货仅售四十九元请勿错过",
			StartMs:     3500,
			EndMs:       7000,
			Words: []benchmark.ReferenceWordAlignment{
				{Word: "首批", StartMs: 3500, EndMs: 4000},
				{Word: "现货", StartMs: 4000, EndMs: 4600},
				{Word: "仅售", StartMs: 4600, EndMs: 5200},
				{Word: "四十九元", StartMs: 5200, EndMs: 6200},
				{Word: "请勿错过", StartMs: 6200, EndMs: 7000},
			},
		},
	}

	critical := []benchmark.ReferenceCriticalItem{
		{
			Type:        "number",
			SourceText:  "四十九元",
			TargetRefVI: "49 tệ",
			TargetRefEN: "49 yuan",
			SegmentID:   "seg_02",
		},
		{
			Type:        "negation",
			SourceText:  "请勿错过",
			TargetRefVI: "đừng bỏ lỡ",
			TargetRefEN: "do not miss out",
			SegmentID:   "seg_02",
		},
	}

	semantic := []benchmark.SemanticReference{
		{
			SegmentID:   "seg_01",
			ReferenceVI: "Chào mừng đến với buổi ra mắt sản phẩm kỳ này",
			ReferenceEN: "Welcome to this product launch event",
		},
		{
			SegmentID:   "seg_02",
			ReferenceVI: "Đợt hàng đầu tiên chỉ bán 49 tệ xin đừng bỏ lỡ",
			ReferenceEN: "First batch is only 49 yuan please do not miss out",
		},
	}

	var subtitleRegions []benchmark.ReferenceSubtitleRegion
	if hasBurnedIn {
		subtitleRegions = []benchmark.ReferenceSubtitleRegion{
			{
				RegionID:    "sub_reg_01",
				StartMs:     0,
				EndMs:       3000,
				Box:         benchmark.SubtitleBoundingBox{X: 0.1, Y: 0.8, Width: 0.8, Height: 0.1},
				ChineseText: "欢迎来到本期产品发布现场",
			},
			{
				RegionID:    "sub_reg_02",
				StartMs:     3500,
				EndMs:       7000,
				Box:         benchmark.SubtitleBoundingBox{X: 0.1, Y: 0.8, Width: 0.8, Height: 0.1},
				ChineseText: "首批现货仅售四十九元请勿错过",
			},
		}
	}

	return &benchmark.ReferenceAnnotationPack{
		PackID:               "pack_" + assetID,
		AssetID:              assetID,
		Adjudication:         adjudication,
		ChineseTranscript:    transcript,
		CriticalItems:        critical,
		SemanticReferences:   semantic,
		HasBurnedInSubtitles: hasBurnedIn,
		SubtitleRegions:      subtitleRegions,
	}
}

// buildSeam1QualityCorpus generates a complete 24 primary + 7 reserve fixture set for Seam 1 tests.
func buildSeam1QualityCorpus() ([]benchmark.QualityCorpusAsset, []benchmark.QualityCorpusAsset) {
	type spec struct {
		id       string
		cat      benchmark.QualityCategory
		w, h     int
		durMs    int64
		multi    bool
		burnedIn bool
		noDub    bool
	}

	primarySpecs := []spec{
		// Clean Single-Speaker: 3
		{"zh_seam1_pri_01", benchmark.CategoryCleanSingleSpeaker, 1080, 1920, 15000, false, true, false},
		{"zh_seam1_pri_02", benchmark.CategoryCleanSingleSpeaker, 1080, 1920, 25000, false, true, false},
		{"zh_seam1_pri_03", benchmark.CategoryCleanSingleSpeaker, 1080, 1920, 35000, false, false, false},

		// Multi-Speaker: 4 (all multi)
		{"zh_seam1_pri_04", benchmark.CategoryMultiSpeaker, 1080, 1920, 20000, true, true, false},
		{"zh_seam1_pri_05", benchmark.CategoryMultiSpeaker, 1080, 1920, 45000, true, true, false},
		{"zh_seam1_pri_06", benchmark.CategoryMultiSpeaker, 1080, 1440, 50000, true, true, false}, // 3:4 portrait #1
		{"zh_seam1_pri_07", benchmark.CategoryMultiSpeaker, 1080, 1920, 75000, true, false, false},

		// Fast-Dense: 4
		{"zh_seam1_pri_08", benchmark.CategoryFastDense, 1080, 1920, 22000, false, true, false},
		{"zh_seam1_pri_09", benchmark.CategoryFastDense, 1080, 1920, 42000, true, true, false}, // multi #5
		{"zh_seam1_pri_10", benchmark.CategoryFastDense, 1080, 1920, 55000, false, true, false},
		{"zh_seam1_pri_11", benchmark.CategoryFastDense, 1080, 1920, 80000, true, true, false}, // multi #6

		// Loud-BGM-Ambience: 3
		{"zh_seam1_pri_12", benchmark.CategoryLoudBGMAmbience, 1080, 1920, 18000, false, true, false},
		{"zh_seam1_pri_13", benchmark.CategoryLoudBGMAmbience, 1080, 1920, 38000, true, true, false}, // multi #7
		{"zh_seam1_pri_14", benchmark.CategoryLoudBGMAmbience, 1080, 1920, 48000, false, true, false},

		// Accent-Entity: 3
		{"zh_seam1_pri_15", benchmark.CategoryAccentEntity, 1080, 1920, 28000, false, true, false},
		{"zh_seam1_pri_16", benchmark.CategoryAccentEntity, 1080, 1920, 58000, false, true, false},
		{"zh_seam1_pri_17", benchmark.CategoryAccentEntity, 1080, 1920, 12000, false, false, true}, // no-dub #1

		// Dense Burned-in Subtitle: 4
		{"zh_seam1_pri_18", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1920, 19000, false, true, false},
		{"zh_seam1_pri_19", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1920, 33000, false, true, false},
		{"zh_seam1_pri_20", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1440, 62000, false, true, false}, // 3:4 portrait #2
		{"zh_seam1_pri_21", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1920, 72000, false, true, false},

		// Duration-Pause stress: 3 (including 2 >= 90s)
		{"zh_seam1_pri_22", benchmark.CategoryDurationPauseStress, 1080, 1920, 65000, false, true, false},
		{"zh_seam1_pri_23", benchmark.CategoryDurationPauseStress, 1080, 1920, 95000, false, true, false},  // >=90s #1
		{"zh_seam1_pri_24", benchmark.CategoryDurationPauseStress, 1080, 1920, 110000, false, true, false}, // >=90s #2
	}

	primaries := make([]benchmark.QualityCorpusAsset, len(primarySpecs))
	for i, s := range primarySpecs {
		hasBurnedInRegions := (s.id == "zh_seam1_pri_18" || s.id == "zh_seam1_pri_19")
		pack := makeSeam1AnnotationPack(s.id, s.multi, hasBurnedInRegions, s.noDub)
		packDigest, _ := pack.ComputePackDigest()
		pack.PackDigest = packDigest

		var tags []string
		if s.multi {
			tags = append(tags, "multi_speaker")
		}
		if s.burnedIn {
			tags = append(tags, "burned_in_dialogue_subtitle")
		}
		if s.noDub {
			tags = append(tags, "no_dub_visual")
		}

		speakerCount := 1
		if s.multi {
			speakerCount = 2
		}

		audioCodec := "aac"
		normAudioSHA := "sha256_norm_" + s.id
		if s.noDub {
			audioCodec = ""
			normAudioSHA = ""
		}

		preflight := domain.PreflightReport{
			ID:                    "preflight_" + s.id,
			AssetID:               s.id,
			DurationSec:           float64(s.durMs) / 1000.0,
			DurationMs:            s.durMs,
			VideoCodec:            "h264",
			AudioCodec:            audioCodec,
			Width:                 s.w,
			Height:                s.h,
			FrameRate:             30.0,
			ContainerFormat:       "mov,mp4",
			ContainerValid:        true,
			FingerprintMatch:      true,
			NormalizedAudioSHA256: normAudioSHA,
			CreatedAt:             time.Now().UTC(),
		}

		shaBytes := sha256.Sum256([]byte("seam1_primary_" + s.id))
		shaHex := hex.EncodeToString(shaBytes[:])

		primaries[i] = benchmark.QualityCorpusAsset{
			AssetID:         s.id,
			SourceVideoID:   "video_" + s.id,
			SourceAssetID:   "source_asset_" + s.id,
			SHA256:          shaHex,
			PrimaryCategory: s.cat,
			Tags:            tags,
			Preflight:       preflight,
			ReferencePack:   pack,
			IsReserve:       false,
			NoDub:           s.noDub,
			SpeakerCount:    speakerCount,
		}
	}

	// 7 Reserves (one per category)
	reserveSpecs := []spec{
		{"zh_seam1_res_01", benchmark.CategoryCleanSingleSpeaker, 1080, 1920, 20000, false, true, false},
		{"zh_seam1_res_02", benchmark.CategoryMultiSpeaker, 1080, 1920, 30000, true, true, false},
		{"zh_seam1_res_03", benchmark.CategoryFastDense, 1080, 1920, 25000, false, true, false},
		{"zh_seam1_res_04", benchmark.CategoryLoudBGMAmbience, 1080, 1920, 35000, false, true, false},
		{"zh_seam1_res_05", benchmark.CategoryAccentEntity, 1080, 1920, 40000, false, true, false},
		{"zh_seam1_res_06", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1920, 45000, false, true, false},
		{"zh_seam1_res_07", benchmark.CategoryDurationPauseStress, 1080, 1920, 95000, false, true, false},
	}

	reserves := make([]benchmark.QualityCorpusAsset, len(reserveSpecs))
	for i, s := range reserveSpecs {
		pack := makeSeam1AnnotationPack(s.id, s.multi, s.burnedIn, s.noDub)
		packDigest, _ := pack.ComputePackDigest()
		pack.PackDigest = packDigest

		var tags []string
		if s.multi {
			tags = append(tags, "multi_speaker")
		}
		if s.burnedIn {
			tags = append(tags, "burned_in_dialogue_subtitle")
		}

		speakerCount := 1
		if s.multi {
			speakerCount = 2
		}

		preflight := domain.PreflightReport{
			ID:                    "preflight_" + s.id,
			AssetID:               s.id,
			DurationSec:           float64(s.durMs) / 1000.0,
			DurationMs:            s.durMs,
			VideoCodec:            "h264",
			AudioCodec:            "aac",
			Width:                 s.w,
			Height:                s.h,
			FrameRate:             30.0,
			ContainerFormat:       "mov,mp4",
			ContainerValid:        true,
			FingerprintMatch:      true,
			NormalizedAudioSHA256: "sha256_norm_" + s.id,
			CreatedAt:             time.Now().UTC(),
		}

		shaBytes := sha256.Sum256([]byte("seam1_reserve_" + s.id))
		shaHex := hex.EncodeToString(shaBytes[:])

		reserves[i] = benchmark.QualityCorpusAsset{
			AssetID:         s.id,
			SourceVideoID:   "video_" + s.id,
			SourceAssetID:   "source_asset_" + s.id,
			SHA256:          shaHex,
			PrimaryCategory: s.cat,
			Tags:            tags,
			Preflight:       preflight,
			ReferencePack:   pack,
			IsReserve:       true,
			NoDub:           s.noDub,
			SpeakerCount:    speakerCount,
		}
	}

	return primaries, reserves
}

func TestSeam1_QualityCorpus_SessionBindingAndGovernance(t *testing.T) {
	h := setupHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	primaries, reserves := buildSeam1QualityCorpus()

	// 1. Verify that every primary asset passes the real product supported-input/preflight envelope
	for _, p := range primaries {
		if err := benchmark.ValidatePreflightEnvelope(&p.Preflight, p.IsNoDub()); err != nil {
			t.Fatalf("primary asset %s failed preflight envelope: %v", p.AssetID, err)
		}
	}

	// 2. Freeze quality corpus
	frozenAt := time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("freeze quality corpus failed: %v", err)
	}

	// 3. Initialize Seam 1 Benchmark Session bound to the frozen manifest digest
	storeDir := filepath.Join(h.dir, "benchmark_sessions_quality")
	store, err := benchmark.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("create benchmark file store: %v", err)
	}

	identity := benchmark.SessionIdentityInput{
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "acquisition_100_urls", Digest: "62ce8541701236f69fb31f39ee724473f09a42b424ea1fef73f323af46d58f2c", EntriesCount: 100},
			{Name: "douyinie_quality_corpus_24_7", Digest: manifest.ManifestDigest, EntriesCount: 31},
		},
		ExecutionProfile: "local",
		BuildIdentity:    "468b3dc44aefb54aba01b73d3c9a158d06506418",
		ConfigSnapshot: map[string]any{
			"zero_overrun_strict": true,
			"profile":             "local",
		},
		ProviderBaselines: []benchmark.ProviderBaseline{
			{Role: "translation", ProviderID: "qwen3_local", SnapshotManifestDigest: "abc123sha"},
		},
		Environment: benchmark.EnvironmentAttestation{OS: "windows", Arch: "amd64"},
		Operator:    benchmark.OperatorMetadata{OperatorID: "seam1_auditor"},
	}

	session, err := store.Resume(identity)
	if err != nil {
		t.Fatalf("session resume: %v", err)
	}

	// 4. Bind QualityCorpusManifest to Session
	if err := session.RecordQualityCorpusManifest(manifest); err != nil {
		t.Fatalf("record quality corpus manifest: %v", err)
	}

	// 5. Generate and bind 28-strata stratified PASS audit plan
	auditPlan, err := benchmark.GenerateStratifiedAuditPlan(manifest)
	if err != nil {
		t.Fatalf("generate stratified audit plan: %v", err)
	}
	if auditPlan.TotalStrata != 28 {
		t.Fatalf("expected exactly 28 strata in audit plan, got %d", auditPlan.TotalStrata)
	}
	if err := session.RecordAuditPlan(auditPlan); err != nil {
		t.Fatalf("record audit plan in session: %v", err)
	}

	// 6. Test Pre-Run Governed Reserve Replacement under Session Envelope
	record, newManifest, err := manifest.ReplacePrimaryWithReserve(
		"zh_seam1_pri_01",
		"zh_seam1_res_01",
		"Upstream video taken private by author before scored run",
		"seam1_auditor",
		time.Date(2026, 9, 3, 11, 30, 0, 0, time.UTC),
		false,
		session.ID,
	)
	if err != nil {
		t.Fatalf("governed reserve replacement failed: %v", err)
	}

	if record.OldAssetID != "zh_seam1_pri_01" || record.NewAssetID != "zh_seam1_res_01" {
		t.Errorf("replacement record mismatch: old=%s new=%s", record.OldAssetID, record.NewAssetID)
	}
	if record.PriorManifestDigest != manifest.ManifestDigest {
		t.Errorf("prior digest mismatch: %s != %s", record.PriorManifestDigest, manifest.ManifestDigest)
	}

	// Mid-run replacement must be rejected fail-closed
	_, _, err = manifest.ReplacePrimaryWithReserve(
		"zh_seam1_pri_02",
		"zh_seam1_res_02",
		"Mid-run substitution attempt",
		"seam1_auditor",
		time.Date(2026, 9, 3, 11, 45, 0, 0, time.UTC),
		true, // run started!
		session.ID,
	)
	if err == nil || !strings.Contains(err.Error(), "mid-run replacement rejected") {
		t.Errorf("expected mid-run replacement rejection, got %v", err)
	}

	// 7. Test Mismatched Corpus Digest Refusal
	tamperedManifest := *manifest
	tamperedManifest.ManifestDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := session.RecordQualityCorpusManifest(&tamperedManifest); err == nil {
		t.Errorf("expected session to refuse mismatched corpus manifest digest")
	}

	// 8. Test Store Persistence Round-Trip of Corpus & Audit Plan
	if err := store.Save(session); err != nil {
		t.Fatalf("save session with quality corpus evidence failed: %v", err)
	}

	loadedSession, err := store.Load(session.ID)
	if err != nil {
		t.Fatalf("load session from store failed: %v", err)
	}
	gotManifest, ok := loadedSession.GetQualityCorpusManifest()
	if !ok || gotManifest == nil {
		t.Fatalf("loaded session missing quality corpus manifest")
	}
	if gotManifest.ManifestDigest != manifest.ManifestDigest {
		t.Errorf("manifest digest mismatch after load: %s != %s", gotManifest.ManifestDigest, manifest.ManifestDigest)
	}

	gotPlan, ok := loadedSession.GetAuditPlan()
	if !ok || gotPlan == nil {
		t.Fatalf("loaded session missing audit plan")
	}
	if gotPlan.PlanDigest != auditPlan.PlanDigest {
		t.Errorf("audit plan digest mismatch after load: %s != %s", gotPlan.PlanDigest, auditPlan.PlanDigest)
	}

	// 9. Verify client can query asset preflight through public localhost endpoint
	client := benchmark.NewRuntimeHostClient(h.server.URL, nil)
	testReport := primaries[0].Preflight
	raID := "ra_" + testReport.AssetID
	if err := h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{
		ID:              raID,
		AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
		DeclaredBy:      "seam1_auditor",
		TermsAccepted:   true,
		ConfirmedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create rights attestation: %v", err)
	}
	if err := h.db.CreateSourceAsset(ctx, domain.SourceAsset{
		ID:                  testReport.AssetID,
		SHA256:              primaries[0].SHA256,
		ByteSize:            2048,
		MimeType:            "video/mp4",
		OriginalFilename:    "test_primary.mp4",
		RightsAttestationID: raID,
		CASPath:             "/cas/objects/" + testReport.AssetID,
		CreatedAt:           time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create source asset: %v", err)
	}
	if err := h.db.SavePreflightReport(ctx, testReport); err != nil {
		t.Fatalf("save preflight report to db: %v", err)
	}
	queriedReport, err := client.GetAssetPreflight(ctx, testReport.AssetID)
	if err != nil {
		t.Fatalf("client.GetAssetPreflight failed: %v", err)
	}
	if queriedReport.AssetID != testReport.AssetID || queriedReport.DurationMs != testReport.DurationMs {
		t.Errorf("queried preflight report mismatch: %+v != %+v", queriedReport, testReport)
	}
	_ = newManifest
}

// TestSeam1_QualityCorpus_SyntheticRegression_BoundProfile48Executions is SYNTHETIC REGRESSION COVERAGE ONLY.
// It verifies deterministic harness mechanics (resumption, error projection, profile isolation, and rollups)
// using synthetic media and generated zh_seam1_pri_* fixtures. One benchmark session is bound to one
// hard execution profile (#65), so this Hybrid session executes 24 sources x VI/EN = 48 cases.
// It does NOT constitute the real 24-video release-evidence denominator for Issue #72.
func TestSeam1_QualityCorpus_SyntheticRegression_BoundProfile48Executions(t *testing.T) {
	h := setupHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	validMediaPath := createSyntheticMedia(t, h.dir, "seam1_qc_source.mp4")
	baseMediaBytes, err := os.ReadFile(validMediaPath)
	if err != nil {
		t.Fatalf("read synthetic media: %v", err)
	}

	primaries, reserves := buildSeam1QualityCorpus()
	for i := range primaries {
		pBytes := append([]byte(nil), baseMediaBytes...)
		pBytes = append(pBytes, []byte("\n// asset "+primaries[i].AssetID)...)
		obj, err := h.casStore.Put(bytes.NewReader(pBytes))
		if err != nil {
			t.Fatalf("put primary video in CAS: %v", err)
		}
		primaries[i].SHA256 = obj.SHA256
	}
	for i := range reserves {
		rBytes := append([]byte(nil), baseMediaBytes...)
		rBytes = append(rBytes, []byte("\n// reserve "+reserves[i].AssetID)...)
		obj, err := h.casStore.Put(bytes.NewReader(rBytes))
		if err != nil {
			t.Fatalf("put reserve video in CAS: %v", err)
		}
		reserves[i].SHA256 = obj.SHA256
	}

	frozenAt := time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("freeze quality corpus failed: %v", err)
	}

	storeDir := filepath.Join(h.dir, "benchmark_sessions_quality_96")
	store, err := benchmark.NewFileStore(storeDir)
	if err != nil {
		t.Fatalf("create benchmark store: %v", err)
	}

	identity := benchmark.SessionIdentityInput{
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "douyinie_quality_corpus_24_7", Digest: manifest.ManifestDigest, EntriesCount: 31},
		},
		ExecutionProfile: "hybrid",
		BuildIdentity:    "seam1-quality-corpus-96-test",
		ConfigSnapshot: map[string]any{
			"profile": "hybrid",
		},
		Environment: benchmark.EnvironmentAttestation{
			OS:             "windows",
			Arch:           "amd64",
			GPUModel:       "NVIDIA GeForce RTX 2060 SUPER",
			TotalVRAMBytes: 8589934592,
		},
		Operator: benchmark.OperatorMetadata{OperatorID: "seam1_qc_auditor"},
	}

	session, err := store.Resume(identity)
	if err != nil {
		t.Fatalf("session resume: %v", err)
	}

	client := benchmark.NewRuntimeHostClient(h.server.URL, h.server.Client())
	collector := benchmark.NewWDDMGPUCollector(50 * time.Millisecond)
	runner, err := benchmark.NewBenchmarkRunner(benchmark.RunnerConfig{
		Client:  client,
		Store:   store,
		Session: session,
		Sampler: collector,
	})
	if err != nil {
		t.Fatalf("create benchmark runner: %v", err)
	}
	// Configure synthetic mock providers to match Seam 1 quality corpus expectations
	if p, ok := h.registry.Get("fake_qwen3_asr"); ok {
		fakeASR := p.(*provider.FakeASRProvider)
		fakeASR.RawSegments = []domain.ASRRawSegment{
			{StartMs: 0, EndMs: 3000, Text: "欢迎来到本期产品发布现场", Confidence: 0.95, LanguageCode: "zh"},
			{StartMs: 3500, EndMs: 7000, Text: "首批现货仅售四十九元请勿错过", Confidence: 0.95, LanguageCode: "zh"},
		}
	}
	if p, ok := h.registry.Get("fake_qwen3_aligner"); ok {
		fakeAlign := p.(*provider.FakeAlignerProvider)
		fakeAlign.WordTimings = []domain.WordTiming{
			{Word: "欢迎", StartMs: 0, EndMs: 800, Confidence: 0.95},
			{Word: "来到", StartMs: 800, EndMs: 1400, Confidence: 0.95},
			{Word: "本期", StartMs: 1400, EndMs: 2000, Confidence: 0.95},
			{Word: "产品", StartMs: 2000, EndMs: 2500, Confidence: 0.95},
			{Word: "发布现场", StartMs: 2500, EndMs: 3000, Confidence: 0.95},
			{Word: "首批", StartMs: 3500, EndMs: 4000, Confidence: 0.95},
			{Word: "现货", StartMs: 4000, EndMs: 4600, Confidence: 0.95},
			{Word: "仅售", StartMs: 4600, EndMs: 5200, Confidence: 0.95},
			{Word: "四十九元", StartMs: 5200, EndMs: 6200, Confidence: 0.95},
			{Word: "请勿错过", StartMs: 6200, EndMs: 7000, Confidence: 0.95},
		}
	}
	if p, ok := h.registry.Get("fake_llm_translator"); ok {
		fakeTrans := p.(*provider.FakeTranslationProvider)
		fakeTrans.CustomTranslations = map[string]string{
			"vi:欢迎来到本期产品发布现场":       "Chào mừng đến với buổi ra mắt sản phẩm kỳ này",
			"en:欢迎来到本期产品发布现场":       "Welcome to this product launch event",
			"vi:首批现货仅售四十九元请勿错过":     "Lô hàng đầu tiên bán chỉ 49 tệ đừng bỏ lỡ",
			"en:首批现货仅售四十九元请勿错过":     "First batch only 49 yuan do not miss out",
			"vi:欢迎 来到 本期 产品 发布现场":   "Chào mừng đến với buổi ra mắt sản phẩm kỳ này",
			"en:欢迎 来到 本期 产品 发布现场":   "Welcome to this product launch event",
			"vi:首批 现货 仅售 四十九元 请勿错过": "Lô hàng đầu tiên bán chỉ 49 tệ đừng bỏ lỡ",
			"en:首批 现货 仅售 四十九元 请勿错过": "First batch only 49 yuan do not miss out",
		}
	}
	if p, ok := h.registry.Get("fake_vieneu_tts_vi"); ok {
		fakeTTS := p.(*provider.FakeTTSProvider)
		fakeTTS.DurationMs = 3000
		fakeTTS.CustomDurations = map[int]int64{
			0: 3000,
			1: 3500,
			2: 3500,
		}
	}
	if p, ok := h.registry.Get("fake_kokoro_tts_en"); ok {
		fakeTTS := p.(*provider.FakeTTSProvider)
		fakeTTS.DurationMs = 3000
		fakeTTS.CustomDurations = map[int]int64{
			0: 3000,
			1: 3500,
			2: 3500,
		}
	}
	// Seed all 24 primary assets in SQLite DB and CAS
	normAudioBytes := media.GeneratePCM16WAV(16000, 1, 3000)
	normAudioObj, err := h.casStore.Put(bytes.NewReader(normAudioBytes))
	if err != nil {
		t.Fatalf("put normalized audio in CAS: %v", err)
	}
	normAudioFile := normAudioObj.Path
	normAudioSHA := normAudioObj.SHA256
	assetSegments := make(map[string][]domain.TranslationInputSegment)
	assetAudioRoles := make(map[string][]domain.AudioSegment)

	for _, p := range primaries {
		raID := "ra_" + p.AssetID
		_ = h.db.CreateRightsAttestation(ctx, domain.RightsAttestation{
			ID:              raID,
			AttestationType: "OPERATOR_EXPLICIT_CONFIRMATION",
			DeclaredBy:      "seam1_qc_auditor",
			TermsAccepted:   true,
			ConfirmedAt:     time.Now().UTC(),
		})
		_ = h.db.CreateSourceAsset(ctx, domain.SourceAsset{
			ID:                  p.SourceAssetID,
			SHA256:              p.SHA256,
			ByteSize:            4096,
			MimeType:            "video/mp4",
			OriginalFilename:    p.AssetID + ".mp4",
			RightsAttestationID: raID,
			CASPath:             "/cas/objects/" + p.SourceAssetID,
			CreatedAt:           time.Now().UTC(),
		})
		pf := p.Preflight
		pf.AssetID = p.SourceAssetID
		pf.NormalizedAudioSHA256 = normAudioSHA
		pf.NormalizedAudioCASPath = normAudioFile
		_ = h.db.SavePreflightReport(ctx, pf)

		if p.NoDub {
			assetAudioRoles[p.AssetID] = []domain.AudioSegment{
				{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleInstrumentalBgm},
			}
		} else {
			assetAudioRoles[p.AssetID] = []domain.AudioSegment{
				{StartMs: 0, EndMs: 3000, Role: domain.AudioRoleNarrationDialogue},
			}
		}
	}
	auditPlan, err := benchmark.GenerateStratifiedAuditPlan(manifest)
	if err != nil {
		t.Fatalf("generate audit plan: %v", err)
	}

	refPacks := make(map[string]*benchmark.ReferenceAnnotationPack, len(primaries))
	for _, p := range primaries {
		refPacks[p.AssetID] = p.ReferencePack
	}

	// Execute the 48 cases bound to this Hybrid session identity.
	execInput := benchmark.QualityCorpusExecutionInput{
		Manifest:        manifest,
		AuditPlan:       auditPlan,
		ReferencePacks:  refPacks,
		AssetSegments:   assetSegments,
		AssetAudioRoles: assetAudioRoles,
	}
	summary, err := runner.ExecuteQualityCorpus(ctx, execInput)
	if err != nil {
		t.Fatalf("ExecuteQualityCorpus failed: %v", err)
	}

	// Invariant 1: Exactly 48 executions for the session-bound Hybrid profile.
	if summary.TotalCases != 48 {
		t.Fatalf("expected exactly 48 Hybrid quality cases, got %d", summary.TotalCases)
	}
	if summary.LocalRollup != nil {
		t.Fatalf("Hybrid session must not contain a Local rollup, got %+v", summary.LocalRollup)
	}
	if summary.HybridRollup == nil {
		t.Fatalf("expected Hybrid rollup")
	}
	if summary.HybridRollup.TotalCases != 48 {
		t.Errorf("expected 48 Hybrid cases, got %d", summary.HybridRollup.TotalCases)
	}

	for _, m := range summary.CaseMetrics {
		if m.Status != "PASS" {
			t.Logf("Case %s [%s] FAILED: %v", m.CaseID, m.Profile, m.FailReasons)
		}
	}
	if summary.HybridRollup.FailCount > 0 {
		t.Logf("Hybrid FailReasons: %v", summary.HybridRollup.FailReasons)
	}
	// Invariant 2: Profile Isolation and Hard Denominator (48 Total, 44 Pass, 4 Review, 0 Fail).
	if summary.HybridRollup.PassCount != 44 {
		t.Errorf("expected 44 Hybrid passes, got %d", summary.HybridRollup.PassCount)
	}
	if summary.HybridRollup.ReviewCount != 4 {
		t.Errorf("expected 4 Hybrid reviews, got %d", summary.HybridRollup.ReviewCount)
	}
	if summary.HybridRollup.FailCount != 0 {
		t.Errorf("expected 0 Hybrid failures, got %d", summary.HybridRollup.FailCount)
	}
	for _, m := range summary.CaseMetrics {
		if m.Profile != "hybrid" {
			t.Errorf("case %s leaked profile %q into Hybrid session", m.CaseID, m.Profile)
		}
	}
	if !summary.HybridRollup.GateSatisfied {
		t.Errorf("expected Hybrid profile gate to be satisfied, fail reasons: %v", summary.HybridRollup.FailReasons)
	}
	if summary.OverallVerdict != "REVIEW_REQUIRED" {
		t.Errorf("expected overall verdict REVIEW_REQUIRED, got %s", summary.OverallVerdict)
	}
	// Invariant 3: Session Resumption
	// Re-running ExecuteQualityCorpus must immediately reuse completed cases without re-execution
	reusedSummary, err := runner.ExecuteQualityCorpus(ctx, execInput)
	if err != nil {
		t.Fatalf("resumed ExecuteQualityCorpus failed: %v", err)
	}
	if reusedSummary.SummaryDigest != summary.SummaryDigest {
		t.Errorf("resumed summary digest %s != original %s", reusedSummary.SummaryDigest, summary.SummaryDigest)
	}
}
