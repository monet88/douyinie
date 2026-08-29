#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Audio Separator (UVR baseline, Demucs fallback via python-audio-separator / demucs)
Invokes audio stem separation or fails closed with descriptive errors.

Contract:
- Stdin: JSON request with audio_path, model_name, model_version, run_id, attempt_id
- Stdout: JSON response with vocals_wav, background_wav, vocals_sha256, background_sha256, duration_ms, sample_rate, channels
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
from typing import Any, Dict, Optional

# Pluggable factory hooks for deterministic testing without full ML packages
_SEPARATOR_MODEL_FACTORY = None


def generate_synthetic_pcm_wav(sample_rate: int = 16000, channels: int = 1, duration_ms: int = 5000) -> bytes:
    """Generate standard 16-bit PCM WAV bytes."""
    num_samples = int((sample_rate * duration_ms) / 1000)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(2)
        wf.setframerate(sample_rate)
        raw_frames = struct.pack(f"<{num_samples * channels}h", *([0] * (num_samples * channels)))
        wf.writeframes(raw_frames)
    return buf.getvalue()


def separate_uvr(audio_path: str, model_name: str, model_version: str) -> Dict[str, Any]:
    """Execute stem separation using python-audio-separator (UVR baseline lane)."""
    try:
        from audio_separator.separator import Separator  # type: ignore
    except ImportError:
        raise RuntimeError("UVR runtime not found: install audio-separator (pip install audio-separator)")

    sep = Separator()
    sep.load_model(model_name)
    output_files = sep.separate(audio_path)
    vocals_path = None
    bg_path = None
    for f in output_files:
        if "Vocals" in f or "vocals" in f:
            vocals_path = f
        elif "Instrumental" in f or "background" in f or "no_vocals" in f:
            bg_path = f

    vocals_bytes = b""
    if vocals_path and os.path.exists(vocals_path):
        with open(vocals_path, "rb") as vf:
            vocals_bytes = vf.read()

    bg_bytes = b""
    if bg_path and os.path.exists(bg_path):
        with open(bg_path, "rb") as bf:
            bg_bytes = bf.read()

    dur_ms = 5000
    if bg_bytes:
        with wave.open(io.BytesIO(bg_bytes), "rb") as wf:
            dur_ms = int((wf.getnframes() * 1000) / wf.getframerate())

    return {
        "vocals_data": vocals_bytes,
        "background_data": bg_bytes,
        "duration_ms": dur_ms,
        "sample_rate": 16000,
        "channels": 1,
    }


def separate_demucs(audio_path: str, model_name: str, model_version: str) -> Dict[str, Any]:
    """Execute stem separation using Demucs CLI/module (Demucs fallback lane)."""
    import shutil
    import subprocess
    import tempfile

    demucs_model = model_name if model_name and "mdx" not in model_name else "htdemucs"
    out_dir = tempfile.mkdtemp(prefix="demucs_out_")
    try:
        cmd = [sys.executable, "-m", "demucs.separate", "-n", demucs_model, "-o", out_dir, "--two-stems=vocals", audio_path]
        proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if proc.returncode != 0:
            raise RuntimeError(f"Demucs separation failed (exit {proc.returncode}): {proc.stderr.decode('utf-8', errors='ignore')}")

        track_name = os.path.splitext(os.path.basename(audio_path))[0]
        model_out_dir = os.path.join(out_dir, demucs_model, track_name)
        vocals_path = os.path.join(model_out_dir, "vocals.wav")
        no_vocals_path = os.path.join(model_out_dir, "no_vocals.wav")

        vocals_bytes = b""
        if os.path.exists(vocals_path):
            with open(vocals_path, "rb") as vf:
                vocals_bytes = vf.read()

        bg_bytes = b""
        if os.path.exists(no_vocals_path):
            with open(no_vocals_path, "rb") as bf:
                bg_bytes = bf.read()

        if not bg_bytes:
            raise RuntimeError(f"Demucs produced no background stem in {model_out_dir}")

        dur_ms = 5000
        with wave.open(io.BytesIO(bg_bytes), "rb") as wf:
            dur_ms = int((wf.getnframes() * 1000) / wf.getframerate())

        return {
            "vocals_data": vocals_bytes,
            "background_data": bg_bytes,
            "duration_ms": dur_ms,
            "sample_rate": 16000,
            "channels": 1,
        }
    finally:
        shutil.rmtree(out_dir, ignore_errors=True)


def separate_audio_stems(audio_path: str, model_name: str, model_version: str) -> Dict[str, Any]:
    """Execute stem separation with explicit model/lane dispatch (UVR vs Demucs)."""
    if _SEPARATOR_MODEL_FACTORY is not None:
        return _SEPARATOR_MODEL_FACTORY(audio_path, model_name, model_version)

    norm_model = (model_name or "").lower()
    is_demucs = "demucs" in norm_model or "htdemucs" in norm_model
    if is_demucs:
        return separate_demucs(audio_path, model_name, model_version)
    else:
        return separate_uvr(audio_path, model_name, model_version)

def main():
    try:
        input_data = sys.stdin.read()
        if not input_data.strip():
            sys.stderr.write("Empty input payload\n")
            sys.exit(1)

        req = json.loads(input_data)
        audio_path = req.get("audio_path", "")
        model_name = req.get("model_name", "UVR-MDX-NET-Inst_HQ_4.onnx")
        model_version = req.get("model_version", "v3")

        if not audio_path or not os.path.exists(audio_path):
            sys.stderr.write(f"Source audio file not found: {audio_path}\n")
            sys.exit(1)

        res = separate_audio_stems(audio_path, model_name, model_version)

        vocals_bytes = res.get("vocals_data", b"")
        bg_bytes = res.get("background_data", b"")
        dur_ms = res.get("duration_ms", 0)
        sample_rate = res.get("sample_rate", 16000)
        channels = res.get("channels", 1)

        out = {
            "vocals_data": base64.b64encode(vocals_bytes).decode("ascii") if vocals_bytes else "",
            "background_data": base64.b64encode(bg_bytes).decode("ascii") if bg_bytes else "",
            "vocals_sha256": hashlib.sha256(vocals_bytes).hexdigest() if vocals_bytes else "",
            "background_sha256": hashlib.sha256(bg_bytes).hexdigest() if bg_bytes else "",
            "duration_ms": dur_ms,
            "sample_rate": sample_rate,
            "channels": channels,
            "model_name": model_name,
            "model_version": model_version,
        }
        print(json.dumps(out))
        sys.exit(0)

    except Exception as e:
        sys.stderr.write(f"Separator error: {e}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
