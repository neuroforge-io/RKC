#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
OUT=${TMPDIR:-/tmp}/rkc-api-smoke-$$
PORT=${RKC_SMOKE_PORT:-18787}
LOG=$OUT/server.log
PID=
cleanup() { [ -n "${PID:-}" ] && kill "$PID" 2>/dev/null || true; rm -rf "$OUT"; }
trap cleanup EXIT INT TERM
./bin/rkc scan --out "$OUT" --force examples >/dev/null
./bin/rkc serve --dir "$OUT" --addr "127.0.0.1:$PORT" >"$LOG" 2>&1 & PID=$!
i=0
while [ $i -lt 50 ]; do
  if curl -fsS "http://127.0.0.1:$PORT/api/v1/health" >"$OUT/health.json"; then break; fi
  i=$((i+1)); sleep 0.1
done
curl -fsS "http://127.0.0.1:$PORT/api/v1/search?q=Login&limit=3" >"$OUT/search.json"
python3 - "$OUT/health.json" "$OUT/search.json" <<'PY'
import json,sys
health=json.load(open(sys.argv[1])); search=json.load(open(sys.argv[2]))
assert health['status']=='ok' and health['nodes']>0
assert search.get('items') or search.get('hits') or search.get('retrieval',{}).get('hits')
PY
echo "api smoke: passed"
