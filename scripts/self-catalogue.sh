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

for required in git go make python3 sha256sum; do
  if ! command -v "$required" >/dev/null 2>&1; then
    echo "self-catalogue: required command not found: $required" >&2
    exit 1
  fi
done

PYTHON=python3
if [[ -x .venv/bin/python ]]; then
  PYTHON=.venv/bin/python
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-self-catalogue.XXXXXX")
SOURCE="$WORK/RKC"
SOURCE_MANIFEST="$WORK/source-manifest.json"
STATUS="$WORK/git-status"
BUILD_INFO="$WORK/rkc-build-info"
MANIFEST="$WORK/MANIFEST.json"
CHECKSUMS="$WORK/SHA256SUMS.txt"
trap 'rm -rf -- "$WORK"' EXIT INT TERM

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

git status --porcelain=v1 -z --untracked-files=all --ignore-submodules=none >"$STATUS"
if [[ -s "$STATUS" ]]; then
  echo "self-catalogue: Git worktree is dirty; commit or remove all changes first" >&2
  "$PYTHON" - "$STATUS" <<'PY' >&2
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

mkdir -m 0700 "$SOURCE"
"$PYTHON" - "$ROOT" "$SOURCE" "$SOURCE_MANIFEST" "$COMMIT" "$TREE" "$OBJECT_FORMAT" "${EXCLUSIONS[@]}" <<'PY'
from __future__ import annotations

import hashlib
import json
import os
import re
import stat
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


result = subprocess.run(
    ["git", "ls-files", "--cached", "--stage", "-z"],
    cwd=root,
    check=False,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
)
if result.returncode != 0:
    fail("cannot enumerate the stage-zero Git index: " + result.stderr.decode(errors="replace"))

records = []
excluded_tracked = []
seen = set()
for raw in result.stdout.split(b"\0"):
    if not raw:
        continue
    try:
        header, raw_name = raw.split(b"\t", 1)
        mode, object_id, stage = header.split(b" ", 2)
        name = raw_name.decode("utf-8")
        expected_object = object_id.decode("ascii")
    except (UnicodeDecodeError, ValueError) as exc:
        fail(f"malformed or non-UTF-8 Git index entry: {exc}")
    path = safe_path(name)
    if stage != b"0":
        fail(f"unmerged Git index entry: {name}")
    if mode in {b"120000", b"160000"}:
        fail(f"tracked symlink or submodule is prohibited: {name}")
    if mode not in {b"100644", b"100755"}:
        fail(f"unsupported tracked mode {mode!r}: {name}")
    if name in seen:
        fail(f"duplicate Git index path: {name}")
    seen.add(name)
    if is_excluded(path):
        excluded_tracked.append(name)
        continue
    if path.name == ".rkc-generated.json":
        fail(f"tracked RKC-generated ownership marker is prohibited: {name}")
    if path.suffix.lower() in weight_suffixes:
        fail(f"tracked model-weight/native tensor suffix is prohibited: {name}")

    cursor = root
    for component in path.parts[:-1]:
        cursor /= component
        info = os.lstat(cursor)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            fail(f"source parent traverses a link or special file: {name}")
    source = root.joinpath(*path.parts)
    initial = os.lstat(source)
    if stat.S_ISLNK(initial.st_mode) or not stat.S_ISREG(initial.st_mode):
        fail(f"tracked source is not a regular file: {name}")
    if initial.st_size > maximum_bytes:
        fail(f"tracked source exceeds the 64 MiB self-scan ceiling: {name}")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(source, flags)
    with os.fdopen(descriptor, "rb") as handle:
        before = os.fstat(handle.fileno())
        payload = handle.read(maximum_bytes + 1)
        after = os.fstat(handle.fileno())
    identity = lambda item: (
        item.st_dev, item.st_ino, item.st_size, item.st_mtime_ns, item.st_ctime_ns
    )
    if identity(initial) != identity(before) or identity(before) != identity(after):
        fail(f"tracked source changed while read: {name}")
    if len(payload) > maximum_bytes:
        fail(f"tracked source grew beyond the 64 MiB ceiling: {name}")
    if payload.startswith(weight_magic) or payload.startswith(
        b"version https://git-lfs.github.com/spec/v1"
    ):
        fail(f"tracked model/binary pointer content is prohibited: {name}")
    object_hash = hashlib.new(object_format)
    object_hash.update(f"blob {len(payload)}\0".encode())
    object_hash.update(payload)
    if object_hash.hexdigest() != expected_object:
        fail(f"working source differs from the Git index object: {name}")

    destination = target.joinpath(*path.parts)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    output_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    output = os.open(destination, output_flags, 0o755 if mode == b"100755" else 0o644)
    with os.fdopen(output, "wb") as handle:
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
    "selection": "git-stage-zero-regular-files",
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

# The build is deliberately inside the same subordinate cgroup as the scan.
make build
RKC_BIN="$ROOT/bin/rkc"
if [[ ! -f $RKC_BIN || ! -x $RKC_BIN || -L $RKC_BIN ]]; then
  echo "self-catalogue: bin/rkc must be a real executable" >&2
  exit 1
fi
go version -m "$RKC_BIN" >"$BUILD_INFO"
if ! grep -Fq $'\tbuild\tvcs.revision='"$COMMIT" "$BUILD_INFO"; then
  echo "self-catalogue: bin/rkc is not bound to the source commit" >&2
  exit 1
fi
if ! grep -Fq $'\tbuild\tvcs.modified=false' "$BUILD_INFO"; then
  echo "self-catalogue: bin/rkc build metadata is dirty" >&2
  exit 1
fi
if ! grep -Fq $'\tbuild\t-trimpath=true' "$BUILD_INFO"; then
  echo "self-catalogue: bin/rkc was not built with -trimpath" >&2
  exit 1
fi

OUT="$ROOT/dist/self-catalogue"
ATLAS="$OUT/atlas"
"$PYTHON" - "$ROOT" "$OUT" <<'PY'
from __future__ import annotations

import json
import os
import stat
import sys
from pathlib import Path

root = Path(sys.argv[1])
output = Path(sys.argv[2])
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
dist_info = os.lstat(dist)
if stat.S_ISLNK(dist_info.st_mode) or not stat.S_ISDIR(dist_info.st_mode):
    raise SystemExit("self-catalogue output: dist is not a real directory")
if not os.path.lexists(output):
    output.mkdir(mode=0o700)
    descriptor = os.open(
        output / marker_name,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
        0o600,
    )
    with os.fdopen(descriptor, "wb") as handle:
        handle.write(marker)
else:
    info = os.lstat(output)
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise SystemExit("self-catalogue output: target is not a real directory")
    if info.st_uid != os.getuid() or info.st_mode & 0o022:
        raise SystemExit("self-catalogue output: target is not private and owner-controlled")
    marker_path = output / marker_name
    marker_info = os.lstat(marker_path)
    if stat.S_ISLNK(marker_info.st_mode) or not stat.S_ISREG(marker_info.st_mode):
        raise SystemExit("self-catalogue output: ownership marker is not regular")
    if marker_path.read_bytes() != marker:
        raise SystemExit("self-catalogue output: ownership marker mismatch")

allowed = {marker_name, "atlas", "MANIFEST.json", "SHA256SUMS.txt"}
for child in output.iterdir():
    info = os.lstat(child)
    if stat.S_ISLNK(info.st_mode):
        raise SystemExit(f"self-catalogue output: symlink is prohibited: {child.name}")
    if child.name in allowed:
        continue
    if child.name.startswith((".rkc-build-", ".rkc-quarantine-")) and stat.S_ISDIR(info.st_mode):
        continue
    raise SystemExit(f"self-catalogue output: unexpected entry: {child.name}")
PY

SCAN_ARGS=(
  scan
  --json
  --fail-on-errors
  --out "$ATLAS"
  --force
  # Keep the canonical toolchain identity independent of checkout location.
  --python python3
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

"$PYTHON" - \
  "$ROOT" "$OUT" "$ATLAS" "$SOURCE_MANIFEST" "$WORK/scan.json" "$WORK/check.json" \
  "$RKC_BIN" "$COMMIT" "$TREE" "$MANIFEST" "$CHECKSUMS" <<'PY'
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

manifest = {
    "schema_version": "1.0.0",
    "kind": "rkc-self-catalogue",
    "source": {
        "commit": commit,
        "tree": tree,
        "selection_manifest": source_manifest,
    },
    "tool": {
        "path": "bin/rkc",
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
PY

"$PYTHON" scripts/publish_file.py \
  --source "$MANIFEST" \
  --destination "$OUT/MANIFEST.json" \
  --mode 0644
"$PYTHON" scripts/publish_file.py \
  --source "$CHECKSUMS" \
  --destination "$OUT/SHA256SUMS.txt" \
  --mode 0644

(
  cd "$OUT"
  sha256sum --check --strict SHA256SUMS.txt >/dev/null
)

echo "self-catalogue: verified atlas, graph, search, docs, manifest, and hashes at $OUT"
