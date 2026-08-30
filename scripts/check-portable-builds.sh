#!/bin/sh
set -eu

# Compile the CGO-free command binaries for the supported desktop/server
# targets. This is a portability contract, not a release publisher: artifacts
# stay in a private temporary directory and are discarded after the check.
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
python3 scripts/git_source_guard.py \
  --root "$ROOT" \
  --operation "portable target build"

VERSION=$(tr -d '\n' < VERSION)
case "$VERSION" in
  ''|*[!0-9A-Za-z._-]*)
    echo "portable builds: VERSION is not a safe release value" >&2
    exit 1
    ;;
esac
LDFLAGS="-s -w -X main.version=$VERSION"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-portable-build.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM

export GOENV=off
export GOFLAGS='-p=1 -mod=readonly'
export GOFIPS140=off
export GOWORK=off
unset GOEXPERIMENT GOAMD64 GOARM64

for target in \
  linux/amd64 linux/arm64 \
  darwin/amd64 darwin/arm64 \
  windows/amd64 windows/arm64; do
  os=${target%/*}
  architecture=${target#*/}
  output="$WORK/$os-$architecture"
  mkdir -p "$output"
  suffix=
  if [ "$os" = windows ]; then
    suffix=.exe
  fi
  case "$architecture" in
    amd64)
      GOAMD64=v1 GOOS="$os" GOARCH="$architecture" CGO_ENABLED=0 \
        go build -buildvcs=true -trimpath -ldflags="$LDFLAGS" \
        -o "$output/rkc$suffix" ./cmd/rkc
      GOAMD64=v1 GOOS="$os" GOARCH="$architecture" CGO_ENABLED=0 \
        go build -buildvcs=true -trimpath -ldflags="$LDFLAGS" \
        -o "$output/rkc-mcp$suffix" ./cmd/rkc-mcp
      ;;
    arm64)
      GOARM64=v8.0 GOOS="$os" GOARCH="$architecture" CGO_ENABLED=0 \
        go build -buildvcs=true -trimpath -ldflags="$LDFLAGS" \
        -o "$output/rkc$suffix" ./cmd/rkc
      GOARM64=v8.0 GOOS="$os" GOARCH="$architecture" CGO_ENABLED=0 \
        go build -buildvcs=true -trimpath -ldflags="$LDFLAGS" \
        -o "$output/rkc-mcp$suffix" ./cmd/rkc-mcp
      ;;
    *)
      echo "portable builds: unsupported architecture: $architecture" >&2
      exit 1
      ;;
  esac
  test -f "$output/rkc$suffix"
  test -f "$output/rkc-mcp$suffix"
  echo "portable target passed: $target"
done

echo "portable builds: all supported targets passed"
