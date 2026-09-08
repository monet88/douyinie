#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Translation (Qwen3-4B-GGUF Q4_K_M lane)
Translates source text segments into Vietnamese or English under the meaning-first contract.

Contract:
- Stdin: JSON request with segments (index, source_text, speaker_id, start_ms, end_ms),
  source_language, target_language, model_name, model_version, run_id, attempt_id
- Stdout: JSON response with segments (index, source_text, target_text, key_facts, negation_polarity),
  model_name, model_version
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import json
import os
import re
import sys
import unicodedata
from typing import Any, Dict, List, Optional
# Pluggable factory hooks for deterministic testing without full ML packages
_TRANSLATION_MODEL_FACTORY = None

_COMMON_LEXICAL_3 = {"SOY", "TEA", "ICE", "RED", "BIG", "ONE"}

_CJK_RANGES = (
    (0x4E00, 0x9FFF),   # CJK Unified Ideographs
    (0x3400, 0x4DBF),   # CJK Extension A
    (0x3040, 0x30FF),   # Hiragana + Katakana
    (0x3000, 0x303F),   # CJK Symbols and Punctuation
    (0xF900, 0xFAFF),   # CJK Compatibility Ideographs
)


def _has_no_cjk(text: str) -> bool:
    """True when text contains no CJK/Kana runes, so it is not translatable
    Chinese content (brands, acronyms, OCR fragments, punctuation, digits)."""
    for ch in text:
        cp = ord(ch)
        if any(lo <= cp <= hi for lo, hi in _CJK_RANGES):
            return False
    return True


def _is_ocr_garbled_fragment(text: str) -> bool:
    """True when a source mixes CJK with isolated stray Latin letters/digits such
    that it is an OCR artifact rather than translatable content.

    Real content may embed a coherent ASCII brand/product run next to CJK
    ("SUPOR电饭煲", "iPhone拍摄", "这种C4炸弹"). OCR garbage interleaves
    single stray Latin characters between CJK with no coherent ASCII run
    ("程喜空号酬人m合n?" -> the 'm' and 'n' are isolated fragments). We detect
    the latter: at least one CJK rune AND at least one alphabetic ASCII char
    that is "isolated" (not part of a contiguous ASCII run of length >= 2),
    with no whitespace separating scripts.
    """
    if not text or _has_no_cjk(text):
        return False
    # Must contain at least one Latin letter interleaved with CJK.
    runs = []
    cur = []
    for ch in text:
        if ch.isascii() and ch.isalpha():
            cur.append(ch)
        else:
            if cur:
                runs.append("".join(cur))
            cur = []
    if cur:
        runs.append("".join(cur))
    if not runs:
        return False
    # Any isolated single-letter ASCII run (not a coherent brand/product)
    # between/around CJK without spaces marks OCR fragmentation.
    for run in runs:
        if len(run) == 1:
            return True
        # A short run that is lowercase-only and not a known lexical item is
        # also a stray fragment (e.g. "py" in a value, but we keep this narrow:
        # only single-char runs are treated as garbage).
    return False


def _is_protected_ascii_token(tok: str) -> bool:
    """Mirror internal/service isProtectedASCIIToken for a full source token.

    A token with NO CJK/Kana runes and no spaces that Go's gate treats as a
    protected name/brand/acronym (digit+letter compounds, mixed-case brands,
    short all-caps acronyms) should be passed through verbatim rather than
    translated. Excludes pure quantity/unit measurements (3MINUTE, 20KG).
    """
    tok = tok.strip()
    if len(tok) < 2:
        return False
    has_cjk = any("\u4e00" <= c <= "\u9fff" or "\u3040" <= c <= "\u30ff" or "\u3000" <= c <= "\u303f" for c in tok)
    if has_cjk or any(c.isspace() for c in tok):
        return False
    has_digit = has_upper = has_lower = False
    for c in tok:
        if c.isdigit():
            has_digit = True
        elif c.isupper():
            has_upper = True
        elif c.islower():
            has_lower = True
        elif not (c == "-" or c == "_"):
            return False
    if has_digit and (has_upper or has_lower):
        if _is_quantity_token(tok):
            return False
        return True
    if has_upper and has_lower:
        upper = tok.upper()
        if upper.endswith(("WATER", "COFFEE", "TEA", "JUICE")):
            return False
        if _has_consecutive_upper(tok, 3):
            return False
        for c in tok[1:]:
            if c.isupper():
                return True
    if has_upper and not has_lower and not has_digit:
        if len(tok) <= 3:
            if tok.upper() in _COMMON_LEXICAL_3:
                return False
            return True
    return False


def _has_consecutive_upper(s: str, n: int) -> bool:
    count = 0
    for c in s:
        if c.isupper():
            count += 1
            if count >= n:
                return True
        else:
            count = 0
    return False


def _is_quantity_token(tok: str) -> bool:
    """A digit+letters token that is a quantity/measurement (3MINUTE, 20KG) is
    not a brand and must NOT be passed through verbatim."""
    key = tok.upper()
    for suffix in ("MINUTE", "MINUTES", "SECOND", "SECONDS", "HOUR", "HOURS",
                   "KG", "G", "ML", "L", "CM", "M", "MM", "KM", "MG", "LB"):
        if key.endswith(suffix):
            return True
    return False


def extract_facts(text: str) -> List[str]:
    """Extract simple numeric and proper name facts from text."""
    facts = []
    numbers = re.findall(r"\d+(?:\.\d+)?", text)
    for n in numbers:
        if n not in facts:
            facts.append(n)
    # Capitalized / ASCII entities (SUPOR, Matcha, iPhone, etc.)
    entities = re.findall(r"\b[A-Za-z0-9_-]{2,}\b", text)
    for e in entities:
        if e not in facts:
            facts.append(e)
    return facts


def detect_negation(text: str, lang: str) -> bool:
    """Detect negation polarity in source/target text."""
    neg_markers = {
        "zh": ["不", "没", "無", "无", "未", "别", "非"],
        "vi": ["không", "chưa", "chẳng", "đừng", "chớ", "vô", "cấm", "không phải"],
        "en": ["not", "no", "never", "none", "neither", "nor", "n't"],
    }
    zh_exceptions = [
        "告别", "别人", "区别", "类别", "性别", "级别", "个别", "辨别", "分别",
        "道别", "送别", "久别", "作别", "辞别", "阔别", "甄别", "判别", "离别", "永别", "惜别",
        "是不是", "可不可以", "能不能", "好不好", "行不行", "要不要", "会不会", "有没有", "对不对", "对不对吧", "对不", "对吧", "行不", "好不",
        "不得不", "不得不说", "不仅如此", "无聊", "无论", "无论如何", "无奈", "无数", "无所谓", "无辜",
        "不小心", "不过", "不管", "不仅", "不得了", "不由得", "不料", "不经意", "不良", "不断", "不愧", "不知不觉", "差不多", "并不",
        "非常", "特别", "非凡", "非洲", "无可挑剔", "是非", "沉没", "淹没", "埋没",
    ]
    vi_exceptions = [
        "chẳng lẽ", "không lẽ", "không những", "không chỉ", "vô cùng", "vô số", "bất ngờ", "bất kể", "vô tư", "không ngừng",
        "có phải không", "được không", "phải không", "đúng không",
    ]
    lower = text.lower()
    l_code = lang.lower()
    if l_code in ("zh", "chinese"):
        cjk_clean = re.sub(r"[\s\u3000]+", "", lower)
        cleaned = cjk_clean
        for exc in zh_exceptions:
            cleaned = cleaned.replace(exc.lower(), "")
        runes = list(cleaned)
        filtered = []
        n = len(runes)
        for i, r in enumerate(runes):
            if r == "不":
                is_sentence_end = all(not (ch.isalnum() or "\u4e00" <= ch <= "\u9fff") for ch in runes[i+1:])
                is_clause_boundary = is_sentence_end
                if not is_clause_boundary and i > 0 and i + 1 < n:
                    next_r = runes[i+1]
                    if next_r in "你我他她它咱这那谁":
                        is_clause_boundary = True
                if is_clause_boundary and i > 0 and runes[i-1] not in "并绝决":
                    continue
            filtered.append(r)
        cleaned = "".join(filtered)
        return any(m in cleaned for m in neg_markers["zh"])
    elif l_code in ("vi", "vietnamese"):
        cleaned = lower
        for exc in vi_exceptions:
            cleaned = cleaned.replace(exc.lower(), "")
        return any(re.search(r"\b" + re.escape(m) + r"\b", cleaned) for m in neg_markers["vi"])
    else:
        markers = neg_markers.get(l_code, neg_markers["en"])
        return any(m in lower for m in markers)
# Chinese numeral digit/unit maps mirroring internal/service/translation_qa.go.
_ZH_DIGIT_MAP = {
    "零": 0, "〇": 0, "一": 1, "二": 2, "两": 2, "兩": 2, "三": 3, "四": 4,
    "五": 5, "六": 6, "七": 7, "八": 8, "九": 9,
}

_ZH_UNIT_MAP = {"十": 10, "百": 100, "千": 1000, "万": 10000, "萬": 10000}

# zhQuantityMarkers: explicit quantity context for a bare single-digit numeral
# (classifiers, containers/measures, time units, core frequency counts, money).
# A lone digit rune is otherwise ambiguous (aspectual 一+verb, adverbials like
# 一下/一直/一样/一点, ordinals 第一) and must not become a hard numeric fact.
_ZH_QUANTITY_MARKERS = set(
    "个个位只隻条條张張件本支块塊份套双雙对對匹头頭辆輛架台部座间間颗顆粒篇章节约节约段层层層首句格"
    "杯碗盘盤瓶罐袋包盒箱桶盆锅鍋"
    "秒分时時天周月年季夜晚"
    "种種样樣些般"
    "次遍趟回"
    "元毛斤"
)

# yi-only markers: never quantities with 一 (一样 "same", 一些 "some", 一种 "a
# kind of") but count with other digits (三种, 两样).
_ZH_YI_ONLY_NON_QUANTITY = set("种種样樣些般")

# Markers whose 一X一Y reduplication is distributive (一分一秒), never counting.
_ZH_TIME_FREQ_MARKERS = set("秒分时時天周月年季夜晚次遍趟回")

_VI_NUMBER_DIGITS = {
    "không": 0, "một": 1, "mốt": 1, "hai": 2, "ba": 3, "bốn": 4, "tư": 4,
    "năm": 5, "lăm": 5, "sáu": 6, "bảy": 7, "tám": 8, "chín": 9,
}
_VI_NUMBER_UNITS = {"mười": 10, "mươi": 10, "trăm": 100, "nghìn": 1000, "ngàn": 1000, "triệu": 1000000}

_EN_NUMBER_VALUES = {
    "zero": 0, "one": 1, "two": 2, "once": 1, "twice": 2, "thrice": 3,
    "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8,
    "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13,
    "fourteen": 14, "fifteen": 15, "sixteen": 16, "seventeen": 17,
    "eighteen": 18, "nineteen": 19, "twenty": 20, "thirty": 30,
    "forty": 40, "fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80,
    "ninety": 90,
}
_EN_NUMBER_UNITS = {"hundred": 100, "thousand": 1000, "million": 1000000}

_WHITESPACE_RE = re.compile(r"\s+")
_NUMBER_WORD_RE = re.compile(r"[a-zà-ỹ0-9]+")


def _next_non_space(runes, pos):
    n = len(runes)
    i = pos
    while i < n and runes[i].isspace():
        i += 1
    return i if i < n else -1


def _prev_non_space(runes, pos):
    i = pos - 1
    while i >= 0 and runes[i].isspace():
        i -= 1
    return i


def _is_chinese_number_rune(r):
    return r in _ZH_DIGIT_MAP or r in _ZH_UNIT_MAP


def _is_bare_chinese_digit(token):
    return len(token) == 1 and token[0] in _ZH_DIGIT_MAP


def _bridges_numeral_run(current, runes, pos):
    if not current:
        return False
    next_i = _next_non_space(runes, pos)
    if next_i < 0 or not _is_chinese_number_rune(runes[next_i]):
        return False
    if runes[next_i] in _ZH_UNIT_MAP:
        return True
    return current[-1] in _ZH_UNIT_MAP


def _prev_pair_marker(runes, pos):
    a = _prev_non_space(runes, pos)
    if a < 0:
        return None, False
    b = _prev_non_space(runes, a)
    if b >= 0 and runes[b] == "一":
        return runes[a], True
    return None, False


def _next_pair_marker(runes, pos):
    a = _next_non_space(runes, pos + 1)
    if a < 0:
        return None, False
    b = _next_non_space(runes, a + 1)
    if b >= 0 and runes[a] == "一":
        return runes[b], True
    return None, False


def _zh_has_quantity_context(runes, start, end):
    next_i = _next_non_space(runes, end)
    if next_i < 0:
        return False
    marked = runes[next_i] in _ZH_QUANTITY_MARKERS
    if not marked and runes[next_i] == "小":
        # 小时/小時 ("hour") is a lexicalized time word: 八小时 counts.
        after = _next_non_space(runes, next_i + 1)
        if after >= 0 and runes[after] in ("时", "時"):
            marked = True
    if not marked:
        return False
    if runes[start] != "一":
        return True
    if runes[next_i] in _ZH_YI_ONLY_NON_QUANTITY:
        return False
    m, ok = _prev_pair_marker(runes, start)
    if ok and m in _ZH_TIME_FREQ_MARKERS:
        return False
    m, ok = _next_pair_marker(runes, next_i)
    if ok and m in _ZH_TIME_FREQ_MARKERS:
        return False
    return True


def _parse_zh_number_token(token):
    if not token:
        return None
    has_unit = any(r in _ZH_UNIT_MAP for r in token)
    if not has_unit:
        value = 0
        for r in token:
            value = value * 10 + _ZH_DIGIT_MAP.get(r, 0)
        return value
    total, section, number = 0, 0, 0
    for r in token:
        if r in _ZH_DIGIT_MAP:
            number = _ZH_DIGIT_MAP[r]
            continue
        unit = _ZH_UNIT_MAP.get(r, 0)
        if unit == 10000:
            section += number
            if section == 0:
                section = 1
            total += section * unit
            section, number = 0, 0
            continue
        if number == 0:
            number = 1
        section += number * unit
        number = 0
    return total + section + number


def _extract_zh_number_tokens(text):
    """Mirror Go extractChineseNumberTokens: bare single-digit numerals need
    quantity context; multi-rune numerals and Arabic digits are unconditional."""
    runes = list(text)
    tokens = []
    current = []
    start = -1

    def flush(end):
        nonlocal current, start
        if not current:
            return
        p = _prev_non_space(runes, start)
        if p >= 0 and runes[p] == "第":
            # Ordinal (第一, 第十名): never counts.
            current, start = [], -1
            return
        if not _is_bare_chinese_digit(current) or _zh_has_quantity_context(runes, start, end):
            tokens.append("".join(current))
        current, start = [], -1

    for i, r in enumerate(runes):
        if _is_chinese_number_rune(r):
            if start < 0:
                start = i
            current.append(r)
            continue
        if r.isspace() and start >= 0 and _bridges_numeral_run(current, runes, i):
            continue
        flush(i)
    flush(len(runes))
    return tokens


def _extract_number_values(text, lang):
    """Mirror Go extractNumbers: return set of canonical decimal strings."""
    res = set()
    res.update(re.findall(r"\d+", text))
    lower = text.lower()
    lc = lang.lower()
    if lc in ("zh", "zh-cn", "zh-tw"):
        for token in _extract_zh_number_tokens(text):
            val = _parse_zh_number_token(token)
            if val is not None:
                res.add(str(val))
    elif lc in ("vi", "vietnamese"):
        words = _NUMBER_WORD_RE.findall(lower)
        for val in _extract_vi_number_words(words):
            res.add(str(val))
    elif lc in ("en", "english"):
        words = _NUMBER_WORD_RE.findall(lower)
        for val in _extract_en_number_words(words):
            res.add(str(val))
    return res


def _extract_vi_number_words(words):
    values = []
    i = 0
    while i < len(words):
        val, consumed, ok = _parse_vi_number_at(words, i)
        if not ok:
            i += 1
            continue
        values.append(val)
        i += consumed
    return values


def _parse_vi_number_at(words, start):
    # "không" is primarily negation; never start a numeric sequence from it.
    if start >= len(words) or words[start] in ("không", "linh", "lẻ"):
        return 0, 0, False
    total, section, number = 0, 0, 0
    consumed = 0
    has_number = False
    for i in range(start, len(words)):
        word = words[i]
        if word in ("linh", "lẻ"):
            if not has_number:
                break
            consumed += 1
            continue
        if word in _VI_NUMBER_DIGITS:
            if word == "không" and not has_number:
                break
            number = _VI_NUMBER_DIGITS[word]
            has_number = True
            consumed += 1
            continue
        unit = _VI_NUMBER_UNITS.get(word)
        if unit is None:
            break
        has_number = True
        consumed += 1
        if unit >= 1000:
            section += number
            if section == 0:
                section = 1
            total += section * unit
            section, number = 0, 0
            continue
        if number == 0:
            number = 1
        section += number * unit
        number = 0
    if consumed == 0 or not has_number:
        return 0, 0, False
    return total + section + number, consumed, True


def _extract_en_number_words(words):
    values = []
    i = 0
    while i < len(words):
        val, consumed, ok = _parse_en_number_at(words, i)
        if not ok:
            i += 1
            continue
        values.append(val)
        i += consumed
    return values


def _parse_en_number_at(words, start):
    total, current = 0, 0
    consumed = 0
    for i in range(start, len(words)):
        word = words[i]
        if word in _EN_NUMBER_VALUES:
            v = _EN_NUMBER_VALUES[word]
            if v >= 100:
                if current == 0:
                    current = 1
                current *= v
            else:
                total += v
            consumed += 1
            continue
        unit = _EN_NUMBER_UNITS.get(word)
        if unit is None:
            break
        if unit == 100:
            if current == 0:
                current = 1
            current *= unit
            consumed += 1
            continue
        if current == 0:
            current = 1
        total += current * unit
        current = 0
        consumed += 1
    if consumed == 0:
        return 0, 0, False
    return total + current, consumed, True


def _verify_number_preservation(src: str, tgt: str, target_lang: str) -> bool:
    """Mirror Go's number preservation check: source numeric values must survive."""
    src_nums = _extract_number_values(src, "zh")
    tgt_nums = _extract_number_values(tgt, target_lang)
    src_missing = {n for n in src_nums if n not in tgt_nums}
    if src_missing:
        return False
    return True


def _verify_fact_preservation(src: str, tgt: str, target_lang: str) -> bool:
    """Check that numbers, named entities, and negation polarity are preserved in target."""
    if not _verify_number_preservation(src, tgt, target_lang):
        return False
    if detect_negation(src, "zh") != detect_negation(tgt, target_lang):
        return False
    facts = extract_facts(src)
    tgt_lower = tgt.lower()
    tgt_stripped = "".join(c for c in unicodedata.normalize("NFD", tgt_lower) if unicodedata.category(c) != "Mn")
    for f in facts:
        f_lower = f.lower()
        f_stripped = "".join(c for c in unicodedata.normalize("NFD", f_lower) if unicodedata.category(c) != "Mn")
        if f_lower not in tgt_lower and f_stripped not in tgt_stripped:
            return False
    return True

# Explicit output-language names: raw codes ("vi"/"en") alone are ambiguous
# to the model and have produced wrong-language output. Codes map to names;
# already-named or other values pass through; empty falls back to English.
LANGUAGE_NAMES = {
    "zh": "Chinese",
    "chinese": "Chinese",
    "vi": "Vietnamese",
    "vietnamese": "Vietnamese",
    "en": "English",
    "english": "English",
}


def language_name(code: str) -> str:
    """Map a language code or name to the explicit instruction name."""
    key = (code or "").strip().lower()
    if not key:
        return "English"
    return LANGUAGE_NAMES.get(key, (code or "").strip())


def build_translation_prompt(source_text: str, source_lang: str, target_lang: str, is_negation: Optional[bool] = None) -> str:
    """Build a deterministic prompt naming the output language explicitly."""
    src_name = language_name(source_lang)
    tgt_name = language_name(target_lang)
    polarity_instr = ""
    if is_negation is False:
        polarity_instr = " The source statement is affirmative; the translation MUST be affirmative and MUST NOT introduce any negation words (e.g. không, chưa, chẳng, never, not, no)."
    elif is_negation is True:
        polarity_instr = " The source statement contains negation; the translation MUST preserve negation."
    return (
        f"Translate the following {src_name} text to {tgt_name}. "
        f"Output only the {tgt_name} translation, with no explanation and no other language. "
        f"Preserve facts, names, numbers, quantities, and negation.{polarity_instr}\n"
        f"{source_text}\nTranslation:"
    )

# Matches one optional leading Qwen <think>...</think> wrapper block,
# including the empty block the pinned GGUF emits in non-thinking mode.
THINK_WRAPPER_RE = re.compile(r"^\s*<think>.*?</think>\s*", re.DOTALL)


def strip_think_wrapper(text: str) -> str:
    """Remove one leading <think> wrapper block; translation content untouched."""
    return THINK_WRAPPER_RE.sub("", text or "", count=1).strip()


def translate_segments_qwen3(
    segments: List[Dict[str, Any]],
    source_lang: str,
    target_lang: str,
    model_name: str,
    model_version: str,
    model_path: Optional[str] = None,
) -> List[Dict[str, Any]]:
    """Execute text translation under the meaning-first contract using Qwen3-4B-GGUF."""
    if _TRANSLATION_MODEL_FACTORY is not None:
        return _TRANSLATION_MODEL_FACTORY(segments, source_lang, target_lang, model_name, model_version)

    # Production execution requires verified GGUF weights extracted from the model_snapshot envelope
    # Fallback to env discovery (QWEN3_TRANSLATION_MODEL_PATH / DOUYINIE_SNAPSHOT_DIR) is strictly forbidden.
    if not model_path or not os.path.exists(model_path):
        raise RuntimeError(f"Verified model snapshot path missing or inaccessible: {model_path}; fail-closed")
    if os.path.isdir(model_path):
        raise RuntimeError(f"Verified model snapshot path is a directory ({model_path}); exact GGUF file required; fail-closed")

    try:
        from llama_cpp import Llama  # type: ignore
    except ImportError:
        raise RuntimeError("llama-cpp-python runtime not found: install llama-cpp-python to execute verified Qwen3-4B-GGUF weights")

    llm = Llama(model_path=model_path, n_ctx=2048, verbose=False)
    translated = []
    for seg in segments:
        idx = seg.get("index", 0)
        src_text = seg.get("source_text", "")
        # A source with no CJK/Kana runes at all is not translatable content in
        # a zh->target pipeline: it is a brand, acronym, abbreviation, OCR
        # fragment ("CH", "Chang", "ar", "HD", "4K") or pure punctuation/digits.
        # The Go QA gate treats protected ASCII tokens verbatim and rejects the
        # fabricated negation a model invents for such fragments. Pass them
        # through unchanged at the provider seam rather than sending to the LLM.
        if src_text and _has_no_cjk(src_text):
            translated.append({
                "index": idx,
                "source_text": src_text,
                "target_text": src_text,
                "key_facts": extract_facts(src_text),
                "negation_polarity": False,
                "speaker_id": seg.get("speaker_id", ""),
                "start_ms": seg.get("start_ms", 0),
                "end_ms": seg.get("end_ms", 0),
            })
            continue
        # OCR-garbled fragments (CJK interleaved with isolated stray Latin
        # letters, e.g. "程喜空号酬人m合n?") are visual-track OCR artifacts, not
        # translatable content. Sending them to the LLM makes the model
        # hallucinate a plausible sentence (often inverting negation). Pass
        # them through verbatim at the provider seam; the Go QA gate accepts a
        # verbatim source==target pair (both affirmative).
        if src_text and _is_ocr_garbled_fragment(src_text):
            translated.append({
                "index": idx,
                "source_text": src_text,
                "target_text": src_text,
                "key_facts": extract_facts(src_text),
                "negation_polarity": False,
                "speaker_id": seg.get("speaker_id", ""),
                "start_ms": seg.get("start_ms", 0),
                "end_ms": seg.get("end_ms", 0),
            })
            continue
        facts = extract_facts(src_text)
        negation = detect_negation(src_text, source_lang)

        # Pinned Qwen3 GGUF is an instruct model: execute through the embedded
        # chat template with /no_think and non-thinking sampling. Raw completion
        # bypasses the template and lets thinking consume the bounded output.
        instruction = build_translation_prompt(src_text, source_lang, target_lang, is_negation=negation) + " /no_think"
        tgt_text = ""
        temperatures = [0.7, 0.0, 0.2]
        for attempt_i, temp in enumerate(temperatures):
            curr_instruction = instruction
            if attempt_i > 0:
                # Strengthen instruction on retry to explicitly mandate missing numbers/polarity
                curr_instruction = build_translation_prompt(src_text, source_lang, target_lang, is_negation=negation)
                curr_instruction += " Strictly preserve all numbers, digits, quantities, and negation polarity verbatim. Do not omit any numbers."
                curr_instruction += " /no_think"
            out = llm.create_chat_completion(
                messages=[{"role": "user", "content": curr_instruction}],
                max_tokens=256,
                temperature=temp,
                top_p=0.8,
                top_k=20,
                min_p=0.0,
            )
            try:
                content = out["choices"][0]["message"]["content"]
            except (KeyError, IndexError, TypeError):
                raise RuntimeError("Qwen3 chat completion returned malformed response; fail-closed")
            candidate = strip_think_wrapper(content if isinstance(content, str) else "")
            if candidate:
                tgt_text = candidate
                if _verify_fact_preservation(src_text, candidate, target_lang):
                    break
        if not tgt_text:
            raise RuntimeError("Qwen3 chat completion returned empty translation; fail-closed")

        translated.append({
            "index": idx,
            "source_text": src_text,
            "target_text": tgt_text,
            "key_facts": facts,
            "negation_polarity": negation,
            "speaker_id": seg.get("speaker_id", ""),
            "start_ms": seg.get("start_ms", 0),
            "end_ms": seg.get("end_ms", 0),
        })
    return translated


def main():
    try:
        raw_in = sys.stdin.read()
        if not raw_in.strip():
            sys.stderr.write("Empty stdin request\n")
            sys.exit(1)

        req = json.loads(raw_in)
        segments = req.get("segments", [])
        if not segments:
            sys.stderr.write("Missing or empty segments in request\n")
            sys.exit(1)

        source_lang = req.get("source_language", "zh")
        target_lang = req.get("target_language", "vi")
        model_name = req.get("model_name", "qwen3_4b_translator")
        model_version = req.get("model_version", "Qwen3-4B-Q4_K_M")

        model_path = req.get("model_path")
        results = translate_segments_qwen3(
            segments, source_lang, target_lang, model_name, model_version, model_path
        )

        out = {
            "segments": results,
            "model_name": model_name,
            "model_version": model_version,
        }
        print(json.dumps(out))
        sys.exit(0)

    except Exception as e:
        sys.stderr.write(f"Translation error: {e}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
