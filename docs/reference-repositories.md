# Douyinie Reference Repository Inventory

Last reviewed: 2026-08-23

This file is the canonical inventory of external repositories worth studying for Douyinie. It is a **reference/mining list, not a dependency lockfile**.

For resolved architecture decisions, the source of truth is the Wayfinder map: https://github.com/monet88/douyinie/issues/1

## How to use this list

- Prefer reading architecture, interfaces, data models, retry logic, timing logic, and UX patterns before copying code.
- Clone repositories being actively mined into the repo-local .ref/ directory; keep .ref/ git-ignored and pin observations to the tested commit SHA.
- A repository being listed here does not mean it is approved as a production dependency.
- Track `CODE_LICENSE`, `MODEL_LICENSE`, and `DATA_LICENSE` separately. Rewriting source code does not remove model/checkpoint/data/service obligations.
- Private/reverse-engineered CapCut endpoints are technically useful references but must remain behind provider adapters and fallbacks.
- Core V1 requirement is timeline-constrained dubbing, **not facial lip sync**.

## P0 — mine first

These are the highest-value repositories to inspect before implementation.

1. SmartSub — provider abstractions, download/transcribe/translate/TTS pipeline, timing control, subtitle UI
   - https://github.com/buxuku/SmartSub
2. YouDub-webui — job architecture, source separation, per-segment voice references, dubbing pipeline
   - https://github.com/liuzhao1225/YouDub-webui
3. VideoLingo — translation/segmentation/timing engineering
   - https://github.com/Huanshere/VideoLingo
4. VideoSubX — decoupled ASR/alignment, stable-ts/MFA options, production-oriented job architecture
   - https://github.com/assassinliujie/VideoSubX
5. pyVideoTrans — mature end-to-end localization workflow
   - https://github.com/jianchang512/pyvideotrans
6. KrillinAI — staged CLI/job manifests and broad source support
   - https://github.com/krillinai/KrillinAI
7. Douyin Downloader — primary Douyin SourceAdapter reference
   - https://github.com/jiji262/douyin-downloader
8. CapCut TTS private API client — Desktop/private protocol reference, STT/TTS/VOD upload
   - https://github.com/K07VN/capcut-tts-api
9. CapCut-TTS — CapCut Web login/session/dynamic endpoint/signing/TTS reference
   - https://github.com/kuwacom/CapCut-TTS
10. capcut-mate — JianYing/CapCut draft/timeline automation and Windows export-worker reference
   - https://github.com/Hommy-master/capcut-mate
11. capcut-cli — direct CapCut draft JSON/timeline manipulation reference
   - https://github.com/renezander030/capcut-cli
12. Qwen3-ASR — baseline Chinese ASR + Qwen3 Forced Aligner family
   - https://github.com/QwenLM/Qwen3-ASR
13. OmniVoice — multilingual zero-shot clone; explicit `speed` and fixed `duration` generation controls
   - https://github.com/k2-fsa/OmniVoice
14. VoxCPM — strong local voice-cloning candidate
   - https://github.com/OpenBMB/VoxCPM
15. VieNeu-TTS — Vietnamese-focused voice cloning/TTS candidate
   - https://github.com/pnnbao97/VieNeu-TTS
16. GPT-SoVITS — mature few-shot voice-cloning ecosystem
   - https://github.com/RVC-Boss/GPT-SoVITS
17. PaddleOCR — Chinese/scene-text OCR baseline
   - https://github.com/PaddlePaddle/PaddleOCR
18. video-subtitle-extractor — temporal hard-subtitle OCR/extraction patterns
   - https://github.com/YaoFANGUK/video-subtitle-extractor
19. video-subtitle-remover — hard-subtitle masks/inpainting with STTN/LAMA/ProPainter options
   - https://github.com/YaoFANGUK/video-subtitle-remover
20. VideoWipe — reviewable WipePlan/detect-plan-inpaint abstraction
   - https://github.com/KKenny0/videowipe
21. video-scene-text-translator — primary scene-text architecture: persistent TextTrack, reference selection, frontalization, edit once, propagate, warp back
   - https://github.com/CMPT743-Team/video-scene-text-translator
22. ProPainter — high-quality video inpainting reference
   - https://github.com/sczhou/ProPainter
23. python-audio-separator — swappable UVR/MDX/MDXC/Demucs stem-provider abstraction
   - https://github.com/nomadkaraoke/python-audio-separator
24. Demucs — source-separation baseline/reference
   - https://github.com/adefossez/demucs

## End-to-end localization / dubbing references

- SmartSub — https://github.com/buxuku/SmartSub
- YouDub-webui — https://github.com/liuzhao1225/YouDub-webui
- VideoLingo — https://github.com/Huanshere/VideoLingo
- pyVideoTrans — https://github.com/jianchang512/pyvideotrans
- KrillinAI — https://github.com/krillinai/KrillinAI
- Linly-Dubbing — https://github.com/Kedreamix/Linly-Dubbing
- MioSub — https://github.com/corvo007/MioSub
- xiaohu-video-translate — https://github.com/xiaohuailabs/xiaohu-video-translate
- VideoSubX — https://github.com/assassinliujie/VideoSubX
- VoiceStudio — https://github.com/debpalash/VoiceStudio
- video_translator — https://github.com/wencharmwang/video_translator
- VideoSyncMaster — https://github.com/TianDongL/VideoSyncMaster
- AutoVidDub — https://github.com/aidayang/AutoVidDub
- VideoLingo-OneClick — https://github.com/aidayang/VideoLingo-OneClick
- vdub — https://github.com/jmpnop/vdub
- ZastTranslate — https://github.com/zast57/ZastTranslate
- AutoDub — https://github.com/shyhirt/AutoDub
- iDubb — https://github.com/vmansus/iDubb

## Douyin / source ingestion

- jiji262/douyin-downloader — https://github.com/jiji262/douyin-downloader
- Douyin_TikTok_Download_API — https://github.com/Evil0ctal/Douyin_TikTok_Download_API
- TikTokDownloader / DouK-Downloader — https://github.com/JoeanAmier/TikTokDownloader
- f2 — https://github.com/Johnserf-Seed/f2
- TikTokDownload — https://github.com/Johnserf-Seed/TikTokDownload

Current Wayfinder direction: Jiji first, F2/Evil-style fallback, browser-assisted auth last. Downstream consumes immutable normalized source artifacts rather than Douyin URLs/CDN URLs.

## CapCut / JianYing private API, draft, and automation

### Speech/private protocol

- K07VN/capcut-tts-api — https://github.com/K07VN/capcut-tts-api
- kuwacom/CapCut-TTS — https://github.com/kuwacom/CapCut-TTS

### Draft/timeline/automation

- Hommy-master/capcut-mate — https://github.com/Hommy-master/capcut-mate
- ashreo/CapCutAPI — https://github.com/ashreo/CapCutAPI
- renezander030/capcut-cli — https://github.com/renezander030/capcut-cli
- Hommy-master/capcut-mate-mcp — https://github.com/Hommy-master/capcut-mate-mcp

Current Wayfinder direction: CapCut is an acceleration/editable-project backend, never the only required renderer or speech backend.

## Chinese ASR / timestamps / forced alignment

- Qwen3-ASR — https://github.com/QwenLM/Qwen3-ASR
- FunASR — https://github.com/modelscope/FunASR
- FireRedASR — https://github.com/FireRedTeam/FireRedASR
- WhisperX — https://github.com/m-bain/whisperX
- stable-ts — https://github.com/jianfch/stable-ts
- Montreal Forced Aligner — https://github.com/MontrealCorpusTools/Montreal-Forced-Aligner

Current baseline: ASR, forced alignment, diarization, and speech-block segmentation are separate responsibilities. Qwen3-ASR + Qwen3 Forced Aligner is the baseline; FireRed/FunASR are challengers; WhisperX/stable-ts/MFA remain references/fallbacks.

## Voice cloning / TTS

### Tier-1 benchmark candidates

- OmniVoice — https://github.com/k2-fsa/OmniVoice
- VoxCPM — https://github.com/OpenBMB/VoxCPM
- VieNeu-TTS — https://github.com/pnnbao97/VieNeu-TTS
- GPT-SoVITS — https://github.com/RVC-Boss/GPT-SoVITS
- ZipVoice — https://github.com/k2-fsa/ZipVoice

### Other strong engines

- CosyVoice — https://github.com/FunAudioLLM/CosyVoice
- Qwen3-TTS — https://github.com/QwenLM/Qwen3-TTS
- F5-TTS — https://github.com/SWivid/F5-TTS
- OpenVoice — https://github.com/myshell-ai/OpenVoice
- fish-speech — https://github.com/fishaudio/fish-speech

### Vietnamese-specific references

- gwen-tts — https://github.com/ggroup-ai-lab/gwen-tts
- vixtts-demo — https://github.com/thinhlpg/vixtts-demo
- v-tts — https://github.com/tronghieuit/v-tts

Provider choice is per `(speaker, target_language, execution_profile)`. If a provider changes for one speaker, regenerate that speaker's full target-language voice set rather than mixing engines sentence-by-sentence.

## Hard subtitles / OCR / text removal

- PaddleOCR — https://github.com/PaddlePaddle/PaddleOCR
- video-subtitle-extractor — https://github.com/YaoFANGUK/video-subtitle-extractor
- video-subtitle-remover — https://github.com/YaoFANGUK/video-subtitle-remover
- VideoWipe — https://github.com/KKenny0/videowipe
- ProPainter — https://github.com/sczhou/ProPainter
- SubErase-Translate-Embed — https://github.com/chenwr727/SubErase-Translate-Embed

Hard dialogue subtitles should generally be detected/erased as image content, then recreated as editable target-language caption tracks.

## Scene-text replacement / style / tracking

- video-scene-text-translator — https://github.com/CMPT743-Team/video-scene-text-translator
- SAM 2 — https://github.com/facebookresearch/sam2
- CoTracker — https://github.com/facebookresearch/co-tracker
- AnyText — https://github.com/tyxsspa/AnyText
- AnyText2 — https://github.com/tyxsspa/AnyText2
- comic-translate — https://github.com/ogkalu2/comic-translate
- koharu — https://github.com/mayocream/koharu
- image-translator — https://github.com/saikoneru/image-translator

Core pattern: persistent `VisualTextTrack` -> choose best reference frame -> canonical/frontal space -> inpaint/edit once -> propagate style/lighting -> de-frontalize/warp back per frame. Avoid independent frame-by-frame text generation because it flickers.

## Full-product behavior reference

- GhostCut-auto_video_translation — https://github.com/JollyToday/GhostCut-auto_video_translation

Use GhostCut mainly as a product/behavior specification reference for OCR -> position/style extraction -> inpaint -> translate -> replace -> speech translate/TTS -> sync/BGM preservation. Do not assume every claimed production algorithm is open-source.

## Audio separation / BGM / ambience / SFX preservation

- python-audio-separator — https://github.com/nomadkaraoke/python-audio-separator
- Demucs — https://github.com/adefossez/demucs
- Ultimate Vocal Remover GUI — https://github.com/Anjok07/ultimatevocalremovergui
- uvr-headless-runner — https://github.com/chyinan/uvr-headless-runner
- AudioSeparation — https://github.com/set-soft/AudioSeparation

Current Wayfinder direction: preserve the original soundtrack outside source-speech windows. Inside aligned source-speech windows, suppress/replace source dialogue using the background stem and localized dub. Do not replace the entire video's soundtrack with the separated instrumental stem.

## Optional / currently out of V1 scope: facial lip synchronization

- LatentSync — https://github.com/bytedance/LatentSync
- MuseTalk — https://github.com/TMElyralab/MuseTalk

These are retained only for future optional facial/mouth-sync work. V1 requires speech timing alignment, not face animation.

## Additional architecture references discovered during research

- TrackExtract — local-first desktop shell + Python ML engine/job/model-registry patterns
  - https://github.com/AdamWentworth/TrackExtract
- VoiceStudio (formerly OmniVoice-Studio; tested v0.5.0 at `98c9e68aae96ce83db580afa4ca0bdce79a454ae`) — local-first Tauri + React/Vite + FastAPI app with engine/model catalogue, CPU/CUDA/MPS/ROCm routing, OpenAI-compatible local API, authenticated remote GPU workers, and a Colab T4 notebook. Dubbing is useful to mine for pre-TTS slot adaptation, measured natural-rate TTS, gap/slack accounting, deterministic fit plans/fingerprints, cache-aware incremental regeneration, and golden tests. Do not copy its Smart Fit policy into Douyinie: it may slow short audio, retime video, or trim overflow; Wayfinder #6 keeps source anchors immutable and prefers silence/rewrite/review. App code is AGPL-3.0-only; bundled upstream `omnivoice/` remains Apache-2.0.
  - https://github.com/debpalash/VoiceStudio
  - Low-resource OmniVoice runtime to benchmark: https://github.com/ServeurpersoCom/omnivoice.cpp (MIT runtime; VoiceStudio pins `886fc079838ca7400cb2b42b36e2a65aa1daabe8` and Q4/Q8/BF16 GGUF profiles. The local VoiceStudio checkout contains zero-byte runtime placeholders, so build/download the binary before any Douyinie benchmark.)
- T-blao — local-first Electron shell, on-demand engine/bootstrap manager, JSONL worker protocol, deterministic subtitle layout/rendering; root PolyForm Noncommercial, mine architecture/UX patterns only
  - https://github.com/NeeyuBL/neeyut-blao
- mlx-audio-separator — Apple Silicon/MLX audio-separation reference
  - https://github.com/ssmall256/mlx-audio-separator
- Music-Separator-GUI — audio-separator/UVR integration and model-selection UX
  - https://github.com/GianlucaApollaro/Music-Separator-GUI

## Current mining order

1. SmartSub
2. YouDub-webui
3. video-scene-text-translator
4. VideoSubX
5. VideoWipe
6. SubErase-Translate-Embed
7. GhostCut behavior/spec
8. VideoLingo / pyVideoTrans / KrillinAI
9. jiji262/douyin-downloader
10. K07VN/capcut-tts-api + kuwacom/CapCut-TTS + capcut-mate + capcut-cli
11. Qwen3-ASR and aligner challengers
12. OmniVoice / VoxCPM / VieNeu / GPT-SoVITS / ZipVoice
13. PaddleOCR / VSE / VSR / ProPainter / SAM2 / CoTracker / AnyText2
14. python-audio-separator / Demucs / UVR

## Related Douyinie decisions

Canonical decision index: https://github.com/monet88/douyinie/issues/1

Important resolved tickets include:

- CapCut speech backend: https://github.com/monet88/douyinie/issues/2
- CapCut timeline/render: https://github.com/monet88/douyinie/issues/3
- Douyin ingestion: https://github.com/monet88/douyinie/issues/4
- Chinese ASR/alignment: https://github.com/monet88/douyinie/issues/5
- Translation/duration control: https://github.com/monet88/douyinie/issues/6
- Voice cloning/provider strategy: https://github.com/monet88/douyinie/issues/7
- Visual-text architecture: https://github.com/monet88/douyinie/issues/8
- Acceptance metrics/benchmark corpus frontier: https://github.com/monet88/douyinie/issues/9
- Audio separation/remix: https://github.com/monet88/douyinie/issues/10
- Default provider matrix (blocked by #9): https://github.com/monet88/douyinie/issues/11
