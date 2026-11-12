#!/usr/bin/env bash
# Shared definitions for the release scripts (package.sh, smoke.sh). Sourced,
# never executed.
#
# OpenWrt archives carry only tsunami-client: a router runs the client, not a
# server. They extract onto the root filesystem (busybox: tar -xzf FILE -C /).
# OpenWrt x86 is built with GO386=softfloat because the generic x86 images
# include CPUs without SSE2, which Go's default 386 code requires.

# shellcheck disable=SC2034  # read by the scripts that source this file
ALL_TARGETS="linux-amd64 linux-arm64 linux-386 linux-armv7 windows-amd64 windows-386 darwin-amd64 darwin-arm64 openwrt-x86_64 openwrt-x86 openwrt-aarch64 openwrt-armv7"

# target_info TARGET sets T_RUNOS T_GOOS T_GOARCH T_GOARM T_GO386 T_FORMAT T_EXT
# and T_BINFMT (the executable format token release_tool.py arch reports).
# It returns 1 for an unknown target.
# shellcheck disable=SC2034  # the T_* variables are read by the sourcing scripts
target_info() {
  T_RUNOS="" T_GOOS="" T_GOARCH="" T_GOARM="" T_GO386="" T_BINFMT=""
  T_FORMAT=tar T_EXT=tar.gz
  case "$1" in
    linux-amd64) T_RUNOS=linux T_GOOS=linux T_GOARCH=amd64 T_BINFMT=elf-x86_64 ;;
    linux-arm64) T_RUNOS=linux T_GOOS=linux T_GOARCH=arm64 T_BINFMT=elf-aarch64 ;;
    linux-386) T_RUNOS=linux T_GOOS=linux T_GOARCH=386 T_BINFMT=elf-386 ;;
    linux-armv7) T_RUNOS=linux T_GOOS=linux T_GOARCH=arm T_GOARM=7 T_BINFMT=elf-arm ;;
    windows-amd64) T_RUNOS=windows T_GOOS=windows T_GOARCH=amd64 T_BINFMT=pe-amd64 T_FORMAT=zip T_EXT=zip ;;
    windows-386) T_RUNOS=windows T_GOOS=windows T_GOARCH=386 T_BINFMT=pe-386 T_FORMAT=zip T_EXT=zip ;;
    darwin-amd64) T_RUNOS=darwin T_GOOS=darwin T_GOARCH=amd64 T_BINFMT=macho-x86_64 ;;
    darwin-arm64) T_RUNOS=darwin T_GOOS=darwin T_GOARCH=arm64 T_BINFMT=macho-arm64 ;;
    openwrt-x86_64) T_RUNOS=linux T_GOOS=linux T_GOARCH=amd64 T_BINFMT=elf-x86_64 T_FORMAT=rootfs ;;
    openwrt-x86) T_RUNOS=linux T_GOOS=linux T_GOARCH=386 T_GO386=softfloat T_BINFMT=elf-386 T_FORMAT=rootfs ;;
    openwrt-aarch64) T_RUNOS=linux T_GOOS=linux T_GOARCH=arm64 T_BINFMT=elf-aarch64 T_FORMAT=rootfs ;;
    openwrt-armv7) T_RUNOS=linux T_GOOS=linux T_GOARCH=arm T_GOARM=7 T_BINFMT=elf-arm T_FORMAT=rootfs ;;
    *) return 1 ;;
  esac
}

# target_members TARGET prints one "NAME MODE KIND" line per archive member, in
# build order. KIND is bin (an executable built from this tree), script or data.
#
# The linux-* names are a contract with scripts/install.sh and
# scripts/install-node.sh, which find tsunami-server*/tsunami-node-<arch> inside
# tsunami-<version>-<arch>.tar.gz.
target_members() {
  target_info "$1" || return 1
  if [ "$T_FORMAT" = rootfs ]; then
    printf '%s\n' \
      "usr/bin/tsunami-client 755 bin" \
      "etc/init.d/tsunami 755 script" \
      "usr/share/tsunami/tsunami.config.example 644 data"
    return 0
  fi
  local ext="" cmd
  if [ "$T_GOOS" = windows ]; then ext=.exe; fi
  for cmd in server client node; do
    printf 'tsunami-%s-%s%s 755 bin\n' "$cmd" "$1" "$ext"
  done
}

# valid_version accepts vMAJOR.MINOR.PATCH with an optional -prerelease. The
# value ends up in file names and in the binaries, so nothing else is allowed.
valid_version() {
  local pattern='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
  [[ "$1" =~ $pattern ]]
}

# find_python prints the first interpreter that is Python 3.8 or newer. The
# Windows Store stub named python3 fails the version probe and is skipped.
find_python() {
  local candidate
  for candidate in python3 python; do
    if command -v "$candidate" >/dev/null 2>&1 &&
      "$candidate" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)' >/dev/null 2>&1; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}

# find_emulator GOARCH prints the qemu user-mode emulator on PATH that runs
# linux binaries of that architecture, or returns 1.
#
# The name depends on the package: Ubuntu 26.04's qemu-user installs
# qemu-aarch64 and qemu-arm and has no qemu-user-static package, while
# qemu-user-static on Ubuntu 24.04 and Debian installs the same emulators with
# a -static suffix.
find_emulator() {
  local base candidate
  case "$1" in
    arm64) base=qemu-aarch64 ;;
    arm) base=qemu-arm ;;
    *) return 1 ;;
  esac
  for candidate in "$base" "$base-static"; do
    if command -v "$candidate" >/dev/null 2>&1; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}
