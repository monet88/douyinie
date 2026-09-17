# Pinned Weights Digests for the TTS Lanes — closing the label-vs-bytes gap

**Date:** 2026-09-18
**Scope:** the ZeroTTS (VI unattended default) and VieNeu (VI compatibility) snapshot lanes gained
pinned weight/file digests, enforced in the snapshot resolver. One seam rule changed: a snapshot
manifest that omits a lane's load-bearing files now fails closed instead of passing unchecked.
**Closes:** the `model weights … gap (follow-up)` row and the label-vs-weights caveat of
`docs/research/tts-runtime-upgrade-2026-09-17.md` §1.
**Evidence computed from:** the provisioned snapshots
`D:\douyinie-ref\zerotts-benchmark\cache\huggingface\hub\models--zeroweight-ai--ZeroTTS\snapshots\c2bfbd67dc648cac455077333f7cf5c18a2e3bb4`
and `D:\douyinie-ref\phase1.1-runtime\staging\vieneu_v3_turbo_5f2a3e93` (SHA-256 per file, read
through the HF cache symlinks).

---

## 1. The gap, stated exactly

`SnapshotService.RegisterAndVerifySnapshot` hashes every **manifest-declared** file and compares it to
the digest the manifest declares (`internal/governance/snapshot.go:196-225`). Nothing compared those
declared digests to a pinned value, so the lanes' revision labels (`c2bfbd67…`, `v3.8.1`) were the
only binding: a snapshot registered under the right label with substituted weights, config, tokenizer
or codec was admitted as long as its own manifest was self-consistent. Verified by grep and by the
overlay red proof in §4.

## 2. What is pinned now

`internal/domain/snapshot.go` gains `PinnedAsset` plus three sets, and
`ResolveTTSVoiceEntrypoint` — the single production call site of voice/asset resolution
(`internal/provider/worker_adapters.go:256`) — fails closed on any of them.

**`PinnedZeroTTSAssets`** (10) — the files the pinned `zerotts` package reads:

| Relative path | Why it is load-bearing (source of truth) |
|---|---|
| `config.json`, `tokenizer.json`, `null_voice_emb.npy` | upstream `zerotts/hub.py` `REQUIRED_FILES` + `synthesizer.py` (loads `null_voice_emb.npy`) |
| `onnx/text_encoder.onnx`, `onnx/prefix_step.onnx`, `onnx/local_frame_decode.onnx` | upstream `REQUIRED_FILES`; the three graphs the synthesizer runs |
| `onnx/codec/moss_audio_tokenizer_decode_full.onnx`, `…_decode_shared.data`, `…_decode_step.onnx` | MOSS codec weights; `decode_full` (whole segment) and `decode_step` (streaming) |
| `onnx/codec/codec_browser_onnx_meta.json` | `zerotts/codec.py:49` reads it for tensor names + the streaming layout; a wrong descriptor mis-drives the codec |

**`PinnedZeroTTSVoiceAssets`** (8) — `voices/<preset>/voice.npz` for every voice in
`FrozenZeroTTSVoiceOrder` (quangminh, maichi, giahuy, baotrang, hamy, huuduc, kimoanh, tiendat): the
voice conditioning tensors the synthesizer concatenates. `voice.bin` is deliberately **not** pinned —
upstream `hub.py` documents it as the browser demo's duplicate of `voice.npz` ("never worth the bytes
here") and no code path in the pinned package reads it.

**`PinnedVieNeuAssets`** (10) — what the pinned SDK (v3.8.1, `VieNeuTTSv3Turbo`, `model_subfolder='update'`) loads:

| Relative path | Why |
|---|---|
| `update/model.safetensors` | the checkpoint (`_v3_turbo_engine/hub_load_v3_turbo.py`); digest `119003a9…` equals the HF LFS object id of revision `5f2a3e93…` |
| `update/config.json`, `update/tokenizer.json`, `update/tokenizer_config.json`, `update/special_tokens_map.json` | the tokenizer/config the engine loads from that subfolder |
| `speaker_encoder.onnx`, `denoiser.onnx` | `inference_v3_turbo.py` `speaker_encoder_filename` / `denoiser_filename`, loaded from the snapshot root |
| `moss_tokenizer/config.json`, `moss_tokenizer/model-00001-of-00001.safetensors` | the fixed in-root MOSS audio tokenizer the adapter passes as `moss_tokenizer=` |
| `src/vieneu/assets/voices_v3_turbo.json` | the preset catalog (speaker embeddings) the resolver requires and the SDK reads |

Kokoro needed nothing: its checkpoint digest was already pinned inside the resolver
(`internal/domain/snapshot.go`, `pinnedKokoroCheckpointSHA`), as were UVR and Demucs.

**What is deliberately not pinned, and why:**

- ZeroTTS `voice.bin`, `voices/*/preview.wav`, `voices/*/meta.json`, `voices/index.json`,
  `samples/*`, `banner.png`, `README.md`, `.gitattributes`, `.gitignore`,
  `onnx/codec/LICENSE-Apache-2.0.txt`: display/audition/packaging files, never read by synthesis.
  `voices/index.json` keeps its pre-existing *presence* requirement only.
- ZeroTTS `silence_frame.npy`: upstream's pack list mentions it for long-form padding, but no code
  path in the pinned `0.1.5` package opens it (grepped `zerotts/*.py`), so pinning it would be
  strictness without enforcement value.
- VieNeu `onnx/`, `onnx_int8/`, `onnx_update/`, `.cache/huggingface/*`, `moss_tokenizer/*.py`,
  `src/vieneu/assets/voices_v3_nano.json`: alternative backends and download bookkeeping this lane
  does not load (`backend` is the default PyTorch path, `precision` is not int8).

## 3. Enforcement rule

For every pinned asset: it must be declared in the snapshot manifest, the declared SHA-256 must equal
the pinned constant, and the file must exist on disk. Undeclared → `ErrSnapshotFileCorrupted`
(voice asset → `ErrTTSVoiceAssetMissing`); digest drift → `ErrSnapshotDigestMismatch`. The
declared→bytes link stays registration's job, so the chain is pinned constant → declared digest →
hashed bytes, exactly the pattern UVR/Demucs already use.

**Operational consequence (documented, intended):** the snapshot manifest must declare the lane's
load-bearing files. A manifest that omits them was never hashing them; it now fails closed at the
first TTS dispatch of that lane instead of passing with unverified weights. The repo's real-snapshot
path already complies — `test/seam2/zerotts_seam2_test.go` stages the provisioned revision and
declares every file it walks (54 of them).

## 4. Verification evidence

| Check | Command | Result |
|---|---|---|
| Full suite, all packages | `go test ./... -count=1` | **exit 0**, 17 packages ok, 179.7 s wall (`test/seam1` 175.3 s, `test/seam2` 102.7 s, `test/steering` 0.038 s) |
| Format / vet / whitespace | `gofmt -l cmd internal test`, `go vet ./...`, `git diff --check` | clean |
| New gate, unit level | `go test ./internal/domain/ -count=1` | ok — substituted ZeroTTS graph / substituted voice tensor / substituted VieNeu weights → `ErrSnapshotDigestMismatch`; undeclared weight → `ErrSnapshotFileCorrupted`; pinned set → resolves |
| New gate, seam 1 | `go test ./test/seam1/ -run TestSeam1_Snapshot_TTS_RegistrationAndVerification` | ok — registration accepts the fixture's bytes (self-consistent manifest), the resolver then **rejects** them as not the pinned weights |
| New gate vs **real** provisioned bytes | `go test ./test/seam2/ -run 'TestSeam2_ZeroTTS\|TestSeam2_WorkerTTSProvider_CPUOnlyResourceContract'` | ok, 33.0 s — `TestSeam2_ZeroTTSRealPinnedRuntime` registers the staged `c2bfbd67…` snapshot (walk-all-files manifest = real SHA-256 per file) and resolves both unattended voices, so the 18 ZeroTTS constants are independently confirmed against the provisioned bytes |
| Red proof (repo unmodified) | `go test -overlay <overlay> ./internal/domain/ -run TestSnapshot_ResolveTTSVoiceEntrypoint` with the two `verifyPinnedAssets` calls and the voice pin removed | **FAIL as expected**: `snapshot_test.go:488 expected ErrSnapshotDigestMismatch for substituted VieNeu weights, got: <nil>` and `:650 expected ErrSnapshotDigestMismatch for substituted ZeroTTS text encoder, got: <nil>`; same overlay turns the seam 1 assertion red at `snapshot_test.go:1034` |

The red proof is the direct evidence that the gap was real: without the pin, a snapshot whose bytes
are not the pinned ones resolves successfully.

## 5. Residual notes / risks

- **A wrong constant is now a hard failure.** Bumping a lane's model revision means re-deriving all
  of its pinned digests from the new provisioned snapshot in the same change; the resolver will
  otherwise reject the new snapshot (fail closed, not silent).
- **`update/model.safetensors` is ~248 MB** and `onnx/text_encoder.onnx` ~323 MB; the pinned checks
  compare declared digests and stat the files, so resolution adds no re-hashing per dispatch. The
  bytes are hashed once, at registration.
- The fixtures that build synthetic TTS snapshots had to either declare the pinned digests or stamp
  them onto the verified binding after registration
  (`internal/domain/snapshot_test.go`, `test/seam1/snapshot_test.go`,
  `test/seam2/tts_cpu_resource_seam2_test.go`); the stamping technique is the one
  `internal/provider/worker_separator_envelope_test.go` already uses for the Demucs checkpoint.
- Cross-language duplication is unchanged in kind: `cmd/stageworker/main.go` and
  `cmd/stageworker/adapters/tts_engine.py` still carry their own required-asset lists (fail closed
  independently of Go). They do not verify weight digests; this change pins the Go resolution seam,
  which every TTS dispatch passes through.
