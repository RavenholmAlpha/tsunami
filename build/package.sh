#!/usr/bin/env bash
#
# Build and package one release target.
#
#   build/package.sh -v v1.2.3 -t linux-amd64 -o dist
#   build/package.sh -t openwrt-aarch64 -P          # print the build plan only
#
# Targets are listed in build/targets.sh. SOURCE_DATE_EPOCH (default: the
# commit time) fixes every timestamp, and the binaries are built with
# -trimpath, so one commit packages to the same bytes with the same toolchain.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=targets.sh
source "$SCRIPT_DIR/targets.sh"

die() {
  printf 'package.sh: %s\n' "$*" >&2
  exit 1
}

VERSION="" VERSION_SET=0 TARGET="" OUT="" PLAN=0
while getopts "v:t:o:Ph" opt; do
  case "$opt" in
    v) VERSION="$OPTARG" VERSION_SET=1 ;;
    t) TARGET="$OPTARG" ;;
    o) OUT="$OPTARG" ;;
    P) PLAN=1 ;;
    h)
      sed -n '2,/^set /p' "$0" | sed -e '/^set /d' -e 's/^# \{0,1\}//'
      exit 0
      ;;
    *) die "usage: package.sh -v VERSION -t TARGET -o DIR [-P]" ;;
  esac
done

# Everything is validated before anything is created or built.
[ -n "$TARGET" ] || die "target is required (-t); known: $ALL_TARGETS"
target_info "$TARGET" || die "unknown target: $TARGET (known: $ALL_TARGETS)"
if [ "$VERSION_SET" = 0 ]; then
  [ "$PLAN" = 1 ] || die "version is required (-v)"
  VERSION="v0.0.0"
fi
valid_version "$VERSION" || die "invalid version: '$VERSION' (expected vMAJOR.MINOR.PATCH[-prerelease])"

ARCHIVE="tsunami-${VERSION}-${TARGET}.${T_EXT}"
MEMBERS="$(target_members "$TARGET")"

if [ "$PLAN" = 1 ]; then
  names=""
  while read -r name _mode _kind; do names="${names:+$names,}$name"; done <<<"$MEMBERS"
  printf 'goos=%s goarch=%s goarm=%s format=%s ext=%s go386=%s archive=%s files=%s\n' \
    "$T_GOOS" "$T_GOARCH" "$T_GOARM" "$T_FORMAT" "$T_EXT" "$T_GO386" "$ARCHIVE" "$names"
  exit 0
fi

[ -n "$OUT" ] || die "output directory is required (-o)"
PY="$(find_python)" || die "python 3.8 or newer is required"
command -v go >/dev/null 2>&1 || die "go is required"

EPOCH="${SOURCE_DATE_EPOCH:-}"
if [ -z "$EPOCH" ]; then EPOCH="$(git -C "$ROOT_DIR" log -1 --format=%ct 2>/dev/null || true)"; fi
if [ -z "$EPOCH" ]; then EPOCH="$(date +%s)"; fi
[[ "$EPOCH" =~ ^[0-9]+$ ]] || die "SOURCE_DATE_EPOCH must be an integer, got '$EPOCH'"

BUILD_TIME="$(date -u -d "@$EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$EPOCH" +%Y-%m-%dT%H:%M:%SZ)"
COMMIT="${COMMIT:-$(git -C "$ROOT_DIR" rev-parse --short=8 HEAD 2>/dev/null || echo unknown)}"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}"

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir "$STAGE/in"

build_binary() {
  local command="$1" output="$2"
  (cd "$ROOT_DIR" && env CGO_ENABLED=0 GOOS="$T_GOOS" GOARCH="$T_GOARCH" GOARM="$T_GOARM" GO386="$T_GO386" \
    go build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o "$output" "./cmd/$command")
}

ADD_ARGS=()
index=0
while read -r name mode kind; do
  index=$((index + 1))
  staged="in/$index"
  case "$kind" in
    bin)
      base="$(basename "$name")"
      command="${base#tsunami-}"
      command="${command%%-*}"
      printf 'package.sh: building %s for %s\n' "tsunami-$command" "$TARGET"
      build_binary "tsunami-$command" "$STAGE/$staged"
      ;;
    script) cp "$SCRIPT_DIR/openwrt/tsunami.init" "$STAGE/$staged" ;;
    data) cp "$SCRIPT_DIR/openwrt/tsunami.config" "$STAGE/$staged" ;;
    *) die "internal error: unknown member kind '$kind'" ;;
  esac
  ADD_ARGS+=(--add "$staged" "$name" "$mode")
done <<<"$MEMBERS"

(cd "$STAGE" && "$PY" "$SCRIPT_DIR/release_tool.py" archive \
  --format "$T_EXT" --epoch "$EPOCH" --out out.pkg "${ADD_ARGS[@]}")

mkdir -p "$OUT"
mv "$STAGE/out.pkg" "$OUT/$ARCHIVE"
printf 'package.sh: wrote %s/%s (%s bytes)\n' "$OUT" "$ARCHIVE" "$(wc -c <"$OUT/$ARCHIVE" | tr -d ' ')"
