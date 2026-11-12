#!/usr/bin/env bash
# Tests build/openwrt/tsunami.init by sourcing it with stand-ins for the
# rc.common/procd/UCI helpers, then checking the tsunami-client command line it
# would hand to procd. UCI values come from CFG_<option> variables.
#
# The CFG_<option> variables are read by the stand-in config_get below through
# eval, so shellcheck cannot see them being used.
# shellcheck disable=SC2034
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INIT="${INIT_SCRIPT:-$ROOT_DIR/build/openwrt/tsunami.init}"
TESTS_RUN=0

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_eq() {
  TESTS_RUN=$((TESTS_RUN + 1))
  [ "$1" = "$2" ] || fail "$3: expected <$1>, got <$2>"
}

CMD=()
LOGGED=""
INSTANCES=0
RESPAWN=""

config_load() { :; }
config_get() { eval "$1=\"\${CFG_$3:-\${4:-}}\""; }
config_get_bool() { eval "$1=\"\${CFG_$3:-\${4:-0}}\""; }
logger() { LOGGED="$*"; }
procd_open_instance() { INSTANCES=$((INSTANCES + 1)); CMD=(); }
procd_close_instance() { :; }
procd_add_reload_trigger() { RELOAD_TRIGGER="$1"; }
procd_set_param() {
  case "$1" in
    command) shift; CMD=("$@") ;;
    respawn) shift; RESPAWN="$*" ;;
  esac
}
procd_append_param() {
  [ "$1" = command ] || return 0
  shift
  CMD+=("$@")
}

# shellcheck source=/dev/null
source "$INIT"

reset() {
  unset "${!CFG_@}" 2>/dev/null || true
  CMD=() LOGGED="" INSTANCES=0 RESPAWN=""
}

join() {
  local IFS='|'
  printf '%s' "$*"
}

test_disabled_service_starts_nothing() {
  reset
  CFG_server=example.com:443 CFG_password=secret
  CFG_enabled=0
  start_service
  assert_eq 0 "$INSTANCES" "a disabled service starts no instance"
}

test_refuses_to_start_without_credentials() {
  local missing
  for missing in server password; do
    reset
    CFG_enabled=1 CFG_server=example.com:443 CFG_password=secret
    unset "CFG_$missing"
    rc=0
    start_service || rc=$?
    assert_eq 1 "$rc" "missing $missing makes start_service fail"
    assert_eq 0 "$INSTANCES" "missing $missing starts no instance"
    case "$LOGGED" in
      *"set server and password"*) TESTS_RUN=$((TESTS_RUN + 1)) ;;
      *) fail "missing $missing logged <$LOGGED>, want a hint about /etc/config/tsunami" ;;
    esac
  done
}

test_defaults() {
  reset
  CFG_enabled=1 CFG_server=example.com:443 CFG_password=secret
  start_service
  assert_eq 1 "$INSTANCES" "one instance"
  assert_eq "/usr/bin/tsunami-client|--server|example.com:443|--password|secret|--socks|127.0.0.1:1080|--http|127.0.0.1:8080|--fingerprint|chrome" \
    "$(join "${CMD[@]}")" "default command line"
  assert_eq "3600 5 5" "$RESPAWN" "respawn policy"
}

test_every_option_reaches_the_client() {
  reset
  # shellcheck disable=SC2016  # the dollar sign is part of the test password
  CFG_enabled=1 CFG_server=host:8443 CFG_password='p w$d' CFG_sni=cover.example
  CFG_skip_verify=1 CFG_socks=192.168.1.1:1080 CFG_http=192.168.1.1:8080
  CFG_fingerprint=firefox CFG_front_path=/a/b CFG_front_secret=s3 CFG_front_transport=websocket
  CFG_max_connections=2
  start_service
  assert_eq "/usr/bin/tsunami-client|--server|host:8443|--password|p w\$d|--socks|192.168.1.1:1080|--http|192.168.1.1:8080|--fingerprint|firefox|--sni|cover.example|--skip-verify|--front-path|/a/b|--front-secret|s3|--front-transport|websocket|--max-connections|2" \
    "$(join "${CMD[@]}")" "full command line"
}

test_skip_verify_is_opt_in() {
  reset
  CFG_enabled=1 CFG_server=example.com:443 CFG_password=secret CFG_skip_verify=0
  start_service
  case "$(join "${CMD[@]}")" in
    *--skip-verify*) fail "skip_verify=0 still passes --skip-verify" ;;
  esac
  TESTS_RUN=$((TESTS_RUN + 1))
}

test_reloads_when_config_changes() {
  RELOAD_TRIGGER=""
  service_triggers
  assert_eq tsunami "$RELOAD_TRIGGER" "reload trigger"
}

test_disabled_service_starts_nothing
test_refuses_to_start_without_credentials
test_defaults
test_every_option_reaches_the_client
test_skip_verify_is_opt_in
test_reloads_when_config_changes

printf 'OK: %s assertions\n' "$TESTS_RUN"
