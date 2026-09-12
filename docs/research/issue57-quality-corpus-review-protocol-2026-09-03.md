# Issue #57 — 24-Video VI/EN Quality Corpus & Exception-First Human Review Protocol

Date: 2026-09-03
Status: RESOLVED DESIGN CONTRACT (HITL decisions accepted 2026-09-03)
Parent: https://github.com/monet88/douyinie/issues/53
Issue: https://github.com/monet88/douyinie/issues/57
Authoritative references:
- Wayfinder Issue #1: https://github.com/monet88/douyinie/issues/1
- Issue #9 (Acceptance metrics & benchmark corpus): https://github.com/monet88/douyinie/issues/9
- Issue #16 (Final Architecture Lock): https://github.com/monet88/douyinie/issues/16
- Issue #18 (Phase 1 implementation spec): https://github.com/monet88/douyinie/issues/18
- Issue #53 (Phase 1.1 release hardening map): https://github.com/monet88/douyinie/issues/53
- Issue #54 (Release benchmark harness audit): https://github.com/monet88/douyinie/issues/54
- Issue #55 (RC provider, model, checkpoint, and license matrix): https://github.com/monet88/douyinie/issues/55
- Issue #56 (100-URL acquisition corpus & drift protocol): https://github.com/monet88/douyinie/issues/56
- Issue #58 (RC evidence bundle & release verdict): https://github.com/monet88/douyinie/issues/58
- Issue #59 (Benchmark execution & metric aggregation contract): https://github.com/monet88/douyinie/issues/59
- Issue #60 (Translation provider for Local 8 GB + Hybrid): https://github.com/monet88/douyinie/issues/60
- Issue #61 (Model snapshot & license enforcement): https://github.com/monet88/douyinie/issues/61
- Issue #62 (Production adapter compatibility & RC pinning): https://github.com/monet88/douyinie/issues/62
- Canonical architecture: `docs/architecture/phase1-architecture.md`
- Domain vocabulary: `CONTEXT.md`
- Target commit: `main@7bbac44969442444934338867db4abcb29a4d24b`

---

## 1. Grounding & Evidence Classification

To ensure strict discipline between hard repository constraints, external methodologies, engineering recommendations, and Human-in-the-Loop (HITL) decisions, every specification clause is labeled with its authority:
- **`[LOCKED FACT: REPO / TRACKER]`**: Non-negotiable decisions locked in `main`, `CONTEXT.md`, `phase1-architecture.md`, or resolved issues (#9, #53, #54, #56, #59, #60, #61, #62).
- **`[EXTERNAL STANDARD / METHODOLOGY]`**: Authoritative external methods (e.g. FunASR/SpeechIO `normalize_zh`, Levenshtein micro-CER, IWSLT dubbing evaluation, ITU-T P.808).
- **`[DERIVED REQUIREMENT]`**: Logically unavoidable consequences of combining locked facts (e.g. 48 logical cases across 2 independent hard gates = 96 total localization runs).
- **`[RECOMMENDATION]`**: Engineering proposal based on best practices, prior fixture evidence, and minimal complexity.
- **`[HUMAN DECISION: ACCEPTED]`**: Product/benchmark policy explicitly chosen by the user during this Wayfinder grilling session.

---

## 2. Locked Constraints vs Open Decisions Summary

| Dimension | Classification | Status & Authority |
|---|---|---|
| **Quality Corpus Size** | `[LOCKED FACT]` | Exactly 24 Chinese-source videos $\times$ 2 target languages (VI/EN) = 48 logical cases (Issue #9, Issue #53, Issue #59). |
| **Execution Profiles** | `[DERIVED REQUIREMENT]` | Local 8 GB and Hybrid are independent hard release gates, so the same 48 logical cases must be executed under both profiles for 96 profile executions; Cloud is smoke-only (Issue #53, Issue #59). |
| **Voice Posture** | `[LOCKED FACT]` | Preset voice naturalness, stable per-speaker assignment, distinguishability; cloning is NOT a hard gate (Issue #53, Issue #62). |
| **Architectural Seams** | `[LOCKED FACT]` | Exactly two testing seams: Seam 1 (REST API) and Seam 2 (NDJSON IPC); no third seam or private test hooks (Issue #16, Issue #59). |
| **Review Posture** | `[LOCKED FACT]` | Exception-first UI; 100% audit of automated `REVIEW_REQUIRED` and `FAIL`; stratified sample of automated `PASS` (Issue #9, Issue #53). |
| **Acceptance Thresholds** | `[LOCKED FACT]` | Acquisition $\ge 98\%$; CER $\le 5\%$ ($\le 10\%$ category); serious entity errors $\le 1\%$; alignment P95 $\le 250$ ms; zero overrun ($D_{\text{tts}} \le T_{\text{source}}$); timing deviation $\le 200$ ms for $\ge 95\%$; drift $\le 100$ ms; PASS $\ge 90\%$, REVIEW $\le 10\%$, FAIL $\le 2\%$; Category Balance (Issue #9, Issue #59). |
| **Corpus Category Quotas** | `[HUMAN DECISION: ACCEPTED]` | Seven mutually exclusive primary categories with quotas **3+4+4+3+3+4+3 = 24** for Clean / Multi-speaker / Fast-Dense / BGM-Ambience / Accent-Entity / Dense-Subtitle / Duration-Pause stress. |
| **Primary vs Overlap Rules** | `[HUMAN DECISION: ACCEPTED]` | Every video has exactly one primary category for gate accounting and PASS sampling, plus any number of secondary challenge tags for coverage analysis. |
| **Source Video Characteristics** | `[HUMAN DECISION: ACCEPTED]` | Coverage-based rather than codec-bucket overfitting: majority 9:16, at least 2 videos at 3:4, broad duration coverage with at least 2 videos $\ge 90$s, at least 16 videos with burned-in dialogue subtitles, and at most 1 visual/no-dub case. Exact codec/FPS/bitrate values remain inside the proven supported input envelope rather than becoming benchmark-only hard constants. |
| **Existing 5 Fixtures Mapping** | `[HUMAN DECISION: ACCEPTED]` | The 5 canonical fixtures remain historical grounding/candidates only; none occupies a release slot automatically. |
| **Reference Annotation Depth** | `[HUMAN DECISION: ACCEPTED]` | Full `ReferenceAnnotationPack` for every corpus and reserve asset: Chinese transcript + speaker IDs, gold timing, critical names/numbers/negation, VI/EN semantic references, subtitle regions, and secondary adjudication. |
| **Corpus Replacement Protocol** | `[HUMAN DECISION: ACCEPTED]` | Exactly 7 fully annotated reserve videos, one per primary category; replacement is pre-run only, 1-for-1 within the same category, auditable, and never permitted mid-run. |
| **Stratified PASS Sample Size** | `[HUMAN DECISION: ACCEPTED]` | Audit exactly one automated PASS execution for every `(profile, primary_category, target_language)` stratum: $2 \times 7 \times 2 = 28$ sampled PASS executions total. |
| **Stratified PASS Selection** | `[HUMAN DECISION: ACCEPTED]` | Deterministic SHA-256 ranking over manifest identity + stratum + asset identity; no HMAC secret is required. |
| **PASS Audit Escalation Rule** | `[HUMAN DECISION: ACCEPTED]` | Minor sampled-PASS defect $\to$ effective `REVIEW_REQUIRED`; critical defect $\to$ effective `FAIL`, immediate profile gate failure because $1/48 > 2\%$, plus 100% audit of remaining automated PASS executions in the same `(profile, primary_category)` stratum to diagnose QC blind spots. |
| **Data Architecture Boundary** | `[LOCKED FACT]` | No new product database tables, domain entities, or API endpoints. Human audit records are persisted in out-of-band JSONL logs and rollups. |

---

## 3. Corpus Composition & Stratification

### 3.1 Required Benchmark Dimensions
`[LOCKED FACT: Issue #9]` The 24-video quality corpus must cover the following specific stress dimensions:
1. Clean single-speaker speech.
2. Multi-speaker clips.
3. Fast/dense speech.
4. Loud BGM / noise / ambience.
5. Accent / dialect / code-switching, names, numbers, and negation-sensitive content.
6. Dense burned-in dialogue subtitles.
7. Varied clip lengths and speech/pause patterns.

### 3.2 Resolved Category Partition `[HUMAN DECISION: ACCEPTED]`

Every video is assigned exactly **one primary challenge category** for category-balance accounting and deterministic PASS sampling. Secondary challenge tags are allowed and should capture cross-cutting stressors without changing the primary denominator.

| Cat ID | Category Focus | Primary Pipeline Stress | Quota |
|---|---|---|:---:|
| **CAT-1** | **Clean Single-Speaker Narration** | Baseline ASR/TTS quality and source-relative cadence | **3** |
| **CAT-2** | **Multi-Speaker Dialogue** | Diarization, stable per-speaker voice assignment, speaker distinguishability | **4** |
| **CAT-3** | **Fast / Dense Tutorials** | Shorten-first translation, zero overrun, timing pressure, instructional UI text | **4** |
| **CAT-4** | **Loud BGM & Foley Ambience** | Dialogue suppression/separation and soundtrack/SFX preservation | **3** |
| **CAT-5** | **Accented / Entity-Dense Content** | Accent/dialect robustness; names, numbers, negation, code-switching | **3** |
| **CAT-6** | **Dense Burned-in Subtitles** | OCR/TextRegionPlan accuracy and compact overlay quality | **4** |
| **CAT-7** | **Extreme Duration & Dynamic Pauses** | Very short timing pressure, long-form drift, pause/breathing stability | **3** |
| **Total** | | | **24** |

Corpus-level secondary-tag minima:
- **At least 6 multi-speaker videos** across the full 24-video corpus.
- **At least 16 videos with burned-in Chinese dialogue subtitles**.
- **At most 1 visual/no-dub case**; all other corpus videos contain active dialogue requiring localization.

The quota intentionally spends the extra slots on multi-speaker, fast/dense, and dense-subtitle stress rather than on the clean baseline. A video that is both fast and noisy still belongs to one primary category while carrying secondary tags for the other stressors.

---

## 4. Source Video Technical Eligibility & Characteristics `[HUMAN DECISION: ACCEPTED]`

### 4.1 Platform & Supported-Input Envelope
1. **Platform**: `[LOCKED FACT: Architecture §4]` Public Douyin video posts with valid numeric `aweme_id`. No `/note/` galleries or live streams.
2. **Media preflight**: `[LOCKED FACT: internal/media/probe.go]` The real ffprobe-based preflight must succeed and report `ContainerValid == true`; current code defines that as parseable format metadata with at least one valid stream. Do not strengthen this into an unimplemented `moov`-atom rule.
3. **Codec/FPS/bitrate policy**: Do **not** turn one observed Douyin encoding pattern into benchmark-only hard constants. Every selected asset must pass the product's real preflight/probe path and remain inside the supported input envelope frozen with the RC evidence. Unsupported or pathological media is excluded before corpus freeze, not normalized into an artificial benchmark format.

### 4.2 Frame Geometry & Aspect Ratio
- The corpus is **majority 9:16 portrait**, reflecting the primary product surface.
- Include **at least 2 valid 3:4 portrait videos** to exercise non-9:16 layout behavior.
- No exact `20–22 / 2–4` split is a gate. Additional supported portrait geometries may be admitted only if they serve an explicit stress dimension and remain inside the product input envelope.

### 4.3 Duration Distribution
- The 24 videos must span **micro, short, medium, and long** source durations rather than concentrating on one easy clip length.
- Include **at least 2 videos with duration $\ge 90$s** to exercise accumulated drift and long-running resource behavior.
- Exact bucket boundaries and counts are not release gates; the frozen manifest records the actual distribution for auditability.

### 4.4 Burned-In Subtitles & Dialogue Ratio
- **At least 16 of 24 videos** must contain burned-in Chinese dialogue subtitles.
- **At most 1 video** may contain zero spoken dialogue and exercise the no-dub/original-audio branch; every other corpus video must contain active dialogue requiring localization.
- These are corpus-coverage requirements, not permission to overfit exact OCR confidence, subtitle position, or source styling.

---

## 5. Human Reference Annotations & Ground-Truth Standards

### 5.1 Annotation Structure (`ReferenceAnnotationPack`) `[HUMAN DECISION: ACCEPTED]`
Each source and reserve asset carries a complete, human-adjudicated reference pack. The contract requires the following **logical reference components**; exact file serialization is an implementation detail:

1. **Chinese source transcript + speaker identity**: verbatim characters, segment boundaries, speaker IDs, and meaningful fillers/colloquialisms needed for scoring.
2. **Gold timing/alignment**: human-verified source speech windows with sufficient granularity to compute the locked P95 alignment and dubbing-window metrics.
3. **Critical-token inventory**: names, brands/products, numerical quantities/dates, negation, and other facts whose corruption is release-critical.
4. **VI and EN semantic references**: human references preserving facts, entities, negation, and intent; spoken adaptation may be recorded separately where useful for dubbing-quality adjudication.
5. **Burned-in dialogue subtitle regions**: expected Chinese text plus spatial/temporal regions for videos where dialogue subtitles exist.
6. **Secondary adjudication**: a second qualified reviewer resolves material transcript, timing, translation, entity, or subtitle-reference disagreements before the corpus manifest is frozen.

### 5.2 Objective Ground-Truth Evaluation Standards `[EXTERNAL STANDARD]`

1. **Chinese ASR Character Error Rate (CER)**:
   - `[EXTERNAL STANDARD: FunASR / SpeechIO / jiwer]` Normalization via `normalize_zh`:
     - Strip Zhon stop and non-stop punctuation;
     - Strip whitespace, tabs, newlines;
     - Full-width to half-width character conversion;
     - Lowercase Latin characters;
     - Preserve CJK ideographs (`\u4e00-\u9fff`).
   - Micro-CER computed via Levenshtein distance across all reference characters:
     $$\text{micro-CER} = \frac{\sum (S + D + I)}{\sum N_{\text{ref}}} \times 100\% \quad (\le 5.0\% \text{ overall}, \le 10.0\% \text{ per category})$$
2. **ASR Serious Entity & Negation Errors**:
   - `[LOCKED FACT: Issue #9]` Evaluated against `critical_entities.json`. Any distortion, substitution, or deletion of a tagged name, number, or negation token flags a serious error. Threshold: $\le 1.0\%$ of speech blocks.
3. **ASR Alignment Timing P95**:
   - `[LOCKED FACT: Issue #9]` Absolute difference between hypothesis boundaries and gold boundaries. Threshold: $P_{95} \le 250$ ms.
4. **Dubbing Zero Overrun & Timing**:
   - `[LOCKED FACT: Issue #9, Issue #18, CONTEXT.md]` Zero overrun: $D_{\text{tts}} \le T_{\text{source}}$ ($100\%$ compliance). Timing deviation $\le 200$ ms for $\ge 95\%$ of blocks; whole-video drift $\le 100$ ms.
5. **Subjective Human Review Scales (1–5)**:
   - IWSLT/isometric-dubbing and ITU subjective-evaluation material inform the review methodology; **the numeric gates below come from Douyinie's resolved Issue #9, not from those external standards**.
     - Semantic Fidelity: $\ge 4.5 / 5.0$ (0 critical translation errors).
     - Spoken Naturalness: $\ge 4.2 / 5.0$.
     - Voice Naturalness: $\ge 4.0 / 5.0$.
     - Background / SFX Preservation: $\ge 4.2 / 5.0$.
     - Subtitle Visual Replacement: $\ge 4.0 / 5.0$.

---

## 6. Corpus Immutability & Replacement Protocol `[HUMAN DECISION: ACCEPTED]`

### 6.1 Pre-Ingested CAS Media Contract `[LOCKED FACT: Issue #59]`
- Quality source videos are pre-ingested into local CAS (`media_cas_sha256`).
- Because all 24 media files are stored locally in content-addressed storage, the quality benchmark is completely immune to upstream Douyin web deletions or network 404/410 errors during execution.

### 6.2 Pre-Curated Reserve Pool
- Maintain exactly **7 reserve videos**, one for each primary challenge category. Every reserve asset must already satisfy the same eligibility checks and carry a complete, secondary-adjudicated `ReferenceAnnotationPack` before the formal benchmark session is frozen.
- **Replacement Rules**:
  1. **Pre-Run Freeze Only**: Replacements are permitted **only before** the formal Release Candidate session is initialized and hashed. Mid-run replacement is strictly prohibited; a scored run is never repaired by silently swapping a corpus member.
  2. **Replacement Criteria**: Permitted only for verified legal/takedown notices, defective media container PTS/DTS sync, or unresolvable ground-truth annotation contradictions discovered during pre-flight dry runs.
  3. **Strict 1-for-1 Category Matching**: A retired video must be replaced by a reserve video with the exact same primary category.
  4. **Audit Log**: The replacement record must bind retired/replacement asset identities, category, reason, timestamp, operator, and old/new manifest digests.

---

## 7. Dual-Profile Evaluation Contract `[DERIVED REQUIREMENT: Issue #53, Issue #59]`

- **Independent Denominators ($N = 48$ each)**:
  - Total logical cases = 24 source videos $\times$ 2 target languages (VI/EN) = **48 logical cases**.
  - Local 8 GB: evaluates 48 runs, fails closed against remote model inference, must complete on the target RTX 2060 SUPER 8 GB without OOM/crash while recording device-memory telemetry, and must satisfy $\text{RTF} \le 5.0\times$.
  - Hybrid: evaluates 48 runs (gateway translation, cost $\le \$0.25/\text{min}$, $\text{RTF} \le 2.0\times$).
  - Total executions = $48 \times 2 = \mathbf{96 \text{ runs}}$.
- **No Cross-Profile Compensation**:
  - High scores in Hybrid cannot subsidize failures in Local. Both profiles must independently achieve:
    $$\text{PASS} \ge 90.0\%, \quad \text{REVIEW\_REQUIRED} \le 10.0\%, \quad \text{FAIL} \le 2.0\%$$
- **Artifact Caching & Isolation**:
  - Source extraction (ASR, AudioRolePlan, vocal separation, OCR) is shared across languages and profiles for the same source video.
  - Target adaptation (Translation, DubScript, TTS, AudioMix, Render) is strictly run-scoped and isolated.

---

## 8. 100% Exception-First Human Audit Protocol `[LOCKED FACT: Issue #53, Issue #59]`

- **100% Coverage**: Every run resulting in automated `REVIEW_REQUIRED` or `FAIL` must be audited by human reviewers.
- **Review Workflow via Seam 1**:
  - Reviewer inspects the run via `GET /api/v1/runs/{id}/review-items`, rendered video, and audio auditions.
  - Reviewer records manual overrides via `POST /api/v1/review-items/{id}/override`.
- **Invariants**:
  - `ReviewOverride` is an append-only audit record; it **never modifies historical automated QA scores**.
  - An automated `FAIL` run **can never be overridden to PASS** (it remains a failure unless attributed to source media corruption).

---

## 9. Deterministic Stratified PASS Sampling & Escalation Rules `[HUMAN DECISION: ACCEPTED]`

### 9.1 Stratified PASS Sampling Formula
Automated multimodal QC can have blind spots. Therefore, automated `PASS` outputs must be sampled and audited.

- For every hard-gate profile (`local`, `hybrid`), every primary category, and both target languages, audit exactly **one automated PASS execution** when that stratum contains at least one PASS.
- Nominal full sample size: $2 \text{ profiles} \times 7 \text{ categories} \times 2 \text{ languages} = \mathbf{28}$ sampled PASS executions.
- If a stratum contains no automated PASS because all of its executions are already `REVIEW_REQUIRED`/`FAIL`, there is no synthetic replacement sample; the exception audit already covers that stratum and the absence of PASS remains visible in the rollup.

### 9.2 Deterministic Selection Formula
Selection must be reproducible and independently verifiable without a secret key:
1. Retrieve eligible executions in stratum $(\text{profile}, \text{primary category}, \text{language})$ where automated `OverallStatus == "PASS"`.
2. For each candidate, compute `selection_digest = SHA256(corpus_manifest_sha256 || profile || primary_category || language || source_asset_id)` using an unambiguous canonical encoding.
3. Sort by `selection_digest`, then by `source_asset_id` as a deterministic tie-breaker.
4. Select the first candidate and preserve the input tuple plus digest as sampling proof.

### 9.3 Escalation Protocol on Sampled PASS Defect
- **Minor quality defect**: effective verdict becomes `REVIEW_REQUIRED`; recompute that profile's effective review rate against the locked $\le 10\%$ ceiling.
- **Critical defect / automated-QC blind spot**: effective verdict becomes `FAIL`. Because each profile denominator is 48, one effective failure is already $1/48 \approx 2.083\%$ and therefore fails the locked $\le 2\%$ hard-failure gate.
- After a critical sampled-PASS defect, audit **100% of the remaining automated PASS executions in the same `(profile, primary_category)` stratum across both languages**. This escalation is diagnostic evidence about whether the QC blind spot is systematic; it is not needed to make the already-mathematical profile failure true.

---

## 10. Data Architecture & Integration Boundaries `[LOCKED FACT: Architecture §10, Issue #59]`

- **Strict Boundary**: Do not add new database tables, domain entities, or API routes to Douyinie for benchmark review.
- **Persistence Mechanism**:
  - Standard product exceptions continue using `ReviewItem` and `ReviewOverride` in SQLite via existing Seam 1 endpoints (`/api/v1/review-items/{id}/override`).
  - Benchmark-specific audit logs (sampling seeds, Likert scores, PASS confirmation/downgrade records) are persisted in out-of-band JSONL files (`human_review_audit_log.jsonl`).
  - Aggregations are computed in-memory by the benchmark harness and output as `quality_corpus_gate_rollup.json` for consumption by Issue #58.

---

## 11. Grounding Audit of the Five Existing Fixtures `[RECOMMENDATION]`

The 5 existing E2E acceptance fixtures (`test/seam1/acceptance_gate_test.go`, `.ref/_live-tests/e2e-acceptance-20260823/`) serve as valuable engineering references, but **do not automatically satisfy release-corpus slots**:

1. **Video 1 (Matcha Ice)**: Visual-only, 0 speech lines, AAC passthrough. Eligible candidate for the single optional no-dub slot in CAT-7.
2. **Video 2 (Flower Care)**: 2-speaker lifestyle vlog. Used an obsolete full-width subtitle cover; eligible for CAT-2 only if re-annotated for V3 compact box.
3. **Video 3 (Baby Snack Drops)**: 15 micro dub slots, cartoon SFX. Eligible candidate for CAT-7 micro-clips.
4. **Video 4 (CapCut Tutorial)**: Fast speech ($>5.5$ char/s), UI buttons, V3 compact box. Excellent candidate for CAT-3.
5. **Video 5 (5 Camera Angles)**: $1080 \times 1440$ 3:4 aspect ratio, Foley SFX. Excellent candidate for CAT-4 or 3:4 geometry.

- *Conclusion*: Any of the 5 assets may be re-qualified as release-corpus candidates only after they satisfy the resolved eligibility and `ReferenceAnnotationPack` contract. Therefore the final curation effort may require **19 to 24 new source assets**, depending on how many historical fixtures actually re-qualify; none is grandfathered in automatically.

---

## 12. Final Human Resolution (Wayfinder HITL)

On 2026-09-03, the user explicitly accepted **all four recommended decision branches**:

1. **Category model**: one primary category + secondary tags; quotas `3+4+4+3+3+4+3`; corpus-level coverage includes at least 6 multi-speaker videos, at least 16 burned-in-dialogue-subtitle videos, and at most one no-dub visual case.
2. **Source characteristics**: coverage-based rather than hard-coded codec buckets; majority 9:16, at least 2 valid 3:4 videos, broad duration coverage including at least 2 videos $\ge 90$s; historical fixtures are candidates only.
3. **Ground truth + replacement**: full secondary-adjudicated `ReferenceAnnotationPack`; exactly 7 fully annotated reserve videos (one per primary category); pre-run, same-category, audited replacement only; no mid-run substitution.
4. **PASS audit**: one sampled automated PASS per `(profile, primary_category, language)` stratum (nominally 28 total), deterministic SHA-256 ranking, minor defects become effective `REVIEW_REQUIRED`, and critical defects become effective `FAIL` plus 100% same-profile/category PASS audit for QC-blind-spot diagnosis.

No material HUMAN decision remains for Issue #57. **FRONTIER_CLEAR.**

---

## 13. External Methodology Evidence Consulted

- SpeechIO Chinese text normalization: https://github.com/speech-io/chinese_text_normalization — confirms Chinese speech-processing TN is task-specific and documents punctuation removal and normalization of non-standard words such as numbers, dates, money, percentages, and phone numbers.
- IWSLT 2022 Isometric Spoken Language Translation: https://iwslt.org/2022/isometric — evaluates translation quality together with source/target length compliance for synchrony-sensitive use cases such as automatic dubbing.
- IWSLT 2024 evaluation campaign: https://aclanthology.org/2024.iwslt-1.1/ — confirms automatic dubbing and speech-to-speech translation as formal shared-task evaluation areas.

These sources inform methodology only. Douyinie's numeric release thresholds remain those already resolved in Issue #9 and later Phase 1.1 decisions.
