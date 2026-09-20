# Douyinie Research & Evaluation Reports

This directory contains empirical research evaluations, provider smoke tests, and acceptance benchmark reports informing Douyinie architectural decisions.

---

## Index of Reports

1. **[Phase 1 E2E Live Acceptance Benchmark (2026-08-23)](phase1-e2e-acceptance-2026-08-23.md)**
   Comprehensive 5-video live acceptance test report proving dialogue-only dubbing, soundtrack preservation, multi-role visual text localization (`TextRegionPlan`), V3 compact subtitles, and strict zero-overrun timing.

2. **[Douyin Ingestion Reference Smoke Test](douyin-ingestion-reference-smoke-test.md)**
   Evaluation of Douyin download mechanisms, metadata extraction, and fallback strategies (Jiji downloader, F2, browser-assisted auth).

3. **[Neeyut-Blao Evaluation (2026-08-20)](neeyut-blao-evaluation-2026-08-20.md)**
   Architectural evaluation of T-blao/Neeyut-Blao for local-first Electron shell, engine manager, and worker protocol patterns.

4. **[OmniVoice & VoiceStudio Evaluation (2026-08-19)](omnivoice-voicestudio-evaluation-2026-08-19.md)**
   Evaluation of VoiceStudio and OmniVoice for local-first voice cloning, duration-controlled TTS, gap accounting, and license constraints.

5. **[Provider Smoke Test (2026-08-19)](provider-smoke-test-2026-08-19.md)**
   Smoke testing of baseline ASR, TTS, and stem separation engines across Vietnamese and English targets.

6. **[Qwen3-ASR & Forced Aligner Smoke Test (2026-08-19)](qwen3-asr-forced-aligner-smoke-2026-08-19.md)**
   Validation of Qwen3-ASR transcription accuracy, timestamp precision, and forced alignment capabilities on Chinese audio.

7. **[pyVideoTrans, Poiiky & CapAssistant Mining (2026-09-21)](pyvideotrans-poiiky-capassistant-mining-2026-09-21.md)**
   Technical mining of pyVideoTrans v4.13 core pipeline, Poiiky scraper/anti-detection, and CapAssistant VAR review / dynamic video stretching.
