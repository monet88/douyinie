package provider

import (
	"context"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
)

const (
	// VieNeu frozen RC identities (Issue #68)
	VieNeuModelID      = "pnnbao-ump/VieNeu-TTS-v3-Turbo"
	VieNeuModelVersion = "v3.2.9"
	VieNeuModelDigest  = "1278db0090b98ccf23e56f2423857fc9d32a5118"
	VieNeuSDKCommit    = "149ff16a6a50093a0cad1b75d5edf9e9d81d97f4"
	VieNeuProviderID   = "vieneu_tts_vi"

	// Kokoro frozen RC identities (Issue #68)
	KokoroModelID       = "hexgrad/Kokoro-82M"
	KokoroModelVersion  = "v1.0"
	KokoroModelCommit   = "f3ff3571791e39611d31c381e3a41a3af07b4987"
	KokoroSourceCommit  = "dfb907a02bba8152ca444717ca5d78747ccb4bec"
	KokoroCheckpointSHA = "496dba118d1a58f5f3db2efc88dbdc216e0483fc89fe6e47ee1f2c53f18ad1e4"
	KokoroProviderID    = "kokoro_tts_en"
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
// For Vietnamese, it returns the frozen ordered VieNeu v3 Turbo rotation (Issue #68):
// 1. Trúc Ly (female natural), 2. Phạm Tuyên (male natural), 3. Đoan Trang (female), 4. Xuân Vĩnh (male).
// For English, it returns the frozen ordered Kokoro rotation (Issue #68):
// 1. af_heart (female natural), 2. am_michael (male natural), 3. af_bella (female), 4. am_fenrir (male).
// CosyVoice3 remains conditional only and is excluded from default hard-route rotation.
func DefaultPresetVoices(lang string) []domain.VoiceProfile {
	switch lang {
	case "vi":
		return []domain.VoiceProfile{
			{
				ID:         "vieneu_vi_truc_ly",
				ProviderID: VieNeuProviderID,
				VoiceID:    "Trúc Ly",
				Name:       "VieNeu Trúc Ly (Nữ Tự nhiên)",
				Language:   "vi",
				Gender:     "female",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "natural_warm",
			},
			{
				ID:         "vieneu_vi_pham_tuyen",
				ProviderID: VieNeuProviderID,
				VoiceID:    "Phạm Tuyên",
				Name:       "VieNeu Phạm Tuyên (Nam Trầm ấm)",
				Language:   "vi",
				Gender:     "male",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "deep_resonant",
			},
			{
				ID:         "vieneu_vi_doan_trang",
				ProviderID: VieNeuProviderID,
				VoiceID:    "Đoan Trang",
				Name:       "VieNeu Đoan Trang (Nữ)",
				Language:   "vi",
				Gender:     "female",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "expressive",
			},
			{
				ID:         "vieneu_vi_xuan_vinh",
				ProviderID: VieNeuProviderID,
				VoiceID:    "Xuân Vĩnh",
				Name:       "VieNeu Xuân Vĩnh (Nam)",
				Language:   "vi",
				Gender:     "male",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "authoritative",
			},
		}
	case "en":
		return []domain.VoiceProfile{
			{
				ID:         "kokoro_en_af_heart",
				ProviderID: KokoroProviderID,
				VoiceID:    "af_heart",
				Name:       "Kokoro af_heart (Female Heart)",
				Language:   "en",
				Gender:     "female",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "clear_friendly",
			},
			{
				ID:         "kokoro_en_am_michael",
				ProviderID: KokoroProviderID,
				VoiceID:    "am_michael",
				Name:       "Kokoro am_michael (Male)",
				Language:   "en",
				Gender:     "male",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "authoritative",
			},
			{
				ID:         "kokoro_en_af_bella",
				ProviderID: KokoroProviderID,
				VoiceID:    "af_bella",
				Name:       "Kokoro af_bella (Female Bella)",
				Language:   "en",
				Gender:     "female",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "expressive",
			},
			{
				ID:         "kokoro_en_am_fenrir",
				ProviderID: KokoroProviderID,
				VoiceID:    "am_fenrir",
				Name:       "Kokoro am_fenrir (Male Fenrir)",
				Language:   "en",
				Gender:     "male",
				Pitch:      1.0,
				Speed:      1.0,
				Timbre:     "deep_resonant",
			},
		}
	default:
		return nil
	}
}

// CosyVoicePresetVoices returns conditional CosyVoice3 voice profiles (not in default rotation).
func CosyVoicePresetVoices(lang string) []domain.VoiceProfile {
	switch lang {
	case "vi":
		return []domain.VoiceProfile{
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

// OrderedVieNeuVoices returns the frozen ordered VieNeu preset voice rotation.
func OrderedVieNeuVoices() []string {
	return append([]string(nil), domain.FrozenVieNeuVoiceOrder...)
}

// OrderedKokoroVoices returns the frozen ordered Kokoro preset voice rotation.
func OrderedKokoroVoices() []string {
	return append([]string(nil), domain.FrozenKokoroVoiceOrder...)
}

// IsVerifiedTTSVoice validates whether voiceID is a verified preset voice for the given provider.
func IsVerifiedTTSVoice(providerID, voiceID string) bool {
	if strings.HasPrefix(providerID, "fake_") || strings.HasPrefix(providerID, "mock_") || strings.HasPrefix(providerID, "test_") {
		return true
	}
	switch providerID {
	case VieNeuProviderID, "vieneu-tts", "pnnbao-ump/VieNeu-TTS-v3-Turbo":
		for _, v := range OrderedVieNeuVoices() {
			if v == voiceID {
				return true
			}
		}
		return false
	case KokoroProviderID, "kokoro-tts", "hexgrad/Kokoro-82M":
		for _, v := range OrderedKokoroVoices() {
			if v == voiceID {
				return true
			}
		}
		return false
	case "cosyvoice3_tts", "cosyvoice3":
		return voiceID == "cosy_vi_f1" || voiceID == "cosy_en_f1"
	default:
		return false
	}
}
