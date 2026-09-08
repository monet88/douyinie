#!/usr/bin/env python3
"""
Unit tests for cmd/stageworker/adapters/translation_qwen3.py
Verifies Qwen3 translation adapter contracts, meaning preservation facts, and mock factory seams.
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import translation_qwen3


class TestTranslationQwen3Adapter(unittest.TestCase):
    def tearDown(self):
        translation_qwen3._TRANSLATION_MODEL_FACTORY = None

    def test_extract_facts(self):
        text = "SUPOR pot has 500ml capacity and cost 120 yuan"
        facts = translation_qwen3.extract_facts(text)
        self.assertIn("500", facts)
        self.assertIn("120", facts)
        self.assertIn("SUPOR", facts)

    def test_detect_negation(self):
        self.assertTrue(translation_qwen3.detect_negation("这不是我的错", "zh"))
        self.assertFalse(translation_qwen3.detect_negation("这是我的朋友", "zh"))
        self.assertTrue(translation_qwen3.detect_negation("không có vấn đề gì", "vi"))
        self.assertFalse(translation_qwen3.detect_negation("rất tốt", "vi"))

    def test_number_preservation_adverbial_bare_digit_not_quantity(self):
        # 一直 = "always" (adverbial single 一, NOT a quantity). The Go QA gate
        # excludes bare single-digit numerals without quantity context. The
        # Python adapter must mirror that; otherwise a valid translation
        # "luôn luôn" is wrongly judged number-corrupted and over-retried.
        self.assertTrue(translation_qwen3._verify_number_preservation("他一直在这里", "Anh ấy luôn luôn ở đây", "vi"))

    def test_number_preservation_real_quantity_still_enforced(self):
        # 两个人 = "two people" (bare 两 WITH quantity marker 个) must be enforced.
        self.assertFalse(translation_qwen3._verify_number_preservation("两个人来了", "Một người đến", "vi"))
        self.assertTrue(translation_qwen3._verify_number_preservation("两个人来了", "Hai người đến", "vi"))

    def test_number_preservation_quantified_with_other_digit_counts(self):
        # 三种 kind marker counts with non-一 digits (three kinds), so 三 is real.
        self.assertFalse(translation_qwen3._verify_number_preservation("三种方案", "Một loại phương án", "vi"))
        self.assertTrue(translation_qwen3._verify_number_preservation("三种方案", "Ba loại phương án", "vi"))

    def test_number_preservation_yi_kind_marker_not_quantity(self):
        # 一样 = "same" (yi-only non-quantity marker): 一 is not a quantity.
        self.assertTrue(translation_qwen3._verify_number_preservation("它们一样大", "Chúng to bằng nhau", "vi"))

    def test_number_preservation_multi_rune_numeral_unconditional(self):
        # 二十 is a multi-rune numeral, unconditional regardless of context.
        self.assertFalse(translation_qwen3._verify_number_preservation("二十个人", "Một người", "vi"))
        self.assertTrue(translation_qwen3._verify_number_preservation("二十个人", "Hai mươi người", "vi"))

    def test_ascii_brand_fragment_passthrough(self):
        # Any source with NO CJK (OCR brand/acronym fragment like "CH", "Chang",
        # or lowercase "ar") is not translatable content in a zh->target pipeline.
        # The adapter must pass it through verbatim instead of sending it to the
        # LLM (which hallucinates a negation sentence and trips the QA gate).
        import tempfile
        calls = []
        called = {"v": False}

        class NoCallLlama:
            def __init__(self, **kwargs):
                pass

            def create_chat_completion(self, **kwargs):
                called["v"] = True
                calls.append(kwargs)
                return {"choices": [{"message": {"content": "Tôi không biết"}}]}

        mod = sys.modules.get("llama_cpp")
        orig = mod
        if mod is not None:
            mod.Llama = NoCallLlama
        else:
            import types
            m = types.ModuleType("llama_cpp")
            m.Llama = NoCallLlama
            sys.modules["llama_cpp"] = m
        self.addCleanup(sys.modules.pop, "llama_cpp", None)
        tmp = tempfile.NamedTemporaryFile(suffix=".gguf", delete=False)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)

        for frag in ("CH", "ar", "Chang"):
            res = translation_qwen3.translate_segments_qwen3(
                [{"index": 0, "source_text": frag}],
                "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
                model_path=tmp.name,
            )
            self.assertEqual(res[0]["target_text"], frag)
        self.assertFalse(called["v"], "CJK-free source must not reach the LLM")

    def test_chinese_source_still_reaches_llm(self):
        # A real CJK source MUST go to the LLM (Qwen3-4B-Q4_K_M mock preserves it).
        import tempfile
        import types
        calls = []

        class GoodLlama:
            def __init__(self, **kwargs):
                pass

            def create_chat_completion(self, **kwargs):
                calls.append(kwargs)
                return {"choices": [{"message": {"content": "Xin chào"}}]}

        mod = types.ModuleType("llama_cpp")
        mod.Llama = GoodLlama
        sys.modules["llama_cpp"] = mod
        self.addCleanup(sys.modules.pop, "llama_cpp", None)
        tmp = tempfile.NamedTemporaryFile(suffix=".gguf", delete=False)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)

        res = translation_qwen3.translate_segments_qwen3(
            [{"index": 0, "source_text": "你好"}], "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
            model_path=tmp.name,
        )
        self.assertEqual(res[0]["target_text"], "Xin chào")
        self.assertGreaterEqual(len(calls), 1)

    def test_mock_factory(self):
        def mock_trans(segs, src, tgt, m_name, m_ver):
            return [
                {
                    "index": 0,
                    "source_text": "你好",
                    "target_text": "Xin chào",
                    "key_facts": [],
                    "negation_polarity": False,
                }
            ]

        translation_qwen3._TRANSLATION_MODEL_FACTORY = mock_trans
        res = translation_qwen3.translate_segments_qwen3(
            [{"index": 0, "source_text": "你好"}], "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M"
        )
        self.assertEqual(len(res), 1)
        self.assertEqual(res[0]["target_text"], "Xin chào")

    def test_fail_closed_without_model_or_factory(self):
        with self.assertRaises(RuntimeError) as ctx:
            translation_qwen3.translate_segments_qwen3(
                [{"index": 0, "source_text": "你好"}], "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M"
            )
        self.assertIn("missing or inaccessible", str(ctx.exception).lower())

    def test_local_execution_ignores_env_model_paths(self):
        os.environ["QWEN3_TRANSLATION_MODEL_PATH"] = "/unrelated/env/path.gguf"
        os.environ["DOUYINIE_SNAPSHOT_DIR"] = "/unrelated/snapshots"
        try:
            with self.assertRaises(RuntimeError) as ctx:
                translation_qwen3.translate_segments_qwen3(
                    [{"index": 0, "source_text": "你好"}], "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M"
                )
            self.assertIn("missing or inaccessible", str(ctx.exception).lower())
        finally:
            os.environ.pop("QWEN3_TRANSLATION_MODEL_PATH", None)
            os.environ.pop("DOUYINIE_SNAPSHOT_DIR", None)
    def test_rejects_directory_path(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmpdir:
            with self.assertRaises(RuntimeError) as ctx:
                translation_qwen3.translate_segments_qwen3(
                    [{"index": 0, "source_text": "你好"}],
                    "zh",
                    "vi",
                    "qwen3_4b_translator",
                    "Qwen3-4B-Q4_K_M",
                    model_path=tmpdir,
                )
            self.assertIn("is a directory", str(ctx.exception).lower())
            self.assertIn("exact gguf file required", str(ctx.exception).lower())

    def test_prompt_names_vietnamese_target_explicitly(self):
        prompt = translation_qwen3.build_translation_prompt("你好", "zh", "vi")
        self.assertIn("Vietnamese", prompt)
        self.assertIn("only the Vietnamese translation", prompt)
        self.assertIn("no other language", prompt)
        self.assertNotIn("to vi ", prompt)

    def test_prompt_names_english_target_explicitly(self):
        prompt = translation_qwen3.build_translation_prompt("你好", "zh", "en")
        self.assertIn("English", prompt)
        self.assertIn("only the English translation", prompt)
        self.assertIn("no other language", prompt)

    def test_prompt_preserves_named_and_unknown_languages(self):
        self.assertIn("Vietnamese", translation_qwen3.build_translation_prompt("x", "Chinese", "Vietnamese"))
        self.assertIn("Chinese", translation_qwen3.build_translation_prompt("x", "zh", "vi"))
        prompt = translation_qwen3.build_translation_prompt("x", "zh", "fr")
        self.assertIn("to fr", prompt)

    def _install_fake_llama(self, content):
        import tempfile
        import types
        calls = {}

        class FakeLlama:
            # No __call__: raw completion must not be used.
            def __init__(self, **kwargs):
                calls["init"] = kwargs

            def create_chat_completion(self, **kwargs):
                calls["chat"] = kwargs
                return {"choices": [{"message": {"content": content}}]}

        mod = types.ModuleType("llama_cpp")
        mod.Llama = FakeLlama
        sys.modules["llama_cpp"] = mod
        tmp = tempfile.NamedTemporaryFile(suffix=".gguf", delete=False)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)
        self.addCleanup(sys.modules.pop, "llama_cpp", None)
        return tmp.name, calls

    def test_chat_completion_semantics_and_no_think(self):
        model_path, calls = self._install_fake_llama("Xin chào")
        res = translation_qwen3.translate_segments_qwen3(
            [{"index": 0, "source_text": "你好"}],
            "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
            model_path=model_path,
        )
        chat = calls["chat"]
        self.assertEqual(chat["max_tokens"], 256)
        self.assertEqual(chat["temperature"], 0.7)
        self.assertEqual(chat["top_p"], 0.8)
        self.assertEqual(chat["top_k"], 20)
        self.assertEqual(chat["min_p"], 0.0)
        content = chat["messages"][0]["content"]
        self.assertIn("Vietnamese", content)
        self.assertTrue(content.rstrip().endswith("/no_think"))
        self.assertEqual(res[0]["target_text"], "Xin chào")
        self.assertEqual(res[0]["source_text"], "你好")

    def test_strip_think_wrapper(self):
        self.assertEqual(
            translation_qwen3.strip_think_wrapper("<think>\n\n</think>\n\nXin chào  "),
            "Xin chào",
        )
        self.assertEqual(
            translation_qwen3.strip_think_wrapper("<think>reasoning here</think>  Chào bạn"),
            "Chào bạn",
        )
        self.assertEqual(translation_qwen3.strip_think_wrapper("Xin chào"), "Xin chào")

    def test_chat_completion_think_wrapper_stripped_in_output(self):
        model_path, _ = self._install_fake_llama("<think></think>\nHai lần")
        res = translation_qwen3.translate_segments_qwen3(
            [{"index": 0, "source_text": "两次"}],
            "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
            model_path=model_path,
        )
        self.assertEqual(res[0]["target_text"], "Hai lần")

    def test_chat_completion_malformed_response_fails_closed(self):
        import types

        class BadLlama:
            def __init__(self, **kwargs):
                pass

            def create_chat_completion(self, **kwargs):
                return {"choices": []}

        mod = types.ModuleType("llama_cpp")
        mod.Llama = BadLlama
        sys.modules["llama_cpp"] = mod
        self.addCleanup(sys.modules.pop, "llama_cpp", None)
        import tempfile
        with tempfile.NamedTemporaryFile(suffix=".gguf", delete=False) as tmp:
            model_path = tmp.name
        self.addCleanup(os.unlink, model_path)
        with self.assertRaises(RuntimeError):
            translation_qwen3.translate_segments_qwen3(
                [{"index": 0, "source_text": "你好"}],
                "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
                model_path=model_path,
            )

    def test_chat_completion_empty_translation_fails_closed(self):
        model_path, _ = self._install_fake_llama("  <think></think>  ")
        with self.assertRaises(RuntimeError):
            translation_qwen3.translate_segments_qwen3(
                [{"index": 0, "source_text": "你好"}],
                "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
                model_path=model_path,
            )

    def test_translate_segments_retries_and_recovers_missing_numbers(self):
        import tempfile
        import types
        attempts = []

        class MultiAttemptLlama:
            def __init__(self, **kwargs):
                pass

            def create_chat_completion(self, **kwargs):
                temp = kwargs.get("temperature", 0.7)
                attempts.append(temp)
                if len(attempts) == 1:
                    # First attempt drops the quantity
                    return {"choices": [{"message": {"content": "Lộn nhào phía sau"}}]}
                # Second attempt preserves the quantity
                return {"choices": [{"message": {"content": "Sau hai tuần"}}]}

        mod = types.ModuleType("llama_cpp")
        mod.Llama = MultiAttemptLlama
        sys.modules["llama_cpp"] = mod
        self.addCleanup(sys.modules.pop, "llama_cpp", None)
        tmp = tempfile.NamedTemporaryFile(suffix=".gguf", delete=False)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)

        res = translation_qwen3.translate_segments_qwen3(
            [{"index": 24, "source_text": "后 空 翻 两 周"}],
            "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
            model_path=tmp.name,
        )
        self.assertEqual(len(res), 1)
        self.assertEqual(res[0]["target_text"], "Sau hai tuần")
        self.assertGreaterEqual(len(attempts), 2)

    def test_translate_segments_retries_and_recovers_missing_entities(self):
        import tempfile
        import types
        attempts = []

        class MultiAttemptLlamaEntity:
            def __init__(self, **kwargs):
                pass

            def create_chat_completion(self, **kwargs):
                temp = kwargs.get("temperature", 0.7)
                attempts.append(temp)
                if len(attempts) == 1:
                    # First attempt translates without brand name
                    return {"choices": [{"message": {"content": "Sản phẩm tốt"}}]}
                # Second attempt preserves the brand name with diacritics
                return {"choices": [{"message": {"content": "VörtexBrand"}}]}
        mod = types.ModuleType("llama_cpp")
        mod.Llama = MultiAttemptLlamaEntity
        sys.modules["llama_cpp"] = mod
        self.addCleanup(sys.modules.pop, "llama_cpp", None)
        tmp = tempfile.NamedTemporaryFile(suffix=".gguf", delete=False)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)

        res = translation_qwen3.translate_segments_qwen3(
            # A protected-ASCII brand alone is passed through verbatim by the
            # provider (no LLM call). To exercise the retry-recovery seam, the
            # brand must be embedded in translatable CJK text.
            [{"index": 0, "source_text": "VortexBrand 商品很好"}],
            "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
            model_path=tmp.name,
        )
        self.assertEqual(len(res), 1)
        self.assertEqual(res[0]["target_text"], "VörtexBrand")
        self.assertGreaterEqual(len(attempts), 2)
    def test_translate_segments_retries_and_recovers_inverted_negation(self):
        import tempfile
        import types
        attempts = []

        class MultiAttemptLlamaNegation:
            def __init__(self, **kwargs):
                pass

            def create_chat_completion(self, **kwargs):
                temp = kwargs.get("temperature", 0.7)
                attempts.append(temp)
                if len(attempts) == 1:
                    # First attempt hallucinates negation marker in target
                    return {"choices": [{"message": {"content": "Không có người"}}]}
                # Second attempt produces affirmative translation
                return {"choices": [{"message": {"content": "Số điện thoại rỗng"}}]}

        mod = types.ModuleType("llama_cpp")
        mod.Llama = MultiAttemptLlamaNegation
        sys.modules["llama_cpp"] = mod
        self.addCleanup(sys.modules.pop, "llama_cpp", None)
        tmp = tempfile.NamedTemporaryFile(suffix=".gguf", delete=False)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)

        res = translation_qwen3.translate_segments_qwen3(
            [{"index": 0, "source_text": "程喜空号"}],
            "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
            model_path=tmp.name,
        )
        self.assertEqual(len(res), 1)
        self.assertEqual(res[0]["target_text"], "Số điện thoại rỗng")
        self.assertGreaterEqual(len(attempts), 2)


    def test_ocr_garbled_fragment_passthrough(self):
        # Asset 7679392272936389915 visual-track region: "程喜空号酬人m合n?" is an
        # OCR artifact (CJK interleaved with isolated stray Latin letters m/n),
        # not translatable content. Sending it to the LLM makes the model
        # hallucinate a negative Vietnamese sentence, which the Go QA gate
        # correctly rejects (affirmative -> negative). The adapter must pass it
        # through verbatim so no fabricated polarity reaches the gate.
        import tempfile
        import types
        called = {"v": False}

        class NoCallLlamaOCR:
            def __init__(self, **kwargs):
                pass

            def create_chat_completion(self, **kwargs):
                called["v"] = True
                return {"choices": [{"message": {"content": "Không có gì"}}]}

        mod = types.ModuleType("llama_cpp")
        mod.Llama = NoCallLlamaOCR
        sys.modules["llama_cpp"] = mod
        self.addCleanup(sys.modules.pop, "llama_cpp", None)
        tmp = tempfile.NamedTemporaryFile(suffix=".gguf", delete=False)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)

        res = translation_qwen3.translate_segments_qwen3(
            [{"index": 0, "source_text": "程喜空号酬人m合n?"}],
            "zh", "vi", "qwen3_4b_translator", "Qwen3-4B-Q4_K_M",
            model_path=tmp.name,
        )
        self.assertEqual(len(res), 1)
        self.assertEqual(res[0]["target_text"], "程喜空号酬人m合n?")
        self.assertFalse(called["v"], "OCR-garbled source must not reach the LLM")
        self.assertFalse(res[0]["negation_polarity"], "OCR-garbled source is not negation")

    def test_ocr_garbled_fragment_detector(self):
        self.assertTrue(translation_qwen3._is_ocr_garbled_fragment("程喜空号酬人m合n?"))
        self.assertTrue(translation_qwen3._is_ocr_garbled_fragment("SoedDoldo 你好m"))
        # Coherent brand/product runs with CJK are NOT garbage
        self.assertFalse(translation_qwen3._is_ocr_garbled_fragment("SUPOR电饭煲"))
        self.assertFalse(translation_qwen3._is_ocr_garbled_fragment("iPhone拍摄"))
        # Pure CJK is not garbage
        self.assertFalse(translation_qwen3._is_ocr_garbled_fragment("程喜空号"))
        self.assertFalse(translation_qwen3._is_ocr_garbled_fragment("没吃完可以这样密封起来"))
        # Pure-ASCII handled by _has_no_cjk, not here
        self.assertFalse(translation_qwen3._is_ocr_garbled_fragment("WrdSefdo"))


if __name__ == "__main__":
    unittest.main()
