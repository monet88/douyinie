# Issue #60 - Production Translation Lane Decision for Local 8 GB + Hybrid

Date: 2026-09-03
Status: RESOLVED DECISION
Parent: https://github.com/monet88/douyinie/issues/53
Issue: https://github.com/monet88/douyinie/issues/60

## Decision

Phase 1.1 uses one required local translation lane and one conditional Hybrid remote fallback. It does not add an unbenchmarked ultralight model or a second cloud provider to the RC hard route.

### Local 8 GB baseline

- Provider ID: `qwen3_4b_translator`.
- Model: `Qwen/Qwen3-4B-GGUF`.
- Repository revision: `bc640142c66e1fdd12af0bd68f40445458f3869b`.
- Artifact: `Qwen3-4B-Q4_K_M.gguf`.
- Artifact SHA-256: `7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5`.
- Model license: Apache-2.0.
- Runtime: `llama.cpp`-compatible local execution, with the exact runtime/package revision bound separately by the Issue #61 runtime-manifest contract.
- Mode: non-thinking translation. Prompt/template, sampling settings, seed if used, context cap, and runtime revision are release evidence and must be pinned by implementation/benchmark work.

Why this model: the official first-party GGUF is substantially smaller than the prior Qwen2.5-7B Q4 candidate, while Qwen documents 100+ languages/dialects and multilingual translation capability. The model card also documents direct `llama.cpp` execution.

The 2.5 GB weight size is not proof of 8 GB GPU safety. Local RC acceptance requires a real RTX 2060 SUPER 8 GB smoke/benchmark measuring peak VRAM with the final runtime and bounded context. If that smoke fails, the Local RC gate fails and this model decision must be revisited; there is no silent lower-quality model fallback in the hard-gate route.

### Hybrid remote fallback

- Provider ID: `deepseek_v4_flash_translator`.
- Service: DeepSeek Open Platform.
- Base URL: `https://api.deepseek.com`.
- Requested model alias: `deepseek-v4-flash`.
- Current documented service version at decision time: `DeepSeek-V4-Flash-0731`.
- Mode: non-thinking (`thinking.type=disabled`).
- Structured output: JSON mode is supported; prompt and request parameters must be pinned as release evidence.

`deepseek-v4-flash` is a rolling service alias, not an immutable model snapshot. DeepSeek documents that the same alias accesses the latest service version. Therefore Hybrid reproducibility uses service evidence, not a fake checkpoint hash.

## Remote service drift contract

For every RC Hybrid benchmark run:

1. Record the requested alias, API contract/base URL, request parameter set, terms/privacy evidence timestamp, and the provider's currently documented service version.
2. Record response `model`, `system_fingerprint`, response/request identifier when exposed, and response timestamp for each provider attempt.
3. Bind remote translation artifacts to an explicit `service_baseline_id` representing that benchmark's observed service baseline.
4. If the documented service version changes, or the service reports a materially different backend identity during the RC benchmark, treat that as provider drift and produce fresh Hybrid benchmark evidence rather than claiming equivalence with the earlier run.
5. Current translation cache lookup occurs before provider invocation and keys provider/model/version. Implementation must ensure reusable remote cache entries cannot hide a service-version change: either include the explicit `service_baseline_id` in the remote cache identity or bypass cross-baseline cache reuse until the current service baseline has been established.

## Privacy, consent, credentials, and obligation evidence

### Local Qwen lane

- `MODEL_LICENSE`: Apache-2.0.
- `CODE_LICENSE`: pin the actual local inference runtime (`llama.cpp` is currently MIT) and Douyinie adapter/runtime identity under the Issue #61 runtime manifest.
- `DATA_LICENSE`: `UPSTREAM_NOT_SEPARATELY_DECLARED` unless a specific upstream training-data license grant is found. Do not copy the model license into the data-license field.
- `SERVICE_TERMS`: local execution; no runtime model service.
- No remote credential is required.

### DeepSeek remote lane

- `MODEL_LICENSE`: remote-service API; no local model-weight license is asserted for the hosted service.
- `DATA_LICENSE`: preserve DeepSeek's published training-data disclosure as evidence; do not invent a redistributable training-data license.
- `SERVICE_TERMS`: DeepSeek Open Platform Terms of Service plus the applicable privacy policy, captured with evidence timestamps.
- Credential: a DeepSeek API-key credential reference resolved through `CredentialService`; never persist the secret in CAS or job artifacts.
- Consent/privacy: outbound transcript text requires explicit operator authorization and the required legal basis/notice for downstream users. Do **not** claim that API prompts are categorically excluded from training. DeepSeek's current privacy/model-method disclosures state that User Input may be used to improve/train technology, with opt-out and de-identification statements applying in their published policies. DeepSeek also states that collected personal data may be processed/stored in the PRC.

If this privacy posture is not accepted, the remote provider is ineligible and Hybrid falls back to the verified local lane only; it must not silently transmit transcripts.

## Profile availability contract

### `local`

- Required: `qwen3_4b_translator` only.
- All remote translation providers are ineligible regardless of credentials or consent.
- Failure of the verified local lane is fail-closed.

The current Router does **not** enforce this hard boundary: execution profile currently affects scoring, while policy/consent/credential checks determine eligibility. A cloud provider can therefore remain eligible under `ExecutionProfileLocal`. The Phase 1.1 implementation spec must add a hard profile/tier eligibility rule before the Local 8 GB release gate is considered valid.

### `hybrid`

- Primary: verified `qwen3_4b_translator` local lane, consistent with the existing cost-first Hybrid preference.
- Conditional fallback: `deepseek_v4_flash_translator`, only with complete service evidence, explicit consent, valid credential authorization, and remote-drift handling.
- No additional unbenchmarked model/API fallback is part of the required RC route.

### `cloud`

Cloud is not a Phase 1.1 hard gate. DeepSeek may be exercised as a conditional capability/smoke provider when authorized. A local GGUF is not required for a cloud-only environment.

## Translation behavior contract

The provider implementation must preserve the locked Phase 1 translation architecture:

1. Meaning-first translation remains separate from spoken-duration adaptation.
2. The provider accepts the ordered segment context needed for coherent VI/EN translation and returns structured segment output without changing source timing identity.
3. Existing QA remains authoritative for fact/name/number/negation preservation; model capability claims do not bypass `MeaningFirstQAGate`.
4. Shorten-first spoken adaptation remains downstream in `SpokenScriptAdapter`; the translation provider must not pre-emptively compress meaning to fit TTS timing.

## Resource placement

- Local Qwen is a model-backed StageWorker translation family and participates in the existing single-GPU lease/supervisor contract. The final implementation must prove clean handoff/reclamation on the real 8 GB machine.
- DeepSeek is a remote I/O provider and does not require a GPU lease.
- No new testing seam is introduced.

## Empirical gates before RC

The decision is implementation-ready, but release acceptance still requires evidence:

- Real Local 8 GB load + translation smoke using the exact Q4_K_M artifact and pinned runtime.
- Peak VRAM and context-cap measurement on the target 8 GB GPU.
- VI/EN corpus quality through the existing meaning-first QA and human-review protocol.
- Hybrid service-baseline/drift evidence and privacy/consent/credential evidence.
- Profile hard-gating verification proving `local` cannot route to a remote translation provider.

## Implementation boundaries

- Issue #61 owns executable local snapshot/runtime identity and byte verification.
- Issue #62 owns the broader production adapter compatibility contract; translation implementation generated after Wayfinder must use the same Seam 2 verified-local-path rules.
- Issue #58 owns final RC evidence packaging and release verdict.
- The post-Wayfinder implementation spec must include the newly verified Router gap: execution-profile tier is currently score-only and must become a hard eligibility boundary for the Local route.

## Primary evidence

External:

- Qwen3-4B-GGUF: https://huggingface.co/Qwen/Qwen3-4B-GGUF
- Pinned Q4_K_M artifact: https://huggingface.co/Qwen/Qwen3-4B-GGUF/blob/bc640142c66e1fdd12af0bd68f40445458f3869b/Qwen3-4B-Q4_K_M.gguf
- Qwen3 release: https://qwenlm.github.io/blog/qwen3/
- DeepSeek current models/pricing: https://api-docs.deepseek.com/quick_start/pricing/
- DeepSeek changelog: https://api-docs.deepseek.com/updates/
- DeepSeek Chat Completions schema: https://api-docs.deepseek.com/api/create-chat-completion/
- DeepSeek thinking-mode contract: https://api-docs.deepseek.com/guides/thinking_mode/
- DeepSeek Open Platform Terms: https://cdn.deepseek.com/policies/en-US/deepseek-open-platform-terms-of-service.html
- DeepSeek Privacy Policy: https://cdn.deepseek.com/policies/en-US/deepseek-privacy-policy.html
- DeepSeek model/training disclosure: https://cdn.deepseek.com/policies/en-US/model-algorithm-disclosure.html

Repo-local:

- `internal/provider/translation.go`
- `internal/service/translation.go`
- `internal/service/translation_qa.go`
- `internal/provider/router.go`
- `internal/domain/governance.go`
- `internal/worker/lease.go`
