#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Audio Role Analyzer via Google YAMNet TFLite v1
Classifies normalized audio and isolated stems into canonical AudioRolePlan segments.

Contract:
- Stdin: JSON request with vocals_audio, background_audio (or source_audio), model_path, config, mode
- Stdout: JSON response with segments, model_name, model_version, runtime_identity
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import csv
import hashlib
import json
import os
import sys
import wave
from typing import Any, Dict, List, Optional, Tuple

_YAMNET_MODEL_FACTORY = None
_YAMNET_PROBE_FACTORY = None

PINNED_YAMNET_PACKAGE = "ai-edge-litert"
PINNED_YAMNET_VERSION = "2.2.0"
PINNED_ADAPTER_REVISION = "cmd/stageworker/adapters/audio_role_yamnet.py@v2.2.0"
PINNED_MODEL_NAME = "yamnet"
PINNED_MODEL_VERSION = "v1"

# AudioSet Class ID Sets for Canonical Roles
DIALOGUE_INDICES = {0, 1, 2, 3, 4, 5, 6, 9, 12}
SINGING_INDICES = {24, 25, 26, 27, 28, 29, 30, 31, 32}
# Classes 132 to 276: Music, musical instruments, genres, background music
INSTRUMENTAL_INDICES = set(range(132, 277))
# All other AudioSet classes fall into ambience / sound effects / foley
SFX_INDICES = (set(range(0, 521)) - DIALOGUE_INDICES - SINGING_INDICES - INSTRUMENTAL_INDICES)

# Versioned conservative bootstrap mapping configuration.
# NOTE: YAMNet raw output scores are uncalibrated activations across 521 AudioSet classes,
# not calibrated posterior probabilities. These thresholds represent conservative operational
# decision bounds for bootstrap classification, preferring "uncertain" over false-positive
# auto-classification or premature no-dub.
BOOTSTRAP_CONSERVATIVE_CONFIG = {
    "version": "1.0-bootstrap-conservative",
    "window_samples": 15600,
    "sample_rate": 16000,
    "dialogue_threshold": 0.30,
    "singing_threshold": 0.25,
    "instrumental_threshold": 0.25,
    "ambience_threshold": 0.25,
    "vocal_rms_threshold": 0.008,
    "uncertain_margin": 0.08,
    "min_confidence": 0.20,
}
DEFAULT_CONFIG = BOOTSTRAP_CONSERVATIVE_CONFIG


def read_wav_mono_16k(path: str) -> Tuple[List[float], int, int]:
    """Read a WAV file and return mono float32 samples in [-1.0, 1.0], sample_rate, duration_ms."""
    if not os.path.exists(path):
        raise FileNotFoundError(f"audio file not found: {path}")
    if os.path.isdir(path):
        raise IsADirectoryError(f"audio path is a directory: {path}")

    try:
        with wave.open(path, "rb") as wf:
            channels = wf.getnchannels()
            sample_width = wf.getsampwidth()
            sample_rate = wf.getframerate()
            num_frames = wf.getnframes()

            if sample_rate != 16000:
                raise ValueError(f"unsupported sample rate: {sample_rate} Hz (expected 16000 Hz)")
            if sample_width != 2:
                raise ValueError(f"unsupported sample width: {sample_width} bytes (expected 16-bit PCM)")

            raw_bytes = wf.readframes(num_frames)
    except wave.Error as e:
        raise ValueError(f"corrupt or unreadable WAV file: {e}")

    # Parse 16-bit signed integers
    import struct
    total_samples = len(raw_bytes) // 2
    if total_samples == 0:
        return [], sample_rate, 0

    fmt = f"<{total_samples}h"
    unpacked = struct.unpack(fmt, raw_bytes)

    if channels == 1:
        float_samples = [s / 32768.0 for s in unpacked]
    else:
        # Downmix multi-channel to mono by averaging
        float_samples = []
        for i in range(0, total_samples, channels):
            frame_avg = sum(unpacked[i : i + channels]) / (channels * 32768.0)
            float_samples.append(frame_avg)

    duration_ms = int(len(float_samples) * 1000 / sample_rate)
    return float_samples, sample_rate, duration_ms


def compute_rms(samples: List[float]) -> float:
    if not samples:
        return 0.0
    s_sq = sum(x * x for x in samples)
    return (s_sq / len(samples)) ** 0.5


def probe_runtime_identity() -> Dict[str, Any]:
    if _YAMNET_PROBE_FACTORY is not None and callable(_YAMNET_PROBE_FACTORY):
        return _YAMNET_PROBE_FACTORY()

    try:
        import ai_edge_litert
        ver = getattr(ai_edge_litert, "__version__", "")
    except ImportError:
        try:
            import importlib.metadata
            ver = importlib.metadata.version("ai-edge-litert")
        except Exception:
            ver = ""

    if ver != PINNED_YAMNET_VERSION:
        raise RuntimeError(
            f"ai-edge-litert runtime mismatch: expected {PINNED_YAMNET_VERSION}, found {ver or 'missing'}"
        )

    return {
        "status": "ok",
        "package_name": PINNED_YAMNET_PACKAGE,
        "package_version": PINNED_YAMNET_VERSION,
        "source_revision": "google/yamnet@v1",
        "runtime_versions": {
            "ai-edge-litert": PINNED_YAMNET_VERSION,
        },
        "adapter_revision": PINNED_ADAPTER_REVISION,
    }


def resolve_model_path(model_path: str) -> str:
    """Resolve tflite file path from snapshot root directory or direct file path."""
    if not model_path:
        raise RuntimeError("model_path is required; fail-closed")
    if os.path.isdir(model_path):
        candidate = os.path.join(model_path, "yamnet.tflite")
        if os.path.exists(candidate):
            return candidate
        raise RuntimeError(f"yamnet.tflite not found in snapshot root: {model_path}")
    if os.path.isfile(model_path):
        return model_path
    raise RuntimeError(f"model_path does not exist: {model_path}")


class YAMNetClassifier:
    def __init__(self, model_path: str):
        if _YAMNET_MODEL_FACTORY is not None and callable(_YAMNET_MODEL_FACTORY):
            self.impl = _YAMNET_MODEL_FACTORY(model_path)
            return

        resolved_tflite = resolve_model_path(model_path)
        try:
            import ai_edge_litert.interpreter as litert
            self.interp = litert.Interpreter(model_path=resolved_tflite)
            self.interp.allocate_tensors()
            self.input_idx = self.interp.get_input_details()[0]["index"]
            self.output_idx = self.interp.get_output_details()[0]["index"]
            self.impl = None
        except Exception as e:
            raise RuntimeError(f"failed to initialize LiteRT interpreter for YAMNet: {e}")

    def infer(self, window_15600: List[float]) -> List[float]:
        if self.impl is not None:
            return self.impl.infer(window_15600)

        import numpy as np
        arr = np.array(window_15600, dtype=np.float32)
        if len(arr) < 15600:
            arr = np.pad(arr, (0, 15600 - len(arr)))
        elif len(arr) > 15600:
            arr = arr[:15600]

        self.interp.set_tensor(self.input_idx, arr)
        self.interp.invoke()
        scores = self.interp.get_tensor(self.output_idx)[0]
        return scores.tolist()


def classify_window(
    vocals_window: List[float],
    bg_window: List[float],
    classifier: YAMNetClassifier,
    cfg: Dict[str, Any],
    is_source_only: bool = False,
) -> str:
    v_rms = compute_rms(vocals_window)
    v_scores = classifier.infer(vocals_window)
    bg_scores = classifier.infer(bg_window) if bg_window != vocals_window else v_scores

    s_dial = max(v_scores[i] for i in DIALOGUE_INDICES)
    s_sing = max(v_scores[i] for i in SINGING_INDICES)
    s_inst = max(bg_scores[i] for i in INSTRUMENTAL_INDICES)
    s_sfx = max(bg_scores[i] for i in SFX_INDICES)

    vocal_rms_thresh = cfg.get("vocal_rms_threshold", 0.008)
    uncertain_margin = cfg.get("uncertain_margin", 0.08)
    min_confidence = cfg.get("min_confidence", 0.20)
    dial_thresh = cfg.get("dialogue_threshold", 0.30)
    sing_thresh = cfg.get("singing_threshold", 0.25)
    inst_thresh = cfg.get("instrumental_threshold", 0.25)
    amb_thresh = cfg.get("ambience_threshold", 0.25)

    if is_source_only:
        # Source-only fallback without isolated vocal stem must be conservative:
        # do not infer confident no-dub from absence of vocal evidence alone.
        if s_dial >= dial_thresh and s_dial > s_sing:
            if abs(s_dial - s_sing) < uncertain_margin:
                return "uncertain"
            return "narration/dialogue"
        if s_sing >= sing_thresh and s_sing > s_dial:
            if abs(s_sing - s_dial) < uncertain_margin:
                return "uncertain"
            return "singing/music-vocal"
        # Only strong positive non-dialogue evidence can classify as background or SFX
        if s_inst >= inst_thresh and s_inst > s_sfx:
            if abs(s_inst - s_sfx) < uncertain_margin:
                return "uncertain"
            return "instrumental/background"
        if s_sfx >= amb_thresh and s_sfx > s_inst:
            if abs(s_sfx - s_inst) < uncertain_margin:
                return "uncertain"
            return "ambience/SFX"
        return "uncertain"

    if v_rms >= vocal_rms_thresh:
        # Vocals stem has measurable sound
        if abs(s_dial - s_sing) < uncertain_margin and max(s_dial, s_sing) >= min_confidence:
            return "uncertain"
        if s_dial >= dial_thresh and s_dial > s_sing:
            return "narration/dialogue"
        if s_sing >= sing_thresh and s_sing > s_dial:
            return "singing/music-vocal"
        return "uncertain"
    else:
        # Vocals stem is silent / below RMS threshold
        # Positive non-dialogue evidence is strictly required for background/SFX
        if abs(s_inst - s_sfx) < uncertain_margin and max(s_inst, s_sfx) >= min_confidence:
            return "uncertain"
        if s_inst >= inst_thresh and s_inst > s_sfx:
            return "instrumental/background"
        if s_sfx >= amb_thresh and s_sfx > s_inst:
            return "ambience/SFX"
        # Low-confidence background (both instrumental and SFX below safe evidence threshold) => uncertain!
        # NEVER default to instrumental/background when evidence is weak or quiet!
        return "uncertain"

def merge_adjacent_segments(raw_segments: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    if not raw_segments:
        return []
    merged = []
    curr = dict(raw_segments[0])
    for s in raw_segments[1:]:
        if s["role"] == curr["role"] and s["start_ms"] == curr["end_ms"]:
            curr["end_ms"] = s["end_ms"]
        else:
            merged.append(curr)
            curr = dict(s)
    merged.append(curr)
    return merged


def run_audio_role_analysis(req: Dict[str, Any]) -> Dict[str, Any]:
    model_path = req.get("model_path", "")
    classifier = YAMNetClassifier(model_path)

    cfg = dict(DEFAULT_CONFIG)
    if "config" in req and isinstance(req["config"], dict):
        cfg.update(req["config"])

    vocals_path = req.get("vocals_audio", "")
    bg_path = req.get("background_audio", "")
    src_path = req.get("source_audio", "")

    if not vocals_path and not src_path:
        raise ValueError("neither vocals_audio nor source_audio provided; fail-closed")

    if vocals_path:
        vocals_samples, sr, duration_ms = read_wav_mono_16k(vocals_path)
    else:
        vocals_samples, sr, duration_ms = read_wav_mono_16k(src_path)

    if bg_path:
        bg_samples, _, _ = read_wav_mono_16k(bg_path)
    elif src_path:
        bg_samples, _, _ = read_wav_mono_16k(src_path)
    else:
        bg_samples = [0.0] * len(vocals_samples)

    # Pad shorter stem to match longer
    max_len = max(len(vocals_samples), len(bg_samples))
    if len(vocals_samples) < max_len:
        vocals_samples.extend([0.0] * (max_len - len(vocals_samples)))
    if len(bg_samples) < max_len:
        bg_samples.extend([0.0] * (max_len - len(bg_samples)))

    window_samples = cfg.get("window_samples", 15600)
    raw_segments = []

    if max_len == 0 or duration_ms == 0:
        raise ValueError("audio input is empty or contains zero readable samples; fail-closed")

    is_source_only = not bool(vocals_path)
    for idx in range(0, max_len, window_samples):
        start_ms = int(idx * 1000 / sr)
        end_ms = min(duration_ms, int((idx + window_samples) * 1000 / sr))
        v_win = vocals_samples[idx : idx + window_samples]
        b_win = bg_samples[idx : idx + window_samples]
        role = classify_window(v_win, b_win, classifier, cfg, is_source_only=is_source_only)
        raw_segments.append({
            "start_ms": start_ms,
            "end_ms": end_ms,
            "role": role,
        })

    merged_segments = merge_adjacent_segments(raw_segments)

    return {
        "segments": merged_segments,
        "model_name": PINNED_MODEL_NAME,
        "model_version": PINNED_MODEL_VERSION,
        "runtime_identity": f"{PINNED_YAMNET_PACKAGE} {PINNED_YAMNET_VERSION}",
        "provider_id": "yamnet_worker",
    }


def main():
    try:
        raw_in = sys.stdin.read()
        if not raw_in.strip():
            sys.stderr.write("Empty stdin JSON request\n")
            sys.exit(1)
        req = json.loads(raw_in)
    except Exception as e:
        sys.stderr.write(f"Failed to parse stdin JSON: {e}\n")
        sys.exit(1)

    mode = req.get("mode", "analyze")
    try:
        if mode == "probe":
            result = probe_runtime_identity()
        elif mode == "analyze":
            result = run_audio_role_analysis(req)
        else:
            sys.stderr.write(f"Unknown mode: {mode}\n")
            sys.exit(1)

        sys.stdout.write(json.dumps(result) + "\n")
        sys.stdout.flush()
        sys.exit(0)
    except Exception as e:
        sys.stderr.write(f"AudioRole analysis failed: {e}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
