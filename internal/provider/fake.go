package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/worker"
)

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
func (p *FakeASRProvider) ProduceTranscript(ctx context.Context, audio worker.ArtifactRef) ([]domain.ASRRawSegment, error) {
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
func (p *FakeAlignerProvider) ProduceAlignment(ctx context.Context, audio worker.ArtifactRef, text string) ([]domain.WordTiming, error) {
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
	DurationMs        int64
	CustomDurations   map[int]int64
	SpeedFitEnabled   bool
	InjectError       error
	CustomPredictedMs int64
	CustomVoices      []domain.VoiceProfile
	Invocations       int
}

func NewFakeTTSProvider(id string, durationMs int64) *FakeTTSProvider {
	lang := "vi"
	if strings.Contains(id, "_en") {
		lang = "en"
	}
	languages := []string{lang}
	if strings.Contains(id, "cosyvoice3") || strings.Contains(id, "chatterbox") {
		languages = []string{"vi", "en"}
	}

	p := &FakeTTSProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeTTS,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeTTS),
				Languages:      languages,
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.92,
				MaxConcurrency: 1,
				Features:       []string{"zero_overrun_fit", "streaming"},
			},
			ModelName:    id,
			ModelVersion: "1.0.0",
		},
		DurationMs:      durationMs,
		CustomDurations: make(map[int]int64),
	}
	if strings.Contains(id, "cosyvoice3") {
		p.SpeedFitEnabled = true
		p.Cap.Features = append(p.Cap.Features, "measured_duration_speed_fit", "multi_pass_lane")
	}
	return p
}

// VoiceCatalog implements TTSProvider.
func (p *FakeTTSProvider) VoiceCatalog() []domain.VoiceProfile {
	if len(p.CustomVoices) > 0 {
		return p.CustomVoices
	}
	var voices []domain.VoiceProfile
	for _, l := range p.Cap.Languages {
		for _, v := range DefaultPresetVoices(l) {
			if v.ProviderID == p.ProviderID || (p.ProviderID == "fake_vieneu_tts_vi" && v.ProviderID == "vieneu_tts_vi") ||
				(p.ProviderID == "fake_kokoro_tts_en" && v.ProviderID == "kokoro_tts_en") ||
				(p.ProviderID == "fake_cosyvoice3_tts" && v.ProviderID == "cosyvoice3_tts") {
				v.ProviderID = p.ProviderID
				voices = append(voices, v)
			}
		}
	}
	if len(voices) == 0 {
		voices = append(voices, domain.VoiceProfile{
			ID:         p.ProviderID + "_voice_1",
			ProviderID: p.ProviderID,
			VoiceID:    "voice_1",
			Name:       p.ProviderID + " Voice 1",
			Language:   p.Cap.Languages[0],
			Gender:     "female",
			Pitch:      1.0,
			Speed:      1.0,
		})
	}
	return voices
}

// SynthesizeSpeech implements TTSProvider.
func (p *FakeTTSProvider) SynthesizeSpeech(ctx context.Context, req TTSSynthesisRequest) (*TTSSynthesisResult, error) {
	p.Invocations++
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if p.InjectError != nil {
		return nil, p.InjectError
	}
	durMs := p.DurationMs
	if p.CustomDurations != nil {
		if d, ok := p.CustomDurations[req.SegmentIndex]; ok && d > 0 {
			durMs = d
		}
	}
	if durMs <= 0 {
		// default based on text length (~200ms per word or 1000ms base)
		words := strings.Fields(req.Text)
		if len(words) > 0 {
			durMs = int64(len(words) * 220)
		} else {
			durMs = 1000
		}
	}

	// Speed adjustment
	if req.Speed > 0 && req.Speed != 1.0 {
		durMs = int64(float64(durMs) / req.Speed)
		if durMs < 100 {
			durMs = 100
		}
	}

	wavBytes := media.GeneratePCM16WAV(16000, 1, durMs)
	sum := sha256.Sum256(wavBytes)
	shaStr := hex.EncodeToString(sum[:])

	predictedMs := durMs
	if p.CustomPredictedMs > 0 {
		predictedMs = p.CustomPredictedMs
	}

	return &TTSSynthesisResult{
		AudioData:           wavBytes,
		AudioSHA256:         shaStr,
		SampleRate:          16000,
		Channels:            1,
		Format:              "wav",
		ProviderID:          p.ProviderID,
		ModelName:           p.ModelName,
		ModelVersion:        p.ModelVersion,
		PredictedDurationMs: predictedMs,
		MeasuredDurationMs:  durMs,
	}, nil
}

// FakeSeparatorProvider simulates python-audio-separator / UVR / Demucs.
type FakeSeparatorProvider struct {
	BaseFakeProvider
	InjectError error
}

func NewFakeSeparatorProvider(id string) *FakeSeparatorProvider {
	return NewFakeSeparatorProviderWithModel(id, "UVR-MDX-NET-Inst_HQ_4.onnx", "v3", 0.94)
}

func NewFakeSeparatorProviderWithModel(id, modelName, modelVer string, quality float64) *FakeSeparatorProvider {
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
				QualityScore:   quality,
				MaxConcurrency: 1,
				Features:       []string{"vocal_extraction", "bgm_preservation"},
			},
			ModelName:    modelName,
			ModelVersion: modelVer,
		},
	}
}

func (p *FakeSeparatorProvider) SeparateStems(ctx context.Context, req SeparationRequest) (*SeparationResult, error) {
	if p.InjectError != nil {
		return nil, p.InjectError
	}
	durationMs := int64(10000)
	vocals := media.GeneratePCM16WAV(16000, 1, durationMs)
	bg := media.GeneratePCM16WAV(16000, 1, durationMs)
	return &SeparationResult{
		ProviderID:    p.ProviderID,
		ModelName:     p.ModelName,
		ModelVersion:  p.ModelVersion,
		VocalsWAV:     vocals,
		BackgroundWAV: bg,
		DurationMs:    durationMs,
		SampleRate:    16000,
		Channels:      1,
	}, nil
}

// FakeOCRProvider simulates OCR text extraction and TextRegionPlan generation.
type FakeOCRProvider struct {
	BaseFakeProvider
	CustomDetections  []RawTextDetection
	FrameWidth        int
	FrameHeight       int
	FrameSampleStepMs int64
	InjectError       error
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
				Languages:      []string{"*"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.90,
				MaxConcurrency: 1,
				Features:       []string{"region_classification", "fit_content_geometry"},
			},
			ModelName:    "paddleocr",
			ModelVersion: "v4",
		},
		FrameWidth:        1080,
		FrameHeight:       1920,
		FrameSampleStepMs: 500,
	}
}

func (p *FakeOCRProvider) DetectRegions(ctx context.Context, req OCRRequest) (*OCRResult, error) {
	if p.InjectError != nil {
		return nil, p.InjectError
	}

	w := p.FrameWidth
	if w <= 0 {
		w = 1080
	}
	h := p.FrameHeight
	if h <= 0 {
		h = 1920
	}
	stepMs := p.FrameSampleStepMs
	if stepMs <= 0 {
		stepMs = 500
	}

	dets := p.CustomDetections
	if dets == nil {
		// Default multi-role synthetic scenario for deterministic Seam 1 testing
		dets = []RawTextDetection{
			// Frame 0: Watermark + Brand + Subtitle line 1 + UI Export button
			{
				FrameIndex:  0,
				TimestampMs: 0,
				Text:        "抖音号: douyin888",
				Confidence:  0.95,
				Box:         domain.BoundingBox{X: 850, Y: 100, Width: 180, Height: 35},
			},
			{
				FrameIndex:  0,
				TimestampMs: 0,
				Text:        "SUPOR",
				Confidence:  0.98,
				Box:         domain.BoundingBox{X: 50, Y: 60, Width: 120, Height: 40},
			},
			{
				FrameIndex:  0,
				TimestampMs: 0,
				Text:        "导出",
				Confidence:  0.94,
				Box:         domain.BoundingBox{X: 960, Y: 80, Width: 70, Height: 35},
			},
			{
				FrameIndex:  0,
				TimestampMs: 0,
				Text:        "Chào mừng bạn đến với kênh nấu ăn",
				Confidence:  0.92,
				Box:         domain.BoundingBox{X: 180, Y: 1550, Width: 720, Height: 65},
			},
			// Frame 1: Subtitle line 1 continues (tracking test) + Semantic step 1 badge
			{
				FrameIndex:  1,
				TimestampMs: 500,
				Text:        "Chào mừng bạn đến với kênh nấu ăn",
				Confidence:  0.93,
				Box:         domain.BoundingBox{X: 180, Y: 1550, Width: 720, Height: 65},
			},
			{
				FrameIndex:  1,
				TimestampMs: 500,
				Text:        "Bước 1: Chuẩn bị nguyên liệu",
				Confidence:  0.90,
				Box:         domain.BoundingBox{X: 120, Y: 350, Width: 450, Height: 55},
			},
			// Frame 2: Subtitle missing on frame 2 to test interpolation; appears back on frame 3
			{
				FrameIndex:  2,
				TimestampMs: 1000,
				Text:        "Bước 1: Chuẩn bị nguyên liệu",
				Confidence:  0.91,
				Box:         domain.BoundingBox{X: 120, Y: 350, Width: 450, Height: 55},
			},
			// Frame 3: Subtitle line 1 tracked again (gap at Frame 2 should be interpolated)
			{
				FrameIndex:  3,
				TimestampMs: 1500,
				Text:        "Chào mừng bạn đến với kênh nấu ăn",
				Confidence:  0.92,
				Box:         domain.BoundingBox{X: 180, Y: 1550, Width: 720, Height: 65},
			},
		}
	}

	return &OCRResult{
		ProviderID:        p.ProviderID,
		ModelName:         p.ModelName,
		ModelVersion:      p.ModelVersion,
		FrameWidth:        w,
		FrameHeight:       h,
		FrameSampleStepMs: stepMs,
		Detections:        dets,
	}, nil
}

// FakeTranslationProvider simulates translation service into VI / EN.
type FakeTranslationProvider struct {
	BaseFakeProvider
	CustomTranslations map[string]string
	CustomSegments     []domain.TranslationSegment
	InjectError        error
	CorruptNumbers     bool
	CorruptNegation    bool
	CorruptNames       bool
	CorruptFacts       bool
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
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.98,
				MaxConcurrency: 4,
				Features:       []string{"shorten_first_adaptation", "contextual_translation"},
			},
			ModelName:    "llm-translator",
			ModelVersion: "1.0",
		},
		CustomTranslations: make(map[string]string),
	}
}

// NewFakeTranslationProviderFallback creates a fallback translation provider with lower quality score and local tier.
func NewFakeTranslationProviderFallback(id string) *FakeTranslationProvider {
	return &FakeTranslationProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeTranslation,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeTranslation),
				Languages:      []string{"vi", "en"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.85,
				MaxConcurrency: 1,
				Features:       []string{"rule_translation"},
			},
			ModelName:    "local-translator-fast",
			ModelVersion: "0.5b",
		},
		CustomTranslations: make(map[string]string),
	}
}

// TranslateText implements TextTranslationProvider.
func (p *FakeTranslationProvider) TranslateText(ctx context.Context, req TranslationRequest) (*TranslationResult, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if p.InjectError != nil {
		return nil, p.InjectError
	}
	if len(p.CustomSegments) > 0 {
		return &TranslationResult{
			ProviderID:   p.ProviderID,
			ModelName:    p.ModelName,
			ModelVersion: p.ModelVersion,
			Segments:     p.CustomSegments,
		}, nil
	}

	segments := make([]domain.TranslationSegment, 0, len(req.Segments))
	for _, seg := range req.Segments {
		srcText := seg.SourceText
		var targetText string

		if p.CustomTranslations != nil {
			if t, ok := p.CustomTranslations[srcText]; ok {
				targetText = t
			} else if t, ok := p.CustomTranslations[req.TargetLanguage+":"+srcText]; ok {
				targetText = t
			}
		}

		if targetText == "" {
			targetText = DefaultTranslateHelper(srcText, req.TargetLanguage)
		}

		if p.CorruptNumbers {
			targetText = "Số lượng 999 độ bất thường"
		}
		if p.CorruptNegation {
			if req.TargetLanguage == "vi" {
				targetText = "Hãy cứ làm điều đó đi nhé"
			} else {
				targetText = "Please go ahead and do it"
			}
		}
		if p.CorruptNames {
			if req.TargetLanguage == "vi" {
				targetText = "Người lạ nào đó làm việc này"
			} else {
				targetText = "Some stranger did this"
			}
		}
		if p.CorruptFacts {
			targetText = "   "
		}

		segments = append(segments, domain.TranslationSegment{
			Index:      seg.Index,
			SourceText: srcText,
			TargetText: targetText,
			SpeakerID:  seg.SpeakerID,
			StartMs:    seg.StartMs,
			EndMs:      seg.EndMs,
		})
	}

	return &TranslationResult{
		ProviderID:   p.ProviderID,
		ModelName:    p.ModelName,
		ModelVersion: p.ModelVersion,
		Segments:     segments,
	}, nil
}

// DefaultTranslateHelper provides deterministic translations for common phrases in test fixtures.
func DefaultTranslateHelper(srcText, targetLang string) string {
	trimmed := strings.TrimSpace(srcText)
	isVI := strings.EqualFold(targetLang, "vi")

	switch trimmed {
	case "测试语音输入":
		if isVI {
			return "Kiểm tra đầu vào giọng nói"
		}
		return "Test voice input"
	case "今天天气很好。":
		if isVI {
			return "Hôm nay thời tiết rất tốt."
		}
		return "The weather is very good today."
	case "我们去公园散步吧。":
		if isVI {
			return "Chúng ta đi dạo công viên nhé."
		}
		return "Let's go for a walk in the park."
	case "明天再继续工作。":
		if isVI {
			return "Ngày mai hãy tiếp tục làm việc."
		}
		return "Continue working tomorrow."
	case "今天天气很好。我们去公园散步吧。明天再继续工作。":
		if isVI {
			return "Hôm nay thời tiết rất tốt. Chúng ta đi dạo công viên nhé. Ngày mai hãy tiếp tục làm việc."
		}
		return "The weather is very good today. Let's go for a walk in the park. Continue working tomorrow."
	case "请将温度调至25度，张伟说不要打开窗户。":
		if isVI {
			return "Vui lòng điều chỉnh nhiệt độ đến 25 độ, Trương Vĩ nói không được mở cửa sổ."
		}
		return "Please set the temperature to 25 degrees, Zhang Wei said do not open the window."
	case "SUPOR电饭煲拥有3升容量，煮饭不粘锅。":
		if isVI {
			return "Nồi cơm điện SUPOR có dung tích 3 lít, nấu cơm không dính nồi."
		}
		return "The SUPOR rice cooker has a 3-liter capacity and does not stick to the pot."
	case "步骤1：准备抹茶粉20克，不要加糖。":
		if isVI {
			return "Bước 1: Chuẩn bị 20 gram bột matcha, đừng thêm đường."
		}
		return "Step 1: Prepare 20 grams of matcha powder, do not add sugar."
	default:
		if isVI {
			return "Bản dịch: " + trimmed
		}
		return "Translation: " + trimmed
	}
}

// FakeDiarizationProvider simulates conditional speaker diarization with evidence probe.
type FakeDiarizationProvider struct {
	BaseFakeProvider
	Evidence        *domain.SpeakerEvidence
	ProbeErr        error
	Assignments     []domain.SpeakerAssignment
	VADModelName    string
	VADModelVersion string
}

func NewFakeDiarizationProvider(id string) *FakeDiarizationProvider {
	return &FakeDiarizationProvider{
		BaseFakeProvider: BaseFakeProvider{
			ProviderID:   id,
			ProviderType: TypeDiarizer,
			Policy:       PolicyAllowed,
			Healthy:      true,
			Cap: domain.ProviderCapability{
				Stage:          string(TypeDiarizer),
				Languages:      []string{"zh", "en", "vi"},
				ExecutionTier:  "local",
				CostPerUnit:    0.0,
				QualityScore:   0.95,
				MaxConcurrency: 1,
				Features:       []string{"speaker_diarization", "speaker_evidence"},
			},
			ModelName:    "iic/speech_campplus_sv_zh_en_16k-common_advanced",
			ModelVersion: "v1.0.0",
		},
		VADModelName:    "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
		VADModelVersion: "v2.0.4",
	}
}

func (p *FakeDiarizationProvider) VADModelInfo() (string, string) {
	return p.VADModelName, p.VADModelVersion
}

func (p *FakeDiarizationProvider) ModelDependencies() []ModelDependency {
	if p.VADModelName != "" {
		return []ModelDependency{{
			Name:    p.VADModelName,
			Version: p.VADModelVersion,
			Role:    "vad",
		}}
	}
	return nil
}

func (p *FakeDiarizationProvider) ProbeSpeakerEvidence(ctx context.Context, audio worker.ArtifactRef) (*domain.SpeakerEvidence, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if p.ProbeErr != nil {
		return nil, p.ProbeErr
	}
	if p.Evidence != nil {
		return p.Evidence, nil
	}
	return &domain.SpeakerEvidence{
		HasMultiSpeakerCues: true,
		SpeakerChangeCount:  2,
		Confidence:          0.95,
		Source:              p.ModelName + "@" + p.ModelVersion,
	}, nil
}

func (p *FakeDiarizationProvider) ProduceDiarization(ctx context.Context, audio worker.ArtifactRef) ([]domain.SpeakerAssignment, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	if p.Assignments != nil {
		return p.Assignments, nil
	}
	return []domain.SpeakerAssignment{
		{
			SpeakerID:  "SPEAKER_00",
			Label:      "SPEAKER_00",
			StartMs:    0,
			EndMs:      3200,
			Confidence: 0.95,
		},
		{
			SpeakerID:  "SPEAKER_01",
			Label:      "SPEAKER_01",
			StartMs:    3201,
			EndMs:      5000,
			Confidence: 0.94,
		},
	}, nil
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
	_ = reg.Register(NewFakeSeparatorProviderWithModel("fake_demucs_separator", "htdemucs", "v4", 0.90))
	_ = reg.Register(NewFakeTranslationProvider("fake_llm_translator"))
	_ = reg.Register(NewFakeTranslationProviderFallback("fake_local_translator_fallback"))
	_ = reg.Register(NewFakeDiarizationProvider("fake_campplus_diarizer"))

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
