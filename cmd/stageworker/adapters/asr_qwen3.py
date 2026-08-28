#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Qwen3-ASR
Invokes official upstream QwenLM/Qwen3-ASR Python API (qwen_asr package)
for Qwen/Qwen3-ASR-1.7B and Qwen/Qwen3-ASR-0.6B.

Contract:
- Stdin: JSON request with audio_path, model_name, model_version, run_id, attempt_id
- Stdout: JSON response with segments, model_name, model_version
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import json
import os
import sys
from typing import Any, Dict, List, Optional

DEFAULT_1_7B_MODEL = "Qwen/Qwen3-ASR-1.7B"
DEFAULT_0_6B_MODEL = "Qwen/Qwen3-ASR-0.6B"


def get_qwen3_asr_model_class():
    """Import first-party Qwen3ASRModel from qwen_asr package."""
    try:
        from qwen_asr import Qwen3ASRModel
        return Qwen3ASRModel
    except ImportError:
        try:
            from qwen_asr.inference.qwen3_asr import Qwen3ASRModel
            return Qwen3ASRModel
        except ImportError as exc:
            raise RuntimeError(
                "qwen_asr package not found: install Qwen3-ASR (pip install qwen-asr) to enable ASR inference"
            ) from exc


def resolve_model_identifier(model_name: str, model_version: str) -> str:
    """Resolve model identifier to upstream checkpoint name or local path."""
    name = (model_name or "").strip()
    ver = (model_version or "").strip().lower()

    if name in ("qwen3-asr", "qwen3_asr", ""):
        if ver in ("0.6b", "0_6b", "small"):
            return DEFAULT_0_6B_MODEL
        return DEFAULT_1_7B_MODEL
    return name


def parse_asr_segments(raw_result: Any) -> List[Dict[str, Any]]:
    """
    Parse raw output from Qwen3ASRModel into canonical domain ASRRawSegment dicts.
    Handles list of results, single result object, dict format, or timestamp structures.
    """
    if raw_result is None:
        return []

    # Unwrap list of results if wrapped
    if isinstance(raw_result, (list, tuple)):
        if len(raw_result) == 0:
            return []
        # If list of results per audio item, take first item if it contains structured results
        if len(raw_result) == 1 and (hasattr(raw_result[0], "time_stamps") or hasattr(raw_result[0], "segments") or isinstance(raw_result[0], dict)):
            raw_result = raw_result[0]

    segments: List[Dict[str, Any]] = []

    # Case 1: result has explicit 'segments' attribute or key
    raw_segs = None
    if isinstance(raw_result, dict):
        raw_segs = raw_result.get("segments")
    elif hasattr(raw_result, "segments"):
        raw_segs = getattr(raw_result, "segments")

    if raw_segs and isinstance(raw_segs, (list, tuple)):
        for s in raw_segs:
            text = ""
            start_ms = 0
            end_ms = 0
            confidence = 0.9
            lang = "zh"

            if isinstance(s, dict):
                text = str(s.get("text", s.get("word", ""))).strip()
                st = s.get("start_ms", s.get("start_time", s.get("start", 0)))
                ed = s.get("end_ms", s.get("end_time", s.get("end", 0)))
                confidence = float(s.get("confidence", 0.9))
                lang = str(s.get("language_code", s.get("language", "zh")))
            elif hasattr(s, "text") or hasattr(s, "word"):
                text = str(getattr(s, "text", getattr(s, "word", ""))).strip()
                st = getattr(s, "start_ms", getattr(s, "start_time", getattr(s, "start", 0)))
                ed = getattr(s, "end_ms", getattr(s, "end_time", getattr(s, "end", 0)))
                confidence = float(getattr(s, "confidence", 0.9))
                lang = str(getattr(s, "language_code", getattr(s, "language", "zh")))
            else:
                continue

            if float(st) < 100.0 and float(ed) < 100.0 and (float(st) > 0 or float(ed) > 0):
                # Times provided in seconds -> convert to ms
                start_ms = int(round(float(st) * 1000))
                end_ms = int(round(float(ed) * 1000))
            else:
                start_ms = int(round(float(st)))
                end_ms = int(round(float(ed)))

            if text:
                segments.append({
                    "start_ms": start_ms,
                    "end_ms": end_ms,
                    "text": text,
                    "confidence": confidence,
                    "language_code": lang,
                })
        if segments:
            return segments

    # Case 2: result has 'time_stamps' attribute or key
    raw_ts = None
    if isinstance(raw_result, dict):
        raw_ts = raw_result.get("time_stamps")
    elif hasattr(raw_result, "time_stamps"):
        raw_ts = getattr(raw_result, "time_stamps")

    if raw_ts and isinstance(raw_ts, (list, tuple)):
        for t in raw_ts:
            text = ""
            start_ms = 0
            end_ms = 0
            confidence = 0.9
            lang = "zh"

            if isinstance(t, dict):
                text = str(t.get("text", t.get("word", ""))).strip()
                st = t.get("start_time", t.get("start_ms", t.get("start", 0)))
                ed = t.get("end_time", t.get("end_ms", t.get("end", 0)))
            elif hasattr(t, "text") or hasattr(t, "word"):
                text = str(getattr(t, "text", getattr(t, "word", ""))).strip()
                st = getattr(t, "start_time", getattr(t, "start_ms", getattr(t, "start", 0)))
                ed = getattr(t, "end_time", getattr(t, "end_ms", getattr(t, "end", 0)))
            else:
                continue

            if float(st) < 100.0 and float(ed) < 100.0 and (float(st) > 0 or float(ed) > 0):
                start_ms = int(round(float(st) * 1000))
                end_ms = int(round(float(ed) * 1000))
            else:
                start_ms = int(round(float(st)))
                end_ms = int(round(float(ed)))

            if text:
                segments.append({
                    "start_ms": start_ms,
                    "end_ms": end_ms,
                    "text": text,
                    "confidence": confidence,
                    "language_code": lang,
                })
        if segments:
            return segments

    # Case 3: single text string
    full_text = ""
    if isinstance(raw_result, str):
        full_text = raw_result.strip()
    elif isinstance(raw_result, dict):
        full_text = str(raw_result.get("text", "")).strip()
    elif hasattr(raw_result, "text"):
        full_text = str(getattr(raw_result, "text", "")).strip()

    if full_text:
        segments.append({
            "start_ms": 0,
            "end_ms": max(1000, len(full_text) * 200),
            "text": full_text,
            "confidence": 0.9,
            "language_code": "zh",
        })

    return segments


def qwen_runtime_load_kwargs() -> Dict[str, Any]:
    """Return runtime-only Qwen load options for the available execution device."""
    try:
        import torch
    except ImportError:
        return {}

    if not torch.cuda.is_available():
        return {}

    return {
        "dtype": torch.float16,
        "device_map": "cuda:0",
        "low_cpu_mem_usage": True,
        "max_inference_batch_size": 1,
    }


def run_asr(
    audio_path: str,
    model_name: str,
    model_version: str,
) -> Dict[str, Any]:
    """Execute Qwen3-ASR model inference over input audio."""
    ModelClass = get_qwen3_asr_model_class()
    model_id = resolve_model_identifier(model_name, model_version)

    try:
        model = ModelClass.from_pretrained(model_id, **qwen_runtime_load_kwargs())
    except Exception as exc:
        raise RuntimeError(
            f"failed to initialize Qwen3ASRModel with checkpoint '{model_id}': {exc}"
        ) from exc

    raw_results = None
    if hasattr(model, "transcribe") and callable(model.transcribe):
        try:
            # ASR and forced alignment are separate canonical stages in Douyinie.
            # Upstream Qwen3ASRModel requires a forced_aligner at model init when
            # return_time_stamps=True; this adapter intentionally requests text
            # only and leaves source-timeline alignment to aligner_qwen3.py.
            raw_results = model.transcribe(
                audio=audio_path,
                language="Chinese",
                return_time_stamps=False,
            )
        except TypeError:
            try:
                raw_results = model.transcribe(audio=audio_path)
            except TypeError:
                raw_results = model.transcribe(audio_path)
    elif hasattr(model, "__call__") and callable(model):
        raw_results = model(audio_path)

    segments = parse_asr_segments(raw_results)
    if not segments:
        raise RuntimeError("Qwen3-ASR inference produced no transcript segments")

    return {
        "segments": segments,
        "model_name": model_name or "qwen3-asr",
        "model_version": model_version or "1.7b",
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

    model_name = req.get("model_name", "")
    model_version = req.get("model_version", "")

    try:
        resp = run_asr(audio_path, model_name, model_version)
        sys.stdout.write(json.dumps(resp, ensure_ascii=False) + "\n")
        sys.stdout.flush()
    except Exception as exc:
        sys.stderr.write(f"Error: {exc}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
