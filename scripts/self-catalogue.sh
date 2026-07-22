#!/usr/bin/env bash
# Build a deterministic RKC atlas of RKC without exposing generated output as input.
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$ROOT"
umask 077

if [[ -z ${RKC_RESOURCE_GUARD_UNIT:-} ]]; then
  echo "self-catalogue: run through 'make self-catalogue'; the resource guard is mandatory" >&2
  exit 1
fi

for required in git go make sha256sum; do
  if ! command -v "$required" >/dev/null 2>&1; then
    echo "self-catalogue: required command not found: $required" >&2
    exit 1
  fi
done

# Self-cataloguing is a supply-chain boundary. Never inherit an activated or
# repository-local virtual environment: every helper in this workflow uses only
# the Python standard library from a system interpreter.
PYTHON=
for candidate in /usr/bin/python3 /usr/local/bin/python3; do
  if [[ -x $candidate ]] && "$candidate" -I -S -c \
    'import sys; raise SystemExit(sys.prefix != sys.base_prefix)'; then
    PYTHON=$candidate
    break
  fi
done
if [[ -z $PYTHON ]]; then
  echo "self-catalogue: an isolated, non-virtualenv system python3 is required" >&2
  exit 1
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-self-catalogue.XXXXXX")
SOURCE="$WORK/RKC"
SOURCE_MANIFEST="$WORK/source-manifest.json"
STATUS="$WORK/git-status"
FINAL_STATUS="$WORK/git-status-final"
BUILD_INFO="$WORK/rkc-build-info"
STAGING=
COMMIT=

quarantine_incomplete_staging() {
  [[ -n ${STAGING:-} && -e $STAGING ]] || return 0
  "$PYTHON" -I -S - "$ROOT" "$STAGING" <<'PY'
from __future__ import annotations

import json
import os
import secrets
import stat
import sys
from pathlib import Path

root = Path(sys.argv[1])
stage = Path(sys.argv[2])
dist = root / "dist"
marker_name = ".rkc-self-catalogue.json"
marker = (
    json.dumps(
        {"kind": "rkc-self-catalogue", "producer": "rkc", "schema_version": "1.0.0"},
        indent=2,
        sort_keys=True,
    )
    + "\n"
).encode()
if stage.parent != dist or not stage.name.startswith(".rkc-self-catalogue-build-"):
    raise SystemExit("refusing to quarantine an unrecognized staging path")
flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
dist_fd = os.open(dist, flags)
try:
    info = os.stat(stage.name, dir_fd=dist_fd, follow_symlinks=False)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise SystemExit("refusing to quarantine staging without private ownership")
    stage_fd = os.open(stage.name, flags, dir_fd=dist_fd)
    try:
        marker_fd = os.open(
            marker_name,
            os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0),
            dir_fd=stage_fd,
        )
        with os.fdopen(marker_fd, "rb") as handle:
            if handle.read(len(marker) + 1) != marker:
                raise SystemExit("refusing to quarantine staging with an invalid marker")
    finally:
        os.close(stage_fd)
    for _attempt in range(128):
        quarantine = f".rkc-self-catalogue-quarantine-failed-{secrets.token_hex(8)}"
        try:
            os.stat(quarantine, dir_fd=dist_fd, follow_symlinks=False)
        except FileNotFoundError:
            os.rename(stage.name, quarantine, src_dir_fd=dist_fd, dst_dir_fd=dist_fd)
            os.fsync(dist_fd)
            print(f"self-catalogue: quarantined incomplete staging at dist/{quarantine}", file=sys.stderr)
            break
    else:
        raise SystemExit("could not allocate an exclusive quarantine name")
finally:
    os.close(dist_fd)
PY
}

cleanup() {
  result=$?
  trap - EXIT INT TERM
  if (( result != 0 )); then
    quarantine_incomplete_staging || \
      echo "self-catalogue: WARNING: failed staging could not be quarantined" >&2
  fi
  rm -rf -- "$WORK"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# Generated and local runtime trees are named explicitly because RKC exclusions
# are exact paths, not globs. The staging policy below additionally rejects every
# path component beginning with .rkc and every model-weight signature/suffix.
EXCLUSIONS=(
  .cache
  .coverage
  .git
  .mypy_cache
  .pytest_cache
  .rkc
  .rkc-coverage
  .rkc-downloads
  .rkc-models
  .rkc-runtime
  .rkc-state
  .rkc.rkc-derived
  .ruff_cache
  .venv
  __pycache__
  bin
  coverage
  coverage.out
  coverage.xml
  dist
  htmlcov
  models/downloads
  models/runtime
  venv
)

COMMIT=$(git rev-parse HEAD)
TREE=$(git rev-parse 'HEAD^{tree}')
OBJECT_FORMAT=$(git rev-parse --show-object-format)
if [[ ! $COMMIT =~ ^[0-9a-f]{40}$ && ! $COMMIT =~ ^[0-9a-f]{64}$ ]]; then
  echo "self-catalogue: invalid Git commit identity" >&2
  exit 1
fi
if [[ ! $TREE =~ ^[0-9a-f]{40}$ && ! $TREE =~ ^[0-9a-f]{64}$ ]]; then
  echo "self-catalogue: invalid Git tree identity" >&2
  exit 1
fi

git status --porcelain=v1 -z --untracked-files=all --ignore-submodules=none >"$STATUS"
if [[ -s "$STATUS" ]]; then
  echo "self-catalogue: Git worktree is dirty; commit or remove all changes first" >&2
  "$PYTHON" -I -S - "$STATUS" <<'PY' >&2
import sys
from pathlib import Path

entries = [item for item in Path(sys.argv[1]).read_bytes().split(b"\0") if item]
for entry in entries[:20]:
    print("  " + entry.decode("utf-8", errors="replace"))
if len(entries) > 20:
    print(f"  ... and {len(entries) - 20} more")
PY
  exit 1
fi
if [[ $(git rev-parse HEAD) != "$COMMIT" || $(git rev-parse 'HEAD^{tree}') != "$TREE" ]]; then
  echo "self-catalogue: HEAD changed during initial source capture" >&2
  exit 1
fi

mkdir -m 0700 "$SOURCE"
"$PYTHON" -I -S - "$ROOT" "$SOURCE" "$SOURCE_MANIFEST" "$COMMIT" "$TREE" "$OBJECT_FORMAT" "${EXCLUSIONS[@]}" <<'PY'
from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path, PurePosixPath

root = Path(sys.argv[1])
target = Path(sys.argv[2])
manifest_path = Path(sys.argv[3])
commit, tree, object_format = sys.argv[4:7]
exclusions = tuple(sorted(set(sys.argv[7:])))
maximum_bytes = 64 * 1024 * 1024
weight_suffixes = {
    ".bin", ".ckpt", ".ggml", ".gguf", ".h5", ".hdf5", ".model",
    ".npy", ".npz", ".onnx", ".pt", ".pth", ".safetensors", ".tflite",
    ".weights",
}
weight_magic = (b"GGUF", b"GGML")


def fail(message: str) -> None:
    raise SystemExit(f"self-catalogue staging: {message}")


def safe_path(value: str) -> PurePosixPath:
    if not value or "\\" in value or any(ord(char) < 32 for char in value):
        fail(f"unportable Git path: {value!r}")
    path = PurePosixPath(value)
    if path.is_absolute() or path.as_posix() != value or any(
        part in {"", ".", ".."} for part in path.parts
    ):
        fail(f"non-canonical Git path: {value!r}")
    return path


def is_excluded(path: PurePosixPath) -> bool:
    value = path.as_posix()
    if any(value == item or value.startswith(item + "/") for item in exclusions):
        return True
    return any(
        part.startswith(".rkc")
        or part in {".venv", "venv", "__pycache__", "coverage", "htmlcov"}
        for part in path.parts
    )


tree_result = subprocess.run(
    ["git", "rev-parse", f"{commit}^{{tree}}"],
    cwd=root,
    check=False,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
)
if tree_result.returncode != 0 or tree_result.stdout.decode("ascii").strip() != tree:
    fail("recorded commit no longer resolves to the recorded tree")
result = subprocess.run(
    ["git", "ls-tree", "-r", "-z", "--full-tree", commit],
    cwd=root,
    check=False,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
)
if result.returncode != 0:
    fail("cannot enumerate the recorded commit tree: " + result.stderr.decode(errors="replace"))

records = []
excluded_tracked = []
seen = set()
for raw in result.stdout.split(b"\0"):
    if not raw:
        continue
    try:
        header, raw_name = raw.split(b"\t", 1)
        mode, kind, object_id = header.split(b" ", 2)
        name = raw_name.decode("utf-8")
        expected_object = object_id.decode("ascii")
    except (UnicodeDecodeError, ValueError) as exc:
        fail(f"malformed or non-UTF-8 commit-tree entry: {exc}")
    path = safe_path(name)
    if mode in {b"120000", b"160000"}:
        fail(f"tracked symlink or submodule is prohibited: {name}")
    if kind != b"blob" or mode not in {b"100644", b"100755"}:
        fail(f"unsupported tracked mode {mode!r}: {name}")
    if name in seen:
        fail(f"duplicate commit-tree path: {name}")
    seen.add(name)
    if is_excluded(path):
        excluded_tracked.append(name)
        continue
    if path.name == ".rkc-generated.json":
        fail(f"tracked RKC-generated ownership marker is prohibited: {name}")
    if path.suffix.lower() in weight_suffixes:
        fail(f"tracked model-weight/native tensor suffix is prohibited: {name}")

    size_result = subprocess.run(
        ["git", "cat-file", "-s", expected_object],
        cwd=root,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    try:
        object_size = int(size_result.stdout.decode("ascii").strip())
    except (UnicodeDecodeError, ValueError):
        fail(f"cannot determine committed blob size: {name}")
    if size_result.returncode != 0 or object_size < 0 or object_size > maximum_bytes:
        fail(f"committed blob exceeds the 64 MiB self-scan ceiling: {name}")
    blob_result = subprocess.run(
        ["git", "cat-file", "blob", expected_object],
        cwd=root,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if blob_result.returncode != 0:
        fail(f"cannot read committed blob: {name}")
    payload = blob_result.stdout
    if len(payload) != object_size:
        fail(f"committed blob size changed while read: {name}")
    if payload.startswith(weight_magic) or payload.startswith(
        b"version https://git-lfs.github.com/spec/v1"
    ):
        fail(f"tracked model/binary pointer content is prohibited: {name}")
    object_hash = hashlib.new(object_format)
    object_hash.update(f"blob {len(payload)}\0".encode())
    object_hash.update(payload)
    if object_hash.hexdigest() != expected_object:
        fail(f"committed blob failed object-integrity verification: {name}")

    destination = target.joinpath(*path.parts)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    output_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    committed_mode = 0o755 if mode == b"100755" else 0o644
    output = os.open(destination, output_flags, committed_mode)
    with os.fdopen(output, "wb") as handle:
        # The workflow-wide umask deliberately makes every new file private.
        # Restore the exact verified Git mode only after exclusive creation;
        # the detached tree itself remains inside a private temporary root.
        os.fchmod(handle.fileno(), committed_mode)
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    records.append(
        {
            "git_object": expected_object,
            "mode": mode.decode("ascii"),
            "path": name,
            "sha256": hashlib.sha256(payload).hexdigest(),
            "size_bytes": len(payload),
        }
    )

records.sort(key=lambda item: item["path"])
if not records:
    fail("no eligible tracked source files")
manifest = {
    "schema_version": "1.0.0",
    "selection": "recorded-commit-tree-regular-blobs",
    "source": {"commit": commit, "tree": tree, "object_format": object_format},
    "policy": {
        "exact_exclusions": list(exclusions),
        "generic_exclusions": [
            "any .rkc-prefixed path component",
            "any coverage, virtualenv, or Python cache path component",
        ],
        "maximum_file_bytes": maximum_bytes,
        "model_weights": "prohibited by suffix, magic, and Git-LFS-pointer policy",
        "symlinks_and_submodules": "prohibited",
    },
    "excluded_tracked_files": sorted(excluded_tracked),
    "file_count": len(records),
    "total_bytes": sum(item["size_bytes"] for item in records),
    "files": records,
}
encoded = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()
with manifest_path.open("xb") as handle:
    handle.write(encoded)
PY

# The build consumes the exact detached tree assembled above. It never reads the
# mutable checkout, index, or an ignored virtual environment.
env -u MAKEFLAGS -u MFLAGS -u MAKEFILES \
  make -C "$SOURCE" PYTHON="$PYTHON" build
RKC_BIN="$SOURCE/bin/rkc"
if [[ ! -f $RKC_BIN || ! -x $RKC_BIN || -L $RKC_BIN ]]; then
  echo "self-catalogue: staged-source bin/rkc must be a real executable" >&2
  exit 1
fi
go version -m "$RKC_BIN" >"$BUILD_INFO"
if grep -Fq $'\tbuild\tvcs.modified=true' "$BUILD_INFO"; then
  echo "self-catalogue: staged-source binary reports dirty VCS metadata" >&2
  exit 1
fi
if ! grep -Fq $'\tbuild\t-trimpath=true' "$BUILD_INFO"; then
  echo "self-catalogue: staged-source bin/rkc was not built with -trimpath" >&2
  exit 1
fi
if grep -Fq $'\tbuild\tvcs.revision=' "$BUILD_INFO" && \
  ! grep -Fq $'\tbuild\tvcs.revision='"$COMMIT" "$BUILD_INFO"; then
  echo "self-catalogue: staged-source binary reports the wrong VCS revision" >&2
  exit 1
fi

# Verify that the build changed only its explicitly excluded bin directory and
# that every committed input still matches the recorded Git blob.
verify_staged_source() {
  "$PYTHON" -I -S - "$SOURCE" "$SOURCE_MANIFEST" <<'PY'
from __future__ import annotations

import hashlib
import json
import os
import stat
import sys
from pathlib import Path

source = Path(sys.argv[1])
manifest = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))
expected = {item["path"]: item for item in manifest["files"]}
allowed_generated = {"bin/rkc", "bin/rkc-mcp"}
observed = set()
for current, directories, files in os.walk(source, topdown=True, followlinks=False):
    directories.sort()
    files.sort()
    base = Path(current)
    for name in directories:
        info = os.lstat(base / name)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise SystemExit(f"self-catalogue source audit: link/special directory: {base / name}")
    for name in files:
        path = base / name
        relative = path.relative_to(source).as_posix()
        info = os.lstat(path)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise SystemExit(f"self-catalogue source audit: link/special file: {relative}")
        if relative in allowed_generated:
            continue
        record = expected.get(relative)
        if record is None:
            raise SystemExit(f"self-catalogue source audit: unexpected build output: {relative}")
        data = path.read_bytes()
        if len(data) != record["size_bytes"] or hashlib.sha256(data).hexdigest() != record["sha256"]:
            raise SystemExit(f"self-catalogue source audit: committed source mutated: {relative}")
        required_mode = 0o755 if record["mode"] == "100755" else 0o644
        if stat.S_IMODE(info.st_mode) != required_mode:
            raise SystemExit(f"self-catalogue source audit: committed mode mutated: {relative}")
        observed.add(relative)
if observed != set(expected):
    missing = sorted(set(expected) - observed)
    raise SystemExit(f"self-catalogue source audit: committed files missing: {missing[:5]}")
for relative in sorted(allowed_generated):
    path = source / relative
    info = os.lstat(path)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise SystemExit(f"self-catalogue source audit: invalid staged binary: {relative}")
PY
}
verify_staged_source

# Allocate a private same-filesystem sibling. Until the final atomic rename or
# exchange, the last-known-good dist/self-catalogue directory is untouched.
STAGING=$("$PYTHON" -I -S - "$ROOT" <<'PY'
from __future__ import annotations

import json
import os
import secrets
import stat
import sys
from pathlib import Path

root = Path(sys.argv[1])
dist = root / "dist"
marker_name = ".rkc-self-catalogue.json"
marker = (
    json.dumps(
        {"kind": "rkc-self-catalogue", "producer": "rkc", "schema_version": "1.0.0"},
        indent=2,
        sort_keys=True,
    )
    + "\n"
).encode()
if not os.path.lexists(dist):
    dist.mkdir(mode=0o755)
info = os.lstat(dist)
if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
    raise SystemExit("self-catalogue output: dist must be a real owner-controlled directory")
flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
dist_fd = os.open(dist, flags)
try:
    for _attempt in range(128):
        name = f".rkc-self-catalogue-build-{secrets.token_hex(8)}"
        try:
            os.mkdir(name, mode=0o700, dir_fd=dist_fd)
            break
        except FileExistsError:
            continue
    else:
        raise SystemExit("self-catalogue output: could not allocate private staging")
    stage_fd = os.open(name, flags, dir_fd=dist_fd)
    try:
        marker_fd = os.open(
            marker_name,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
            0o600,
            dir_fd=stage_fd,
        )
        with os.fdopen(marker_fd, "wb") as handle:
            handle.write(marker)
            handle.flush()
            os.fsync(handle.fileno())
        os.fsync(stage_fd)
    finally:
        os.close(stage_fd)
    os.fsync(dist_fd)
finally:
    os.close(dist_fd)
print(dist / name)
PY
)
OUT="$STAGING"
ATLAS="$OUT/atlas"
MANIFEST="$OUT/MANIFEST.json"
CHECKSUMS="$OUT/SHA256SUMS.txt"

SCAN_ARGS=(
  scan
  --json
  --fail-on-errors
  --out "$ATLAS"
  --force
  # Keep the canonical toolchain identity independent of checkout location.
  --python "$PYTHON"
)
for excluded in "${EXCLUSIONS[@]}"; do
  SCAN_ARGS+=(--exclude "$excluded")
done
"$RKC_BIN" "${SCAN_ARGS[@]}" "$SOURCE" >"$WORK/scan.json"

"$RKC_BIN" check \
  --json \
  --coverage "$ATLAS/coverage.json" \
  --bundle "$ATLAS/bundle.json" \
  --export-manifest "$ATLAS/rkc-export-manifest.json" \
  --min-inventory-accounting 1 \
  --min-symbol-evidence 1 \
  --min-claim-citation 1 \
  --max-errors 0 \
  --max-fatal 0 \
  --max-high-confidence-secrets 0 \
  >"$WORK/check.json"

"$PYTHON" -I -S - \
  "$ROOT" "$OUT" "$ATLAS" "$SOURCE_MANIFEST" "$WORK/scan.json" "$WORK/check.json" \
  "$RKC_BIN" "$BUILD_INFO" "$COMMIT" "$TREE" "$MANIFEST" "$CHECKSUMS" <<'PY'
from __future__ import annotations

import hashlib
import json
import os
import stat
import sys
from pathlib import Path

(
    root_value,
    output_value,
    atlas_value,
    source_manifest_value,
    scan_value,
    check_value,
    binary_value,
    build_info_value,
    commit,
    tree,
    manifest_value,
    checksums_value,
) = sys.argv[1:]
root = Path(root_value)
output = Path(output_value)
atlas = Path(atlas_value)
manifest_path = Path(manifest_value)
checksums_path = Path(checksums_value)
weight_suffixes = {
    ".bin", ".ckpt", ".ggml", ".gguf", ".h5", ".hdf5", ".model",
    ".npy", ".npz", ".onnx", ".pt", ".pth", ".safetensors", ".tflite",
    ".weights",
}

expected_outer = {
    ".rkc-self-catalogue.json", "atlas", "MANIFEST.json", "SHA256SUMS.txt"
}
for child in output.iterdir():
    info = os.lstat(child)
    if child.name not in expected_outer or stat.S_ISLNK(info.st_mode):
        raise SystemExit(f"self-catalogue manifest: unexpected outer output: {child.name}")
    if child.name == "atlas" and not stat.S_ISDIR(info.st_mode):
        raise SystemExit("self-catalogue manifest: atlas is not a real directory")
    if child.name != "atlas" and not stat.S_ISREG(info.st_mode):
        raise SystemExit(f"self-catalogue manifest: outer receipt is not regular: {child.name}")


def strict_json(path: Path) -> dict:
    def pairs(items):
        value = {}
        for key, item in items:
            if key in value:
                raise SystemExit(f"self-catalogue manifest: duplicate JSON key {key!r} in {path.name}")
            value[key] = item
        return value

    value = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=pairs)
    if not isinstance(value, dict):
        raise SystemExit(f"self-catalogue manifest: {path.name} is not an object")
    return value


def stable_file(path: Path) -> tuple[bytes, os.stat_result]:
    initial = os.lstat(path)
    if stat.S_ISLNK(initial.st_mode) or not stat.S_ISREG(initial.st_mode):
        raise SystemExit(f"self-catalogue manifest: non-regular file: {path}")
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "rb") as handle:
        before = os.fstat(handle.fileno())
        data = handle.read()
        after = os.fstat(handle.fileno())
    identity = lambda item: (item.st_dev, item.st_ino, item.st_size, item.st_mtime_ns)
    if identity(initial) != identity(before) or identity(before) != identity(after):
        raise SystemExit(f"self-catalogue manifest: file changed while hashing: {path}")
    return data, after


for required in ("graph", "docs", "search"):
    path = atlas / required
    info = os.lstat(path)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise SystemExit(f"self-catalogue manifest: required {required} output is not a real directory")
    if not any(candidate.is_file() and not candidate.is_symlink() for candidate in path.rglob("*")):
        raise SystemExit(f"self-catalogue manifest: required {required} output is empty")
for required in ("bundle.json", "coverage.json", "rkc.manifest.json", "rkc-export-manifest.json"):
    stable_file(atlas / required)

observed = {}
for current, directories, files in os.walk(atlas, topdown=True, followlinks=False):
    directories.sort()
    files.sort()
    current_path = Path(current)
    for directory in directories:
        path = current_path / directory
        info = os.lstat(path)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise SystemExit(f"self-catalogue manifest: output directory link/special file: {path}")
    for name in files:
        path = current_path / name
        relative = path.relative_to(output).as_posix()
        if path.suffix.lower() in weight_suffixes:
            raise SystemExit(f"self-catalogue manifest: model-weight suffix in output: {relative}")
        data, info = stable_file(path)
        if data.startswith((b"GGUF", b"GGML")):
            raise SystemExit(f"self-catalogue manifest: model-weight magic in output: {relative}")
        observed[relative] = {
            "path": relative,
            "sha256": hashlib.sha256(data).hexdigest(),
            "size_bytes": info.st_size,
        }
if not observed:
    raise SystemExit("self-catalogue manifest: atlas contains no files")

source_manifest = strict_json(Path(source_manifest_value))
scan = strict_json(Path(scan_value))
check = strict_json(Path(check_value))
coverage = strict_json(atlas / "coverage.json")
export_manifest = strict_json(atlas / "rkc-export-manifest.json")
if scan.get("snapshot_id") != coverage.get("snapshot_id"):
    raise SystemExit("self-catalogue manifest: scan and coverage snapshot identities differ")
if scan.get("deterministic_digest") != coverage.get("deterministic_output_digest"):
    raise SystemExit("self-catalogue manifest: scan and coverage digests differ")
if check.get("passed") is not True:
    raise SystemExit("self-catalogue manifest: quality gate did not pass")
export_files = export_manifest.get("files")
if not isinstance(export_files, list) or not export_files:
    raise SystemExit("self-catalogue manifest: RKC export manifest has no files")
declared = set()
records = []
noncanonical = []
for item in export_files:
    if not isinstance(item, dict) or not isinstance(item.get("path"), str):
        raise SystemExit("self-catalogue manifest: malformed RKC export file record")
    relative = "atlas/" + item["path"]
    if relative in declared or relative not in observed:
        raise SystemExit(f"self-catalogue manifest: duplicate or missing RKC export: {relative}")
    declared.add(relative)
    actual = observed[relative]
    if item.get("sha256") != actual["sha256"] or item.get("size_bytes") != actual["size_bytes"]:
        raise SystemExit(f"self-catalogue manifest: RKC export digest mismatch: {relative}")
    if item.get("canonical") is True:
        records.append(actual)
    elif item.get("canonical") is False:
        noncanonical.append(relative)
    else:
        raise SystemExit(f"self-catalogue manifest: missing canonical classification: {relative}")
if noncanonical != ["atlas/rkc.execution.json"]:
    raise SystemExit(f"self-catalogue manifest: unexpected noncanonical outputs: {noncanonical}")
allowed_unlisted = {"atlas/.rkc-generated.json", "atlas/rkc-export-manifest.json"}
if set(observed) - declared != allowed_unlisted:
    raise SystemExit(
        "self-catalogue manifest: unlisted atlas outputs differ from ownership/export manifests"
    )
# RKC's deterministic ownership marker is part of the outer canonical receipt.
records.append(observed["atlas/.rkc-generated.json"])
records.sort(key=lambda item: item["path"])
binary, _binary_info = stable_file(Path(binary_value))
build_info = stable_file(Path(build_info_value))[0].decode("utf-8")
build_metadata = [line for line in build_info.splitlines() if line.startswith("\t")]
reported_revision = next(
    (
        line.removeprefix("\tbuild\tvcs.revision=")
        for line in build_metadata
        if line.startswith("\tbuild\tvcs.revision=")
    ),
    None,
)
if reported_revision not in {None, commit}:
    raise SystemExit("self-catalogue manifest: staged binary reports the wrong VCS revision")
if source_manifest.get("selection") != "recorded-commit-tree-regular-blobs":
    raise SystemExit("self-catalogue manifest: source selection is not commit-tree bound")
source_identity = source_manifest.get("source", {})
if source_identity.get("commit") != commit or source_identity.get("tree") != tree:
    raise SystemExit("self-catalogue manifest: source receipt identity mismatch")

manifest = {
    "schema_version": "1.0.0",
    "kind": "rkc-self-catalogue",
    "source": {
        "commit": commit,
        "tree": tree,
        "selection_manifest": source_manifest,
    },
    "tool": {
        "build_provenance": {
            "binary_retained": False,
            "go_version_metadata": build_metadata,
            "source": "exact detached recorded commit tree",
            "vcs_metadata": (
                "commit-bound" if reported_revision else "absent-by-design-detached-tree"
            ),
        },
        "path": "ephemeral-staged-source/bin/rkc",
        "sha256": hashlib.sha256(binary).hexdigest(),
    },
    "atlas": {
        "deterministic_output_digest": coverage["deterministic_output_digest"],
        "canonical_files_digest": export_manifest["canonical_files_digest"],
        "file_count": len(records),
        "files": records,
        "operational_files_excluded_from_deterministic_hashes": [
            "atlas/rkc-export-manifest.json",
            "atlas/rkc.execution.json",
        ],
        "required_products": ["docs", "graph", "search"],
        "snapshot_id": coverage["snapshot_id"],
        "total_bytes": sum(item["size_bytes"] for item in records),
    },
    "safety": {
        "generated_output_ingested": False,
        "model_execution": False,
        "model_weights_ingested_or_emitted": False,
        "source_and_output_disjoint": True,
        "symlinks_accepted": False,
    },
}
encoded = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode()
with manifest_path.open("xb") as handle:
    handle.write(encoded)
    handle.flush()
    os.fchmod(handle.fileno(), 0o644)
    os.fsync(handle.fileno())

checksum_records = [
    {
        "path": ".rkc-self-catalogue.json",
        "sha256": hashlib.sha256(stable_file(output / ".rkc-self-catalogue.json")[0]).hexdigest(),
    },
    {"path": "MANIFEST.json", "sha256": hashlib.sha256(encoded).hexdigest()},
    *({"path": item["path"], "sha256": item["sha256"]} for item in records),
]
checksum_records.sort(key=lambda item: item["path"])
with checksums_path.open("x", encoding="utf-8") as handle:
    for item in checksum_records:
        handle.write(f"{item['sha256']}  {item['path']}\n")
    handle.flush()
    os.fchmod(handle.fileno(), 0o644)
    os.fsync(handle.fileno())
PY

verify_staged_source

(
  cd "$OUT"
  sha256sum --check --strict SHA256SUMS.txt >/dev/null
)

# Require a complete, link-free staging tree and durably flush every inode before
# the publication point. No byte in dist/self-catalogue has been touched yet.
"$PYTHON" -I -S - "$OUT" <<'PY'
from __future__ import annotations

import os
import stat
import sys
from pathlib import Path

output = Path(sys.argv[1])
expected = {".rkc-self-catalogue.json", "atlas", "MANIFEST.json", "SHA256SUMS.txt"}
if {item.name for item in output.iterdir()} != expected:
    raise SystemExit("self-catalogue publication: staging is not a complete exact output")
directories = []
for current, names, files in os.walk(output, topdown=True, followlinks=False):
    names.sort()
    files.sort()
    base = Path(current)
    directories.append(base)
    for name in names:
        info = os.lstat(base / name)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise SystemExit(f"self-catalogue publication: invalid directory: {base / name}")
    for name in files:
        path = base / name
        info = os.lstat(path)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise SystemExit(f"self-catalogue publication: invalid file: {path}")
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
for directory in reversed(directories):
    descriptor = os.open(directory, flags)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
PY

# Abort rather than publishing stale evidence if any concurrent process changed
# HEAD, its tree, the index, a tracked file, or an untracked source path.
git status --porcelain=v1 -z --untracked-files=all --ignore-submodules=none >"$FINAL_STATUS"
if [[ -s "$FINAL_STATUS" ]]; then
  echo "self-catalogue: Git state changed during generation; preserving the last-known-good output" >&2
  exit 1
fi
if [[ $(git rev-parse HEAD) != "$COMMIT" || $(git rev-parse 'HEAD^{tree}') != "$TREE" ]]; then
  echo "self-catalogue: HEAD or its tree changed during generation; publication refused" >&2
  exit 1
fi

# Linux renameat2(RENAME_EXCHANGE) gives an atomic whole-directory replacement
# when a last-known-good catalogue exists. Failures are rolled back; a replaced
# verified catalogue is retained under a marker-bound quarantine name.
"$PYTHON" -I -S - "$ROOT" "$STAGING" "$COMMIT" <<'PY'
from __future__ import annotations

import ctypes
import errno
import json
import os
import secrets
import stat
import sys
from pathlib import Path

root = Path(sys.argv[1])
stage = Path(sys.argv[2])
commit = sys.argv[3]
dist = root / "dist"
final_name = "self-catalogue"
marker_name = ".rkc-self-catalogue.json"
expected_entries = {marker_name, "atlas", "MANIFEST.json", "SHA256SUMS.txt"}
marker = (
    json.dumps(
        {"kind": "rkc-self-catalogue", "producer": "rkc", "schema_version": "1.0.0"},
        indent=2,
        sort_keys=True,
    )
    + "\n"
).encode()

if not sys.platform.startswith("linux"):
    raise SystemExit("self-catalogue publication: atomic directory exchange requires Linux")
if stage.parent != dist or not stage.name.startswith(".rkc-self-catalogue-build-"):
    raise SystemExit("self-catalogue publication: invalid staging identity")

directory_flags = (
    os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
)
dist_fd = os.open(dist, directory_flags)


def inspect_owned(name: str, *, private: bool) -> tuple[int, int]:
    info = os.stat(name, dir_fd=dist_fd, follow_symlinks=False)
    forbidden = 0o077 if private else 0o022
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & forbidden:
        raise SystemExit(f"self-catalogue publication: unsafe owned directory: {name}")
    descriptor = os.open(name, directory_flags, dir_fd=dist_fd)
    try:
        if set(os.listdir(descriptor)) != expected_entries:
            raise SystemExit(f"self-catalogue publication: incomplete owned directory: {name}")
        marker_fd = os.open(
            marker_name,
            os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0),
            dir_fd=descriptor,
        )
        with os.fdopen(marker_fd, "rb") as handle:
            if handle.read(len(marker) + 1) != marker:
                raise SystemExit(f"self-catalogue publication: ownership marker mismatch: {name}")
    finally:
        os.close(descriptor)
    return (info.st_dev, info.st_ino)


libc = ctypes.CDLL(None, use_errno=True)
renameat2 = getattr(libc, "renameat2", None)
if renameat2 is None:
    raise SystemExit("self-catalogue publication: libc renameat2 is unavailable")
renameat2.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
renameat2.restype = ctypes.c_int


def exchange(left: str, right: str) -> None:
    if renameat2(dist_fd, os.fsencode(left), dist_fd, os.fsencode(right), 2) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))


def no_replace(left: str, right: str) -> None:
    if renameat2(dist_fd, os.fsencode(left), dist_fd, os.fsencode(right), 1) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))


def rollback_exchange(stage_name: str) -> None:
    try:
        exchange(stage_name, final_name)
        os.fsync(dist_fd)
    except OSError as exc:
        raise SystemExit(
            "self-catalogue publication: FATAL atomic rollback failed; inspect both owned directories: "
            + str(exc)
        ) from exc


try:
    stage_identity = inspect_owned(stage.name, private=True)
    try:
        os.stat(final_name, dir_fd=dist_fd, follow_symlinks=False)
    except FileNotFoundError:
        moved = False
        try:
            no_replace(stage.name, final_name)
            moved = True
            os.fsync(dist_fd)
        except OSError as exc:
            if moved:
                final_info = os.stat(final_name, dir_fd=dist_fd, follow_symlinks=False)
                if (final_info.st_dev, final_info.st_ino) == stage_identity:
                    os.rename(final_name, stage.name, src_dir_fd=dist_fd, dst_dir_fd=dist_fd)
                    os.fsync(dist_fd)
            raise SystemExit(f"self-catalogue publication: atomic initial publish failed: {exc}") from exc
        print("self-catalogue: atomically published first verified catalogue")
    else:
        inspect_owned(final_name, private=False)
        exchanged = False
        try:
            exchange(stage.name, final_name)
            exchanged = True
            os.fsync(dist_fd)
        except OSError as exc:
            if exchanged:
                rollback_exchange(stage.name)
            raise SystemExit(f"self-catalogue publication: atomic exchange failed: {exc}") from exc

        quarantine = ""
        try:
            for _attempt in range(128):
                candidate = (
                    f".rkc-self-catalogue-quarantine-{commit[:12]}-{secrets.token_hex(6)}"
                )
                try:
                    os.stat(candidate, dir_fd=dist_fd, follow_symlinks=False)
                except FileNotFoundError:
                    quarantine = candidate
                    break
            if not quarantine:
                raise OSError(errno.EEXIST, "cannot allocate quarantine name")
            os.rename(stage.name, quarantine, src_dir_fd=dist_fd, dst_dir_fd=dist_fd)
            try:
                os.fsync(dist_fd)
            except OSError:
                os.rename(quarantine, stage.name, src_dir_fd=dist_fd, dst_dir_fd=dist_fd)
                raise
        except OSError as exc:
            rollback_exchange(stage.name)
            raise SystemExit(
                f"self-catalogue publication: quarantine failed; publication rolled back: {exc}"
            ) from exc
        print(f"self-catalogue: prior verified catalogue quarantined at dist/{quarantine}")
finally:
    os.close(dist_fd)
PY

STAGING=
FINAL_OUT="$ROOT/dist/self-catalogue"
echo "self-catalogue: verified and atomically published atlas, graph, search, docs, manifest, and hashes at $FINAL_OUT"
