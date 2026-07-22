#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
OUT=${1:-dist/benchmark}
rm -rf "$OUT"
mkdir -p "$OUT"
START=$(python3 -c 'import time; print(time.monotonic_ns())')
SCAN_ARGS="--out $OUT/atlas --exclude dist --exclude bin --exclude .rkc-smoke --exclude .rkc-state-smoke --no-static-site --no-integrations --no-jsonl-graph --no-search-index --include-sources=false --force ."
if command -v /usr/bin/time >/dev/null 2>&1; then
  # shellcheck disable=SC2086
  /usr/bin/time -v ./bin/rkc scan $SCAN_ARGS >"$OUT/scan.stdout" 2>"$OUT/time.txt"
else
  # shellcheck disable=SC2086
  ./bin/rkc scan $SCAN_ARGS >"$OUT/scan.stdout"
  : >"$OUT/time.txt"
fi
END=$(python3 -c 'import time; print(time.monotonic_ns())')
python3 - "$START" "$END" "$OUT/atlas/coverage.json" "$OUT/result.json" "$OUT/time.txt" <<'PY'
import json,re,sys
start,end=map(int,sys.argv[1:3]); coverage=json.load(open(sys.argv[3]))
text=open(sys.argv[5],errors='replace').read()
match=re.search(r'Maximum resident set size \(kbytes\):\s*(\d+)',text)
json.dump({'schema_version':'1.0','profile':'self-analysis-with-heavy-derived-exports-disabled','elapsed_seconds':(end-start)/1e9,'maximum_rss_kib':int(match.group(1)) if match else None,'coverage':coverage},open(sys.argv[4],'w'),indent=2,sort_keys=True);open(sys.argv[4],'a').write('\n')
PY
echo "benchmark: $OUT/result.json"
