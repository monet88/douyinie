# Phase 1 E2E Live Acceptance Benchmark Suite

**Date:** 2026-08-23
**Status:** 5/5 final variants passed recorded AV QA; operator feedback incorporated
**Test Harness Location:** `.ref/_live-tests/e2e-acceptance-20260823/` (Git-ignored)

---

## Executive Summary

An empirical end-to-end acceptance benchmark was executed across five representative Douyin short videos, testing Phase 1 localization behavior: audio-role analysis and separation, transcription, concise translation, timeline-constrained TTS dubbing, multi-role visual text localization, and multimodal audiovisual quality inspection.

The test fixtures demonstrated that:
1. **Dialogue-only dubbing** preserves the vitality of video soundtracks, retaining background music, cartoon sound effects, and Foley ambience (e.g. water drops, frying pans, knife cuts) outside and through dialogue replacement.
2. **Deterministic in-place overlay** (`TextRegionPlan`) cleanly covers and localizes Chinese dialogue, recipe steps, and instructional UI buttons without requiring complex inpainting on the common path.
3. **Compact fit-content subtitle presentation** (V3) ensures that subtitles hug the rendered text tightly, keeping timeline tracks, finger tap targets, and UI controls unobstructed.
4. **Source-relative cadence alignment** (V2/V3) with concise rewriting, measured-duration fitting, and perceptual pacing checks satisfies zero speech overrun (`tts_finish <= source_end`) and maintains perceptible natural pauses.

---

## Benchmark Fixture Results

| # | Fixture & Category | Resolution & Duration | Pipeline & Audio Configuration | Visual Text Strategy | Multimodal QA Verdict |
|---|---|---|---|---|:---:|
| **1** | **Matcha Melon Ice Recipe** (`1.mp4`) | 1080x1920 (23.99s) | Visual-only localization (Original AAC passthrough; 0 spoken lines) | Solid in-place cover (top disclaimer, step badges, title hashtag) | **PASS** (0.78) |
| **2** | **Flower Care & Revival Vlog** (`2.mp4`) | 1080x1920 (42.80s) | Lifestyle narration dubbing + vocal separation | Earlier bottom subtitle cover (later superseded by compact fit-content policy) + 7 floating in-place callouts | **PASS** (0.72) |
| **3** | **Dragon Fruit Baby Snack Drops** (`3.mp4`) | 1080x1920 (17.07s) | 15 micro-slot dubbing + cartoon SFX/BGM (Primary dub accepted; NO-DUB preserved for A/B reference) | 15 top step badges + packaging caution badge + final sticker | **PASS** (0.78) |
| **4** | **CapCut Transition Tutorial** (`4.mp4`) | 1080x1920 (33.14s) | Tech tutorial dubbing + pure music showcase outro (27.2s - 33.11s) | **V3 Compact Subtitle Box** (fit-content, padding 18px) + UI controls (`Xuất`, `Lớp phủ`, `Keyframe`, `Độ mờ`) | **PASS** (0.72) |
| **5** | **5 Creative Vlog Camera Angles** (`5.mp4`) | 1080x1440 3:4 (27.17s) | Fast-cut lifestyle dubbing + Foley SFX (water plop at 24-26s, frying, milk pour) | Bottom subtitle overlay + brand/object KEEP (`SUPOR`, `Samyang`) | **PASS** (0.72) |

---

## Key Technical Decisions & Observations

### 1. Visual Text Presentation Policy (V1 &rarr; V2 &rarr; V3 Evolution)
- **Earlier Fixtures (V1/V2)**: Initial tests applied continuous full-width background bands. While maximizing source-glyph concealment, this obscured unnecessary visual areas.
- **Global Product Policy (V3)**: The normative policy supersedes full-width bands with a **compact, fit-content background box** that tightly hugs 1–2 line text with ~18–28px horizontal padding. Crucial tutorial buttons, timeline tracks, and finger tap targets remain unobstructed. Slight source glyph edge peeking is acceptable when necessary for aesthetics and non-occlusion.

### 2. Speech Timing & Cadence Observations (Fixture-Specific vs Universal)
Acceptance testing on fast tutorial narration (Video 4) demonstrated that basic duration fitting is insufficient if sentences are dragged out or run together.
- **Universal Contract**: Shorten/rewrite concise target text first; probe actual synthesized duration; enforce zero overrun (`tts_finish <= source_end`); maintain natural pause quality.
- **Video 4 Fixture Measurements**: Video 4 V2/V3 achieved an average fit ratio of **74.1%** across 10 speech windows, zero overrun (maximum overrun 0.000s), and a minimum pause gap of **0.506s** before subsequent actions.

### 3. Audiovisual Multimodal QA Protocol
Acceptance testing utilized native MP4 video+audio inspection via `gemini-3.7-flash-high`, verifying natural pronunciation, Chinese vocal suppression, text elimination, soundtrack preservation, and unobstructed action targets.

*(Note: Model confidence scores serve as automated acceptance signals and do not represent formal release-quality guarantees).*
