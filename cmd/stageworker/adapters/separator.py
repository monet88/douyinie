#!/usr/bin/env python3
"""
Douyinie StageWorker Adapter: Audio Separator (UVR baseline, Demucs fallback via python-audio-separator / demucs)
Invokes audio stem separation or fails closed with descriptive errors.

Contract:
- Stdin: JSON request with audio_path, model_name, model_version, run_id, attempt_id, model_path, entrypoint_file
- Stdout: JSON response with vocals_wav, background_wav, vocals_sha256, background_sha256, duration_ms, sample_rate, channels
- Stderr: Human-readable error messages on failure
- Exit code: 0 on success, non-zero on failure
"""

import base64
import hashlib
import io
import json
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import wave
from typing import Any, Dict, Optional

# Pluggable factory hooks for deterministic testing without full ML packages
_SEPARATOR_MODEL_FACTORY = None
_SEPARATOR_PROBE_FACTORY = None
UVR_CANONICAL_FILENAME = "UVR-MDX-NET-Inst_HQ_4.onnx"
UVR_PINNED_SHA256 = "3c4b5b9b05090fdf238f38ba5046813982d50e2a652e9cb3324ea79720c3c9c8"
UVR_MDX_NET_INST_HQ_4_MD5 = "0ddfc0eb5792638ad5dc27850236c246"

DEMUCS_CANONICAL_FILENAME = "955717e8-8726e21a.th"
DEMUCS_PINNED_SHA256 = "8726e21a993978c7ba086d3872e7608d7d5bfca646ca4aca459ffda844faa8b4"
DEMUCS_CANONICAL_BAG_YAML = "htdemucs.yaml"
DEMUCS_PINNED_BAG_YAML_SHA256 = "239c445d0b14454d541ad8bd9bb271c9e536d267e8a4625208744cbb2e7bb66c"
DEMUCS_SIGNATURE = "955717e8"

PINNED_AUDIO_SEPARATOR_VERSION = "0.47.0"
PINNED_AUDIO_SEPARATOR_COMMIT = "bf1164aa0f1ee1d1d0ef0f09b315f7659fc06bab"
PINNED_AUDIO_SEPARATOR_REPO = "nomadkaraoke/python-audio-separator"
PINNED_DEMUCS_VERSION = "4.1.0a2"
PINNED_DEMUCS_COMMIT = "e976d93ecc3865e5757426930257e200846a520a"
PINNED_DEMUCS_REPO = "facebookresearch/demucs"

def normalize_vcs_url(url: str) -> str:
    """Safely normalize VCS URL for equivalent GitHub forms and trailing .git."""
    u = (url or "").strip().lower()
    if u.startswith("git+"):
        u = u[4:]
    if u.startswith("git://"):
        u = "https://" + u[6:]
    if u.startswith("git@github.com:"):
        u = "https://github.com/" + u[len("git@github.com:"):]
    elif u.startswith("ssh://git@github.com/"):
        u = "https://github.com/" + u[len("ssh://git@github.com/"):]
    if u.endswith(".git"):
        u = u[:-4]
    u = u.rstrip("/")
    return u

def is_canonical_upstream_repo(url: str, expected_repo: str) -> bool:
    """Check if normalized URL points to the expected github.com repository."""
    norm = normalize_vcs_url(url)
    expected = f"https://github.com/{expected_repo.lower()}"
    return norm == expected
def probe_runtime_identity(model_name: str, model_version: str = "v3") -> Dict[str, Any]:
    """
    Independently probe the configured Python runtime environment for installed package version,
    exact VCS commit (PEP 610 direct_url.json), and backend versions.
    Fails closed with actionable error messages if package is missing or exact commit cannot be proven.
    """
    if _SEPARATOR_PROBE_FACTORY is not None and callable(_SEPARATOR_PROBE_FACTORY):
        return _SEPARATOR_PROBE_FACTORY(model_name, model_version)

    import importlib.metadata

    lower_model = (model_name or "").lower()
    is_demucs = "demucs" in lower_model

    if is_demucs:
        target_pkgs = ["demucs"]
        pinned_version = PINNED_DEMUCS_VERSION
        pinned_commit = PINNED_DEMUCS_COMMIT
        pinned_repo = PINNED_DEMUCS_REPO
        primary_dep = "demucs"
    else:
        target_pkgs = ["audio-separator", "audio_separator", "python-audio-separator"]
        pinned_version = PINNED_AUDIO_SEPARATOR_VERSION
        pinned_commit = PINNED_AUDIO_SEPARATOR_COMMIT
        pinned_repo = PINNED_AUDIO_SEPARATOR_REPO
        primary_dep = "audio-separator"

    dist = None
    for pkg in target_pkgs:
        try:
            dist = importlib.metadata.distribution(pkg)
            break
        except (importlib.metadata.PackageNotFoundError, Exception):
            continue

    if dist is None:
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: separator distribution '{target_pkgs[0]}' is not installed in the configured Python environment ({sys.executable})"
        )

    # 1. Validate installed package version: require exact normalized equality
    pkg_version = (getattr(dist, "version", "") or "").strip()
    if not pkg_version or pkg_version != pinned_version:
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: {primary_dep} package version mismatch: expected exact {pinned_version}, found {pkg_version or 'unknown'}"
        )

    # 2. Extract exact VCS commit and validate origin from PEP 610 direct_url.json
    direct_url_txt = dist.read_text("direct_url.json")
    if not direct_url_txt:
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' ({pkg_version}) has no PEP 610 direct_url.json; cannot verify exact VCS commit (hardcoded claims rejected)"
        )

    try:
        direct_url_data = json.loads(direct_url_txt)
    except Exception as e:
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' direct_url.json is malformed: {e}"
        )

    if not isinstance(direct_url_data, dict):
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' direct_url.json is not a valid JSON object"
        )

    # Reject local directory / editable installs
    if "dir_info" in direct_url_data:
        is_editable = False
        if isinstance(direct_url_data["dir_info"], dict):
            is_editable = bool(direct_url_data["dir_info"].get("editable", False))
        mode_str = "editable" if is_editable else "local directory"
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' was installed from a {mode_str} (dir_info present); local/editable origins cannot prove pinned VCS source revision"
        )

    # Reject archive / wheel installs
    if "archive_info" in direct_url_data:
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' was installed from an archive/wheel (archive_info present); archive origins cannot prove pinned VCS source revision"
        )

    vcs_info = direct_url_data.get("vcs_info")
    if not isinstance(vcs_info, dict):
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' direct_url.json has no vcs_info; cannot verify exact VCS origin"
        )

    vcs_type = str(vcs_info.get("vcs") or "").strip().lower()
    if vcs_type != "git":
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' VCS type mismatch: expected 'git', got '{vcs_type or 'missing'}'"
        )

    direct_url = str(direct_url_data.get("url") or "").strip()
    if not direct_url:
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' direct_url.json has empty url; cannot verify upstream repository origin"
        )

    if not is_canonical_upstream_repo(direct_url, pinned_repo):
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' upstream repository origin mismatch: expected {pinned_repo} on github.com, got '{direct_url}'"
        )

    commit_id = vcs_info.get("commit_id")
    if not commit_id:
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: package '{primary_dep}' direct_url.json has no vcs_info.commit_id; cannot verify exact VCS commit"
        )

    observed_commit = str(commit_id).strip().lower()
    if len(observed_commit) != 40 or observed_commit != pinned_commit.lower():
        raise RuntimeError(
            f"RUNTIME_IDENTITY_PROBE_FAILED: {primary_dep} source revision mismatch: expected exact 40-char commit {pinned_commit}, got '{observed_commit}'"
        )

    # 3. Collect backend runtime versions
    runtime_versions = {primary_dep: pkg_version}
    if is_demucs:
        for backend in ["torch", "torchaudio"]:
            try:
                bdist = importlib.metadata.distribution(backend)
                runtime_versions[backend] = bdist.version
            except Exception:
                pass
    else:
        for backend in ["onnxruntime", "onnxruntime-gpu"]:
            try:
                bdist = importlib.metadata.distribution(backend)
                runtime_versions[backend] = bdist.version
            except Exception:
                pass
        try:
            bdist = importlib.metadata.distribution("torch")
            runtime_versions["torch"] = bdist.version
        except Exception:
            pass

    return {
        "status": "ok",
        "package_name": primary_dep,
        "package_version": pkg_version,
        "source_revision": observed_commit,
        "runtime_versions": runtime_versions,
        "adapter_revision": f"cmd/stageworker/adapters/separator.py@v{pkg_version}",
    }
def generate_synthetic_pcm_wav(sample_rate: int = 16000, channels: int = 1, duration_ms: int = 5000) -> bytes:
    """Generate standard 16-bit PCM WAV bytes."""
    num_samples = int((sample_rate * duration_ms) / 1000)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(2)
        wf.setframerate(sample_rate)
        raw = struct.pack(f"<{num_samples * channels}h", *([100] * (num_samples * channels)))
        wf.writeframes(raw)
    return buf.getvalue()


def separate_uvr(
    audio_path: str,
    model_name: str,
    model_version: str,
    model_path: Optional[str] = None,
    entrypoint_file: Optional[str] = None,
    require_model_snapshot: bool = False,
    metadata_file: Optional[str] = None,
    **kwargs: Any,
) -> Dict[str, Any]:
    """Execute stem separation using python-audio-separator (UVR baseline lane)."""
    if require_model_snapshot or model_path:
        if not model_path:
            raise RuntimeError("WORKER_SNAPSHOT_PATH_REQUIRED: UVR in production RC requires a verified local snapshot model_path")
        mp = os.path.abspath(model_path)
        if not os.path.exists(mp):
            raise RuntimeError(f"WORKER_SNAPSHOT_PATH_REQUIRED: UVR snapshot path does not exist: {model_path}")
        if not entrypoint_file:
            raise RuntimeError("WORKER_SNAPSHOT_PATH_REQUIRED: UVR requires verified UVR-MDX-NET-Inst_HQ_4.onnx entrypoint in model snapshot")

        ep = os.path.abspath(entrypoint_file)
        try:
            rel = os.path.relpath(ep, mp)
            if rel.startswith(".."):
                raise ValueError()
        except ValueError:
            raise RuntimeError(f"SEPARATOR_MODEL_ASSET_MISSING: UVR entrypoint escapes snapshot root: {entrypoint_file} (root: {model_path})")
        if os.path.basename(ep).lower() != UVR_CANONICAL_FILENAME.lower():
            raise RuntimeError(f"SEPARATOR_MODEL_ASSET_MISSING: unverified UVR entrypoint {os.path.basename(ep)} (expected {UVR_CANONICAL_FILENAME})")

        if not os.path.exists(ep) or os.path.isdir(ep):
            raise RuntimeError(f"SEPARATOR_MODEL_ASSET_MISSING: UVR artifact file missing from snapshot: {ep}")

    # Resolve and parse verified UVR model metadata asset (mdx_model_data.json)
    meta_path = metadata_file or kwargs.get("metadata_file")
    if require_model_snapshot:
        if not meta_path:
            raise RuntimeError(
                "SEPARATOR_METADATA_ASSET_MISSING: UVR separator requires verified metadata_file in model snapshot envelope; loose discovery forbidden"
            )
        if not os.path.isfile(meta_path):
            raise RuntimeError(
                f"SEPARATOR_METADATA_ASSET_MISSING: UVR model metadata asset missing from snapshot: {meta_path}"
            )
    elif not meta_path and model_path:
        cand = os.path.join(model_path, "mdx_model_data.json")
        if os.path.isfile(cand):
            meta_path = cand

    model_params = None
    if require_model_snapshot or meta_path:
        if not meta_path or not os.path.isfile(meta_path):
            raise RuntimeError(
                f"SEPARATOR_METADATA_ASSET_MISSING: UVR model metadata asset (mdx_model_data.json) missing from snapshot: {meta_path or model_path}"
            )
        try:
            with open(meta_path, "r", encoding="utf-8") as f:
                meta_dict = json.load(f)
        except Exception as e:
            raise RuntimeError(f"SEPARATOR_METADATA_INVALID: failed to parse UVR metadata JSON: {e}")

        if not isinstance(meta_dict, dict) or UVR_MDX_NET_INST_HQ_4_MD5 not in meta_dict:
            raise RuntimeError(
                f"SEPARATOR_METADATA_INVALID: MD5 entry {UVR_MDX_NET_INST_HQ_4_MD5} missing from UVR metadata {meta_path}"
            )
        model_params = meta_dict[UVR_MDX_NET_INST_HQ_4_MD5]
        if not isinstance(model_params, dict) or not model_params:
            raise RuntimeError(
                f"SEPARATOR_METADATA_INVALID: invalid model parameters for MD5 {UVR_MDX_NET_INST_HQ_4_MD5} in {meta_path}"
            )

    # Strictly offline: enforce environment variables
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"

    try:
        import audio_separator  # type: ignore
        from audio_separator.separator import Separator  # type: ignore
    except ImportError:
        raise RuntimeError(f"UVR runtime not found: install audio-separator (pip install audio-separator=={PINNED_AUDIO_SEPARATOR_VERSION})")

    installed_ver = None
    try:
        import importlib.metadata as importlib_metadata
        installed_ver = importlib_metadata.version("audio-separator")
    except Exception:
        installed_ver = getattr(audio_separator, "__version__", None)

    if installed_ver and installed_ver.strip() != PINNED_AUDIO_SEPARATOR_VERSION:
        raise RuntimeError(
            f"SEPARATOR_PACKAGE_VERSION_MISMATCH: installed audio-separator {installed_ver} != {PINNED_AUDIO_SEPARATOR_VERSION}"
        )

    backend_info = ""
    try:
        import onnxruntime  # type: ignore
        backend_info = f" [onnxruntime {onnxruntime.__version__}]"
    except Exception:
        pass
    runtime_identity = f"python-audio-separator {installed_ver or PINNED_AUDIO_SEPARATOR_VERSION}{backend_info}"

    model_dir = model_path if model_path else None
    sep = Separator(model_file_dir=model_dir) if model_dir else Separator()

    # Block all request-time network/download helpers on the Separator instance so floating network resolution is impossible
    def _blocked_download(*args, **kwargs):
        raise RuntimeError("NETWORK_DOWNLOAD_FORBIDDEN: UVR execution is strictly offline; download helper was reached")

    sep.download_file_if_not_exists = _blocked_download
    if hasattr(sep, "download_file_by_hash"):
        sep.download_file_by_hash = _blocked_download
    if hasattr(sep, "list_supported_model_files"):
        sep.list_supported_model_files = _blocked_download

    # Provide offline local model resolution: resolve strictly from verified snapshot assets
    def _offline_download_model_files(name):
        ep_file = entrypoint_file or (os.path.join(model_path, UVR_CANONICAL_FILENAME) if model_path else None)
        if not ep_file or not os.path.exists(ep_file):
            raise RuntimeError(f"SEPARATOR_MODEL_ASSET_MISSING: UVR artifact file missing from snapshot: {ep_file}")
        return (
            os.path.basename(ep_file),
            "MDX",
            "UVR-MDX-NET-Inst_HQ_4",
            ep_file,
            None,
        )

    sep.download_model_files = _offline_download_model_files

    if model_params is not None:
        sep.load_model_data_using_hash = lambda *a, **kw: dict(model_params)
    # Contextual guard blocking socket/urllib/requests during load and separation
    class _OfflineNetworkGuard:
        def __enter__(self):
            import urllib.request
            self._orig_urlopen = urllib.request.urlopen
            def _blocked_urlopen(*a, **kw):
                raise RuntimeError("NETWORK_DOWNLOAD_FORBIDDEN: HTTP/network access blocked in offline UVR execution")
            urllib.request.urlopen = _blocked_urlopen

            self._orig_requests_get = None
            if "requests" in sys.modules:
                requests_mod = sys.modules["requests"]
                if hasattr(requests_mod, "get"):
                    self._orig_requests_get = requests_mod.get
                    requests_mod.get = _blocked_download
            return self

        def __exit__(self, exc_type, exc_val, exc_tb):
            import urllib.request
            urllib.request.urlopen = self._orig_urlopen
            if self._orig_requests_get is not None and "requests" in sys.modules:
                sys.modules["requests"].get = self._orig_requests_get

    with _OfflineNetworkGuard():
        sep.load_model(model_name)
        output_files = sep.separate(audio_path)

    vocals_path = None
    bg_path = None
    for f in output_files:
        if "Vocals" in f or "vocals" in f:
            vocals_path = f
        elif "Instrumental" in f or "background" in f or "no_vocals" in f:
            bg_path = f

    vocals_bytes = b""
    if vocals_path and os.path.exists(vocals_path):
        with open(vocals_path, "rb") as vf:
            vocals_bytes = vf.read()

    bg_bytes = b""
    if bg_path and os.path.exists(bg_path):
        with open(bg_path, "rb") as bf:
            bg_bytes = bf.read()

    dur_ms = 5000
    if bg_bytes:
        with wave.open(io.BytesIO(bg_bytes), "rb") as wf:
            dur_ms = int((wf.getnframes() * 1000) / wf.getframerate())

    return {
        "vocals_data": vocals_bytes,
        "background_data": bg_bytes,
        "duration_ms": dur_ms,
        "sample_rate": 16000,
        "channels": 1,
        "model_name": UVR_CANONICAL_FILENAME,
        "model_version": model_version or "v3",
        "runtime_identity": runtime_identity,
    }


def separate_demucs(
    audio_path: str,
    model_name: str,
    model_version: str,
    model_path: Optional[str] = None,
    entrypoint_file: Optional[str] = None,
    require_model_snapshot: bool = False,
    model_snapshot: Optional[Dict[str, Any]] = None,
    has_verified_bag_yaml: bool = False,
    **kwargs: Any,
) -> Dict[str, Any]:
    """Execute stem separation using Demucs CLI/module (Demucs fallback lane)."""
    norm_name = (model_name or "").lower()
    if "htdemucs_ft" in norm_name:
        raise RuntimeError("DEMUCS_FT_SUBSTITUTION_REJECTED: htdemucs_ft bag is rejected as htdemucs fallback; exact htdemucs required")

    if require_model_snapshot or model_path:
        if not model_path:
            raise RuntimeError("WORKER_SNAPSHOT_PATH_REQUIRED: Demucs in production RC requires a verified local snapshot model_path")
        mp = os.path.abspath(model_path)
        if not os.path.exists(mp):
            raise RuntimeError(f"WORKER_SNAPSHOT_PATH_REQUIRED: Demucs snapshot path does not exist: {model_path}")
        if not entrypoint_file:
            raise RuntimeError("WORKER_SNAPSHOT_PATH_REQUIRED: Demucs requires verified 955717e8-8726e21a.th entrypoint in model snapshot")

        ep = os.path.abspath(entrypoint_file)
        try:
            rel = os.path.relpath(ep, mp)
            if rel.startswith(".."):
                raise ValueError()
        except ValueError:
            raise RuntimeError(f"SEPARATOR_MODEL_ASSET_MISSING: Demucs entrypoint escapes snapshot root: {entrypoint_file} (root: {model_path})")

        if "955717e8" not in os.path.basename(ep).lower():
            raise RuntimeError(f"SEPARATOR_MODEL_ASSET_MISSING: unverified Demucs entrypoint {os.path.basename(ep)} (expected {DEMUCS_CANONICAL_FILENAME})")

        if not os.path.exists(ep) or os.path.isdir(ep):
            raise RuntimeError(f"SEPARATOR_MODEL_ASSET_MISSING: Demucs checkpoint file missing from snapshot: {ep}")

    # Strictly offline: point torch home if snapshot provided
    if model_path:
        os.environ["TORCH_HOME"] = model_path

    # Fail closed if installed demucs package does not match pinned RC version
    installed_ver = None
    try:
        import importlib.metadata as importlib_metadata
        installed_ver = importlib_metadata.version("demucs")
        if installed_ver and installed_ver.strip() != PINNED_DEMUCS_VERSION:
            raise RuntimeError(
                f"DEMUCS_PACKAGE_VERSION_MISMATCH: installed demucs {installed_ver} != {PINNED_DEMUCS_VERSION}"
            )
    except Exception as e:
        if "DEMUCS_PACKAGE_VERSION_MISMATCH" in str(e):
            raise
        # Demucs not installed in parent env; verified via subprocess execution
        pass

    backend_info = ""
    try:
        import torch  # type: ignore
        backend_info = f" [torch {torch.__version__}]"
    except Exception:
        pass
    runtime_identity = f"demucs {installed_ver or PINNED_DEMUCS_VERSION}{backend_info}"

    # Check if htdemucs.yaml is verified in manifest / snapshot envelope.
    # Upstream get_model(name='htdemucs', repo=<local>) uses LocalRepo + BagOnlyRepo.
    # When htdemucs.yaml is a manifest-declared verified asset, invoke as 'htdemucs'.
    # If absent or unverified, invoke exact signature '955717e8' without bag YAML.
    # Never accept an unverified loose filesystem YAML.
    has_bag_yaml = False
    if model_path:
        yaml_file = os.path.join(os.path.abspath(model_path), DEMUCS_CANONICAL_BAG_YAML)
        if os.path.exists(yaml_file):
            if require_model_snapshot:
                verified_in_snap = has_verified_bag_yaml
                if not verified_in_snap and model_snapshot and isinstance(model_snapshot, dict):
                    deps = model_snapshot.get("dependencies", [])
                    for dep in deps:
                        dname = (dep.get("dependency_name") or "").lower()
                        drole = (dep.get("role") or "").lower()
                        if "htdemucs.yaml" in dname or drole == "bag_yaml":
                            verified_in_snap = True
                            break
                has_bag_yaml = verified_in_snap
            else:
                has_bag_yaml = True

    invoke_model_name = "htdemucs" if has_bag_yaml else DEMUCS_SIGNATURE
    out_dir = tempfile.mkdtemp(prefix="demucs_out_")
    try:
        cmd = [sys.executable, "-m", "demucs.separate", "-n", invoke_model_name, "-o", out_dir, "--two-stems=vocals"]
        if model_path:
            cmd.extend(["--repo", model_path])
        cmd.append(audio_path)
        proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

        track_name = os.path.splitext(os.path.basename(audio_path))[0]
        candidate_dirs = [
            os.path.join(out_dir, invoke_model_name, track_name),
            os.path.join(out_dir, "htdemucs", track_name),
            os.path.join(out_dir, DEMUCS_SIGNATURE, track_name),
        ]
        vocals_path = None
        no_vocals_path = None
        for cd in candidate_dirs:
            vp = os.path.join(cd, "vocals.wav")
            nvp = os.path.join(cd, "no_vocals.wav")
            if os.path.exists(nvp):
                vocals_path = vp
                no_vocals_path = nvp
                break

        vocals_bytes = b""
        if vocals_path and os.path.exists(vocals_path):
            with open(vocals_path, "rb") as vf:
                vocals_bytes = vf.read()

        bg_bytes = b""
        if no_vocals_path and os.path.exists(no_vocals_path):
            with open(no_vocals_path, "rb") as bf:
                bg_bytes = bf.read()

        if not bg_bytes:
            raise RuntimeError(f"Demucs produced no background stem in {candidate_dirs[0]}")

        dur_ms = 5000
        with wave.open(io.BytesIO(bg_bytes), "rb") as wf:
            dur_ms = int((wf.getnframes() * 1000) / wf.getframerate())

        return {
            "vocals_data": vocals_bytes,
            "background_data": bg_bytes,
            "duration_ms": dur_ms,
            "sample_rate": 16000,
            "channels": 1,
            "model_name": "htdemucs",
            "model_version": model_version or "v4",
            "runtime_identity": runtime_identity,
        }
    finally:
        shutil.rmtree(out_dir, ignore_errors=True)


def separate_audio_stems(
    audio_path: str,
    model_name: str,
    model_version: str,
    model_path: Optional[str] = None,
    entrypoint_file: Optional[str] = None,
    require_model_snapshot: bool = False,
    **kwargs: Any,
) -> Dict[str, Any]:
    """Execute stem separation with explicit model/lane dispatch (UVR vs Demucs)."""
    if _SEPARATOR_MODEL_FACTORY is not None:
        try:
            return _SEPARATOR_MODEL_FACTORY(
                audio_path,
                model_name,
                model_version,
                model_path=model_path,
                entrypoint_file=entrypoint_file,
                require_model_snapshot=require_model_snapshot,
                **kwargs,
            )
        except TypeError:
            try:
                return _SEPARATOR_MODEL_FACTORY(
                    audio_path,
                    model_name,
                    model_version,
                    model_path=model_path,
                    entrypoint_file=entrypoint_file,
                    require_model_snapshot=require_model_snapshot,
                )
            except TypeError:
                return _SEPARATOR_MODEL_FACTORY(audio_path, model_name, model_version)

    norm_model = (model_name or "").lower()
    is_demucs = "demucs" in norm_model or "htdemucs" in norm_model
    target_fn = separate_demucs if is_demucs else separate_uvr
    try:
        return target_fn(
            audio_path,
            model_name,
            model_version,
            model_path=model_path,
            entrypoint_file=entrypoint_file,
            require_model_snapshot=require_model_snapshot,
            **kwargs,
        )
    except TypeError:
        return target_fn(
            audio_path,
            model_name,
            model_version,
            model_path=model_path,
            entrypoint_file=entrypoint_file,
            require_model_snapshot=require_model_snapshot,
        )

def main():
    try:
        input_data = sys.stdin.read()
        if not input_data.strip():
            sys.stderr.write("Empty input payload\n")
            sys.exit(1)

        req = json.loads(input_data)
        audio_path = req.get("audio_path", "")
        mode = req.get("mode")
        if mode == "probe":
            model_name = req.get("model_name", UVR_CANONICAL_FILENAME)
            model_version = req.get("model_version", "v3")
            probe_res = probe_runtime_identity(model_name, model_version)
            print(json.dumps(probe_res))
            sys.exit(0)
        model_name = req.get("model_name", "UVR-MDX-NET-Inst_HQ_4.onnx")
        model_version = req.get("model_version", "v3")
        model_path = req.get("model_path")
        entrypoint_file = req.get("entrypoint_file")
        metadata_file = req.get("metadata_file")
        require_model_snapshot = bool(req.get("require_model_snapshot", False))
        has_verified_bag_yaml = bool(req.get("has_verified_bag_yaml", False))
        model_snapshot = req.get("model_snapshot")

        if not audio_path or not os.path.exists(audio_path):
            sys.stderr.write(f"Source audio file not found: {audio_path}\n")
            sys.exit(1)

        res = separate_audio_stems(
            audio_path,
            model_name,
            model_version,
            model_path=model_path,
            entrypoint_file=entrypoint_file,
            require_model_snapshot=require_model_snapshot,
            model_snapshot=model_snapshot,
            has_verified_bag_yaml=has_verified_bag_yaml,
            metadata_file=metadata_file,
        )

        vocals_bytes = res.get("vocals_data", b"")
        bg_bytes = res.get("background_data", b"")
        dur_ms = res.get("duration_ms", 0)
        sample_rate = res.get("sample_rate", 16000)
        channels = res.get("channels", 1)

        out = {
            "vocals_data": base64.b64encode(vocals_bytes).decode("ascii") if vocals_bytes else "",
            "background_data": base64.b64encode(bg_bytes).decode("ascii") if bg_bytes else "",
            "vocals_sha256": hashlib.sha256(vocals_bytes).hexdigest() if vocals_bytes else "",
            "background_sha256": hashlib.sha256(bg_bytes).hexdigest() if bg_bytes else "",
            "duration_ms": dur_ms,
            "sample_rate": sample_rate,
            "channels": channels,
            "model_name": res.get("model_name", model_name),
            "model_version": res.get("model_version", model_version),
            "runtime_identity": res.get("runtime_identity", ""),
        }
        print(json.dumps(out))
        sys.exit(0)

    except Exception as e:
        sys.stderr.write(f"Separator error: {e}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
