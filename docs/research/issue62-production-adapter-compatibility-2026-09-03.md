# Issue #62 — Production Adapter Compatibility & Pinning Contract

Date: 2026-09-03
Status: RESOLVED DECISION (research/design only; implementation-ready)
Errata (2026-09-18): the gateway DeepSeek alias is now `deepseek-v4.1-flash` (no `deepseek/` prefix); ladder order and all other decisions here are unchanged.
Parent: https://github.com/monet88/douyinie/issues/53
Issue: https://github.com/monet88/douyinie/issues/62

## Decision summary

Phase 1.1 keeps the two approved testing seams and fixes the current production-adapter mismatches without reopening Phase 1 architecture.

The RC contract is:

1. **OCR:** replace the placeholder `paddleocr-v6@v6` behavior with real sampled-frame execution using the PP-OCRv6 medium detector and recognizer plus the text-line orientation model, all provisioned as verified local snapshots under the Issue #61 contract.
2. **VI TTS:** keep VieNeu v3 Turbo as the required Vietnamese preset lane, but replace internal aliases with exact upstream preset IDs from a pinned voice catalog.
3. **EN TTS:** keep Kokoro 82M as the required English preset lane, using exact upstream voice IDs and voice assets.
4. **Separation:** keep UVR as primary and `htdemucs` as fallback, but load exact provisioned local artifacts only; runtime auto-download is forbidden.
5. **Remote translation:** for Hybrid/Cloud, use the user's OpenAI-compatible gateway in this order: `gemini-3.8-flash`, then `deepseek/deepseek-v4-flash-vision-exp`, then verified local Qwen. `ExecutionProfileLocal` remains local-only and fail-closed.
6. **Optional lanes:** Chatterbox cloning and direct Google/DeepSeek API paths remain outside the RC hard route. CosyVoice3 remains conditional and is not part of the default preset rotation unless later benchmark evidence promotes it.

## 1. OCR — executable PP-OCRv6 lane

### 1.1 Exact RC identities

Pin the OCR runtime to:

- PaddleOCR package: `3.7.0`.
- PaddleOCR source tag: `v3.7.0` at commit `b03f46425e8ff4442b268ce449e3eef758146cd4`.
- Text detection: `PP-OCRv6_medium_det`.
- Text recognition: `PP-OCRv6_medium_rec`.
- Text-line orientation: `PP-LCNet_x1_0_textline_ori`.

Do not use the worker's earlier PP-OCRv4 model proposal. The shipped adapter currently calls the legacy-style `PaddleOCR(use_angle_cls=True, lang="ch")` and then returns an empty detection list; that is not an executable RC identity.

Document orientation classification and document unwarping are not required for sampled Douyin video frames and should be disabled for this RC lane unless benchmark evidence later proves a need for them.

### 1.2 Snapshot and reporting contract

Each OCR submodel is an independent Issue #61 dependency with its own verified snapshot. The RuntimeHost/StageWorker envelope must identify at least:

- dependency role (`det`, `rec`, `textline_orientation`);
- exact model name;
- local verified snapshot path;
- snapshot manifest digest;
- pinned PaddleOCR package/runtime identity.

Do not hardcode guessed model-file sizes or hashes in this decision. The RC materializer records every file actually downloaded for each selected model and computes the canonical snapshot digest from those bytes.

The OCR artifact/provenance must report the exact three model names plus their snapshot digests; friendly `paddleocr-v6@v6` alone is insufficient.

### 1.3 Adapter execution contract

`cmd/stageworker/adapters/ocr.py` must:

1. open `media_path` with `cv2.VideoCapture`;
2. sample frames deterministically from `frame_sample_step_ms` and `max_frames`;
3. run the pinned local PP-OCRv6 pipeline against each sampled frame;
4. return text, confidence, frame index/timestamp, and the source polygon or deterministic axis-aligned box conversion;
5. preserve real frame width/height rather than returning fixed placeholder dimensions;
6. fail rather than silently returning a successful empty placeholder when the adapter itself did not execute;
7. load only verified local snapshot paths and perform no model-hub/network resolution during request execution.

A legitimate frame with no text may still produce zero detections; acceptance therefore uses fixtures with known visible text rather than treating every empty result as a runtime error.

## 2. TTS — exact preset identities and stable speaker mapping

### 2.1 VieNeu v3 Turbo (Vietnamese)

Pin:

- model repository: `pnnbao-ump/VieNeu-TTS-v3-Turbo` at revision `1278db0090b98ccf23e56f2423857fc9d32a5118`;
- SDK/source: `pnnbao97/VieNeu-TTS` tag `v3.2.9` at commit `149ff16a6a50093a0cad1b75d5edf9e9d81d97f4`;
- model snapshot: retain the Issue #55 model snapshot contract, including the pinned `model.safetensors` digest and every codec/tokenizer/ONNX dependency actually loaded by this SDK revision;
- voice catalog: the `voices_v3_turbo.json` bundled with the pinned `v3.2.9` source snapshot.

This explicitly supersedes only the older Issue #55 **SDK/voice-catalog source revision** where necessary to freeze the current 20-preset catalog; it does not replace the already-pinned HF model repository revision.

Required RC Vietnamese preset rotation, in order:

1. `Trúc Ly` — female, Northern, natural;
2. `Phạm Tuyên` — male, Northern, natural;
3. `Đoan Trang` — female, Northern, natural;
4. `Xuân Vĩnh` — male, Southern, natural.

The first two are the default female/male pair. The third/fourth provide deterministic distinct voices for additional speakers. `Minh Đức` remains a valid upstream preset but is not the default natural male lane because the pinned catalog describes it as a news-style voice.

VieNeu v3 Turbo output is 48 kHz; the adapter may encode to mono PCM WAV for Douyinie artifacts, but the artifact must preserve the actual measured sample rate and duration.

### 2.2 Kokoro 82M (English)

Retain the Issue #55 pins:

- model repository: `hexgrad/Kokoro-82M@f3ff3571791e39611d31c381e3a41a3af07b4987`;
- core model file `kokoro-v1_0.pth` SHA-256: `496dba118d1a58f5f3db2efc88dbdc216e0483fc89fe6e47ee1f2c53f18ad1e4`;
- inference source: `hexgrad/kokoro@dfb907a02bba8152ca444717ca5d78747ccb4bec`.

Required RC English preset rotation, in order:

1. `af_heart` — American female;
2. `am_michael` — American male;
3. `af_bella` — American female;
4. `am_fenrir` — American male.

Each selected voice file is part of the model snapshot manifest and receives its own file digest. Kokoro output is 24 kHz.

### 2.3 Mapping semantics

Current `DubbingService.AssignVoices` already sorts speaker IDs and assigns ordered presets deterministically with modulo rotation. Preserve that behavior.

Implementation must change `DefaultPresetVoices` so the RC-required ordered lists above carry the **real upstream `VoiceID` values**, not aliases such as `vi_female_natural`, `vi_male_deep`, `en_heart`, or `en_deep`.

For a run:

- sorted `SPEAKER_00`, `SPEAKER_01`, ... map to the ordered preset list;
- `UseSameVoiceForAll` continues to use the first preset unless the operator supplied an explicit assignment;
- the resulting `VoiceAssignment` remains immutable for that run;
- `VoiceProfile.VoiceID` and downstream provenance record the exact upstream preset ID;
- sentence-level engine/voice hopping remains prohibited.

CosyVoice3 remains conditional and must not silently enter the default RC preset rotation.

## 3. Audio separation — exact local snapshots, no runtime download

### 3.1 UVR primary

Pin:

- wrapper source: `nomadkaraoke/python-audio-separator@bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab`;
- RC model filename: `UVR-MDX-NET-Inst_HQ_4.onnx`.

The RC materializer must resolve the exact ONNX artifact and any model metadata required by the pinned wrapper, record every file SHA-256/size, and publish the verified local snapshot before execution. The filename and friendly `@v3` label are not checkpoint digests.

The current `Separator().load_model(model_name)` behavior may download missing models. The RC adapter must instead point the wrapper at the verified local artifact/cache and fail closed if it is absent; no network/model-hub fallback is permitted during a request.

### 3.2 Demucs fallback

Pin:

- source: `facebookresearch/demucs@e976d93ecc3865e5757426930257e200846a520a`;
- model: `htdemucs`;
- `htdemucs.yaml` model signature: `955717e8`;
- checkpoint artifact: `955717e8-8726e21a.th`.

Do **not** substitute the four-checkpoint `htdemucs_ft` bag while reporting it as `htdemucs`; they are different model identities.

The StageWorker must load the provisioned local checkpoint/config only and perform two-stem vocals extraction without downloading weights at runtime.

## 4. Gateway translation — user-priority override and reproducibility

### 4.1 Routing order

For `hybrid` and conditional cloud capability:

1. `gemini_gateway_translator` requesting `gemini-3.8-flash`;
2. `deepseek_gateway_translator` requesting `deepseek/deepseek-v4-flash-vision-exp`;
3. `qwen3_4b_translator` using the pinned local Qwen3-4B GGUF lane from Issue #60.

Both remote lanes use the user's existing OpenAI-compatible gateway. The verified non-secret smoke configuration identifies the gateway base as `https://cliproxy.monet.uno/v1`; secrets remain outside Git-tracked files.

For `local`:

- only `qwen3_4b_translator` is eligible;
- no gateway/network call is allowed;
- local failure is fail-closed.

No direct Google Gemini API or direct DeepSeek API path belongs to the Phase 1.1 RC route.

### 4.2 Provider contract

The two remote aliases have distinct provider IDs even though they share one gateway transport. Each provider record must capture:

- requested gateway model alias;
- gateway endpoint/contract identity;
- observed `response.model` when the gateway exposes it;
- `system_fingerprint` or equivalent backend fingerprint when exposed;
- request/response identifier and timestamp when exposed;
- a release `service_baseline_id` that changes when the observed gateway/backend identity materially changes;
- credential-reference identity, never the secret value;
- applicable gateway/operator and upstream-service privacy/terms evidence as independent obligation evidence where it can be established.

Do not invent an immutable checkpoint hash for a remote service. Reusable translation cache identity must include the established remote service baseline (or otherwise prevent cross-baseline cache reuse) so a cached result cannot hide service drift.

The new gateway aliases must each pass a live preflight before RC use. Do not assume that a parameter supported by the older Gemini smoke or by a provider's direct API is automatically supported by both new gateway aliases. Structured JSON mode, timeout behavior, retryable status classes, and any model-specific thinking-control parameter are capabilities to verify against the gateway, then pin in the runtime/provider contract.

The user states that both configured gateway models accept image input. Treat that as an available configured capability, but require a gateway image-input smoke before using it as RC evidence. Vision capability does not replace the dedicated OCR/TextRegion stage. Any future remote-vision OCR lane requires a separate provider identity, consent/policy evaluation, provenance, and acceptance evidence.

### 4.3 Meaning-first translation remains unchanged

`TextTranslationProvider` receives one `TargetLanguage` per request and returns `TranslationSegment.TargetText` for that target language. The gateway adapter therefore runs the same provider contract independently for VI and EN jobs; it must not return a special `vi_short` field as part of the meaning-first artifact.

The translation stage produces faithful `TranslationVariant` content only. Duration shortening remains downstream in `SpokenScriptAdapter`, which creates `DubScriptVariant.SpokenText`. Existing fact/name/number/negation QA remains authoritative.

## 5. Required pre-corpus smoke evidence

Every RC lane must pass its own smoke before entering the 24-video corpus.

### OCR

- exact PaddleOCR 3.7.0 runtime loads the three pinned local model snapshots offline;
- a fixture video containing known Chinese text is sampled through the real adapter;
- expected text-region detections contain non-empty text, confidence, bounding geometry, and timestamps in the expected frame windows;
- provider evidence reports all three model identities and verified snapshot digests;
- no model download/network resolution occurs during the request.

### VieNeu VI TTS

- exact pinned SDK/model snapshot loads offline;
- synthesize a fixed Vietnamese phrase with `Trúc Ly` and `Phạm Tuyên`;
- output is non-empty valid mono WAV at the expected 48 kHz and measured duration is positive;
- artifact/provenance records the exact voice IDs and snapshot identity.

### Kokoro EN TTS

- exact pinned model/source snapshot loads offline;
- synthesize a fixed English phrase with `af_heart` and `am_michael`;
- output is non-empty valid mono WAV at 24 kHz and measured duration is positive;
- artifact/provenance records exact voice asset IDs/digests.

### Separator

- UVR primary loads the provisioned `UVR-MDX-NET-Inst_HQ_4.onnx` snapshot with network disabled;
- Demucs fallback loads the provisioned `955717e8-8726e21a.th` snapshot with network disabled;
- a fixed audio fixture produces non-empty vocals and background/no-vocals outputs with duration aligned to the input within the benchmark's accepted tolerance;
- evidence records the exact model snapshot used for each attempt.

### Gateway translation

Run separate smokes for both gateway aliases and both target languages:

- ordered source segments in, ordered target segments out;
- output parses into the existing target-language `TranslationResult` contract;
- number/name/negation preservation reaches the existing QA gate;
- requested alias and observed model/fingerprint metadata are captured where exposed;
- consent and authorized credential references are required before outbound transcript transmission;
- fallback from Gemini gateway to DeepSeek gateway and then local Qwen is observable in provider-attempt/selection evidence.

If image capability is used by the RC, run a separate image-input gateway smoke for each remote alias; image capability is otherwise not a hard-gate requirement for translation.

### Local Qwen translation

- exact Issue #60 Qwen snapshot runs fully offline through Seam 2;
- real peak VRAM is measured on the target RTX 2060 SUPER 8 GB machine with the final bounded context/runtime;
- acceptance criterion is successful execution without OOM within the 8 GB target, not an invented fixed VRAM threshold;
- no external socket/model download occurs during request execution.

## 6. Optional/disabled lanes for the Phase 1.1 hard gate

- Chatterbox voice cloning: disabled; preset voices are sufficient for RC.
- CosyVoice3: conditional quality/duration-fit lane only; not part of default VI/EN preset assignment until benchmark evidence promotes it.
- Direct Google Gemini / direct DeepSeek translation APIs: disabled; RC remote translation uses the user's gateway only.
- Remote vision as OCR replacement: not part of the hard gate.
- CapCut private TTS APIs: reference-only, not a default production voice lane.

## 7. Minimum implementation delta after Wayfinder

The implementation spec generated after the Wayfinder must include, at minimum:

1. real PP-OCRv6 frame sampling/execution and exact model-snapshot reporting in `cmd/stageworker/adapters/ocr.py`;
2. Issue #61 `model_snapshot` path/digest plumbing for OCR/TTS/separator/local translation workers;
3. exact upstream preset IDs in `DefaultPresetVoices` and removal of silent VieNeu alias fallback for RC voices;
4. local-only UVR/Demucs model loading with runtime network/download fallback disabled;
5. production `TextTranslationProvider` implementations for the two gateway aliases and the local Qwen lane;
6. hard execution-profile eligibility so `ExecutionProfileLocal` cannot route to a remote provider;
7. remote `service_baseline_id`/observed-model handling in translation provenance/cache identity;
8. focused Seam 1 and Seam 2 smoke/acceptance coverage for the identities above.

Error-code names, numeric confidence cutoffs, retry counts, timeouts, VRAM limits, and other operational thresholds not already fixed by architecture/benchmark decisions are **not** created by this research ticket; they must come from existing domain contracts or the benchmark execution/metric decision.

## 8. Evidence anchors

Repo-local:

- `cmd/stageworker/adapters/ocr.py`
- `cmd/stageworker/adapters/tts_engine.py`
- `cmd/stageworker/adapters/separator.py`
- `internal/provider/tts.go`
- `internal/provider/translation.go`
- `internal/service/dubbing.go`
- `internal/service/translation.go`
- `internal/domain/translation.go`
- `internal/provider/router.go`
- `docs/research/provider-smoke-test-2026-08-19.md`
- Issue #55, #60 and #61 resolution evidence.

Primary upstream identities:

- PaddleOCR `v3.7.0` / PP-OCRv6.
- VieNeu-TTS tag `v3.2.9` at `149ff16a6a50093a0cad1b75d5edf9e9d81d97f4`; model repo `pnnbao-ump/VieNeu-TTS-v3-Turbo@1278db0090b98ccf23e56f2423857fc9d32a5118`.
- Kokoro source `dfb907a02bba8152ca444717ca5d78747ccb4bec`; model repo `f3ff3571791e39611d31c381e3a41a3af07b4987`.
- python-audio-separator source `bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab`.
- Demucs source `e976d93ecc3865e5757426930257e200846a520a`; `htdemucs` signature `955717e8` / checkpoint `955717e8-8726e21a.th`.

No implementation is performed by this ticket. The two approved testing seams remain unchanged.
