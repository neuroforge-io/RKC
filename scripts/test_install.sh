#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-install-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT HUP INT TERM

PREFIX=$WORK/prefix
PATH=/usr/bin:/bin "$ROOT/install.sh" --skip-build --prefix "$PREFIX" >"$WORK/output.txt"

test -x "$PREFIX/bin/rkc"
test -x "$PREFIX/bin/rkc-mcp"
test -f "$PREFIX/share/doc/rkc/LICENSE"
test -f "$PREFIX/share/doc/rkc/NOTICE"
test -f "$PREFIX/share/doc/rkc/THIRD_PARTY_NOTICES.md"
test -f "$PREFIX/share/rkc/models/models.lock.json"
test -f "$PREFIX/share/rkc/models/qualification/rkc-local-model-v1.json"
test -f "$PREFIX/share/rkc/schemas/model-lock.schema.json"
cmp "$ROOT/LICENSE" "$PREFIX/share/doc/rkc/LICENSE"
cmp "$ROOT/NOTICE" "$PREFIX/share/doc/rkc/NOTICE"
cmp "$ROOT/THIRD_PARTY_NOTICES.md" "$PREFIX/share/doc/rkc/THIRD_PARTY_NOTICES.md"
"$PREFIX/bin/rkc" version >"$WORK/version.txt"
cmp "$ROOT/VERSION" "$WORK/version.txt"
"$PREFIX/bin/rkc" open --help >"$WORK/open-help.txt" 2>&1
grep -F -- "-no-browser" "$WORK/open-help.txt" >/dev/null
grep -F -- "-workbench" "$WORK/open-help.txt" >/dev/null
grep -F "trusted-user local command launcher" "$WORK/open-help.txt" >/dev/null
"$PREFIX/bin/rkc" wizard --help >"$WORK/wizard-help.txt" 2>&1
grep -F "rkc tui" "$WORK/wizard-help.txt" >/dev/null
grep -F "not a replacement for every CLI option" "$WORK/wizard-help.txt" >/dev/null
grep -F "First run: '$PREFIX/bin/rkc' wizard" "$WORK/output.txt" >/dev/null
grep -F "For this shell: export PATH='$PREFIX/bin':\"\$PATH\"" "$WORK/output.txt" >/dev/null
FIRST_RUN=$(sed -n 's/^First run: //p' "$WORK/output.txt")
PATH=/usr/bin:/bin sh -c "$FIRST_RUN --help" >"$WORK/emitted-help.txt" 2>&1
grep -F "rkc tui" "$WORK/emitted-help.txt" >/dev/null
PATH_COMMAND=$(sed -n 's/^For this shell: //p' "$WORK/output.txt")
RESOLVED=$(PATH=/usr/bin:/bin sh -c "$PATH_COMMAND; command -v rkc")
test "$RESOLVED" = "$PREFIX/bin/rkc"

PATH="$PREFIX/bin:/usr/bin:/bin" \
	"$ROOT/install.sh" --skip-build --prefix "$PREFIX" >"$WORK/on-path-output.txt"
grep -F "First run: rkc wizard" "$WORK/on-path-output.txt" >/dev/null
if grep -F "For this shell:" "$WORK/on-path-output.txt" >/dev/null; then
	echo "install test: PATH instruction printed for an already reachable binary" >&2
	exit 1
fi

ODD_PREFIX="$WORK/prefix with a 'quote"
PATH=/usr/bin:/bin \
	"$ROOT/install.sh" --skip-build --prefix "$ODD_PREFIX" >"$WORK/odd-output.txt"
ODD_FIRST_RUN=$(sed -n 's/^First run: //p' "$WORK/odd-output.txt")
PATH=/usr/bin:/bin sh -c "$ODD_FIRST_RUN --help" >"$WORK/odd-help.txt" 2>&1
grep -F "rkc tui" "$WORK/odd-help.txt" >/dev/null
ODD_PATH_COMMAND=$(sed -n 's/^For this shell: //p' "$WORK/odd-output.txt")
ODD_RESOLVED=$(PATH=/usr/bin:/bin sh -c "$ODD_PATH_COMMAND; command -v rkc")
test "$ODD_RESOLVED" = "$ODD_PREFIX/bin/rkc"

COLON_PREFIX="$WORK/prefix:colon"
PATH=/usr/bin:/bin \
	"$ROOT/install.sh" --skip-build --prefix "$COLON_PREFIX" >"$WORK/colon-output.txt"
COLON_FIRST_RUN=$(sed -n 's/^First run: //p' "$WORK/colon-output.txt")
PATH=/usr/bin:/bin sh -c "$COLON_FIRST_RUN --help" >"$WORK/colon-help.txt" 2>&1
grep -F "cannot be added to PATH portably" "$WORK/colon-output.txt" >/dev/null
if grep -F "For this shell:" "$WORK/colon-output.txt" >/dev/null; then
	echo "install test: emitted a non-portable PATH instruction" >&2
	exit 1
fi

mkdir "$WORK/unsafe"
ln -s "$WORK/unsafe" "$WORK/linked-prefix"
if "$ROOT/install.sh" --skip-build --prefix "$WORK/linked-prefix" >"$WORK/unsafe.out" 2>"$WORK/unsafe.err"; then
	echo "install test: symlink prefix unexpectedly succeeded" >&2
	exit 1
fi
grep -F "destination is not a real directory" "$WORK/unsafe.err" >/dev/null

echo "install test: passed"
