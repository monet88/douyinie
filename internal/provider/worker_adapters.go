// Worker-backed speech providers: concrete StageWorker adapters that satisfy
// the provider.Provider interfaces plus the speech capability interfaces
// (ASRTranscriptProvider / AlignWordProvider / DiarizationProvider). Each
// invocation spawns a real StageWorker subprocess over the Seam 2 NDJSON
// protocol via the worker Supervisor/Client stack and runs through the Router's
// ExecuteWithRetry in production composition. No fake providers exist here.
package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
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
	defaultHandshakeTimeout    = 15 * time.Second
	defaultRunTimeout          = 10 * time.Minute
	defaultHeartbeatTimeout    = worker.HeartbeatTimeout
	stageWorkerFamilyASR       = "asr"
	stageWorkerFamilyAligner   = "aligner"
	stageWorkerFamilyDiarizer  = "diarizer"
	stageWorkerFamilyTTS       = "tts"
	stageWorkerFamilySeparator = "separator"
	stageWorkerFamilyOCR       = "ocr"
	stageWorkerFamilyAudioRole = "audio_role"
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
	snapshotSvc      *governance.SnapshotService
}

func newWorkerBridge(leaseManager *worker.GPULeaseManager, snapshotSvc *governance.SnapshotService) (*workerBridge, error) {
	bin, err := resolveStageWorkerBinary()
	if err != nil {
		return nil, fmt.Errorf("stage worker binary unavailable: %w", err)
	}
	return &workerBridge{
		binary:           bin,
		handshakeTimeout: defaultHandshakeTimeout,
		runTimeout:       defaultRunTimeout,
		leaseManager:     leaseManager,
		snapshotSvc:      snapshotSvc,
	}, nil
}

// run spawns the StageWorker, hands it the command, waits for completion, and
// reads back the output artifact JSON while holding the GPU lease. It guarantees
// deterministic cleanup: on success, error, timeout, or context cancellation,
// the supervisor process tree is terminated AND reaped before the GPU lease
// is released (Issue #44 Blocker 2). A command whose resource contract is
// CPU-only (cmd.CPUOnly) never acquires the lease (Issue #91); every other
// command keeps the accelerator-backed serialization.
func (b *workerBridge) run(ctx context.Context, cmd worker.Command) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, b.runTimeout)
	defer cancel()

	// Verify snapshot bytes and check fingerprints BEFORE GPU lease acquisition (Issue #64)
	if b.snapshotSvc != nil {
		if cmd.Config != nil && cmd.Stage != "separator_probe" {
			if _, ok := cmd.Config[worker.ConfigKeyModelSnapshot]; !ok {
				if cmd.Family == "ocr" || cmd.Stage == "ocr" {
					detName, _ := cmd.Config["det_model_name"].(string)
					if detName == "" {
						detName = "PP-OCRv6_medium_det"
					}
					detVer, _ := cmd.Config["det_model_version"].(string)
					if detVer == "" {
						detVer = "v6"
					}

					recName, _ := cmd.Config["rec_model_name"].(string)
					if recName == "" {
						recName = "PP-OCRv6_medium_rec"
					}
					recVer, _ := cmd.Config["rec_model_version"].(string)
					if recVer == "" {
						recVer = "v6"
					}

					oriName, _ := cmd.Config["ori_model_name"].(string)
					if oriName == "" {
						oriName = "PP-LCNet_x1_0_textline_ori"
					}
					oriVer, _ := cmd.Config["ori_model_version"].(string)
					if oriVer == "" {
						oriVer = "v1"
					}

					detBinding, errDet := b.snapshotSvc.GetBinding(detName, detVer)
					recBinding, errRec := b.snapshotSvc.GetBinding(recName, recVer)
					oriBinding, errOri := b.snapshotSvc.GetBinding(oriName, oriVer)

					reqSnap, _ := cmd.Config["require_model_snapshot"].(bool)
					if reqSnap {
						if errDet != nil || detBinding == nil {
							return nil, fmt.Errorf("ocr missing or unverified det snapshot (%s:%s): %w", detName, detVer, domain.ErrSnapshotUnverified)
						}
						if errRec != nil || recBinding == nil {
							return nil, fmt.Errorf("ocr missing or unverified rec snapshot (%s:%s): %w", recName, recVer, domain.ErrSnapshotUnverified)
						}
						if errOri != nil || oriBinding == nil {
							return nil, fmt.Errorf("ocr missing or unverified ori snapshot (%s:%s): %w", oriName, oriVer, domain.ErrSnapshotUnverified)
						}
					}

					if detBinding != nil && recBinding != nil && oriBinding != nil {
						envelope := worker.ModelSnapshotEnvelope{
							Primary: worker.ModelSnapshotRef{
								Role:                   "det",
								DependencyName:         detBinding.DependencyName,
								Version:                detBinding.Version,
								SnapshotManifestSHA256: detBinding.SnapshotManifestSHA256,
								LocalPath:              detBinding.LocalPath,
							},
							Dependencies: []worker.ModelSnapshotRef{
								{
									Role:                   "rec",
									DependencyName:         recBinding.DependencyName,
									Version:                recBinding.Version,
									SnapshotManifestSHA256: recBinding.SnapshotManifestSHA256,
									LocalPath:              recBinding.LocalPath,
								},
								{
									Role:                   "ori",
									DependencyName:         oriBinding.DependencyName,
									Version:                oriBinding.Version,
									SnapshotManifestSHA256: oriBinding.SnapshotManifestSHA256,
									LocalPath:              oriBinding.LocalPath,
								},
							},
						}
						worker.SetModelSnapshotEnvelope(cmd.Config, envelope)
					}
				} else {
					mName, _ := cmd.Config[cfgModelName].(string)
					mVer, _ := cmd.Config[cfgModelVersion].(string)
					if mName != "" {
						primaryBinding, err := b.snapshotSvc.GetBinding(mName, mVer)
						if err != nil || primaryBinding == nil {
							// Exact pinned snapshot bindings required for TTS:
							// ZeroTTS: zeroweight-ai/ZeroTTS@c2bfbd67dc648cac455077333f7cf5c18a2e3bb4
							// VieNeu: pnnbao-ump/VieNeu-TTS-v3-Turbo:v3.8.1
							// Kokoro: hexgrad/Kokoro-82M:v1.0
							// Legacy aliases such as vieneu-tts 1.0.0 or kokoro-tts 1.0.0 are strictly prohibited when exact binding is absent.
							if strings.Contains(strings.ToLower(mName), "vieneu") || strings.Contains(strings.ToLower(mName), "pnnbao") {
								b2, err2 := b.snapshotSvc.GetBinding(VieNeuModelID, VieNeuModelVersion)
								if err2 == nil && b2 != nil {
									primaryBinding = b2
								}
							} else if strings.Contains(strings.ToLower(mName), "kokoro") || strings.Contains(strings.ToLower(mName), "hexgrad") {
								b2, err2 := b.snapshotSvc.GetBinding(KokoroModelID, KokoroModelVersion)
								if err2 == nil && b2 != nil {
									primaryBinding = b2
								}
							} else if strings.Contains(strings.ToLower(mName), "uvr") || strings.Contains(strings.ToLower(mName), "mdx") {
								b2, err2 := b.snapshotSvc.GetBinding(UVRModelID, UVRModelVersion)
								if err2 == nil && b2 != nil {
									primaryBinding = b2
								}
							} else if strings.Contains(strings.ToLower(mName), "demucs") {
								b2, err2 := b.snapshotSvc.GetBinding(DemucsModelID, DemucsModelVersion)
								if err2 == nil && b2 != nil {
									primaryBinding = b2
								}
							}
						}

						reqSnap, _ := cmd.Config["require_model_snapshot"].(bool)
						if reqSnap && primaryBinding == nil {
							return nil, fmt.Errorf("missing or unverified model snapshot for %s:%s: %w", mName, mVer, domain.ErrSnapshotUnverified)
						}

						if primaryBinding != nil {
							var entrypointFile string
							if cmd.Family == "translation" || cmd.Stage == "translation" {
								ep, epErr := domain.ResolveTranslationGGUFEntrypoint(primaryBinding.Manifest, primaryBinding.LocalPath, primaryBinding.Version)
								if epErr != nil {
									return nil, fmt.Errorf("resolve translation model entrypoint: %w", epErr)
								}
								entrypointFile = ep
							} else if (cmd.Family == "tts" || cmd.Stage == "tts") && cmd.Stage != "tts_probe" {
								// Enforce that the resolved primaryBinding matches the exact RC identity
								if strings.Contains(strings.ToLower(mName), "zerotts") || strings.Contains(strings.ToLower(mName), "zeroweight") {
									if primaryBinding.DependencyName != ZeroTTSModelID || primaryBinding.Version != ZeroTTSModelVersion {
										return nil, fmt.Errorf("invalid ZeroTTS snapshot binding %s:%s (must be exact pin %s:%s): %w",
											primaryBinding.DependencyName, primaryBinding.Version, ZeroTTSModelID, ZeroTTSModelVersion, domain.ErrSnapshotUnverified)
									}
								} else if strings.Contains(strings.ToLower(mName), "vieneu") || strings.Contains(strings.ToLower(mName), "pnnbao") {
									if primaryBinding.DependencyName != VieNeuModelID || primaryBinding.Version != VieNeuModelVersion {
										return nil, fmt.Errorf("invalid VieNeu snapshot binding %s:%s (must be exact RC %s:%s): %w",
											primaryBinding.DependencyName, primaryBinding.Version, VieNeuModelID, VieNeuModelVersion, domain.ErrSnapshotUnverified)
									}
								} else if strings.Contains(strings.ToLower(mName), "kokoro") || strings.Contains(strings.ToLower(mName), "hexgrad") {
									if primaryBinding.DependencyName != KokoroModelID || primaryBinding.Version != KokoroModelVersion {
										return nil, fmt.Errorf("invalid Kokoro snapshot binding %s:%s (must be exact RC %s:%s): %w",
											primaryBinding.DependencyName, primaryBinding.Version, KokoroModelID, KokoroModelVersion, domain.ErrSnapshotUnverified)
									}
								}

								vID, _ := cmd.Config["voice_id"].(string)
								ep, epErr := domain.ResolveTTSVoiceEntrypoint(primaryBinding.Manifest, primaryBinding.LocalPath, mName, vID)
								if epErr != nil {
									return nil, fmt.Errorf("resolve tts voice entrypoint: %w", epErr)
								}
								entrypointFile = ep
							} else if cmd.Family == "separator" || cmd.Stage == "separator" {
								// Enforce that the resolved primaryBinding matches the exact RC identity
								if strings.Contains(strings.ToLower(mName), "uvr") || strings.Contains(strings.ToLower(mName), "mdx") {
									if primaryBinding.DependencyName != UVRModelID || primaryBinding.Version != UVRModelVersion {
										return nil, fmt.Errorf("invalid UVR snapshot binding %s:%s (must be exact RC %s:%s): %w",
											primaryBinding.DependencyName, primaryBinding.Version, UVRModelID, UVRModelVersion, domain.ErrSnapshotUnverified)
									}
								} else if strings.Contains(strings.ToLower(mName), "demucs") {
									if primaryBinding.DependencyName != DemucsModelID || primaryBinding.Version != DemucsModelVersion {
										return nil, fmt.Errorf("invalid Demucs snapshot binding %s:%s (must be exact RC %s:%s): %w",
											primaryBinding.DependencyName, primaryBinding.Version, DemucsModelID, DemucsModelVersion, domain.ErrSnapshotUnverified)
									}
								}

								ep, epErr := domain.ResolveSeparatorEntrypoint(primaryBinding.Manifest, primaryBinding.LocalPath, mName)
								if epErr != nil {
									return nil, fmt.Errorf("resolve separator model entrypoint: %w", epErr)
								}
								entrypointFile = ep
							} else if (cmd.Family == "audio_role" || cmd.Stage == "audio_role") && cmd.Stage != "audio_role_probe" {
								if primaryBinding.DependencyName != YAMNetModelID || primaryBinding.Version != YAMNetModelVersion {
									return nil, fmt.Errorf("invalid YAMNet snapshot binding %s:%s (must be exact RC %s:%s): %w",
										primaryBinding.DependencyName, primaryBinding.Version, YAMNetModelID, YAMNetModelVersion, domain.ErrSnapshotUnverified)
								}
								ep, epErr := domain.ResolveAudioRoleEntrypoint(primaryBinding.Manifest, primaryBinding.LocalPath, mName)
								if epErr != nil {
									return nil, fmt.Errorf("resolve audio_role model entrypoint: %w", epErr)
								}
								entrypointFile = ep
							}
							envelope := worker.ModelSnapshotEnvelope{
								Primary: worker.ModelSnapshotRef{
									Role:                   "primary",
									DependencyName:         primaryBinding.DependencyName,
									Version:                primaryBinding.Version,
									SnapshotManifestSHA256: primaryBinding.SnapshotManifestSHA256,
									LocalPath:              primaryBinding.LocalPath,
									EntrypointFile:         entrypointFile,
								},
								EntrypointFile: entrypointFile,
							}

							// Finding 1: Attach manifest-declared verified separator metadata assets
							if cmd.Family == "separator" || cmd.Stage == "separator" {
								if strings.Contains(strings.ToLower(mName), "uvr") || strings.Contains(strings.ToLower(mName), "mdx") {
									metaPath, metaErr := domain.ResolveSeparatorMetadataPath(primaryBinding.Manifest, primaryBinding.LocalPath)
									if metaErr != nil {
										return nil, fmt.Errorf("resolve UVR model metadata: %w", metaErr)
									}
									envelope.Dependencies = append(envelope.Dependencies, worker.ModelSnapshotRef{
										Role: "model_metadata",
										// Manifest-contained metadata shares the primary binding identity;
										// CheckBindingFingerprints covers every manifest file via the primary.
										DependencyName:         primaryBinding.DependencyName,
										Version:                primaryBinding.Version,
										SnapshotManifestSHA256: primaryBinding.SnapshotManifestSHA256,
										LocalPath:              primaryBinding.LocalPath,
										EntrypointFile:         metaPath,
									})
								} else if strings.Contains(strings.ToLower(mName), "demucs") {
									for _, f := range primaryBinding.Manifest.Files {
										norm := strings.ToLower(f.RelativePath)
										if norm == strings.ToLower(domain.PinnedDemucsBagYAML) || strings.HasSuffix(norm, "/"+strings.ToLower(domain.PinnedDemucsBagYAML)) {
											envelope.Dependencies = append(envelope.Dependencies, worker.ModelSnapshotRef{
												Role: "bag_yaml",
												// Manifest-contained bag YAML shares the primary binding identity; see above.
												DependencyName:         primaryBinding.DependencyName,
												Version:                primaryBinding.Version,
												SnapshotManifestSHA256: primaryBinding.SnapshotManifestSHA256,
												LocalPath:              primaryBinding.LocalPath,
												EntrypointFile:         filepath.Join(primaryBinding.LocalPath, f.RelativePath),
											})
											break
										}
									}
								}
							}
							if (cmd.Family == "audio_role" || cmd.Stage == "audio_role") && cmd.Stage != "audio_role_probe" {
								classMapPath, cmErr := domain.ResolveAudioRoleClassMapPath(primaryBinding.Manifest, primaryBinding.LocalPath)
								if cmErr != nil {
									return nil, fmt.Errorf("resolve audio_role class map: %w", cmErr)
								}
								envelope.Dependencies = append(envelope.Dependencies, worker.ModelSnapshotRef{
									Role:                   "class_map",
									DependencyName:         primaryBinding.DependencyName,
									Version:                primaryBinding.Version,
									SnapshotManifestSHA256: primaryBinding.SnapshotManifestSHA256,
									LocalPath:              primaryBinding.LocalPath,
									EntrypointFile:         classMapPath,
								})
							}
							if vadName, ok := cmd.Config["vad_model_name"].(string); ok && vadName != "" {
								vadVer, _ := cmd.Config["vad_model_version"].(string)
								if vadBinding, err := b.snapshotSvc.GetBinding(vadName, vadVer); err == nil && vadBinding != nil {
									envelope.Dependencies = append(envelope.Dependencies, worker.ModelSnapshotRef{
										Role:                   "vad",
										DependencyName:         vadBinding.DependencyName,
										Version:                vadBinding.Version,
										SnapshotManifestSHA256: vadBinding.SnapshotManifestSHA256,
										LocalPath:              vadBinding.LocalPath,
									})
								}
							}
							worker.SetModelSnapshotEnvelope(cmd.Config, envelope)
						}
					}
				}
			}
		}
	}

	// Validate model_snapshot envelope and check fingerprints before GPU lease
	if snapEnv, err := worker.GetModelSnapshotEnvelope(cmd.Config); err == nil && snapEnv != nil {
		if b.snapshotSvc != nil {
			if err := b.snapshotSvc.CheckBindingFingerprints(snapEnv.Primary.DependencyName, snapEnv.Primary.Version); err != nil {
				return nil, fmt.Errorf("pre-invocation snapshot verification failed: %w", err)
			}
			for _, dep := range snapEnv.Dependencies {
				if err := b.snapshotSvc.CheckBindingFingerprints(dep.DependencyName, dep.Version); err != nil {
					return nil, fmt.Errorf("pre-invocation dependency snapshot verification failed (%s): %w", dep.DependencyName, err)
				}
			}
		}
		if err := worker.ValidateSnapshotPaths(snapEnv); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrWorkerSnapshotPathRequired, err)
		}
	}

	if b.leaseManager != nil && !cmd.CPUOnly && cmd.Stage != "separator_probe" {
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
	id               string
	modelName        string
	modelVer         string
	capability       domain.ProviderCapability
	leaseManager     *worker.GPULeaseManager
	snapshotSvc      *governance.SnapshotService
	requiresSnapshot bool
	cpuOnly          bool
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
func (p *workerProviderBase) SetSnapshotService(svc *governance.SnapshotService) {
	p.snapshotSvc = svc
}
func (p *workerProviderBase) RequiresSnapshot() bool {
	return p.requiresSnapshot
}
func (p *workerProviderBase) SetRequiresSnapshot(req bool) {
	p.requiresSnapshot = req
}

// SetCPUOnly declares that this provider's worker stage runs on CPU only, so its
// commands must not acquire the authoritative GPU lease (Issue #91). It is off
// by default, which preserves accelerator-backed behavior for every provider
// that does not opt in.
func (p *workerProviderBase) SetCPUOnly(cpu bool) {
	p.cpuOnly = cpu
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
		id:               id,
		modelName:        modelName,
		modelVer:         modelVersion,
		requiresSnapshot: true,
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

// GatewayTranslationConfig configures optional OpenAI-compatible gateway translation lanes.
// Remote translation lanes are gateway-only and require an explicit gateway endpoint
// and runtime service baseline IDs. Without gateway configuration, the production
// registry exposes no translation lane and translation fails closed.
type GatewayTranslationConfig struct {
	Endpoint           string
	GeminiBaselineID   string
	DeepSeekBaselineID string
	HTTPClient         *http.Client
}

// NewProductionSpeechRegistry creates a Registry populated with the production
// worker-backed speech understanding providers (Qwen3-ASR 1.7B quality,
// Qwen3-ASR 0.6B fallback, Qwen3-ForcedAligner, and conditional Diarization).
// No fake providers are registered here (Issue #44 Finding 1).
func NewProductionSpeechRegistry(opts ...any) (*Registry, error) {
	var mgr *worker.GPULeaseManager
	var snapshotSvc *governance.SnapshotService
	requireSnapshots := true // Fail-closed by default for production RC path (Finding 1)
	var secretResolver SecretResolver
	var gatewayCfg GatewayTranslationConfig

	gatewayEndpoint := strings.TrimSpace(os.Getenv("DOUYINIE_GATEWAY_URL"))
	if gatewayEndpoint == "" {
		gatewayEndpoint = strings.TrimSpace(os.Getenv("DOUYINIE_GATEWAY_ENDPOINT"))
	}
	gatewayCfg.Endpoint = gatewayEndpoint
	gatewayCfg.GeminiBaselineID = strings.TrimSpace(os.Getenv("DOUYINIE_SERVICE_BASELINE_GEMINI"))
	gatewayCfg.DeepSeekBaselineID = strings.TrimSpace(os.Getenv("DOUYINIE_SERVICE_BASELINE_DEEPSEEK"))

	for _, opt := range opts {
		switch v := opt.(type) {
		case *worker.GPULeaseManager:
			mgr = v
		case *governance.SnapshotService:
			snapshotSvc = v
		case bool:
			requireSnapshots = v
		case SecretResolver:
			secretResolver = v
		case func(context.Context, string, string) (string, error):
			secretResolver = v
		case GatewayTranslationConfig:
			if v.Endpoint != "" {
				gatewayCfg.Endpoint = strings.TrimSpace(v.Endpoint)
			}
			if v.GeminiBaselineID != "" {
				gatewayCfg.GeminiBaselineID = strings.TrimSpace(v.GeminiBaselineID)
			}
			if v.DeepSeekBaselineID != "" {
				gatewayCfg.DeepSeekBaselineID = strings.TrimSpace(v.DeepSeekBaselineID)
			}
			if v.HTTPClient != nil {
				gatewayCfg.HTTPClient = v.HTTPClient
			}
		}
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

	// Production TTS worker adapters.
	// 1. ZeroTTS default VI lane (Issue #92/#93). CPU-only; preset voices; no rate control.
	zerotts, err := NewWorkerTTSProvider(ZeroTTSProviderID, ZeroTTSModelID, ZeroTTSModelVersion, []string{"vi"}, 0.95, FeatureFixedRateVoice)
	if err != nil {
		return nil, err
	}
	zerotts.SetCPUOnly(true)
	zerotts.SetLeaseManager(mgr)
	if err := reg.Register(zerotts); err != nil {
		return nil, err
	}

	// 2. VieNeu VI compatibility lane for historical frozen assignments.
	vieneu, err := NewWorkerTTSProvider(VieNeuProviderID, VieNeuModelID, VieNeuModelVersion, []string{"vi"}, 0.95)
	if err != nil {
		return nil, err
	}
	vieneu.SetLeaseManager(mgr)
	if err := reg.Register(vieneu); err != nil {
		return nil, err
	}

	// 3. CosyVoice3 measured-duration speed-fit lane (conditional only, not in default preset rotation)
	cosyvoice, err := NewWorkerTTSProvider(CosyVoiceProviderID, CosyVoiceModelID, CosyVoiceModelVersion, []string{"vi", "en"}, 0.98, "measured_duration_speed_fit")
	if err != nil {
		return nil, err
	}
	cosyvoice.SetLeaseManager(mgr)
	if err := reg.Register(cosyvoice); err != nil {
		return nil, err
	}

	// 4. Kokoro EN baseline (Issue #68: hexgrad/Kokoro-82M@f3ff3571791e39611d31c381e3a41a3af07b4987)
	kokoro, err := NewWorkerTTSProvider(KokoroProviderID, KokoroModelID, KokoroModelVersion, []string{"en"}, 0.95)
	if err != nil {
		return nil, err
	}
	kokoro.SetLeaseManager(mgr)
	if err := reg.Register(kokoro); err != nil {
		return nil, err
	}

	// Note (Issue #68): Chatterbox/voice cloning is excluded from the Phase 1.1 hard release route.

	// 5. Separator UVR baseline (Issue #69: UVR-MDX-NET-Inst_HQ_4.onnx)
	uvr, err := NewWorkerSeparatorProvider(UVRProviderID, UVRModelID, UVRModelVersion, 0.94)
	if err != nil {
		return nil, err
	}
	uvr.SetRequiresSnapshot(requireSnapshots)
	uvr.SetLeaseManager(mgr)
	if err := reg.Register(uvr); err != nil {
		return nil, err
	}

	// 6. Separator Demucs fallback (Issue #69: htdemucs)
	demucs, err := NewWorkerSeparatorProvider(DemucsProviderID, DemucsModelID, DemucsModelVersion, 0.88)
	if err != nil {
		return nil, err
	}
	demucs.SetRequiresSnapshot(requireSnapshots)
	demucs.SetLeaseManager(mgr)
	if err := reg.Register(demucs); err != nil {
		return nil, err
	}

	// 7. OCR PP-OCRv6 baseline
	ppocr, err := NewWorkerOCRProvider("ppocr_v6", "paddleocr-v6", "v6", 0.92)
	if err != nil {
		return nil, err
	}
	ppocr.SetLeaseManager(mgr)
	if err := reg.Register(ppocr); err != nil {
		return nil, err
	}

	// 7b. AudioRole YAMNet baseline (Issue #80)
	yamnet, err := NewWorkerAudioRoleProvider(YAMNetProviderID, YAMNetModelID, YAMNetModelVersion, 0.95)
	if err != nil {
		return nil, err
	}
	yamnet.SetRequiresSnapshot(requireSnapshots)
	yamnet.SetSnapshotService(snapshotSvc)
	yamnet.SetLeaseManager(mgr)
	if err := reg.Register(yamnet); err != nil {
		return nil, err
	}
	// 8. Production Translation Providers
	// Gateway translation providers are registered ONLY when an explicit
	// OpenAI-compatible gateway endpoint and baseline IDs are provided.
	// No local general-purpose translation model is registered in production.
	if gatewayCfg.Endpoint != "" && !isForbiddenDirectPublicEndpoint(gatewayCfg.Endpoint) {
		var gwOpts []any
		if secretResolver != nil {
			gwOpts = append(gwOpts, secretResolver)
		}
		if gatewayCfg.HTTPClient != nil {
			gwOpts = append(gwOpts, gatewayCfg.HTTPClient)
		}
		gwOpts = append(gwOpts, gatewayCfg.Endpoint)

		if gatewayCfg.GeminiBaselineID != "" {
			geminiGateway, err := NewGatewayTranslationProvider(
				"gateway_gemini_3_8_flash",
				"gemini-3.8-flash",
				gatewayCfg.GeminiBaselineID,
				0.99,
				gwOpts...,
			)
			if err != nil {
				return nil, err
			}
			if err := reg.Register(geminiGateway); err != nil {
				return nil, err
			}
		}

		if gatewayCfg.DeepSeekBaselineID != "" {
			deepseekGateway, err := NewGatewayTranslationProvider(
				"gateway_deepseek_v4_flash_vision_exp",
				"deepseek/deepseek-v4-flash-vision-exp",
				gatewayCfg.DeepSeekBaselineID,
				0.95,
				gwOpts...,
			)
			if err != nil {
				return nil, err
			}
			if err := reg.Register(deepseekGateway); err != nil {
				return nil, err
			}
		}
	}
	if snapshotSvc != nil {
		for _, p := range reg.ListAll() {
			if workerBacked, ok := p.(interface {
				SetSnapshotService(*governance.SnapshotService)
			}); ok {
				workerBacked.SetSnapshotService(snapshotSvc)
			}
		}
	}
	if !requireSnapshots {
		reg.SetRequireSnapshots(false)
	}
	return reg, nil
}

type asrArtifact struct {
	Segments []domain.ASRRawSegment `json:"segments"`
}

func (p *WorkerASRProvider) ProduceTranscript(ctx context.Context, audio worker.ArtifactRef) ([]domain.ASRRawSegment, error) {
	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("asr", stageWorkerFamilyASR, audio, p.modelName, p.modelVer)
	if p.requiresSnapshot {
		cmd.Config["require_model_snapshot"] = true
	}
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
		id:               id,
		modelName:        modelName,
		modelVer:         modelVersion,
		requiresSnapshot: true,
		capability: domain.ProviderCapability{
			Stage:          "aligner",
			Languages:      []string{"zh"},
			ExecutionTier:  "local",
			CostPerUnit:    0,
			QualityScore:   0.9,
			MaxConcurrency: 1,
			Features:       []string{"forced_alignment", "character_timestamps"},
		},
	}}, nil
}

type alignerArtifact struct {
	WordTimings []domain.WordTiming `json:"word_timings"`
}

func (p *WorkerAlignerProvider) ProduceAlignment(ctx context.Context, audio worker.ArtifactRef, text string) ([]domain.WordTiming, error) {
	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("aligner", stageWorkerFamilyAligner, audio, p.modelName, p.modelVer)
	if p.requiresSnapshot {
		cmd.Config["require_model_snapshot"] = true
	}
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
			id:               id,
			modelName:        modelName,
			modelVer:         modelVersion,
			requiresSnapshot: true,
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
	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
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
	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
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
			id:               id,
			modelName:        modelName,
			modelVer:         modelVersion,
			requiresSnapshot: true,
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
	if strings.Contains(p.id, "cosyvoice") {
		var voices []domain.VoiceProfile
		for _, l := range p.capability.Languages {
			voices = append(voices, CosyVoicePresetVoices(l)...)
		}
		return voices
	}
	if p.id == ZeroTTSProviderID {
		// The full verified preset catalog stays selectable; only its leading
		// entries are the unattended default rotation (DefaultPresetVoices).
		return ZeroTTSPresetVoices()
	}
	if p.id == VieNeuProviderID {
		// VieNeu compatibility lane for historical frozen assignments.
		return VieNeuPresetVoices()
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

func (p *WorkerTTSProvider) ensureZeroTTSRuntimeIdentity(ctx context.Context) error {
	if p.id != ZeroTTSProviderID || !p.requiresSnapshot {
		return nil
	}
	if p.snapshotSvc == nil {
		return fmt.Errorf("%w: snapshot service not configured for ZeroTTS %s:%s",
			domain.ErrSnapshotUnverified, p.modelName, p.modelVer)
	}
	binding, err := p.snapshotSvc.GetBinding(ZeroTTSModelID, ZeroTTSModelVersion)
	if err != nil || binding == nil {
		return fmt.Errorf("%w: missing exact ZeroTTS model snapshot %s:%s",
			domain.ErrSnapshotUnverified, ZeroTTSModelID, ZeroTTSModelVersion)
	}
	if err := p.snapshotSvc.CheckBindingFingerprints(binding.DependencyName, binding.Version); err != nil {
		return fmt.Errorf("%w: ZeroTTS snapshot validation failed: %v", domain.ErrSnapshotMutatedRehashRequired, err)
	}
	if binding.RuntimeIdentity != nil {
		rt := binding.RuntimeIdentity
		if rt.SourceRevision != domain.PinnedZeroTTSSourceRevision ||
			rt.RuntimeVersions["zerotts"] != domain.PinnedZeroTTSPackageVersion ||
			rt.AdapterRevision != domain.PinnedZeroTTSAdapterRevision {
			return fmt.Errorf("%w: invalid ZeroTTS runtime identity evidence", domain.ErrSnapshotUnverified)
		}
		return nil
	}

	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrSnapshotUnverified, err)
	}
	cmd := newCommand("tts_probe", stageWorkerFamilyTTS, worker.ArtifactRef{}, p.modelName, p.modelVer)
	cmd.CPUOnly = true
	cmd.Config["mode"] = "probe"
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-tts-probe-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)
	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%w: ZeroTTS runtime probe failed: %v", domain.ErrSnapshotUnverified, err)
	}
	var probe struct {
		Status          string            `json:"status"`
		PackageName     string            `json:"package_name"`
		PackageVersion  string            `json:"package_version"`
		SourceRevision  string            `json:"source_revision"`
		RuntimeVersions map[string]string `json:"runtime_versions"`
		AdapterRevision string            `json:"adapter_revision"`
		Error           string            `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("%w: invalid ZeroTTS runtime probe output: %v", domain.ErrSnapshotUnverified, err)
	}
	if probe.Status != "ok" || probe.Error != "" {
		return fmt.Errorf("%w: ZeroTTS runtime probe rejected: status=%q error=%q", domain.ErrSnapshotUnverified, probe.Status, probe.Error)
	}
	rt := domain.RuntimeIdentity{
		AdapterRevision:       probe.AdapterRevision,
		SourceRevision:        probe.SourceRevision,
		RuntimeVersions:       probe.RuntimeVersions,
		PrimarySnapshotSHA256: binding.SnapshotManifestSHA256,
	}
	rt.RuntimeManifestSHA256 = rt.ComputeRuntimeManifestSHA256()
	if err := p.snapshotSvc.SetRuntimeIdentity(binding.DependencyName, binding.Version, rt); err != nil {
		return fmt.Errorf("set ZeroTTS runtime identity: %w", err)
	}
	return nil
}

func (p *WorkerTTSProvider) SynthesizeSpeech(ctx context.Context, req TTSSynthesisRequest) (*TTSSynthesisResult, error) {
	if !IsVerifiedTTSVoice(p.id, req.Voice.VoiceID) {
		return nil, fmt.Errorf("%w: unverified voice preset %q for provider %s", domain.ErrTTSVoiceAssetMissing, req.Voice.VoiceID, p.id)
	}
	if p.id == ZeroTTSProviderID && req.Speed != 1.0 {
		return nil, fmt.Errorf("%w: ZeroTTS requires speed=1.0, got %.3f", domain.ErrTTSSpeedUnsupported, req.Speed)
	}
	if err := p.ensureZeroTTSRuntimeIdentity(ctx); err != nil {
		return nil, err
	}

	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("tts", stageWorkerFamilyTTS, worker.ArtifactRef{SHA256: req.AssetID}, p.modelName, p.modelVer)
	cmd.CPUOnly = p.cpuOnly
	cmd.Config["text"] = req.Text
	cmd.Config["language"] = req.Language
	cmd.Config["voice_id"] = req.Voice.VoiceID
	cmd.Config["speed"] = fmt.Sprintf("%.2f", req.Speed)
	cmd.Config["slot_duration_ms"] = fmt.Sprintf("%d", req.SlotDurationMs)
	if p.requiresSnapshot {
		cmd.Config["require_model_snapshot"] = true
	}
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

	// Truthful media metadata is probed from the produced artifact, never assumed:
	// VieNeu emits 48 kHz and Kokoro/CosyVoice3 24 kHz mono WAV, so a generic
	// 16000 Hz/mono answer would be a lie. The WAV probe is authoritative for the
	// media properties and for measured duration, and fails closed on media that
	// is missing, truncated or not a valid WAV container.
	probe, err := media.ParseWAVHeader(audioBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: synthesized TTS media is not a valid WAV artifact: %v", domain.ErrQualityRejected, err)
	}

	return &TTSSynthesisResult{
		AudioData:           audioBytes,
		AudioSHA256:         art.AudioSHA256,
		AudioCASPath:        art.AudioPath,
		SampleRate:          int(probe.SampleRate),
		Channels:            int(probe.NumChannels),
		Format:              "wav",
		ProviderID:          p.id,
		ModelName:           p.modelName,
		ModelVersion:        p.modelVer,
		PredictedDurationMs: art.PredictedDurationMs,
		MeasuredDurationMs:  probe.DurationMs,
	}, nil
}

// WorkerSeparatorProvider is a StageWorker-backed AudioSeparatorProvider.
type WorkerSeparatorProvider struct {
	workerProviderBase
}

func NewWorkerSeparatorProvider(id, modelName, modelVer string, qualityScore float64) (*WorkerSeparatorProvider, error) {
	if id == "" || modelName == "" || modelVer == "" {
		return nil, fmt.Errorf("worker separator provider requires id, model_name and model_version")
	}
	if qualityScore <= 0 {
		qualityScore = 0.94
	}
	return &WorkerSeparatorProvider{workerProviderBase{
		id:               id,
		modelName:        modelName,
		modelVer:         modelVer,
		requiresSnapshot: true,
		capability: domain.ProviderCapability{
			Stage:          "separator",
			Languages:      []string{"*"},
			ExecutionTier:  "local",
			CostPerUnit:    0,
			QualityScore:   qualityScore,
			MaxConcurrency: 1,
			Features:       []string{"stem_separation"},
		},
	}}, nil
}

type separatorArtifact struct {
	VocalsData       string `json:"vocals_data"`
	BackgroundData   string `json:"background_data"`
	VocalsSHA256     string `json:"vocals_sha256"`
	BackgroundSHA256 string `json:"background_sha256"`
	DurationMs       int64  `json:"duration_ms"`
	SampleRate       int    `json:"sample_rate"`
	Channels         int    `json:"channels"`
	RuntimeIdentity  string `json:"runtime_identity"`
}

// EnsureRuntimeIdentity probes the configured separator Python runtime and establishes
// validated RuntimeIdentity evidence via SnapshotService.SetRuntimeIdentity if not already set.
// It caches the validated binding and fails closed on missing package, wrong version, or
// inability to prove the exact pinned VCS commit from trustworthy PEP 610 metadata.
func (p *WorkerSeparatorProvider) EnsureRuntimeIdentity(ctx context.Context) error {
	if !p.requiresSnapshot {
		return nil
	}
	if p.snapshotSvc == nil {
		return fmt.Errorf("%w: snapshot service not configured for separator %s:%s",
			domain.ErrSnapshotUnverified, p.modelName, p.modelVer)
	}

	binding, _ := p.snapshotSvc.GetBinding(p.modelName, p.modelVer)
	if binding == nil {
		if strings.Contains(strings.ToLower(p.modelName), "demucs") {
			binding, _ = p.snapshotSvc.GetBinding(DemucsModelID, DemucsModelVersion)
		} else {
			binding, _ = p.snapshotSvc.GetBinding(UVRModelID, UVRModelVersion)
		}
	}
	if binding == nil {
		return fmt.Errorf("%w: missing or unverified model snapshot for separator %s:%s",
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

	// Probe the configured separator Python runtime via StageWorker
	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrSnapshotUnverified, err)
	}

	cmd := worker.Command{
		ID:        uuid.NewString(),
		Family:    stageWorkerFamilySeparator,
		Stage:     "separator_probe",
		AttemptID: uuid.NewString(),
		Config: map[string]any{
			cfgModelName:                         p.modelName,
			cfgModelVersion:                      p.modelVer,
			"mode":                               "probe",
			worker.ConfigKeyRequireModelSnapshot: true,
		},
		OutputPath: filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-sep-probe-%s.json", uuid.NewString())),
	}
	defer os.Remove(cmd.OutputPath)

	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%w: separator runtime probe failed: %v", domain.ErrSnapshotUnverified, err)
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
		return fmt.Errorf("%w: invalid separator probe output: %v", domain.ErrSnapshotUnverified, err)
	}
	if probeArt.Error != "" {
		return fmt.Errorf("%w: %s", domain.ErrSnapshotUnverified, probeArt.Error)
	}
	if !strings.HasPrefix(probeArt.AdapterRevision, "cmd/stageworker/adapters/separator.py@v") {
		return fmt.Errorf("%w: unverified adapter revision %q in runtime identity (must originate from repo adapter cmd/stageworker/adapters/separator.py)",
			domain.ErrSnapshotUnverified, probeArt.AdapterRevision)
	}
	// Collect declared dependency snapshot SHAs from verified binding
	var depSHAs []string
	if len(binding.Manifest.Files) > 1 {
		for _, f := range binding.Manifest.Files {
			norm := strings.ToLower(domain.NormalizeRelativePath(f.RelativePath))
			if !strings.EqualFold(norm, strings.ToLower(binding.DependencyName)) &&
				!strings.HasSuffix(norm, "/"+strings.ToLower(binding.DependencyName)) &&
				len(f.SHA256) == 64 {
				depSHAs = append(depSHAs, f.SHA256)
			}
		}
	}

	adapterRev := probeArt.AdapterRevision
	if adapterRev == "" {
		adapterRev = fmt.Sprintf("cmd/stageworker/adapters/separator.py@v%s", probeArt.PackageVersion)
	}

	rt := domain.RuntimeIdentity{
		AdapterRevision:        adapterRev,
		SourceRevision:         probeArt.SourceRevision,
		RuntimeVersions:        probeArt.RuntimeVersions,
		PrimarySnapshotSHA256:  binding.SnapshotManifestSHA256,
		DependencySnapshotSHAs: depSHAs,
	}
	rt.RuntimeManifestSHA256 = rt.ComputeRuntimeManifestSHA256()

	if err := p.snapshotSvc.SetRuntimeIdentity(binding.DependencyName, binding.Version, rt); err != nil {
		return fmt.Errorf("set runtime identity: %w", err)
	}

	return nil
}

func (p *WorkerSeparatorProvider) SeparateStems(ctx context.Context, req SeparationRequest) (*SeparationResult, error) {
	if p.requiresSnapshot {
		if err := p.EnsureRuntimeIdentity(ctx); err != nil {
			return nil, err
		}
		if p.snapshotSvc == nil {
			return nil, fmt.Errorf("%w: snapshot service not configured for separator %s:%s",
				domain.ErrSnapshotUnverified, p.modelName, p.modelVer)
		}
		binding, _ := p.snapshotSvc.GetBinding(p.modelName, p.modelVer)
		if binding == nil {
			if strings.Contains(strings.ToLower(p.modelName), "demucs") {
				binding, _ = p.snapshotSvc.GetBinding(DemucsModelID, DemucsModelVersion)
			} else {
				binding, _ = p.snapshotSvc.GetBinding(UVRModelID, UVRModelVersion)
			}
		}
		if binding == nil || binding.RuntimeIdentity == nil {
			return nil, fmt.Errorf("%w: missing required runtime identity evidence for separator %s:%s",
				domain.ErrSnapshotUnverified, p.modelName, p.modelVer)
		}
		if strings.Contains(strings.ToLower(p.modelName), "demucs") {
			if binding.RuntimeIdentity.SourceRevision != domain.PinnedDemucsSourceRevision {
				return nil, fmt.Errorf("%w: Demucs runtime source revision mismatch: expected %s, got %s",
					domain.ErrSnapshotUnverified, domain.PinnedDemucsSourceRevision, binding.RuntimeIdentity.SourceRevision)
			}
			if ver, ok := binding.RuntimeIdentity.RuntimeVersions["demucs"]; !ok || strings.TrimSpace(ver) != domain.PinnedDemucsPackageVersion {
				return nil, fmt.Errorf("%w: Demucs runtime package version mismatch: expected exact %s, got %s",
					domain.ErrSnapshotUnverified, domain.PinnedDemucsPackageVersion, ver)
			}
		} else {
			if binding.RuntimeIdentity.SourceRevision != domain.PinnedUVRSourceRevision {
				return nil, fmt.Errorf("%w: UVR runtime source revision mismatch: expected %s, got %s",
					domain.ErrSnapshotUnverified, domain.PinnedUVRSourceRevision, binding.RuntimeIdentity.SourceRevision)
			}
			if ver, ok := binding.RuntimeIdentity.RuntimeVersions["audio-separator"]; !ok || strings.TrimSpace(ver) != domain.PinnedUVRPackageVersion {
				return nil, fmt.Errorf("%w: UVR runtime package version mismatch: expected exact %s, got %s",
					domain.ErrSnapshotUnverified, domain.PinnedUVRPackageVersion, ver)
			}
		}
	}

	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("separator", stageWorkerFamilySeparator, req.SourceAudio, p.modelName, p.modelVer)
	cmd.Config["require_model_snapshot"] = p.requiresSnapshot
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-sep-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)

	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var art separatorArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return nil, fmt.Errorf("%w: invalid separator artifact JSON: %v", domain.ErrQualityRejected, err)
	}

	var vocalsBytes, bgBytes []byte
	if art.VocalsData != "" {
		if decoded, err := base64.StdEncoding.DecodeString(art.VocalsData); err == nil {
			vocalsBytes = decoded
		} else if fileData, err := os.ReadFile(art.VocalsData); err == nil {
			vocalsBytes = fileData
		}
	}
	if art.BackgroundData != "" {
		if decoded, err := base64.StdEncoding.DecodeString(art.BackgroundData); err == nil {
			bgBytes = decoded
		} else if fileData, err := os.ReadFile(art.BackgroundData); err == nil {
			bgBytes = fileData
		}
	}

	if len(bgBytes) == 0 {
		return nil, fmt.Errorf("%w: separator produced empty or unreadable background stem", domain.ErrQualityRejected)
	}

	durMs := art.DurationMs
	if durMs <= 0 && len(bgBytes) > 0 {
		durMs, _ = media.ProbeWAVBytes(bgBytes)
	}
	var snapSHA string
	if snapEnv, err := worker.GetModelSnapshotEnvelope(cmd.Config); err == nil && snapEnv != nil {
		snapSHA = snapEnv.Primary.SnapshotManifestSHA256
	}
	runtimeIdentity := art.RuntimeIdentity
	if p.snapshotSvc != nil {
		binding, _ := p.snapshotSvc.GetBinding(p.modelName, p.modelVer)
		if binding == nil {
			if strings.Contains(strings.ToLower(p.modelName), "demucs") {
				binding, _ = p.snapshotSvc.GetBinding(DemucsModelID, DemucsModelVersion)
			} else {
				binding, _ = p.snapshotSvc.GetBinding(UVRModelID, UVRModelVersion)
			}
		}

		if p.requiresSnapshot {
			if binding == nil || binding.RuntimeIdentity == nil {
				return nil, fmt.Errorf("%w: missing required runtime identity evidence for separator %s:%s",
					domain.ErrSnapshotUnverified, p.modelName, p.modelVer)
			}

			// Validate binding runtime identity against pinned source revision and package version
			if strings.Contains(strings.ToLower(p.modelName), "demucs") {
				if binding.RuntimeIdentity.SourceRevision != domain.PinnedDemucsSourceRevision {
					return nil, fmt.Errorf("%w: Demucs runtime source revision mismatch: expected %s, got %s",
						domain.ErrSnapshotUnverified, domain.PinnedDemucsSourceRevision, binding.RuntimeIdentity.SourceRevision)
				}
				if ver, ok := binding.RuntimeIdentity.RuntimeVersions["demucs"]; !ok || strings.TrimSpace(ver) != domain.PinnedDemucsPackageVersion {
					return nil, fmt.Errorf("%w: Demucs runtime package version mismatch: expected exact %s, got %s",
						domain.ErrSnapshotUnverified, domain.PinnedDemucsPackageVersion, ver)
				}
			} else {
				if binding.RuntimeIdentity.SourceRevision != domain.PinnedUVRSourceRevision {
					return nil, fmt.Errorf("%w: UVR runtime source revision mismatch: expected %s, got %s",
						domain.ErrSnapshotUnverified, domain.PinnedUVRSourceRevision, binding.RuntimeIdentity.SourceRevision)
				}
				if ver, ok := binding.RuntimeIdentity.RuntimeVersions["audio-separator"]; !ok || strings.TrimSpace(ver) != domain.PinnedUVRPackageVersion {
					return nil, fmt.Errorf("%w: UVR runtime package version mismatch: expected exact %s, got %s",
						domain.ErrSnapshotUnverified, domain.PinnedUVRPackageVersion, ver)
				}
			}
		}

		if binding != nil && binding.RuntimeIdentity != nil {
			rmSHA := binding.RuntimeIdentity.RuntimeManifestSHA256
			if rmSHA == "" {
				rmSHA = binding.RuntimeIdentity.ComputeRuntimeManifestSHA256()
			}
			runtimeIdentity = fmt.Sprintf("%s @ %s [manifest=%s, snap=%s]",
				binding.DependencyName, binding.RuntimeIdentity.SourceRevision, rmSHA, binding.SnapshotManifestSHA256)
		}
	}

	if runtimeIdentity == "" {
		if p.requiresSnapshot {
			return nil, fmt.Errorf("%w: failed to establish evidence-backed runtime identity for %s:%s",
				domain.ErrSnapshotUnverified, p.modelName, p.modelVer)
		}
		if strings.Contains(strings.ToLower(p.modelName), "demucs") {
			runtimeIdentity = "demucs " + domain.PinnedDemucsPackageVersion
		} else {
			runtimeIdentity = "python-audio-separator " + domain.PinnedUVRPackageVersion
		}
	}

	return &SeparationResult{
		ProviderID:             p.id,
		ModelName:              p.modelName,
		ModelVersion:           p.modelVer,
		SnapshotManifestSHA256: snapSHA,
		RuntimeIdentity:        runtimeIdentity,
		VocalsWAV:              vocalsBytes,
		BackgroundWAV:          bgBytes,
		DurationMs:             durMs,
		SampleRate:             art.SampleRate,
		Channels:               art.Channels,
	}, nil
}

func (p *WorkerSeparatorProvider) ObservedModel() string {
	return p.modelName
}

func (p *WorkerSeparatorProvider) ServiceBaselineID() string {
	if p.snapshotSvc != nil {
		_ = p.EnsureRuntimeIdentity(context.Background())
		binding, _ := p.snapshotSvc.GetBinding(p.modelName, p.modelVer)
		if binding == nil {
			if strings.Contains(strings.ToLower(p.modelName), "demucs") {
				binding, _ = p.snapshotSvc.GetBinding(DemucsModelID, DemucsModelVersion)
			} else {
				binding, _ = p.snapshotSvc.GetBinding(UVRModelID, UVRModelVersion)
			}
		}
		if binding != nil && binding.RuntimeIdentity != nil {
			rmSHA := binding.RuntimeIdentity.RuntimeManifestSHA256
			if rmSHA == "" {
				rmSHA = binding.RuntimeIdentity.ComputeRuntimeManifestSHA256()
			}
			if rmSHA != "" {
				return fmt.Sprintf("baseline-%s-%s", p.id, rmSHA)
			}
		}
	}
	if p.requiresSnapshot {
		return ""
	}
	return p.id
}

var _ interface {
	Provider
	AudioSeparatorProvider
	interface{ ObservedModel() string }
	interface{ ServiceBaselineID() string }
} = (*WorkerSeparatorProvider)(nil)

type WorkerOCRProvider struct {
	workerProviderBase
	detModelName string
	detModelVer  string
	recModelName string
	recModelVer  string
	oriModelName string
	oriModelVer  string
}

func NewWorkerOCRProvider(id, modelName, modelVer string, qualityScore float64) (*WorkerOCRProvider, error) {
	if id == "" || modelName == "" || modelVer == "" {
		return nil, fmt.Errorf("worker ocr provider requires id, model_name and model_version")
	}
	if qualityScore <= 0 {
		qualityScore = 0.92
	}
	return &WorkerOCRProvider{
		workerProviderBase: workerProviderBase{
			id:               id,
			modelName:        modelName,
			modelVer:         modelVer,
			requiresSnapshot: true,
			capability: domain.ProviderCapability{
				Stage:          "ocr",
				Languages:      []string{"zh", "en"},
				ExecutionTier:  "local",
				CostPerUnit:    0,
				QualityScore:   qualityScore,
				MaxConcurrency: 1,
				Features:       []string{"text_detection", "text_recognition"},
			},
		},
		detModelName: "PP-OCRv6_medium_det",
		detModelVer:  "v6",
		recModelName: "PP-OCRv6_medium_rec",
		recModelVer:  "v6",
		oriModelName: "PP-LCNet_x1_0_textline_ori",
		oriModelVer:  "v1",
	}, nil
}

// ModelDependencies implements DependentModelProvider for WorkerOCRProvider.
func (p *WorkerOCRProvider) ModelDependencies() []ModelDependency {
	return []ModelDependency{
		{Role: "det", Name: p.detModelName, Version: p.detModelVer},
		{Role: "rec", Name: p.recModelName, Version: p.recModelVer},
		{Role: "ori", Name: p.oriModelName, Version: p.oriModelVer},
	}
}

// PrimaryCheckpointRequired implements PrimaryCheckpointProvider for WorkerOCRProvider.
// ModelInfo() (paddleocr-v6:v6) is a logical/runtime family identity, not a real
// primary checkpoint; the 3 ModelDependencies() (det/rec/ori) are the actual
// checkpoints. Returns false so Router skips only top-level checkpoint checks.
// Dependency license + snapshot validation stays mandatory, and requiresSnapshot
// stays true so workerBridge still validates the det/rec/ori envelope.
func (p *WorkerOCRProvider) PrimaryCheckpointRequired() bool { return false }

// SetDependencies allows configuring custom dependency names and versions.
func (p *WorkerOCRProvider) SetDependencies(detName, detVer, recName, recVer, oriName, oriVer string) {
	if detName != "" {
		p.detModelName = detName
	}
	if detVer != "" {
		p.detModelVer = detVer
	}
	if recName != "" {
		p.recModelName = recName
	}
	if recVer != "" {
		p.recModelVer = recVer
	}
	if oriName != "" {
		p.oriModelName = oriName
	}
	if oriVer != "" {
		p.oriModelVer = oriVer
	}
}

// OCRSnapshotDigests returns the verified SHA-256 digests of all 3 model snapshots and the runtime identity.
func (p *WorkerOCRProvider) OCRSnapshotDigests() (detSHA, recSHA, oriSHA, runtimeIdentity string) {
	runtimeIdentity = "paddleocr:3.7.0@b03f46425e8ff4442b268ce449e3eef758146cd4"
	if p.snapshotSvc != nil {
		if b, _ := p.snapshotSvc.GetBinding(p.detModelName, p.detModelVer); b != nil {
			detSHA = b.SnapshotManifestSHA256
		}
		if b, _ := p.snapshotSvc.GetBinding(p.recModelName, p.recModelVer); b != nil {
			recSHA = b.SnapshotManifestSHA256
		}
		if b, _ := p.snapshotSvc.GetBinding(p.oriModelName, p.oriModelVer); b != nil {
			oriSHA = b.SnapshotManifestSHA256
		}
	}
	return detSHA, recSHA, oriSHA, runtimeIdentity
}

// ObservedModel exposes the verified snapshot short digests in execution attempts.
func (p *WorkerOCRProvider) ObservedModel() string {
	detSHA, recSHA, oriSHA, _ := p.OCRSnapshotDigests()
	if len(detSHA) > 8 {
		detSHA = detSHA[:8]
	}
	if len(recSHA) > 8 {
		recSHA = recSHA[:8]
	}
	if len(oriSHA) > 8 {
		oriSHA = oriSHA[:8]
	}
	if detSHA == "" {
		detSHA = "unverified"
	}
	if recSHA == "" {
		recSHA = "unverified"
	}
	if oriSHA == "" {
		oriSHA = "unverified"
	}
	return fmt.Sprintf("PP-OCRv6_medium[det=%s,rec=%s,ori=%s]", detSHA, recSHA, oriSHA)
}

// ServiceBaselineID exposes the service baseline ID for attempt provenance.
func (p *WorkerOCRProvider) ServiceBaselineID() string {
	return "paddleocr-3.7.0-b03f46425e8ff4442b268ce449e3eef758146cd4"
}

type ocrArtifact struct {
	FrameWidth        int                `json:"frame_width"`
	FrameHeight       int                `json:"frame_height"`
	FrameSampleStepMs int64              `json:"frame_sample_step_ms"`
	Detections        []RawTextDetection `json:"detections"`
}

func (p *WorkerOCRProvider) DetectRegions(ctx context.Context, req OCRRequest) (*OCRResult, error) {
	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}
	cmd := newCommand("ocr", stageWorkerFamilyOCR, req.SourceVideo, p.modelName, p.modelVer)
	cmd.Config["frame_sample_step_ms"] = req.FrameSampleStepMs
	cmd.Config["max_frames"] = req.MaxFrames
	cmd.Config["require_model_snapshot"] = p.requiresSnapshot
	cmd.Config["det_model_name"] = p.detModelName
	cmd.Config["det_model_version"] = p.detModelVer
	cmd.Config["rec_model_name"] = p.recModelName
	cmd.Config["rec_model_version"] = p.recModelVer
	cmd.Config["ori_model_name"] = p.oriModelName
	cmd.Config["ori_model_version"] = p.oriModelVer
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-ocr-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)

	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var art ocrArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return nil, fmt.Errorf("%w: invalid ocr artifact JSON: %v", domain.ErrQualityRejected, err)
	}

	detSHA, recSHA, oriSHA, runtimeIdentity := p.OCRSnapshotDigests()

	return &OCRResult{
		ProviderID:        p.id,
		ModelName:         p.modelName,
		ModelVersion:      p.modelVer,
		FrameWidth:        art.FrameWidth,
		FrameHeight:       art.FrameHeight,
		FrameSampleStepMs: art.FrameSampleStepMs,
		Detections:        art.Detections,
		DetSnapshotDigest: detSHA,
		RecSnapshotDigest: recSHA,
		OriSnapshotDigest: oriSHA,
		RuntimeIdentity:   runtimeIdentity,
	}, nil
}

var _ interface {
	Provider
	OCRRegionProvider
	DependentModelProvider
	PrimaryCheckpointProvider
	OCRSnapshotProvider
	interface{ ObservedModel() string }
	interface{ ServiceBaselineID() string }
} = (*WorkerOCRProvider)(nil)

// ---------------------------------------------------------------------------
// Translation adapter
// ---------------------------------------------------------------------------

// WorkerTranslationProvider is a StageWorker-backed TextTranslationProvider for Qwen3-4B-GGUF Q4_K_M.
type WorkerTranslationProvider struct {
	workerProviderBase
}

var _ interface {
	Provider
	TextTranslationProvider
} = (*WorkerTranslationProvider)(nil)

// NewWorkerTranslationProvider builds a local translation provider adapter bound to Qwen3-4B-GGUF Q4_K_M.
func NewWorkerTranslationProvider(id, modelName, modelVer string, qualityScore float64) (*WorkerTranslationProvider, error) {
	if id == "" || modelName == "" || modelVer == "" {
		return nil, fmt.Errorf("worker translation provider requires id, model_name and model_version")
	}
	if qualityScore <= 0 {
		qualityScore = 0.85
	}
	return &WorkerTranslationProvider{workerProviderBase{
		id:               id,
		modelName:        modelName,
		modelVer:         modelVer,
		requiresSnapshot: true,
		capability: domain.ProviderCapability{
			Stage:          string(TypeTranslation),
			Languages:      []string{"vi", "en"},
			ExecutionTier:  "local",
			CostPerUnit:    0,
			QualityScore:   qualityScore,
			MaxConcurrency: 1,
			Features:       []string{"meaning_preservation", "qwen3_translation"},
		},
	}}, nil
}

type translationArtifact struct {
	Segments     []domain.TranslationSegment `json:"segments"`
	ModelName    string                      `json:"model_name"`
	ModelVersion string                      `json:"model_version"`
}

func (p *WorkerTranslationProvider) TranslateText(ctx context.Context, req TranslationRequest) (*TranslationResult, error) {
	if len(req.Segments) == 0 {
		return nil, domain.ErrEmptyTranslationInput
	}
	bridge, err := newWorkerBridge(p.leaseManager, p.snapshotSvc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNoEligibleProvider, err)
	}

	cmd := worker.Command{
		ID:        uuid.NewString(),
		Family:    "translation",
		Stage:     "translation",
		AttemptID: uuid.NewString(),
		Config: map[string]any{
			cfgModelName:             p.modelName,
			cfgModelVersion:          p.modelVer,
			"require_model_snapshot": p.requiresSnapshot,
			"source_language":        req.SourceLanguage,
			"target_language":        req.TargetLanguage,
			"segments":               req.Segments,
		},
	}
	cmd.OutputPath = filepath.Join(os.TempDir(), fmt.Sprintf("douyinie-trans-%s.json", cmd.ID))
	defer os.Remove(cmd.OutputPath)

	data, err := bridge.run(ctx, cmd)
	if err != nil {
		return nil, err
	}

	var art translationArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return nil, fmt.Errorf("%w: invalid translation artifact JSON: %v", domain.ErrQualityRejected, err)
	}

	if len(art.Segments) == 0 {
		return nil, fmt.Errorf("%w: translation produced no segments", domain.ErrQualityRejected)
	}

	return &TranslationResult{
		ProviderID:   p.id,
		ModelName:    p.modelName,
		ModelVersion: p.modelVer,
		Segments:     art.Segments,
	}, nil
}
