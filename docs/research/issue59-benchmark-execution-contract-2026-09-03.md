# Issue #59 — Benchmark Execution & Metric Aggregation Contract

Date: 2026-09-03
Status: RESOLVED CONTRACT (research/design only; implementation-ready)
Errata (2026-09-18): the gateway DeepSeek alias is `deepseek-v4.1-flash` (bare alias with no namespace prefix); ladder order and all other decisions here are unchanged.
Parent: https://github.com/monet88/douyinie/issues/53
Issue: https://github.com/monet88/douyinie/issues/59
Authoritative references:
- Wayfinder Issue #1: https://github.com/monet88/douyinie/issues/1
- Issue #9 (Acceptance metrics & benchmark corpus): https://github.com/monet88/douyinie/issues/9
- Issue #16 (Final Architecture Lock): https://github.com/monet88/douyinie/issues/16
- Issue #18 (Phase 1 implementation spec): https://github.com/monet88/douyinie/issues/18
- Issue #53 (Phase 1.1 release hardening map): https://github.com/monet88/douyinie/issues/53
- Issue #54 (Release benchmark harness audit): https://github.com/monet88/douyinie/issues/54
- Issue #56 (100-URL acquisition corpus & drift protocol): https://github.com/monet88/douyinie/issues/56
- Issue #57 (24-video VI/EN quality corpus & review protocol): https://github.com/monet88/douyinie/issues/57
- Issue #58 (RC evidence bundle & release verdict): https://github.com/monet88/douyinie/issues/58
- Issue #60 (Translation provider decision for Local 8 GB + Hybrid): https://github.com/monet88/douyinie/issues/60
- Issue #61 (Model snapshot & license enforcement): https://github.com/monet88/douyinie/issues/61
- Issue #62 (Production adapter compatibility & RC pinning): https://github.com/monet88/douyinie/issues/62
- Canonical architecture: `docs/architecture/phase1-architecture.md`
- Domain vocabulary: `CONTEXT.md`
- Target commit: `main@7bbac44969442444934338867db4abcb29a4d24b`

---

## 1. Executive Summary & Design Mission

Issue #59 establishes the canonical execution lifecycle, runtime boundaries, and metric aggregation contract for Douyinie Phase 1.1 release benchmarking. It bridges the audited measurement gaps from Issue #54 and the acquisition oracle/drift protocol from Issue #56, defines a typed execution/aggregation handoff to the still-open Issue #57 corpus/review protocol, incorporates the production adapter compatibility pins from Issues #60 and #62, and feeds the release candidate evidence bundle and verdict in Issue #58.

### Non-Negotiable Contract Boundaries:
1. **Exactly Two Architectural Testing Seams**:
   - **Seam 1 (RuntimeHost Localhost API Contract)**: Sole acceptance surface for end-to-end acquisition, stage execution, review projection, inspector overrides, final-render handoff, and multimodal QC ingestion.
   - **Seam 2 (StageWorker Runtime Contract)**: Subprocess NDJSON protocol, process-tree supervision (Windows Job Objects), GPU lease lifecycle/VRAM reclamation, and adapter execution evidence.
   - **No Third Architectural Testing Seam**: The benchmark runner is a client of Seam 1 and an observer of Seam 2. It introduces no internal bypasses, private-package test hooks, or un-versioned IPC channels.
2. **Sequential Corpus Execution**: Phase 1 enforces a single-active-run architecture and a single GPU lease (8 GB VRAM baseline). All corpus runs (100 acquisition URLs, quality runs across profiles) execute sequentially. Harness-level concurrency is strictly prohibited.
3. **Immutable Evidence & Persistence Boundary**: Media and stage artifacts are content-addressed in CAS and indexed in SQLite. `QualityResult` and `ReviewOverride` are persisted relational audit evidence tied to run/job/asset identity; they are not currently CAS-backed objects. Issue #58 owns packaging these records into an immutable reproducible RC evidence bundle.
4. **Execution Profile Gates**:
   - **Local 8 GB Profile (Hard Release Gate)**: Runs on target hardware (RTX 2060 SUPER 8 GB VRAM). Local-only inference, fail-closed against remote inference providers, empirical completion on the 8192 MB target without OOM/crash, recorded device-memory telemetry, and Real-Time Factor (RTF) $\le 5.0\times$.
   - **Hybrid Profile (Hard Release Gate)**: Primary local execution with gateway translation (Gemini $\to$ DeepSeek Vision-Flash $\to$ Local Qwen), cost $\le \$0.25$ per source media minute, RTF $\le 2.0\times$.
   - **Cloud Profile**: Conditional smoke-only capability verification. Does not block Local/Hybrid RC verdict.
5. **Separation of Responsibilities**:
   - Issue #59 owns benchmark execution lifecycle, stage sequencing via public Seam 1, resume/retry semantics, metric extraction, and gate aggregation logic.
   - Issue #56 owns the 100-URL acquisition corpus manifest, validity oracle, and rotation protocol.
   - Issue #57 owns the 24-video corpus assembly, ground-truth reference transcripts, and human review sampling formulas.
   - Issue #58 owns the durable packaging of artifacts and final RC verdict synthesis.

---

## 2. Decision Tree & Resolution Map

```
Benchmark Contract (Issue #59)
├── 1. Testing Seams & Integration Boundary
│   ├── [FACT: Repo / Architecture §10] Seam 1: RuntimeHost Localhost REST API (Acquisition, Stage Endpoints, Review, Handoff)
│   ├── [FACT: Repo / Architecture §10] Seam 2: StageWorker NDJSON IPC (Process hygiene, Single-GPU lease lifecycle)
│   └── [FACT: Architecture / NVML Manual] Telemetry: Host GPU device telemetry (nvmlDeviceGetMemoryInfo) is harness measurement, not Seam 3
├── 2. Execution Topology & Scheduling
│   ├── [FACT: Repo / Invariant] Single-Active-Run invariant: 100-URL acquisition executes sequentially
│   ├── [FACT: Issue #9 / #53] 24-source × 2 target languages (VI/EN) = 48 logical localization cases
│   ├── [DERIVED REQUIREMENT: Issues #53, #54, #60, #62] Both Local 8 GB and Hybrid are hard release gates: 48 cases × 2 profiles = 96 runs
│   └── [FACT: Repo / internal/worker/lease.go] Single GPU Lease: GPULeaseManager terminates/reaps previous family before granting lease
├── 3. Identity, Resume, & Retry Semantics
│   ├── [FACT: Design] BenchmarkSession binds corpus manifest digest, profile, commit SHA, and config snapshot
│   ├── [FACT: Repo / internal/domain/types.go] Run identities use native domain UUIDs: LocalizationJob.ID and LocalizationRun.ID
│   ├── [FACT: Repo / Invariant] Resume: Reuses committed CAS artifacts for succeeded stages; interrupted stages restart cleanly
│   └── [FACT: Repo / internal/provider/router.go] Retries: Router executes max 1 attempt per candidate; external CLI retry policy is frozen as benchmark provenance
├── 4. Evidence Integrity & Cache Isolation
│   ├── [FACT: Repo / Invariant] Media and plans keyed by CAS SHA-256
│   ├── [FACT: Repo / Invariant] Source extraction artifacts (ASR, RolePlan, Stems, TextRegion) reused across VI/EN for same AssetID
│   ├── [FACT: Repo / Invariant] Target adaptation artifacts (Translation, DubScript, VoiceAssignment, RenderPlan) run-scoped
│   └── [FACT: Issue #60 / #62] Remote translation cache keyed with service_baseline_id to prevent masking upstream API drift
├── 5. Profile Gating Rules
│   ├── [FACT: Issue #53 / #60] Local 8 GB: fail-closed against remote inference; completes on the 8192 MB target without OOM/crash with device-memory telemetry recorded; RTF <= 5.0x
│   ├── [FACT: Issue #53 / #62] Hybrid: Gateway priority order (Gemini -> DeepSeek -> Qwen), cost <= $0.25/min, RTF <= 2.0x
│   └── [FACT: Issue #53] Cloud: Conditional capability smoke only; failure does not block Local/Hybrid RC
├── 6. Metric Extraction & Aggregation
│   ├── [FACT: Issue #9 / #56] Acquisition: >= 98% success across 100 valid URLs; strict aweme_id match, duration tolerance
│   ├── [FACT: Issue #9 / #53] Quality: >= 90% PASS, <= 10% REVIEW_REQUIRED, <= 2% FAIL across runs
│   ├── [FACT: Issue #9] Sub-gates: CER <= 5% (category <= 10%, serious <= 1%), ASR/alignment P95 <= 250ms, dubbing-block >=95% within ±200ms, drift <= 100ms
│   └── [FACT: Issue #9] Category Balance: No category hides systematic failure behind global average
├── 7. Automated AV/QC Ingestion
│   ├── [FACT: Repo / internal/server/server.go] Append-only QualityResult ingested via POST /api/v1/quality-results
│   └── [FACT: Repo / internal/domain/review.go] Invariant: QualityResult.OverallStatus decoupled from StageExecution.Status
├── 8. Human Review Interface
│   ├── [FACT: Issue #53] 100% audit of all REVIEW_REQUIRED and FAIL outputs
│   └── [FACT: Delegated to Issue #57] PASS sampling formula input boundary delegated to Issue #57
└── 9. Orchestration Contract
    └── [FACT: Repo / CODING_STANDARDS] Public Seam 1 stage endpoint sequencing without private bypass or duplicate orchestrator
```

---

## 3. Run Identity, Session Lifecycle, and Resume Semantics

### 3.1 Identity Model
Benchmark execution preserves native domain entity identities while binding execution context:

1. **`BenchmarkSession`**:
   - A logical harness-level execution envelope that binds:
     - Corpus manifest identity and digest (`corpus_manifest_sha256`);
     - Execution profile (`local`, `hybrid`, `cloud`);
     - Target commit SHA;
     - Layered configuration snapshot digest;
     - Operator/environment attestation;
     - Start and completion timestamps.
   - The exact string serialization is an implementation detail; its binding to immutable input digests is the release invariant.
2. **`SourceAsset.ID` & `AssetCASHash`**:
   - Canonical media representation in CAS (`SourceAsset.ID` is a UUIDv4; `SHA256` represents media content bytes).
   - For acquisition: established upon container preflight and CAS commit.
   - For quality runs: pre-curated 24 source MP4s are pre-ingested into CAS with immutable SHA-256 digests.
3. **`LocalizationJob.ID`**:
   - Native UUIDv4 representing the task `SourceAssetID × TargetLanguage` (`vi` or `en`).
   - For 24 source assets, exactly 48 logical localization jobs exist.
4. **`LocalizationRun.ID`**:
   - Native UUIDv4 representing an execution attempt of a `LocalizationJob`.
   - Binds `JobID`, execution profile, configuration snapshot JSON, and timestamps.
   - SQLite table `localization_runs` persists lifecycle status: `queued` $\to$ `running` $\to$ `completed` | `interrupted` | `cancelled`.

### 3.2 Seam 1 Execution Sequence for Quality Runs
In the shipped codebase, `POST /api/v1/jobs/{id}/runs` creates and enqueues a `LocalizationRun` in SQLite (`RunStatusQueued`), but does not autonomously execute the multi-stage DAG. The public Seam 1 API provides versioned, asset-scoped stage execution endpoints.

To maintain Seam 1 compliance without introducing private package hooks or duplicating orchestrator business logic, the benchmark runner executes each run via the following public Seam 1 API sequence:

```
                          Client / Benchmark Runner
                                      │
 1. Create Job & Run                  ├─► POST /api/v1/jobs
                                      ├─► POST /api/v1/jobs/{id}/runs
                                      │
 2. Audio Role Planning               ├─► POST /api/v1/assets/{id}/audio-role-plan
                                      │
 3. Speech Understanding              ├─► POST /api/v1/assets/{id}/speech-understand
                                      │
 4. Meaning-First Translation         ├─► POST /api/v1/assets/{id}/translate
                                      │
 5. Dub Script Adaptation             ├─► POST /api/v1/assets/{id}/dub-script
                                      │
 6. Voice Assignment (Frozen)         ├─► POST /api/v1/assets/{id}/voice-assignment
                                      │
 7. TTS Fit & Synthesis               ├─► POST /api/v1/assets/{id}/dub-synthesize
                                      │
 8. Stem Separation & Remix           ├─► POST /api/v1/assets/{id}/separate-stems
                                      ├─► POST /api/v1/assets/{id}/audio-mix
                                      │
 9. Text Tracking & Subtitles         ├─► POST /api/v1/assets/{id}/detect-text
                                      ├─► POST /api/v1/assets/{id}/visual-track
                                      │
10. Render Plan & Final Render        ├─► POST /api/v1/assets/{id}/render-plan
                                      ├─► POST /api/v1/assets/{id}/render/final
                                      │
11. Review Items & Handoff            ├─► GET  /api/v1/runs/{id}/review-items
                                      ├─► POST /api/v1/runs/{id}/render/handoff
                                      │
12. AV/QC Report Ingestion            └─► POST /api/v1/quality-results
```

Each stage endpoint consumes inputs derived from prior CAS artifacts, persists its output to CAS, indexes the artifact in SQLite, and updates stage status in `stage_executions`.

### 3.3 Resume and Crash Recovery Contract
- **Artifact-First Preservation**:
  - `RuntimeHost` relies on SQLite write-ahead logging (WAL) and atomic CAS file writes (`.tmp` $\to$ `rename`).
  - On harness restart or failure recovery, the runner queries `GET /api/v1/runs/{id}` and `GET /api/v1/runs/{id}/stages`.
  - Stages in `succeeded` status with verified CAS hashes are **immutable and preserved**.
  - Stages in `running` or `cancelling` at daemon crash transition to `interrupted`.
- **Targeted Rerun**:
  - The runner re-invokes interrupted stages, passing existing CAS hashes from upstream dependencies.
- **Cache Reuse Boundary**:
  - **Source Extraction Artifacts**: `AudioRolePlan`, `TranscriptArtifact`, `AudioStemArtifacts`, and `TextRegionPlan` depend on `SourceAssetID` and verified model snapshot digests. When running VI and EN jobs for the same source video, these artifacts are computed once and reused.
  - **Target Adaptation Artifacts**: `TranslationVariant`, `DubScriptVariant`, `VoiceAssignment`, `DubSegmentsVariant`, and `RenderPlan` are run-scoped and language-specific.

### 3.4 Retry Accounting Contract
- **No Artificial Harness Retries**: The benchmark harness never wraps HTTP requests in ad-hoc retry loops.
- **Router Candidate Attempts**: `Router.ExecuteRoutedWithRetry(..., 1, ...)` specifies that the Router makes exactly **one attempt per eligible candidate**. It does not perform internal retry loops over the same candidate.
- **Adapter-Internal Retries**: Upstream CLI tools (e.g. `jiji`, `f2`) may execute internal retry/backoff logic. These tool-internal retries are frozen in the environment configuration and documented in `provider_attempts` and acquisition provenance.

---

## 4. Sequential Corpus Execution Topology

### 4.1 Single-Active-Run Invariant
Douyinie Phase 1 is architected for single-GPU 8 GB environments. To prevent resource contention and VRAM thrashing, benchmark execution is strictly sequential:
- No concurrent acquisition requests.
- No concurrent localization runs.
- No concurrent StageWorker subprocess families.

### 4.2 Acquisition Benchmark Execution (100 URLs)
Governed by Issue #56:
1. **Precondition ($T_0$ Oracle Gate)**: Run out-of-band Validity Oracle against candidate pool. Rotate verified dead/deleted sources from reserve. Freeze the 100-URL manifest.
2. **Sequential Loop**: For each URL in the frozen manifest:
   - Runner calls `POST /api/v1/sources/acquire` with canonical Douyin URL, operator `CredentialRef`, and rights attestation.
   - `AcquisitionService` runs the ladder (`jiji_douyin` $\to$ `f2_douyin` $\to$ `douyin_browser_assist`).
   - Validate Expected-Source Identity:
     - `SourceDescriptor.SourceID == douyin:aweme:<expected_aweme_id>`
     - `|PreflightReport.DurationMs - ExpectedDurationMs| <= duration_tolerance_ms`
     - `PreflightReport.ContainerValid == true`
     - Video stream/container valid and uncorrupted; audio presence matches the frozen per-entry `expected_audio_presence` oracle field (silent public videos are valid when audio is not expected).
   - Record attempt outcome, HTTP codes, latency, and container status.
3. **Threshold Gate**: $\ge 98$ successful acquisitions out of exactly 100 valid URLs.

### 4.3 Quality Benchmark Execution Matrix (48 Cases × 2 Profiles = 96 Runs)

#### Derived Requirement:
- **Baseline Corpus Definition**: The resolved V1 benchmark contract (Issue #9) and Issue #53 define the quality benchmark corpus as **24 representative Chinese-source videos × 2 target languages (VI + EN) = 48 localized outputs** (logical localization cases).
- **Two Hard Release Gates**: Issue #53, Issue #54, and Issue #58 mandate that **both Local 8 GB and Hybrid are hard execution-profile release gates**. Cloud remains an optional smoke/capability check.
- **Different Production Lanes**: Issues #60 and #62 establish that Local 8 GB and Hybrid execute materially distinct pipeline paths:
  - Local 8 GB routes translation through the offline, local Qwen3-4B GGUF adapter (`qwen3_4b_translator`) under strict 8192 MB VRAM and 5.0× RTF constraints;
  - Hybrid routes translation through the remote gateway ladder (`gemini-3.8-flash` $\to$ `deepseek-v4.1-flash` $\to$ local Qwen) under $\le \$0.25$/min API cost and 2.0× RTF constraints.
- **Reasoning**: A Release Candidate cannot claim compliance with both hard gates without executing both paths against the full 48-case evaluation surface. If only Local 8 GB ran 48 cases, the Hybrid gateway translation lane, API cost bounds, and 2.0× RTF would remain unevidenced. Conversely, if only Hybrid ran 48 cases, the local Qwen3-4B translation quality, real 8 GB no-OOM resource behavior, and offline fail-closed posture would remain unproven across the 7 mandatory categories.
- **Conclusion**: Executing 48 logical cases under Local 8 GB plus 48 logical cases under Hybrid ($48 \times 2 = 96$ localization runs) is a **derived requirement** of having two independent hard release gates.

#### Execution Order:
For each profile, execute videos sequentially:
$$\text{Video 01 (VI)} \to \text{Video 01 (EN)} \to \text{Video 02 (VI)} \to \text{Video 02 (EN)} \to \dots \to \text{Video 24 (EN)}$$
Executing VI and EN consecutively for the same asset maximizes cache reuse of source extraction artifacts (`TranscriptArtifact`, `AudioStemArtifacts`, `TextRegionPlan`).

---

## 5. Execution Profile Contracts & Resource Telemetry

### 5.1 Local 8 GB Profile (Hard Release Gate)
1. **Routing Policy (Fail-Closed)**:
   - Translation: Pinned `qwen3_4b_translator` (Qwen3-4B-Q4_K_M.gguf) running locally via Seam 2.
   - Remote Inference Prohibition: The Router must fail closed against any remote model/inference provider under `ExecutionProfileLocal`. Any remote model call during a Local quality run is an immediate benchmark **FAIL**.
   - Network Scope: Local quality runs operate strictly on pre-ingested CAS assets and local weights. Douyin URL acquisition is an independent phase that requires network; once assets are in CAS, local localization requires zero external network calls.
2. **GPU Lease Lifecycle (Seam 2)**:
   - Managed via `worker.GPULeaseManager`.
   - Between worker families (`ASR` $\to$ `Separator` $\to$ `OCR` $\to$ `TTS` $\to$ `Render`):
     - Terminate previous worker process tree via Windows Job Object.
     - Wait for exit and reap exit status.
     - Relinquish GPU lease; record `reclaimed` timestamp.
     - Acquire lease for next family.
3. **Hardware Telemetry (Host VRAM Observation)**:
   - *Architecture Boundary*: StageWorker Protocol v1 contains no in-band VRAM payload. GPULeaseManager validates lease handoff and process cleanup, not allocated VRAM bytes.
   - *Measurement Boundary*: Host GPU device telemetry via NVML (`nvmlDeviceGetMemoryInfo`) or `nvidia-smi` logging is **benchmark harness telemetry**, not a third architectural testing seam.
   - *External Fact (Windows WDDM Support)*: NVIDIA NVML documentation (`struct nvmlProcessInfo_v1_t`) states:
     > *"Under WDDM, NVML_VALUE_NOT_AVAILABLE is always reported because Windows KMD manages all the memory and not the NVIDIA driver."*
     Empirical verification on the target host confirms that `nvidia-smi --query-compute-apps=...` reports `[N/A]` for per-process memory on Windows 11 WDDM, while whole-device queries `nvidia-smi --query-gpu=memory.total,memory.used,memory.free` and NVML `nvmlDeviceGetMemoryInfo` return accurate physical device memory bytes.
   - *Measurement Method*: The benchmark harness samples whole-device memory via `nvmlDeviceGetMemoryInfo` (or `nvidia-smi --query-gpu=memory.used`) at regular intervals (e.g. 1 Hz) throughout execution, mapped against stage start/end timestamps.
   - *8 GB Resource Gate*: The Local profile must complete on the target 8192 MB GPU without OOM/crash. Record pre-run baseline, peak device-used memory, peak-above-baseline, and any CUDA allocation/OOM evidence. Under WDDM these are device-wide observations, not exact per-process allocation.
4. **Runtime Performance Gate**:
   - Aggregate Real-Time Factor: $\text{RTF}_{\text{local}} = \frac{\text{WallClockExecutionSec}}{\text{MediaDurationSec}} \le 5.0\times$.

### 5.2 Hybrid Profile (Hard Release Gate)
1. **Routing Policy (Gateway Translation)**:
   - Translation priority ladder (Issue #62):
     1. `gemini_gateway_translator` requesting `gemini-3.8-flash` via user gateway (`https://cliproxy.monet.uno/v1`).
     2. `deepseek_gateway_translator` requesting `deepseek-v4.1-flash` via user gateway.
     3. Pinned local `qwen3_4b_translator` GGUF fallback.
   - Remote Service Drift: Record `service_baseline_id`, `observed_model`, and `system_fingerprint` when returned by the gateway.
2. **Cost Gate**:
   - Total API expenditure across all remote provider attempts $\le \$0.25$ per source media minute.
   - Aggregated from `provider_attempts.cost_units` in SQLite across all stages.
3. **Runtime Performance Gate**:
   - Aggregate Real-Time Factor: $\text{RTF}_{\text{hybrid}} \le 2.0\times$.

### 5.3 Cloud Profile (Conditional Smoke Only)
- Optional capability smoke verification when external cloud providers are configured.
- Does not block Local 8 GB or Hybrid Release Candidate verdict.

---

## 6. Corpus Metric Extraction & Aggregation Contract

### 6.1 Metric Dimensions & Evidence Sources

| Dimension | Primary Evidence Surface | Schema / Artifact Field | Release Gate Threshold |
| :--- | :--- | :--- | :--- |
| **1. Source Acquisition** | `source_acquisitions`, `PreflightReport` | `ContainerValid`, `aweme_id`, `DurationMs` | $\ge 98\%$ success on 100 valid URLs |
| **2. Chinese ASR (CER)** | `TranscriptArtifact` vs Ground Truth | `TranscriptSegment.Tokens` | Overall CER $\le 5\%$, per-category $\le 10\%$ |
| **2a. ASR Serious Errors** | `TranscriptArtifact` vs Ground Truth | Categorized word errors (names, numbers, negation) | $\le 1\%$ of speech blocks |
| **2b. ASR Timing** | `TranscriptArtifact` vs Ground Truth | Segment/word start and end boundaries | P95 timestamp error $\le 250$ ms |
| **3. Meaning QA** | `TranslationVariant` | `OverallQAScore`, `PassedQAGate`, `Segments` | 0 critical errors (names, numbers, negation) |
| **4. Voice & Speaker** | `VoiceAssignment`, `DubSegmentsVariant` | `VoiceProfile.VoiceID`, `SpeakerID` | Frozen per run; 100% distinct multi-speaker presets |
| **5. Zero Overrun** | `DubSegmentsVariant` | `MeasuredDurationMs <= SlotDurationMs` | 100% compliance (overruns quarantined) |
| **5a. Inter-Turn Spacing** | `DubSegmentsVariant` | `NaturalGapAfterMs >= 0` | 0 adjacent speech collisions |
| **5b. Timing P95** | `DubSegmentsVariant` | `|MeasuredDurationMs - TargetDurationMs|` | $\ge 95\%$ within $\pm 200$ ms |
| **5c. Video Drift** | `DubSegmentsVariant` | Cumulative offset deviation | $\le 100$ ms whole-video drift |
| **6. Soundtrack Preservation** | `DubMixArtifact`, `AudioRolePlan` | Dialogue suppression windows; BGM outro | Dialogue suppressed only in speech windows; outro preserved |
| **7. Subtitle / Visual Text** | `LocalizedVisualTrack`, `TextRegionPlan` | `BoundingBox`, `SceneProtectedRegion` | Compact fit box; 0 protected UI collisions; scene text untouched |
| **8. Resource / Telemetry** | NVML logger, `stage_executions` | Wall-clock timestamps, baseline/peak device memory, OOM/crash evidence | Local completes on the 8192 MB target without OOM/crash and RTF $\le 5.0\times$; Hybrid RTF $\le 2.0\times$ |
| **9. Provider Cost** | SQLite `provider_attempts` | `cost_units` rolled up per source minute | Hybrid $\le \$0.25$ per source minute |
| **10. Run Classification** | `QualityResult`, `ReviewItem` | `OverallStatus`, pending review items | $\ge 90\%$ PASS, $\le 10\%$ REVIEW_REQUIRED, $\le 2\%$ FAIL |
| **11. Category Balance** | Grouped by 7 content categories | All metrics aggregated per stratum | No category hides systematic failure |

### 6.2 Meaning-First Translation QA Extraction
In the domain model (`internal/domain/translation.go`):
- `TranslationVariant.OverallQAScore`: Float score (0.0–1.0) aggregating meaning preservation.
- `TranslationSegment.PassedQAGate`: Boolean flag indicating rule-based validation passed.
- `TranslationSegment.QAConfidence`: Segment-level score.
- `TranslationSegment.KeyFacts`: Extracted facts, entities, numbers.
- `TranslationSegment.NegationPolarity`: Boolean polarity flag.
- Invariant: Zero critical errors permitted across the corpus. If any segment has `PassedQAGate == false` due to numerical discrepancy, corrupted name, or inverted negation, the run is flagged `REVIEW_REQUIRED` or `FAIL`.

### 6.3 Category Balance Invariant
The 24 quality benchmark videos represent 7 mandatory content dimensions (Issue #9):
1. Clean Single-Speaker Speech
2. Multi-Speaker Rapid Dialogue
3. Fast / Dense Speech Cadence
4. Loud BGM & Ambient Foley / SFX
5. Acoustic & Linguistic Edge Cases (dialects, numbers, names)
6. Dense Burned-in Dialogue Subtitles
7. Varied Clip Lengths & Pause Tempos

**Balance Gate**: A passing global average does not permit systematic failure in any single stratum. If any category exhibits $> 10\%$ CER, $> 20\%$ `REVIEW_REQUIRED`, or any unresolvable `FAIL`, the release gate is failed.

---

## 7. Automated AV/QC Ingestion & Review State Machine

### 7.1 Automated AV/QC Ingestion via Seam 1
Automated multimodal AV/QC results are ingested into `RuntimeHost` via the public Seam 1 endpoint:
`POST /api/v1/quality-results`

The request payload populates:
- `RunID`, `AssetID`, `JobID`, `TargetLanguage`, `Stage` (`"render"`);
- `OverallStatus` (`PASS` | `REVIEW_REQUIRED` | `FAIL`);
- `Metrics`: Array of `QualityMetric` (e.g. naturalness, soundtrack preservation, text elimination, A/V synchronization);
- `Issues`: Array of `ReviewItem`s flagged by automated evaluation;
- `Details`: Evaluator metadata (model, timestamp, evaluation parameters).

### 7.2 Decoupling Invariant
- `StageExecution.Status` records **operational execution** (`queued`, `running`, `cancelling`, `succeeded`, `failed`, `interrupted`).
- `QualityResult.OverallStatus` records **quality evaluation** (`PASS`, `REVIEW_REQUIRED`, `FAIL`).
- A stage execution may be `succeeded` while its `QualityResult` is `REVIEW_REQUIRED` or `FAIL`.

### 7.3 Run Classification State Machine
1. **`PASS`**:
   - Container preflight valid.
   - All pipeline stages `succeeded`.
   - `MeaningFirstQAGate` 100% passed (zero critical errors).
   - Zero overrun ($D_{\text{tts}} \le T_{\text{source}}$) and zero adjacent collision.
   - Automated QC metrics meet all thresholds.
   - Pending `ReviewItem` count in queue is exactly 0.
2. **`REVIEW_REQUIRED`**:
   - Pipeline stages `succeeded`.
   - Non-fatal exceptions present in `ReviewItem` queue:
     - Advisory cadence ratio ($0.50 \le \text{ratio} \le 1.50$);
     - Low OCR confidence on non-critical text;
     - Ambiguous audio role.
   - Run output is complete, but release requires human review adjudication.
3. **`FAIL`**:
   - Stage execution `failed` or `interrupted` (OOM, crash, timeout, missing CAS artifact).
   - Container corruption or desync $> 500$ ms.
   - Critical translation error (number/name/negation corruption).
   - Subtitle collision with protected UI region.
   - Unhandled Chinese vocal bleed into localized audio.

---

## 8. Human Review Interface & Issue #57 Boundary

### 8.1 Human Review Protocol (Issue #53)
Automated gates do not replace human accountability:
1. **100% Exception Audit**:
   - Every run with status `REVIEW_REQUIRED` or `FAIL` must be audited by an operator.
   - The operator inspects flagged `ReviewItem`s via `GET /api/v1/runs/{id}/review-items`.
   - Operator interventions use public Seam 1 endpoints:
     - Text edit: `POST /api/v1/runs/{id}/inspector/correct-text`
     - Voice reassignment: `POST /api/v1/runs/{id}/inspector/reassign-voice`
     - Region adjustment: `POST /api/v1/runs/{id}/inspector/override-region`
     - Item override: `POST /api/v1/review-items/{id}/override`
   - **Append-Only Audit Invariant**: Operator overrides create an auditable `ReviewOverride` in SQLite. An override **never overwrites historical quality scores or erases original metrics**.
2. **Stratified PASS Audit Boundary (Handoff to Issue #57)**:
   - Issue #57 defines the exact sampling formula and strata selection for human inspection of automated `PASS` runs.
   - Issue #59 defines the **typed input boundary**: the benchmark aggregator accepts a list of audited `PassAuditRecord`s from Issue #57:
     ```
     PassAuditRecord {
       RunID: string,
       AuditorID: string,
       Category: string,
       TargetLanguage: string,
       ConfirmedPass: bool,
       Notes: string,
       AuditedAt: time.Time
     }
     ```
   - If any sampled `PASS` run is rejected by human audit as a false pass, that run transitions to `FAIL` and triggers release gate re-evaluation.

### 8.2 Final Render Handoff
Readiness for final render is queried via `POST /api/v1/runs/{id}/render/handoff`:
- `ReviewPostureAuto`: Resumes final rendering automatically when the review queue reaches zero.
- `ReviewPostureReview`: Pauses at queue-zero, requiring explicit operator confirmation.

---

## 9. Release Candidate Verdict Aggregation Interface (Input to Issue #58)

The benchmark execution contract produces a structured, typed rollup payload consumed by the Release Candidate evidence bundle (Issue #58). The typed contract defines:

1. **Benchmark Context**:
   - Session identifier, commit SHA, profile, target hardware metadata, evaluation timestamp.
2. **Acquisition Rollup**:
   - `TotalURLs`: exactly 100.
   - `SuccessfulAcquisitions`: count ($\ge 98$ required).
   - `FailedAcquisitions`: count ($\le 2$ allowed).
   - `SuccessRate`: float.
   - `Verdict`: `PASS` | `FAIL`.
3. **Quality Rollup (per Profile)**:
   - `Profile`: `local` or `hybrid`.
   - `TotalRuns`: exactly 48 per profile.
   - `PassCount`: count.
   - `ReviewRequiredCount`: count.
   - `FailCount`: count.
   - `PassRate`: float ($\ge 0.90$ required).
   - `ReviewRate`: float ($\le 0.10$ required).
   - `FailRate`: float ($\le 0.02$ required).
   - `CategoryBreakdown`: per-category metrics ensuring balance.
   - `Verdict`: `PASS` | `REVIEW_REQUIRED` | `FAIL`.
4. **Sub-Gate Summary**:
   - ASR CER (overall, worst category, serious error rate, P95 timing).
   - Translation Meaning QA (critical errors count = 0).
   - Dubbing Timing (zero overrun compliance = 100%, P95 deviation, max whole-video drift).
   - Background Audio (dialogue suppression pass rate).
   - Visual Text (protected region collisions = 0).
   - Resource Telemetry (Peak VRAM in MB, aggregate RTF, provider cost per source minute).
5. **Human Audit Attestation**:
   - Exception audit completion (100% of review/fail runs adjudicated).
   - PASS audit confirmation matching the Issue #57 sample set.

---

## 10. Proposed Domain Glossary Additions (for future CONTEXT.md update)

The following domain terms are established by this contract and proposed for future inclusion in `CONTEXT.md` (no edits made to `CONTEXT.md` in this ticket):

1. **`BenchmarkSession`**:
   A logical harness execution envelope binding an immutable corpus manifest digest, target execution profile (`local`, `hybrid`, `cloud`), operator attestation, and git commit SHA.
2. **`ValidityOracle`**:
   An out-of-band precondition verification mechanism that proves candidate Douyin URLs are active, accessible public video posts at $T_0$, preventing external link rot from corrupting the $\ge 98\%$ acquisition reliability measurement.
3. **`ExpectedSourceContract`**:
   An immutable per-video identity specification (canonical `aweme_id`, pinned duration tolerance, audio presence, expected category) enforced to detect wrong-video redirects, truncated downloads, or false successes.
4. **`CorpusGateRollup`**:
   A deterministic aggregation contract that computes global release thresholds ($\ge 98\%$ acquisition, $\ge 90\%$ PASS, $\le 10\%$ REVIEW_REQUIRED, $\le 2\%$ FAIL) and the category-balance invariant across a multi-run corpus.
5. **`ExceptionFirstReviewProtocol`**:
   Quality governance workflow combining 100% human audit of all `REVIEW_REQUIRED` and `FAIL` outputs with a deterministic stratified sample of automated `PASS` outputs.
6. **`RealTimeFactor` (`RTF`)**:
   Execution efficiency metric defined as total wall-clock pipeline execution time divided by source media duration ($\text{WallClockSec} / \text{MediaSec}$). Enforced $\le 5.0\times$ for Local 8 GB and $\le 2.0\times$ for Hybrid.

---

## 11. Known Source-Grounded Contradictions & Gaps

1. **Router ExecutionProfileLocal Leak (`internal/provider/router.go`)**:
   - *Contradiction*: `ExecutionProfileLocal` adjusts candidate scoring weights, but does not strictly eliminate remote/cloud providers from candidate eligibility.
   - *Remedy*: The Router must enforce a hard fail-closed eligibility check: for `ExecutionProfileLocal`, remote model providers are filtered out before scoring.
2. **`cliAcquisitionProvider.Probe` HTTP 403 False Dead-Source Mapping (`internal/provider/acquisition_cli.go`)**:
   - *Contradiction*: The adapter maps any HTTP 403 response during redirect resolution to `CONTENT_UNAVAILABLE`.
   - *Remedy*: HTTP 403 alone must not be classified as dead source. It is scored as an acquisition failure unless the out-of-band Validity Oracle independently confirms that the source was deleted or made private.
3. **Missing Corpus-Level Aggregation State in SQLite**:
   - *Observation*: Current SQLite schema stores individual jobs, runs, and quality results, but lacks an entity representing a multi-video `BenchmarkSession` or release gate rollup.
   - *Remedy*: Aggregation is computed deterministically by the benchmark runner over the SQLite run records and CAS artifacts without requiring schema mutation.

---

## 12. External Documentation Verification & Evidence Attribution

| Topic | External / Primary Document | Documented Capability / Limitation | Benchmark Contract Impact | Evidence Class |
| :--- | :--- | :--- | :--- | :--- |
| **NVIDIA NVML on Windows (WDDM)** | NVIDIA NVML API Reference (`struct nvmlProcessInfo_v1_t`), URL: `https://docs.nvidia.com/deploy/nvml-api/structnvmlProcessInfo__v1__t.html` | Under Windows WDDM, per-process `usedGpuMemory` reports `NVML_VALUE_NOT_AVAILABLE`. | The benchmark must not claim per-process VRAM attribution from NVML on this host. Bind device-wide samples to stage timestamps and preserve the raw telemetry. | External Doc Fact + host observation |
| **NVIDIA device memory telemetry** | NVIDIA NVML API Reference (`nvmlDeviceGetMemoryInfo`), URL: `https://docs.nvidia.com/deploy/nvml-api/group__nvmlDeviceQueries.html` | Reports device-wide `total`, `free`, and `used` memory. It does not isolate Douyinie from desktop/other GPU consumers under WDDM. | Record pre-run baseline, peak device-used memory, and peak-above-baseline as resource evidence. The Local hard gate is empirical completion on the 8 GB target without OOM/crash; do not mislabel whole-device `used` as exact process allocation. | External Doc Fact + derived measurement rule |
| **Gemini 3.8 Flash** | Google DeepMind Gemini 3.8 Flash model card, URL: `https://deepmind.google/models/model-cards/gemini-3-8-flash/` | Gemini 3.8 Flash is an official Google model exposed through Gemini API channels. | `gemini-3.8-flash` is a legitimate upstream model identity. The custom gateway route still requires a live compatibility smoke and observed-response provenance. | External Doc Fact |
| **DeepSeek V4.1 Flash** | DeepSeek API Docs, URL: `https://api-docs.deepseek.com/` | Official model IDs include `deepseek-v4.1-flash`; service model names can advance to newer backend releases without becoming immutable checkpoint hashes. | The gateway request string `deepseek-v4.1-flash` is the user's gateway-specific route to the official upstream model family. Record requested route plus observed model/fingerprint when exposed and bind reusable remote artifacts to `service_baseline_id`. | External Doc Fact + gateway-specific contract |
| **Gateway response metadata** | User gateway live smoke + Issue #62 contract | OpenAI compatibility does not guarantee every optional response/vendor field on every backend. | Capture `model`, `system_fingerprint`, request/response ID, and timestamps only when actually returned; absence of an optional field is not fabricated or silently filled from model memory. | Runtime evidence requirement |
| **Acquisition CLI retry policy** | Exact installed/provisioned Jiji/F2 version, config, and upstream source/docs at implementation time | Tool-internal retry behavior is version-sensitive and separate from Router candidate attempts. | Freeze the exact adapter/tool version and retry configuration as provenance. Do not add harness-level retries and do not assume a generic upstream backoff policy without live evidence for the pinned version. | Version-sensitive implementation evidence |
| **Local Qwen inference memory** | Qwen/Qwen3-4B-GGUF model card + pinned runtime docs (Issue #60) | Weight size alone does not determine runtime VRAM; context/batch/runtime settings materially affect memory. | Local 8 GB support is proven by the real target-hardware benchmark with the exact snapshot/runtime/config, not by model file size. | External Doc Fact + empirical gate |
