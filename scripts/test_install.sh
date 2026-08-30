#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-install-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT HUP INT TERM

PREFIX=$WORK/prefix
"$ROOT/install.sh" --skip-build --prefix "$PREFIX" >"$WORK/output.txt"

test -x "$PREFIX/bin/rkc"
test -x "$PREFIX/bin/rkc-mcp"
test -f "$PREFIX/share/doc/rkc/LICENSE"
test -f "$PREFIX/share/doc/rkc/NOTICE"
test -f "$PREFIX/share/doc/rkc/THIRD_PARTY_NOTICES.md"
test -f "$PREFIX/share/rkc/models/models.lock.json"
test -f "$PREFIX/share/rkc/models/qualification/rkc-local-model-v1.json"
test -f "$PREFIX/share/rkc/schemas/model-lock.schema.json"
"$PREFIX/bin/rkc" version >"$WORK/version.txt"
cmp "$ROOT/VERSION" "$WORK/version.txt"
grep -F "First run: rkc open" "$WORK/output.txt" >/dev/null

mkdir "$WORK/unsafe"
ln -s "$WORK/unsafe" "$WORK/linked-prefix"
if "$ROOT/install.sh" --skip-build --prefix "$WORK/linked-prefix" >"$WORK/unsafe.out" 2>"$WORK/unsafe.err"; then
	echo "install test: symlink prefix unexpectedly succeeded" >&2
	exit 1
fi
grep -F "destination is not a real directory" "$WORK/unsafe.err" >/dev/null

echo "install test: passed"
