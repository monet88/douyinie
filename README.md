# Douyinie

Automated, policy-gated video localization pipeline transforming Chinese short-form videos (Douyin) into natural, timeline-constrained **Vietnamese and English (VI/EN)** versions with preserved soundtracks and clean in-place visual text localization.

---

## Source of Truth Hierarchy

When making architecture, provider, or implementation decisions, consult documents in this strict priority order:

1. **[Wayfinder Issue #1](https://github.com/monet88/douyinie/issues/1)** — Authoritative canonical decision index.
2. **[Implementation Spec #18](https://github.com/monet88/douyinie/issues/18)** — Authoritative Phase 1 implementation specification, including normative revalidation amendments.
3. **[`docs/diagrams/douyinie-architecture.json`](docs/diagrams/douyinie-architecture.json)** — Canonical repo-local Phase 1 architecture materialization (generated diagram set) of the locked #16 architecture plus authoritative #18 amendments.
4. **[`PRODUCT.md`](PRODUCT.md)** — Core product charter, user personas, capabilities, and constraints.
5. **[`CONTEXT.md`](CONTEXT.md)** — Domain vocabulary, invariant contracts, and testing seams.
6. **[`docs/reference-repositories.md`](docs/reference-repositories.md)** — External reference repository inventory (mining list only, **not** a dependency lockfile).

---

## Repository Layout

```
douyinie/
├── docs/
│   ├── architecture/             # Canonical Phase 1 architecture specification
│   ├── agents/                   # Agent operational guides (domain, issue-tracker, triage)
│   ├── prototypes/               # Prototype decisions (operator-review-workflow)
│   ├── research/                 # Empirical evaluations, benchmarks, and E2E acceptance reports
│   └── reference-repositories.md # External reference repository mining inventory
├── .ref/                         # [Git-ignored] Cloned reference codebases & live test work directories
├── AGENTS.md                     # Agent system instructions and operating context
├── CONTEXT.md                    # Core domain concepts, artifact schemas, and invariant contracts
├── PRODUCT.md                    # Product charter and pipeline requirements
└── README.md                     # Repository entrypoint and architecture overview
```

---

## Pipeline Overview

```text
Source Video (Douyin MP4)
  |
  +-- [1] Source analysis (audio-role analysis/separation adapters, probe, frame sampling)
  +-- [2] Speech understanding (ASR, forced alignment, diarization when warranted)
  +-- [3] Translation + spoken adaptation (VI/EN, source-relative cadence)
  +-- [4] VoiceAssignment + audition (AI recommendation, ~5s / ~10s contextual preview)
  +-- [5] Measured-duration TTS fitting immutable source speech windows
  +-- [6] TextRegionPlan (subtitle, semantic text, instructional UI, brand/keep)
  +-- [7] Deterministic cover/overlay + compact fit-content subtitle render
  +-- [8] Soundtrack-preserving mix + audiovisual quality gate
```

---

## Testing & Verification Seams

Phase 1 verifies architectural behavior through **exactly two approved integration/acceptance testing seams** (ordinary pure unit tests remain allowed):

1. **Seam 1 — RuntimeHost Localhost API Contract**:
   Versioned localhost API acceptance seam testing end-to-end pipeline execution, audio-role routing, soundtrack preservation, `TextRegionPlan` classification, compact subtitle rendering, voice audition, and exception projection.
2. **Seam 2 — StageWorker Runtime Contract**:
   Subprocess NDJSON protocol seam testing worker lifecycle, error propagation, process-tree termination, cancellation, heartbeat, and GPU leasing.

---

## Security & Credential Hygiene

- **Zero Secrets in Repository**: No API keys, passwords, bearer tokens, or session cookies may be committed to Git.
- **Local Artifact Guard**: Large media assets, stem separations, and temporary render directories reside under git-ignored `.ref/`.
