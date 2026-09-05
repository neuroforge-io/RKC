#!/usr/bin/env python3
"""Assemble small, deterministic portable downloads from exact-source Go binaries."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import tempfile
import zipfile
from datetime import datetime, timezone
from pathlib import Path

try:
    from scripts.git_source_guard import SourceGuardError, require_clean_worktree
except ModuleNotFoundError:
    from git_source_guard import SourceGuardError, require_clean_worktree

ROOT = Path(__file__).resolve().parents[1]
PLATFORMS = tuple(f"{system}-{arch}" for system in ("linux", "darwin", "windows") for arch in ("amd64", "arm64"))
MAX_FILE_BYTES = 128 * 1024 * 1024
MAX_PACKAGE_BYTES = 256 * 1024 * 1024
MANIFEST = "share/doc/rkc/portable-manifest.json"


class PackageError(ValueError):
    """A release input failed provenance or package admission."""


def regular_bytes(path: Path) -> bytes:
    """Read a bounded regular file, refusing links in every path component."""
    if path.resolve() != path.absolute() or not stat.S_ISREG(path.lstat().st_mode):
        raise PackageError(f"release input is not a real regular file: {path}")
    if path.stat().st_size > MAX_FILE_BYTES:
        raise PackageError(f"release input is too large: {path}")
    with path.open("rb") as handle:
        data = handle.read(MAX_FILE_BYTES + 1)
    if len(data) > MAX_FILE_BYTES:
        raise PackageError(f"release input grew beyond its bound: {path}")
    return data


def source_identity(root: Path) -> dict[str, object]:
    """Capture exact clean source identity, independent of local host paths."""
    require_clean_worktree(root, "portable package assembly")
    def git(*args: str) -> str:
        return subprocess.check_output(["git", "-C", str(root), *args], timeout=30, text=True).strip()
    version = regular_bytes(root / "VERSION").decode("utf-8").strip()
    if re.fullmatch(r"[0-9][0-9A-Za-z._-]{0,63}", version) is None:
        raise PackageError("VERSION is not a safe release version")
    return {"version": version, "commit": git("rev-parse", "HEAD"), "tree": git("rev-parse", "HEAD^{tree}"), "commit_time_unix": int(git("show", "-s", "--format=%ct", "HEAD"))}


def payload(root: Path, binaries: Path, platform: str, source: dict[str, object]) -> dict[str, tuple[bytes, int]]:
    """Create the prefix-relative file set; never include models or source caches."""
    if platform not in PLATFORMS:
        raise PackageError("unsupported portable platform")
    files: dict[str, tuple[bytes, int]] = {}
    total_bytes = 0
    def add(path: Path, destination: str, mode: int = 0o644) -> None:
        nonlocal total_bytes
        data = regular_bytes(path)
        total_bytes += len(data)
        if len(files) >= 1024 or total_bytes > MAX_PACKAGE_BYTES:
            raise PackageError("portable package exceeds its file or byte bound")
        files[destination] = (data, mode)
    suffix = ".exe" if platform.startswith("windows-") else ""
    for binary in ("rkc", "rkc-mcp"):
        add(binaries / platform / f"{binary}{suffix}", f"bin/{binary}{suffix}", 0o755)
        add(binaries / platform / f"{binary}.spdx.json", f"share/doc/rkc/{binary}.spdx.json")
    for name in ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md", "VERSION"):
        add(root / name, f"share/doc/rkc/{name}")
    licenses = root / "LICENSES"
    if not licenses.is_dir() or licenses.is_symlink():
        raise PackageError("LICENSES must be a real directory")
    for path in sorted(licenses.rglob("*")):
        if path.is_dir() and not path.is_symlink():
            continue
        add(path, "share/doc/rkc/" + path.relative_to(root).as_posix())
    add(root / "third_party/go-modules.lock.json", "share/doc/rkc/third_party/go-modules.lock.json")
    for name in ("models/models.lock.json", "models/qualification/rkc-local-model-v1.json", "schemas/model-lock.schema.json", "schemas/model-qualification.schema.json"):
        add(root / name, "share/rkc/" + name)
    manifest = {
        "schema_version": "rkc-portable-release/v1", "platform": platform, "source": source,
        "capabilities": {
            "portable_analysis": "scan --no-python; open; search, context, knowledge packs, HTTP and MCP",
            "protected_execution": "Linux user-systemd and delegated cgroup v2 required for workbench, Python adapter, and model execution",
            "qualification": "cross-compiled; native execution evidence is published separately by the release workflow",
        },
        "files": [{"path": name, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest(), "mode": format(mode, "04o")} for name, (data, mode) in sorted(files.items())],
        "excluded_generated_files": [MANIFEST],
    }
    files[MANIFEST] = ((json.dumps(manifest, sort_keys=True, indent=2) + "\n").encode(), 0o644)
    return files


def write_archive(path: Path, files: dict[str, tuple[bytes, int]], epoch: int) -> None:
    """Write deterministic ZIP metadata and stable file order on every host."""
    timestamp = datetime.fromtimestamp(max(315532800, epoch), timezone.utc).timetuple()[:6]
    with zipfile.ZipFile(path, "x", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, (data, mode) in sorted(files.items()):
            info = zipfile.ZipInfo(name, timestamp)
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | mode) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(info, data, compresslevel=9)


def verify_binary_receipts(root: Path, binaries: Path, source: dict[str, object]) -> None:
    """Check binary build settings and SBOMs against the audited module lock."""
    for platform in PLATFORMS:
        system, architecture = platform.split("-")
        suffix = ".exe" if system == "windows" else ""
        for binary in ("rkc", "rkc-mcp"):
            subprocess.run([
                sys.executable, str(root / "scripts/generate-go-sbom.py"),
                "--binary", str(binaries / platform / f"{binary}{suffix}"),
                "--verify-document", str(binaries / platform / f"{binary}.spdx.json"),
                "--source-root", str(root), "--lock", str(root / "third_party/go-modules.lock.json"),
                "--source-commit", str(source["commit"]), "--source-tree", str(source["tree"]),
                "--source-date-epoch", str(source["commit_time_unix"]),
                "--version", str(source["version"]), "--goos", system, "--goarch", architecture,
            ], check=True, timeout=120)


def assemble(root: Path, binaries: Path, output: Path) -> None:
    """Publish a complete new generation only after every receipt is verified."""
    source = source_identity(root)
    dist = root / "dist"
    if output.parent != dist or output.name != "portable-release" or output.exists() or output.is_symlink():
        raise PackageError("output must be the absent dist/portable-release directory")
    if not dist.is_dir() or dist.is_symlink():
        raise PackageError("dist must be a real directory")
    with tempfile.TemporaryDirectory(prefix=".rkc-portable-", dir=dist) as temporary:
        # Verify a private snapshot of the actual bytes that will enter ZIPs,
        # preventing later changes to shared build outputs from crossing over.
        admitted = Path(temporary) / "binaries"
        for platform in PLATFORMS:
            (admitted / platform).mkdir(parents=True)
            suffix = ".exe" if platform.startswith("windows-") else ""
            for binary in ("rkc", "rkc-mcp"):
                for name in (f"{binary}{suffix}", f"{binary}.spdx.json"):
                    (admitted / platform / name).write_bytes(regular_bytes(binaries / platform / name))
        verify_binary_receipts(root, admitted, source)
        stage = Path(temporary) / "release"
        stage.mkdir()
        receipts = []
        for platform in PLATFORMS:
            archive = stage / f"rkc-{platform}.zip"
            write_archive(archive, payload(root, admitted, platform, source), int(source["commit_time_unix"]))
            receipts.append(f"{hashlib.sha256(regular_bytes(archive)).hexdigest()}  {archive.name}\n")
        for name in ("install-release.sh", "install-release.ps1"):
            body = regular_bytes(root / "scripts" / name)
            (stage / name).write_bytes(body)
            receipts.append(f"{hashlib.sha256(body).hexdigest()}  {name}\n")
        (stage / "SHA256SUMS.txt").write_text("".join(receipts), encoding="ascii")
        if source_identity(root) != source:
            raise PackageError("source identity changed during portable packaging")
        os.rename(stage, output)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binaries", type=Path, default=ROOT / "dist/portable-binaries")
    arguments = parser.parse_args()
    try:
        assemble(ROOT, arguments.binaries.absolute(), ROOT / "dist/portable-release")
    except (OSError, ValueError, SourceGuardError, subprocess.SubprocessError) as exc:
        print(f"portable package: {exc}", file=sys.stderr)
        return 1
    print("portable release: six verified downloads published in dist/portable-release")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
