package benchmark_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/benchmark"
	"github.com/monet88/douyinie/internal/domain"
)


// makeValidAnnotationPack builds a fully secondary-adjudicated ReferenceAnnotationPack fixture.
func makeValidAnnotationPack(assetID string, isMultiSpeaker, hasBurnedIn, isNoDub bool) *benchmark.ReferenceAnnotationPack {
	adjudicatedAt := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	adjudication := benchmark.SecondaryAdjudication{
		AdjudicatedBy: "lead_linguist_adjudicator_01",
		AdjudicatedAt: adjudicatedAt,
		Status:        "APPROVED",
		Notes:         "Secondary adjudication verified against audio and visual tracks.",
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

	speaker1 := "speaker_01"
	speaker2 := "speaker_01"
	if isMultiSpeaker {
		speaker2 = "speaker_02"
	}

	transcript := []benchmark.ReferenceTranscriptSegment{
		{
			SegmentID:   "seg_01",
			SpeakerID:   speaker1,
			ChineseText: "欢迎来到本期视频我们今天介绍核心产品",
			StartMs:     500,
			EndMs:       3500,
			Words: []benchmark.ReferenceWordAlignment{
				{Word: "欢迎", StartMs: 500, EndMs: 1000},
				{Word: "来到", StartMs: 1000, EndMs: 1500},
				{Word: "本期", StartMs: 1500, EndMs: 2000},
				{Word: "视频", StartMs: 2000, EndMs: 2500},
				{Word: "我们", StartMs: 2500, EndMs: 2800},
				{Word: "今天", StartMs: 2800, EndMs: 3100},
				{Word: "介绍", StartMs: 3100, EndMs: 3500},
			},
		},
		{
			SegmentID:   "seg_02",
			SpeakerID:   speaker2,
			ChineseText: "请注意这款设备售价为九十九元不含运费",
			StartMs:     4000,
			EndMs:       7500,
			Words: []benchmark.ReferenceWordAlignment{
				{Word: "请注意", StartMs: 4000, EndMs: 4600},
				{Word: "这款", StartMs: 4600, EndMs: 5000},
				{Word: "设备", StartMs: 5000, EndMs: 5500},
				{Word: "售价", StartMs: 5500, EndMs: 6000},
				{Word: "为", StartMs: 6000, EndMs: 6200},
				{Word: "九十九元", StartMs: 6200, EndMs: 7000},
				{Word: "不含运费", StartMs: 7000, EndMs: 7500},
			},
		},
	}

	critical := []benchmark.ReferenceCriticalItem{
		{
			Type:        "number",
			SourceText:  "九十九元",
			TargetRefVI: "99 tệ",
			TargetRefEN: "99 yuan",
			SegmentID:   "seg_02",
		},
		{
			Type:        "negation",
			SourceText:  "不含运费",
			TargetRefVI: "không bao gồm phí vận chuyển",
			TargetRefEN: "shipping not included",
			SegmentID:   "seg_02",
		},
	}

	semantic := []benchmark.SemanticReference{
		{
			SegmentID:   "seg_01",
			ReferenceVI: "Chào mừng đến với video này hôm nay chúng tôi giới thiệu sản phẩm cốt lõi",
			ReferenceEN: "Welcome to this video today we introduce our core product",
		},
		{
			SegmentID:   "seg_02",
			ReferenceVI: "Xin lưu ý thiết bị này có giá 99 tệ không bao gồm phí vận chuyển",
			ReferenceEN: "Please note this device is priced at 99 yuan excluding shipping",
		},
	}

	var subtitleRegions []benchmark.ReferenceSubtitleRegion
	if hasBurnedIn {
		subtitleRegions = []benchmark.ReferenceSubtitleRegion{
			{
				RegionID:    "sub_reg_01",
				StartMs:     500,
				EndMs:       3500,
				Box:         benchmark.SubtitleBoundingBox{X: 0.1, Y: 0.8, Width: 0.8, Height: 0.1},
				ChineseText: "欢迎来到本期视频我们今天介绍核心产品",
			},
			{
				RegionID:    "sub_reg_02",
				StartMs:     4000,
				EndMs:       7500,
				Box:         benchmark.SubtitleBoundingBox{X: 0.1, Y: 0.8, Width: 0.8, Height: 0.1},
				ChineseText: "请注意这款设备售价为九十九元不含运费",
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

// makeValidPreflight builds a valid PreflightReport meeting real product envelope.
func makeValidPreflight(assetID string, width, height int, durMs int64, isNoDub bool) domain.PreflightReport {
	audioCodec := "aac"
	normAudioSHA := "sha256_norm_audio_" + assetID
	if isNoDub {
		audioCodec = ""
		normAudioSHA = ""
	}
	return domain.PreflightReport{
		ID:                     "preflight_" + assetID,
		AssetID:                assetID,
		DurationSec:            float64(durMs) / 1000.0,
		DurationMs:             durMs,
		VideoCodec:             "h264",
		AudioCodec:             audioCodec,
		Width:                  width,
		Height:                 height,
		FrameRate:              30.0,
		AudioChannels:          2,
		AudioSampleRate:        44100,
		ContainerFormat:        "mov,mp4,m4a,3gp,3g2,mj2",
		ContainerValid:         true,
		FingerprintMatch:       true,
		NormalizedAudioSHA256:  normAudioSHA,
		NormalizedAudioCASPath: "/cas/objects/" + normAudioSHA,
		CreatedAt:              time.Now().UTC(),
	}
}

// buildValidTestCorpus generates a complete 24 primary + 7 reserve fixture set.
func buildValidTestCorpus() ([]benchmark.QualityCorpusAsset, []benchmark.QualityCorpusAsset) {
	// Quotas:
	// Clean Single-Speaker: 3
	// Multi-Speaker: 4
	// Fast-Dense: 4
	// Loud-BGM-Ambience: 3
	// Accent-Entity: 3
	// Dense Burned-in Subtitle: 4
	// Duration-Pause stress: 3
	// Total = 24 primary assets.
	//
	// Minima:
	// Multi-speaker >= 6
	// Burned-in subtitle >= 16
	// No-dub <= 1
	// Majority 9:16: >= 13
	// 3:4 portrait >= 2
	// Duration >= 90s: >= 2
	// Varied duration buckets >= 3

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
		// 1. Clean Single-Speaker: 3
		{"zh_pri_01", benchmark.CategoryCleanSingleSpeaker, 1080, 1920, 15000, false, true, false},
		{"zh_pri_02", benchmark.CategoryCleanSingleSpeaker, 1080, 1920, 25000, false, true, false},
		{"zh_pri_03", benchmark.CategoryCleanSingleSpeaker, 1080, 1920, 35000, false, false, false},

		// 2. Multi-Speaker: 4 (all multi-speaker)
		{"zh_pri_04", benchmark.CategoryMultiSpeaker, 1080, 1920, 20000, true, true, false},
		{"zh_pri_05", benchmark.CategoryMultiSpeaker, 1080, 1920, 45000, true, true, false},
		{"zh_pri_06", benchmark.CategoryMultiSpeaker, 1080, 1440, 50000, true, true, false}, // 3:4 portrait #1
		{"zh_pri_07", benchmark.CategoryMultiSpeaker, 1080, 1920, 75000, true, false, false},

		// 3. Fast-Dense: 4
		{"zh_pri_08", benchmark.CategoryFastDense, 1080, 1920, 22000, false, true, false},
		{"zh_pri_09", benchmark.CategoryFastDense, 1080, 1920, 42000, true, true, false}, // multi-speaker #5
		{"zh_pri_10", benchmark.CategoryFastDense, 1080, 1920, 55000, false, true, false},
		{"zh_pri_11", benchmark.CategoryFastDense, 1080, 1920, 80000, true, true, false}, // multi-speaker #6

		// 4. Loud-BGM-Ambience: 3
		{"zh_pri_12", benchmark.CategoryLoudBGMAmbience, 1080, 1920, 18000, false, true, false},
		{"zh_pri_13", benchmark.CategoryLoudBGMAmbience, 1080, 1920, 38000, true, true, false}, // multi-speaker #7
		{"zh_pri_14", benchmark.CategoryLoudBGMAmbience, 1080, 1920, 48000, false, true, false},

		// 5. Accent-Entity: 3
		{"zh_pri_15", benchmark.CategoryAccentEntity, 1080, 1920, 28000, false, true, false},
		{"zh_pri_16", benchmark.CategoryAccentEntity, 1080, 1920, 58000, false, true, false},
		{"zh_pri_17", benchmark.CategoryAccentEntity, 1080, 1920, 12000, false, false, true}, // no-dub #1

		// 6. Dense Burned-in Subtitle: 4 (all burned-in)
		{"zh_pri_18", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1920, 19000, false, true, false},
		{"zh_pri_19", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1920, 33000, false, true, false},
		{"zh_pri_20", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1440, 62000, false, true, false}, // 3:4 portrait #2
		{"zh_pri_21", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1920, 72000, false, true, false},

		// 7. Duration-Pause stress: 3 (including 2 >= 90s)
		{"zh_pri_22", benchmark.CategoryDurationPauseStress, 1080, 1920, 65000, false, true, false},
		{"zh_pri_23", benchmark.CategoryDurationPauseStress, 1080, 1920, 95000, false, true, false},  // >=90s #1
		{"zh_pri_24", benchmark.CategoryDurationPauseStress, 1080, 1920, 110000, false, true, false}, // >=90s #2
	}

	primaries := make([]benchmark.QualityCorpusAsset, len(primarySpecs))
	for i, s := range primarySpecs {
		pack := makeValidAnnotationPack(s.id, s.multi, s.burnedIn, s.noDub)
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

		shaBytes := sha256.Sum256([]byte("primary_video_" + s.id))
		shaHex := hex.EncodeToString(shaBytes[:])

		primaries[i] = benchmark.QualityCorpusAsset{
			AssetID:         s.id,
			SourceVideoID:   "source_video_" + s.id,
			SourceAssetID:   "source_asset_" + s.id,
			SHA256:          shaHex,
			PrimaryCategory: s.cat,
			Tags:            tags,
			Preflight:       makeValidPreflight(s.id, s.w, s.h, s.durMs, s.noDub),
			ReferencePack:   pack,
			IsReserve:       false,
			NoDub:           s.noDub,
			SpeakerCount:    speakerCount,
		}
	}

	// 7 Reserves (one per category)
	reserveSpecs := []spec{
		{"zh_res_01", benchmark.CategoryCleanSingleSpeaker, 1080, 1920, 20000, false, true, false},
		{"zh_res_02", benchmark.CategoryMultiSpeaker, 1080, 1920, 30000, true, true, false},
		{"zh_res_03", benchmark.CategoryFastDense, 1080, 1920, 25000, false, true, false},
		{"zh_res_04", benchmark.CategoryLoudBGMAmbience, 1080, 1920, 35000, false, true, false},
		{"zh_res_05", benchmark.CategoryAccentEntity, 1080, 1920, 40000, false, true, false},
		{"zh_res_06", benchmark.CategoryDenseBurnedInSubtitle, 1080, 1920, 45000, false, true, false},
		{"zh_res_07", benchmark.CategoryDurationPauseStress, 1080, 1920, 95000, false, true, false},
	}

	reserves := make([]benchmark.QualityCorpusAsset, len(reserveSpecs))
	for i, s := range reserveSpecs {
		pack := makeValidAnnotationPack(s.id, s.multi, s.burnedIn, s.noDub)
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

		shaBytes := sha256.Sum256([]byte("reserve_video_" + s.id))
		shaHex := hex.EncodeToString(shaBytes[:])

		reserves[i] = benchmark.QualityCorpusAsset{
			AssetID:         s.id,
			SourceVideoID:   "source_video_" + s.id,
			SourceAssetID:   "source_asset_" + s.id,
			SHA256:          shaHex,
			PrimaryCategory: s.cat,
			Tags:            tags,
			Preflight:       makeValidPreflight(s.id, s.w, s.h, s.durMs, s.noDub),
			ReferencePack:   pack,
			IsReserve:       true,
			NoDub:           s.noDub,
			SpeakerCount:    speakerCount,
		}
	}

	return primaries, reserves
}

func TestQualityCorpus_FreezeAndManifestIntegrity(t *testing.T) {
	primaries, reserves := buildValidTestCorpus()

	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	manifest1, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("freeze valid quality corpus failed: %v", err)
	}

	if len(manifest1.PrimaryAssets) != 24 {
		t.Errorf("expected 24 primary assets, got %d", len(manifest1.PrimaryAssets))
	}
	if len(manifest1.ReserveAssets) != 7 {
		t.Errorf("expected 7 reserve assets, got %d", len(manifest1.ReserveAssets))
	}
	if manifest1.ManifestDigest == "" {
		t.Fatalf("expected non-empty manifest digest")
	}

	// 1. Verify Manifest Digest
	if err := manifest1.VerifyManifestDigest(); err != nil {
		t.Errorf("verify manifest digest failed: %v", err)
	}

	// 2. Determinism check: freezing again yields identical digest
	manifest2, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("second freeze failed: %v", err)
	}
	if manifest1.ManifestDigest != manifest2.ManifestDigest {
		t.Errorf("manifest digest must be byte-for-byte deterministic: got %s != %s", manifest1.ManifestDigest, manifest2.ManifestDigest)
	}

	// 3. Mutation detection
	tampered := *manifest1
	tampered.ManifestDigest = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := tampered.VerifyManifestDigest(); err == nil {
		t.Errorf("expected tampered digest to fail verification")
	}
}

func TestQualityCorpus_QuotaRejection(t *testing.T) {
	// 1. Primary count != 24
	primaries, reserves := buildValidTestCorpus()
	shortPrimaries := primaries[:23]
	err := benchmark.ValidateCorpusInputs(shortPrimaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "expected exactly 24 primary assets") {
		t.Errorf("expected rejection for 23 primary assets, got %v", err)
	}

	// 2. Quota mismatch: 4 clean single-speaker and 3 multi-speaker (instead of 3 and 4)
	primaries, reserves = buildValidTestCorpus()
	primaries[3].PrimaryCategory = benchmark.CategoryCleanSingleSpeaker // was CategoryMultiSpeaker
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "quota mismatch") {
		t.Errorf("expected quota mismatch rejection, got %v", err)
	}

	// 3. Duplicate asset ID
	primaries, reserves = buildValidTestCorpus()
	primaries[1].AssetID = primaries[0].AssetID
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "duplicate primary asset_id") {
		t.Errorf("expected duplicate asset ID rejection, got %v", err)
	}
}

func TestQualityCorpus_MinimaRejection(t *testing.T) {
	// 1. Multi-speaker count < 6
	primaries, reserves := buildValidTestCorpus()
	for i := range primaries {
		primaries[i].SpeakerCount = 1
		primaries[i].Tags = nil
		if primaries[i].ReferencePack != nil {
			for si := range primaries[i].ReferencePack.ChineseTranscript {
				primaries[i].ReferencePack.ChineseTranscript[si].SpeakerID = "single_spk"
			}
		}
	}
	// CategoryMultiSpeaker assets are only 4, so without tags/multi-speakers total multi-speaker is 4 < 6!
	err := benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "multi-speaker count") {
		t.Errorf("expected rejection for <6 multi-speaker count, got %v", err)
	}

	// 2. Burned-in subtitle count < 16
	primaries, reserves = buildValidTestCorpus()
	for i := range primaries {
		if primaries[i].PrimaryCategory != benchmark.CategoryDenseBurnedInSubtitle {
			primaries[i].Tags = nil
			if primaries[i].ReferencePack != nil {
				primaries[i].ReferencePack.HasBurnedInSubtitles = false
				primaries[i].ReferencePack.SubtitleRegions = nil
			}
		}
	}
	// CategoryDenseBurnedInSubtitle has only 4 assets, so burnedInCount is 4 < 16!
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "burned-in dialogue subtitle count") {
		t.Errorf("expected rejection for <16 burned-in count, got %v", err)
	}

	// 3. No-dub count > 1
	primaries, reserves = buildValidTestCorpus()
	primaries[0].NoDub = true
	primaries[1].NoDub = true
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "no-dub visual count") {
		t.Errorf("expected rejection for >1 no-dub visual cases, got %v", err)
	}

	// 4. Aspect ratio majority 9:16 violated (< 13)
	primaries, reserves = buildValidTestCorpus()
	for i := range 15 {
		primaries[i].Preflight.Width = 1080
		primaries[i].Preflight.Height = 1440 // 3:4 portrait
	}
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "is not majority") {
		t.Errorf("expected rejection when 9:16 is not majority, got %v", err)
	}

	// 5. At least two 3:4 portrait videos violated (< 2)
	primaries, reserves = buildValidTestCorpus()
	for i := range primaries {
		primaries[i].Preflight.Width = 1080
		primaries[i].Preflight.Height = 1920 // All 9:16, 0 3:4!
	}
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "3:4 portrait count") {
		t.Errorf("expected rejection when 3:4 portrait count < 2, got %v", err)
	}

	// 6. At least two videos >= 90s violated (< 2)
	primaries, reserves = buildValidTestCorpus()
	primaries[22].Preflight.DurationMs = 50000
	primaries[23].Preflight.DurationMs = 60000
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "videos >=90s count") {
		t.Errorf("expected rejection when videos >=90s count < 2, got %v", err)
	}
}

func TestQualityCorpus_PreflightEnvelopeRejection(t *testing.T) {
	cases := []struct {
		name        string
		modify      func(p *domain.PreflightReport)
		expectedErr string
	}{
		{
			name: "invalid container",
			modify: func(p *domain.PreflightReport) {
				p.ContainerValid = false
				p.Errors = []string{"corrupted moov atom"}
			},
			expectedErr: "invalid container",
		},
		{
			name: "fingerprint mismatch",
			modify: func(p *domain.PreflightReport) {
				p.FingerprintMatch = false
			},
			expectedErr: "fingerprint match failure",
		},
		{
			name: "zero duration",
			modify: func(p *domain.PreflightReport) {
				p.DurationMs = 0
				p.DurationSec = 0
			},
			expectedErr: "duration must be positive",
		},
		{
			name: "invalid dimensions",
			modify: func(p *domain.PreflightReport) {
				p.Width = 0
				p.Height = 0
			},
			expectedErr: "video dimensions invalid",
		},
		{
			name: "missing video codec",
			modify: func(p *domain.PreflightReport) {
				p.VideoCodec = ""
			},
			expectedErr: "video codec missing",
		},
		{
			name: "missing audio codec for dubbed asset",
			modify: func(p *domain.PreflightReport) {
				p.AudioCodec = ""
			},
			expectedErr: "audio codec missing",
		},
		{
			name: "missing normalized audio SHA256",
			modify: func(p *domain.PreflightReport) {
				p.NormalizedAudioSHA256 = ""
			},
			expectedErr: "normalized 16kHz audio SHA-256 missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := makeValidPreflight("test_asset", 1080, 1920, 20000, false)
			tc.modify(&pf)
			err := benchmark.ValidatePreflightEnvelope(&pf, false)
			if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
				t.Errorf("expected error containing %q, got %v", tc.expectedErr, err)
			}
		})
	}
}

func TestQualityCorpus_AnnotationPackContractRejection(t *testing.T) {
	cases := []struct {
		name        string
		modify      func(pack *benchmark.ReferenceAnnotationPack)
		expectedErr string
	}{
		{
			name: "missing pack_id",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.PackID = ""
			},
			expectedErr: "missing pack_id",
		},
		{
			name: "missing secondary adjudication person",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.Adjudication.AdjudicatedBy = ""
			},
			expectedErr: "adjudicated_by is required",
		},
		{
			name: "zero adjudication timestamp",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.Adjudication.AdjudicatedAt = time.Time{}
			},
			expectedErr: "adjudicated_at timestamp is required",
		},
		{
			name: "unapproved adjudication status",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.Adjudication.Status = "PENDING_REVIEW"
			},
			expectedErr: "adjudication status must be APPROVED or VERIFIED",
		},
		{
			name: "empty transcript for non-no-dub video",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.ChineseTranscript = nil
			},
			expectedErr: "must have at least one speech segment",
		},
		{
			name: "invalid segment time window",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.ChineseTranscript[0].EndMs = p.ChineseTranscript[0].StartMs // end <= start
			},
			expectedErr: "must be strictly greater than start_ms",
		},
		{
			name: "transcript out of order",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.ChineseTranscript[1].StartMs = 100 // starts before seg[0] which starts at 500
			},
			expectedErr: "starts before previous segment",
		},
		{
			name: "critical item with invalid type",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.CriticalItems[0].Type = "unsupported_type"
			},
			expectedErr: "invalid type",
		},
		{
			name: "missing semantic reference for segment",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.SemanticReferences = p.SemanticReferences[:1] // drops seg_02 semantic ref
			},
			expectedErr: "missing semantic reference",
		},
		{
			name: "burned-in true but empty subtitle regions",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.HasBurnedInSubtitles = true
				p.SubtitleRegions = nil
			},
			expectedErr: "has_burned_in_subtitles is true but subtitle_regions is empty",
		},
		{
			name: "subtitle region invalid box coordinates",
			modify: func(p *benchmark.ReferenceAnnotationPack) {
				p.HasBurnedInSubtitles = true
				p.SubtitleRegions[0].Box.Width = -0.5
			},
			expectedErr: "invalid box coordinates",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pack := makeValidAnnotationPack("asset_test", true, true, false)
			tc.modify(pack)
			err := benchmark.ValidateReferenceAnnotationPack(pack, false)
			if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
				t.Errorf("expected error containing %q, got %v", tc.expectedErr, err)
			}
		})
	}
}

func TestQualityCorpus_ReserveGovernance(t *testing.T) {
	primaries, reserves := buildValidTestCorpus()
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", time.Now().UTC(), primaries, reserves)
	if err != nil {
		t.Fatalf("freeze failed: %v", err)
	}

	// 1. Mid-run replacement rejection (isRunStarted == true)
	_, _, err = manifest.ReplacePrimaryWithReserve(
		"zh_pri_01",
		"zh_res_01",
		"Source disappeared upstream",
		"operator_01",
		time.Now().UTC(),
		true, // isRunStarted!
		"bms_test_session",
	)
	if err == nil || !strings.Contains(err.Error(), "mid-run replacement rejected") {
		t.Errorf("expected mid-run replacement rejection, got %v", err)
	}

	// 2. Category mismatch rejection (Clean Single-Speaker replaced by Fast-Dense reserve)
	_, _, err = manifest.ReplacePrimaryWithReserve(
		"zh_pri_01", // Clean Single-Speaker
		"zh_res_03", // Fast-Dense
		"Source video audio damaged",
		"operator_01",
		time.Now().UTC(),
		false,
		"bms_test_session",
	)
	if err == nil || !strings.Contains(err.Error(), "replacement must be 1-for-1 within the same primary category") {
		t.Errorf("expected category mismatch rejection, got %v", err)
	}

	// 3. Reason required
	_, _, err = manifest.ReplacePrimaryWithReserve(
		"zh_pri_01",
		"zh_res_01",
		"", // empty reason
		"operator_01",
		time.Now().UTC(),
		false,
		"bms_test_session",
	)
	if err == nil || !strings.Contains(err.Error(), "replacement reason is required") {
		t.Errorf("expected empty reason rejection, got %v", err)
	}

	// 4. Operator required
	_, _, err = manifest.ReplacePrimaryWithReserve(
		"zh_pri_01",
		"zh_res_01",
		"Valid reason",
		"", // empty operator
		time.Now().UTC(),
		false,
		"bms_test_session",
	)
	if err == nil || !strings.Contains(err.Error(), "operator ID is required") {
		t.Errorf("expected empty operator rejection, got %v", err)
	}

	// 5. Successful pre-run replacement
	priorDigest := manifest.ManifestDigest
	record, newManifest, err := manifest.ReplacePrimaryWithReserve(
		"zh_pri_01",
		"zh_res_01",
		"Source video taken private on Douyin",
		"operator_lead_01",
		time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC),
		false,
		"bms_session_468b3dc",
	)
	if err != nil {
		t.Fatalf("valid replacement failed: %v", err)
	}

	if record.OldAssetID != "zh_pri_01" || record.NewAssetID != "zh_res_01" {
		t.Errorf("record identities mismatch: old=%s new=%s", record.OldAssetID, record.NewAssetID)
	}
	if record.PriorManifestDigest != priorDigest {
		t.Errorf("expected record prior digest %s, got %s", priorDigest, record.PriorManifestDigest)
	}
	if record.NewManifestDigest == "" || record.NewManifestDigest == priorDigest {
		t.Errorf("expected new distinct manifest digest, got %s", record.NewManifestDigest)
	}
	if newManifest.ManifestDigest != record.NewManifestDigest {
		t.Errorf("manifest digest mismatch with record: %s != %s", newManifest.ManifestDigest, record.NewManifestDigest)
	}
	if len(newManifest.ReplacementHistory) != 1 {
		t.Errorf("expected 1 replacement history entry, got %d", len(newManifest.ReplacementHistory))
	}
	if len(newManifest.ReserveAssets) != 6 {
		t.Errorf("expected reserve count reduced to 6, got %d", len(newManifest.ReserveAssets))
	}

	// Verify new manifest digest integrity
	if err := newManifest.VerifyManifestDigest(); err != nil {
		t.Errorf("new manifest digest verification failed: %v", err)
	}
}

func TestQualityCorpus_DeterministicStratifiedAuditPlan(t *testing.T) {
	primaries, reserves := buildValidTestCorpus()
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("freeze failed: %v", err)
	}

	// 1. Generate nominal 28-strata audit plan
	plan1, err := benchmark.GenerateStratifiedAuditPlan(manifest)
	if err != nil {
		t.Fatalf("generate audit plan: %v", err)
	}

	if plan1.TotalStrata != 28 {
		t.Errorf("expected exactly 28 strata, got %d", plan1.TotalStrata)
	}
	if len(plan1.Selections) != 28 {
		t.Errorf("expected 28 selections, got %d", len(plan1.Selections))
	}

	// 2. Determinism and byte-for-byte reproducibility
	plan2, err := benchmark.GenerateStratifiedAuditPlan(manifest)
	if err != nil {
		t.Fatalf("second audit plan generation: %v", err)
	}
	if plan1.PlanDigest != plan2.PlanDigest {
		t.Errorf("plan digest must be deterministic: %s != %s", plan1.PlanDigest, plan2.PlanDigest)
	}

	for i := range plan1.Selections {
		s1 := plan1.Selections[i]
		s2 := plan2.Selections[i]
		if s1.Stratum.Key() != s2.Stratum.Key() {
			t.Errorf("stratum %d key mismatch: %s != %s", i, s1.Stratum.Key(), s2.Stratum.Key())
		}
		if s1.SelectedAssetID != s2.SelectedAssetID {
			t.Errorf("stratum %s selected asset mismatch: %s != %s", s1.Stratum.Key(), s1.SelectedAssetID, s2.SelectedAssetID)
		}
		if s1.SelectedSourceAssetID != s2.SelectedSourceAssetID {
			t.Errorf("stratum %s selected source asset mismatch: %s != %s", s1.Stratum.Key(), s1.SelectedSourceAssetID, s2.SelectedSourceAssetID)
		}
		if s1.SelectionRankHex != s2.SelectionRankHex {
			t.Errorf("stratum %s rank hex mismatch", s1.Stratum.Key())
		}
	}

	// 3. Test SelectPassAudits with automated PASS candidates
	var candidates []benchmark.PassAuditCandidate
	for _, a := range manifest.PrimaryAssets {
		for _, profile := range benchmark.AuditProfiles {
			for _, lang := range benchmark.AuditTargetLanguages {
				candidates = append(candidates, benchmark.PassAuditCandidate{
					Profile:        profile,
					Category:       a.PrimaryCategory,
					TargetLanguage: lang,
					AssetID:        a.AssetID,
					SourceAssetID:  a.SourceAssetID,
					AutomatedQC:    "PASS",
				})
			}
		}
	}

	execPlan, err := benchmark.SelectPassAudits(manifest.ManifestDigest, candidates, manifest.FrozenAt)
	if err != nil {
		t.Fatalf("select pass audits: %v", err)
	}
	if execPlan.TotalStrata != 28 {
		t.Errorf("expected 28 strata from candidates, got %d", execPlan.TotalStrata)
	}

	// Selected asset from candidates should match nominal plan since all primaries passed
	for _, nominalSel := range plan1.Selections {
		execSel, ok := execPlan.SelectionsByStratum[nominalSel.Stratum.Key()]
		if !ok {
			t.Fatalf("missing stratum in execPlan: %s", nominalSel.Stratum.Key())
		}
		if execSel.SelectedAssetID != nominalSel.SelectedAssetID {
			t.Errorf("stratum %s: expected candidate selection %s, got %s",
				nominalSel.Stratum.Key(), nominalSel.SelectedAssetID, execSel.SelectedAssetID)
		}
		if execSel.SelectedSourceAssetID != nominalSel.SelectedSourceAssetID {
			t.Errorf("stratum %s: expected candidate source asset %s, got %s",
				nominalSel.Stratum.Key(), nominalSel.SelectedSourceAssetID, execSel.SelectedSourceAssetID)
		}
	}
}

func TestQualityCorpus_SessionBinding(t *testing.T) {
	primaries, reserves := buildValidTestCorpus()
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeQualityCorpus("quality_24_videos", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("freeze failed: %v", err)
	}

	// 1. Session initialized without matching corpus digest must reject RecordQualityCorpusManifest
	unboundInput := benchmark.SessionIdentityInput{
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "acquisition_100_urls", Digest: "62ce8541701236f69fb31f39ee724473f09a42b424ea1fef73f323af46d58f2c", EntriesCount: 100},
		},
		ExecutionProfile: "local",
		BuildIdentity:    "unit-test-build-01",
		ConfigSnapshot:   "{}",
		ProviderBaselines: []benchmark.ProviderBaseline{
			{Role: "translation", ProviderID: "qwen_local", SnapshotManifestDigest: "abc123"},
		},
		Environment: benchmark.EnvironmentAttestation{OS: "windows", Arch: "amd64"},
		Operator:    benchmark.OperatorMetadata{OperatorID: "operator_lead"},
	}
	unboundSess, err := benchmark.NewSession(unboundInput)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := unboundSess.RecordQualityCorpusManifest(manifest); err == nil {
		t.Errorf("expected session without quality corpus digest in identity to reject RecordQualityCorpusManifest")
	}

	// 2. Session initialized with matching corpus digest binds successfully
	input := benchmark.SessionIdentityInput{
		CorpusDigests: []benchmark.CorpusDigest{
			{Name: "quality_24_videos", Digest: manifest.ManifestDigest, EntriesCount: 24},
		},
		ExecutionProfile: "local",
		BuildIdentity:    "unit-test-build-01",
		ConfigSnapshot:   "{}",
		ProviderBaselines: []benchmark.ProviderBaseline{
			{Role: "translation", ProviderID: "qwen_local", SnapshotManifestDigest: "abc123"},
		},
		Environment: benchmark.EnvironmentAttestation{OS: "windows", Arch: "amd64"},
		Operator:    benchmark.OperatorMetadata{OperatorID: "operator_lead"},
	}

	sess, err := benchmark.NewSession(input)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// 3. Reject RecordAuditPlan before manifest is bound
	plan, err := benchmark.GenerateStratifiedAuditPlan(manifest)
	if err != nil {
		t.Fatalf("generate audit plan: %v", err)
	}
	if err := sess.RecordAuditPlan(plan); err == nil {
		t.Errorf("expected RecordAuditPlan to fail when no manifest is bound yet")
	}

	// 4. Bind manifest successfully
	if err := sess.RecordQualityCorpusManifest(manifest); err != nil {
		t.Fatalf("record quality corpus manifest: %v", err)
	}
	gotManifest, ok := sess.GetQualityCorpusManifest()
	if !ok || gotManifest == nil {
		t.Fatalf("expected to retrieve bound manifest")
	}
	if gotManifest.ManifestDigest != manifest.ManifestDigest {
		t.Errorf("manifest digest mismatch in session: %s != %s", gotManifest.ManifestDigest, manifest.ManifestDigest)
	}

	// 5. Audit plan binding
	if err := sess.RecordAuditPlan(plan); err != nil {
		t.Fatalf("record audit plan: %v", err)
	}
	gotPlan, ok := sess.GetAuditPlan()
	if !ok || gotPlan == nil {
		t.Fatalf("expected to retrieve bound audit plan")
	}
	if gotPlan.PlanDigest != plan.PlanDigest {
		t.Errorf("plan digest mismatch in session: %s != %s", gotPlan.PlanDigest, plan.PlanDigest)
	}

	// 6. Corpus digest mismatch rejection
	mismatchedManifest := *manifest
	mismatchedManifest.ManifestDigest = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if err := sess.RecordQualityCorpusManifest(&mismatchedManifest); err == nil {
		t.Errorf("expected session to reject mismatched corpus digest")
	}

	// 7. Audit plan digest mismatch rejection (tampered plan digest)
	mismatchedPlan := *plan
	mismatchedPlan.PlanDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := sess.RecordAuditPlan(&mismatchedPlan); err == nil {
		t.Errorf("expected session to reject audit plan with tampered plan digest")
	}
}

func TestAuditPlan_SourceAssetIDRanking(t *testing.T) {
	manifestDigest := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	stratum := benchmark.AuditStratum{
		Profile:        "local",
		Category:       benchmark.CategoryCleanSingleSpeaker,
		TargetLanguage: "vi",
	}

	// 1. Prove ranking hash uses SourceAssetID
	hashA := benchmark.ComputeRankHash(manifestDigest, stratum.Profile, string(stratum.Category), stratum.TargetLanguage, "source_asset_uuid_01")
	hashB := benchmark.ComputeRankHash(manifestDigest, stratum.Profile, string(stratum.Category), stratum.TargetLanguage, "source_asset_uuid_02")
	if hashA == hashB {
		t.Fatalf("different source asset IDs produced identical hash")
	}

	// Changing local AssetID must NOT change ranking hash when SourceAssetID is unchanged
	candidates1 := []benchmark.CandidateAssetRef{
		{AssetID: "local_asset_foo", SourceAssetID: "source_asset_uuid_01"},
		{AssetID: "local_asset_bar", SourceAssetID: "source_asset_uuid_02"},
	}
	candidates2 := []benchmark.CandidateAssetRef{
		{AssetID: "different_local_id_x", SourceAssetID: "source_asset_uuid_01"},
		{AssetID: "different_local_id_y", SourceAssetID: "source_asset_uuid_02"},
	}

	rankings1 := benchmark.RankCandidates(manifestDigest, stratum, candidates1)
	rankings2 := benchmark.RankCandidates(manifestDigest, stratum, candidates2)

	if len(rankings1) != 2 || len(rankings2) != 2 {
		t.Fatalf("expected 2 rankings")
	}
	if rankings1[0].SourceAssetID != rankings2[0].SourceAssetID {
		t.Errorf("selected source asset ID changed when only local asset ID changed: %s != %s",
			rankings1[0].SourceAssetID, rankings2[0].SourceAssetID)
	}
	if rankings1[0].RankHex != rankings2[0].RankHex {
		t.Errorf("rank hash changed when only local asset ID changed: %s != %s",
			rankings1[0].RankHex, rankings2[0].RankHex)
	}
}

func TestAuditPlan_ByteForByteReproducibility(t *testing.T) {
	primaries, reserves := buildValidTestCorpus()
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("freeze failed: %v", err)
	}

	var canonicalBytesList [][]byte
	var serializedBytesList [][]byte
	var planDigests []string

	for range 5 {
		plan, err := benchmark.GenerateStratifiedAuditPlan(manifest)
		if err != nil {
			t.Fatalf("generate audit plan: %v", err)
		}
		can, err := plan.Canonicalize()
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		ser, err := json.Marshal(plan)
		if err != nil {
			t.Fatalf("marshal plan: %v", err)
		}
		canonicalBytesList = append(canonicalBytesList, can)
		serializedBytesList = append(serializedBytesList, ser)
		planDigests = append(planDigests, plan.PlanDigest)
	}

	// Assert 100% byte-for-byte reproducibility across all runs
	for i := 1; i < len(canonicalBytesList); i++ {
		if !bytes.Equal(canonicalBytesList[0], canonicalBytesList[i]) {
			t.Errorf("run %d canonical bytes differed from run 0", i)
		}
		if !bytes.Equal(serializedBytesList[0], serializedBytesList[i]) {
			t.Errorf("run %d serialized bytes differed from run 0", i)
		}
		if planDigests[0] != planDigests[i] {
			t.Errorf("run %d plan digest differed: %s != %s", i, planDigests[0], planDigests[i])
		}
	}
}

func TestQualityCorpus_StrictIdentityValidation(t *testing.T) {
	// 1. Empty SourceAssetID rejected
	primaries, reserves := buildValidTestCorpus()
	primaries[0].SourceAssetID = ""
	err := benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "empty source_asset_id") {
		t.Errorf("expected empty source_asset_id rejection, got %v", err)
	}

	// 2. Empty SourceVideoID rejected
	primaries, reserves = buildValidTestCorpus()
	primaries[0].SourceVideoID = ""
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "empty source_video_id") {
		t.Errorf("expected empty source_video_id rejection, got %v", err)
	}

	// 3. Invalid SHA-256 (not 64-char hex) rejected
	primaries, reserves = buildValidTestCorpus()
	primaries[0].SHA256 = "invalid_short_sha"
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "invalid sha256") {
		t.Errorf("expected invalid sha256 rejection, got %v", err)
	}

	// 4. Mismatched ReferenceAnnotationPack.AssetID rejected
	primaries, reserves = buildValidTestCorpus()
	primaries[0].ReferencePack.AssetID = "different_asset_id"
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "does not match corpus asset_id") {
		t.Errorf("expected reference pack asset_id mismatch rejection, got %v", err)
	}

	// 5. Tampered PackDigest rejected
	primaries, reserves = buildValidTestCorpus()
	primaries[0].ReferencePack.PackDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "pack digest verification failed") {
		t.Errorf("expected tampered pack digest rejection, got %v", err)
	}
}

func TestQualityCorpus_GoldTimingContract(t *testing.T) {
	// 1. Dubbed speech missing word alignments rejected
	primaries, reserves := buildValidTestCorpus()
	primaries[0].ReferencePack.ChineseTranscript[0].Words = nil
	err := benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "missing required word alignments") {
		t.Errorf("expected missing word alignments rejection, got %v", err)
	}

	// 2. Word start time before segment start rejected
	primaries, reserves = buildValidTestCorpus()
	primaries[0].ReferencePack.ChineseTranscript[0].Words[0].StartMs = primaries[0].ReferencePack.ChineseTranscript[0].StartMs - 100
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "outside segment bounds") {
		t.Errorf("expected word outside segment bounds rejection, got %v", err)
	}

	// 3. Word end time after segment end rejected
	primaries, reserves = buildValidTestCorpus()
	words := primaries[0].ReferencePack.ChineseTranscript[0].Words
	words[len(words)-1].EndMs = primaries[0].ReferencePack.ChineseTranscript[0].EndMs + 500
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "outside segment bounds") {
		t.Errorf("expected word end after segment end rejection, got %v", err)
	}

	// 4. Word end time before start time rejected
	primaries, reserves = buildValidTestCorpus()
	primaries[0].ReferencePack.ChineseTranscript[0].Words[0].EndMs = primaries[0].ReferencePack.ChineseTranscript[0].Words[0].StartMs - 50
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "has end_ms") {
		t.Errorf("expected word end < start rejection, got %v", err)
	}

	// 5. Non-monotonic word start times rejected
	primaries, reserves = buildValidTestCorpus()
	primaries[0].ReferencePack.ChineseTranscript[0].Words[0].StartMs = 1200
	primaries[0].ReferencePack.ChineseTranscript[0].Words[0].EndMs = 1400
	primaries[0].ReferencePack.ChineseTranscript[0].Words[1].StartMs = 1000 // >= seg.StartMs (500), but precedes word 0 (1200)
	primaries[0].ReferencePack.ChineseTranscript[0].Words[1].EndMs = 1500
	err = benchmark.ValidateCorpusInputs(primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "out of chronological order") {
		t.Errorf("expected non-monotonic word start times rejection, got %v", err)
	}
}

func TestQualityCorpus_FailClosedFreezeTimestamp(t *testing.T) {
	primaries, reserves := buildValidTestCorpus()

	// 1. Zero frozenAt timestamp fails closed
	_, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", time.Time{}, primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "frozen_at timestamp is required") {
		t.Errorf("expected zero frozen_at rejection, got %v", err)
	}

	// 2. Empty corpus_name fails closed
	frozenAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	_, err = benchmark.FreezeQualityCorpus("", "1.1", frozenAt, primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "corpus_name is required") {
		t.Errorf("expected empty corpus_name rejection, got %v", err)
	}

	// 3. Empty version fails closed
	_, err = benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "", frozenAt, primaries, reserves)
	if err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Errorf("expected empty version rejection, got %v", err)
	}

	// 4. Zero timestamp in ReplacePrimaryWithReserve fails closed
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("freeze failed: %v", err)
	}
	_, _, err = manifest.ReplacePrimaryWithReserve("zh_pri_01", "zh_res_01", "reason", "operator", time.Time{}, false, "bms_01")
	if err == nil || !strings.Contains(err.Error(), "replacement timestamp is required") {
		t.Errorf("expected zero replacement timestamp rejection, got %v", err)
	}
}

func TestRealQualityCorpus_DraftValidation(t *testing.T) {
	corpusPath := os.Getenv("DOUYINIE_QUALITY_CORPUS_FILE")
	if corpusPath == "" {
		t.Skip("skipping real quality corpus draft validation: DOUYINIE_QUALITY_CORPUS_FILE not set")
	}

	data, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("failed to read real quality corpus from %s: %v", corpusPath, err)
	}
	var rawAssets []struct {
		AssetID         string                          `json:"asset_id"`
		SourceVideoID   string                          `json:"source_video_id"`
		SourceAssetID   string                          `json:"source_asset_id"`
		SHA256          string                          `json:"sha256"`
		PrimaryCategory string                          `json:"primary_category"`
		Tags            []string                        `json:"tags"`
		IsReserve       bool                            `json:"is_reserve"`
		Preflight       domain.PreflightReport          `json:"preflight"`
		ReferencePack   *benchmark.ReferenceAnnotationPack `json:"reference_pack"`
	}
	if err := json.Unmarshal(data, &rawAssets); err != nil {
		t.Fatalf("failed to unmarshal real quality corpus: %v", err)
	}

	var primaries []benchmark.QualityCorpusAsset
	var reserves []benchmark.QualityCorpusAsset

	for _, raw := range rawAssets {
		cat, err := benchmark.NormalizeCategory(raw.PrimaryCategory)
		if err != nil {
			t.Fatalf("invalid category %q for asset %s: %v", raw.PrimaryCategory, raw.AssetID, err)
		}
		asset := benchmark.QualityCorpusAsset{
			AssetID:         raw.AssetID,
			SourceVideoID:   raw.SourceVideoID,
			SourceAssetID:   raw.SourceAssetID,
			SHA256:          raw.SHA256,
			PrimaryCategory: cat,
			Tags:            raw.Tags,
			Preflight:       raw.Preflight,
			ReferencePack:   raw.ReferencePack,
			IsReserve:       raw.IsReserve,
		}
		if raw.IsReserve {
			reserves = append(reserves, asset)
		} else {
			primaries = append(primaries, asset)
		}
	}

	if len(primaries) != 24 {
		t.Errorf("expected 24 primary assets, got %d", len(primaries))
	}
	if len(reserves) != 7 {
		t.Errorf("expected 7 reserve assets, got %d", len(reserves))
	}

	// Run Draft Corpus Validation
	if err := benchmark.ValidateDraftCorpusInputs(primaries, reserves); err != nil {
		t.Fatalf("ValidateDraftCorpusInputs failed for real 31 assets: %v", err)
	}
}

func TestRealQualityCorpus_AdjudicatedValidation(t *testing.T) {
	corpusPath := os.Getenv("DOUYINIE_QUALITY_CORPUS_FILE")
	if corpusPath == "" {
		t.Skip("skipping real quality corpus adjudicated validation: DOUYINIE_QUALITY_CORPUS_FILE not set")
	}

	data, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatalf("failed to read real quality corpus from %s: %v", corpusPath, err)
	}
	var rawAssets []struct {
		AssetID         string                             `json:"asset_id"`
		SourceVideoID   string                             `json:"source_video_id"`
		SourceAssetID   string                             `json:"source_asset_id"`
		SHA256          string                             `json:"sha256"`
		PrimaryCategory string                             `json:"primary_category"`
		Tags            []string                           `json:"tags"`
		IsReserve       bool                               `json:"is_reserve"`
		Preflight       domain.PreflightReport             `json:"preflight"`
		ReferencePack   *benchmark.ReferenceAnnotationPack `json:"reference_pack"`
	}
	if err := json.Unmarshal(data, &rawAssets); err != nil {
		t.Fatalf("failed to unmarshal real quality corpus: %v", err)
	}

	var primaries []benchmark.QualityCorpusAsset
	var reserves []benchmark.QualityCorpusAsset

	for _, raw := range rawAssets {
		cat, err := benchmark.NormalizeCategory(raw.PrimaryCategory)
		if err != nil {
			t.Fatalf("invalid category %q for asset %s: %v", raw.PrimaryCategory, raw.AssetID, err)
		}
		asset := benchmark.QualityCorpusAsset{
			AssetID:         raw.AssetID,
			SourceVideoID:   raw.SourceVideoID,
			SourceAssetID:   raw.SourceAssetID,
			SHA256:          raw.SHA256,
			PrimaryCategory: cat,
			Tags:            raw.Tags,
			Preflight:       raw.Preflight,
			ReferencePack:   raw.ReferencePack,
			IsReserve:       raw.IsReserve,
		}
		if raw.IsReserve {
			reserves = append(reserves, asset)
		} else {
			primaries = append(primaries, asset)
		}
	}

	if len(primaries) != 24 {
		t.Errorf("expected 24 primary assets, got %d", len(primaries))
	}
	if len(reserves) != 7 {
		t.Errorf("expected 7 reserve assets, got %d", len(reserves))
	}

	// 1. Full Secondary Adjudication validation (requires APPROVED or VERIFIED)
	if err := benchmark.ValidateCorpusInputs(primaries, reserves); err != nil {
		t.Fatalf("ValidateCorpusInputs failed for real 31 adjudicated assets: %v", err)
	}

	// 2. Freeze Quality Corpus Manifest using the actual adjudication timestamp
	frozenAt := primaries[0].ReferencePack.Adjudication.AdjudicatedAt
	manifest, err := benchmark.FreezeQualityCorpus("douyinie_quality_corpus_24_7", "1.1", frozenAt, primaries, reserves)
	if err != nil {
		t.Fatalf("FreezeQualityCorpus failed: %v", err)
	}
	if err := manifest.VerifyManifestDigest(); err != nil {
		t.Fatalf("VerifyManifestDigest failed: %v", err)
	}

	// 3. Generate and verify deterministic 28-stratum audit plan
	auditPlan, err := benchmark.GenerateStratifiedAuditPlan(manifest)
	if err != nil {
		t.Fatalf("GenerateStratifiedAuditPlan failed: %v", err)
	}
	if auditPlan.TotalStrata != 28 {
		t.Fatalf("expected 28 strata, got %d", auditPlan.TotalStrata)
	}
	if err := auditPlan.VerifyPlanDigest(); err != nil {
		t.Fatalf("VerifyPlanDigest failed: %v", err)
	}

	// 4. Save frozen manifest and plan artifacts alongside corpus file
	corpusDir := filepath.Dir(corpusPath)
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(corpusDir, "quality_corpus_manifest.json"), manifestJSON, 0644)
	}
	planJSON, err := json.MarshalIndent(auditPlan, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(corpusDir, "stratified_audit_plan_28.json"), planJSON, 0644)
	}

	// 5. Verify persistent session binding evidence outside git if session identity evidence path is provided
	sessionEvidencePath := os.Getenv("DOUYINIE_QUALITY_SESSION_IDENTITY_FILE")
	if sessionEvidencePath == "" {
		sessionEvidencePath = os.Getenv("DOUYINIE_QUALITY_SESSION_IDENTITIES_FILE")
	}
	if sessionEvidencePath == "" {
		t.Skip("skipping real quality corpus session validation: DOUYINIE_QUALITY_SESSION_IDENTITY_FILE not set")
	}

	sessionData, err := os.ReadFile(sessionEvidencePath)
	if err != nil {
		t.Fatalf("failed to read session evidence file %s: %v", sessionEvidencePath, err)
	}

	var sessionBundle struct {
		SessionStoreDir string                           `json:"session_store_dir,omitempty"`
		ManifestDigest  string                           `json:"manifest_digest,omitempty"`
		AuditPlanDigest string                           `json:"audit_plan_digest,omitempty"`
		Identities      []benchmark.SessionIdentityInput `json:"identities"`
	}

	if err := json.Unmarshal(sessionData, &sessionBundle); err != nil || len(sessionBundle.Identities) == 0 {
		var rawIdentities []benchmark.SessionIdentityInput
		if errArray := json.Unmarshal(sessionData, &rawIdentities); errArray != nil {
			t.Fatalf("failed to unmarshal session identity evidence from %s: %v", sessionEvidencePath, err)
		}
		sessionBundle.Identities = rawIdentities
	}

	if sessionBundle.ManifestDigest != "" && !strings.EqualFold(sessionBundle.ManifestDigest, manifest.ManifestDigest) {
		t.Fatalf("session evidence manifest digest %s != computed manifest digest %s", sessionBundle.ManifestDigest, manifest.ManifestDigest)
	}
	if sessionBundle.AuditPlanDigest != "" && !strings.EqualFold(sessionBundle.AuditPlanDigest, auditPlan.PlanDigest) {
		t.Fatalf("session evidence audit plan digest %s != computed audit plan digest %s", sessionBundle.AuditPlanDigest, auditPlan.PlanDigest)
	}

	sessionStoreDir := sessionBundle.SessionStoreDir
	if sessionStoreDir == "" {
		sessionStoreDir = filepath.Join(filepath.Dir(sessionEvidencePath), "sessions")
	}

	store, err := benchmark.NewFileStore(sessionStoreDir)
	if err != nil {
		t.Fatalf("NewFileStore failed on %s: %v", sessionStoreDir, err)
	}

	if len(sessionBundle.Identities) == 0 {
		t.Fatalf("session identity evidence in %s contains no identities", sessionEvidencePath)
	}

	for _, idInput := range sessionBundle.Identities {
		prof := idInput.ExecutionProfile
		if prof == "" {
			t.Fatalf("session identity input missing execution_profile: %+v", idInput)
		}

		sessionID, expectedIdentityDigest, err := benchmark.ComputeSessionID(idInput)
		if err != nil {
			t.Fatalf("ComputeSessionID failed for profile %s: %v", prof, err)
		}

		sess, err := store.Resume(idInput)
		if err != nil {
			t.Fatalf("store.Resume failed for profile %s: %v", prof, err)
		}
		if err := sess.RecordQualityCorpusManifest(manifest); err != nil {
			t.Fatalf("RecordQualityCorpusManifest failed for profile %s: %v", prof, err)
		}
		if err := sess.RecordAuditPlan(auditPlan); err != nil {
			t.Fatalf("RecordAuditPlan failed for profile %s: %v", prof, err)
		}
		if err := store.Save(sess); err != nil {
			t.Fatalf("store.Save failed for profile %s: %v", prof, err)
		}

		// Verify re-read from disk
		loaded, err := store.Load(sessionID)
		if err != nil {
			t.Fatalf("store.Load failed for session %s (profile %s): %v", sessionID, prof, err)
		}
		if !strings.EqualFold(loaded.IdentityDigest, expectedIdentityDigest) {
			t.Fatalf("loaded session %s identity digest %s != expected %s", sessionID, loaded.IdentityDigest, expectedIdentityDigest)
		}

		boundManifest, ok := loaded.GetQualityCorpusManifest()
		if !ok || boundManifest == nil || !strings.EqualFold(boundManifest.ManifestDigest, manifest.ManifestDigest) {
			t.Fatalf("loaded session %s missing or mismatched manifest digest (got %v, expected %s)", sessionID, boundManifest, manifest.ManifestDigest)
		}

		boundPlan, ok := loaded.GetAuditPlan()
		if !ok || boundPlan == nil || !strings.EqualFold(boundPlan.PlanDigest, auditPlan.PlanDigest) {
			t.Fatalf("loaded session %s missing or mismatched plan digest (got %v, expected %s)", sessionID, boundPlan, auditPlan.PlanDigest)
		}

		t.Logf("Verified Persistent Session [%s] ID: %s, IdentityDigest: %s", prof, sessionID, loaded.IdentityDigest)
	}

	t.Logf("Adjudicated Quality Corpus Manifest Digest: %s", manifest.ManifestDigest)
	t.Logf("Stratified Audit Plan Digest: %s", auditPlan.PlanDigest)
}
