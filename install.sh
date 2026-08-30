#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PREFIX=${RKC_INSTALL_PREFIX:-"$HOME/.local"}
BUILD=1

usage() {
	cat <<'EOF'
Install RKC from this verified source checkout.

Usage: ./install.sh [--prefix DIRECTORY] [--skip-build]

Defaults:
  prefix      $RKC_INSTALL_PREFIX or $HOME/.local
  build       enabled; uses RKC's low-priority guard when available on Linux
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--prefix)
			[ "$#" -ge 2 ] || { echo "install: --prefix requires a directory" >&2; exit 2; }
			PREFIX=$2
			shift 2
			;;
		--skip-build)
			BUILD=0
			shift
			;;
		--help|-h)
			usage
			exit 0
			;;
		*)
			echo "install: unknown argument: $1" >&2
			usage >&2
			exit 2
			;;
	esac
done

case "$PREFIX" in
	""|/|.|..)
		echo "install: refusing unsafe prefix: $PREFIX" >&2
		exit 2
		;;
esac

cd "$ROOT"
if [ "$BUILD" -eq 1 ]; then
	if [ "$(uname -s)" = Linux ] &&
	   command -v systemctl >/dev/null 2>&1 &&
	   systemctl --user is-system-running >/dev/null 2>&1; then
		make safe-build
	else
		make build
	fi
fi

BIN_DIRECTORY=$PREFIX/bin
DOC_DIRECTORY=$PREFIX/share/doc/rkc
DATA_DIRECTORY=$PREFIX/share/rkc
for directory in \
	"$PREFIX" \
	"$BIN_DIRECTORY" \
	"$DOC_DIRECTORY" \
	"$DATA_DIRECTORY/models/qualification" \
	"$DATA_DIRECTORY/schemas"; do
	if [ -L "$directory" ] || { [ -e "$directory" ] && [ ! -d "$directory" ]; }; then
		echo "install: destination is not a real directory: $directory" >&2
		exit 1
	fi
	mkdir -p "$directory"
done

install_file() {
	source=$1
	destination=$2
	mode=$3
	if [ -L "$source" ] || [ ! -f "$source" ]; then
		echo "install: source is missing or unsafe: $source" >&2
		exit 1
	fi
	if [ -L "$destination" ] || { [ -e "$destination" ] && [ ! -f "$destination" ]; }; then
		echo "install: refusing unsafe destination: $destination" >&2
		exit 1
	fi
	temporary=$(mktemp "$(dirname "$destination")/.rkc-install.XXXXXX")
	trap 'rm -f "$temporary"' EXIT HUP INT TERM
	cp "$source" "$temporary"
	chmod "$mode" "$temporary"
	mv -f "$temporary" "$destination"
	trap - EXIT HUP INT TERM
}

install_file "$ROOT/bin/rkc" "$BIN_DIRECTORY/rkc" 0755
install_file "$ROOT/bin/rkc-mcp" "$BIN_DIRECTORY/rkc-mcp" 0755
install_file "$ROOT/LICENSE" "$DOC_DIRECTORY/LICENSE" 0644
install_file "$ROOT/NOTICE" "$DOC_DIRECTORY/NOTICE" 0644
install_file "$ROOT/THIRD_PARTY_NOTICES.md" "$DOC_DIRECTORY/THIRD_PARTY_NOTICES.md" 0644
install_file "$ROOT/models/models.lock.json" "$DATA_DIRECTORY/models/models.lock.json" 0644
install_file \
	"$ROOT/models/qualification/rkc-local-model-v1.json" \
	"$DATA_DIRECTORY/models/qualification/rkc-local-model-v1.json" \
	0644
install_file \
	"$ROOT/schemas/model-lock.schema.json" \
	"$DATA_DIRECTORY/schemas/model-lock.schema.json" \
	0644
install_file \
	"$ROOT/schemas/model-qualification.schema.json" \
	"$DATA_DIRECTORY/schemas/model-qualification.schema.json" \
	0644

echo "RKC installed in $BIN_DIRECTORY"
case ":$PATH:" in
	*":$BIN_DIRECTORY:"*) ;;
	*) echo "Add $BIN_DIRECTORY to PATH." ;;
esac
echo "First run: rkc open /path/to/repository"
