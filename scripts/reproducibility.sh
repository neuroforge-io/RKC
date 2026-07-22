#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-repro.XXXXXX")
A=$WORK/a
B=$WORK/b
trap 'rm -rf "$WORK"' EXIT INT TERM
./bin/rkc scan --out "$A" --force examples >/dev/null
./bin/rkc scan --out "$B" --force examples >/dev/null
cmp "$A/bundle.json" "$B/bundle.json"
cmp "$A/coverage.json" "$B/coverage.json"
DA=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["deterministic_output_digest"])' "$A/coverage.json")
DB=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["deterministic_output_digest"])' "$B/coverage.json")
[ "$DA" = "$DB" ]
echo "reproducibility: passed ($DA)"
