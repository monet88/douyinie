package provider

// BaseFakeProvider provides shared fields for fake providers.
type BaseFakeProvider struct {
	ProviderID   string
	ProviderType ProviderType
	Policy       PolicyState
	Healthy      bool
}

func (b *BaseFakeProvider) ID() string               { return b.ProviderID }
func (b *BaseFakeProvider) Type() ProviderType       { return b.ProviderType }
func (b *BaseFakeProvider) PolicyState() PolicyState { return b.Policy }
func (b *BaseFakeProvider) IsHealthy() bool          { return b.Healthy }

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
		},
	}
}

// FakeTTSProvider simulates VieNeu-TTS or CosyVoice3 with controllable duration.
type FakeTTSProvider struct {
	BaseFakeProvider
	DurationMs int64
}

func NewFakeTTSProvider(id string, durationMs int64) *FakeTTSProvider {
	return &FakeTTSProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeTTS,
			Policy:       PolicyAllowed,
			Healthy:      true,
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
	return reg
}
