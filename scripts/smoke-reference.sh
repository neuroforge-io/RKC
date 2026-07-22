#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-smoke.XXXXXX")
ATLAS=$WORK/atlas
trap 'rm -rf "$WORK"' EXIT INT TERM

./bin/rkc scan --out "$ATLAS" --force examples
./bin/rkc check \
  --coverage "$ATLAS/coverage.json" \
  --min-inventory-accounting 1 \
  --min-symbol-evidence 1 \
  --min-edge-resolution 0.5 \
  --max-errors 0 \
  --max-high-confidence-secrets 0
./bin/rkc query --dir "$ATLAS" --limit 5 Login
./bin/rkc synthesize \
  --dir "$ATLAS" \
  --repo-root examples \
  --packet-only \
  --query Login \
  --limit 1 \
  --force
