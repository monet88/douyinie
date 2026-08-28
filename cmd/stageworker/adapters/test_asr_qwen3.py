#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/asr_qwen3.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

from asr_qwen3 import (
    DEFAULT_1_7B_MODEL,
    DEFAULT_0_6B_MODEL,
    parse_asr_segments,
    resolve_model_identifier,
)


class TestASRQwen3(unittest.TestCase):
    def test_resolve_model_identifier(self):
        self.assertEqual(resolve_model_identifier("qwen3-asr", "1.7b"), DEFAULT_1_7B_MODEL)
        self.assertEqual(resolve_model_identifier("qwen3-asr", "0.6b"), DEFAULT_0_6B_MODEL)
        self.assertEqual(resolve_model_identifier("custom-asr-model", "1.0"), "custom-asr-model")

    def test_parse_asr_segments_dict_format(self):
        raw = {
            "segments": [
                {"start_ms": 0, "end_ms": 1200, "text": "你好世界", "confidence": 0.95, "language_code": "zh"},
                {"start_time": 1.2, "end_time": 2.5, "text": "再见", "confidence": 0.9},
            ]
        }
        parsed = parse_asr_segments(raw)
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["text"], "你好世界")
        self.assertEqual(parsed[0]["start_ms"], 0)
        self.assertEqual(parsed[0]["end_ms"], 1200)
        self.assertEqual(parsed[1]["text"], "再见")
        self.assertEqual(parsed[1]["start_ms"], 1200)
        self.assertEqual(parsed[1]["end_ms"], 2500)

    def test_parse_asr_segments_timestamps_format(self):
        raw = {
            "time_stamps": [
                {"start_time": 0.0, "end_time": 0.5, "text": "你"},
                {"start_time": 0.5, "end_time": 1.0, "text": "好"},
            ]
        }
        parsed = parse_asr_segments(raw)
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["start_ms"], 0)
        self.assertEqual(parsed[0]["end_ms"], 500)
        self.assertEqual(parsed[1]["start_ms"], 500)
        self.assertEqual(parsed[1]["end_ms"], 1000)

    def test_parse_asr_segments_plain_text_fallback(self):
        raw = {"text": "测试文本"}
        parsed = parse_asr_segments(raw)
        self.assertEqual(len(parsed), 1)
        self.assertEqual(parsed[0]["text"], "测试文本")
        self.assertEqual(parsed[0]["start_ms"], 0)

    def test_cli_execution_with_mock_qwen_asr(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            pkg_dir = os.path.join(tmpdir, "qwen_asr")
            os.makedirs(pkg_dir, exist_ok=True)
            with open(os.path.join(pkg_dir, "__init__.py"), "w", encoding="utf-8") as f:
                f.write("""
class ASRTranscription:
    def __init__(self):
        self.text = "今天天气很好。我们去散步。"
        self.language = "Chinese"
        self.time_stamps = None

class Qwen3ASRModel:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    @classmethod
    def from_pretrained(cls, *args, **kwargs):
        return cls(*args, **kwargs)

    def transcribe(self, audio, **kwargs):
        if kwargs.get("return_time_stamps") is not False:
            raise ValueError("ASR stage must not request forced-alignment timestamps")
        return [ASRTranscription()]
""")

            audio_file = os.path.join(tmpdir, "test.wav")
            with open(audio_file, "wb") as f:
                f.write(b"fake wav header and pcm data")

            script_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "asr_qwen3.py")
            )

            req = {
                "audio_path": audio_file,
                "model_name": "qwen3-asr",
                "model_version": "1.7b",
            }

            env = os.environ.copy()
            env["PYTHONPATH"] = tmpdir + os.pathsep + env.get("PYTHONPATH", "")

            proc = subprocess.run(
                [sys.executable, script_path],
                input=json.dumps(req),
                text=True,
                capture_output=True,
                env=env,
            )

            self.assertEqual(proc.returncode, 0, f"Process failed: {proc.stderr}")
            out = json.loads(proc.stdout)
            self.assertIn("segments", out)
            self.assertEqual(len(out["segments"]), 1)
            self.assertEqual(out["segments"][0]["text"], "今天天气很好。我们去散步。")


if __name__ == "__main__":
    unittest.main()
