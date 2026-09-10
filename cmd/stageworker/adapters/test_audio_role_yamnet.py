#!/usr/bin/env python3
import json
import os
import struct
import tempfile
import unittest
import wave
from unittest.mock import patch

import audio_role_yamnet


class MockYAMNetImpl:
    def __init__(self, scores_to_return):
        self.scores_to_return = scores_to_return
        self.call_count = 0

    def infer(self, window_15600):
        self.call_count += 1
        if callable(self.scores_to_return):
            return self.scores_to_return(window_15600)
        return list(self.scores_to_return)


class TestAudioRoleYAMNetAdapter(unittest.TestCase):
    def setUp(self):
        audio_role_yamnet._YAMNET_MODEL_FACTORY = None
        audio_role_yamnet._YAMNET_PROBE_FACTORY = None
        audio_role_yamnet._YAMNET_PROBE_VERSION_RESOLVER = None

    def tearDown(self):
        audio_role_yamnet._YAMNET_MODEL_FACTORY = None
        audio_role_yamnet._YAMNET_PROBE_FACTORY = None
        audio_role_yamnet._YAMNET_PROBE_VERSION_RESOLVER = None
    def _create_wav(self, duration_sec=2.0, amplitude=10000, sample_rate=16000, channels=1):
        fd, path = tempfile.mkstemp(suffix=".wav")
        os.close(fd)
        total_samples = int(duration_sec * sample_rate)
        with wave.open(path, "wb") as wf:
            wf.setnchannels(channels)
            wf.setsampwidth(2)
            wf.setframerate(sample_rate)
            # Alternate positive and negative amplitude
            data = []
            for i in range(total_samples * channels):
                val = amplitude if (i % 2 == 0) else -amplitude
                data.append(val)
            raw = struct.pack(f"<{len(data)}h", *data)
            wf.writeframes(raw)
        return path

    def test_probe_runtime_identity(self):
        audio_role_yamnet._YAMNET_PROBE_VERSION_RESOLVER = lambda: "2.2.0"
        res = audio_role_yamnet.probe_runtime_identity()
        self.assertEqual(res["status"], "ok")
        self.assertEqual(res["package_name"], "ai-edge-litert")
        self.assertEqual(res["package_version"], "2.2.0")
        self.assertEqual(res["runtime_versions"]["ai-edge-litert"], "2.2.0")
        self.assertEqual(res["adapter_revision"], "cmd/stageworker/adapters/audio_role_yamnet.py@v2.2.0")

    def test_probe_runtime_version_mismatch(self):
        audio_role_yamnet._YAMNET_PROBE_VERSION_RESOLVER = lambda: "2.1.0"
        with self.assertRaises(RuntimeError) as ctx:
            audio_role_yamnet.probe_runtime_identity()
        self.assertIn("ai-edge-litert runtime mismatch", str(ctx.exception))

    def test_probe_runtime_missing(self):
        audio_role_yamnet._YAMNET_PROBE_VERSION_RESOLVER = lambda: ""
        with self.assertRaises(RuntimeError) as ctx:
            audio_role_yamnet.probe_runtime_identity()
        self.assertIn("ai-edge-litert runtime mismatch", str(ctx.exception))
    def test_missing_audio_files_fails_closed(self):
        req = {
            "vocals_audio": "nonexistent_vocals.wav",
            "model_path": "some_dir",
        }
        audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl([0.0] * 521)
        with self.assertRaises(FileNotFoundError):
            audio_role_yamnet.run_audio_role_analysis(req)

    def test_corrupt_audio_fails_closed(self):
        fd, path = tempfile.mkstemp(suffix=".wav")
        os.write(fd, b"not a wav file")
        os.close(fd)
        try:
            req = {
                "vocals_audio": path,
                "model_path": "some_dir",
            }
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl([0.0] * 521)
            with self.assertRaises(ValueError):
                audio_role_yamnet.run_audio_role_analysis(req)
        finally:
            os.remove(path)

    def test_invalid_sample_rate_fails_closed(self):
        path = self._create_wav(duration_sec=1.0, sample_rate=44100)
        try:
            req = {
                "vocals_audio": path,
                "model_path": "some_dir",
            }
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl([0.0] * 521)
            with self.assertRaises(ValueError) as ctx:
                audio_role_yamnet.run_audio_role_analysis(req)
            self.assertIn("expected 16000 Hz", str(ctx.exception))
        finally:
            os.remove(path)

    def test_classify_dialogue(self):
        v_path = self._create_wav(duration_sec=1.5, amplitude=15000)
        try:
            scores = [0.0] * 521
            scores[0] = 0.85  # Speech
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "narration/dialogue")
        finally:
            os.remove(v_path)

    def test_classify_singing(self):
        v_path = self._create_wav(duration_sec=1.5, amplitude=15000)
        try:
            scores = [0.0] * 521
            scores[24] = 0.88  # Singing
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "singing/music-vocal")
        finally:
            os.remove(v_path)

    def test_conflicting_vocal_evidence_maps_to_uncertain(self):
        v_path = self._create_wav(duration_sec=1.5, amplitude=15000)
        try:
            scores = [0.0] * 521
            scores[0] = 0.52  # Speech
            scores[24] = 0.50  # Singing (diff < 0.08 margin)
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "uncertain")
        finally:
            os.remove(v_path)

    def test_low_confidence_vocal_maps_to_uncertain(self):
        v_path = self._create_wav(duration_sec=1.5, amplitude=15000)
        try:
            scores = [0.0] * 521
            scores[0] = 0.12  # Below min_confidence 0.20
            scores[24] = 0.05
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "uncertain")
        finally:
            os.remove(v_path)

    def test_classify_instrumental(self):
        v_path = self._create_wav(duration_sec=1.5, amplitude=100)  # Near silent vocals
        b_path = self._create_wav(duration_sec=1.5, amplitude=12000)  # Strong background
        try:
            scores = [0.0] * 521
            scores[132] = 0.75  # Music
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "background_audio": b_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "instrumental/background")
        finally:
            os.remove(v_path)
            os.remove(b_path)

    def test_classify_ambience_sfx(self):
        v_path = self._create_wav(duration_sec=1.5, amplitude=100)  # Near silent vocals
        b_path = self._create_wav(duration_sec=1.5, amplitude=12000)  # Strong background
        try:
            scores = [0.0] * 521
            scores[283] = 0.80  # Rain
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "background_audio": b_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "ambience/SFX")
        finally:
            os.remove(v_path)
            os.remove(b_path)

    def test_model_snapshot_resolution_enforcement(self):
        with self.assertRaises(RuntimeError) as ctx:
            audio_role_yamnet.resolve_model_path("")
        self.assertIn("model_path is required", str(ctx.exception))

        with self.assertRaises(RuntimeError) as ctx:
            audio_role_yamnet.resolve_model_path("nonexistent_dir_123")
        self.assertIn("does not exist", str(ctx.exception))

    def test_low_confidence_quiet_background_maps_to_uncertain(self):
        v_path = self._create_wav(duration_sec=1.5, amplitude=100)
        b_path = self._create_wav(duration_sec=1.5, amplitude=100)
        try:
            # Both instrumental (132) and SFX (100) below safe threshold (e.g. 0.05)
            scores = [0.0] * 521
            scores[132] = 0.05
            scores[100] = 0.04
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "background_audio": b_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "uncertain",
                             "low-confidence background must not default to instrumental/background")
        finally:
            os.remove(v_path)
            os.remove(b_path)

    def test_empty_wav_fails_closed_with_error(self):
        fd, path = tempfile.mkstemp(suffix=".wav")
        with wave.open(path, "wb") as wf:
            wf.setnchannels(1)
            wf.setsampwidth(2)
            wf.setframerate(16000)
            # 0 frames
        audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl([0.0] * 521)
        try:
            with self.assertRaises(ValueError) as ctx:
                audio_role_yamnet.run_audio_role_analysis({
                    "source_audio": path,
                    "model_path": "fake_root",
                })
            self.assertIn("empty", str(ctx.exception).lower())
        finally:
            os.close(fd)
            os.remove(path)

    def test_conflicting_background_maps_to_uncertain(self):
        v_path = self._create_wav(duration_sec=1.5, amplitude=100)
        b_path = self._create_wav(duration_sec=1.5, amplitude=15000)
        try:
            # Both instrumental and SFX are strong and near-identical (conflicting evidence)
            scores = [0.0] * 521
            scores[132] = 0.40  # instrumental
            scores[100] = 0.39  # SFX (diff 0.01 < margin 0.08)
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "background_audio": b_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "uncertain")
        finally:
            os.remove(v_path)
            os.remove(b_path)

    def test_source_only_low_confidence_maps_to_uncertain(self):
        s_path = self._create_wav(duration_sec=1.5, amplitude=1000)
        try:
            # Unseparated source has low confidence across all roles
            scores = [0.0] * 521
            scores[0] = 0.10  # low speech
            scores[132] = 0.12  # low music
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "source_audio": s_path,
                "model_path": "fake_root",
            })
            self.assertEqual(len(res["segments"]), 1)
            self.assertEqual(res["segments"][0]["role"], "uncertain",
                             "source-only fallback must be conservative and not infer confident no-dub")
        finally:
            os.remove(s_path)

    def test_output_contract_provider_id_and_aligned_duration(self):
        v_path = self._create_wav(duration_sec=1.0, amplitude=1000)
        b_path = self._create_wav(duration_sec=2.5, amplitude=1000)
        try:
            scores = [0.0] * 521
            scores[0] = 0.90
            audio_role_yamnet._YAMNET_MODEL_FACTORY = lambda p: MockYAMNetImpl(scores)

            res = audio_role_yamnet.run_audio_role_analysis({
                "vocals_audio": v_path,
                "background_audio": b_path,
                "model_path": "fake_root",
            })
            self.assertEqual(res["provider_id"], "worker_yamnet")
            self.assertTrue(len(res["segments"]) > 0)
            last_end = res["segments"][-1]["end_ms"]
            self.assertEqual(last_end, 2500, "duration must derive from aligned sample lengths, not short stem")
        finally:
            os.remove(v_path)
            os.remove(b_path)


if __name__ == "__main__":
    unittest.main()
