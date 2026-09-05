#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
TARGET=dist/binaries
PLATFORMS='linux-amd64 linux-arm64'
case "${1-}" in
  '') ;;
  --portable)
    TARGET=dist/portable-binaries
    PLATFORMS='linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64'
    shift
    ;;
  *) echo "Usage: build-release-binaries.sh [--portable]" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { echo "release binaries: unexpected arguments" >&2; exit 2; }
python3 scripts/git_source_guard.py \
  --root "$ROOT" \
  --operation "release binary build"
SOURCE_COMMIT=$(git rev-parse --verify 'HEAD^{commit}')
SOURCE_TREE=$(git rev-parse --verify "${SOURCE_COMMIT}^{tree}")
SOURCE_DATE_EPOCH=$(git show -s --format=%ct "$SOURCE_COMMIT")
case "$SOURCE_COMMIT:$SOURCE_TREE:$SOURCE_DATE_EPOCH" in
  *[!0-9a-f:]*|*::*|:*)
    echo "release binaries: invalid Git source identity" >&2
    exit 1
    ;;
esac
if [ -L dist ] || { [ -e dist ] && [ ! -d dist ]; }; then
  echo "release binaries: dist must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
if [ -L "$TARGET" ] || { [ -e "$TARGET" ] && [ ! -d "$TARGET" ]; }; then
  echo "release binaries: $TARGET must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
mkdir -p "$TARGET"
for platform in $PLATFORMS; do
  directory=$TARGET/$platform
  [ ! -L "$directory" ] || { echo "release binaries: unsafe platform directory" >&2; exit 1; }
  mkdir -p "$directory"
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
TOOLCHAIN=$(awk '$1 == "toolchain" && NF == 2 { print $2 }' "$SOURCE/go.mod")
case "$TOOLCHAIN" in
  go[0-9]*.[0-9]*.[0-9]*) ;;
  *)
    echo "release binaries: go.mod must declare one exact Go toolchain" >&2
    exit 1
    ;;
esac
LDFLAGS="-s -w -X main.version=$VERSION"
export GOENV=off
export GOFLAGS='-p=1 -modcacherw'
export GOFIPS140=off
export GOTOOLCHAIN=$TOOLCHAIN
export GOWORK=off
unset GOEXPERIMENT GOAMD64 GOARM64
case "$(go version)" in
  "go version $TOOLCHAIN "*) ;;
  *)
    echo "release binaries: resolved Go executable does not match $TOOLCHAIN" >&2
    exit 1
    ;;
esac
(
  cd "$SOURCE"
  go mod download
  go mod verify
)
for platform in $PLATFORMS; do
  operating_system=${platform%-*}
  architecture=${platform#*-}
  suffix=
  [ "$operating_system" != windows ] || suffix=.exe
  mkdir -p "$WORK/$platform"
  (
    cd "$SOURCE"
    case "$architecture" in
      amd64)
        GOEXPERIMENT= GOAMD64=v1 GOOS="$operating_system" GOARCH=amd64 CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath -ldflags="$LDFLAGS" -o "$WORK/$platform/rkc$suffix" ./cmd/rkc
        GOEXPERIMENT= GOAMD64=v1 GOOS="$operating_system" GOARCH=amd64 CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath -ldflags="$LDFLAGS" -o "$WORK/$platform/rkc-mcp$suffix" ./cmd/rkc-mcp
        ;;
      arm64)
        GOEXPERIMENT= GOARM64=v8.0 GOOS="$operating_system" GOARCH=arm64 CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath -ldflags="$LDFLAGS" -o "$WORK/$platform/rkc$suffix" ./cmd/rkc
        GOEXPERIMENT= GOARM64=v8.0 GOOS="$operating_system" GOARCH=arm64 CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath -ldflags="$LDFLAGS" -o "$WORK/$platform/rkc-mcp$suffix" ./cmd/rkc-mcp
        ;;
      *)
        echo "release binaries: unsupported architecture: $architecture" >&2
        exit 1
        ;;
    esac
  )
  python3 "$SOURCE/scripts/generate-go-sbom.py" \
    --binary "$WORK/$platform/rkc$suffix" \
    --output "$WORK/$platform/rkc.spdx.json" \
    --lock "$SOURCE/third_party/go-modules.lock.json" \
    --source-root "$SOURCE" \
    --source-commit "$SOURCE_COMMIT" \
    --source-tree "$SOURCE_TREE" \
    --source-date-epoch "$SOURCE_DATE_EPOCH" \
    --goos "$operating_system" \
    --goarch "$architecture" \
    --version "$VERSION"
  python3 "$SOURCE/scripts/generate-go-sbom.py" \
    --binary "$WORK/$platform/rkc-mcp$suffix" \
    --output "$WORK/$platform/rkc-mcp.spdx.json" \
    --lock "$SOURCE/third_party/go-modules.lock.json" \
    --source-root "$SOURCE" \
    --source-commit "$SOURCE_COMMIT" \
    --source-tree "$SOURCE_TREE" \
    --source-date-epoch "$SOURCE_DATE_EPOCH" \
    --goos "$operating_system" \
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
  python3 "$SOURCE/scripts/publish_file.py" \
    --source "$source" \
    --destination "$destination" \
    --repository-root "$ROOT" \
    --mode "$mode"
}

python3 "$SOURCE/scripts/git_source_guard.py" \
  --root "$ROOT" \
  --operation "release binary publication"

for platform in $PLATFORMS; do
  suffix=
  case "$platform" in windows-*) suffix=.exe ;; esac
  publish_file "$WORK/$platform/rkc$suffix" "$TARGET/$platform/rkc$suffix"
  publish_file "$WORK/$platform/rkc-mcp$suffix" "$TARGET/$platform/rkc-mcp$suffix"
  publish_file "$WORK/$platform/rkc.spdx.json" "$TARGET/$platform/rkc.spdx.json"
  publish_file "$WORK/$platform/rkc-mcp.spdx.json" "$TARGET/$platform/rkc-mcp.spdx.json"
done

for notice in LICENSE NOTICE THIRD_PARTY_NOTICES.md; do
  source_notice=$SOURCE/$notice
  if [ -f "$source_notice" ] && [ ! -L "$source_notice" ]; then
    publish_file "$source_notice" "$TARGET/$notice"
    for platform in $PLATFORMS; do
      publish_file "$source_notice" "$TARGET/$platform/$notice"
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
mkdir -p "$TARGET/LICENSES"
find "$SOURCE/LICENSES" -type f -print | LC_ALL=C sort | while IFS= read -r license; do
  relative=${license#"$SOURCE/LICENSES/"}
  for platform in . $PLATFORMS; do
    destination=$TARGET/$platform/LICENSES/$relative
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
for platform in . $PLATFORMS; do
  destination=$TARGET/$platform/$MODULE_LOCK
  mkdir -p "$(dirname "$destination")"
  publish_file "$SOURCE_MODULE_LOCK" "$destination"
done

echo "release binaries: published to $TARGET"
