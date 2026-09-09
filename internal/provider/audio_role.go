package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/worker"
)

const (
	YAMNetProviderID   = "worker_yamnet"
	YAMNetModelID      = domain.PinnedYAMNetModelID
	YAMNetModelVersion = domain.PinnedYAMNetModelVersion
)

// AudioRoleConfig defines versioned threshold and calibration settings for YAMNet audio role analysis.
type AudioRoleConfig struct {
	Version               string  `json:"version"`
	WindowSamples         int     `json:"window_samples"`
	SampleRate            int     `json:"sample_rate"`
	DialogueThreshold     float64 `json:"dialogue_threshold"`
	SingingThreshold      float64 `json:"singing_threshold"`
	InstrumentalThreshold float64 `json:"instrumental_threshold"`
	AmbienceThreshold     float64 `json:"ambience_threshold"`
	VocalRMSThreshold     float64 `json:"vocal_rms_threshold"`
	UncertainMargin       float64 `json:"uncertain_margin"`
	MinConfidence         float64 `json:"min_confidence"`
}

// DefaultAudioRoleConfig returns the calibrated default configuration for Google YAMNet classification TFLite v1.
func DefaultAudioRoleConfig() AudioRoleConfig {
	return AudioRoleConfig{
		Version:               "1.0",
		WindowSamples:         15600,
		SampleRate:            16000,
		DialogueThreshold:     0.30,
		SingingThreshold:      0.25,
		InstrumentalThreshold: 0.25,
		AmbienceThreshold:     0.25,
		VocalRMSThreshold:     0.008,
		UncertainMargin:       0.08,
		MinConfidence:         0.20,
	}
}

// Hash returns the deterministic SHA-256 digest of the canonical JSON configuration.
func (c AudioRoleConfig) Hash() string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// WorkerAudioRoleProvider implements the concrete StageWorker adapter provider for YAMNet AudioRolePlan analysis.
type WorkerAudioRoleProvider struct {
	workerProviderBase
	config AudioRoleConfig
}

// NewWorkerAudioRoleProvider constructs a new StageWorker-backed AudioRole analyzer provider.
func NewWorkerAudioRoleProvider(id, modelName, modelVer string, qualityScore float64) (*WorkerAudioRoleProvider, error) {
	if id == "" || modelName == "" || modelVer == "" {
		return nil, fmt.Errorf("worker audio role provider requires id, model_name and model_version")
	}
	if qualityScore <= 0 {
		qualityScore = 0.95
	}
	return &WorkerAudioRoleProvider{
		workerProviderBase: workerProviderBase{
			id:               id,
			modelName:        modelName,
			modelVer:         modelVer,
			requiresSnapshot: true,
			capability: domain.ProviderCapability{
				Stage:          string(TypeAudioRole),
				Languages:      []string{"*"},
				ExecutionTier:  "local",
				CostPerUnit:    0,
				QualityScore:   qualityScore,
				MaxConcurrency: 1,
				Features:       []string{"yamnet_classification"},
			},
		},
		config: DefaultAudioRoleConfig(),
	}, nil
}

func (p *WorkerAudioRoleProvider) SetConfig(cfg AudioRoleConfig) {
	p.config = cfg
}

func (p *WorkerAudioRoleProvider) Config() AudioRoleConfig {
	return p.config
}

func (p *WorkerAudioRoleProvider) AnalyzerInfo() (providerID, modelName, modelVersion, configHash string) {
	return p.id, p.modelName, p.modelVer, p.config.Hash()
}

// EnsureRuntimeIdentity probes the configured audio role Python runtime and establishes
// validated RuntimeIdentity evidence via SnapshotService.SetRuntimeIdentity if not already set.
func (p *WorkerAudioRoleProvider) EnsureRuntimeIdentity(ctx context.Context) error {
	if !p.requiresSnapshot {
		return nil
	}
	if p.snapshotSvc == nil {
		return fmt.Errorf("%w: snapshot service not configured for audio role %s:%s",
			domain.ErrSnapshotUnverified, p.modelName, p.modelVer)
	}

	binding, _ := p.snapshotSvc.GetBinding(p.modelName, p.modelVer)
	if binding == nil {
		binding, _ = p.snapshotSvc.GetBinding(YAMNetModelID, YAMNetModelVersion)
	}
	if binding == nil {
		return fmt.Errorf("%w: missing or unverified model snapshot for audio role %s:%s",
			domain.ErrSnapshotUnverified, p.modelName, p.modelVer)
	}

	// Re-verify snapshot file fingerprints on disk (revokes on mutation/deletion)
	if err := p.snapshotSvc.CheckBindingFingerprints(binding.DependencyName, binding.Version); err != nil {
		return fmt.Errorf("%w: snapshot validation failed: %v", domain.ErrSnapshotMutatedRehashRequired, err)
	}

	// Cache hit: If runtime identity was already validated and attached, safe to reuse
	if binding.RuntimeIdentity != nil {
		return nil
	}

	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrSnapshotUnverified, err)
	}

	cmd := worker.Command{
		ID:        uuid.NewString(),
		Family:    stageWorkerFamilyAudioRole,
		Stage:     "audio_role_probe",
		AttemptID: uuid.NewString(),
		Config: map[string]any{
			cfgModelName:                         p.modelName,
			cfgModelVersion:                      p.modelVer,
			"mode":                               "probe",
			worker.ConfigKeyRequireModelSnapshot: true,
		},
		OutputPath: filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-audiorole-probe-%s.json", uuid.NewString())),
	}
	defer os.Remove(cmd.OutputPath)

	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%w: audio role runtime probe failed: %v", domain.ErrSnapshotUnverified, err)
	}

	var probeArt struct {
		Status          string            `json:"status"`
		PackageName     string            `json:"package_name"`
		PackageVersion  string            `json:"package_version"`
		SourceRevision  string            `json:"source_revision"`
		RuntimeVersions map[string]string `json:"runtime_versions"`
		AdapterRevision string            `json:"adapter_revision"`
		Error           string            `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &probeArt); err != nil {
		return fmt.Errorf("%w: invalid audio role probe output: %v", domain.ErrSnapshotUnverified, err)
	}
	if probeArt.Error != "" {
		return fmt.Errorf("%w: %s", domain.ErrSnapshotUnverified, probeArt.Error)
	}
	if !strings.HasPrefix(probeArt.AdapterRevision, "cmd/stageworker/adapters/audio_role_yamnet.py@v") {
		return fmt.Errorf("%w: unverified adapter revision %q in runtime identity",
			domain.ErrSnapshotUnverified, probeArt.AdapterRevision)
	}

	if ver, ok := probeArt.RuntimeVersions["ai-edge-litert"]; !ok || ver != domain.PinnedYAMNetPackageVersion {
		return fmt.Errorf("%w: ai-edge-litert runtime package version mismatch: expected exact %s, got %s",
			domain.ErrSnapshotUnverified, domain.PinnedYAMNetPackageVersion, ver)
	}

	var depSHAs []string
	for _, f := range binding.Manifest.Files {
		norm := strings.ToLower(domain.NormalizeRelativePath(f.RelativePath))
		if !strings.HasSuffix(norm, "yamnet.tflite") && len(f.SHA256) == 64 {
			depSHAs = append(depSHAs, f.SHA256)
		}
	}

	rt := domain.NewYAMNetRuntimeIdentity(binding.SnapshotManifestSHA256, depSHAs)
	if err := p.snapshotSvc.SetRuntimeIdentity(binding.DependencyName, binding.Version, rt); err != nil {
		return fmt.Errorf("set runtime identity: %w", err)
	}

	return nil
}

// AnalyzeAudioRoles executes YAMNet classification via StageWorker subprocess over Seam 2 NDJSON.
func (p *WorkerAudioRoleProvider) AnalyzeAudioRoles(ctx context.Context, req domain.AudioRoleAnalysisRequest) (*domain.AudioRoleAnalysisResult, error) {
	if p.requiresSnapshot {
		if err := p.EnsureRuntimeIdentity(ctx); err != nil {
			return nil, err
		}
	}

	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}

	// Prepare StageWorker command
	cmd := newCommand("audio_role", stageWorkerFamilyAudioRole, worker.ArtifactRef{Path: req.SourceAudioPath}, p.modelName, p.modelVer)
	cmd.Config["require_model_snapshot"] = p.requiresSnapshot
	cmd.Config["vocals_audio"] = req.VocalsPath
	cmd.Config["background_audio"] = req.BackgroundPath
	cmd.Config["source_audio"] = req.SourceAudioPath
	cmd.Config["config"] = p.config
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-audiorole-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)

	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}

	var out struct {
		Segments        []domain.AudioSegment `json:"segments"`
		ModelName       string                `json:"model_name"`
		ModelVersion    string                `json:"model_version"`
		RuntimeIdentity string                `json:"runtime_identity"`
		ProviderID      string                `json:"provider_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w: invalid audio role artifact JSON: %v", domain.ErrQualityRejected, err)
	}

	return &domain.AudioRoleAnalysisResult{
		Segments:        out.Segments,
		ModelName:       out.ModelName,
		ModelVersion:    out.ModelVersion,
		RuntimeIdentity: out.RuntimeIdentity,
		ProviderID:      p.id,
	}, nil
}

func (p *WorkerAudioRoleProvider) Execute(ctx context.Context, input any) (any, error) {
	req, ok := input.(domain.AudioRoleAnalysisRequest)
	if !ok {
		return nil, fmt.Errorf("WorkerAudioRoleProvider expects domain.AudioRoleAnalysisRequest, got %T", input)
	}
	return p.AnalyzeAudioRoles(ctx, req)
}
