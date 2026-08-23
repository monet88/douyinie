# Douyinie — Product Charter

## Purpose & Vision

Douyinie is an automated, policy-gated video localization pipeline that transforms Chinese short-form videos (Douyin) into natural, timeline-constrained localized versions in **Vietnamese and English (VI/EN)**.

The system is designed for unattended execution by default, surfacing only genuine low-confidence, timing, cover, or policy exceptions to a human operator via an exception-only review queue.

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

5. **Exception-Only Human Review**:
   - The pipeline runs unattended; passing and auto-resolved segments bypass human intervention.
   - The review queue surfaces only actionable exceptions (tight timing, uncertain text roles, occlusion risks, or pronunciation anomalies).
   - Direct-manipulation operator override (drag/resize/reclassify text regions, tweak translations, adjust voice).

6. **Out of Scope for V1**:
   - Facial lip synchronization (LatentSync, MuseTalk) is explicitly out of V1 scope.
   - Arbitrary free-form video editing and timeline restructuring are out of scope.

## Source of Truth Hierarchy

1. **[Wayfinder Issue #1](https://github.com/monet88/douyinie/issues/1)**: Authoritative architectural and product decisions.
2. **[Implementation Spec #18](https://github.com/monet88/douyinie/issues/18)**: Authoritative implementation spec, including normative revalidation amendments.
3. **`PRODUCT.md` & `CONTEXT.md`**: Repo-local product charter and domain invariants.
4. **`docs/reference-repositories.md`**: External mining inventory (not a dependency lockfile).
