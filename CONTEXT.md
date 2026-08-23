# Douyinie — Domain Context & Invariant Glossary

> [!IMPORTANT]
> This document defines the canonical domain vocabulary and invariant contracts for Douyinie Phase 1.
> If this document ever drifts from **[Implementation Spec #18](https://github.com/monet88/douyinie/issues/18)** (including normative amendment comments) or **[Wayfinder #1](https://github.com/monet88/douyinie/issues/1)**, the GitHub issues remain authoritative.

---

## Domain Concepts & Data Contracts

### 1. `AudioRolePlan`
Classifies audio content across the source timeline into distinct operational branches:
- `narration/dialogue`: Spoken speech requiring translation and TTS dubbing.
- `singing/music-vocal`: Sung vocals or musical lyrics; ASR-positive singing is triage only and does **not** enter the dubbing branch, remaining preserved in the soundtrack.
- `instrumental/background`: Pure background music, demo showcases, and outros; 0% TTS injection.
- `ambience/SFX`: Environmental sounds and Foley effects (e.g. cutting, water pouring/drops, sizzling pans, UI taps); preserved as separation/mix permits.

### 2. `TextRegionPlan`
Source-derived spatial and temporal tracking plan for on-screen text with first-class roles:
- `speech_subtitle`: Dialogue captions. Replaced with compact fit-content subtitle box.
- `semantic_text`: Content-critical visual text (recipe steps, ingredient names, key labels, floating titles). Covered and localized in-place.
- `instructional_ui_text`: Application UI buttons, menus, and controls taught in tutorials (e.g. CapCut/JianYing buttons). Covered and localized in-place using standard target-language terminology.
- `brand_keep`: Non-instructional brand marks, manufacturer logos, and prop packaging; preserved for visual authenticity.
- `ignore/noise`: Incidental text, watermarks, compression noise; no action required.

### 3. `VoiceAssignment` & `VoiceAudition`
- `VoiceProfile`: Provider-agnostic target-language voice identity descriptor. Provider/engine selection is routing metadata managed under policy and license constraints.
- `VoiceAssignment`: Frozen mapping of `(speaker_id, target_language) -> VoiceProfile` assigned before full render. Mixing engines sentence-by-sentence for a single speaker is prohibited.
- `VoiceAudition`: Pre-dub operator control allowing:
  - **Standalone Audition**: ~5s isolated voice sample.
  - **Contextual Audition**: ~10s preview synthesizing an actual translated segment from the video mixed with the preserved background soundtrack.

### 4. `DubbingFitPlan`
Calculates cadence and duration adaptation for each speech segment:
- **Immutable Source Window**: Segment start/end anchors are strictly locked to source speech boundaries.
- **Shorten First**: Translation must adapt text length to match source speaking tempo before applying audio speed adjustments.
- **Zero Overrun**: Probe actual synthesized duration; enforce `tts_finish <= source_end` (no adjacent speech overrun).
- **Natural Breathing Room**: Maintain perceptible inter-turn pauses to prevent adjacent sentences from running together.

### 5. `SoundtrackPreservationPlan`
- **Dialogue-Only Suppression**: Outside active source speech windows, the original soundtrack is preserved as separation/mix permits.
- **Stem Remix**: Inside active speech windows, source dialogue is replaced by combining the isolated background stem with the localized TTS dub.
- **Full Mix Replacement Prohibited**: Replacing the entire audio track with TTS or separated instrumental stems is banned as a normal operating mode.

### 6. `RenderPlan`, `QualityResult` & `ReviewItem`
- `RenderPlan`: Complete execution specification combining localized video stream (in-place overlays and compact subtitles) and final mixed audio stream.
- `Compact Subtitle Presentation`: Background box hugs the rendered text tightly (1–2 lines max, ~18–28px horizontal padding, ~10–16px vertical padding), scene-aware, leaving UI controls, timeline tracks, and finger tap targets unobstructed.
- `QualityResult`: Multimodal audiovisual QC report (evaluating naturalness, soundtrack preservation, text elimination, and synchronization).
- `ReviewItem`: Actionable exception item surfaced in the operator review queue when automated quality gates flag low confidence, tight timing, or occlusion risks.

---

## Approved Testing Seams

Phase 1 maintains **exactly two approved testing seams**:

1. **Seam 1 — RuntimeHost Localhost API Contract**:
   Acceptance seam testing end-to-end pipeline execution over versioned localhost API endpoints (audio routing, `TextRegionPlan`, compact overlays, voice audition, zero-overrun timing, and exception queues).
2. **Seam 2 — StageWorker Runtime Contract**:
   Worker protocol seam testing subprocess NDJSON message interchange, lifecycle states, cancel/heartbeat handling, and process-tree termination.
