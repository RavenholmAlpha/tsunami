#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT_DIR/scripts/install-node.sh"

if ! bash -n "$SCRIPT"; then
  printf 'FAIL: install-node.sh has invalid Bash syntax\n' >&2
  exit 1
fi

help_output="$(bash "$SCRIPT" --help)"
for required in TSUNAMI_XBOARD_URL TSUNAMI_ENROLLMENT_TOKEN sync; do
  case "$help_output" in
    *"$required"*) ;;
    *)
      printf 'FAIL: install-node.sh help does not expose %s\n' "$required" >&2
      exit 1
      ;;
  esac
done

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
export TSUNAMI_TEST_SOURCE=1
export TSUNAMI_CONFIG_DIR="$tmp_dir/etc/tsunami"
export TSUNAMI_CREDENTIAL_FILE="$tmp_dir/etc/tsunami/node.credential"
export TSUNAMI_XBOARD_URL="https://panel.example.com"
export TSUNAMI_NODE_ID="42"

# shellcheck source=/dev/null
source "$SCRIPT"
write_node_config "one-time-token"
grep -Fq '"enrollment_token": "one-time-token"' "$CONFIG_FILE"

printf 'durable-node-credential\n' >"$CREDENTIAL_FILE"
write_node_config ""
if grep -Fq 'enrollment_token' "$CONFIG_FILE"; then
  printf 'FAIL: enrollment token remained after durable credential was present\n' >&2
  exit 1
fi
grep -Fq '"credential_file":' "$CONFIG_FILE"

printf 'ok - install-node script checks\n'
