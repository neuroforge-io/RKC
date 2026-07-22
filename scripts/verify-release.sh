#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
EXPECTED_STEPS='go-modules python-environment format vet coverage contracts docs licenses model-lock build plugins smoke reproducibility api-smoke mcp-smoke git-smoke race benchmark'

prepare_validation_output() {
  output=$1
  if [ -L "$(dirname "$output")" ] || { [ -e "$(dirname "$output")" ] && [ ! -d "$(dirname "$output")" ]; }; then
    echo "release verification: output parent must be a real directory" >&2
    exit 1
  fi
  mkdir -p "$(dirname "$output")"
  if [ -L "$output" ] || { [ -e "$output" ] && [ ! -d "$output" ]; }; then
    echo "release verification: $output must be a real directory, not a symlink or non-directory" >&2
    exit 1
  fi
  if [ -L "$output/logs" ] || { [ -e "$output/logs" ] && [ ! -d "$output/logs" ]; }; then
    echo "release verification: $output/logs must be a real directory, not a symlink or non-directory" >&2
    exit 1
  fi
  if [ -d "$output" ]; then
    python3 - "$output" "$EXPECTED_STEPS" <<'PY'
import os
import stat
import sys
from pathlib import Path

root = Path(sys.argv[1])
steps = sys.argv[2].split()
# The legacy names are accepted only for one-time safe cleanup. Package
# validation accepts solely the current exact inventory.
allowed = {"summary.json", "steps.tsv"}
allowed.update(f"logs/{name}.log" for name in steps)
allowed.update({"logs/go-tests.log", "logs/python-tests.log"})
for current, directories, files in os.walk(root, topdown=True, followlinks=False):
    relative_root = Path(current).relative_to(root)
    for directory in directories:
        path = Path(current) / directory
        info = os.lstat(path)
        relative = (relative_root / directory).as_posix()
        if relative != "logs" or stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise SystemExit(f"release verification: unexpected existing path: {path}")
    for filename in files:
        path = Path(current) / filename
        info = os.lstat(path)
        relative = (relative_root / filename).as_posix()
        if relative not in allowed or stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise SystemExit(f"release verification: unexpected existing file: {path}")
PY
    for name in $EXPECTED_STEPS go-tests python-tests; do
      rm -f "$output/logs/$name.log"
    done
    rm -f "$output/steps.tsv" "$output/summary.json"
    rmdir "$output/logs" 2>/dev/null || true
    rmdir "$output" 2>/dev/null || true
  fi
  mkdir -p "$output/logs"
}

stage_validation_output() {
  source=$1
  destination=$2
  if [ ! -d "$source" ] || [ -L "$source" ]; then
    echo "release verification: successful validation evidence is missing or unsafe" >&2
    return 1
  fi
  if [ -e "$destination" ] || [ -L "$destination" ]; then
    echo "release verification: private validation staging target already exists" >&2
    return 1
  fi
  # Only the fixed evidence vocabulary can cross from the private checkout.
  python3 - "$source" "$EXPECTED_STEPS" <<'PY'
import os
import stat
import sys
from pathlib import Path

root = Path(sys.argv[1])
expected = {"summary.json", "steps.tsv"}
expected.update(f"logs/{name}.log" for name in sys.argv[2].split())
observed = set()
observed_directories = set()
for current, directories, files in os.walk(root, topdown=True, followlinks=False):
    relative_root = Path(current).relative_to(root)
    for directory in directories:
        path = Path(current) / directory
        info = os.lstat(path)
        relative = (relative_root / directory).as_posix()
        if relative != "logs" or stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise SystemExit(f"release verification: unsafe private evidence path: {path}")
        observed_directories.add(relative)
    for filename in files:
        path = Path(current) / filename
        info = os.lstat(path)
        relative = (relative_root / filename).as_posix()
        if relative not in expected or stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
            raise SystemExit(f"release verification: unsafe private evidence file: {path}")
        observed.add(relative)
if observed_directories != {"logs"} or observed != expected:
    raise SystemExit("release verification: successful evidence inventory is incomplete")
PY
  mkdir -p "$destination/logs"
  for relative in summary.json steps.tsv; do
    if [ -f "$source/$relative" ] && [ ! -L "$source/$relative" ]; then
      python3 "$SOURCE/scripts/publish_file.py" \
        --source "$source/$relative" \
        --destination "$destination/$relative" \
        --repository-root "$ROOT" \
        --mode 0644
    fi
  done
  for name in $EXPECTED_STEPS; do
    relative="logs/$name.log"
    if [ -f "$source/$relative" ] && [ ! -L "$source/$relative" ]; then
      python3 "$SOURCE/scripts/publish_file.py" \
        --source "$source/$relative" \
        --destination "$destination/$relative" \
        --repository-root "$ROOT" \
        --mode 0644
    fi
  done
}

stage_benchmark_output() {
  source=$1
  destination=$2
  for name in result.json time.txt scan.stdout; do
    if [ ! -f "$source/$name" ] || [ -L "$source/$name" ]; then
      echo "release verification: private benchmark output is missing or unsafe: $source/$name" >&2
      return 1
    fi
  done
  if [ -e "$destination" ] || [ -L "$destination" ]; then
    echo "release verification: private benchmark staging target already exists" >&2
    return 1
  fi
  mkdir -p "$destination"
  for name in result.json time.txt scan.stdout; do
    python3 "$SOURCE/scripts/publish_file.py" \
      --source "$source/$name" \
      --destination "$destination/$name" \
      --repository-root "$ROOT" \
      --mode 0644
  done
}

if [ "${RKC_RELEASE_VERIFY_INNER:-0}" != 1 ]; then
  python3 scripts/git_source_guard.py \
    --root "$ROOT" \
    --operation "release verification"
  if [ -n "${RKC_VALIDATION_PYTHON:-}" ]; then
    VALIDATION_PYTHON=$RKC_VALIDATION_PYTHON
  elif [ -x "$ROOT/.venv/bin/python" ]; then
    VALIDATION_PYTHON=$ROOT/.venv/bin/python
  else
    VALIDATION_PYTHON=$(command -v python3 || true)
  fi
  if [ -z "$VALIDATION_PYTHON" ] || [ ! -x "$VALIDATION_PYTHON" ]; then
    echo "release verification: no executable validation Python was found" >&2
    exit 1
  fi
  case "$VALIDATION_PYTHON" in
    /*) ;;
    *)
      VALIDATION_PYTHON=$(CDPATH= cd -- "$(dirname -- "$VALIDATION_PYTHON")" && pwd -P)/$(basename -- "$VALIDATION_PYTHON")
      ;;
  esac
  "$VALIDATION_PYTHON" scripts/verify_python_environment.py \
    --requirements requirements-dev.txt >/dev/null
  SOURCE_COMMIT=$(git rev-parse --verify 'HEAD^{commit}')
  SOURCE_TREE=$(git rev-parse --verify "${SOURCE_COMMIT}^{tree}")
  SOURCE_COMMIT_TIME=$(git show -s --format=%ct "$SOURCE_COMMIT")
  case "$SOURCE_COMMIT:$SOURCE_TREE:$SOURCE_COMMIT_TIME" in
    *[!0-9a-f:]*|*::*|:*)
      echo "release verification: invalid Git source identity" >&2
      exit 1
      ;;
  esac
  if [ -L "$ROOT/dist" ] || { [ -e "$ROOT/dist" ] && [ ! -d "$ROOT/dist" ]; }; then
    echo "release verification: dist must be a real directory" >&2
    exit 1
  fi
  mkdir -p "$ROOT/dist"
  WORK=$(mktemp -d "$ROOT/dist/.rkc-release-verification.XXXXXX")
  trap 'rm -rf "$WORK"' EXIT INT TERM
  SOURCE=$WORK/source
  git clone --quiet --no-hardlinks --no-checkout -- "$ROOT" "$SOURCE"
  git -C "$SOURCE" -c advice.detachedHead=false checkout --quiet --detach "$SOURCE_COMMIT"
  git -C "$SOURCE" remote set-url origin https://github.com/neuroforge-io/RKC.git
  if [ "$(git -C "$SOURCE" rev-parse HEAD)" != "$SOURCE_COMMIT" ] ||
     [ "$(git -C "$SOURCE" rev-parse 'HEAD^{tree}')" != "$SOURCE_TREE" ] ||
     [ -n "$(git -C "$SOURCE" status --porcelain=v1 --untracked-files=all)" ]; then
    echo "release verification: private source checkout does not match immutable HEAD" >&2
    exit 1
  fi
  if RKC_RELEASE_VERIFY_INNER=1 PYTHON="$VALIDATION_PYTHON" \
    sh "$SOURCE/scripts/verify-release.sh"; then
    verification_status=0
  else
    verification_status=$?
  fi
  if [ "$(git -C "$SOURCE" rev-parse HEAD)" != "$SOURCE_COMMIT" ] ||
     [ "$(git -C "$SOURCE" rev-parse 'HEAD^{tree}')" != "$SOURCE_TREE" ] ||
     [ -n "$(git -C "$SOURCE" status --porcelain=v1 --untracked-files=all)" ]; then
    echo "release verification: private source checkout changed during verification" >&2
    exit 1
  fi
  if [ "$verification_status" -ne 0 ]; then
    echo "release verification: failed evidence was not published; prior evidence is unchanged" >&2
    exit "$verification_status"
  fi
  python3 "$SOURCE/scripts/git_source_guard.py" \
    --root "$ROOT" \
    --operation "release evidence staging"
  EVIDENCE=$WORK/evidence
  stage_validation_output "$SOURCE/dist/validation" "$EVIDENCE/validation"
  stage_benchmark_output "$SOURCE/dist/benchmark" "$EVIDENCE/benchmark"
  python3 "$SOURCE/scripts/git_source_guard.py" \
    --root "$ROOT" \
    --operation "release evidence publication"
  python3 "$SOURCE/scripts/publish_directory.py" \
    --source "$EVIDENCE" \
    --destination "$ROOT/dist/evidence" \
    --repository-root "$ROOT"
  exit 0
fi

OUT=$ROOT/dist/validation
SOURCE_COMMIT=$(git rev-parse --verify 'HEAD^{commit}')
SOURCE_TREE=$(git rev-parse --verify "${SOURCE_COMMIT}^{tree}")
SOURCE_COMMIT_TIME=$(git show -s --format=%ct "$SOURCE_COMMIT")
prepare_validation_output "$OUT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-release-verification.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
STEPS="$WORK/steps.tsv"
: >"$STEPS"
START=$(date +%s)
SUMMARY="$WORK/summary.json"
python3 - "$SUMMARY" "$START" "$SOURCE_COMMIT" "$SOURCE_TREE" "$SOURCE_COMMIT_TIME" <<'PY'
import json,sys
path,start=sys.argv[1],int(sys.argv[2])
source={'commit':sys.argv[3],'tree':sys.argv[4],'commit_time_unix':sys.argv[5]}
with open(path,'w',encoding='utf-8') as output:
    json.dump({'schema_version':'2.0','status':'running','ok':False,'started_at_unix':start,'source':source},output,indent=2,sort_keys=True)
    output.write('\n')
PY
python3 scripts/publish_file.py --source "$STEPS" --destination "$OUT/steps.tsv" --mode 0644
python3 scripts/publish_file.py --source "$SUMMARY" --destination "$OUT/summary.json" --mode 0644
run_step() {
  name=$1
  shift
  echo "==> $name"
  step_start=$(date +%s)
  step_log="$WORK/$name.log"
  if "$@" >"$step_log" 2>&1; then
    status=passed
  else
    status=failed
    python3 scripts/publish_file.py --source "$step_log" --destination "$OUT/logs/$name.log" --mode 0644
    cat "$OUT/logs/$name.log" >&2
    python3 - "$SUMMARY" "$START" "$name" "$SOURCE_COMMIT" "$SOURCE_TREE" "$SOURCE_COMMIT_TIME" <<'PY'
import json,sys,time
path,start,name=sys.argv[1],int(sys.argv[2]),sys.argv[3]
source={'commit':sys.argv[4],'tree':sys.argv[5],'commit_time_unix':sys.argv[6]}
json.dump({'schema_version':'2.0','ok':False,'failed_step':name,'elapsed_seconds':int(time.time()-start),'source':source},open(path,'w'),indent=2,sort_keys=True);open(path,'a').write('\n')
PY
    python3 scripts/publish_file.py --source "$STEPS" --destination "$OUT/steps.tsv" --mode 0644
    python3 scripts/publish_file.py --source "$SUMMARY" --destination "$OUT/summary.json" --mode 0644
    exit 1
  fi
  python3 scripts/publish_file.py --source "$step_log" --destination "$OUT/logs/$name.log" --mode 0644
  step_end=$(date +%s)
  printf '%s\t%s\t%s\n' "$name" "$status" "$((step_end-step_start))" >>"$STEPS"
}
run_step go-modules make go-mod-verify
run_step python-environment "$PYTHON" scripts/verify_python_environment.py --requirements requirements-dev.txt
run_step format make format-check
run_step vet make vet
run_step coverage make coverage
run_step contracts make contracts
run_step docs make docs-check
run_step licenses "$PYTHON" scripts/validate-licenses.py
run_step model-lock make model-lock-check
run_step build make build
run_step plugins make plugins
run_step smoke make smoke
run_step reproducibility make reproducibility
run_step api-smoke make smoke-api
run_step mcp-smoke make smoke-mcp
run_step git-smoke make smoke-git
run_step race make test-race
run_step benchmark timeout 180 sh scripts/benchmark-reference.sh dist/benchmark
END=$(date +%s)
if [ "$(git rev-parse --verify 'HEAD^{commit}')" != "$SOURCE_COMMIT" ] ||
   [ "$(git rev-parse --verify 'HEAD^{tree}')" != "$SOURCE_TREE" ]; then
  echo "release verification: repository HEAD changed during verification" >&2
  exit 1
fi
python3 - "$STEPS" "$SUMMARY" "$START" "$END" "$OUT/logs" "$SOURCE_COMMIT" "$SOURCE_TREE" "$SOURCE_COMMIT_TIME" <<'PY'
import hashlib,json,sys
steps=[]
for line in open(sys.argv[1]):
    name,status,duration=line.rstrip('\n').split('\t')
    payload=open(f"{sys.argv[5]}/{name}.log",'rb').read()
    steps.append({'name':name,'status':status,'duration_seconds':int(duration),'log_sha256':hashlib.sha256(payload).hexdigest()})
source={'commit':sys.argv[6],'tree':sys.argv[7],'commit_time_unix':sys.argv[8]}
document={'schema_version':'2.0','ok':all(s['status']=='passed' for s in steps),'elapsed_seconds':int(sys.argv[4])-int(sys.argv[3]),'source':source,'steps':steps}
json.dump(document,open(sys.argv[2],'w'),indent=2,sort_keys=True);open(sys.argv[2],'a').write('\n')
PY
python3 scripts/publish_file.py --source "$STEPS" --destination "$OUT/steps.tsv" --mode 0644
python3 scripts/publish_file.py --source "$SUMMARY" --destination "$OUT/summary.json" --mode 0644
printf 'release verification: passed (%ss)\n' "$((END-START))"
