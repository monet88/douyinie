#!/usr/bin/env python3
"""
Douyinie Lightweight ONNX Model Suite Downloader & Integrity Verifier
Downloads and verifies SHA-256 checksums for SenseVoice-Small, CAM++, Silero VAD, and RapidOCR.

Usage:
    python scripts/download_models.py [--target-dir models] [--verify-only] [--dry-run]
"""

import argparse
import hashlib
import json
import os
import sys
import urllib.request
from typing import Dict, Any, List

DEFAULT_MANIFEST: List[Dict[str, Any]] = [
    {
        "id": "sensevoice_int8",
        "name": "SenseVoice-Small INT8 ONNX",
        "category": "asr",
        "filename": "sensevoice-small/model.int8.onnx",
        "size_bytes": 239243764,
        "sha256": "3046f882419409ce81bc0939b4b0e515e01b7a2d67ec1655ad5aa21fcff8548c",
        "urls": [
            "https://huggingface.co/csukuangfj/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/resolve/main/model.int8.onnx",
            "https://modelscope.cn/api/v1/models/damo/SenseVoiceSmall/repo?Revision=master&FilePath=model.int8.onnx",
        ],
    },
    {
        "id": "sensevoice_tokens",
        "name": "SenseVoice Tokens Dictionary",
        "category": "asr",
        "filename": "sensevoice-small/tokens.txt",
        "size_bytes": 1421045,
        "sha256": "4b68e9b6a12bfa4c585c5dfa1441864a781b0a8f9024f0cba1e755fae2f49774",
        "urls": [
            "https://huggingface.co/csukuangfj/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/resolve/main/tokens.txt",
        ],
    },
    {
        "id": "campplus_onnx",
        "name": "CAM++ Speaker Embedding ONNX",
        "category": "diarizer",
        "filename": "diarizer/campplus.onnx",
        "size_bytes": 28413692,
        "sha256": "5c1f5449aa5b62b16194bcf9db77fa2ec53ee2fa42ee5a2a67746cb9833777f9",
        "urls": [
            "https://huggingface.co/csukuangfj/sherpa-onnx-campplus-sv-zh-cn/resolve/main/campplus.onnx",
        ],
    },
    {
        "id": "silero_vad_onnx",
        "name": "Silero VAD v5 ONNX",
        "category": "vad",
        "filename": "vad/silero_vad.onnx",
        "size_bytes": 2235928,
        "sha256": "8a72ec22a10bebb0f41a808064a51e6be31d68dc3b9fa76ec5337eb131065719",
        "urls": [
            "https://raw.githubusercontent.com/snakers4/silero-vad/master/src/silero_vad/data/silero_vad.onnx",
        ],
    },
    {
        "id": "rapidocr_det_v4",
        "name": "RapidOCR PP-OCRv4 Detection ONNX",
        "category": "ocr",
        "filename": "rapidocr/ch_PP-OCRv4_det_infer.onnx",
        "size_bytes": 4843934,
        "sha256": "16317d74fbb7bf886fe811b7bc888a7cba8b31a310bb243e8bb4ba01570ff2a7",
        "urls": [
            "https://github.com/RapidAI/RapidOCR/releases/download/v1.1.0/ch_PP-OCRv4_det_infer.onnx",
        ],
    },
    {
        "id": "rapidocr_rec_v4",
        "name": "RapidOCR PP-OCRv4 Recognition ONNX",
        "category": "ocr",
        "filename": "rapidocr/ch_PP-OCRv4_rec_infer.onnx",
        "size_bytes": 11110438,
        "sha256": "cf2c99a6cfb1fa6f932f94b3017a4c7f0fae7bca5c26b52a16c14be14cf73da2",
        "urls": [
            "https://github.com/RapidAI/RapidOCR/releases/download/v1.1.0/ch_PP-OCRv4_rec_infer.onnx",
        ],
    },
    {
        "id": "rapidocr_cls_v2",
        "name": "RapidOCR PP-OCR Direction Classifier ONNX",
        "category": "ocr",
        "filename": "rapidocr/ch_ppocr_mobile_v2.0_cls_infer.onnx",
        "size_bytes": 1493414,
        "sha256": "8b51d459eb54bc7ba82bcfb1fcaadcae4a5d3f1be11e74a87ad998782eeaeec8",
        "urls": [
            "https://github.com/RapidAI/RapidOCR/releases/download/v1.1.0/ch_ppocr_mobile_v2.0_cls_infer.onnx",
        ],
    },
]


def compute_sha256(filepath: str) -> str:
    """Compute SHA-256 hash of a file."""
    h = hashlib.sha256()
    with open(filepath, "rb") as f:
        while chunk := f.read(65536):
            h.update(chunk)
    return h.hexdigest()


def verify_file(filepath: str, expected_sha256: str) -> bool:
    """Check if file exists and matches expected SHA-256."""
    if not os.path.isfile(filepath):
        return False
    actual_sha = compute_sha256(filepath)
    return actual_sha.lower() == expected_sha256.lower()


def download_file_with_progress(url: str, dest_path: str) -> None:
    """Download file with basic progress display."""
    os.makedirs(os.path.dirname(dest_path), exist_ok=True)
    temp_path = dest_path + ".tmp"
    try:
        req = urllib.request.Request(
            url,
            headers={"User-Agent": "Douyinie-Model-Fetcher/1.0"},
        )
        with urllib.request.urlopen(req) as resp, open(temp_path, "wb") as out_file:
            total = int(resp.headers.get("Content-Length") or 0)
            downloaded = 0
            while True:
                chunk = resp.read(65536)
                if not chunk:
                    break
                out_file.write(chunk)
                downloaded += len(chunk)
                if total > 0:
                    pct = (downloaded / total) * 100.0
                    sys.stdout.write(f"\r  [{pct:5.1f}%] {downloaded}/{total} bytes")
                    sys.stdout.flush()
            sys.stdout.write("\n")
        os.replace(temp_path, dest_path)
    except Exception:
        if os.path.isfile(temp_path):
            os.remove(temp_path)
        raise


def run_downloader(target_dir: str = "models", verify_only: bool = False, dry_run: bool = False) -> bool:
    """Run model suite verification and optional download."""
    print("========================================================")
    print(" Douyinie Lightweight ONNX Model Suite Manager")
    print(f" Target Directory: {os.path.abspath(target_dir)}")
    print("========================================================")

    all_ok = True
    manifest_path = os.path.join(target_dir, "manifest.json")
    os.makedirs(target_dir, exist_ok=True)
    with open(manifest_path, "w", encoding="utf-8") as f:
        json.dump(DEFAULT_MANIFEST, f, indent=2)

    for item in DEFAULT_MANIFEST:
        dest_path = os.path.join(target_dir, item["filename"])
        name = item["name"]
        expected_sha = item["sha256"]
        size_mb = item["size_bytes"] / (1024 * 1024)

        if verify_file(dest_path, expected_sha):
            print(f"[OK] {name} ({size_mb:.1f} MB) — SHA-256 verified")
            continue

        if verify_only:
            print(f"[MISSING/MISMATCH] {name} ({size_mb:.1f} MB) at {dest_path}")
            all_ok = False
            continue

        if dry_run:
            print(f"[DRY-RUN] Would download: {name} ({size_mb:.1f} MB) -> {dest_path}")
            continue

        print(f"[DOWNLOAD] Fetching {name} ({size_mb:.1f} MB)...")
        downloaded = False
        for url in item["urls"]:
            try:
                print(f"  Source: {url}")
                download_file_with_progress(url, dest_path)
                if verify_file(dest_path, expected_sha):
                    print(f"  [SUCCESS] Checksum verified: {expected_sha[:16]}...")
                    downloaded = True
                    break
                else:
                    print(f"  [ERROR] Checksum mismatch after download from {url}")
                    if os.path.isfile(dest_path):
                        os.remove(dest_path)
            except Exception as exc:
                print(f"  [WARNING] Download failed from {url}: {exc}")

        if not downloaded:
            print(f"[FAILED] Could not download valid {name}")
            all_ok = False

    print("========================================================")
    if all_ok:
        print("[STATUS] All lightweight ONNX models are present and verified!")
    else:
        print("[STATUS] Some models are missing or unverified.")
    print("========================================================")
    return all_ok


def main():
    parser = argparse.ArgumentParser(description="Douyinie ONNX Model Downloader")
    parser.add_argument("--target-dir", default="models", help="Directory to save models")
    parser.add_argument("--verify-only", action="store_true", help="Only verify existing models")
    parser.add_argument("--dry-run", action="store_true", help="Simulate download actions")
    args = parser.parse_args()

    ok = run_downloader(
        target_dir=args.target_dir,
        verify_only=args.verify_only,
        dry_run=args.dry_run,
    )
    if not ok and args.verify_only:
        sys.exit(1)


if __name__ == "__main__":
    main()
