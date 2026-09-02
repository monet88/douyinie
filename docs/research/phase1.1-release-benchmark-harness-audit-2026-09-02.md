# Phase 1.1 Release Benchmark Harness & Measurement Gaps Audit

**Date:** 2026-09-02
**Target Commit:** `main@7bbac44969442444934338867db4abcb29a4d24b`
**Parent Wayfinder:** [Issue #53 (Wayfinder: Douyinie Release Hardening / Phase 1.1)](https://github.com/monet88/douyinie/issues/53)
**Parent Scope & Decisions:** [Issue #54](https://github.com/monet88/douyinie/issues/54), [Issue #9 Resolution](https://github.com/monet88/douyinie/issues/9), [Issue #18 Locked Spec](https://github.com/monet88/douyinie/issues/18)
**Authoritative Architectural Anchor:** [`docs/architecture/phase1-architecture.md`](docs/architecture/phase1-architecture.md), [`PRODUCT.md`](PRODUCT.md), [`CONTEXT.md`](CONTEXT.md)

---

## 1. Executive Summary & Audit Mission

Issue #54 mandates a comprehensive audit of Douyinie's current `main` codebase against the **Phase 1.1 release contract** to determine what benchmark execution and measurement capabilities exist, and what exact harness/measurement gaps must be closed before Douyinie can execute the **100-URL acquisition benchmark** and the **24-source-video × VI/EN (48 localized outputs) quality benchmark** as a reproducible release gate.

### Key Audit Findings

1. **Architecture & Pipeline Completeness:** The core Phase 1 pipeline is architecturally sound and functionally complete across both approved integration seams (`Seam 1`: RuntimeHost localhost REST API; `Seam 2`: StageWorker NDJSON subprocess IPC). The system features end-to-end support for media ingest, preflight probing, audio role planning, speech understanding (`Qwen3-ASR` + forced aligner + conditional diarization), meaning-first translation, spoken script duration adaptation, frozen run-scoped voice assignments, measured-duration TTS fitting, audio stem separation & remixing, multi-role visual text tracking (`TextRegionPlan`), and deterministic rendering.
2. **Current Benchmark & Regression Testing State:** Existing automated benchmark coverage is anchored by the 5-video synthetic regression suite (`test/seam1/acceptance_gate_test.go` and `test/seam2/acceptance_gate_test.go`), derived from the 2026-08-23 live acceptance tests (`docs/research/phase1-e2e-acceptance-2026-08-23.md`). These suites successfully prove fundamental cross-video invariants (measured-duration media truth, zero adjacent overrun, inter-turn spacing, cadence bounds, dialogue-only stem suppression, and non-occlusion).
3. **Primary Release Hardening Gap:** Douyinie **currently lacks a dedicated automated harness for multi-item benchmark corpus orchestration, aggregate metric extraction, and release gate decision evaluation**. The primitives (database tables, domain entities, and API endpoints) exist, but the automated runner that takes a 100-URL manifest or a 24-video test matrix, drives execution across profiles, extracts quantitative gate metrics, calculates pass/review/fail rates, and produces a structured release verdict report does not yet exist.
4. **Separation of Concerns:** Crucially, the gaps identified are **measurement, tooling, and test-harness gaps**, rather than fundamental pipeline flaws. The underlying production services (`IngestService`, `SpeechService`, `TranslationService`, `DubbingService`, `AudioMixService`, `VisualTextService`, `RenderService`, `ReviewService`) already emit the necessary data points.

---

## 2. Benchmark Corpus Contract Reconciliation

The benchmark requirements originate in the **Issue #9 Resolution** and are refined by **Issue #53 / #54** to align with the locked Phase 1 architecture and Phase 1.1 release posture.

### 2.1 Acquisition Benchmark (100-URL Set)
- **Scope:** 100 valid public Douyin URLs representing real-world distribution (single videos, creator shares, varied audio/visual qualities). URLs must still exist and be genuinely accessible; wrong-video, truncated-file, or false-success results count as failures.
- **Target Threshold:** $\ge 98\%$ success rate after configured retries and fallbacks.
- **Success Criteria:** Correct acquisition of canonical media bytes, container preflight pass (`container_valid == true`), fingerprint match, and extraction metadata normalization into an immutable `SourceAsset` in CAS.
- **Failures:** `CONTENT_UNAVAILABLE`, `INVALID_URL`, truncated files, corrupted containers, or unhandled anti-bot/captcha states.
- **Phase 1.1 Policy:** Acquisition is policy-gated (`REQUIRES_AUTHORIZATION`); session credentials remain local-only (`CredentialRef`). No anti-bot bypass is built; failure to acquire under bot challenges surfaces as structural exceptions.

### 2.2 Quality Benchmark (24 Videos × 2 Languages = 48 Localized Outputs)
- **Scope:** 24 representative Chinese-source videos localized to both Vietnamese (`vi`) and English (`en`), totaling 48 runs.
- **Minimum Required Content Dimensions (Issue #9):**
  1. *Clean Single-Speaker Speech:* High-fidelity narration, studio voice, clear articulation.
  2. *Multi-Speaker Dialogue:* Rapid turn-taking, distinct speaker identities, conversational overlap.
  3. *Fast / Dense Speech Cadence:* Brisk pacing, rapid speech flow, dense information delivery.
  4. *Loud BGM & Ambient Noise:* Noisy outdoor settings, cooking Foley (frying, pouring, chopping), heavy music backing.
  5. *Acoustic & Linguistic Edge Cases:* Dialects/accents, code-switching, proper names, numerical quantities, negation-critical dialogue.
  6. *Dense Burned-in Dialogue Subtitles:* Fast speech subtitles requiring in-place cover boxes.
  7. *Varied Clip Lengths & Pause Patterns:* Diverse video lengths and speech/pause tempos so timing is not optimized to one easy sample.
- **Out-of-Scope Elements:** Arbitrary titles, labels, signs, watermarks, and moving/perspective scene text are explicitly outside the V1 benchmark contract; arbitrary scene text remains untouched.

### 2.3 Phase 1.1 Policy Refinements (Superseding Issue #9 Drafts)
1. **Preset-Voice Policy (No Voice Cloning Requirement):** Issue #9 initially discussed voice cloning and speaker similarity ratings (e.g. $\ge 4.0/5$ similarity, $\ge 90\%$ same speaker identity in blind comparison). Under **Issue #53**, voice cloning is explicitly **de-scoped as a release requirement**. Phase 1.1 evaluates preset-voice naturalness (MOS $\ge 4.0/5$), stable per-speaker assignment (`VoiceAssignment` frozen per run, fail-closed `ErrVoiceAssignmentFrozen`, `ErrEngineHoppingForbidden`), and multi-speaker distinguishability (`EvaluateVoiceDistinguishability` / `VoiceDistinguishabilityQC`). Voice cloning remains an optional provider capability only.
2. **Multi-Role `TextRegionPlan` & V3 Compact Subtitles:** Issue #9 referred to generic subtitle replacement. Phase 1.1 enforces the canonical 5-role `TextRegionPlan` (`speech_subtitle`, `semantic_text`, `instructional_ui_text`, `brand_keep`, `ignore/noise`), deterministic in-place cover boxes, and compact fit-content subtitle presentation ($\sim 18\text{--}28$px horizontal padding) with scene-aware non-occlusion. Arbitrary scene text remains untouched.
3. **Execution Profile Gates:** Local 8 GB (single GPU lease, sequential family switching) and Hybrid profiles are hard execution gates. Cloud is an optional smoke validation gate only when an eligible configured provider is available.

---

## 3. Inventory of Existing Seam 1 & Seam 2 Benchmark Surfaces

Douyinie maintains exactly two approved integration seams. The current repository contains substantial testing harnesses across these seams:

| Seam & Test Harness File | Covered Domain Stages | Verification Invariants | Capabilities Present | Missing Capabilities for Release Benchmark |
|---|---|---|---|---|
| **Seam 1:** `test/seam1/acceptance_gate_test.go` | Ingest $\to$ Speech $\to$ Trans $\to$ Dub $\to$ Mix $\to$ OCR $\to$ VisualTrack $\to$ RenderPlan | All 5 canonical acceptance fixtures (2026-08-23); zero adjacent overrun; inter-turn gap preservation; cadence ratio bounds; soundtrack preservation (music outro passthrough); role classification; non-occlusion. | Synthetic fixtures; deterministic mock execution; exact invariant checking; negative mutation test proofs. | Fixed synthetic corpus only; does not run against raw MP4s or external provider models; no automated batch summary report or aggregate CER/MOS scoring. |
| **Seam 1:** `test/seam1/vertical_slice_test.go` | Full pipeline end-to-end via public HTTP API | End-to-end vertical slice; proxy preview and high-quality final render parity; ReviewItem exception queue verification. | Full API round-trip; CAS storage verification; DB indices verification. | Single hard-coded scenario; no multi-video parameterized execution. |
| **Seam 1:** `test/seam1/acquisition_test.go` | Acquisition & Ingestion (`IngestService`, `AcquisitionService`, `Router`) | Multi-candidate fallback ladder; `PreflightReport` container integrity; CAS deduplication; fail-closed URL validation; credential resolution; offline simulation. | Mock HTTP server simulating Douyin CDN/API; error classification (`INVALID_URL`, `CONTENT_UNAVAILABLE`, `ANTI_BOT_OR_EMPTY_RESPONSE`). | Exercises single endpoints (`POST /api/v1/sources/acquire`, `POST /api/v1/sources/probe`); lacks a batch runner for executing a 100-URL manifest and aggregating acquisition success rates. |
| **Seam 1:** `test/seam1/real_tts_seam1_test.go` | TTS Synthesis & Fit Controller | Real model inference via `VieNeu-TTS` and `CosyVoice3`; probed waveform duration validation; speed-fit resynthesis loop. | Environment-gated real Python inference (`DOUYINIE_TTS_PYTHON_BIN`); WAV duration probing. | Evaluates isolated synthetic phrases, not full 24-video dialogue scripts. |
| **Seam 2:** `test/seam2/acceptance_gate_test.go` | StageWorker IPC & Transport | Worker-produced artifacts travel as filesystem refs (path + SHA-256); CAS content-address integrity; JSON schema validation. | Real StageWorker binary invocation; fake model binary stub; SHA-256 byte-level verification. | Limited to ASR stage contract; does not drive full pipeline sequence across families. |
| **Seam 2:** `test/seam2/seam2_test.go` | StageWorker Lifecycle & Resource Scheduler | NDJSON IPC protocol; heartbeat timeout; process-tree cleanup (Windows Job Objects); single GPU lease acquire/release; VRAM reclamation. | Subprocess management; graceful SIGTERM/SIGKILL escalation; lease handoff verification. | Protocol verification only; not designed for benchmark workload execution. |
| **Seam 2:** `test/seam2/real_tts_seam2_test.go` | StageWorker TTS Adapters | Real `VieNeu` and `CosyVoice3` adapters running under StageWorker CLI; multi-pass speed-fit lane. | Probed waveform duration; base64 audio decoding; speed parameter validation. | Single-segment execution; no pipeline chaining. |

---

## 4. Persisted Evidence & Quality Artifacts Inventory

The Douyinie architecture is artifact-first. Every stage produces content-addressed, immutable artifacts stored in CAS and indexed in SQLite:

```
SourceAsset (CAS: media bytes)
  ├── PreflightReport (SQLite: container, codecs, duration, normalized audio)
  ├── AudioRolePlan (SQLite/CAS: narration vs singing vs BGM vs SFX)
  ├── TextRegionPlan (SQLite/CAS: spatial/temporal boxes, roles, keyframes)
  ├── TranscriptArtifact (SQLite/CAS: ASR tokens, word timings, speaker turns)
  ├── AudioStemArtifacts (SQLite/CAS: vocals, BGM, SFX stems)
  ├── TranslationVariant (SQLite/CAS: meaning-first translation, QA gate flags)
  ├── DubScriptVariant (SQLite/CAS: cadence adaptation, word budget, review reasons)
  ├── VoiceAssignment (SQLite/CAS: frozen speaker -> VoiceProfile mapping, distinguishability QC)
  ├── DubSegmentsVariant (SQLite/CAS: synthesized audio WAVs, measured durations, fit verdicts)
  ├── DubMixArtifact (SQLite/CAS: remixed audio stem with dialogue-only suppression)
  ├── LocalizedVisualTrack (SQLite/CAS: in-place overlay images, subtitle layout plan)
  ├── LocalizedSubtitleTrack (SQLite/CAS: ASS/SRT subtitle cues)
  ├── RenderPlan (SQLite/CAS: frozen composition recipe)
  ├── FinalRenderArtifact (CAS: output MP4, SQLite: render log)
  ├── QualityResult (SQLite: append-only multimodal QC report)
  └── ReviewItem & ReviewOverride (SQLite: exceptions and operator audit trail)
```

### 4.1 Persisted Evidence Tables in SQLite
- `source_assets`: Media container metadata, SHA-256, byte size, ingest timestamp.
- `source_acquisitions`: Canonical URL mapping, unique platform source IDs, deduplication links to `source_assets`, provenance JSON, acquisition timestamps (Schema v16).
- `preflight_reports`: Video/audio codecs, duration, frame rate, container validity, normalized audio CAS ref.
- `stage_executions`: Per-stage status (`queued`, `running`, `cancelling`, `succeeded`, `failed`, `interrupted`), started/completed timestamps, artifact SHA-256, error messages.
- `provider_attempts`: Per-attempt routing logs including `stage`, `provider_id`, `model_name`, `model_version`, `input_hash`, `attempt_number`, `status`, `error_message`, `latency_ms`, and `cost_units` (Schema v2).
- `selection_decisions`: Immutable audit log of provider routing choices, evaluated candidates, policy checks, and selection rationales.
- `quality_results`: Append-only multimodal QC records with structured `metrics` and `issues`.
- `review_items`: Actionable exceptions flagged by automated gates (`tts_overrun`, `low_confidence_ocr`, `uncertain_role`, `translation_qa`, `dub_script_qa`, `audio_role_uncertain`, `visual_occlusion`).
- `review_overrides`: Auditable records of operator interventions and manual approvals.

---

## 5. Metric Extraction & Verification Gap Analysis

To evaluate Douyinie against the **Issue #9 Acceptance Gates** and **Phase 1.1 Policy Overrides**, the following table maps each gate to what the pipeline currently calculates/persists vs. what measurement harness capabilities are missing:

| Gate | Target Threshold (Issue #9 & Phase 1.1) | Pipeline Evidence Currently Calculated / Persisted | Existing In-Code Thresholds & Checks | Missing Measurement & Harness Capabilities |
|---|---|---|---|---|
| **1. Source Acquisition** | $\ge 98\%$ success on 100 URLs; correct media & metadata; truncated/wrong content = failure. | `AcquisitionProvenance`, `source_acquisitions`, `PreflightReport`, `SourceAsset` SHA-256, error states (`AcquisitionState`). | `candidateSplitProber`, preflight container check, fingerprint validation. | **No batch acquisition harness:** missing harness orchestration to ingest a 100-URL manifest over `POST /api/v1/sources/acquire`, track attempt history, verify CAS/container validity, and aggregate overall acquisition success rates from `source_acquisitions` and `provider_attempts`. |
| **2. Chinese ASR + Timing** | Overall CER $\le 5\%$; no category $> 10\%$ CER; serious recognition errors (names, numbers, negation, core meaning) $\le 1\%$ of speech blocks; P95 timing error $\le 250$ms. | `TranscriptArtifact` with `WordTiming` (start/end ms), ASR confidence, raw segments. | Monotonic word boundaries, forced alignment boundary enforcement. | **No CER & adjudication harness:** missing tooling to compare `TranscriptArtifact` against human ground-truth transcripts, compute CER overall and per-category, evaluate P95 timestamp error, and adjudicate the $\le 1\%$ serious error rate across speech blocks. |
| **3. Translation & Adaptation** | Semantic Fidelity $\ge 4.5/5$; Spoken Naturalness $\ge 4.2/5$; 0 critical translation errors (names, numbers, negation, facts). | `MeaningFirstQAGate`: extracted numbers (Chinese/Vietnamese/English), ASCII proper names, negation polarity matching; `PassedQAGate`, `Violations`. | Deterministic rule-based validation of digits, units, and polarity in `internal/service/translation_qa.go`. | **No semantic evaluation aggregator:** missing tooling to aggregate meaning QA violations across a 48-job corpus and collect human or automated evaluator scores for fidelity and spoken naturalness. |
| **4. Voice Naturalness & Identity (Phase 1.1)** | Naturalness MOS $\ge 4.0/5$; stable per-speaker assignment; multi-speaker distinguishability. *(Voice cloning de-scoped).* | `VoiceAssignment` (frozen per run); `EvaluateVoiceDistinguishability` (flags duplicate voices across speakers without explicit flag); preset profiles (`DefaultPresetVoices`). | Fail-closed frozen assignment (`ErrVoiceAssignmentFrozen`); engine hopping forbidden (`ErrEngineHoppingForbidden`). | **No voice QC aggregation harness:** missing tooling to aggregate voice assignment audits, verify absence of duplicate presets in multi-speaker runs, and capture subjective naturalness ratings for preset voices across localized outputs. |
| **5. Dubbing Timing & Cadence** | $\ge 95\%$ dub blocks end within $\pm 200$ms of anchor/budget; $>500$ms error triggers review/fail; whole-video drift $\le 100$ms. | `DubSegmentsVariant`: `SlotDurationMs`, `MeasuredDurationMs`, `NaturalGapAfterMs`, `CadenceRatio`; `FitController` decision ladder (`ACCEPT`, `RESYNTH`, `REWRITE`, `REGROUP`, `REVIEW`). | Invariant checks: $D_{\text{tts}} \le T_{\text{source}}$; no adjacent collision. *Observed in-code adapter planning priors (not release contracts):* cadence trigger ratio ($0.65\text{--}1.20$); review ratio ($0.50\text{--}1.50$). | **Corpus-level timing aggregator:** Pipeline records segment-level timing, but no harness aggregates block timing error distribution (P95), whole-video drift, or generates a timing compliance histogram across all 48 runs. |
| **6. Background Audio Preservation** | Chinese dialogue inaudible in $\ge 95\%$ speech windows; BGM/SFX naturalness $\ge 4.2/5$; $\ge 95\%$ joins click/pump free. | `AudioRolePlan` (dialogue vs singing vs BGM/SFX); `AudioStemArtifacts` (vocals, instrumental); `DubMixArtifact` with dialogue-only suppression windows. | Negative tests proving instrumental outros are unsuppressed; passthrough for non-dub videos. | **No acoustic residual / join inspection harness:** missing automated objective SNR/residual vocal leakage measurement in speech windows; missing spectral discontinuity checks at segment boundaries. |
| **7. Subtitle Replacement** | Dialogue subtitle replaced; arbitrary text untouched; $\ge 95\%$ regions artifact-free; position/style match $\ge 4.0/5$. | `TextRegionPlan` (roles, bounding boxes, keyframes); `SceneProtectedRegion`; `LocalizedVisualTrack` (compact fit-content box, scale-aware padding). | Overlap detection with protected UI/tap targets; role validation; non-occlusion assertions. | **No automated visual residual checker:** missing automated detection of unmasked Chinese glyph bleed or OCR confidence drop post-render; perceptual style score collection remains manual. |
| **8. Runtime / Cost / 8 GB Profile** | Hybrid: $\le 2\times$ real-time; Cost: $\le \$0.25$/min; Local 8 GB: $\le 5\times$ real-time, no OOM; Cloud smoke pass. | `StageExecution` (started/completed timestamps); `ProviderCapability.CostPerUnit`; `ResourceScheduler` (single GPU lease, profile memory 8 GB); `provider_attempts.cost_units`. | Single GPU lease mutual exclusion; Windows Job Object process tree termination. | **No resource & cost profiler:** missing automated wall-clock vs media duration ratio calculation (RTF); missing GPU VRAM high-water mark logging; missing aggregation of `provider_attempts.cost_units` into total dollar expenditure per source minute. |

---

## 6. PASS / REVIEW_REQUIRED / FAIL Classification State Machine

Issue #9 and Phase 1 Architecture §9 define three release benchmark outcomes:
1. **`PASS`**: All applicable automated hard gates met; zero blocking review exceptions.
2. **`REVIEW_REQUIRED`**: Output usable, but one or more metrics sit in the review tolerance band (e.g. non-fatal duration overrun requiring operator verification, low OCR confidence, ambiguous text role, or cadence adapter flag).
3. **`FAIL`**: Fatal acquisition failure, unresolvable timing collision, critical translation error (negation/name/number), unmasked Chinese dialogue/subtitle collision, or pipeline crash/OOM.

### Release Gate Criteria (Issue #9 & Issue #53):
- **$\ge 90\%$ `PASS`** across the quality benchmark corpus.
- **$\le 10\%$ `REVIEW_REQUIRED`** exception rate.
- **$\le 2\%$ `FAIL`** hard failure rate.
- **Category Balance Rule:** No required benchmark category may hide systematic failure behind a good global average.

### Current Implementation Status in Code
- **Supported:** The pipeline natively models these states:
  - In `domain.QualityStatus`: `PASS`, `REVIEW_REQUIRED`, `FAIL`.
  - In `domain.ReviewItemStatus`: `pending`, `auto_pass`, `auto_resolved`, `manual_override`.
  - In `domain.DubSegmentsVariant`: `OverallStatus` (`PASS`, `REVIEW_REQUIRED`, `FAIL`).
  - In `service.ReviewService`: `ProjectReviewItems` projects all active exceptions; `EvaluateFinalRenderHandoff` evaluates queue-zero state and blocks final render if pending review items exist (`CanStartFinalRender == false`, `Action == "review_required"`).
- **Harness Gap:** There is **no top-level corpus classification aggregator**. If 48 runs complete, Douyinie does not currently have tooling to aggregate the 48 runs, compute the percentage of PASS / REVIEW_REQUIRED / FAIL runs, check per-category balance, and validate against the release gate ($\ge 90\%$ PASS, $\le 10\%$ REVIEW_REQUIRED, $\le 2\%$ FAIL, no category failing systematically).

---

## 7. Automated AV/QC vs. Still-Manual Verification Steps

The 2026-08-23 live acceptance benchmark (`docs/research/phase1-e2e-acceptance-2026-08-23.md`) was conducted with a mixture of automated pipeline execution, multimodal LLM evaluation (`gemini-3.7-flash-high`), and human inspection.

### 7.1 What is Already Automated in Code
- **Media Ingestion & Container Preflight:** Automated FFprobe container format and codec validation (`internal/media/probe.go`).
- **Waveform Probing:** Automated WAV header inspection and exact duration calculation (`internal/media/wav.go`).
- **Timing & Overrun Gating:** Automated zero-overrun enforcement and natural pause calculation in the TTS `FitController`.
- **Text Role & Occlusion Gating:** Automated geometry calculation and protected region collision detection in `internal/service/visual_text.go`.
- **Meaning Preservation Rules:** Automated regex-based extraction of numbers, names, and negation markers in `internal/service/translation_qa.go`.
- **Single GPU Lease & Process Hygiene:** Automated VRAM reclamation and process termination via `worker.GPULeaseManager` and Windows Job Objects.
- **Review Queue Projection & Auto-Resolution:** Automated projection of pipeline exceptions into `ReviewItem`s, and automatic clearing when reruns pass.
- **Attempt & Provenance Persistence:** Automated capture of provider attempts, latencies, cost units, and acquisition deduplication in SQLite tables `provider_attempts` and `source_acquisitions`.

### 7.2 What Remains Manual or Script-External (Harness Gaps)
1. **Batch Corpus Execution:** Currently, running multiple videos requires invoking HTTP endpoints sequentially via manual scripts or curl. There is no automated harness to drive a 24-video matrix across profiles.
2. **Multimodal AV/QC Evaluation:** The `QualityResult` data structure and API endpoints exist (`POST /api/v1/quality-results`), but running multimodal evaluation over the rendered video to grade naturalness, background preservation, and residual dialogue is an external manual/scripted step. It is not packaged into an automated benchmark post-processor.
3. **Reference Transcript & Adjudication Tooling:** Comparing ASR transcripts to reference texts, calculating CER, and adjudicating whether word errors violate the $\le 1\%$ serious meaning error gate currently requires manual inspection.
4. **Human Review Exception & Sampling Workflow:** The `ReviewService` backend provides complete API support (`/inspector/correct-text`, `/inspector/reassign-voice`, `/inspector/override-region`), but executing the Issue #53 human audit protocol—evaluating all `REVIEW_REQUIRED`/critical cases plus a stratified sample of automated `PASS` results—lacks harness sampling support.
5. **VRAM Peak Tracking:** While the `ResourceScheduler` enforces single-lease mutual exclusion, continuous sampling of NVIDIA VRAM utilization (e.g. via NVML or `nvidia-smi` logging) to prove that Local 8 GB never exceeds 8192 MB during peak model inference is not integrated into test output.
6. **Per-Run Cost Aggregation:** While `provider_attempts.cost_units` records per-attempt usage in SQLite, the aggregate dollar expenditure per source minute across all pipeline stages is not rolled up into a benchmark cost verdict against the $\le \$0.25$/min threshold.
7. **Conditional Cloud Smoke Harness:** Running smoke validation on Cloud profile without treating it as a hard gating requirement (which applies to Local 8 GB and Hybrid) lacks a formal harness configuration switch.

---

## 8. Preserved Architecture & Domain Policies Check

Before establishing the harness specification, we verify compliance with canonical Phase 1.1 architectural constraints:

- [x] **Two Testing Seams Only:** All harness additions must exercise Seam 1 (Localhost REST API) or Seam 2 (StageWorker NDJSON IPC). No internal private package hooks or ad-hoc intermediate seams may be introduced.
- [x] **Phase 1.1 Preset-Voice Policy:** Voice cloning is not evaluated for release gating. Preset voices (`DefaultPresetVoices`) and multi-speaker distinguishability (`VoiceDistinguishabilityQC`) are authoritative.
- [x] **TextRegionPlan & Compact Subtitles:** Bounding boxes must conform to the 5 canonical roles and compact fit-content geometry; full-width destructive bars and arbitrary scene text inpainting are strictly rejected.
- [x] **Immutable Source Anchors:** Video visual timing is strictly preserved; spoken adaptation must fit within immutable slots without shifting downstream anchors.
- [x] **Dialogue-Only Dubbing:** Musical vocals, BGM, and Foley effects are preserved outside and through dialogue replacement.
- [x] **Observed Heuristics vs. Release Contracts:** Cadence trigger ($0.65\text{--}1.20$) and review ($0.50\text{--}1.50$) ratios remain understood as internal adapter planning priors, not release contract gates.

---

## 9. Capability Gaps to Close for Phase 1.1 Benchmark Readiness

To make the release benchmark fully runnable and reproducible without altering product architecture or inventing speculative designs, the following specific harness capabilities must be established:

### 9.1 Corpus Specification & Ground-Truth Infrastructure
- **Acquisition Corpus Specification:** A structured catalog of 100 valid public Douyin source URLs representing real-world distribution (varied styles, audio environments, and creators).
- **Quality Corpus Specification:** A structured catalog of 24 representative Chinese-source videos covering the 7 mandatory dimensions from Issue #9, paired with human-verified reference Chinese transcripts, expected speaker counts, and known dialogue subtitle regions.
- **Offline / Recorded Mock Fixtures:** Deterministic playback fixtures for both acquisition and quality sets to support regression testing in environments without live external network or GPU access.

### 9.2 Execution & Orchestration Capabilities
- **Batch Acquisition Driver:** Automated sequential orchestration of the 100-URL acquisition set via the public endpoint `POST /api/v1/sources/acquire` (and probe via `POST /api/v1/sources/probe`), capturing HTTP status codes, retry counts, latency, and preflight container validation. This preserves the Phase 1 single-active-run posture rather than introducing benchmark-only throughput concurrency.
- **Quality Matrix Driver:** Automated orchestration to drive the 24 videos through the complete localization lifecycle for both Vietnamese (`vi`) and English (`en`) targets (48 runs) through **Seam 1** HTTP endpoints. **Seam 2** remains the separate StageWorker runtime-contract evidence surface for worker IPC, process-tree cleanup, GPU-lease lifecycle, and resource measurements; the benchmark must not invent a third integration seam.
- **Profile-Aware Execution Gating:** Explicit profile management that enforces hard release gates on **Local 8 GB** (verifying single-GPU lease handoff and peak VRAM bounds) and **Hybrid**, with an optional smoke-test capability for **Cloud** when credentials are configured.

### 9.3 Metric Extraction & Aggregation Capabilities
- **CER & Serious Error Adjudication Tooling:** Automated Levenshtein character-error-rate calculation comparing `TranscriptArtifact` text to reference transcripts overall and across each required content category, coupled with error classification tooling to adjudicate the $\le 1\%$ serious error threshold (names, numbers, negation, core meaning).
- **Timing & Drift Distribution Aggregator:** Extraction of segment-level $D_{\text{tts}}$ versus $T_{\text{source}}$ slot durations from `DubSegmentsVariant` across all 48 runs, calculating P95 timing error, verifying compliance with the $\pm 200$ms window, flagging $>500$ms exceptions, and calculating accumulated whole-video drift against the $\le 100$ms ceiling.
- **Meaning QA Gate Aggregator:** Roll-up of `MeaningFirstQAGate` violations from `TranslationVariant` artifacts across all runs, validating the zero critical errors requirement.
- **Review Queue & State Machine Aggregator:** Querying `GET /api/v1/runs/{id}/review-items` and `DubSegmentsVariant.OverallStatus`, tallying pending review items, and classifying each run into `PASS`, `REVIEW_REQUIRED`, or `FAIL`.
- **Cost & Attempt Evidence Aggregator:** Querying SQLite `provider_attempts` and `source_acquisitions` tables to aggregate total compute latency, model versions, retry counts, and total cost units per source minute against the $\le \$0.25$/min ceiling.

### 9.4 Quality Verification & Human Audit Infrastructure
- **Automated Multimodal AV/QC Packaging:** Tooling to execute multimodal AV/QC evaluation across rendered MP4s and record structured results via `POST /api/v1/quality-results`.
- **Stratified Human Audit Support:** Tooling to select all `REVIEW_REQUIRED` and `FAIL` outputs plus a deterministic stratified sample of automated `PASS` runs for human inspection of naturalness, background preservation, and residual dialogue/subtitle bleed.

### 9.5 Release Gate Evaluation & Reporting Capabilities
- **Corpus-Wide Gate Verifier:** Automated validation of aggregate metrics against the Issue #9 / Phase 1.1 thresholds:
  - Acquisition Success $\ge 98\%$.
  - Pipeline Outcomes: $\ge 90\%$ PASS, $\le 10\%$ REVIEW_REQUIRED, $\le 2\%$ FAIL.
  - Category Balance: No single category hiding systematic failure behind global averages.
  - Chinese ASR: CER $\le 5\%$ overall, $\le 10\%$ per category, serious errors $\le 1\%$, P95 timing $\le 250$ms.
  - Timing: $\ge 95\%$ blocks within $\pm 200$ms, whole-video drift $\le 100$ms.
  - Translation: 0 critical errors, fidelity $\ge 4.5/5$, naturalness $\ge 4.2/5$.
  - Preset Voice: Naturalness MOS $\ge 4.0/5$, frozen assignment preserved, distinguishable speaker identities.
  - Background Audio: Inaudible source dialogue in $\ge 95\%$ speech windows, joins click/pump-free $\ge 95\%$, BGM naturalness $\ge 4.2/5$.
  - Subtitle Replacement: $\ge 95\%$ regions artifact-free, style match $\ge 4.0/5$, arbitrary scene text untouched.
  - Resource & Cost: Hybrid RTF $\le 2.0$, Local 8 GB RTF $\le 5.0$, peak VRAM $\le 8192$ MB, API cost $\le \$0.25$/source min.
- **Structured Report Generator:** Outputting an auditable Markdown report containing the full evidence table and release verdict.

---

## 10. Conclusion & Recommended Next Steps

Douyinie's underlying Phase 1 engineering is robust, strictly typed, and policy-governed. The pipeline primitives and invariant checking mechanisms are already functioning on `main`. The remaining work to satisfy Issue #54 and advance Issue #53 is strictly **harness orchestration, corpus curation, and aggregate metric reporting**.

### Recommended Next Steps for Phase 1.1:
1. **Corpus Specification & Assembly:** Finalize the 100 Douyin acquisition URLs and 24 canonical quality benchmark source videos with ground-truth reference transcripts and category metadata.
2. **Benchmark Execution & Metric Aggregation Tooling:** Implement the Seam 1 / Seam 2-driven benchmark execution runner and metric aggregation capabilities.
3. **Multimodal AV/QC & Human Sampling Workflow:** Package the post-render multimodal evaluation flow and stratified sampling mechanism for human audit.
4. **Execution-Profile Verification Runs (Local 8 GB & Hybrid):** Execute the full 48-output quality benchmark on the target hardware (RTX 2060 SUPER 8 GB), collect runtime, VRAM, and cost evidence, and publish the formal Phase 1.1 Release Candidate benchmark report.
