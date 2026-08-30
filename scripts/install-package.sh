#!/bin/sh
set -eu

fail() {
	echo "rkc package install: $*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Install the checksum-verified Linux binaries from an RKC complete package.

Usage: ./install.sh [--prefix DIRECTORY]

Defaults:
  prefix      $RKC_INSTALL_PREFIX or $HOME/.local

Supported hosts: Linux x86_64/amd64 and aarch64/arm64.
No network connection or root access is required.
EOF
}

PREFIX_SET=0
PREFIX=
while [ "$#" -gt 0 ]; do
	case "$1" in
		--prefix)
			[ "$#" -ge 2 ] || { echo "rkc package install: --prefix requires a directory" >&2; exit 2; }
			[ "$PREFIX_SET" -eq 0 ] || { echo "rkc package install: --prefix may be supplied only once" >&2; exit 2; }
			PREFIX=$2
			PREFIX_SET=1
			shift 2
			;;
		--help|-h)
			usage
			exit 0
			;;
		*)
			echo "rkc package install: unknown argument: $1" >&2
			usage >&2
			exit 2
			;;
	esac
done

if [ -L "$0" ]; then
	fail "install.sh must be a regular package file, not a symlink"
fi
PACKAGE_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P) ||
	fail "cannot resolve the package root"
RECEIPT=$PACKAGE_ROOT/SHA256SUMS.txt
SOURCE_INSTALLER=$PACKAGE_ROOT/source/install.sh

if [ -L "$RECEIPT" ] || [ ! -f "$RECEIPT" ]; then
	fail "SHA256SUMS.txt is missing or unsafe"
fi

case "$(uname -s)" in
	Linux) ;;
	*) fail "unsupported operating system; packaged binaries support Linux only" ;;
esac
case "$(uname -m)" in
	x86_64|amd64) PLATFORM=linux-amd64 ;;
	aarch64|arm64) PLATFORM=linux-arm64 ;;
	*) fail "unsupported Linux architecture; expected x86_64/amd64 or aarch64/arm64" ;;
esac
BINARY_DIRECTORY=$PACKAGE_ROOT/artifacts/binaries/$PLATFORM
if [ -L "$BINARY_DIRECTORY" ] || [ ! -d "$BINARY_DIRECTORY" ]; then
	fail "selected prebuilt binary directory is missing or unsafe: $PLATFORM"
fi

receipt_digest() {
	target=$1
	found=0
	digest=
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
			*"  $target")
				candidate=${line%"  $target"}
				case "$candidate" in
					""|*[!0-9a-f]*) fail "invalid checksum receipt entry: $target" ;;
				esac
				[ "${#candidate}" -eq 64 ] || fail "invalid checksum receipt entry: $target"
				found=$((found + 1))
				digest=$candidate
				;;
		esac
	done < "$RECEIPT"
	[ "$found" -eq 1 ] || fail "checksum receipt must contain exactly one entry: $target"
	printf '%s\n' "$digest"
}

sha256_file() {
	path=$1
	if command -v sha256sum >/dev/null 2>&1; then
		line=$(sha256sum < "$path") || fail "cannot hash package file"
	elif command -v shasum >/dev/null 2>&1; then
		line=$(shasum -a 256 < "$path") || fail "cannot hash package file"
	else
		fail "sha256sum or shasum is required to verify this package"
	fi
	digest=${line%% *}
	case "$digest" in
		""|*[!0-9a-f]*) fail "checksum tool returned an invalid SHA-256 digest" ;;
	esac
	[ "${#digest}" -eq 64 ] || fail "checksum tool returned an invalid SHA-256 digest"
	printf '%s\n' "$digest"
}

verify_package_file() {
	relative=$1
	path=$PACKAGE_ROOT/$relative
	if [ -L "$path" ] || [ ! -f "$path" ]; then
		fail "package file is missing or unsafe: $relative"
	fi
	expected=$(receipt_digest "$relative")
	actual=$(sha256_file "$path")
	[ "$actual" = "$expected" ] || fail "checksum mismatch: $relative"
}

# Verify every executable input before delegating to any packaged program.
verify_package_file install.sh
verify_package_file source/install.sh
verify_package_file artifacts/binaries/$PLATFORM/rkc
verify_package_file artifacts/binaries/$PLATFORM/rkc-mcp

if [ "$PREFIX_SET" -eq 1 ]; then
	exec "$SOURCE_INSTALLER" \
		--skip-build \
		--prebuilt-binary-dir "$BINARY_DIRECTORY" \
		--prefix "$PREFIX"
fi
exec "$SOURCE_INSTALLER" \
	--skip-build \
	--prebuilt-binary-dir "$BINARY_DIRECTORY"
