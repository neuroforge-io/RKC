#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
OUT=${TMPDIR:-/tmp}/rkc-mcp-smoke-$$
trap 'rm -rf "$OUT"' EXIT INT TERM
./bin/rkc scan --out "$OUT" --force examples >/dev/null
cat <<'JSON' | ./bin/rkc-mcp --dir "$OUT" >"$OUT/mcp.jsonl"
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rkc.search","arguments":{"query":"Login","limit":3}}}
JSON
python3 - "$OUT/mcp.jsonl" <<'PY'
import json,sys
rows=[json.loads(line) for line in open(sys.argv[1]) if line.strip()]
assert len(rows)==3
assert rows[0]['result']['protocolVersion']=='2025-11-25'
assert any(t['name']=='rkc.search' for t in rows[1]['result']['tools'])
assert rows[2]['result']['isError'] is False
PY
echo "mcp smoke: passed"
