# Lightweight C++/ONNX StageWorker Engines for Windows & macOS ARM64 (Issue #112)

**Issue:** [#112](https://github.com/monet88/douyinie/issues/112) — *Research: Lightweight C++/ONNX StageWorker engines for Windows & macOS ARM64* (part of [#111](https://github.com/monet88/douyinie/issues/111), Cross-platform Desktop App with Lightweight Pipeline)
**Date:** 2026-09-19
**Branch:** `research/issue-112-lightweight-engines`
**Scope:** exact minimal binary runtimes, wheels, and model weights to run Faster-Whisper (CTranslate2), SenseVoice (Sherpa-ONNX), Sherpa-ONNX diarization + Silero VAD, RapidOCR (ONNX Runtime), and Edge-TTS/Piper-TTS **without PyTorch**, on Windows x64 (CPU / CUDA / DirectML) and macOS Apple Silicon (ARM64, CPU), measured against the current Qwen3-ASR + Qwen3-ForcedAligner + PyTorch-CUDA stack.

Evidence labels used throughout:

- `[MEASURED]` — read off this workstation or a downloaded artifact during this research pass.
- `[REGISTRY]` — read from a package registry / model hub API (PyPI, NuGet, Hugging Face, GitHub Releases).
- `[CITED]` — quoted from upstream documentation or a repo document.
- `[INFERENCE]` — derived arithmetic or reasoning from the above, not directly observed.

---

## 0. Answer in one paragraph

Every lane the desktop pipeline needs has a PyTorch-free ONNX/CTranslate2 runtime that is packaged and prebuilt for both Windows x64 and macOS ARM64, and the whole lightweight runtime — Python interpreter included — fits in **~0.16 GB of wheels** plus **~0.5–1.0 GB of model weights**, against **27.8 GiB of Python venvs (19.3 GiB of it four duplicate `torch` installs) plus 10.8 GB of model snapshots** measured on this workstation today. The engine set is: `ctranslate2` + `faster-whisper` (MIT) for Whisper-family ASR, `sherpa-onnx` (Apache-2.0) for SenseVoice ASR + pyannote-segmentation diarization + 3D-Speaker embeddings + Silero VAD + Piper VITS TTS, `onnxruntime` + `rapidocr` (MIT/Apache-2.0) over PP-OCRv4 ONNX weights for OCR, and Edge-TTS (cloud, LGPLv3) or Piper (MIT binary from `rhasspy/piper`; the `piper-tts` PyPI package is **GPL-3.0-or-later** and must not ship in a proprietary bundle). GPU remains optional and additive: on Windows the CUDA path costs one 1.50 GB runtime pack (CTranslate2) and/or a 595 MB sherpa-onnx CUDA archive; on macOS ARM64 there is **no GPU path at all** (neither CTranslate2 nor sherpa-onnx targets Metal/MPS) — the ARM64 builds are CPU-only (CTranslate2's macOS CPU path is a 1.27 MB wheel with a 2.97 MB dylib, i.e. system-BLAS/Accelerate-class CPU kernels).

---

## 1. Current baseline that must be beaten (measured on this workstation)

### 1.1 Python runtimes

`du -smL` over `D:\douyinie-ref\phase1.1-runtime\venvs\*` `[MEASURED]` (the venv matrix is documented in `docs/agents/live-runtime-e2e.md` §"venv matrix"):

| Venv | On-disk | Note |
| --- | ---: | --- |
| `asr` (junction → `D:\douyinie-ref\Qwen3-ASR\.venv`) | **7 865 MB** | `torch` alone: 7 088 MB |
| `diarizer` | **5 115 MB** | `torch` 4 215 MB + `torchaudio` 11 MB |
| `tts` | **5 203 MB** | `torch` 4 215 MB + `torchaudio` 11 MB |
| `separator` | **5 476 MB** | `torch` 4 215 MB + `torchaudio` 11 MB |
| `ocr` (junction → `D:\douyinie-ref\_live-tests\ocr-mask-20260819\.venv`) | **3 642 MB** | pip `nvidia-*` CUDA wheels: 2 321 MB |
| `translation` | **826 MB** | `llama-cpp-python` wheel (6.8 MB) + runtime |
| `tts-zerotts-0.1.5-47e466d` | **231 MB** | CPU ONNX pack |
| `audio_role` | **96 MB** | TFLite |
| **Total** | **28 454 MB ≈ 27.8 GiB** | `torch` is installed **four times** (19 733 MB ≈ 19.3 GiB) |

The venv entries `asr`/`ocr` are NTFS junctions onto other trees; a plain `du -sm` reports 0 for them, which is why `du -smL` is used here `[MEASURED]`.

### 1.2 Model snapshots

`D:\douyinie-ref\phase1.1-runtime\staging\` `[MEASURED]`, cross-checked against Hugging Face file metadata `[REGISTRY]`:

| Snapshot | On-disk (measured) | HF published |
| --- | ---: | ---: |
| `qwen3_asr_0_6b/model.safetensors` | 1.7 GB | `Qwen/Qwen3-ASR-0.6B` = 1 876.1 MB |
| `qwen3_asr_1_7b/` (2 shards) | 3.9 GB + 456.0 MB | `Qwen/Qwen3-ASR-1.7B` = 4 699.9 MB |
| `qwen3_forced_aligner_0_6b/model.safetensors` | 1.7 GB | `Qwen/Qwen3-ForcedAligner-0.6B` = 1 835.5 MB |
| `ocr_pp_ocrv6_det/inference.pdiparams` | 59.1 MB | Paddle PP-OCRv6 medium det (ONNX twin 62.03 MB) |
| `ocr_pp_ocrv6_rec/inference.pdiparams` | 72.9 MB | Paddle PP-OCRv6 medium rec (ONNX twin 76.55 MB) |
| `ocr_pp_lcnet_ori/inference.pdiparams` | 6.4 MB | text-line orientation classifier |
| `demucs_htdemucs/*.th` | 80.2 MB | separation lane (torch) |
| `uvr_mdx_net/UVR-MDX-NET-Inst_HQ_4.onnx` | 56.3 MB | separation lane (ONNX) |
| `qwen3_4b_translator/Qwen3-4B-Q4_K_M.gguf` | 2.3 GB | translation lane (llama.cpp) |
| `kokoro_v1/kokoro-v1_0.pth` | 312.1 MB | TTS candidate (torch) |
| `vieneu_v3_turbo/` (`model.safetensors` 249.7 MB + `speaker_encoder.onnx` 27.0 MB) | ~277 MB | TTS candidate |
| `yamnet_v1/yamnet.tflite` | 3.9 MB | audio-role lane |
| **Total** | **≈ 10.83 GB** | |

ASR-lane-only core (0.6B ASR + 0.6B aligner + OCR trio) = **3.54 GB** `[INFERENCE from MEASURED]`. The issue's estimates ("Qwen3-ASR 1.2–3.5 GB", "Aligner 1.2 GB") are **lower than the actual bf16 repositories**; the shipped 0.6B weights are 1.8 GB each.

### 1.3 Runtime cost actually paid at inference time

From `docs/research/qwen3-asr-forced-aligner-smoke-2026-08-19.md` `[MEASURED in-repo]` — RTX 2060 SUPER 8 GB, `Qwen3-ASR-0.6B` + `Qwen3-ForcedAligner-0.6B`, FP16, batch 1:

- 60.0 s clip → **17.346 s**, RTF **0.289**, peak allocated VRAM **≈ 4.080 GiB**.
- 445.1 s clip in 8 × 60 s chunks → **112.593 s**, mean RTF **0.245**, peak VRAM **≈ 4.125 GiB**.
- 445.1 s one-pass → ~7.4–7.6 GiB VRAM, no result after >11 min (aborted; the 8 GB profile must chunk).

This is the yardstick for §7: any lightweight engine must land inside the same RTF ≤ 5.0× Local-8GB gate (`docs/research/issue59-benchmark-execution-contract-2026-09-03.md` §4) while removing the 27.8 GiB runtime and the 4.1 GiB resident VRAM of the ASR pair.

---

## 2. Reference implementation: `.ref/CapCap`

`.ref/CapCap` is a checkout of the predecessor project (the same tree exists at `D:\douyinie-ref\CapCap`). It already ships the lightweight engine set this issue is asking about, so it is the strongest available precedent.

### 2.1 Pins

`.ref/CapCap/requirements-local.txt` (verbatim, comments included):

```
-r requirements-base.txt
edge-tts
soundfile
av
numpy
huggingface_hub
faster-whisper
# CUDA 12.8 / Blackwell-compatible CTranslate2 runtime. faster-whisper
# allows older 4.x releases, which may not support the bundled CUDA pack.
ctranslate2>=4.6.3,<5
vietnormalizer
piper-tts
onnxruntime-gpu
scipy
openai
librosa
rapidocr
opencv-python-headless
sherpa-onnx
```

`.ref/CapCap/requirements-base.txt`: `PySide6`, `requests`, `python-dotenv`, `pydub`, `python-mpv`.

Notable: **no `torch` anywhere**, one venv for everything (contrast with Douyinie's eight venvs), `sherpa-onnx` used for ASR *and* diarization *and* VAD, `rapidocr` + `onnxruntime-gpu` + `opencv-python-headless` for OCR, `av` (PyAV) for decoding, `piper-tts` for offline TTS plus `edge-tts` for cloud TTS.

### 2.2 Engine map and packaging

`.ref/CapCap/docs/technical-stack.md`:

| Area | Technology |
| --- | --- |
| Audio transcription | Faster-Whisper / CTranslate2, SenseVoice / Sherpa-ONNX |
| OCR | RapidOCR PP-OCRv4 with OpenCV and ONNX Runtime |
| Speaker diarization | Sherpa-ONNX |
| VAD | Silero VAD via Sherpa-ONNX |
| TTS | Piper, Edge TTS, CapCut TTS, VieNeu TTS |
| Packaging | PyInstaller |

Also documented there: "GPU Faster-Whisper uses CUDA when available, with standard inference as the safe path and optional batched inference controls"; "RapidOCR uses one GPU inference worker to avoid competing CUDA sessions"; NVENC with `libx264` failover.

### 2.3 Code-level facts worth copying

- `app/services/engine_runtime.py` — a lazy adapter registry (`ffmpeg`, `whisper`, `ocr`, `sensevoice`, `translator`, `tts`) with a remote-profile substitution for whisper/translator/TTS. This is the same shape as Douyinie's `cmd/stageworker` family dispatch, and it is what makes one process host multiple engines.
- `app/sensevoice_processor.py` — `sherpa_onnx.OfflineRecognizer.from_sense_voice(model=<model.int8.onnx>, tokens=tokens.txt, num_threads=4, use_itn=True, sense_voice_language=<lang>)`, with a `TypeError` fallback for the language kwarg name. It prefers `model.int8.onnx` when present and fails closed if `tokens.txt` is missing.
- `app/services/speaker_diarization_service.py` — sherpa-onnx only, CPU provider by default, `OfflineSpeakerSegmentationModelConfig(pyannote.model=...)` + `SpeakerEmbeddingExtractorConfig(model=<3dspeaker eres2net>)` + `FastClusteringConfig` + `OfflineSpeakerDiarization`. The file explicitly states it "does not import pyannote.audio or PyTorch".
- `app/ocr_processor.py` — two accepted OCR model sets: `ch_PP-OCRv4_det_mobile.onnx` / `ch_PP-OCRv4_rec_mobile.onnx` / `ch_ppocr_mobile_v2.0_cls_mobile.onnx` / `ppocr_keys_v1.txt`, and the newer `PP-OCRv6_det_small.onnx` / `PP-OCRv6_rec_small.onnx` / cls. It also probes the ONNX Runtime CUDA provider by `ctypes.WinDLL`-loading `onnxruntime/capi/onnxruntime_providers_cuda.dll`, because `get_available_providers()` reports compiled-in providers even when their dependent CUDA DLLs are absent — a real Windows trap.
- `app/services/resource_download_service.py` — first-class resource manager for every download: whisper zips, CUDA pack, SenseVoice zip, RapidOCR models, diarization segmentation + embedding, Piper voice packs, VieNeu. Sizes fetched from the same trees used for this report are listed in §3.1/§4/§5.
- `runtime_paths.py` — a SenseVoice model dir is only valid if `model.int8.onnx` **and** `tokens.txt` are both present.

---

## 3. Lane A — ASR

### 3.1 Faster-Whisper via CTranslate2

**Wheels (PyPI, 2026-09-19)** `[REGISTRY]`:

| Package | Version | License | File | Size |
| --- | --- | --- | --- | ---: |
| `ctranslate2` | 4.8.2 | MIT | `ctranslate2-4.8.2-cp312-cp312-win_amd64.whl` | 19.22 MB |
| `ctranslate2` | 4.8.2 | MIT | `ctranslate2-4.8.2-cp312-cp312-macosx_11_0_arm64.whl` | **1.27 MB** |
| `ctranslate2` | 4.8.2 | MIT | `ctranslate2-4.8.2-cp312-cp312-macosx_11_0_x86_64.whl` | 11.93 MB |
| `ctranslate2` | 4.8.2 | MIT | `ctranslate2-4.8.2-cp312-cp312-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl` | 39.56 MB |
| `ctranslate2` | 4.8.2 | MIT | `ctranslate2-4.8.2-cp312-cp312-manylinux_2_27_aarch64.manylinux_2_28_aarch64.whl` | 16.90 MB |
| `faster-whisper` | 1.2.1 | MIT | `faster_whisper-1.2.1-py3-none-any.whl` | 1.12 MB |
| `av` (PyAV) | 18.1.0 | BSD-3 | `av-18.1.0-cp311-abi3-win_amd64.whl` / `…-macosx_14_0_arm64.whl` | 27.60 MB / 18.22 MB |
| `tokenizers` | 0.23.2 | Apache-2.0 | `tokenizers-0.23.2-cp310-abi3-win_amd64.whl` | 2.86 MB |
| `numpy` | 2.5.3 | BSD-3 | `numpy-2.5.3-cp312-cp312-win_amd64.whl` | 12.57 MB |

`faster-whisper` 1.2.1 hard dependencies: `ctranslate2<5,>=4.0`, `huggingface-hub>=0.21`, `tokenizers<1,>=0.13`, `onnxruntime<2,>=1.14`, `av>=11`, `tqdm` `[REGISTRY]`. The `onnxruntime` dependency exists solely for the bundled VAD.

**What is actually inside the wheels** `[MEASURED by extracting the wheels]`:

| Wheel | Unpacked contents > 200 KB | Uncompressed total |
| --- | --- | ---: |
| `ctranslate2 … win_amd64` (19.22 MB) | `ctranslate2/ctranslate2.dll` 59.30 MB, `ctranslate2/libiomp5md.dll` 1.61 MB, `ctranslate2/cudnn64_9.dll` 0.27 MB, `ctranslate2/_ext.cp312-win_amd64.pyd` 0.72 MB | 62.3 MB |
| `ctranslate2 … macosx_11_0_arm64` (1.27 MB) | `ctranslate2/.dylibs/libctranslate2.4.8.2.dylib` 2.971 MB, `ctranslate2/_ext.cpython-312-darwin.so` 1.626 MB | 4.6 MB |
| `faster-whisper` (1.12 MB) | `faster_whisper/assets/silero_vad_v6.onnx` 1.25 MB | 1.4 MB |

Two consequences:

1. **No separate Silero download is needed on this lane** — faster-whisper ships its VAD ONNX asset inside the wheel `[MEASURED]`.
2. **`cudnn64_9.dll` in the Windows wheel is the cuDNN-9 front-end loader, not the engine.** The engine DLLs are external (see below), which is exactly why CapCap ships a "CUDA pack".

**Required native libraries** `[CITED faster-whisper README + MEASURED contents]`:

- CPU (Windows): `ctranslate2.dll` + `libiomp5md.dll` (both bundled) — nothing else.
- CPU (macOS ARM64): `libctranslate2.4.8.2.dylib` (bundled) + the system frameworks it links against; the wheel ships **no** BLAS dylib, i.e. it uses the OS-provided BLAS `[INFERENCE from the 4.6 MB unpacked size]`.
- GPU (Windows/Linux): cuBLAS **for CUDA 12** and cuDNN **9 for CUDA 12** `[CITED]`. faster-whisper's README is explicit: "The latest versions of `ctranslate2` only support CUDA 12 and cuDNN 9. For CUDA 11 and cuDNN 8 … downgrading to `3.24.0`; for CUDA 12 and cuDNN 8, downgrade to `4.4.0`."
- GPU (macOS): **not supported.** CTranslate2's hardware-support page lists CPU `x86-64 (SSE 4.1+)`/`AArch64` and GPU "NVIDIA GPUs with Compute Capability ≥ 3.5" only — there is no Metal/MPS/CoreML backend `[CITED]`.

The concrete Windows CUDA-12.8 file set, taken from the pack CapCap actually ships (`Hacht/CapCapResource/cuda12_fw_new`, listed by the resource manager as "GPU Acceleration Pack (CUDA 12.8, ~1.6 GB)", installed into `bin/cuda12_fw`, validated by the existence of `cublas64_12.dll`) `[MEASURED via HF API]`:

| DLL | Size |
| --- | ---: |
| `cublasLt64_12.dll` | 692.4 MB |
| `cudnn_engines_precompiled64_9.dll` | 513.9 MB |
| `cudnn_adv64_9.dll` | 282.4 MB |
| `cufft64_11.dll` | 276.1 MB |
| `cudnn_ops64_9.dll` | 126.5 MB |
| `cublas64_12.dll` | 113.7 MB |
| `nvrtc64_120_0.dll` | 86.7 MB |
| `cudnn_heuristic64_9.dll` | 56.8 MB |
| `cudnn_engines_runtime_compiled64_9.dll` | 20.2 MB |
| `nvrtc-builtins64_128.dll` | 6.4 MB |
| `cudnn_cnn64_9.dll` | 4.6 MB |
| `cudnn_graph64_9.dll` | 2.4 MB |
| `cudart64_12.dll` | 0.57 MB |
| `cudnn64_9.dll` | 0.27 MB |
| `cufftw64_11.dll` | 0.16 MB |
| **Unpacked total** | **≈ 2 183 MB** (zip: 1 500 649 700 B) |

`onnxruntime-gpu` also installs CUDA runtime pieces through pip (`nvidia-cudnn-cu13~=9.0`, `nvidia-cuda-runtime~=13.0`, …); its `win_amd64` wheel is 160.48 MB `[REGISTRY]`.

**Model weights** `[REGISTRY + MEASURED]`:

| Model | Repo | File | Size | License |
| --- | --- | --- | ---: | --- |
| faster-whisper base | [`Systran/faster-whisper-base`](https://huggingface.co/Systran/faster-whisper-base) | `model.bin` | 145.2 MB | MIT |
| faster-whisper small | [`Systran/faster-whisper-small`](https://huggingface.co/Systran/faster-whisper-small) | `model.bin` | 483.5 MB | MIT |
| faster-whisper large-v3 | [`Systran/faster-whisper-large-v3`](https://huggingface.co/Systran/faster-whisper-large-v3) | `model.bin` | 3 087.3 MB | MIT |
| large-v3-turbo (CT2) | [`deepdml/faster-whisper-large-v3-turbo-ct2`](https://huggingface.co/deepdml/faster-whisper-large-v3-turbo-ct2) | `model.bin` | 1 617.9 MB | MIT |
| (CapCap mirror) | `Hacht/CapCapResource/zipResource/` | `…faster-whisper-base.zip` / `-small.zip` / `-medium.zip` | 133.1 MB / 445.7 MB / 1 411.5 MB | MIT |

A CTranslate2 model directory is `model.bin` + `config.json` + `tokenizer.json` + `vocabulary.json`/`preprocessor_config.json` (downloaded by `huggingface_hub` on first use, or staged offline exactly as Douyinie already stages snapshots).

**Measured/upstream speed** `[CITED]` — faster-whisper README, 13 minutes of audio:

- large-v2, **RTX 3070 Ti 8 GB**, CUDA 12.4, beam 5: fp16 1 m 03 s (4 525 MB VRAM), fp16 + `batch_size=8` **17 s** (6 090 MB), int8 59 s (2 926 MB), int8 + `batch_size=8` 16 s (4 500 MB).
- small model on **CPU**, 8 threads, Intel i7-12700K: fp32 2 m 37 s (2 257 MB RAM), int8 **1 m 42 s** (1 477 MB), int8 + `batch_size=8` 51 s (3 608 MB).

### 3.2 SenseVoice via Sherpa-ONNX

**Python packaging** `[REGISTRY]`:

| Package | Version | License | File | Size |
| --- | --- | --- | --- | ---: |
| `sherpa-onnx` | 1.13.8 | Apache-2.0 | `sherpa_onnx-1.13.8-cp312-cp312-win_amd64.whl` | 2.29 MB |
| `sherpa-onnx` | 1.13.8 | Apache-2.0 | `sherpa_onnx-1.13.8-cp312-cp312-macosx_11_0_arm64.whl` | 2.15 MB |
| `sherpa-onnx-core` | 1.13.8 | Apache-2.0 | `sherpa_onnx_core-1.13.8-py3-none-win_amd64.whl` | 16.9 MB |
| `sherpa-onnx-core` | 1.13.8 | Apache-2.0 | `sherpa_onnx_core-1.13.8-py3-none-macosx_11_0_arm64.whl` | 9.6 MB |
| `sherpa-onnx-core` | 1.13.8 | Apache-2.0 | `sherpa_onnx_core-1.13.8-py3-none-win_arm64.whl` | 16.1 MB |
| `sherpa-onnx-core` | 1.13.8 | Apache-2.0 | `sherpa_onnx_core-1.13.8-py3-none-macosx_10_15_universal2.whl` | 20.4 MB |

`sherpa-onnx` is a thin Python binding; the native payload ships in `sherpa-onnx-core` `[MEASURED by extraction]`:

| Wheel | Native payload | Uncompressed |
| --- | --- | ---: |
| `sherpa_onnx_core … win_amd64` (16.9 MB) | `sherpa_onnx/lib/onnxruntime.dll` 17.80 MB, `…/sherpa-onnx-c-api.dll` 4.61 MB, `…/sherpa-onnx-cxx-api.dll` 0.26 MB, `.lib` import libs, `include/sherpa-onnx/c-api/c-api.h` | 46.2 MB |
| `sherpa_onnx_core … macosx_11_0_arm64` (9.6 MB) | `sherpa_onnx/lib/libonnxruntime.dylib` 29.01 MB, `…/libsherpa-onnx-c-api.dylib` 4.18 MB, `…/libsherpa-onnx-cxx-api.dylib` 0.16 MB, headers | 33.6 MB |

**Key architectural finding:** the C API (`sherpa-onnx-c-api.dll` / `libsherpa-onnx-c-api.dylib`, plus `c-api.h`) is shipped inside the wheel and links a **self-contained** `onnxruntime.dll` / `libonnxruntime.dylib`. A native (non-Python) desktop host therefore does **not** need a second ONNX Runtime build for the ASR/diarization/VAD/TTS lanes — one set of three libraries covers all of them `[MEASURED]`.

**Prebuilt native archives (GitHub release `v1.13.8`)** `[REGISTRY]`:

| Asset | Size |
| --- | ---: |
| `sherpa-onnx-v1.13.8-win-x64-shared-MD-Release.tar.bz2` | 20 494 724 B |
| `sherpa-onnx-v1.13.8-win-x64-shared-MD-Release-lib.tar.bz2` (libs only) | 7 383 373 B |
| `sherpa-onnx-v1.13.8-win-x64-shared-MD-Release-no-tts.tar.bz2` | 19 164 933 B |
| `sherpa-onnx-v1.13.8-win-x64-shared-MD-Release-no-tts-lib.tar.bz2` | 6 907 798 B |
| `sherpa-onnx-v1.13.8-osx-arm64-shared.tar.bz2` | 20 314 448 B |
| `sherpa-onnx-v1.13.8-osx-arm64-shared-lib.tar.bz2` | 8 773 198 B |
| `sherpa-onnx-v1.13.8-cuda-12.x-cudnn-9.x-onnxruntime1.28.2-win-x64-cuda.tar.bz2` | 595 017 373 B |
| `sherpa-onnx-v1.13.8-cuda-13.x-cudnn-9.x-onnxruntime1.28.2-win-x64-cuda.tar.bz2` | 478 110 488 B |
| `sherpa-onnx-non-streaming-asr-x64-v1.13.8.exe` (standalone CLI) | 23 361 024 B |
| `sherpa-onnx-1.13.8.aar` / `sherpa-onnx-native-lib-win-x64-1.13.8.jar` | 50.1 MB / 8.28 MB |

**DirectML:** the `v1.13.8` release publishes **no** DirectML prebuilt archive `[MEASURED by enumerating release assets]`. DirectML is a source-build option (`cmake/onnxruntime-win-x64-directml.cmake`, referenced from `CMakeLists.txt`, `sherpa-onnx/csrc/provider.cc`, `session.cc`) `[REGISTRY via GitHub code search]`. Consequence: on Windows without an NVIDIA GPU, the supported shipping paths are CPU (20.5 MB archive) or a locally built DirectML bundle; DirectML is not a downloadable artifact today. The generic alternative is ONNX Runtime's own DirectML package (`Microsoft.ML.OnnxRuntime.DirectML` NuGet 1.24.4 = 12 458 649 B; generic `Microsoft.ML.OnnxRuntime` 1.30.0 nupkg = 157 230 753 B, which bundles runtimes for all RIDs) `[REGISTRY — NuGet flat-container HEAD]` used directly by RapidOCR rather than by sherpa-onnx.

**SenseVoice weights** `[REGISTRY + CITED]`:

| Artifact | Source | Size |
| --- | --- | ---: |
| `model.int8.onnx` | [`csukuangfj/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17`](https://huggingface.co/csukuangfj/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17) | **239.2 MB** (docs print `228M`) |
| `model.onnx` (fp32) | same | **937.6 MB** (docs print `894M`) |
| `tokens.txt` | same | 0.3 MB (`308K`) |
| CapCap repack | `Hacht/CapCapResource/zipResource/sensevoice.zip` | 283.8 MB |
| CLI/Android int8 tar | release tag `asr-models`: `sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17.tar.bz2` | 494 168 KB unpacked (228 MB model + `test_wavs`) `[CITED]` |

**Licensing nuance (important):** the SenseVoice **source repository is MIT**, but the **official weights are distributed under the FunASR Model Open Source License Agreement**, and the upstream README records an official clarification for v1.1: commercial use of the official `SenseVoiceSmall` weights is permitted when the license is followed, §2.2 attribution and model-name requirements still apply, and §3 is a responsibility/risk disclaimer rather than a non-commercial restriction. Third-party conversions may carry different terms and must be checked per model card `[CITED]`. This is a **license-manifest fact Douyinie's Issue #61 enforcement must record per lane** (the repo already has the license-manifest + snapshot machinery).

**Speed evidence** `[CITED]`:

- SenseVoice README: non-autoregressive end-to-end architecture; at similar parameter count it "runs more than 5 times faster than Whisper-Small and 15 times faster than Whisper-Large".
- sherpa-onnx docs, `sherpa-onnx-offline` on the int8 model, **1 thread**: `zh.wav` 12.744 s audio → 1.392 s (**RTF 0.109**), with ITN 1.396 s (RTF 0.110), a 5.592 s clip → 0.550 s (**RTF 0.098**). RK3588 table: Cortex-A76 int8 4 threads **RTF 0.049**, 1 thread 0.099; Cortex-A55 1 thread 0.436.
- Model load: "recognizer created in 0.542–0.576 s" `[CITED]`.

SenseVoice decodes Chinese/English/Cantonese/Japanese/Korean with language ID, punctuation via `use_itn`, and emits **per-token timestamps** in the CLI JSON payload (`"timestamps": [...]`, one entry per token, plus `"tokens"`) `[CITED]`. That output shape is the natural replacement candidate for the Qwen3-ForcedAligner lane, but Douyinie's aligner contract is character/word-level `word_timings` with `word`/`start_ms`/`end_ms` (`cmd/stageworker/adapters/aligner_qwen3.py`), so a mapping + accuracy validation against the annotated corpus is a follow-up task, not a given `[INFERENCE]`.

---

## 4. Lane B — Diarization and VAD (Sherpa-ONNX, no PyTorch)

**Current Douyinie lane:** `cmd/stageworker/adapters/diarizer_3dspeaker.py` runs the ModelScope 3D-Speaker `Diarization3Dspeaker` pipeline — FSMN-VAD `iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4` → CAM++ embeddings `iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0` → spectral clustering — and needs a dedicated venv carrying `modelscope`/`torchaudio` (5 115 MB measured, `docs/agents/live-runtime-e2e.md`). `docs/research/issue44-diarization-provider-2026-08-27.md` records the CAM++ checkpoint as ≈ 28.5 MB and Apache-2.0.

**Sherpa-ONNX replacement** `[REGISTRY + CITED]`:

| Role | Artifact | Size | Source |
| --- | --- | ---: | --- |
| Segmentation | `model.int8.onnx` / `model.onnx` (pyannote segmentation 3.0) | 1.5 MB / 6.0 MB | [`csukuangfj/sherpa-onnx-pyannote-segmentation-3-0`](https://huggingface.co/csukuangfj/sherpa-onnx-pyannote-segmentation-3-0) |
| Segmentation (pack) | `sherpa-onnx-pyannote-segmentation-3-0.tar.bz2` | 6 958 444 B | release tag `speaker-segmentation-models` |
| Speaker embedding | `3dspeaker_speech_eres2net_base_sv_zh-cn_3dspeaker_16k.onnx` | **39 593 761 B** | release tag `speaker-recongition-models` (upstream spelling) |
| Speaker embedding (alt) | `nemo_en_titanet_small.onnx` / `nemo_en_titanet_large.onnx` | 40.3 MB / 101.4 MB | same tag |
| VAD | `silero_vad.onnx` | **643 854 B** | release tag `asr-models` |
| VAD (alt) | `silero_vad_v5.onnx` / `silero_vad_v4.onnx` | 2 313 101 B / 1 807 522 B | same tag |

Total replacement footprint: **≈ 41.7 MB** (int8 segmentation + eres2net + silero_vad), versus 5 115 MB of venv plus the ModelScope download cache. The CapCap code path (`speaker_diarization_service.py`) is a working, PyTorch-free reference for exactly this configuration, and it defaults to the **CPU provider** deliberately ("Diarization is an optional timeline aid, so reserving the GPU for ASR/playback keeps the editor responsive"), with CUDA opt-in only when a CUDA-enabled sherpa build is installed.

Sherpa-ONNX also provides a VAD module that is *separate* from diarization (`silero_vad.onnx`, `provider=…`, `num_threads=…`) `[CITED]`, so the pipeline `VAD → ASR` and `VAD → segmentation → embeddings → clustering` can share one runtime and one model file.

---

## 5. Lane C — OCR (RapidOCR / PP-OCRv4 on ONNX Runtime)

**Current Douyinie lane:** PaddleOCR 3.7.0 with PaddlePaddle inference (`cmd/stageworker/adapters/ocr.py`; baseline id `paddleocr-3.7.0@b03f46425e8ff4442b268ce449e3eef758146cd4` in `internal/provider/worker_adapters.go`), models `PP-OCRv6_medium_det` + `PP-OCRv6_medium_rec` + line-orientation classifier staged as `inference.pdiparams` (59.1 MB + 72.9 MB + 6.4 MB measured).

**Lightweight replacement** `[REGISTRY + MEASURED via the CapCap model mirror]`:

| Package/weight | Version | License | Size |
| --- | --- | --- | ---: |
| `rapidocr` | 3.9.2 (`py3-none-any`) | Apache-2.0 | 27.28 MB (does **not** declare `onnxruntime`; the engine is installed separately) |
| `rapidocr-onnxruntime` | 1.4.4 | Apache-2.0 | 14.92 MB (declares `onnxruntime>=1.7.0`, `python<3.13`) |
| `onnxruntime` | 1.30.0 win_amd64 | MIT | 14.31 MB (unpacked 41.9 MB: `onnxruntime.dll` 18.45 MB + `onnxruntime_pybind11_state.pyd` 19.11 MB) |
| `onnxruntime` | 1.30.0 macosx_14_0_arm64 | MIT | 21.54 MB |
| `opencv-python-headless` | 5.0.0.93 win_amd64 | Apache-2.0 | 43.8 MB |
| `ch_PP-OCRv4_det_infer.onnx` (= `_det_mobile`) | — | Apache-2.0 | **4 745 517 B** |
| `ch_PP-OCRv4_rec_infer.onnx` (= `_rec_mobile`) | — | Apache-2.0 | **10 857 958 B** |
| `ch_ppocr_mobile_v2.0_cls_infer.onnx` (= `_cls_mobile`) | — | Apache-2.0 | 585 532 B |
| `ppocr_keys_v1.txt` | — | — | 26 249 B |
| `ppocrv5_dict.txt` | — | — | 74 012 B |

Full OCR model set = **≈ 16.2 MB**, i.e. ~9× smaller than the current Paddle PP-OCRv6 pair (132 MB) while keeping the same det/rec/cls contract. Candidate sources: the CapCap resource mirror (sizes above) and the upstream mirrors [`SWHL/RapidOCR`](https://huggingface.co/SWHL/RapidOCR) (PP-OCRv1/v2/v3 ONNX, e.g. v3 det 2.43 MB / rec 10.69 MB) plus [`PaddlePaddle/PP-OCRv6_*_onnx`](https://huggingface.co/PaddlePaddle/PP-OCRv6_medium_det_onnx) (medium det 62.03 MB, medium rec 76.55 MB, small rec 21.16 MB) for anyone who wants to stay on v6 while dropping PaddlePaddle.

Note the current PaddlePaddle runtime alone is 104.5 MB per platform wheel (`paddlepaddle-3.3.1-cp312-cp312-win_amd64.whl` and `…-macosx_11_0_arm64.whl`), while the `paddleocr` Python wheel is 0.1 MB `[REGISTRY]` — the cost is the engine, not the wrapper.

Platform notes:

- Windows GPU: `onnxruntime-gpu` 1.30.0 win_amd64 = 160.48 MB and requires `onnxruntime/capi/onnxruntime_providers_cuda.dll`; CapCap's `_onnx_cuda_provider_ready()` demonstrates why the DLL must be probed with `ctypes.WinDLL` rather than trusting `get_available_providers()` `[CITED]`.
- Windows DirectML: `Microsoft.ML.OnnxRuntime.DirectML` NuGet 1.24.4 = 12 458 649 B `[REGISTRY]`.
- macOS ARM64: `onnxruntime` ships `macosx_14_0_arm64` wheels (21.54 MB) providing the CPU and CoreML execution providers; nothing else to install.

---

## 6. Lane D — TTS

### 6.1 Edge-TTS (cloud, zero local weights)

| Fact | Value |
| --- | --- |
| Package | `edge-tts` 7.2.8, `py3-none-any`, 0.03 MB `[REGISTRY]` |
| Dependencies | `aiohttp<4`, `certifi`, `tabulate`, `typing-extensions` `[REGISTRY]` |
| License | `LICENSE` file: "The MIT license is used for `src/edge_tts/srt_composer.py` only. All remaining files are licensed under the LGPLv3." PyPI classifier: LGPLv3 `[MEASURED + REGISTRY]` |
| Weight footprint | **0** |
| Runtime cost | network round-trip per utterance; requires the Microsoft Edge read-aloud service and its terms; unusable offline; not a hard release gate for the offline profile |

It is correct as an *optional online* lane (CapCap uses it exactly that way, and Douyinie's production translation is already gateway-based). It cannot be the offline default, and its LGPL boundary must be recorded in the license manifest.

### 6.2 Piper, three ways to run it

| Path | Artifact | Size | License |
| --- | --- | ---: | --- |
| C++ binary | `rhasspy/piper` release `2023.11.14-2`: `piper_windows_amd64.zip` / `piper_macos_aarch64.tar.gz` / `piper_macos_x64.tar.gz` | 22 477 236 B / 19 146 957 B / 19 146 927 B `[REGISTRY]` | repo MIT `[REGISTRY]` |
| Python package | `piper-tts` 1.8.0 wheels (`cp39-abi3-win_amd64`, `cp39-abi3-macosx_11_0_arm64`) | 34.12 MB each `[REGISTRY]` | **GPL-3.0-or-later**, homepage `github.com/OHF-voice/piper1-gpl` `[REGISTRY]` |
| Sherpa-ONNX VITS path | `sherpa-onnx` offline TTS loading the same Piper voice ONNX | 0 extra MB (reuses the sherpa runtime) `[INFERENCE]` | sherpa-onnx Apache-2.0 + voice terms |

Phonemization data either way: `espeak-ng-data/` — **355 files, 18.0 MB** measured in the sherpa Piper-voice repository `[MEASURED]`; the Python route pulls `espeakng-loader` (9.4 MB wheel) `[REGISTRY]`.

Vietnamese voice weights `[MEASURED]`:

- [`csukuangfj/vits-piper-vi_VN-vais1000-medium`](https://huggingface.co/csukuangfj/vits-piper-vi_VN-vais1000-medium): `vi_VN-vais1000-medium.onnx` **63 201 425 B**, `.onnx.json` 4 860 B, `tokens.txt` 921 B, `espeak-ng-data/` 18.0 MB.
- Alternatives: `csukuangfj/vits-piper-vi_VN-25hours_single-low`, `csukuangfj/vits-piper-vi_VN-vivos-x_low`.
- CapCap's own packs for comparison: `piper-new.zip` 1 460 825 551 B (VI, many voices), `piper-en.zip` 129 446 542 B, legacy `piper.zip` 817 481 058 B `[MEASURED]`.

**Recommendation:** prefer the **sherpa-onnx VITS/Piper path** for the desktop pipeline — it reuses the ASR runtime already loaded for SenseVoice/VAD/diarization, avoids shipping a second TTS engine, and sidesteps the `piper-tts` **GPL-3.0-or-later** Python package (a copyleft obligation Douyinie's license enforcement should not be forced to model). The standalone MIT `rhasspy/piper` binary remains a fallback if upstream parity with a specific voice/config is required.

---

## 7. Footprint comparison

### 7.1 Total package + weight footprint

| Component | Current (measured) | Lightweight, Windows x64 CPU | Lightweight, macOS ARM64 |
| --- | ---: | ---: | ---: |
| Python runtime | 28 454 MB in 8 venvs (`torch` ×4 = 19 733 MB) | CPython embeddable 3.12 zip **11.1 MB** `[REGISTRY]` + one venv of wheels | same |
| Engine wheels | n/a (included above) | **156.4 MB** (ctranslate2 19.22, av 27.60, sherpa-onnx 2.28, sherpa-onnx-core 16.9, onnxruntime 14.31, opencv-python-headless 43.8, rapidocr-onnxruntime 14.92, faster-whisper 1.12, tokenizers 2.86, numpy 12.57, huggingface-hub 0.84) | **54.8 MB** (ctranslate2 1.27, sherpa-core 9.6, onnxruntime 21.54, av 18.22, faster-whisper 1.12, tokenizers 3.10) |
| Native unpacked | n/a | 151.8 MB measured for the four inspected wheels alone (ctranslate2 62.3 + sherpa-core 46.2 + onnxruntime 41.9 + faster-whisper 1.4) | ctranslate2 4.6 + sherpa-core 33.4 + onnxruntime (not extracted) |
| ASR | Qwen3-ASR-0.6B 1.7 GB + ForcedAligner-0.6B 1.7 GB | SenseVoice int8 **239.2 MB** + tokens 0.3 MB | same |
| ASR (Whisper option) | — | faster-whisper small 483.5 MB (base 145.2 MB) | same |
| Diarization + VAD | CAM++ 28.5 MB + FSMN-VAD (ModelScope, torch) | pyannote seg int8 1.5 MB + eres2net 39.6 MB + silero_vad 0.64 MB = **41.7 MB** | same |
| OCR | PP-OCRv6 132.0 MB + paddlepaddle 104.5 MB | PP-OCRv4 det/rec/cls/dict = **16.2 MB** | same |
| TTS | ZeroTTS pack 231 MB (CPU) / CosyVoice3+Kokoro+VieNeu | Piper VI voice 63.2 MB + espeak-ng-data 18.0 MB = **81.2 MB** | same |
| **Total** | **≈ 39.3 GB** (27.8 GiB venvs + 10.83 GB models) | **≈ 1.02 GB** (0.17 GB runtime + 0.86 GB models) | **≈ 0.9 GB** |

With `faster-whisper-base` instead of `small` the lightweight total drops to ≈ 0.69 GB `[INFERENCE]`.

### 7.2 Optional GPU add-ons (Windows only)

| Add-on | Download | Unpacked |
| --- | ---: | ---: |
| CTranslate2 CUDA pack (`cuda12_fw_new.zip`) | 1 500.6 MB | 2 183 MB |
| sherpa-onnx CUDA-12 win-x64 archive | 595.0 MB | (archive) |
| sherpa-onnx CUDA-13 win-x64 archive | 478.1 MB | (archive) |
| `onnxruntime-gpu` wheel | 160.5 MB | — |

Even with the full CUDA stack the ASR lane is ≈ 3.0 GB, i.e. still below the 3.46 GB of the *single* `torch-2.8.0+cu128-cp312-cp312-win_amd64.whl` the current ASR venv installs `[REGISTRY: download.pytorch.org HEAD = 3 461 384 651 B]`.

### 7.3 VRAM / RAM

| Engine | VRAM (GPU) | RAM (CPU) | Source |
| --- | --- | --- | --- |
| Qwen3-ASR-0.6B + Aligner-0.6B fp16 (current) | **≈ 4.08–4.13 GiB** resident on a 60 s chunk; 7.4–7.6 GiB on a 445 s one-pass | n/a (GPU-only path) | in-repo smoke report `[MEASURED]` |
| faster-whisper large-v2 fp16 (beam 5) | 4 525 MB; int8 2 926 MB | — | upstream README `[CITED]` |
| faster-whisper large-v2 fp16, `batch_size=8` | 6 090 MB (int8 4 500 MB) | — | upstream README `[CITED]` |
| faster-whisper small int8 (CPU) | — | 1 477 MB | upstream README `[CITED]` |
| SenseVoice int8 (sherpa-onnx) | CPU-only path measured; nothing close to 4 GiB | a few hundred MB for a 240 MB int8 graph `[INFERENCE]` | sherpa docs `[CITED]` |

The ASR lane therefore drops from ~4.1 GiB resident VRAM + 3.4 GB of workspace to ~0 GiB to a few hundred MB, which is what makes an 8 GB (and an Apple-Silicon unified-memory) profile comfortable instead of chunk-forced.

### 7.4 Latency

| Engine | Hardware | Audio | Result | RTF |
| --- | --- | --- | --- | --- |
| Qwen3-ASR-0.6B + Aligner (current) | RTX 2060 SUPER 8 GB, fp16 | 60.0 s | 17.346 s | **0.289** |
| Qwen3-ASR-0.6B + Aligner (chunked) | RTX 2060 SUPER 8 GB, fp16 | 445.1 s | 112.593 s | **0.245** |
| SenseVoice int8 (sherpa-onnx) | 1 thread (dev laptop) | 12.744 s | 1.392 s | **0.109** |
| SenseVoice int8 (sherpa-onnx) | 1 thread | 5.592 s | 0.550 s | **0.098** |
| SenseVoice int8 (sherpa-onnx) | Cortex-A76, 4 threads | — | — | **0.049** |
| faster-whisper large-v2 fp16 | RTX 3070 Ti 8 GB | 13 min | 63 s | **0.081** |
| faster-whisper large-v2 fp16, batch 8 | RTX 3070 Ti 8 GB | 13 min | 17 s | **0.022** |
| faster-whisper small int8 | i7-12700K, 8 threads | 13 min | 102 s | **0.131** |

Conclusion `[INFERENCE]`: on the target RTX 2060 SUPER, a SenseVoice int8 CPU lane (~0.1 RTF) plus a Whisper small/base lane for fallback already clears the Local-8GB RTF ≤ 5.0× gate with a wide margin, and the GPU becomes optional rather than load-bearing — which is what makes a macOS ARM64 build viable at all (no Metal backend exists for either CTranslate2 or sherpa-onnx; ARM64 is a CPU story). Direct Qwen3-ASR quality comparison on the Chinese Douyin corpus is **not** settled by this report and remains an engine-selection task for the quality corpus.

---

## 8. Architecture findings that constrain the implementation

1. **One runtime can host four lanes.** `sherpa-onnx-core` bundles `onnxruntime.dll`/`libonnxruntime.dylib` plus the C/C++ API, and supports SenseVoice ASR, pyannote segmentation, speaker embedding extraction, silero VAD, and VITS/Piper TTS `[MEASURED + CITED]`. A desktop build therefore needs one `sherpa-onnx-c-api` dependency, not four.
2. **CTranslate2 is the only lane that needs an external BLAS/CUDA stack.** CPU is self-contained (`ctranslate2.dll` + `libiomp5md.dll`); GPU needs cuBLAS-12 + cuDNN-9 DLLs external to the wheel `[MEASURED + CITED]`.
3. **macOS ARM64 is CPU-only.** Both engines are CPU on Apple Silicon; the CTranslate2 wheel is 1.27 MB with a 4.6 MB unpacked payload, and sherpa-onnx ships a 20.3 MB `osx-arm64-shared` archive (8.77 MB libs-only) `[MEASURED]`. Any roadmap claim of "ARM64 MPS acceleration" is unsupported by these projects and should be re-scoped.
4. **DirectML is not a downloadable sherpa-onnx artifact today.** Either build it from source (`cmake/onnxruntime-win-x64-directml.cmake`) or accept CPU on AMD/Intel Windows GPU machines `[MEASURED by release enumeration]`.
5. **Native (non-Python) hosting is possible but not free.** The C API headers ship inside the wheels, and sherpa-onnx publishes shared archives for win-x64/osx-arm64, so a C++/Rust desktop host can embed ASR/diarization/VAD/TTS. The CTranslate2 side would use `ctranslate2.dll`/`libctranslate2.dylib` with its own C++ API; `faster-whisper`'s value-add (VAD pre-pass, batching, timestamp heuristics) is Python-side and would have to be re-implemented `[INFERENCE]`.
6. **Wheel-archived artifacts are verifiable.** Extracting wheels gave exact native payload lists — a packaging digest (`sha256` of the extracted DLL/dylib set) is a cheap, strong addition to Douyinie's existing snapshot-manifest machinery.

---

## 9. Fit with Douyinie's existing seams

The repo's constraints that the lightweight pipeline must satisfy without change:

- **Two seams only** (Seam 1 REST, Seam 2 NDJSON IPC) and the StageWorker stdin-JSON/stdout-JSON adapter contract used by every family (`cmd/stageworker/adapters/*`) `[CITED: docs/research/issue57-quality-corpus-review-protocol-2026-09-03.md §"Architectural Seams"]`.
- **Per-family interpreters.** `main.go` resolves one shared `DOUYINIE_PYTHON_BIN` for asr/aligner plus a fail-closed per-family variable for diarizer/OCR/etc. A lightweight pipeline collapses this to **one** interpreter, but the resolver contract (fail closed, name the variable) should be kept `[CITED: cmd/stageworker/main.go, docs/agents/live-runtime-e2e.md]`.
- **Snapshot + license enforcement (Issue #61).** Each new lane must register a primary checkpoint plus `ModelDependencies` and a license record; the OCR lane already demonstrates a composite identity (`paddleocr-v6:v6` logical, with det/rec/ori checkpoint dependencies and `PrimaryCheckpointRequired() == false`) — the same pattern applies to a `sensevoice`/`faster-whisper`/`sherpa-piper` family `[CITED: internal/provider/worker_adapters.go, router_ocr_composite_test.go]`.
- **Stage cache identity.** `provider.TTSRuntimeIdentities()` (and the DubSegments provenance hash that consumes it) must gain an entry for any new TTS lane, otherwise swapping the engine silently reuses cached audio `[CITED: memory of PR #102 — `tts_runtime_identity` semantic input; `internal/provider/tts.go`]`. ASR/aligner/OCR stage identities need the same treatment if the engine changes for an already-processed asset.
- **Offline fail-closed.** `cmd/stageworker/main.go` already exports `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1`, `MODELSCOPE_OFFLINE=1`; the lightweight equivalents are "point `sherpa-onnx` and `faster_whisper` at staged local directories and never let `huggingface_hub` reach the network".
- **Model/voice license files travel with weights.** CapCap's `THIRD_PARTY_LICENSES.md` states the rule the Douyinie manifest already assumes: "Model, voice, and font files may have licenses different from the software that loads them. Their original license files should be retained in their respective resource folders."

---

## 10. License matrix

| Component | Code license | Weights license | Ship in a proprietary desktop bundle? |
| --- | --- | --- | --- |
| CTranslate2 | MIT `[REGISTRY]` | n/a | Yes |
| faster-whisper | MIT | Whisper CT2 conversions MIT (`Systran/*`) | Yes |
| sherpa-onnx | Apache-2.0 (GitHub API) | n/a | Yes |
| SenseVoice | MIT (repo) | **FunASR Model License v1.1** — commercial use permitted per the official clarification, §2.2 attribution + model-name requirements apply | Yes, **with attribution + recorded model license** |
| pyannote segmentation 3.0 (ONNX conversion) | Apache-2.0 (conversion repo) | upstream pyannote segmentation is MIT/CC — verify per model card | Yes, verify card |
| 3D-Speaker eres2net / CAM++ | Apache-2.0 `[CITED: docs/research/issue44-*]` | Apache-2.0 | Yes |
| Silero VAD | MIT `[REGISTRY]` | MIT | Yes |
| RapidOCR | Apache-2.0 | PP-OCRv4 weights Apache-2.0 | Yes |
| ONNX Runtime | MIT | n/a | Yes |
| Edge-TTS | **LGPLv3** (except `srt_composer.py`, MIT) | cloud service | Avoid as the offline default; if shipped, LGPL relinking obligations + service terms |
| Piper (rhasspy/piper binary) | MIT | voice terms per voice | Yes |
| `piper-tts` (PyPI 1.8.0, OHF-voice/piper1-gpl) | **GPL-3.0-or-later** | — | **No** — copyleft; use the sherpa-onnx VITS path instead |

---

## 11. Open items / not verified in this pass

1. **Quality parity.** No WER/cer comparison of SenseVoice int8 vs Qwen3-ASR-0.6B/1.7B on the Chinese Douyin corpus was run here; the quality corpus (`docs/research/issue57-*`, `phase1.1-quality-corpus`) is the right venue.
2. **SenseVoice timestamps → aligner contract.** The per-token `timestamps` array exists in the CLI JSON; whether it meets Douyinie's `word_timings` precision on fast CutCap-style speech (the corpus CAT-3 category) is unproven.
3. **macOS ARM64 timings.** No Apple Silicon hardware was measured; all ARM64 numbers here are package sizes plus upstream CPU RTF evidence from other CPUs.
4. **DirectML runtime behavior.** Absence of a prebuilt sherpa-onnx DirectML archive is verified; whether a source build with `onnxruntime-win-x64-directml.cmake` actually accelerates SenseVoice/OCR meaningfully on this app's typical 30–60 s clips is untested.
5. **Latency measured on the deployment host.** The 0.245–0.289 RTF figure is from the in-repo Qwen3 smoke test; the SenseVoice 0.098–0.110 RTF figures are upstream dev-laptop numbers for 5–13 s clips. A 30–60 s Douyin clip on the RTX 2060 SUPER should be measured before locking the engine choice.
6. **NuGet `Microsoft.ML.OnnxRuntime` nupkg size (157 MB) bundles runtimes for every RID**; the per-platform extraction cost was not measured `[INFERENCE for the interpretation]`.

---

## 12. Sources

Registry / artifact facts retrieved 2026-09-19:

- PyPI JSON API: `ctranslate2` 4.8.2, `faster-whisper` 1.2.1, `sherpa-onnx` 1.13.8, `sherpa-onnx-core` 1.13.8, `onnxruntime` 1.30.0, `onnxruntime-gpu` 1.30.0, `rapidocr` 3.9.2, `rapidocr-onnxruntime` 1.4.4, `piper-tts` 1.8.0, `edge-tts` 7.2.8, `av` 18.1.0, `huggingface-hub` 1.32.0, `tokenizers` 0.23.2, `numpy` 2.5.3, `paddlepaddle` 3.3.1, `paddleocr` 3.7.0, `modelscope` 1.40.1, `funasr` 1.4.16, `torchaudio` 2.11.0, `espeakng-loader` 0.2.4, `qwen-asr` 0.0.6.
- Hugging Face API (`?blobs=true`): `Systran/faster-whisper-{base,small,large-v3}`, `deepdml/faster-whisper-large-v3-turbo-ct2`, `Qwen/Qwen3-ASR-{0.6B,1.7B}`, `Qwen/Qwen3-ForcedAligner-0.6B`, `csukuangfj/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17`, `csukuangfj/sherpa-onnx-pyannote-segmentation-3-0`, `csukuangfj/vits-piper-vi_VN-vais1000-medium`, `csukuangfj/vits-piper-vi_VN-25hours_single-low`, `SWHL/RapidOCR`, `PaddlePaddle/PP-OCRv6_{medium_det,medium_rec,small_rec}_onnx`, `FunAudioLLM/SenseVoiceSmall`, `rhasspy/piper-voices`, `Hacht/CapCapResource` (`cuda12_fw_new`, `zipResource`, `rapidocr/models`).
- GitHub API: `k2-fsa/sherpa-onnx` release `v1.13.8` assets, tags `asr-models`, `speaker-recongition-models`, `speaker-segmentation-models`; `rhasspy/piper` release `2023.11.14-2`; `RapidAI/RapidOCR` release `v3.9.2`; license endpoints for `k2-fsa/sherpa-onnx`, `SYSTRAN/faster-whisper`, `OpenNMT/CTranslate2`, `RapidAI/RapidOCR`, `rhasspy/piper`, `snakers4/silero-vad`, `microsoft/onnxruntime`, `rany2/edge-tts`, `FunAudioLLM/SenseVoice`.
- `download.pytorch.org/whl/cu128` HEAD: `torch-2.8.0+cu128-cp312-cp312-win_amd64.whl` = 3 461 384 651 B; `cu126` `torch-2.9.1+cu126-cp312-cp312-win_amd64.whl` = 2 584 508 946 B.
- NuGet flat container HEAD: `microsoft.ml.onnxruntime` 1.30.0, `microsoft.ml.onnxruntime.directml` 1.24.4, `microsoft.ml.onnxruntime.gpu` 1.30.0.
- Documentation: CTranslate2 hardware support <https://opennmt.net/CTranslate2/hardware_support.html>; faster-whisper README <https://github.com/SYSTRAN/faster-whisper>; sherpa-onnx SenseVoice pretrained/benchmark page <https://k2-fsa.github.io/sherpa/onnx/sense-voice/pretrained.html>; SenseVoice README + license clarification <https://github.com/FunAudioLLM/SenseVoice>.
- In-repo: `docs/research/qwen3-asr-forced-aligner-smoke-2026-08-19.md`, `docs/research/issue44-diarization-provider-2026-08-27.md`, `docs/research/issue59-benchmark-execution-contract-2026-09-03.md`, `docs/agents/live-runtime-e2e.md`, `cmd/stageworker/main.go`, `cmd/stageworker/adapters/*`, `internal/provider/worker_adapters.go`.
- Reference implementation: `.ref/CapCap/requirements-local.txt`, `.ref/CapCap/requirements-base.txt`, `.ref/CapCap/docs/technical-stack.md`, `.ref/CapCap/THIRD_PARTY_LICENSES.md`, `.ref/CapCap/app/services/{engine_runtime,resource_download_service,speaker_diarization_service,asr_merge_service}.py`, `.ref/CapCap/app/{sensevoice_processor,ocr_processor,runtime_paths}.py`.
- Local measurements: `du -smL D:\douyinie-ref\phase1.1-runtime\venvs\{asr,ocr,diarizer,tts,separator,translation,audio_role,tts-zerotts-0.1.5-47e466d}` and per-package `torch`/`nvidia` subtotals; directory listings of `D:\douyinie-ref\phase1.1-runtime\staging\*`; wheel extractions of `ctranslate2`, `faster-whisper`, `onnxruntime`, `sherpa-onnx-core`.
