#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: SenseVoice-Small ONNX
Invokes Sherpa-ONNX or FunASR-ONNX for fast, lightweight Chinese speech recognition.

Contract:
- Stdin: JSON request with audio_path, model_name, model_version, run_id, attempt_id,
  model_path, require_model_snapshot
- Stdout: JSON response with segments, model_name, model_version
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import json
import os
import re
import sys
from typing import Any, Dict, List, Optional, Tuple

DEFAULT_MODEL_NAME = "sensevoice-small"
DEFAULT_MODEL_VERSION = "int8"

# Hook for deterministic unit testing without native ONNX runtime wheels
_SENSEVOICE_MODEL_FACTORY = None


def clean_sensevoice_text(text: str) -> str:
    """
    Remove SenseVoice special tags such as <|zh|>, <|NEUTRAL|>, <|Speech|>, <|woitn|>, etc.
    Returns clean natural speech text.
    """
    if not text:
        return ""
    # Strip <|tag|> patterns
    cleaned = re.sub(r"<\|[^>]+?>", "", text)
    return cleaned.strip()


def parse_sensevoice_output(raw_output: Any) -> List[Dict[str, Any]]:
    """
    Parse raw output from SenseVoice engine into canonical domain ASRRawSegment dicts.
    Handles Sherpa-ONNX OfflineRecognizer results, FunASR dict output, or custom mocks.
    """
    if not raw_output:
        return []

    segments: List[Dict[str, Any]] = []

    # If raw_output is a string
    if isinstance(raw_output, str):
        cleaned = clean_sensevoice_text(raw_output)
        if cleaned:
            segments.append({
                "start_ms": 0,
                "end_ms": 0,
                "text": cleaned,
                "confidence": 1.0,
                "language_code": "zh",
                "words": [],
            })
        return segments

    # If raw_output is a list
    if isinstance(raw_output, (list, tuple)):
        for item in raw_output:
            if isinstance(item, dict):
                text = clean_sensevoice_text(str(item.get("text", "")))
                if not text:
                    continue
                start_ms = int(item.get("start_ms", item.get("start", 0) * 1000 if isinstance(item.get("start"), (int, float)) else 0))
                end_ms = int(item.get("end_ms", item.get("end", 0) * 1000 if isinstance(item.get("end"), (int, float)) else 0))
                confidence = float(item.get("confidence", item.get("score", 1.0)))
                words = item.get("words", [])
                segments.append({
                    "start_ms": start_ms,
                    "end_ms": end_ms,
                    "text": text,
                    "confidence": confidence,
                    "language_code": item.get("language_code", "zh"),
                    "words": words,
                })
            elif isinstance(item, str):
                cleaned = clean_sensevoice_text(item)
                if cleaned:
                    segments.append({
                        "start_ms": 0,
                        "end_ms": 0,
                        "text": cleaned,
                        "confidence": 1.0,
                        "language_code": "zh",
                        "words": [],
                    })
        return segments

    # If raw_output is a dict
    if isinstance(raw_output, dict):
        if "segments" in raw_output and isinstance(raw_output["segments"], list):
            return parse_sensevoice_output(raw_output["segments"])

        text = clean_sensevoice_text(str(raw_output.get("text", "")))
        if not text:
            return []

        start_ms = int(raw_output.get("start_ms", 0))
        end_ms = int(raw_output.get("end_ms", 0))
        confidence = float(raw_output.get("confidence", 1.0))
        words_in = raw_output.get("words", [])
        words = []
        if isinstance(words_in, list):
            for w in words_in:
                if isinstance(w, dict):
                    w_text = clean_sensevoice_text(str(w.get("word", w.get("text", ""))))
                    if w_text:
                        words.append({
                            "word": w_text,
                            "start_ms": int(w.get("start_ms", 0)),
                            "end_ms": int(w.get("end_ms", 0)),
                        })

        segments.append({
            "start_ms": start_ms,
            "end_ms": end_ms,
            "text": text,
            "confidence": confidence,
            "language_code": raw_output.get("language_code", "zh"),
            "words": words,
        })
        return segments

    # If raw_output is an object (e.g. Sherpa-ONNX result object)
    if hasattr(raw_output, "text"):
        raw_text = getattr(raw_output, "text", "")
        cleaned = clean_sensevoice_text(raw_text)
        words: List[Dict[str, Any]] = []
        if hasattr(raw_output, "tokens") and hasattr(raw_output, "timestamps"):
            tokens = getattr(raw_output, "tokens", [])
            timestamps = getattr(raw_output, "timestamps", [])
            # Convert token timestamps (seconds) to ms
            for idx, tok in enumerate(tokens):
                tok_clean = clean_sensevoice_text(tok)
                if not tok_clean:
                    continue
                t_sec = timestamps[idx] if idx < len(timestamps) else 0.0
                t_ms = int(t_sec * 1000)
                words.append({
                    "word": tok_clean,
                    "start_ms": t_ms,
                    "end_ms": t_ms + 200,
                })
        start_ms = words[0]["start_ms"] if words else 0
        end_ms = words[-1]["end_ms"] if words else 0
        segments.append({
            "start_ms": start_ms,
            "end_ms": end_ms,
            "text": cleaned,
            "confidence": 1.0,
            "language_code": "zh",
            "words": words,
        })
        return segments

    return segments


def run_sensevoice_asr(
    audio_path: str,
    model_name: str,
    model_version: str,
    model_path: str = "",
    require_model_snapshot: bool = False,
) -> Dict[str, Any]:
    """Execute SenseVoice ONNX speech recognition."""
    if _SENSEVOICE_MODEL_FACTORY is not None:
        raw_res = _SENSEVOICE_MODEL_FACTORY(
            audio_path=audio_path,
            model_name=model_name,
            model_version=model_version,
            model_path=model_path,
            require_model_snapshot=require_model_snapshot,
        )
        segments = parse_sensevoice_output(raw_res)
        if not segments:
            raise RuntimeError("SenseVoice ASR inference produced no transcript segments")
        return {
            "segments": segments,
            "model_name": model_name or DEFAULT_MODEL_NAME,
            "model_version": model_version or DEFAULT_MODEL_VERSION,
        }

    snapshot_path = (model_path or "").strip()
    if require_model_snapshot:
        if not snapshot_path or not (os.path.isdir(snapshot_path) or os.path.isfile(snapshot_path)):
            raise RuntimeError(
                f"WORKER_SNAPSHOT_PATH_REQUIRED: verified local SenseVoice snapshot path missing or inaccessible: {model_path!r}"
            )

    # Attempt Sherpa-ONNX first
    try:
        import sherpa_onnx  # type: ignore

        # Resolve model files in snapshot_path or default dir
        model_dir = snapshot_path if os.path.isdir(snapshot_path) else os.path.dirname(snapshot_path)
        model_file = os.path.join(model_dir, "model.int8.onnx")
        if not os.path.isfile(model_file):
            model_file = os.path.join(model_dir, "model.onnx")
        tokens_file = os.path.join(model_dir, "tokens.txt")

        if not os.path.isfile(model_file) or not os.path.isfile(tokens_file):
            raise FileNotFoundError(
                f"SenseVoice ONNX model files missing in {model_dir} (expected model.int8.onnx/model.onnx and tokens.txt)"
            )

        recognizer = sherpa_onnx.OfflineRecognizer.from_sense_voice(
            model=model_file,
            tokens=tokens_file,
            num_threads=2,
            use_itn=True,
        )
        stream = recognizer.create_stream()
        import wave
        with wave.open(audio_path, "rb") as wf:
            num_samples = wf.getnframes()
            samples = wf.readframes(num_samples)
            import numpy as np
            audio_data = np.frombuffer(samples, dtype=np.int16).astype(np.float32) / 32768.0
            stream.accept_waveform(wf.getframerate(), audio_data)
        recognizer.decode_stream(stream)
        raw_res = stream.result
        segments = parse_sensevoice_output(raw_res)
    except ImportError:
        # Fallback to FunASR ONNX if available
        try:
            from funasr_onnx import SenseVoiceSmall  # type: ignore
            model_dir = snapshot_path if os.path.isdir(snapshot_path) else "models/sensevoice-small"
            model = SenseVoiceSmall(model_dir=model_dir, batch_size=1)
            raw_res = model(audio_path)
            segments = parse_sensevoice_output(raw_res)
        except ImportError as exc:
            raise RuntimeError(
                "Neither sherpa-onnx nor funasr-onnx is installed. Install sherpa-onnx (pip install sherpa-onnx) for SenseVoice inference."
            ) from exc

    if not segments:
        raise RuntimeError("SenseVoice ASR inference produced no transcript segments")

    return {
        "segments": segments,
        "model_name": model_name or DEFAULT_MODEL_NAME,
        "model_version": model_version or DEFAULT_MODEL_VERSION,
    }


def main() -> None:
    if hasattr(sys.stdin, "reconfigure"):
        sys.stdin.reconfigure(encoding="utf-8")
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")

    try:
        raw_input = sys.stdin.read()
        if not raw_input.strip():
            sys.stderr.write("Error: empty request on stdin\n")
            sys.exit(1)
        req = json.loads(raw_input)
    except Exception as exc:
        sys.stderr.write(f"Error: failed to parse stdin JSON: {exc}\n")
        sys.exit(1)

    audio_path = req.get("audio_path", "")
    if not audio_path or not os.path.isfile(audio_path):
        sys.stderr.write(f"Error: audio_path missing or not a file: {audio_path}\n")
        sys.exit(1)

    model_name = req.get("model_name", DEFAULT_MODEL_NAME)
    model_version = req.get("model_version", DEFAULT_MODEL_VERSION)
    model_path = req.get("model_path", "")
    require_model_snapshot = req.get("require_model_snapshot", False)

    try:
        resp = run_sensevoice_asr(
            audio_path=audio_path,
            model_name=model_name,
            model_version=model_version,
            model_path=model_path,
            require_model_snapshot=require_model_snapshot,
        )
        sys.stdout.write(json.dumps(resp, ensure_ascii=False) + "\n")
        sys.stdout.flush()
    except Exception as exc:
        sys.stderr.write(f"Error: {exc}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
