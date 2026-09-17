package seam2_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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

// mockTTSAdapterSource is a StageWorker TTS adapter stand-in that emits the TTS
// artifact contract from a real WAV container: 24000 Hz stereo, 1000 ms. Both
// properties differ from the provider-agnostic 16000 Hz/mono assumption, and the
// reported measured_duration_ms is deliberately wrong so the probe — not the
// adapter's self-report — has to decide the duration.
const mockTTSAdapterSource = `import base64, hashlib, io, json, os, sys, wave

req = json.loads(sys.stdin.read())

# The adapter deliberately reports a duration that does not match the WAV it
# produces, so the probe must decide the measured duration.
reported_ms = 4242

if os.environ.get("MOCK_TTS_MODE") == "corrupt":
    payload = b"NOT-A-WAV-CONTAINER"
elif os.environ.get("MOCK_TTS_MODE") == "truncated":
    # Keep the RIFF/WAVE/fmt header and its declared data chunk size intact while
    # cutting the audio bytes short: only declaration-vs-availability validation
    # can reject this container.
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(2)
        wf.setsampwidth(2)
        wf.setframerate(24000)
        wf.writeframes(b"\x00" * (24000 * 2 * 2))
    payload = buf.getvalue()[:-4096]
else:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(2)
        wf.setsampwidth(2)
        wf.setframerate(24000)
        wf.writeframes(b"\x00" * (24000 * 2 * 2))
    payload = buf.getvalue()

out = {
    "audio_data": base64.b64encode(payload).decode("ascii"),
    "audio_sha256": hashlib.sha256(payload).hexdigest(),
    "measured_duration_ms": reported_ms,
    "predicted_duration_ms": reported_ms,
    "model_name": req.get("model_name", ""),
    "model_version": req.get("model_version", ""),
}
print(json.dumps(out))
`

// TestSeam2_WorkerTTSProvider_CPUOnlyResourceContract proves, over Seam 2 and the
// real StageWorker + workerBridge stack, that (1) a CPU-only TTS resource contract
// runs without acquiring the authoritative GPU lease while the default provider
// contract still cannot bypass a held lease, and (2) TTS results report media
// properties probed from the produced WAV artifact instead of generic constants,
// failing closed on corrupt or truncated media.
func TestSeam2_WorkerTTSProvider_CPUOnlyResourceContract(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	t.Setenv("DOUYINIE_STAGEWORKER_BIN", buildStageWorker(t))

	adapterPath := filepath.Join(tmpDir, "mock_tts_adapter.py")
	if err := os.WriteFile(adapterPath, []byte(mockTTSAdapterSource), 0644); err != nil {
		t.Fatalf("write mock tts adapter: %v", err)
	}
	t.Setenv("DOUYINIE_TTS_ADAPTER", adapterPath)

	snapSvc := registerVieNeuTTSSnapshot(t, tmpDir)

	req := provider.TTSSynthesisRequest{
		RunID:          "run-tts-cpu-contract",
		AssetID:        "asset-tts-cpu-contract",
		SegmentIndex:   0,
		SpeakerID:      "speaker-1",
		Text:           "Xin chào Việt Nam",
		Language:       "vi",
		Voice:          domain.VoiceProfile{VoiceID: "Trúc Ly", Language: "vi"},
		Speed:          1.0,
		SlotDurationMs: 1500,
		UsableSlotMs:   1500,
		AttemptNumber:  1,
	}

	t.Run("cpu_only_bypasses_held_gpu_lease_and_probes_truthful_metadata", func(t *testing.T) {
		sched := scheduler.New()
		leaseMgr := worker.NewGPULeaseManager(sched)

		// Another accelerator-backed family holds the single authoritative lease.
		heldLease, err := sched.Acquire(ctx, "asr")
		if err != nil {
			t.Fatalf("acquire competing lease: %v", err)
		}
		defer func() { _ = sched.Release(ctx, heldLease) }()

		res, err := newSeam2TTSProvider(t, snapSvc, leaseMgr, true).SynthesizeSpeech(ctx, req)
		if err != nil {
			t.Fatalf("CPU-only TTS must not require the GPU lease, got: %v", err)
		}
		if holder := sched.Holder(); holder != "asr" {
			t.Fatalf("CPU-only TTS must not touch the GPU lease, holder changed to %q", holder)
		}

		if res.SampleRate != 24000 {
			t.Errorf("expected probed sample rate 24000 Hz, got %d", res.SampleRate)
		}
		if res.Channels != 2 {
			t.Errorf("expected probed channel count 2, got %d", res.Channels)
		}
		if res.Format != "wav" {
			t.Errorf("expected probed container format %q, got %q", "wav", res.Format)
		}

		probedMs, err := media.ProbeWAVBytes(res.AudioData)
		if err != nil {
			t.Fatalf("probe returned media: %v", err)
		}
		if probedMs != 1000 {
			t.Fatalf("expected 1000 ms WAV fixture, got %d", probedMs)
		}
		if res.MeasuredDurationMs != probedMs {
			t.Errorf("probed duration must be authoritative: adapter reported 4242 ms, result says %d ms, probe says %d ms",
				res.MeasuredDurationMs, probedMs)
		}
	})

	t.Run("default_gpu_contract_still_cannot_bypass_held_lease", func(t *testing.T) {
		sched := scheduler.New()
		leaseMgr := worker.NewGPULeaseManager(sched)

		heldLease, err := sched.Acquire(ctx, "asr")
		if err != nil {
			t.Fatalf("acquire competing lease: %v", err)
		}
		defer func() { _ = sched.Release(ctx, heldLease) }()

		_, err = newSeam2TTSProvider(t, snapSvc, leaseMgr, false).SynthesizeSpeech(ctx, req)
		if err == nil {
			t.Fatal("accelerator-backed TTS must fail closed while the GPU lease is held by another family")
		}
		if !strings.Contains(err.Error(), "gpu lease") {
			t.Fatalf("expected a GPU lease error, got %v", err)
		}
		if holder := sched.Holder(); holder != "asr" {
			t.Fatalf("failed TTS invocation must leave the competing lease intact, holder is %q", holder)
		}
	})

	t.Run("corrupt_media_fails_closed", func(t *testing.T) {
		t.Setenv("MOCK_TTS_MODE", "corrupt")

		sched := scheduler.New()
		leaseMgr := worker.NewGPULeaseManager(sched)

		_, err := newSeam2TTSProvider(t, snapSvc, leaseMgr, true).SynthesizeSpeech(ctx, req)
		if err == nil {
			t.Fatal("invalid/corrupt synthesized media must fail closed")
		}
		if !strings.Contains(err.Error(), "not a valid WAV artifact") {
			t.Fatalf("expected a WAV artifact rejection, got %v", err)
		}
		if holder := sched.Holder(); holder != "" {
			t.Fatalf("CPU-only failing invocation must not acquire the GPU lease, holder is %q", holder)
		}
	})

	t.Run("truncated_media_fails_closed", func(t *testing.T) {
		t.Setenv("MOCK_TTS_MODE", "truncated")

		sched := scheduler.New()
		leaseMgr := worker.NewGPULeaseManager(sched)

		_, err := newSeam2TTSProvider(t, snapSvc, leaseMgr, true).SynthesizeSpeech(ctx, req)
		if err == nil {
			t.Fatal("a header-valid WAV that declares more data bytes than it carries must fail closed")
		}
		if !strings.Contains(err.Error(), "not a valid WAV artifact") {
			t.Fatalf("expected a WAV artifact rejection for truncated media, got %v", err)
		}
		if holder := sched.Holder(); holder != "" {
			t.Fatalf("CPU-only failing invocation must not acquire the GPU lease, holder is %q", holder)
		}
	})
}

// newSeam2TTSProvider builds the production worker-backed TTS adapter wired to the
// verified snapshot service and the given GPU lease manager.
func newSeam2TTSProvider(t *testing.T, snapSvc *governance.SnapshotService, leaseMgr *worker.GPULeaseManager, cpuOnly bool) *provider.WorkerTTSProvider {
	t.Helper()
	p, err := provider.NewWorkerTTSProvider(provider.VieNeuProviderID, provider.VieNeuModelID, provider.VieNeuModelVersion, []string{"vi"}, 0.95)
	if err != nil {
		t.Fatalf("NewWorkerTTSProvider: %v", err)
	}
	p.SetSnapshotService(snapSvc)
	p.SetLeaseManager(leaseMgr)
	p.SetCPUOnly(cpuOnly)
	return p
}

// registerVieNeuTTSSnapshot provisions a license-registered, digest-verified VieNeu
// snapshot root (catalog + tokenizer layout) so the TTS bridge performs its real
// snapshot resolution, envelope construction and fingerprint validation.
func registerVieNeuTTSSnapshot(t *testing.T, tmpDir string) *governance.SnapshotService {
	t.Helper()
	ctx := context.Background()

	db, err := storage.Open(filepath.Join(tmpDir, "tts-cpu-resource.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	licSvc := governance.NewLicenseService(db)
	snapSvc := governance.NewSnapshotService(db, licSvc)

	snapRoot := filepath.Join(tmpDir, "vieneu-snapshot")
	catalogRel := "src/vieneu/assets/voices_v3_turbo.json"
	if err := os.MkdirAll(filepath.Join(snapRoot, "moss_tokenizer"), 0755); err != nil {
		t.Fatalf("create snapshot dirs: %v", err)
	}
	files := map[string]string{
		catalogRel:                      `{"presets": {"Trúc Ly": {"id": "Trúc Ly", "speaker_emb": [0.1], "codes": [1]}}}`,
		"moss_tokenizer/tokenizer.json": `{"model": {"type": "BPE"}}`,
		"model.safetensors":             "pinned vieneu weights placeholder",
	}
	// The lane's load-bearing files must be present and declared; their digests are stamped onto the
	// verified binding below (registration hashes the fixture bytes, the resolver pins the real ones).
	for _, asset := range domain.PinnedVieNeuAssets {
		if _, ok := files[asset.RelativePath]; !ok {
			files[asset.RelativePath] = "pinned fixture placeholder: " + asset.RelativePath
		}
	}
	manifest := domain.SnapshotManifest{
		SchemaVersion: "1.0",
		ModelID:       provider.VieNeuModelID,
		ModelVersion:  provider.VieNeuModelVersion,
	}
	for rel, content := range files {
		full := filepath.Join(snapRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("create %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		sum := sha256.Sum256([]byte(content))
		manifest.Files = append(manifest.Files, domain.SnapshotFileEntry{
			RelativePath: rel,
			SHA256:       hex.EncodeToString(sum[:]),
			SizeBytes:    int64(len(content)),
		})
	}
	manifestSHA, err := domain.ComputeSnapshotManifestSHA256(&manifest)
	if err != nil {
		t.Fatalf("ComputeSnapshotManifestSHA256: %v", err)
	}
	manifest.SnapshotManifestSHA256 = manifestSHA
	if err := licSvc.RegisterManifest(ctx, domain.LicenseManifestEntry{
		ID:             uuid.NewString(),
		DependencyName: provider.VieNeuModelID,
		Version:        provider.VieNeuModelVersion,
		SHA256:         manifestSHA,
		CodeLicense:    "Apache-2.0",
		ModelLicense:   "Apache-2.0",
		DataLicense:    "CC-BY-4.0",
		ServiceTerms:   "Local-Offline",
		Verified:       true,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	if _, err := snapSvc.RegisterAndVerifySnapshot(ctx, manifest, snapRoot); err != nil {
		t.Fatalf("RegisterAndVerifySnapshot: %v", err)
	}

	// The resolver pins the lane's load-bearing weight digests. Registration above is what proves the
	// declared-digest -> on-disk-bytes link; stamping the pinned digests onto the verified binding's
	// manifest exercises resolution the way a provisioned snapshot reaches it (same technique as
	// worker_separator_envelope_test.go for the Demucs checkpoint).
	binding, err := snapSvc.GetBinding(provider.VieNeuModelID, provider.VieNeuModelVersion)
	if err != nil {
		t.Fatalf("GetBinding: %v", err)
	}
	for i := range binding.Manifest.Files {
		for _, asset := range domain.PinnedVieNeuAssets {
			if binding.Manifest.Files[i].RelativePath == asset.RelativePath {
				binding.Manifest.Files[i].SHA256 = asset.SHA256
			}
		}
	}
	return snapSvc
}
