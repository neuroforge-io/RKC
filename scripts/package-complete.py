#!/usr/bin/env python3
"""Build a deterministic, fail-closed RKC release ZIP."""
from __future__ import annotations

import argparse
import fnmatch
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import zipfile
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath

try:
    from scripts.git_source_guard import SourceGuardError, require_clean_worktree
except ModuleNotFoundError as exc:
    if exc.name not in {"scripts", "scripts.git_source_guard"}:
        raise
    from git_source_guard import SourceGuardError, require_clean_worktree

ROOT = Path(__file__).resolve().parents[1]
DIST = ROOT / "dist"
TOP = "repository-knowledge-compiler-complete"

REQUIRED_INPUTS = (
    Path("binaries"),
    Path("demo"),
    Path("evidence"),
)
RELEASE_STEPS = (
    "go-modules",
    "python-environment",
    "format",
    "vet",
    "coverage",
    "contracts",
    "docs",
    "licenses",
    "model-lock",
    "build",
    "plugins",
    "smoke",
    "reproducibility",
    "api-smoke",
    "mcp-smoke",
    "git-smoke",
    "race",
    "benchmark",
)
RELEASE_SUMMARY_KEYS = frozenset(
    {"schema_version", "ok", "source", "elapsed_seconds", "steps"}
)
RELEASE_SOURCE_KEYS = frozenset(
    {"commit", "tree", "commit_time_unix"}
)
RELEASE_STEP_KEYS = frozenset(
    {"name", "status", "duration_seconds", "log_sha256"}
)
RESERVED_OUTPUT_ROOTS = frozenset(path.parts[0] for path in REQUIRED_INPUTS)
TOP_LEVEL_LICENSE_FILES = ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md")
GO_MODULE_LOCK = PurePosixPath("third_party/go-modules.lock.json")
GO_LOCK_KEYS = frozenset({"schema_version", "go", "root_requirements", "modules"})
GO_LOCK_METADATA_KEYS = frozenset({"directive", "toolchain"})
GO_ROOT_REQUIREMENT_KEYS = frozenset({"path", "version"})
GO_MODULE_KEYS = frozenset(
    {
        "path",
        "version",
        "direct",
        "module_sum",
        "go_mod_sum",
        "source_url",
        "license_spdx",
        "licenses",
        "notice_path",
    }
)
GO_LICENSE_KEYS = frozenset({"source_path", "path", "sha256"})
EXPECTED_BINARIES = frozenset(
    {
        PurePosixPath("linux-amd64/rkc"),
        PurePosixPath("linux-amd64/rkc-mcp"),
        PurePosixPath("linux-arm64/rkc"),
        PurePosixPath("linux-arm64/rkc-mcp"),
    }
)
EXPECTED_BINARY_SBOMS = frozenset(
    PurePosixPath(f"linux-{architecture}/{binary}.spdx.json")
    for architecture in ("amd64", "arm64")
    for binary in ("rkc", "rkc-mcp")
)
DISTRIBUTION_SBOM_EXCLUSIONS = (
    "./SBOM.spdx.json",
    "./MANIFEST.json",
    "./SHA256SUMS.txt",
)
PROHIBITED_SUFFIXES = frozenset(
    {
        ".a",
        ".bin",
        ".ckpt",
        ".class",
        ".dll",
        ".dylib",
        ".exe",
        ".ggml",
        ".gguf",
        ".h5",
        ".hdf5",
        ".jar",
        ".lib",
        ".model",
        ".o",
        ".obj",
        ".onnx",
        ".pt",
        ".pth",
        ".pyc",
        ".pyo",
        ".safetensors",
        ".so",
        ".tflite",
        ".wasm",
        ".weights",
        ".whl",
    }
)
PROHIBITED_NAMES = (
    "consolidated*.pth",
    "model*.bin",
    "pytorch_model*.bin",
)
NATIVE_MAGIC = (
    b"\x7fELF",
    b"MZ",
    b"\x00asm",
    b"!<arch>\n",
    b"\xfe\xed\xfa\xce",
    b"\xce\xfa\xed\xfe",
    b"\xfe\xed\xfa\xcf",
    b"\xcf\xfa\xed\xfe",
    b"\xca\xfe\xba\xbe",
    b"\xbe\xba\xfe\xca",
)
MODEL_MAGIC = (b"GGUF",)
MAX_ARTIFACT_FILE_BYTES = 512 * 1024 * 1024
MAX_ARTIFACT_TOTAL_BYTES = 2 * 1024 * 1024 * 1024


class PackageError(RuntimeError):
    """Raised when a release input violates a packaging invariant."""


@dataclass(frozen=True)
class TrackedFile:
    """One regular blob recorded in the immutable source commit."""

    path: PurePosixPath
    executable: bool
    object_id: str


@dataclass(frozen=True)
class SourceIdentity:
    """Exact immutable Git identity used by source, binaries, and evidence."""

    commit: str
    tree: str
    commit_time_unix: str


def sha256(path: Path) -> str:
    """Return the SHA-256 digest of a regular file."""
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def sha1(path: Path) -> str:
    """Return the SHA-1 digest required by SPDX package verification codes."""
    digest = hashlib.sha1(usedforsecurity=False)
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def safe_relative_path(value: str, label: str) -> PurePosixPath:
    """Validate a portable, canonical relative archive path."""
    if not value or "\\" in value or any(ord(char) < 32 for char in value):
        raise PackageError(f"{label} is not a portable archive path: {value!r}")
    path = PurePosixPath(value)
    if path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts):
        raise PackageError(f"{label} is not a canonical relative path: {value!r}")
    if path.as_posix() != value:
        raise PackageError(f"{label} is not canonical: {value!r}")
    return path


def git_text(arguments: list[str], label: str) -> str:
    """Run a bounded Git identity query and return one canonical line."""
    result = subprocess.run(
        ["git", *arguments],
        cwd=ROOT,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=30,
    )
    if result.returncode != 0:
        detail = result.stderr.strip() or "Git query failed"
        raise PackageError(f"cannot resolve {label}: {detail}")
    value = result.stdout.strip()
    if not value or "\n" in value:
        raise PackageError(f"{label} is not one canonical line")
    return value


def source_identity() -> SourceIdentity:
    """Resolve HEAD once as an exact commit, tree, and commit timestamp."""
    commit = git_text(["rev-parse", "--verify", "HEAD^{commit}"], "source commit")
    tree = git_text(["rev-parse", "--verify", f"{commit}^{{tree}}"], "source tree")
    commit_time = git_text(
        ["show", "-s", "--format=%ct", commit], "source commit timestamp"
    )
    for value, label in ((commit, "commit"), (tree, "tree")):
        if len(value) not in {40, 64} or any(char not in "0123456789abcdef" for char in value):
            raise PackageError(f"source {label} is not a canonical Git object ID")
    if not commit_time.isdecimal():
        raise PackageError("source commit timestamp is not a non-negative integer")
    return SourceIdentity(commit=commit, tree=tree, commit_time_unix=commit_time)


def tracked_files(commit: str) -> list[TrackedFile]:
    """Read regular blobs from one immutable commit without filename quoting."""
    result = subprocess.run(
        ["git", "ls-tree", "-r", "-z", "--full-tree", commit],
        cwd=ROOT,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", errors="replace").strip()
        raise PackageError(f"cannot enumerate tracked source files: {detail}")

    files: list[TrackedFile] = []
    seen: set[PurePosixPath] = set()
    for record in result.stdout.split(b"\0"):
        if not record:
            continue
        try:
            header, raw_path = record.split(b"\t", 1)
            mode, object_type, raw_object_id = header.split(b" ", 2)
            value = raw_path.decode("utf-8")
        except (UnicodeDecodeError, ValueError) as exc:
            raise PackageError("Git index contains an unportable entry") from exc
        path = safe_relative_path(value, "tracked source path")
        if mode == b"120000":
            raise PackageError(f"tracked source symlinks are prohibited: {path}")
        if mode == b"160000":
            raise PackageError(f"Git submodules require explicit release review: {path}")
        if mode not in {b"100644", b"100755"} or object_type != b"blob":
            raise PackageError(f"unsupported tracked source mode {mode!r}: {path}")
        try:
            object_id = raw_object_id.decode("ascii")
        except UnicodeDecodeError as exc:
            raise PackageError(f"invalid Git object ID for tracked source: {path}") from exc
        if len(object_id) not in {40, 64} or any(
            char not in "0123456789abcdef" for char in object_id
        ):
            raise PackageError(f"invalid Git object ID for tracked source: {path}")
        if path in seen:
            raise PackageError(f"duplicate tracked source path: {path}")
        if path.parts[0] in {"bin", "dist"} or path.parts[0].startswith(".rkc"):
            raise PackageError(f"generated output is tracked as source: {path}")
        seen.add(path)
        files.append(
            TrackedFile(
                path=path,
                executable=mode == b"100755",
                object_id=object_id,
            )
        )
    if not files:
        raise PackageError("Git index contains no source files")
    return sorted(files, key=lambda item: item.path.as_posix())


def require_clean_tracked_source() -> None:
    """Reject packages whose tracked source differs from the recorded commit."""
    for arguments, label in (
        (["git", "diff", "--quiet", "--ignore-submodules", "--"], "working tree"),
        (["git", "diff", "--cached", "--quiet", "--ignore-submodules", "--"], "index"),
    ):
        result = subprocess.run(arguments, cwd=ROOT, check=False)
        if result.returncode == 1:
            raise PackageError(f"tracked source {label} is dirty; commit it before packaging")
        if result.returncode != 0:
            raise PackageError(f"cannot verify clean tracked source {label}")


def prohibited_name(path: PurePosixPath) -> bool:
    """Return whether a path names an unreviewed model or compiled artifact."""
    name = path.name.lower()
    return path.suffix.lower() in PROHIBITED_SUFFIXES or any(
        fnmatch.fnmatchcase(name, pattern) for pattern in PROHIBITED_NAMES
    )


def assert_no_symlink_components(root: Path, relative: PurePosixPath) -> None:
    """Reject a source path whose parent traversal crosses a symlink."""
    cursor = root
    for part in relative.parts[:-1]:
        cursor = cursor / part
        try:
            info = os.lstat(cursor)
        except FileNotFoundError as exc:
            raise PackageError(f"source parent disappeared: {cursor}") from exc
        if stat.S_ISLNK(info.st_mode):
            raise PackageError(f"source parent symlink is prohibited: {cursor}")
        if not stat.S_ISDIR(info.st_mode):
            raise PackageError(f"source parent is not a directory: {cursor}")


def copy_regular_file(
    source: Path,
    target: Path,
    *,
    mode: int,
    label: str,
    reject_native: bool = True,
) -> int:
    """Copy one stable, non-symlink regular file into private staging."""
    try:
        initial = os.lstat(source)
    except FileNotFoundError as exc:
        raise PackageError(f"{label} is missing: {source}") from exc
    if stat.S_ISLNK(initial.st_mode):
        raise PackageError(f"{label} symlink is prohibited: {source}")
    if not stat.S_ISREG(initial.st_mode):
        raise PackageError(f"{label} is not a regular file: {source}")
    if initial.st_size > MAX_ARTIFACT_FILE_BYTES:
        raise PackageError(f"{label} exceeds the per-file package limit: {source}")

    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(source, flags)
    except OSError as exc:
        raise PackageError(f"cannot open {label} without following links: {source}") from exc

    target.parent.mkdir(parents=True, exist_ok=True)
    try:
        with os.fdopen(descriptor, "rb") as source_handle:
            before = os.fstat(source_handle.fileno())
            if not stat.S_ISREG(before.st_mode):
                raise PackageError(f"{label} changed file type: {source}")
            header = source_handle.read(16)
            if any(header.startswith(magic) for magic in MODEL_MAGIC):
                raise PackageError(f"model artifact is prohibited: {source}")
            if reject_native and any(header.startswith(magic) for magic in NATIVE_MAGIC):
                raise PackageError(f"compiled/native artifact is prohibited: {source}")
            source_handle.seek(0)
            try:
                with target.open("xb") as target_handle:
                    shutil.copyfileobj(source_handle, target_handle, 1024 * 1024)
            except FileExistsError as exc:
                raise PackageError(f"duplicate staged package path: {target}") from exc
            after = os.fstat(source_handle.fileno())
    except Exception:
        target.unlink(missing_ok=True)
        raise

    identity_before = (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
    identity_after = (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)
    if identity_before != identity_after:
        target.unlink(missing_ok=True)
        raise PackageError(f"{label} changed while it was copied: {source}")
    os.chmod(target, mode)
    return before.st_size


def copy_commit_blob(item: TrackedFile, target: Path) -> int:
    """Materialize and verify one immutable Git blob in private staging."""
    raw_size = git_text(["cat-file", "-s", item.object_id], "Git blob size")
    if not raw_size.isdecimal():
        raise PackageError(f"Git blob size is invalid: {item.path}")
    size = int(raw_size)
    if size > MAX_ARTIFACT_FILE_BYTES:
        raise PackageError(f"tracked source exceeds the per-file package limit: {item.path}")
    target.parent.mkdir(parents=True, exist_ok=True)
    try:
        with target.open("xb") as output:
            result = subprocess.run(
                ["git", "cat-file", "blob", item.object_id],
                cwd=ROOT,
                check=False,
                stdout=output,
                stderr=subprocess.PIPE,
                timeout=60,
            )
    except (OSError, subprocess.TimeoutExpired) as exc:
        target.unlink(missing_ok=True)
        raise PackageError(f"cannot materialize tracked source blob: {item.path}") from exc
    if result.returncode != 0 or target.stat().st_size != size:
        target.unlink(missing_ok=True)
        detail = result.stderr.decode("utf-8", errors="replace").strip()
        raise PackageError(f"Git blob materialization failed for {item.path}: {detail}")
    observed_object = git_text(
        ["hash-object", str(target)], f"materialized Git blob {item.path}"
    )
    if observed_object != item.object_id:
        target.unlink(missing_ok=True)
        raise PackageError(f"materialized Git blob identity differs: {item.path}")
    with target.open("rb") as handle:
        header = handle.read(16)
    if any(header.startswith(magic) for magic in MODEL_MAGIC):
        target.unlink(missing_ok=True)
        raise PackageError(f"model artifact is prohibited: {item.path}")
    if any(header.startswith(magic) for magic in NATIVE_MAGIC):
        target.unlink(missing_ok=True)
        raise PackageError(f"compiled/native artifact is prohibited: {item.path}")
    os.chmod(target, 0o755 if item.executable else 0o644)
    return size


def copy_tracked_source(target: Path, identity: SourceIdentity) -> None:
    """Copy source exclusively from immutable blobs in one Git commit."""
    for item in tracked_files(identity.commit):
        if prohibited_name(item.path):
            raise PackageError(f"prohibited tracked source artifact: {item.path}")
        destination = target.joinpath(*item.path.parts)
        copy_commit_blob(item, destination)

    for name in TOP_LEVEL_LICENSE_FILES:
        if not (target / name).is_file():
            raise PackageError(f"required license file is not tracked: {name}")
    licenses = target / "LICENSES"
    if not licenses.is_dir() or not any(licenses.iterdir()):
        raise PackageError("the tracked LICENSES directory is missing or empty")
    module_lock = target.joinpath(*GO_MODULE_LOCK.parts)
    if not module_lock.is_file():
        raise PackageError(f"required Go module audit lock is not tracked: {GO_MODULE_LOCK}")
    validate_go_module_lock(target)


def tree_files(root: Path, label: str) -> list[tuple[PurePosixPath, Path]]:
    """Enumerate a tree without accepting links or special files."""
    try:
        root_info = os.lstat(root)
    except FileNotFoundError as exc:
        raise PackageError(f"required {label} directory is missing: {root}") from exc
    if stat.S_ISLNK(root_info.st_mode) or not stat.S_ISDIR(root_info.st_mode):
        raise PackageError(f"required {label} path is not a real directory: {root}")

    output: list[tuple[PurePosixPath, Path]] = []
    for current, directories, files in os.walk(root, topdown=True, followlinks=False):
        directories.sort()
        files.sort()
        current_path = Path(current)
        for directory in directories:
            path = current_path / directory
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
                raise PackageError(f"{label} directory link/special path is prohibited: {path}")
        for filename in files:
            path = current_path / filename
            relative_value = path.relative_to(root).as_posix()
            relative = safe_relative_path(relative_value, f"{label} path")
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                raise PackageError(f"{label} link/special file is prohibited: {path}")
            output.append((relative, path))
    return output


def audited_license_paths(root: Path | None = None) -> tuple[PurePosixPath, ...]:
    """Return the complete reviewed license-file inventory."""
    root = ROOT if root is None else root
    files = tuple(
        PurePosixPath("LICENSES") / relative
        for relative, _path in tree_files(root / "LICENSES", "audited licenses")
    )
    if not files:
        raise PackageError("the audited LICENSES directory is empty")
    return files


def binary_notice_files(root: Path | None = None) -> frozenset[PurePosixPath]:
    """Return every license/notice file required beside each binary set."""
    root = ROOT if root is None else root
    canonical = (
        tuple(PurePosixPath(name) for name in TOP_LEVEL_LICENSE_FILES)
        + audited_license_paths(root)
        + (GO_MODULE_LOCK,)
    )
    return frozenset(
        PurePosixPath(prefix) / relative if prefix else relative
        for prefix in ("", "linux-amd64", "linux-arm64")
        for relative in canonical
    )


def expected_binary_bundle(root: Path | None = None) -> frozenset[PurePosixPath]:
    """Return the exact binary plus dynamically audited notice inventory."""
    root = ROOT if root is None else root
    return EXPECTED_BINARIES | EXPECTED_BINARY_SBOMS | binary_notice_files(root)


def strict_json_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    """Construct a JSON object while rejecting duplicate member names."""
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise PackageError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def load_strict_json(path: Path, label: str, maximum: int = 16 * 1024 * 1024) -> object:
    """Load one bounded regular UTF-8 JSON document without duplicate keys."""
    try:
        info = os.lstat(path)
    except FileNotFoundError as exc:
        raise PackageError(f"required {label} is missing: {path}") from exc
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise PackageError(f"{label} must be a regular file: {path}")
    if info.st_size > maximum:
        raise PackageError(f"{label} exceeds {maximum} bytes: {path}")
    try:
        return json.loads(
            path.read_text(encoding="utf-8"), object_pairs_hook=strict_json_object
        )
    except PackageError:
        raise
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PackageError(f"{label} is not strict UTF-8 JSON: {path}") from exc


def validate_release_evidence(
    source: Path, benchmark_source: Path, identity: SourceIdentity
) -> dict[str, object]:
    """Validate exact successful validation/benchmark evidence and receipt it."""
    expected = {
        PurePosixPath("summary.json"),
        PurePosixPath("steps.tsv"),
        *(PurePosixPath("logs") / f"{name}.log" for name in RELEASE_STEPS),
    }
    files = tree_files(source, "release validation evidence")
    total_bytes = 0
    for _relative, path in files:
        size = os.lstat(path).st_size
        if size > MAX_ARTIFACT_FILE_BYTES:
            raise PackageError(f"release validation evidence file is too large: {path}")
        total_bytes += size
    if total_bytes > MAX_ARTIFACT_TOTAL_BYTES:
        raise PackageError("release validation evidence exceeds the total size limit")
    directories: set[PurePosixPath] = set()
    for current, names, _files in os.walk(source, topdown=True, followlinks=False):
        current_path = Path(current)
        for name in names:
            directories.add(
                PurePosixPath((current_path / name).relative_to(source).as_posix())
            )
    if directories != {PurePosixPath("logs")}:
        raise PackageError(
            "release validation directory inventory differs; "
            f"observed={sorted(str(path) for path in directories)}"
        )
    actual = {relative for relative, _path in files}
    if actual != expected:
        missing = sorted(str(path) for path in expected - actual)
        unexpected = sorted(str(path) for path in actual - expected)
        raise PackageError(
            "release validation evidence inventory differs; "
            f"missing={missing}, unexpected={unexpected}"
        )

    summary = load_strict_json(source / "summary.json", "release validation summary")
    if not isinstance(summary, dict) or set(summary) != RELEASE_SUMMARY_KEYS:
        raise PackageError("release validation summary keys drifted")
    if summary.get("schema_version") != "2.0" or summary.get("ok") is not True:
        raise PackageError("release validation summary is not a successful schema 2.0 run")
    elapsed = summary.get("elapsed_seconds")
    if type(elapsed) is not int or elapsed < 0:
        raise PackageError("release validation elapsed_seconds is invalid")
    source_record = summary.get("source")
    expected_source = {
        "commit": identity.commit,
        "tree": identity.tree,
        "commit_time_unix": identity.commit_time_unix,
    }
    if (
        not isinstance(source_record, dict)
        or set(source_record) != RELEASE_SOURCE_KEYS
        or source_record != expected_source
    ):
        raise PackageError("release validation source identity differs from package source")

    steps = summary.get("steps")
    if not isinstance(steps, list) or len(steps) != len(RELEASE_STEPS):
        raise PackageError("release validation step inventory is incomplete")
    stable_steps: list[dict[str, str]] = []
    tsv_rows: list[str] = []
    for expected_name, step in zip(RELEASE_STEPS, steps, strict=True):
        if not isinstance(step, dict) or set(step) != RELEASE_STEP_KEYS:
            raise PackageError(f"release validation step record drifted: {expected_name}")
        duration = step.get("duration_seconds")
        digest = step.get("log_sha256")
        if (
            step.get("name") != expected_name
            or step.get("status") != "passed"
            or type(duration) is not int
            or duration < 0
            or not isinstance(digest, str)
            or len(digest) != 64
            or any(char not in "0123456789abcdef" for char in digest)
        ):
            raise PackageError(f"release validation step failed policy: {expected_name}")
        log = source / "logs" / f"{expected_name}.log"
        if sha256(log) != digest:
            raise PackageError(f"release validation log digest differs: {expected_name}")
        tsv_rows.append(f"{expected_name}\tpassed\t{duration}")
        stable_steps.append({"name": expected_name, "status": "passed"})
    try:
        observed_tsv = (source / "steps.tsv").read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as exc:
        raise PackageError("release validation steps.tsv is not UTF-8") from exc
    if observed_tsv != "\n".join(tsv_rows) + "\n":
        raise PackageError("release validation steps.tsv differs from summary")

    expected_benchmark = {
        PurePosixPath("result.json"),
        PurePosixPath("time.txt"),
        PurePosixPath("scan.stdout"),
    }
    benchmark_files = tree_files(benchmark_source, "release benchmark evidence")
    actual_benchmark = {relative for relative, _path in benchmark_files}
    if actual_benchmark != expected_benchmark:
        missing = sorted(str(path) for path in expected_benchmark - actual_benchmark)
        unexpected = sorted(str(path) for path in actual_benchmark - expected_benchmark)
        raise PackageError(
            "release benchmark evidence inventory differs; "
            f"missing={missing}, unexpected={unexpected}"
        )
    benchmark_bytes = sum(os.lstat(path).st_size for _relative, path in benchmark_files)
    if any(
        os.lstat(path).st_size > MAX_ARTIFACT_FILE_BYTES
        for _relative, path in benchmark_files
    ):
        raise PackageError("release benchmark evidence contains an oversized file")
    if benchmark_bytes > MAX_ARTIFACT_TOTAL_BYTES:
        raise PackageError("release benchmark evidence exceeds the total size limit")
    benchmark_result = load_strict_json(
        benchmark_source / "result.json", "release benchmark result"
    )
    if (
        not isinstance(benchmark_result, dict)
        or benchmark_result.get("schema_version") != "1.0"
    ):
        raise PackageError("release benchmark result is not schema 1.0 evidence")

    evidence_paths = [
        ("validation", source, PurePosixPath("summary.json")),
        ("validation", source, PurePosixPath("steps.tsv")),
        *(
            ("validation", source, PurePosixPath("logs") / f"{name}.log")
            for name in RELEASE_STEPS
        ),
        *(
            ("benchmark", benchmark_source, relative)
            for relative in (
                PurePosixPath("result.json"),
                PurePosixPath("time.txt"),
                PurePosixPath("scan.stdout"),
            )
        ),
    ]
    evidence_files: list[dict[str, object]] = []
    for namespace, evidence_root, relative in evidence_paths:
        path = evidence_root.joinpath(*relative.parts)
        evidence_files.append(
            {
                "path": f"{namespace}/{relative.as_posix()}",
                "size_bytes": os.lstat(path).st_size,
                "sha256": sha256(path),
            }
        )
    evidence_manifest_payload = json.dumps(
        evidence_files, separators=(",", ":"), sort_keys=True
    ).encode("utf-8")
    return {
        "schema_version": "1.0",
        "status": "passed",
        "source": expected_source,
        "steps": stable_steps,
        "raw_evidence_packaged": False,
        "raw_evidence_root": "evidence",
        "raw_evidence_files": evidence_files,
        "raw_evidence_manifest_sha256": hashlib.sha256(
            evidence_manifest_payload
        ).hexdigest(),
        "raw_evidence_policy": (
            "Validation logs, durations, and raw benchmark outputs are retained in "
            "the same atomic release generation but excluded from the release ZIP; "
            "this receipt retains every exact path, size, SHA-256, and the canonical "
            "evidence-manifest digest for external verification."
        ),
    }


def write_json_file(path: Path, document: object) -> None:
    """Write canonical pretty JSON to a new package path."""
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        with path.open("x", encoding="utf-8") as handle:
            json.dump(document, handle, indent=2, sort_keys=True)
            handle.write("\n")
    except FileExistsError as exc:
        raise PackageError(f"duplicate staged package path: {path}") from exc


def validate_go_module_lock(source: Path) -> None:
    """Require a bounded module lock whose license paths and hashes close."""
    path = source.joinpath(*GO_MODULE_LOCK.parts)
    try:
        info = os.lstat(path)
    except FileNotFoundError as exc:
        raise PackageError(f"required Go module audit lock is missing: {path}") from exc
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise PackageError(f"Go module audit lock must be a regular file: {path}")
    if info.st_size > 1024 * 1024:
        raise PackageError(f"Go module audit lock exceeds 1048576 bytes: {path}")
    try:
        document = json.loads(
            path.read_text(encoding="utf-8"), object_pairs_hook=strict_json_object
        )
    except PackageError:
        raise
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PackageError(f"Go module audit lock is not valid UTF-8 JSON: {path}") from exc
    if (
        not isinstance(document, dict)
        or set(document) != GO_LOCK_KEYS
        or document.get("schema_version") != "1.0"
    ):
        raise PackageError("Go module audit lock must use object schema_version 1.0")
    go = document.get("go")
    if not isinstance(go, dict) or set(go) != GO_LOCK_METADATA_KEYS or not all(
        isinstance(go.get(field), str) and go[field]
        for field in ("directive", "toolchain")
    ):
        raise PackageError("Go module audit lock has invalid Go directive/toolchain identity")
    requirements = document.get("root_requirements")
    if not isinstance(requirements, list) or not requirements:
        raise PackageError("Go module audit lock has no root requirements")
    for requirement in requirements:
        if (
            not isinstance(requirement, dict)
            or set(requirement) != GO_ROOT_REQUIREMENT_KEYS
            or not all(
                isinstance(requirement.get(field), str) and requirement[field]
                for field in ("path", "version")
            )
        ):
            raise PackageError("Go module audit lock has an invalid root requirement")

    modules = document.get("modules")
    if not isinstance(modules, list) or not modules:
        raise PackageError("Go module audit lock has no resolved modules")
    identities: set[tuple[str, str]] = set()
    direct_identities: set[tuple[str, str]] = set()
    locked_licenses: dict[PurePosixPath, str] = {}
    for module in modules:
        if not isinstance(module, dict) or set(module) != GO_MODULE_KEYS:
            raise PackageError("Go module audit lock has a non-object module")
        if not all(
            isinstance(module.get(field), str) and module[field]
            for field in (
                "path",
                "version",
                "module_sum",
                "go_mod_sum",
                "source_url",
                "license_spdx",
            )
        ) or not isinstance(module.get("direct"), bool):
            raise PackageError("Go module audit lock has an invalid module record")
        identity = (module["path"], module["version"])
        if identity in identities:
            raise PackageError(
                f"duplicate module in Go audit lock: {identity[0]} {identity[1]}"
            )
        identities.add(identity)
        if module["direct"]:
            direct_identities.add(identity)
        notice_path = module.get("notice_path")
        if not isinstance(notice_path, str) or not notice_path:
            raise PackageError(f"invalid notice path for Go module {identity[0]}")
        notice = safe_relative_path(
            notice_path, f"notice path for Go module {identity[0]}"
        )
        notice_file = source.joinpath(*notice.parts)
        if not notice_file.is_file() or notice_file.is_symlink():
            raise PackageError(f"locked Go module notice is missing or unsafe: {notice}")
        licenses = module.get("licenses")
        if not isinstance(licenses, list) or not licenses:
            raise PackageError(f"Go module has no audited license: {identity[0]}")
        for license_record in licenses:
            if (
                not isinstance(license_record, dict)
                or set(license_record) != GO_LICENSE_KEYS
            ):
                raise PackageError(f"invalid license record for Go module {identity[0]}")
            license_path_value = license_record.get("path")
            source_path = license_record.get("source_path")
            digest = license_record.get("sha256")
            if not isinstance(license_path_value, str) or not isinstance(source_path, str):
                raise PackageError(f"invalid license path for Go module {identity[0]}")
            license_path = safe_relative_path(
                license_path_value, f"license path for Go module {identity[0]}"
            )
            if license_path.parts[0] != "LICENSES" or not source_path:
                raise PackageError(f"unscoped license path for Go module {identity[0]}")
            if not isinstance(digest, str) or len(digest) != 64 or any(
                character not in "0123456789abcdef" for character in digest
            ):
                raise PackageError(f"invalid license digest for Go module {identity[0]}")
            previous = locked_licenses.setdefault(license_path, digest)
            if previous != digest:
                raise PackageError(
                    f"conflicting license digest in Go module lock: {license_path}"
                )

    root_identities = {(item["path"], item["version"]) for item in requirements}
    if root_identities != direct_identities:
        raise PackageError("Go module audit lock direct modules differ from root requirements")

    for relative, digest in locked_licenses.items():
        path = source.joinpath(*relative.parts)
        if not path.is_file() or path.is_symlink():
            raise PackageError(f"locked Go module license is missing or unsafe: {relative}")
        if sha256(path) != digest:
            raise PackageError(f"locked Go module license digest differs: {relative}")
    actual_module_licenses = {
        relative
        for relative in audited_license_paths(source)
        if relative.parts[:2] == ("LICENSES", "go-modules")
    }
    if actual_module_licenses != set(locked_licenses):
        missing = sorted(
            str(path) for path in set(locked_licenses) - actual_module_licenses
        )
        unlocked = sorted(
            str(path) for path in actual_module_licenses - set(locked_licenses)
        )
        raise PackageError(
            f"Go module audit license inventory differs; missing={missing}, unlocked={unlocked}"
        )


def validate_binary_sbom(
    path: Path,
    binary: Path,
    module_lock: Path,
    source_root: Path,
    identity: SourceIdentity,
    goos: str,
    goarch: str,
) -> None:
    """Cryptographically bind one SPDX document to its paired Go binary."""
    try:
        result = subprocess.run(
            [
                sys.executable,
                str(ROOT / "scripts" / "generate-go-sbom.py"),
                "--binary",
                str(binary),
                "--verify-document",
                str(path),
                "--lock",
                str(module_lock),
                "--source-root",
                str(source_root),
                "--source-commit",
                identity.commit,
                "--source-tree",
                identity.tree,
                "--source-date-epoch",
                identity.commit_time_unix,
                "--goos",
                goos,
                "--goarch",
                goarch,
                "--version",
                (source_root / "VERSION").read_text(encoding="utf-8").strip(),
            ],
            cwd=ROOT,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise PackageError(f"cannot verify binary SPDX document {path}: {exc}") from exc
    if result.returncode != 0:
        detail = result.stderr.strip() or result.stdout.strip() or "verification failed"
        raise PackageError(f"binary SPDX document does not bind {binary}: {detail}")


def copy_release_binaries(
    source: Path,
    target: Path,
    module_lock: Path,
    source_root: Path,
    identity: SourceIdentity,
) -> None:
    """Copy the four supported executables and their exact license bundles."""
    files = tree_files(source, "release binaries")
    actual = frozenset(relative for relative, _path in files)
    expected = expected_binary_bundle(source_root)
    if actual != expected:
        missing = sorted(str(path) for path in expected - actual)
        unexpected = sorted(str(path) for path in actual - expected)
        raise PackageError(
            f"release binary set differs from policy; missing={missing}, unexpected={unexpected}"
        )
    for relative, path in files:
        if relative in EXPECTED_BINARIES:
            with path.open("rb") as handle:
                if handle.read(4) != b"\x7fELF":
                    raise PackageError(f"release binary is not an ELF executable: {path}")
            copy_regular_file(
                path,
                target.joinpath(*relative.parts),
                mode=0o755,
                label="release binary bundle",
                reject_native=False,
            )

    for relative, path in files:
        if relative in EXPECTED_BINARIES:
            continue
        if relative in EXPECTED_BINARY_SBOMS:
            binary_name = relative.name[: -len(".spdx.json")]
            paired_binary = target / relative.parent / binary_name
            staged_sbom = target.joinpath(*relative.parts)
            platform = relative.parts[0]
            try:
                goos, goarch = platform.split("-", 1)
            except ValueError as exc:
                raise PackageError(f"invalid release binary platform: {platform}") from exc
            copy_regular_file(
                path,
                staged_sbom,
                mode=0o644,
                label="release binary bundle",
            )
            validate_binary_sbom(
                staged_sbom,
                paired_binary,
                module_lock,
                source_root,
                identity,
                goos,
                goarch,
            )
            continue
        notice_parts = relative.parts
        if notice_parts[0] in {"linux-amd64", "linux-arm64"}:
            notice_parts = notice_parts[1:]
        canonical_notice = source_root.joinpath(*notice_parts)
        if sha256(path) != sha256(canonical_notice):
            raise PackageError(f"release binary notice differs from tracked source: {path}")
        copy_regular_file(
            path,
            target.joinpath(*relative.parts),
            mode=0o644,
            label="release binary bundle",
        )


def copy_data_tree(source: Path, target: Path, label: str) -> None:
    """Copy a bounded generated-data tree, rejecting models and native code."""
    total = 0
    for relative, path in tree_files(source, label):
        if prohibited_name(relative):
            raise PackageError(f"prohibited {label} artifact: {relative}")
        total += copy_regular_file(
            path,
            target.joinpath(*relative.parts),
            mode=0o644,
            label=label,
        )
        if total > MAX_ARTIFACT_TOTAL_BYTES:
            raise PackageError(f"{label} exceeds the total package limit")


def copy_selected_files(source: Path, target: Path, names: tuple[str, ...], label: str) -> None:
    """Copy an explicit optional-file allowlist from a generated directory."""
    for name in names:
        path = source / name
        if not path.exists():
            continue
        relative = safe_relative_path(name, f"{label} path")
        if prohibited_name(relative):
            raise PackageError(f"prohibited {label} artifact: {relative}")
        copy_regular_file(
            path,
            target.joinpath(*relative.parts),
            mode=0o644,
            label=label,
        )


def copy_required_files(source: Path, target: Path, names: tuple[str, ...], label: str) -> None:
    """Copy an exact required-file allowlist and reject missing inputs."""
    for name in names:
        if not (source / name).exists():
            raise PackageError(f"required {label} is missing: {source / name}")
    copy_selected_files(source, target, names, label)


def validate_demo_outputs(source: Path, identity: SourceIdentity) -> None:
    """Bind the deterministic packaged demo files to the immutable source commit."""
    bundle = load_strict_json(source / "bundle.json", "demo bundle", 512 * 1024 * 1024)
    coverage = load_strict_json(source / "coverage.json", "demo coverage")
    if not isinstance(bundle, dict) or not isinstance(coverage, dict):
        raise PackageError("demo bundle and coverage must be JSON objects")
    snapshot = bundle.get("snapshot")
    if not isinstance(snapshot, dict):
        raise PackageError("demo bundle snapshot is missing")
    git = snapshot.get("git")
    if (
        not isinstance(git, dict)
        or git.get("commit") != identity.commit
        # Canonical Go JSON omits the zero-valued dirty:false field. An absent
        # field and the explicit boolean false are therefore the same clean
        # state; every other value remains fail-closed.
        or git.get("dirty", False) is not False
        or git.get("unavailable", False) is not False
        or "working_tree_digest" in git
    ):
        raise PackageError(
            "demo bundle is not bound to the clean immutable source commit"
        )
    if coverage.get("snapshot_id") != snapshot.get("id"):
        raise PackageError("demo coverage is not bound to the packaged demo snapshot")


def copy_license_material(source: Path, stage: Path) -> None:
    """Place project and third-party terms at the archive's visible root."""
    for name in TOP_LEVEL_LICENSE_FILES:
        copy_regular_file(
            source / name,
            stage / name,
            mode=0o644,
            label="license material",
        )
    copy_data_tree(source / "LICENSES", stage / "LICENSES", "license material")
    copy_regular_file(
        source.joinpath(*GO_MODULE_LOCK.parts),
        stage.joinpath(*GO_MODULE_LOCK.parts),
        mode=0o644,
        label="Go module audit lock",
    )
    validate_go_module_lock(stage)


def write_source_receipt(stage: Path, identity: SourceIdentity) -> None:
    """Bind the package source payload to one immutable Git commit and tree."""
    write_json_file(
        stage / "SOURCE.json",
        {
            "schema_version": "1.0",
            "repository": "https://github.com/neuroforge-io/RKC.git",
            "commit": identity.commit,
            "tree": identity.tree,
            "commit_time_unix": identity.commit_time_unix,
        },
    )


def write_readme(stage: Path, identity: SourceIdentity) -> None:
    """Write the archive entrypoint with explicit license boundaries."""
    version = (stage / "source" / "VERSION").read_text(encoding="utf-8").strip()
    content = f"""# Repository Knowledge Compiler complete package

Version: {version}
Source commit: {identity.commit}
Source tree: {identity.tree}

This byte-reproducible archive contains the immutable source commit, Linux
amd64/arm64 binaries built from that commit, deterministic mixed-language demo
artifacts, a canonical successful-validation receipt, a complete-distribution
SPDX 2.3 SBOM, contracts, and the detailed remainder implementation plan.
Volatile validation logs and benchmark
timing metrics are retained outside the ZIP in the same atomic release
generation; the receipt preserves their exact paths, sizes, SHA-256 values, and
aggregate evidence-manifest digest.

## Fast path

```sh
cd source
make verify
make build
./bin/rkc scan --out /tmp/rkc-output --force examples
./bin/rkc serve --dir /tmp/rkc-output --addr 127.0.0.1:8787
```

## Licensing

RKC-owned work is Apache-2.0; see `LICENSE` and `NOTICE`. Linked third-party
components retain their original terms; see `THIRD_PARTY_NOTICES.md` and
`LICENSES/`. The exact audited Go module graph is retained at
`third_party/go-modules.lock.json`. This package contains neither model weights
nor a llama.cpp source tree or executable.

The reference implementation is functional and tested. Production gaps are
stated explicitly in `source/docs/REMAINDER_IMPLEMENTATION_PLAN.md`; in
particular compiler-grade semantic adapters, enforced WASI/native-worker
isolation, the complete durable `rkcstore` SQLite backend, multi-tenant
PostgreSQL mode, release signatures, container SBOMs, provenance, and a
qualified measured real-GGUF profile remain planned work. `SBOM.spdx.json` is
the complete distribution SBOM for this archive.
"""
    with (stage / "README-FIRST.md").open("x", encoding="utf-8") as handle:
        handle.write(content)


def package_verification_code(digests: list[str]) -> str:
    """Compute the SPDX 2.3 package verification code from file SHA-1 values."""
    if not digests or any(
        len(value) != 40 or any(char not in "0123456789abcdef" for char in value)
        for value in digests
    ):
        raise PackageError("SPDX package verification input is invalid")
    payload = "".join(sorted(digests)).encode("ascii")
    return hashlib.sha1(payload, usedforsecurity=False).hexdigest()


def write_distribution_sbom(stage: Path, identity: SourceIdentity) -> None:
    """Write a deterministic SPDX 2.3 SBOM for the complete distribution."""
    excluded_names = {value.removeprefix("./") for value in DISTRIBUTION_SBOM_EXCLUSIONS}
    files: list[dict[str, object]] = []
    file_ids: dict[PurePosixPath, str] = {}
    file_sha1: dict[PurePosixPath, str] = {}
    for path in sorted(stage.rglob("*")):
        if path.is_symlink():
            raise PackageError(f"staged package contains a symlink: {path}")
        if not path.is_file():
            continue
        relative = PurePosixPath(path.relative_to(stage).as_posix())
        if relative.as_posix() in excluded_names:
            continue
        identifier = "SPDXRef-File-" + hashlib.sha256(
            relative.as_posix().encode("utf-8")
        ).hexdigest()[:24]
        if identifier in file_ids.values():
            raise PackageError("distribution SPDX file identifier collision")
        digest_sha1 = sha1(path)
        file_ids[relative] = identifier
        file_sha1[relative] = digest_sha1
        files.append(
            {
                "SPDXID": identifier,
                "fileName": f"./{relative.as_posix()}",
                "checksums": [
                    {"algorithm": "SHA1", "checksumValue": digest_sha1},
                    {"algorithm": "SHA256", "checksumValue": sha256(path)},
                ],
                "licenseConcluded": "NOASSERTION",
                "licenseInfoInFiles": ["NOASSERTION"],
                "copyrightText": "NOASSERTION",
            }
        )
    if not files:
        raise PackageError("distribution SPDX file inventory is empty")

    version = (stage / "source" / "VERSION").read_text(encoding="utf-8").strip()
    verification_code = package_verification_code(list(file_sha1.values()))
    distribution_package: dict[str, object] = {
        "SPDXID": "SPDXRef-Package-RKC-Distribution",
        "name": "Repository Knowledge Compiler complete distribution",
        "versionInfo": version,
        "downloadLocation": "https://github.com/neuroforge-io/RKC",
        "filesAnalyzed": True,
        "packageVerificationCode": {
            "packageVerificationCodeValue": verification_code,
            "packageVerificationCodeExcludedFiles": list(
                DISTRIBUTION_SBOM_EXCLUSIONS
            ),
        },
        "licenseConcluded": "NOASSERTION",
        "licenseDeclared": "NOASSERTION",
        "licenseInfoFromFiles": ["NOASSERTION"],
        "copyrightText": "NOASSERTION",
        "sourceInfo": f"Git commit {identity.commit}; Git tree {identity.tree}",
        "comment": (
            "The SPDX document, MANIFEST.json, and SHA256SUMS.txt are explicitly "
            "excluded to avoid circular self-reference. MANIFEST.json hashes this "
            "SBOM, and SHA256SUMS.txt hashes both generated receipts."
        ),
        "externalRefs": [
            {
                "referenceCategory": "OTHER",
                "referenceType": "vcs",
                "referenceLocator": (
                    "git+https://github.com/neuroforge-io/RKC.git@" + identity.commit
                ),
            }
        ],
    }

    packages: list[dict[str, object]] = [distribution_package]
    relationships: list[dict[str, str]] = [
        {
            "spdxElementId": "SPDXRef-DOCUMENT",
            "relationshipType": "DESCRIBES",
            "relatedSpdxElement": "SPDXRef-Package-RKC-Distribution",
        }
    ]
    relationships.extend(
        {
            "spdxElementId": "SPDXRef-Package-RKC-Distribution",
            "relationshipType": "CONTAINS",
            "relatedSpdxElement": item["SPDXID"],
        }
        for item in files
    )

    module_packages: dict[tuple[str, str], dict[str, object]] = {}
    component_dependencies: dict[str, set[tuple[str, str]]] = {}
    extracted_licenses: dict[str, dict[str, object]] = {}
    for binary_relative in sorted(EXPECTED_BINARIES, key=str):
        staged_binary = PurePosixPath("artifacts/binaries") / binary_relative
        staged_sbom = staged_binary.with_name(staged_binary.name + ".spdx.json")
        if staged_binary not in file_ids or staged_sbom not in file_ids:
            raise PackageError(f"distribution SPDX component is missing: {staged_binary}")
        sbom_path = stage.joinpath(*staged_sbom.parts)
        nested = load_strict_json(sbom_path, "verified per-binary SPDX document")
        if not isinstance(nested, dict) or nested.get("spdxVersion") != "SPDX-2.3":
            raise PackageError(f"per-binary SPDX document is malformed: {staged_sbom}")
        nested_packages = nested.get("packages")
        if not isinstance(nested_packages, list):
            raise PackageError(f"per-binary SPDX package inventory is malformed: {staged_sbom}")
        roots = [
            item
            for item in nested_packages
            if isinstance(item, dict) and item.get("SPDXID") == "SPDXRef-Package-RKC"
        ]
        if len(roots) != 1:
            raise PackageError(f"per-binary SPDX root package is missing: {staged_sbom}")

        platform = binary_relative.parts[0]
        command = binary_relative.name
        component_id = f"SPDXRef-Binary-{platform}-{command}"
        component_code = package_verification_code([file_sha1[staged_binary]])
        packages.append(
            {
                "SPDXID": component_id,
                "name": f"RKC {command} for {platform}",
                "versionInfo": version,
                "downloadLocation": "https://github.com/neuroforge-io/RKC",
                "filesAnalyzed": True,
                "packageVerificationCode": {
                    "packageVerificationCodeValue": component_code
                },
                "licenseConcluded": "NOASSERTION",
                "licenseDeclared": "Apache-2.0",
                "licenseInfoFromFiles": ["NOASSERTION"],
                "copyrightText": "Copyright 2026 RKC contributors",
                "sourceInfo": (
                    f"Git commit {identity.commit}; Git tree {identity.tree}; "
                    f"target {platform}"
                ),
            }
        )
        relationships.append(
            {
                "spdxElementId": "SPDXRef-Package-RKC-Distribution",
                "relationshipType": "CONTAINS",
                "relatedSpdxElement": component_id,
            }
        )
        relationships.append(
            {
                "spdxElementId": component_id,
                "relationshipType": "CONTAINS",
                "relatedSpdxElement": file_ids[staged_binary],
            }
        )

        dependencies: set[tuple[str, str]] = set()
        for item in nested_packages:
            if not isinstance(item, dict) or item.get("SPDXID") == "SPDXRef-Package-RKC":
                continue
            name, dependency_version = item.get("name"), item.get("versionInfo")
            if not isinstance(name, str) or not isinstance(dependency_version, str):
                raise PackageError(f"per-binary SPDX dependency is malformed: {staged_sbom}")
            key = (name, dependency_version)
            normalized = {
                field: item[field]
                for field in (
                    "name",
                    "versionInfo",
                    "downloadLocation",
                    "filesAnalyzed",
                    "licenseConcluded",
                    "licenseDeclared",
                    "copyrightText",
                    "externalRefs",
                )
                if field in item
            }
            previous = module_packages.setdefault(key, normalized)
            if previous != normalized:
                raise PackageError(f"per-binary SPDX dependency drifted: {name}")
            dependencies.add(key)
        component_dependencies[component_id] = dependencies

        nested_extracted = nested.get("hasExtractedLicensingInfos", [])
        if not isinstance(nested_extracted, list):
            raise PackageError(f"per-binary extracted licenses are malformed: {staged_sbom}")
        for item in nested_extracted:
            if not isinstance(item, dict) or not isinstance(item.get("licenseId"), str):
                raise PackageError(f"per-binary extracted license is malformed: {staged_sbom}")
            identifier = item["licenseId"]
            previous = extracted_licenses.setdefault(identifier, item)
            if previous != item:
                raise PackageError(f"per-binary extracted license drifted: {identifier}")

    module_ids: dict[tuple[str, str], str] = {}
    for key in sorted(module_packages):
        name, dependency_version = key
        identifier = "SPDXRef-GoModule-" + hashlib.sha256(
            f"{name}@{dependency_version}".encode("utf-8")
        ).hexdigest()[:24]
        module_ids[key] = identifier
        package = {"SPDXID": identifier, **module_packages[key]}
        if package.get("filesAnalyzed") is not False or package.get(
            "licenseConcluded"
        ) != "NOASSERTION":
            raise PackageError(f"per-binary dependency SPDX posture is invalid: {name}")
        packages.append(package)
    for component_id in sorted(component_dependencies):
        for dependency in sorted(component_dependencies[component_id]):
            relationships.append(
                {
                    "spdxElementId": component_id,
                    "relationshipType": "DEPENDS_ON",
                    "relatedSpdxElement": module_ids[dependency],
                }
            )

    created = datetime.fromtimestamp(
        int(identity.commit_time_unix), timezone.utc
    ).strftime("%Y-%m-%dT%H:%M:%SZ")
    document: dict[str, object] = {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": f"RKC-complete-distribution-{version}",
        "documentNamespace": (
            "https://neuroforge.io/rkc/spdx/distribution/"
            f"{identity.tree}/{verification_code}"
        ),
        "creationInfo": {
            "created": created,
            "creators": ["Tool: RKC package-complete.py"],
        },
        "comment": (
            f"Immutable source commit {identity.commit}; source tree {identity.tree}; "
            "all substantive files plus platform binaries and linked Go modules are "
            "enumerated. Circular generated receipts are explicitly excluded."
        ),
        "packages": packages,
        "files": files,
        "relationships": relationships,
    }
    if extracted_licenses:
        document["hasExtractedLicensingInfos"] = [
            extracted_licenses[key] for key in sorted(extracted_licenses)
        ]
    write_json_file(stage / "SBOM.spdx.json", document)


def stage_manifest(stage: Path, identity: SourceIdentity) -> None:
    """Write deterministic payload metadata, including the distribution SBOM."""
    payload: list[dict[str, object]] = []
    for path in sorted(stage.rglob("*")):
        if path.is_symlink():
            raise PackageError(f"staged package contains a symlink: {path}")
        if path.is_file() and path.name not in {"SHA256SUMS.txt", "MANIFEST.json"}:
            payload.append(
                {
                    "path": path.relative_to(stage).as_posix(),
                    "size_bytes": path.stat().st_size,
                    "sha256": sha256(path),
                }
            )
    manifest = {
        "schema_version": "1.0",
        "name": "repository-knowledge-compiler-complete",
        "version": (stage / "source" / "VERSION").read_text(encoding="utf-8").strip(),
        "source_commit": identity.commit,
        "source_tree": identity.tree,
        "source_commit_time_unix": identity.commit_time_unix,
        "project_license": "Apache-2.0",
        "third_party_notices": "THIRD_PARTY_NOTICES.md",
        "go_module_lock": GO_MODULE_LOCK.as_posix(),
        "excluded_generated_files": ["MANIFEST.json", "SHA256SUMS.txt"],
        "payload_files": len(payload),
        "payload_bytes": sum(int(item["size_bytes"]) for item in payload),
        "files": payload,
    }
    with (stage / "MANIFEST.json").open("x", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2, sort_keys=True)
        handle.write("\n")


def write_stage_checksums(stage: Path) -> None:
    """Write the final checksum receipt, excluding only its own circular hash."""
    checks: list[str] = []
    for path in sorted(stage.rglob("*")):
        if path.is_file() and path.name != "SHA256SUMS.txt":
            checks.append(f"{sha256(path)}  {path.relative_to(stage).as_posix()}")
    with (stage / "SHA256SUMS.txt").open("x", encoding="utf-8") as handle:
        handle.write("\n".join(checks) + "\n")


def prepare_output(raw_output: str, force: bool) -> Path:
    """Resolve an output below dist without traversing symlink directories."""
    try:
        dist_info = os.lstat(DIST)
    except FileNotFoundError as exc:
        raise PackageError("dist does not exist; build release inputs first") from exc
    if stat.S_ISLNK(dist_info.st_mode) or not stat.S_ISDIR(dist_info.st_mode):
        raise PackageError("dist must be a real directory, not a symlink")

    candidate = Path(raw_output)
    if not candidate.is_absolute():
        candidate = ROOT / candidate
    candidate = Path(os.path.abspath(candidate))
    try:
        relative = candidate.relative_to(DIST)
    except ValueError as exc:
        raise PackageError("--output must be inside the repository dist directory") from exc
    if candidate.suffix.lower() != ".zip" or candidate.name in {"", ".", ".."}:
        raise PackageError("--output must name a .zip file")
    if not relative.parts or relative.parts[0] in RESERVED_OUTPUT_ROOTS:
        raise PackageError("--output must not overlap release input directories")

    cursor = DIST
    for part in relative.parts[:-1]:
        cursor = cursor / part
        if os.path.lexists(cursor):
            info = os.lstat(cursor)
            if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
                raise PackageError(f"output parent is not a real directory: {cursor}")
        else:
            cursor.mkdir(mode=0o755)

    if os.path.lexists(candidate):
        info = os.lstat(candidate)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise PackageError("existing output is not a replaceable regular file")
        if not force:
            raise PackageError("output already exists; pass --force to replace it")
    return candidate


def write_zip(stage: Path, output: Path, force: bool) -> None:
    """Write through an exclusive sibling and publish without unsafe clobbering."""
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{output.name}.", suffix=".tmp", dir=output.parent
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w+b") as temporary_handle:
            # Stored entries avoid zlib-version-dependent DEFLATE bytes. The
            # archive is intentionally larger in exchange for reproducibility
            # across conforming ZIP writers and supported build hosts.
            with zipfile.ZipFile(
                temporary_handle,
                "w",
                compression=zipfile.ZIP_STORED,
            ) as archive:
                for path in sorted(stage.rglob("*")):
                    if not path.is_file():
                        continue
                    relative = Path(TOP) / path.relative_to(stage)
                    info = zipfile.ZipInfo(
                        relative.as_posix(), date_time=(1980, 1, 1, 0, 0, 0)
                    )
                    permissions = 0o755 if path.stat().st_mode & stat.S_IXUSR else 0o644
                    info.external_attr = (permissions & 0xFFFF) << 16
                    info.compress_type = zipfile.ZIP_STORED
                    info.create_system = 3
                    with path.open("rb") as source, archive.open(info, "w") as target:
                        shutil.copyfileobj(source, target, 1024 * 1024)
            temporary_handle.flush()
            os.fsync(temporary_handle.fileno())
        os.chmod(temporary, 0o644)

        if force:
            if os.path.lexists(output):
                info = os.lstat(output)
                if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
                    raise PackageError("output changed into an unsafe replacement target")
            os.replace(temporary, output)
        else:
            try:
                os.link(temporary, output)
            except FileExistsError as exc:
                raise PackageError("output appeared during packaging; refusing to replace it") from exc
            temporary.unlink()

        if hasattr(os, "O_DIRECTORY"):
            directory_fd = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
    finally:
        temporary.unlink(missing_ok=True)


def build_package(output: Path, force: bool) -> None:
    """Assemble and atomically publish the complete package."""
    identity = source_identity()
    require_clean_tracked_source()
    for relative in REQUIRED_INPUTS:
        path = DIST / relative
        try:
            info = os.lstat(path)
        except FileNotFoundError as exc:
            raise PackageError(f"missing release input: {path}") from exc
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise PackageError(f"release input is not a real directory: {path}")

    with tempfile.TemporaryDirectory(prefix="rkc-package-") as temporary_directory:
        stage = Path(temporary_directory)
        source = stage / "source"
        source.mkdir(mode=0o755)
        copy_tracked_source(source, identity)
        copy_license_material(source, stage)
        copy_release_binaries(
            DIST / "binaries",
            stage / "artifacts" / "binaries",
            source.joinpath(*GO_MODULE_LOCK.parts),
            source,
            identity,
        )
        validate_demo_outputs(DIST / "demo", identity)
        copy_required_files(
            DIST / "demo",
            stage / "artifacts" / "demo",
            ("bundle.json", "coverage.json"),
            "deterministic demo output",
        )
        validation_receipt = validate_release_evidence(
            DIST / "evidence" / "validation",
            DIST / "evidence" / "benchmark",
            identity,
        )
        write_json_file(
            stage / "artifacts" / "validation" / "receipt.json",
            validation_receipt,
        )
        write_source_receipt(stage, identity)
        write_readme(stage, identity)
        write_distribution_sbom(stage, identity)
        stage_manifest(stage, identity)
        write_stage_checksums(stage)
        if source_identity() != identity:
            raise PackageError("repository HEAD changed during package assembly")
        write_zip(stage, output, force)


def main() -> None:
    """Parse command-line arguments and build one package."""
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True)
    parser.add_argument(
        "--force",
        action="store_true",
        help="atomically replace an existing regular output file",
    )
    args = parser.parse_args()
    try:
        require_clean_worktree(ROOT, "complete package creation")
        output = prepare_output(args.output, args.force)
        build_package(output, args.force)
    except SourceGuardError as exc:
        raise SystemExit(f"package error: {exc}") from exc
    except PackageError as exc:
        raise SystemExit(f"package error: {exc}") from exc
    print(
        json.dumps(
            {
                "output": str(output),
                "size_bytes": output.stat().st_size,
                "sha256": sha256(output),
            },
            indent=2,
            sort_keys=True,
        )
    )


if __name__ == "__main__":
    main()
