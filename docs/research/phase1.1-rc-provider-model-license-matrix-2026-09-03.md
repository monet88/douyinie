# Phase 1.1 RC provider / model / checkpoint / license matrix — 2026-09-03

## Scope

Research resolution for Wayfinder Issue #55 against `main@7bbac44969442444934338867db4abcb29a4d24b`.

This note resolves the Release Candidate pinning contract and records live gaps in the shipped production composition. It does **not** implement new providers or relax Phase 1 policy/license invariants.

## Decision summary

1. **RC identity is an immutable artifact snapshot, not a friendly model/version string.** Every local model-backed provider must resolve to:
   - provider ID;
   - upstream code/package identity and immutable revision/version;
   - model repository + immutable revision/tag;
   - a canonical snapshot SHA-256 (for multi-file snapshots: SHA-256 over a stable sorted `path -> file_sha256` manifest; for a single-file checkpoint the checkpoint SHA-256 may be used directly);
   - the four independent obligation layers: `CODE_LICENSE`, `MODEL_LICENSE`, `DATA_LICENSE`, `SERVICE_TERMS`;
   - policy state and evidence URLs/timestamps.
2. **`Verified=true` means the evidence and downloaded bytes were verified; it is not itself a permission grant.** If upstream has no separate data-license grant, the manifest must say so explicitly (for example, `UPSTREAM_NOT_SEPARATELY_DECLARED`) instead of silently copying the model license into `DATA_LICENSE`. Policy decides whether that evidence posture is eligible.
3. **Local RC execution may not resolve a floating branch or model name at runtime.** Model hubs may be used to materialize the approved snapshot, but workers must execute the pinned local snapshot (or an equivalent revision-pinned cache resolution) and verify its digest before invocation.
4. **Remote-service providers cannot be cryptographically pinned like local weights.** A remote entry instead pins provider/service identity, requested model/version string, API contract/version when available, service-terms/privacy evidence and evidence timestamp; the service remains `REQUIRES_EXPLICIT_CONSENT` / `REQUIRES_AUTHORIZATION` as appropriate.
5. **Anything without complete evidence or byte-level pin enforcement is not `ALLOWED` for the RC automatic route.** It remains conditional/blocked until the Release Hardening implementation closes that gap.

## Shipped production composition observations

Verified directly in source at the scoped commit:

- `internal/provider/worker_adapters.go` registers production ASR, aligner, diarizer, TTS, separator and OCR providers.
- `cmd/runtimehost/main.go` composes that registry into the production Router.
- `internal/service/translation.go` routes production translation through `provider.TypeTranslation`, but the production registry registers **no `TextTranslationProvider`**. Only fake/test translation providers exist. A normal production Local/Hybrid translation request therefore has no production candidate.
- `internal/provider/router.go` calls `LicenseService.VerifyCheckpoint(modelName, modelVersion, "")`. This proves a verified manifest exists, but because `expectedSHA256` is empty it does **not** compare the manifest digest with the model bytes the worker actually loads.
- `internal/governance/license.go` stores a SHA-256 in `LicenseManifestEntry`, but current worker adapters resolve floating model IDs/names independently; the stored digest is not bound to the loaded snapshot.
- `cmd/stageworker/adapters/ocr.py` initializes `PaddleOCR(use_angle_cls=True, lang="ch")` but the production function currently returns an empty detection set without sampling/processing video frames. The registry label `paddleocr-v6@v6` therefore does not describe a release-capable OCR checkpoint path.
- `internal/provider/tts.go` uses internal preset aliases (`en_heart`, `en_deep`, `vi_female_natural`, `vi_male_deep`) while the real adapters forward/fallback against upstream voice identities. Kokoro upstream documents `af_heart`, `am_*` voices; VieNeu v3 Turbo documents named v3 presets such as `Ngọc Lan` and `Bình An`. The RC must pin actual upstream voice assets/IDs rather than unverified aliases.

These are Release Hardening gaps, not reasons to reopen the locked Phase 1 architecture.

## RC matrix

`Required` means the lane is part of the Phase 1.1 Local 8 GB / Hybrid release contract. `Conditional` means it may remain available only after its pin/evidence contract is satisfied. `Excluded` means it is not needed to pass Phase 1.1.

| Stage | Provider / lane | Current adapter identity | RC pin / decision | Evidence posture | RC status |
|---|---|---|---|---|---|
| ASR quality | `qwen3_asr_1_7b` | `qwen3-asr@1.7b` -> `Qwen/Qwen3-ASR-1.7B` | Model repo revision `7278e1e70fe206f11671096ffdd38061171dd6e5`; upstream code `QwenLM/Qwen3-ASR@7c6daf77a2421100f5fb066495372c00129d39ff`; pin runtime package independently. Current model shards include SHA-256 `a4cd1f1a04d90b757dc7f7dd26254e69a013b19e80efe590a83c6a3bde8608d6` and `6e0b9d9e09e2e0238e7ef3cc8a484ab387e91b90f1900bedf88bc92d7929ccfc`; release artifact uses the whole snapshot digest. | Code/model: Apache-2.0. Data: upstream disclosure, no separate data-license grant assumed. Service: local execution; host terms only for download. | **Required**, pending executable snapshot enforcement + Local 8 GB benchmark evidence. |
| ASR fallback | `qwen3_asr_0_6b` | `qwen3-asr@0.6b` -> `Qwen/Qwen3-ASR-0.6B` | Model revision `5eb144179a02acc5e5ba31e748d22b0cf3e303b0`; model SHA-256 `79d6cbd4c98c7bbffe9db2edac07f56cd6637d0d5944b27f6c2b8353840323ea`; same upstream code pin. Existing 8 GB smoke used `qwen-asr==0.0.6`, Torch `2.8.0+cu128`, FP16/batch-1/chunked input; preserve that as the known-good baseline until intentionally superseded. | Code/model: Apache-2.0. Data: upstream disclosure only. Service: local. | **Required fallback**; known 8 GB viability. |
| Forced alignment | `qwen3_forced_aligner` | `Qwen3-ForcedAligner-0.6B@0.6b` | Model revision `c7cbfc2048c462b0d63a45797104fc9db3ad62b7`; model SHA-256 `47831d0e82f96b20e9034dba01a075ee06436654719f6a68289e49f1b65ce0e7`; same Qwen code/runtime package pin. | Code/model: Apache-2.0. Data: upstream disclosure only. Service: local. | **Required**. |
| Diarization embedding | `campplus_diarizer` dependency | `iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0` | Pin ModelScope tag `v1.0.0` (`61dd513bae70ac7153e343e4d3c1c08c06ea117d`); 3D-Speaker code `065629c313eaf1a01c65c640c46d77e61e9607b4`; release snapshot must record full `campplus_cn_en_common.pt` hash from the downloaded tag. | 3D-Speaker code: Apache-2.0. Model card/license evidence must be copied into RC manifest; data provenance remains separate. Service: local. | **Required for multi-speaker cases**, conditional until local snapshot digest/evidence is materialized. |
| Diarization VAD dependency | CAMPPlus VAD | `iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4` | Pin ModelScope tag `v2.0.4` (`8e72ae941ffebf7b4df5a00b89e3b80f42f6d2a7`); materialize full checkpoint snapshot digest. | Treat as an independent dependency in `LicenseManifestEntry`; do not inherit CAMPPlus obligations implicitly. | **Required dependency**, conditional until digest/evidence materialized. |
| Translation | `provider.TypeTranslation` | **No production provider registered** | No valid RC pin exists as shipped. Select an explicit Local and/or Hybrid production translation lane that implements `TextTranslationProvider`, VI/EN output, meaning-first QA compatibility and shorten-first adaptation, then pin its local weights or remote service contract. | Unknown until provider selection. | **RC BLOCKER**. |
| VI TTS baseline | `vieneu_tts_vi` | registry says `vieneu-tts@1.0.0`; adapter actually constructs `Vieneu(mode="v3turbo")` | Pin `pnnbao-ump/VieNeu-TTS-v3-Turbo@1278db0090b98ccf23e56f2423857fc9d32a5118`; upstream SDK source `pnnbao97/VieNeu-TTS@d4af71c5fb999c5538f2f1ec6d5ed7cee83eddce`; current primary `model.safetensors` SHA-256 `0ea96dbb5a7618814b10fde6b5027d9589bf91208f41b8954fdf184a608e9d2c`; snapshot digest must include codec/encoder/ONNX assets actually used. Freeze actual preset IDs, e.g. `Ngọc Lan` (female) and `Bình An` (male), rather than internal aliases. | Model/package page declares Apache-2.0 and says model trained from scratch on ~10k hours EN-VI; no separate data-license grant should be invented. Service: local. | **Required VI baseline**, conditional on preset mapping + executable pin enforcement. |
| Duration-fit TTS | `cosyvoice3_tts` | registry `cosyvoice3@3.0.0`; adapter defaults to `FunAudioLLM/Fun-CosyVoice3-0.5B-2512` | Pin HF model revision `29e01c4e8d000f4bcd70751be16fa94bf3d85a18` and CosyVoice source `074ca6dc9e80a2f424f1f74b48bdd7d3fea531cc`; model is a multi-file ~9.75 GB snapshot, so use canonical snapshot digest rather than one arbitrary weight file. | Code/model metadata: Apache-2.0. Data/examples must remain a separate evidence line; the model discussion contains commercial-use ambiguity around demo/example text, so RC evidence must preserve the exact model-license metadata and any upstream clarification instead of inferring from examples. Service: local. | **Conditional quality / measured-fit lane**; benchmark decides whether it is required on Local 8 GB or Hybrid only. |
| EN TTS baseline | `kokoro_tts_en` | `kokoro-tts@1.0.0` | Pin `hexgrad/Kokoro-82M@f3ff3571791e39611d31c381e3a41a3af07b4987`; `kokoro-v1_0.pth` SHA-256 `496dba118d1a58f5f3db2efc88dbdc216e0483fc89fe6e47ee1f2c53f18ad1e4`; pin inference library source `hexgrad/kokoro@dfb907a02bba8152ca444717ca5d78747ccb4bec` / package version used by RC. Freeze real voice assets: `af_heart` for the default female lane and a documented `am_*` voice (recommend `am_michael`) for male; each voice file gets its own SHA-256 in the snapshot manifest. | Code/model: Apache-2.0. Model card explicitly describes permissive/non-copyrighted training data and lists CC-BY contributions; preserve that evidence instead of collapsing it into model license. Service: local. | **Required EN baseline**, conditional on preset-ID repair + executable pin enforcement. |
| Clone fallback | `chatterbox_tts` | `chatterbox-tts@1.0.0` | Not needed for the Phase 1.1 hard gate. Current HF `ResembleAI/chatterbox@5bb1f6ee58e50c3b8d408bc82a6d3740c2db6e18`; source `resemble-ai/chatterbox@5de7a54aa4e5e2baadb0182dde554908b48b85c2`. Do not silently follow current main if re-enabled; create a frozen snapshot and verify reference-voice authorization. | Code/model currently marked MIT; voice-reference rights are independent operator data/consent obligations. | **Excluded from required RC route**; keep blocked/optional unless explicitly enabled. Voice cloning is not a Release Hardening requirement. |
| Separator primary | `uvr_separator` | `UVR-MDX-NET-Inst_HQ_4.onnx@v3` via `audio-separator` | Pin `python-audio-separator` source/release (`bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab`; project metadata reports v0.44.2 at research time) and materialize the exact `UVR-MDX-NET-Inst_HQ_4.onnx` file SHA-256 used by the RC. Do not treat filename `@v3` as a checkpoint digest. | Wrapper code: MIT. Maintainer docs ask integrations using UVR-trained models to honor MIT and credit UVR; training-data provenance remains a separate evidence item. | **Required candidate**, conditional until exact model bytes/evidence are pinned. |
| Separator fallback | `demucs_separator` | `htdemucs@v4` | Pin Demucs source `e976d93ecc3865e5757426930257e200846a520a`; `htdemucs.yaml` resolves model signature `955717e8` and remote artifact `955717e8-8726e21a.th`. The RC materializer records the downloaded artifact's full SHA-256. | Code: MIT. Model/data provenance must be captured separately; do not infer a data license from the code license. Service: local. | **Required fallback**, conditional on full weight/data evidence. |
| OCR / text detection | `ppocr_v6` | registry says `paddleocr-v6@v6`; adapter calls floating `PaddleOCR(use_angle_cls=True, lang="ch")` and returns no frame detections | No valid RC checkpoint pin exists as shipped. Freeze exact PP-OCRv6 detector/recognizer/orientation submodels and package revision only after the adapter actually processes sampled frames and reports model identities. PaddleOCR source HEAD observed as `2661c7c0ef5c613e8f93c6e93b2e052399f0f854`; do not use that floating head as the release pin. | PaddleOCR code is Apache-2.0; each downloaded model artifact still needs exact model/data/service evidence. | **RC BLOCKER** for visual localization. |

## Mandatory pin materialization format

For every RC local dependency, the release evidence must contain a machine-readable entry equivalent to:

```text
provider_id
stage
execution_profiles
code_source + code_revision + package_version
model_source + model_revision
snapshot_manifest_sha256
files[] = { relative_path, sha256, size_bytes }
CODE_LICENSE = { value, evidence_url, evidence_sha256 }
MODEL_LICENSE = { value, evidence_url, evidence_sha256 }
DATA_LICENSE = { value_or_explicit_not_separately_declared, evidence_url, evidence_sha256 }
SERVICE_TERMS = { local_no_runtime_service | remote_terms, evidence_url, captured_at }
policy_state
verified_at
```

The snapshot digest is the value that should back `LicenseManifestEntry.SHA256` for a multi-file dependency. A provider must not be eligible merely because a database row contains a matching friendly `ModelName/ModelVersion`.

## Required follow-up decisions surfaced by this audit

1. **Production translation lane:** choose the actual VI/EN translation provider(s) for Local 8 GB + Hybrid and decide their model/service pinning and privacy policy. The current production registry has none.
2. **Executable pin contract:** decide how RuntimeHost materializes approved snapshots, seeds/loads immutable license manifests, passes pinned local model locations/revisions to workers, and verifies the loaded snapshot digest before policy eligibility. Current manifests are evidence records but are not cryptographically bound to the bytes workers resolve.
3. **Production adapter compatibility:** close the concrete release gaps before corpus execution: real frame OCR with explicit PP-OCRv6 submodel pins; real upstream TTS preset IDs/assets (Kokoro and VieNeu); and verification that the chosen separator/TTS assets satisfy the 8 GB / Hybrid route.

## Evidence anchors

Repo-local source:

- `internal/provider/worker_adapters.go`
- `internal/provider/router.go`
- `internal/governance/license.go`
- `internal/provider/tts.go`
- `internal/service/translation.go`
- `cmd/stageworker/adapters/asr_qwen3.py`
- `cmd/stageworker/adapters/aligner_qwen3.py`
- `cmd/stageworker/adapters/diarizer_3dspeaker.py`
- `cmd/stageworker/adapters/tts_engine.py`
- `cmd/stageworker/adapters/separator.py`
- `cmd/stageworker/adapters/ocr.py`
- `docs/research/qwen3-asr-forced-aligner-smoke-2026-08-19.md`

Upstream / model evidence:

- Qwen code: https://github.com/QwenLM/Qwen3-ASR
- Qwen model repos: https://huggingface.co/Qwen/Qwen3-ASR-1.7B ; https://huggingface.co/Qwen/Qwen3-ASR-0.6B ; https://huggingface.co/Qwen/Qwen3-ForcedAligner-0.6B
- 3D-Speaker: https://github.com/modelscope/3D-Speaker
- VieNeu v3 Turbo: https://huggingface.co/pnnbao-ump/VieNeu-TTS-v3-Turbo ; https://github.com/pnnbao97/VieNeu-TTS
- CosyVoice3: https://huggingface.co/FunAudioLLM/Fun-CosyVoice3-0.5B-2512 ; https://github.com/FunAudioLLM/CosyVoice
- Kokoro: https://huggingface.co/hexgrad/Kokoro-82M ; https://github.com/hexgrad/kokoro
- Chatterbox: https://huggingface.co/ResembleAI/chatterbox ; https://github.com/resemble-ai/chatterbox
- audio-separator / UVR: https://github.com/nomadkaraoke/python-audio-separator
- Demucs: https://github.com/facebookresearch/demucs
- PaddleOCR: https://github.com/PaddlePaddle/PaddleOCR

## Release implication

Issue #55 can be resolved as a **decision/research ticket**: the RC identity and matrix posture are now specified. The Release Candidate itself is **not** ready. Production translation, byte-level snapshot enforcement/manifest materialization, and production adapter compatibility remain explicit downstream blockers to be ticketed and resolved before the 48-output corpus can constitute release evidence.
