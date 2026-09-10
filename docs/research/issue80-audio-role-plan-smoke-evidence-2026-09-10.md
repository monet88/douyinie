# Issue #80: Production AudioRolePlan Real Media Smoke Test Evidence

**Date:** 2026-09-10
**Environment:** Windows 11 Pro, Intel Core i5-12400F, NVIDIA GeForce RTX 2060 SUPER
**Model:** Google YAMNet `v1` (TFLite, 521 AudioSet classes, uncalibrated activations)
**Model Location:** `D:/douyinie-ref/phase1.1-runtime/staging/yamnet_v1/yamnet.tflite`
**License Manifest:** SHA-256 `305743f2153ec1250ed1149fcb5acbfe1944d548630f9b23c6c53655dea79943`
**Runtime:** `ai-edge-litert 2.2.0` in `D:/douyinie-ref/phase1.1-runtime/venvs/audio_role/`
**Adapter:** `cmd/stageworker/adapters/audio_role_yamnet.py` (Revision: `v2.2.0`)

---

## 1. Direct Python Adapter Probes (Model Sanity Only)

Real Douyin video fixtures from `D:/Chrome Download/` were converted to 16 kHz mono WAV and evaluated with the isolated `audio_role_yamnet.py` adapter to verify model sanity:

### Fixture 1: Matcha Melon Ice Recipe (`1.mp4`)
- **Duration:** 8.00 seconds (128,000 samples @ 16 kHz)
- **Architectural Profile:** Visual-only localization fixture (0 spoken lines, recipe steps with ambient sound effects and BGM).
- **YAMNet Classification Result:**
  - `[0ms - 975ms]`: `ambience/SFX`
  - `[975ms - 1950ms]`: `uncertain`
  - `[1950ms - 2925ms]`: `ambience/SFX`
  - `[2925ms - 4875ms]`: `uncertain`
  - `[4875ms - 6825ms]`: `ambience/SFX`
  - `[6825ms - 8000ms]`: `uncertain`
- **Cumulative Durations:** `ambience/SFX` = 3900 ms, `uncertain` = 4100 ms, `narration/dialogue` = 0 ms.
- **Semantics & Review Verdict:** **REVIEW_REQUIRED**. Because 4100 ms of audio is classified as `uncertain`, this plan does **NOT** constitute autonomous no-dub completion. Instead, it correctly halts automated dubbing and surfaces actionable review items to the operator queue.

### Fixture 2: Flower Care & Revival Vlog (`2.mp4`)
- **Duration:** 7.02 seconds (112,288 samples @ 16 kHz)
- **Architectural Profile:** Lifestyle narration vlog.
- **YAMNet Classification Result:**
  - `[0ms - 975ms]`: `ambience/SFX`
  - `[975ms - 3900ms]`: `uncertain`
  - `[3900ms - 7018ms]`: `ambience/SFX`
- **Cumulative Durations:** `ambience/SFX` = 4093 ms, `uncertain` = 2925 ms.
- **Semantics & Review Verdict:** **REVIEW_REQUIRED**. Low-confidence background and speech activity properly mapped to `uncertain` and `ambience/SFX`, preventing false-positive narration while flagging review.

---

## 2. Production Seam Smoke Test (Full Seam 1 + Seam 2 Pipeline)

A durable production-seam smoke test was executed via `TestSeam1_AudioRole_RealProductionSeam_SmokeEvidence` (`test/seam1/real_audio_role_seam1_test.go`), verifying the complete end-to-end production architecture:

1. **Exact License Manifest Bootstrap:**
   - `provider.BootstrapYAMNetLicenseManifest` validated and bootstrapped all 4 obligation layers.
   - Verified entry SHA256: `305743f2153ec1250ed1149fcb5acbfe1944d548630f9b23c6c53655dea79943`.

2. **Model Snapshot Verification & Fingerprinting:**
   - Primary artifact `yamnet.tflite` (SHA-256: `10c95ea3eb9a7bb4cb8bddf6feb023250381008177ac162ce169694d05c317de`) and dependency `yamnet_class_map.csv` (SHA-256: `cdf24d193e196d9e95912a2667051ae203e92a2ba09449218ccb40ef787c6df2`) registered in `SnapshotService`.
   - `CheckBindingFingerprints` passed; anti-tamper / anti-symlink enforcement validated.

3. **Router Candidate Selection:**
   - Router evaluated all candidates for stage `audio_role_plan`.
   - Selected `worker_yamnet` with score 9.80 under `domain.PolicyAllowed`.
   - `SelectionDecision` recorded with stage `audio_role_plan` and `SelectedProviderID: "worker_yamnet"`.

4. **StageWorker Subprocess IPC Execution (Seam 2):**
   - Spawned worker supervisor for family `audio_role` (`cmd/stageworker`).
   - Invoked Python virtual environment (`D:/douyinie-ref/phase1.1-runtime/venvs/audio_role/Scripts/python.exe`) running production adapter `cmd/stageworker/adapters/audio_role_yamnet.py`.
   - Reaped worker processes and reclaimed GPU lease deterministically.

5. **Canonical Plan & Attempt Persistence:**
   - Canonical `AudioRolePlan` persisted to SQLite and committed to CAS.
   - `ProviderAttempt` recorded with stage `audio_role_plan`, status `succeeded`, and `InputHash` bound to the deterministic `plan.ProvenanceHash`.

6. **Exception-First Review Projection:**
   - Uncertain intervals (4100 ms) projected into operator review queue.
   - Created `ReviewItem` of type `audio_role_uncertain` with status `pending`.
   - Proved that uncertain intervals prevent autonomous no-dub completion.

---

## 3. Durable Evidence Files on D: Drive (Outside Git)

Per repository standards against committing large ephemeral raw blobs into Git, durable execution payloads and traces are recorded at:
- `D:/douyinie-ref/smoke_evidence/fixture1_16k.wav` (Source WAV media)
- `D:/douyinie-ref/smoke_evidence/fixture1_result.json` (Raw adapter output)
- `D:/douyinie-ref/smoke_evidence/fixture2_16k.wav` (Source WAV media)
- `D:/douyinie-ref/smoke_evidence/fixture2_result.json` (Raw adapter output)
- `D:/douyinie-ref/smoke_evidence/production_seam_evidence.json` (Full production-seam trace artifact)
- `D:/douyinie-ref/smoke_evidence/run_smoke.py` (Local test runner)
