#!/usr/bin/env bash
#
# TSUNAMI multi-platform build script (Bash)
#
# Usage:
#   ./build.sh                                   # build all platforms, version=dev
#   ./build.sh -v 1.2.0                          # build all platforms, version=v1.2.0
#   ./build.sh -v 1.2.0 -p linux-amd64,linux-arm64  # specific platforms
#   ./build.sh -v 2.0.0 -c                       # clean before building
#
# Output structure:
#   build/
#     v1.0.0/
#       linux-amd64/tsunami-server, tsunami-client
#       linux-arm64/tsunami-server, tsunami-client
#       windows-amd64/tsunami-server.exe, tsunami-client.exe
#       darwin-amd64/tsunami-server, tsunami-client
#       darwin-arm64/tsunami-server, tsunami-client
#       checksums.sha256

set -euo pipefail

# ── Defaults ──────────────────────────────────────────────────────────────
VERSION="dev"
PLATFORMS="linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64"
EXTRA_LDFLAGS=""
CLEAN=false
SKIP_CHECKSUM=false

# ── Parse args ────────────────────────────────────────────────────────────
while getopts "v:p:l:csh" opt; do
    case $opt in
        v) VERSION="$OPTARG" ;;
        p) PLATFORMS="$OPTARG" ;;
        l) EXTRA_LDFLAGS="$OPTARG" ;;
        c) CLEAN=true ;;
        s) SKIP_CHECKSUM=true ;;
        h)
            sed -n '2,/^$/p' "$0" | sed 's/^# \?//'
            exit 0
            ;;
        *)
            echo "Usage: $0 [-v version] [-p platforms] [-l ldflags] [-c] [-s]" >&2
            exit 1
            ;;
    esac
done

# ── Paths ─────────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
MODULE="github.com/tsunami-protocol/tsunami"
COMMANDS=("tsunami-server" "tsunami-client")

VERSION_TAG="v${VERSION}"
OUT_DIR="${SCRIPT_DIR}/${VERSION_TAG}"

# ── Banner ────────────────────────────────────────────────────────────────
echo ""
echo "╔══════════════════════════════════════════════════╗"
echo "║            TSUNAMI  Build  System                ║"
echo "╚══════════════════════════════════════════════════╝"
echo ""
echo "  Version   : ${VERSION_TAG}"
echo "  Module    : ${MODULE}"
echo "  Platforms : ${PLATFORMS}"
echo "  Output    : ${OUT_DIR}"
echo ""

# ── Pre-flight ────────────────────────────────────────────────────────────
if ! command -v go &>/dev/null; then
    echo "ERROR: 'go' not found in PATH." >&2
    exit 1
fi
echo "  Go        : $(go version | sed 's/go version //')"
echo ""

# ── Clean ─────────────────────────────────────────────────────────────────
if [[ "$CLEAN" == true ]] && [[ -d "$OUT_DIR" ]]; then
    echo "[clean] Removing ${OUT_DIR} ..."
    rm -rf "$OUT_DIR"
fi

# ── Build matrix ──────────────────────────────────────────────────────────
BUILD_TIME="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
GIT_COMMIT="$(git -C "$PROJECT_DIR" rev-parse --short HEAD 2>/dev/null || echo "unknown")"

BASE_LDFLAGS="-s -w -X main.version=${VERSION_TAG} -X main.commit=${GIT_COMMIT} -X main.buildTime=${BUILD_TIME}"
[[ -n "$EXTRA_LDFLAGS" ]] && BASE_LDFLAGS="${BASE_LDFLAGS} ${EXTRA_LDFLAGS}"

IFS=',' read -ra TARGET_LIST <<< "$PLATFORMS"
TOTAL=$(( ${#TARGET_LIST[@]} * ${#COMMANDS[@]} ))
CURRENT=0
FAILED=()

for target in "${TARGET_LIST[@]}"; do
    target="$(echo "$target" | xargs)"  # trim
    IFS='-' read -ra PARTS <<< "$target"
    if [[ ${#PARTS[@]} -ne 2 ]]; then
        echo "[skip] Invalid target format: '${target}' (expected GOOS-GOARCH)"
        continue
    fi
    GOOS_VAL="${PARTS[0]}"
    GOARCH_VAL="${PARTS[1]}"

    PLATFORM_DIR="${OUT_DIR}/${GOOS_VAL}-${GOARCH_VAL}"
    mkdir -p "$PLATFORM_DIR"

    for cmd in "${COMMANDS[@]}"; do
        CURRENT=$((CURRENT + 1))
        EXT=""
        [[ "$GOOS_VAL" == "windows" ]] && EXT=".exe"
        OUT_FILE="${PLATFORM_DIR}/${cmd}${EXT}"
        SRC_PATH="${MODULE}/cmd/${cmd}"

        printf "[%d/%d] Building %s (%s/%s) ..." "$CURRENT" "$TOTAL" "$cmd" "$GOOS_VAL" "$GOARCH_VAL"

        if CGO_ENABLED=0 GOOS="$GOOS_VAL" GOARCH="$GOARCH_VAL" \
            go build -trimpath -ldflags "$BASE_LDFLAGS" -o "$OUT_FILE" "$SRC_PATH" 2>&1; then
            SIZE=$(stat -f%z "$OUT_FILE" 2>/dev/null || stat -c%s "$OUT_FILE" 2>/dev/null || echo "?")
            if [[ "$SIZE" =~ ^[0-9]+$ ]]; then
                SIZE_MB=$(awk "BEGIN { printf \"%.2f\", $SIZE / 1048576 }")
                echo " OK (${SIZE_MB} MB)"
            else
                echo " OK"
            fi
        else
            echo " FAILED"
            FAILED+=("${cmd} (${GOOS_VAL}/${GOARCH_VAL})")
        fi
    done
done

# ── Checksums ─────────────────────────────────────────────────────────────
if [[ "$SKIP_CHECKSUM" == false ]] && [[ -d "$OUT_DIR" ]]; then
    echo ""
    echo "[checksum] Generating SHA-256 checksums ..."

    CHECKSUM_FILE="${OUT_DIR}/checksums.sha256"
    : > "$CHECKSUM_FILE"

    find "$OUT_DIR" -type f ! -name "checksums.sha256" -print0 | sort -z | while IFS= read -r -d '' file; do
        hash="$(sha256sum "$file" | awk '{print $1}')"
        rel="${file#${OUT_DIR}/}"
        echo "${hash}  ${rel}" >> "$CHECKSUM_FILE"
    done

    echo "  Saved to: ${CHECKSUM_FILE}"
fi

# ── Summary ───────────────────────────────────────────────────────────────
echo ""
echo "═══════════════════════════════════════════════════"

if [[ ${#FAILED[@]} -gt 0 ]]; then
    echo "  Build completed with ${#FAILED[@]} failure(s):"
    for f in "${FAILED[@]}"; do
        echo "    ✗ ${f}"
    done
    exit 1
else
    echo "  ✓ All ${TOTAL} build(s) succeeded."
    echo "  Output: ${OUT_DIR}"
fi
echo "═══════════════════════════════════════════════════"
echo ""
