#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/diarizer_3dspeaker.py
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

from diarizer_3dspeaker import (
    DEFAULT_MODEL_NAME,
    DEFAULT_MODEL_VERSION,
    DEFAULT_VAD_MODEL,
    DEFAULT_VAD_VERSION,
    compute_cosine_similarity,
    parse_speaker_segments,
)


class TestDiarizer3DSpeaker(unittest.TestCase):
    def test_cosine_similarity(self):
        v1 = [1.0, 0.0, 0.0]
        v2 = [1.0, 0.0, 0.0]
        self.assertAlmostEqual(compute_cosine_similarity(v1, v2), 1.0)

        v3 = [0.0, 1.0, 0.0]
        self.assertAlmostEqual(compute_cosine_similarity(v1, v3), 0.0)

    def test_parse_speaker_segments_list_of_tuples(self):
        raw = [
            [0.0, 1.5, "speaker0"],
            [1.5, 3.2, "speaker1"],
        ]
        parsed = parse_speaker_segments(raw)
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["speaker_id"], "speaker0")
        self.assertEqual(parsed[0]["start_ms"], 0)
        self.assertEqual(parsed[0]["end_ms"], 1500)
        self.assertEqual(parsed[0]["confidence"], 0.0)
        self.assertEqual(parsed[1]["speaker_id"], "speaker1")
        self.assertEqual(parsed[1]["start_ms"], 1500)
        self.assertEqual(parsed[1]["end_ms"], 3200)

    def test_parse_speaker_segments_list_of_dicts(self):
        raw = [
            {"start": 0.5, "end": 2.0, "speaker": "SPEAKER_00"},
            {"start": 2.2, "end": 4.0, "speaker": "SPEAKER_01"},
        ]
        parsed = parse_speaker_segments(raw)
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["start_ms"], 500)
        self.assertEqual(parsed[0]["end_ms"], 2000)
        self.assertEqual(parsed[0]["speaker_id"], "SPEAKER_00")

    def test_cli_execution_with_mock_speakerlab(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Create mock speakerlab package
            pkg_dir = os.path.join(tmpdir, "speakerlab", "bin")
            os.makedirs(pkg_dir, exist_ok=True)
            with open(os.path.join(tmpdir, "speakerlab", "__init__.py"), "w", encoding="utf-8") as f:
                f.write("")
            with open(os.path.join(tmpdir, "speakerlab", "bin", "__init__.py"), "w", encoding="utf-8") as f:
                f.write("")

            mock_code = """
class Diarization3Dspeaker:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    def __call__(self, audio_path):
        return [
            [0.0, 1.5, "SPEAKER_00"],
            [1.5, 3.2, "SPEAKER_01"],
        ]

    def probe_evidence(self, audio_path):
        return {
            "has_multi_speaker_cues": True,
            "speaker_change_count": 2,
            "confidence": 0.0,
            "source": "mock_probe",
        }
"""
            with open(os.path.join(pkg_dir, "infer_diarization.py"), "w", encoding="utf-8") as f:
                f.write(mock_code)

            # Create dummy audio file
            audio_path = os.path.join(tmpdir, "test.wav")
            with open(audio_path, "wb") as f:
                f.write(b"RIFFdummyWAVEfmt ")

            adapter_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "diarizer_3dspeaker.py")
            )
            env = dict(os.environ)
            env["PYTHONPATH"] = tmpdir + os.pathsep + env.get("PYTHONPATH", "")

            # 1. Test Diarize Mode
            req_diarize = {
                "mode": "diarize",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
                "embedding_cosine_threshold": 0.65,
            }
            proc = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req_diarize).encode("utf-8"),
                capture_output=True,
                env=env,
                check=True,
            )
            out_diarize = json.loads(proc.stdout.decode("utf-8"))
            self.assertIn("speaker_assignments", out_diarize)
            self.assertEqual(len(out_diarize["speaker_assignments"]), 2)
            self.assertEqual(out_diarize["model_name"], DEFAULT_MODEL_NAME)
            self.assertEqual(out_diarize["vad_model_name"], DEFAULT_VAD_MODEL)

            # 2. Test Evidence Mode
            req_evidence = {
                "mode": "evidence",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
                "embedding_cosine_threshold": 0.65,
            }
            proc = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req_evidence).encode("utf-8"),
                capture_output=True,
                env=env,
                check=True,
            )
            out_evidence = json.loads(proc.stdout.decode("utf-8"))
            self.assertIn("speaker_evidence", out_evidence)
            self.assertTrue(out_evidence["speaker_evidence"]["has_multi_speaker_cues"])
            self.assertEqual(out_evidence["speaker_evidence"]["speaker_change_count"], 2)
            self.assertEqual(out_evidence["speaker_evidence"]["confidence"], 0.0)


    def test_cli_execution_with_real_upstream_shaped_speakerlab(self):
        """
        Regression for BLOCKER 1: Diarization3Dspeaker with NO convenience probe_evidence/extract_embeddings/vad methods,
        exposing only real upstream methods: load_audio, do_vad, chunk, do_emb_extraction, do_clustering, __call__.
        Proves bounded pre-clustering acoustic speaker evidence works over the real upstream surface.
        """
        with tempfile.TemporaryDirectory() as tmpdir:
            pkg_dir = os.path.join(tmpdir, "speakerlab", "bin")
            os.makedirs(pkg_dir, exist_ok=True)
            with open(os.path.join(tmpdir, "speakerlab", "__init__.py"), "w", encoding="utf-8") as f:
                f.write("")
            with open(os.path.join(tmpdir, "speakerlab", "bin", "__init__.py"), "w", encoding="utf-8") as f:
                f.write("")

            # Upstream-shaped fake class: NO probe_evidence, NO extract_embeddings, NO vad convenience methods!
            mock_code = """
def load_audio(audio_path):
    return "fake_wav_tensor"

class Diarization3Dspeaker:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs

    def do_vad(self, wav):
        # Return 2 speech turns
        return [[0.0, 1.5], [1.8, 3.2]]

    def chunk(self, st, ed):
        return [f"chunk_{st}_{ed}"]

    def do_emb_extraction(self, chunks, wav):
        # Distinct speaker embeddings: cosine similarity < 0.65
        return [
            [1.0, 0.0, 0.0],
            [0.0, 1.0, 0.0],
        ]

    def do_clustering(self, chunks, embeddings, speaker_num):
        # Not called in bounded evidence mode
        return [0, 1]

    def __call__(self, audio_path):
        return [
            [0.0, 1.5, "SPEAKER_00"],
            [1.8, 3.2, "SPEAKER_01"],
        ]
"""
            with open(os.path.join(pkg_dir, "infer_diarization.py"), "w", encoding="utf-8") as f:
                f.write(mock_code)

            audio_path = os.path.join(tmpdir, "test.wav")
            with open(audio_path, "wb") as f:
                f.write(b"RIFFdummyWAVEfmt ")

            adapter_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "diarizer_3dspeaker.py")
            )
            env = dict(os.environ)
            env["PYTHONPATH"] = tmpdir + os.pathsep + env.get("PYTHONPATH", "")

            # 1. Test Diarize Mode with upstream-shaped fake
            req_diarize = {
                "mode": "diarize",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
            }
            proc = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req_diarize).encode("utf-8"),
                capture_output=True,
                env=env,
                check=True,
            )
            out_diarize = json.loads(proc.stdout.decode("utf-8"))
            self.assertIn("speaker_assignments", out_diarize)
            self.assertEqual(len(out_diarize["speaker_assignments"]), 2)

            # 2. Test Evidence Mode with upstream-shaped fake (bounded pre-clustering acoustic evidence)
            req_evidence = {
                "mode": "evidence",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
                "embedding_cosine_threshold": 0.65,
            }
            proc = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req_evidence).encode("utf-8"),
                capture_output=True,
                env=env,
                check=True,
            )
            out_evidence = json.loads(proc.stdout.decode("utf-8"))
            self.assertIn("speaker_evidence", out_evidence)
            self.assertTrue(out_evidence["speaker_evidence"]["has_multi_speaker_cues"])
            self.assertEqual(out_evidence["speaker_evidence"]["speaker_change_count"], 1)
            self.assertEqual(out_evidence["speaker_evidence"]["confidence"], 0.0)

    def test_snapshot_model_cache_dir_resolution(self):
        """
        Verify that snapshot paths resolving to ROOT/<model_id>/configuration.json pass model_cache_dir.
        """
        with tempfile.TemporaryDirectory() as tmpdir:
            pkg_dir = os.path.join(tmpdir, "speakerlab", "bin")
            os.makedirs(pkg_dir, exist_ok=True)
            with open(os.path.join(tmpdir, "speakerlab", "__init__.py"), "w", encoding="utf-8") as f:
                f.write("")
            with open(os.path.join(tmpdir, "speakerlab", "bin", "__init__.py"), "w", encoding="utf-8") as f:
                f.write("")

            mock_code = """
class Diarization3Dspeaker:
    def __init__(self, *args, **kwargs):
        self.kwargs = kwargs
        # Assert model_cache_dir was provided
        if "model_cache_dir" not in kwargs or not kwargs["model_cache_dir"]:
            raise ValueError("model_cache_dir missing in constructor")

    def __call__(self, audio_path):
        return [[0.0, 1.5, "SPEAKER_00"], [1.5, 3.2, "SPEAKER_01"]]
"""
            with open(os.path.join(pkg_dir, "infer_diarization.py"), "w", encoding="utf-8") as f:
                f.write(mock_code)

            cache_root = os.path.join(tmpdir, "cache_root")
            campplus_dir = os.path.join(cache_root, "iic", "speech_campplus_sv_zh_en_16k-common_advanced")
            vad_dir = os.path.join(cache_root, "iic", "speech_fsmn_vad_zh-cn-16k-common-pytorch")
            os.makedirs(campplus_dir, exist_ok=True)
            os.makedirs(vad_dir, exist_ok=True)

            with open(os.path.join(campplus_dir, "configuration.json"), "w", encoding="utf-8") as f:
                f.write("{}")
            with open(os.path.join(vad_dir, "configuration.json"), "w", encoding="utf-8") as f:
                f.write("{}")

            audio_path = os.path.join(tmpdir, "test.wav")
            with open(audio_path, "wb") as f:
                f.write(b"RIFFdummyWAVEfmt ")

            adapter_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "diarizer_3dspeaker.py")
            )
            env = dict(os.environ)
            env["PYTHONPATH"] = tmpdir + os.pathsep + env.get("PYTHONPATH", "")

            req = {
                "mode": "diarize",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
                "model_path": campplus_dir,
                "vad_model_path": vad_dir,
                "require_model_snapshot": True,
            }
            proc = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req).encode("utf-8"),
                capture_output=True,
                env=env,
                check=True,
            )
            out = json.loads(proc.stdout.decode("utf-8"))
            self.assertIn("speaker_assignments", out)
            self.assertEqual(len(out["speaker_assignments"]), 2)

            # Test invalid cache root fails closed
            bad_req = dict(req)
            bad_req["vad_model_path"] = os.path.join(tmpdir, "other_dir")
            proc_bad = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(bad_req).encode("utf-8"),
                capture_output=True,
                env=env,
            )
            self.assertNotEqual(proc_bad.returncode, 0)
            self.assertIn(b"WORKER_SNAPSHOT_PATH_REQUIRED", proc_bad.stderr)
    def test_strict_mode_rejects_constructor_type_error_without_fallback(self):
        """
        Verify that when Diarization3Dspeaker rejects model_cache_dir with TypeError:
        - require_model_snapshot=True fails closed with WORKER_SNAPSHOT_PATH_REQUIRED (no fallback to unbound cache).
        - require_model_snapshot=False allows fallback to DiarizationClass() for non-strict mock testing.
        """
        with tempfile.TemporaryDirectory() as tmpdir:
            pkg_dir = os.path.join(tmpdir, "speakerlab", "bin")
            os.makedirs(pkg_dir, exist_ok=True)
            with open(os.path.join(tmpdir, "speakerlab", "__init__.py"), "w", encoding="utf-8") as f:
                f.write("")
            with open(os.path.join(tmpdir, "speakerlab", "bin", "__init__.py"), "w", encoding="utf-8") as f:
                f.write("")

            # Class that accepts NO kwargs (raises TypeError if model_cache_dir is passed)
            mock_code = """
class Diarization3Dspeaker:
    def __init__(self):
        pass

    def __call__(self, audio_path):
        return [[0.0, 1.5, "SPEAKER_00"], [1.5, 3.2, "SPEAKER_01"]]

    def probe_evidence(self, audio_path):
        return {"has_multi_speaker_cues": True, "speaker_change_count": 2, "confidence": 0.0}
"""
            with open(os.path.join(pkg_dir, "infer_diarization.py"), "w", encoding="utf-8") as f:
                f.write(mock_code)

            cache_root = os.path.join(tmpdir, "cache_root")
            campplus_dir = os.path.join(cache_root, "iic", "speech_campplus_sv_zh_en_16k-common_advanced")
            vad_dir = os.path.join(cache_root, "iic", "speech_fsmn_vad_zh-cn-16k-common-pytorch")
            os.makedirs(campplus_dir, exist_ok=True)
            os.makedirs(vad_dir, exist_ok=True)

            with open(os.path.join(campplus_dir, "configuration.json"), "w", encoding="utf-8") as f:
                f.write("{}")
            with open(os.path.join(vad_dir, "configuration.json"), "w", encoding="utf-8") as f:
                f.write("{}")

            audio_path = os.path.join(tmpdir, "test.wav")
            with open(audio_path, "wb") as f:
                f.write(b"RIFFdummyWAVEfmt ")

            adapter_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "diarizer_3dspeaker.py")
            )
            env = dict(os.environ)
            env["PYTHONPATH"] = tmpdir + os.pathsep + env.get("PYTHONPATH", "")

            # 1. Strict mode (diarize): MUST fail closed, no silent fallback to unbound DiarizationClass()
            req_strict_diarize = {
                "mode": "diarize",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
                "model_path": campplus_dir,
                "vad_model_path": vad_dir,
                "require_model_snapshot": True,
            }
            proc_sd = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req_strict_diarize).encode("utf-8"),
                capture_output=True,
                env=env,
            )
            self.assertNotEqual(proc_sd.returncode, 0)
            self.assertIn(b"WORKER_SNAPSHOT_PATH_REQUIRED", proc_sd.stderr)
            self.assertIn(b"does not accept verified model_cache_dir", proc_sd.stderr)

            # 2. Strict mode (evidence): MUST fail closed, no silent fallback
            req_strict_evidence = {
                "mode": "evidence",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
                "model_path": campplus_dir,
                "vad_model_path": vad_dir,
                "embedding_cosine_threshold": 0.65,
                "require_model_snapshot": True,
            }
            proc_se = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req_strict_evidence).encode("utf-8"),
                capture_output=True,
                env=env,
            )
            self.assertNotEqual(proc_se.returncode, 0)
            self.assertIn(b"WORKER_SNAPSHOT_PATH_REQUIRED", proc_se.stderr)
            self.assertIn(b"does not accept verified model_cache_dir", proc_se.stderr)

            # 3. Non-strict mode (diarize): allows fallback to DiarizationClass()
            req_nonstrict_diarize = {
                "mode": "diarize",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
                "require_model_snapshot": False,
            }
            proc_nd = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req_nonstrict_diarize).encode("utf-8"),
                capture_output=True,
                env=env,
                check=True,
            )
            out_nd = json.loads(proc_nd.stdout.decode("utf-8"))
            self.assertIn("speaker_assignments", out_nd)

            # 4. Non-strict mode (evidence): allows fallback to DiarizationClass()
            req_nonstrict_evidence = {
                "mode": "evidence",
                "audio_path": audio_path,
                "model_name": DEFAULT_MODEL_NAME,
                "model_version": DEFAULT_MODEL_VERSION,
                "vad_model_name": DEFAULT_VAD_MODEL,
                "vad_model_version": DEFAULT_VAD_VERSION,
                "embedding_cosine_threshold": 0.65,
                "require_model_snapshot": False,
            }
            proc_ne = subprocess.run(
                [sys.executable, adapter_path],
                input=json.dumps(req_nonstrict_evidence).encode("utf-8"),
                capture_output=True,
                env=env,
                check=True,
            )
            out_ne = json.loads(proc_ne.stdout.decode("utf-8"))
            self.assertIn("speaker_evidence", out_ne)

if __name__ == "__main__":
    unittest.main()
