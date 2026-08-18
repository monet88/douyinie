# Phase 1 selected-stack license and service-policy audit

Reviewed: 2026-08-19

Scope: Wayfinder ticket **Audit licensing and service-policy constraints for the selected stack**.

This is an engineering policy audit based on current primary sources, not legal advice. License and service terms can change; implementation must pin and re-check the exact artifact/provider terms before release.

## Conclusion

The selected Phase 1 architecture remains technically viable, but provider routing must gain an explicit policy gate separate from runtime health.

The following rules are required:

1. `CODE_LICENSE`, `MODEL_LICENSE`, `DATA_LICENSE`, and `SERVICE_TERMS` are independent fields. A permissive client repository license does not authorize an upstream service, content source, model checkpoint, or training dataset.
2. Current Douyin public terms make unauthorized automated arbitrary-URL acquisition unsuitable as a production default. The existing `SourceAdapter` remains valid technically, but automated URL acquisition must be `REQUIRES_AUTHORIZATION` until Douyin-authorized or written permission exists.
3. Current CapCut terms prohibit unauthorized automated interaction/reverse-engineering patterns. Private/Web endpoint automation must be `REQUIRES_AUTHORIZATION`; an MIT/Apache client does not make the service integration policy-safe.
4. Gemini must record an explicit paid/unpaid data-handling tier. Unpaid Gemini must never be selected silently for source content that is confidential, personal, or otherwise unsuitable for model-improvement review.
5. ProPainter is non-commercial under its current S-Lab license unless separate commercial permission is obtained. It cannot be an automatic commercial Phase 1 fallback.
6. Any automatically downloaded ML checkpoint must fail closed unless its exact artifact/revision/hash and model license are present in the project allowlist. The wrapper package license is not enough.

## Decision matrix

| Stage / component | Code license | Model / service constraint | Phase 1 policy |
| --- | --- | --- | --- |
| `jiji262/douyin-downloader` | MIT | Douyin service/content terms separate | `REQUIRES_AUTHORIZATION` for automated arbitrary-URL acquisition |
| `Johnserf-Seed/f2` | Apache-2.0 | Douyin service/content terms separate | `REQUIRES_AUTHORIZATION` when used to automate Douyin access |
| Qwen3-ASR 1.7B / 0.6B | Apache-2.0 | Pin exact model revision/hash; training-data rights remain separate | `ALLOW` when exact model artifact is manifest-listed |
| Qwen3-ForcedAligner-0.6B | Apache-2.0 | Pin exact model revision/hash | `ALLOW` when manifest-listed |
| FireRedASR2S models | Apache-2.0 code; current published model family is Apache-2.0 | Pin exact model artifact; training-data rights remain separate | `ALLOW` when manifest-listed |
| FunASR toolkit | MIT | Model weights have separate terms; repo also carries a custom FunASR model license | `CONDITIONAL`; allow only exact reviewed checkpoints |
| FunASR FSMN-VAD / CAMPPlus | Exact current model cards report Apache-2.0 | Do not generalize that license to every FunASR model | `ALLOW` only for pinned reviewed revisions |
| Gemini API | Service terms | Paid and unpaid data handling differ materially | `CONDITIONAL`; explicit `data_handling_tier`, no silent unpaid fallback |
| VieNeu-TTS v3 Turbo | Apache-2.0 | v3 package/default voices are currently Apache-2.0; preserve attribution and third-party notices | `ALLOW` when pinned |
| VieNeu-TTS v2 preset voices | Repository/model metadata is not sufficient | `voices.json` states CC BY-NC 4.0 / non-commercial | `BLOCKED` for commercial delivery unless permission/clarification obtained |
| `kuwacom/CapCut-TTS` | MIT | CapCut service terms remain independent | `REQUIRES_AUTHORIZATION` for automated service access |
| CapCut Mate / VectCutAPI | Apache-2.0 | CapCut/JianYing service/private API terms remain independent | `REQUIRES_AUTHORIZATION`; optional integration only |
| `python-audio-separator` | MIT | Auto-downloads many third-party checkpoints with independent licenses | `CONDITIONAL`; exact checkpoint allowlist required |
| maintained Demucs fork | MIT code | Pretrained-weight license was not separately established in checked primary sources | `CONDITIONAL`; no automatic weight use without explicit artifact record |
| PaddleOCR | Apache-2.0 code | Exact PP-OCRv6 checkpoint license must be recorded separately | `CONDITIONAL` until selected checkpoint is manifest-listed |
| video-subtitle-extractor | Apache-2.0 | OCR/model artifacts remain separately governed | `ALLOW` as code/reference; models separately gated |
| video-subtitle-remover | Apache-2.0 | Bundles/invokes multiple inpainting models with separate provenance | `ALLOW` as wrapper; each model separately gated |
| STTN | MIT code | Exact checkpoint/training-data provenance must still be recorded | `CONDITIONAL` checkpoint |
| LaMa | Apache-2.0 code | Exact `big-lama` checkpoint must be separately manifest-listed | `CONDITIONAL` checkpoint |
| ProPainter | S-Lab License 1.0 | Non-commercial; commercial use requires permission | `BLOCKED` in commercial profile by default |
| FFmpeg | LGPL-2.1-or-later by default; build may become GPL with optional components | Redistribution obligations depend on actual build configuration | `ALLOW` with build provenance and notices |
| libass | ISC | Standard notice retention | `ALLOW` |

## Required implementation controls

### 1. License manifest

Every executable dependency, model/checkpoint, or remote service used by a Run must resolve to a versioned `LicenseManifestEntry` containing at least:

- `component_id`, `artifact_id`, upstream source, immutable revision and/or SHA-256;
- `code_license`, `model_license`, `data_license_status`, `service_terms_url`;
- `terms_checked_at`, commercial-use status, redistribution/attribution obligations;
- `policy_status` and allowed execution profiles.
Unknown is not equivalent to allowed. Runtime must reject an unmanifested artifact instead of downloading and executing it opportunistically.

### 2. Provider policy gate is separate from runtime health

Canonical policy states:

- `ALLOWED`
- `REQUIRES_EXPLICIT_CONSENT`
- `REQUIRES_AUTHORIZATION`
- `BLOCKED`

These are independent from provider health (`AVAILABLE`, `DEGRADED`, `UNAVAILABLE`). A healthy provider that is policy-blocked is ineligible for routing. `RunConfigSnapshot` should freeze the policy version and selected remote data-handling tier; credentials remain outside the snapshot.

### 3. Source rights and acquisition preflight

A Douyin URL is a locator, not proof that the operator has rights to download, transform, or commercially reuse the media. Preflight must capture a `SourceRightsBasis`/operator attestation before localization proceeds.

The checked current Douyin Open Platform documentation exposes authorized-account video list/data capabilities, but no generic arbitrary public-video media-download endpoint was found. Therefore the production-safe product path must not assume that a public URL can always be fetched automatically.

Until the follow-up acquisition decision is resolved, implementation should support a user-supplied/local source artifact as the policy-safe ingestion path while preserving URL metadata/context and the existing `SourceAdapter` seam.

### 4. Remote-provider data policy

Gemini routing must distinguish paid from unpaid service use. For content that is not explicitly suitable for unpaid processing, require a billing-enabled tier or explicit operator consent; never downgrade silently from paid to unpaid because of availability or cost.

### 5. Model download and benchmark policy

Benchmark/calibration candidates must first pass the license allowlist. Quality selection happens only among policy-eligible artifacts.

Auto-download logic must verify exact model identity, revision/hash, expected license record, and integrity before the artifact enters the model cache. A model already present on disk is not grandfathered into eligibility.

### 6. Distribution compliance

Packaged distributions must carry required third-party license/notice material and record the exact FFmpeg build/configuration used. Do not assume every FFmpeg binary is merely LGPL; enabled GPL/nonfree components can change distribution obligations.

## Impact on prior Wayfinder decisions

The technical architecture from the existing provider and runtime tickets remains valid, but routing gains a policy gate before health/fallback evaluation.

Two previous technical defaults become conditional rather than automatically production-eligible:
1. **Douyin acquisition:** Jiji/F2 remain useful SourceAdapter implementations/references, but current public platform terms prevent treating unauthorized automated arbitrary-URL acquisition as the production default. This is a product-policy decision, not a code-license problem.
2. **CapCut acceleration:** CapCut TTS/draft/render clients remain useful technical references, but automatic private/Web service invocation is disabled unless the operator/project has an authorized integration basis.

ProPainter is removed from the commercial automatic fallback set unless commercial permission is obtained.

## Open questions

1. What Phase 1 acquisition UX should preserve the destination's “start from a Douyin URL” flow while complying with the current platform-policy constraint: creator/account authorization, written/partner authorization, user-supplied source media, or some combination?
2. Which exact RoFormer/MDXC checkpoint(s) should be benchmarked after model-license review? `python-audio-separator` cannot answer this at the wrapper-license level.
3. Which exact PP-OCRv6 checkpoint/package will be pinned, and what license/provenance record accompanies that artifact?

Only question 1 is a material product/architecture decision that blocks locking the implementation-ready architecture. Questions 2–3 are implementation artifact-selection gates and should be enforced by the manifest rather than reopening the overall architecture.

## Recommended Wayfinder follow-up

Create a grilling ticket: **Decide Phase 1 source acquisition UX under Douyin platform-policy constraints**. It should resolve the policy-safe user flow while preserving `SourceAdapter`, immutable `SourceAsset`, source-rights attestation, and fail-closed routing.

## Primary sources

- Douyin User Service Agreement (updated 2026-02-13, effective 2026-02-20): https://www.douyin.com/agreements/?id=6773906068725565448
- Douyin Open Platform Developer Service Agreement: https://developer.open-douyin.com/docs/resource/zh-CN/dop/operation-standard/developers-service-agreement
- Douyin Open Platform video list API: https://developer.open-douyin.com/docs/resource/zh-CN/mini-app/develop/server/content/video-data/video-list
- CapCut Terms of Service: https://www.capcut.com/clause/terms-of-service
- Gemini API Additional Terms: https://ai.google.dev/gemini-api/terms
- FFmpeg legal/license page: https://ffmpeg.org/legal.html
- `jiji262/douyin-downloader` license: https://github.com/jiji262/douyin-downloader/blob/main/LICENSE
- `Johnserf-Seed/f2` license: https://github.com/Johnserf-Seed/f2/blob/main/LICENSE
- Qwen3-ASR license: https://github.com/QwenLM/Qwen3-ASR/blob/main/LICENSE
- FireRedASR2S license: https://github.com/FireRedTeam/FireRedASR2S/blob/main/LICENSE
- FunASR model license: https://github.com/modelscope/FunASR/blob/main/MODEL_LICENSE
- VieNeu-TTS code license: https://github.com/pnnbao97/VieNeu-TTS/blob/main/LICENSE
- VieNeu-TTS v3 Turbo model card: https://huggingface.co/pnnbao-ump/VieNeu-TTS-v3-Turbo
- VieNeu-TTS v2 voice metadata: https://huggingface.co/pnnbao-ump/VieNeu-TTS-v2/blob/main/voices.json
- `python-audio-separator` license: https://github.com/nomadkaraoke/python-audio-separator/blob/main/LICENSE
- Demucs maintained-fork license: https://github.com/adefossez/demucs/blob/main/LICENSE
- PaddleOCR license: https://github.com/PaddlePaddle/PaddleOCR/blob/main/LICENSE
- video-subtitle-extractor license: https://github.com/YaoFANGUK/video-subtitle-extractor/blob/main/LICENSE
- video-subtitle-remover license: https://github.com/YaoFANGUK/video-subtitle-remover/blob/main/LICENSE
- STTN license: https://github.com/researchmm/STTN/blob/master/LICENSE
- LaMa license: https://github.com/advimman/lama/blob/main/LICENSE
- ProPainter license: https://github.com/sczhou/ProPainter/blob/main/LICENSE
- libass license: https://github.com/libass/libass/blob/master/COPYING
- CapCut-TTS client license: https://github.com/kuwacom/CapCut-TTS/blob/main/LICENSE
- CapCut Mate license: https://github.com/Hommy-master/capcut-mate/blob/main/LICENSE
- VectCutAPI license: https://github.com/sun-guannan/VectCutAPI/blob/main/LICENSE

## Audit boundary

This audit does not claim that an upstream project's description of its training data independently proves redistribution/commercial rights to every training sample. Where no separate data license was verified, `DATA_LICENSE` remains unresolved/declared-provenance-only and must not be collapsed into the code/model license.

Likewise, an operator's entitlement to transform and publish a particular Douyin video is source-specific. The implementation can enforce an attestation/policy workflow, but it cannot infer those rights merely from a public URL.