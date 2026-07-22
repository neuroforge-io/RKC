#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
OUT=dist/demo
if [ -L dist ] || { [ -e dist ] && [ ! -d dist ]; }; then
  echo "demo: dist must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
mkdir -p dist
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-demo.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
run_and_publish() {
  destination=$1
  shift
  temporary="$WORK/$destination"
  "$@" >"$temporary"
  python3 scripts/publish_file.py \
    --source "$temporary" \
    --destination "dist/$destination" \
    --mode 0644
}
run_and_publish demo-scan.txt ./bin/rkc scan --out "$OUT" --force examples
run_and_publish demo-check.txt ./bin/rkc check --coverage "$OUT/coverage.json" --min-inventory-accounting 1 --min-symbol-evidence 1 --min-edge-resolution 0.5 --max-errors 0 --max-high-confidence-secrets 0
run_and_publish demo-synthesis.txt ./bin/rkc synthesize --dir "$OUT" --repo-root examples --packet-only --query Login --limit 1 --force
