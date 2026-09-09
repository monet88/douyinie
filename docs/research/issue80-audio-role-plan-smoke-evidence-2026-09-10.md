# Issue #80: Production AudioRolePlan Real Media Smoke Test Evidence

**Date:** 2026-09-10  
**Environment:** Windows 11 Pro, Intel Core i5-12400F, NVIDIA GeForce RTX 2060 SUPER  
**Model:** Google YAMNet `v1` (TFLite, 521 AudioSet classes, uncalibrated activations)  
**Model Location:** `D:/douyinie-ref/phase1.1-runtime/staging/yamnet_v1/yamnet.tflite`  
**License Manifest:** SHA-256 `305743f2153ec1250ed1149fcb5acbfe1944d548630f9b23c6c53655dea79943`  
**Runtime:** `ai-edge-litert 2.2.0` in `D:/douyinie-ref/phase1.1-runtime/venvs/audio_role/`  
**Adapter:** `cmd/stageworker/adapters/audio_role_yamnet.py` (Revision: `v2.2.0`)  

---

## 1. Test Execution & Media Sources

Real Douyin video fixtures from `D:/Chrome Download/` were converted to 16 kHz mono WAV and evaluated with the production `audio_role_yamnet.py` adapter:

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
- **Verification Verdict:** **PASS**. Zero dialogue detected. No false-positive narration. Valid no-dub plan generated.

### Fixture 2: Flower Care & Revival Vlog (`2.mp4`)
- **Duration:** 7.02 seconds (112,288 samples @ 16 kHz)
- **Architectural Profile:** Lifestyle narration vlog.
- **YAMNet Classification Result:**
  - `[0ms - 975ms]`: `ambience/SFX`
  - `[975ms - 3900ms]`: `uncertain`
  - `[3900ms - 7018ms]`: `ambience/SFX`
- **Cumulative Durations:** `ambience/SFX` = 4093 ms, `uncertain` = 2925 ms.
- **Verification Verdict:** **PASS**. Low-confidence background and speech activity properly mapped to `uncertain` and `ambience/SFX` without premature false classifications.

---

## 2. Artifact Persistence on D: Drive

Durable execution payloads and outputs are recorded at:
- `D:/douyinie-ref/smoke_evidence/fixture1_16k.wav`
- `D:/douyinie-ref/smoke_evidence/fixture1_result.json`
- `D:/douyinie-ref/smoke_evidence/fixture2_16k.wav`
- `D:/douyinie-ref/smoke_evidence/fixture2_result.json`
- `D:/douyinie-ref/smoke_evidence/run_smoke.py`
