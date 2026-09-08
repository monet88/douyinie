#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: TTS Engines (VieNeu, CosyVoice3, Kokoro, Chatterbox)
Invokes official upstream TTS Python APIs or fails closed with descriptive errors.

Contract:
- Stdin: JSON request with text, language, voice_id, speed, slot_duration_ms, model_name, model_version, run_id, attempt_id
- Stdout: JSON response with audio_data (base64 WAV), audio_sha256, measured_duration_ms, predicted_duration_ms
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import base64
import hashlib
import io
import json
import os
import struct
import sys
import wave
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple, Union

# Frozen Phase 1.1 RC Model Identities & Allowed Voices (Issue #68)
VIENEU_MODEL_ID = "pnnbao-ump/VieNeu-TTS-v3-Turbo"
VIENEU_MODEL_VERSION = "v3.2.9"
VIENEU_MODEL_DIGEST = "1278db0090b98ccf23e56f2423857fc9d32a5118"
VIENEU_SDK_COMMIT = "149ff16a6a50093a0cad1b75d5edf9e9d81d97f4"
VIENEU_ALLOWED_VOICES = {"Trúc Ly", "Phạm Tuyên", "Đoan Trang", "Xuân Vĩnh"}

KOKORO_MODEL_ID = "hexgrad/Kokoro-82M"
KOKORO_MODEL_VERSION = "v1.0"
KOKORO_MODEL_DIGEST = "f3ff3571791e39611d31c381e3a41a3af07b4987"
KOKORO_CHECKPOINT_SHA256 = "496dba118d1a58f5f3db2efc88dbdc216e0483fc89fe6e47ee1f2c53f18ad1e4"
KOKORO_ALLOWED_VOICES = {"af_heart", "am_michael", "af_bella", "am_fenrir"}

# Pluggable factory hooks for deterministic testing without full ML packages
_VIENEU_MODEL_FACTORY = None
_COSYVOICE_MODEL_FACTORY = None
_KOKORO_MODEL_FACTORY = None
_CHATTERBOX_MODEL_FACTORY = None

def _instantiate_model(factory: Any, default_kwargs: Optional[Dict[str, Any]] = None) -> Any:
    """Instantiate model class or factory callable into an active instance."""
    if factory is None:
        return None
    kwargs = default_kwargs or {}
    if isinstance(factory, type):
        try:
            return factory(**kwargs)
        except TypeError:
            try:
                return factory()
            except Exception:
                pass
    elif callable(factory):
        try:
            return factory(**kwargs)
        except TypeError:
            try:
                return factory()
            except Exception:
                pass
    return factory


def _to_numpy_or_list(audio_obj: Any) -> Any:
    """Convert tensor, ndarray, or list to flat float sample sequence."""
    if hasattr(audio_obj, "detach"):
        audio_obj = audio_obj.detach()
    if hasattr(audio_obj, "cpu"):
        audio_obj = audio_obj.cpu()
    if hasattr(audio_obj, "numpy"):
        audio_obj = audio_obj.numpy()
    if hasattr(audio_obj, "squeeze"):
        audio_obj = audio_obj.squeeze()
    return audio_obj


def _concat_audio_chunks(chunks: List[Any]) -> Any:
    """Concatenate multiple audio chunks into a single audio array/tensor."""
    if not chunks:
        return []
    flat = []
    for c in chunks:
        c_flat = _to_numpy_or_list(c)
        flat.append(c_flat)

    try:
        import numpy as np
        return np.concatenate(flat, axis=-1)
    except Exception:
        # Fallback list concat
        result = []
        for f in flat:
            if hasattr(f, "tolist"):
                result.extend(f.tolist())
            elif isinstance(f, (list, tuple)):
                result.extend(f)
            else:
                result.append(f)
        return result


def encode_audio_to_wav(audio_data: Any, sample_rate: int = 24000) -> Tuple[bytes, int]:
    """
    Encodes raw float/int audio array or bytes into standard 16-bit PCM WAV.
    Returns: (wav_bytes, duration_ms)
    """
    # 1. If already raw WAV bytes
    if isinstance(audio_data, bytes):
        if audio_data.startswith(b"RIFF") and len(audio_data) >= 44:
            try:
                with wave.open(io.BytesIO(audio_data), "rb") as wf:
                    frames = wf.getnframes()
                    rate = wf.getframerate()
                    dur_ms = int((frames * 1000) / rate) if rate > 0 else 0
                    return audio_data, dur_ms
            except Exception:
                pass

    # 2. Convert tensor / ndarray / list to flat float samples
    samples = _to_numpy_or_list(audio_data)

    try:
        import numpy as np
        if isinstance(samples, np.ndarray):
            samples = samples.flatten()
            # Normalize float to int16 range
            if samples.dtype in (np.float32, np.float64, np.float16):
                # Clamp [-1.0, 1.0]
                samples = np.clip(samples, -1.0, 1.0)
                pcm16 = (samples * 32767.0).astype(np.int16)
            elif samples.dtype == np.int16:
                pcm16 = samples
            else:
                pcm16 = samples.astype(np.int16)
            pcm_bytes = pcm16.tobytes()
            total_frames = len(pcm16)
            dur_ms = int((total_frames * 1000) / sample_rate) if sample_rate > 0 else 0

            buf = io.BytesIO()
            with wave.open(buf, "wb") as wf:
                wf.setnchannels(1)
                wf.setsampwidth(2)
                wf.setframerate(sample_rate)
                wf.writeframes(pcm_bytes)
            return buf.getvalue(), dur_ms
    except ImportError:
        pass

    # Pure Python fallback
    if hasattr(samples, "tolist"):
        samples = samples.tolist()
    if not isinstance(samples, (list, tuple)):
        samples = [samples]

    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(sample_rate)
        pcm_frames = bytearray()
        for s in samples:
            val = float(s)
            val = max(-1.0, min(1.0, val))
            ival = int(val * 32767.0)
            pcm_frames.extend(struct.pack("<h", ival))
        wf.writeframes(bytes(pcm_frames))
        total_frames = len(samples)
        dur_ms = int((total_frames * 1000) / sample_rate) if sample_rate > 0 else 0

    return buf.getvalue(), dur_ms


def build_tts_response(
    wav_bytes: bytes,
    measured_duration_ms: int,
    model_name: str,
    model_version: str,
    predicted_duration_ms: Optional[int] = None,
) -> Dict[str, Any]:
    """Package synthesis result into StageWorker output contract."""
    sha256_hex = hashlib.sha256(wav_bytes).hexdigest()
    b64_data = base64.b64encode(wav_bytes).decode("ascii")
    pred_dur = predicted_duration_ms if predicted_duration_ms is not None else measured_duration_ms

    return {
        "audio_data": b64_data,
        "audio_sha256": sha256_hex,
        "measured_duration_ms": measured_duration_ms,
        "predicted_duration_ms": pred_dur,
        "model_name": model_name,
        "model_version": model_version,
    }


def run_vieneu_tts(
    text: str,
    language: str,
    voice_id: str,
    speed: float,
    model_name: str = "vieneu-tts",
    model_version: str = "1.0.0",
    model_path: Optional[str] = None,
    entrypoint_file: Optional[str] = None,
) -> Dict[str, Any]:
    """
    VieNeu-TTS (Vietnamese baseline) inference adapter.
    Uses official pnnbao97/VieNeu-TTS API:
    - from vieneu import Vieneu
    - engine = Vieneu(mode='v3turbo')
    - voice_data = engine.get_preset_voice(voice_id)
    - audio = engine.infer(text=text, voice=voice_data)
    """
    if not voice_id or voice_id not in VIENEU_ALLOWED_VOICES:
        raise ValueError(
            f"TTS_VOICE_ASSET_MISSING: unverified or unknown VieNeu voice preset {voice_id!r} "
            f"(must be one of {sorted(list(VIENEU_ALLOWED_VOICES))})"
        )

    if model_path:
        mp = Path(model_path).resolve()
        if not mp.exists():
            raise RuntimeError(f"WORKER_SNAPSHOT_PATH_REQUIRED: snapshot path does not exist: {model_path}")

        # Strictly resolve within verified snapshot root without parent/cache fallback.
        # Require exact canonical upstream catalog relative path: src/vieneu/assets/voices_v3_turbo.json.
        # Alternate catalog entrypoints are strictly rejected.
        canonical_catalog = (mp / "src" / "vieneu" / "assets" / "voices_v3_turbo.json").resolve()
        catalog_file = None
        if entrypoint_file:
            ep = Path(entrypoint_file).resolve()
            try:
                ep.relative_to(mp)
            except ValueError:
                raise RuntimeError(
                    f"TTS_MODEL_ASSET_MISSING: VieNeu catalog entrypoint escapes snapshot root: {entrypoint_file} (root: {model_path})"
                )
            if ep != canonical_catalog:
                raise RuntimeError(
                    f"TTS_MODEL_ASSET_MISSING: alternate or unverified VieNeu catalog entrypoint rejected: {entrypoint_file} (expected canonical: {canonical_catalog})"
                )
            if ep.is_file():
                catalog_file = ep

        if catalog_file is None:
            if canonical_catalog.is_file():
                catalog_file = canonical_catalog

        if catalog_file is None:
            raise RuntimeError(
                f"TTS_VOICE_ASSET_MISSING: VieNeu v3 Turbo voice catalog (src/vieneu/assets/voices_v3_turbo.json) missing in snapshot: {model_path}"
            )
        try:
            with open(catalog_file, "r", encoding="utf-8") as f:
                cat_data = json.load(f)
        except Exception as exc:
            raise RuntimeError(
                f"TTS_VOICE_ASSET_MISSING: failed to parse VieNeu voice catalog {catalog_file}: {exc}"
            ) from exc

        available_voices = set()
        if isinstance(cat_data, dict):
            presets = cat_data.get("presets", {})
            if isinstance(presets, dict):
                available_voices.update(presets.keys())
                for pk, pv in presets.items():
                    if isinstance(pv, dict):
                        if "id" in pv:
                            available_voices.add(str(pv["id"]))
                        if "name" in pv:
                            available_voices.add(str(pv["name"]))
            available_voices.update(cat_data.keys())
            if "voices" in cat_data and isinstance(cat_data["voices"], list):
                for item in cat_data["voices"]:
                    if isinstance(item, dict):
                        if "id" in item:
                            available_voices.add(str(item["id"]))
                        if "name" in item:
                            available_voices.add(str(item["name"]))
        elif isinstance(cat_data, list):
            for item in cat_data:
                if isinstance(item, dict):
                    if "id" in item:
                        available_voices.add(str(item["id"]))
                    if "name" in item:
                        available_voices.add(str(item["name"]))
                elif isinstance(item, str):
                    available_voices.add(item)

        if voice_id not in available_voices:
            raise RuntimeError(
                f"TTS_VOICE_ASSET_MISSING: requested voice {voice_id!r} not found in VieNeu catalog {catalog_file}"
            )
    factory = _VIENEU_MODEL_FACTORY
    if factory is None:
        if not model_path:
            raise RuntimeError(
                "WORKER_SNAPSHOT_PATH_REQUIRED: VieNeu in production RC requires a verified local snapshot model_path, unverified user cache / default Hub execution is strictly prohibited"
            )
        try:
            from vieneu import Vieneu
            factory = Vieneu
        except ImportError as exc:
            raise RuntimeError(
                "vieneu package not found: install VieNeu-TTS (pip install vieneu) to enable VieNeu synthesis"
            ) from exc
    # Offline defense-in-depth: guarantee no default HF repos are contacted at request time
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"
    os.environ["HF_DATASETS_OFFLINE"] = "1"

    init_kwargs = {"mode": "v3turbo"}
    if model_path:
        mp = Path(model_path).resolve()
        # Pinned VieNeu factory forwards kwargs to V3TurboVieNeuTTS, whose actual local model arg is backbone_repo
        init_kwargs["backbone_repo"] = str(mp)
        init_kwargs["model_path"] = str(mp)

        # Fixed in-root verified MOSS tokenizer layout (moss_tokenizer/).
        # Strictly no mp.parent or unverified discovery outside the snapshot root.
        moss_cand = mp / "moss_tokenizer"
        if not moss_cand.exists():
            raise RuntimeError(
                f"TTS_MODEL_ASSET_MISSING: verified local MOSS tokenizer missing in snapshot {model_path} (expected {moss_cand}); Hub fallback is strictly prohibited"
            )
        try:
            moss_cand.resolve().relative_to(mp)
        except ValueError:
            raise RuntimeError(
                f"TTS_MODEL_ASSET_MISSING: verified local MOSS tokenizer escapes snapshot root: {moss_cand}"
            )
        init_kwargs["moss_tokenizer"] = str(moss_cand)
    engine = _instantiate_model(factory, init_kwargs)
    if not hasattr(engine, "infer") and not callable(engine):
        raise RuntimeError("VieNeu engine instance missing required 'infer' method")

    # The synthesis voice data must come from the verified snapshot catalog
    # so the selected VoiceID actually uses snapshot-contained speaker_emb/codes,
    # rather than calling engine.get_preset_voice() which reads the installed SDK package catalog.
    voice_target = None
    if model_path and catalog_file is not None:
        voice_entry = None
        if isinstance(cat_data, dict):
            presets = cat_data.get("presets", {})
            if isinstance(presets, dict):
                if voice_id in presets:
                    voice_entry = presets[voice_id]
                else:
                    for pk, pv in presets.items():
                        if isinstance(pv, dict) and (pv.get("id") == voice_id or pv.get("name") == voice_id):
                            voice_entry = pv
                            break
            if voice_entry is None and voice_id in cat_data:
                voice_entry = cat_data[voice_id]
        elif isinstance(cat_data, list):
            for item in cat_data:
                if isinstance(item, dict) and (item.get("id") == voice_id or item.get("name") == voice_id):
                    voice_entry = item
                    break

        if voice_entry is not None:
            # Pinned V3Turbo _resolve_ref accepts a preset dict and needs both speaker_emb and codes.
            # Do NOT reduce the verified catalog entry to codes or speaker_emb; pass the full
            # snapshot-derived preset dict (with arrays converted as needed), or inject it then
            # resolve from that verified dict.
            voice_target = dict(voice_entry) if isinstance(voice_entry, dict) else voice_entry
            if isinstance(voice_target, dict):
                if "speaker_emb" not in voice_target and "embedding" in voice_target:
                    voice_target["speaker_emb"] = voice_target["embedding"]
                if "codes" not in voice_target and "ref_codes" in voice_target:
                    voice_target["codes"] = voice_target["ref_codes"]
        # Also inject verified snapshot presets into engine if it maintains internal presets
        if hasattr(engine, "preset_voices") and isinstance(engine.preset_voices, dict):
            engine.preset_voices = cat_data.get("presets", cat_data)
        if hasattr(engine, "_preset_voices") and isinstance(engine._preset_voices, dict):
            engine._preset_voices = cat_data.get("presets", cat_data)
    elif hasattr(engine, "get_preset_voice"):
        voice_target = engine.get_preset_voice(voice_id)
    else:
        voice_target = voice_id

    if voice_target is None:
        voice_target = voice_id

    # Call official infer method with verified snapshot voice data
    if hasattr(engine, "infer"):
        audio = engine.infer(text=text, voice=voice_target)
    else:
        audio = engine(text=text, voice=voice_target)
    # Determine audio array and sample rate (VieNeu v3 turbo is 48 kHz float32)
    sr = 48000
    if isinstance(audio, tuple) and len(audio) == 2:
        audio, sr = audio
    elif isinstance(audio, dict):
        sr = audio.get("sample_rate", sr)
        audio = audio.get("audio") or audio.get("audio_data") or audio.get("waveform")

    wav_bytes, dur_ms = encode_audio_to_wav(audio, sample_rate=sr)
    return build_tts_response(wav_bytes, dur_ms, model_name, model_version)


def run_cosyvoice_tts(
    text: str,
    language: str,
    voice_id: str,
    speed: float,
    slot_duration_ms: int = 0,
    model_name: str = "cosyvoice3",
    model_version: str = "3.0.0",
) -> Dict[str, Any]:
    """
    CosyVoice3 (Measured-duration speed-fit lane) inference adapter.
    Uses official FunAudioLLM/CosyVoice API:
    - from cosyvoice.cli.cosyvoice import AutoModel
    - cosyvoice = AutoModel(model_dir=...)
    - for j in cosyvoice.inference_sft(text, spk_id, stream=False, speed=curr_speed): ...
    Implements benchmark #21 multi-pass lane:
    Natural Pass -> Measure Duration -> Speed-Fit Recalibration -> Re-Measure.
    """
    factory = _COSYVOICE_MODEL_FACTORY
    if factory is None:
        try:
            from cosyvoice.cli.cosyvoice import AutoModel
            factory = AutoModel
        except ImportError:
            try:
                import cosyvoice.cli.cosyvoice as cv_cli
                factory = getattr(cv_cli, "AutoModel", None) or getattr(cv_cli, "CosyVoice3", None) or getattr(cv_cli, "CosyVoice", None)
            except ImportError as exc:
                raise RuntimeError(
                    "cosyvoice package not found: install CosyVoice3 to enable CosyVoice synthesis"
                ) from exc
        if factory is None:
            raise RuntimeError("CosyVoice package does not expose AutoModel / CosyVoice class")

    # Resolve local model directory if available
    model_dir = os.getenv("COSYVOICE_MODEL_DIR")
    if not model_dir:
        candidate_dirs = [
            os.path.expanduser(r"~/.cache/modelscope/hub/FunAudioLLM/Fun-CosyVoice3-0___5B-2512"),
            os.path.expanduser(r"~/.cache/modelscope/hub/FunAudioLLM/Fun-CosyVoice3-0.5B-2512"),
            "pretrained_models/Fun-CosyVoice3-0.5B",
            "pretrained_models/Fun-CosyVoice3-0.5B-2512",
            "FunAudioLLM/Fun-CosyVoice3-0.5B-2512",
        ]
        for c in candidate_dirs:
            if os.path.exists(c):
                model_dir = c
                break
        if not model_dir:
            model_dir = "FunAudioLLM/Fun-CosyVoice3-0.5B-2512"
    import torch
    use_fp16 = torch.cuda.is_available()
    model = _instantiate_model(factory, {"model_dir": model_dir, "fp16": use_fp16})

    target_spk = voice_id or "中文女"
    sample_rate = getattr(model, "sample_rate", 24000)

    # Resolve zero-shot prompt audio & text if spk2info is not populated
    has_spk_info = hasattr(model, "frontend") and hasattr(model.frontend, "spk2info") and target_spk in model.frontend.spk2info
    prompt_wav = os.getenv("COSYVOICE_PROMPT_AUDIO")
    if not prompt_wav and not has_spk_info:
        try:
            import cosyvoice
            pkg_root = os.path.dirname(os.path.dirname(os.path.abspath(cosyvoice.__file__)))
            pkg_dir = os.path.dirname(os.path.abspath(cosyvoice.__file__))
        except Exception:
            pkg_root = ""
            pkg_dir = ""

        for candidate_wav in [
            os.path.join(pkg_root, "asset", "zero_shot_prompt.wav"),
            os.path.join(pkg_dir, "asset", "zero_shot_prompt.wav"),
            os.path.join(model_dir, "asset", "zero_shot_prompt.wav"),
            "./asset/zero_shot_prompt.wav",
        ]:
            if candidate_wav and os.path.exists(candidate_wav):
                prompt_wav = candidate_wav
                break
    prompt_text = "You are a helpful assistant.<|endofprompt|>希望你以后能够做的比我还好呦。"

    def synthesize_pass(curr_speed: float) -> Tuple[bytes, int]:
        if has_spk_info and hasattr(model, "inference_sft"):
            gen = model.inference_sft(text, target_spk, stream=False, speed=curr_speed)
        elif prompt_wav and hasattr(model, "inference_zero_shot"):
            gen = model.inference_zero_shot(text, prompt_text, prompt_wav, stream=False, speed=curr_speed)
        elif hasattr(model, "inference_sft"):
            gen = model.inference_sft(text, target_spk, stream=False, speed=curr_speed)
        elif hasattr(model, "inference_zero_shot"):
            gen = model.inference_zero_shot(text, text, target_spk, stream=False, speed=curr_speed)
        elif callable(model):
            gen = model(text, target_spk, stream=False, speed=curr_speed)
        else:
            raise RuntimeError("CosyVoice model instance missing inference_sft / inference_zero_shot method")

        # Collect audio segments from generator, list, or single object
        if isinstance(gen, (list, tuple)) and len(gen) > 0 and isinstance(gen[0], dict):
            chunks = []
            for item in gen:
                if isinstance(item, dict) and "tts_speech" in item:
                    chunks.append(item["tts_speech"])
                elif isinstance(item, dict) and "audio" in item:
                    chunks.append(item["audio"])
                else:
                    chunks.append(item)
            audio = _concat_audio_chunks(chunks)
        elif hasattr(gen, "__iter__") and not isinstance(gen, (bytes, list, tuple)):
            chunks = []
            for item in gen:
                if isinstance(item, dict) and "tts_speech" in item:
                    chunks.append(item["tts_speech"])
                elif isinstance(item, dict) and "audio" in item:
                    chunks.append(item["audio"])
                elif isinstance(item, (bytes, list, tuple)) or hasattr(item, "flatten"):
                    chunks.append(item)
            if not chunks:
                raise RuntimeError("CosyVoice generator produced empty audio")
            audio = _concat_audio_chunks(chunks)
        elif isinstance(gen, dict):
            audio = gen.get("tts_speech") or gen.get("audio") or gen.get("waveform") or gen
        else:
            audio = gen
        return encode_audio_to_wav(audio, sample_rate=sample_rate)

    # 1. Natural Pass
    wav_bytes, natural_dur_ms = synthesize_pass(speed)
    final_dur_ms = natural_dur_ms

    # 2. Measured-duration speed-fit lane (Benchmark #21)
    # If slot_duration_ms is constrained and natural duration exceeds slot, re-synthesize with speed-fit
    if slot_duration_ms > 0 and natural_dur_ms > slot_duration_ms:
        # Calculate calibrated speed adjustment factor
        speed_adjustment = natural_dur_ms / float(slot_duration_ms)
        fitted_speed = min(2.0, max(0.5, speed * speed_adjustment))
        fit_wav_bytes, fit_dur_ms = synthesize_pass(fitted_speed)
        wav_bytes = fit_wav_bytes
        final_dur_ms = fit_dur_ms

    return build_tts_response(
        wav_bytes=wav_bytes,
        measured_duration_ms=final_dur_ms,
        model_name=model_name,
        model_version=model_version,
        predicted_duration_ms=natural_dur_ms,
    )


def run_kokoro_tts(
    text: str,
    language: str,
    voice_id: str,
    speed: float,
    model_name: str = "kokoro-tts",
    model_version: str = "1.0.0",
    model_path: Optional[str] = None,
    entrypoint_file: Optional[str] = None,
) -> Dict[str, Any]:
    """
    Kokoro-TTS (English baseline) inference adapter.
    Uses official kokoro / KPipeline API:
    - from kokoro import KPipeline
    - pipeline = KPipeline(lang_code='a')
    - for result in pipeline(text, voice=voice_id, speed=speed):
    -     audio = result.audio
    """
    if not voice_id or voice_id not in KOKORO_ALLOWED_VOICES:
        raise ValueError(
            f"TTS_VOICE_ASSET_MISSING: unverified or unknown Kokoro voice preset {voice_id!r} "
            f"(must be one of {sorted(list(KOKORO_ALLOWED_VOICES))})"
        )

    config_file = None
    weights_file = None
    voice_file = None
    if model_path:
        mp = Path(model_path).resolve()
        if not mp.exists():
            raise RuntimeError(f"WORKER_SNAPSHOT_PATH_REQUIRED: snapshot path does not exist: {model_path}")

        # 1. Validate checkpoint sha256 within snapshot root
        for cand in [mp / "kokoro-v1_0.pth", mp / "kokoro-v1.0.pth", mp / "kokoro.pth", mp / "model.pth"]:
            if cand.is_file():
                try:
                    cand.resolve().relative_to(mp)
                except ValueError:
                    raise RuntimeError(f"TTS_MODEL_ASSET_MISSING: Kokoro weights file escapes snapshot root: {cand}")
                weights_file = cand
                h = hashlib.sha256()
                with open(cand, "rb") as f:
                    while chunk := f.read(65536):
                        h.update(chunk)
                if h.hexdigest() != KOKORO_CHECKPOINT_SHA256:
                    raise RuntimeError(
                        f"TTS_VOICE_ASSET_MISSING: Kokoro checkpoint sha256 mismatch: expected {KOKORO_CHECKPOINT_SHA256}, got {h.hexdigest()}"
                    )
                break

        # 2. Verify voice asset file exists in snapshot root (canonical path: voices/<voiceID>.pt)
        canonical_voice = (mp / "voices" / f"{voice_id}.pt").resolve()
        if entrypoint_file:
            ep = Path(entrypoint_file).resolve()
            try:
                ep.relative_to(mp)
            except ValueError:
                raise RuntimeError(
                    f"TTS_VOICE_ASSET_MISSING: Kokoro voice entrypoint escapes snapshot root: {entrypoint_file} (root: {model_path})"
                )
            if ep != canonical_voice:
                raise RuntimeError(
                    f"TTS_VOICE_ASSET_MISSING: alternate or unverified Kokoro voice entrypoint rejected: {entrypoint_file} (expected canonical: {canonical_voice})"
                )
            if ep.is_file():
                voice_file = ep

        if voice_file is None:
            if not canonical_voice.is_file():
                raise RuntimeError(
                    f"TTS_VOICE_ASSET_MISSING: Kokoro voice asset missing from snapshot: {canonical_voice}"
                )
            voice_file = canonical_voice
        # 3. Check config.json strictly in root (no parent fallback)
        config_cand = mp / "config.json"
        if not config_cand.is_file():
            raise RuntimeError(f"TTS_MODEL_ASSET_MISSING: Kokoro config.json missing in snapshot: {model_path}")
        try:
            config_cand.resolve().relative_to(mp)
        except ValueError:
            raise RuntimeError(f"TTS_MODEL_ASSET_MISSING: Kokoro config.json escapes snapshot root: {config_cand}")
        config_file = config_cand
    lang_code = "a" if language in ("en", "en-us") else "b" if language == "en-gb" else "a"
    factory = _KOKORO_MODEL_FACTORY
    voice_target = voice_id
    if factory is not None:
        init_kwargs = {"lang_code": lang_code}
        pipeline = _instantiate_model(factory, init_kwargs)
        if voice_file is not None:
            voice_target = str(voice_file)
    else:
        try:
            from kokoro import KPipeline, KModel
        except ImportError as exc:
            raise RuntimeError(
                "kokoro package not found: install Kokoro (pip install kokoro misaki) to enable Kokoro synthesis"
            ) from exc

        # Fail closed: Kokoro in production RC requires a verified snapshot model_path
        if not model_path:
            raise RuntimeError("WORKER_SNAPSHOT_PATH_REQUIRED: Kokoro requires verified local snapshot paths; Hub fallback is strictly prohibited")
        if config_file is None:
            raise RuntimeError(f"TTS_MODEL_ASSET_MISSING: Kokoro config.json missing in snapshot: {model_path}")
        if weights_file is None:
            raise RuntimeError(f"TTS_MODEL_ASSET_MISSING: Kokoro model weights (.pth) missing in snapshot: {model_path}")

        # Strictly local-only path: zero request-time Hub/network download
        os.environ["HF_HUB_OFFLINE"] = "1"
        os.environ["TRANSFORMERS_OFFLINE"] = "1"

        try:
            kmodel = KModel(config=str(config_file), model=str(weights_file))
        except Exception as e:
            raise RuntimeError(
                f"TTS_VOICE_ASSET_MISSING: failed to initialize local Kokoro KModel from snapshot {model_path}: {e}"
            ) from e

        pipeline = KPipeline(lang_code=lang_code, model=kmodel)
        # KPipeline accepts local .pt path directly
        voice_target = str(voice_file)
        try:
            import torch
            loaded_voice = torch.load(str(voice_file), weights_only=True)
            pipeline.voices[voice_id] = loaded_voice
            pipeline.voices[str(voice_file)] = loaded_voice
        except Exception:
            pass

    chunks = []
    if callable(pipeline):
        gen = pipeline(text, voice=voice_target, speed=speed)
        for res in gen:
            # Official KPipeline yields Result objects with .audio property
            if hasattr(res, "audio") and res.audio is not None:
                chunks.append(res.audio)
            elif isinstance(res, (list, tuple)) and len(res) >= 3:
                # Backward-compat tuple (graphemes, phonemes, audio)
                chunks.append(res[2])
            elif isinstance(res, dict) and "audio" in res:
                chunks.append(res["audio"])
            else:
                chunks.append(res)

    if not chunks:
        raise RuntimeError("Kokoro pipeline produced empty audio")

    audio = _concat_audio_chunks(chunks)
    # Kokoro native sample rate is 24000 Hz
    wav_bytes, dur_ms = encode_audio_to_wav(audio, sample_rate=24000)
    return build_tts_response(wav_bytes, dur_ms, model_name, model_version)

def run_chatterbox_tts(
    text: str,
    language: str,
    voice_id: str,
    speed: float,
    model_name: str = "chatterbox-tts",
    model_version: str = "1.0.0",
) -> Dict[str, Any]:
    """
    Chatterbox-TTS inference adapter.
    Uses official Chatterbox API:
    - from chatterbox.tts import ChatterboxTTS
    - model = ChatterboxTTS.from_pretrained(device=device)
    - wav = model.generate(text, audio_prompt_path=...)
    """
    factory = _CHATTERBOX_MODEL_FACTORY
    if factory is None:
        try:
            from chatterbox.tts import ChatterboxTTS
            factory = ChatterboxTTS
        except ImportError as exc:
            raise RuntimeError(
                "chatterbox package not found: install Chatterbox to enable Chatterbox synthesis"
            ) from exc

    import torch
    device = "cuda" if torch.cuda.is_available() else "cpu"

    if hasattr(factory, "from_pretrained"):
        model = factory.from_pretrained(device=device)
    else:
        model = _instantiate_model(factory, {"device": device})

    # Only pass audio_prompt_path if voice_id points to an actual file on disk
    prompt_path = voice_id if (voice_id and os.path.isfile(voice_id)) else None

    if hasattr(model, "generate"):
        audio = model.generate(text, audio_prompt_path=prompt_path)
    elif callable(model):
        audio = model(text, audio_prompt_path=prompt_path)
    else:
        raise RuntimeError("Chatterbox model instance missing generate method")

    sr = getattr(model, "sr", 24000)
    wav_bytes, dur_ms = encode_audio_to_wav(audio, sample_rate=sr)
    return build_tts_response(wav_bytes, dur_ms, model_name, model_version)


def run_tts(req: Dict[str, Any]) -> Dict[str, Any]:
    """Main dispatch for StageWorker TTS request."""
    text = req.get("text", "")
    if not text.strip():
        raise ValueError("empty text provided for TTS synthesis")

    language = req.get("language", "vi").lower()
    voice_id = req.get("voice_id", "")
    speed_raw = req.get("speed", 1.0)
    try:
        speed = float(speed_raw)
    except (ValueError, TypeError):
        speed = 1.0

    slot_duration_ms = int(req.get("slot_duration_ms", 0))
    model_name = req.get("model_name", "vieneu-tts").lower()
    model_version = req.get("model_version", "1.0.0")
    model_path = req.get("model_path")
    entrypoint_file = req.get("entrypoint_file")

    # Explicit recognized model names take precedence over language fallback
    if "vieneu" in model_name or "pnnbao" in model_name:
        return run_vieneu_tts(
            text=text,
            language=language,
            voice_id=voice_id,
            speed=speed,
            model_name=model_name,
            model_version=model_version,
            model_path=model_path,
            entrypoint_file=entrypoint_file,
        )
    elif "cosyvoice" in model_name or "cosy" in model_name:
        return run_cosyvoice_tts(
            text=text,
            language=language,
            voice_id=voice_id,
            speed=speed,
            slot_duration_ms=slot_duration_ms,
            model_name=model_name,
            model_version=model_version,
        )
    elif "kokoro" in model_name:
        return run_kokoro_tts(
            text=text,
            language=language,
            voice_id=voice_id,
            speed=speed,
            model_name=model_name,
            model_version=model_version,
            model_path=model_path,
            entrypoint_file=entrypoint_file,
        )
    elif "chatterbox" in model_name:
        return run_chatterbox_tts(
            text=text,
            language=language,
            voice_id=voice_id,
            speed=speed,
            model_name=model_name,
            model_version=model_version,
        )
    else:
        # Default to VieNeu for VI, Kokoro for EN, CosyVoice for others
        if language == "en":
            return run_kokoro_tts(
                text, language, voice_id, speed, model_name, model_version, model_path=model_path, entrypoint_file=entrypoint_file
            )
        elif language == "vi":
            return run_vieneu_tts(
                text, language, voice_id, speed, model_name, model_version, model_path=model_path, entrypoint_file=entrypoint_file
            )
        elif language == "zh":
            return run_cosyvoice_tts(text, language, voice_id, speed, slot_duration_ms, model_name, model_version)
        else:
            return run_vieneu_tts(
                text, language, voice_id, speed, model_name, model_version, model_path=model_path, entrypoint_file=entrypoint_file
            )

def main():
    if hasattr(sys.stdin, "reconfigure"):
        sys.stdin.reconfigure(encoding="utf-8")
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")
    if hasattr(sys.stderr, "reconfigure"):
        sys.stderr.reconfigure(encoding="utf-8")

    try:
        raw_in = sys.stdin.read()
        if not raw_in.strip():
            sys.stderr.write("Error: empty input received on stdin\n")
            sys.exit(1)
        req = json.loads(raw_in)
        res = run_tts(req)
        sys.stdout.write(json.dumps(res) + "\n")
        sys.stdout.flush()
    except Exception as exc:
        sys.stderr.write(f"TTS Error: {exc}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
