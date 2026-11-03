#!/usr/bin/env bash
# shellcheck disable=SC2016
#
# TSUNAMI XBoard node installer and management script.
#
# Remote install:
#   curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install-node.sh | sudo bash
#
# The panel-generated command should inject TSUNAMI_XBOARD_URL,
# TSUNAMI_NODE_ID, and TSUNAMI_ENROLLMENT_TOKEN for first enrollment.
set -Eeuo pipefail

SERVICE_NAME="${TSUNAMI_NODE_SERVICE_NAME:-tsunami-node}"
REPO="${TSUNAMI_REPO:-RavenholmAlpha/tsunami}"
INSTALL_DIR="${TSUNAMI_INSTALL_DIR:-/usr/local/bin}"
CONFIG_DIR="${TSUNAMI_CONFIG_DIR:-/etc/tsunami}"
CONFIG_FILE="${TSUNAMI_NODE_CONFIG_FILE:-$CONFIG_DIR/node.json}"
CREDENTIAL_FILE="${TSUNAMI_CREDENTIAL_FILE:-$CONFIG_DIR/node.credential}"
STATE_FILE="$CONFIG_DIR/node-install.env"
SYSTEMD_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
NODE_BIN="$INSTALL_DIR/tsunami-node"
MANAGE_BIN="$INSTALL_DIR/tsunami-node-manage"
SCRIPT_URL="${TSUNAMI_NODE_SCRIPT_URL:-https://raw.githubusercontent.com/$REPO/main/scripts/install-node.sh}"
BOOTSTRAP_ENROLLMENT=0

log() {
  printf '[tsunami-node] %s\n' "$*"
}

die() {
  printf '[tsunami-node] error: %s\n' "$*" >&2
  exit 1
}

need_root() {
  [ "$(id -u)" -eq 0 ] || die "run as root"
}

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) printf 'linux-amd64' ;;
    aarch64 | arm64) printf 'linux-arm64' ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  printf '%s' "$value"
}

shell_escape() {
  printf '%q' "$1"
}

is_interactive() {
  [ -t 0 ] || [ -r /dev/tty ]
}

prompt_value() {
  local label="$1"
  local default="$2"
  local value=""
  if ! is_interactive; then
    printf '%s' "$default"
    return
  fi
  if [ -t 0 ]; then
    read -r -p "$label [$default]: " value || true
  else
    read -r -p "$label [$default]: " value </dev/tty || true
  fi
  printf '%s' "${value:-$default}"
}

prompt_secret() {
  local label="$1"
  local value=""
  if ! is_interactive; then
    printf '%s' ''
    return
  fi
  if [ -t 0 ]; then
    read -r -s -p "$label: " value || true
  else
    read -r -s -p "$label: " value </dev/tty || true
  fi
  printf '\n' >&2
  printf '%s' "$value"
}

load_state() {
  [ -f "$STATE_FILE" ] || return 0

  local supplied_url="${TSUNAMI_XBOARD_URL-}"
  local supplied_node_id="${TSUNAMI_NODE_ID-}"
  local supplied_credential_file="${TSUNAMI_CREDENTIAL_FILE-}"
  # The state file is created by this script with shell-escaped values only.
  # shellcheck source=/dev/null
  . "$STATE_FILE"
  [ -z "$supplied_url" ] || TSUNAMI_XBOARD_URL="$supplied_url"
  [ -z "$supplied_node_id" ] || TSUNAMI_NODE_ID="$supplied_node_id"
  [ -z "$supplied_credential_file" ] || TSUNAMI_CREDENTIAL_FILE="$supplied_credential_file"
}

write_state() {
  cat >"$STATE_FILE" <<EOF
TSUNAMI_XBOARD_URL=$(shell_escape "$TSUNAMI_XBOARD_URL")
TSUNAMI_NODE_ID=$(shell_escape "$TSUNAMI_NODE_ID")
TSUNAMI_CREDENTIAL_FILE=$(shell_escape "$CREDENTIAL_FILE")
EOF
  chmod 0600 "$STATE_FILE"
}

sha256_file() {
  local file="$1"
  if has_cmd sha256sum; then
    sha256sum "$file" | awk '{print $1}'
    return
  fi
  if has_cmd openssl; then
    openssl dgst -sha256 -r "$file" | awk '{print $1}'
    return
  fi
  die "sha256sum or openssl is required for release verification"
}

release_asset_url() {
  local arch="$1"
  local version="${TSUNAMI_VERSION:-latest}"
  if [ "$version" != "latest" ]; then
    printf 'https://github.com/%s/releases/download/%s/tsunami-%s-%s.tar.gz' "$REPO" "$version" "$version" "$arch"
    return
  fi

  has_cmd curl || die "curl is required to download releases"
  curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -nE "s/.*\"browser_download_url\": \"([^\"]*tsunami-[^\"]*-${arch}\\.tar\\.gz)\".*/\\1/p" \
    | head -n 1
}

install_from_source() {
  [ -d "cmd/tsunami-node" ] || return 1
  has_cmd go || return 1
  log "building tsunami-node from local source"
  mkdir -p "$INSTALL_DIR"
  go build -trimpath -ldflags='-s -w' -o "$NODE_BIN" ./cmd/tsunami-node/
  chmod 0755 "$NODE_BIN"
  return 0
}

install_from_release() {
  local arch="$1"
  local url
  url="$(release_asset_url "$arch")"
  [ -n "$url" ] || die "could not find a release for $arch"
  has_cmd curl || die "curl is required to download releases"
  has_cmd tar || die "tar is required to unpack releases"

  local temporary
  temporary="$(mktemp -d)"
  trap 'rm -rf "$temporary"' RETURN
  log "downloading $url"
  curl -fL "$url" -o "$temporary/tsunami.tar.gz"

  local checksums_url expected actual archive_name
  checksums_url="${url%/*}/checksums.sha256"
  curl -fsSL "$checksums_url" -o "$temporary/checksums.sha256" \
    || die "failed to download checksums.sha256"
  archive_name="$(basename "$url")"
  expected="$(sed -n "s/^\([a-f0-9]\{64\}\)  .*${archive_name}$/\1/p" "$temporary/checksums.sha256" | head -n 1)"
  [ -n "$expected" ] || die "no checksum entry found for $archive_name"
  actual="$(sha256_file "$temporary/tsunami.tar.gz")"
  [ "$actual" = "$expected" ] || die "release checksum mismatch"

  tar -xzf "$temporary/tsunami.tar.gz" -C "$temporary"
  local binary
  binary="$(find "$temporary" -type f -name "tsunami-node-${arch}" -print -quit)"
  [ -n "$binary" ] || die "tsunami-node binary missing from release archive"
  mkdir -p "$INSTALL_DIR"
  install -m 0755 "$binary" "$NODE_BIN"
  trap - RETURN
  rm -rf "$temporary"
}

install_binary() {
  if install_from_source; then
    return
  fi
  install_from_release "$(detect_arch)"
}

install_manager() {
  local refresh="${1:-}"
  local current_script="${BASH_SOURCE[0]}"
  if [ "$refresh" = "refresh" ]; then
    has_cmd curl || die "curl is required to update the management script"
    local temporary
    temporary="$(mktemp)"
    curl -fsSL "$SCRIPT_URL" -o "$temporary"
    install -m 0755 "$temporary" "$MANAGE_BIN"
    rm -f "$temporary"
    return
  fi
  if [ -e "$MANAGE_BIN" ] && [ "$current_script" -ef "$MANAGE_BIN" ]; then
    return
  fi
  if [ -f "$current_script" ] && [ -r "$current_script" ]; then
    install -m 0755 "$current_script" "$MANAGE_BIN"
    return
  fi
  has_cmd curl || die "curl is required to install the management script"
  curl -fsSL "$SCRIPT_URL" -o "$MANAGE_BIN"
  chmod 0755 "$MANAGE_BIN"
}

write_node_config() {
  load_state
  TSUNAMI_XBOARD_URL="${TSUNAMI_XBOARD_URL:-$(prompt_value 'XBoard URL' '')}"
  TSUNAMI_NODE_ID="${TSUNAMI_NODE_ID:-$(prompt_value 'TSUNAMI node ID' '')}"
  CREDENTIAL_FILE="${TSUNAMI_CREDENTIAL_FILE:-$CREDENTIAL_FILE}"

  [ -n "$TSUNAMI_XBOARD_URL" ] || die "TSUNAMI_XBOARD_URL is required"
  [ -n "$TSUNAMI_NODE_ID" ] || die "TSUNAMI_NODE_ID is required"
  case "$TSUNAMI_XBOARD_URL" in
    http://* | https://*) ;;
    *) die "TSUNAMI_XBOARD_URL must start with http:// or https://" ;;
  esac

  local enrollment_token="${1:-}"
  BOOTSTRAP_ENROLLMENT=0
  if [ ! -s "$CREDENTIAL_FILE" ]; then
    if [ -z "$enrollment_token" ]; then
      enrollment_token="${TSUNAMI_ENROLLMENT_TOKEN:-$(prompt_secret 'One-time enrollment token')}"
    fi
    [ -n "$enrollment_token" ] || die "TSUNAMI_ENROLLMENT_TOKEN is required until this node is enrolled"
    BOOTSTRAP_ENROLLMENT=1
  fi

  mkdir -p "$CONFIG_DIR" "$(dirname "$CREDENTIAL_FILE")"
  chmod 0700 "$CONFIG_DIR" "$(dirname "$CREDENTIAL_FILE")"
  cat >"$CONFIG_FILE" <<EOF
{
  "xboard": {
    "base_url": "$(json_escape "$TSUNAMI_XBOARD_URL")",
    "node_id": "$(json_escape "$TSUNAMI_NODE_ID")",
    "credential_file": "$(json_escape "$CREDENTIAL_FILE")",
    "sync_interval": "${TSUNAMI_SYNC_INTERVAL:-1m}",
    "config_interval": "${TSUNAMI_CONFIG_INTERVAL:-1m}",
    "request_timeout": "${TSUNAMI_REQUEST_TIMEOUT:-15s}"$( [ -n "$enrollment_token" ] && printf ',\n    "enrollment_token": "%s"' "$(json_escape "$enrollment_token")" )
  }
}
EOF
  chmod 0600 "$CONFIG_FILE"
  write_state
}

write_systemd() {
  cat >"$SYSTEMD_FILE" <<EOF
[Unit]
Description=TSUNAMI XBoard Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$NODE_BIN --config $CONFIG_FILE
Restart=on-failure
RestartSec=3
LimitNOFILE=1048576
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF
}

wait_for_enrollment() {
  local attempts=30
  while [ "$attempts" -gt 0 ]; do
    if [ -s "$CREDENTIAL_FILE" ]; then
      chmod 0600 "$CREDENTIAL_FILE"
      return 0
    fi
    if ! systemctl is-active --quiet "$SERVICE_NAME"; then
      return 1
    fi
    sleep 1
    attempts=$((attempts - 1))
  done
  return 1
}

install_all() {
  need_root
  load_state
  install_binary
  write_node_config "${TSUNAMI_ENROLLMENT_TOKEN:-}"
  write_systemd
  install_manager
  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME" >/dev/null
  systemctl restart "$SERVICE_NAME"

  if [ "$BOOTSTRAP_ENROLLMENT" = "1" ]; then
    if ! wait_for_enrollment; then
      journalctl -u "$SERVICE_NAME" -n 80 --no-pager >&2 || true
      die "enrollment did not complete; the temporary token remains in $CONFIG_FILE for retry"
    fi
    write_node_config ""
    systemctl restart "$SERVICE_NAME"
    log "enrollment completed; removed the one-time token from $CONFIG_FILE"
  fi

  log "installed and started $SERVICE_NAME"
  printf '  Node ID: %s\n  Config : %s\n  Credential: %s\n' "$TSUNAMI_NODE_ID" "$CONFIG_FILE" "$CREDENTIAL_FILE"
}

status_service() {
  systemctl status "$SERVICE_NAME" --no-pager
}

restart_service() {
  need_root
  systemctl restart "$SERVICE_NAME"
  status_service
}

sync_node() {
  need_root
  systemctl kill -s HUP "$SERVICE_NAME"
  log "requested immediate XBoard sync"
}

configure_node() {
  need_root
  load_state
  write_node_config "${TSUNAMI_ENROLLMENT_TOKEN:-}"
  systemctl restart "$SERVICE_NAME"
  log "node bootstrap configuration updated"
}

update_binary() {
  need_root
  install_binary
  install_manager refresh
  systemctl restart "$SERVICE_NAME"
  log "node binary updated and service restarted"
}

logs_service() {
  journalctl -u "$SERVICE_NAME" -f --no-pager
}

uninstall_all() {
  need_root
  systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
  rm -f "$SYSTEMD_FILE" "$NODE_BIN" "$MANAGE_BIN"
  systemctl daemon-reload
  if [ "${TSUNAMI_KEEP_CONFIG:-1}" = "0" ]; then
    rm -rf "$CONFIG_DIR"
  fi
  log "uninstalled. Set TSUNAMI_KEEP_CONFIG=0 to remove config and credential files."
}

usage() {
  cat <<EOF
Usage: tsunami-node-manage <command>

Commands:
  install     Install or reinstall tsunami-node, enroll if needed, and start it
  status      Show systemd service status
  restart     Restart the node service
  logs        Follow node logs
  sync        Request immediate XBoard config, user, and traffic sync
  update      Install the selected release and restart the service
  config      Rewrite bootstrap config and restart the service
  uninstall   Remove service and binary; keep config by default

First enrollment environment:
  TSUNAMI_XBOARD_URL=https://panel.example.com
  TSUNAMI_NODE_ID=42
  TSUNAMI_ENROLLMENT_TOKEN=<short-lived-one-time-token>

Optional:
  TSUNAMI_CREDENTIAL_FILE=/etc/tsunami/node.credential
  TSUNAMI_SYNC_INTERVAL=1m
  TSUNAMI_CONFIG_INTERVAL=1m
  TSUNAMI_REQUEST_TIMEOUT=15s
  TSUNAMI_VERSION=latest or v1.3.1
EOF
}

main() {
  local command="${1:-install}"
  case "$command" in
    install) install_all ;;
    status) status_service ;;
    restart) restart_service ;;
    logs) logs_service ;;
    sync) sync_node ;;
    update) update_binary ;;
    config | configure) configure_node ;;
    uninstall) uninstall_all ;;
    help | -h | --help) usage ;;
    *) usage; exit 1 ;;
  esac
}

if [ "${TSUNAMI_TEST_SOURCE:-0}" != "1" ]; then
  main "$@"
fi
