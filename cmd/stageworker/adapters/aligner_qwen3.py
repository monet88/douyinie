#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Qwen3-ForcedAligner
Invokes official upstream QwenLM/Qwen3-ASR Python API (qwen_asr package)
for Qwen/Qwen3-ForcedAligner-0.6B.

Contract:
- Stdin: JSON request with audio_path, text, model_name, model_version, run_id, attempt_id
- Stdout: JSON response with word_timings, model_name, model_version
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import json
import os
import sys
from typing import Any, Dict, List, Optional

DEFAULT_ALIGNER_MODEL = "Qwen/Qwen3-ForcedAligner-0.6B"


def get_qwen3_forced_aligner_class():
    """Import first-party Qwen3ForcedAligner from qwen_asr package."""
    try:
        from qwen_asr import Qwen3ForcedAligner
        return Qwen3ForcedAligner
    except ImportError:
        try:
            from qwen_asr.inference.qwen3_forced_aligner import Qwen3ForcedAligner
            return Qwen3ForcedAligner
        except ImportError as exc:
            raise RuntimeError(
                "qwen_asr package not found: install Qwen3-ForcedAligner (pip install qwen-asr) to enable forced alignment"
            ) from exc


def resolve_aligner_identifier(model_name: str, model_version: str) -> str:
    """Resolve model identifier to upstream aligner checkpoint name or local path."""
    name = (model_name or "").strip()
    if name in ("qwen3-aligner", "qwen3_aligner", "Qwen3-ForcedAligner", "Qwen3-ForcedAligner-0.6B", ""):
        return DEFAULT_ALIGNER_MODEL
    return name


def parse_word_timings(raw_result: Any) -> List[Dict[str, Any]]:
    """
    Parse raw output from Qwen3ForcedAligner into canonical domain WordTiming dicts.
    Handles List[ForcedAlignResult], items with text/word/start_time/end_time in seconds or ms.
    """
    if raw_result is None:
        return []

    # Unwrap single-sample outer list if batch size was 1 (e.g. [[item1, item2]])
    if isinstance(raw_result, (list, tuple)) and len(raw_result) == 1:
        if isinstance(raw_result[0], (list, tuple)) and (
            len(raw_result[0]) == 0
            or isinstance(raw_result[0][0], (dict, list, tuple))
            or hasattr(raw_result[0][0], "text")
            or hasattr(raw_result[0][0], "word")
        ):
            raw_result = raw_result[0]
        elif hasattr(raw_result[0], "items") and isinstance(getattr(raw_result[0], "items"), (list, tuple)):
            raw_result = getattr(raw_result[0], "items")
    timings: List[Dict[str, Any]] = []

    # Check for direct list of timing items
    raw_items = raw_result
    if isinstance(raw_result, dict):
        raw_items = raw_result.get("word_timings", raw_result.get("items", raw_result.get("words", [])))
    elif hasattr(raw_result, "word_timings"):
        raw_items = getattr(raw_result, "word_timings")
    elif hasattr(raw_result, "items") and not callable(getattr(raw_result, "items")):
        raw_items = getattr(raw_result, "items")

    if not isinstance(raw_items, (list, tuple)):
        raw_items = [raw_items]

    for item in raw_items:
        word = ""
        start_ms = 0
        end_ms = 0
        confidence = 0.9

        if isinstance(item, dict):
            word = str(item.get("word", item.get("text", ""))).strip()
            st = item.get("start_ms", item.get("start_time", item.get("start", 0)))
            ed = item.get("end_ms", item.get("end_time", item.get("end", 0)))
            confidence = float(item.get("confidence", 0.9))
        elif hasattr(item, "word") or hasattr(item, "text"):
            word = str(getattr(item, "word", getattr(item, "text", ""))).strip()
            st = getattr(item, "start_ms", getattr(item, "start_time", getattr(item, "start", 0)))
            ed = getattr(item, "end_ms", getattr(item, "end_time", getattr(item, "end", 0)))
            confidence = float(getattr(item, "confidence", 0.9))
        elif isinstance(item, (list, tuple)) and len(item) >= 3:
            # Format: [st, ed, word] or [word, st, ed]
            if isinstance(item[0], (int, float)) and isinstance(item[1], (int, float)):
                st, ed, word = item[0], item[1], str(item[2]).strip()
            else:
                word, st, ed = str(item[0]).strip(), item[1], item[2]
        else:
            continue

        if float(st) < 100.0 and float(ed) < 100.0 and (float(st) > 0 or float(ed) > 0):
            # Provided in seconds -> convert to ms
            start_ms = int(round(float(st) * 1000))
            end_ms = int(round(float(ed) * 1000))
        else:
            start_ms = int(round(float(st)))
            end_ms = int(round(float(ed)))

        if word:
            timings.append({
                "word": word,
                "start_ms": start_ms,
                "end_ms": end_ms,
                "confidence": confidence,
            })

    return timings


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
    }


def run_aligner(
    audio_path: str,
    text: str,
    model_name: str,
    model_version: str,
    model_path: str = "",
    require_model_snapshot: bool = False,
) -> Dict[str, Any]:
    """Execute Qwen3ForcedAligner model inference over input audio and accepted text."""
    # Verified local snapshot is the source of truth. Under strict snapshot
    # requirement a missing/inaccessible model_path fails closed with
    # WORKER_SNAPSHOT_PATH_REQUIRED — Hub/model-ID fallback is prohibited.
    snapshot_path = (model_path or "").strip()
    if require_model_snapshot:
        if not snapshot_path or not os.path.exists(snapshot_path):
            raise RuntimeError(
                f"WORKER_SNAPSHOT_PATH_REQUIRED: verified local snapshot model_path missing or inaccessible: {model_path!r}; Hub fallback is strictly prohibited"
            )
        model_id = snapshot_path
        init_fns = [
            lambda: AlignerClass.from_pretrained(model_id, **qwen_runtime_load_kwargs()),
            lambda: AlignerClass(model_id),
        ]
    elif snapshot_path and os.path.exists(snapshot_path):
        model_id = snapshot_path
        init_fns = [
            lambda: AlignerClass.from_pretrained(model_id, **qwen_runtime_load_kwargs()),
            lambda: AlignerClass(model_id),
            lambda: AlignerClass(),
        ]
    else:
        model_id = resolve_aligner_identifier(model_name, model_version)
        init_fns = [
            lambda: AlignerClass.from_pretrained(model_id, **qwen_runtime_load_kwargs()),
            lambda: AlignerClass(model_id),
            lambda: AlignerClass(),
        ]

    AlignerClass = get_qwen3_forced_aligner_class()

    aligner = None
    for init_fn in init_fns:
        try:
            aligner = init_fn()
            break
        except Exception:
            continue

    if aligner is None:
        if require_model_snapshot:
            raise RuntimeError(f"WORKER_SNAPSHOT_PATH_REQUIRED: failed to initialize Qwen3ForcedAligner with verified snapshot ({model_id})")
        raise RuntimeError(f"failed to initialize Qwen3ForcedAligner with checkpoint '{model_id}'")

    raw_results = None
    if hasattr(aligner, "align") and callable(aligner.align):
        try:
            raw_results = aligner.align(audio=audio_path, text=text, language="Chinese")
        except TypeError:
            try:
                raw_results = aligner.align(audio=audio_path, text=text)
            except TypeError:
                raw_results = aligner.align(audio_path, text)
    elif hasattr(aligner, "__call__") and callable(aligner):
        raw_results = aligner(audio_path, text)

    timings = parse_word_timings(raw_results)
    if not timings:
        raise RuntimeError("Qwen3-ForcedAligner inference produced no word timings")

    return {
        "word_timings": timings,
        "model_name": model_name or "Qwen3-ForcedAligner-0.6B",
        "model_version": model_version or "0.6b",
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

    text = req.get("text", "")
    if not text or not text.strip():
        sys.stderr.write("Error: text missing or empty for forced alignment\n")
        sys.exit(1)

    model_name = req.get("model_name", "")
    model_version = req.get("model_version", "")
    model_path = req.get("model_path", "")
    require_model_snapshot = req.get("require_model_snapshot", False)

    try:
        resp = run_aligner(audio_path, text, model_name, model_version, model_path, require_model_snapshot)
        sys.stdout.write(json.dumps(resp, ensure_ascii=False) + "\n")
        sys.stdout.flush()
    except Exception as exc:
        sys.stderr.write(f"Error: {exc}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
