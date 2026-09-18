#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Visual Text Detection / OCR (PP-OCRv6-family via PaddleOCR 3.7.0)
Samples video frames deterministically and extracts text bounding boxes, confidence, and timestamps.

Contract:
- Stdin: JSON request with media_path, frame_sample_step_ms, max_frames, model_name, model_version,
  det_model_dir, rec_model_dir, cls_model_dir (or ori_model_dir), run_id, attempt_id
- Stdout: JSON response with frame_width, frame_height, frame_sample_step_ms,
  detections (frame_index, timestamp_ms, text, confidence, box), model_name, model_version
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import hashlib
import json
import math
import os
import sys
from typing import Any, Dict, List, Optional, Tuple

try:
    import cv2
except ImportError:
    cv2 = None

# Pluggable hooks for deterministic testing without full ML wheels
_OCR_MODEL_FACTORY = None
_PADDLE_OCR_CLASS = None

def extract_v3_res_payload(result_item: Any) -> Dict[str, Any]:
    """Extract inner res dictionary from PaddleOCR v3 Result object or dict."""
    if hasattr(result_item, "res") and isinstance(result_item.res, dict):
        return result_item.res
    if isinstance(result_item, dict):
        if "res" in result_item and isinstance(result_item["res"], dict):
            return result_item["res"]
        if "rec_texts" in result_item:
            return result_item
    if hasattr(result_item, "__getitem__"):
        try:
            val = result_item["res"]
            if isinstance(val, dict):
                return val
        except (KeyError, TypeError, IndexError):
            pass
        try:
            if "rec_texts" in result_item:
                return result_item
        except (KeyError, TypeError, IndexError):
            pass
    raise ValueError(f"Malformed PaddleOCR v3 Result payload: expected Result object or dict with 'res', got {type(result_item)}")


def parse_v3_result(predict_results: Any, frame_idx: int, ts_ms: int) -> List[Dict[str, Any]]:
    """
    Parse PaddleOCR v3 Result list deterministically.
    Aligns rec_texts, rec_scores, and rec_polys (fallback to dt_polys).
    Fails closed on structurally inconsistent array lengths or malformed outputs.
    """
    if predict_results is None:
        return []

    if not isinstance(predict_results, (list, tuple)):
        items = [predict_results]
    else:
        items = predict_results

    detections: List[Dict[str, Any]] = []

    for item in items:
        if item is None:
            continue
        res = extract_v3_res_payload(item)

        rec_texts = res.get("rec_texts")
        rec_scores = res.get("rec_scores")
        rec_polys = res.get("rec_polys")

        # If rec_polys missing or empty, fall back to dt_polys if present
        if rec_polys is None or (hasattr(rec_polys, "__len__") and len(rec_polys) == 0):
            dt_polys = res.get("dt_polys")
            if dt_polys is not None and hasattr(dt_polys, "__len__") and len(dt_polys) > 0:
                rec_polys = dt_polys

        if rec_texts is None or rec_scores is None or rec_polys is None:
            raise ValueError(
                f"Malformed PaddleOCR v3 Result payload: missing required fields "
                f"(rec_texts={'present' if rec_texts is not None else 'missing'}, "
                f"rec_scores={'present' if rec_scores is not None else 'missing'}, "
                f"rec_polys={'present' if rec_polys is not None else 'missing'})"
            )

        texts_list = rec_texts.tolist() if hasattr(rec_texts, "tolist") else list(rec_texts)
        scores_list = rec_scores.tolist() if hasattr(rec_scores, "tolist") else list(rec_scores)
        polys_list = rec_polys.tolist() if hasattr(rec_polys, "tolist") else list(rec_polys)

        n_texts = len(texts_list)
        n_scores = len(scores_list)
        n_polys = len(polys_list)

        if n_texts != n_scores or n_texts != n_polys:
            raise ValueError(
                f"PaddleOCR v3 structurally inconsistent result lengths: "
                f"rec_texts={n_texts}, rec_scores={n_scores}, rec_polys={n_polys}"
            )

        for i in range(n_texts):
            raw_text = texts_list[i]
            text = str(raw_text).strip() if raw_text is not None else ""
            if not text:
                continue

            raw_score = scores_list[i]
            try:
                conf = float(raw_score)
            except (ValueError, TypeError):
                raise ValueError(f"Malformed PaddleOCR v3 rec_score at index {i}: {raw_score!r}")
            if math.isnan(conf) or math.isinf(conf) or conf < 0.0 or conf > 1.0:
                raise ValueError(f"Invalid PaddleOCR v3 confidence score at index {i}: {conf}")

            poly = polys_list[i]
            if hasattr(poly, "tolist"):
                poly = poly.tolist()
            if not isinstance(poly, (list, tuple)) or len(poly) < 3:
                raise ValueError(f"Malformed PaddleOCR v3 polygon at index {i}: expected at least 3 points, got {poly!r}")

            xs: List[float] = []
            ys: List[float] = []
            for pt in poly:
                if hasattr(pt, "tolist"):
                    pt = pt.tolist()
                if not isinstance(pt, (list, tuple)) or len(pt) < 2:
                    raise ValueError(f"Malformed point in PaddleOCR v3 polygon at index {i}: {pt!r}")
                try:
                    px = float(pt[0])
                    py = float(pt[1])
                except (ValueError, TypeError):
                    raise ValueError(f"Non-numeric coordinate in polygon at index {i}: {pt!r}")
                if math.isnan(px) or math.isnan(py):
                    raise ValueError(f"NaN coordinate in polygon at index {i}: {pt!r}")
                xs.append(px)
                ys.append(py)

            min_x = min(xs)
            max_x = max(xs)
            min_y = min(ys)
            max_y = max(ys)

            x = int(round(min_x))
            y = int(round(min_y))
            width = int(round(max_x - min_x))
            height = int(round(max_y - min_y))

            if width <= 0 or height <= 0:
                raise ValueError(
                    f"Degenerate PaddleOCR v3 bounding box at index {i}: width={width}, height={height}, poly={poly}"
                )

            box = {
                "x": max(0, x),
                "y": max(0, y),
                "width": width,
                "height": height,
            }

            detections.append({
                "frame_index": frame_idx,
                "timestamp_ms": ts_ms,
                "text": text,
                "confidence": round(conf, 4),
                "box": box,
            })

    return detections

def preprocess_for_ocr(image):
    """Boost local contrast before detection.

    Subtitles and packaging labels are often low-contrast over busy footage; CLAHE on the luma
    channel recovers strokes the detector otherwise drops. Disable with DOUYINIE_OCR_CLAHE=0.
    """
    if os.environ.get("DOUYINIE_OCR_CLAHE", "1").strip() in ("0", "false", "off"):
        return image
    if cv2 is None:
        return image
    try:
        gray = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
        clahe = cv2.createCLAHE(clipLimit=1.5, tileGridSize=(8, 8))
        return cv2.cvtColor(clahe.apply(gray), cv2.COLOR_GRAY2BGR)
    except cv2.error:
        # A frame the color conversion cannot handle must never take the whole stage down:
        # the raw frame is still a valid OCR input.
        return image


def sample_media_frames(media_path: str, frame_sample_step_ms: int = 500, max_frames: int = 0) -> Tuple[int, int, List[Dict[str, Any]]]:
    """
    Deterministically sample video/image frames at the requested cadence.
    Returns (width, height, list_of_frames), where each frame is:
    {"frame_index": int, "timestamp_ms": int, "image": np.ndarray}
    """
    if cv2 is None:
        raise RuntimeError("OpenCV (cv2) runtime not found: install opencv-python")

    if not os.path.exists(media_path):
        raise RuntimeError(f"Source media file not found: {media_path}")

    ext = os.path.splitext(media_path)[1].lower()
    # Image file: return single frame at t=0
    if ext in [".png", ".jpg", ".jpeg", ".bmp", ".webp", ".tiff"]:
        img = cv2.imread(media_path)
        if img is None:
            raise RuntimeError(f"Failed to read image media: {media_path}")
        h, w = img.shape[:2]
        return w, h, [{"frame_index": 0, "timestamp_ms": 0, "image": preprocess_for_ocr(img)}]

    # Video file: sample frames at cadence
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
        # If video had no frames readable via set(), attempt simple sequential fallback
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


def detect_text_paddleocr(
    media_path: str,
    frame_sample_step_ms: int = 500,
    max_frames: int = 0,
    model_name: str = "paddleocr-v6",
    model_version: str = "v6",
    det_model_dir: str = "",
    rec_model_dir: str = "",
    cls_model_dir: str = "",
) -> Dict[str, Any]:
    """Execute visual OCR text detection across sampled frames using PaddleOCR."""
    if _OCR_MODEL_FACTORY is not None:
        try:
            return _OCR_MODEL_FACTORY(
                media_path,
                frame_sample_step_ms,
                max_frames,
                model_name,
                model_version,
                det_model_dir=det_model_dir,
                rec_model_dir=rec_model_dir,
                cls_model_dir=cls_model_dir,
            )
        except TypeError:
            return _OCR_MODEL_FACTORY(
                media_path,
                frame_sample_step_ms,
                max_frames,
                model_name,
                model_version,
            )

    # Validate snapshot directories - fail closed if missing or not a directory
    for role, d in [("detection", det_model_dir), ("recognition", rec_model_dir), ("orientation", cls_model_dir)]:
        if not d or not os.path.isdir(d):
            raise RuntimeError(f"Verified OCR {role} model snapshot directory missing or inaccessible: {d}; fail-closed")

    # Sample frames deterministically
    w, h, sampled_frames = sample_media_frames(media_path, frame_sample_step_ms, max_frames)

    # Initialize PaddleOCR engine
    if _PADDLE_OCR_CLASS is not None:
        OCRClass = _PADDLE_OCR_CLASS
    else:
        try:
            from paddleocr import PaddleOCR  # type: ignore
            OCRClass = PaddleOCR
        except ImportError:
            raise RuntimeError("PaddleOCR runtime not found: install paddleocr (pip install paddlepaddle paddleocr)")

    ocr = OCRClass(
        text_detection_model_name="PP-OCRv6_medium_det",
        text_detection_model_dir=det_model_dir,
        text_recognition_model_name="PP-OCRv6_medium_rec",
        text_recognition_model_dir=rec_model_dir,
        textline_orientation_model_name="PP-LCNet_x1_0_textline_ori",
        textline_orientation_model_dir=cls_model_dir,
        use_textline_orientation=True,
        use_doc_orientation_classify=False,
        use_doc_unwarping=False,
    )

    detections = []
    # Frames repeat bit-for-bit in a static shot, so the detector and recognizer would re-read the
    # same pixels several times: hash the preprocessed frame and replay the previous frame's
    # detections for an identical one, re-labelled with this frame's index and timestamp. The hash is
    # over the exact bytes, so a frame that differs at all (a caption appearing, an object moving) is
    # still read fresh - the reuse can never drop a detection the detector would have found.
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

        raw_res = ocr.predict(img)
        frame_detections = parse_v3_result(raw_res, frame_idx, ts_ms) if raw_res else []
        detections.extend(frame_detections)
        last_hash = frame_hash
        last_frame_detections = frame_detections
    return {
        "frame_width": w,
        "frame_height": h,
        "frame_sample_step_ms": frame_sample_step_ms or 500,
        "detections": detections,
        "model_name": model_name,
        "model_version": model_version,
    }


def main():
    try:
        input_data = sys.stdin.read()
        if not input_data.strip():
            sys.stderr.write("Empty input payload\n")
            sys.exit(1)

        req = json.loads(input_data)
        media_path = req.get("media_path", "")
        frame_sample_step_ms = int(req.get("frame_sample_step_ms") or 500)
        max_frames = int(req.get("max_frames") or 0)
        model_name = req.get("model_name", "paddleocr-v6")
        model_version = req.get("model_version", "v6")
        det_model_dir = req.get("det_model_dir", "")
        rec_model_dir = req.get("rec_model_dir", "")
        cls_model_dir = req.get("cls_model_dir", "") or req.get("ori_model_dir", "")

        if not media_path or not os.path.exists(media_path):
            sys.stderr.write(f"Source media file not found: {media_path}\n")
            sys.exit(1)

        res = detect_text_paddleocr(
            media_path,
            frame_sample_step_ms,
            max_frames,
            model_name,
            model_version,
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
        print(json.dumps(out))
        sys.exit(0)

    except Exception as e:
        sys.stderr.write(f"OCR error: {e}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
