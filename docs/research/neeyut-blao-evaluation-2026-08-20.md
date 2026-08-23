# NeeyuBL/neeyut-blao evaluation for Douyinie

## Conclusion

T-blao is worth mining as an architecture and desktop-UX reference, not as a new core model/provider. Its highest-value ideas for Douyinie are the local-first engine lifecycle manager, executable/JSONL worker boundary, deterministic subtitle planning/rendering, and release/runtime hardening.

Do not copy root application code into a commercial Douyinie product without separate permission: the root repository uses PolyForm Noncommercial 1.0.0. The vendored Douyin downloader engine has its own MIT license from jiji262, but Douyinie should prefer the current upstream `jiji262/douyin-downloader` already in the reference inventory.

Inspected repository state: `NeeyuBL/neeyut-blao` `main` at commit `e07d404d3a88582e9ba9abbf6caf0f5795b6e65d` (`release: hotfix T-blao v0.1.19`).

## Findings

### High-value patterns

- **On-demand engine/bootstrap lifecycle.** `src/main/deps.ts` pins heavy app assets to a fixed release tag, installs under Electron `userData`, reports download progress, verifies checksums, probes actual binary capabilities, and keeps rollback paths while replacing managed binaries.
- **Small engine version registry.** `src/main/engines-update.ts` keeps per-engine numeric versions in a remote manifest plus local state, and treats network failure as non-blocking.
- **Typed executable worker boundary.** `src/main/douyin.ts`, `whisper.ts`, and `ocr.ts` spawn external engines instead of importing Python into Electron. Whisper/OCR communicate through JSON-lines events such as `status`, `progress`, `done`, and `error`.
- **Lazy heavy dependency provisioning.** OCR is a separate optional engine; Whisper CUDA libraries are a separate opt-in pack; models live under user data rather than the application bundle.
- **Deterministic subtitle render plan.** `src/shared/subtitleLayout.ts` measures pixel width, uses grapheme-aware character counts, splits overlong cues into render-only segments, preserves total timing, and emits cue-health diagnostics for overflow/line-count/reading-speed problems.
- **Preview/render agreement.** `src/main/subtitlePlanner.ts` makes the main process the glyph-measurement source of truth for preview and ASS output. `burn.ts` sets ASS `PlayResX/PlayResY` to real video dimensions instead of relying on generic `force_style` scaling.
- **Portrait/social subtitle handling.** Separate layout profiles, portrait-specific sizing, actual-font measurement, automatic script-aware font selection, and render-only cue splitting are directly relevant to Douyinie output QC.
- **Subtitle effects with regression tests.** Standard, word-reveal, and word-highlight/pop modes are generated from the same render plan. `scripts/smoke-subtitles.ts` covers Unicode scripts, timing conservation, portrait regressions, overflow, fast CPS, and audio-mix behavior.
- **Translation failure containment.** Gemini API keys use Electron `safeStorage`; requests have explicit timeouts and model fallback; timestamps remain local; structured `{n,t}` results preserve one-to-one subtitle line mapping; missing translations fall back to source text rather than corrupt timing.
- **Site-specific capability policy.** `sitePolicy.ts` probes yt-dlp browser-impersonation support and applies workarounds only to affected sites.
- **Release discipline.** Release scripts validate package/lock/release-note/tag consistency, while runtime dependencies and large engines are versioned separately from the app release.

### Lower incremental value for Douyinie

- The nested `engines/douyin-engine` is the jiji262 Douyin Downloader V2 family. Its `download_manifest.jsonl`, SQLite dedup/incremental logic, retries and browser fallback remain useful, but Douyinie already tracks the current upstream repository and should mine there first.
- The ASR path is faster-whisper/CTranslate2. Douyinie's current Chinese baseline remains Qwen3-ASR + forced alignment, so this is mainly an engine-packaging reference rather than an ASR challenger.
- The OCR engine is region/band OCR that produces subtitle files. It is not a replacement for Douyinie's persistent visual-text track + tracking/propagation architecture for arbitrary scene text.
- Video2X is optional enhancement infrastructure and is not material to Phase 1 localization.
- T-blao does not provide a competitive TTS/voice-cloning or duration-constrained dubbing scheduler.

## Implications for this repo

- Add T-blao to the architecture-reference inventory, explicitly marked **architecture/UX patterns only** because the root code license is noncommercial.
- Mine the engine-manager contract before implementing Douyinie's local provider runtime: `EngineDescriptor`, install/update state, pinned artifact source, checksum, capability probe, rollback, and JSONL worker protocol.
- Mine the subtitle render-plan concepts into Douyinie's caption QC: source cue vs rendered segment, measured-width wrapping, CPS/overflow health, deterministic preview/render parity, and portrait/social profiles.
- Keep TTS, ASR, OCR, downloader, and renderer choices behind Douyinie's existing provider boundaries; T-blao is evidence for the boundary pattern, not a reason to collapse those responsibilities into one desktop app module.

## Open questions

- A local smoke run was not performed in this evaluation; findings are source-code and repository-activity observations, not runtime performance measurements.
- If Douyinie later needs a full Electron desktop distribution, compare T-blao's asset/update scheme with TrackExtract and Douyinie's eventual deployment constraints before selecting a packaging design.
- Commercial reuse of any root T-blao implementation requires license review or permission from NeeyuBL; architecture ideas should be reimplemented independently.

## Sources

- T-blao repository and README — https://github.com/NeeyuBL/neeyut-blao
- T-blao root license (PolyForm Noncommercial 1.0.0) — https://github.com/NeeyuBL/neeyut-blao/blob/main/LICENSE
- Runtime/bootstrap manager — https://github.com/NeeyuBL/neeyut-blao/blob/main/src/main/deps.ts
- Engine version registry — https://github.com/NeeyuBL/neeyut-blao/blob/main/src/main/engines-update.ts
- Douyin shell adapter — https://github.com/NeeyuBL/neeyut-blao/blob/main/src/main/douyin.ts
- Whisper and OCR engine adapters — https://github.com/NeeyuBL/neeyut-blao/blob/main/src/main/whisper.ts ; https://github.com/NeeyuBL/neeyut-blao/blob/main/src/main/ocr.ts
- Subtitle planner/layout/burn — https://github.com/NeeyuBL/neeyut-blao/blob/main/src/main/subtitlePlanner.ts ; https://github.com/NeeyuBL/neeyut-blao/blob/main/src/shared/subtitleLayout.ts ; https://github.com/NeeyuBL/neeyut-blao/blob/main/src/main/burn.ts
- Subtitle smoke tests — https://github.com/NeeyuBL/neeyut-blao/blob/main/scripts/smoke-subtitles.ts
- Gemini subtitle translation — https://github.com/NeeyuBL/neeyut-blao/blob/main/src/main/gemini.ts
- Vendored Douyin engine summary/license — https://github.com/NeeyuBL/neeyut-blao/tree/main/engines/douyin-engine
- Current upstream Douyin Downloader — https://github.com/jiji262/douyin-downloader
