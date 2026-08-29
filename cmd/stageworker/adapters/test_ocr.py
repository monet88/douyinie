#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/ocr.py
Verifies visual OCR adapter contracts and mock factory seams.
"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import ocr


class TestOCRAdapter(unittest.TestCase):
    def tearDown(self):
        ocr._OCR_MODEL_FACTORY = None

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


if __name__ == "__main__":
    unittest.main()
