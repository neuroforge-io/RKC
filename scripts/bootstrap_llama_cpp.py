#!/usr/bin/env python3
"""Build the checksum-pinned llama.cpp runtime under RKC's resource guard."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
from pathlib import Path, PurePosixPath
from typing import Callable

from model_assets import (
    AssetError,
    DEFAULT_LOCK,
    IntegrityError,
    ModelLock,
    PriorityBlocked,
    assert_priority_available,
    assert_disk_headroom,
    assert_resource_guard,
    fetch_asset,
    load_lock,
    verify_cached_asset,
)

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_DOWNLOAD_ROOT = ROOT / ".rkc-downloads"
DEFAULT_RUNTIME_ROOT = ROOT / ".rkc-runtime" / "llama.cpp"
MAX_ARCHIVE_MEMBERS = 100_000
MAX_ARCHIVE_FILE_BYTES = 128 * 1024 * 1024
MAX_ARCHIVE_TOTAL_BYTES = 768 * 1024 * 1024
PRIORITY_RECHECK_BYTES = 16 * 1024 * 1024
RUNTIME_STAGING_WRITE_BYTES = 4 * 1024 * 1024 * 1024
CONFIGURE_TIMEOUT_SECONDS = 15 * 60
BUILD_TIMEOUT_SECONDS = 60 * 60
BUILD_POLL_SECONDS = 0.5
RECEIPT_NAME = "rkc-llama-runtime.json"


def _mapping(value: object, label: str) -> dict[str, object]:
    if not isinstance(value, dict):
        raise AssetError(f"{label} must be an object")
    return value


def _string_list(value: object, label: str) -> list[str]:
    if not isinstance(value, list) or any(not isinstance(item, str) for item in value):
        raise AssetError(f"{label} must be a string array")
    return list(value)


def _sha256_file(
    path: Path,
    maximum_bytes: int = 2 * 1024 * 1024 * 1024,
    priority_check: Callable[[], None] | None = None,
) -> tuple[str, int]:
    if priority_check is None:
        priority_check = assert_priority_available
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags)
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode) or before.st_size > maximum_bytes:
            raise IntegrityError(f"runtime artifact is not a bounded regular file: {path}")
        digest = hashlib.sha256()
        total = 0
        since_priority_check = PRIORITY_RECHECK_BYTES
        while True:
            if since_priority_check >= PRIORITY_RECHECK_BYTES:
                priority_check()
                since_priority_check = 0
            chunk = os.read(descriptor, 1024 * 1024)
            if not chunk:
                break
            total += len(chunk)
            since_priority_check += len(chunk)
            if total > maximum_bytes:
                raise IntegrityError(f"runtime artifact exceeds {maximum_bytes} bytes: {path}")
            digest.update(chunk)
        after = os.fstat(descriptor)
        pathname = os.lstat(path)
        identity = (before.st_dev, before.st_ino)
        if identity != (after.st_dev, after.st_ino) or identity != (pathname.st_dev, pathname.st_ino):
            raise IntegrityError(f"runtime artifact changed while hashing: {path}")
        if total != after.st_size:
            raise IntegrityError(f"runtime artifact size changed while hashing: {path}")
        priority_check()
        return digest.hexdigest(), total
    finally:
        os.close(descriptor)


def _private_directory(path: Path) -> Path:
    path = path.absolute()
    parent = path.parent
    if not parent.exists():
        _private_directory(parent)
    try:
        os.mkdir(path, 0o700)
    except FileExistsError:
        pass
    info = os.lstat(path)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise AssetError(f"runtime path is not a real directory: {path}")
    if hasattr(os, "getuid") and info.st_uid != os.getuid():
        raise AssetError(f"runtime path is not owned by the current user: {path}")
    if info.st_mode & 0o022:
        raise AssetError(f"runtime path is group/other writable: {path}")
    return path


def _safe_relative_member(name: str, extraction_root: str) -> Path | None:
    if "\\" in name or "\0" in name:
        raise IntegrityError(f"source archive contains a non-portable member: {name!r}")
    pure = PurePosixPath(name)
    if pure.is_absolute() or any(part in ("", ".", "..") for part in pure.parts):
        raise IntegrityError(f"source archive contains an unsafe member: {name!r}")
    if not pure.parts or pure.parts[0] != extraction_root:
        raise IntegrityError(f"source archive member is outside {extraction_root!r}: {name!r}")
    if len(pure.parts) == 1:
        return None
    return Path(*pure.parts[1:])


def _extract_source(
    archive: Path,
    destination: Path,
    extraction_root: str,
    priority_check: Callable[[], None] | None = None,
) -> None:
    """Extract regular files/directories only, without tarfile.extract()."""
    if priority_check is None:
        priority_check = assert_priority_available
    seen: set[Path] = set()
    total = 0
    since_priority_check = PRIORITY_RECHECK_BYTES
    with tarfile.open(archive, mode="r:gz") as source:
        members = source.getmembers()
        if len(members) > MAX_ARCHIVE_MEMBERS:
            raise IntegrityError(
                f"source archive has {len(members)} members; limit is {MAX_ARCHIVE_MEMBERS}"
            )
        for member in members:
            if since_priority_check >= PRIORITY_RECHECK_BYTES:
                priority_check()
                since_priority_check = 0
            relative = _safe_relative_member(member.name, extraction_root)
            if relative is None:
                if not member.isdir():
                    raise IntegrityError("source archive root is not a directory")
                continue
            if relative in seen:
                raise IntegrityError(f"source archive repeats member: {relative.as_posix()}")
            seen.add(relative)
            target = destination / relative
            if member.isdir():
                target.mkdir(mode=0o700, parents=True, exist_ok=True)
                continue
            if not member.isreg():
                raise IntegrityError(
                    f"source archive contains a link or special file: {relative.as_posix()}"
                )
            if member.size < 0 or member.size > MAX_ARCHIVE_FILE_BYTES:
                raise IntegrityError(
                    f"source archive member exceeds {MAX_ARCHIVE_FILE_BYTES} bytes: "
                    f"{relative.as_posix()}"
                )
            total += member.size
            if total > MAX_ARCHIVE_TOTAL_BYTES:
                raise IntegrityError(
                    f"source archive expands beyond {MAX_ARCHIVE_TOTAL_BYTES} bytes"
                )
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            stream = source.extractfile(member)
            if stream is None:
                raise IntegrityError(f"cannot read source archive member: {relative.as_posix()}")
            mode = 0o700 if member.mode & 0o111 else 0o600
            flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_CLOEXEC", 0)
            flags |= getattr(os, "O_NOFOLLOW", 0)
            descriptor = os.open(target, flags, mode)
            try:
                remaining = member.size
                while remaining:
                    chunk = stream.read(min(1024 * 1024, remaining))
                    if not chunk:
                        raise IntegrityError(
                            f"truncated source archive member: {relative.as_posix()}"
                        )
                    view = memoryview(chunk)
                    while view:
                        written = os.write(descriptor, view)
                        if written <= 0:
                            raise OSError("short write while extracting llama.cpp")
                        view = view[written:]
                    remaining -= len(chunk)
                    since_priority_check += len(chunk)
                    if since_priority_check >= PRIORITY_RECHECK_BYTES:
                        priority_check()
                        since_priority_check = 0
                if stream.read(1):
                    raise IntegrityError(
                        f"oversized source archive member: {relative.as_posix()}"
                    )
            finally:
                os.close(descriptor)
                stream.close()
    priority_check()
    for required in ("CMakeLists.txt", "LICENSE", "ggml/CMakeLists.txt"):
        path = destination / required
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode):
            raise IntegrityError(f"llama.cpp source archive is missing regular {required}")


def _cmake_version(cmake: str) -> tuple[int, int, str]:
    assert_priority_available()
    result = subprocess.run(
        [cmake, "--version"],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=15,
    )
    if result.returncode != 0:
        raise AssetError(f"cmake --version failed: {result.stderr.strip()}")
    first = result.stdout.splitlines()[0] if result.stdout else ""
    match = re.search(r"([0-9]+)\.([0-9]+)(?:\.[0-9]+)?", first)
    if match is None:
        raise AssetError(f"cannot parse CMake version: {first!r}")
    return int(match.group(1)), int(match.group(2)), first


def _terminate_process_group(process: subprocess.Popen[bytes]) -> None:
    """Terminate and reap every process in a build command's private group."""
    if process.poll() is not None:
        process.wait()
        return
    try:
        if os.name == "posix":
            os.killpg(process.pid, signal.SIGTERM)
        else:
            process.terminate()
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=10)
        return
    except subprocess.TimeoutExpired:
        pass
    try:
        if os.name == "posix":
            os.killpg(process.pid, signal.SIGKILL)
        else:
            process.kill()
    except ProcessLookupError:
        pass
    process.wait(timeout=5)


def _run(
    command: list[str],
    environment: dict[str, str],
    timeout_seconds: float,
    priority_check: Callable[[], None] | None = None,
) -> None:
    """Run one bounded build group while continuously yielding to ERAIS."""
    if priority_check is None:
        priority_check = assert_priority_available
    if timeout_seconds <= 0:
        raise AssetError("build command timeout must be positive")
    priority_check()
    try:
        process = subprocess.Popen(
            command,
            stdin=subprocess.DEVNULL,
            env=environment,
            start_new_session=True,
        )
    except OSError as exc:
        raise AssetError(f"cannot start build command {command[0]}: {exc}") from exc
    deadline = time.monotonic() + timeout_seconds
    try:
        while True:
            status = process.poll()
            if status is not None:
                process.wait()
                if status != 0:
                    raise AssetError(
                        f"build command failed with status {status}: {command[0]}"
                    )
                priority_check()
                return
            priority_check()
            if time.monotonic() >= deadline:
                raise AssetError(
                    f"build command exceeded {timeout_seconds:g} seconds: {command[0]}"
                )
            time.sleep(BUILD_POLL_SECONDS)
    except BaseException:
        _terminate_process_group(process)
        raise


def _staging_identity(path: Path) -> tuple[int, int]:
    info = os.lstat(path)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise IntegrityError(f"runtime staging is not a real directory: {path}")
    if hasattr(os, "getuid") and info.st_uid != os.getuid():
        raise IntegrityError(f"runtime staging is not owned by the current user: {path}")
    if info.st_mode & 0o077:
        raise IntegrityError(f"runtime staging is not private: {path}")
    return info.st_dev, info.st_ino


def _cleanup_owned_staging(path: Path, expected: tuple[int, int]) -> None:
    """Quarantine then remove only the exact .building inode we created."""
    if ".building-" not in path.name:
        raise AssetError(f"refusing cleanup of non-building path: {path}")
    try:
        current = _staging_identity(path)
    except FileNotFoundError as exc:
        raise AssetError(f"runtime staging disappeared before cleanup: {path}") from exc
    if current != expected:
        raise AssetError(f"runtime staging inode changed; refusing cleanup: {path}")
    quarantine = Path(
        tempfile.mkdtemp(prefix=f".{path.name}.failed-", dir=path.parent)
    )
    os.chmod(quarantine, 0o700)
    try:
        os.rename(path, quarantine)
    except BaseException:
        os.rmdir(quarantine)
        raise
    if _staging_identity(quarantine) != expected:
        raise AssetError(
            f"runtime staging inode changed during quarantine; retained at {quarantine}"
        )
    try:
        shutil.rmtree(quarantine)
    except OSError as exc:
        raise AssetError(f"cannot remove quarantined runtime staging {quarantine}: {exc}") from exc


def _atomic_json(path: Path, value: object) -> None:
    encoded = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")
    temporary = path.with_name(f".{path.name}.{os.getpid()}.{time.time_ns()}.tmp")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_CLOEXEC", 0)
    flags |= getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(temporary, flags, 0o600)
    try:
        view = memoryview(encoded)
        while view:
            written = os.write(descriptor, view)
            if written <= 0:
                raise OSError("short write while storing runtime receipt")
            view = view[written:]
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    os.link(temporary, path, follow_symlinks=False)
    os.unlink(temporary)


def _runtime_name(lock: ModelLock, profile: str) -> str:
    llama = _mapping(lock.document["llama_cpp"], "llama_cpp")
    return f"{llama['tag']}-{str(llama['commit'])[:12]}-{profile}"


def _verify_existing_runtime(path: Path, lock: ModelLock, profile: str) -> dict[str, object]:
    receipt_path = path / RECEIPT_NAME
    raw = receipt_path.read_bytes()
    if len(raw) > 1024 * 1024:
        raise IntegrityError("llama.cpp runtime receipt is oversized")
    try:
        receipt = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise IntegrityError(f"invalid llama.cpp runtime receipt: {exc}") from exc
    if not isinstance(receipt, dict):
        raise IntegrityError("llama.cpp runtime receipt is not an object")
    expected = {
        "lock_sha256": lock.digest,
        "profile": profile,
        "qualification_status": "not-run",
    }
    for key, value in expected.items():
        if receipt.get(key) != value:
            raise IntegrityError(f"llama.cpp runtime receipt {key} does not match the request")
    binaries = receipt.get("binaries")
    if not isinstance(binaries, list) or not binaries:
        raise IntegrityError("llama.cpp runtime receipt has no binary inventory")
    llama = _mapping(lock.document["llama_cpp"], "llama_cpp")
    cmake_policy = _mapping(llama["cmake"], "llama_cpp.cmake")
    suffix = ".exe" if os.name == "nt" else ""
    expected_paths = {
        f"build/bin/{target}{suffix}"
        for target in _string_list(cmake_policy["targets"], "llama_cpp.cmake.targets")
    }
    observed_paths: set[str] = set()
    for entry in binaries:
        if not isinstance(entry, dict) or set(entry) != {
            "path",
            "sha256",
            "size_bytes",
        }:
            raise IntegrityError("llama.cpp runtime binary receipt is malformed")
        relative = entry.get("path")
        if not isinstance(relative, str) or relative not in expected_paths:
            raise IntegrityError("llama.cpp runtime receipt contains an unsafe binary path")
        if relative in observed_paths:
            raise IntegrityError("llama.cpp runtime receipt repeats a binary path")
        observed_paths.add(relative)
        digest, size = _sha256_file(path / relative)
        if digest != entry.get("sha256") or size != entry.get("size_bytes"):
            raise IntegrityError(
                f"llama.cpp runtime binary no longer matches receipt: {relative}"
            )
    if observed_paths != expected_paths:
        missing = sorted(expected_paths - observed_paths)
        extra = sorted(observed_paths - expected_paths)
        raise IntegrityError(
            f"llama.cpp runtime binary inventory differs: missing={missing}, extra={extra}"
        )
    return receipt


def build_runtime(
    lock: ModelLock,
    download_root: Path,
    runtime_root: Path,
    profile: str,
    cmake: str,
) -> Path:
    assert_priority_available()
    assert_resource_guard()
    llama = _mapping(lock.document["llama_cpp"], "llama_cpp")
    cmake_policy = _mapping(llama["cmake"], "llama_cpp.cmake")
    source_asset = lock.asset(str(llama["source_asset_id"]))
    archive = fetch_asset(source_asset, download_root)
    verify_cached_asset(source_asset, download_root)
    runtime_root = _private_directory(runtime_root)
    final = runtime_root / _runtime_name(lock, profile)
    if final.exists():
        _verify_existing_runtime(final, lock, profile)
        return final
    minimum = tuple(int(part) for part in str(cmake_policy["minimum_version"]).split("."))
    major, minor, cmake_description = _cmake_version(cmake)
    if (major, minor) < minimum:
        raise AssetError(
            f"CMake {minimum[0]}.{minimum[1]} or newer is required; got {major}.{minor}"
        )
    assert_disk_headroom(
        runtime_root,
        RUNTIME_STAGING_WRITE_BYTES,
        "llama.cpp runtime staging",
    )
    staging = Path(tempfile.mkdtemp(prefix=f".{final.name}.building-", dir=runtime_root))
    os.chmod(staging, 0o700)
    staging_identity = _staging_identity(staging)
    published = False
    try:
        source = staging / "source"
        build = staging / "build"
        source.mkdir(mode=0o700)
        extraction_root = source_asset.extraction_root
        if extraction_root is None:
            raise IntegrityError("locked llama.cpp source has no extraction root")
        _extract_source(archive, source, extraction_root)
        options = _string_list(cmake_policy["common_options"], "common_options")
        profiles = _mapping(cmake_policy["profiles"], "profiles")
        options.extend(_string_list(profiles[profile], f"profiles.{profile}"))
        release_number = str(llama["tag"])[1:]
        options.extend(
            [
                f"-DCMAKE_BUILD_TYPE={cmake_policy['build_type']}",
                f"-DLLAMA_BUILD_COMMIT={llama['commit']}",
                f"-DLLAMA_BUILD_NUMBER={release_number}",
            ]
        )
        environment = {
            "HOME": os.environ.get("HOME", "/nonexistent"),
            "LANG": "C",
            "LC_ALL": "C",
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "SOURCE_DATE_EPOCH": "0",
            "TZ": "UTC",
        }
        configure = [
            cmake,
            "-S",
            str(source),
            "-B",
            str(build),
            "-G",
            str(cmake_policy["generator"]),
            *options,
        ]
        targets = _string_list(cmake_policy["targets"], "targets")
        jobs = int(cmake_policy["parallel_jobs"])
        build_command = [
            cmake,
            "--build",
            str(build),
            "--parallel",
            str(jobs),
            "--target",
            *targets,
        ]
        _run(configure, environment, CONFIGURE_TIMEOUT_SECONDS)
        _run(build_command, environment, BUILD_TIMEOUT_SECONDS)
        suffix = ".exe" if os.name == "nt" else ""
        binaries: list[dict[str, object]] = []
        for target in targets:
            path = build / "bin" / f"{target}{suffix}"
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                raise IntegrityError(f"llama.cpp build did not produce regular {path.name}")
            if os.name != "nt" and not info.st_mode & 0o100:
                raise IntegrityError(f"llama.cpp build output is not executable: {path.name}")
            digest, size = _sha256_file(path)
            binaries.append(
                {
                    "path": path.relative_to(staging).as_posix(),
                    "sha256": digest,
                    "size_bytes": size,
                }
            )
        receipt = {
            "schema_version": "1.0.0",
            "runtime": "llama.cpp",
            "tag": llama["tag"],
            "commit": llama["commit"],
            "source_sha256": source_asset.sha256,
            "source_size_bytes": source_asset.size_bytes,
            "lock_sha256": lock.digest,
            "profile": profile,
            "cmake": cmake_description,
            "configure_argv": configure,
            "build_argv": build_command,
            "platform": platform.platform(),
            "machine": platform.machine(),
            "python": platform.python_version(),
            "binaries": binaries,
            "qualification_status": "not-run",
            "default_model_status": "none",
        }
        _atomic_json(staging / RECEIPT_NAME, receipt)
        assert_priority_available()
        try:
            os.rename(staging, final)
            published = True
        except FileExistsError:
            _verify_existing_runtime(final, lock, profile)
            raise AssetError(f"a concurrent identical runtime was published at {final}")
        _verify_existing_runtime(final, lock, profile)
        return final
    except BaseException:
        if not published:
            _cleanup_owned_staging(staging, staging_identity)
        raise


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lock", type=Path, default=DEFAULT_LOCK)
    parser.add_argument("--download-root", type=Path, default=DEFAULT_DOWNLOAD_ROOT)
    parser.add_argument("--runtime-root", type=Path, default=DEFAULT_RUNTIME_ROOT)
    parser.add_argument("--profile", choices=("portable", "native"), default="portable")
    parser.add_argument("--cmake", default="cmake")
    return parser


def main(argv: list[str] | None = None) -> int:
    arguments = build_parser().parse_args(argv)
    try:
        lock = load_lock(arguments.lock)
        runtime = build_runtime(
            lock,
            arguments.download_root,
            arguments.runtime_root,
            arguments.profile,
            arguments.cmake,
        )
        print(
            json.dumps(
                {
                    "ok": True,
                    "runtime": str(runtime),
                    "receipt": str(runtime / RECEIPT_NAME),
                },
                indent=2,
                sort_keys=True,
            )
        )
        return 0
    except PriorityBlocked as exc:
        print(f"llama.cpp bootstrap deferred: {exc}", file=sys.stderr)
        return 75
    except (AssetError, OSError, tarfile.TarError) as exc:
        print(f"llama.cpp bootstrap failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
