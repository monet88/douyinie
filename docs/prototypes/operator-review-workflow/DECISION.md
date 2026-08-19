# Issue 14 decision — operator review workflow

## Question
What is the simplest operator workflow for correcting low-confidence or timing-problem segments without turning Douyinie into a full video editor?

## Decision
Use **Variant A — Queue + Inspector** as the Phase 1 operator workflow.

- **Auto is the default mode.** The pipeline runs unattended while quality gates pass.
- Human review is **exception-only**: unresolved confidence, timing, subtitle, speaker/voice, policy, or quality blockers enter the queue.
- Passing or auto-resolved segments do not remain in the queue.
- Operator edits are segment-scoped and create new attempts/selections rather than mutating committed artifacts in place.
- Timeline-first UI remains supporting context inside the inspector, not the primary workflow.
- Arbitrary scene-text editing remains out of V1.

## Approval semantics
- `auto_pass`: quality gates passed without intervention.
- `auto_resolved`: a targeted rerun produced a passing candidate; the item leaves the queue automatically.
- `manual_override`: the operator explicitly accepts a still-flagged candidate; the override remains auditable.
- Editing target text or voice assignment marks the segment dirty, unapproved, and requiring a targeted rerun.

## Targeted invalidation
- Target-text edit → TTS Attempt → DubSegment → DubMix → LocalizedSubtitleTrack → FinalRender.
- Voice-assignment edit → TTS Attempt → DubSegment → DubMix → FinalRender.
- SourceAsset, source timing anchors, transcript/alignment, clean plates, and preserved background stems remain reusable unless their own inputs changed.

## Final-render handoff
- Auto mode: when the exception queue reaches zero, final render proceeds automatically.
- Review mode: queue zero exposes an explicit `Start final render` action.

## Shell
Keep **browser-on-localhost** as the canonical Phase 1 shell over the RuntimeHost/API boundary. A packaged desktop app may later be a thin wrapper around the same localhost UI/API rather than a separate architecture.
