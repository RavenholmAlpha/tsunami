#!/usr/bin/env bash
# shellcheck disable=SC1090,SC2059,SC2086,SC2155
#
# Tsunami — One-click deployment script
#
# Remote install (wget):  wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
# Remote install (curl):  curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
#
# The interactive wizard is quick: domain -> TLS mode -> site camouflage -> confirm.
# A public deployment needs an explicit TLS choice. Recommended:
#   curl -fsSL https://...install.sh | TSUNAMI_PUBLIC_HOST=<your-domain> TSUNAMI_LETSENCRYPT=y sudo -E bash
#
# Local testing only (insecure self-signed certificate):
#   sudo TSUNAMI_TLS_MODE=self-signed TSUNAMI_DEV=1 TSUNAMI_PUBLIC_HOST=dev.tsunami.lan bash install.sh install
#
# Advanced knobs (listen/fallback/surge/front path/secret/site name/padding):
#   sudo bash install.sh advanced        # or TSUNAMI_ADVANCED=1 for one run
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
if [ ! -t 0 ] && [ ! -r /dev/tty ] && [ "${TSUNAMI_TEST_SOURCE:-0}" != "1" ]; then
  TSUNAMI_ASSUME_YES="${TSUNAMI_ASSUME_YES:-1}"
fi

# Server built-in default (pkg/fronting.DefaultPath); only used for reporting.
SERVER_DEFAULT_FRONT_PATH="/assets/update"

# Local ports probed when suggesting a same-server reverse-proxy upstream.
LOCAL_SITE_PORTS="80 8080 3000 8000"

# Site-camouflage settings resolved by resolve_site_settings().
SITE_MODE="builtin"
SITE_UPSTREAM=""
SITE_PRESERVE_HOST="false"
SITE_INSECURE_SKIP_VERIFY="false"

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

warn() {
  printf "${C_YELLOW}[tsunami] warning:${C_RESET} %s\n" "$*" >&2
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
  if is_interactive; then
    if ! answer="$(read_prompt "$prompt [$default]: ")"; then
      answer="$default"
    fi
    printf '%s' "${answer:-$default}"
  else
    printf '%s' "$default"
  fi
}

has_prompt_tty() {
  [ "${TSUNAMI_TEST_HAS_TTY:-0}" = "1" ] || [ -r /dev/tty ]
}

read_prompt() {
  local prompt="$1"
  local answer
  if [ -t 0 ] || [ "${TSUNAMI_TEST_SOURCE:-0}" = "1" ]; then
    read -r -p "$prompt" answer || return 1
  else
    read -r -p "$prompt" answer </dev/tty || return 1
  fi
  printf '%s' "$answer"
}

is_interactive() {
  [ "${TSUNAMI_ASSUME_YES:-}" != "1" ] && { [ -t 0 ] || has_prompt_tty; }
}

default_command_for_context() {
  local stdin_is_tty="$1"
  local prompt_tty="${2:-$stdin_is_tty}"
  if [ "$stdin_is_tty" = "1" ] || [ "$prompt_tty" = "1" ]; then
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

# ── Validation Helpers ───────────────────────────────────────────────────

is_ip_literal() {
  local value="$1"
  case "$value" in
    *:* | *'['* | *']'*) return 0 ;;
  esac
  case "$value" in
    '' | *[!0-9.]*) return 1 ;;
    *.*.*.*) return 0 ;;
    *) return 1 ;;
  esac
}

is_placeholder_domain() {
  local lower="${1,,}"
  case "$lower" in
    localhost | localhost.localdomain | example.com | www.example.com | \
      example.org | www.example.org | example.net | www.example.net | \
      your-domain.example)
      return 0
      ;;
    *.example | *.example.com | *.example.org | *.example.net | *.invalid | *.localhost)
      return 0
      ;;
  esac
  return 1
}

# Prints the reason a value is not a usable public domain name.
# Returns 0 when the value is invalid (a reason was printed), 1 when it is valid.
domain_error() {
  local value="$1"
  local label rest

  if [ -z "$value" ]; then
    printf 'is required (use your real public domain name)'
    return 0
  fi
  case "$value" in
    *[[:space:]]*)
      printf 'must not contain whitespace'
      return 0
      ;;
  esac
  if [ "${#value}" -gt 253 ]; then
    printf 'is longer than 253 characters'
    return 0
  fi
  case "$value" in
    .* | *. | *..*)
      printf 'has an empty DNS label'
      return 0
      ;;
  esac
  if is_ip_literal "$value"; then
    printf 'must be a hostname, not an IP address'
    return 0
  fi
  case "$value" in
    *.*) ;;
    *)
      printf 'must be a fully qualified domain name containing a dot'
      return 0
      ;;
  esac
  case "$value" in
    *[!A-Za-z0-9.-]*)
      printf 'contains invalid characters (allowed: letters, digits, hyphen, dot)'
      return 0
      ;;
  esac
  rest="$value."
  while [ -n "$rest" ]; do
    label="${rest%%.*}"
    rest="${rest#*.}"
    if [ "${#label}" -lt 1 ] || [ "${#label}" -gt 63 ]; then
      printf 'has a DNS label outside the 1-63 character range'
      return 0
    fi
  done
  if is_placeholder_domain "$value"; then
    printf 'is a placeholder or reserved example domain; use your real domain'
    return 0
  fi
  return 1
}

domain_is_valid() {
  local reason
  if reason="$(domain_error "$1")"; then
    return 1
  fi
  return 0
}

require_domain() {
  local name="$1"
  local value="$2"
  local reason
  if reason="$(domain_error "$value")"; then
    if [ -n "$value" ]; then
      die "$name $reason: $value"
    fi
    die "$name $reason"
  fi
}

# Default value for the interactive domain prompt: explicit env/state value,
# then the machine FQDN when it looks like a real public domain.
default_public_domain() {
  local candidate="${TSUNAMI_PUBLIC_HOST:-}"
  if domain_is_valid "$candidate"; then
    printf '%s' "$candidate"
    return 0
  fi
  if has_cmd hostname; then
    candidate="$(hostname -f 2>/dev/null || true)"
    if domain_is_valid "$candidate"; then
      printf '%s' "$candidate"
      return 0
    fi
  fi
  printf ''
}

# Prompts until a valid domain is entered (interactive only; dies otherwise).
ask_domain() {
  local prompt="$1"
  local default="${2:-}"
  local answer reason attempts=0
  while true; do
    attempts=$((attempts + 1))
    if [ "$attempts" -gt 10 ]; then
      die "$prompt: too many invalid entries"
    fi
    answer="$(ask "$prompt" "$default")"
    if reason="$(domain_error "$answer")"; then
      printf 'Invalid domain: %s\n' "$reason" >&2
      if ! is_interactive; then
        die "$prompt: $reason"
      fi
      default=""
      continue
    fi
    printf '%s' "$answer"
    return 0
  done
}

# Like ask_domain, but an empty answer is allowed (self-signed test flow).
ask_domain_optional() {
  local prompt="$1"
  local default="${2:-}"
  local answer reason attempts=0
  while true; do
    attempts=$((attempts + 1))
    if [ "$attempts" -gt 10 ]; then
      die "$prompt: too many invalid entries"
    fi
    answer="$(ask "$prompt" "$default")"
    if [ -z "$answer" ]; then
      return 0
    fi
    if reason="$(domain_error "$answer")"; then
      printf 'Invalid domain: %s\n' "$reason" >&2
      if ! is_interactive; then
        die "$prompt: $reason"
      fi
      default=""
      continue
    fi
    printf '%s' "$answer"
    return 0
  done
}

normalize_tls_mode() {
  case "${1,,}" in
    letsencrypt | le | acme) printf 'letsencrypt' ;;
    manual | cert | certificate) printf 'manual' ;;
    self-signed | selfsigned | self_signed) printf 'self-signed' ;;
    *) return 1 ;;
  esac
}

validate_front_path() {
  local value="$1"
  case "$value" in
    /*) ;;
    *) die "fronting path must start with '/': $value" ;;
  esac
  if [ "$value" = "/" ]; then
    die "fronting path must not be the site root"
  fi
  case "$value" in
    *[[:space:]]* | *\"* | *\'* | *\\*)
      die "fronting path must not contain whitespace, quotes or backslashes: $value"
      ;;
  esac
  case "$value" in
    *..*)
      die "fronting path must not contain '..' segments: $value"
      ;;
  esac
  printf '%s' "$value"
}

# Validates and normalizes an HTTP(S) origin (URL or bare host:port).
validate_http_origin() {
  local label="$1"
  local value="$2"
  local rest
  [ -n "$value" ] || die "$label is required"
  case "$value" in
    *[[:space:]]*) die "$label must not contain whitespace: $value" ;;
  esac
  case "$value" in
    http://?* | https://?*) ;;
    *://*) die "$label must use http or https: $value" ;;
    *) value="http://$value" ;;
  esac
  rest="${value#*://}"
  case "$rest" in
    '' | /*) die "$label needs a host: $value" ;;
  esac
  printf '%s' "$value"
}

# Legacy decoy_proxy env override; empty means "no decoy proxy".
validate_decoy_proxy() {
  local value="$1"
  [ -n "$value" ] || return 0
  validate_http_origin "decoy proxy" "$value"
}

# ── Site Camouflage Helpers ──────────────────────────────────────────────

is_advanced_mode() {
  normalize_yes "${TSUNAMI_ADVANCED:-}"
}

# Parses socket-listing text (ss/lsof) and returns the first local HTTP origin
# found on the probed ports. Returns non-zero when nothing is listening.
local_site_upstream_from_listing() {
  local listing="$1"
  local port
  for port in $LOCAL_SITE_PORTS; do
    if printf '%s\n' "$listing" | grep -Eq "(127\.0\.0\.1|0\.0\.0\.0|\[::\]|\[::1\]|\*)[:.]${port}([^0-9]|$)" >/dev/null 2>&1; then
      printf 'http://127.0.0.1:%s' "$port"
      return 0
    fi
  done
  return 1
}

# Detects a plausible same-server HTTP site for the site-camouflage default.
detect_local_site() {
  local listing=""
  if has_cmd ss; then
    listing="$(ss -ltn 2>/dev/null || true)"
  fi
  if [ -z "$listing" ] && has_cmd lsof; then
    listing="$(lsof -nP -iTCP -sTCP:LISTEN 2>/dev/null || true)"
  fi
  if [ -n "$listing" ]; then
    if local_site_upstream_from_listing "$listing"; then
      return 0
    fi
  fi
  printf ''
  return 0
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

    if ! answer="$(read_prompt "Choice${default:+ [$default]}: ")"; then
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
  if ! is_interactive; then
    normalize_yes "$default"
    return
  fi

  if normalize_yes "$default"; then
    suffix="Y/n"
  else
    suffix="y/N"
  fi

  while true; do
    if ! answer="$(read_prompt "$prompt [$suffix]: ")"; then
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

# Resolves the site-camouflage settings and stores them in the SITE_* globals.
# Arguments: interactive reuse fronting explicit_mode explicit_upstream
#            explicit_preserve explicit_insecure explicit_decoy
resolve_site_settings() {
  local interactive="$1"
  local reuse="$2"
  local fronting="$3"
  local explicit_mode="$4"
  local explicit_upstream="$5"
  local explicit_preserve="$6"
  local explicit_insecure="$7"
  local explicit_decoy="$8"

  SITE_MODE="builtin"
  SITE_UPSTREAM=""
  SITE_PRESERVE_HOST="false"
  SITE_INSECURE_SKIP_VERIFY="false"

  if [ "$fronting" != "true" ]; then
    return 0
  fi

  local mode="" upstream="" preserve="" insecure=""
  local detected="" choice="" default_choice="" default_upstream=""

  if [ "$reuse" = "1" ]; then
    mode="${TSUNAMI_SITE_MODE:-}"
    upstream="${TSUNAMI_SITE_UPSTREAM:-}"
    preserve="${TSUNAMI_SITE_PRESERVE_HOST:-}"
    insecure="${TSUNAMI_SITE_INSECURE_SKIP_VERIFY:-}"
  elif [ -n "$explicit_mode" ]; then
    mode="$explicit_mode"
    upstream="$explicit_upstream"
    preserve="$explicit_preserve"
    insecure="$explicit_insecure"
  elif [ -n "$explicit_decoy" ]; then
    mode="remote"
    upstream="$explicit_decoy"
  fi

  # Legacy state written before site camouflage existed.
  if [ "$reuse" = "1" ] && [ -z "$mode" ] && [ -n "${TSUNAMI_DECOY_PROXY:-}" ]; then
    mode="remote"
    upstream="${upstream:-$TSUNAMI_DECOY_PROXY}"
  fi

  if [ -n "$mode" ]; then
    case "${mode,,}" in
      local) mode="local" ;;
      remote) mode="remote" ;;
      builtin | built-in | built_in) mode="builtin" ;;
      *) die "invalid TSUNAMI_SITE_MODE='$mode' (expected local, remote or builtin)" ;;
    esac
  fi

  if [ -z "$mode" ]; then
    if [ "$interactive" = "1" ]; then
      detected="$(detect_local_site)"
      default_upstream="${TSUNAMI_SITE_UPSTREAM:-}"
      default_choice="2"
      if [ -n "$detected" ]; then
        default_choice="1"
        default_upstream="$detected"
      fi
      if [ -z "$default_upstream" ]; then
        default_upstream="http://127.0.0.1:8080"
      fi
      choice="$(prompt_choice "Site camouflage: what should unauthenticated visitors see?" "$default_choice" \
        "Local real site on this server (reverse proxy)" \
        "Borrow a remote website" \
        "Built-in page (least realistic)")"
      case "$choice" in
        1) mode="local" ;;
        2) mode="remote" ;;
        *) mode="builtin" ;;
      esac
      if [ "$mode" = "local" ]; then
        upstream="$(ask "Local site upstream URL" "$default_upstream")"
        upstream="$(validate_http_origin "site upstream" "$upstream")"
      elif [ "$mode" = "remote" ]; then
        upstream="$(ask "Remote site upstream URL (e.g. https://www.wikipedia.org, https://news.ycombinator.com, https://example.org; empty = built-in page)" "")"
        if [ -z "$upstream" ]; then
          warn "no remote upstream provided; falling back to the built-in page (easy to cluster)"
          mode="builtin"
        else
          upstream="$(validate_http_origin "site upstream" "$upstream")"
        fi
      else
        warn "built-in page selected: it is the easiest deployment to cluster; a local real site or remote upstream is recommended"
      fi
    else
      mode="builtin"
    fi
  fi

  case "$mode" in
    local)
      if [ -z "$upstream" ]; then
        upstream="http://127.0.0.1:8080"
      fi
      upstream="$(validate_http_origin "site upstream" "$upstream")"
      SITE_PRESERVE_HOST="true"
      ;;
    remote)
      if [ -z "$upstream" ]; then
        die "TSUNAMI_SITE_MODE=remote requires TSUNAMI_SITE_UPSTREAM (or TSUNAMI_DECOY_PROXY)"
      fi
      upstream="$(validate_http_origin "site upstream" "$upstream")"
      SITE_PRESERVE_HOST="false"
      ;;
    *)
      SITE_MODE="builtin"
      SITE_UPSTREAM=""
      SITE_PRESERVE_HOST="false"
      SITE_INSECURE_SKIP_VERIFY="false"
      return 0
      ;;
  esac

  if [ -n "$preserve" ]; then
    if normalize_yes "$preserve"; then
      SITE_PRESERVE_HOST="true"
    elif normalize_no "$preserve"; then
      SITE_PRESERVE_HOST="false"
    else
      die "TSUNAMI_SITE_PRESERVE_HOST must be true or false (got '$preserve')"
    fi
  fi
  if [ -n "$insecure" ]; then
    if normalize_yes "$insecure"; then
      SITE_INSECURE_SKIP_VERIFY="true"
    elif normalize_no "$insecure"; then
      SITE_INSECURE_SKIP_VERIFY="false"
    else
      die "TSUNAMI_SITE_INSECURE_SKIP_VERIFY must be true or false (got '$insecure')"
    fi
  fi

  SITE_MODE="$mode"
  SITE_UPSTREAM="$upstream"
}

load_state_defaults() {
  [ -f "$STATE_FILE" ] || return 0

  local have_listen="${TSUNAMI_LISTEN+x}"
  local have_password="${TSUNAMI_PASSWORD+x}"
  local have_user="${TSUNAMI_USER+x}"
  local have_public_host="${TSUNAMI_PUBLIC_HOST+x}"
  local have_tls_mode="${TSUNAMI_TLS_MODE+x}"
  local have_dev="${TSUNAMI_DEV+x}"
  local have_fronting="${TSUNAMI_FRONTING+x}"
  local have_front_path="${TSUNAMI_FRONT_PATH+x}"
  local have_front_secret="${TSUNAMI_FRONT_SECRET+x}"
  local have_site_name="${TSUNAMI_SITE_NAME+x}"
  local have_decoy_proxy="${TSUNAMI_DECOY_PROXY+x}"
  local have_sni="${TSUNAMI_SNI+x}"
  local have_cert_file="${TSUNAMI_CERT_FILE+x}"
  local have_key_file="${TSUNAMI_KEY_FILE+x}"
  local have_site_mode="${TSUNAMI_SITE_MODE+x}"
  local have_site_upstream="${TSUNAMI_SITE_UPSTREAM+x}"
  local have_site_preserve_host="${TSUNAMI_SITE_PRESERVE_HOST+x}"
  local have_site_insecure_skip_verify="${TSUNAMI_SITE_INSECURE_SKIP_VERIFY+x}"
  local current_listen="${TSUNAMI_LISTEN-}"
  local current_password="${TSUNAMI_PASSWORD-}"
  local current_user="${TSUNAMI_USER-}"
  local current_public_host="${TSUNAMI_PUBLIC_HOST-}"
  local current_tls_mode="${TSUNAMI_TLS_MODE-}"
  local current_dev="${TSUNAMI_DEV-}"
  local current_fronting="${TSUNAMI_FRONTING-}"
  local current_front_path="${TSUNAMI_FRONT_PATH-}"
  local current_front_secret="${TSUNAMI_FRONT_SECRET-}"
  local current_site_name="${TSUNAMI_SITE_NAME-}"
  local current_decoy_proxy="${TSUNAMI_DECOY_PROXY-}"
  local current_sni="${TSUNAMI_SNI-}"
  local current_cert_file="${TSUNAMI_CERT_FILE-}"
  local current_key_file="${TSUNAMI_KEY_FILE-}"
  local current_site_mode="${TSUNAMI_SITE_MODE-}"
  local current_site_upstream="${TSUNAMI_SITE_UPSTREAM-}"
  local current_site_preserve_host="${TSUNAMI_SITE_PRESERVE_HOST-}"
  local current_site_insecure_skip_verify="${TSUNAMI_SITE_INSECURE_SKIP_VERIFY-}"

  . "$STATE_FILE" 2>/dev/null || true

  [ -z "$have_listen" ] || TSUNAMI_LISTEN="$current_listen"
  [ -z "$have_password" ] || TSUNAMI_PASSWORD="$current_password"
  [ -z "$have_user" ] || TSUNAMI_USER="$current_user"
  [ -z "$have_public_host" ] || TSUNAMI_PUBLIC_HOST="$current_public_host"
  [ -z "$have_tls_mode" ] || TSUNAMI_TLS_MODE="$current_tls_mode"
  [ -z "$have_dev" ] || TSUNAMI_DEV="$current_dev"
  [ -z "$have_fronting" ] || TSUNAMI_FRONTING="$current_fronting"
  [ -z "$have_front_path" ] || TSUNAMI_FRONT_PATH="$current_front_path"
  [ -z "$have_front_secret" ] || TSUNAMI_FRONT_SECRET="$current_front_secret"
  [ -z "$have_site_name" ] || TSUNAMI_SITE_NAME="$current_site_name"
  [ -z "$have_decoy_proxy" ] || TSUNAMI_DECOY_PROXY="$current_decoy_proxy"
  [ -z "$have_sni" ] || TSUNAMI_SNI="$current_sni"
  [ -z "$have_cert_file" ] || TSUNAMI_CERT_FILE="$current_cert_file"
  [ -z "$have_key_file" ] || TSUNAMI_KEY_FILE="$current_key_file"
  [ -z "$have_site_mode" ] || TSUNAMI_SITE_MODE="$current_site_mode"
  [ -z "$have_site_upstream" ] || TSUNAMI_SITE_UPSTREAM="$current_site_upstream"
  [ -z "$have_site_preserve_host" ] || TSUNAMI_SITE_PRESERVE_HOST="$current_site_preserve_host"
  [ -z "$have_site_insecure_skip_verify" ] || TSUNAMI_SITE_INSECURE_SKIP_VERIFY="$current_site_insecure_skip_verify"
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
  local front_enabled="$8"
  local front_path="$9"
  local site_name="${10}"
  local dev="${11}"
  local site_mode="${12}"
  local site_upstream="${13}"

  local tls_label="$tls_mode"
  local front_label="enabled"
  local site_label=""
  local dev_label="no"

  case "$tls_mode" in
    letsencrypt) tls_label="letsencrypt (real certificate, auto-renew)" ;;
    manual) tls_label="manual (existing certificate files)" ;;
    self-signed) tls_label="self-signed (INSECURE, dev/testing only)" ;;
  esac
  if normalize_no "$front_enabled"; then
    front_label="DISABLED (raw post-TLS mode)"
  fi
  case "$site_mode" in
    local) site_label="local real site -> ${site_upstream:-http://127.0.0.1:8080} (same IP, reverse proxy)" ;;
    remote) site_label="remote borrowed site -> ${site_upstream:-?} (Host rewritten)" ;;
    *) site_label="built-in page (least realistic; easy to cluster)" ;;
  esac
  if [ "$tls_mode" = "self-signed" ] && normalize_yes "$dev"; then
    dev_label="yes (--skip-verify in the client command)"
  fi

  printf '\n'
  printf 'Configuration summary:\n'
  printf '  Public host    : %s\n' "$domain"
  printf '  TLS mode       : %s\n' "$tls_label"
  printf '  Site camouflage: %s\n' "$site_label"
  if ! normalize_no "$front_enabled"; then
    printf '  Front path     : %s\n' "$front_path"
    printf '  Decoy site     : %s\n' "$site_name"
  fi
  printf '  Listen address : %s\n' "$listen"
  printf '  Fallback       : %s\n' "${fallback:-built-in page}"
  printf '  Surge max conn : %s\n' "$max_conn"
  printf '  Surge threshold: %s\n' "$threshold"
  printf '  Password       : %s\n' "$(mask_secret "$password")"
  printf '  Dev mode       : %s\n' "$dev_label"
  printf '\n'
}

random_password() {
  if has_cmd openssl; then
    openssl rand -base64 24 | tr -d '\r\n'
    return
  fi
  set +o pipefail
  tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32
  set -o pipefail
}

random_hex() {
  local bytes="${1:-4}"
  local chars=$((bytes * 2))
  if has_cmd openssl; then
    openssl rand -hex "$bytes" | tr -d '\r\n'
    return
  fi
  set +o pipefail
  tr -dc 'a-f0-9' </dev/urandom | head -c "$chars"
  set -o pipefail
}

# Generate a plausible build-hashed static asset path for fronting.
generate_front_path() {
  local hex
  local -a words=(app bundle chunk index main runtime vendor)
  local word
  hex="$(random_hex 4)"
  word="${words[$((RANDOM % ${#words[@]}))]}"
  case $((RANDOM % 3)) in
    0) printf '/assets/%s.%s.js' "$word" "$hex" ;;
    1) printf '/static/js/main.%s.js' "$hex" ;;
    *) printf '/static/js/%s.%s.js' "$word" "$hex" ;;
  esac
}

derive_site_name() {
  local domain="$1"
  local label="${domain%%.*}"
  local out="" word
  label="${label//-/ }"
  for word in $label; do
    if [ -z "$out" ]; then
      out="${word^}"
    else
      out="$out ${word^}"
    fi
  done
  if [ -n "$out" ]; then
    printf '%s' "$out"
    return
  fi
  printf 'TSUNAMI'
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
  # Explicit environment decisions must survive state-file defaults.
  local env_tls_mode="${TSUNAMI_TLS_MODE:-}"
  local env_cert="${TSUNAMI_CERT_FILE:-}"
  local env_key="${TSUNAMI_KEY_FILE:-}"
  local env_front_path="${TSUNAMI_FRONT_PATH:-}"
  local env_front_secret="${TSUNAMI_FRONT_SECRET:-}"
  local env_site_name="${TSUNAMI_SITE_NAME:-}"
  local env_site_mode="${TSUNAMI_SITE_MODE:-}"
  local env_site_upstream="${TSUNAMI_SITE_UPSTREAM:-}"
  local env_site_preserve="${TSUNAMI_SITE_PRESERVE_HOST:-}"
  local env_site_insecure="${TSUNAMI_SITE_INSECURE_SKIP_VERIFY:-}"
  local env_decoy="${TSUNAMI_DECOY_PROXY:-}"

  local interactive=0 advanced=0 reuse=0 domain_missing=0
  if is_interactive; then
    interactive=1
    load_state_defaults
  fi
  if is_advanced_mode; then
    advanced=1
  fi

  mkdir -p "$CONFIG_DIR"
  chmod 0700 "$CONFIG_DIR"

  local listen fallback max_conn threshold padding
  local domain sni cert key password user_name
  local fronting_enabled="true"
  local front_path="" front_secret="" site_name=""
  local custom_front_path="" custom_front_secret="" custom_site_name=""
  local explicit_mode="" tls_mode="" dev="0"
  local padding_field="" token_hash=""
  local default_domain="" choice="" default_choice=""

  password="${TSUNAMI_PASSWORD:-$(random_password)}"
  user_name="${TSUNAMI_USER:-default}"

  # Interactive re-runs can reuse the previous deployment without prompts.
  if [ "$interactive" = "1" ] && [ "$advanced" = "0" ] && [ -f "$STATE_FILE" ]; then
    if confirm "Existing TSUNAMI settings found. Reuse them?" "y"; then
      reuse=1
    fi
  fi

  # ── Advanced prompts (never shown in the quick flow) ──────────────────
  if [ "$interactive" = "1" ] && [ "$advanced" = "1" ] && [ "$reuse" = "0" ]; then
    listen="$(ask "Listen address" "${TSUNAMI_LISTEN:-:443}")"
    fallback="$(ask "Fallback backend, empty to use built-in page" "${TSUNAMI_FALLBACK:-}")"
    max_conn="$(ask "Surge max connections" "${TSUNAMI_MAX_CONNECTIONS:-4}")"
    threshold="$(ask "Surge threshold" "${TSUNAMI_THRESHOLD:-8}")"
  else
    listen="${TSUNAMI_LISTEN:-:443}"
    fallback="${TSUNAMI_FALLBACK:-}"
    max_conn="${TSUNAMI_MAX_CONNECTIONS:-4}"
    threshold="${TSUNAMI_THRESHOLD:-8}"
  fi
  padding="${TSUNAMI_PADDING_SCHEME_JSON:-}"

  cert="${TSUNAMI_CERT_FILE:-}"
  key="${TSUNAMI_KEY_FILE:-}"
  domain="${TSUNAMI_PUBLIC_HOST:-}"
  sni="${TSUNAMI_SNI:-}"

  if normalize_yes "${TSUNAMI_DEV:-0}"; then
    dev="1"
  fi
  if [ -n "$env_tls_mode" ]; then
    if ! explicit_mode="$(normalize_tls_mode "$env_tls_mode")"; then
      die "invalid TSUNAMI_TLS_MODE='$env_tls_mode' (expected letsencrypt, manual or self-signed)"
    fi
  fi
  if [ -n "$env_cert" ] || [ -n "$env_key" ]; then
    if [ -z "$env_cert" ] || [ -z "$env_key" ]; then
      die "TSUNAMI_CERT_FILE and TSUNAMI_KEY_FILE must both be set, or both unset"
    fi
    if [ -n "$explicit_mode" ] && [ "$explicit_mode" = "self-signed" ]; then
      die "TSUNAMI_TLS_MODE=self-signed cannot be combined with TSUNAMI_CERT_FILE/TSUNAMI_KEY_FILE"
    fi
    cert="$env_cert"
    key="$env_key"
  fi

  # ── Quick flow step 1: public domain ──────────────────────────────────
  if [ "$interactive" = "1" ] && [ "$reuse" = "0" ]; then
    default_domain="$(default_public_domain)"
    domain="$(ask_domain_optional "Public domain for this server (must resolve here; Enter = self-signed test mode)" "$default_domain")"
    if [ -z "$domain" ]; then
      domain_missing=1
      domain="dev.tsunami.lan"
      warn "no public domain supplied: using an INSECURE self-signed test deployment"
      warn "the generated client command uses '$domain' as a placeholder host; edit it for your local test client"
    fi
  fi

  # ── Quick flow step 2: TLS mode ───────────────────────────────────────
  if [ "$domain_missing" = "1" ]; then
    tls_mode="self-signed"
  elif [ -n "$explicit_mode" ]; then
    tls_mode="$explicit_mode"
  elif [ "$reuse" = "1" ]; then
    if ! tls_mode="$(normalize_tls_mode "${TSUNAMI_TLS_MODE:-}")"; then
      if [ -n "$cert" ] && [ -n "$key" ]; then
        tls_mode="manual"
      elif [ -n "$domain" ]; then
        tls_mode="letsencrypt"
      else
        tls_mode="self-signed"
      fi
    fi
  elif [ -n "$env_cert" ] && [ -n "$env_key" ]; then
    tls_mode="manual"
  elif [ "$interactive" = "1" ]; then
    default_choice="1"
    case "${TSUNAMI_TLS_MODE:-}" in
      letsencrypt) default_choice="1" ;;
      manual) default_choice="2" ;;
      self-signed) default_choice="3" ;;
    esac
    choice="$(prompt_choice "TLS mode" "$default_choice" \
      "Let's Encrypt (recommended)" \
      "Existing certificate and key files" \
      "Self-signed (INSECURE, testing only)")"
    case "$choice" in
      1) tls_mode="letsencrypt" ;;
      2) tls_mode="manual" ;;
      *) tls_mode="self-signed" ;;
    esac
  elif normalize_yes "${TSUNAMI_LETSENCRYPT:-}"; then
    tls_mode="letsencrypt"
  else
    die "no safe TLS configuration was provided.

Choose one of:
  Public deployment (Let's Encrypt):  TSUNAMI_PUBLIC_HOST=<your-domain> TSUNAMI_LETSENCRYPT=y
  Public deployment (own certificate): TSUNAMI_TLS_MODE=manual TSUNAMI_PUBLIC_HOST=<your-domain> TSUNAMI_CERT_FILE=/path/fullchain.pem TSUNAMI_KEY_FILE=/path/privkey.pem
  Local testing only (INSECURE):       TSUNAMI_TLS_MODE=self-signed TSUNAMI_DEV=1 TSUNAMI_PUBLIC_HOST=<hostname>

The installer never falls back to a self-signed certificate silently."
  fi

  case "$tls_mode" in
    self-signed)
      if [ "$dev" != "1" ]; then
        if [ "$interactive" = "1" ]; then
          warn "Self-signed certificates are trivially detected and provide no MITM protection."
          warn "Client certificate verification must be disabled (--skip-verify)."
          warn "Use this mode only for local testing, never for a public deployment."
          if ! confirm "Continue with an INSECURE self-signed certificate?" "n"; then
            die "configuration cancelled: choose Let's Encrypt or provide certificate files"
          fi
          dev="1"
        else
          die "TSUNAMI_TLS_MODE=self-signed requires TSUNAMI_DEV=1 (testing only).
For a public deployment use TSUNAMI_PUBLIC_HOST=<your-domain> TSUNAMI_LETSENCRYPT=y
or TSUNAMI_TLS_MODE=manual with TSUNAMI_CERT_FILE and TSUNAMI_KEY_FILE."
        fi
      fi
      warn "Self-signed mode: clients must pass --skip-verify; the SNI may not match the generated certificate."
      cert=""
      key=""
      ;;
    letsencrypt)
      dev="0"
      if [ -z "$domain" ] && [ "$interactive" = "1" ] && [ "$reuse" = "0" ]; then
        domain="$(ask_domain "Public domain for Let's Encrypt" "${TSUNAMI_PUBLIC_HOST:-}")"
      fi
      require_domain "TSUNAMI_PUBLIC_HOST" "$domain"
      if [ -n "$env_cert" ] && [ -n "$env_key" ]; then
        log "using the provided certificate files (skipping certbot)"
        cert="$env_cert"
        key="$env_key"
      else
        if [ "$interactive" = "1" ]; then
          log "Let's Encrypt requires DNS to point at this server and port 80 to be reachable."
        fi
        obtain_letsencrypt_cert "$domain"
        write_certbot_hooks
        cert="/etc/letsencrypt/live/$domain/fullchain.pem"
        key="/etc/letsencrypt/live/$domain/privkey.pem"
      fi
      ;;
    manual)
      dev="0"
      if [ -z "$cert" ] && [ "$interactive" = "1" ] && [ "$reuse" = "0" ]; then
        cert="$(ask "TLS certificate file (fullchain.pem)" "")"
      fi
      if [ -z "$key" ] && [ "$interactive" = "1" ] && [ "$reuse" = "0" ]; then
        key="$(ask "TLS private key file (privkey.pem)" "")"
      fi
      if [ -z "$cert" ] || [ -z "$key" ]; then
        die "manual TLS mode requires TSUNAMI_CERT_FILE and TSUNAMI_KEY_FILE"
      fi
      if [ -z "$domain" ] && [ "$interactive" = "1" ] && [ "$reuse" = "0" ]; then
        domain="$(ask_domain "Public hostname for the certificate (SNI)" "${TSUNAMI_PUBLIC_HOST:-}")"
      fi
      require_domain "TSUNAMI_PUBLIC_HOST" "$domain"
      ;;
    *)
      die "unsupported TLS mode: $tls_mode"
      ;;
  esac

  # Every mode still needs a hostname for SNI: never emit an IP or placeholder.
  if [ "$domain_missing" != "1" ]; then
    if [ -z "$domain" ] && [ "$interactive" = "1" ] && [ "$reuse" = "0" ]; then
      domain="$(ask_domain "Public server hostname (SNI, for example edge.tsunami.io)" "$default_domain")"
    fi
    require_domain "TSUNAMI_PUBLIC_HOST" "$domain"
  fi
  if [ -z "$sni" ]; then
    sni="$domain"
  fi
  require_domain "TSUNAMI_SNI" "$sni"

  # ── Fronting switch and quick flow step 3: site camouflage ────────────
  if [ -n "${TSUNAMI_FRONTING:-}" ]; then
    if normalize_no "$TSUNAMI_FRONTING"; then
      fronting_enabled="false"
    elif ! normalize_yes "$TSUNAMI_FRONTING"; then
      die "TSUNAMI_FRONTING must be 1 or 0 (got '$TSUNAMI_FRONTING')"
    fi
  fi

  resolve_site_settings "$interactive" "$reuse" "$fronting_enabled" \
    "$env_site_mode" "$env_site_upstream" "$env_site_preserve" "$env_site_insecure" "$env_decoy"

  # ── Advanced post-domain prompts (custom fronting/padding values) ─────
  custom_front_path="$env_front_path"
  custom_front_secret="$env_front_secret"
  custom_site_name="$env_site_name"
  if [ "$reuse" = "1" ]; then
    custom_front_path="${TSUNAMI_FRONT_PATH:-}"
    custom_front_secret="${TSUNAMI_FRONT_SECRET:-}"
    custom_site_name="${TSUNAMI_SITE_NAME:-}"
  elif [ "$interactive" = "1" ] && [ "$advanced" = "1" ]; then
    custom_front_path="$(ask "Fronting path override, empty = generated" "${TSUNAMI_FRONT_PATH:-}")"
    custom_front_secret="$(ask "Fronting secret override, empty = generated" "${TSUNAMI_FRONT_SECRET:-}")"
    custom_site_name="$(ask "Decoy site name override, empty = derived from domain" "${TSUNAMI_SITE_NAME:-}")"
    padding="$(ask "Padding scheme JSON override, empty = server default" "${TSUNAMI_PADDING_SCHEME_JSON:-}")"
  fi

  if [ "$fronting_enabled" = "true" ]; then
    if [ -n "$custom_front_path" ]; then
      front_path="$(validate_front_path "$custom_front_path")"
    else
      front_path="$(generate_front_path)"
    fi
    if [ -n "$custom_front_secret" ]; then
      front_secret="$custom_front_secret"
      if [ "$front_secret" = "$password" ]; then
        warn "TSUNAMI_FRONT_SECRET equals the client password; use a separate secret"
      fi
    else
      front_secret="$(random_hex 16)"
    fi
  else
    warn "fronting is disabled (TSUNAMI_FRONTING=0): raw post-TLS mode is not suitable for public deployment"
  fi
  site_name="${custom_site_name:-$(derive_site_name "$domain")}"

  if [ -n "$padding" ]; then
    padding_field=",
    \"padding_scheme\": \"$(json_escape "$padding")\""
  fi

  require_uint "max_connections" "$max_conn"
  require_uint "threshold" "$threshold"
  token_hash="$(sha256_hex "$password")"

  if [ "$interactive" = "1" ]; then
    show_config_summary "$listen" "$domain" "$tls_mode" "$fallback" "$max_conn" "$threshold" "$password" \
      "$fronting_enabled" "$front_path" "$site_name" "$dev" "$SITE_MODE" "$SITE_UPSTREAM"
    confirm "Press Enter to install" "y" || die "configuration cancelled"
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
    "fronting": {
      "enabled": $fronting_enabled,
      "path": "$(json_escape "$front_path")",
      "secret": "$(json_escape "$front_secret")",
      "site_name": "$(json_escape "$site_name")",
      "decoy_proxy": "",
      "site": {
        "mode": "$(json_escape "$SITE_MODE")",
        "upstream": "$(json_escape "$SITE_UPSTREAM")",
        "preserve_host": $SITE_PRESERVE_HOST,
        "insecure_skip_verify": $SITE_INSECURE_SKIP_VERIFY
      }
    }${padding_field}
  }
}
EOF
  chmod 0600 "$CONFIG_FILE"

  cat >"$STATE_FILE" <<EOF
TSUNAMI_LISTEN=$(shell_escape "$listen")
TSUNAMI_PASSWORD=$(shell_escape "$password")
TSUNAMI_USER=$(shell_escape "$user_name")
TSUNAMI_PUBLIC_HOST=$(shell_escape "$domain")
TSUNAMI_SNI=$(shell_escape "$sni")
TSUNAMI_TLS_MODE=$(shell_escape "$tls_mode")
TSUNAMI_DEV=$(shell_escape "$dev")
TSUNAMI_CERT_FILE=$(shell_escape "$cert")
TSUNAMI_KEY_FILE=$(shell_escape "$key")
TSUNAMI_FRONTING=$(shell_escape "$fronting_enabled")
TSUNAMI_FRONT_PATH=$(shell_escape "$front_path")
TSUNAMI_FRONT_SECRET=$(shell_escape "$front_secret")
TSUNAMI_SITE_NAME=$(shell_escape "$site_name")
TSUNAMI_SITE_MODE=$(shell_escape "$SITE_MODE")
TSUNAMI_SITE_UPSTREAM=$(shell_escape "$SITE_UPSTREAM")
TSUNAMI_SITE_PRESERVE_HOST=$(shell_escape "$SITE_PRESERVE_HOST")
TSUNAMI_SITE_INSECURE_SKIP_VERIFY=$(shell_escape "$SITE_INSECURE_SKIP_VERIFY")
TSUNAMI_DECOY_PROXY=
EOF
  chmod 0600 "$STATE_FILE"

  write_client_hint "$listen" "$password" "$domain" "$sni" "$tls_mode" \
    "$fronting_enabled" "$front_path" "$front_secret"
}

write_client_hint() {
  local listen="$1"
  local password="$2"
  local host="$3"
  local sni="$4"
  local tls_mode="$5"
  local front_enabled="$6"
  local front_path="$7"
  local front_secret="$8"
  local port="${TSUNAMI_PUBLIC_PORT:-}"
  if [ -z "$port" ]; then
    port="${listen##*:}"
    if [ "$port" = "$listen" ]; then
      port="443"
    fi
  fi
  local skip_verify="false"
  if [ "$tls_mode" = "self-signed" ]; then
    skip_verify="true"
  fi

  local command
  command="tsunami-client --server $(shell_escape "${host}:${port}") --password $(shell_escape "$password") --sni $(shell_escape "$sni")"
  if normalize_no "$front_enabled"; then
    command="$command --fronting=false"
  else
    command="$command --fronting"
    if [ -n "$front_path" ]; then
      command="$command --front-path $(shell_escape "$front_path")"
    fi
    if [ -n "$front_secret" ]; then
      command="$command --front-secret $(shell_escape "$front_secret")"
    fi
  fi
  if [ "$skip_verify" = "true" ]; then
    command="$command --skip-verify"
  fi
  command="$command --socks 127.0.0.1:1080 --http 127.0.0.1:8080"

  {
    if [ "$skip_verify" = "true" ]; then
      printf '# DEV ONLY: this server uses a self-signed certificate.\n'
      printf '# --skip-verify disables TLS certificate validation and provides no\n'
      printf '# MITM protection. Do not use this command in production.\n'
    fi
    printf '%s\n' "$command"
  } >"$CLIENT_FILE"
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

repeat_char() {
  local char="$1"
  local count="$2"
  local out="" i
  for ((i = 0; i < count; i++)); do
    out="$out$char"
  done
  printf '%s' "$out"
}

# Print a box whose width adapts to the longest line. Content stays ASCII so
# long generated flags/paths cannot break the alignment (and colour codes are
# never mixed into the width math).
print_info_box() {
  local -a lines=("$@")
  local width=0 line padding border first=1
  for line in "${lines[@]}"; do
    if [ "${#line}" -gt "$width" ]; then
      width="${#line}"
    fi
  done
  width=$((width + 2))
  border="$(repeat_char '═' "$width")"
  printf '%s\n' "${C_BOX}╔${border}╗${C_RESET}"
  for line in "${lines[@]}"; do
    padding="$(repeat_char ' ' "$((width - ${#line} - 1))")"
    if [ "$first" = "1" ]; then
      printf '%s\n' "${C_BOX}║${C_RESET} ${C_BOLD}${line}${C_RESET}${padding}${C_BOX}║${C_RESET}"
      first=0
    else
      printf '%s\n' "${C_BOX}║${C_RESET} ${line}${padding}${C_BOX}║${C_RESET}"
    fi
  done
  printf '%s\n' "${C_BOX}╚${border}╝${C_RESET}"
}

print_connection_info() {
  # Read state
  [ -f "$STATE_FILE" ] || return 0
  . "$STATE_FILE" 2>/dev/null || true

  local password="${TSUNAMI_PASSWORD:-???}"
  local host="${TSUNAMI_PUBLIC_HOST:-???}"
  local listen="${TSUNAMI_LISTEN:-:443}"
  local tls_mode="${TSUNAMI_TLS_MODE:-unknown}"
  local front_enabled="${TSUNAMI_FRONTING:-true}"
  local front_path="${TSUNAMI_FRONT_PATH:-}"
  local site_name="${TSUNAMI_SITE_NAME:-}"
  local site_mode="${TSUNAMI_SITE_MODE:-}"
  local site_upstream="${TSUNAMI_SITE_UPSTREAM:-}"
  local decoy="${TSUNAMI_DECOY_PROXY:-}"
  local dev="${TSUNAMI_DEV:-0}"
  local port="${listen##*:}"
  if [ "$port" = "$listen" ]; then
    port="443"
  fi

  local tls_label
  case "$tls_mode" in
    letsencrypt) tls_label="Let's Encrypt (auto-renew)" ;;
    manual) tls_label="Manual certificate" ;;
    self-signed) tls_label="Self-signed DEV ONLY (--skip-verify)" ;;
    *) tls_label="$tls_mode" ;;
  esac

  local status_label="stopped"
  if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
    status_label="running"
  fi

  local front_label="enabled"
  if normalize_no "$front_enabled"; then
    front_label="DISABLED (raw post-TLS mode)"
  fi

  local site_label=""
  case "$site_mode" in
    local) site_label="local -> ${site_upstream:-http://127.0.0.1:8080}" ;;
    remote) site_label="remote -> ${site_upstream:-?}" ;;
    builtin) site_label="built-in page (easy to cluster)" ;;
    *)
      if [ -n "$decoy" ]; then
        site_label="decoy origin -> ${decoy}"
      else
        site_label="built-in page (legacy config; easy to cluster)"
      fi
      ;;
  esac

  local -a lines=(
    "TSUNAMI Deploy Complete"
    "Server       : ${host}:${port}"
    "Password     : $(mask_secret "$password")"
    "TLS          : ${tls_label}"
    "Fronting     : ${front_label}"
  )
  if ! normalize_no "$front_enabled"; then
    lines+=("Front path   : ${front_path:-$SERVER_DEFAULT_FRONT_PATH}")
    lines+=("Site         : ${site_label}")
    lines+=("Decoy site   : ${site_name:-built-in}")
  fi
  lines+=("Status       : ${status_label}")
  lines+=("Config       : ${CONFIG_FILE}")

  echo ""
  print_info_box "${lines[@]}"
  echo ""

  if [ "$tls_mode" = "self-signed" ]; then
    printf '%bDEV ONLY%b: self-signed certificate with --skip-verify. Never expose this server publicly.\n' "$C_YELLOW" "$C_RESET"
    echo ""
  elif ! normalize_no "$front_enabled" && [ -z "$site_upstream" ] && [ "$site_mode" != "builtin" ] && [ -z "$decoy" ]; then
    printf '%bNote%b: no site camouflage origin configured. Use a local real site or a remote upstream to reduce the deployment fingerprint.\n' "$C_DIM" "$C_RESET"
    echo ""
  elif [ "$site_mode" = "builtin" ]; then
    printf '%bNote%b: the built-in page is the easiest deployment to cluster. A local real site or a remote upstream is recommended (run install.sh advanced to change it).\n' "$C_DIM" "$C_RESET"
    echo ""
  fi

  printf 'Client command (copy-paste, also saved at %s):\n' "$CLIENT_FILE"
  if [ -f "$CLIENT_FILE" ]; then
    cat "$CLIENT_FILE"
  else
    printf 'client command file is missing: %s\n' "$CLIENT_FILE"
  fi
  echo ""
}

# ── Commands ─────────────────────────────────────────────────────────────

install_all() {
  need_root
  # Configure first: the quick wizard asks its questions (and a single
  # confirm) before any download or service changes happen.
  write_config
  install_binary
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

# ── Security Self-Check ──────────────────────────────────────────────────

SECURITY_PASSED=0
SECURITY_WARNINGS=0
SECURITY_FAILURES=0

security_pass() {
  SECURITY_PASSED=$((SECURITY_PASSED + 1))
  printf '  %bPASS%b  %s\n' "$C_GREEN" "$C_RESET" "$*"
}

security_warn() {
  SECURITY_WARNINGS=$((SECURITY_WARNINGS + 1))
  printf '  %bWARN%b  %s\n' "$C_YELLOW" "$C_RESET" "$*"
}

security_fail() {
  SECURITY_FAILURES=$((SECURITY_FAILURES + 1))
  printf '  %bFAIL%b  %s\n' "$C_RED" "$C_RESET" "$*"
}

file_mode() {
  local file="$1"
  if stat -c '%a' "$file" >/dev/null 2>&1; then
    stat -c '%a' "$file"
    return
  fi
  stat -f '%Lp' "$file"
}

# The installer targets Linux. Git bash/Cygwin ignore chmod, so permission
# checks are reported as skipped instead of failing there.
permissions_enforced() {
  case "$(uname -s 2>/dev/null || true)" in
    MSYS* | MINGW* | CYGWIN*) return 1 ;;
  esac
  return 0
}

# Best-effort extraction from the installer-generated JSON config (no jq).
config_json_string() {
  local field="$1"
  local value=""
  value="$(grep -m1 -o "\"${field}\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$CONFIG_FILE" 2>/dev/null | sed 's/^"[^"]*"[[:space:]]*:[[:space:]]*"//; s/"$//')" || true
  printf '%s' "$value"
}

config_json_bool() {
  local field="$1"
  local value=""
  value="$(grep -E -m1 -o "\"${field}\"[[:space:]]*:[[:space:]]*(true|false)" "$CONFIG_FILE" 2>/dev/null | sed 's/.*:[[:space:]]*//')" || true
  printf '%s' "$value"
}

# Extracts a string field from within config.json's fronting.site block.
config_json_site_string() {
  local field="$1"
  local value=""
  value="$(awk -v field="$field" '
    /"site"[[:space:]]*:/ { in_site = 1 }
    in_site {
      if (match($0, "\"" field "\"[[:space:]]*:[[:space:]]*\"[^\"]*\"")) {
        value = substr($0, RSTART, RLENGTH)
        sub(/^"[^"]*"[[:space:]]*:[[:space:]]*"/, "", value)
        sub(/"$/, "", value)
        print value
        exit
      }
      if ($0 ~ /}/) { in_site = 0 }
    }
  ' "$CONFIG_FILE" 2>/dev/null)" || true
  printf '%s' "$value"
}

# Best-effort extraction of a flag value from the generated client command.
client_flag_value() {
  local flag="$1"
  local value=""
  if [ -f "$CLIENT_FILE" ]; then
    value="$(sed -n "s/.*${flag} \([^ ]*\).*/\1/p" "$CLIENT_FILE" 2>/dev/null | sed -n '1p')" || true
  fi
  printf '%s' "$value"
}

security_check_summary() {
  printf '\nSummary: %s passed, %s warning(s), %s failure(s)\n' "$SECURITY_PASSED" "$SECURITY_WARNINGS" "$SECURITY_FAILURES"
  if [ "$SECURITY_FAILURES" -gt 0 ]; then
    printf '%bResult: UNSAFE%b - fix the failures above before exposing this server.\n' "$C_RED" "$C_RESET"
    return
  fi
  if [ "$SECURITY_WARNINGS" -gt 0 ]; then
    printf '%bResult: OK with warnings%b - review the warnings for adversarial deployments.\n' "$C_YELLOW" "$C_RESET"
    return
  fi
  printf '%bResult: OK%b\n' "$C_GREEN" "$C_RESET"
}

# Runs the deployment security self-check. Exits non-zero when any check fails.
# Defined with a subshell body so the state-file values it sources cannot leak
# into the caller's environment.
security_check() (
  SECURITY_PASSED=0
  SECURITY_WARNINGS=0
  SECURITY_FAILURES=0

  printf '\n%bTSUNAMI security self-check%b\n' "$C_CYAN" "$C_RESET"
  printf '  Config: %s\n\n' "$CONFIG_FILE"

  if [ ! -f "$CONFIG_FILE" ]; then
    security_fail "config file is missing: $CONFIG_FILE"
    security_check_summary
    exit 1
  fi

  local have_state="no"
  if [ -f "$STATE_FILE" ]; then
    . "$STATE_FILE" 2>/dev/null || true
    have_state="yes"
  fi

  local tls_mode="${TSUNAMI_TLS_MODE:-unknown}"
  local dev="${TSUNAMI_DEV:-0}"
  local domain="${TSUNAMI_PUBLIC_HOST:-}"
  local front_path="${TSUNAMI_FRONT_PATH:-}"
  local front_secret="${TSUNAMI_FRONT_SECRET:-}"
  local decoy="${TSUNAMI_DECOY_PROXY:-}"
  local password="${TSUNAMI_PASSWORD:-}"
  local site_mode="${TSUNAMI_SITE_MODE:-}"
  local site_upstream="${TSUNAMI_SITE_UPSTREAM:-}"
  local cert="" key="" front_enabled="" reason="" mode=""

  cert="$(config_json_string cert)"
  key="$(config_json_string key)"
  front_enabled="$(config_json_bool enabled)"
  if [ -z "$site_mode" ]; then
    site_mode="$(config_json_site_string mode)"
  fi
  if [ -z "$site_upstream" ]; then
    site_upstream="$(config_json_site_string upstream)"
  fi

  # 1. A real certificate, or an explicit dev/self-signed decision.
  if [ -n "$cert" ] && [ -n "$key" ]; then
    if [ -f "$cert" ] && [ -f "$key" ]; then
      security_pass "TLS: certificate configured (cert=$cert)"
    else
      security_fail "TLS: certificate or key file is missing (cert=$cert key=$key)"
    fi
  elif [ "$tls_mode" = "self-signed" ] && normalize_yes "$dev"; then
    security_warn "TLS: self-signed mode with TSUNAMI_DEV=1 (testing only; trivially detected, no MITM protection)"
  else
    security_fail "TLS: no real certificate and no explicit dev mode"
  fi

  # 2. SNI is a real hostname, never a placeholder or an IP address.
  # Fall back to the generated client command when the state file is missing.
  if [ -z "$domain" ]; then
    domain="$(client_flag_value --sni)"
  fi
  if reason="$(domain_error "$domain")"; then
    security_fail "SNI: public host '$domain' $reason"
  else
    security_pass "SNI: $domain"
  fi

  # 3. Fronting enabled.
  if [ "$front_enabled" = "false" ]; then
    security_fail "Fronting: disabled (raw post-TLS mode is not for public deployment)"
  elif [ -z "$front_enabled" ]; then
    security_warn "Fronting: config.json has no fronting block; the server default enables it"
  else
    security_pass "Fronting: enabled"
  fi

  # 4. Non-default fronting path.
  if [ -z "$front_path" ]; then
    front_path="$(config_json_string path)"
  fi
  if [ -z "$front_path" ]; then
    security_warn "Fronting path: not recorded (server uses its built-in default)"
  elif [ "$front_path" = "$SERVER_DEFAULT_FRONT_PATH" ]; then
    security_warn "Fronting path: uses the well-known default $SERVER_DEFAULT_FRONT_PATH"
  else
    security_pass "Fronting path: $front_path (non-default)"
  fi

  # 5. Separate fronting secret (not the client password).
  if [ -z "$front_secret" ]; then
    front_secret="$(config_json_string secret)"
  fi
  if [ -z "$front_secret" ]; then
    security_warn "Fronting secret: empty; falls back to the client password"
  elif [ -n "$password" ] && [ "$front_secret" = "$password" ]; then
    security_warn "Fronting secret: same as the client password"
  else
    security_pass "Fronting secret: separate from the client password"
  fi

  # 6. Real origin for unauthenticated requests (site camouflage or legacy decoy).
  if [ -z "$decoy" ]; then
    decoy="$(config_json_string decoy_proxy)"
  fi
  if [ -n "$decoy" ]; then
    security_pass "Unauthenticated origin: $decoy (a real origin substantially reduces the deployment fingerprint)"
  elif { [ "$site_mode" = "local" ] || [ "$site_mode" = "remote" ]; } && [ -n "$site_upstream" ]; then
    security_pass "Unauthenticated origin: $site_upstream (site camouflage)"
  else
    security_warn "Unauthenticated origin: built-in page (configure a local real site or remote upstream)"
  fi

  # 7. Site camouflage mode.
  case "$site_mode" in
    local)
      if [ -z "$site_upstream" ]; then
        security_fail "Site camouflage: local mode has no upstream (set TSUNAMI_SITE_UPSTREAM)"
      else
        security_pass "Site camouflage: local upstream $site_upstream (the same IP should be serving a real site)"
      fi
      ;;
    remote)
      if [ -z "$site_upstream" ]; then
        security_fail "Site camouflage: remote mode has no upstream"
      else
        security_pass "Site camouflage: remote upstream $site_upstream (review the origin's terms and availability)"
      fi
      ;;
    builtin)
      security_warn "Site camouflage: built-in page - deployment is easy to cluster; configure a local real site or remote upstream"
      ;;
    '')
      security_warn "Site camouflage: not configured (server falls back to the built-in page)"
      ;;
    *)
      security_fail "Site camouflage: invalid mode '$site_mode' (expected local, remote or builtin)"
      ;;
  esac

  # 8. Client command: no --skip-verify with a real certificate, and fronting flags present.
  if [ -f "$CLIENT_FILE" ]; then
    if grep -Fq -- '--skip-verify' "$CLIENT_FILE"; then
      if [ "$tls_mode" = "self-signed" ] && normalize_yes "$dev"; then
        security_warn "Client command: --skip-verify present (expected in explicit dev mode)"
      else
        security_fail "Client command: --skip-verify present with a real certificate"
      fi
    else
      security_pass "Client command: TLS certificate verification is enabled"
    fi
    if [ "$front_enabled" = "false" ]; then
      security_warn "Client command: fronting is disabled for this deployment"
    elif grep -Fq -- '--front-path' "$CLIENT_FILE" && grep -Fq -- '--front-secret' "$CLIENT_FILE"; then
      security_pass "Client command: carries --front-path and --front-secret"
    else
      security_warn "Client command: missing --front-path/--front-secret for the configured fronting"
    fi
  else
    security_warn "Client command: file is missing ($CLIENT_FILE)"
  fi

  # 9. Permissions: config dir not group/other accessible; files not group/other accessible.
  if permissions_enforced; then
    if [ -d "$CONFIG_DIR" ]; then
      mode="$(file_mode "$CONFIG_DIR")"
      case "$mode" in
        *00)
          security_pass "Permissions: $CONFIG_DIR is $mode (no group/other access)"
          ;;
        *)
          security_fail "Permissions: $CONFIG_DIR is $mode; tighten to 0700"
          ;;
      esac
    else
      security_fail "Permissions: config directory $CONFIG_DIR is missing"
    fi
    local checked_file
    for checked_file in "$CONFIG_FILE" "$STATE_FILE" "$CLIENT_FILE"; do
      if [ -f "$checked_file" ]; then
        mode="$(file_mode "$checked_file")"
        case "$mode" in
          *00)
            security_pass "Permissions: $checked_file is $mode"
            ;;
          *)
            security_fail "Permissions: $checked_file is $mode; tighten to 0600"
            ;;
        esac
      fi
    done
  else
    security_warn "Permissions: skipped (this platform does not enforce chmod; deployment targets Linux)"
  fi

  if [ "$have_state" = "no" ]; then
    security_warn "State file is missing ($STATE_FILE); some checks used config.json only"
  fi

  security_check_summary
  if [ "$SECURITY_FAILURES" -gt 0 ]; then
    exit 1
  fi
  exit 0
)

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
  Interactive quick install (recommended):
    curl -fsSL https://raw.githubusercontent.com/$REPO/main/scripts/install.sh | sudo bash
    (opens the management menu when a controlling terminal is available)

  Quick wizard: domain -> TLS mode -> site camouflage -> single confirm.
  Everything else uses safe defaults; advanced knobs are only asked for with
  the 'advanced' command or TSUNAMI_ADVANCED=1.

  Non-interactive public deployment (requires your real domain):
    curl -fsSL https://.../install.sh | \\
      TSUNAMI_PUBLIC_HOST=example.com TSUNAMI_LETSENCRYPT=y sudo -E bash

  Local testing only (INSECURE self-signed certificate):
    sudo TSUNAMI_TLS_MODE=self-signed TSUNAMI_DEV=1 TSUNAMI_PUBLIC_HOST=dev.tsunami.lan bash install.sh install

  Local:
    sudo bash install.sh              Open interactive menu
    sudo bash install.sh install      Quick install immediately
    sudo bash install.sh advanced    Install with the advanced prompts
    sudo bash install.sh config      Reconfigure (offers to reuse settings)

Commands:
  menu        Open interactive management menu
  install     Install binary, configure, and start service (default for pipe/non-TTY)
  advanced    Install/configure with the advanced prompts (TSUNAMI_ADVANCED=1)
  config      Re-configure and restart
  update      Download latest binary and restart
  status      Show service status
  restart     Restart the service
  logs        Follow service logs
  client      Show connection info and client command
  cert        Show Let's Encrypt certificate status
  security    Run a deployment security self-check
  uninstall   Remove binary, service, and optionally config

TLS modes (TSUNAMI_TLS_MODE): letsencrypt (default), manual, self-signed
Site camouflage modes (TSUNAMI_SITE_MODE): local (default upstream
http://127.0.0.1:8080), remote (requires TSUNAMI_SITE_UPSTREAM), builtin

Environment:
  TSUNAMI_LISTEN=:443
  TSUNAMI_PASSWORD=<raw client password>
  TSUNAMI_PUBLIC_HOST=example.com     Real domain name (replace the placeholder)
  TSUNAMI_TLS_MODE=letsencrypt|manual|self-signed
  TSUNAMI_DEV=1                       Required for self-signed (testing only)
  TSUNAMI_CERT_FILE=/path/fullchain.pem
  TSUNAMI_KEY_FILE=/path/privkey.pem
  TSUNAMI_FALLBACK=127.0.0.1:8080
  TSUNAMI_LETSENCRYPT=y               Auto-approve Let's Encrypt
  TSUNAMI_ACME_EMAIL=you@example.com  Email for Let's Encrypt notifications
  TSUNAMI_SITE_MODE=local|remote|builtin
  TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:8080
  TSUNAMI_SITE_PRESERVE_HOST=true|false
  TSUNAMI_SITE_INSECURE_SKIP_VERIFY=true|false
  TSUNAMI_DECOY_PROXY=<legacy alias for remote + upstream>
  TSUNAMI_FRONTING=0                  Disable fronting (not for public deployments)
  TSUNAMI_FRONT_PATH=/assets/app.1a2b3c4d.js
  TSUNAMI_FRONT_SECRET=<separate fronting HMAC secret>
  TSUNAMI_SITE_NAME=<decoy site name>
  TSUNAMI_SNI=<TLS SNI; defaults to TSUNAMI_PUBLIC_HOST>
  TSUNAMI_PADDING_SCHEME_JSON=<override the server default padding scheme>
  TSUNAMI_ADVANCED=1                  Ask the advanced prompts in the wizard
  TSUNAMI_VERSION=v1.3.0 or latest
  TSUNAMI_ASSUME_YES=1
EOF
}

# The single confirmation lives in write_config(), so the menu wrappers must
# not ask a second time.
interactive_install_all() {
  install_all
}

interactive_configure_all() {
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
    9) security_check || true ;;
    10) printf 'exit' ;;
    *) return 1 ;;
  esac
}

interactive_menu() {
  local choice
  while true; do
    choice="$(prompt_choice "TSUNAMI management" "5" \
      "Install / reinstall (quick)" \
      "Reconfigure and restart" \
      "Update binary and restart" \
      "Show service status" \
      "Show client connection information" \
      "Follow service logs" \
      "Show Let's Encrypt certificate status" \
      "Uninstall" \
      "Run deployment security self-check" \
      "Exit")"
    if [ "$choice" = "10" ]; then
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
    advanced) TSUNAMI_ADVANCED=1 install_all ;;
    config | configure) configure_all ;;
    update) update_binary ;;
    status) status_service ;;
    restart) restart_service ;;
    logs) logs_service ;;
    client) show_client ;;
    cert) cert_status ;;
    security) security_check ;;
    uninstall) uninstall_all ;;
    -h | --help | help) usage ;;
    *) usage; exit 1 ;;
  esac
}

if [ "${TSUNAMI_TEST_SOURCE:-0}" != "1" ]; then
  main "$@"
fi
