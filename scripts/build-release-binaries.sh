#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
TARGET=dist/binaries
if [ -L dist ] || { [ -e dist ] && [ ! -d dist ]; }; then
  echo "release binaries: dist must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
if [ -L "$TARGET" ] || { [ -e "$TARGET" ] && [ ! -d "$TARGET" ]; }; then
  echo "release binaries: $TARGET must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
mkdir -p "$TARGET/linux-amd64" "$TARGET/linux-arm64"
for directory in "$TARGET/linux-amd64" "$TARGET/linux-arm64"; do
  if [ -L "$directory" ] || [ ! -d "$directory" ]; then
    echo "release binaries: unsafe platform directory: $directory" >&2
    exit 1
  fi
done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-release-binaries.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
VERSION=$(tr -d '\n' < VERSION)
LDFLAGS="-s -w -X main.version=$VERSION"
for architecture in amd64 arm64; do
  mkdir -p "$WORK/linux-$architecture"
  GOOS=linux GOARCH=$architecture CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o "$WORK/linux-$architecture/rkc" ./cmd/rkc
  GOOS=linux GOARCH=$architecture CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o "$WORK/linux-$architecture/rkc-mcp" ./cmd/rkc-mcp
done

publish_file() {
  source=$1
  destination=$2
  if [ -x "$source" ]; then
    mode=0755
  else
    mode=0644
  fi
  python3 scripts/publish_file.py \
    --source "$source" \
    --destination "$destination" \
    --mode "$mode"
}

for architecture in amd64 arm64; do
  platform="$TARGET/linux-$architecture"
  publish_file "$WORK/linux-$architecture/rkc" "$platform/rkc"
  publish_file "$WORK/linux-$architecture/rkc-mcp" "$platform/rkc-mcp"
done

for notice in LICENSE NOTICE THIRD_PARTY_NOTICES.md; do
  if [ -f "$notice" ] && [ ! -L "$notice" ]; then
    publish_file "$notice" "$TARGET/$notice"
    for platform in "$TARGET/linux-amd64" "$TARGET/linux-arm64"; do
      publish_file "$notice" "$platform/$notice"
    done
  fi
done
if [ -d LICENSES ] && [ ! -L LICENSES ]; then
  mkdir -p "$TARGET/LICENSES" "$TARGET/linux-amd64/LICENSES" "$TARGET/linux-arm64/LICENSES"
  for license in LICENSES/*; do
    [ -f "$license" ] || continue
    [ ! -L "$license" ] || { echo "release binaries: refusing symlinked license $license" >&2; exit 1; }
    name=$(basename "$license")
    publish_file "$license" "$TARGET/LICENSES/$name"
    publish_file "$license" "$TARGET/linux-amd64/LICENSES/$name"
    publish_file "$license" "$TARGET/linux-arm64/LICENSES/$name"
  done
fi

echo "release binaries: published to $TARGET"
