#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT_DIR/scripts/install.sh"
TESTS_RUN=0

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_eq() {
  local expected="$1"
  local actual="$2"
  local label="$3"
  TESTS_RUN=$((TESTS_RUN + 1))
  if [ "$expected" != "$actual" ]; then
    fail "$label: expected <$expected>, got <$actual>"
  fi
}

assert_success() {
  local label="$1"
  shift
  TESTS_RUN=$((TESTS_RUN + 1))
  "$@" || fail "$label: command failed"
}

assert_failure() {
  local label="$1"
  shift
  TESTS_RUN=$((TESTS_RUN + 1))
  if "$@"; then
    fail "$label: command unexpectedly succeeded"
  fi
}

source_installer() {
  local tmp_dir="$1"
  unset TSUNAMI_LISTEN TSUNAMI_PASSWORD TSUNAMI_USER TSUNAMI_PUBLIC_HOST
  unset TSUNAMI_TLS_MODE TSUNAMI_ASSUME_YES
  export TSUNAMI_TEST_SOURCE=1
  export TSUNAMI_CONFIG_DIR="$tmp_dir/etc/tsunami"
  export TSUNAMI_SERVICE_NAME="tsunami-test"
  # shellcheck source=../scripts/install.sh
  source "$SCRIPT"
}

test_default_dispatch_context() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  assert_eq "menu" "$(default_command_for_context 1)" "tty no-arg dispatch opens menu"
  assert_eq "install" "$(default_command_for_context 0)" "non-tty no-arg dispatch installs"

  rm -rf "$tmp_dir"
}

test_main_explicit_command_bypasses_menu() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  interactive_menu() { printf 'menu\n'; }
  status_service() { printf 'status\n'; }

  output="$(main status)"
  assert_eq "status" "$output" "explicit status command bypasses menu"

  rm -rf "$tmp_dir"
}

test_main_default_menu_path() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  is_interactive() { return 0; }
  interactive_menu() { printf 'menu\n'; }

  output="$(main)"
  assert_eq "menu" "$output" "interactive no-arg main opens menu"

  rm -rf "$tmp_dir"
}

test_main_default_pipe_path() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  is_interactive() { return 1; }
  install_all() { printf 'install\n'; }

  output="$(main)"
  assert_eq "install" "$output" "noninteractive no-arg main installs"

  rm -rf "$tmp_dir"
}

test_load_state_defaults_reuses_previous_values() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  mkdir -p "$tmp_dir/etc/tsunami"
  cat >"$tmp_dir/etc/tsunami/install.env" <<'STATE'
TSUNAMI_LISTEN=:8443
TSUNAMI_PASSWORD=secret-value
TSUNAMI_USER=alice
TSUNAMI_PUBLIC_HOST=example.com
TSUNAMI_TLS_MODE=letsencrypt
STATE
  source_installer "$tmp_dir"

  load_state_defaults

  assert_eq ":8443" "$TSUNAMI_LISTEN" "state default listen"
  assert_eq "secret-value" "$TSUNAMI_PASSWORD" "state default password"
  assert_eq "alice" "$TSUNAMI_USER" "state default user"
  assert_eq "example.com" "$TSUNAMI_PUBLIC_HOST" "state default public host"
  assert_eq "letsencrypt" "$TSUNAMI_TLS_MODE" "state default TLS mode"

  rm -rf "$tmp_dir"
}

test_load_state_defaults_preserves_env_values() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  mkdir -p "$tmp_dir/etc/tsunami"
  cat >"$tmp_dir/etc/tsunami/install.env" <<'STATE'
TSUNAMI_PUBLIC_HOST=state.example
STATE
  source_installer "$tmp_dir"

  TSUNAMI_PUBLIC_HOST=env.example
  load_state_defaults

  assert_eq "env.example" "$TSUNAMI_PUBLIC_HOST" "environment public host wins over state"

  rm -rf "$tmp_dir"
}

test_config_summary_masks_password() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  output="$(show_config_summary ":443" "example.com" "letsencrypt" "127.0.0.1:8080" "4" "8" "super-secret-password")"

  case "$output" in
    *"super-secret-password"*) fail "summary leaked raw password" ;;
  esac
  case "$output" in *":443"*) ;; *) fail "summary missed listen field: $output" ;; esac
  case "$output" in *"example.com"*) ;; *) fail "summary missed host field: $output" ;; esac
  case "$output" in *"letsencrypt"*) ;; *) fail "summary missed TLS field: $output" ;; esac

  rm -rf "$tmp_dir"
}

test_menu_action_dispatches_existing_commands() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  interactive_install_all() { printf 'install\n'; }
  interactive_configure_all() { printf 'config\n'; }
  interactive_update_binary() { printf 'update\n'; }
  status_service() { printf 'status\n'; }
  show_client() { printf 'client\n'; }
  logs_service() { printf 'logs\n'; }
  cert_status() { printf 'cert\n'; }
  interactive_uninstall_all() { printf 'uninstall\n'; }

  output="$(run_menu_action 1)"
  assert_eq "install" "$output" "menu install dispatch"
  output="$(run_menu_action 2)"
  assert_eq "config" "$output" "menu config dispatch"
  output="$(run_menu_action 3)"
  assert_eq "update" "$output" "menu update dispatch"
  output="$(run_menu_action 4)"
  assert_eq "status" "$output" "menu status dispatch"
  output="$(run_menu_action 5)"
  assert_eq "client" "$output" "menu client dispatch"
  output="$(run_menu_action 6)"
  assert_eq "logs" "$output" "menu logs dispatch"
  output="$(run_menu_action 7)"
  assert_eq "cert" "$output" "menu cert dispatch"
  output="$(run_menu_action 8)"
  assert_eq "uninstall" "$output" "menu uninstall dispatch"
  output="$(run_menu_action 9)"
  assert_eq "exit" "$output" "menu exit dispatch"

  rm -rf "$tmp_dir"
}

test_interactive_configure_cancel_skips_write() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  configure_all() { printf 'configured\n'; }

  output="$(printf 'n\n' | interactive_configure_all)"
  assert_eq "" "$output" "cancelled config wrapper produces no write"

  rm -rf "$tmp_dir"
}

test_write_config_cancel_skips_config_file() {
  local tmp_dir output status
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  is_interactive() { return 0; }

  TSUNAMI_LISTEN=:443
  TSUNAMI_PUBLIC_HOST=example.com
  TSUNAMI_LETSENCRYPT=n
  TSUNAMI_PASSWORD=secret-password
  TSUNAMI_MAX_CONNECTIONS=4
  TSUNAMI_THRESHOLD=8

  set +e
  output="$(printf 'n\n' | write_config 2>&1)"
  status=$?
  set -e

  if [ "$status" -eq 0 ]; then
    fail "cancelled write_config unexpectedly succeeded: $output"
  fi
  if [ -f "$tmp_dir/etc/tsunami/config.json" ]; then
    fail "cancelled write_config wrote config file"
  fi

  rm -rf "$tmp_dir"
}

test_prompt_choice_defaults_and_retries() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  output="$(printf '\n' | prompt_choice "Pick one" "2" "First" "Second" 2>/dev/null)"
  assert_eq "2" "$output" "prompt_choice returns default on empty input"

  output="$(printf 'x\n3\n' | prompt_choice "Pick one" "" "First" "Second" "Third" 2>/dev/null)"
  assert_eq "3" "$output" "prompt_choice retries invalid input"

  rm -rf "$tmp_dir"
}

test_confirm_defaults_and_assume_yes() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  run_confirm_default_yes() { printf '\n' | confirm "Proceed?" "y"; }
  run_confirm_explicit_no() { printf 'n\n' | confirm "Proceed?" "y"; }

  assert_success "confirm default yes" run_confirm_default_yes
  assert_failure "confirm explicit no" run_confirm_explicit_no
  TSUNAMI_ASSUME_YES=1
  assert_success "confirm assume yes" confirm "Proceed?" "n"
  unset TSUNAMI_ASSUME_YES

  rm -rf "$tmp_dir"
}

test_default_dispatch_context
test_main_explicit_command_bypasses_menu
test_main_default_menu_path
test_main_default_pipe_path
test_prompt_choice_defaults_and_retries
test_confirm_defaults_and_assume_yes
test_load_state_defaults_reuses_previous_values
test_load_state_defaults_preserves_env_values
test_config_summary_masks_password
test_menu_action_dispatches_existing_commands
test_interactive_configure_cancel_skips_write
test_write_config_cancel_skips_config_file

printf 'ok - %s install script tests\n' "$TESTS_RUN"
