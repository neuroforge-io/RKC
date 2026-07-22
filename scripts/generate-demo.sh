#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
OUT=dist/demo
STATE=dist/demo-state
rm -rf "$OUT" "$STATE"
mkdir -p dist
./bin/rkc scan --out "$OUT" --state-dir "$STATE" --force examples >dist/demo-scan.txt
./bin/rkc check --coverage "$OUT/coverage.json" --min-inventory-accounting 1 --min-symbol-evidence 1 --min-edge-resolution 0.5 --max-errors 0 --max-high-confidence-secrets 0 >dist/demo-check.txt
./bin/rkc synthesize --dir "$OUT" --repo-root examples --out "$OUT/derived/packet-example" --packet-only --query Login --limit 1 --force >dist/demo-synthesis.txt
