#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-api-smoke.XXXXXX")
OUT=$WORK/atlas
LOG=$WORK/server.log
READY=$WORK/ready.json
PID=
cleanup() {
  if [ -n "${PID:-}" ]; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM
./bin/rkc scan --out "$OUT" --force examples >/dev/null
./bin/rkc serve --dir "$OUT" --addr "127.0.0.1:0" --ready-file "$READY" >"$LOG" 2>&1 & PID=$!
i=0
ready=false
while [ $i -lt 50 ]; do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "api smoke: server exited before readiness" >&2
    sed -n '1,200p' "$LOG" >&2
    exit 1
  fi
  if [ -f "$READY" ]; then
    ready=true
    break
  fi
  i=$((i+1)); sleep 0.1
done
if [ "$ready" != true ]; then
  echo "api smoke: server did not become ready" >&2
  sed -n '1,200p' "$LOG" >&2
  exit 1
fi
URL=$(python3 - "$READY" "$OUT/rkc.manifest.json" <<'PY'
import json,sys
ready=json.load(open(sys.argv[1])); manifest=json.load(open(sys.argv[2]))
assert ready['schema_version']=='1.0'
assert ready['snapshot_id']==manifest['id']
assert ready['url']=='http://'+ready['address']
print(ready['url'])
PY
)
curl -fsS "$URL/api/v1/health" >"$WORK/health.json"
curl -fsS "$URL/api/v1/search?q=Login&limit=3" >"$WORK/search.json"
python3 - "$WORK/health.json" "$WORK/search.json" "$OUT/rkc.manifest.json" <<'PY'
import json,sys
health=json.load(open(sys.argv[1])); search=json.load(open(sys.argv[2])); manifest=json.load(open(sys.argv[3]))
assert health['status']=='ok' and health['nodes']>0 and health['snapshot_id']==manifest['id']
assert search.get('items') or search.get('hits') or search.get('retrieval',{}).get('hits')
PY
echo "api smoke: passed"
