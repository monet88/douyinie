#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/ocr_rapidocr.py
Verifies RapidOCR adapter contracts, polygon-to-box conversion, ink refinement,
deterministic testing hooks, and CLI execution.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

from ocr_rapidocr import (
    parse_rapidocr_result,
    refine_box_to_ink,
    detect_text_rapidocr,
    DEFAULT_MODEL_NAME,
    DEFAULT_MODEL_VERSION,
)
import ocr_rapidocr


class TestOCRRapidOCR(unittest.TestCase):
    def test_parse_rapidocr_result_standard(self):
        # Format returned by RapidOCR: tuple ([ [poly, text, score], ... ], elapse_list)
        raw = (
            [
                [
                    [[100, 200], [300, 200], [300, 250], [100, 250]],
                    "同款上帝视角",
                    0.96,
                ],
                [
                    [[50, 50], [150, 50], [150, 80], [50, 80]],
                    "关注我",
                    0.92,
                ],
            ],
            [0.01, 0.02, 0.01],
        )

        detections = parse_rapidocr_result(raw, frame_idx=3, ts_ms=1500)
        self.assertEqual(len(detections), 2)

        d1 = detections[0]
        self.assertEqual(d1["text"], "同款上帝视角")
        self.assertAlmostEqual(d1["confidence"], 0.96)
        self.assertEqual(d1["frame_index"], 3)
        self.assertEqual(d1["timestamp_ms"], 1500)
        self.assertEqual(d1["box"]["x"], 100)
        self.assertEqual(d1["box"]["y"], 200)
        self.assertEqual(d1["box"]["width"], 200)
        self.assertEqual(d1["box"]["height"], 50)
        self.assertEqual(d1["polygon"], [[100, 200], [300, 200], [300, 250], [100, 250]])

        d2 = detections[1]
        self.assertEqual(d2["text"], "关注我")
        self.assertEqual(d2["box"]["x"], 50)
        self.assertEqual(d2["box"]["y"], 50)
        self.assertEqual(d2["box"]["width"], 100)
        self.assertEqual(d2["box"]["height"], 30)

    def test_parse_rapidocr_result_empty_or_none(self):
        self.assertEqual(parse_rapidocr_result(None, 0, 0), [])
        self.assertEqual(parse_rapidocr_result(([], []), 0, 0), [])
        self.assertEqual(parse_rapidocr_result([], 0, 0), [])

    def test_refine_box_to_ink_disabled(self):
        box = {"x": 100, "y": 200, "width": 50, "height": 20}
        os.environ["DOUYINIE_OCR_INK_REFINE"] = "0"
        try:
            refined = refine_box_to_ink(None, box)
            self.assertEqual(refined, box)
        finally:
            os.environ.pop("DOUYINIE_OCR_INK_REFINE", None)

    def test_detect_text_rapidocr_with_mock_factory(self):
        def mock_factory(media_path, **kwargs):
            return {
                "frame_width": 1080,
                "frame_height": 1920,
                "frame_sample_step_ms": 500,
                "detections": [
                    {
                        "frame_index": 0,
                        "timestamp_ms": 0,
                        "text": "测试字幕",
                        "confidence": 0.99,
                        "box": {"x": 100, "y": 100, "width": 200, "height": 40},
                    }
                ],
                "model_name": "rapidocr-v4",
                "model_version": "v4",
            }

        orig_factory = ocr_rapidocr._RAPIDOCR_MODEL_FACTORY
        try:
            ocr_rapidocr._RAPIDOCR_MODEL_FACTORY = mock_factory
            res = detect_text_rapidocr("dummy.mp4")
            self.assertEqual(res["frame_width"], 1080)
            self.assertEqual(res["frame_height"], 1920)
            self.assertEqual(len(res["detections"]), 1)
            self.assertEqual(res["detections"][0]["text"], "测试字幕")
        finally:
            ocr_rapidocr._RAPIDOCR_MODEL_FACTORY = orig_factory

    def test_cli_execution_missing_file_fail_closed(self):
        script_path = os.path.abspath(
            os.path.join(os.path.dirname(__file__), "ocr_rapidocr.py")
        )

        req = {
            "media_path": "/nonexistent/video.mp4",
            "frame_sample_step_ms": 500,
        }

        proc = subprocess.run(
            [sys.executable, script_path],
            input=json.dumps(req),
            text=True,
            capture_output=True,
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("Source media file not found", proc.stderr)


if __name__ == "__main__":
    unittest.main()
