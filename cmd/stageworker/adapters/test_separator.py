#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/separator.py
Verifies audio separator adapter contracts and mock factory seams.
"""

import base64
import io
import json
import os
import struct
import sys
import unittest
import wave

# Ensure adapters directory is on path
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import separator


def _make_dummy_wav(duration_ms=2000, sample_rate=16000, channels=1):
    num_samples = int((sample_rate * duration_ms) / 1000)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(2)
        wf.setframerate(sample_rate)
        raw = struct.pack(f"<{num_samples * channels}h", *([100] * (num_samples * channels)))
        wf.writeframes(raw)
    return buf.getvalue()


class TestSeparatorAdapter(unittest.TestCase):
    def tearDown(self):
        separator._SEPARATOR_MODEL_FACTORY = None

    def test_mock_factory_separation(self):
        dummy_wav = _make_dummy_wav(duration_ms=3000)

        def fake_factory(audio_path, model_name, model_version):
            return {
                "vocals_data": dummy_wav,
                "background_data": dummy_wav,
                "duration_ms": 3000,
                "sample_rate": 16000,
                "channels": 1,
            }

        separator._SEPARATOR_MODEL_FACTORY = fake_factory

        res = separator.separate_audio_stems("dummy.wav", "UVR-MDX-NET-Inst_HQ_4.onnx", "v3")
        self.assertEqual(res["duration_ms"], 3000)
        self.assertEqual(res["sample_rate"], 16000)
        self.assertEqual(len(res["vocals_data"]), len(dummy_wav))
        self.assertEqual(len(res["background_data"]), len(dummy_wav))

    def test_demucs_codepath_execution_and_file_reading(self):
        """Exercises the actual separate_demucs implementation using a mock subprocess."""
        from unittest.mock import patch
        import tempfile
        import shutil

        dummy_wav = _make_dummy_wav(duration_ms=2500)

        # Mock subprocess.run to simulate demucs outputting vocals.wav and no_vocals.wav
        def fake_subprocess_run(cmd, stdout=None, stderr=None):
            # cmd is [sys.executable, "-m", "demucs.separate", "-n", model, "-o", out_dir, ...]
            out_dir = cmd[cmd.index("-o") + 1]
            model_name = cmd[cmd.index("-n") + 1]
            audio_path = cmd[-1]
            track_name = os.path.splitext(os.path.basename(audio_path))[0]
            model_out_dir = os.path.join(out_dir, model_name, track_name)
            os.makedirs(model_out_dir, exist_ok=True)
            with open(os.path.join(model_out_dir, "vocals.wav"), "wb") as vf:
                vf.write(dummy_wav)
            with open(os.path.join(model_out_dir, "no_vocals.wav"), "wb") as bf:
                bf.write(dummy_wav)

            class ProcResult:
                returncode = 0
                stderr = b""
                stdout = b""
            return ProcResult()

        with patch("subprocess.run", side_effect=fake_subprocess_run):
            # Calling separate_demucs directly exercises the actual Demucs file handling and validation logic
            res = separator.separate_demucs("sample_track.wav", "htdemucs", "v4")
            self.assertEqual(res["duration_ms"], 2500)
            self.assertEqual(res["sample_rate"], 16000)
            self.assertEqual(len(res["vocals_data"]), len(dummy_wav))
            self.assertEqual(len(res["background_data"]), len(dummy_wav))

    def test_lane_dispatch_demucs_vs_uvr(self):
        """Verifies separate_audio_stems routes Demucs models to Demucs lane and UVR models to UVR lane."""
        from unittest.mock import patch

        dummy_wav = _make_dummy_wav(duration_ms=1000)
        demucs_called = False
        uvr_called = False

        def fake_demucs(audio_path, model_name, model_version):
            nonlocal demucs_called
            demucs_called = True
            return {"vocals_data": dummy_wav, "background_data": dummy_wav, "duration_ms": 1000, "sample_rate": 16000, "channels": 1}

        def fake_uvr(audio_path, model_name, model_version):
            nonlocal uvr_called
            uvr_called = True
            return {"vocals_data": dummy_wav, "background_data": dummy_wav, "duration_ms": 1000, "sample_rate": 16000, "channels": 1}

        with patch.object(separator, "separate_demucs", side_effect=fake_demucs), \
             patch.object(separator, "separate_uvr", side_effect=fake_uvr):

            # 1. Demucs model dispatch
            res_demucs = separator.separate_audio_stems("track.wav", "htdemucs", "v4")
            self.assertTrue(demucs_called)
            self.assertFalse(uvr_called)

            # 2. UVR model dispatch
            demucs_called = False
            res_uvr = separator.separate_audio_stems("track.wav", "UVR-MDX-NET-Inst_HQ_4.onnx", "v3")
            self.assertFalse(demucs_called)
            self.assertTrue(uvr_called)

if __name__ == "__main__":
    unittest.main()
