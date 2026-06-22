#!/usr/bin/env bash
# shellcheck disable=SC1090,SC2059,SC2086,SC2155
#
# Tsunami — One-click deployment script
#
# Remote install (wget):  wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
# Remote install (curl):  curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
#
# With options:           curl -fsSL https://...install.sh | TSUNAMI_PUBLIC_HOST=example.com TSUNAMI_LETSENCRYPT=y sudo -E bash
#
set -Eeuo pipefail

SERVICE_NAME="${TSUNAMI_SERVICE_NAME:-tsunami-server}"
REPO="${TSUNAMI_REPO:-RavenholmAlpha/tsunami}"
INSTALL_DIR="${TSUNAMI_INSTALL_DIR:-/usr/local/bin}"
CONFIG_DIR="${TSUNAMI_CONFIG_DIR:-/etc/tsunami}"
CONFIG_FILE="${TSUNAMI_CONFIG_FILE:-$CONFIG_DIR/config.json}"
STATE_FILE="$CONFIG_DIR/install.env"
CLIENT_FILE="$CONFIG_DIR/client-command.txt"
SYSTEMD_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

# Detect piped execution (wget/curl | bash) — disable interactive prompts
if [ ! -t 0 ] && [ "${TSUNAMI_TEST_SOURCE:-0}" != "1" ]; then
  TSUNAMI_ASSUME_YES="${TSUNAMI_ASSUME_YES:-1}"
fi

DEFAULT_PADDING_JSON='stop=8\n0=30-30\n1=100-400\n2=400-500,c,500-1000,c,500-1000,c,500-1000,c,500-1000\n3=9-9,500-1000\n4=500-1000\n5=500-1000\n6=500-1000\n7=500-1000\nkeepalive=30000-60000:4-8'

# ── Colours ───────────────────────────────────────────────────────────────
if [ -t 1 ]; then
  C_RESET='\033[0m'
  C_BOLD='\033[1m'
  C_GREEN='\033[1;32m'
  C_CYAN='\033[1;36m'
  C_YELLOW='\033[1;33m'
  C_RED='\033[1;31m'
  C_DIM='\033[0;90m'
  C_BOX='\033[1;34m'
else
  C_RESET='' C_BOLD='' C_GREEN='' C_CYAN='' C_YELLOW='' C_RED='' C_DIM='' C_BOX=''
fi

log() {
  printf "${C_GREEN}[tsunami]${C_RESET} %s\n" "$*"
}

die() {
  printf "${C_RED}[tsunami] error:${C_RESET} %s\n" "$*" >&2
  exit 1
}

need_root() {
  if [ "$(id -u)" -ne 0 ]; then
    die "please run as root. Example: curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install.sh | sudo bash"
  fi
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

ask() {
  local prompt="$1"
  local default="$2"
  local answer
  if [ -t 0 ] && [ "${TSUNAMI_ASSUME_YES:-}" != "1" ]; then
    read -r -p "$prompt [$default]: " answer
    printf '%s' "${answer:-$default}"
  else
    printf '%s' "$default"
  fi
}

is_interactive() {
  [ -t 0 ] && [ "${TSUNAMI_ASSUME_YES:-}" != "1" ]
}

default_command_for_context() {
  local stdin_is_tty="$1"
  if [ "$stdin_is_tty" = "1" ]; then
    printf 'menu'
  else
    printf 'install'
  fi
}

normalize_yes() {
  case "${1,,}" in
    y | yes | 1 | true) return 0 ;;
    *) return 1 ;;
  esac
}

normalize_no() {
  case "${1,,}" in
    n | no | 0 | false) return 0 ;;
    *) return 1 ;;
  esac
}

prompt_choice() {
  local prompt="$1"
  local default="$2"
  shift 2
  local count="$#"
  local answer i option

  while true; do
    printf '%s\n' "$prompt" >&2
    i=1
    for option in "$@"; do
      if [ "$i" = "$default" ]; then
        printf '  %s) %s [default]\n' "$i" "$option" >&2
      else
        printf '  %s) %s\n' "$i" "$option" >&2
      fi
      i=$((i + 1))
    done

    if ! read -r -p "Choice${default:+ [$default]}: " answer; then
      [ -n "$default" ] || return 1
      answer="$default"
    fi
    answer="${answer:-$default}"

    case "$answer" in
      '' | *[!0-9]*) printf 'Please enter a number from 1 to %s.\n' "$count" >&2 ;;
      *)
        if [ "$answer" -ge 1 ] && [ "$answer" -le "$count" ]; then
          printf '%s' "$answer"
          return 0
        fi
        printf 'Please enter a number from 1 to %s.\n' "$count" >&2
        ;;
    esac
  done
}

confirm() {
  local prompt="$1"
  local default="${2:-n}"
  local answer suffix

  if [ "${TSUNAMI_ASSUME_YES:-}" = "1" ]; then
    return 0
  fi
  if ! [ -t 0 ] && [ "${TSUNAMI_TEST_SOURCE:-0}" != "1" ]; then
    normalize_yes "$default"
    return
  fi

  if normalize_yes "$default"; then
    suffix="Y/n"
  else
    suffix="y/N"
  fi

  while true; do
    if ! read -r -p "$prompt [$suffix]: " answer; then
      answer="$default"
    fi
    answer="${answer:-$default}"
    if normalize_yes "$answer"; then
      return 0
    fi
    if normalize_no "$answer"; then
      return 1
    fi
    printf 'Please answer y or n.\n' >&2
  done
}

load_state_defaults() {
  [ -f "$STATE_FILE" ] || return 0

  local have_listen="${TSUNAMI_LISTEN+x}"
  local have_password="${TSUNAMI_PASSWORD+x}"
  local have_user="${TSUNAMI_USER+x}"
  local have_public_host="${TSUNAMI_PUBLIC_HOST+x}"
  local current_listen="${TSUNAMI_LISTEN-}"
  local current_password="${TSUNAMI_PASSWORD-}"
  local current_user="${TSUNAMI_USER-}"
  local current_public_host="${TSUNAMI_PUBLIC_HOST-}"
  local loaded_tls_mode

  . "$STATE_FILE" 2>/dev/null || true
  loaded_tls_mode="${TSUNAMI_TLS_MODE:-}"

  [ -z "$have_listen" ] || TSUNAMI_LISTEN="$current_listen"
  [ -z "$have_password" ] || TSUNAMI_PASSWORD="$current_password"
  [ -z "$have_user" ] || TSUNAMI_USER="$current_user"
  [ -z "$have_public_host" ] || TSUNAMI_PUBLIC_HOST="$current_public_host"
  TSUNAMI_PREVIOUS_TLS_MODE="$loaded_tls_mode"
}

mask_secret() {
  local value="$1"
  if [ -z "$value" ]; then
    printf '(generated)'
  elif [ "${#value}" -gt 12 ]; then
    printf '%s...%s' "${value:0:6}" "${value: -4}"
  else
    printf '********'
  fi
}

show_config_summary() {
  local listen="$1"
  local domain="$2"
  local tls_mode="$3"
  local fallback="$4"
  local max_conn="$5"
  local threshold="$6"
  local password="$7"

  printf '\n'
  printf 'Configuration summary:\n'
  printf '  Listen address : %s\n' "$listen"
  printf '  Public host    : %s\n' "$domain"
  printf '  TLS mode       : %s\n' "$tls_mode"
  printf '  Fallback       : %s\n' "${fallback:-built-in page}"
  printf '  Surge max conn : %s\n' "$max_conn"
  printf '  Surge threshold: %s\n' "$threshold"
  printf '  Password       : %s\n' "$(mask_secret "$password")"
  printf '\n'
}

random_password() {
  if has_cmd openssl; then
    openssl rand -base64 24 | tr -d '\n'
    return
  fi
  set +o pipefail
  tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32
  set -o pipefail
}

sha256_hex() {
  local value="$1"
  if has_cmd sha256sum; then
    printf '%s' "$value" | sha256sum | awk '{print $1}'
    return
  fi
  if has_cmd openssl; then
    printf '%s' "$value" | openssl dgst -sha256 -r | awk '{print $1}'
    return
  fi
  die "sha256sum or openssl is required"
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
  die "sha256sum or openssl is required for checksum verification"
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

require_uint() {
  local name="$1"
  local value="$2"
  case "$value" in
    '' | *[!0-9]*) die "$name must be a positive integer" ;;
  esac
  if [ "$value" -le 0 ]; then
    die "$name must be a positive integer"
  fi
}

# ── Binary Installation ──────────────────────────────────────────────────

install_from_local_build() {
  local arch="$1"
  local server_bin="build/$arch/tsunami-server"
  local client_bin="build/$arch/tsunami-client"
  if [ -x "$server_bin" ]; then
    install -m 0755 "$server_bin" "$INSTALL_DIR/tsunami-server"
    if [ -x "$client_bin" ]; then
      install -m 0755 "$client_bin" "$INSTALL_DIR/tsunami-client"
    fi
    return 0
  fi
  return 1
}

install_from_source() {
  if [ ! -d "cmd/tsunami-server" ]; then
    return 1
  fi
  has_cmd go || return 1
  log "building from local source"
  go build -trimpath -ldflags="-s -w" -o "$INSTALL_DIR/tsunami-server" ./cmd/tsunami-server/
  go build -trimpath -ldflags="-s -w" -o "$INSTALL_DIR/tsunami-client" ./cmd/tsunami-client/
  chmod 0755 "$INSTALL_DIR/tsunami-server" "$INSTALL_DIR/tsunami-client"
  return 0
}

release_asset_url() {
  local arch="$1"
  local version="${TSUNAMI_VERSION:-latest}"
  if [ "$version" != "latest" ]; then
    printf 'https://github.com/%s/releases/download/%s/tsunami-%s-%s.tar.gz' "$REPO" "$version" "$version" "$arch"
    return
  fi

  has_cmd curl || die "curl is required to download latest release"
  curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -nE "s/.*\"browser_download_url\": \"([^\"]*tsunami-[^\"]*-$arch\\.tar\\.gz)\".*/\\1/p" \
    | head -n 1
}

install_from_release() {
  local arch="$1"
  local url
  url="$(release_asset_url "$arch")"
  [ -n "$url" ] || die "could not find release asset for $arch"
  has_cmd curl || die "curl is required"
  has_cmd tar || die "tar is required"

  local tmp
  tmp="$(mktemp -d)"

  log "downloading $url"
  curl -fL "$url" -o "$tmp/tsunami.tar.gz"

  # Verify checksum
  local checksums_url="${url%/*}/checksums.sha256"
  log "verifying integrity (checksums.sha256)"
  if curl -fsSL "$checksums_url" -o "$tmp/checksums.sha256"; then
    local tarball_name
    tarball_name="$(basename "$url")"
    local expected
    expected="$(sed -n "s/^\([a-f0-9]\{64\}\)  .*${tarball_name}$/\1/p" "$tmp/checksums.sha256" | head -n 1)"
    if [ -z "$expected" ]; then
      rm -rf "$tmp"
      die "checksum entry not found for $tarball_name in checksums.sha256"
    fi
    local actual
    actual="$(sha256_file "$tmp/tsunami.tar.gz")"
    if [ "$actual" != "$expected" ]; then
      rm -rf "$tmp"
      die "checksum mismatch: expected $expected, got $actual"
    fi
    log "checksum verified"
  else
    rm -rf "$tmp"
    die "failed to download checksums.sha256 — cannot verify binary integrity"
  fi

  tar -xzf "$tmp/tsunami.tar.gz" -C "$tmp"

  local server_bin
  server_bin="$(find "$tmp" -type f -name "tsunami-server*" | head -n 1)"
  [ -n "$server_bin" ] || die "server binary not found in release archive"
  install -m 0755 "$server_bin" "$INSTALL_DIR/tsunami-server"

  local client_bin
  client_bin="$(find "$tmp" -type f -name "tsunami-client*" | head -n 1 || true)"
  if [ -n "$client_bin" ]; then
    install -m 0755 "$client_bin" "$INSTALL_DIR/tsunami-client"
  fi
  rm -rf "$tmp"
}

install_binary() {
  mkdir -p "$INSTALL_DIR"
  local arch
  arch="$(detect_arch)"

  if install_from_source; then
    return
  fi
  if install_from_local_build "$arch"; then
    log "installed local build for $arch"
    return
  fi
  install_from_release "$arch"
}

# ── Let's Encrypt ────────────────────────────────────────────────────────

install_certbot() {
  if has_cmd certbot; then
    return 0
  fi

  log "installing certbot..."
  if has_cmd apt-get; then
    apt-get update -qq
    apt-get install -y -qq certbot >/dev/null
  elif has_cmd yum; then
    yum install -y -q certbot >/dev/null
  elif has_cmd dnf; then
    dnf install -y -q certbot >/dev/null
  elif has_cmd snap; then
    snap install --classic certbot >/dev/null
    [ -L /usr/bin/certbot ] || ln -s /snap/bin/certbot /usr/bin/certbot 2>/dev/null || true
  else
    die "cannot install certbot: no supported package manager found (apt/yum/dnf/snap)"
  fi

  has_cmd certbot || die "certbot installation failed"
  log "certbot installed"
}

obtain_letsencrypt_cert() {
  local domain="$1"
  local cert_dir="/etc/letsencrypt/live/$domain"

  # Already have a valid cert?
  if [ -f "$cert_dir/fullchain.pem" ] && [ -f "$cert_dir/privkey.pem" ]; then
    log "Let's Encrypt certificate for $domain already exists"
    return 0
  fi

  install_certbot

  # Stop tsunami if running (certbot needs port 80 and/or 443)
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true

  local email_flag="--register-unsafely-without-email"
  if [ -n "${TSUNAMI_ACME_EMAIL:-}" ]; then
    email_flag="--email ${TSUNAMI_ACME_EMAIL}"
  fi

  log "requesting Let's Encrypt certificate for $domain ..."
  certbot certonly \
    --standalone \
    --preferred-challenges http \
    -d "$domain" \
    $email_flag \
    --agree-tos \
    --non-interactive \
    --quiet || die "certbot failed. Make sure port 80 is open and DNS points to this server."

  log "certificate obtained: $cert_dir"
}

write_certbot_hooks() {
  local hook_dir
  # certbot renewal hooks
  for stage in pre post; do
    hook_dir="/etc/letsencrypt/renewal-hooks/$stage"
    mkdir -p "$hook_dir"
    if [ "$stage" = "pre" ]; then
      cat >"$hook_dir/tsunami.sh" <<'HOOK'
#!/bin/sh
systemctl stop tsunami-server 2>/dev/null || true
HOOK
    else
      cat >"$hook_dir/tsunami.sh" <<'HOOK'
#!/bin/sh
systemctl start tsunami-server 2>/dev/null || true
HOOK
    fi
    chmod 0755 "$hook_dir/tsunami.sh"
  done
  log "certbot renewal hooks installed"
}

# ── Configuration ────────────────────────────────────────────────────────

write_config() {
  if is_interactive; then
    load_state_defaults
  fi

  mkdir -p "$CONFIG_DIR"
  chmod 0755 "$CONFIG_DIR"

  local listen="${TSUNAMI_LISTEN:-$(ask "Listen address" ":443")}"
  local fallback="${TSUNAMI_FALLBACK:-$(ask "Fallback backend, empty to use built-in page" "")}"
  local cert="${TSUNAMI_CERT_FILE:-}"
  local key="${TSUNAMI_KEY_FILE:-}"
  local password="${TSUNAMI_PASSWORD:-$(random_password)}"
  local user_name="${TSUNAMI_USER:-default}"
  local max_conn="${TSUNAMI_MAX_CONNECTIONS:-$(ask "Surge max connections" "4")}"
  local threshold="${TSUNAMI_THRESHOLD:-$(ask "Surge threshold" "8")}"
  local padding="${TSUNAMI_PADDING_SCHEME_JSON:-$DEFAULT_PADDING_JSON}"
  local domain="${TSUNAMI_PUBLIC_HOST:-}"
  local tls_mode="self-signed"
  local token_hash
  token_hash="$(sha256_hex "$password")"

  # Determine TLS mode
  if [ -n "$cert" ] && [ -n "$key" ]; then
    # User provided cert/key paths
    tls_mode="manual"
  elif [ -z "$cert" ] && [ -z "$key" ]; then
    # Ask about Let's Encrypt
    if [ -z "$domain" ]; then
      domain="$(ask "Server domain (for Let's Encrypt), or press Enter for self-signed" "")"
    fi

    if [ -n "$domain" ] && [ "$domain" != "your-domain.example" ]; then
      local use_le="${TSUNAMI_LETSENCRYPT:-}"
      if [ -z "$use_le" ]; then
        use_le="$(ask "Use Let's Encrypt for $domain? (y/n)" "y")"
      fi
      if [ "$use_le" = "y" ] || [ "$use_le" = "yes" ] || [ "$use_le" = "1" ]; then
        if is_interactive; then
          log "Let's Encrypt requires DNS to point at this server and port 80 to be reachable."
        fi
        obtain_letsencrypt_cert "$domain"
        write_certbot_hooks
        cert="/etc/letsencrypt/live/$domain/fullchain.pem"
        key="/etc/letsencrypt/live/$domain/privkey.pem"
        tls_mode="letsencrypt"
      fi
    fi
  elif { [ -n "$cert" ] && [ -z "$key" ]; } || { [ -z "$cert" ] && [ -n "$key" ]; }; then
    die "cert and key must both be set, or both empty"
  fi

  # If no domain was provided yet (self-signed), ask for public host
  if [ -z "$domain" ] || [ "$domain" = "your-domain.example" ]; then
    domain="$(ask "Public server host for client command" "your-domain.example")"
  fi

  require_uint "max_connections" "$max_conn"
  require_uint "threshold" "$threshold"

  if is_interactive; then
    show_config_summary "$listen" "$domain" "$tls_mode" "$fallback" "$max_conn" "$threshold" "$password"
    confirm "Write this configuration?" "y" || die "configuration cancelled"
  fi

  cat >"$CONFIG_FILE" <<EOF
{
  "server": {
    "listen": "$(json_escape "$listen")",
    "tls": {
      "cert": "$(json_escape "$cert")",
      "key": "$(json_escape "$key")"
    },
    "users": [
      {
        "id": "$(json_escape "$user_name")",
        "name": "$(json_escape "$user_name")",
        "token_hash": "$token_hash"
      }
    ],
    "surge": {
      "mode": "auto",
      "max_connections": $max_conn,
      "threshold": $threshold
    },
    "fallback": "$(json_escape "$fallback")",
    "padding_scheme": "$padding"
  }
}
EOF
  chmod 0600 "$CONFIG_FILE"

  cat >"$STATE_FILE" <<EOF
TSUNAMI_LISTEN=$(shell_escape "$listen")
TSUNAMI_PASSWORD=$(shell_escape "$password")
TSUNAMI_USER=$(shell_escape "$user_name")
TSUNAMI_PUBLIC_HOST=$(shell_escape "$domain")
TSUNAMI_TLS_MODE=$(shell_escape "$tls_mode")
EOF
  chmod 0600 "$STATE_FILE"

  write_client_hint "$listen" "$password" "$domain" "$tls_mode"
}

write_client_hint() {
  local listen="$1"
  local password="$2"
  local host="$3"
  local tls_mode="$4"
  local port="${TSUNAMI_PUBLIC_PORT:-}"
  if [ -z "$port" ]; then
    port="${listen##*:}"
    [ "$port" = "$listen" ] && port="443"
  fi
  local sni="${TSUNAMI_SNI:-$host}"
  local skip_verify="${TSUNAMI_SKIP_VERIFY:-false}"

  # Auto-set skip-verify for self-signed mode
  if [ "$tls_mode" = "self-signed" ]; then
    skip_verify="true"
  fi

  local skip_flag=""
  [ "$skip_verify" = "true" ] && skip_flag=" --skip-verify"

  cat >"$CLIENT_FILE" <<EOF
tsunami-client --server $(shell_escape "${host}:${port}") --password $(shell_escape "$password") --sni $(shell_escape "$sni")${skip_flag} --socks 127.0.0.1:1080 --http 127.0.0.1:8080
EOF
  chmod 0600 "$CLIENT_FILE"
}

# ── Systemd ──────────────────────────────────────────────────────────────

write_systemd() {
  cat >"$SYSTEMD_FILE" <<EOF
[Unit]
Description=TSUNAMI Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$INSTALL_DIR/tsunami-server --config $CONFIG_FILE
Restart=on-failure
RestartSec=3
LimitNOFILE=1048576
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME"
  systemctl restart "$SERVICE_NAME"
}

# ── Connection Info Panel ────────────────────────────────────────────────

print_connection_info() {
  # Read state
  [ -f "$STATE_FILE" ] || return 0
  . "$STATE_FILE" 2>/dev/null || true

  local password="${TSUNAMI_PASSWORD:-???}"
  local host="${TSUNAMI_PUBLIC_HOST:-???}"
  local listen="${TSUNAMI_LISTEN:-:443}"
  local tls_mode="${TSUNAMI_TLS_MODE:-self-signed}"
  local port="${listen##*:}"
  [ "$port" = "$listen" ] && port="443"

  local tls_label
  case "$tls_mode" in
    letsencrypt) tls_label="Let's Encrypt (auto-renew)" ;;
    manual)      tls_label="Manual certificate" ;;
    *)           tls_label="Self-signed (--skip-verify)" ;;
  esac

  # Check service status
  local status_label status_color
  if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
    status_label="● running"
    status_color="$C_GREEN"
  else
    status_label="○ stopped"
    status_color="$C_RED"
  fi

  local skip_flag=""
  [ "$tls_mode" = "self-signed" ] && skip_flag=" \\\\\n    --skip-verify"

  # Mask password for display (show first 6 + last 4)
  local pw_display="$password"
  if [ "${#password}" -gt 12 ]; then
    pw_display="${password:0:6}...${password: -4}"
  fi

  echo ""
  printf "${C_BOX}╔══════════════════════════════════════════════════════════════╗${C_RESET}\n"
  printf "${C_BOX}║${C_RESET}${C_BOLD}           TSUNAMI  Deploy Complete ✓                        ${C_BOX}║${C_RESET}\n"
  printf "${C_BOX}╠══════════════════════════════════════════════════════════════╣${C_RESET}\n"
  printf "${C_BOX}║${C_RESET}  ${C_CYAN}Server :${C_RESET} %-49s ${C_BOX}║${C_RESET}\n" "${host}:${port}"
  printf "${C_BOX}║${C_RESET}  ${C_CYAN}Password:${C_RESET} %-48s ${C_BOX}║${C_RESET}\n" "$pw_display"
  printf "${C_BOX}║${C_RESET}  ${C_CYAN}TLS    :${C_RESET} %-49s ${C_BOX}║${C_RESET}\n" "$tls_label"
  printf "${C_BOX}║${C_RESET}  ${C_CYAN}Status :${C_RESET} ${status_color}%-49s${C_RESET} ${C_BOX}║${C_RESET}\n" "$status_label"
  printf "${C_BOX}╠══════════════════════════════════════════════════════════════╣${C_RESET}\n"
  printf "${C_BOX}║${C_RESET}  ${C_YELLOW}Client command:${C_RESET}                                            ${C_BOX}║${C_RESET}\n"
  printf "${C_BOX}║${C_RESET}                                                              ${C_BOX}║${C_RESET}\n"
  printf "${C_BOX}║${C_RESET}  ${C_GREEN}tsunami-client \\\\${C_RESET}                                          ${C_BOX}║${C_RESET}\n"
  printf "${C_BOX}║${C_RESET}    --server ${C_BOLD}%s${C_RESET} \\\\%-*s${C_BOX}║${C_RESET}\n" "${host}:${port}" $((42 - ${#host} - ${#port})) ""
  printf "${C_BOX}║${C_RESET}    --password '${C_BOLD}%s${C_RESET}' \\\\%-*s${C_BOX}║${C_RESET}\n" "$pw_display" $((39 - ${#pw_display})) ""
  printf "${C_BOX}║${C_RESET}    --sni ${C_BOLD}%s${C_RESET} \\\\%-*s${C_BOX}║${C_RESET}\n" "$host" $((46 - ${#host})) ""
  printf "${C_BOX}║${C_RESET}    --socks 127.0.0.1:1080 \\\\                                  ${C_BOX}║${C_RESET}\n"
  printf "${C_BOX}║${C_RESET}    --http 127.0.0.1:8080                                     ${C_BOX}║${C_RESET}\n"
  printf "${C_BOX}╠══════════════════════════════════════════════════════════════╣${C_RESET}\n"
  printf "${C_BOX}║${C_RESET}  ${C_DIM}Config  : %-48s${C_RESET} ${C_BOX}║${C_RESET}\n" "$CONFIG_FILE"
  printf "${C_BOX}║${C_RESET}  ${C_DIM}Service : systemctl {start|stop|restart} %-17s${C_RESET} ${C_BOX}║${C_RESET}\n" "$SERVICE_NAME"
  if [ "$tls_mode" = "letsencrypt" ]; then
    printf "${C_BOX}║${C_RESET}  ${C_DIM}Cert    : /etc/letsencrypt/live/%-27s${C_RESET} ${C_BOX}║${C_RESET}\n" "${host}/"
  fi
  printf "${C_BOX}╚══════════════════════════════════════════════════════════════╝${C_RESET}\n"
  echo ""

  # Also print the raw command for easy copy-paste
  printf "${C_DIM}Full client command (copy-paste):${C_RESET}\n"
  cat "$CLIENT_FILE"
  echo ""
}

# ── Commands ─────────────────────────────────────────────────────────────

install_all() {
  need_root
  install_binary
  write_config
  write_systemd
  log "installed and started $SERVICE_NAME"
  print_connection_info
}

status_service() {
  systemctl status "$SERVICE_NAME" --no-pager
}

restart_service() {
  need_root
  systemctl restart "$SERVICE_NAME"
  status_service
}

configure_all() {
  need_root
  write_config
  systemctl restart "$SERVICE_NAME"
  log "configuration updated"
  print_connection_info
}

update_binary() {
  need_root
  install_binary
  systemctl restart "$SERVICE_NAME"
  log "binary updated and service restarted"
  status_service
}

logs_service() {
  journalctl -u "$SERVICE_NAME" -f --no-pager
}

show_client() {
  [ -f "$CLIENT_FILE" ] || die "client command not found: $CLIENT_FILE"
  if [ -f "$STATE_FILE" ]; then
    print_connection_info
  else
    cat "$CLIENT_FILE"
  fi
}

cert_status() {
  need_root
  if ! has_cmd certbot; then
    log "certbot is not installed (no Let's Encrypt certificates)"
    return
  fi
  certbot certificates
  echo ""
  log "to force renewal: certbot renew --force-renewal"
}

uninstall_all() {
  need_root
  systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
  rm -f "$SYSTEMD_FILE"
  systemctl daemon-reload
  rm -f "$INSTALL_DIR/tsunami-server" "$INSTALL_DIR/tsunami-client"
  # Clean up certbot hooks
  rm -f /etc/letsencrypt/renewal-hooks/pre/tsunami.sh
  rm -f /etc/letsencrypt/renewal-hooks/post/tsunami.sh
  if [ "${TSUNAMI_KEEP_CONFIG:-1}" = "0" ]; then
    rm -rf "$CONFIG_DIR"
  fi
  log "uninstalled. Set TSUNAMI_KEEP_CONFIG=0 to remove config during uninstall."
}

usage() {
  cat <<EOF
Usage:
  One-click install (remote):
    wget -qO- https://raw.githubusercontent.com/$REPO/main/scripts/install.sh | sudo bash
    curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install.sh | sudo bash

  With options:
    curl -fsSL https://...install.sh | TSUNAMI_PUBLIC_HOST=example.com TSUNAMI_LETSENCRYPT=y sudo -E bash

  Local:
    sudo bash install.sh              Open interactive menu
    sudo bash install.sh install      Install immediately

Commands:
  menu        Open interactive management menu
  install     Install binary, configure, and start service (default for pipe/non-TTY)
  config      Re-configure and restart
  update      Download latest binary and restart
  status      Show service status
  restart     Restart the service
  logs        Follow service logs
  client      Show connection info and client command
  cert        Show Let's Encrypt certificate status
  uninstall   Remove binary, service, and optionally config

Environment:
  TSUNAMI_LISTEN=:443
  TSUNAMI_PASSWORD=<raw client password>
  TSUNAMI_PUBLIC_HOST=example.com
  TSUNAMI_CERT_FILE=/path/fullchain.pem
  TSUNAMI_KEY_FILE=/path/privkey.pem
  TSUNAMI_FALLBACK=127.0.0.1:8080
  TSUNAMI_LETSENCRYPT=y              Auto-approve Let's Encrypt
  TSUNAMI_ACME_EMAIL=you@example.com Email for Let's Encrypt notifications
  TSUNAMI_VERSION=v1.2.3 or latest
  TSUNAMI_ASSUME_YES=1
EOF
}

interactive_install_all() {
  confirm "Install or reinstall $SERVICE_NAME now?" "y" || return 0
  install_all
}

interactive_configure_all() {
  confirm "Reconfigure $SERVICE_NAME and restart it?" "y" || return 0
  configure_all
}

interactive_update_binary() {
  confirm "Update tsunami binaries and restart $SERVICE_NAME?" "y" || return 0
  update_binary
}

interactive_uninstall_all() {
  confirm "Uninstall $SERVICE_NAME?" "n" || return 0
  if confirm "Also remove $CONFIG_DIR?" "n"; then
    TSUNAMI_KEEP_CONFIG=0 uninstall_all
  else
    uninstall_all
  fi
}

run_menu_action() {
  local choice="$1"
  case "$choice" in
    1) interactive_install_all ;;
    2) interactive_configure_all ;;
    3) interactive_update_binary ;;
    4) status_service ;;
    5) show_client ;;
    6) logs_service ;;
    7) cert_status ;;
    8) interactive_uninstall_all ;;
    9) printf 'exit' ;;
    *) return 1 ;;
  esac
}

interactive_menu() {
  local choice
  while true; do
    choice="$(prompt_choice "TSUNAMI management" "5" \
      "Install or reinstall service" \
      "Reconfigure and restart" \
      "Update binary and restart" \
      "Show service status" \
      "Show client connection information" \
      "Follow service logs" \
      "Show Let's Encrypt certificate status" \
      "Uninstall" \
      "Exit")"
    if [ "$choice" = "9" ]; then
      return 0
    fi
    run_menu_action "$choice"
    if [ "$choice" = "6" ]; then
      return 0
    fi
  done
}

main() {
  local cmd="${1:-}"
  if [ -z "$cmd" ]; then
    if is_interactive; then
      cmd="$(default_command_for_context 1)"
    else
      cmd="$(default_command_for_context 0)"
    fi
  fi

  case "$cmd" in
    menu) interactive_menu ;;
    install) install_all ;;
    config | configure) configure_all ;;
    update) update_binary ;;
    status) status_service ;;
    restart) restart_service ;;
    logs) logs_service ;;
    client) show_client ;;
    cert) cert_status ;;
    uninstall) uninstall_all ;;
    -h | --help | help) usage ;;
    *) usage; exit 1 ;;
  esac
}

if [ "${TSUNAMI_TEST_SOURCE:-0}" != "1" ]; then
  main "$@"
fi
