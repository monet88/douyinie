#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/tts_engine.py
Verifies exact official API adapter contracts against checked-in reference implementations:
1. VieNeu: Vieneu().infer(text=..., voice=...) and get_preset_voice(voice_id)
2. CosyVoice3: AutoModel(model_dir=...).inference_sft(..., stream=False, speed=...) with model.sample_rate
3. Kokoro: KPipeline(lang_code=...) yielding Result objects with result.audio
4. Chatterbox: ChatterboxTTS.from_pretrained(device=...).generate(text) with model.sr
"""

import base64
import hashlib
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock
import wave

# Ensure adapters directory is on path
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import tts_engine
from tts_engine import (
    build_tts_response,
    encode_audio_to_wav,
    run_chatterbox_tts,
    run_cosyvoice_tts,
    run_kokoro_tts,
    run_tts,
    run_vieneu_tts,
    run_zerotts_tts,
)


def make_zerotts_snapshot(root, voices=("quangminh",)):
    required = {
        "config.json": "{}",
        "tokenizer.json": "{}",
        "null_voice_emb.npy": "null",
        "onnx/text_encoder.onnx": "onnx",
        "onnx/prefix_step.onnx": "onnx",
        "onnx/local_frame_decode.onnx": "onnx",
        "onnx/codec/decode_full.onnx": "onnx",
        "voices/index.json": json.dumps({"voices": [{"name": v} for v in voices]}),
    }
    for rel, content in required.items():
        path = os.path.join(root, *rel.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(content.encode("utf-8"))
    for voice in voices:
        voice_path = os.path.join(root, "voices", voice, "voice.npz")
        os.makedirs(os.path.dirname(voice_path), exist_ok=True)
        with open(voice_path, "wb") as f:
            f.write(b"voice")
    return os.path.join(root, "voices", voices[0], "voice.npz")


class TestTTSEngineUpstreamContracts(unittest.TestCase):
    def tearDown(self):
        tts_engine._ZEROTTS_MODEL_FACTORY = None
        tts_engine._ZEROTTS_PROBE_FACTORY = None
        tts_engine._VIENEU_MODEL_FACTORY = None
        tts_engine._COSYVOICE_MODEL_FACTORY = None
        tts_engine._KOKORO_MODEL_FACTORY = None
        tts_engine._CHATTERBOX_MODEL_FACTORY = None

    def test_run_zerotts_exact_upstream_contract_cpu_only(self):
        calls = {}

        class MockZeroTTS:
            def __init__(self, model_dir=None, providers=None):
                calls["model_dir"] = model_dir
                calls["providers"] = providers
                self.sample_rate = 48000

            def synthesize(self, text, voice=None):
                calls["synthesize"] = (text, voice)
                return [0.05] * 48000

        tts_engine._ZEROTTS_MODEL_FACTORY = MockZeroTTS
        with tempfile.TemporaryDirectory() as tmpdir:
            entrypoint = make_zerotts_snapshot(tmpdir)
            resp = run_zerotts_tts(
                "Xin chào Việt Nam",
                "vi",
                "quangminh",
                1.0,
                model_path=tmpdir,
                entrypoint_file=entrypoint,
            )

        self.assertEqual(calls["providers"], ["CPUExecutionProvider"])
        self.assertEqual(calls["synthesize"], ("Xin chào Việt Nam", "quangminh"))
        self.assertEqual(resp["measured_duration_ms"], 1000)
        self.assertEqual(resp["model_name"], tts_engine.ZEROTTS_MODEL_ID)
        self.assertEqual(os.environ.get("HF_HUB_OFFLINE"), "1")
        self.assertEqual(os.environ.get("TRANSFORMERS_OFFLINE"), "1")

    def test_run_zerotts_rejects_speed_and_unverified_voice(self):
        with self.assertRaises(ValueError) as ctx:
            run_zerotts_tts("Xin chào", "vi", "quangminh", 1.01)
        self.assertIn("TTS_SPEED_UNSUPPORTED", str(ctx.exception))

        with self.assertRaises(ValueError) as ctx:
            run_zerotts_tts("Xin chào", "vi", "unknown", 1.0)
        self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))

    def test_run_zerotts_requires_canonical_snapshot_assets_and_entrypoint(self):
        tts_engine._ZEROTTS_MODEL_FACTORY = MagicMock()
        with tempfile.TemporaryDirectory() as tmpdir:
            with self.assertRaises(RuntimeError) as ctx:
                run_zerotts_tts(
                    "Xin chào", "vi", "quangminh", 1.0,
                    model_path=tmpdir,
                    entrypoint_file=os.path.join(tmpdir, "voices", "quangminh", "voice.npz"),
                )
            self.assertIn("TTS_MODEL_ASSET_MISSING", str(ctx.exception))

            entrypoint = make_zerotts_snapshot(tmpdir)
            wrong = os.path.join(tmpdir, "voices", "maichi", "voice.npz")
            with self.assertRaises(RuntimeError) as ctx:
                run_zerotts_tts(
                    "Xin chào", "vi", "quangminh", 1.0,
                    model_path=tmpdir,
                    entrypoint_file=wrong,
                )
            self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))

    def test_run_zerotts_empty_audio_fails_closed(self):
        class EmptyZeroTTS:
            def __init__(self, **kwargs):
                self.sample_rate = 48000

            def synthesize(self, text, voice=None):
                return []

        tts_engine._ZEROTTS_MODEL_FACTORY = EmptyZeroTTS
        with tempfile.TemporaryDirectory() as tmpdir:
            entrypoint = make_zerotts_snapshot(tmpdir)
            with self.assertRaises(RuntimeError) as ctx:
                run_zerotts_tts(
                    "Xin chào", "vi", "quangminh", 1.0,
                    model_path=tmpdir,
                    entrypoint_file=entrypoint,
                )
        self.assertIn("TTS_NO_AUDIO", str(ctx.exception))

    def test_probe_zerotts_runtime_identity_requires_exact_pep610_vcs_evidence(self):
        direct_url = {
            "url": "https://github.com/zeroweight-ai/ZeroTTS.git",
            "vcs_info": {
                "vcs": "git",
                "commit_id": tts_engine.ZEROTTS_SOURCE_COMMIT,
            },
        }
        dist = MagicMock()
        dist.version = tts_engine.ZEROTTS_PACKAGE_VERSION
        dist.read_text.return_value = json.dumps(direct_url)

        import unittest.mock as mock
        with mock.patch("importlib.metadata.distribution", return_value=dist), \
             mock.patch("importlib.metadata.version", side_effect=lambda name: "1.0.0"):
            out = tts_engine.probe_zerotts_runtime_identity()

        self.assertEqual(out["status"], "ok")
        self.assertEqual(out["source_revision"], tts_engine.ZEROTTS_SOURCE_COMMIT)
        self.assertEqual(out["runtime_versions"]["zerotts"], tts_engine.ZEROTTS_PACKAGE_VERSION)
        self.assertEqual(out["adapter_revision"], tts_engine.ZEROTTS_ADAPTER_REVISION)

    def test_probe_zerotts_runtime_identity_fails_without_pep610(self):
        dist = MagicMock()
        dist.version = tts_engine.ZEROTTS_PACKAGE_VERSION
        dist.read_text.return_value = None

        import unittest.mock as mock
        with mock.patch("importlib.metadata.distribution", return_value=dist):
            with self.assertRaises(RuntimeError) as ctx:
                tts_engine.probe_zerotts_runtime_identity()
        self.assertIn("no PEP 610 direct_url.json", str(ctx.exception))

    def test_encode_audio_to_wav_from_samples(self):
        # 24000 samples at 24000 Hz = exactly 1000 ms (1 second)
        samples = [0.1 * ((i % 100) - 50) / 50.0 for i in range(24000)]
        wav_bytes, dur_ms = encode_audio_to_wav(samples, sample_rate=24000)

        self.assertEqual(dur_ms, 1000)
        self.assertTrue(wav_bytes.startswith(b"RIFF"))

        with wave.open(io.BytesIO(wav_bytes), "rb") as wf:
            self.assertEqual(wf.getnchannels(), 1)
            self.assertEqual(wf.getsampwidth(), 2)
            self.assertEqual(wf.getframerate(), 24000)
            self.assertEqual(wf.getnframes(), 24000)

    def test_encode_audio_to_wav_passthrough_valid_wav(self):
        samples = [0.0] * 12000  # 500ms at 24000 Hz
        orig_wav, orig_dur = encode_audio_to_wav(samples, sample_rate=24000)

        re_wav, re_dur = encode_audio_to_wav(orig_wav, sample_rate=24000)
        self.assertEqual(re_dur, 500)
        self.assertEqual(re_wav, orig_wav)

    def test_build_tts_response(self):
        samples = [0.0] * 48000  # 1000ms at 48000 Hz
        wav_bytes, dur_ms = encode_audio_to_wav(samples, sample_rate=48000)

        resp = build_tts_response(
            wav_bytes=wav_bytes,
            measured_duration_ms=dur_ms,
            model_name="vieneu-tts",
            model_version="1.0.0",
            predicted_duration_ms=1100,
        )

        self.assertEqual(resp["measured_duration_ms"], 1000)
        self.assertEqual(resp["predicted_duration_ms"], 1100)
        self.assertEqual(resp["model_name"], "vieneu-tts")
        self.assertEqual(resp["model_version"], "1.0.0")

        decoded_bytes = base64.b64decode(resp["audio_data"])
        self.assertEqual(hashlib.sha256(decoded_bytes).hexdigest(), resp["audio_sha256"])

    def test_run_vieneu_tts_exact_upstream_contract(self):
        """VieNeu must call engine.infer(text=..., voice=...) and get_preset_voice(voice_id)."""
        calls = []

        class MockVieneu:
            def __init__(self, mode="v3turbo"):
                self.mode = mode

            def get_preset_voice(self, voice_name):
                calls.append(("get_preset_voice", voice_name))
                return {"codes": [1, 2, 3], "text": "ref_sample"}

            def infer(self, text, voice=None):
                calls.append(("infer", text, voice))
                # 48000 samples at 48000 Hz = 1000ms
                return [0.05] * 48000

        tts_engine._VIENEU_MODEL_FACTORY = MockVieneu

        resp = run_vieneu_tts(
            text="Xin chào Việt Nam",
            language="vi",
            voice_id="Trúc Ly",
            speed=1.0,
        )

        self.assertEqual(resp["measured_duration_ms"], 1000)
        self.assertEqual(resp["model_name"], "vieneu-tts")
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0], ("get_preset_voice", "Trúc Ly"))
        self.assertEqual(calls[1][0], "infer")
        self.assertEqual(calls[1][1], "Xin chào Việt Nam")
        self.assertEqual(calls[1][2], {"codes": [1, 2, 3], "text": "ref_sample"})
    def test_run_vieneu_tts_fails_if_only_synthesize_method(self):
        """VieNeu adapter must reject fake models that only have synthesize instead of infer."""
        class MockOldVieNeu:
            def synthesize(self, text, voice, speed):
                return [0.01] * 48000

        tts_engine._VIENEU_MODEL_FACTORY = MockOldVieNeu

        with self.assertRaises(RuntimeError) as ctx:
            run_vieneu_tts("Text", "vi", "Trúc Ly", 1.0)
        self.assertIn("missing required 'infer' method", str(ctx.exception))

    def test_run_cosyvoice_tts_exact_upstream_contract_and_speed_fit(self):
        """CosyVoice3 must call AutoModel with inference_sft and multi-pass speed-fit lane."""
        calls = []

        class MockCosyVoiceAutoModel:
            def __init__(self, model_dir=None):
                self.model_dir = model_dir
                self.sample_rate = 24000

            def inference_sft(self, text, spk_id, stream=False, speed=1.0):
                calls.append({"text": text, "spk_id": spk_id, "stream": stream, "speed": speed})
                # Natural pass (speed=1.0) -> 48000 samples (2000ms)
                # Fitted pass (speed=2.0) -> 24000 samples (1000ms)
                dur_samples = int(48000 / speed)
                yield {"tts_speech": [0.02] * dur_samples}

        tts_engine._COSYVOICE_MODEL_FACTORY = MockCosyVoiceAutoModel

        # Request with slot_duration_ms = 1000 triggering speed-fit
        resp = run_cosyvoice_tts(
            text="测试语音合成",
            language="zh",
            voice_id="中文女",
            speed=1.0,
            slot_duration_ms=1000,
        )

        self.assertEqual(len(calls), 2)
        # Pass 1: Natural pass with stream=False
        self.assertEqual(calls[0]["stream"], False)
        self.assertEqual(calls[0]["speed"], 1.0)
        self.assertEqual(calls[0]["spk_id"], "中文女")
        # Pass 2: Calibrated speed-fit pass
        self.assertEqual(calls[1]["stream"], False)
        self.assertAlmostEqual(calls[1]["speed"], 2.0, delta=0.01)

        self.assertEqual(resp["measured_duration_ms"], 1000)
        self.assertEqual(resp["predicted_duration_ms"], 2000)

    def test_run_kokoro_tts_exact_upstream_contract(self):
        """Kokoro must instantiate KPipeline and extract Result.audio property."""
        calls = []

        class MockResult:
            def __init__(self, audio_data):
                self.audio = audio_data
                self.graphemes = "Hello"
                self.phonemes = "həˈloʊ"

        class MockKPipeline:
            def __init__(self, lang_code="a"):
                self.lang_code = lang_code

            def __call__(self, text, voice="af_heart", speed=1.0):
                calls.append({"text": text, "voice": voice, "speed": speed})
                # 2 chunks of 12000 samples = 24000 samples (1000ms at 24 kHz)
                yield MockResult([0.01] * 12000)
                yield MockResult([0.02] * 12000)

        tts_engine._KOKORO_MODEL_FACTORY = MockKPipeline

        resp = run_kokoro_tts(
            text="Hello world from Kokoro",
            language="en",
            voice_id="af_heart",
            speed=1.1,
        )

        self.assertEqual(resp["measured_duration_ms"], 1000)
        self.assertEqual(resp["model_name"], "kokoro-tts")
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0]["voice"], "af_heart")
        self.assertEqual(calls[0]["speed"], 1.1)

    def test_run_chatterbox_tts_exact_upstream_contract(self):
        """Chatterbox must use from_pretrained(device) and generate(text)."""
        calls = []

        class MockChatterboxModel:
            def __init__(self, device="cpu"):
                self.device = device
                self.sr = 24000

            def generate(self, text, audio_prompt_path=None):
                calls.append({"text": text, "audio_prompt_path": audio_prompt_path})
                return [0.03] * 24000  # 1000ms at 24 kHz

        class MockChatterboxTTS:
            @classmethod
            def from_pretrained(cls, device="cpu"):
                return MockChatterboxModel(device=device)

        tts_engine._CHATTERBOX_MODEL_FACTORY = MockChatterboxTTS

        # 1. Non-file voice_id must NOT be passed as audio_prompt_path
        resp = run_chatterbox_tts(
            text="Chatterbox clone speech",
            language="en",
            voice_id="spk_preset_voice_id",
            speed=1.0,
        )

        self.assertEqual(resp["measured_duration_ms"], 1000)
        self.assertEqual(resp["model_name"], "chatterbox-tts")
        self.assertEqual(len(calls), 1)
        self.assertIsNone(calls[0]["audio_prompt_path"])
        self.assertEqual(calls[0]["text"], "Chatterbox clone speech")

        # 2. Existing file audio prompt is passed
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tmp_prompt:
            tmp_prompt.write(b"RIFF....WAVE")
            tmp_prompt_path = tmp_prompt.name

        try:
            calls.clear()
            resp2 = run_chatterbox_tts(
                text="Cloned voice speech",
                language="en",
                voice_id=tmp_prompt_path,
                speed=1.0,
            )
            self.assertEqual(len(calls), 1)
            self.assertEqual(calls[0]["audio_prompt_path"], tmp_prompt_path)
        finally:
            if os.path.exists(tmp_prompt_path):
                os.remove(tmp_prompt_path)

    def test_fail_closed_on_missing_package(self):
        import unittest.mock as mock

        with tempfile.TemporaryDirectory() as tmpdir:
            # Create valid snapshot layouts so missing package errors can be tested
            v_cat = os.path.join(tmpdir, "src", "vieneu", "assets")
            os.makedirs(v_cat, exist_ok=True)
            with open(os.path.join(v_cat, "voices_v3_turbo.json"), "w", encoding="utf-8") as f:
                json.dump({"presets": {"Trúc Ly": {"id": "Trúc Ly"}}}, f)
            os.makedirs(os.path.join(tmpdir, "moss_tokenizer"), exist_ok=True)

            with open(os.path.join(tmpdir, "config.json"), "w") as f:
                f.write("{}")
            orig_sha = tts_engine.KOKORO_CHECKPOINT_SHA256
            tts_engine.KOKORO_CHECKPOINT_SHA256 = hashlib.sha256(b"").hexdigest()
            with open(os.path.join(tmpdir, "kokoro-v1_0.pth"), "wb") as f:
                f.write(b"")
            v_dir = os.path.join(tmpdir, "voices")
            os.makedirs(v_dir, exist_ok=True)
            with open(os.path.join(v_dir, "af_heart.pt"), "wb") as f:
                f.write(b"")

            try:
                with mock.patch.dict(
                    "sys.modules",
                    {
                        "vieneu": None,
                        "vieneu.Vieneu": None,
                        "cosyvoice": None,
                        "cosyvoice.cli": None,
                        "cosyvoice.cli.cosyvoice": None,
                        "kokoro": None,
                        "chatterbox": None,
                        "chatterbox.tts": None,
                    },
                ):
                    with self.assertRaises(RuntimeError) as ctx:
                        run_vieneu_tts("Text", "vi", "Trúc Ly", 1.0, model_path=tmpdir)
                    self.assertIn("vieneu package not found", str(ctx.exception))

                    with self.assertRaises(RuntimeError) as ctx:
                        run_cosyvoice_tts("Text", "zh", "voice", 1.0)
                    self.assertIn("cosyvoice package not found", str(ctx.exception))

                    with self.assertRaises(RuntimeError) as ctx:
                        run_kokoro_tts("Text", "en", "af_heart", 1.0, model_path=tmpdir)
                    self.assertIn("kokoro package not found", str(ctx.exception))

                    with self.assertRaises(RuntimeError) as ctx:
                        run_chatterbox_tts("Text", "en", "voice", 1.0)
                    self.assertIn("chatterbox package not found", str(ctx.exception))
            finally:
                tts_engine.KOKORO_CHECKPOINT_SHA256 = orig_sha

    def test_cli_execution_via_stdin_stdout(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Create a mock vieneu.py module adhering to official Vieneu interface
            mock_module_path = os.path.join(tmpdir, "vieneu.py")
            with open(mock_module_path, "w", encoding="utf-8") as f:
                f.write(
                    """
class Vieneu:
    def __init__(self, **kwargs):
        self.mode = kwargs.get("mode")
    def get_preset_voice(self, voice_id):
        return {"id": voice_id}
    def infer(self, text, voice=None, **kwargs):
        return [0.0] * 48000
"""
                )

            # Setup verified snapshot structure in tmpdir
            v_cat = os.path.join(tmpdir, "src", "vieneu", "assets")
            os.makedirs(v_cat, exist_ok=True)
            with open(os.path.join(v_cat, "voices_v3_turbo.json"), "w", encoding="utf-8") as f:
                json.dump({"presets": {"Trúc Ly": {"id": "Trúc Ly", "codes": [1, 2], "speaker_emb": [0.1]}}}, f)
            os.makedirs(os.path.join(tmpdir, "moss_tokenizer"), exist_ok=True)

            adapter_path = os.path.abspath(
                os.path.join(os.path.dirname(__file__), "tts_engine.py")
            )
            req = {
                "text": "Kiểm tra CLI adapter",
                "model_name": "vieneu-tts",
                "model_version": "1.0.0",
                "model_path": tmpdir,
                "language": "vi",
                "voice_id": "Trúc Ly",
                "speed": "1.0",
            }
            env = os.environ.copy()
            env["PYTHONPATH"] = tmpdir + os.pathsep + env.get("PYTHONPATH", "")

            proc = subprocess.Popen(
                [sys.executable, adapter_path],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=env,
                text=True,
            )
            stdout, stderr = proc.communicate(input=json.dumps(req))

            self.assertEqual(proc.returncode, 0, f"Process failed: {stderr}")
            out = json.loads(stdout)
            self.assertEqual(out["measured_duration_ms"], 1000)
            self.assertEqual(out["model_name"], "vieneu-tts")
            self.assertIn("audio_data", out)
            self.assertTrue(len(out["audio_sha256"]) == 64)
    def test_model_name_precedence_over_language_fallback(self):
        """Explicit model name takes precedence over language fallback."""
        import unittest.mock as mock

        # Mock run functions
        with mock.patch("tts_engine.run_zerotts_tts") as mock_zero, \
             mock.patch("tts_engine.run_cosyvoice_tts") as mock_cosy, \
             mock.patch("tts_engine.run_vieneu_tts") as mock_vieneu, \
             mock.patch("tts_engine.run_kokoro_tts") as mock_kokoro:

            mock_zero.return_value = {"model_name": "zerotts", "measured_duration_ms": 1000}
            mock_cosy.return_value = {"model_name": "cosyvoice3", "measured_duration_ms": 1000}
            mock_vieneu.return_value = {"model_name": "vieneu-tts", "measured_duration_ms": 1000}
            mock_kokoro.return_value = {"model_name": "kokoro-tts", "measured_duration_ms": 1000}

            # 1. CosyVoice3 with language=vi must route to CosyVoice, NOT VieNeu
            res1 = run_tts({
                "text": "Xin chào",
                "model_name": "cosyvoice3",
                "language": "vi",
                "voice_id": "中文女",
                "speed": 1.0,
            })
            mock_cosy.assert_called_once()
            mock_vieneu.assert_not_called()
            self.assertEqual(res1["model_name"], "cosyvoice3")

            # 2. VieNeu with language=vi routes to VieNeu
            res2 = run_tts({
                "text": "Xin chào",
                "model_name": "vieneu-tts",
                "language": "vi",
                "voice_id": "Thục Đoan",
                "speed": 1.0,
            })
            mock_vieneu.assert_called_once()

            # 3. Kokoro with language=en routes to Kokoro
            res3 = run_tts({
                "text": "Hello world",
                "model_name": "kokoro-tts",
                "language": "en",
                "voice_id": "af_bella",
                "speed": 1.0,
            })
            mock_kokoro.assert_called_once()

            # 4. Explicit ZeroTTS with language=vi routes only to ZeroTTS.
            mock_vieneu.reset_mock()
            res4 = run_tts({
                "text": "Xin chào",
                "model_name": "zeroweight-ai/ZeroTTS",
                "model_version": tts_engine.ZEROTTS_MODEL_REVISION,
                "language": "vi",
                "voice_id": "quangminh",
                "speed": 1.0,
            })
            mock_zero.assert_called_once()
            mock_vieneu.assert_not_called()
            self.assertEqual(res4["model_name"], "zerotts")

            # 5. Omitted model_name preserves the historical VI VieNeu fallback.
            mock_zero.reset_mock()
            mock_vieneu.reset_mock()
            res5 = run_tts({
                "text": "Xin chào",
                "language": "vi",
                "voice_id": "Trúc Ly",
                "speed": 1.0,
            })
            mock_zero.assert_not_called()
            mock_vieneu.assert_called_once()
            self.assertEqual(res5["model_name"], "vieneu-tts")

            # 6. A non-canonical ZeroTTS identity fails closed instead of falling through.
            mock_zero.reset_mock()
            with self.assertRaises(ValueError) as ctx:
                run_tts({
                    "text": "Xin chào",
                    "model_name": "zeroweight-ai/ZeroTTS-fake",
                    "model_version": tts_engine.ZEROTTS_MODEL_REVISION,
                    "language": "vi",
                    "voice_id": "maichi",
                    "speed": 1.0,
                })
            self.assertIn("TTS_MODEL_UNSUPPORTED", str(ctx.exception))
            mock_zero.assert_not_called()

    def test_unverified_voice_fails_closed(self):
        """Unverified voice presets must fail closed with TTS_VOICE_ASSET_MISSING."""
        with self.assertRaises(ValueError) as ctx:
            run_vieneu_tts("Text", "vi", "unknown_voice", 1.0)
        self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))

        with self.assertRaises(ValueError) as ctx:
            run_kokoro_tts("Text", "en", "unknown_voice", 1.0)
        self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))

    def test_kokoro_checkpoint_sha256_mismatch_fails_closed(self):
        """Kokoro checkpoint with mismatched sha256 must fail closed."""
        with tempfile.TemporaryDirectory() as tmpdir:
            ckpt = os.path.join(tmpdir, "kokoro-v1_0.pth")
            with open(ckpt, "wb") as f:
                f.write(b"corrupted bytes")
            voices_dir = os.path.join(tmpdir, "voices")
            os.makedirs(voices_dir, exist_ok=True)
            with open(os.path.join(voices_dir, "af_heart.pt"), "wb") as f:
                f.write(b"voice data")

            with self.assertRaises(RuntimeError) as ctx:
                run_kokoro_tts("Text", "en", "af_heart", 1.0, model_path=tmpdir)
            self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))
            self.assertIn("sha256 mismatch", str(ctx.exception))

    def test_kokoro_missing_voice_asset_fails_closed(self):
        """Kokoro snapshot missing requested voice asset must fail closed."""
        with tempfile.TemporaryDirectory() as tmpdir:
            with self.assertRaises(RuntimeError) as ctx:
                run_kokoro_tts("Text", "en", "af_heart", 1.0, model_path=tmpdir)
            self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))
            self.assertIn("missing from snapshot", str(ctx.exception))

    def test_vieneu_missing_snapshot_catalog_fails_closed(self):
        """VieNeu snapshot missing voices_v3_turbo.json must fail closed."""
        with tempfile.TemporaryDirectory() as tmpdir:
            empty_sub = os.path.join(tmpdir, "empty_model")
            os.makedirs(empty_sub, exist_ok=True)
            with self.assertRaises(RuntimeError) as ctx:
                run_vieneu_tts("Text", "vi", "Trúc Ly", 1.0, model_path=empty_sub)
            self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))
            self.assertIn("voices_v3_turbo.json", str(ctx.exception))

    def test_vieneu_fabricated_voices_pt_fails_closed(self):
        """VieNeu snapshot with fabricated voices/*.pt or generic config.json must fail closed."""
        with tempfile.TemporaryDirectory() as tmpdir:
            # Create fake config.json and voices/Trúc Ly.pt
            with open(os.path.join(tmpdir, "config.json"), "w") as f:
                f.write('{"model_type": "vieneu_v3_turbo"}')
            voices_dir = os.path.join(tmpdir, "voices")
            os.makedirs(voices_dir, exist_ok=True)
            with open(os.path.join(voices_dir, "Trúc Ly.pt"), "w") as f:
                f.write("fake pt")
            with self.assertRaises(RuntimeError) as ctx:
                run_vieneu_tts("Text", "vi", "Trúc Ly", 1.0, model_path=tmpdir)
            self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))
            self.assertIn("voices_v3_turbo.json", str(ctx.exception))

    def test_vieneu_valid_v3_turbo_catalog_succeeds(self):
        """VieNeu snapshot with authentic voices_v3_turbo.json resolves requested preset voice."""
        with tempfile.TemporaryDirectory() as tmpdir:
            cat_dir = os.path.join(tmpdir, "src", "vieneu", "assets")
            os.makedirs(cat_dir, exist_ok=True)
            cat_file = os.path.join(cat_dir, "voices_v3_turbo.json")
            with open(cat_file, "w", encoding="utf-8") as f:
                json.dump({"presets": {"Trúc Ly": {"id": "Trúc Ly", "name": "Trúc Ly"}}}, f)
            os.makedirs(os.path.join(tmpdir, "moss_tokenizer"), exist_ok=True)

            mock_engine = MagicMock()
            mock_engine.infer.return_value = [0.0] * 24000
            tts_engine._VIENEU_MODEL_FACTORY = lambda **kwargs: mock_engine

            res = run_vieneu_tts("Xin chào", "vi", "Trúc Ly", 1.0, model_path=tmpdir)
            self.assertEqual(res["model_name"], "vieneu-tts")
            self.assertGreater(res["measured_duration_ms"], 0)

    def test_vieneu_voice_not_in_catalog_fails_closed(self):
        """VieNeu snapshot with voices_v3_turbo.json missing requested voice fails closed."""
        with tempfile.TemporaryDirectory() as tmpdir:
            cat_dir = os.path.join(tmpdir, "src", "vieneu", "assets")
            os.makedirs(cat_dir, exist_ok=True)
            cat_file = os.path.join(cat_dir, "voices_v3_turbo.json")
            with open(cat_file, "w", encoding="utf-8") as f:
                json.dump({"presets": {"Phạm Tuyên": {"id": "Phạm Tuyên"}}}, f)
            os.makedirs(os.path.join(tmpdir, "moss_tokenizer"), exist_ok=True)

            with self.assertRaises(RuntimeError) as ctx:
                run_vieneu_tts("Xin chào", "vi", "Trúc Ly", 1.0, model_path=tmpdir)
            self.assertIn("TTS_VOICE_ASSET_MISSING", str(ctx.exception))
            self.assertIn("not found in VieNeu catalog", str(ctx.exception))
    def test_vieneu_backbone_repo_wiring_and_offline_env(self):
        """VieNeu wires snapshot to backbone_repo and moss_tokenizer, and sets offline environment variables."""
        with tempfile.TemporaryDirectory() as tmpdir:
            cat_dir = os.path.join(tmpdir, "src", "vieneu", "assets")
            os.makedirs(cat_dir, exist_ok=True)
            cat_file = os.path.join(cat_dir, "voices_v3_turbo.json")
            with open(cat_file, "w", encoding="utf-8") as f:
                json.dump({"presets": {"Trúc Ly": {"id": "Trúc Ly", "codes": [1, 2, 3], "speaker_emb": [0.1, 0.2]}}}, f)
            # Create a mock moss_tokenizer dir
            moss_dir = os.path.join(tmpdir, "moss_tokenizer")
            os.makedirs(moss_dir, exist_ok=True)

            captured_kwargs = {}
            mock_engine = MagicMock()
            mock_engine.infer.return_value = [0.0] * 24000
            def mock_factory(**kwargs):
                captured_kwargs.update(kwargs)
                return mock_engine

            tts_engine._VIENEU_MODEL_FACTORY = mock_factory
            run_vieneu_tts("Xin chào", "vi", "Trúc Ly", 1.0, model_path=tmpdir)

            self.assertEqual(captured_kwargs.get("backbone_repo"), tmpdir)
            self.assertEqual(captured_kwargs.get("moss_tokenizer"), moss_dir)
            self.assertEqual(os.environ.get("HF_HUB_OFFLINE"), "1")
            self.assertEqual(os.environ.get("TRANSFORMERS_OFFLINE"), "1")

    def test_vieneu_synthesis_voice_data_from_snapshot_catalog(self):
        """VieNeu preserves both speaker_emb and codes from snapshot catalog and does not call get_preset_voice."""
        with tempfile.TemporaryDirectory() as tmpdir:
            cat_dir = os.path.join(tmpdir, "src", "vieneu", "assets")
            os.makedirs(cat_dir, exist_ok=True)
            cat_file = os.path.join(cat_dir, "voices_v3_turbo.json")
            custom_preset = {
                "id": "Trúc Ly",
                "name": "Trúc Ly",
                "speaker_emb": [0.11, 0.22, 0.33],
                "codes": [42, 43, 44],
            }
            with open(cat_file, "w", encoding="utf-8") as f:
                json.dump({"presets": {"Trúc Ly": custom_preset}}, f)
            os.makedirs(os.path.join(tmpdir, "moss_tokenizer"), exist_ok=True)

            mock_engine = MagicMock()
            mock_engine.infer.return_value = [0.0] * 24000
            mock_engine.get_preset_voice = MagicMock(side_effect=AssertionError("get_preset_voice must NOT be called"))
            tts_engine._VIENEU_MODEL_FACTORY = lambda **kwargs: mock_engine

            run_vieneu_tts("Xin chào", "vi", "Trúc Ly", 1.0, model_path=tmpdir)
            mock_engine.infer.assert_called_once()
            _, call_kwargs = mock_engine.infer.call_args
            voice_arg = call_kwargs.get("voice")
            self.assertIsInstance(voice_arg, dict)
            self.assertEqual(voice_arg.get("speaker_emb"), [0.11, 0.22, 0.33])
            self.assertEqual(voice_arg.get("codes"), [42, 43, 44])
            mock_engine.get_preset_voice.assert_not_called()

    def test_vieneu_missing_moss_tokenizer_fails_closed(self):
        """VieNeu without mock factory fails closed if local MOSS tokenizer is missing from snapshot."""
        tts_engine._VIENEU_MODEL_FACTORY = None
        mock_vieneu_mod = MagicMock()
        orig_vieneu = sys.modules.get("vieneu")
        sys.modules["vieneu"] = mock_vieneu_mod
        try:
            with tempfile.TemporaryDirectory() as tmpdir:
                cat_dir = os.path.join(tmpdir, "src", "vieneu", "assets")
                os.makedirs(cat_dir, exist_ok=True)
                with open(os.path.join(cat_dir, "voices_v3_turbo.json"), "w", encoding="utf-8") as f:
                    json.dump({"presets": {"Trúc Ly": {"id": "Trúc Ly"}}}, f)

                with self.assertRaises(RuntimeError) as ctx:
                    run_vieneu_tts("Xin chào", "vi", "Trúc Ly", 1.0, model_path=tmpdir)
                self.assertIn("TTS_MODEL_ASSET_MISSING", str(ctx.exception))
                self.assertIn("verified local MOSS tokenizer missing in snapshot", str(ctx.exception))
        finally:
            if orig_vieneu is not None:
                sys.modules["vieneu"] = orig_vieneu
            else:
                sys.modules.pop("vieneu", None)
    def test_vieneu_real_factory_refuses_missing_model_path_before_import(self):
        """VieNeu with real upstream factory (_VIENEU_MODEL_FACTORY is None) refuses missing model_path before import."""
        tts_engine._VIENEU_MODEL_FACTORY = None
        with self.assertRaises(RuntimeError) as ctx:
            run_vieneu_tts("Xin chào", "vi", "Trúc Ly", 1.0, model_path=None)
        self.assertIn("WORKER_SNAPSHOT_PATH_REQUIRED", str(ctx.exception))
        self.assertIn("requires a verified local snapshot model_path", str(ctx.exception))
    def test_kokoro_local_only_model_path_required_without_factory(self):
        """Kokoro without model_path and without factory fails closed prohibiting Hub download."""
        tts_engine._KOKORO_MODEL_FACTORY = None
        mock_kokoro = MagicMock()
        orig_kokoro = sys.modules.get("kokoro")
        sys.modules["kokoro"] = mock_kokoro
        try:
            with self.assertRaises(RuntimeError) as ctx:
                run_kokoro_tts("Hello", "en", "af_heart", 1.0, model_path=None)
            self.assertIn("WORKER_SNAPSHOT_PATH_REQUIRED", str(ctx.exception))
            self.assertIn("Hub fallback is strictly prohibited", str(ctx.exception))
        finally:
            if orig_kokoro is not None:
                sys.modules["kokoro"] = orig_kokoro
            else:
                sys.modules.pop("kokoro", None)

    def test_kokoro_missing_config_fails_closed(self):
        """Kokoro with model_path missing config.json fails closed."""
        tts_engine._KOKORO_MODEL_FACTORY = None
        mock_kokoro = MagicMock()
        orig_kokoro = sys.modules.get("kokoro")
        sys.modules["kokoro"] = mock_kokoro
        with tempfile.TemporaryDirectory() as tmpdir:
            ckpt = os.path.join(tmpdir, "kokoro-v1_0.pth")
            with open(ckpt, "wb") as f:
                f.write(b"")
            v_dir = os.path.join(tmpdir, "voices")
            os.makedirs(v_dir, exist_ok=True)
            with open(os.path.join(v_dir, "af_heart.pt"), "wb") as f:
                f.write(b"")

            orig_sha = tts_engine.KOKORO_CHECKPOINT_SHA256
            tts_engine.KOKORO_CHECKPOINT_SHA256 = hashlib.sha256(b"").hexdigest()
            try:
                with self.assertRaises(RuntimeError) as ctx:
                    run_kokoro_tts("Hello", "en", "af_heart", 1.0, model_path=tmpdir)
                self.assertIn("TTS_MODEL_ASSET_MISSING", str(ctx.exception))
                self.assertIn("config.json missing", str(ctx.exception))
            finally:
                tts_engine.KOKORO_CHECKPOINT_SHA256 = orig_sha
                if orig_kokoro is not None:
                    sys.modules["kokoro"] = orig_kokoro
                else:
                    sys.modules.pop("kokoro", None)

    def test_kokoro_missing_weights_fails_closed(self):
        """Kokoro with model_path missing .pth weights fails closed."""
        tts_engine._KOKORO_MODEL_FACTORY = None
        mock_kokoro = MagicMock()
        orig_kokoro = sys.modules.get("kokoro")
        sys.modules["kokoro"] = mock_kokoro
        with tempfile.TemporaryDirectory() as tmpdir:
            with open(os.path.join(tmpdir, "config.json"), "w") as f:
                f.write("{}")
            v_dir = os.path.join(tmpdir, "voices")
            os.makedirs(v_dir, exist_ok=True)
            with open(os.path.join(v_dir, "af_heart.pt"), "wb") as f:
                f.write(b"")

            try:
                with self.assertRaises(RuntimeError) as ctx:
                    run_kokoro_tts("Hello", "en", "af_heart", 1.0, model_path=tmpdir)
                self.assertIn("TTS_MODEL_ASSET_MISSING", str(ctx.exception))
                self.assertIn("model weights (.pth) missing", str(ctx.exception))
            finally:
                if orig_kokoro is not None:
                    sys.modules["kokoro"] = orig_kokoro
                else:
                    sys.modules.pop("kokoro", None)
    def test_kokoro_local_pt_voice_forwarded(self):
        """Kokoro forwards verified local .pt voice path to KPipeline."""
        with tempfile.TemporaryDirectory() as tmpdir:
            with open(os.path.join(tmpdir, "config.json"), "w") as f:
                f.write("{}")
            ckpt = os.path.join(tmpdir, "kokoro-v1_0.pth")
            # Write dummy data and calculate sha256 to match KOKORO_CHECKPOINT_SHA256
            # For testing with factory, we can mock factory:
            v_dir = os.path.join(tmpdir, "voices")
            os.makedirs(v_dir, exist_ok=True)
            v_path = os.path.join(v_dir, "af_heart.pt")
            with open(v_path, "wb") as f:
                f.write(b"mock_voice_pt")

            # Mock weights with proper sha256
            with open(ckpt, "wb") as f:
                f.write(b"")
            # Monkey patch KOKORO_CHECKPOINT_SHA256 for test
            orig_sha = tts_engine.KOKORO_CHECKPOINT_SHA256
            tts_engine.KOKORO_CHECKPOINT_SHA256 = hashlib.sha256(b"").hexdigest()
            try:
                captured_call = {}
                class MockPipeline:
                    def __init__(self, lang_code="a"):
                        pass
                    def __call__(self, text, voice="af_heart", speed=1.0):
                        captured_call["voice"] = voice
                        yield MagicMock(audio=[0.0] * 24000)

                tts_engine._KOKORO_MODEL_FACTORY = MockPipeline
                run_kokoro_tts("Hello", "en", "af_heart", 1.0, model_path=tmpdir)
                self.assertEqual(captured_call.get("voice"), v_path)
            finally:
                tts_engine.KOKORO_CHECKPOINT_SHA256 = orig_sha
    def test_kokoro_parent_config_fallback_removed_fails_closed(self):
        """Kokoro with config.json only in parent directory must fail closed (boundary isolation)."""
        with tempfile.TemporaryDirectory() as parent_dir:
            child_dir = os.path.join(parent_dir, "child_snapshot")
            os.makedirs(child_dir, exist_ok=True)
            # Put config.json in parent only
            with open(os.path.join(parent_dir, "config.json"), "w") as f:
                f.write('{"model_type": "kokoro"}')
            valid_bytes = b"valid kokoro checkpoint bytes for testing boundary"
            orig_sha = tts_engine.KOKORO_CHECKPOINT_SHA256
            tts_engine.KOKORO_CHECKPOINT_SHA256 = hashlib.sha256(valid_bytes).hexdigest()
            try:
                ckpt = os.path.join(child_dir, "kokoro-v1_0.pth")
                with open(ckpt, "wb") as f:
                    f.write(valid_bytes)
                voices_dir = os.path.join(child_dir, "voices")
                os.makedirs(voices_dir, exist_ok=True)
                with open(os.path.join(voices_dir, "af_heart.pt"), "wb") as f:
                    f.write(b"voice data")

                with self.assertRaises(RuntimeError) as ctx:
                    run_kokoro_tts("Hello", "en", "af_heart", 1.0, model_path=child_dir)
                self.assertIn("config.json missing in snapshot", str(ctx.exception))
            finally:
                tts_engine.KOKORO_CHECKPOINT_SHA256 = orig_sha

    def test_vieneu_parent_moss_tokenizer_fallback_removed_fails_closed(self):
        """VieNeu with moss_tokenizer only in parent directory must fail closed (boundary isolation)."""
        with tempfile.TemporaryDirectory() as parent_dir:
            child_dir = os.path.join(parent_dir, "child_snapshot")
            os.makedirs(child_dir, exist_ok=True)
            # Put moss_tokenizer in parent only
            os.makedirs(os.path.join(parent_dir, "moss_tokenizer"), exist_ok=True)
            # Put valid catalog in child
            cat_dir = os.path.join(child_dir, "src", "vieneu", "assets")
            os.makedirs(cat_dir, exist_ok=True)
            with open(os.path.join(cat_dir, "voices_v3_turbo.json"), "w", encoding="utf-8") as f:
                json.dump({"presets": {"Trúc Ly": {"id": "Trúc Ly"}}}, f)

            tts_engine._VIENEU_MODEL_FACTORY = lambda **kwargs: MagicMock()
            with self.assertRaises(RuntimeError) as ctx:
                run_vieneu_tts("Xin chào", "vi", "Trúc Ly", 1.0, model_path=child_dir)
            self.assertIn("verified local MOSS tokenizer missing in snapshot", str(ctx.exception))
    def test_tts_outside_root_entrypoint_fails_closed(self):
        """TTS entrypoint file located outside model_path snapshot root must fail closed."""
        with tempfile.TemporaryDirectory() as parent_dir:
            root_dir = os.path.join(parent_dir, "root")
            outside_dir = os.path.join(parent_dir, "outside")
            os.makedirs(root_dir, exist_ok=True)
            os.makedirs(outside_dir, exist_ok=True)

            outside_file = os.path.join(outside_dir, "escaped_entrypoint.json")
            with open(outside_file, "w") as f:
                f.write("{}")

            with self.assertRaises(RuntimeError) as ctx:
                run_vieneu_tts("Xin chào", "vi", "Trúc Ly", 1.0, model_path=root_dir, entrypoint_file=outside_file)
            self.assertIn("escapes snapshot root", str(ctx.exception))

            outside_pt = os.path.join(outside_dir, "escaped_voice.pt")
            with open(outside_pt, "w") as f:
                f.write("pt")
            with self.assertRaises(RuntimeError) as ctx:
                run_kokoro_tts("Hello", "en", "af_heart", 1.0, model_path=root_dir, entrypoint_file=outside_pt)
            self.assertIn("escapes snapshot root", str(ctx.exception))
if __name__ == "__main__":
    unittest.main()
