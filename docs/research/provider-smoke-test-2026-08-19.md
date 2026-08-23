# Provider smoke test — custom Gemini, CapCut, and VieNeu-TTS

## Conclusion

The current Phase 1 provider stack is technically viable for a local-first pipeline, with one important CapCut limitation.

- The user-configured Gemini endpoint works through an OpenAI-compatible `/chat/completions` contract, returns valid structured JSON, preserves Chinese input and Vietnamese/English output under PowerShell 7.6.4, and can produce duration-targeted Vietnamese copy.
- The authenticated CapCut Web wrapper successfully establishes a real account session and returns valid TTS audio. However, current live voice-category discovery fails and falls back to a 15-speaker catalog with no Vietnamese voices, so CapCut is not yet a reliable default Vietnamese voice provider.
- VieNeu-TTS v3 Turbo works locally on Windows through the CPU/ONNX path and is fast enough for Phase 1 sequential dubbing. It produced both Vietnamese and English speech substantially faster than real time after model cache warm-up.
- A direct `Gemini vi_short -> VieNeu` integration smoke test succeeded and produced 3.36 s speech for a 4.0 s requested duration target, demonstrating the planned translate/rewrite/measure loop.

No API keys, passwords, account identifiers, cookies, or session tokens are recorded in this note.

## Tested revisions

- `kuwacom/CapCut-TTS`: `d5be4e1e3751052cc3a406a7f79fa0a31d6606fe`
- `K07VN/capcut-tts-api`: `e06da1f4e0c0010354f4e7702f02c18cbdd419a2`
- `pnnbao97/VieNeu-TTS`: `149ff16a6a50093a0cad1b75d5edf9e9d81d97f4`

## Findings — custom Gemini endpoint

- Local provider config uses protocol `openai` and a custom base URL; the requested model alias is `gemini-3.7-flash-high`.
- Raw-byte POST to `/chat/completions` succeeded in about 4.6 s, returned one valid choice, and the response identified the effective model as `gemini-3.7-flash`.
- The response body parsed as valid compact JSON with `vi`, `en`, and `vi_short` fields.
- Chinese source text was understood correctly and both Vietnamese and English translations were semantically correct in the smoke sample.
- Windows PowerShell 5.1 `Invoke-RestMethod` mis-decoded the UTF-8 response in the first harness. Re-running the same provider test under PowerShell 7.6.4 produced correct Vietnamese directly with `Invoke-RestMethod`, confirming the proxy response itself is valid UTF-8.
- Local Windows automation should standardize on PowerShell 7 (`pwsh`). Provider adapters should still make UTF-8 request/response handling explicit rather than depending on shell defaults.

## Findings — CapCut

- `CapCut-TTS` dependencies installed successfully and `npm run typecheck` plus `npm run build` completed successfully.
- With the user-authorized account credentials loaded only from local DPAPI storage, the wrapper started on localhost and established an authenticated CapCut Web session.
- Live CapCut bundle extraction succeeded and discovered current editor/login configuration.
- Live voice-category fetches failed for the tested category IDs. `/v2/speakers` therefore returned 15 fallback speakers whose reported languages were `en`, `es`, `ja`, and `jp`; no Vietnamese speaker was available through this live catalog path.
- `/v2/synthesize` still succeeded through the authenticated session using a fallback speaker and Vietnamese test text: HTTP 200, `audio/mpeg`, 42,477 bytes, about 6.3 s request latency.
- The resulting MP3 is valid: 24 kHz mono, duration 5.304 s, ~64 kbps, SHA-256 `C98B0449F5D0B37E2B426B3F7F5D7928EBD1005B729D43A8C4E7DEDD1A864E27`.
- The account subscription tier was not inspected, so this test does not establish whether a specific voice requires Free, Pro, or credits.
- `capcut-tts-api` independently exposes a static Vietnamese voice catalog, but its account-less/private-protocol path was not used as the production authorization basis in this test.

## Findings — VieNeu-TTS

- Local machine: NVIDIA RTX 2060 SUPER 8 GB is present, but the first smoke path intentionally used CPU/ONNX because upstream recommends CPU for short interactive synthesis and requires a newer CUDA stack for its GPU path.
- `uv sync` completed successfully for the torch-free CPU/ONNX environment.
- VieNeu v3 Turbo preset synthesis succeeded at 48 kHz mono PCM.
- Warm local Vietnamese baseline: 3.440 s audio generated in 1.149 s, RTF 0.334.
- Raw Gemini `vi_short` -> VieNeu preset voice: 3.360 s audio generated in 1.443 s, RTF 0.429, absolute error 0.640 s against a requested 4.000 s target.
- Raw Gemini English -> VieNeu `Adam`: 3.360 s audio generated in 1.039 s, RTF 0.309.
## Implications for Douyinie

- Keep the LLM stage behind a generic provider contract rather than a Google-specific Gemini SDK contract. The tested provider needs `base_url`, `model`, auth-header configuration, and explicit UTF-8 handling. Use PowerShell 7 (`pwsh`) for local Windows provider tooling.
- Treat requested model aliases and effective response model IDs as separate observability fields.
- Use the planned translation loop as `translate -> duration rewrite -> synthesize -> measure -> retry if outside tolerance`; the smoke test proves the first full loop works but does not yet define the final duration tolerance.
- VieNeu is currently the strongest demonstrated default Vietnamese preset-voice backend for Phase 1 because it is local, fast, and does not depend on CapCut catalog availability.
- Keep CapCut behind a provider/policy gate as an optional accelerator. Current authenticated TTS is technically viable, but Vietnamese voice discovery must be repaired or independently validated before CapCut can be selected as the default VI provider.
- Keep provider credentials and generated sessions outside tracked files. All smoke-test credential/session artifacts are under git-ignored `.ref/_live-tests/provider-setup/`.

## Open questions

- What duration error threshold should trigger automatic rewrite versus mild time-stretch? The current sample missed a 4.0 s target by 0.64 s.
- Can the current CapCut Web voice catalog be discovered through a new category/panel path that includes Vietnamese voices, using only the authenticated account path?
- Should the custom Gemini adapter expose the requested model alias, effective returned model, and endpoint fingerprint in each Run for reproducibility?
- VieNeu GPU mode remains untested on the RTX 2060 SUPER; CPU/ONNX is already sufficient for short sequential Phase 1 work.

## Sources

- `kuwacom/CapCut-TTS` — https://github.com/kuwacom/CapCut-TTS
- `K07VN/capcut-tts-api` — https://github.com/K07VN/capcut-tts-api
- `pnnbao97/VieNeu-TTS` — https://github.com/pnnbao97/VieNeu-TTS
- Local empirical smoke-test artifacts — `.ref/_live-tests/provider-setup/` (git-ignored; secrets excluded from this note)


## CapCut Pro retest — 2026-08-19

- A newly supplied CapCut account described by the user as Pro was stored separately through the local DPAPI wizard and tested with a fresh session file; the prior CapCut session was not reused.
- The new account authenticated successfully and established a fresh CapCut Web session. Subscription tier is user-reported; the current wrapper/session data exposes no Pro/subscription entitlement field, so the tier was not independently verified through this API surface.
- Live Web voice-category discovery still failed across the same category IDs. `/v2/speakers` again returned only the 15 fallback voices with languages `en`, `es`, `ja`, and `jp`. Therefore the catalog failure is not explained by the earlier account being non-Pro.
- Web TTS synthesis remained functional on the fresh account. However, the wrapper `speed` control was not reliable for duration targeting: nominal playback rates from 0.1x through 2.0x all produced roughly 9–10 second audio for the same sentence, and intermediate values were not monotonic.
- The separate `K07VN/capcut-tts-api` private protocol successfully created a Vietnamese TTS task with `BV421_vivn_streaming`; the current API returns terminal status `succeed` rather than the client's expected `success`, which explains the earlier 60-second `generate_speech()` timeout.
- Downloading the completed Vietnamese result succeeded: MP3, 24 kHz mono, 2.352 s, 160 kbps, SHA-256 `ADA03C49DCF1253BD7BF851DC758F075CE9B257DCFB8A69B58B1ECE681077638`.
- The private protocol's `rate` parameter also did not control duration in this smoke test: `0.5`, `1.0`, and `1.5` all produced byte-identical 3.600 s files for the same Vietnamese text.
- Conclusion: CapCut can generate Vietnamese speech, but current tested CapCut paths should not own timeline-constrained duration control. Keep CapCut optional; use text rewrite plus post-synthesis measurement/time-stretch or another TTS backend for timing guarantees.

## English TTS mini-benchmark — 2026-08-19

- Test sentence: `This dress is perfect for commuting. It looks flattering and feels comfortable all day.`
- CapCut `Labebe` (`ICL_en_female_jiaoao`): 8.256 s audio, 6.880 s request latency, MP3 24 kHz mono.
- CapCut `Cool Lady` (`ICL_en_female_guanggao`): 5.592 s audio, 5.998 s request latency, MP3 24 kHz mono.
- VieNeu `Adam`: 5.200 s audio, 1.596 s warm inference, RTF 0.307, PCM WAV 48 kHz mono. Model init measured separately at 7.821 s and should be amortized by a resident provider process.
- VieNeu v3 Turbo currently exposes one native English preset (`Adam`), so the fair small comparison is two CapCut English voices versus Adam rather than forcing a Vietnamese preset to read English.
- Voice selection changes duration materially: CapCut Labebe was ~48% longer than Cool Lady for identical text. Therefore provider/voice selection must be measured per output and cannot rely on text length alone.
- Objective result: VieNeu Adam is much faster locally and its duration is close to CapCut Cool Lady. Subjective naturalness/style still requires listening to the three generated files under `.ref/_live-tests/provider-setup/en-bench/`.

## K07VN capcut-tts-api English mini-benchmark — 2026-08-19

- Same English sentence as the prior benchmark: `This dress is perfect for commuting. It looks flattering and feels comfortable all day.`
- K07 voice catalog query for `en-US` returned many English voices, including `BV029_streaming` (American Female), `en_female_sherry` (Sherry), `ICL_en_male_philosopher_dsp` (Narrator), and `en-US-JennyMultilingualNeural` (Jenny).
- `American Female` succeeded: task-to-audio latency 3.102 s; MP3 24 kHz mono; 5.664 s audio; 113,280 bytes; request/audio ratio 0.548.
- `Narrator` succeeded: latency 3.649 s; MP3 24 kHz mono; 5.730 s audio; 46,509 bytes; request/audio ratio 0.637.
- `Sherry` succeeded: latency 1.795 s; MP3 24 kHz mono; 4.872 s audio; 77,997 bytes; request/audio ratio 0.368.
- `Jenny` failed at the task level in this run and should not be selected as a default until independently revalidated.
- For comparison, authenticated CapCut Web `Cool Lady` measured 5.998 s request latency for 5.592 s audio (ratio 1.073), while warm local VieNeu `Adam` measured 1.596 s for 5.200 s audio (RTF 0.307).
- Objective implication: the K07 private TTS path is substantially faster than the tested CapCut Web wrapper and approaches VieNeu warm throughput for some voices. It also exposes a much broader English catalog than the Web wrapper fallback list.
- This K07 path is a private protocol/device-signing reference rather than the authenticated CapCut Pro Web session. Technical success does not by itself establish account entitlement or production-service authorization.
- Subjective naturalness/style still requires listening to generated files under `.ref/_live-tests/provider-setup/en-bench-k07/` and comparing them with the prior `.ref/_live-tests/provider-setup/en-bench/` files.
