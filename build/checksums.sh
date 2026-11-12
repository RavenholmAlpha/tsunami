#!/usr/bin/env bash
#
# Write DIR/checksums.sha256 covering every file in DIR, one
# "<sha256>  <name>" line per file in byte order. scripts/install.sh looks up
# its download with the same pattern, so keep the two-space format.
#
#   build/checksums.sh dist
set -Eeuo pipefail

die() {
  printf 'checksums.sh: %s\n' "$*" >&2
  exit 1
}

DIR="${1:-}"
[ -n "$DIR" ] || die "usage: checksums.sh DIR"
[ -d "$DIR" ] || die "no files to checksum: '$DIR' is not a directory"

# Byte order for the glob below; locale collation would reorder the lines.
export LC_ALL=C

# The file is read from stdin so no path appears in the tool's output: GNU
# sha256sum prefixes a line with "\" when the path holds a backslash (any file
# under C:\...), and scripts/install.sh anchors on the 64 hex digits.
hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum <"$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 <"$1" | awk '{print $1}'
  else
    die "sha256sum or shasum is required"
  fi
}

OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

count=0
for path in "$DIR"/*; do
  [ -f "$path" ] || continue
  name="$(basename "$path")"
  if [ "$name" = checksums.sha256 ]; then continue; fi
  printf '%s  %s\n' "$(hash_file "$path")" "$name" >>"$OUT"
  count=$((count + 1))
done

[ "$count" -gt 0 ] || die "no files to checksum in $DIR"
cp "$OUT" "$DIR/checksums.sha256"
printf 'checksums.sh: wrote %s entries to %s/checksums.sha256\n' "$count" "$DIR"
