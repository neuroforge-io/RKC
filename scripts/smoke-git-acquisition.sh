#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-git-smoke.XXXXXX")
REPO=$WORK/source
OUT=$WORK/output
trap 'rm -rf "$WORK"' EXIT INT TERM
mkdir -p "$REPO"
cp -R examples/sample-go/. "$REPO/"
git -C "$REPO" init -q
git -C "$REPO" config user.email rkc@example.invalid
git -C "$REPO" config user.name RKC
git -C "$REPO" add .
git -C "$REPO" commit -qm initial
./bin/rkc scan --allow-file-url --out "$OUT" --force "file://$REPO" >/dev/null
python3 - "$OUT/rkc.manifest.json" <<'PY'
import json,sys
m=json.load(open(sys.argv[1])); assert m['git']['commit']; assert m['content_digest']
PY
echo "git acquisition smoke: passed"
