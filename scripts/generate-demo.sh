#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"
python3 scripts/git_source_guard.py \
  --root "$ROOT" \
  --operation "release demo generation"
SOURCE_COMMIT=$(git rev-parse --verify 'HEAD^{commit}')
SOURCE_TREE=$(git rev-parse --verify "${SOURCE_COMMIT}^{tree}")
OUT=dist/demo
if [ -L dist ] || { [ -e dist ] && [ ! -d dist ]; }; then
  echo "demo: dist must be a real directory, not a symlink or non-directory" >&2
  exit 1
fi
mkdir -p dist
WORK=$(mktemp -d "${TMPDIR:-/tmp}/rkc-demo.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
SOURCE=$WORK/source
git clone --quiet --no-hardlinks --no-checkout -- "$ROOT" "$SOURCE"
git -C "$SOURCE" -c advice.detachedHead=false checkout --quiet --detach "$SOURCE_COMMIT"
git -C "$SOURCE" remote set-url origin https://github.com/neuroforge-io/RKC.git
if [ "$(git -C "$SOURCE" rev-parse 'HEAD^{tree}')" != "$SOURCE_TREE" ] ||
   [ -n "$(git -C "$SOURCE" status --porcelain=v1 --untracked-files=all)" ]; then
  echo "demo: private source checkout does not match immutable HEAD" >&2
  exit 1
fi
VERSION=$(tr -d '\n' < "$SOURCE/VERSION")
export GOENV=off
export GOFLAGS='-p=1 -modcacherw'
export GOFIPS140=off
export GOTOOLCHAIN=local
export GOWORK=off
unset GOEXPERIMENT GOAMD64 GOARM64
(
  cd "$SOURCE"
  CGO_ENABLED=0 go build -mod=readonly -buildvcs=true -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" -o "$WORK/rkc" ./cmd/rkc
)
run_and_publish() {
  destination=$1
  shift
  temporary="$WORK/$destination"
  "$@" >"$temporary"
  python3 "$SOURCE/scripts/git_source_guard.py" \
    --root "$ROOT" \
    --operation "release demo publication"
  python3 "$SOURCE/scripts/publish_file.py" \
    --source "$temporary" \
    --destination "dist/$destination" \
    --repository-root "$ROOT" \
    --mode 0644
}
run_and_publish demo-scan.txt "$WORK/rkc" scan --out "$OUT" --force "$SOURCE/examples"
run_and_publish demo-check.txt "$WORK/rkc" check --coverage "$OUT/coverage.json" --min-inventory-accounting 1 --min-symbol-evidence 1 --min-edge-resolution 0.5 --max-errors 0 --max-high-confidence-secrets 0
run_and_publish demo-synthesis.txt "$WORK/rkc" synthesize --dir "$OUT" --repo-root "$SOURCE/examples" --packet-only --query Login --limit 1 --force
