# OmniVoice and VoiceStudio evaluation — 2026-08-19

## Conclusion

OmniVoice is a high-priority TTS benchmark candidate for Douyinie Phase 1/Phase 2. VoiceStudio is primarily an orchestration/product reference built around OmniVoice and other engines, not a separate TTS model competitor.

- `k2-fsa/OmniVoice` supports 646 languages, zero-shot voice cloning, voice design, native speed control, and fixed-duration generation.
- Its language table includes Chinese, English, and Vietnamese; Vietnamese has 8,481.98 hours of reported training data.
- For timeline-constrained dubbing, `duration=<seconds>` is directly relevant; disabling post-processing avoids silence trimming when exact output length is required.
- `debpalash/VoiceStudio` uses OmniVoice as its default TTS engine and adds dubbing, ASR, diarization, Demucs separation, translation dispatch, APIs, job persistence, and multi-engine routing.
- VoiceStudio's Smart Fit planner is directly relevant to Douyinie timing architecture and should be mined as a design reference.

## Revisions checked

- `k2-fsa/OmniVoice`: latest checked commit `38e992bc60f85548faeb77e8fa70158ba71deb30` (2026-08-05).
- `debpalash/VoiceStudio`: latest checked commit `2d5f2e800e5564e366d9154125fa8d0c59c2b8b2` (2026-08-16).

## OmniVoice findings

- Apache-2.0 repository license; Python >=3.10; Torch/Torchaudio-based.
- README reports 600+ languages, zero-shot cloning, voice design, pronunciation/non-verbal controls, and RTF as low as 0.025 on benchmarked hardware.
- Recommended clone reference is 3–10 seconds; cross-lingual cloning can carry source-language accent.
- `duration` overrides `speed`; `postprocess_output=False` preserves the requested fixed duration rather than trimming trailing silence.
- Long-form generation chunks text automatically to keep VRAM usage nearer constant.
- Current `uv` config targets PyTorch 2.8 / CUDA 12.8 on Windows/Linux.
## VoiceStudio findings

- Local-first desktop application with 16 TTS engines, 11 ASR engines, local REST/SSE/WebSocket API, OpenAI-compatible audio API, and MCP server.
- Default VoiceStudio TTS backend wraps `k2-fsa/OmniVoice`; the app also supports alternative local engines and isolated/subprocess variants.
- Dubbing pipeline includes ASR, translation, per-speaker cloning/consistent reference selection, synthesis, vocal/background separation, mixing, and export.
- Smart Fit consumes natural TTS durations and source slots, absorbs silent-gap slack, allows mild pitch-preserving audio stretch, optionally splits larger mismatch between audio speed-up and video slow-down, and records residual overflow.
- Default Smart Fit limits in the checked source: audio-only <=1.2x, audio cap 1.5x, video slow cap 2.0x, underrun floor 0.85x, hard audio-only guard 1.8x.
- VoiceStudio app code is AGPL-3.0; its license notice says commercial/internal use is permitted, while modified network-accessible deployments carry AGPL source obligations. The bundled OmniVoice package remains separately Apache-2.0.
- VoiceStudio explicitly advises leaving FlashInfer off on tight-VRAM GPUs because fused copies add extra VRAM. Its general recommendation is 8 GB+ VRAM, but this is not proof that OmniVoice will fit the local RTX 2060 SUPER 8 GB in the desired mode.

## Implications for Douyinie

- Add OmniVoice as a direct benchmark/provider candidate rather than depending on VoiceStudio for TTS.
- Mine VoiceStudio's provider registry, job lifecycle, consistent-speaker reference selection, memory routing, and Smart Fit planner rather than importing the whole application.
- OmniVoice fixed-duration synthesis could reduce the number of translate/rewrite retries, but quality under aggressive duration constraints must be measured before making it the timing authority.
- Keep the existing fallback structure: synthesize naturally, measure, then use text rewrite and/or mild pitch-preserving time-stretch when fixed-duration synthesis sounds degraded.
- Because the local GPU is RTX 2060 SUPER 8 GB, benchmark standard OmniVoice first with FlashInfer disabled, one resident model, one worker, and short segments.

## Recommended smoke test

1. Clone one clean 5–10 s reference voice.
2. Generate the same VI and EN sentences at natural speed and at fixed 4.0 s duration.
3. Compare `num_step=32` versus 16.
4. Measure cold load, warm latency/RTF, peak VRAM, output duration, pronunciation, speaker similarity, and artifacts.
5. If standard CUDA is too tight, evaluate CPU/offload or VoiceStudio's OmniVoice GGUF path separately rather than enabling FlashInfer.

## Sources

- https://github.com/k2-fsa/OmniVoice
- https://github.com/debpalash/VoiceStudio
## Local OmniVoice smoke test — RTX 2060 SUPER 8 GB

- Tested upstream commit `38e992bc60f85548faeb77e8fa70158ba71deb30` under PowerShell 7.6.4, Python 3.12.13, PyTorch 2.8.0+cu128, CUDA 12.8. Python 3.14 was rejected because upstream Torch 2.8 has no cp314 wheel.
- CUDA initialization passed on RTX 2060 SUPER (compute capability 7.5). The Hugging Face model snapshot is ~3.043 GiB; initial download+load measured 45.832 s, cached load 4.763 s.
- Resident model allocation was ~1.892 GiB VRAM; inference peak was ~2.159–2.161 GiB, so the tested standard fp16 path fits comfortably in 8 GB without FlashInfer.
- Voice-clone prompt creation from a clean 3.36 s Vietnamese VieNeu reference took 0.659 s.
- VI natural, 32 steps: 3.860 s audio in 1.530 s, RTF 0.396.
- VI fixed 4 s, 32 steps: 4.000 s audio in 1.356 s, RTF 0.339 when `postprocess_output=False,pad_duration=0`.
- VI fixed 4 s, 16 steps: 4.000 s audio in 0.639 s, RTF 0.160.
- EN natural, 32 steps: 5.040 s audio in 1.334 s, RTF 0.265.
- EN fixed 4 s, 16 steps: 4.000 s audio in 0.610 s, RTF 0.153.
- Important duration detail: `duration=4.0, postprocess_output=False` alone produced 4.200 s because default `pad_duration=0.1` adds 0.1 s at both ends. Exact wall-clock duration requires `pad_duration=0` as well.
- A 40 dB trim check showed fixed outputs were not mostly padding: VI 16-step had ~3.765 s active audio, EN 16-step ~3.915 s active audio, both with no detected trailing silence at that threshold.
- Empirical implication: OmniVoice is now a serious candidate for the Phase 1/Phase 2 TTS provider matrix. The remaining gate is subjective pronunciation/naturalness/speaker-similarity review, especially 16-step vs 32-step and cross-lingual cloning.

Artifacts: `.ref/_live-tests/omnivoice/bench-20260819/` (git-ignored).

## Real-speaker Douyin clone benchmark — 2026-08-19

- Real reference: first 5.760 s of the authenticated Jiji sample `7659396126545619322`, extracted as 24 kHz mono PCM after Qwen3-ASR + Forced Aligner produced matching Chinese text for the interval.
- Cached OmniVoice load took 10.166 s; creating the clone prompt from the real reference took 0.919 s.
- ZH natural, 32 steps: 2.760 s audio in 1.757 s, RTF 0.637, peak ~2.157 GiB VRAM.
- VI natural, 32 steps: 2.760 s audio in 1.404 s, RTF 0.509, peak ~2.157 GiB.
- VI fixed 4 s, 32 steps: exactly 4.000 s in 1.505 s, RTF 0.376, peak ~2.159 GiB.
- VI fixed 4 s, 16 steps: exactly 4.000 s in 0.769 s, RTF 0.192, peak ~2.159 GiB.
- EN natural, 32 steps: 3.870 s audio in 1.531 s, RTF 0.396, peak ~2.159 GiB.
- EN fixed 4 s, 16 steps: exactly 4.000 s in 0.767 s, RTF 0.192, peak ~2.159 GiB.
- Exact fixed duration again required `postprocess_output=False,pad_duration=0.0`.

### Round-trip ASR quality proxy

- Qwen3-ASR-0.6B transcribed the real-reference OmniVoice ZH and EN samples exactly after normalization (`match_ratio=1.0`).
- The real Chinese-reference -> Vietnamese samples scored ~0.83–0.85 and repeatedly produced recognition substitutions around the opening phrase and `tôn dáng`.
- Control check: prior OmniVoice Vietnamese samples cloned from a synthetic Vietnamese VieNeu reference scored `1.0` for both natural 32-step and fixed 16-step outputs.
- VieNeu itself scored ~0.84 against its requested sentence because Qwen3-ASR changed some wording while preserving meaning, so round-trip ASR is only an intelligibility proxy, not proof of a TTS pronunciation defect.
- Architecture implication: keep OmniVoice promoted as a strong provider candidate, but treat Chinese-reference -> Vietnamese cross-lingual cloning as an unresolved subjective quality gate. Human A/B listening is still required before selecting it as the default real-speaker Vietnamese clone path.

Artifacts: `.ref/_live-tests/omnivoice/real-speaker-20260819/` (git-ignored).
