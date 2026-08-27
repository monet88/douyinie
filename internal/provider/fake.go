package provider

import (
	"context"
	"errors"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
)

// BaseFakeProvider provides shared fields for fake providers.
type BaseFakeProvider struct {
	ProviderID   string
	ProviderType ProviderType
	Policy       PolicyState
	Healthy      bool
	Cap          domain.ProviderCapability
	ModelName    string
	ModelVersion string
}

func (b *BaseFakeProvider) ID() string                            { return b.ProviderID }
func (b *BaseFakeProvider) Type() ProviderType                    { return b.ProviderType }
func (b *BaseFakeProvider) PolicyState() PolicyState              { return b.Policy }
func (b *BaseFakeProvider) IsHealthy() bool                       { return b.Healthy }
func (b *BaseFakeProvider) Capability() domain.ProviderCapability { return b.Cap }
func (b *BaseFakeProvider) ModelInfo() (string, string)           { return b.ModelName, b.ModelVersion }

// FakeASRProvider simulates Qwen3-ASR (1.7B quality attempt / 0.6B fallback).
// Output is controllable per test: RawSegments overrides the default single
// short segment so Seam 1 can drive a long multi-sentence VAD turn.
type FakeASRProvider struct {
	BaseFakeProvider
	TranscribedText string
	RawSegments     []domain.ASRRawSegment
}

func NewFakeASRProvider(id string) *FakeASRProvider {
	return &FakeASRProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeASR,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeASR),
				Languages:      []string{"zh", "en", "vi"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.95,
				MaxConcurrency: 1,
				Features:       []string{"vad_split", "timestamp_alignment"},
			},
			ModelName:    "qwen3-asr",
			ModelVersion: "1.7b",
		},
		TranscribedText: "测试语音输入",
	}
}

// ProduceTranscript implements provider.ASRTranscriptProvider.
// It returns the controllable RawSegments when set; otherwise a single
// VAD-turn segment over TranscribedText (the default short-unit path).
func (p *FakeASRProvider) ProduceTranscript(ctx context.Context, audioPath string) ([]domain.ASRRawSegment, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if len(p.RawSegments) > 0 {
		return p.RawSegments, nil
	}
	text := strings.TrimSpace(p.TranscribedText)
	if text == "" {
		return nil, domain.ErrQualityRejected
	}
	return []domain.ASRRawSegment{{
		StartMs:      0,
		EndMs:        2000,
		Text:         text,
		Confidence:   0.90,
		LanguageCode: "zh",
	}}, nil
}

// produceAlignmentHelper produces a synthetic word-timing alignment for the
// given accepted text when no explicit WordTimings were supplied. It splits
// the text on whitespace into words with a fixed per-word cadence so the
// forced-alignment contract (word timings) is honored deterministically.
func produceAlignmentHelper(text string) []domain.WordTiming {
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return nil
	}
	words := make([]domain.WordTiming, 0, len(tokens))
	start := int64(0)
	for _, tok := range tokens {
		w := domain.WordTiming{
			Word:       tok,
			StartMs:    start,
			EndMs:      start + 300,
			Confidence: 0.9,
		}
		words = append(words, w)
		start += 350
	}
	return words
}

// NewFakeASRProvider06B simulates the Qwen3-ASR 0.6B fallback model
// (lower quality score, same stage contract) for policy-fallback routing tests.
func NewFakeASRProvider06B(id string) *FakeASRProvider {
	return &FakeASRProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeASR,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeASR),
				Languages:      []string{"zh", "en", "vi"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.88,
				MaxConcurrency: 1,
				Features:       []string{"vad_split", "timestamp_alignment"},
			},
			ModelName:    "qwen3-asr",
			ModelVersion: "0.6b",
		},
		TranscribedText: "测试语音输入",
	}
}

// FakeAlignerProvider simulates Qwen3-ForcedAligner.
// WordTimings is controllable per test; when empty, a default synthetic
// alignment is produced by the test driver.
type FakeAlignerProvider struct {
	BaseFakeProvider
	WordTimings []domain.WordTiming
}

func NewFakeAlignerProvider(id string) *FakeAlignerProvider {
	return &FakeAlignerProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeAligner,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeAligner),
				Languages:      []string{"zh", "en", "vi"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.95,
				MaxConcurrency: 1,
				Features:       []string{"phoneme_alignment", "word_alignment"},
			},
			ModelName:    "qwen3-aligner",
			ModelVersion: "1.0.0",
		},
	}
}

// ProduceAlignment implements provider.AlignWordProvider.
// It returns the controllable WordTimings when set; otherwise a synthetic
// alignment derived from the accepted text via produceAlignmentHelper.
func (p *FakeAlignerProvider) ProduceAlignment(ctx context.Context, audioPath string, text string) ([]domain.WordTiming, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if len(p.WordTimings) > 0 {
		return p.WordTimings, nil
	}
	words := produceAlignmentHelper(text)
	if len(words) == 0 {
		return nil, domain.ErrQualityRejected
	}
	return words, nil
}

// FakeTTSProvider simulates VieNeu-TTS or CosyVoice3 with controllable duration.
type FakeTTSProvider struct {
	BaseFakeProvider
	DurationMs int64
}

func NewFakeTTSProvider(id string, durationMs int64) *FakeTTSProvider {
	lang := "vi"
	if id == "fake_kokoro_tts_en" {
		lang = "en"
	}
	return &FakeTTSProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeTTS,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeTTS),
				Languages:      []string{lang},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.92,
				MaxConcurrency: 1,
				Features:       []string{"zero_overrun_fit", "streaming"},
			},
			ModelName:    id,
			ModelVersion: "1.0.0",
		},
		DurationMs: durationMs,
	}
}

// FakeSeparatorProvider simulates python-audio-separator / UVR / Demucs.
type FakeSeparatorProvider struct {
	BaseFakeProvider
}

func NewFakeSeparatorProvider(id string) *FakeSeparatorProvider {
	return &FakeSeparatorProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeSeparator,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeSeparator),
				Languages:      []string{"*"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.94,
				MaxConcurrency: 1,
				Features:       []string{"vocal_extraction", "bgm_preservation"},
			},
			ModelName:    "uvr-mdx-net",
			ModelVersion: "v3",
		},
	}
}

// FakeOCRProvider simulates OCR text extraction and TextRegionPlan generation.
type FakeOCRProvider struct {
	BaseFakeProvider
}

func NewFakeOCRProvider(id string) *FakeOCRProvider {
	return &FakeOCRProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeOCR,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeOCR),
				Languages:      []string{"zh", "en", "vi"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.90,
				MaxConcurrency: 1,
				Features:       []string{"region_classification", "fit_content_geometry"},
			},
			ModelName:    "paddleocr",
			ModelVersion: "v4",
		},
	}
}

// FakeTranslationProvider simulates translation service into VI / EN.
type FakeTranslationProvider struct {
	BaseFakeProvider
}

func NewFakeTranslationProvider(id string) *FakeTranslationProvider {
	return &FakeTranslationProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeTranslation,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeTranslation),
				Languages:      []string{"vi", "en"},
				ExecutionTier:  "hybrid",
				CostPerUnit:    0.001,
				QualityScore:   0.96,
				MaxConcurrency: 4,
				Features:       []string{"shorten_first_adaptation", "contextual_translation"},
			},
			ModelName:    "llm-translator",
			ModelVersion: "1.0",
		},
	}
}

// NewSeam1FakeRegistry sets up a deterministic mock provider registry for Seam 1 acceptance testing.
func NewSeam1FakeRegistry() *Registry {
	reg := NewRegistry()
	_ = reg.Register(NewFakeASRProvider("fake_qwen3_asr"))
	_ = reg.Register(NewFakeASRProvider06B("fake_qwen3_asr_06b"))
	_ = reg.Register(NewFakeAlignerProvider("fake_qwen3_aligner"))
	_ = reg.Register(NewFakeTTSProvider("fake_vieneu_tts_vi", 1500))
	_ = reg.Register(NewFakeTTSProvider("fake_kokoro_tts_en", 1400))
	_ = reg.Register(NewFakeSeparatorProvider("fake_uvr_separator"))
	_ = reg.Register(NewFakeOCRProvider("fake_paddle_ocr"))
	_ = reg.Register(NewFakeTranslationProvider("fake_llm_translator"))

	// Blocked provider (for testing policy-before-health: Healthy=true, but Policy=BLOCKED)
	_ = reg.Register(&FakeTTSProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   "fake_indextts2_blocked",
			ProviderType: TypeTTS,
			Policy:       PolicyBlocked,
			Healthy:      true, // healthy but blocked!
			Cap: domain.ProviderCapability{
				Stage:          string(TypeTTS),
				Languages:      []string{"vi", "en", "zh"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.99, // high quality but blocked!
				MaxConcurrency: 1,
				Features:       []string{"zero_shot_clone"},
			},
			ModelName:    "indextts2",
			ModelVersion: "2.0",
		},
		DurationMs: 1200,
	})

	// Consent-required provider
	_ = reg.Register(&FakeTTSProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   "fake_cloud_tts_consent",
			ProviderType: TypeTTS,
			Policy:       PolicyRequiresExplicitConsent,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeTTS),
				Languages:      []string{"vi", "en"},
				ExecutionTier:  "cloud",
				CostPerUnit:    0.05,
				QualityScore:   0.98,
				MaxConcurrency: 4,
				Features:       []string{"high_fidelity", "streaming"},
			},
			ModelName:    "cloud-voice",
			ModelVersion: "v1",
		},
		DurationMs: 1300,
	})

	// Authorization-required provider (for testing credential-backed authorization seam)
	_ = reg.Register(&FakeASRProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   "fake_auth_cloud_asr",
			ProviderType: TypeASR,
			Policy:       PolicyRequiresAuthorization,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeASR),
				Languages:      []string{"zh", "vi", "en"},
				ExecutionTier:  "cloud",
				CostPerUnit:    0.02,
				QualityScore:   0.98,
				MaxConcurrency: 8,
				Features:       []string{"enterprise_asr", "custom_vocabulary"},
			},
			ModelName:    "cloud-asr-enterprise",
			ModelVersion: "v2",
		},
		TranscribedText: "企业级语音识别输出",
	})

	return reg
}
