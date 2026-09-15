package seam2_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/governance"
	"github.com/monet88/douyinie/internal/media"
	"github.com/monet88/douyinie/internal/provider"
	"github.com/monet88/douyinie/internal/scheduler"
	"github.com/monet88/douyinie/internal/storage"
	"github.com/monet88/douyinie/internal/worker"
)

const zeroTTSRealSnapshotWindows = `D:\douyinie-ref\zerotts-benchmark\cache\huggingface\hub\models--zeroweight-ai--ZeroTTS\snapshots\8a0c3c29f6f047011f5cae02d0b14475a690be86`
const zeroTTSRealPythonWindows = `D:\douyinie-ref\phase1.1-runtime\venvs\tts-zerotts-0.1.2-9d85578\Scripts\python.exe`

func resolveRealZeroTTSFixture(t *testing.T) (pythonBin, snapshotRoot string) {
	t.Helper()
	pythonBin = strings.TrimSpace(os.Getenv("DOUYINIE_ZEROTTS_PYTHON_BIN"))
	if pythonBin == "" {
		pythonBin = strings.TrimSpace(os.Getenv("DOUYINIE_TTS_PYTHON_BIN"))
	}
	snapshotRoot = strings.TrimSpace(os.Getenv("DOUYINIE_ZEROTTS_MODEL_SNAPSHOT"))
	if runtime.GOOS == "windows" {
		if pythonBin == "" {
			pythonBin = zeroTTSRealPythonWindows
		}
		if snapshotRoot == "" {
			snapshotRoot = zeroTTSRealSnapshotWindows
		}
	}
	if fi, err := os.Stat(pythonBin); err != nil || fi.IsDir() {
		t.Skipf("real ZeroTTS runtime pack absent: %s", pythonBin)
	}
	if fi, err := os.Stat(snapshotRoot); err != nil || !fi.IsDir() {
		t.Skipf("real ZeroTTS model snapshot absent: %s", snapshotRoot)
	}
	return pythonBin, snapshotRoot
}

func stageZeroTTSSnapshotForGovernance(t *testing.T, sourceRoot string) string {
	t.Helper()
	stageRoot, err := os.MkdirTemp(filepath.Dir(sourceRoot), "douyinie-zerotts-seam2-")
	if err != nil {
		t.Fatalf("create ZeroTTS staging root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stageRoot) })

	rels := []string{
		"config.json",
		"tokenizer.json",
		"null_voice_emb.npy",
		filepath.ToSlash(filepath.Join("onnx", "text_encoder.onnx")),
		filepath.ToSlash(filepath.Join("onnx", "prefix_step.onnx")),
		filepath.ToSlash(filepath.Join("onnx", "local_frame_decode.onnx")),
		filepath.ToSlash(filepath.Join("voices", "index.json")),
		filepath.ToSlash(filepath.Join("voices", "quangminh", "voice.npz")),
		filepath.ToSlash(filepath.Join("voices", "maichi", "voice.npz")),
	}
	codecRoot := filepath.Join(sourceRoot, "onnx", "codec")
	if err := filepath.WalkDir(codecRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatalf("enumerate ZeroTTS codec assets: %v", err)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		sourcePath := filepath.Join(sourceRoot, filepath.FromSlash(rel))
		resolved, err := filepath.EvalSymlinks(sourcePath)
		if err != nil {
			t.Fatalf("resolve pinned ZeroTTS asset %s: %v", rel, err)
		}
		destPath := filepath.Join(stageRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			t.Fatalf("create staging dir for %s: %v", rel, err)
		}
		if err := os.Link(resolved, destPath); err != nil {
			in, openErr := os.Open(resolved)
			if openErr != nil {
				t.Fatalf("open pinned ZeroTTS asset %s: %v", rel, openErr)
			}
			out, createErr := os.Create(destPath)
			if createErr != nil {
				_ = in.Close()
				t.Fatalf("create staged ZeroTTS asset %s: %v", rel, createErr)
			}
			_, copyErr := io.Copy(out, in)
			closeOutErr := out.Close()
			closeInErr := in.Close()
			if copyErr != nil || closeOutErr != nil || closeInErr != nil {
				t.Fatalf("copy staged ZeroTTS asset %s: copy=%v close_out=%v close_in=%v", rel, copyErr, closeOutErr, closeInErr)
			}
		}
	}
	return stageRoot
}

func registerZeroTTSSnapshot(t *testing.T, snapshotRoot string) *governance.SnapshotService {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(filepath.Join(t.TempDir(), "zerotts-seam2.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	var rels []string
	if err := filepath.WalkDir(snapshotRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(snapshotRoot, path)
		if err != nil {
			return err
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatalf("walk staged ZeroTTS snapshot: %v", err)
	}
	sort.Strings(rels)
	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       provider.ZeroTTSModelID,
		ModelVersion:  provider.ZeroTTSModelVersion,
	}
	for _, rel := range rels {
		full := filepath.Join(snapshotRoot, filepath.FromSlash(rel))
		f, err := os.Open(full)
		if err != nil {
			t.Fatalf("open staged asset %s: %v", rel, err)
		}
		h := sha256.New()
		size, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("hash staged asset %s: copy=%v close=%v", rel, copyErr, closeErr)
		}
		manifest.Files = append(manifest.Files, domain.SnapshotFileEntry{
			RelativePath: rel,
			SHA256:       hex.EncodeToString(h.Sum(nil)),
			SizeBytes:    size,
		})
	}
	manifestSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		t.Fatalf("ComputeSnapshotManifestSHA256: %v", err)
	}
	manifest.SnapshotManifestSHA256 = manifestSHA
	if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: provider.ZeroTTSModelID,
		Version:        provider.ZeroTTSModelVersion,
		SHA256:         manifestSHA,
		CodeLicense:    "MIT",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "Unknown",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	if _, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, snapshotRoot); err != nil {
		t.Fatalf("RegisterAndVerifySnapshot: %v", err)
	}
	return snapSvc
}

func newZeroTTSSeam2Provider(t *testing.T, snapSvc *governance.SnapshotService, leaseMgr *worker.GPULeaseManager) *provider.WorkerTTSProvider {
	t.Helper()
	p, err := provider.NewWorkerTTSProvider(provider.ZeroTTSProviderID, provider.ZeroTTSModelID, provider.ZeroTTSModelVersion, []string{"vi"}, 0.95)
	if err != nil {
		t.Fatalf("NewWorkerTTSProvider ZeroTTS: %v", err)
	}
	p.SetSnapshotService(snapSvc)
	p.SetLeaseManager(leaseMgr)
	p.SetCPUOnly(true)
	return p
}

func TestSeam2_ZeroTTSRealPinnedRuntime(t *testing.T) {
	pythonBin, sourceSnapshot := resolveRealZeroTTSFixture(t)
	stagedSnapshot := stageZeroTTSSnapshotForGovernance(t, sourceSnapshot)
	snapSvc := registerZeroTTSSnapshot(t, stagedSnapshot)
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", buildStageWorker(t))
	t.Setenv("DOUYINIE_TTS_PYTHON_BIN", pythonBin)
	// The ZeroTTS lane must ignore the generic wrapper overrides and use the
	// repo-owned adapter with the configured Python runtime pack.
	t.Setenv("DOUYINIE_TTS_BIN", "must-not-be-used-by-zerotts")
	t.Setenv("DOUYINIE_TTS_ADAPTER", filepath.Join(t.TempDir(), "must-not-be-used.py"))
	t.Setenv("HF_HUB_OFFLINE", "1")
	t.Setenv("TRANSFORMERS_OFFLINE", "1")
	t.Setenv("HF_DATASETS_OFFLINE", "1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")

	ctx := context.Background()
	sched := scheduler.New()
	leaseMgr := worker.NewGPULeaseManager(sched)
	heldLease, err := sched.Acquire(ctx, "asr")
	if err != nil {
		t.Fatalf("acquire competing ASR lease: %v", err)
	}
	defer func() { _ = sched.Release(ctx, heldLease) }()
	p := newZeroTTSSeam2Provider(t, snapSvc, leaseMgr)

	for _, voiceID := range []string{"quangminh", "maichi"} {
		voiceID := voiceID
		t.Run("real_"+voiceID, func(t *testing.T) {
			req := provider.TTSSynthesisRequest{
				RunID:          "run-zerotts-real-" + voiceID,
				AssetID:        "asset-zerotts-real-" + voiceID,
				SegmentIndex:   0,
				SpeakerID:      "speaker-1",
				Text:           "Xin chào Việt Nam.",
				Language:       "vi",
				Voice:          domain.VoiceProfile{ProviderID: provider.ZeroTTSProviderID, VoiceID: voiceID, Language: "vi"},
				Speed:          1.0,
				SlotDurationMs: 4000,
				UsableSlotMs:   4000,
				AttemptNumber:  1,
			}
			callCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
			defer cancel()
			res, err := p.SynthesizeSpeech(callCtx, req)
			if err != nil {
				t.Fatalf("real ZeroTTS %s synthesis: %v", voiceID, err)
			}
			if holder := sched.Holder(); holder != "asr" {
				t.Fatalf("ZeroTTS CPU-only path touched authoritative GPU lease: holder=%q", holder)
			}
			if res.ProviderID != provider.ZeroTTSProviderID || res.ModelName != provider.ZeroTTSModelID || res.ModelVersion != provider.ZeroTTSModelVersion {
				t.Fatalf("untruthful ZeroTTS provenance: provider=%q model=%q version=%q", res.ProviderID, res.ModelName, res.ModelVersion)
			}
			if res.SampleRate != 48000 || res.Channels != 1 || res.Format != "wav" {
				t.Fatalf("unexpected probed ZeroTTS media metadata: %d Hz, %d ch, %s", res.SampleRate, res.Channels, res.Format)
			}
			probedMs, err := media.ProbeWAVBytes(res.AudioData)
			if err != nil {
				t.Fatalf("ProbeWAVBytes ZeroTTS %s: %v", voiceID, err)
			}
			if probedMs <= 0 || res.MeasuredDurationMs != probedMs {
				t.Fatalf("ZeroTTS duration truth mismatch: result=%d probe=%d", res.MeasuredDurationMs, probedMs)
			}
			sum := sha256.Sum256(res.AudioData)
			if got := hex.EncodeToString(sum[:]); got != res.AudioSHA256 {
				t.Fatalf("ZeroTTS audio sha mismatch: result=%s actual=%s", res.AudioSHA256, got)
			}
		})
	}

	binding, err := snapSvc.GetBinding(provider.ZeroTTSModelID, provider.ZeroTTSModelVersion)
	if err != nil || binding == nil || binding.RuntimeIdentity == nil {
		t.Fatalf("ZeroTTS runtime identity missing after real probe: binding=%+v err=%v", binding, err)
	}
	if rt := binding.RuntimeIdentity; rt.SourceRevision != domain.PinnedZeroTTSSourceRevision ||
		rt.RuntimeVersions["zerotts"] != domain.PinnedZeroTTSPackageVersion ||
		rt.AdapterRevision != domain.PinnedZeroTTSAdapterRevision {
		t.Fatalf("ZeroTTS runtime identity not observed at exact pins: %+v", rt)
	}

	t.Run("unsupported_speed_fails_before_synthesis", func(t *testing.T) {
		_, err := p.SynthesizeSpeech(ctx, provider.TTSSynthesisRequest{
			AssetID: "asset-speed", Text: "Xin chào", Language: "vi",
			Voice: domain.VoiceProfile{VoiceID: "quangminh"}, Speed: 1.1,
		})
		if !errors.Is(err, domain.ErrTTSSpeedUnsupported) {
			t.Fatalf("expected ErrTTSSpeedUnsupported, got %v", err)
		}
		if holder := sched.Holder(); holder != "asr" {
			t.Fatalf("unsupported speed touched GPU lease: %q", holder)
		}
	})

	t.Run("missing_verified_voice_asset_fails_closed", func(t *testing.T) {
		_, err := p.SynthesizeSpeech(ctx, provider.TTSSynthesisRequest{
			AssetID: "asset-missing-voice", Text: "Xin chào", Language: "vi",
			Voice: domain.VoiceProfile{VoiceID: "giahuy"}, Speed: 1.0,
		})
		if !errors.Is(err, domain.ErrTTSVoiceAssetMissing) {
			t.Fatalf("expected missing ZeroTTS voice asset failure, got %v", err)
		}
	})

	t.Run("tampered_snapshot_fails_before_synthesis", func(t *testing.T) {
		target := filepath.Join(stagedSnapshot, "voices", "maichi", "voice.npz")
		original, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read voice before tamper: %v", err)
		}
		// The staged fixture is hardlinked to the cache for speed. Unlink it first so
		// the tamper mutation cannot modify the original Hugging Face blob.
		if err := os.Remove(target); err != nil {
			t.Fatalf("unlink staged voice before tamper: %v", err)
		}
		if err := os.WriteFile(target, append(original, []byte("tamper")...), 0644); err != nil {
			t.Fatalf("write tampered staged voice: %v", err)
		}
		_, err = p.SynthesizeSpeech(ctx, provider.TTSSynthesisRequest{
			AssetID: "asset-tamper", Text: "Xin chào", Language: "vi",
			Voice: domain.VoiceProfile{VoiceID: "quangminh"}, Speed: 1.0,
		})
		if !errors.Is(err, domain.ErrSnapshotMutatedRehashRequired) {
			t.Fatalf("expected tampered ZeroTTS snapshot to fail closed, got %v", err)
		}
	})
}

func TestSeam2_ZeroTTSAbsentSnapshotAndMissingRuntimeFailClosed(t *testing.T) {
	pythonBin, sourceSnapshot := resolveRealZeroTTSFixture(t)
	t.Setenv("DOUYINIE_STAGEWORKER_BIN", buildStageWorker(t))

	t.Run("absent_snapshot", func(t *testing.T) {
		db, err := storage.Open(filepath.Join(t.TempDir(), "empty.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		snapSvc := governance.NewSnapshotService(db, governance.NewLicenseService(db))
		p := newZeroTTSSeam2Provider(t, snapSvc, worker.NewGPULeaseManager(scheduler.New()))
		_, err = p.SynthesizeSpeech(context.Background(), provider.TTSSynthesisRequest{
			AssetID: "asset-no-snapshot", Text: "Xin chào", Language: "vi",
			Voice: domain.VoiceProfile{VoiceID: "quangminh"}, Speed: 1.0,
		})
		if !errors.Is(err, domain.ErrSnapshotUnverified) {
			t.Fatalf("expected absent ZeroTTS snapshot to fail closed, got %v", err)
		}
	})

	t.Run("missing_runtime_dependency", func(t *testing.T) {
		staged := stageZeroTTSSnapshotForGovernance(t, sourceSnapshot)
		snapSvc := registerZeroTTSSnapshot(t, staged)
		emptyVenv := filepath.Join(t.TempDir(), "empty-venv")
		cmd := exec.Command(pythonBin, "-m", "venv", emptyVenv)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create empty Python venv: %v\n%s", err, out)
		}
		emptyPython := filepath.Join(emptyVenv, "Scripts", "python.exe")
		if runtime.GOOS != "windows" {
			emptyPython = filepath.Join(emptyVenv, "bin", "python")
		}
		t.Setenv("DOUYINIE_TTS_PYTHON_BIN", emptyPython)
		p := newZeroTTSSeam2Provider(t, snapSvc, worker.NewGPULeaseManager(scheduler.New()))
		_, err := p.SynthesizeSpeech(context.Background(), provider.TTSSynthesisRequest{
			AssetID: "asset-no-runtime", Text: "Xin chào", Language: "vi",
			Voice: domain.VoiceProfile{VoiceID: "quangminh"}, Speed: 1.0,
		})
		if err == nil || !strings.Contains(err.Error(), "runtime probe") {
			t.Fatalf("expected missing ZeroTTS runtime dependency to fail closed, got %v", err)
		}
	})
}

func runZeroTTSStageWorkerCommand(t *testing.T, pythonBin, snapshotRoot, voiceID, entrypoint string) error {
	t.Helper()
	t.Setenv("DOUYINIE_TTS_PYTHON_BIN", pythonBin)
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatalf("spawn StageWorker: %v", err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 10*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatalf("StageWorker handshake: %v", err)
	}
	defer func() { _ = client.Shutdown() }()
	outPath := filepath.Join(t.TempDir(), "zerotts-negative.json")
	envelope := worker.ModelSnapshotEnvelope{
		Primary: worker.ModelSnapshotRef{
			Role:                   "primary",
			DependencyName:         provider.ZeroTTSModelID,
			Version:                provider.ZeroTTSModelVersion,
			SnapshotManifestSHA256: strings.Repeat("a", 64),
			LocalPath:              snapshotRoot,
			EntrypointFile:         entrypoint,
		},
		EntrypointFile: entrypoint,
	}
	config := map[string]any{
		"text":                   "Xin chào",
		"language":               "vi",
		"voice_id":               voiceID,
		"speed":                  "1.0",
		"slot_duration_ms":       "4000",
		"model_name":             provider.ZeroTTSModelID,
		"model_version":          provider.ZeroTTSModelVersion,
		"require_model_snapshot": true,
	}
	worker.SetModelSnapshotEnvelope(config, envelope)
	_, err := client.Run(context.Background(), worker.Command{
		ID: "cmd-zerotts-negative", Family: "tts", Stage: "tts", CPUOnly: true,
		AttemptID: "attempt-zerotts-negative", RunID: "run-zerotts-negative",
		OutputPath: outPath, Config: config,
	}, 30*time.Second, 30*time.Second)
	return err
}

func TestSeam2_ZeroTTSInvalidEntrypointAndEmptyAudioFailClosed(t *testing.T) {
	pythonBin, sourceSnapshot := resolveRealZeroTTSFixture(t)
	staged := stageZeroTTSSnapshotForGovernance(t, sourceSnapshot)

	t.Run("invalid_entrypoint", func(t *testing.T) {
		wrong := filepath.Join(staged, "voices", "maichi", "voice.npz")
		err := runZeroTTSStageWorkerCommand(t, pythonBin, staged, "quangminh", wrong)
		if err == nil || !strings.Contains(err.Error(), "TTS_VOICE_ASSET_MISSING") {
			t.Fatalf("expected invalid ZeroTTS entrypoint to fail closed, got %v", err)
		}
	})

	t.Run("empty_audio", func(t *testing.T) {
		fakePkg := t.TempDir()
		fakeSource := `__version__ = "0.1.2"
class ZeroTTS:
    def __init__(self, model_dir=None, providers=None): self.sample_rate = 48000
    def synthesize(self, text, voice=None): return []
`
		if err := os.WriteFile(filepath.Join(fakePkg, "zerotts.py"), []byte(fakeSource), 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PYTHONPATH", fakePkg)
		entry := filepath.Join(staged, "voices", "quangminh", "voice.npz")
		err := runZeroTTSStageWorkerCommand(t, pythonBin, staged, "quangminh", entry)
		if err == nil || !strings.Contains(err.Error(), "TTS_NO_AUDIO") {
			t.Fatalf("expected empty ZeroTTS audio to fail closed, got %v", err)
		}
	})
}

func TestSeam2_ZeroTTSExactModelIdentityFailsClosed(t *testing.T) {
	pythonBin, sourceSnapshot := resolveRealZeroTTSFixture(t)
	staged := stageZeroTTSSnapshotForGovernance(t, sourceSnapshot)
	t.Setenv("DOUYINIE_TTS_PYTHON_BIN", pythonBin)
	exe := buildStageWorker(t)
	sup := worker.NewSupervisor()
	if err := sup.Spawn(context.Background(), "tts", exe, "-family", "tts", "-heartbeat-ms", "1000"); err != nil {
		t.Fatal(err)
	}
	client := worker.NewClient(sup)
	if _, err := client.Handshake(context.Background(), 10*time.Second); err != nil {
		_ = sup.Terminate()
		t.Fatal(err)
	}
	defer func() { _ = client.Shutdown() }()
	entry := filepath.Join(staged, "voices", "quangminh", "voice.npz")
	config := map[string]any{
		"text": "Xin chào", "language": "vi", "voice_id": "quangminh", "speed": "1.0",
		"model_name": "zeroweight-ai/ZeroTTS-fake", "model_version": provider.ZeroTTSModelVersion,
	}
	worker.SetModelSnapshotEnvelope(config, worker.ModelSnapshotEnvelope{
		Primary:        worker.ModelSnapshotRef{Role: "primary", DependencyName: provider.ZeroTTSModelID, Version: provider.ZeroTTSModelVersion, SnapshotManifestSHA256: strings.Repeat("b", 64), LocalPath: staged, EntrypointFile: entry},
		EntrypointFile: entry,
	})
	_, err := client.Run(context.Background(), worker.Command{
		ID: "cmd-zerotts-wrong-model", Family: "tts", Stage: "tts", CPUOnly: true,
		AttemptID: "attempt", RunID: "run", OutputPath: filepath.Join(t.TempDir(), "out.json"), Config: config,
	}, 30*time.Second, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "TTS_MODEL_UNSUPPORTED") {
		t.Fatalf("expected exact ZeroTTS model identity failure, got %v", err)
	}
}
