#!/usr/bin/env python3
"""Verify and fetch immutable RKC model/runtime assets without a shell.

The lock file is repository policy, not a hint. Downloads are bounded by the
locked byte count, hashed while streaming, written through a private temporary
inode, and published with a no-replace hard link. Existing paths are reused only
after an inode-bound verification; links and non-regular files fail closed.
"""
from __future__ import annotations

import argparse
import hashlib
import ipaddress
import json
import os
import re
import secrets
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO, Callable, Iterable, Mapping, NoReturn

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_LOCK = ROOT / "models" / "models.lock.json"
LOCK_SCHEMA_VERSION = "1.0.0"
MAX_LOCK_BYTES = 1024 * 1024
MAX_ASSET_BYTES = 8 * 1024 * 1024 * 1024
DOWNLOAD_CHUNK_BYTES = 1024 * 1024
PRIORITY_RECHECK_BYTES = 16 * 1024 * 1024
MIN_DISK_HEADROOM_BYTES = 4 * 1024 * 1024 * 1024
PRIORITY_NAMES = (b"erais", b"torchrun", b"lm_eval")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
REVISION_RE = re.compile(r"^[0-9a-f]{40}$")
ID_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,95}$")
FILENAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,159}$")
CMAKE_OPTION_RE = re.compile(r"^-D[A-Z0-9_]+=[A-Za-z0-9_.:+/-]+$")


class AssetError(RuntimeError):
    """Base class for model supply-chain failures."""


class LockError(AssetError):
    """The checked-in lock is malformed or internally inconsistent."""


class IntegrityError(AssetError):
    """A local or downloaded asset does not match its immutable lock."""


class MissingAsset(IntegrityError):
    """The requested immutable cache entry is absent."""


class PriorityBlocked(AssetError):
    """A higher-priority ERAIS workload is active."""


@dataclass(frozen=True)
class Asset:
    """One immutable downloadable object from the model lock."""

    asset_id: str
    kind: str
    status: str
    default_eligible: bool
    repository: str
    revision: str
    filename: str
    url: str
    allowed_hosts: tuple[str, ...]
    sha256: str
    size_bytes: int
    license_spdx: str
    license_url: str
    redistribution: str
    quantization: str | None
    native_context_tokens: int | None
    qualification_spec: str | None
    extraction_root: str | None


@dataclass(frozen=True)
class ModelLock:
    """Validated lock document plus a digest of its exact checked-in bytes."""

    path: Path
    digest: str
    document: Mapping[str, object]
    assets: tuple[Asset, ...]

    def asset(self, asset_id: str) -> Asset:
        matches = [asset for asset in self.assets if asset.asset_id == asset_id]
        if len(matches) != 1:
            raise LockError(f"model asset is not uniquely locked: {asset_id!r}")
        return matches[0]


def _fail(message: str) -> NoReturn:
    raise LockError(message)


def _strict_object(pairs: Iterable[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise LockError(f"duplicate JSON object key: {key!r}")
        result[key] = value
    return result


def _same_inode(left: os.stat_result, right: os.stat_result) -> bool:
    return left.st_dev == right.st_dev and left.st_ino == right.st_ino


def _read_regular(path: Path, maximum_bytes: int) -> bytes:
    """Read a bounded regular file and prove its pathname stayed on one inode."""
    flags = os.O_RDONLY
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise LockError(f"open {path}: {exc}") from exc
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode):
            raise LockError(f"{path} must be a regular file")
        if before.st_size > maximum_bytes:
            raise LockError(f"{path} exceeds {maximum_bytes} bytes")
        chunks: list[bytes] = []
        total = 0
        while True:
            chunk = os.read(descriptor, min(128 * 1024, maximum_bytes + 1 - total))
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > maximum_bytes:
                raise LockError(f"{path} exceeds {maximum_bytes} bytes")
        after = os.fstat(descriptor)
        pathname = os.lstat(path)
        if not _same_inode(before, after) or not _same_inode(after, pathname):
            raise LockError(f"{path} changed while it was read")
        if after.st_size != total:
            raise LockError(f"{path} size changed while it was read")
        return b"".join(chunks)
    finally:
        os.close(descriptor)


def _exact_keys(value: Mapping[str, object], expected: set[str], label: str) -> None:
    actual = set(value)
    missing = sorted(expected - actual)
    extra = sorted(actual - expected)
    if missing or extra:
        _fail(f"{label} keys differ: missing={missing}, extra={extra}")


def _string(value: object, label: str) -> str:
    if not isinstance(value, str):
        _fail(f"{label} must be a string")
    return value


def _boolean(value: object, label: str) -> bool:
    if not isinstance(value, bool):
        _fail(f"{label} must be a boolean")
    return value


def _integer(value: object, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        _fail(f"{label} must be an integer")
    return value


def _optional_string(value: object, label: str) -> str | None:
    if value is None:
        return None
    return _string(value, label)


def _optional_integer(value: object, label: str) -> int | None:
    if value is None:
        return None
    return _integer(value, label)


def _https_url(value: object, label: str) -> str:
    url = _string(value, label)
    parsed = urllib.parse.urlsplit(url)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.fragment
    ):
        _fail(f"{label} must be an HTTPS URL without credentials or a fragment")
    try:
        ipaddress.ip_address(parsed.hostname)
    except ValueError:
        pass
    else:
        _fail(f"{label} must use a DNS hostname, not an IP literal")
    return url


def _host_allowed(host: str, allowed_hosts: tuple[str, ...]) -> bool:
    host = host.rstrip(".").lower()
    for rule in allowed_hosts:
        if rule.startswith("."):
            if host.endswith(rule) and host != rule[1:]:
                return True
        elif host == rule:
            return True
    return False


def _validate_hosts(value: object, label: str) -> tuple[str, ...]:
    if not isinstance(value, list) or not 1 <= len(value) <= 8:
        _fail(f"{label} must contain between one and eight host rules")
    hosts: list[str] = []
    host_re = re.compile(
        r"^\.?(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\."
        r"(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))*$"
    )
    for index, raw in enumerate(value):
        host = _string(raw, f"{label}[{index}]").lower()
        if not host_re.fullmatch(host):
            _fail(f"{label}[{index}] is not a valid DNS host rule")
        hosts.append(host)
    if len(hosts) != len(set(hosts)):
        _fail(f"{label} contains duplicate host rules")
    return tuple(hosts)


def _validate_cmake(llama: Mapping[str, object]) -> None:
    expected = {
        "minimum_version",
        "generator",
        "build_type",
        "parallel_jobs",
        "targets",
        "common_options",
        "profiles",
    }
    raw = llama.get("cmake")
    if not isinstance(raw, dict):
        _fail("llama_cpp.cmake must be an object")
    _exact_keys(raw, expected, "llama_cpp.cmake")
    minimum = _string(raw["minimum_version"], "llama_cpp.cmake.minimum_version")
    if not re.fullmatch(r"[0-9]+\.[0-9]+", minimum):
        _fail("llama_cpp.cmake.minimum_version must be major.minor")
    if raw["generator"] not in ("Ninja", "Unix Makefiles"):
        _fail("llama_cpp.cmake.generator is not approved")
    if raw["build_type"] != "Release":
        _fail("llama_cpp.cmake.build_type must be Release")
    jobs = _integer(raw["parallel_jobs"], "llama_cpp.cmake.parallel_jobs")
    if not 1 <= jobs <= 2:
        _fail("llama_cpp.cmake.parallel_jobs must be between 1 and 2")
    targets = raw["targets"]
    if not isinstance(targets, list) or not 1 <= len(targets) <= 8:
        _fail("llama_cpp.cmake.targets must contain one to eight targets")
    target_values = [_string(value, "llama_cpp.cmake.targets[]") for value in targets]
    if len(target_values) != len(set(target_values)):
        _fail("llama_cpp.cmake.targets contains duplicates")
    required_targets = {"llama-cli", "llama-server", "llama-embedding", "llama-bench"}
    if required_targets - set(target_values):
        _fail(
            "llama_cpp.cmake.targets must build llama-cli, llama-server, "
            "llama-embedding, and llama-bench"
        )

    def validate_options(value: object, label: str) -> None:
        if not isinstance(value, list) or not 1 <= len(value) <= 64:
            _fail(f"{label} must contain one to 64 options")
        options = [_string(option, f"{label}[]") for option in value]
        if any(not CMAKE_OPTION_RE.fullmatch(option) for option in options):
            _fail(f"{label} contains an invalid CMake cache option")
        if len(options) != len(set(options)):
            _fail(f"{label} contains duplicate options")

    validate_options(raw["common_options"], "llama_cpp.cmake.common_options")
    profiles = raw["profiles"]
    if not isinstance(profiles, dict):
        _fail("llama_cpp.cmake.profiles must be an object")
    _exact_keys(profiles, {"portable", "native"}, "llama_cpp.cmake.profiles")
    validate_options(profiles["portable"], "llama_cpp.cmake.profiles.portable")
    validate_options(profiles["native"], "llama_cpp.cmake.profiles.native")


def _parse_asset(value: object, index: int) -> Asset:
    if not isinstance(value, dict):
        _fail(f"assets[{index}] must be an object")
    expected = {
        "id",
        "kind",
        "status",
        "default_eligible",
        "repository",
        "revision",
        "filename",
        "url",
        "allowed_hosts",
        "sha256",
        "size_bytes",
        "license_spdx",
        "license_url",
        "redistribution",
        "quantization",
        "native_context_tokens",
        "qualification_spec",
        "extraction_root",
    }
    _exact_keys(value, expected, f"assets[{index}]")
    label = f"assets[{index}]"
    asset_id = _string(value["id"], f"{label}.id")
    if not ID_RE.fullmatch(asset_id):
        _fail(f"{label}.id has an invalid portable identifier")
    kind = _string(value["kind"], f"{label}.kind")
    if kind not in ("source-archive", "generation-model", "embedding-model"):
        _fail(f"{label}.kind is unsupported")
    status_value = _string(value["status"], f"{label}.status")
    if status_value not in ("runtime-pinned", "unqualified", "qualified", "rejected"):
        _fail(f"{label}.status is unsupported")
    filename = _string(value["filename"], f"{label}.filename")
    if not FILENAME_RE.fullmatch(filename) or Path(filename).name != filename:
        _fail(f"{label}.filename must be one portable basename")
    revision = _string(value["revision"], f"{label}.revision")
    if not REVISION_RE.fullmatch(revision):
        _fail(f"{label}.revision must be a full lowercase commit digest")
    digest = _string(value["sha256"], f"{label}.sha256")
    if not SHA256_RE.fullmatch(digest):
        _fail(f"{label}.sha256 must be a lowercase SHA-256")
    size_bytes = _integer(value["size_bytes"], f"{label}.size_bytes")
    if not 1 <= size_bytes <= MAX_ASSET_BYTES:
        _fail(f"{label}.size_bytes exceeds the global download boundary")
    url = _https_url(value["url"], f"{label}.url")
    allowed_hosts = _validate_hosts(value["allowed_hosts"], f"{label}.allowed_hosts")
    hostname = urllib.parse.urlsplit(url).hostname or ""
    if not _host_allowed(hostname, allowed_hosts):
        _fail(f"{label}.url host is not present in its redirect allowlist")
    if revision not in urllib.parse.unquote(url):
        _fail(f"{label}.url must embed the immutable revision")
    license_spdx = _string(value["license_spdx"], f"{label}.license_spdx")
    if license_spdx not in ("Apache-2.0", "MIT"):
        _fail(f"{label}.license_spdx is not compatible with this lock version")
    if value["redistribution"] != "not-bundled-download-on-demand":
        _fail(f"{label}.redistribution must preserve the no-bundling boundary")
    quantization = _optional_string(value["quantization"], f"{label}.quantization")
    if quantization is not None and not re.fullmatch(r"[A-Za-z0-9_.-]{1,32}", quantization):
        _fail(f"{label}.quantization is invalid")
    context_tokens = _optional_integer(
        value["native_context_tokens"], f"{label}.native_context_tokens"
    )
    if context_tokens is not None and not 1 <= context_tokens <= 1024 * 1024:
        _fail(f"{label}.native_context_tokens is outside the supported bound")
    qualification = _optional_string(value["qualification_spec"], f"{label}.qualification_spec")
    extraction_root = _optional_string(value["extraction_root"], f"{label}.extraction_root")
    if extraction_root is not None and not FILENAME_RE.fullmatch(extraction_root):
        _fail(f"{label}.extraction_root must be one portable directory name")
    if kind == "source-archive":
        if status_value != "runtime-pinned" or license_spdx != "MIT":
            _fail(f"{label} source archive must be runtime-pinned under MIT")
        if any(item is not None for item in (quantization, context_tokens, qualification)):
            _fail(f"{label} source archive has model-only metadata")
        if extraction_root is None:
            _fail(f"{label} source archive requires extraction_root")
    else:
        if license_spdx != "Apache-2.0":
            _fail(f"{label} model must use Apache-2.0")
        if status_value == "runtime-pinned":
            _fail(f"{label} model cannot use the runtime-pinned status")
        if quantization is None or context_tokens is None or qualification is None:
            _fail(f"{label} model requires quantization, context, and qualification metadata")
        qualification_path = Path(qualification)
        if (
            qualification_path.is_absolute()
            or ".." in qualification_path.parts
            or qualification_path.parts[:2] != ("models", "qualification")
            or qualification_path.suffix != ".json"
        ):
            _fail(f"{label}.qualification_spec escapes the qualification directory")
        if extraction_root is not None:
            _fail(f"{label} model must not define extraction_root")
    default_eligible = _boolean(value["default_eligible"], f"{label}.default_eligible")
    if default_eligible and status_value != "qualified":
        _fail(f"{label} is default-eligible without a qualified status")
    return Asset(
        asset_id=asset_id,
        kind=kind,
        status=status_value,
        default_eligible=default_eligible,
        repository=_https_url(value["repository"], f"{label}.repository"),
        revision=revision,
        filename=filename,
        url=url,
        allowed_hosts=allowed_hosts,
        sha256=digest,
        size_bytes=size_bytes,
        license_spdx=license_spdx,
        license_url=_https_url(value["license_url"], f"{label}.license_url"),
        redistribution=_string(value["redistribution"], f"{label}.redistribution"),
        quantization=quantization,
        native_context_tokens=context_tokens,
        qualification_spec=qualification,
        extraction_root=extraction_root,
    )


def load_lock(path: Path = DEFAULT_LOCK) -> ModelLock:
    """Load and semantically validate the exact model lock."""
    path = path.absolute()
    raw = _read_regular(path, MAX_LOCK_BYTES)
    try:
        document = json.loads(raw.decode("utf-8"), object_pairs_hook=_strict_object)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise LockError(f"parse {path}: {exc}") from exc
    if not isinstance(document, dict):
        _fail("model lock root must be an object")
    _exact_keys(
        document,
        {
            "$schema",
            "schema_version",
            "default_generation_model",
            "default_embedding_model",
            "llama_cpp",
            "assets",
        },
        "model lock",
    )
    if document["$schema"] != "../schemas/model-lock.schema.json":
        _fail("model lock $schema is not the checked-in schema")
    if document["schema_version"] != LOCK_SCHEMA_VERSION:
        _fail(f"unsupported model lock schema: {document['schema_version']!r}")
    llama = document["llama_cpp"]
    if not isinstance(llama, dict):
        _fail("llama_cpp must be an object")
    _exact_keys(
        llama,
        {
            "repository",
            "tag",
            "commit",
            "license_spdx",
            "license_url",
            "source_asset_id",
            "cmake",
        },
        "llama_cpp",
    )
    if llama["repository"] != "https://github.com/ggml-org/llama.cpp":
        _fail("llama_cpp.repository is not the reviewed upstream")
    tag = _string(llama["tag"], "llama_cpp.tag")
    if not re.fullmatch(r"b[0-9]+", tag):
        _fail("llama_cpp.tag is invalid")
    commit = _string(llama["commit"], "llama_cpp.commit")
    if not REVISION_RE.fullmatch(commit):
        _fail("llama_cpp.commit must be a full lowercase commit digest")
    if llama["license_spdx"] != "MIT":
        _fail("llama_cpp.license_spdx must preserve upstream MIT licensing")
    _https_url(llama["license_url"], "llama_cpp.license_url")
    _validate_cmake(llama)
    raw_assets = document["assets"]
    if not isinstance(raw_assets, list) or not 3 <= len(raw_assets) <= 32:
        _fail("assets must contain between three and 32 entries")
    assets = tuple(_parse_asset(value, index) for index, value in enumerate(raw_assets))
    ids = [asset.asset_id for asset in assets]
    if len(ids) != len(set(ids)):
        _fail("model lock contains duplicate asset identifiers")
    source_id = _string(llama["source_asset_id"], "llama_cpp.source_asset_id")
    source = next((asset for asset in assets if asset.asset_id == source_id), None)
    if source is None or source.kind != "source-archive" or source.revision != commit:
        _fail("llama_cpp.source_asset_id does not bind the pinned commit archive")
    if tag not in source.filename:
        _fail("llama.cpp source filename does not record the pinned release tag")
    for default_key, kind in (
        ("default_generation_model", "generation-model"),
        ("default_embedding_model", "embedding-model"),
    ):
        default_id = document[default_key]
        if default_id is None:
            continue
        if not isinstance(default_id, str) or not ID_RE.fullmatch(default_id):
            _fail(f"{default_key} is not a valid asset identifier")
        candidate = next((asset for asset in assets if asset.asset_id == default_id), None)
        if (
            candidate is None
            or candidate.kind != kind
            or candidate.status != "qualified"
            or not candidate.default_eligible
        ):
            _fail(f"{default_key} does not name a qualified, default-eligible asset")
    return ModelLock(
        path=path,
        digest=hashlib.sha256(raw).hexdigest(),
        document=document,
        assets=assets,
    )


def _ancestor_pids() -> set[int]:
    """Return this process and its Linux parent chain.

    The invoking shell can contain the literal priority preflight expression in
    its command line. Treating that wrapper as an independent workload produces
    a false positive, so wrapper ancestors are ignored unless their process name
    itself identifies a priority workload.
    """
    ancestors: set[int] = set()
    pid = os.getpid()
    for _ in range(64):
        if pid <= 0 or pid in ancestors:
            break
        ancestors.add(pid)
        try:
            raw = Path(f"/proc/{pid}/stat").read_text(
                encoding="utf-8", errors="replace"
            )
            closing = raw.rfind(")")
            fields = raw[closing + 2 :].split()
            if closing < 0 or len(fields) < 2:
                break
            pid = int(fields[1])
        except (
            FileNotFoundError,
            PermissionError,
            ProcessLookupError,
            OSError,
            ValueError,
        ):
            break
    return ancestors


def _matches_priority_process(
    pid: int,
    command: bytes,
    process_name: bytes,
    ancestors: set[int],
) -> bool:
    """Match a real priority workload without matching an invoking wrapper."""
    process_name = process_name.lower()
    if pid in ancestors and not any(name in process_name for name in PRIORITY_NAMES):
        return False
    lowered = command.lower()
    return any(name in lowered for name in PRIORITY_NAMES)


def active_priority_processes() -> list[tuple[int, str]]:
    """Return bounded command summaries for higher-priority ERAIS processes."""
    matches: list[tuple[int, str]] = []
    if sys.platform.startswith("linux") and Path("/proc").is_dir():
        ancestors = _ancestor_pids()
        for entry in Path("/proc").iterdir():
            if not entry.name.isdigit():
                continue
            pid = int(entry.name)
            try:
                command = (entry / "cmdline").read_bytes()[:4096].replace(b"\0", b" ")
                process_name = (entry / "comm").read_bytes()[:256].strip().lower()
            except (FileNotFoundError, PermissionError, ProcessLookupError, OSError):
                continue
            if _matches_priority_process(pid, command, process_name, ancestors):
                summary = command.decode("utf-8", errors="replace")[:300]
                matches.append((pid, summary))
        return sorted(matches)
    try:
        result = subprocess.run(
            ["pgrep", "-af", "[e]rais|[t]orchrun|[l]m_eval"],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=5,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        raise PriorityBlocked(f"cannot prove ERAIS is idle on this platform: {exc}") from exc
    for line in result.stdout.decode("utf-8", errors="replace").splitlines()[:32]:
        raw_pid, _, command = line.partition(" ")
        if raw_pid.isdigit() and int(raw_pid) != os.getpid():
            matches.append((int(raw_pid), command[:300]))
    return matches


def assert_priority_available() -> None:
    """Fail before or during heavy work whenever ERAIS is active."""
    matches = active_priority_processes()
    if matches:
        summary = "; ".join(f"pid={pid} {command}" for pid, command in matches[:4])
        raise PriorityBlocked(f"ERAIS has priority; RKC work is deferred: {summary}")


def _read_cgroup_value(directory: Path, name: str) -> str:
    try:
        return (directory / name).read_text(encoding="ascii").strip()
    except OSError as exc:
        raise AssetError(f"resource guard value {name} is unavailable: {exc}") from exc


def assert_resource_guard() -> None:
    """Prove the current process is inside RKC's low-priority Linux cgroup."""
    if not sys.platform.startswith("linux"):
        raise AssetError("model download/build/qualification requires the Linux cgroup v2 guard")
    try:
        unified = next(
            line.split(":", 2)[2]
            for line in Path("/proc/self/cgroup").read_text(encoding="ascii").splitlines()
            if line.startswith("0::")
        )
    except (OSError, StopIteration) as exc:
        raise AssetError("current process has no unified cgroup v2 path") from exc
    unit = Path(unified).name
    if not (unit.startswith("rkc-low-") and unit.endswith((".scope", ".service"))):
        raise AssetError(f"current process is outside an RKC resource guard: {unit!r}")
    cgroup = Path("/sys/fs/cgroup") / unified.lstrip("/")
    if _read_cgroup_value(cgroup, "cpu.weight") != "1":
        raise AssetError("RKC resource guard CPUWeight is not 1")
    quota, period = _read_cgroup_value(cgroup, "cpu.max").split()
    if quota == "max" or int(quota) > int(period):
        raise AssetError("RKC resource guard exceeds one CPU core")
    expected = {
        "memory.high": str(2 * 1024 * 1024 * 1024),
        "memory.max": str(2560 * 1024 * 1024),
        "memory.swap.max": str(256 * 1024 * 1024),
        "pids.max": "128",
    }
    for name, value in expected.items():
        if _read_cgroup_value(cgroup, name) != value:
            raise AssetError(f"RKC resource guard {name} does not equal {value}")
    io_weight = cgroup / "io.weight"
    if io_weight.exists() and "default 1" not in _read_cgroup_value(cgroup, "io.weight").splitlines():
        raise AssetError("RKC resource guard IOWeight is not 1")
    if os.getpriority(os.PRIO_PROCESS, 0) != 19:
        raise AssetError("RKC process nice level is not 19")
    if Path("/proc/self/oom_score_adj").read_text(encoding="ascii").strip() != "750":
        raise AssetError("RKC process OOM score adjustment is not 750")
    try:
        ionice = subprocess.run(
            ["ionice", "-p", str(os.getpid())],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=5,
            text=True,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        raise AssetError(f"cannot inspect RKC I/O priority: {exc}") from exc
    if ionice.returncode != 0 or not ionice.stdout.lower().startswith("idle"):
        raise AssetError("RKC process I/O scheduling class is not idle")


def _assert_no_symlink_components(path: Path) -> None:
    absolute = path.absolute()
    current = Path(absolute.anchor)
    for part in absolute.parts[1:]:
        current /= part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            raise AssetError(f"cache parent does not exist: {current}")
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise AssetError(f"cache path component is not a real directory: {current}")


def _open_cache_root(path: Path) -> tuple[Path, int, os.stat_result]:
    path = path.absolute()
    _assert_no_symlink_components(path.parent)
    try:
        os.mkdir(path, 0o700)
    except FileExistsError:
        pass
    info = os.lstat(path)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise AssetError(f"cache root must be a real directory: {path}")
    if hasattr(os, "getuid") and info.st_uid != os.getuid():
        raise AssetError(f"cache root is not owned by the current user: {path}")
    if info.st_mode & 0o022:
        raise AssetError(f"cache root is group/other writable: {path}")
    flags = os.O_RDONLY
    for optional in ("O_CLOEXEC", "O_DIRECTORY", "O_NOFOLLOW"):
        flags |= getattr(os, optional, 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise AssetError(f"open cache root {path}: {exc}") from exc
    opened = os.fstat(descriptor)
    if not _same_inode(info, opened):
        os.close(descriptor)
        raise AssetError(f"cache root changed while it was opened: {path}")
    return path, descriptor, opened


def _required_free_bytes(write_bytes: int) -> int:
    """Return a conservative free-space floor for one staged write."""
    if write_bytes < 0:
        raise AssetError("planned disk write cannot be negative")
    return write_bytes + max(MIN_DISK_HEADROOM_BYTES, write_bytes // 4)


def assert_disk_headroom(
    directory: Path | int,
    write_bytes: int,
    operation: str,
) -> None:
    """Fail before staging unless the exact filesystem has bounded headroom."""
    try:
        values = os.fstatvfs(directory) if isinstance(directory, int) else os.statvfs(directory)
    except OSError as exc:
        raise AssetError(f"cannot inspect disk headroom for {operation}: {exc}") from exc
    fragment_size = values.f_frsize or values.f_bsize
    available = values.f_bavail * fragment_size
    required = _required_free_bytes(write_bytes)
    if available < required:
        raise AssetError(
            f"insufficient disk headroom for {operation}: "
            f"available={available}, required={required}"
        )


def _verify_open_asset(
    descriptor: int,
    asset: Asset,
    priority_check: Callable[[], None] = assert_priority_available,
) -> tuple[str, int]:
    before = os.fstat(descriptor)
    if not stat.S_ISREG(before.st_mode):
        raise IntegrityError(f"{asset.filename} is not a regular file")
    if before.st_size != asset.size_bytes:
        raise IntegrityError(
            f"{asset.filename} size mismatch: expected {asset.size_bytes}, got {before.st_size}"
        )
    digest = hashlib.sha256()
    total = 0
    since_priority_check = PRIORITY_RECHECK_BYTES
    while True:
        if since_priority_check >= PRIORITY_RECHECK_BYTES:
            priority_check()
            since_priority_check = 0
        chunk = os.read(descriptor, DOWNLOAD_CHUNK_BYTES)
        if not chunk:
            break
        total += len(chunk)
        since_priority_check += len(chunk)
        if total > asset.size_bytes:
            raise IntegrityError(f"{asset.filename} exceeded its locked byte count")
        digest.update(chunk)
    after = os.fstat(descriptor)
    if not _same_inode(before, after) or after.st_size != total:
        raise IntegrityError(f"{asset.filename} changed while it was verified")
    actual = digest.hexdigest()
    if actual != asset.sha256:
        raise IntegrityError(
            f"{asset.filename} SHA-256 mismatch: expected {asset.sha256}, got {actual}"
        )
    priority_check()
    return actual, total


def verify_cached_asset(
    asset: Asset,
    cache_root: Path,
    *,
    priority_check: Callable[[], None] = assert_priority_available,
) -> Path:
    """Verify a cached asset through a no-follow directory descriptor."""
    priority_check()
    root, root_fd, root_info = _open_cache_root(cache_root)
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        try:
            descriptor = os.open(asset.filename, flags, dir_fd=root_fd)
        except FileNotFoundError as exc:
            raise MissingAsset(f"cached asset is absent: {asset.filename}") from exc
        except OSError as exc:
            raise IntegrityError(f"open cached asset {asset.filename}: {exc}") from exc
        try:
            _verify_open_asset(descriptor, asset, priority_check)
            pathname = os.stat(asset.filename, dir_fd=root_fd, follow_symlinks=False)
            if not _same_inode(os.fstat(descriptor), pathname):
                raise IntegrityError(f"cached asset pathname changed: {asset.filename}")
        finally:
            os.close(descriptor)
        if not _same_inode(root_info, os.lstat(root)):
            raise IntegrityError(f"cache root pathname changed during verification: {root}")
        return root / asset.filename
    finally:
        os.close(root_fd)


class _StrictRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Reject downgrade, credential, IP-literal, and off-policy redirects."""

    def __init__(self, asset: Asset) -> None:
        super().__init__()
        self.asset = asset

    def redirect_request(self, req, fp, code, msg, headers, newurl):  # type: ignore[no-untyped-def]
        _validate_fetch_url(newurl, self.asset)
        redirects = getattr(req, "redirect_dict", {})
        if sum(redirects.values()) >= 5:
            raise IntegrityError("asset download exceeded five redirects")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def _validate_fetch_url(url: str, asset: Asset) -> None:
    parsed = urllib.parse.urlsplit(url)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.fragment
    ):
        raise IntegrityError("asset fetch URL is not credential-free HTTPS")
    try:
        ipaddress.ip_address(parsed.hostname)
    except ValueError:
        pass
    else:
        raise IntegrityError("asset fetch URL resolved to an IP-literal authority")
    if not _host_allowed(parsed.hostname, asset.allowed_hosts):
        raise IntegrityError(f"asset fetch redirected to an unapproved host: {parsed.hostname}")


def _default_opener(asset: Asset):
    return urllib.request.build_opener(_StrictRedirectHandler(asset))


def _write_all(descriptor: int, value: bytes) -> None:
    view = memoryview(value)
    while view:
        written = os.write(descriptor, view)
        if written <= 0:
            raise OSError("short write while storing downloaded asset")
        view = view[written:]


def _download_to_descriptor(
    asset: Asset,
    descriptor: int,
    opener,
    priority_check: Callable[[], None],
) -> None:
    _validate_fetch_url(asset.url, asset)
    request = urllib.request.Request(
        asset.url,
        headers={
            "Accept": "application/octet-stream",
            "Accept-Encoding": "identity",
            "User-Agent": "RKC-model-bootstrap/1.0",
        },
        method="GET",
    )
    try:
        response_context = opener.open(request, timeout=60)
    except (OSError, urllib.error.URLError) as exc:
        raise AssetError(f"download {asset.asset_id}: {exc}") from exc
    with response_context as response:
        status_code = getattr(response, "status", None) or response.getcode()
        if status_code != 200:
            raise AssetError(f"download {asset.asset_id}: unexpected HTTP {status_code}")
        _validate_fetch_url(response.geturl(), asset)
        content_encoding = response.headers.get("Content-Encoding", "identity").lower()
        if content_encoding not in ("", "identity"):
            raise IntegrityError(f"download uses unsupported content encoding: {content_encoding}")
        raw_length = response.headers.get("Content-Length")
        if raw_length is not None:
            try:
                advertised = int(raw_length)
            except ValueError as exc:
                raise IntegrityError("download Content-Length is not an integer") from exc
            if advertised != asset.size_bytes:
                raise IntegrityError(
                    f"download size header mismatch: expected {asset.size_bytes}, got {advertised}"
                )
        digest = hashlib.sha256()
        total = 0
        since_priority_check = PRIORITY_RECHECK_BYTES
        while True:
            if since_priority_check >= PRIORITY_RECHECK_BYTES:
                priority_check()
                since_priority_check = 0
            chunk = response.read(DOWNLOAD_CHUNK_BYTES)
            if not chunk:
                break
            total += len(chunk)
            since_priority_check += len(chunk)
            if total > asset.size_bytes:
                raise IntegrityError(f"download exceeded {asset.size_bytes} locked bytes")
            digest.update(chunk)
            _write_all(descriptor, chunk)
        if total != asset.size_bytes:
            raise IntegrityError(
                f"download size mismatch: expected {asset.size_bytes}, got {total}"
            )
        actual = digest.hexdigest()
        if actual != asset.sha256:
            raise IntegrityError(
                f"download SHA-256 mismatch: expected {asset.sha256}, got {actual}"
            )


def _fsync_directory(descriptor: int) -> None:
    try:
        os.fsync(descriptor)
    except OSError as exc:
        if exc.errno not in (22, 95):
            raise


def _unlink_exact_temporary(
    root_fd: int,
    name: str,
    expected: os.stat_result,
) -> None:
    """Remove a download temporary only while its exact inode remains bound."""
    try:
        current = os.stat(name, dir_fd=root_fd, follow_symlinks=False)
    except FileNotFoundError:
        return
    if not _same_inode(expected, current) or not stat.S_ISREG(current.st_mode):
        raise IntegrityError(f"download temporary inode changed; refusing cleanup: {name}")
    os.unlink(name, dir_fd=root_fd)


def fetch_asset(
    asset: Asset,
    cache_root: Path,
    *,
    require_guard: bool = True,
    opener=None,
    priority_check: Callable[[], None] = assert_priority_available,
) -> Path:
    """Fetch one locked asset and atomically publish it without replacement."""
    priority_check()
    if require_guard:
        assert_resource_guard()
    try:
        return verify_cached_asset(asset, cache_root, priority_check=priority_check)
    except MissingAsset:
        pass
    root, root_fd, root_info = _open_cache_root(cache_root)
    assert_disk_headroom(root_fd, asset.size_bytes, f"download {asset.asset_id}")
    temporary = f".download-{secrets.token_hex(16)}.part"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_CLOEXEC", 0)
    flags |= getattr(os, "O_NOFOLLOW", 0)
    descriptor = -1
    published = False
    temporary_info: os.stat_result | None = None
    try:
        descriptor = os.open(temporary, flags, 0o600, dir_fd=root_fd)
        before = os.fstat(descriptor)
        temporary_info = before
        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
            raise IntegrityError("download temporary object is not a private regular file")
        _download_to_descriptor(
            asset,
            descriptor,
            opener if opener is not None else _default_opener(asset),
            priority_check,
        )
        os.fsync(descriptor)
        after = os.fstat(descriptor)
        if not _same_inode(before, after) or after.st_size != asset.size_bytes:
            raise IntegrityError("download temporary object changed before publication")
        os.close(descriptor)
        descriptor = -1
        priority_check()
        try:
            os.link(
                temporary,
                asset.filename,
                src_dir_fd=root_fd,
                dst_dir_fd=root_fd,
                follow_symlinks=False,
            )
            published = True
        except FileExistsError:
            verify_cached_asset(asset, root, priority_check=priority_check)
        if published:
            _fsync_directory(root_fd)
        _unlink_exact_temporary(root_fd, temporary, before)
        temporary_info = None
        _fsync_directory(root_fd)
        if not _same_inode(root_info, os.lstat(root)):
            raise IntegrityError(f"cache root pathname changed during publication: {root}")
        return verify_cached_asset(asset, root, priority_check=priority_check)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        try:
            if temporary_info is not None:
                _unlink_exact_temporary(root_fd, temporary, temporary_info)
        finally:
            os.close(root_fd)


def _asset_summary(asset: Asset) -> dict[str, object]:
    return {
        "id": asset.asset_id,
        "kind": asset.kind,
        "status": asset.status,
        "default_eligible": asset.default_eligible,
        "revision": asset.revision,
        "filename": asset.filename,
        "sha256": asset.sha256,
        "size_bytes": asset.size_bytes,
        "license_spdx": asset.license_spdx,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lock", type=Path, default=DEFAULT_LOCK)
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("validate-lock", help="validate lock shape and policy")
    subparsers.add_parser("list", help="list immutable assets")
    for name in ("verify", "fetch"):
        child = subparsers.add_parser(name, help=f"{name} one immutable asset")
        child.add_argument("--asset", required=True)
        child.add_argument("--cache-root", type=Path, required=True)
        if name == "fetch":
            child.add_argument(
                "--accept-license",
                help="required SPDX identifier for explicit model-license acceptance",
            )
    return parser


def main(argv: list[str] | None = None) -> int:
    arguments = build_parser().parse_args(argv)
    try:
        lock = load_lock(arguments.lock)
        if arguments.command == "validate-lock":
            result: object = {
                "ok": True,
                "lock": str(lock.path),
                "lock_sha256": lock.digest,
                "asset_count": len(lock.assets),
            }
        elif arguments.command == "list":
            result = [_asset_summary(asset) for asset in lock.assets]
        else:
            asset = lock.asset(arguments.asset)
            if arguments.command == "fetch":
                if asset.kind.endswith("model") and arguments.accept_license != asset.license_spdx:
                    raise AssetError(
                        f"fetching {asset.asset_id} requires --accept-license {asset.license_spdx}"
                    )
                path = fetch_asset(asset, arguments.cache_root)
            else:
                assert_priority_available()
                assert_resource_guard()
                path = verify_cached_asset(asset, arguments.cache_root)
            result = {**_asset_summary(asset), "path": str(path), "verified": True}
        print(json.dumps(result, indent=2, sort_keys=True))
        return 0
    except PriorityBlocked as exc:
        print(f"model asset operation deferred: {exc}", file=sys.stderr)
        return 75
    except AssetError as exc:
        print(f"model asset operation failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
