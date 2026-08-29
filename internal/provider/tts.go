package provider

import (
	"context"

	"github.com/monet88/douyinie/internal/domain"
)

// TTSSynthesisRequest encapsulates the parameters needed to synthesize speech for one segment.
type TTSSynthesisRequest struct {
	RunID          string              `json:"run_id"`
	AssetID        string              `json:"asset_id"`
	SegmentIndex   int                 `json:"segment_index"`
	SpeakerID      string              `json:"speaker_id"`
	Text           string              `json:"text"`
	Language       string              `json:"language"` // "vi", "en"
	Voice          domain.VoiceProfile `json:"voice"`
	Speed          float64             `json:"speed"` // Speed factor (1.0 default)
	SlotDurationMs int64               `json:"slot_duration_ms"`
	UsableSlotMs   int64               `json:"usable_slot_ms"`
	AttemptNumber  int                 `json:"attempt_number"`
}

// TTSSynthesisResult encapsulates the synthesized audio output.
type TTSSynthesisResult struct {
	AudioData           []byte `json:"-"`
	AudioSHA256         string `json:"audio_sha256"`
	AudioCASPath        string `json:"audio_cas_path,omitempty"`
	SampleRate          int    `json:"sample_rate"`
	Channels            int    `json:"channels"`
	Format              string `json:"format"` // "wav"
	ProviderID          string `json:"provider_id"`
	ModelName           string `json:"model_name"`
	ModelVersion        string `json:"model_version"`
	PredictedDurationMs int64  `json:"predicted_duration_ms"`
	MeasuredDurationMs  int64  `json:"measured_duration_ms"` // probed waveform duration
}

// TTSProvider defines the capability interface for text-to-speech providers.
type TTSProvider interface {
	// SynthesizeSpeech generates synthesized audio for the given request.
	SynthesizeSpeech(ctx context.Context, req TTSSynthesisRequest) (*TTSSynthesisResult, error)
	// VoiceCatalog returns the list of voice profiles supported by this provider.
	VoiceCatalog() []domain.VoiceProfile
}

// DefaultPresetVoices returns standard default preset voices by language.
func DefaultPresetVoices(lang string) []domain.VoiceProfile {
	switch lang {
	case "vi":
		return []domain.VoiceProfile{
			{
				ID:         "vieneu_vi_female_1",
				ProviderID: "vieneu_tts_vi",
				VoiceID:    "vi_female_natural",
				Name:       "VieNeu Nữ (Tự nhiên)",
				Language:   "vi",
				Gender:     "female",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "natural_warm",
			},
			{
				ID:         "vieneu_vi_male_1",
				ProviderID: "vieneu_tts_vi",
				VoiceID:    "vi_male_deep",
				Name:       "VieNeu Nam (Trầm ấm)",
				Language:   "vi",
				Gender:     "male",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "deep_resonant",
			},
			{
				ID:         "cosyvoice3_vi_female_1",
				ProviderID: "cosyvoice3_tts",
				VoiceID:    "cosy_vi_f1",
				Name:       "CosyVoice3 VI Nữ (Fit)",
				Language:   "vi",
				Gender:     "female",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "expressive",
			},
		}
	case "en":
		return []domain.VoiceProfile{
			{
				ID:         "kokoro_en_female_1",
				ProviderID: "kokoro_tts_en",
				VoiceID:    "en_heart",
				Name:       "Kokoro EN Female (Heart)",
				Language:   "en",
				Gender:     "female",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "clear_friendly",
			},
			{
				ID:         "kokoro_en_male_1",
				ProviderID: "kokoro_tts_en",
				VoiceID:    "en_deep",
				Name:       "Kokoro EN Male (Deep)",
				Language:   "en",
				Gender:     "male",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "authoritative",
			},
			{
				ID:         "cosyvoice3_en_female_1",
				ProviderID: "cosyvoice3_tts",
				VoiceID:    "cosy_en_f1",
				Name:       "CosyVoice3 EN Female (Fit)",
				Language:   "en",
				Gender:     "female",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "natural_clone",
			},
		}
	default:
		return nil
	}
}
