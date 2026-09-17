package provider

import (
	"context"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
)

const (
	// ZeroTTS production VI identities (Issue #92).
	ZeroTTSProviderID   = "zerotts_tts_vi"
	ZeroTTSModelID      = domain.PinnedZeroTTSModelID
	ZeroTTSModelVersion = domain.PinnedZeroTTSModelVersion

	// VieNeu frozen RC identities (Issue #68; SDK upgraded to v3.8.1). The provisioned SDK
	// commit and model revision are provenance records in
	// docs/research/tts-runtime-upgrade-2026-09-17.md §1 - nothing reads them at runtime.
	VieNeuModelID      = "pnnbao-ump/VieNeu-TTS-v3-Turbo"
	VieNeuModelVersion = "v3.8.1"
	VieNeuProviderID   = "vieneu_tts_vi"

	// Kokoro frozen RC identities (Issue #68)
	KokoroModelID       = "hexgrad/Kokoro-82M"
	KokoroModelVersion  = "v1.0"
	KokoroModelCommit   = "f3ff3571791e39611d31c381e3a41a3af07b4987"
	KokoroSourceCommit  = "dfb907a02bba8152ca444717ca5d78747ccb4bec"
	KokoroCheckpointSHA = "496dba118d1a58f5f3db2efc88dbdc216e0483fc89fe6e47ee1f2c53f18ad1e4"
	KokoroProviderID    = "kokoro_tts_en"

	// FeatureFixedRateVoice marks a TTS lane that synthesizes only at its native
	// speaking rate: a non-1.0 speed request fails closed instead of being
	// honored or silently ignored. Overrun remediation for such a lane must
	// rewrite/regroup/review rather than request a speed resynthesis.
	FeatureFixedRateVoice = "fixed_rate_voice"

	// CosyVoiceProviderID is the duration-controlled fallback lane identity
	// (Issue #94): the only lane an unresolved fixed-rate (ZeroTTS) timing
	// failure may escalate a whole speaker to.
	CosyVoiceProviderID = "cosyvoice3_tts"

	// CosyVoiceModelID / CosyVoiceModelVersion are the same fallback lane's registered
	// model identity, used both at registration and in TTSRuntimeIdentities.
	CosyVoiceModelID      = "cosyvoice3"
	CosyVoiceModelVersion = "3.0.0"

	// DefaultVIUnattendedVoiceCount is the size of the approved unattended
	// Vietnamese rotation (Issue #93): the leading verified ZeroTTS presets,
	// quangminh then maichi. Presets beyond it are selectable only through
	// explicit operator audition/assignment; catalog presence alone never makes
	// a preset an unattended default.
	DefaultVIUnattendedVoiceCount = 2
)

// TTSRuntimeIdentities returns the pinned runtime/model identity of every TTS lane that
// can produce a DubSegment, keyed by provider ID. It is the semantic input a stage cache
// identity needs when its emitted audio depends on which runtime produced it: the
// DubSegments stage hashes this map, so bumping any lane's pin (model revision, runtime
// pack, adapter revision) invalidates the artifacts that lane produced instead of silently
// replaying audio from the previous runtime. A lane that gains a pinned identity must be
// added here in the same change, or its bump goes back to being invisible to the cache.
func TTSRuntimeIdentities() map[string]string {
	join := func(parts ...string) string { return strings.Join(parts, "|") }
	return map[string]string{
		// ZeroTTS audio is determined by the model revision plus the runtime pack
		// (source revision + adapter revision the snapshot governance pins).
		ZeroTTSProviderID:   join(ZeroTTSModelID, ZeroTTSModelVersion, domain.PinnedZeroTTSSourceRevision, domain.PinnedZeroTTSAdapterRevision),
		VieNeuProviderID:    join(VieNeuModelID, VieNeuModelVersion),
		KokoroProviderID:    join(KokoroModelID, KokoroModelVersion, KokoroCheckpointSHA),
		CosyVoiceProviderID: join(CosyVoiceModelID, CosyVoiceModelVersion),
	}
}

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

// DefaultPresetVoices returns the unattended default preset voices by language.
// For Vietnamese, it returns the approved unattended ZeroTTS rotation (Issue #93):
// 1. quangminh, 2. maichi.
// For English, it returns the frozen ordered Kokoro rotation (Issue #68):
// 1. af_heart (female natural), 2. am_michael (male natural), 3. af_bella (female), 4. am_fenrir (male).
// VieNeu remains the Vietnamese compatibility lane for historical frozen
// assignments (see VieNeuPresetVoices), and CosyVoice3 remains conditional for
// duration-controlled fit; neither is part of the default rotation.
func DefaultPresetVoices(lang string) []domain.VoiceProfile {
	switch lang {
	case "vi":
		return UnattendedZeroTTSVoices()
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

// ZeroTTSPresetVoices returns all verified packaged ZeroTTS presets for explicit selection.
func ZeroTTSPresetVoices() []domain.VoiceProfile {
	voices := make([]domain.VoiceProfile, 0, len(domain.FrozenZeroTTSVoiceOrder))
	for _, voiceID := range domain.FrozenZeroTTSVoiceOrder {
		voices = append(voices, domain.VoiceProfile{
			ID:         "zerotts_vi_" + voiceID,
			ProviderID: ZeroTTSProviderID,
			VoiceID:    voiceID,
			Name:       "ZeroTTS " + voiceID,
			Language:   "vi",
			Pitch:      1.0,
			Speed:      1.0,
		})
	}
	return voices
}

// UnattendedZeroTTSVoices returns the approved unattended Vietnamese rotation:
// the leading verified ZeroTTS presets in frozen catalog order (quangminh, then
// maichi). Verified presets beyond the rotation stay selectable/auditionable via
// ZeroTTSPresetVoices but are never auto-assigned.
func UnattendedZeroTTSVoices() []domain.VoiceProfile {
	presets := ZeroTTSPresetVoices()
	if len(presets) <= DefaultVIUnattendedVoiceCount {
		return presets
	}
	return append([]domain.VoiceProfile(nil), presets[:DefaultVIUnattendedVoiceCount]...)
}

// VieNeuPresetVoices returns the frozen VieNeu v3 Turbo rotation (Issue #68):
// 1. Trúc Ly (female natural), 2. Phạm Tuyên (male natural), 3. Đoan Trang (female), 4. Xuân Vĩnh (male).
// These stay routable for historical frozen assignments but are no longer the
// unattended Vietnamese default.
func VieNeuPresetVoices() []domain.VoiceProfile {
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
}

// CosyVoicePresetVoices returns conditional CosyVoice3 voice profiles (not in default rotation).
func CosyVoicePresetVoices(lang string) []domain.VoiceProfile {
	switch lang {
	case "vi":
		return []domain.VoiceProfile{
			{
				ID:         "cosyvoice3_vi_female_1",
				ProviderID: CosyVoiceProviderID,
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
				ProviderID: CosyVoiceProviderID,
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

// OrderedZeroTTSVoices returns the verified ZeroTTS packaged voice order.
func OrderedZeroTTSVoices() []string {
	return append([]string(nil), domain.FrozenZeroTTSVoiceOrder...)
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
	case ZeroTTSProviderID, "zerotts", ZeroTTSModelID:
		for _, v := range OrderedZeroTTSVoices() {
			if v == voiceID {
				return true
			}
		}
		return false
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
	case CosyVoiceProviderID, "cosyvoice3":
		return voiceID == "cosy_vi_f1" || voiceID == "cosy_en_f1"
	default:
		return false
	}
}
