#!/usr/bin/env bash
#
# Smoke-test a packaged release archive.
#
#   build/smoke.sh -t linux-arm64 -v v1.2.3 dist/tsunami-v1.2.3-linux-arm64.tar.gz
#
# In order: the archive holds exactly the expected files and modes; every binary
# is built for the target's architecture; the OpenWrt init script is a plain LF
# script; and, if the binary can run on this host, --version reports VERSION.
#
# SMOKE_RUN=auto (default) runs what this host can run, natively or through
# qemu-user for linux arm and arm64. always fails when it cannot run the
# binary. never checks the files only.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=targets.sh
source "$SCRIPT_DIR/targets.sh"

die() {
  printf 'smoke.sh: %s\n' "$*" >&2
  exit 1
}

TARGET="" VERSION=""
while getopts "t:v:h" opt; do
  case "$opt" in
    t) TARGET="$OPTARG" ;;
    v) VERSION="$OPTARG" ;;
    h)
      sed -n '2,/^set /p' "$0" | sed -e '/^set /d' -e 's/^# \{0,1\}//'
      exit 0
      ;;
    *) die "usage: smoke.sh -t TARGET -v VERSION ARCHIVE" ;;
  esac
done
shift $((OPTIND - 1))
ARCHIVE="${1:-}"

[ -n "$TARGET" ] || die "target is required (-t)"
target_info "$TARGET" || die "unknown target: $TARGET"
valid_version "$VERSION" || die "invalid version: '$VERSION'"
[ -f "$ARCHIVE" ] || die "archive not found: '$ARCHIVE'"
RUN_MODE="${SMOKE_RUN:-auto}"
case "$RUN_MODE" in auto | always | never) ;; *) die "SMOKE_RUN must be auto, always or never" ;; esac
PY="$(find_python)" || die "python 3.8 or newer is required"
TOOL="$SCRIPT_DIR/release_tool.py"

# Work on a copy under relative names so path conversion never matters.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cp "$ARCHIVE" "$WORK/a.pkg"
cd "$WORK"

expected="$(target_members "$TARGET" | awk '{print $1, $2, "f"}' | LC_ALL=C sort)"
actual="$("$PY" "$TOOL" list a.pkg | LC_ALL=C sort)"
if [ "$expected" != "$actual" ]; then
  printf 'expected:\n%s\nactual:\n%s\n' "$expected" "$actual" >&2
  die "archive contents differ from the expected layout for $TARGET"
fi
mkdir x
"$PY" "$TOOL" extract a.pkg x

case "$(uname -s)" in
  Linux) HOST_OS=linux ;;
  Darwin) HOST_OS=darwin ;;
  MINGW* | MSYS* | CYGWIN*) HOST_OS=windows ;;
  *) HOST_OS=unknown ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) HOST_ARCH=amd64 ;;
  aarch64 | arm64) HOST_ARCH=arm64 ;;
  i?86) HOST_ARCH=386 ;;
  armv7* | armv8l) HOST_ARCH=arm ;;
  *) HOST_ARCH=unknown ;;
esac

# A 64-bit x86 host also runs 386 binaries (Linux ia32 emulation, Windows WOW64).
runnable=0
PREFIX=()
if [ "$T_RUNOS" = "$HOST_OS" ]; then
  if [ "$T_GOARCH" = "$HOST_ARCH" ] || { [ "$HOST_ARCH" = amd64 ] && [ "$T_GOARCH" = 386 ]; }; then
    runnable=1
  elif [ "$HOST_OS" = linux ] && emulator="$(find_emulator "$T_GOARCH")"; then
    runnable=1
    PREFIX=("$emulator")
  fi
fi
if [ "$RUN_MODE" = always ] && [ "$runnable" = 0 ]; then
  die "cannot execute $TARGET binaries on $HOST_OS/$HOST_ARCH"
fi

while read -r name _mode kind; do
  case "$kind" in
    bin)
      format="$("$PY" "$TOOL" arch "x/$name")"
      [ "$format" = "$T_BINFMT" ] ||
        die "architecture mismatch for $name: built for $format, expected $T_BINFMT"
      ;;
    script)
      # Count bytes with tr: Git Bash's grep and $(...) both hide a trailing CR.
      if [ "$(tr -cd '\r' <"x/$name" | wc -c | tr -d ' ')" != 0 ]; then
        die "$name contains carriage returns (CRLF line endings); procd cannot run it"
      fi
      [ "$(head -n 1 "x/$name")" = '#!/bin/sh /etc/rc.common' ] ||
        die "$name does not start with '#!/bin/sh /etc/rc.common'"
      ;;
  esac
done <<<"$(target_members "$TARGET")"

if [ "$RUN_MODE" = never ] || [ "$runnable" = 0 ]; then
  printf 'smoke.sh: %s: contents and architecture OK; not executed on %s/%s\n' "$TARGET" "$HOST_OS" "$HOST_ARCH"
  exit 0
fi

while read -r name _mode kind; do
  [ "$kind" = bin ] || continue
  base="$(basename "$name")"
  command="${base#tsunami-}"
  command="${command%%-*}"
  output="$("${PREFIX[@]+"${PREFIX[@]}"}" "x/$name" --version)" || die "$name --version failed"
  printf '%s\n' "$output"
  case "$output" in
    "tsunami-$command $VERSION (commit="*) ;;
    *) die "version mismatch for $name: expected 'tsunami-$command $VERSION (commit=...', got '$output'" ;;
  esac
done <<<"$(target_members "$TARGET")"

printf 'smoke.sh: %s: OK\n' "$TARGET"
