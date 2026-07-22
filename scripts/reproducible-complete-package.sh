#!/bin/sh
# Build the complete package twice from independent immutable checkouts and
# publish only a byte-identical result plus the first verified artifact set.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
EXPECTED_STEPS='go-modules python-environment format vet coverage contracts docs licenses model-lock build plugins smoke reproducibility api-smoke mcp-smoke git-smoke race benchmark'
EVIDENCE_ROOT=$ROOT/dist/evidence
FINAL=$ROOT/dist/release/repository-knowledge-compiler-complete.zip

if [ -L dist ] || [ ! -d dist ]; then
  echo "complete package: dist must be a real directory" >&2
  exit 1
fi
if [ -L "$EVIDENCE_ROOT" ] || [ ! -d "$EVIDENCE_ROOT" ]; then
  echo "complete package: verified evidence generation is missing or unsafe" >&2
  exit 1
fi
python3 scripts/git_source_guard.py \
  --root "$ROOT" \
  --operation "complete package assembly"
SOURCE_COMMIT=$(git rev-parse --verify 'HEAD^{commit}')
SOURCE_TREE=$(git rev-parse --verify "${SOURCE_COMMIT}^{tree}")
SOURCE_COMMIT_TIME=$(git show -s --format=%ct "$SOURCE_COMMIT")
case "$SOURCE_COMMIT:$SOURCE_TREE:$SOURCE_COMMIT_TIME" in
  *[!0-9a-f:]*|*::*|:*)
    echo "complete package: invalid Git source identity" >&2
    exit 1
    ;;
esac

WORK=$(mktemp -d "$ROOT/dist/.rkc-complete-reproducibility.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
TOOLS_SOURCE=$WORK/tool-source
git clone --quiet --no-hardlinks --no-checkout -- "$ROOT" "$TOOLS_SOURCE"
git -C "$TOOLS_SOURCE" -c advice.detachedHead=false checkout --quiet --detach "$SOURCE_COMMIT"
if [ "$(git -C "$TOOLS_SOURCE" rev-parse 'HEAD^{tree}')" != "$SOURCE_TREE" ] ||
   [ -n "$(git -C "$TOOLS_SOURCE" status --porcelain=v1 --untracked-files=all)" ]; then
  echo "complete package: immutable publication helpers do not match source" >&2
  exit 1
fi

validate_validation_inventory() {
  directory=$1
  python3 - "$directory" "$EXPECTED_STEPS" <<'PY'
import os
import stat
import sys
from pathlib import Path

root = Path(sys.argv[1])
root_info = os.lstat(root)
if stat.S_ISLNK(root_info.st_mode) or not stat.S_ISDIR(root_info.st_mode):
    raise SystemExit("complete package: validation evidence root is unsafe")
expected = {"summary.json", "steps.tsv"}
expected.update(f"logs/{name}.log" for name in sys.argv[2].split())
observed = set()
directories = set()
for current, names, files in os.walk(root, topdown=True, followlinks=False):
    relative_root = Path(current).relative_to(root)
    for name in names:
        path = Path(current) / name
        info = os.lstat(path)
        relative = (relative_root / name).as_posix()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise SystemExit(f"complete package: unsafe evidence directory: {path}")
        directories.add(relative)
    for name in files:
        path = Path(current) / name
        info = os.lstat(path)
        relative = (relative_root / name).as_posix()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise SystemExit(f"complete package: unsafe evidence file: {path}")
        observed.add(relative)
if directories != {"logs"} or observed != expected:
    raise SystemExit("complete package: validation evidence inventory differs from policy")
PY
}

validate_benchmark_inventory() {
  directory=$1
  python3 - "$directory" <<'PY'
import os
import stat
import sys
from pathlib import Path

root = Path(sys.argv[1])
root_info = os.lstat(root)
if stat.S_ISLNK(root_info.st_mode) or not stat.S_ISDIR(root_info.st_mode):
    raise SystemExit("complete package: benchmark evidence root is unsafe")
expected = {"result.json", "time.txt", "scan.stdout"}
observed = set()
for current, directories, files in os.walk(root, topdown=True, followlinks=False):
    if Path(current) != root or directories:
        raise SystemExit("complete package: benchmark evidence directories are prohibited")
    for name in files:
        path = Path(current) / name
        info = os.lstat(path)
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise SystemExit(f"complete package: unsafe benchmark evidence file: {path}")
        observed.add(name)
if observed != expected:
    raise SystemExit("complete package: benchmark evidence inventory differs from policy")
PY
}

snapshot_release_evidence() {
  snapshot=$WORK/evidence
  validate_validation_inventory "$EVIDENCE_ROOT/validation"
  validate_benchmark_inventory "$EVIDENCE_ROOT/benchmark"
  mkdir -p "$snapshot/validation/logs" "$snapshot/benchmark"
  for relative in summary.json steps.tsv; do
    python3 "$TOOLS_SOURCE/scripts/publish_file.py" \
      --source "$EVIDENCE_ROOT/validation/$relative" \
      --destination "$snapshot/validation/$relative" \
      --repository-root "$ROOT" \
      --mode 0644
  done
  for name in $EXPECTED_STEPS; do
    relative="logs/$name.log"
    python3 "$TOOLS_SOURCE/scripts/publish_file.py" \
      --source "$EVIDENCE_ROOT/validation/$relative" \
      --destination "$snapshot/validation/$relative" \
      --repository-root "$ROOT" \
      --mode 0644
  done
  for name in result.json time.txt scan.stdout; do
    python3 "$TOOLS_SOURCE/scripts/publish_file.py" \
      --source "$EVIDENCE_ROOT/benchmark/$name" \
      --destination "$snapshot/benchmark/$name" \
      --repository-root "$ROOT" \
      --mode 0644
  done
}

require_unchanged_release_inputs() {
  if [ "$(git rev-parse --verify 'HEAD^{commit}')" != "$SOURCE_COMMIT" ] ||
     [ "$(git rev-parse --verify 'HEAD^{tree}')" != "$SOURCE_TREE" ]; then
    echo "complete package: root HEAD or tree changed during reproducibility work" >&2
    return 1
  fi
  validate_validation_inventory "$EVIDENCE_ROOT/validation"
  validate_benchmark_inventory "$EVIDENCE_ROOT/benchmark"
  for relative in summary.json steps.tsv; do
    if [ ! -f "$EVIDENCE_ROOT/validation/$relative" ] ||
       [ -L "$EVIDENCE_ROOT/validation/$relative" ] ||
       ! cmp "$WORK/evidence/validation/$relative" "$EVIDENCE_ROOT/validation/$relative"; then
      echo "complete package: validation evidence changed: $relative" >&2
      return 1
    fi
  done
  for name in $EXPECTED_STEPS; do
    relative="logs/$name.log"
    if [ ! -f "$EVIDENCE_ROOT/validation/$relative" ] ||
       [ -L "$EVIDENCE_ROOT/validation/$relative" ] ||
       ! cmp "$WORK/evidence/validation/$relative" "$EVIDENCE_ROOT/validation/$relative"; then
      echo "complete package: validation evidence changed: $relative" >&2
      return 1
    fi
  done
  for name in result.json time.txt scan.stdout; do
    if [ ! -f "$EVIDENCE_ROOT/benchmark/$name" ] ||
       [ -L "$EVIDENCE_ROOT/benchmark/$name" ] ||
       ! cmp "$WORK/evidence/benchmark/$name" "$EVIDENCE_ROOT/benchmark/$name"; then
      echo "complete package: benchmark evidence changed: $name" >&2
      return 1
    fi
  done
}

copy_release_evidence() {
  destination_root=$1
  mkdir -p \
    "$destination_root/dist/evidence/validation/logs" \
    "$destination_root/dist/evidence/benchmark"
  for relative in summary.json steps.tsv; do
    python3 "$destination_root/scripts/publish_file.py" \
      --source "$WORK/evidence/validation/$relative" \
      --destination "$destination_root/dist/evidence/validation/$relative" \
      --mode 0644
  done
  for name in $EXPECTED_STEPS; do
    relative="logs/$name.log"
    python3 "$destination_root/scripts/publish_file.py" \
      --source "$WORK/evidence/validation/$relative" \
      --destination "$destination_root/dist/evidence/validation/$relative" \
      --mode 0644
  done
  for name in result.json time.txt scan.stdout; do
    python3 "$destination_root/scripts/publish_file.py" \
      --source "$WORK/evidence/benchmark/$name" \
      --destination "$destination_root/dist/evidence/benchmark/$name" \
      --mode 0644
  done
}

build_lane() {
  lane=$1
  source=$WORK/$lane/source
  lane_go_cache=$WORK/$lane/go-build-cache
  lane_module_cache=$WORK/$lane/go-module-cache
  mkdir -p "$WORK/$lane" "$lane_go_cache" "$lane_module_cache"
  git clone --quiet --no-hardlinks --no-checkout -- "$ROOT" "$source"
  git -C "$source" -c advice.detachedHead=false checkout --quiet --detach "$SOURCE_COMMIT"
  git -C "$source" remote set-url origin https://github.com/neuroforge-io/RKC.git
  if [ "$(git -C "$source" rev-parse HEAD)" != "$SOURCE_COMMIT" ] ||
     [ "$(git -C "$source" rev-parse 'HEAD^{tree}')" != "$SOURCE_TREE" ] ||
     [ -n "$(git -C "$source" status --porcelain=v1 --untracked-files=all)" ]; then
    echo "complete package: lane $lane is not a clean immutable checkout" >&2
    exit 1
  fi
  copy_release_evidence "$source"
  (
    cd "$source"
    export GOCACHE="$lane_go_cache"
    export GOMODCACHE="$lane_module_cache"
    sh scripts/generate-demo.sh
    sh scripts/build-release-binaries.sh
    python3 scripts/package-complete.py \
      --output dist/repository-knowledge-compiler-complete.zip \
      --force
  )
  if [ "$(git -C "$source" rev-parse HEAD)" != "$SOURCE_COMMIT" ] ||
     [ "$(git -C "$source" rev-parse 'HEAD^{tree}')" != "$SOURCE_TREE" ] ||
     [ -n "$(git -C "$source" status --porcelain=v1 --untracked-files=all)" ]; then
    echo "complete package: lane $lane changed its immutable checkout" >&2
    exit 1
  fi
}

# Both lanes rebuild the binaries, SPDX documents, and deterministic demo from
# separate detached source checkouts. They share only the exact raw validation
# and benchmark evidence from the immediately preceding immutable verification
# run.
python3 "$TOOLS_SOURCE/scripts/git_source_guard.py" \
  --root "$ROOT" \
  --operation "complete package evidence snapshot"
snapshot_release_evidence
build_lane a
build_lane b
cmp \
  "$WORK/a/source/dist/repository-knowledge-compiler-complete.zip" \
  "$WORK/b/source/dist/repository-knowledge-compiler-complete.zip"

# The authoritative output is one complete generation. It contains the exact
# raw evidence snapshot referenced by the ZIP receipt, so a later verification
# cannot detach the uploaded evidence from the release that names its hashes.
require_unchanged_release_inputs
RELEASE_STAGE=$WORK/release
mkdir "$RELEASE_STAGE"
mv "$WORK/a/source/dist/demo" "$RELEASE_STAGE/demo"
mv "$WORK/a/source/dist/binaries" "$RELEASE_STAGE/binaries"
for name in demo-scan.txt demo-check.txt demo-synthesis.txt; do
  mv "$WORK/a/source/dist/$name" "$RELEASE_STAGE/$name"
done
mv \
  "$WORK/a/source/dist/repository-knowledge-compiler-complete.zip" \
  "$RELEASE_STAGE/repository-knowledge-compiler-complete.zip"
mv "$WORK/evidence" "$RELEASE_STAGE/evidence"
python3 "$TOOLS_SOURCE/scripts/git_source_guard.py" \
  --root "$ROOT" \
  --operation "complete package publication"
python3 "$TOOLS_SOURCE/scripts/publish_directory.py" \
  --source "$RELEASE_STAGE" \
  --destination "$ROOT/dist/release" \
  --repository-root "$ROOT"

echo "complete package: two cache-isolated immutable builds are byte-identical: $FINAL"
