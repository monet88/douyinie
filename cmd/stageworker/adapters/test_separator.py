#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/separator.py
Verifies audio separator adapter contracts, mock factory seams,
fail-closed snapshot validation for UVR and Demucs, and offline execution.
"""

import base64
import io
import json
import os
import struct
import subprocess
import sys
import tempfile
import unittest
import wave
from unittest import mock

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
        separator._SEPARATOR_PROBE_FACTORY = None
    def test_mock_factory_separation(self):
        dummy_wav = _make_dummy_wav(duration_ms=3000)

        def mock_factory(path, model, ver, **kwargs):
            return {
                "vocals_data": dummy_wav,
                "background_data": dummy_wav,
                "duration_ms": 3000,
                "sample_rate": 16000,
                "channels": 1,
            }

        separator._SEPARATOR_MODEL_FACTORY = mock_factory
        res = separator.separate_audio_stems("dummy.wav", "UVR-MDX-NET-Inst_HQ_4.onnx", "v3")
        self.assertEqual(res["duration_ms"], 3000)
        self.assertEqual(len(res["vocals_data"]), len(dummy_wav))
        self.assertEqual(len(res["background_data"]), len(dummy_wav))

    def test_demucs_codepath_execution_and_file_reading(self):
        """Exercises the actual separate_demucs implementation using a mock subprocess."""
        dummy_wav = _make_dummy_wav(duration_ms=2000)

        def fake_subprocess_run(cmd, stdout=None, stderr=None):
            out_dir = cmd[cmd.index("-o") + 1]
            model_name = cmd[cmd.index("-n") + 1]
            audio_file = cmd[-1]
            track_name = os.path.splitext(os.path.basename(audio_file))[0]
            target_dir = os.path.join(out_dir, model_name, track_name)
            os.makedirs(target_dir, exist_ok=True)
            with open(os.path.join(target_dir, "vocals.wav"), "wb") as f:
                f.write(dummy_wav)
            with open(os.path.join(target_dir, "no_vocals.wav"), "wb") as f:
                f.write(dummy_wav)

            mock_proc = mock.MagicMock()
            mock_proc.returncode = 0
            mock_proc.stdout = b""
            mock_proc.stderr = b""
            return mock_proc

        with mock.patch("subprocess.run", side_effect=fake_subprocess_run):
            res = separator.separate_demucs("sample_audio.wav", "htdemucs", "v4")
            self.assertEqual(res["duration_ms"], 2000)
            self.assertEqual(res["sample_rate"], 16000)
            self.assertEqual(res["channels"], 1)
            self.assertEqual(len(res["vocals_data"]), len(dummy_wav))
            self.assertEqual(len(res["background_data"]), len(dummy_wav))

    def test_demucs_native_rate_stems_leave_the_adapter_at_the_pipeline_contract(self):
        """Demucs always writes its model's native 44.1 kHz stereo, but the pipeline contract - and
        the audio-role analyzer that consumes these stems - is 16 kHz mono. A native-rate stem
        dead-ends audio_role_plan, the first stage of every real run, while the persisted metadata
        claims the contract rate; stems leaving the adapter must really be at the contract rate."""
        native_wav = _make_dummy_wav(duration_ms=2000, sample_rate=44100, channels=2)
        converted_wav = _make_dummy_wav(duration_ms=2000, sample_rate=16000, channels=1)
        calls = []
        ffmpeg_commands = []

        def fake_subprocess_run(cmd, *args, **kwargs):
            if "-m" in cmd and "demucs.separate" in cmd:
                calls.append("demucs")
                out_dir = cmd[cmd.index("-o") + 1]
                model_name = cmd[cmd.index("-n") + 1]
                track_name = os.path.splitext(os.path.basename(cmd[-1]))[0]
                target_dir = os.path.join(out_dir, model_name, track_name)
                os.makedirs(target_dir, exist_ok=True)
                for name in ["vocals.wav", "no_vocals.wav"]:
                    with open(os.path.join(target_dir, name), "wb") as f:
                        f.write(native_wav)
                proc = mock.MagicMock()
                proc.returncode = 0
                proc.stdout = b""
                proc.stderr = b""
                return proc
            if cmd and cmd[0] == "ffmpeg":
                calls.append("ffmpeg")
                ffmpeg_commands.append(list(cmd))
                out_path = cmd[-1]
                with open(out_path, "wb") as f:
                    f.write(converted_wav)
                proc = mock.MagicMock()
                proc.returncode = 0
                proc.stdout = b""
                proc.stderr = b""
                return proc
            raise AssertionError(f"unexpected subprocess command: {cmd}")

        with mock.patch("subprocess.run", side_effect=fake_subprocess_run):
            res = separator.separate_audio_stems("sample_audio.wav", "htdemucs", "v4")

        self.assertEqual(calls[0], "demucs")
        self.assertIn("ffmpeg", calls)
        self.assertEqual(len(ffmpeg_commands), 2, "ffmpeg must be invoked for both vocals and background stems")
        for cmd in ffmpeg_commands:
            self.assertEqual(cmd[:-1], [
                "ffmpeg", "-v", "error", "-y", "-i", "pipe:0",
                "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le",
            ], "ffmpeg conversion argv must strictly match the pipeline contract")
        self.assertEqual(res["sample_rate"], 16000)
        self.assertEqual(res["channels"], 1)
        for key in ["vocals_data", "background_data"]:
            rate, channels, dur_ms = separator.measure_wav_properties(res[key])
            self.assertEqual(rate, 16000, f"{key} must really be at the contract rate")
            self.assertEqual(channels, 1, f"{key} must be mono")
            self.assertGreater(dur_ms, 1900, f"{key} header must declare its real length")
            self.assertLess(dur_ms, 2100, f"{key} header must declare its real length")

    def test_contract_rate_stems_are_not_re_encoded(self):
        """Stems already at the contract rate are passed through byte-identical, so their hashes keep
        matching the bytes the separator actually produced."""
        contract_wav = _make_dummy_wav(duration_ms=500, sample_rate=16000, channels=1)
        self.assertEqual(separator.normalize_stem_to_contract(contract_wav), contract_wav)

    def test_contract_rate_stems_non_16bit_are_re_encoded(self):
        """A 16 kHz mono stem that is not 16-bit PCM must be re-encoded to 16-bit PCM."""
        expected_16bit = _make_dummy_wav(duration_ms=500, sample_rate=16000, channels=1)
        # Re-pack with 24-bit width (3 bytes per sample)
        buf = io.BytesIO()
        with wave.open(buf, "wb") as wf:
            wf.setnchannels(1)
            wf.setsampwidth(3)
            wf.setframerate(16000)
            wf.writeframes(b"".join(struct.pack("<i", 1000)[:3] for _ in range(8000)))
        input_24bit = buf.getvalue()

        ffmpeg_invoked = False
        def fake_ffmpeg(cmd, *args, **kwargs):
            nonlocal ffmpeg_invoked
            self.assertEqual(cmd[0], "ffmpeg")
            self.assertEqual(cmd[:-1], [
                "ffmpeg", "-v", "error", "-y", "-i", "pipe:0",
                "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le",
            ])
            ffmpeg_invoked = True
            out_path = cmd[-1]
            with open(out_path, "wb") as f:
                f.write(expected_16bit)
            proc = mock.MagicMock()
            proc.returncode = 0
            proc.stdout = b""
            proc.stderr = b""
            return proc

        with mock.patch("subprocess.run", side_effect=fake_ffmpeg):
            res = separator.normalize_stem_to_contract(input_24bit)
        self.assertTrue(ffmpeg_invoked)
        self.assertEqual(res, expected_16bit)

    def test_lane_dispatch_demucs_vs_uvr(self):
        """Verifies separate_audio_stems routes Demucs models to Demucs lane and UVR models to UVR lane."""
        demucs_called = False
        uvr_called = False

        def fake_demucs(audio_path, model_name, model_version, **kwargs):
            nonlocal demucs_called
            demucs_called = True
            return {"vocals_data": b"", "background_data": b"", "duration_ms": 1000, "sample_rate": 16000, "channels": 1}

        def fake_uvr(audio_path, model_name, model_version, **kwargs):
            nonlocal uvr_called
            uvr_called = True
            return {"vocals_data": b"", "background_data": b"", "duration_ms": 1000, "sample_rate": 16000, "channels": 1}

        with mock.patch("separator.separate_demucs", side_effect=fake_demucs), \
             mock.patch("separator.separate_uvr", side_effect=fake_uvr):
            res_demucs = separator.separate_audio_stems("track.wav", "htdemucs", "v4")
            self.assertTrue(demucs_called)
            res_uvr = separator.separate_audio_stems("track.wav", "UVR-MDX-NET-Inst_HQ_4.onnx", "v3")
            self.assertTrue(uvr_called)

    # --- UVR Snapshot Validation Tests ---

    def test_uvr_snapshot_validation_missing_path(self):
        with self.assertRaises(RuntimeError) as ctx:
            separator.separate_uvr(
                "track.wav",
                "UVR-MDX-NET-Inst_HQ_4.onnx",
                "v3",
                model_path="/nonexistent/uvr/dir",
                require_model_snapshot=True,
            )
        self.assertIn("WORKER_SNAPSHOT_PATH_REQUIRED", str(ctx.exception))

    def test_uvr_snapshot_validation_escaping_entrypoint(self):
        with tempfile.TemporaryDirectory() as tmp_dir:
            with self.assertRaises(RuntimeError) as ctx:
                separator.separate_uvr(
                    "track.wav",
                    "UVR-MDX-NET-Inst_HQ_4.onnx",
                    "v3",
                    model_path=tmp_dir,
                    entrypoint_file="/etc/passwd",
                    require_model_snapshot=True,
                )
            self.assertIn("SEPARATOR_MODEL_ASSET_MISSING", str(ctx.exception))
            self.assertIn("escapes snapshot root", str(ctx.exception))

    def test_uvr_snapshot_validation_wrong_filename(self):
        with tempfile.TemporaryDirectory() as tmp_dir:
            wrong_file = os.path.join(tmp_dir, "wrong_model.onnx")
            with open(wrong_file, "w") as f:
                f.write("fake")
            with self.assertRaises(RuntimeError) as ctx:
                separator.separate_uvr(
                    "track.wav",
                    "UVR-MDX-NET-Inst_HQ_4.onnx",
                    "v3",
                    model_path=tmp_dir,
                    entrypoint_file=wrong_file,
                    require_model_snapshot=True,
                )
            self.assertIn("SEPARATOR_MODEL_ASSET_MISSING", str(ctx.exception))
            self.assertIn("expected UVR-MDX-NET-Inst_HQ_4.onnx", str(ctx.exception))
    def test_exact_artifact_sha256_constants_parity(self):
        """Verifies Python separator constants match exact authoritative SHA-256 digests from Go domain."""
        self.assertEqual(
            separator.UVR_PINNED_SHA256,
            "3c4b5b9b05090fdf238f38ba5046813982d50e2a652e9cb3324ea79720c3c9c8",
        )
        self.assertEqual(
            separator.DEMUCS_PINNED_SHA256,
            "8726e21a993978c7ba086d3872e7608d7d5bfca646ca4aca459ffda844faa8b4",
        )
        self.assertEqual(
            separator.DEMUCS_PINNED_BAG_YAML_SHA256,
            "239c445d0b14454d541ad8bd9bb271c9e536d267e8a4625208744cbb2e7bb66c",
        )

    def test_compute_sha256_helper_removed(self):
        """Proves StageWorker does not carry unused compute_sha256; RuntimeHost is the byte authority."""
        self.assertFalse(hasattr(separator, "compute_sha256"))
        self.assertFalse(hasattr(separator, "verify_artifact_sha256"))
    def test_uvr_stems_are_requested_at_contract_rate_and_reported_as_measured(self):
        """Pin the audio contract: UVR must be asked for 16 kHz stems and the artifact must report the
        rate/channels actually written. audio-separator defaults to 44100, and the audio-role analyzer
        rejects anything but 16 kHz, so a hardcoded 16000 in the response (or a default-rate
        Separator) dead-ends every real run at audio_role_plan while the metadata claims otherwise."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            onnx_file = os.path.join(tmp_dir, "UVR-MDX-NET-Inst_HQ_4.onnx")
            with open(onnx_file, "wb") as f:
                f.write(b"valid_model")
            mdx_json = os.path.join(tmp_dir, "mdx_model_data.json")
            with open(mdx_json, "w", encoding="utf-8") as f:
                json.dump({separator.UVR_MDX_NET_INST_HQ_4_MD5: {"primary_stem": "Vocals", "compensate": 1.035}}, f)
            # Stems as the pinned separator version writes them when no rate is requested.
            drifted_wav = _make_dummy_wav(duration_ms=1000, sample_rate=44100, channels=2)
            vocals_wav_path = os.path.join(tmp_dir, "track_Vocals.wav")
            inst_wav_path = os.path.join(tmp_dir, "track_Instrumental.wav")
            with open(vocals_wav_path, "wb") as vf:
                vf.write(drifted_wav)
            with open(inst_wav_path, "wb") as bf:
                bf.write(drifted_wav)

            mock_sep_inst = mock.MagicMock()
            mock_sep_inst.separate.return_value = [vocals_wav_path, inst_wav_path]
            mock_sep_mod = mock.MagicMock()
            mock_sep_mod.Separator.return_value = mock_sep_inst
            with mock.patch.dict("sys.modules", {"audio_separator": mock.MagicMock(), "audio_separator.separator": mock_sep_mod}):
                res = separator.separate_uvr(
                    "track.wav",
                    "UVR-MDX-NET-Inst_HQ_4.onnx",
                    "v3",
                    model_path=tmp_dir,
                    entrypoint_file=onnx_file,
                    require_model_snapshot=True,
                    metadata_file=mdx_json,
                )

            self.assertEqual(separator.CONTRACT_SAMPLE_RATE, 16000)
            _, ctor_kwargs = mock_sep_mod.Separator.call_args
            self.assertEqual(
                ctor_kwargs.get("sample_rate"),
                separator.CONTRACT_SAMPLE_RATE,
                "the separator must be constructed at the pipeline contract rate, not its 44.1 kHz default",
            )
            self.assertEqual(res["sample_rate"], 44100, "the artifact must report the rate actually written")
            self.assertEqual(res["channels"], 2, "the artifact must report the channel count actually written")

    def test_uvr_request_time_no_rehash(self):
        """Proves StageWorker respects #64: RuntimeHost is the byte-verification authority, so StageWorker does not re-hash on request."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            onnx_file = os.path.join(tmp_dir, "UVR-MDX-NET-Inst_HQ_4.onnx")
            with open(onnx_file, "wb") as f:
                f.write(b"valid_model")
            mdx_json = os.path.join(tmp_dir, "mdx_model_data.json")
            with open(mdx_json, "w", encoding="utf-8") as f:
                json.dump({separator.UVR_MDX_NET_INST_HQ_4_MD5: {"primary_stem": "Vocals", "compensate": 1.035}}, f)
            dummy_wav = _make_dummy_wav(duration_ms=1000)
            vocals_wav_path = os.path.join(tmp_dir, "track_Vocals.wav")
            inst_wav_path = os.path.join(tmp_dir, "track_Instrumental.wav")
            with open(vocals_wav_path, "wb") as vf:
                vf.write(dummy_wav)
            with open(inst_wav_path, "wb") as bf:
                bf.write(dummy_wav)

            mock_sep_inst = mock.MagicMock()
            mock_sep_inst.separate.return_value = [vocals_wav_path, inst_wav_path]
            mock_sep_mod = mock.MagicMock()
            mock_sep_mod.Separator.return_value = mock_sep_inst
            with mock.patch.dict("sys.modules", {"audio_separator": mock.MagicMock(), "audio_separator.separator": mock_sep_mod}):
                self.assertFalse(hasattr(separator, "compute_sha256"))
                res = separator.separate_uvr(
                    "track.wav",
                    "UVR-MDX-NET-Inst_HQ_4.onnx",
                    "v3",
                    model_path=tmp_dir,
                    entrypoint_file=onnx_file,
                    require_model_snapshot=True,
                    metadata_file=mdx_json,
                )
                self.assertEqual(res["duration_ms"], 1000)
                self.assertEqual(res["model_name"], "UVR-MDX-NET-Inst_HQ_4.onnx")

    def test_uvr_offline_network_helper_blocked(self):
        """Proves network/download helpers on Separator are blocked and raise NETWORK_DOWNLOAD_FORBIDDEN."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            onnx_file = os.path.join(tmp_dir, "UVR-MDX-NET-Inst_HQ_4.onnx")
            with open(onnx_file, "wb") as f:
                f.write(b"model")
            mdx_json = os.path.join(tmp_dir, "mdx_model_data.json")
            with open(mdx_json, "w", encoding="utf-8") as f:
                json.dump({separator.UVR_MDX_NET_INST_HQ_4_MD5: {"primary_stem": "Vocals", "compensate": 1.035}}, f)

            download_called = False
            class FakeSeparatorWithDownload:
                def __init__(self, *args, **kwargs):
                    pass
                def load_model(self, name):
                    # Simulates an unpatched Separator attempting to download
                    self.download_file_if_not_exists("http://example.com/check.json", "check.json")
                def download_file_if_not_exists(self, url, path):
                    nonlocal download_called
                    download_called = True
                def separate(self, audio):
                    return []

            mock_sep_mod = mock.MagicMock()
            mock_sep_mod.Separator = FakeSeparatorWithDownload
            with mock.patch.dict("sys.modules", {"audio_separator": mock.MagicMock(), "audio_separator.separator": mock_sep_mod}):
                with self.assertRaises(RuntimeError) as ctx:
                    separator.separate_uvr(
                        "track.wav",
                        "UVR-MDX-NET-Inst_HQ_4.onnx",
                        "v3",
                        model_path=tmp_dir,
                        entrypoint_file=onnx_file,
                        require_model_snapshot=True,
                        metadata_file=mdx_json,
                    )
                self.assertIn("NETWORK_DOWNLOAD_FORBIDDEN", str(ctx.exception))
                self.assertFalse(download_called)

    def test_uvr_missing_metadata_asset_fails_closed(self):
        """Proves UVR fails closed if mdx_model_data.json is missing from the snapshot."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            onnx_file = os.path.join(tmp_dir, "UVR-MDX-NET-Inst_HQ_4.onnx")
            with open(onnx_file, "wb") as f:
                f.write(b"model")
            with self.assertRaises(RuntimeError) as ctx:
                separator.separate_uvr(
                    "track.wav",
                    "UVR-MDX-NET-Inst_HQ_4.onnx",
                    "v3",
                    model_path=tmp_dir,
                    entrypoint_file=onnx_file,
                    require_model_snapshot=True,
                )
            self.assertIn("SEPARATOR_METADATA_ASSET_MISSING", str(ctx.exception))

    def test_uvr_missing_md5_entry_in_metadata_fails_closed(self):
        """Proves UVR fails closed if mdx_model_data.json does not contain the required MD5 entry."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            onnx_file = os.path.join(tmp_dir, "UVR-MDX-NET-Inst_HQ_4.onnx")
            with open(onnx_file, "wb") as f:
                f.write(b"model")
            mdx_json = os.path.join(tmp_dir, "mdx_model_data.json")
            with open(mdx_json, "w", encoding="utf-8") as f:
                json.dump({"different_md5": {"primary_stem": "Vocals"}}, f)
            with self.assertRaises(RuntimeError) as ctx:
                separator.separate_uvr(
                    "track.wav",
                    "UVR-MDX-NET-Inst_HQ_4.onnx",
                    "v3",
                    model_path=tmp_dir,
                    entrypoint_file=onnx_file,
                    require_model_snapshot=True,
                )
    def test_uvr_snapshot_rejects_missing_explicit_metadata_path_even_if_loose_file_exists(self):
        """Proves require_model_snapshot=true rejects missing explicit metadata_file even if loose mdx_model_data.json exists in model_path."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            onnx_file = os.path.join(tmp_dir, "UVR-MDX-NET-Inst_HQ_4.onnx")
            with open(onnx_file, "wb") as f:
                f.write(b"valid_model")
            loose_mdx = os.path.join(tmp_dir, "mdx_model_data.json")
            with open(loose_mdx, "w", encoding="utf-8") as f:
                json.dump({separator.UVR_MDX_NET_INST_HQ_4_MD5: {"primary_stem": "Vocals"}}, f)
            with self.assertRaises(RuntimeError) as ctx:
                separator.separate_uvr(
                    "track.wav",
                    "UVR-MDX-NET-Inst_HQ_4.onnx",
                    "v3",
                    model_path=tmp_dir,
                    entrypoint_file=onnx_file,
                    require_model_snapshot=True,
                    metadata_file=None,
                )
            self.assertIn("SEPARATOR_METADATA_ASSET_MISSING", str(ctx.exception))
            self.assertIn("loose discovery forbidden", str(ctx.exception))
    def test_uvr_snapshot_validation_valid_offline(self):
        with tempfile.TemporaryDirectory() as tmp_dir:
            onnx_file = os.path.join(tmp_dir, "UVR-MDX-NET-Inst_HQ_4.onnx")
            with open(onnx_file, "wb") as f:
                f.write(b"valid_model")
            mdx_json = os.path.join(tmp_dir, "mdx_model_data.json")
            expected_params = {
                "primary_stem": "Vocals",
                "compensate": 1.035,
                "mdx_dim_f_set": 2048,
                "mdx_dim_t_set": 8,
                "mdx_n_fft_scale_set": 6144,
            }
            with open(mdx_json, "w", encoding="utf-8") as f:
                json.dump({separator.UVR_MDX_NET_INST_HQ_4_MD5: expected_params}, f)

            files_before = set(os.listdir(tmp_dir))

            dummy_wav = _make_dummy_wav(duration_ms=1000)
            vocals_wav_path = os.path.join(tmp_dir, "track_Vocals.wav")
            inst_wav_path = os.path.join(tmp_dir, "track_Instrumental.wav")
            with open(vocals_wav_path, "wb") as vf:
                vf.write(dummy_wav)
            with open(inst_wav_path, "wb") as bf:
                bf.write(dummy_wav)

            mock_sep_inst = mock.MagicMock()
            mock_sep_inst.separate.return_value = [vocals_wav_path, inst_wav_path]
            mock_sep_mod = mock.MagicMock()
            mock_sep_mod.Separator.return_value = mock_sep_inst
            with mock.patch.dict("sys.modules", {"audio_separator": mock.MagicMock(), "audio_separator.separator": mock_sep_mod}):
                res = separator.separate_uvr(
                    "track.wav",
                    "UVR-MDX-NET-Inst_HQ_4.onnx",
                    "v3",
                    model_path=tmp_dir,
                    entrypoint_file=onnx_file,
                    require_model_snapshot=True,
                    metadata_file=mdx_json,
                )
                self.assertEqual(res["duration_ms"], 1000)
                self.assertEqual(os.environ.get("HF_HUB_OFFLINE"), "1")
                self.assertEqual(os.environ.get("TRANSFORMERS_OFFLINE"), "1")

                # Finding 1: Verify load_model_data_using_hash was bound to params extracted from verified mdx_model_data.json
                self.assertTrue(callable(mock_sep_inst.load_model_data_using_hash))
                loaded = mock_sep_inst.load_model_data_using_hash("dummy_hash")
                self.assertEqual(loaded, expected_params)

                # Finding 1: Verify snapshot directory was not mutated (no synthetic download_checks.json or vr_model_data.json)
                files_after = set(os.listdir(tmp_dir))
                new_files = files_after - files_before - {"track_Vocals.wav", "track_Instrumental.wav"}
                self.assertEqual(new_files, set(), "StageWorker must not create files in snapshot root")

                # Finding 2: Verify runtime_identity does NOT assert an unverified commit hash from Python code
                self.assertNotIn("@", res["runtime_identity"])
                self.assertTrue(res["runtime_identity"].startswith("python-audio-separator"))
    def test_demucs_ft_substitution_rejected(self):
        with self.assertRaises(RuntimeError) as ctx:
            separator.separate_demucs("track.wav", "htdemucs_ft", "v4")
        self.assertIn("DEMUCS_FT_SUBSTITUTION_REJECTED", str(ctx.exception))

    def test_demucs_snapshot_validation_missing_path(self):
        with self.assertRaises(RuntimeError) as ctx:
            separator.separate_demucs(
                "track.wav",
                "htdemucs",
                "v4",
                model_path="/nonexistent/demucs/dir",
                require_model_snapshot=True,
            )
        self.assertIn("WORKER_SNAPSHOT_PATH_REQUIRED", str(ctx.exception))

    def test_demucs_snapshot_validation_escaping_entrypoint(self):
        with tempfile.TemporaryDirectory() as tmp_dir:
            with self.assertRaises(RuntimeError) as ctx:
                separator.separate_demucs(
                    "track.wav",
                    "htdemucs",
                    "v4",
                    model_path=tmp_dir,
                    entrypoint_file="/var/run/demucs.th",
                    require_model_snapshot=True,
                )
            self.assertIn("SEPARATOR_MODEL_ASSET_MISSING", str(ctx.exception))
            self.assertIn("escapes snapshot root", str(ctx.exception))

    def test_demucs_snapshot_validation_wrong_filename(self):
        with tempfile.TemporaryDirectory() as tmp_dir:
            wrong_th = os.path.join(tmp_dir, "wrong_model.th")
            with open(wrong_th, "w") as f:
                f.write("fake")
            with self.assertRaises(RuntimeError) as ctx:
                separator.separate_demucs(
                    "track.wav",
                    "htdemucs",
                    "v4",
                    model_path=tmp_dir,
                    entrypoint_file=wrong_th,
                    require_model_snapshot=True,
                )
            self.assertIn("SEPARATOR_MODEL_ASSET_MISSING", str(ctx.exception))
            self.assertIn("expected 955717e8-8726e21a.th", str(ctx.exception))

    def test_demucs_request_time_no_rehash(self):
        """Proves StageWorker respects #64: RuntimeHost is the byte-verification authority, so Demucs does not re-hash on request."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            th_file = os.path.join(tmp_dir, "955717e8-8726e21a.th")
            with open(th_file, "wb") as f:
                f.write(b"valid_demucs_model")
            dummy_wav = _make_dummy_wav(duration_ms=2000)

            def fake_subprocess_run(cmd, stdout=None, stderr=None):
                out_dir = cmd[cmd.index("-o") + 1]
                model_name = cmd[cmd.index("-n") + 1]
                audio_file = cmd[-1]
                track_name = os.path.splitext(os.path.basename(audio_file))[0]
                target_dir = os.path.join(out_dir, model_name, track_name)
                os.makedirs(target_dir, exist_ok=True)
                with open(os.path.join(target_dir, "vocals.wav"), "wb") as f:
                    f.write(dummy_wav)
                with open(os.path.join(target_dir, "no_vocals.wav"), "wb") as f:
                    f.write(dummy_wav)
                mock_proc = mock.MagicMock()
                mock_proc.returncode = 0
                return mock_proc

            with mock.patch("subprocess.run", side_effect=fake_subprocess_run):
                self.assertFalse(hasattr(separator, "compute_sha256"))
                res = separator.separate_demucs(
                    "track.wav",
                    "htdemucs",
                    "v4",
                    model_path=tmp_dir,
                    entrypoint_file=th_file,
                    require_model_snapshot=True,
                )
                self.assertEqual(res["duration_ms"], 2000)
                self.assertEqual(res["model_name"], "htdemucs")

    def test_demucs_local_repo_with_bag_yaml(self):
        """Proves complete Demucs local repo with htdemucs.yaml invokes -n htdemucs --repo <path> and returns htdemucs provenance."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            th_file = os.path.join(tmp_dir, "955717e8-8726e21a.th")
            with open(th_file, "wb") as f:
                f.write(b"valid_demucs_model")
            yaml_file = os.path.join(tmp_dir, "htdemucs.yaml")
            with open(yaml_file, "w") as f:
                f.write("models: ['955717e8']\n")

            dummy_wav = _make_dummy_wav(duration_ms=2000)
            invoked_n = []
            def fake_subprocess_run(cmd, stdout=None, stderr=None):
                self.assertIn("--repo", cmd)
                self.assertEqual(cmd[cmd.index("--repo") + 1], tmp_dir)
                n_val = cmd[cmd.index("-n") + 1]
                invoked_n.append(n_val)
                out_dir = cmd[cmd.index("-o") + 1]
                audio_file = cmd[-1]
                track_name = os.path.splitext(os.path.basename(audio_file))[0]
                target_dir = os.path.join(out_dir, n_val, track_name)
                os.makedirs(target_dir, exist_ok=True)
                with open(os.path.join(target_dir, "vocals.wav"), "wb") as f:
                    f.write(dummy_wav)
                with open(os.path.join(target_dir, "no_vocals.wav"), "wb") as f:
                    f.write(dummy_wav)
                mock_proc = mock.MagicMock()
                mock_proc.returncode = 0
                return mock_proc

            with mock.patch("subprocess.run", side_effect=fake_subprocess_run):
                res = separator.separate_demucs(
                    "track.wav",
                    "htdemucs",
                    "v4",
                    model_path=tmp_dir,
                    entrypoint_file=th_file,
                    require_model_snapshot=True,
                    has_verified_bag_yaml=True,
                )
                self.assertEqual(invoked_n, ["htdemucs"])
                self.assertEqual(res["model_name"], "htdemucs")

    def test_demucs_unverified_bag_yaml_rejected_for_signature(self):
        """Proves unverified loose htdemucs.yaml is not trusted; Demucs falls back to exact signature 955717e8."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            th_file = os.path.join(tmp_dir, "955717e8-8726e21a.th")
            with open(th_file, "wb") as f:
                f.write(b"valid_demucs_model")
            yaml_file = os.path.join(tmp_dir, "htdemucs.yaml")
            with open(yaml_file, "w") as f:
                f.write("models: ['955717e8']\n")

            dummy_wav = _make_dummy_wav(duration_ms=2000)
            invoked_n = []
            def fake_subprocess_run(cmd, stdout=None, stderr=None):
                n_val = cmd[cmd.index("-n") + 1]
                invoked_n.append(n_val)
                out_dir = cmd[cmd.index("-o") + 1]
                track_name = os.path.splitext(os.path.basename(cmd[-1]))[0]
                target_dir = os.path.join(out_dir, n_val, track_name)
                os.makedirs(target_dir, exist_ok=True)
                with open(os.path.join(target_dir, "vocals.wav"), "wb") as f:
                    f.write(dummy_wav)
                with open(os.path.join(target_dir, "no_vocals.wav"), "wb") as f:
                    f.write(dummy_wav)
                mock_proc = mock.MagicMock()
                mock_proc.returncode = 0
                return mock_proc

            with mock.patch("subprocess.run", side_effect=fake_subprocess_run):
                res = separator.separate_demucs(
                    "track.wav",
                    "htdemucs",
                    "v4",
                    model_path=tmp_dir,
                    entrypoint_file=th_file,
                    require_model_snapshot=True,
                    has_verified_bag_yaml=False,  # Unverified on filesystem!
                )
                # Must NOT invoke as htdemucs; must invoke exact signature 955717e8!
                self.assertEqual(invoked_n, ["955717e8"])
                self.assertEqual(res["model_name"], "htdemucs")
    def test_demucs_local_repo_signature_fallback_provenance(self):
        """Proves Demucs local repo without htdemucs.yaml falls back to exact signature 955717e8 while preserving htdemucs provenance."""
        with tempfile.TemporaryDirectory() as tmp_dir:
            th_file = os.path.join(tmp_dir, "955717e8-8726e21a.th")
            with open(th_file, "wb") as f:
                f.write(b"valid_demucs_model")

            dummy_wav = _make_dummy_wav(duration_ms=2000)
            invoked_n = []
            def fake_subprocess_run(cmd, stdout=None, stderr=None):
                self.assertIn("--repo", cmd)
                self.assertEqual(cmd[cmd.index("--repo") + 1], tmp_dir)
                n_val = cmd[cmd.index("-n") + 1]
                invoked_n.append(n_val)
                out_dir = cmd[cmd.index("-o") + 1]
                audio_file = cmd[-1]
                track_name = os.path.splitext(os.path.basename(audio_file))[0]
                target_dir = os.path.join(out_dir, n_val, track_name)
                os.makedirs(target_dir, exist_ok=True)
                with open(os.path.join(target_dir, "vocals.wav"), "wb") as f:
                    f.write(dummy_wav)
                with open(os.path.join(target_dir, "no_vocals.wav"), "wb") as f:
                    f.write(dummy_wav)
                mock_proc = mock.MagicMock()
                mock_proc.returncode = 0
                return mock_proc

            with mock.patch("subprocess.run", side_effect=fake_subprocess_run):
                res = separator.separate_demucs(
                    "track.wav",
                    "htdemucs",
                    "v4",
                    model_path=tmp_dir,
                    entrypoint_file=th_file,
                    require_model_snapshot=True,
                )
                self.assertEqual(invoked_n, ["955717e8"])
                self.assertEqual(res["model_name"], "htdemucs")
    def test_audio_separator_version_mismatch_fails_closed(self):
        """Proves installed audio-separator package version mismatch fails closed."""
        with mock.patch.dict("sys.modules", {"audio_separator": mock.MagicMock(), "audio_separator.separator": mock.MagicMock()}):
            with mock.patch("importlib.metadata.version", return_value="0.46.0"):
                with self.assertRaises(RuntimeError) as ctx:
                    separator.separate_uvr("audio.wav", "model.onnx", "v3")
                self.assertIn("SEPARATOR_PACKAGE_VERSION_MISMATCH", str(ctx.exception))

    def test_demucs_version_mismatch_fails_closed(self):
        """Proves installed demucs package version mismatch fails closed."""
        with mock.patch("importlib.metadata.version", return_value="4.0.0"):
            with self.assertRaises(RuntimeError) as ctx:
                separator.separate_demucs("audio.wav", "htdemucs", "v4")
            self.assertIn("DEMUCS_PACKAGE_VERSION_MISMATCH", str(ctx.exception))

    def test_probe_runtime_identity_success_uvr(self):
        """Proves probe_runtime_identity successfully extracts valid PEP 610 metadata for UVR."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = json.dumps({
            "url": "https://github.com/nomadkaraoke/python-audio-separator.git",
            "vcs_info": {
                "vcs": "git",
                "commit_id": separator.PINNED_AUDIO_SEPARATOR_COMMIT,
            },
        })
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            res = separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertEqual(res["status"], "ok")
            self.assertEqual(res["package_name"], "audio-separator")
            self.assertEqual(res["package_version"], separator.PINNED_AUDIO_SEPARATOR_VERSION)
            self.assertEqual(res["source_revision"], separator.PINNED_AUDIO_SEPARATOR_COMMIT)
            self.assertIn("audio-separator", res["runtime_versions"])

    def test_probe_runtime_identity_success_demucs(self):
        """Proves probe_runtime_identity successfully extracts valid PEP 610 metadata for Demucs."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_DEMUCS_VERSION
        mock_dist.read_text.return_value = json.dumps({
            "url": "https://github.com/facebookresearch/demucs.git",
            "vcs_info": {
                "vcs": "git",
                "commit_id": separator.PINNED_DEMUCS_COMMIT,
            },
        })
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            res = separator.probe_runtime_identity("htdemucs")
            self.assertEqual(res["status"], "ok")
            self.assertEqual(res["package_name"], "demucs")
            self.assertEqual(res["package_version"], separator.PINNED_DEMUCS_VERSION)
            self.assertEqual(res["source_revision"], separator.PINNED_DEMUCS_COMMIT)
            self.assertIn("demucs", res["runtime_versions"])

    def test_probe_runtime_identity_package_not_installed(self):
        """Proves probe_runtime_identity fails closed when package distribution is missing."""
        import importlib.metadata
        with mock.patch("importlib.metadata.distribution", side_effect=importlib.metadata.PackageNotFoundError("pkg")):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("RUNTIME_IDENTITY_PROBE_FAILED", str(ctx.exception))
            self.assertIn("not installed", str(ctx.exception))

    def test_probe_runtime_identity_wrong_package_version(self):
        """Proves probe_runtime_identity fails closed when package version does not match pinned version."""
        mock_dist = mock.MagicMock()
        mock_dist.version = "0.46.0"
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("RUNTIME_IDENTITY_PROBE_FAILED", str(ctx.exception))
            self.assertIn("package version mismatch", str(ctx.exception))

    def test_probe_runtime_identity_version_suffix_rejected(self):
        """Proves probe_runtime_identity rejects version suffixes (+modified, .post1)."""
        for bad_ver in ["0.47.0+modified", "0.47.0.post1", "0.47.0-beta"]:
            mock_dist = mock.MagicMock()
            mock_dist.version = bad_ver
            with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
                with self.assertRaises(RuntimeError) as ctx:
                    separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
                self.assertIn("package version mismatch: expected exact", str(ctx.exception))

    def test_probe_runtime_identity_demucs_version_suffix_rejected(self):
        """Proves probe_runtime_identity rejects Demucs version suffixes."""
        for bad_ver in ["4.1.0a2.post1", "4.1.0a2+custom"]:
            mock_dist = mock.MagicMock()
            mock_dist.version = bad_ver
            with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
                with self.assertRaises(RuntimeError) as ctx:
                    separator.probe_runtime_identity("htdemucs")
                self.assertIn("package version mismatch: expected exact", str(ctx.exception))

    def test_probe_runtime_identity_non_git_vcs_rejected(self):
        """Proves probe_runtime_identity rejects non-git VCS types in direct_url.json."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = json.dumps({
            "url": "https://github.com/nomadkaraoke/python-audio-separator.git",
            "vcs_info": {"vcs": "hg", "commit_id": separator.PINNED_AUDIO_SEPARATOR_COMMIT},
        })
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("VCS type mismatch: expected 'git'", str(ctx.exception))

    def test_probe_runtime_identity_wrong_upstream_repo_rejected(self):
        """Proves probe_runtime_identity rejects fork or non-canonical upstream repository URLs."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = json.dumps({
            "url": "https://github.com/evilfork/python-audio-separator.git",
            "vcs_info": {"vcs": "git", "commit_id": separator.PINNED_AUDIO_SEPARATOR_COMMIT},
        })
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("upstream repository origin mismatch", str(ctx.exception))

    def test_probe_runtime_identity_local_directory_origin_rejected(self):
        """Proves probe_runtime_identity rejects local directory installs (dir_info without editable)."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = json.dumps({
            "url": "file:///path/to/local/source",
            "dir_info": {"editable": False},
        })
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("local directory", str(ctx.exception))
            self.assertIn("cannot prove pinned VCS source revision", str(ctx.exception))

    def test_probe_runtime_identity_editable_origin_rejected(self):
        """Proves probe_runtime_identity rejects editable installs (dir_info with editable: true)."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = json.dumps({
            "url": "file:///path/to/local/source",
            "dir_info": {"editable": True},
        })
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("editable", str(ctx.exception))
            self.assertIn("cannot prove pinned VCS source revision", str(ctx.exception))

    def test_probe_runtime_identity_archive_origin_rejected(self):
        """Proves probe_runtime_identity rejects source archive or wheel installs."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = json.dumps({
            "url": "https://example.com/audio_separator-0.47.0-py3-none-any.whl",
            "archive_info": {"hashes": {"sha256": "abcdef"}},
        })
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("archive/wheel", str(ctx.exception))
            self.assertIn("cannot prove pinned VCS source revision", str(ctx.exception))

    def test_probe_runtime_identity_canonical_vcs_url_forms_accepted(self):
        """Proves probe_runtime_identity accepts equivalent canonical GitHub URL forms."""
        urls = [
            "https://github.com/nomadkaraoke/python-audio-separator.git",
            "https://github.com/nomadkaraoke/python-audio-separator",
            "git+https://github.com/nomadkaraoke/python-audio-separator.git",
            "git+https://github.com/nomadkaraoke/python-audio-separator",
            "git@github.com:nomadkaraoke/python-audio-separator.git",
            "ssh://git@github.com/nomadkaraoke/python-audio-separator.git",
        ]
        for u in urls:
            mock_dist = mock.MagicMock()
            mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
            mock_dist.read_text.return_value = json.dumps({
                "url": u,
                "vcs_info": {"vcs": "git", "commit_id": separator.PINNED_AUDIO_SEPARATOR_COMMIT},
            })
            with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
                res = separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
                self.assertEqual(res["status"], "ok")
                self.assertEqual(res["source_revision"], separator.PINNED_AUDIO_SEPARATOR_COMMIT)

    def test_probe_runtime_identity_missing_direct_url(self):
        """Proves probe_runtime_identity fails closed when direct_url.json is missing (cannot prove VCS commit)."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = None
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("RUNTIME_IDENTITY_PROBE_FAILED", str(ctx.exception))
            self.assertIn("has no PEP 610 direct_url.json", str(ctx.exception))
            self.assertIn("hardcoded claims rejected", str(ctx.exception))

    def test_probe_runtime_identity_missing_commit_id(self):
        """Proves probe_runtime_identity fails closed when direct_url.json has no commit_id."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = json.dumps({"url": "https://github.com/nomadkaraoke/python-audio-separator.git", "vcs_info": {"vcs": "git"}})
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("RUNTIME_IDENTITY_PROBE_FAILED", str(ctx.exception))
            self.assertIn("has no vcs_info.commit_id", str(ctx.exception))

    def test_probe_runtime_identity_wrong_commit_id(self):
        """Proves probe_runtime_identity fails closed when VCS commit does not match pinned commit."""
        mock_dist = mock.MagicMock()
        mock_dist.version = separator.PINNED_AUDIO_SEPARATOR_VERSION
        mock_dist.read_text.return_value = json.dumps({
            "url": "https://github.com/nomadkaraoke/python-audio-separator.git",
            "vcs_info": {"vcs": "git", "commit_id": "0000000000000000000000000000000000000000"},
        })
        with mock.patch("importlib.metadata.distribution", return_value=mock_dist):
            with self.assertRaises(RuntimeError) as ctx:
                separator.probe_runtime_identity("UVR-MDX-NET-Inst_HQ_4.onnx")
            self.assertIn("RUNTIME_IDENTITY_PROBE_FAILED", str(ctx.exception))
            self.assertIn("source revision mismatch", str(ctx.exception))

    def test_probe_cli_mode_success(self):
        """Proves main() handles mode=probe via stdin and emits valid JSON on stdout."""
        separator._SEPARATOR_PROBE_FACTORY = lambda m, v: {
            "status": "ok",
            "package_name": "audio-separator",
            "package_version": separator.PINNED_AUDIO_SEPARATOR_VERSION,
            "source_revision": separator.PINNED_AUDIO_SEPARATOR_COMMIT,
            "runtime_versions": {"audio-separator": separator.PINNED_AUDIO_SEPARATOR_VERSION},
            "adapter_revision": "cmd/stageworker/adapters/separator.py@v0.47.0",
        }
        stdin_data = json.dumps({"mode": "probe", "model_name": "UVR-MDX-NET-Inst_HQ_4.onnx"})
        with mock.patch("sys.stdin", io.StringIO(stdin_data)):
            with mock.patch("sys.stdout", new_callable=io.StringIO) as mock_out:
                with self.assertRaises(SystemExit) as ctx:
                    separator.main()
                self.assertEqual(ctx.exception.code, 0)
                out = json.loads(mock_out.getvalue())
                self.assertEqual(out["status"], "ok")
                self.assertEqual(out["source_revision"], separator.PINNED_AUDIO_SEPARATOR_COMMIT)

    def test_probe_cli_mode_failure_exits_nonzero(self):
        """Proves main() handles mode=probe failure by exiting with code 1 and reporting error on stderr."""
        separator._SEPARATOR_PROBE_FACTORY = lambda m, v: (_ for _ in ()).throw(
            RuntimeError("RUNTIME_IDENTITY_PROBE_FAILED: unverified")
        )
        stdin_data = json.dumps({"mode": "probe", "model_name": "UVR-MDX-NET-Inst_HQ_4.onnx"})
        with mock.patch("sys.stdin", io.StringIO(stdin_data)):
            with mock.patch("sys.stderr", new_callable=io.StringIO) as mock_err:
                with self.assertRaises(SystemExit) as ctx:
                    separator.main()
                self.assertEqual(ctx.exception.code, 1)
                self.assertIn("RUNTIME_IDENTITY_PROBE_FAILED", mock_err.getvalue())
if __name__ == "__main__":
    unittest.main()
