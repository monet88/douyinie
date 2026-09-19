#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Sherpa-ONNX Speaker Diarization & Evidence Probe
Lightweight CAM++ / 3D-Speaker ONNX speaker embedding + Silero VAD without PyTorch or ModelScope.

Contract:
- Stdin: JSON request with mode ("diarize" | "evidence"), audio_path,
  model_name, model_version, vad_model_name, vad_model_version,
  embedding_cosine_threshold (for evidence mode), run_id, attempt_id
- Stdout: JSON response with speaker_assignments or speaker_evidence
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import json
import math
import os
import sys
from typing import Any, Dict, List, Optional, Tuple

DEFAULT_MODEL_NAME = "sherpa-onnx-campplus"
DEFAULT_MODEL_VERSION = "onnx"
DEFAULT_VAD_MODEL = "silero-vad"
DEFAULT_VAD_VERSION = "v5"

# Pluggable hook for deterministic unit testing
_DIARIZER_MODEL_FACTORY = None


def compute_cosine_similarity(vec1: List[float], vec2: List[float]) -> float:
    """Compute cosine similarity between two 1-D vectors."""
    if not vec1 or not vec2 or len(vec1) != len(vec2):
        return 0.0
    dot = sum(a * b for a, b in zip(vec1, vec2))
    norm1 = math.sqrt(sum(a * a for a in vec1))
    norm2 = math.sqrt(sum(b * b for b in vec2))
    if norm1 <= 0.0 or norm2 <= 0.0:
        return 0.0
    return dot / (norm1 * norm2)


def run_diarization_sherpa(
    audio_path: str,
    model_name: str,
    model_version: str,
    vad_model_name: str,
    vad_model_version: str,
    model_path: Optional[str] = None,
    vad_model_path: Optional[str] = None,
    require_model_snapshot: bool = False,
) -> List[Dict[str, Any]]:
    """Run speaker diarization returning canonical speaker assignments."""
    if _DIARIZER_MODEL_FACTORY is not None:
        res = _DIARIZER_MODEL_FACTORY(
            mode="diarize",
            audio_path=audio_path,
            model_name=model_name,
            model_version=model_version,
            vad_model_name=vad_model_name,
            vad_model_version=vad_model_version,
            model_path=model_path,
            vad_model_path=vad_model_path,
            require_model_snapshot=require_model_snapshot,
        )
        if isinstance(res, list):
            return res
        if isinstance(res, dict) and "speaker_assignments" in res:
            return res["speaker_assignments"]

    if require_model_snapshot:
        if not model_path or not os.path.exists(model_path):
            raise RuntimeError(
                f"WORKER_SNAPSHOT_PATH_REQUIRED: verified local Diarizer snapshot missing or inaccessible: {model_path!r}"
            )
        if not vad_model_path or not os.path.exists(vad_model_path):
            raise RuntimeError(
                f"WORKER_SNAPSHOT_PATH_REQUIRED: verified local VAD snapshot missing or inaccessible: {vad_model_path!r}"
            )

    try:
        import sherpa_onnx  # type: ignore
        import wave
        import numpy as np

        model_file = model_path or "models/diarizer/campplus.onnx"
        vad_file = vad_model_path or "models/vad/silero_vad.onnx"

        if not os.path.isfile(model_file) or not os.path.isfile(vad_file):
            raise FileNotFoundError(f"Diarization models missing: {model_file} or {vad_file}")

        with wave.open(audio_path, "rb") as wf:
            sample_rate = wf.getframerate()
            num_samples = wf.getnframes()
            samples = np.frombuffer(wf.readframes(num_samples), dtype=np.int16).astype(np.float32) / 32768.0

        vad_config = sherpa_onnx.VadModelConfig(
            silero_vad=sherpa_onnx.SileroVadModelConfig(
                model=vad_file,
                threshold=0.5,
                min_speech_duration=0.25,
                min_silence_duration=0.5,
                window_size=512,
            ),
            sample_rate=sample_rate,
            num_threads=2,
        )
        vad = sherpa_onnx.VoiceActivityDetector(vad_config, buffer_size_in_seconds=max(60, int(len(samples) / sample_rate) + 10))
        window_size = 512
        for i in range(0, len(samples), window_size):
            chunk = samples[i : i + window_size]
            if len(chunk) < window_size:
                chunk = np.pad(chunk, (0, window_size - len(chunk)))
            vad.accept_waveform(chunk)
        vad.flush()

        vad_segments = []
        while not vad.empty():
            seg = vad.front
            start_s = seg.start / sample_rate
            end_s = (seg.start + len(seg.samples)) / sample_rate
            vad_segments.append((start_s, end_s, seg.samples))
            vad.pop()

        if not vad_segments:
            return [{
                "speaker_id": "SPEAKER_00",
                "label": "SPEAKER_00",
                "start_ms": 0,
                "end_ms": int((len(samples) / sample_rate) * 1000),
                "confidence": 0.0,
            }]

        extractor_config = sherpa_onnx.SpeakerEmbeddingExtractorConfig(model=model_file, num_threads=2)
        extractor = sherpa_onnx.SpeakerEmbeddingExtractor(extractor_config)

        clusters: List[Dict[str, Any]] = []
        assignments = []

        for start_s, end_s, chunk_samples in vad_segments:
            stream = extractor.create_stream()
            stream.accept_waveform(sample_rate, chunk_samples)
            emb = extractor.compute(stream)

            assigned_id = None
            best_sim = -1.0
            for c in clusters:
                sim = compute_cosine_similarity(emb, c["embedding"])
                if sim > best_sim:
                    best_sim = sim
                    if sim >= 0.65:
                        assigned_id = c["id"]

            if assigned_id is None:
                assigned_id = f"SPEAKER_{len(clusters):02d}"
                clusters.append({"id": assigned_id, "embedding": emb})

            assignments.append({
                "speaker_id": assigned_id,
                "label": assigned_id,
                "start_ms": int(start_s * 1000),
                "end_ms": int(end_s * 1000),
                "confidence": float(round(best_sim if best_sim >= 0 else 1.0, 3)),
            })
        return assignments

    except ImportError as exc:
        raise RuntimeError(
            "sherpa-onnx runtime not found: install sherpa-onnx (pip install sherpa-onnx)"
        ) from exc


def run_evidence_probe_sherpa(
    audio_path: str,
    model_name: str,
    model_version: str,
    vad_model_name: str,
    vad_model_version: str,
    embedding_cosine_threshold: float,
    model_path: Optional[str] = None,
    vad_model_path: Optional[str] = None,
    require_model_snapshot: bool = False,
) -> Dict[str, Any]:
    """Probe speaker evidence using ONNX embedding extractor and cosine similarity."""
    source_tag = f"{model_name}@{model_version}+{vad_model_name}@{vad_model_version}"

    if _DIARIZER_MODEL_FACTORY is not None:
        res = _DIARIZER_MODEL_FACTORY(
            mode="evidence",
            audio_path=audio_path,
            model_name=model_name,
            model_version=model_version,
            vad_model_name=vad_model_name,
            vad_model_version=vad_model_version,
            embedding_cosine_threshold=embedding_cosine_threshold,
            model_path=model_path,
            vad_model_path=vad_model_path,
            require_model_snapshot=require_model_snapshot,
        )
        if isinstance(res, dict):
            if "speaker_evidence" in res:
                return res["speaker_evidence"]
            return res

    if require_model_snapshot:
        if not model_path or not os.path.exists(model_path):
            raise RuntimeError(
                f"WORKER_SNAPSHOT_PATH_REQUIRED: verified local Diarizer snapshot missing or inaccessible: {model_path!r}"
            )
        if not vad_model_path or not os.path.exists(vad_model_path):
            raise RuntimeError(
                f"WORKER_SNAPSHOT_PATH_REQUIRED: verified local VAD snapshot missing or inaccessible: {vad_model_path!r}"
            )

    try:
        import sherpa_onnx  # type: ignore
        import wave
        import numpy as np

        model_file = model_path or "models/campplus.onnx"
        if not os.path.isfile(model_file):
            raise FileNotFoundError(f"Speaker embedding model missing: {model_file}")

        extractor_config = sherpa_onnx.SpeakerEmbeddingExtractorConfig(model=model_file, num_threads=2)
        extractor = sherpa_onnx.SpeakerEmbeddingExtractor(extractor_config)

        # Extract embeddings over short windows (e.g. 1.5s windows with 0.75s step)
        with wave.open(audio_path, "rb") as wf:
            sample_rate = wf.getframerate()
            num_samples = wf.getnframes()
            raw_bytes = wf.readframes(num_samples)
            samples = np.frombuffer(raw_bytes, dtype=np.int16).astype(np.float32) / 32768.0

        window_size = int(1.5 * sample_rate)
        step_size = int(0.75 * sample_rate)
        embeddings = []

        for start in range(0, max(1, len(samples) - window_size), step_size):
            chunk = samples[start : start + window_size]
            stream = extractor.create_stream()
            stream.accept_waveform(sample_rate, chunk)
            if extractor.is_ready(stream):
                emb = extractor.compute(stream)
                embeddings.append(emb)

        change_count = 0
        if len(embeddings) > 1:
            for i in range(1, len(embeddings)):
                sim = compute_cosine_similarity(embeddings[i - 1], embeddings[i])
                if sim < embedding_cosine_threshold:
                    change_count += 1

        return {
            "has_multi_speaker_cues": change_count > 0,
            "speaker_change_count": change_count,
            "confidence": 0.0,
            "source": source_tag,
        }

    except ImportError as exc:
        raise RuntimeError(
            "sherpa-onnx runtime not found: install sherpa-onnx (pip install sherpa-onnx)"
        ) from exc


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

    mode = req.get("mode", "diarize")
    model_name = req.get("model_name") or DEFAULT_MODEL_NAME
    model_version = req.get("model_version") or DEFAULT_MODEL_VERSION
    vad_model_name = req.get("vad_model_name") or DEFAULT_VAD_MODEL
    vad_model_version = req.get("vad_model_version") or DEFAULT_VAD_VERSION

    embedding_cosine_threshold = None
    if mode == "evidence":
        if "embedding_cosine_threshold" not in req:
            sys.stderr.write("Error: embedding_cosine_threshold is required for evidence mode\n")
            sys.exit(1)
        try:
            embedding_cosine_threshold = float(req["embedding_cosine_threshold"])
        except (TypeError, ValueError):
            sys.stderr.write("Error: embedding_cosine_threshold must be numeric\n")
            sys.exit(1)
        if embedding_cosine_threshold <= 0 or embedding_cosine_threshold > 1:
            sys.stderr.write("Error: embedding_cosine_threshold must be in (0, 1]\n")
            sys.exit(1)

    model_path = req.get("model_path")
    vad_model_path = req.get("vad_model_path")
    require_model_snapshot = bool(req.get("require_model_snapshot", False))

    try:
        if mode == "evidence":
            evidence = run_evidence_probe_sherpa(
                audio_path=audio_path,
                model_name=model_name,
                model_version=model_version,
                vad_model_name=vad_model_name,
                vad_model_version=vad_model_version,
                embedding_cosine_threshold=embedding_cosine_threshold,
                model_path=model_path,
                vad_model_path=vad_model_path,
                require_model_snapshot=require_model_snapshot,
            )
            resp = {
                "speaker_evidence": evidence,
                "model_name": model_name,
                "model_version": model_version,
                "vad_model_name": vad_model_name,
                "vad_model_version": vad_model_version,
            }
        else:
            assignments = run_diarization_sherpa(
                audio_path=audio_path,
                model_name=model_name,
                model_version=model_version,
                vad_model_name=vad_model_name,
                vad_model_version=vad_model_version,
                model_path=model_path,
                vad_model_path=vad_model_path,
                require_model_snapshot=require_model_snapshot,
            )
            if not assignments:
                sys.stderr.write("Error: Sherpa-ONNX diarization produced no speaker assignments\n")
                sys.exit(1)
            resp = {
                "speaker_assignments": assignments,
                "model_name": model_name,
                "model_version": model_version,
                "vad_model_name": vad_model_name,
                "vad_model_version": vad_model_version,
            }

        sys.stdout.write(json.dumps(resp, ensure_ascii=False) + "\n")
        sys.stdout.flush()
    except Exception as exc:
        sys.stderr.write(f"Error: {exc}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
