#!/usr/bin/env python3
"""
ModelScope 3D-Speaker Diarization3Dspeaker JSON Adapter

Implements the StageWorker stdin JSON request / stdout JSON response contract
for speaker diarization and pre-diarization speaker evidence probing using the
ModelScope 3D-Speaker Diarization3Dspeaker non-overlap runtime.

Upstream Model Identities:
- Speaker Embedding / Diarizer: iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0
- VAD: FSMN-VAD v2.0.4 (iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4)

Request (stdin JSON):
{
    "mode": "diarize" | "evidence",
    "audio_path": "/path/to/source.wav",
    "model_name": "iic/speech_campplus_sv_zh_en_16k-common_advanced",
    "model_version": "v1.0.0",
    "vad_model_name": "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch",
    "vad_model_version": "v2.0.4",
    "run_id": "run-id-123",
    "attempt_id": "attempt-id-123"
}

Response (stdout JSON):
- Diarize: {
    "speaker_assignments": [{"speaker_id": "SPEAKER_00", "label": "SPEAKER_00", "start_ms": 0, "end_ms": 1500, "confidence": 0.0}],
    "model_name": "...", "model_version": "...",
    "vad_model_name": "...", "vad_model_version": "..."
  }
- Evidence: {
    "speaker_evidence": {"has_multi_speaker_cues": true, "speaker_change_count": 2, "confidence": 0.0, "source": "iic/speech_campplus_sv_zh_en_16k-common_advanced@v1.0.0+iic/speech_fsmn_vad_zh-cn-16k-common-pytorch@v2.0.4"},
    "model_name": "...", "model_version": "...",
    "vad_model_name": "...", "vad_model_version": "..."
  }
"""

import json
import math
import os
import sys
from typing import Any, Dict, List, Optional

DEFAULT_MODEL_NAME = "iic/speech_campplus_sv_zh_en_16k-common_advanced"
DEFAULT_MODEL_VERSION = "v1.0.0"
DEFAULT_VAD_MODEL = "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch"
DEFAULT_VAD_VERSION = "v2.0.4"


def get_diarization_class_and_audio_loader():
    """Import first-party 3D-Speaker Diarization3Dspeaker and load_audio."""
    try:
        from speakerlab.bin.infer_diarization import Diarization3Dspeaker, load_audio
        return Diarization3Dspeaker, load_audio
    except ImportError:
        try:
            from infer_diarization import Diarization3Dspeaker, load_audio
            return Diarization3Dspeaker, load_audio
        except ImportError:
            pass
    try:
        from speakerlab.bin.infer_diarization import Diarization3Dspeaker
        return Diarization3Dspeaker, None
    except ImportError:
        try:
            from infer_diarization import Diarization3Dspeaker
            return Diarization3Dspeaker, None
        except ImportError as exc:
            raise RuntimeError(
                f"3D-Speaker (speakerlab) runtime not installed: cannot run 3D-Speaker diarization: {exc}"
            ) from exc


def get_diarization_class():
    cls, _ = get_diarization_class_and_audio_loader()
    return cls

def parse_speaker_segments(result: Any) -> List[Dict[str, Any]]:
    """
    Parse 3D-Speaker diarization output into a list of absolute-ms SpeakerAssignments.
    Upstream 3D-Speaker returns segments as list of [start, end, spk] or list of dicts.
    Per Finding 4: do not fabricate confidence; emit 0.0 for uncalibrated confidence.
    """
    assignments: List[Dict[str, Any]] = []

    if isinstance(result, list):
        for item in result:
            if isinstance(item, (list, tuple)) and len(item) >= 3:
                # [start_sec, end_sec, speaker_id]
                start_sec = float(item[0])
                end_sec = float(item[1])
                spk = str(item[2])
                start_ms = int(round(start_sec * 1000)) if start_sec < 10000 else int(round(start_sec))
                end_ms = int(round(end_sec * 1000)) if end_sec < 10000 else int(round(end_sec))
                if end_ms > start_ms:
                    assignments.append({
                        "speaker_id": spk,
                        "label": spk,
                        "start_ms": start_ms,
                        "end_ms": end_ms,
                        "confidence": 0.0,
                    })
            elif isinstance(item, dict):
                raw_start = float(item.get("start", item.get("start_ms", 0)))
                raw_end = float(item.get("end", item.get("end_ms", 0)))
                start_ms = int(round(raw_start * 1000)) if raw_start < 10000 and "start_ms" not in item else int(round(raw_start))
                end_ms = int(round(raw_end * 1000)) if raw_end < 10000 and "end_ms" not in item else int(round(raw_end))
                spk = str(item.get("speaker", item.get("speaker_id", item.get("spk", "SPEAKER_00"))))
                conf = float(item.get("confidence", 0.0))
                if end_ms > start_ms:
                    assignments.append({
                        "speaker_id": spk,
                        "label": spk,
                        "start_ms": start_ms,
                        "end_ms": end_ms,
                        "confidence": conf,
                    })
    elif isinstance(result, dict):
        if "speaker_assignments" in result and isinstance(result["speaker_assignments"], list):
            return parse_speaker_segments(result["speaker_assignments"])
        if "text" in result and isinstance(result["text"], list):
            return parse_speaker_segments(result["text"])
        if "segments" in result and isinstance(result["segments"], list):
            return parse_speaker_segments(result["segments"])

    assignments.sort(key=lambda a: (a["start_ms"], a["end_ms"], a["speaker_id"]))
    return assignments


def run_diarization(
    audio_path: str,
    model_name: str,
    model_version: str,
    vad_model_name: str,
    vad_model_version: str,
) -> List[Dict[str, Any]]:
    """Execute 3D-Speaker Diarization3Dspeaker non-overlap diarization."""
    DiarizationClass = get_diarization_class()

    diarizer = None
    init_attempts = [
        lambda: DiarizationClass(
            model_id=model_name or DEFAULT_MODEL_NAME,
            model_revision=model_version or DEFAULT_MODEL_VERSION,
            vad_model_id=vad_model_name or DEFAULT_VAD_MODEL,
            vad_model_revision=vad_model_version or DEFAULT_VAD_VERSION,
        ),
        lambda: DiarizationClass(
            model=model_name or DEFAULT_MODEL_NAME,
            vad_model=vad_model_name or DEFAULT_VAD_MODEL,
        ),
        lambda: DiarizationClass(
            embedding_model=model_name or DEFAULT_MODEL_NAME,
            vad_model=vad_model_name or DEFAULT_VAD_MODEL,
        ),
        lambda: DiarizationClass(
            conf={
                "model_id": model_name or DEFAULT_MODEL_NAME,
                "model_revision": model_version or DEFAULT_MODEL_VERSION,
                "vad_model_id": vad_model_name or DEFAULT_VAD_MODEL,
                "vad_model_revision": vad_model_version or DEFAULT_VAD_VERSION,
            }
        ),
        lambda: DiarizationClass(),
    ]

    last_err = None
    for attempt in init_attempts:
        try:
            diarizer = attempt()
            break
        except TypeError as err:
            last_err = err
            continue
        except Exception as err:
            last_err = err
            break

    if diarizer is None:
        raise RuntimeError(f"failed to initialize 3D-Speaker Diarization3Dspeaker: {last_err}")

    try:
        if callable(diarizer):
            result = diarizer(audio_path)
        elif hasattr(diarizer, "infer"):
            result = diarizer.infer(audio_path)
        elif hasattr(diarizer, "run"):
            result = diarizer.run(audio_path)
        else:
            raise RuntimeError("Diarization3Dspeaker instance is not callable and has no infer/run method")
    except Exception as exc:
        raise RuntimeError(f"3D-Speaker diarization inference failed: {exc}") from exc

    return parse_speaker_segments(result)


def compute_cosine_similarity(v1: Any, v2: Any) -> float:
    """Compute cosine similarity between two embedding vectors."""
    try:
        dot = sum(float(a) * float(b) for a, b in zip(v1, v2))
        norm1 = math.sqrt(sum(float(a) * float(a) for a in v1))
        norm2 = math.sqrt(sum(float(b) * float(b) for b in v2))
        if norm1 > 0 and norm2 > 0:
            return dot / (norm1 * norm2)
    except Exception:
        pass
    return 1.0


def run_evidence_probe(
    audio_path: str,
    model_name: str,
    model_version: str,
    vad_model_name: str,
    vad_model_version: str,
    embedding_cosine_threshold: float,
) -> Dict[str, Any]:
    """
    Execute pre-diarization speaker evidence probe.
    Does NOT emit final speaker assignments or run full spectral clustering pipeline.
    Probes bounded speech windows / embeddings for conservative speaker change cues.
    """
    DiarizationClass, load_audio_fn = get_diarization_class_and_audio_loader()
    source_tag = (
        f"{model_name or DEFAULT_MODEL_NAME}@{model_version or DEFAULT_MODEL_VERSION}"
        f"+{vad_model_name or DEFAULT_VAD_MODEL}@{vad_model_version or DEFAULT_VAD_VERSION}"
    )

    diarizer = None
    for attempt in [
        lambda: DiarizationClass(
            model_id=model_name or DEFAULT_MODEL_NAME,
            model_revision=model_version or DEFAULT_MODEL_VERSION,
            vad_model_id=vad_model_name or DEFAULT_VAD_MODEL,
            vad_model_revision=vad_model_version or DEFAULT_VAD_VERSION,
        ),
        lambda: DiarizationClass(),
    ]:
        try:
            diarizer = attempt()
            break
        except Exception:
            continue

    if diarizer is None:
        raise RuntimeError("failed to initialize speakerlab for evidence probe")

    # 1. First-party ModelScope 3D-Speaker Diarization3Dspeaker execution:
    # Upstream speakerlab.bin.infer_diarization exposes:
    # - load_audio(audio_path) as module-level function -> wav
    # - Diarization3Dspeaker exposes:
    #   - do_vad(wav) -> vad_segments (list of [st, ed])
    #   - chunk(st, ed) -> list of sub-chunks
    #   - do_emb_extraction(chunks, wav) -> embeddings
    #   - do_clustering(chunks, embeddings, speaker_num) -> cluster labels (skipped in bounded evidence probe!)
    # Bounded pre-clustering acoustic speaker evidence: extract embeddings for up to 16 speech
    # chunks, compute consecutive/cross-window cosine similarities.
    # Do NOT treat VAD turn count alone as multi-speaker proof.

    # Load audio via module-level load_audio or instance method fallback
    wav = None
    if load_audio_fn is not None and callable(load_audio_fn):
        try:
            wav = load_audio_fn(audio_path)
        except Exception as exc:
            raise RuntimeError(f"failed to load audio via speakerlab.load_audio: {exc}") from exc
    elif hasattr(diarizer, "load_audio") and callable(diarizer.load_audio):
        try:
            wav = diarizer.load_audio(audio_path)
        except Exception as exc:
            raise RuntimeError(f"failed to load audio via diarizer.load_audio: {exc}") from exc

    if wav is not None and hasattr(diarizer, "do_vad") and hasattr(diarizer, "chunk") and hasattr(diarizer, "do_emb_extraction"):
        try:
            vad_segments = diarizer.do_vad(wav)
            if vad_segments and len(vad_segments) > 0:
                all_chunks = []
                for seg in vad_segments:
                    sub_chunks = None
                    if isinstance(seg, (list, tuple)) and len(seg) >= 2:
                        st, ed = float(seg[0]), float(seg[1])
                        try:
                            sub_chunks = diarizer.chunk(st, ed)
                        except TypeError:
                            sub_chunks = diarizer.chunk(wav, seg)
                    elif isinstance(seg, dict) and "start" in seg and "end" in seg:
                        st, ed = float(seg["start"]), float(seg["end"])
                        try:
                            sub_chunks = diarizer.chunk(st, ed)
                        except TypeError:
                            sub_chunks = diarizer.chunk(wav, seg)
                    else:
                        try:
                            sub_chunks = diarizer.chunk(seg)
                        except Exception:
                            sub_chunks = None

                    if isinstance(sub_chunks, list):
                        all_chunks.extend(sub_chunks)
                    elif sub_chunks is not None:
                        all_chunks.append(sub_chunks)

                bounded_chunks = all_chunks[:16] if len(all_chunks) > 0 else []
                if bounded_chunks:
                    try:
                        embs = diarizer.do_emb_extraction(bounded_chunks, wav)
                    except TypeError:
                        embs = diarizer.do_emb_extraction(bounded_chunks)

                    if hasattr(embs, "tolist"):
                        embs = embs.tolist()

                    change_count = 0
                    if isinstance(embs, (list, tuple)) and len(embs) > 1:
                        for i in range(1, len(embs)):
                            sim = compute_cosine_similarity(embs[i - 1], embs[i])
                            if sim < embedding_cosine_threshold:
                                change_count += 1
                    return {
                        "has_multi_speaker_cues": change_count > 0,
                        "speaker_change_count": change_count,
                        "confidence": 0.0,
                        "source": source_tag,
                    }
                else:
                    return {
                        "has_multi_speaker_cues": False,
                        "speaker_change_count": 0,
                        "confidence": 0.0,
                        "source": source_tag,
                    }
            else:
                return {
                    "has_multi_speaker_cues": False,
                    "speaker_change_count": 0,
                    "confidence": 0.0,
                    "source": source_tag,
                }
        except Exception as exc:
            raise RuntimeError(f"3D-Speaker bounded evidence extraction failed: {exc}") from exc

    # 2. Test harness / mock fallback: dedicated probe_evidence method if present
    if hasattr(diarizer, "probe_evidence") and callable(diarizer.probe_evidence):
        evidence_res = diarizer.probe_evidence(audio_path)
        if isinstance(evidence_res, dict):
            has_multi = bool(evidence_res.get("has_multi_speaker_cues", False))
            change_count = int(evidence_res.get("speaker_change_count", 1 if has_multi else 0))
            return {
                "has_multi_speaker_cues": has_multi,
                "speaker_change_count": change_count,
                "confidence": float(evidence_res.get("confidence", 0.0)),
                "source": str(evidence_res.get("source", source_tag)),
            }

    # 3. Test harness / mock fallback: direct extract_embeddings if present
    if hasattr(diarizer, "extract_embeddings") and callable(diarizer.extract_embeddings):
        embeddings = diarizer.extract_embeddings(audio_path, max_windows=16)
        change_count = 0
        if isinstance(embeddings, list) and len(embeddings) > 1:
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

    return {
        "has_multi_speaker_cues": False,
        "speaker_change_count": 0,
        "confidence": 0.0,
        "source": source_tag,
    }


def main() -> None:
    # Ensure UTF-8 streams
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

    try:
        if mode == "evidence":
            evidence = run_evidence_probe(
                audio_path, model_name, model_version, vad_model_name, vad_model_version, embedding_cosine_threshold
            )
            resp = {
                "speaker_evidence": evidence,
                "model_name": model_name,
                "model_version": model_version,
                "vad_model_name": vad_model_name,
                "vad_model_version": vad_model_version,
            }
        else:
            assignments = run_diarization(
                audio_path, model_name, model_version, vad_model_name, vad_model_version
            )
            if not assignments:
                sys.stderr.write("Error: 3D-Speaker diarization produced no speaker assignments\n")
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
