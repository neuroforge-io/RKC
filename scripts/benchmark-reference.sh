#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
case $# in
  0) ;;
  1)
    if [ "$1" != "dist/benchmark" ]; then
      echo "usage: scripts/benchmark-reference.sh [dist/benchmark]" >&2
      exit 2
    fi
    ;;
  *)
    echo "usage: scripts/benchmark-reference.sh [dist/benchmark]" >&2
    exit 2
    ;;
esac
OUT=dist/benchmark
if [ -L dist ] || { [ -e dist ] && [ ! -d dist ]; }; then
  echo "benchmark: dist must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
if [ -L "$OUT" ] || { [ -e "$OUT" ] && [ ! -d "$OUT" ]; }; then
  echo "benchmark: $OUT must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
mkdir -p "$OUT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-benchmark.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
START=$(python3 -c 'import time; print(time.monotonic_ns())')
set -- ./bin/rkc scan \
  --out "$OUT/atlas" \
  --exclude .cache \
  --exclude .coverage \
  --exclude .git \
  --exclude .mypy_cache \
  --exclude .pytest_cache \
  --exclude .rkc \
  --exclude .rkc-coverage \
  --exclude .rkc-downloads \
  --exclude .rkc-models \
  --exclude .rkc-runtime \
  --exclude .rkc-state \
  --exclude .rkc.rkc-derived \
  --exclude .ruff_cache \
  --exclude .venv \
  --exclude __pycache__ \
  --exclude bin \
  --exclude coverage \
  --exclude coverage.out \
  --exclude coverage.xml \
  --exclude dist \
  --exclude htmlcov \
  --exclude venv \
  --exclude .rkc-smoke \
  --exclude .rkc-state-smoke \
  --exclude .rkc-smoke.rkc-derived \
  --exclude plugins/python-ast/__pycache__ \
  --exclude scripts/__pycache__ \
  --no-static-site \
  --no-integrations \
  --no-jsonl-graph \
  --no-search-index \
  --include-sources=false \
  --force \
  .
if command -v /usr/bin/time >/dev/null 2>&1; then
  if ! /usr/bin/time -v "$@" >"$WORK/scan.stdout" 2>"$WORK/time.txt"; then
    sed -n '1,200p' "$WORK/time.txt" >&2
    exit 1
  fi
else
  "$@" >"$WORK/scan.stdout"
  : >"$WORK/time.txt"
fi
END=$(python3 -c 'import time; print(time.monotonic_ns())')
python3 - "$START" "$END" "$OUT/atlas/coverage.json" "$WORK/result.json" "$WORK/time.txt" <<'PY'
import json,re,sys
start,end=map(int,sys.argv[1:3]); coverage=json.load(open(sys.argv[3]))
text=open(sys.argv[5],errors='replace').read()
match=re.search(r'Maximum resident set size \(kbytes\):\s*(\d+)',text)
json.dump({'schema_version':'1.0','profile':'self-analysis-with-heavy-derived-exports-disabled','elapsed_seconds':(end-start)/1e9,'maximum_rss_kib':int(match.group(1)) if match else None,'coverage':coverage},open(sys.argv[4],'w'),indent=2,sort_keys=True);open(sys.argv[4],'a').write('\n')
PY
publish_file() {
  source=$1
  destination=$2
  python3 scripts/publish_file.py \
    --source "$source" \
    --destination "$OUT/$destination" \
    --mode 0644
}
publish_file "$WORK/scan.stdout" scan.stdout
publish_file "$WORK/time.txt" time.txt
publish_file "$WORK/result.json" result.json
echo "benchmark: $OUT/result.json"
