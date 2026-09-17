# TTS Runtime Upgrade — ZeroTTS v0.1.2 → v0.1.5 and VieNeu-TTS v3.2.9 → v3.8.1

**Date:** 2026-09-17
**Scope:** runtime/model pin upgrade of the two Vietnamese TTS lanes (ZeroTTS unattended
default, VieNeu compatibility/history). No seam was added, no routing policy changed, no
frozen voice catalog changed.
**Upstream sources of truth:** `zeroweight-ai/ZeroTTS` release notes + source at tag `v0.1.5`;
`pnnbao97/VieNeu-TTS` git history at tag `v3.8.1`; Hugging Face model API trees for both
model repos.

---

## 1. New pinned identities

| Lane | Identity | Old | New |
|---|---|---|---|
| ZeroTTS | package (distribution metadata) | `0.1.2` | `0.1.5` |
| ZeroTTS | source commit | `9d85578bee9321d6ef8305a4d454baf33e3fe861` | `47e466d7a1a36517cfd240de536523d17c00adac` (tag `v0.1.5`) |
| ZeroTTS | model revision (`zeroweight-ai/ZeroTTS`) | `8a0c3c29f6f047011f5cae02d0b14475a690be86` | `c2bfbd67dc648cac455077333f7cf5c18a2e3bb4` |
| ZeroTTS | adapter revision label | `tts_engine.py@zerotts-0.1.2` | `tts_engine.py@zerotts-0.1.5` |
| VieNeu | SDK tag / commit | `v3.2.9` / `149ff16a6a50093a0cad1b75d5edf9e9d81d97f4` | `v3.8.1` / `592ba27c8f932b80768f6cee405badaef32bdb17` |
| VieNeu | model revision (`pnnbao-ump/VieNeu-TTS-v3-Turbo`) | `1278db0090b98ccf23e56f2423857fc9d32a5118` | `5f2a3e93092efaba9153253ff5f2e6a8e810e4f2` |

Both lanes keep their existing runtime contract: verified local snapshot only, `HF_HUB_OFFLINE=1`,
no Hub download at request time, exact-identity fail-closed checks.

This table supersedes the VieNeu pin block in
`docs/research/issue62-production-adapter-compatibility-2026-09-03.md` §2.1 (`v3.2.9` /
`1278db00…`).

**Enforcement map** (what actually fails closed at runtime, as of this upgrade):

| Value | Enforced by |
|---|---|
| `ZeroTTSModelVersion`, `VieNeuModelVersion`, `KokoroModelVersion` | exact snapshot-binding version equality (`internal/provider/worker_adapters.go`), exact voice/catalog/checkpoint resolution (`internal/domain/snapshot.go`), `cmd/stageworker/main.go` |
| `ZEROTTS_PACKAGE_VERSION` + `ZEROTTS_MODULE_VERSION_LITERAL` | the adapter's dual runtime-identity gate, `cmd/stageworker/adapters/tts_engine.py` (`TTS_RUNTIME_VERSION_MISMATCH`) |
| `ZEROTTS_SOURCE_COMMIT` | PEP 610 `direct_url.json` commit check in `probe_zerotts_runtime_identity()` |
| `KokoroModelCommit`, `KokoroSourceCommit` | nothing at runtime — provenance records only. |

The VieNeu SDK commit and model revision are likewise records, not checks, so no constant carries
them: they live in the §1 table above. A VieNeu snapshot registered under the `v3.8.1` label with
other weights is **not** rejected (registration accepts the operator-supplied version label plus file
hashes). Enforcing the weights digest is a follow-up, not part of this upgrade.

---

## 2. Upstream deltas actually observed

### 2.1 ZeroTTS `v0.1.2` → `v0.1.5`

- Releases: `v0.1.2` (09-06) → `v0.1.3-release` → `v0.1.4` "Bring your own voice" (09-14) →
  `v0.1.5` "Always point to latest commit in Huggingface" (09-15). PyPI publishes
  `0.1.0 0.1.1 0.1.2 0.1.4 0.1.5` — `0.1.3` was tagged but never published.
- **No API change on our path.** `ZeroTTS.__init__(model_dir, providers=None, intra_op_num_threads=4,
  codec_intra_op_num_threads=None, warmup=True)` and `synthesize(text, voice=None, ...) -> np.ndarray`
  are source-identical in shape to `0.1.2`; preset voices are still resolved from
  `<model_dir>/voices/<name>/voice.npz`. `add_voices()` / `ZEROTTS_VOICES_HOME` are additive and
  optional (`installed_voices()` returns `{}` when `~/.zerotts/voices` is absent).
- **Snapshot layout unchanged.** The loader still requires `config.json`, `tokenizer.json`,
  `null_voice_emb.npy`, `onnx/{text_encoder,prefix_step,local_frame_decode}.onnx`,
  `onnx/codec/*`, `voices/index.json`, `voices/<name>/voice.npz` — byte-for-byte the set the repo
  already validates in `internal/domain/snapshot.go` and `cmd/stageworker/main.go`.
- **Dependency set unchanged, resolved pack pins moved** (`pyproject.toml` dependency block is
  identical between `9d85578` and `47e466d7`: `numpy`, `onnxruntime`, `tokenizers`,
  `huggingface_hub`, `soundfile`, `scipy`, `sounddevice` — still torch-free). Re-resolving the pack
  did move two transitive pins: `filelock` `3.32.6` → `4.0.0` and `huggingface-hub` `1.31.0` →
  `1.32.0`.
- `DEFAULT_REVISION` changed from the pinned commit to `"main"`. Irrelevant here: it is only used
  when the caller passes a *remote repo id*; `resolve_model_dir()` returns early for an existing
  local directory, which is the only path this repo uses.
- **Model revision delta (measured, not inferred):** hashing all 54 files of both HF revisions
  shows exactly four differing files — `voices/maichi/{voice.npz,voice.bin,meta.json,preview.wav}`.
  Every ONNX graph, `config.json`, `tokenizer.json`, `null_voice_emb.npy`, the codec assets and the
  other seven presets are byte-identical. `maichi` is the second voice of the unattended VI
  rotation, so this is a real (small) quality change, not a metadata-only bump.
- **Upstream packaging defect, handled fail-closed:** `src/zerotts/__init__.py` still hardcodes
  `__version__ = "0.1.2"` at the `v0.1.5` tag (it was never bumped for `0.1.4`/`0.1.5`), while
  `pyproject.toml` and the distribution metadata say `0.1.5`. The adapter used to compare that
  module literal against the package version, which would have failed closed for every request after
  the upgrade. It now checks **both** signals exactly: distribution metadata == `0.1.5`
  (`ZEROTTS_PACKAGE_VERSION`) and module literal == `0.1.2` (`ZEROTTS_MODULE_VERSION_LITERAL`).

### 2.2 VieNeu-TTS `v3.2.9` → `v3.8.1`

- Intermediate tags: `v3.3.0` (connector-aware chunking), `v3.4.0` (**ONNX/CPU default precision
  `int8` → `fp32`**, i.e. subfolder `onnx_int8` → `onnx_update`), `v3.5.x` (`v3nano` mode +
  zero-shot cloning for it), `v3.6.x` (LoRA toolkit, model-shipped preset catalog), `v3.7.x`
  (fused CUDA graphs for the GPU backend), `v3.8.0` (**preset catalog 20 → 25 voices, `featured`
  top-10 flag, voice aliases, `Trúc Ly` reference refreshed**), `v3.8.1` (release bump).
- **Our effective backend is unchanged and stays the PyTorch/GPU lane.** `Vieneu(mode="v3turbo", …)`
  selects PyTorch when `backend="auto"` and CUDA is visible; the production adapter passes neither
  `device` nor `backend`, so with `torch 2.11.0+cu128` present it loads `update/model.safetensors`
  and the snapshot-local `moss_tokenizer/`. This is why the snapshot-local MOSS tokenizer remains a
  hard requirement — it is the PyTorch-tokenizer path, not the ONNX one (the torch-free ONNX engine
  reads `tokenizer.json` from `onnx_update/` and would additionally need a local codec dir).
- `infer(text=..., voice=...)` and `get_preset_voice()` are unchanged; output stays 48 kHz
  float32. The new `apply_watermark=True` default is a no-op here: `_init_watermarker()` swallows
  `ImportError` and `_apply_watermark()` returns the raw waveform when the watermarker is `None`.
- **Model revision delta (measured via HF LFS oids):** `update/model.safetensors` and
  `onnx_int8/vieneu_backbone_shared.data` changed content (same byte size, different SHA-256);
  `onnx_update/*`, `tokenizer.json`, `speaker_encoder.onnx`, `denoiser.onnx` and `update/tokenizer*`
  are unchanged, and `README.md` changed. The changed `update/` weights are exactly the subfolder
  this lane loads, so the revision bump is substantive — re-provisioning was required, not optional.
- Voice catalog: 25 presets; all four frozen voices (`Trúc Ly`, `Phạm Tuyên`, `Đoan Trang`,
  `Xuân Vĩnh`) remain present and the frozen order is untouched. `Trúc Ly`'s reference was refreshed
  upstream, so its synthesized audio differs from `v3.2.9` output at equal text.
- **Which model is "new" as of this upgrade** (SDK `config.yaml` at tag `v3.8.1`): the backbone list
  marks `pnnbao-ump/VieNeu-TTS-v3-Turbo` as *"VieNeu-TTS-v3-Turbo (new release)"* — 48 kHz, ONNX on
  CPU / PyTorch on GPU, still flagged early access with the full v3 promised "in a few weeks". That is
  exactly the repo this lane pins, at its newest revision (`5f2a3e93…`, HF `lastModified`
  2026-09-16). The same list offers `pnnbao-ump/VieNeu-TTS-v3-Nano` (preview, 24 kHz, 6 presets, no
  cloning), which is **out of scope** for this lane's 48 kHz contract, and the older v2/v1 repos.
  The newest SDK release tag remains `v3.8.1` (other recent tags are desktop-app builds).

---

## 3. Provisioning performed (out-of-git, machine `D:\`)

| Artifact | Path | Provenance check |
|---|---|---|
| ZeroTTS runtime pack `0.1.5` | `D:\douyinie-ref\phase1.1-runtime\venvs\tts-zerotts-0.1.5-47e466d` | fresh `uv venv --python 3.11` + `pip install` of the VCS pin; PEP 610 `direct_url.json` → `commit_id=47e466d7a1a36517cfd240de536523d17c00adac`, dist version `0.1.5` |
| ZeroTTS model snapshot | `…\zerotts-benchmark\cache\huggingface\hub\models--zeroweight-ai--ZeroTTS\snapshots\c2bfbd67dc648cac455077333f7cf5c18a2e3bb4` | `hf download --revision c2bfbd67…` into the existing HF cache root |
| VieNeu SDK | `D:\douyinie-ref\VieNeu-TTS` checked out at `v3.8.1` (editable install into `venvs\tts`) | `git log -1` = `592ba27 release: 3.8.1`; `importlib.metadata.version("vieneu")` = `3.8.1` |
| VieNeu snapshot | `D:\douyinie-ref\phase1.1-runtime\staging\vieneu_v3_turbo_5f2a3e93` | `hf download --revision 5f2a3e93…`; `update/model.safetensors` SHA-256 `119003a9…` equals the HF LFS oid of that revision; catalog copied from the `v3.8.1` SDK; `moss_tokenizer/` carried over from the previous pack (unchanged) |

The reproducible pip set for the ZeroTTS pack is committed as
`cmd/stageworker/adapters/requirements-zerotts-0.1.5.txt` (supersedes the `0.1.2` file).

---

## 4. Repo changes

- `internal/domain/snapshot.go` — `PinnedZeroTTS*` identity constants.
- `cmd/stageworker/adapters/tts_engine.py` — `ZEROTTS_*` constants plus the VieNeu frozen
  `VIENEU_MODEL_ID` / allowed-voices pair; the ZeroTTS runtime identity check now pins the
  distribution version and the stale upstream module literal separately.
- `cmd/stageworker/adapters/requirements-zerotts-0.1.5.txt` — new runtime pack manifest (old file
  removed); the re-resolved pack pins also moved (`filelock`, `huggingface-hub`).
- `cmd/stageworker/adapters/test_tts_engine.py` — four fail-closed tests for the ZeroTTS dual-pin
  identity gate (review remediation).
- Review remediation also deleted the unread `VieNeuModelDigest` / `VIENEU_MODEL_DIGEST`-style
  provenance scalars (`VieNeuModelRevision`, `VieNeuSDKCommit`, `VIENEU_MODEL_VERSION`,
  `VIENEU_MODEL_REVISION`, `VIENEU_SDK_COMMIT`): nothing read them, and the §1 table above is where
  those values are recorded.
- `internal/provider/tts.go`, `internal/provider/worker_adapters.go` — the VieNeu identities the
  runtime enforces (`VieNeuModelID` / `VieNeuModelVersion`) and the pinned-identity doc comment.
- `test/seam2/zerotts_seam2_test.go` — real-snapshot/real-venv fallback paths and a note on the
  pinned module literal.
- `test/seam2/snapshot_seam2_test.go`, `internal/domain/snapshot_test.go` — VieNeu fixture version
  literals moved to `v3.8.1` (they model the pinned snapshot; the provider validates them exactly).
- `docs/architecture/phase1-architecture.md` §"pending pins" — pin record.

Not changed on purpose: the frozen ZeroTTS/Kokoro/VieNeu voice orders, the unattended VI rotation,
routing/escalation policy, the fixed-rate ZeroTTS contract, and the seam count.

---

## 5. Verification evidence

| Check | Command | Result |
|---|---|---|
| Formatting / vet | `gofmt -l cmd internal test`; `go vet ./...` | clean |
| Adapter contract tests | `python test_tts_engine.py` (in `cmd/stageworker/adapters`) | **39 tests OK** (35 pre-review + 4 added in review remediation) |
| ZeroTTS dual-pin identity gate (fail-closed) | `python -m unittest test_tts_engine.TestTTSEngineUpstreamContracts.test_run_zerotts_distribution_version_mismatch_fails_closed test_run_zerotts_missing_distribution_metadata_fails_closed test_run_zerotts_module_literal_drift_fails_closed test_run_zerotts_shipped_pin_pair_is_admitted` | all **OK** — distribution-metadata mismatch, absent distribution metadata and module-literal drift each fail closed with `TTS_RUNTIME_VERSION_MISMATCH`; the shipped pair (`0.1.5` dist + `0.1.2` literal) is admitted. Red-proof: deleting the distribution-metadata check turns 3 of the 4 red (`FF.F`) |
| Real pinned ZeroTTS runtime (new venv + new snapshot) | `go test ./test/seam2/ -count=1 -v -run 'TestSeam2_ZeroTTS'` | **PASS**, 47.58 s — `TestSeam2_ZeroTTSRealPinnedRuntime` 31.96 s: real StageWorker synthesis for `quangminh` and `maichi`, runtime identity observed at the new exact pins, 48 kHz mono WAV, `MeasuredDurationMs == ProbeWAVBytes`, audio SHA-256 self-consistent, competing `asr` GPU lease still held (CPU-only), plus absent-snapshot / missing-runtime / invalid-entrypoint / empty-audio / exact-identity fail-closed subtests |
| VieNeu `v3.8.1` production adapter path (offline) | `run_vieneu_tts(text, "vi", "Trúc Ly", 1.0, model_path=<new snapshot>, entrypoint_file=<snapshot catalog>)` with `HF_HUB_OFFLINE=1`, `venvs\tts` runtime | success: `measured_duration_ms=2880`, audio SHA-256 `0875ea0d…`, 276 525 WAV bytes |
| VieNeu `v3.8.1` against the *old* snapshot (control) | same probe with `…\staging\vieneu_v3_turbo` | success: `measured_duration_ms=3760`, SHA-256 `e7b34c65…` — the SDK loads either revision offline; the different duration reflects the changed weights/catalog |
| Full repository suite | `go test ./... -count=1` | see §5.1 |

### 5.1 Full-suite result

`go test ./... -count=1 -timeout 3600s` → **exit 0**, 195.9 s wall clock, **17 packages ok / 0 FAIL**
(3 packages report `[no test files]`):

```
ok  cmd/runtimehost 0.087s          ok  internal/provider  20.167s
ok  cmd/stageworker 3.491s          ok  internal/queue      0.590s
ok  internal/benchmark 3.249s       ok  internal/server     0.937s
ok  internal/cas 0.174s             ok  internal/service   26.901s
ok  internal/config 0.076s          ok  internal/speech     0.052s
ok  internal/domain 0.169s          ok  internal/storage    2.628s
ok  internal/governance 4.305s      ok  test/seam1       190.534s
ok  internal/media 1.257s           ok  test/seam2       128.192s
                                    ok  test/steering      0.044s
```

Graph impact: `gitnexus detect-changes --scope compare --base-ref ace14d3` → 10 files, 50 symbols,
risk **medium**, 4 affected processes, all TTS/snapshot-related (`ProbeSpeakerEvidence → SetModelSnapshotEnvelope`,
`DefaultASRInvoke → MakeSnapshotKey`, `DefaultAlignerInvoke → MakeSnapshotKey`,
`ProbeSpeakerEvidence → MakeSnapshotKey` — the ASR/aligner entries appear only because they share the
snapshot-key helpers the pin constants feed). The review-remediation working tree measured
separately: `--scope all` → 4 files, 32 symbols, risk **low**, 0 affected processes. `gitnexus impact` on `VieNeuModelVersion` and
`ZeroTTSModelVersion` returns `UNKNOWN` (module-scope const reads produce no graph edges by design);
every reference site was confirmed by text search instead — all of them go through the constants
listed in §4, and no literal pin remains outside synthetic fixture markers.

---

Re-run at the pushed head (the three remediation commits: dual-pin tests, unread-constant removal,
record corrections): `go test ./... -count=1 -timeout 3600s` → **exit 0**, 190.2 s wall clock,
**17 packages ok / 0 FAIL** (`test/seam1` 181.4 s, `test/seam2` 116.7 s, `test/steering`);
`python test_tts_engine.py` → **39 tests OK**. Graph impact at
the pushed head (`--scope compare --base-ref origin/main`, i.e. `ace14d3`): 11 files, 57 symbols,
risk **medium**, same four TTS/snapshot flows.

## 6. Residual notes / risks

- The ZeroTTS `maichi` preset and the VieNeu `Trúc Ly` preset produce different audio than before
  this upgrade. Historical frozen `VoiceAssignment`s keep their provider/voice identity and remain
  routable, but re-synthesis of a historical assignment is not bit-identical — consistent with the
  documented "identity preserved, artifacts regenerated on demand" contract.
- The VieNeu lane still resolves its codec/tokenizer via the snapshot-local `moss_tokenizer/` on the
  PyTorch path. If that lane is ever moved to the torch-free ONNX engine (`backend="onnx"`,
  `precision="fp32"`), the snapshot must additionally carry a local codec directory, otherwise the
  engine would attempt a Hub fetch and fail closed offline.
- `zerotts.__version__` remains a stale `"0.1.2"` literal upstream; the pin is deliberate and
  documented next to the constant. If upstream fixes the literal, the adapter will fail closed until
  the constant is updated — intended.
- The VieNeu lane's model revision / SDK commit are provenance records only — recorded in the §1
  table, carried by no constant (see the §1 enforcement map): snapshot registration accepts the
  operator-supplied version label, so a VieNeu snapshot
  registered as `v3.8.1` with different weights would not be rejected. Enforcing the weights digest
  (the HF LFS oid of `update/model.safetensors`) through snapshot verification is a follow-up, not
  part of this upgrade.
