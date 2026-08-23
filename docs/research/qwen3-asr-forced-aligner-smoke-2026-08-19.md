# Qwen3-ASR + Forced Aligner smoke test — 2026-08-19

## Conclusion

Qwen3-ASR + Qwen3 Forced Aligner is viable on the local RTX 2060 SUPER 8 GB when audio is processed in bounded chunks. A single 445 s inference pass is not a suitable 8 GB execution profile.

- Tested upstream `QwenLM/Qwen3-ASR` commit `7c6daf77a2421100f5fb066495372c00129d39ff`.
- Environment: PowerShell 7, Python 3.12.13, `qwen-asr` 0.0.6, Torch 2.8.0+cu128, RTX 2060 SUPER 8 GB.
- Smoke models: `Qwen/Qwen3-ASR-0.6B` + `Qwen/Qwen3-ForcedAligner-0.6B`, FP16, Transformers backend, batch size 1, no FlashAttention.
- Source: authenticated Jiji Douyin sample `7659396126545619322`, normalized to 16 kHz mono PCM.

## 60-second baseline

- The first 60.0 s transcribed coherently as Chinese and produced aligned character-level timestamps spanning the clip.
- ASR + alignment inference took 17.346 s, RTF 0.289.
- Peak allocated VRAM was ~4.080 GiB.
- First model load included weight downloads; cached execution is the relevant steady-state profile.
- Python stdout initially hit Windows `cp1252` when printing Chinese despite using PowerShell 7. Subsequent Python runs explicitly set `PYTHONUTF8=1`; this was an output-encoding issue, not an ASR failure.

## Whole-file one-pass probe

- Full normalized audio duration: 445.105 s.
- One-pass ASR + alignment consumed roughly 7.4–7.6 GiB GPU memory, frequently reached 100% GPU utilization, and had not returned a result after more than 11 minutes.
- The benchmark was intentionally terminated rather than treating near-OOM, unbounded latency as an acceptable desktop profile.
- No transcript result from that aborted whole-file probe is used as evidence.

## Chunked full-video probe

- Strategy: 60 s chunks with 2 s overlap, one resident ASR + aligner model pair, sequential inference, timestamps shifted into source time.
- All 445.105 s completed in 8 chunks.
- Total inference time: 112.593 s; aggregate mean RTF 0.245.
- Maximum peak allocated VRAM across chunks: ~4.125 GiB.
- Per-chunk RTF ranged from ~0.227 to ~0.268 for the 60 s chunks; the final 39.105 s chunk measured ~0.253.
- Timestamp counts per chunk ranged from 283 to 437 and covered the complete source duration.

## Architecture implications

- Default 8 GB desktop execution should chunk ASR/alignment rather than send a multi-minute source through one monolithic request.
- Chunk overlap is only an inference safety mechanism. Production code must merge/deduplicate overlap boundaries before creating durable speech blocks.
- ASR, forced alignment, diarization, and speech-block segmentation remain separate responsibilities even when ASR and alignment share one model-loading wrapper.
- The 0.6B model proves local viability; it does not lock final recognition quality. The 1.7B model remains a benchmark challenger if accuracy gains justify additional memory/latency.
- The next de-risk target is a 30–60 s vertical localization slice using these timestamps, followed by provider/architecture lock.

Artifacts: `.ref/_live-tests/qwen3-asr/` (git-ignored).
