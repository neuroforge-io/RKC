#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
SOURCE_COMMIT=$(git rev-parse --verify 'HEAD^{commit}')
SOURCE_TREE=$(git rev-parse --verify "${SOURCE_COMMIT}^{tree}")
SOURCE_DATE_EPOCH=$(git show -s --format=%ct "$SOURCE_COMMIT")
case "$SOURCE_COMMIT:$SOURCE_TREE:$SOURCE_DATE_EPOCH" in
  *[!0-9a-f:]*|*::*|:*)
    echo "release binaries: invalid Git source identity" >&2
    exit 1
    ;;
esac
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
SOURCE=$WORK/source
git clone --quiet --no-hardlinks --no-checkout -- "$ROOT" "$SOURCE"
git -C "$SOURCE" -c advice.detachedHead=false checkout --quiet --detach "$SOURCE_COMMIT"
if [ "$(git -C "$SOURCE" rev-parse HEAD)" != "$SOURCE_COMMIT" ] ||
   [ "$(git -C "$SOURCE" rev-parse 'HEAD^{tree}')" != "$SOURCE_TREE" ] ||
   [ -n "$(git -C "$SOURCE" status --porcelain=v1 --untracked-files=all)" ]; then
  echo "release binaries: private source checkout does not match immutable HEAD" >&2
  exit 1
fi
VERSION=$(tr -d '\n' < "$SOURCE/VERSION")
LDFLAGS="-s -w -X main.version=$VERSION"
export GOENV=off
export GOFLAGS='-p=1 -modcacherw'
export GOFIPS140=off
export GOTOOLCHAIN=local
export GOWORK=off
unset GOEXPERIMENT GOAMD64 GOARM64
(
  cd "$SOURCE"
  go mod download
  go mod verify
)
for architecture in amd64 arm64; do
  mkdir -p "$WORK/linux-$architecture"
  (
    cd "$SOURCE"
    case "$architecture" in
      amd64)
        GOEXPERIMENT= GOAMD64=v1 GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath -ldflags="$LDFLAGS" -o "$WORK/linux-amd64/rkc" ./cmd/rkc
        GOEXPERIMENT= GOAMD64=v1 GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath -ldflags="$LDFLAGS" -o "$WORK/linux-amd64/rkc-mcp" ./cmd/rkc-mcp
        ;;
      arm64)
        GOEXPERIMENT= GOARM64=v8.0 GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath -ldflags="$LDFLAGS" -o "$WORK/linux-arm64/rkc" ./cmd/rkc
        GOEXPERIMENT= GOARM64=v8.0 GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath -ldflags="$LDFLAGS" -o "$WORK/linux-arm64/rkc-mcp" ./cmd/rkc-mcp
        ;;
      *)
        echo "release binaries: unsupported architecture: $architecture" >&2
        exit 1
        ;;
    esac
  )
  python3 "$SOURCE/scripts/generate-go-sbom.py" \
    --binary "$WORK/linux-$architecture/rkc" \
    --output "$WORK/linux-$architecture/rkc.spdx.json" \
    --lock "$SOURCE/third_party/go-modules.lock.json" \
    --source-root "$SOURCE" \
    --source-commit "$SOURCE_COMMIT" \
    --source-tree "$SOURCE_TREE" \
    --source-date-epoch "$SOURCE_DATE_EPOCH" \
    --goos linux \
    --goarch "$architecture" \
    --version "$VERSION"
  python3 "$SOURCE/scripts/generate-go-sbom.py" \
    --binary "$WORK/linux-$architecture/rkc-mcp" \
    --output "$WORK/linux-$architecture/rkc-mcp.spdx.json" \
    --lock "$SOURCE/third_party/go-modules.lock.json" \
    --source-root "$SOURCE" \
    --source-commit "$SOURCE_COMMIT" \
    --source-tree "$SOURCE_TREE" \
    --source-date-epoch "$SOURCE_DATE_EPOCH" \
    --goos linux \
    --goarch "$architecture" \
    --version "$VERSION"
done
if [ -n "$(git -C "$SOURCE" status --porcelain=v1 --untracked-files=all)" ]; then
  echo "release binaries: immutable source checkout changed during build" >&2
  exit 1
fi

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
  publish_file "$WORK/linux-$architecture/rkc.spdx.json" "$platform/rkc.spdx.json"
  publish_file "$WORK/linux-$architecture/rkc-mcp.spdx.json" "$platform/rkc-mcp.spdx.json"
done

for notice in LICENSE NOTICE THIRD_PARTY_NOTICES.md; do
  source_notice=$SOURCE/$notice
  if [ -f "$source_notice" ] && [ ! -L "$source_notice" ]; then
    publish_file "$source_notice" "$TARGET/$notice"
    for platform in "$TARGET/linux-amd64" "$TARGET/linux-arm64"; do
      publish_file "$source_notice" "$platform/$notice"
    done
  fi
done
if [ ! -d "$SOURCE/LICENSES" ] || [ -L "$SOURCE/LICENSES" ]; then
  echo "release binaries: LICENSES must be a real directory" >&2
  exit 1
fi
unsafe_license=$(find "$SOURCE/LICENSES" \( -type l -o \( ! -type d ! -type f \) \) -print -quit)
if [ -n "$unsafe_license" ]; then
  echo "release binaries: unsafe license entry: $unsafe_license" >&2
  exit 1
fi
mkdir -p "$TARGET/LICENSES" "$TARGET/linux-amd64/LICENSES" "$TARGET/linux-arm64/LICENSES"
find "$SOURCE/LICENSES" -type f -print | LC_ALL=C sort | while IFS= read -r license; do
  relative=${license#"$SOURCE/LICENSES/"}
  for destination in \
    "$TARGET/LICENSES/$relative" \
    "$TARGET/linux-amd64/LICENSES/$relative" \
    "$TARGET/linux-arm64/LICENSES/$relative"; do
    mkdir -p "$(dirname "$destination")"
    publish_file "$license" "$destination"
  done
done

MODULE_LOCK=third_party/go-modules.lock.json
SOURCE_MODULE_LOCK=$SOURCE/$MODULE_LOCK
if [ ! -f "$SOURCE_MODULE_LOCK" ] || [ -L "$SOURCE_MODULE_LOCK" ]; then
  echo "release binaries: audited Go module lock is missing or unsafe" >&2
  exit 1
fi
for destination in \
  "$TARGET/$MODULE_LOCK" \
  "$TARGET/linux-amd64/$MODULE_LOCK" \
  "$TARGET/linux-arm64/$MODULE_LOCK"; do
  mkdir -p "$(dirname "$destination")"
  publish_file "$SOURCE_MODULE_LOCK" "$destination"
done

echo "release binaries: published to $TARGET"
