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
import tempfile
import zipfile
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

ROOT = Path(__file__).resolve().parents[1]
DIST = ROOT / "dist"
TOP = "repository-knowledge-compiler-complete"

REQUIRED_INPUTS = (
    Path("binaries"),
    Path("demo"),
    Path("validation"),
    Path("benchmark"),
)
RESERVED_OUTPUT_ROOTS = frozenset(path.parts[0] for path in REQUIRED_INPUTS)
TOP_LEVEL_LICENSE_FILES = ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md")
EXPECTED_BINARIES = frozenset(
    {
        PurePosixPath("linux-amd64/rkc"),
        PurePosixPath("linux-amd64/rkc-mcp"),
        PurePosixPath("linux-arm64/rkc"),
        PurePosixPath("linux-arm64/rkc-mcp"),
    }
)
BINARY_NOTICE_FILES = frozenset(
    PurePosixPath(path)
    for prefix in ("", "linux-amd64/", "linux-arm64/")
    for path in (
        f"{prefix}LICENSE",
        f"{prefix}NOTICE",
        f"{prefix}THIRD_PARTY_NOTICES.md",
        f"{prefix}LICENSES/Go.txt",
    )
)
EXPECTED_BINARY_BUNDLE = EXPECTED_BINARIES | BINARY_NOTICE_FILES
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
    """One stage-zero regular file recorded in Git's index."""

    path: PurePosixPath
    executable: bool


def sha256(path: Path) -> str:
    """Return the SHA-256 digest of a regular file."""
    digest = hashlib.sha256()
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


def tracked_files() -> list[TrackedFile]:
    """Read stage-zero tracked files from Git without filename quoting."""
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--stage", "-z"],
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
            mode, _object_id, stage = header.split(b" ", 2)
            value = raw_path.decode("utf-8")
        except (UnicodeDecodeError, ValueError) as exc:
            raise PackageError("Git index contains an unportable entry") from exc
        path = safe_relative_path(value, "tracked source path")
        if stage != b"0":
            raise PackageError(f"tracked source is unmerged: {path}")
        if mode == b"120000":
            raise PackageError(f"tracked source symlinks are prohibited: {path}")
        if mode == b"160000":
            raise PackageError(f"Git submodules require explicit release review: {path}")
        if mode not in {b"100644", b"100755"}:
            raise PackageError(f"unsupported tracked source mode {mode!r}: {path}")
        if path in seen:
            raise PackageError(f"duplicate tracked source path: {path}")
        if path.parts[0] in {"bin", "dist"} or path.parts[0].startswith(".rkc"):
            raise PackageError(f"generated output is tracked as source: {path}")
        seen.add(path)
        files.append(TrackedFile(path=path, executable=mode == b"100755"))
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


def copy_tracked_source(target: Path) -> None:
    """Copy only regular files recorded in the Git index."""
    for item in tracked_files():
        if prohibited_name(item.path):
            raise PackageError(f"prohibited tracked source artifact: {item.path}")
        assert_no_symlink_components(ROOT, item.path)
        source = ROOT.joinpath(*item.path.parts)
        destination = target.joinpath(*item.path.parts)
        copy_regular_file(
            source,
            destination,
            mode=0o755 if item.executable else 0o644,
            label="tracked source",
        )

    for name in TOP_LEVEL_LICENSE_FILES:
        if not (target / name).is_file():
            raise PackageError(f"required license file is not tracked: {name}")
    licenses = target / "LICENSES"
    if not licenses.is_dir() or not any(licenses.iterdir()):
        raise PackageError("the tracked LICENSES directory is missing or empty")


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


def copy_release_binaries(source: Path, target: Path) -> None:
    """Copy the four supported executables and their exact license bundles."""
    files = tree_files(source, "release binaries")
    actual = frozenset(relative for relative, _path in files)
    if actual != EXPECTED_BINARY_BUNDLE:
        missing = sorted(str(path) for path in EXPECTED_BINARY_BUNDLE - actual)
        unexpected = sorted(str(path) for path in actual - EXPECTED_BINARY_BUNDLE)
        raise PackageError(
            f"release binary set differs from policy; missing={missing}, unexpected={unexpected}"
        )
    for relative, path in files:
        if relative in EXPECTED_BINARIES:
            with path.open("rb") as handle:
                if handle.read(4) != b"\x7fELF":
                    raise PackageError(f"release binary is not an ELF executable: {path}")
            mode = 0o755
            reject_native = False
        else:
            notice_parts = relative.parts
            if notice_parts[0] in {"linux-amd64", "linux-arm64"}:
                notice_parts = notice_parts[1:]
            canonical_notice = ROOT.joinpath(*notice_parts)
            if sha256(path) != sha256(canonical_notice):
                raise PackageError(f"release binary notice differs from tracked source: {path}")
            mode = 0o644
            reject_native = True
        copy_regular_file(
            path,
            target.joinpath(*relative.parts),
            mode=mode,
            label="release binary bundle",
            reject_native=reject_native,
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


def write_readme(stage: Path) -> None:
    """Write the archive entrypoint with explicit license boundaries."""
    version = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    content = f"""# Repository Knowledge Compiler complete package

Version: {version}

This archive contains the tracked source tree, Linux amd64/arm64 binaries, a
generated mixed-language demonstration atlas, release-verification logs,
contracts, and the detailed remainder implementation plan.

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
`LICENSES/`. This package contains neither model weights nor a llama.cpp source
tree or executable.

The reference implementation is functional and tested. Production gaps are
stated explicitly in `source/docs/REMAINDER_IMPLEMENTATION_PLAN.md`; in
particular compiler-grade semantic adapters, enforced WASI/native-worker
isolation, a canonical SQLite runtime writer, multi-tenant PostgreSQL mode, and
a measured real-GGUF under-2.5-GiB benchmark remain planned work.
"""
    with (stage / "README-FIRST.md").open("x", encoding="utf-8") as handle:
        handle.write(content)


def stage_manifest(stage: Path) -> None:
    """Write deterministic payload metadata and checksums."""
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
        "version": (ROOT / "VERSION").read_text(encoding="utf-8").strip(),
        "project_license": "Apache-2.0",
        "third_party_notices": "THIRD_PARTY_NOTICES.md",
        "payload_files": len(payload),
        "payload_bytes": sum(int(item["size_bytes"]) for item in payload),
        "files": payload,
    }
    with (stage / "MANIFEST.json").open("x", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2, sort_keys=True)
        handle.write("\n")

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
            with zipfile.ZipFile(
                temporary_handle,
                "w",
                compression=zipfile.ZIP_DEFLATED,
                compresslevel=9,
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
                    info.compress_type = zipfile.ZIP_DEFLATED
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
        copy_tracked_source(source)
        copy_license_material(source, stage)
        copy_release_binaries(DIST / "binaries", stage / "artifacts" / "binaries")
        copy_data_tree(DIST / "demo", stage / "artifacts" / "demo", "demo output")
        copy_data_tree(
            DIST / "validation",
            stage / "artifacts" / "validation",
            "validation output",
        )
        copy_selected_files(
            DIST / "benchmark",
            stage / "artifacts" / "benchmark",
            ("result.json", "time.txt", "scan.stdout"),
            "benchmark output",
        )
        copy_selected_files(
            DIST,
            stage / "artifacts" / "demo-logs",
            ("demo-scan.txt", "demo-check.txt", "demo-synthesis.txt"),
            "demo log",
        )
        write_readme(stage)
        stage_manifest(stage)
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
        output = prepare_output(args.output, args.force)
        build_package(output, args.force)
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
