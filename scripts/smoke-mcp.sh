#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-mcp-smoke.XXXXXX")
OUT=$WORK/atlas
RESPONSES=$WORK/mcp.jsonl
trap 'rm -rf "$WORK"' EXIT INT TERM
./bin/rkc scan --out "$OUT" --force examples >/dev/null
cat <<'JSON' | ./bin/rkc-mcp --dir "$OUT" >"$RESPONSES"
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rkc.search","arguments":{"query":"Login","limit":3}}}
JSON
python3 - "$RESPONSES" <<'PY'
import json,sys
rows=[json.loads(line) for line in open(sys.argv[1]) if line.strip()]
assert len(rows)==3
assert rows[0]['result']['protocolVersion']=='2025-11-25'
assert any(t['name']=='rkc.search' for t in rows[1]['result']['tools'])
assert rows[2]['result']['isError'] is False
PY
echo "mcp smoke: passed"
