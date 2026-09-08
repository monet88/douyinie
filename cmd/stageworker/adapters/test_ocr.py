#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/ocr.py
Verifies visual OCR adapter contracts, deterministic frame sampling,
fail-closed snapshot validation, upstream PaddleOCR 3.7.0 predict() contract,
and strict v3 Result payload parsing.
"""

import json
import math
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import ocr

try:
    import cv2
    import numpy as np
except ImportError:
    cv2 = None
    np = None


class MockV3Result:
    """Simulates PaddleOCR 3.7.0 Result object exposing .res mapping."""
    def __init__(self, res: dict):
        self.res = res

    def __getitem__(self, key):
        return self.res[key]


class TestOCRAdapter(unittest.TestCase):
    def tearDown(self):
        ocr._OCR_MODEL_FACTORY = None
        ocr._PADDLE_OCR_CLASS = None

    def test_mock_factory_detection(self):
        with tempfile.NamedTemporaryFile(suffix=".mp4", delete=False) as f:
            f.write(b"fake_mp4_bytes")
            tmp_path = f.name

        try:
            def mock_ocr(path, step_ms, max_frames, model_name, model_version):
                return {
                    "frame_width": 1080,
                    "frame_height": 1920,
                    "frame_sample_step_ms": step_ms,
                    "detections": [
                        {
                            "frame_index": 0,
                            "timestamp_ms": 0,
                            "text": "SUPOR",
                            "confidence": 0.98,
                            "box": {"x": 50, "y": 60, "width": 120, "height": 40},
                        }
                    ],
                    "model_name": model_name,
                    "model_version": model_version,
                }

            ocr._OCR_MODEL_FACTORY = mock_ocr
            res = ocr.detect_text_paddleocr(tmp_path, 500, 10, "paddleocr-v6", "v6")
            self.assertEqual(res["frame_width"], 1080)
            self.assertEqual(len(res["detections"]), 1)
            self.assertEqual(res["detections"][0]["text"], "SUPOR")
        finally:
            if os.path.exists(tmp_path):
                os.unlink(tmp_path)

    def test_missing_snapshot_dirs_fails_closed(self):
        with tempfile.NamedTemporaryFile(suffix=".png", delete=False) as f:
            f.write(b"fake_image_bytes")
            tmp_path = f.name

        try:
            with self.assertRaises(RuntimeError) as ctx:
                ocr.detect_text_paddleocr(
                    tmp_path,
                    500,
                    10,
                    "PP-OCRv6",
                    "v6",
                    det_model_dir="/nonexistent/det",
                    rec_model_dir="/nonexistent/rec",
                    cls_model_dir="/nonexistent/cls",
                )
            self.assertIn("snapshot directory missing or inaccessible", str(ctx.exception))
        finally:
            if os.path.exists(tmp_path):
                os.unlink(tmp_path)

    @unittest.skipIf(cv2 is None or np is None, "OpenCV and numpy required for frame sampling tests")
    def test_sample_media_frames_image(self):
        with tempfile.NamedTemporaryFile(suffix=".png", delete=False) as f:
            img = np.zeros((240, 320, 3), dtype=np.uint8)
            cv2.imwrite(f.name, img)
            tmp_path = f.name

        try:
            w, h, frames = ocr.sample_media_frames(tmp_path, 500, 0)
            self.assertEqual(w, 320)
            self.assertEqual(h, 240)
            self.assertEqual(len(frames), 1)
            self.assertEqual(frames[0]["frame_index"], 0)
            self.assertEqual(frames[0]["timestamp_ms"], 0)
            self.assertEqual(frames[0]["image"].shape, (240, 320, 3))
        finally:
            if os.path.exists(tmp_path):
                os.unlink(tmp_path)

    @unittest.skipIf(cv2 is None or np is None, "OpenCV and numpy required for frame sampling tests")
    def test_sample_media_frames_synthetic_video(self):
        """
        Proves cadence 500ms at known FPS (20 FPS) yields expected source frame indices,
        not sample ordinal indices (0, 1, 2...).
        At 20 FPS:
          t=0ms    -> frame_index = 0
          t=500ms  -> frame_index = 10 (0.5 * 20)
          t=1000ms -> frame_index = 20 (1.0 * 20)
        """
        with tempfile.NamedTemporaryFile(suffix=".mp4", delete=False) as f:
            tmp_video_path = f.name

        try:
            fps = 20.0
            width, height = 320, 240
            fourcc = cv2.VideoWriter_fourcc(*"mp4v")
            writer = cv2.VideoWriter(tmp_video_path, fourcc, fps, (width, height))
            total_frames_to_write = 30  # 1.5 seconds at 20 FPS
            for i in range(total_frames_to_write):
                frame = np.full((height, width, 3), i, dtype=np.uint8)
                writer.write(frame)
            writer.release()

            w, h, frames = ocr.sample_media_frames(tmp_video_path, frame_sample_step_ms=500, max_frames=0)
            self.assertEqual(w, width)
            self.assertEqual(h, height)
            self.assertGreaterEqual(len(frames), 3)

            self.assertEqual(frames[0]["frame_index"], 0)
            self.assertEqual(frames[0]["timestamp_ms"], 0)

            self.assertEqual(frames[1]["frame_index"], 10)
            self.assertEqual(frames[1]["timestamp_ms"], 500)

            self.assertEqual(frames[2]["frame_index"], 20)
            self.assertEqual(frames[2]["timestamp_ms"], 1000)
        finally:
            if os.path.exists(tmp_video_path):
                os.unlink(tmp_video_path)

    @unittest.skipIf(cv2 is None or np is None, "OpenCV and numpy required for mock pipeline tests")
    def test_paddleocr_370_pipeline_and_contract(self):
        """
        Verifies upstream PaddleOCR 3.7.0 contract:
        - Rejects show_log in __init__
        - Calls predict(), not legacy .ocr()
        - Rejects legacy cls=True kwarg in predict()
        - Parses v3 Result object exposing rec_texts, rec_scores, rec_polys
        """
        det_dir = tempfile.mkdtemp(prefix="det_snap_")
        rec_dir = tempfile.mkdtemp(prefix="rec_snap_")
        cls_dir = tempfile.mkdtemp(prefix="cls_snap_")

        with tempfile.NamedTemporaryFile(suffix=".png", delete=False) as f:
            img = np.zeros((480, 640, 3), dtype=np.uint8)
            cv2.imwrite(f.name, img)
            tmp_path = f.name

        try:
            captured_init_kwargs = {}
            predict_invoked = False
            captured_predict_kwargs = {}

            class Upstream370PaddleOCR:
                def __init__(self, **kwargs):
                    nonlocal captured_init_kwargs
                    captured_init_kwargs = kwargs
                    # PaddleOCR 3.7.0 raises ValueError or Unknown argument on unsupported kwargs like show_log
                    if "show_log" in kwargs:
                        raise ValueError("Unknown argument 'show_log' in PaddleOCR 3.7.0")

                def predict(self, input_img, **kwargs):
                    nonlocal predict_invoked, captured_predict_kwargs
                    predict_invoked = True
                    captured_predict_kwargs = kwargs
                    # In 3.7.0 predict() does not accept legacy cls=True kwarg
                    if "cls" in kwargs:
                        raise TypeError("predict() got an unexpected keyword argument 'cls'")

                    # Return upstream v3 Result object
                    v3_res = {
                        "input_path": "sampled_frame.png",
                        "rec_texts": ["SUPOR", "304不锈钢"],
                        "rec_scores": np.array([0.985, 0.962]),
                        "rec_polys": np.array([
                            [[20, 30], [180, 30], [180, 75], [20, 75]],
                            [[20, 90], [220, 90], [220, 130], [20, 130]],
                        ]),
                    }
                    return [MockV3Result(v3_res)]

                def ocr(self, *args, **kwargs):
                    raise AssertionError("Legacy ocr() method must not be called; use predict()")

            ocr._PADDLE_OCR_CLASS = Upstream370PaddleOCR
            res = ocr.detect_text_paddleocr(
                tmp_path,
                500,
                5,
                "PP-OCRv6",
                "v6",
                det_model_dir=det_dir,
                rec_model_dir=rec_dir,
                cls_model_dir=cls_dir,
            )

            # 1. Verify show_log was NOT passed
            self.assertNotIn("show_log", captured_init_kwargs)
            self.assertEqual(captured_init_kwargs.get("text_detection_model_name"), "PP-OCRv6_medium_det")
            self.assertEqual(captured_init_kwargs.get("text_detection_model_dir"), det_dir)
            self.assertEqual(captured_init_kwargs.get("text_recognition_model_name"), "PP-OCRv6_medium_rec")
            self.assertEqual(captured_init_kwargs.get("text_recognition_model_dir"), rec_dir)
            self.assertEqual(captured_init_kwargs.get("textline_orientation_model_name"), "PP-LCNet_x1_0_textline_ori")
            self.assertEqual(captured_init_kwargs.get("textline_orientation_model_dir"), cls_dir)
            self.assertTrue(captured_init_kwargs.get("use_textline_orientation"))

            # 2. Verify predict was invoked and cls was NOT passed
            self.assertTrue(predict_invoked)
            self.assertNotIn("cls", captured_predict_kwargs)

            # 3. Verify parsed detections contract
            self.assertEqual(res["frame_width"], 640)
            self.assertEqual(res["frame_height"], 480)
            self.assertEqual(len(res["detections"]), 2)

            det0 = res["detections"][0]
            self.assertEqual(det0["text"], "SUPOR")
            self.assertAlmostEqual(det0["confidence"], 0.985, places=3)
            self.assertEqual(det0["box"], {"x": 20, "y": 30, "width": 160, "height": 45})

            det1 = res["detections"][1]
            self.assertEqual(det1["text"], "304不锈钢")
            self.assertAlmostEqual(det1["confidence"], 0.962, places=3)
            self.assertEqual(det1["box"], {"x": 20, "y": 90, "width": 200, "height": 40})

        finally:
            if os.path.exists(tmp_path):
                os.unlink(tmp_path)
            for d in [det_dir, rec_dir, cls_dir]:
                if os.path.exists(d):
                    os.rmdir(d)

    def test_mismatched_array_lengths_fails_closed(self):
        """Mismatched rec_texts, rec_scores, or rec_polys lengths must raise ValueError."""
        res_payload = {
            "rec_texts": ["A", "B"],
            "rec_scores": [0.95],  # only 1 score for 2 texts
            "rec_polys": [[[0, 0], [10, 0], [10, 10], [0, 10]], [[0, 0], [10, 0], [10, 10], [0, 10]]],
        }
        with self.assertRaises(ValueError) as ctx:
            ocr.parse_v3_result([MockV3Result(res_payload)], 0, 0)
        self.assertIn("structurally inconsistent result lengths", str(ctx.exception))

    def test_malformed_confidence_fails_closed(self):
        """Non-numeric or out-of-range confidence scores must fail closed."""
        res_nan = {
            "rec_texts": ["A"],
            "rec_scores": [float("nan")],
            "rec_polys": [[[0, 0], [10, 0], [10, 10], [0, 10]]],
        }
        with self.assertRaises(ValueError) as ctx:
            ocr.parse_v3_result([MockV3Result(res_nan)], 0, 0)
        self.assertIn("Invalid PaddleOCR v3 confidence score", str(ctx.exception))

        res_out_of_range = {
            "rec_texts": ["A"],
            "rec_scores": [1.5],
            "rec_polys": [[[0, 0], [10, 0], [10, 10], [0, 10]]],
        }
        with self.assertRaises(ValueError) as ctx:
            ocr.parse_v3_result([MockV3Result(res_out_of_range)], 0, 0)
        self.assertIn("Invalid PaddleOCR v3 confidence score", str(ctx.exception))

    def test_degenerate_polygon_fails_closed(self):
        """Polygons with fewer than 3 points or collapsing to 0 area must fail closed."""
        res_few_points = {
            "rec_texts": ["A"],
            "rec_scores": [0.9],
            "rec_polys": [[[0, 0], [10, 10]]],
        }
        with self.assertRaises(ValueError) as ctx:
            ocr.parse_v3_result([MockV3Result(res_few_points)], 0, 0)
        self.assertIn("expected at least 3 points", str(ctx.exception))

        res_zero_area = {
            "rec_texts": ["A"],
            "rec_scores": [0.9],
            "rec_polys": [[[10, 10], [10, 10], [10, 10], [10, 10]]],
        }
        with self.assertRaises(ValueError) as ctx:
            ocr.parse_v3_result([MockV3Result(res_zero_area)], 0, 0)
        self.assertIn("Degenerate PaddleOCR v3 bounding box", str(ctx.exception))

    def test_dt_polys_fallback(self):
        """When rec_polys is missing/empty, falls back to dt_polys cleanly."""
        res_dt_fallback = {
            "rec_texts": ["FALLBACK"],
            "rec_scores": [0.92],
            "rec_polys": [],
            "dt_polys": [[[15, 25], [100, 25], [100, 60], [15, 60]]],
        }
        dets = ocr.parse_v3_result([MockV3Result(res_dt_fallback)], 1, 500)
        self.assertEqual(len(dets), 1)
        self.assertEqual(dets[0]["text"], "FALLBACK")
        self.assertEqual(dets[0]["confidence"], 0.92)
        self.assertEqual(dets[0]["box"], {"x": 15, "y": 25, "width": 85, "height": 35})


if __name__ == "__main__":
    unittest.main()
