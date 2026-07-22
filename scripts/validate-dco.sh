#!/bin/sh
set -eu

base=${1:-}
head=${2:-HEAD}
approved_import_root=${RKC_DCO_IMPORT_ROOT:-d70cd3f7f2d76b2eb21c20c8a3bb1908d9c9f11b}

case "$head" in
  -* )
    echo "DCO validation: invalid head commit: $head" >&2
    exit 1
    ;;
esac
if ! git cat-file -e "$head^{commit}" 2>/dev/null; then
  echo "DCO validation: head commit is unavailable: $head" >&2
  exit 1
fi

case "$base" in
  "")
    zero_base=true
    ;;
  *[!0]*)
    zero_base=false
    ;;
  *)
    zero_base=true
    ;;
esac

if ! git cat-file -e "$approved_import_root^{commit}" 2>/dev/null; then
  echo "DCO validation: approved import root is unavailable: $approved_import_root" >&2
  exit 1
fi
root_record=$(git rev-list --parents -n 1 "$approved_import_root")
# The sole policy exemption is the original root import. Refuse an override
# that points at an ordinary descendant or a second, unrelated history.
set -- $root_record
if [ "$#" -ne 1 ] || [ "$1" != "$approved_import_root" ]; then
  echo "DCO validation: approved import commit is not a repository root: $approved_import_root" >&2
  exit 1
fi
if ! git merge-base --is-ancestor "$approved_import_root" "$head"; then
  echo "DCO validation: head is not descended from the approved import root" >&2
  exit 1
fi

if [ "$zero_base" = true ]; then
  range=$approved_import_root..$head
else
  case "$base" in
    -* )
      echo "DCO validation: invalid base commit: $base" >&2
      exit 1
      ;;
  esac
  if ! git cat-file -e "$base^{commit}" 2>/dev/null; then
    echo "DCO validation: base commit is unavailable: $base" >&2
    exit 1
  fi
  range=$base..$head
fi

commits=$(git rev-list --reverse --no-merges "$range")
[ -n "$commits" ] || { echo "DCO validation: no non-merge commits selected"; exit 0; }
failed=0
for commit in $commits; do
  author=$(git show -s --format='%an <%ae>' "$commit")
  trailers=$(git show -s --format=%B "$commit" | git interpret-trailers --parse)
  if ! printf '%s\n' "$trailers" | awk -v expected="$author" '
    BEGIN { found = 0 }
    {
      separator = index($0, ":")
      if (separator == 0) {
        next
      }
      key = tolower(substr($0, 1, separator - 1))
      value = substr($0, separator + 1)
      sub(/^[[:space:]]+/, "", value)
      sub(/[[:space:]]+$/, "", value)
      if (key == "signed-off-by" && value == expected &&
          value ~ /^.+ <[^<>[:space:]]+@[^<>[:space:]]+>$/) {
        found = 1
      }
    }
    END { exit(found ? 0 : 1) }
  '; then
    echo "DCO validation: missing author-matching Signed-off-by trailer: $commit ($author)" >&2
    failed=1
  fi
done
[ "$failed" -eq 0 ] || exit 1
echo "DCO validation: passed"
