# Douyinie — Domain Context & Invariant Glossary

> [!IMPORTANT]
> This document defines the canonical domain vocabulary and invariant contracts for Douyinie Phase 1. Detailed system architecture is materialized in the generated diagram set **[`docs/diagrams/douyinie-architecture.json`](docs/diagrams/douyinie-architecture.json)**.
> If this document or the architecture document ever drifts from **[Implementation Spec #18](https://github.com/monet88/douyinie/issues/18)** (including normative amendment comments) or **[Wayfinder #1](https://github.com/monet88/douyinie/issues/1)**, the GitHub issues remain authoritative.

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
- **Playback Window**: Probe actual synthesized waveform duration. Source start/end anchors stay immutable; bounded borrowing may use only proven source silence before the next canonical speech/vocal boundary, with a frozen reserve. The decoded waveform must end at or before `DubPlaybackEndMs` and never collide with adjacent localized speech.
- **Natural Breathing Room**: Maintain perceptible inter-turn pauses to prevent adjacent sentences from running together.

### 4.1 Production Translation Routing
- `TranslationVariant` generation for spoken VI/EN text and translatable `TextRegionPlan` regions is **remote-only in production**.
- Provider order is `gemini-3.8-flash` first, then `deepseek-v4.1-flash`, through the authorized OpenAI-compatible gateway.
- Local model-backed translation adapters may remain for diagnostics, historical evidence, or isolated adapter tests, but `Router` must not select them for production translation and they are never a fallback after remote failure.
- If both remote translation lanes are unavailable, unauthorized, policy-ineligible, or rejected by meaning-first QA, translation fails closed and may surface review; the system must not silently degrade to a weaker local LLM.
- `ExecutionProfileLocal` is therefore **not an end-to-end localization profile**. It is used to verify local media stages such as ASR/alignment/diarization, TTS, separation, OCR/tracking, mixing, and render without requiring a local general-purpose translation model.

### 4.2 `DubTempoCandidate` (review-only tempo alternative)
- `DubTempoCandidate`: The single FFmpeg `atempo` alternative derived for a speech unit whose accepted playback window is still overrun after the natural remedies (shorten, bounded regroup, whole-speaker escalation) are exhausted. It is audition evidence, never an automatic decision: the unit stays `REVIEW_REQUIRED`, the candidate stays unselected, and no stage consumes it as output unless the operator explicitly accepts that exact reviewed candidate (§4.3).
- **Canonical Pass**: The run's dub-variant row - what the inspector, the review projection and playback resolve - belongs to the pass of the voice assignment actually in force. A pass synthesized under an assignment superseded while its request was in flight is committed and readable by hash and returned as review evidence, but it never claims that row and derives no candidate: the candidate belongs to the current assignment's own pass, derived from its own retained waveform.
- **Factor Window**: The factor is the unit's own retained waveform frame extent resampled to the mixer output rate over its accepted playback window. A transform exists only for `1 < factor <= 1.25`; outside that window no DSP is generated and the recorded outcome keeps the overrun in review.
- **No Crop**: The transform is one `atempo` filter with finite numeric arguments and a bounded temporary output. The result is never trimmed, retimed to force a fit, or discarded for still overrunning - an overlong transform is reported as an unselectable candidate.
- **Evidence, Not Output**: Generation and playback never select the alternative on their own: it enters the mix or a final render only through the operator's exact-candidate acceptance (§4.3), and never invokes a provider. The operator auditions the retained natural waveform against it through the run-scoped media endpoint (`GET /api/v1/runs/{id}/dub-media/{hash}`), which serves only hashes that run's own dubbing variant owns - including the retained natural parent and the selected waveform after an acceptance.

### 4.3 `AcceptedReviewCandidate` (exact reviewed-candidate selection)
- **Exact Act, Never a Blanket Override**: An acceptance names one unresolved review unit and the one waveform the operator selected, and is proven against the run's current dubbing artifact, its pinned translation/dub contract and voice assignment, the governing fit policy, the unit's canonical source membership and the named waveform's actual re-probed bytes. Every mismatch is a refusal - a superseded artifact, contract or parameter is `409`, a timing or media gate is `422`, a malformed request or missing waiver is `400` - never a silent downgrade.
- **Hard Gates Are Never Waived**: timing, missing media, wrong ownership, invalid lineage and incomplete required coverage are not overridable. A transformed waveform requires an explicit `manual_override` quality waiver and is recorded as `ReviewOverrideActionReviewedCandidate`, never as an automatic PASS; a generic audit-only override can never stand in for an acceptance.
- **Evidence Extends One Family**: The acceptance is appended to the successor `DubSegmentsVariant` (schema 7) with the append-only `ReviewOverride` id and the immutable candidate hashes. The original waveform, the QA `QualityResult` rows, the `ProviderAttempt` rows, the source anchors and the run's audit history stay untouched, and the operator's note lives in the override row rather than in the artifact.
- **Per-Group Coverage, Never Partial Mixing**: A run with any unresolved unit stays paused and mixes nothing; the affected descendants (mix, render plan, preview) are rebuilt from the accepted waveforms only once the last required group is accepted, and the preview/final the replaced delivery had made current are withdrawn before the rebuild so a failure mid-way cannot leave superseded media servable. The run's own posture then decides between an automatic final render and the explicit `start_final_render`.
- **Idempotent and Never Transferable**: A repeated identical selection returns the same successor with no second audit row; a changed parameter, a cross-run item, a superseded artifact or a concurrent acceptance fails closed, and an interrupted rebuild is resumed by the operator's retry instead of being reported as delivered.

### 5. `SoundtrackPreservationPlan`
- **Dialogue-Only Suppression**: Outside active source speech windows, the original soundtrack is preserved as separation/mix permits.
- **Stem Remix**: Inside active speech windows, source dialogue is replaced by combining the isolated background stem with the localized TTS dub.
- **Full Mix Replacement Prohibited**: Replacing the entire audio track with TTS or separated instrumental stems is banned as a normal operating mode.

### 6. `RenderPlan`, `QualityResult` & `ReviewItem`
- `RenderPlan`: Complete execution specification combining localized video stream (in-place overlays and compact subtitles) and final mixed audio stream.
- `Compact Subtitle Presentation`: Background box hugs the rendered text tightly (1–2 lines max, ~18–28px horizontal padding, ~10–16px vertical padding), scene-aware, leaving UI controls, timeline tracks, and finger tap targets unobstructed.
- `QualityResult`: Multimodal audiovisual QC report (evaluating naturalness, soundtrack preservation, text elimination, and synchronization).
- `ReviewItem`: Actionable exception item surfaced in the operator review queue when automated quality gates flag low confidence, tight timing, or occlusion risks.

### 7. Douyin Discovery & Followed Creators
- `DouyinCreator`: A creator identity on Douyin, canonically identified by stable `sec_uid` independent of the provider used to observe it.
  _Avoid_: treating display name, profile URL, or provider-specific cursor as creator identity.
- `DiscoveredVideo`: A Douyin video observation canonically identified by `aweme_id`; title, cover, like count, publish time, and other display metadata may change between observations without changing identity.
  _Avoid_: treating signed CDN URLs or provider-specific result objects as durable video identity.
- `FollowedCreator`: A `DouyinCreator` the operator has chosen to monitor for subsequently discovered videos.
  _Avoid_: using “follow” to mean an action on the operator's actual Douyin social account.
- `FollowBaseline`: The bounded set of already-existing videos observed when a creator is first followed; baseline videos are historical context, not `New`. Older history may be loaded explicitly without redefining the baseline.
- `DiscoveryDisposition`: The operator-facing state of a discovered video: `New`, `Seen`, or `Ignored`. Opening/detailing or explicitly marking a `New` video makes it `Seen`; `Ignored` is durable and reversible.
- `DownloadRequest`: An explicit request to acquire a discovered video through the existing acquisition boundary. Requesting download also makes a `New` video `Seen`; download state is not a replacement for `DiscoveryDisposition`.

### 8. Workstation Libraries
- `MediaLibraryEntry`: The operator-facing view of one requested or acquired Douyin source, combining its stable source identity, a browsing metadata snapshot, acquisition state, and any localization state derived from the existing production records.
  _Avoid_: treating it as a second media asset independent from `SourceAsset`.
- `RenderLibraryEntry`: The operator-facing view of a final localized render associated with its source, job, run, target language, and immutable render evidence.
  _Avoid_: treating preview renders as finished library outputs.
- `LibraryRemoval`: Removing an entry from normal library browsing without implying destruction of immutable source/run evidence. A separate purge may physically reclaim unreferenced media when no production evidence depends on it.
  _Avoid_: using “delete” ambiguously for both hide/remove and irreversible storage reclamation.

### 9. Followed-Creator Automation
- `ChannelAutomationMode`: The per-`FollowedCreator` policy controlling what happens to newly discovered videos: `A` observes only, `B` also requests acquisition, and `C` additionally requests localization after acquisition.
  _Avoid_: treating the mode as a workflow engine or as permission to bypass acquisition/localization policy gates.
- `ChannelLocalizationDefaults`: The target language, review posture, and presentation defaults attached to a creator when mode `C` is enabled; they apply to future automatic localization requests until the operator changes them.
- `AutomationFailure`: A video-scoped failure of an automatic action that remains visible and retryable without disabling the followed creator or silently changing its automation mode.

### 10. Followed-Creator Polling
- `CreatorPoll`: A bounded attempt to refresh one `FollowedCreator` from the newest available feed toward its previously known boundary. A poll may succeed, fail, or become blocked, but only a successful poll may advance durable discovery progress.
- `PollBlocked`: A followed creator state requiring operator intervention before automatic polling resumes, used for conditions such as authorization/session/CAPTCHA requirements or an unavailable required browser/page execution lane.
  _Avoid_: treating a blocked poll as a successful empty feed or as a reason to discard the follow.
- `PollSchedule`: The monitoring cadence for followed creators. It is independent from `ChannelAutomationMode`; A/B/C decide post-discovery actions, not how often discovery occurs.

---

## Approved Testing Seams

Phase 1 maintains **exactly two approved architectural integration/acceptance testing seams**; ordinary in-memory unit tests for pure helpers/parsers/math do not count as additional architectural seams:

1. **Seam 1 — RuntimeHost Localhost API Contract**:
   Acceptance seam testing end-to-end pipeline execution over versioned localhost API endpoints (audio routing, `TextRegionPlan`, compact overlays, voice audition, zero-overrun timing, and exception queues).
2. **Seam 2 — StageWorker Runtime Contract**:
   Worker protocol seam testing subprocess NDJSON message interchange, lifecycle states, cancel/heartbeat handling, and process-tree termination.
