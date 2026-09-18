# Verification Note: Issue 104 - audio_role_plan fails on real media (stem rate vs analyzer contract)

## Root Cause
The `audio_role_plan` stage was failing closed on all real videos when processing stems separated by Demucs. The Yamnet audio-role analyzer strictly enforces a `16000 Hz` / `1 ch` contract for input audio (`cmd/stageworker/adapters/audio_role_yamnet.py:72`). However, Demucs natively outputs `44100 Hz` / `2 ch` audio.

Although the adapter attempted to construct the Separator instance with `sample_rate=16000`, Demucs ignores this during processing and writes 44.1 kHz audio to disk. The separator adapter previously hard-coded `sample_rate: 16000` into its output artifact metadata, masking the issue from downstream until the analyzer crashed processing the actual bytes.

## Resolution
The separator adapter (`cmd/stageworker/adapters/separator.py`) was updated (commit `0a821c8`) to enforce the pipeline contract rate natively across all stem lanes. It now wraps the raw output of `_dispatch_audio_stems` in `enforce_contract_rate`. This function resamples any mismatched stems down to `16000 Hz` mono using FFmpeg (writing back to file instead of streaming since RIFF chunk size needs seeking) and correctly replaces the reported rate and channel metadata based on actual measurement instead of blindly assuming `16000`.

The Yamnet analyzer contract is maintained at 16 kHz mono because the `yamnet.tflite` model expects 16 kHz PCM inputs natively; passing higher sample rates directly into Yamnet without downsampling results in silent accuracy degradation, leading to incorrect plans.

## Live Evidence
- Pre-fix Stems (measured from WAV bytes, independent of false metadata):
  - `2d2de41f` (asset `f6e0436f`): 44100 Hz / 2 ch (Declared 16000 Hz / 1 ch)
  - `9b164d9a` (asset `41fb83c0`): 44100 Hz / 2 ch (Declared 16000 Hz / 1 ch)
  - `d017311c` (asset `ebba79f3`): 44100 Hz / 2 ch (Declared 44100 Hz / 2 ch, post adapter reporting fix but pre normalization)

- Post-fix Stems:
  - `34fba224` (asset `3.mp4`): 16000 Hz / 1 ch (Declared 16000 Hz / 1 ch)
  - `39bfbf65` (fixture `source-5s.mp4`): 16000 Hz / 1 ch (80248 frames = 5015 ms, plan generated successfully with ID `029c40cc-aced-4fd5-ad83-36723c9c721c`)

## Gate

`test/seam2/audio_role_seam2_test.go` (`TestSeam2_AudioRolePlan_EndToEndRealMedia`) drives the real separator
worker and the real YAMNet worker over Seam 2 on `source-5s.mp4`, then asserts the produced vocals WAV measures
16 kHz mono, that the separator's declared rate/channels agree with the measurement, and that the analyzer
returns a non-empty plan. It skips (never fakes) when a venv or the fixture is missing.

Red proof (pre-fix adapter, no repo mutation): `git show 0a821c8^:cmd/stageworker/adapters/separator.py` into a
scratch path, then `DOUYINIE_SEPARATOR_ADAPTER=<scratch> go test ./test/seam2/ -run TestSeam2_AudioRolePlan_EndToEndRealMedia -count=1`
fails with `measured vocals WAV: 44100 Hz / 2 ch`, `separator declared 44100 Hz / 2 ch` and the analyzer's
`AUDIO_ROLE_EXEC_FAILED: unsupported sample rate: 44100 Hz`.

## Caveats
Already-persisted stem artifacts (`audio_stems_artifacts` rows) generated before the fix retain their original 44.1 kHz audio and incorrect metadata. `audio_role_plan` will continue failing closed on those runs because it will reuse the stale artifact from the stage cache without re-processing it.

To unblock older assets, the `audio_stems_artifacts` rows must be explicitly dropped from the dev database, forcing a fresh run to re-separate the stems through the now-fixed adapter logic.
