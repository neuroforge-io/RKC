#!/bin/sh
set -eu

# Definitions precede the final invocation so a truncated piped download does
# not begin installation before the script has arrived in full.
rkc_fail() { echo "rkc install: $*" >&2; exit 1; }
rkc_quote() { printf "'"; printf '%s' "$1" | sed "s/'/'\\\\''/g"; printf "'"; }
rkc_hash() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum < "$1" | cut -d ' ' -f 1
  else
    shasum -a 256 < "$1" | cut -d ' ' -f 1
  fi
}
rkc_download() {
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent \
    --show-error --location --connect-timeout 15 --max-time 300 \
    --max-filesize "$3" --output "$2" "$1" ||
    rkc_fail "download failed; check the release/version and network connection"
}
rkc_directory() {
  [ ! -L "$1" ] || rkc_fail "destination directory is a symlink: $1"
  if [ -e "$1" ]; then
    [ -d "$1" ] || rkc_fail "destination is not a directory: $1"
  else
    rkc_directory "$(dirname -- "$1")"
    mkdir "$1"
  fi
}
rkc_install_main() {
  rkc_prefix=${RKC_INSTALL_PREFIX:-"$HOME/.local"}
  rkc_version=latest
  rkc_archive=
  rkc_checksums=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --prefix|--version|--archive|--checksums)
        [ "$#" -ge 2 ] && [ -n "$2" ] || rkc_fail "$1 requires a value"
        case "$1" in
          --prefix) rkc_prefix=$2 ;;
          --version) rkc_version=$2 ;;
          --archive) rkc_archive=$2 ;;
          --checksums) rkc_checksums=$2 ;;
        esac
        shift 2 ;;
      --help|-h)
        cat <<'EOF'
Install RKC for Linux or macOS (amd64/arm64), without root or a Go toolchain.
Usage: install-release.sh [--prefix DIRECTORY] [--version TAG]
Offline: install-release.sh --archive rkc-PLATFORM.zip --checksums SHA256SUMS.txt
Defaults: latest GitHub release; $RKC_INSTALL_PREFIX or $HOME/.local.
Requires curl, unzip, and sha256sum or shasum. No models are downloaded.
Python isolation, protected workbench commands, and models require Linux
user-systemd with delegated cgroup v2; this installer never disables guards.
EOF
        return 0 ;;
      *) rkc_fail "unknown argument: $1" ;;
    esac
  done
  case "$rkc_version" in ''|*[!a-zA-Z0-9._-]*) rkc_fail "invalid release tag" ;; esac
  while [ "${rkc_prefix%/}" != "$rkc_prefix" ]; do rkc_prefix=${rkc_prefix%/}; done
  case "$rkc_prefix" in ''|/|.|..) rkc_fail "unsafe install prefix" ;; esac
  rkc_newline='
'
  rkc_return=$(printf '\r')
  case "$rkc_prefix" in *"$rkc_newline"*|*"$rkc_return"*) rkc_fail "prefix contains a line break" ;; esac
  case "$(uname -s)" in Linux) rkc_os=linux ;; Darwin) rkc_os=darwin ;; *) rkc_fail "use the PowerShell installer on Windows; other operating systems are unsupported" ;; esac
  case "$(uname -m)" in x86_64|amd64) rkc_arch=amd64 ;; aarch64|arm64) rkc_arch=arm64 ;; *) rkc_fail "unsupported architecture; expected amd64 or arm64" ;; esac
  rkc_asset=rkc-$rkc_os-$rkc_arch.zip
  command -v unzip >/dev/null 2>&1 || rkc_fail "unzip is required"
  command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || rkc_fail "sha256sum or shasum is required"
  rkc_work=$(mktemp -d "${TMPDIR:-/tmp}/rkc-download.XXXXXX")
  rkc_temporary=
  trap 'if [ -n "$rkc_temporary" ]; then rm -f "$rkc_temporary"; fi; rm -rf "$rkc_work"' EXIT HUP INT TERM
  if [ -z "$rkc_archive" ] && [ -z "$rkc_checksums" ]; then
    command -v curl >/dev/null 2>&1 || rkc_fail "curl is required"
    rkc_base=https://github.com/neuroforge-io/RKC/releases
    if [ "$rkc_version" = latest ]; then rkc_base=$rkc_base/latest/download; else rkc_base=$rkc_base/download/$rkc_version; fi
    rkc_archive=$rkc_work/$rkc_asset
    rkc_checksums=$rkc_work/SHA256SUMS.txt
    rkc_download "$rkc_base/SHA256SUMS.txt" "$rkc_checksums" 65536
    rkc_download "$rkc_base/$rkc_asset" "$rkc_archive" 134217728
  elif [ -z "$rkc_archive" ] || [ -z "$rkc_checksums" ]; then
    rkc_fail "--archive and --checksums must be supplied together"
  fi
  for rkc_input in "$rkc_archive" "$rkc_checksums"; do
    [ ! -L "$rkc_input" ] && [ -f "$rkc_input" ] || rkc_fail "download input is missing or unsafe"
  done
  [ "$(wc -c < "$rkc_archive")" -le 134217728 ] || rkc_fail "archive exceeds 128 MiB"
  [ "$(wc -c < "$rkc_checksums")" -le 65536 ] || rkc_fail "checksum receipt exceeds 64 KiB"
  rkc_expected=$(awk -v name="$rkc_asset" '$2 == name { if (NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-f]/) exit 1; digest=$1; count++ } END { if (count != 1) exit 1; print digest }' "$rkc_checksums") || rkc_fail "receipt must contain exactly one valid checksum for $rkc_asset"
  [ "$(rkc_hash "$rkc_archive")" = "$rkc_expected" ] || rkc_fail "archive checksum mismatch; nothing was installed"
  unzip -Z -1 "$rkc_archive" > "$rkc_work/members" || rkc_fail "invalid ZIP archive"
  [ "$(wc -l < "$rkc_work/members")" -le 1024 ] || rkc_fail "too many archive files"
  # Reject nonregular ZIP entries and excessive expanded size before extraction.
  unzip -Z -l "$rkc_archive" > "$rkc_work/details" || rkc_fail "cannot inspect ZIP entries"
  awk 'length($1) == 10 && $1 ~ /^[-dlcbps?]/ { if (substr($1,1,1) != "-") exit 1; bytes += $4 } END { if (bytes > 268435456) exit 1 }' "$rkc_work/details" || rkc_fail "archive contains links, special files, or excessive expanded data"
  while IFS= read -r rkc_member; do
    case "$rkc_member" in ''|*[!a-zA-Z0-9_./@+!-]*|/*|../*|*/../*|*/..|./*|*/./*|*/.) rkc_fail "unsafe archive path" ;; esac
    case "$rkc_member" in bin/rkc|bin/rkc-mcp|share/doc/rkc/*|share/rkc/models/*|share/rkc/schemas/*) ;; *) rkc_fail "unexpected archive path: $rkc_member" ;; esac
  done < "$rkc_work/members"
  [ "$(LC_ALL=C sort "$rkc_work/members" | uniq -d | wc -l)" -eq 0 ] || rkc_fail "archive contains duplicate paths"
  mkdir "$rkc_work/unpacked"
  unzip -q -P '' "$rkc_archive" -d "$rkc_work/unpacked" || rkc_fail "cannot extract verified archive"
  for rkc_required in bin/rkc bin/rkc-mcp share/doc/rkc/portable-manifest.json share/doc/rkc/VERSION; do
    [ -f "$rkc_work/unpacked/$rkc_required" ] && [ ! -L "$rkc_work/unpacked/$rkc_required" ] || rkc_fail "archive omits a required file"
  done
  rkc_directory "$rkc_prefix"
  rkc_prefix=$(CDPATH= cd -- "$rkc_prefix" && pwd -P)
  # Preflight every destination before replacing any installed file.
  while IFS= read -r rkc_member; do
    rkc_destination=$rkc_prefix/$rkc_member
    rkc_directory "$(dirname -- "$rkc_destination")"
    [ ! -L "$rkc_destination" ] || rkc_fail "destination is a symlink: $rkc_destination"
    [ ! -e "$rkc_destination" ] || [ -f "$rkc_destination" ] || rkc_fail "destination is not a regular file: $rkc_destination"
  done < "$rkc_work/members"
  while IFS= read -r rkc_member; do
    rkc_destination=$rkc_prefix/$rkc_member
    rkc_temporary=$(mktemp "$(dirname -- "$rkc_destination")/.rkc-install.XXXXXX")
    cp "$rkc_work/unpacked/$rkc_member" "$rkc_temporary"
    case "$rkc_member" in bin/*) chmod 0755 "$rkc_temporary" ;; *) chmod 0644 "$rkc_temporary" ;; esac
    mv -f "$rkc_temporary" "$rkc_destination"
    rkc_temporary=
  done < "$rkc_work/members"
  printf 'RKC installed in %s/bin\nFirst run: ' "$rkc_prefix"
  rkc_quote "$rkc_prefix/bin/rkc"
  printf ' gui\n'
  case "$rkc_prefix" in *:*) echo "Use the full binary path; ':' cannot be represented in PATH." ;; *) printf 'For this shell: export PATH='; rkc_quote "$rkc_prefix/bin"; printf ':"$PATH"\n' ;; esac
  echo "Protected workbench, Python isolation, and model execution require Linux user-systemd/cgroup v2."
}

rkc_install_main "$@"
