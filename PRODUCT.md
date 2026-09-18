# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Solo creator and local video operator translating high volumes of short-form Chinese videos (Douyin) into Vietnamese and English (VI/EN) for distribution on TikTok, Facebook Reels, and YouTube Shorts. Operates from a local GPU workstation with single active slot execution, stepping in only when the automated pipeline encounters flagged quality, timing, or visual exceptions.

## Product Purpose

Douyinie is an automated, policy-gated video localization pipeline that transforms Chinese short-form videos into natural, timeline-constrained localized versions in Vietnamese and English (VI/EN). Success means unattended end-to-end execution producing production-ready videos with natural cadence, intact original soundtrack (BGM/SFX/singing preserved), and clean in-place visual text localization, surfacing only actionable exceptions in a dedicated review workspace.

## Positioning

Unlike generic auto-subtitlers or blunt voice-over tools that wipe background audio and destroy source timing:
- **Immutable Timeline & Cuts**: Zero retiming, stretching, or cutting of the original video.
- **Stem Remix & Selective Dialogue Replacement**: Separates vocals from background audio; suppresses only spoken dialogue inside speech windows while keeping BGM, Foley effects, ambient sound, and singing intact.
- **Fit-Content Cover & Scene-Aware Non-Occlusion**: Subtitle backgrounds hug text tightly without full-width blocking rectangles; semantic text and tutorial UI buttons are covered and localized in-place without obscuring critical demonstration areas.
- **Meaning-First Gatekeeping**: Production translation is remote-LLM only (Gemini 3.8 Flash primary -> DeepSeek V4.1 Flash secondary) enforced by deterministic QA gates checking negation polarity, numeric precision, and entity preservation.

## Operating Context

- **Hardware & Runtime**: Local Windows 11 workstation with NVIDIA GPU (e.g. RTX 2060 SUPER), running a Go RuntimeHost orchestrator and specialized Python 3.10/3.11 StageWorkers (ASR, Forced Aligner, CAM++ Diarizer, ZeroTTS/CosyVoice, Demucs/UVR, PaddleOCR, YAMNet).
- **Interface**: Local browser-based Operator Web Workspace (`http://127.0.0.1:8080/ui/`) featuring Job Queue, Stage-by-Stage Workflow Stepper, Review Workspace with interactive video player, Text Region Inspector, Voice Audition, and Final Render Gate.
- **Execution Model**: Single active processing slot per machine; unattended by default, stopping at `review_required` only when exceptions are flagged.

## Capabilities and Constraints

- **Confirmed Capabilities**:
  - Ingest and extraction of Douyin short-form video assets.
  - Audio role separation (`narration/dialogue`, `singing/music-vocal`, `instrumental/background`, `ambience/SFX`).
  - Speech understanding: ASR, forced word-level alignment, speaker diarization.
  - Remote-only production translation (Gemini 3.8 Flash primary, DeepSeek V4.1 Flash secondary) with strict negation/numeric/entity QA.
  - Speech fit adaptation: shorten-first text adaptation with zero adjacent overrun (`tts_finish <= source_end`) and natural breathing pauses.
  - Voice assignment with AI recommendation and standalone (~5s) or contextual (~10s) voice audition.
  - OCR text region detection, classification (`speech_subtitle`, `semantic_text`, `instructional_ui_text`, `brand_keep`, `ignore/noise`), tracking, and translation.
  - Direct operator overrides in Review Workspace: drag/resize/reclassify text regions, edit translation text, swap voices.
  - Video composition: compact subtitle rendering, in-place covers, audio remixing, and FFmpeg final export.
- **Constraints & Non-Goals**:
  - Out of scope for V1: Facial lip synchronization (LatentSync, MuseTalk) and arbitrary video editing/timeline restructuring.
  - Compute partitioning: GPU/CPU compute reserved for local media stages; local LLM translation is banned for production runs.
  - Single active processing slot: queue handles serial execution to prevent GPU VRAM exhaustion.

## Brand Commitments

- **Name**: Douyinie.
- **Identity & Tone**: Local production workspace, utility-first, high density, dark-mode focused (`color-scheme: dark`, electric lime accent `#e7ff57`, slate/charcoal backgrounds), precision-crafted for rapid keyboard/mouse operator triage.

## Evidence on Hand

- **Architecture & Invariants**: `docs/architecture/phase1-architecture.md`, `CONTEXT.md`, `docs/agents/domain.md`, Wayfinder Issue #1, Implementation Spec #18.
- **Live E2E Runbook & Real Media Corpus**: `docs/agents/live-runtime-e2e.md`, verified real Douyin runs on test assets (e.g. 4.mp4, 5.mp4).
- **Architectural Seams**: Seam 1 (RuntimeHost localhost HTTP API) and Seam 2 (StageWorker NDJSON subprocess protocol), with complete test suites in `test/seam1/` and `test/seam2/`.
- **Quality Benchmarks**: Quality corpus and benchmark suites in `internal/benchmark/` and `test/seam1/`.

## Product Principles

1. **Respect the Source Timeline**: The original video cadence and cuts are immutable anchors; dubbing and subtitles must adapt to the video, never force the video to stretch or retime.
2. **Preserve Audio Identity**: Never replace an entire soundtrack; isolate and replace dialogue while keeping original BGM, sound effects, and musical performances intact.
3. **Fit-Content & Scene Awareness**: Localized text must cover only what is necessary, hugging text tightly and never blinding the viewer to critical UI buttons, tap targets, or visual demonstration.
4. **Exception-Only Human Triage**: Automate everything that meets confidence thresholds; respect the operator's time by surfacing only genuine ambiguities, timing conflicts, or quality risks.
5. **Fail Closed on Meaning Drift**: Translations that invert negation, drop entities, or distort numerical facts must fail closed or require review rather than shipping silent hallucinations.

## Core Product Invariants

1. **Dialogue & Narration Dubbing Only**:
   - Dub only spoken dialogue, narration, and monologue requiring translation.
   - Do **not** replace the full soundtrack with TTS.
   - Preserve background music (BGM), sound effects (Foley/ambient/touch SFX), singing/music-vocals, and instrumental outro showcase sections perceptually and semantically outside and through dialogue replacement as separation/mix permits.
   - Inside aligned source speech windows, suppress source dialogue using the separated background stem combined with the localized dub; full-track TTS replacement is prohibited.

2. **Immutable Source-Timing Anchors**:
   - Source video timeline and visual cuts remain immutable; do not stretch, shrink, or retime the video.
   - Spoken adaptation must **shorten/rewrite concise target text first** when source speech cadence is brisk.
   - Probe actual synthesized audio duration; enforce zero adjacent speech overrun (`tts_finish <= immutable source window end`).
   - Maintain perceptible natural breathing pauses between speech turns to prevent run-on or glued delivery.
   - Source-relative cadence and pause spacing are evaluated via perceptual audiovisual quality gates, not arithmetic duration alone.

3. **Multi-Role Visual Text Localization (`TextRegionPlan`)**:
   - Classify on-screen text into first-class roles:
     - `speech_subtitle`: Spoken dialogue/narration captions.
     - `semantic_text`: Step labels, ingredients, key callouts, and floating titles necessary to understand video content.
     - `instructional_ui_text`: Software menus, button labels, and controls actively taught in tutorial videos (e.g. CapCut/JianYing tools).
     - `brand_keep`: Non-instructional manufacturer logos, packaging brand marks, and decorative icons.
     - `ignore/noise`: Watermarks, compression artifacts, and decorative non-content text.
   - **Deterministic In-Place Cover/Overlay Default**: Inpainting is non-default; meaningful Chinese text is covered and replaced in-place.
   - **Compact Fit-Content Subtitle Presentation**: Subtitle backgrounds must hug the rendered text tightly (1–2 lines max, ~18–28px horizontal padding, ~10–16px vertical padding), never a full-width destructive rectangle. Minor source glyph edge peeking is acceptable when necessary for composition and non-occlusion.
   - **Scene-Aware Non-Occlusion**: Subtitles and overlays must never obscure tutorial buttons, timeline tracks, sliders, finger/cursor tap targets, or active demonstration areas.

4. **Pre-Dub Voice Audition & Selection**:
   - AI Recommended voice assigned by default per speaker.
   - Operator can preview ~5s standalone audio or ~10s contextual audio (translated line mixed with actual video BGM/SFX).
   - One stable voice assigned per speaker per run (no sentence-by-sentence engine hopping).
   - Videos with no spoken dialogue display `No dubbing required` and bypass voice audition.

5. **API-First Production Translation; Local Media Processing**:
   - Production VI/EN translation uses the authorized remote gateway: `gemini-3.8-flash` first, then `deepseek-v4.1-flash`.
   - Local LLM translation is not a production requirement and is not a fallback. If the remote translation lanes cannot produce an acceptable result, fail closed or request review instead of silently lowering model quality.
   - The target desktop's limited compute budget is reserved for specialized local media work: ASR/alignment/diarization, voice/TTS, separation, OCR/tracking, mixing, and render.
   - Local OCR/tracking remains valid; detected visual text is translated through the same remote translation policy.

6. **Exception-Only Human Review**:
   - The pipeline runs unattended; passing and auto-resolved segments bypass human intervention.
   - The review queue surfaces only actionable exceptions (tight timing, uncertain text roles, occlusion risks, or pronunciation anomalies).
   - Direct-manipulation operator override (drag/resize/reclassify text regions, tweak translations, adjust voice).

7. **Out of Scope for V1**:
   - Facial lip synchronization (LatentSync, MuseTalk) is explicitly out of V1 scope.
   - Arbitrary free-form video editing and timeline restructuring are out of scope.

## Source of Truth Hierarchy

1. **[Wayfinder Issue #1](https://github.com/monet88/douyinie/issues/1)**: Authoritative architectural and product decisions.
2. **[Implementation Spec #18](https://github.com/monet88/douyinie/issues/18)**: Authoritative implementation spec, including normative revalidation amendments.
3. **[`docs/architecture/phase1-architecture.md`](docs/architecture/phase1-architecture.md)**: Canonical repo-local Phase 1 architecture synthesis of the locked #16 architecture plus authoritative #18 amendments.
4. **`PRODUCT.md` & `CONTEXT.md`**: Repo-local product charter and domain invariants.
5. **`docs/reference-repositories.md`**: External mining inventory (not a dependency lockfile).
