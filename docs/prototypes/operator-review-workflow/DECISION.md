# Issue 14 Decision — Operator Review Workflow

## Question
What is the simplest operator workflow for correcting low-confidence or timing-problem segments without turning Douyinie into a full video editor?

## Decision
Adopt **Variant A — Queue + Inspector** as the Phase 1 operator workflow.

- **Automated by default**: The pipeline runs unattended while quality gates pass.
- **Exception-only review**: Unresolved confidence, timing, subtitle, speaker/voice, policy, or quality blockers enter the queue. Passing or auto-resolved segments do not enter or remain in the queue.
- **Segment-scoped overrides**: Operator edits create new candidate attempts/selections rather than mutating committed source artifacts in-place.
- **Direct-manipulation visual text controls**: Operators can adjust, resize, drag, or reclassify bounding boxes for `TextRegionPlan` elements (dialogue subtitles, semantic labels, instructional UI text) when auto-detection requires correction.
- **Arbitrary video editing out of scope**: Full-track non-linear video editing, timeline splicing, and arbitrary scene transformations remain out of scope for V1.

## Approval Semantics
- `auto_pass`: Quality gates passed without human intervention.
- `auto_resolved`: A targeted rerun produced a passing candidate; the item leaves the queue automatically.
- `manual_override`: The operator explicitly accepts a flagged candidate with an auditable decision note.
- Editing target text, region geometry, or voice assignment marks the segment dirty and triggers a targeted rerun.

## Targeted Invalidation DAG
- **Target text edit** &rarr; `TTSAttempt` &rarr; `DubSegment` &rarr; `DubMix` &rarr; `LocalizedSubtitleTrack` &rarr; `FinalRender`.
- **Voice assignment edit** &rarr; `TTSAttempt` &rarr; `DubSegment` &rarr; `DubMix` &rarr; `FinalRender`.
- **Text region / overlay edit** &rarr; `TextRegionPlan` &rarr; `LocalizedVisualTrack` &rarr; `FinalRender` (visual/render descendants only; source-derived extraction and transcript inputs remain untouched).
- Source video assets, extracted audio stems, transcription alignments, and preserved background stems remain cached unless upstream inputs change.

## Final Render Handoff
- **Auto mode**: When the exception queue reaches zero, final render proceeds automatically.
- **Review mode**: Queue zero exposes an explicit `Start final render` action.

## Host Shell
Maintain **browser-on-localhost** as the canonical Phase 1 shell over the RuntimeHost API boundary.
