#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/aligner_qwen3.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

from aligner_qwen3 import (
    DEFAULT_ALIGNER_MODEL,
    parse_word_timings,
    resolve_aligner_identifier,
    run_aligner,
)


class TestAlignerQwen3(unittest.TestCase):
    def test_resolve_aligner_identifier(self):
        self.assertEqual(resolve_aligner_identifier("qwen3-aligner", "0.6b"), DEFAULT_ALIGNER_MODEL)
        self.assertEqual(resolve_aligner_identifier("Qwen3-ForcedAligner-0.6B", "0.6b"), DEFAULT_ALIGNER_MODEL)
        self.assertEqual(resolve_aligner_identifier("custom-aligner", "1.0"), "custom-aligner")

    def test_parse_word_timings_dict_format(self):
        raw = {
            "items": [
                {"word": "今天", "start_time": 0.0, "end_time": 0.3, "confidence": 0.95},
                {"word": "天气", "start_time": 0.3, "end_time": 0.8, "confidence": 0.92},
            ]
        }
        parsed = parse_word_timings(raw)
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["word"], "今天")
        self.assertEqual(parsed[0]["start_ms"], 0)
        self.assertEqual(parsed[0]["end_ms"], 300)
        self.assertEqual(parsed[1]["word"], "天气")
        self.assertEqual(parsed[1]["start_ms"], 300)
        self.assertEqual(parsed[1]["end_ms"], 800)

    def test_parse_word_timings_tuples_format(self):
        raw = [
            [0.0, 0.5, "你好"],
            [0.5, 1.2, "世界"],
        ]
        parsed = parse_word_timings(raw)
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["word"], "你好")
        self.assertEqual(parsed[0]["start_ms"], 0)
        self.assertEqual(parsed[0]["end_ms"], 500)
        self.assertEqual(parsed[1]["word"], "世界")
        self.assertEqual(parsed[1]["start_ms"], 500)
        self.assertEqual(parsed[1]["end_ms"], 1200)

    def test_cli_execution_with_mock_qwen_aligner(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            pkg_dir = os.path.join(tmpdir, "qwen_asr")
            os.makedirs(pkg_dir, exist_ok=True)
            with open(os.path.join(pkg_dir, "__init__.py"), "w", encoding="utf-8") as f:
                f.write("""
class Qwen3ForcedAligner:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    @classmethod
    def from_pretrained(cls, *args, **kwargs):
        return cls(*args, **kwargs)

    def align(self, audio, text, **kwargs):
        return [[
            {"text": "今天", "start_time": 0.0, "end_time": 0.3},
            {"text": "天气", "start_time": 0.3, "end_time": 0.8},
            {"text": "很好", "start_time": 0.8, "end_time": 1.2},
        ]]
""")

            audio_file = os.path.join(tmpdir, "test.wav")
            with open(audio_file, "wb") as f:
                f.write(b"fake wav header and pcm data")

            script_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "aligner_qwen3.py")
            )

            req = {
                "audio_path": audio_file,
                "text": "今天天气很好",
                "model_name": "Qwen3-ForcedAligner-0.6B",
                "model_version": "0.6b",
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
            self.assertIn("word_timings", out)
            self.assertEqual(len(out["word_timings"]), 3)
            self.assertEqual(out["word_timings"][0]["word"], "今天")
            self.assertEqual(out["word_timings"][0]["start_ms"], 0)
            self.assertEqual(out["word_timings"][0]["end_ms"], 300)

    def _install_fake_qwen_aligner(self, tmpdir):
        pkg_dir = os.path.join(tmpdir, "qwen_asr")
        os.makedirs(pkg_dir, exist_ok=True)
        with open(os.path.join(pkg_dir, "__init__.py"), "w", encoding="utf-8") as f:
            f.write(
                "class Qwen3ForcedAligner:\n"
                "    last_checkpoint = None\n"
                "\n"
                "    def __init__(self, *args, **kwargs):\n"
                "        pass\n"
                "\n"
                "    @classmethod\n"
                "    def from_pretrained(cls, checkpoint, **kwargs):\n"
                "        cls.last_checkpoint = checkpoint\n"
                "        return cls()\n"
                "\n"
                "    def align(self, audio=None, text=None, **kwargs):\n"
                '        return [{"word": "snap", "start_ms": 0, "end_ms": 300, "confidence": 0.9}]\n'
            )
        sys.path.insert(0, tmpdir)
        self.addCleanup(sys.path.remove, tmpdir)
        for mod in [m for m in list(sys.modules) if m == "qwen_asr" or m.startswith("qwen_asr.")]:
            del sys.modules[mod]

    def test_run_aligner_prefers_verified_snapshot_path(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            self._install_fake_qwen_aligner(tmpdir)
            snap = os.path.join(tmpdir, "snap")
            os.makedirs(snap)
            import qwen_asr
            out = run_aligner("whatever.wav", "快照语音", "Qwen3-ForcedAligner-0.6B", "0.6b", snap, True)
            self.assertEqual(qwen_asr.Qwen3ForcedAligner.last_checkpoint, snap)
            self.assertEqual(out["word_timings"][0]["word"], "snap")

    def test_run_aligner_strict_missing_snapshot_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            self._install_fake_qwen_aligner(tmpdir)
            with self.assertRaises(RuntimeError) as ctx:
                run_aligner("whatever.wav", "快照语音", "Qwen3-ForcedAligner-0.6B", "0.6b", os.path.join(tmpdir, "nope"), True)
            self.assertIn("WORKER_SNAPSHOT_PATH_REQUIRED", str(ctx.exception))

    def test_run_aligner_nonstrict_legacy_hub_fallback(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            self._install_fake_qwen_aligner(tmpdir)
            import qwen_asr
            out = run_aligner("whatever.wav", "快照语音", "Qwen3-ForcedAligner-0.6B", "0.6b")
            self.assertEqual(qwen_asr.Qwen3ForcedAligner.last_checkpoint, DEFAULT_ALIGNER_MODEL)
            self.assertEqual(out["word_timings"][0]["word"], "snap")

    def test_cli_strict_missing_snapshot_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            audio_file = os.path.join(tmpdir, "test.wav")
            with open(audio_file, "wb") as f:
                f.write(b"fake wav header and pcm data")
            script_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "aligner_qwen3.py")
            )
            req = {
                "audio_path": audio_file,
                "text": "快照语音",
                "model_name": "Qwen3-ForcedAligner-0.6B",
                "model_version": "0.6b",
                "model_path": os.path.join(tmpdir, "nope"),
                "require_model_snapshot": True,
            }
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
