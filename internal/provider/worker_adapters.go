// Worker-backed speech providers: concrete StageWorker adapters that satisfy
// the provider.Provider interfaces plus the speech capability interfaces
// (ASRTranscriptProvider / AlignWordProvider / DiarizationProvider). Each
// invocation spawns a real StageWorker subprocess over the Seam 2 NDJSON
// protocol via the worker Supervisor/Client stack and runs through the Router's
// ExecuteWithRetry in production composition. No fake providers exist here.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/worker"
)

// StageWorker env/config keys (Finding 2: manifest/config-driven identity).
const (
	envStageWorkerBin = "DOUYINIE_STAGEWORKER_BIN"
	cfgModelName      = "model_name"
	cfgModelVersion   = "model_version"
)

// Default run timeouts for one worker-backed speech invocation.
const (
	defaultHandshakeTimeout   = 15 * time.Second
	defaultRunTimeout         = 10 * time.Minute
	defaultHeartbeatTimeout   = worker.HeartbeatTimeout
	stageWorkerFamilyASR      = "asr"
	stageWorkerFamilyAligner  = "aligner"
	stageWorkerFamilyDiarizer = "diarizer"
	stageWorkerFamilyTTS      = "tts"
)

// resolveStageWorkerBinary locates the StageWorker executable: explicit
// DOUYINIE_STAGEWORKER_BIN first, then a stageworker binary next to the host
// process, then PATH.
func resolveStageWorkerBinary() (string, error) {
	if p := os.Getenv(envStageWorkerBin); p != "" {
		if _, err := exec.LookPath(p); err != nil {
			return "", fmt.Errorf("stage worker binary from %s not runnable: %w", envStageWorkerBin, err)
		}
		return p, nil
	}
	exePath, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(exePath), "stageworker")
		if runtime.GOOS == "windows" {
			candidate += ".exe"
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return exec.LookPath("stageworker")
}

func (p *workerProviderBase) Type() ProviderType { return ProviderType(p.capability.Stage) }

// workerBridge executes exactly one stage command against a fresh StageWorker
// subprocess and returns the committed artifact bytes read from OutputPath.
// It acquires the authoritative GPU lease from GPULeaseManager before spawning
// and guarantees release and supervisor cleanup on exit/error/cancel (Finding 1).
type workerBridge struct {
	binary           string
	handshakeTimeout time.Duration
	runTimeout       time.Duration
	leaseManager     *worker.GPULeaseManager
}

func newWorkerBridge(leaseManager *worker.GPULeaseManager) (*workerBridge, error) {
	bin, err := resolveStageWorkerBinary()
	if err != nil {
		return nil, fmt.Errorf("stage worker binary unavailable: %w", err)
	}
	return &workerBridge{
		binary:           bin,
		handshakeTimeout: defaultHandshakeTimeout,
		runTimeout:       defaultRunTimeout,
		leaseManager:     leaseManager,
	}, nil
}

// run spawns the StageWorker, hands it the command, waits for completion, and
// reads back the output artifact JSON while holding the GPU lease. It guarantees
// deterministic cleanup: on success, error, timeout, or context cancellation,
// the supervisor process tree is terminated AND reaped before the GPU lease
// is released (Issue #44 Blocker 2).
func (b *workerBridge) run(ctx context.Context, cmd worker.Command) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, b.runTimeout)
	defer cancel()

	if b.leaseManager != nil {
		leaseID, err := b.leaseManager.Acquire(runCtx, cmd.Family, nil)
		if err != nil {
			return nil, fmt.Errorf("acquire gpu lease for %s: %w", cmd.Family, err)
		}
		defer func() {
			_ = b.leaseManager.Release(context.Background(), leaseID)
		}()
	}

	sup := worker.NewSupervisor()
	var client *worker.Client
	defer func() {
		if client != nil {
			_ = client.Shutdown()
		} else {
			_ = sup.Terminate()
			_ = sup.Wait()
		}
	}()

	if err := sup.Spawn(runCtx, cmd.Family, b.binary, "-family", cmd.Family); err != nil {
		return nil, fmt.Errorf("spawn stage worker family %s: %w", cmd.Family, err)
	}
	client = worker.NewClient(sup)
	if _, err := client.Handshake(runCtx, b.handshakeTimeout); err != nil {
		return nil, fmt.Errorf("stage worker handshake: %w", err)
	}
	artifact, err := client.Run(runCtx, cmd, b.handshakeTimeout, defaultHeartbeatTimeout)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(artifact.Path)
	if err != nil {
		return nil, fmt.Errorf("read stage worker artifact %s: %w", artifact.Path, err)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Shared Provider plumbing
// ---------------------------------------------------------------------------

// workerProviderBase carries the manifest-driven model identity and static
// capability shared by all three adapters (Finding 2).
type workerProviderBase struct {
	id           string
	modelName    string
	modelVer     string
	capability   domain.ProviderCapability
	leaseManager *worker.GPULeaseManager
}

func (p *workerProviderBase) ID() string               { return p.id }
func (p *workerProviderBase) PolicyState() PolicyState { return PolicyAllowed }
func (p *workerProviderBase) IsHealthy() bool          { return true }
func (p *workerProviderBase) Capability() domain.ProviderCapability {
	return p.capability
}
func (p *workerProviderBase) ModelInfo() (string, string) { return p.modelName, p.modelVer }
func (p *workerProviderBase) SetLeaseManager(mgr *worker.GPULeaseManager) {
	p.leaseManager = mgr
}

// newCommand builds the StageWorker command skeleton with manifest-driven
// model identity in Config and carries the real immutable CAS/SourceAsset SHA256 (Finding 4).
func newCommand(stage, family string, audio worker.ArtifactRef, modelName, modelVersion string) worker.Command {
	return worker.Command{
		ID:        uuid.NewString(),
		Family:    family,
		Stage:     stage,
		AttemptID: uuid.NewString(),
		Config: map[string]any{
			cfgModelName:    modelName,
			cfgModelVersion: modelVersion,
		},
		Inputs: []worker.ArtifactRef{audio},
	}
}

// ---------------------------------------------------------------------------
// ASR adapter
// ---------------------------------------------------------------------------

// WorkerASRProvider produces transcripts through the asr StageWorker stage.
// The 1.7B and 0.6B models are two instances distinguished by their
// manifest-driven ModelInfo; the Router quality-falls-back between them.
type WorkerASRProvider struct {
	workerProviderBase
}

var _ interface {
	Provider
	ASRTranscriptProvider
} = (*WorkerASRProvider)(nil)

// NewWorkerASRProvider builds an ASR provider adapter bound to one checkpoint.
func NewWorkerASRProvider(id, modelName, modelVersion string, qualityScore float64) (*WorkerASRProvider, error) {
	if id == "" || modelName == "" || modelVersion == "" {
		return nil, fmt.Errorf("worker ASR provider requires id, model_name and model_version")
	}
	if qualityScore <= 0 {
		qualityScore = 0.8
	}
	return &WorkerASRProvider{workerProviderBase{
		id:        id,
		modelName: modelName,
		modelVer:  modelVersion,
		capability: domain.ProviderCapability{
			Stage:          "asr",
			Languages:      []string{"zh"},
			ExecutionTier:  "local",
			CostPerUnit:    0,
			QualityScore:   qualityScore,
			MaxConcurrency: 1,
			Features:       []string{"qwen3_asr"},
		},
	}}, nil
}

// NewProductionSpeechRegistry creates a Registry populated with the production
// worker-backed speech understanding providers (Qwen3-ASR 1.7B quality,
// Qwen3-ASR 0.6B fallback, Qwen3-ForcedAligner, and conditional Diarization).
// No fake providers are registered here (Issue #44 Finding 1).
func NewProductionSpeechRegistry(leaseManager ...*worker.GPULeaseManager) (*Registry, error) {
	var mgr *worker.GPULeaseManager
	if len(leaseManager) > 0 {
		mgr = leaseManager[0]
	}
	reg := NewRegistry()
	asr17b, err := NewWorkerASRProvider("qwen3_asr_1_7b", "qwen3-asr", "1.7b", 0.9)
	if err != nil {
		return nil, err
	}
	asr17b.SetLeaseManager(mgr)
	if err := reg.Register(asr17b); err != nil {
		return nil, err
	}

	asr06b, err := NewWorkerASRProvider("qwen3_asr_0_6b", "qwen3-asr", "0.6b", 0.7)
	if err != nil {
		return nil, err
	}
	asr06b.SetLeaseManager(mgr)
	if err := reg.Register(asr06b); err != nil {
		return nil, err
	}

	aligner, err := NewWorkerAlignerProvider("qwen3_forced_aligner", "Qwen3-ForcedAligner-0.6B", "0.6b")
	if err != nil {
		return nil, err
	}
	aligner.SetLeaseManager(mgr)
	if err := reg.Register(aligner); err != nil {
		return nil, err
	}

	diarizer, err := NewWorkerDiarizationProvider("campplus_diarizer", "iic/speech_campplus_sv_zh_en_16k-common_advanced", "v1.0.0", "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch", "v2.0.4")
	if err != nil {
		return nil, err
	}
	diarizer.SetLeaseManager(mgr)
	if err := reg.Register(diarizer); err != nil {
		return nil, err
	}

	// Production TTS worker adapters per policy & benchmark #21
	// 1. VieNeu VI baseline
	vieneu, err := NewWorkerTTSProvider("vieneu_tts_vi", "vieneu-tts", "1.0.0", []string{"vi"}, 0.95)
	if err != nil {
		return nil, err
	}
	vieneu.SetLeaseManager(mgr)
	if err := reg.Register(vieneu); err != nil {
		return nil, err
	}

	// 2. CosyVoice3 measured-duration speed-fit lane
	cosyvoice, err := NewWorkerTTSProvider("cosyvoice3_tts", "cosyvoice3", "3.0.0", []string{"vi", "en"}, 0.98, "measured_duration_speed_fit")
	if err != nil {
		return nil, err
	}
	cosyvoice.SetLeaseManager(mgr)
	if err := reg.Register(cosyvoice); err != nil {
		return nil, err
	}

	// 3. Kokoro EN baseline
	kokoro, err := NewWorkerTTSProvider("kokoro_tts_en", "kokoro-tts", "1.0.0", []string{"en"}, 0.95)
	if err != nil {
		return nil, err
	}
	kokoro.SetLeaseManager(mgr)
	if err := reg.Register(kokoro); err != nil {
		return nil, err
	}

	// 4. Chatterbox clone fallback (lower-VRAM clone fallback)
	chatterbox, err := NewWorkerTTSProvider("chatterbox_tts", "chatterbox-tts", "1.0.0", []string{"vi", "en"}, 0.85)
	if err != nil {
		return nil, err
	}
	chatterbox.SetLeaseManager(mgr)
	if err := reg.Register(chatterbox); err != nil {
		return nil, err
	}

	return reg, nil
}

type asrArtifact struct {
	Segments []domain.ASRRawSegment `json:"segments"`
}

func (p *WorkerASRProvider) ProduceTranscript(ctx context.Context, audio worker.ArtifactRef) ([]domain.ASRRawSegment, error) {
	bridge, err := newWorkerBridge(p.leaseManager)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("asr", stageWorkerFamilyASR, audio, p.modelName, p.modelVer)
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-asr-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)
	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var art asrArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return nil, fmt.Errorf("%w: invalid ASR artifact JSON: %v", domain.ErrQualityRejected, err)
	}
	return art.Segments, nil
}

// ---------------------------------------------------------------------------
// Aligner adapter
// ---------------------------------------------------------------------------

// WorkerAlignerProvider aligns accepted text to the source timeline through
// the aligner StageWorker stage.
type WorkerAlignerProvider struct {
	workerProviderBase
}

var _ interface {
	Provider
	AlignWordProvider
} = (*WorkerAlignerProvider)(nil)

// NewWorkerAlignerProvider builds a forced-alignment provider adapter.
func NewWorkerAlignerProvider(id, modelName, modelVersion string) (*WorkerAlignerProvider, error) {
	if id == "" || modelName == "" || modelVersion == "" {
		return nil, fmt.Errorf("worker aligner provider requires id, model_name and model_version")
	}
	return &WorkerAlignerProvider{workerProviderBase{
		id:        id,
		modelName: modelName,
		modelVer:  modelVersion,
		capability: domain.ProviderCapability{
			Stage:          "aligner",
			Languages:      []string{"zh"},
			ExecutionTier:  "local",
			CostPerUnit:    0,
			QualityScore:   0.8,
			MaxConcurrency: 1,
			Features:       []string{"qwen3_forced_aligner"},
		},
	}}, nil
}

type alignerArtifact struct {
	WordTimings []domain.WordTiming `json:"word_timings"`
}

func (p *WorkerAlignerProvider) ProduceAlignment(ctx context.Context, audio worker.ArtifactRef, text string) ([]domain.WordTiming, error) {
	bridge, err := newWorkerBridge(p.leaseManager)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("aligner", stageWorkerFamilyAligner, audio, p.modelName, p.modelVer)
	cmd.Config["text"] = text
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-aligner-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)
	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var art alignerArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return nil, fmt.Errorf("%w: invalid aligner artifact JSON: %v", domain.ErrQualityRejected, err)
	}
	return art.WordTimings, nil
}

// ---------------------------------------------------------------------------
// Diarization adapter (Issue #44 Finding 3)
// ---------------------------------------------------------------------------

// WorkerDiarizationProvider segments source audio into speaker regions through
// the diarize StageWorker stage.
type WorkerDiarizationProvider struct {
	workerProviderBase
	vadModelName string
	vadModelVer  string
}

var _ interface {
	Provider
	DiarizationProvider
} = (*WorkerDiarizationProvider)(nil)

// NewWorkerDiarizationProvider builds a diarization provider adapter.
func NewWorkerDiarizationProvider(id, modelName, modelVersion string, vadOpts ...string) (*WorkerDiarizationProvider, error) {
	if id == "" || modelName == "" || modelVersion == "" {
		return nil, fmt.Errorf("worker diarization provider requires id, model_name and model_version")
	}
	vadName := "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch"
	vadVer := "v2.0.4"
	if len(vadOpts) > 0 && vadOpts[0] != "" {
		vadName = vadOpts[0]
	}
	if len(vadOpts) > 1 && vadOpts[1] != "" {
		vadVer = vadOpts[1]
	}
	return &WorkerDiarizationProvider{
		workerProviderBase: workerProviderBase{
			id:        id,
			modelName: modelName,
			modelVer:  modelVersion,
			capability: domain.ProviderCapability{
				Stage:          "diarizer",
				Languages:      []string{"zh"},
				ExecutionTier:  "local",
				CostPerUnit:    0,
				QualityScore:   0.7,
				MaxConcurrency: 1,
				Features:       []string{"speaker_diarization"},
			},
		},
		vadModelName: vadName,
		vadModelVer:  vadVer,
	}, nil
}

// VADModelInfo returns the VAD model name and version used by this diarizer.
func (p *WorkerDiarizationProvider) VADModelInfo() (name, version string) {
	return p.vadModelName, p.vadModelVer
}

// ModelDependencies implements DependentModelProvider for WorkerDiarizationProvider.
func (p *WorkerDiarizationProvider) ModelDependencies() []ModelDependency {
	if p.vadModelName != "" {
		return []ModelDependency{{
			Name:    p.vadModelName,
			Version: p.vadModelVer,
			Role:    "vad",
		}}
	}
	return nil
}

type diarizerArtifact struct {
	SpeakerAssignments []domain.SpeakerAssignment `json:"speaker_assignments"`
	ModelName          string                     `json:"model_name,omitempty"`
	ModelVersion       string                     `json:"model_version,omitempty"`
	VADModelName       string                     `json:"vad_model_name,omitempty"`
	VADModelVersion    string                     `json:"vad_model_version,omitempty"`
}

type diarizerEvidenceArtifact struct {
	SpeakerEvidence domain.SpeakerEvidence `json:"speaker_evidence"`
	ModelName       string                 `json:"model_name,omitempty"`
	ModelVersion    string                 `json:"model_version,omitempty"`
	VADModelName    string                 `json:"vad_model_name,omitempty"`
	VADModelVersion string                 `json:"vad_model_version,omitempty"`
}

type EmbeddingCosineThresholdContextKey struct{}

// WithEmbeddingCosineThreshold attaches an embedding cosine change threshold to the context for diarization evidence probing.
func WithEmbeddingCosineThreshold(ctx context.Context, threshold float64) context.Context {
	return context.WithValue(ctx, EmbeddingCosineThresholdContextKey{}, threshold)
}

func (p *WorkerDiarizationProvider) ProbeSpeakerEvidence(ctx context.Context, audio worker.ArtifactRef) (*domain.SpeakerEvidence, error) {
	bridge, err := newWorkerBridge(p.leaseManager)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	threshold, ok := ctx.Value(EmbeddingCosineThresholdContextKey{}).(float64)
	if !ok || threshold <= 0 || threshold > 1 {
		return nil, fmt.Errorf("diarization evidence requires embedding cosine threshold in (0,1]")
	}
	cmd := newCommand("diarize_evidence", stageWorkerFamilyDiarizer, audio, p.modelName, p.modelVer)
	cmd.Config["vad_model_name"] = p.vadModelName
	cmd.Config["vad_model_version"] = p.vadModelVer
	cmd.Config["embedding_cosine_threshold"] = threshold
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-diarizer-evidence-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)
	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var art diarizerEvidenceArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return nil, fmt.Errorf("%w: invalid diarizer evidence artifact JSON: %v", domain.ErrQualityRejected, err)
	}
	if art.SpeakerEvidence.Source == "" {
		art.SpeakerEvidence.Source = fmt.Sprintf("%s@%s+%s@%s", p.modelName, p.modelVer, p.vadModelName, p.vadModelVer)
	}
	return &art.SpeakerEvidence, nil
}

func (p *WorkerDiarizationProvider) ProduceDiarization(ctx context.Context, audio worker.ArtifactRef) ([]domain.SpeakerAssignment, error) {
	bridge, err := newWorkerBridge(p.leaseManager)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("diarize", stageWorkerFamilyDiarizer, audio, p.modelName, p.modelVer)
	cmd.Config["vad_model_name"] = p.vadModelName
	cmd.Config["vad_model_version"] = p.vadModelVer
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-diarizer-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)
	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var art diarizerArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return nil, fmt.Errorf("%w: invalid diarizer artifact JSON: %v", domain.ErrQualityRejected, err)
	}
	return art.SpeakerAssignments, nil
}

// ---------------------------------------------------------------------------
// TTS adapter
// ---------------------------------------------------------------------------

// WorkerTTSProvider executes text-to-speech synthesis through the tts StageWorker stage.
type WorkerTTSProvider struct {
	workerProviderBase
	customVoices []domain.VoiceProfile
}

var _ interface {
	Provider
	TTSProvider
} = (*WorkerTTSProvider)(nil)

// NewWorkerTTSProvider builds a worker-backed TTS provider adapter.
func NewWorkerTTSProvider(id, modelName, modelVersion string, languages []string, qualityScore float64, features ...string) (*WorkerTTSProvider, error) {
	if id == "" || modelName == "" || modelVersion == "" {
		return nil, fmt.Errorf("worker tts provider requires id, model_name and model_version")
	}
	if len(languages) == 0 {
		languages = []string{"vi", "en"}
	}
	if qualityScore <= 0 {
		qualityScore = 0.9
	}
	feats := []string{"measured_duration_fit", "streaming"}
	if len(features) > 0 {
		feats = append(feats, features...)
	}
	return &WorkerTTSProvider{
		workerProviderBase: workerProviderBase{
			id:        id,
			modelName: modelName,
			modelVer:  modelVersion,
			capability: domain.ProviderCapability{
				Stage:          "tts",
				Languages:      languages,
				ExecutionTier:  "local",
				CostPerUnit:    0,
				QualityScore:   qualityScore,
				MaxConcurrency: 1,
				Features:       feats,
			},
		},
	}, nil
}

// VoiceCatalog implements TTSProvider.
func (p *WorkerTTSProvider) VoiceCatalog() []domain.VoiceProfile {
	if len(p.customVoices) > 0 {
		return p.customVoices
	}
	var voices []domain.VoiceProfile
	for _, l := range p.capability.Languages {
		voices = append(voices, DefaultPresetVoices(l)...)
	}
	return voices
}

type ttsArtifact struct {
	AudioData           []byte `json:"audio_data"`
	AudioPath           string `json:"audio_path"`
	AudioSHA256         string `json:"audio_sha256"`
	MeasuredDurationMs  int64  `json:"measured_duration_ms"`
	PredictedDurationMs int64  `json:"predicted_duration_ms"`
}

func (p *WorkerTTSProvider) SynthesizeSpeech(ctx context.Context, req TTSSynthesisRequest) (*TTSSynthesisResult, error) {
	bridge, err := newWorkerBridge(p.leaseManager)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("tts", stageWorkerFamilyTTS, worker.ArtifactRef{SHA256: req.AssetID}, p.modelName, p.modelVer)
	cmd.Config["text"] = req.Text
	cmd.Config["language"] = req.Language
	cmd.Config["voice_id"] = req.Voice.VoiceID
	cmd.Config["speed"] = fmt.Sprintf("%.2f", req.Speed)
	cmd.Config["slot_duration_ms"] = fmt.Sprintf("%d", req.SlotDurationMs)
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-tts-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)

	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var art ttsArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return nil, fmt.Errorf("%w: invalid TTS artifact JSON: %v", domain.ErrQualityRejected, err)
	}

	audioBytes := art.AudioData
	if len(audioBytes) == 0 && art.AudioPath != "" {
		audioBytes, _ = os.ReadFile(art.AudioPath)
	}

	durMs := art.MeasuredDurationMs
	if durMs <= 0 && len(audioBytes) > 0 {
		durMs, _ = media.ProbeWAVBytes(audioBytes)
	}

	return &TTSSynthesisResult{
		AudioData:           audioBytes,
		AudioSHA256:         art.AudioSHA256,
		AudioCASPath:        art.AudioPath,
		SampleRate:          16000,
		Channels:            1,
		Format:              "wav",
		ProviderID:          p.id,
		ModelName:           p.modelName,
		ModelVersion:        p.modelVer,
		PredictedDurationMs: art.PredictedDurationMs,
		MeasuredDurationMs:  durMs,
	}, nil
}
