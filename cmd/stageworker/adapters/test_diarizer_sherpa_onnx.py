#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/diarizer_sherpa_onnx.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

from diarizer_sherpa_onnx import (
    compute_cosine_similarity,
    run_diarization_sherpa,
    run_evidence_probe_sherpa,
    DEFAULT_MODEL_NAME,
    DEFAULT_MODEL_VERSION,
)
import diarizer_sherpa_onnx


class TestDiarizerSherpaONNX(unittest.TestCase):
    def test_compute_cosine_similarity(self):
        v1 = [1.0, 0.0, 0.0]
        v2 = [1.0, 0.0, 0.0]
        self.assertAlmostEqual(compute_cosine_similarity(v1, v2), 1.0)

        v3 = [0.0, 1.0, 0.0]
        self.assertAlmostEqual(compute_cosine_similarity(v1, v3), 0.0)

        v4 = [-1.0, 0.0, 0.0]
        self.assertAlmostEqual(compute_cosine_similarity(v1, v4), -1.0)

        self.assertEqual(compute_cosine_similarity([], []), 0.0)
        self.assertEqual(compute_cosine_similarity([1.0], [1.0, 2.0]), 0.0)

    def test_diarization_with_mock_factory(self):
        def mock_factory(**kwargs):
            return [
                {"speaker_id": "SPEAKER_00", "label": "SPEAKER_00", "start_ms": 0, "end_ms": 1200, "confidence": 0.0},
                {"speaker_id": "SPEAKER_01", "label": "SPEAKER_01", "start_ms": 1200, "end_ms": 2500, "confidence": 0.0},
            ]

        orig = diarizer_sherpa_onnx._DIARIZER_MODEL_FACTORY
        try:
            diarizer_sherpa_onnx._DIARIZER_MODEL_FACTORY = mock_factory
            res = run_diarization_sherpa(
                audio_path="dummy.wav",
                model_name="sherpa-onnx-campplus",
                model_version="onnx",
                vad_model_name="silero-vad",
                vad_model_version="v5",
            )
            self.assertEqual(len(res), 2)
            self.assertEqual(res[0]["speaker_id"], "SPEAKER_00")
            self.assertEqual(res[1]["start_ms"], 1200)
        finally:
            diarizer_sherpa_onnx._DIARIZER_MODEL_FACTORY = orig

    def test_evidence_probe_with_mock_factory(self):
        def mock_factory(**kwargs):
            return {
                "has_multi_speaker_cues": True,
                "speaker_change_count": 3,
                "confidence": 0.0,
                "source": "test-source",
            }

        orig = diarizer_sherpa_onnx._DIARIZER_MODEL_FACTORY
        try:
            diarizer_sherpa_onnx._DIARIZER_MODEL_FACTORY = mock_factory
            res = run_evidence_probe_sherpa(
                audio_path="dummy.wav",
                model_name="sherpa-onnx-campplus",
                model_version="onnx",
                vad_model_name="silero-vad",
                vad_model_version="v5",
                embedding_cosine_threshold=0.6,
            )
            self.assertTrue(res["has_multi_speaker_cues"])
            self.assertEqual(res["speaker_change_count"], 3)
            self.assertEqual(res["source"], "test-source")
        finally:
            diarizer_sherpa_onnx._DIARIZER_MODEL_FACTORY = orig

    def test_fail_closed_snapshot(self):
        with self.assertRaises(RuntimeError) as ctx:
            run_diarization_sherpa(
                audio_path="dummy.wav",
                model_name="sherpa-onnx-campplus",
                model_version="onnx",
                vad_model_name="silero-vad",
                vad_model_version="v5",
                model_path="/nonexistent/model.onnx",
                vad_model_path="/nonexistent/vad.onnx",
                require_model_snapshot=True,
            )
        self.assertIn("WORKER_SNAPSHOT_PATH_REQUIRED", str(ctx.exception))

    def test_cli_execution_evidence_mode_threshold_validation(self):
        script_path = os.path.abspath(
            os.path.join(os.path.dirname(__file__), "diarizer_sherpa_onnx.py")
        )

        with tempfile.TemporaryDirectory() as td:
            dummy_wav = os.path.join(td, "test.wav")
            with open(dummy_wav, "wb") as f:
                f.write(b"RIFFdummyWAVEfmt ")

            # Missing threshold
            req = {
                "audio_path": dummy_wav,
                "mode": "evidence",
            }
            proc = subprocess.run(
                [sys.executable, script_path],
                input=json.dumps(req),
                text=True,
                capture_output=True,
            )
            self.assertNotEqual(proc.returncode, 0)
            self.assertIn("embedding_cosine_threshold is required", proc.stderr)

            # Invalid threshold range (>1)
            req["embedding_cosine_threshold"] = 1.5
            proc2 = subprocess.run(
                [sys.executable, script_path],
                input=json.dumps(req),
                text=True,
                capture_output=True,
            )
            self.assertNotEqual(proc2.returncode, 0)
            self.assertIn("must be in (0, 1]", proc2.stderr)


if __name__ == "__main__":
    unittest.main()
