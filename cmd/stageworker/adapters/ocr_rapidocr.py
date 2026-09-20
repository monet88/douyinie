#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Visual Text Detection / OCR via RapidOCR (ONNX Runtime)
Fast, lightweight PP-OCRv4 engine running without PaddlePaddle or PyTorch.

Contract:
- Stdin: JSON request with media_path, frame_sample_step_ms, max_frames, model_name, model_version,
  det_model_dir, rec_model_dir, cls_model_dir, run_id, attempt_id
- Stdout: JSON response with frame_width, frame_height, frame_sample_step_ms,
  detections (frame_index, timestamp_ms, text, confidence, box, polygon), model_name, model_version
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import hashlib
import json
import os
import sys
from typing import Any, Dict, List, Optional, Tuple

try:
    import cv2
except ImportError:
    cv2 = None

DEFAULT_MODEL_NAME = "rapidocr-v4"
DEFAULT_MODEL_VERSION = "v4"

# Pluggable hook for deterministic unit testing without ONNX wheels
_RAPIDOCR_MODEL_FACTORY = None
_RAPIDOCR_CLASS = None


def preprocess_for_ocr(frame: Any) -> Any:
    """Pass-through preprocessor preserving raw frame BGR pixels."""
    return frame


def sample_media_frames(media_path: str, frame_sample_step_ms: int = 500, max_frames: int = 0) -> Tuple[int, int, List[Dict[str, Any]]]:
    """Sample video frames at deterministic timestamp steps using cv2.VideoCapture."""
    if cv2 is None:
        raise RuntimeError("OpenCV (cv2) runtime not found: install opencv-python-headless")

    cap = cv2.VideoCapture(media_path)
    if not cap.isOpened():
        raise RuntimeError(f"Failed to open video media: {media_path}")

    try:
        w = int(cap.get(cv2.CAP_PROP_FRAME_WIDTH) or 1080)
        h = int(cap.get(cv2.CAP_PROP_FRAME_HEIGHT) or 1920)
        fps = float(cap.get(cv2.CAP_PROP_FPS) or 25.0)
        if fps <= 0.0:
            fps = 25.0
        total_frames = int(cap.get(cv2.CAP_PROP_FRAME_COUNT) or 0)
        duration_ms = int((total_frames / fps) * 1000.0) if total_frames > 0 else 0

        step_ms = max(frame_sample_step_ms or 500, 50)
        sampled = []
        current_ts = 0

        while True:
            if max_frames > 0 and len(sampled) >= max_frames:
                break
            if total_frames > 0 and duration_ms > 0 and current_ts > duration_ms and len(sampled) > 0:
                break

            target_frame_num = int(round((current_ts / 1000.0) * fps))
            if total_frames > 0 and target_frame_num >= total_frames and len(sampled) > 0:
                break

            cap.set(cv2.CAP_PROP_POS_FRAMES, target_frame_num)
            ret, frame = cap.read()
            if not ret:
                break

            sampled.append({
                "frame_index": target_frame_num,
                "timestamp_ms": current_ts,
                "image": preprocess_for_ocr(frame),
            })

            current_ts += step_ms

        if not sampled:
            cap.set(cv2.CAP_PROP_POS_FRAMES, 0)
            ret, frame = cap.read()
            if ret:
                sampled.append({
                    "frame_index": 0,
                    "timestamp_ms": 0,
                    "image": preprocess_for_ocr(frame),
                })

        return w, h, sampled
    finally:
        cap.release()


INK_SEARCH_X_RATIO = 0.06
INK_SEARCH_Y_RATIO = 0.30
INK_GROW_CAP_X_RATIO = 0.05
INK_GROW_CAP_Y_RATIO = 0.20


def refine_box_to_ink(image: Any, box: Dict[str, int]) -> Dict[str, int]:
    """Grow a detection box to the text ink it overlaps, within a search ring and a growth cap."""
    if cv2 is None or image is None or os.environ.get("DOUYINIE_OCR_INK_REFINE", "1").strip() in ("0", "false", "off"):
        return box
    try:
        gray = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY) if image.ndim == 3 else image
    except Exception:
        return box
    h, w = gray.shape[:2]
    bx, by, bw, bh = int(box["x"]), int(box["y"]), int(box["width"]), int(box["height"])
    if bw <= 0 or bh <= 0:
        return box
    mx = max(4, int(round(bw * INK_SEARCH_X_RATIO)))
    my = max(3, int(round(bh * INK_SEARCH_Y_RATIO)))
    x0, x1 = max(0, bx - mx), min(w, bx + bw + mx)
    y0, y1 = max(0, by - my), min(h, by + bh + my)
    if x1 - x0 <= 1 or y1 - y0 <= 1:
        return box
    crop = gray[y0:y1, x0:x1]
    _, mask = cv2.threshold(crop, 0, 255, cv2.THRESH_BINARY + cv2.THRESH_OTSU)

    def ink_union(candidate: Any) -> Tuple[int, int, int, int, bool]:
        num, _labels, stats, _centroids = cv2.connectedComponentsWithStats(candidate, 8)
        ux0, uy0, ux1, uy1 = bx, by, bx + bw, by + bh
        found = False
        for i in range(1, num):
            fx, fy, fw, fh = stats[i, 0] + x0, stats[i, 1] + y0, stats[i, 2], stats[i, 3]
            if fx + fw <= bx or fx >= bx + bw or fy + fh <= by or fy >= by + bh:
                continue
            if fw > max(8, int(bw * 0.75)) or fh > max(6, int(bh * 1.6)) or fw * fh < 4:
                continue
            ux0, uy0 = min(ux0, fx), min(uy0, fy)
            ux1, uy1 = max(ux1, fx + fw), max(uy1, fy + fh)
            found = True
        return ux0, uy0, ux1, uy1, found

    ux0, uy0, ux1, uy1, found = ink_union(mask)
    if not found:
        ux0, uy0, ux1, uy1, found = ink_union(cv2.bitwise_not(mask))
    if not found:
        return box

    cap_x = max(2, int(round(bw * INK_GROW_CAP_X_RATIO)))
    cap_y = max(2, int(round(bh * INK_GROW_CAP_Y_RATIO)))
    nx0 = max(bx - cap_x, ux0)
    ny0 = max(by - cap_y, uy0)
    nx1 = min(bx + bw + cap_x, ux1)
    ny1 = min(by + bh + cap_y, uy1)
    return {"x": int(nx0), "y": int(ny0), "width": int(nx1 - nx0), "height": int(ny1 - ny0)}


def parse_rapidocr_result(raw_result: Any, frame_idx: int, ts_ms: int) -> List[Dict[str, Any]]:
    """
    Parse RapidOCR raw prediction result: (results, elapse_list).
    Each result entry is [dt_box, text, score].
    dt_box is [[x1, y1], [x2, y2], [x3, y3], [x4, y4]].
    """
    if not raw_result:
        return []

    # RapidOCR 3.8+ returns RapidOCROutput with .boxes, .txts, .scores
    if hasattr(raw_result, "boxes") and hasattr(raw_result, "txts") and hasattr(raw_result, "scores"):
        raw_boxes = getattr(raw_result, "boxes", None)
        raw_txts = getattr(raw_result, "txts", None)
        raw_scores = getattr(raw_result, "scores", None)
        if raw_boxes is not None and raw_txts is not None and raw_scores is not None and len(raw_boxes) > 0:
            items = list(zip(raw_boxes, raw_txts, raw_scores))
        else:
            items = []
    elif isinstance(raw_result, (list, tuple)) and len(raw_result) == 2 and isinstance(raw_result[1], list):
        items = raw_result[0] if raw_result[0] is not None else []
    elif isinstance(raw_result, (list, tuple)):
        items = raw_result
    else:
        items = []
    if not items:
        return []

    detections = []
    for item in items:
        if not item or len(item) < 3:
            continue
        poly = item[0]
        text = str(item[1]).strip()
        score = float(item[2])

        if not text:
            continue

        # Extract bounding box from polygon
        xs = [pt[0] for pt in poly]
        ys = [pt[1] for pt in poly]
        min_x = max(0, int(min(xs)))
        min_y = max(0, int(min(ys)))
        max_x = int(max(xs))
        max_y = int(max(ys))
        width = max(1, max_x - min_x)
        height = max(1, max_y - min_y)

        detections.append({
            "frame_index": frame_idx,
            "timestamp_ms": ts_ms,
            "text": text,
            "confidence": score,
            "box": {
                "x": min_x,
                "y": min_y,
                "width": width,
                "height": height,
            },
            "polygon": [[int(pt[0]), int(pt[1])] for pt in poly],
        })

    return detections


def detect_text_rapidocr(
    media_path: str,
    frame_sample_step_ms: int = 500,
    max_frames: int = 0,
    model_name: str = DEFAULT_MODEL_NAME,
    model_version: str = DEFAULT_MODEL_VERSION,
    det_model_dir: str = "",
    rec_model_dir: str = "",
    cls_model_dir: str = "",
) -> Dict[str, Any]:
    """Execute visual OCR text detection across sampled frames using RapidOCR (ONNX)."""
    if _RAPIDOCR_MODEL_FACTORY is not None:
        return _RAPIDOCR_MODEL_FACTORY(
            media_path=media_path,
            frame_sample_step_ms=frame_sample_step_ms,
            max_frames=max_frames,
            model_name=model_name,
            model_version=model_version,
            det_model_dir=det_model_dir,
            rec_model_dir=rec_model_dir,
            cls_model_dir=cls_model_dir,
        )

    w, h, sampled_frames = sample_media_frames(media_path, frame_sample_step_ms, max_frames)

    # Initialize RapidOCR engine
    if _RAPIDOCR_CLASS is not None:
        OCRClass = _RAPIDOCR_CLASS
    else:
        try:
            from rapidocr import RapidOCR  # type: ignore
            OCRClass = RapidOCR
        except ImportError:
            try:
                from rapidocr_onnxruntime import RapidOCR  # type: ignore
                OCRClass = RapidOCR
            except ImportError as exc:
                raise RuntimeError(
                    "RapidOCR runtime not found: install rapidocr or rapidocr_onnxruntime (pip install rapidocr)"
                ) from exc

    kwargs = {}
    if det_model_dir and os.path.isfile(det_model_dir):
        kwargs["det_model_path"] = det_model_dir
    if rec_model_dir and os.path.isfile(rec_model_dir):
        kwargs["rec_model_path"] = rec_model_dir
    if cls_model_dir and os.path.isfile(cls_model_dir):
        kwargs["cls_model_path"] = cls_model_dir

    ocr = OCRClass(**kwargs)

    detections = []
    last_hash = None
    last_frame_detections = []

    for f in sampled_frames:
        frame_idx = f["frame_index"]
        ts_ms = f["timestamp_ms"]
        img = f["image"]

        frame_hash = hashlib.blake2b(img.tobytes(), digest_size=16).digest() if hasattr(img, "tobytes") else None
        if frame_hash is not None and frame_hash == last_hash:
            detections.extend(
                {**d, "frame_index": frame_idx, "timestamp_ms": ts_ms}
                for d in last_frame_detections
            )
            continue

        raw_res = ocr(img)
        frame_detections = parse_rapidocr_result(raw_res, frame_idx, ts_ms)
        for d in frame_detections:
            d["box"] = refine_box_to_ink(img, d["box"])
        detections.extend(frame_detections)
        last_hash = frame_hash
        last_frame_detections = frame_detections

    return {
        "frame_width": w,
        "frame_height": h,
        "frame_sample_step_ms": frame_sample_step_ms or 500,
        "detections": detections,
        "model_name": model_name or DEFAULT_MODEL_NAME,
        "model_version": model_version or DEFAULT_MODEL_VERSION,
    }


def main():
    if hasattr(sys.stdin, "reconfigure"):
        sys.stdin.reconfigure(encoding="utf-8")
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")

    try:
        input_data = sys.stdin.read()
        if not input_data.strip():
            sys.stderr.write("Empty input payload\n")
            sys.exit(1)

        req = json.loads(input_data)
        media_path = req.get("media_path", "")
        frame_sample_step_ms = int(req.get("frame_sample_step_ms") or 500)
        max_frames = int(req.get("max_frames") or 0)
        model_name = req.get("model_name", DEFAULT_MODEL_NAME)
        model_version = req.get("model_version", DEFAULT_MODEL_VERSION)
        det_model_dir = req.get("det_model_dir", "")
        rec_model_dir = req.get("rec_model_dir", "")
        cls_model_dir = req.get("cls_model_dir", "") or req.get("ori_model_dir", "")

        if not media_path or not os.path.exists(media_path):
            sys.stderr.write(f"Source media file not found: {media_path}\n")
            sys.exit(1)

        res = detect_text_rapidocr(
            media_path=media_path,
            frame_sample_step_ms=frame_sample_step_ms,
            max_frames=max_frames,
            model_name=model_name,
            model_version=model_version,
            det_model_dir=det_model_dir,
            rec_model_dir=rec_model_dir,
            cls_model_dir=cls_model_dir,
        )

        out = {
            "frame_width": res.get("frame_width", 1080),
            "frame_height": res.get("frame_height", 1920),
            "frame_sample_step_ms": res.get("frame_sample_step_ms", frame_sample_step_ms),
            "detections": res.get("detections", []),
            "model_name": model_name,
            "model_version": model_version,
        }
        sys.stdout.write(json.dumps(out, ensure_ascii=False) + "\n")
        sys.stdout.flush()
        sys.exit(0)

    except Exception as e:
        sys.stderr.write(f"RapidOCR error: {e}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
