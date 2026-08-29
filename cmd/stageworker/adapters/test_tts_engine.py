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
)


class TestTTSEngineUpstreamContracts(unittest.TestCase):
    def tearDown(self):
        tts_engine._VIENEU_MODEL_FACTORY = None
        tts_engine._COSYVOICE_MODEL_FACTORY = None
        tts_engine._KOKORO_MODEL_FACTORY = None
        tts_engine._CHATTERBOX_MODEL_FACTORY = None

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
            voice_id="phuong_nam",
            speed=1.0,
        )

        self.assertEqual(resp["measured_duration_ms"], 1000)
        self.assertEqual(resp["model_name"], "vieneu-tts")
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0], ("get_preset_voice", "phuong_nam"))
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
            run_vieneu_tts("Text", "vi", "voice", 1.0)
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

        # Hide packages from import system using mock.patch.dict
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
                run_vieneu_tts("Text", "vi", "voice", 1.0)
            self.assertIn("vieneu package not found", str(ctx.exception))

            with self.assertRaises(RuntimeError) as ctx:
                run_cosyvoice_tts("Text", "zh", "voice", 1.0)
            self.assertIn("cosyvoice package not found", str(ctx.exception))

            with self.assertRaises(RuntimeError) as ctx:
                run_kokoro_tts("Text", "en", "voice", 1.0)
            self.assertIn("kokoro package not found", str(ctx.exception))

            with self.assertRaises(RuntimeError) as ctx:
                run_chatterbox_tts("Text", "en", "voice", 1.0)
            self.assertIn("chatterbox package not found", str(ctx.exception))
    def test_cli_execution_via_stdin_stdout(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Create a mock vieneu.py module adhering to official Vieneu interface
            mock_module_path = os.path.join(tmpdir, "vieneu.py")
            with open(mock_module_path, "w", encoding="utf-8") as f:
                f.write(
                    """
class Vieneu:
    def __init__(self, mode="v3turbo"):
        self.mode = mode

    def get_preset_voice(self, voice_id):
        return voice_id

    def infer(self, text, voice=None):
        return [0.01] * 48000
"""
                )

            adapter_path = os.path.join(
                os.path.dirname(os.path.abspath(__file__)), "tts_engine.py"
            )

            req = {
                "text": "Kiểm tra CLI adapter",
                "model_name": "vieneu-tts",
                "model_version": "1.0.0",
                "language": "vi",
                "voice_id": "phuong_nam",
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
        with mock.patch("tts_engine.run_cosyvoice_tts") as mock_cosy, \
             mock.patch("tts_engine.run_vieneu_tts") as mock_vieneu, \
             mock.patch("tts_engine.run_kokoro_tts") as mock_kokoro:

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

            # 4. Default / unresolved model_name with language=vi routes to VieNeu
            mock_vieneu.reset_mock()
            res4 = run_tts({
                "text": "Xin chào",
                "model_name": "default",
                "language": "vi",
                "voice_id": "Thục Đoan",
                "speed": 1.0,
            })
            mock_vieneu.assert_called_once()


if __name__ == "__main__":
    unittest.main()
