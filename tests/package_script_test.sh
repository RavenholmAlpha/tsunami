#!/usr/bin/env bash
# Tests for the release packaging scripts (build/package.sh, build/smoke.sh,
# build/checksums.sh) and for the release workflow's target matrix.
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PACKAGE="$ROOT_DIR/build/package.sh"
SMOKE="$ROOT_DIR/build/smoke.sh"
CHECKSUMS="$ROOT_DIR/build/checksums.sh"
TARGETS_FILE="$ROOT_DIR/build/targets.sh"
WORKFLOW="$ROOT_DIR/.github/workflows/ci-cd.yml"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

VERSION="v9.8.7"
# 2023-11-14T22:13:20Z. Fixed so archives do not depend on the wall clock.
export SOURCE_DATE_EPOCH=1700000000
TESTS_RUN=0

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_eq() {
  TESTS_RUN=$((TESTS_RUN + 1))
  [ "$1" = "$2" ] || fail "$3: expected <$1>, got <$2>"
}

assert_contains() {
  TESTS_RUN=$((TESTS_RUN + 1))
  case "$2" in
    *"$1"*) ;;
    *) fail "$3: <$2> does not contain <$1>" ;;
  esac
}

assert_not_contains() {
  TESTS_RUN=$((TESTS_RUN + 1))
  case "$2" in
    *"$1"*) fail "$3: <$2> unexpectedly contains <$1>" ;;
  esac
}

assert_file() {
  TESTS_RUN=$((TESTS_RUN + 1))
  [ -f "$1" ] || fail "$2: missing file $1"
}

# assert_fails_with LABEL EXPECTED_SUBSTRING COMMAND...
assert_fails_with() {
  local label="$1" expected="$2" out
  shift 2
  TESTS_RUN=$((TESTS_RUN + 1))
  if out="$("$@" 2>&1)"; then
    fail "$label: command unexpectedly succeeded"
  fi
  case "$out" in
    *"$expected"*) ;;
    *) fail "$label: output <$out> does not contain <$expected>" ;;
  esac
}

plan() {
  bash "$PACKAGE" "$@" -P | tr '\n' ' ' | sed 's/ *$//'
}

# Reads stdin so a backslash in the path cannot add GNU sha256sum's "\" prefix.
sha256_of() {
  sha256sum <"$1" | awk '{print $1}'
}

archive_in() {
  local dir="$1" target="$2" found=""
  local candidate
  for candidate in "$dir"/tsunami-"$VERSION"-"$target".*; do
    [ -f "$candidate" ] && found="$candidate"
  done
  [ -n "$found" ] || fail "no archive for $target in $dir"
  printf '%s' "$found"
}

build_target() {
  local target="$1" out="$2"
  mkdir -p "$out"
  bash "$PACKAGE" -v "$VERSION" -t "$target" -o "$out" >/dev/null
}

HOST_OS="$(go env GOHOSTOS)"
HOST_ARCH="$(go env GOHOSTARCH)"
NATIVE="${HOST_OS}-${HOST_ARCH}"

# ── Input validation ─────────────────────────────────────────────────────
test_rejects_unknown_target() {
  assert_fails_with "unknown target" "unknown target" \
    bash "$PACKAGE" -v "$VERSION" -t plan9-mips -o "$WORK/none"
  assert_fails_with "missing target" "target is required" \
    bash "$PACKAGE" -v "$VERSION" -o "$WORK/none"
}

test_rejects_malformed_versions() {
  local bad
  for bad in "" latest 1.2.3 v1.2 'v1.2.3;rm' 'v1.2.3/../x' ' v1.2.3'; do
    assert_fails_with "version <$bad>" "version" \
      bash "$PACKAGE" -v "$bad" -t linux-amd64 -o "$WORK/none"
  done
  [ ! -e "$WORK/none" ] || fail "an invalid request left output behind"
}

# ── Target table ─────────────────────────────────────────────────────────
test_target_mapping() {
  local target goos goarch goarm format expected
  while read -r target goos goarch goarm format; do
    [ "$goarm" = "-" ] && goarm=""
    expected="goos=$goos goarch=$goarch goarm=$goarm format=$format"
    assert_eq "$expected" \
      "$(plan -t "$target" | sed -E 's/ ext=.*//')" "mapping of $target"
  done <<'EOF'
linux-amd64 linux amd64 - tar
linux-arm64 linux arm64 - tar
linux-386 linux 386 - tar
linux-armv7 linux arm 7 tar
windows-amd64 windows amd64 - zip
windows-386 windows 386 - zip
darwin-amd64 darwin amd64 - tar
darwin-arm64 darwin arm64 - tar
openwrt-x86_64 linux amd64 - rootfs
openwrt-x86 linux 386 - rootfs
openwrt-aarch64 linux arm64 - rootfs
openwrt-armv7 linux arm 7 rootfs
EOF
}

# The one-click installers download tsunami-<version>-linux-<arch>.tar.gz and
# look for tsunami-node-<arch> inside it; those names are a public contract.
test_installer_contract() {
  local line
  line="$(plan -v v1.2.3 -t linux-amd64)"
  assert_contains "archive=tsunami-v1.2.3-linux-amd64.tar.gz" "$line" "linux-amd64 archive"
  assert_contains "files=tsunami-server-linux-amd64,tsunami-client-linux-amd64,tsunami-node-linux-amd64" \
    "$line" "linux-amd64 members"
  assert_contains "archive=tsunami-v1.2.3-linux-arm64.tar.gz" \
    "$(plan -v v1.2.3 -t linux-arm64)" "linux-arm64 archive"
  assert_contains "archive=tsunami-v1.2.3-windows-386.zip" \
    "$(plan -v v1.2.3 -t windows-386)" "windows-386 archive"
  assert_contains "files=tsunami-server-windows-386.exe,tsunami-client-windows-386.exe,tsunami-node-windows-386.exe" \
    "$(plan -v v1.2.3 -t windows-386)" "windows-386 members"
  assert_contains "files=usr/bin/tsunami-client,etc/init.d/tsunami,usr/share/tsunami/tsunami.config.example" \
    "$(plan -v v1.2.3 -t openwrt-armv7)" "openwrt members"

  # The installer's "latest" lookup must not confuse the new targets with the
  # two it supports.
  local name
  for name in tsunami-v1.2.3-linux-386.tar.gz tsunami-v1.2.3-linux-armv7.tar.gz \
    tsunami-v1.2.3-openwrt-aarch64.tar.gz tsunami-v1.2.3-openwrt-x86_64.tar.gz; do
    TESTS_RUN=$((TESTS_RUN + 1))
    if printf '"browser_download_url": "https://x/%s"\n' "$name" |
      grep -Eq 'tsunami-[^"]*-linux-(amd64|arm64)\.tar\.gz'; then
      fail "installer pattern for linux-amd64/arm64 also matches $name"
    fi
  done
}

# ── Real builds ──────────────────────────────────────────────────────────
test_native_package_runs_and_is_reproducible() {
  case "$NATIVE" in
    linux-amd64 | linux-arm64 | windows-amd64 | darwin-amd64 | darwin-arm64) ;;
    *)
      printf 'skip: no release target for host %s\n' "$NATIVE"
      return 0
      ;;
  esac

  build_target "$NATIVE" "$WORK/native-a"
  local archive
  archive="$(archive_in "$WORK/native-a" "$NATIVE")"
  assert_eq "1" "$(find "$WORK/native-a" -type f | wc -l | tr -d ' ')" "native output holds one archive"

  local out
  out="$(SMOKE_RUN=always bash "$SMOKE" -t "$NATIVE" -v "$VERSION" "$archive")"
  assert_contains "tsunami-server $VERSION (commit=" "$out" "server reports its version"
  assert_contains "tsunami-client $VERSION (commit=" "$out" "client reports its version"
  assert_contains "tsunami-node $VERSION (commit=" "$out" "node reports its version"
  assert_contains "built=2023-11-14T22:13:20Z" "$out" "build time comes from SOURCE_DATE_EPOCH"

  build_target "$NATIVE" "$WORK/native-b"
  assert_eq "$(sha256_of "$archive")" \
    "$(sha256_of "$(archive_in "$WORK/native-b" "$NATIVE")")" "native archive is reproducible"
}

test_openwrt_archive() {
  build_target openwrt-aarch64 "$WORK/owrt-a"
  local archive listing
  archive="$(archive_in "$WORK/owrt-a" openwrt-aarch64)"
  assert_eq "$WORK/owrt-a/tsunami-$VERSION-openwrt-aarch64.tar.gz" "$archive" "openwrt archive name"

  listing="$(TZ=UTC tar -tvzf "$archive")"
  assert_contains "usr/bin/tsunami-client" "$listing" "openwrt client path"
  assert_contains "etc/init.d/tsunami" "$listing" "openwrt init script path"
  assert_contains "usr/share/tsunami/tsunami.config.example" "$listing" "openwrt config example path"
  assert_not_contains "tsunami-server" "$listing" "openwrt carries no server"
  assert_not_contains "tsunami-node" "$listing" "openwrt carries no node"
  assert_contains "2023-11-14 22:13" "$listing" "openwrt entries use SOURCE_DATE_EPOCH"
  assert_contains "0/0" "$listing" "openwrt entries are owned by root"
  assert_eq "2" "$(printf '%s\n' "$listing" | grep -c '^-rwxr-xr-x')" "client and init script are executable"
  assert_eq "1" "$(printf '%s\n' "$listing" | grep -c '^-rw-r--r--')" "config example is not executable"
  # Extracted with -C /, so an absolute or parent-relative entry would write
  # outside the intended tree. Directory entries are also absent: extracting
  # one would rewrite the mode and mtime of /etc and /usr on the router.
  TESTS_RUN=$((TESTS_RUN + 1))
  if tar -tzf "$archive" | grep -Eq '^(/|\./)|\.\./|/$'; then
    fail "openwrt archive has an absolute, parent-relative or directory entry"
  fi

  SMOKE_RUN=never bash "$SMOKE" -t openwrt-aarch64 -v "$VERSION" "$archive" >/dev/null

  build_target openwrt-aarch64 "$WORK/owrt-b"
  assert_eq "$(sha256_of "$archive")" \
    "$(sha256_of "$(archive_in "$WORK/owrt-b" openwrt-aarch64)")" "openwrt archive is reproducible"
}

test_windows_386_zip() {
  build_target windows-386 "$WORK/win386"
  local archive
  archive="$(archive_in "$WORK/win386" windows-386)"
  assert_eq "$WORK/win386/tsunami-$VERSION-windows-386.zip" "$archive" "windows archive name"
  if [ "$HOST_OS" = windows ]; then
    SMOKE_RUN=always bash "$SMOKE" -t windows-386 -v "$VERSION" "$archive" >/dev/null
  else
    SMOKE_RUN=never bash "$SMOKE" -t windows-386 -v "$VERSION" "$archive" >/dev/null
  fi
}

# ── Smoke checks must be able to fail ────────────────────────────────────
test_smoke_rejects_wrong_architecture() {
  local archive="$WORK/owrt-a/tsunami-$VERSION-openwrt-aarch64.tar.gz"
  assert_fails_with "arm64 binary labelled x86_64" "architecture mismatch" \
    env SMOKE_RUN=never bash "$SMOKE" -t openwrt-x86_64 -v "$VERSION" "$archive"
  assert_fails_with "arm64 binary labelled armv7" "architecture mismatch" \
    env SMOKE_RUN=never bash "$SMOKE" -t openwrt-armv7 -v "$VERSION" "$archive"
}

test_smoke_rejects_wrong_contents() {
  local dir="$WORK/tampered"
  mkdir -p "$dir"
  tar -xzf "$WORK/owrt-a/tsunami-$VERSION-openwrt-aarch64.tar.gz" -C "$dir"
  # Members are listed explicitly (not `usr etc`) so the archive has no
  # directory entries and each case differs from the real archive in one way.
  tar -czf "$WORK/missing-init.tar.gz" -C "$dir" \
    usr/bin/tsunami-client usr/share/tsunami/tsunami.config.example
  assert_fails_with "missing init script" "archive contents" \
    env SMOKE_RUN=never bash "$SMOKE" -t openwrt-aarch64 -v "$VERSION" "$WORK/missing-init.tar.gz"

  printf 'x' >"$dir/usr/share/tsunami/extra"
  tar -czf "$WORK/extra.tar.gz" -C "$dir" \
    etc/init.d/tsunami usr/bin/tsunami-client usr/share/tsunami/tsunami.config.example usr/share/tsunami/extra
  assert_fails_with "unexpected extra file" "archive contents" \
    env SMOKE_RUN=never bash "$SMOKE" -t openwrt-aarch64 -v "$VERSION" "$WORK/extra.tar.gz"

  tar -czf "$WORK/with-dirs.tar.gz" -C "$dir" etc usr/bin usr/share/tsunami/tsunami.config.example
  assert_fails_with "directory entries" "archive contents" \
    env SMOKE_RUN=never bash "$SMOKE" -t openwrt-aarch64 -v "$VERSION" "$WORK/with-dirs.tar.gz"
}

test_smoke_rejects_wrong_version() {
  case "$NATIVE" in
    linux-amd64 | linux-arm64 | windows-amd64 | darwin-amd64 | darwin-arm64) ;;
    *) return 0 ;;
  esac
  assert_fails_with "version mismatch" "version" \
    env SMOKE_RUN=always bash "$SMOKE" -t "$NATIVE" -v v1.0.0 "$(archive_in "$WORK/native-a" "$NATIVE")"
}

# A CRLF init script makes the interpreter line `/bin/sh^M`, which procd cannot
# run, so a Windows checkout that rewrote line endings must fail the smoke test.
test_smoke_rejects_crlf_init_script() {
  # shellcheck source-path=SCRIPTDIR/../build
  # shellcheck source=targets.sh
  source "$TARGETS_FILE"
  local py tool="$ROOT_DIR/build/release_tool.py" dir="$WORK/crlf"
  py="$(find_python)" || fail "python 3.8+ is required"
  mkdir -p "$dir/x"
  (
    cd "$dir"
    "$py" "$tool" extract "$WORK/owrt-a/tsunami-$VERSION-openwrt-aarch64.tar.gz" x
    sed 's/$/\r/' x/etc/init.d/tsunami >init.crlf
    "$py" "$tool" archive --format tar.gz --epoch "$SOURCE_DATE_EPOCH" --out crlf.tar.gz \
      --add x/usr/bin/tsunami-client usr/bin/tsunami-client 755 \
      --add init.crlf etc/init.d/tsunami 755 \
      --add x/usr/share/tsunami/tsunami.config.example usr/share/tsunami/tsunami.config.example 644
  )
  assert_fails_with "CRLF init script" "carriage return" \
    env SMOKE_RUN=never bash "$SMOKE" -t openwrt-aarch64 -v "$VERSION" "$dir/crlf.tar.gz"
}

# ── Checksums ────────────────────────────────────────────────────────────
test_checksums() {
  local dir="$WORK/release"
  mkdir -p "$dir"
  printf 'one' >"$dir/tsunami-v1.0.0-linux-amd64.tar.gz"
  printf 'two' >"$dir/tsunami-v1.0.0-windows-amd64.zip"
  printf 'three' >"$dir/tsunami-v1.0.0-linux-arm64.tar.gz"

  bash "$CHECKSUMS" "$dir" >/dev/null
  assert_file "$dir/checksums.sha256" "checksums file"
  assert_eq "3" "$(wc -l <"$dir/checksums.sha256" | tr -d ' ')" "one line per asset"
  assert_eq "tsunami-v1.0.0-linux-amd64.tar.gz tsunami-v1.0.0-linux-arm64.tar.gz tsunami-v1.0.0-windows-amd64.zip" \
    "$(awk '{print $2}' "$dir/checksums.sha256" | tr '\n' ' ' | sed 's/ *$//')" "lines are sorted by name"
  (cd "$dir" && sha256sum -c checksums.sha256 >/dev/null) || fail "sha256sum -c rejects the checksums file"

  # The exact lookup scripts/install.sh uses to verify a download.
  local name="tsunami-v1.0.0-linux-arm64.tar.gz" expected
  expected="$(sed -n "s/^\([a-f0-9]\{64\}\)  .*${name}$/\1/p" "$dir/checksums.sha256" | head -n 1)"
  assert_eq "$(sha256_of "$dir/$name")" "$expected" "installer lookup finds the hash"

  bash "$CHECKSUMS" "$dir" >/dev/null
  assert_eq "3" "$(wc -l <"$dir/checksums.sha256" | tr -d ' ')" "rerun does not checksum its own output"

  assert_fails_with "empty directory" "no files" bash "$CHECKSUMS" "$WORK/empty-release-dir-that-is-missing"
}

# GNU sha256sum starts a line with "\" when the file path contains a backslash,
# which happens for every file under a Windows-style directory such as
# C:\Users\...\dist. scripts/install.sh anchors on the 64 hex digits, so a
# leading "\" would make its checksum lookup fail.
test_checksums_have_no_escape_prefix_for_backslash_paths() {
  local dir="$WORK/back\\slash" name="tsunami-v1.0.0-linux-amd64.tar.gz" lines expected
  mkdir -p "$dir"
  printf 'payload' >"$dir/$name"
  bash "$CHECKSUMS" "$dir" >/dev/null

  lines="$(grep -cE '^[a-f0-9]{64}  tsunami-v1\.0\.0-linux-amd64\.tar\.gz$' "$dir/checksums.sha256" || true)"
  assert_eq "1" "$lines" "the line starts with the 64 hex digits and nothing else"
  expected="$(sed -n "s/^\([a-f0-9]\{64\}\)  .*${name}$/\1/p" "$dir/checksums.sha256" | head -n 1)"
  assert_eq "$(sha256_of "$dir/$name")" "$expected" "installer lookup works under a backslash path"
}

# ── Workflow ─────────────────────────────────────────────────────────────
test_workflow_builds_every_target() {
  local from_table from_workflow
  from_table="$(bash -c "source '$TARGETS_FILE' && printf '%s\n' \$ALL_TARGETS" | sort | tr '\n' ' ')"
  from_workflow="$(sed -nE 's/^ +- target: ([a-z0-9_-]+) *$/\1/p' "$WORKFLOW" | sort | tr '\n' ' ')"
  assert_eq "$from_table" "$from_workflow" "workflow matrix lists exactly the targets in build/targets.sh"
}

test_rejects_unknown_target
test_rejects_malformed_versions
test_target_mapping
test_installer_contract
test_native_package_runs_and_is_reproducible
test_openwrt_archive
test_windows_386_zip
test_smoke_rejects_wrong_architecture
test_smoke_rejects_wrong_contents
test_smoke_rejects_wrong_version
test_smoke_rejects_crlf_init_script
test_checksums
test_checksums_have_no_escape_prefix_for_backslash_paths
test_workflow_builds_every_target

printf 'OK: %s assertions\n' "$TESTS_RUN"
