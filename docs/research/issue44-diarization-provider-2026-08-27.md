# Issue #44 — production diarization provider decision

## Conclusion
Use a **3D-Speaker / CAM++ non-overlap diarization worker** (`iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0` with FSMN-VAD `v2.0.4`) as the Phase-1 production implementation behind Douyinie's existing `DiarizationProvider` seam. Keep Qwen3-ASR + Qwen3-ForcedAligner unchanged. Do not use the invented `qwen3-diarizer` or `campplus-diarizer` standalone binary assumption.

The verified runtime is invoked via a repo-owned JSON adapter (`cmd/stageworker/adapters/diarizer_3dspeaker.py`) executing the first-party ModelScope 3D-Speaker `Diarization3Dspeaker` non-overlap pipeline: FSMN-VAD (`iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4`) -> speech chunks -> CAM++ speaker embeddings (`iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0`) -> spectral clustering -> speaker time regions.

## Findings
- Issue #44 requires conditional diarization/speaker assignment before canonical SpeechBlock segmentation, but does not lock a diarizer model.
- Phase-1 architecture explicitly keeps FunASR-family VAD/diarization useful independently of the Qwen3 recognizer baseline.
- FunASR's official README lists `cam++` as a 7.2M-parameter speaker-diarization model and shows speaker pipelines using CAM++.
- 3D-Speaker's official `infer_diarization.py` implements a standalone non-ASR diarization path using FSMN-VAD, CAM++ embeddings, and spectral clustering; output is `[start, end, speaker_id]` regions.
- 3D-Speaker code is Apache-2.0. The official `funasr/campplus` model card is Apache-2.0 and the checkpoint is about 28.5 MB.
- Qwen3-ASR exposes ASR and ForcedAligner classes, not a Qwen3 diarization model; therefore `qwen3-diarizer` is not a valid production dependency.

## Implications for this repo
- Replace production provider identity with the real upstream model identity `iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0` and VAD `FSMN-VAD v2.0.4`.
- Keep the current host sequence: accepted Qwen3 transcript -> Qwen3 forced alignment -> evidence probe -> conditional diarization -> host-side speaker stamping -> canonical segmentation.
- The diarization worker must consume source CAS audio and return absolute-ms speaker regions only; it must not replace accepted transcript text or make its own boundaries canonical.
- Invoke the ModelScope 3D-Speaker runtime via the repo-owned JSON adapter `cmd/stageworker/adapters/diarizer_3dspeaker.py` with multi-tier runner resolution in StageWorker (`resolveDiarizerRunner`), preserving the StageWorker NDJSON protocol seam. Missing runtime/checkpoint fails closed with structured error `DIARIZER_BINARY_NOT_FOUND`.
- Keep the current no-evidence `SPEAKER_00` path. Probe/runtime failure must remain an error rather than silently becoming single-speaker success.
- Preserve single-GPU/process supervision contracts; a CPU configuration is acceptable if validated, but should not create a third seam.

## Open questions
- Exact checkpoint SHA and packaged Python environment hash remain implementation/validation pins under the architecture's residual-risk rules.
- Windows smoke testing of the packaged runtime is still required; do not infer it from synthetic fake-model tests.

## Sources
- Douyinie Issue #44 and Implementation Spec #18.
- `docs/architecture/phase1-architecture.md` and `docs/reference-repositories.md`.
- FunASR official repository README/model zoo and CAM++ model implementation.
- ModelScope/3D-Speaker official README and `speakerlab/bin/infer_diarization.py`.
- Hugging Face `funasr/campplus` official model card (Apache-2.0, CAM++, 16 kHz, speaker diarization).
