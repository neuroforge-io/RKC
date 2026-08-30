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

PREBUILT=$WORK/prebuilt
PREBUILT_PREFIX=$WORK/prebuilt-prefix
mkdir "$PREBUILT"
cp "$ROOT/bin/rkc" "$PREBUILT/rkc"
cp "$ROOT/bin/rkc-mcp" "$PREBUILT/rkc-mcp"
PATH=/usr/bin:/bin "$ROOT/install.sh" \
	--skip-build \
	--prebuilt-binary-dir "$PREBUILT" \
	--prefix "$PREBUILT_PREFIX" >"$WORK/prebuilt-output.txt"
cmp "$PREBUILT/rkc" "$PREBUILT_PREFIX/bin/rkc"
cmp "$PREBUILT/rkc-mcp" "$PREBUILT_PREFIX/bin/rkc-mcp"
cmp "$ROOT/NOTICE" "$PREBUILT_PREFIX/share/doc/rkc/NOTICE"
cmp \
	"$ROOT/models/qualification/rkc-local-model-v1.json" \
	"$PREBUILT_PREFIX/share/rkc/models/qualification/rkc-local-model-v1.json"

if "$ROOT/install.sh" \
	--prebuilt-binary-dir "$PREBUILT" \
	--prefix "$WORK/prebuilt-with-build" \
	>"$WORK/prebuilt-with-build.out" 2>"$WORK/prebuilt-with-build.err"; then
	echo "install test: prebuilt binary directory worked without --skip-build" >&2
	exit 1
fi
grep -F "requires --skip-build" "$WORK/prebuilt-with-build.err" >/dev/null

if "$ROOT/install.sh" \
	--skip-build \
	--prebuilt-binary-dir "$WORK/missing-prebuilt" \
	--prefix "$WORK/missing-prefix" \
	>"$WORK/missing-prebuilt.out" 2>"$WORK/missing-prebuilt.err"; then
	echo "install test: missing prebuilt binary directory unexpectedly worked" >&2
	exit 1
fi
grep -F "missing or is a symlink" "$WORK/missing-prebuilt.err" >/dev/null

ln -s "$PREBUILT" "$WORK/linked-prebuilt"
if "$ROOT/install.sh" \
	--skip-build \
	--prebuilt-binary-dir "$WORK/linked-prebuilt" \
	--prefix "$WORK/linked-prebuilt-prefix" \
	>"$WORK/linked-prebuilt.out" 2>"$WORK/linked-prebuilt.err"; then
	echo "install test: linked prebuilt binary directory unexpectedly worked" >&2
	exit 1
fi
grep -F "missing or is a symlink" "$WORK/linked-prebuilt.err" >/dev/null

UNSAFE_PREBUILT=$WORK/unsafe-prebuilt
mkdir "$UNSAFE_PREBUILT"
cp "$ROOT/bin/rkc" "$UNSAFE_PREBUILT/rkc"
ln -s "$ROOT/bin/rkc-mcp" "$UNSAFE_PREBUILT/rkc-mcp"
if "$ROOT/install.sh" \
	--skip-build \
	--prebuilt-binary-dir "$UNSAFE_PREBUILT" \
	--prefix "$WORK/unsafe-prebuilt-prefix" \
	>"$WORK/unsafe-prebuilt.out" 2>"$WORK/unsafe-prebuilt.err"; then
	echo "install test: linked prebuilt binary unexpectedly worked" >&2
	exit 1
fi
grep -F "prebuilt binary is missing or unsafe" "$WORK/unsafe-prebuilt.err" >/dev/null

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

PACKAGE=$WORK/complete-package
PACKAGE_SOURCE=$PACKAGE/source
PACKAGE_BINARIES=$PACKAGE/artifacts/binaries
mkdir -p \
	"$PACKAGE_SOURCE/models/qualification" \
	"$PACKAGE_SOURCE/schemas" \
	"$PACKAGE_BINARIES/linux-amd64" \
	"$PACKAGE_BINARIES/linux-arm64"
cp "$ROOT/scripts/install-package.sh" "$PACKAGE/install.sh"
cp "$ROOT/install.sh" "$PACKAGE_SOURCE/install.sh"
chmod 0755 "$PACKAGE/install.sh" "$PACKAGE_SOURCE/install.sh"
for document in LICENSE NOTICE THIRD_PARTY_NOTICES.md; do
	cp "$ROOT/$document" "$PACKAGE_SOURCE/$document"
done
cp "$ROOT/models/models.lock.json" "$PACKAGE_SOURCE/models/models.lock.json"
cp \
	"$ROOT/models/qualification/rkc-local-model-v1.json" \
	"$PACKAGE_SOURCE/models/qualification/rkc-local-model-v1.json"
cp "$ROOT/schemas/model-lock.schema.json" "$PACKAGE_SOURCE/schemas/model-lock.schema.json"
cp \
	"$ROOT/schemas/model-qualification.schema.json" \
	"$PACKAGE_SOURCE/schemas/model-qualification.schema.json"
for platform in linux-amd64 linux-arm64; do
	printf '%s\n' "$platform-rkc" >"$PACKAGE_BINARIES/$platform/rkc"
	printf '%s\n' "$platform-rkc-mcp" >"$PACKAGE_BINARIES/$platform/rkc-mcp"
	chmod 0755 \
		"$PACKAGE_BINARIES/$platform/rkc" \
		"$PACKAGE_BINARIES/$platform/rkc-mcp"
done

write_package_checksums() {
	(
		cd "$PACKAGE"
		sha256sum \
			install.sh \
			source/install.sh \
			artifacts/binaries/linux-amd64/rkc \
			artifacts/binaries/linux-amd64/rkc-mcp \
			artifacts/binaries/linux-arm64/rkc \
			artifacts/binaries/linux-arm64/rkc-mcp \
			>SHA256SUMS.txt
	)
}
write_package_checksums

FAKE_BIN=$WORK/fake-bin
mkdir "$FAKE_BIN"
cat >"$FAKE_BIN/uname" <<'EOF'
#!/bin/sh
case "${1-}" in
	-s) printf '%s\n' "${RKC_TEST_UNAME_SYSTEM:-Linux}" ;;
	-m) printf '%s\n' "${RKC_TEST_UNAME_MACHINE:-x86_64}" ;;
	*) exit 2 ;;
esac
EOF
chmod 0755 "$FAKE_BIN/uname"

assert_package_architecture() {
	machine=$1
	platform=$2
	package_prefix=$WORK/package-prefix-$machine
	PATH="$FAKE_BIN:/usr/bin:/bin" \
		RKC_TEST_UNAME_SYSTEM=Linux \
		RKC_TEST_UNAME_MACHINE=$machine \
		"$PACKAGE/install.sh" --prefix "$package_prefix" \
		>"$WORK/package-$machine.out"
	grep -F "$platform-rkc" "$package_prefix/bin/rkc" >/dev/null
	grep -F "$platform-rkc-mcp" "$package_prefix/bin/rkc-mcp" >/dev/null
	cmp "$PACKAGE_SOURCE/LICENSE" "$package_prefix/share/doc/rkc/LICENSE"
}
assert_package_architecture x86_64 linux-amd64
assert_package_architecture amd64 linux-amd64
assert_package_architecture aarch64 linux-arm64
assert_package_architecture arm64 linux-arm64

PACKAGE_ODD_PREFIX="$WORK/package prefix with a 'quote"
PATH="$FAKE_BIN:/usr/bin:/bin" \
	RKC_TEST_UNAME_SYSTEM=Linux \
	RKC_TEST_UNAME_MACHINE=x86_64 \
	"$PACKAGE/install.sh" --prefix "$PACKAGE_ODD_PREFIX" \
	>"$WORK/package-odd-prefix.out"
grep -F "linux-amd64-rkc" "$PACKAGE_ODD_PREFIX/bin/rkc" >/dev/null

if PATH="$FAKE_BIN:/usr/bin:/bin" \
	RKC_TEST_UNAME_SYSTEM=Darwin \
	RKC_TEST_UNAME_MACHINE=x86_64 \
	"$PACKAGE/install.sh" --prefix "$WORK/package-darwin" \
	>"$WORK/package-darwin.out" 2>"$WORK/package-darwin.err"; then
	echo "install test: package installer accepted a non-Linux host" >&2
	exit 1
fi
grep -F "packaged binaries support Linux only" "$WORK/package-darwin.err" >/dev/null

if PATH="$FAKE_BIN:/usr/bin:/bin" \
	RKC_TEST_UNAME_SYSTEM=Linux \
	RKC_TEST_UNAME_MACHINE=riscv64 \
	"$PACKAGE/install.sh" --prefix "$WORK/package-riscv" \
	>"$WORK/package-riscv.out" 2>"$WORK/package-riscv.err"; then
	echo "install test: package installer accepted an unsupported architecture" >&2
	exit 1
fi
grep -F "unsupported Linux architecture" "$WORK/package-riscv.err" >/dev/null

tamper_index=0
for relative in \
	install.sh \
	source/install.sh \
	artifacts/binaries/linux-amd64/rkc \
	artifacts/binaries/linux-amd64/rkc-mcp; do
	tamper_index=$((tamper_index + 1))
	tampered=$PACKAGE/$relative
	backup=$WORK/tamper-backup-$tamper_index
	cp "$tampered" "$backup"
	printf '\n# tampered\n' >>"$tampered"
	tamper_prefix=$WORK/tamper-prefix-$tamper_index
	if PATH="$FAKE_BIN:/usr/bin:/bin" \
		RKC_TEST_UNAME_SYSTEM=Linux \
		RKC_TEST_UNAME_MACHINE=x86_64 \
		"$PACKAGE/install.sh" --prefix "$tamper_prefix" \
		>"$WORK/tamper-$tamper_index.out" 2>"$WORK/tamper-$tamper_index.err"; then
		echo "install test: tampered package file unexpectedly worked: $relative" >&2
		exit 1
	fi
	grep -F "checksum mismatch: $relative" "$WORK/tamper-$tamper_index.err" >/dev/null
	test ! -e "$tamper_prefix"
	cp "$backup" "$tampered"
done

mkdir "$WORK/unsafe"
ln -s "$WORK/unsafe" "$WORK/linked-prefix"
if "$ROOT/install.sh" --skip-build --prefix "$WORK/linked-prefix" >"$WORK/unsafe.out" 2>"$WORK/unsafe.err"; then
	echo "install test: symlink prefix unexpectedly succeeded" >&2
	exit 1
fi
grep -F "destination is not a real directory" "$WORK/unsafe.err" >/dev/null

echo "install test: passed"
