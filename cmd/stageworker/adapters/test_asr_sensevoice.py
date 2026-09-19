#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/asr_sensevoice.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

from asr_sensevoice import (
    clean_sensevoice_text,
    parse_sensevoice_output,
    run_sensevoice_asr,
    DEFAULT_MODEL_NAME,
    DEFAULT_MODEL_VERSION,
)
import asr_sensevoice


class TestASRSenseVoice(unittest.TestCase):
    def test_clean_sensevoice_text(self):
        self.assertEqual(clean_sensevoice_text("<|zh|><|NEUTRAL|><|Speech|><|woitn|>你好世界"), "你好世界")
        self.assertEqual(clean_sensevoice_text("普通文本无标签"), "普通文本无标签")
        self.assertEqual(clean_sensevoice_text(""), "")
        self.assertEqual(clean_sensevoice_text("<|en|> Hello world <|HAPPY|>"), "Hello world")

    def test_parse_sensevoice_output_string(self):
        raw = "<|zh|><|NEUTRAL|>你好世界"
        parsed = parse_sensevoice_output(raw)
        self.assertEqual(len(parsed), 1)
        self.assertEqual(parsed[0]["text"], "你好世界")
        self.assertEqual(parsed[0]["language_code"], "zh")

    def test_parse_sensevoice_output_dict_with_words(self):
        raw = {
            "text": "<|zh|>今天天气不错",
            "start_ms": 100,
            "end_ms": 2100,
            "confidence": 0.98,
            "words": [
                {"word": "<|zh|>今天", "start_ms": 100, "end_ms": 500},
                {"word": "天气", "start_ms": 500, "end_ms": 1100},
                {"word": "不错", "start_ms": 1100, "end_ms": 2100},
            ]
        }
        parsed = parse_sensevoice_output(raw)
        self.assertEqual(len(parsed), 1)
        self.assertEqual(parsed[0]["text"], "今天天气不错")
        self.assertEqual(parsed[0]["start_ms"], 100)
        self.assertEqual(parsed[0]["end_ms"], 2100)
        self.assertEqual(len(parsed[0]["words"]), 3)
        self.assertEqual(parsed[0]["words"][0]["word"], "今天")

    def test_parse_sensevoice_output_list_segments(self):
        raw = [
            {"text": "第一句", "start_ms": 0, "end_ms": 1000, "confidence": 0.9},
            {"text": "第二句", "start_ms": 1000, "end_ms": 2000, "confidence": 0.95},
        ]
        parsed = parse_sensevoice_output(raw)
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["text"], "第一句")
        self.assertEqual(parsed[1]["text"], "第二句")

    def test_run_sensevoice_asr_with_mock_factory(self):
        def mock_factory(audio_path, model_name, model_version, **kwargs):
            return {"text": "<|zh|>测试成功", "start_ms": 0, "end_ms": 1500}

        orig_factory = asr_sensevoice._SENSEVOICE_MODEL_FACTORY
        try:
            asr_sensevoice._SENSEVOICE_MODEL_FACTORY = mock_factory
            resp = run_sensevoice_asr("dummy.wav", "sensevoice-small", "int8")
            self.assertEqual(resp["model_name"], "sensevoice-small")
            self.assertEqual(resp["model_version"], "int8")
            self.assertEqual(len(resp["segments"]), 1)
            self.assertEqual(resp["segments"][0]["text"], "测试成功")
        finally:
            asr_sensevoice._SENSEVOICE_MODEL_FACTORY = orig_factory

    def test_run_sensevoice_asr_fail_closed_snapshot(self):
        with self.assertRaises(RuntimeError) as ctx:
            run_sensevoice_asr("dummy.wav", "sensevoice-small", "int8", model_path="/nonexistent/path", require_model_snapshot=True)
        self.assertIn("WORKER_SNAPSHOT_PATH_REQUIRED", str(ctx.exception))

    def test_cli_execution_with_mock_factory(self):
        with tempfile.TemporaryDirectory() as td:
            audio_file = os.path.join(td, "test.wav")
            with open(audio_file, "wb") as f:
                f.write(b"RIFFdummyWAVEfmt ")

            script_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "asr_sensevoice.py")
            )

            req = {
                "audio_path": audio_file,
                "model_name": "sensevoice-small",
                "model_version": "int8",
                "require_model_snapshot": True,
                "model_path": "/nonexistent/model",
            }

            # Test fail-closed on snapshot requirement via CLI
            proc = subprocess.run(
                [sys.executable, script_path],
                input=json.dumps(req),
                text=True,
                capture_output=True,
            )
            self.assertNotEqual(proc.returncode, 0)
            self.assertIn("WORKER_SNAPSHOT_PATH_REQUIRED", proc.stderr)


if __name__ == "__main__":
    unittest.main()
