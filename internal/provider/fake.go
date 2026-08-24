package provider

import "github.com/monet88/douyinie/internal/domain"

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

// FakeASRProvider simulates Qwen3-ASR.
type FakeASRProvider struct {
	BaseFakeProvider
	TranscribedText string
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

// FakeAlignerProvider simulates Qwen3-ForcedAligner.
type FakeAlignerProvider struct {
	BaseFakeProvider
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
