#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT_DIR/scripts/install.sh"
TESTS_RUN=0
FAIL_OUTPUT=""

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

assert_ne() {
  local unexpected="$1"
  local actual="$2"
  local label="$3"
  TESTS_RUN=$((TESTS_RUN + 1))
  if [ "$unexpected" = "$actual" ]; then
    fail "$label: value unexpectedly equals <$unexpected>"
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

assert_file_contains() {
  local file="$1"
  local pattern="$2"
  local label="$3"
  TESTS_RUN=$((TESTS_RUN + 1))
  if ! grep -Fq -- "$pattern" "$file"; then
    fail "$label: $file does not contain <$pattern>"
  fi
}

assert_file_not_contains() {
  local file="$1"
  local pattern="$2"
  local label="$3"
  TESTS_RUN=$((TESTS_RUN + 1))
  if grep -Fq -- "$pattern" "$file"; then
    fail "$label: $file unexpectedly contains <$pattern>"
  fi
}

assert_line_count() {
  local expected="$1"
  local file="$2"
  local label="$3"
  local actual
  actual="$(wc -l <"$file" | tr -d ' ')"
  assert_eq "$expected" "$actual" "$label"
}

# Runs a command (usually a function that calls die/exit) in a subshell so a
# deliberate failure cannot terminate the test runner. Captures output.
run_expect_failure() {
  local label="$1"
  shift
  local status
  TESTS_RUN=$((TESTS_RUN + 1))
  set +e
  FAIL_OUTPUT="$( ( "$@" ) 2>&1 )"
  status=$?
  set -e
  if [ "$status" -eq 0 ]; then
    fail "$label: expected failure, got success. Output: $FAIL_OUTPUT"
  fi
}

assert_output_contains() {
  local pattern="$1"
  local label="$2"
  TESTS_RUN=$((TESTS_RUN + 1))
  case "$FAIL_OUTPUT" in
    *"$pattern"*) ;;
    *) fail "$label: output does not contain <$pattern>: $FAIL_OUTPUT" ;;
  esac
}

assert_output_not_contains() {
  local pattern="$1"
  local label="$2"
  TESTS_RUN=$((TESTS_RUN + 1))
  case "$FAIL_OUTPUT" in
    *"$pattern"*) fail "$label: output unexpectedly contains <$pattern>: $FAIL_OUTPUT" ;;
    *) ;;
  esac
}

# Counts fixed-string occurrences in an arbitrary text variable.
text_count() {
  printf '%s\n' "$1" | grep -Fc -- "$2" || true
}

source_installer() {
  local tmp_dir="$1"
  unset TSUNAMI_LISTEN TSUNAMI_PASSWORD TSUNAMI_USER TSUNAMI_PUBLIC_HOST
  unset TSUNAMI_TLS_MODE TSUNAMI_ASSUME_YES TSUNAMI_TEST_HAS_TTY
  unset TSUNAMI_DEV TSUNAMI_FRONTING TSUNAMI_FRONT_PATH TSUNAMI_FRONT_SECRET
  unset TSUNAMI_SITE_NAME TSUNAMI_DECOY_PROXY TSUNAMI_SNI
  unset TSUNAMI_CERT_FILE TSUNAMI_KEY_FILE TSUNAMI_LETSENCRYPT
  unset TSUNAMI_PADDING_SCHEME_JSON TSUNAMI_MAX_CONNECTIONS TSUNAMI_THRESHOLD
  unset TSUNAMI_FALLBACK TSUNAMI_ACME_EMAIL TSUNAMI_PUBLIC_PORT TSUNAMI_SKIP_VERIFY
  unset TSUNAMI_SITE_MODE TSUNAMI_SITE_UPSTREAM TSUNAMI_SITE_PRESERVE_HOST
  unset TSUNAMI_SITE_INSECURE_SKIP_VERIFY TSUNAMI_ADVANCED
  export TSUNAMI_TEST_SOURCE=1
  export TSUNAMI_CONFIG_DIR="$tmp_dir/etc/tsunami"
  export TSUNAMI_SERVICE_NAME="tsunami-test"
  # shellcheck source=../scripts/install.sh
  source "$SCRIPT"
}

make_cert_files() {
  local dir="$1"
  mkdir -p "$dir"
  printf 'test certificate\n' >"$dir/fullchain.pem"
  printf 'test key\n' >"$dir/privkey.pem"
}

state_value() {
  local key="$1"
  sed -n "s/^${key}=//p" "$STATE_FILE"
}

test_default_dispatch_context() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  assert_eq "menu" "$(default_command_for_context 1 1)" "tty no-arg dispatch opens menu"
  assert_eq "menu" "$(default_command_for_context 0 1)" "piped no-arg dispatch opens menu when prompt tty exists"
  assert_eq "install" "$(default_command_for_context 0 0)" "non-tty no-arg dispatch installs without prompt tty"

  rm -rf "$tmp_dir"
}

test_main_piped_with_prompt_tty_opens_menu() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  TSUNAMI_TEST_HAS_TTY=1
  interactive_menu() { printf 'menu\n'; }
  install_all() { printf 'install\n'; }

  output="$(main)"
  assert_eq "menu" "$output" "piped command with prompt tty opens menu"

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

test_load_saved_settings_reads_previous_values() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  mkdir -p "$tmp_dir/etc/tsunami"
  cat >"$tmp_dir/etc/tsunami/install.env" <<'STATE'
TSUNAMI_LISTEN=:8443
TSUNAMI_PASSWORD=secret-value
TSUNAMI_USER=alice
TSUNAMI_PUBLIC_HOST=edge.tsunami.io
TSUNAMI_TLS_MODE=letsencrypt
TSUNAMI_DEV=0
TSUNAMI_FRONTING=true
TSUNAMI_FRONT_PATH=/static/js/main.deadbeef.js
TSUNAMI_FRONT_SECRET=front-secret-value
TSUNAMI_SITE_NAME=Edge
TSUNAMI_DECOY_PROXY=
TSUNAMI_SNI=edge.tsunami.io
TSUNAMI_SITE_MODE=local
TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:3000
TSUNAMI_SITE_PRESERVE_HOST=true
TSUNAMI_SITE_INSECURE_SKIP_VERIFY=false
TSUNAMI_FALLBACK=127.0.0.1:8080
TSUNAMI_MAX_CONNECTIONS=6
TSUNAMI_THRESHOLD=12
TSUNAMI_PADDING_SCHEME_JSON=$'stop=2\nc0=~80-200'
STATE
  source_installer "$tmp_dir"

  load_saved_settings

  assert_eq ":8443" "$(saved_setting TSUNAMI_LISTEN)" "saved listen"
  assert_eq "secret-value" "$(saved_setting TSUNAMI_PASSWORD)" "saved password"
  assert_eq "alice" "$(saved_setting TSUNAMI_USER)" "saved user"
  assert_eq "edge.tsunami.io" "$(saved_setting TSUNAMI_PUBLIC_HOST)" "saved public host"
  assert_eq "letsencrypt" "$(saved_setting TSUNAMI_TLS_MODE)" "saved TLS mode"
  assert_eq "0" "$(saved_setting TSUNAMI_DEV)" "saved dev flag"
  assert_eq "true" "$(saved_setting TSUNAMI_FRONTING)" "saved fronting flag"
  assert_eq "/static/js/main.deadbeef.js" "$(saved_setting TSUNAMI_FRONT_PATH)" "saved front path"
  assert_eq "front-secret-value" "$(saved_setting TSUNAMI_FRONT_SECRET)" "saved front secret"
  assert_eq "Edge" "$(saved_setting TSUNAMI_SITE_NAME)" "saved site name"
  assert_eq "" "$(saved_setting TSUNAMI_DECOY_PROXY)" "saved decoy proxy"
  assert_eq "edge.tsunami.io" "$(saved_setting TSUNAMI_SNI)" "saved SNI"
  assert_eq "local" "$(saved_setting TSUNAMI_SITE_MODE)" "saved site mode"
  assert_eq "http://127.0.0.1:3000" "$(saved_setting TSUNAMI_SITE_UPSTREAM)" "saved site upstream"
  assert_eq "true" "$(saved_setting TSUNAMI_SITE_PRESERVE_HOST)" "saved site preserve_host"
  assert_eq "false" "$(saved_setting TSUNAMI_SITE_INSECURE_SKIP_VERIFY)" "saved site insecure_skip_verify"
  assert_eq "127.0.0.1:8080" "$(saved_setting TSUNAMI_FALLBACK)" "saved fallback"
  assert_eq "6" "$(saved_setting TSUNAMI_MAX_CONNECTIONS)" "saved Surge max connections"
  assert_eq "12" "$(saved_setting TSUNAMI_THRESHOLD)" "saved Surge threshold"
  assert_eq $'stop=2\nc0=~80-200' "$(saved_setting TSUNAMI_PADDING_SCHEME_JSON)" "saved multi-line padding scheme"

  rm -rf "$tmp_dir"
}

# Saved settings are read into SAVED_* only. They must never appear as
# TSUNAMI_* variables, where they would look like operator overrides, and an
# operator's own value must be left alone.
test_load_saved_settings_leaves_the_environment_alone() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  mkdir -p "$tmp_dir/etc/tsunami"
  cat >"$tmp_dir/etc/tsunami/install.env" <<'STATE'
TSUNAMI_PUBLIC_HOST=state.tsunami.io
TSUNAMI_FRONT_PATH=/state/path.11111111.js
TSUNAMI_FRONT_SECRET=state-secret
TSUNAMI_SITE_MODE=local
TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:3000
STATE
  source_installer "$tmp_dir"

  TSUNAMI_PUBLIC_HOST=env.tsunami.io
  load_saved_settings

  assert_eq "env.tsunami.io" "$TSUNAMI_PUBLIC_HOST" "the operator's value is untouched"
  assert_eq "state.tsunami.io" "$(saved_setting TSUNAMI_PUBLIC_HOST)" "the saved value is read separately"
  assert_eq "unset" "${TSUNAMI_SITE_MODE-unset}" "a saved site mode does not become an environment variable"
  assert_eq "unset" "${TSUNAMI_FRONT_PATH-unset}" "a saved front path does not become an environment variable"
  assert_failure "a saved-only setting is not an operator override" operator_set TSUNAMI_SITE_MODE
  assert_success "an environment setting is an operator override" operator_set TSUNAMI_PUBLIC_HOST

  # Showing the client information must not load saved settings either.
  print_connection_info >/dev/null 2>&1
  assert_eq "unset" "${TSUNAMI_SITE_MODE-unset}" "showing client info does not set environment variables"
  assert_eq "env.tsunami.io" "$TSUNAMI_PUBLIC_HOST" "showing client info does not overwrite the operator's value"

  rm -rf "$tmp_dir"
}

test_config_summary_masks_password() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  output="$(show_config_summary ":443" "edge.tsunami.io" "letsencrypt" "127.0.0.1:8080" "4" "8" \
    "super-secret-password" "true" "/static/js/main.deadbeef.js" "Edge" "0" "local" "http://127.0.0.1:3000")"

  case "$output" in
    *"super-secret-password"*) fail "summary leaked raw password" ;;
  esac
  case "$output" in *":443"*) ;; *) fail "summary missed listen field: $output" ;; esac
  case "$output" in *"edge.tsunami.io"*) ;; *) fail "summary missed host field: $output" ;; esac
  case "$output" in *"letsencrypt"*) ;; *) fail "summary missed TLS field: $output" ;; esac
  case "$output" in *"/static/js/main.deadbeef.js"*) ;; *) fail "summary missed front path: $output" ;; esac
  case "$output" in *"local real site -> http://127.0.0.1:3000"*) ;; *) fail "summary missed site camouflage: $output" ;; esac

  rm -rf "$tmp_dir"
}

test_menu_first_entry_is_quick_install() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  prompt_choice() { printf '%s\n' "$*" >&2; printf '10'; }

  output="$(interactive_menu 2>&1)"
  case "$output" in *"Install / reinstall (quick)"*) ;; *) fail "menu first entry is not the quick install: $output" ;; esac
  case "$output" in *"Run deployment security self-check"*) ;; *) fail "menu security entry missing: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 2))

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
  security_check() { printf 'security\n'; }

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
  assert_eq "security" "$output" "menu security dispatch"
  output="$(run_menu_action 10)"
  assert_eq "exit" "$output" "menu exit dispatch"
  assert_failure "menu unknown choice fails" run_menu_action 11

  rm -rf "$tmp_dir"
}

test_interactive_wrappers_do_not_double_confirm() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  install_all() { printf 'installed\n'; }
  configure_all() { printf 'configured\n'; }

  # The single confirmation lives in write_config(); wrappers must not read stdin.
  output="$(printf 'n\n' | interactive_install_all)"
  assert_eq "installed" "$output" "quick install wrapper does not add a confirm"
  output="$(printf 'n\n' | interactive_configure_all)"
  assert_eq "configured" "$output" "reconfigure wrapper does not add a confirm"

  rm -rf "$tmp_dir"
}

test_write_config_cancel_skips_config_file() {
  local tmp_dir output status
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  is_interactive() { return 0; }

  export TSUNAMI_LISTEN=:443
  export TSUNAMI_PUBLIC_HOST=edge.tsunami.io
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_DECOY_PROXY=http://127.0.0.1:8080
  export TSUNAMI_FALLBACK=127.0.0.1:8080
  export TSUNAMI_PASSWORD=secret-password
  export TSUNAMI_MAX_CONNECTIONS=4
  export TSUNAMI_THRESHOLD=8

  # Chain: Enter accepts the domain default, then "n" declines the install.
  set +e
  output="$(printf '\nn\n' | write_config 2>&1)"
  status=$?
  set -e

  if [ "$status" -eq 0 ]; then
    fail "cancelled write_config unexpectedly succeeded: $output"
  fi
  if [ -f "$CONFIG_FILE" ]; then
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

test_ask_reads_input_when_prompt_tty_exists() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  TSUNAMI_TEST_HAS_TTY=1
  output="$(printf 'typed-value\n' | ask "Value" "default")"
  assert_eq "typed-value" "$output" "ask reads prompt tty input even when stdin is piped"

  rm -rf "$tmp_dir"
}

test_confirm_defaults_and_assume_yes() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  TSUNAMI_TEST_HAS_TTY=1
  run_confirm_default_yes() { printf '\n' | confirm "Proceed?" "y"; }
  run_confirm_explicit_no() { printf 'n\n' | confirm "Proceed?" "y"; }

  assert_success "confirm default yes" run_confirm_default_yes
  assert_failure "confirm explicit no" run_confirm_explicit_no
  TSUNAMI_ASSUME_YES=1
  assert_success "confirm assume yes" confirm "Proceed?" "n"
  unset TSUNAMI_ASSUME_YES

  rm -rf "$tmp_dir"
}

test_require_domain_accepts_valid_hosts() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  assert_success "accept edge.tsunami.io" require_domain "host" "edge.tsunami.io"
  assert_success "accept uppercase domain" require_domain "host" "Edge.Tsunami.IO"
  assert_success "accept hyphenated labels" require_domain "host" "vpn.node-1.tsunami.dev"
  assert_success "accept long-ish domain" require_domain "host" "a1.b2.c3.tsunami.io"

  rm -rf "$tmp_dir"
}

test_require_domain_rejects_invalid_values() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  local bad_value
  for bad_value in "" "nodot" "has space.io" "localhost" "127.0.0.1" "::1" "2001:db8::1" \
    "example.com" "www.example.com" "your-domain.example" "foo.example" "foo.invalid" \
    ".leading.io" "trailing.io." "double..dot.io" "under_score.io"; do
    run_expect_failure "require_domain rejects <$bad_value>" require_domain "host" "$bad_value"
  done

  local long_label long_domain
  long_label="$(printf 'a%.0s' {1..64}).tsunami.io"
  run_expect_failure "require_domain rejects a 64-char label" require_domain "host" "$long_label"

  long_domain="$(printf 'abcdefghij.%.0s' {1..26})io"
  run_expect_failure "require_domain rejects >253 chars" require_domain "host" "$long_domain"

  run_expect_failure "require_domain rejects empty with clear reason" require_domain "TSUNAMI_PUBLIC_HOST" ""
  assert_output_contains "TSUNAMI_PUBLIC_HOST is required" "empty domain error names the variable"

  rm -rf "$tmp_dir"
}

test_front_path_generation_and_override() {
  local tmp_dir path1 path2 path3 i
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  path1="$(generate_front_path)"
  path2="$(generate_front_path)"
  path3="$(generate_front_path)"
  assert_ne "$path1" "$path2" "generated front paths are unique"
  assert_ne "$path2" "$path3" "generated front paths are unique (second pair)"
  for i in "$path1" "$path2" "$path3"; do
    case "$i" in
      /*) ;;
      *) fail "generated front path does not start with /: $i" ;;
    esac
    if ! printf '%s' "$i" | grep -Eq '^/[A-Za-z0-9._/-]+\.[0-9a-f]{8}\.js$'; then
      fail "generated front path does not look build-hashed: $i"
    fi
  done
  TESTS_RUN=$((TESTS_RUN + 3))

  assert_eq "/static/js/main.0123abcd.js" \
    "$(validate_front_path "/static/js/main.0123abcd.js")" "front path override is honored"

  run_expect_failure "reject relative front path" validate_front_path "assets/app.js"
  run_expect_failure "reject front path with space" validate_front_path "/assets/app file.js"
  run_expect_failure "reject front path root" validate_front_path "/"
  run_expect_failure "reject front path traversal" validate_front_path "/assets/../x.js"

  assert_eq "http://127.0.0.1:8080" "$(validate_decoy_proxy "127.0.0.1:8080")" \
    "decoy proxy host:port gets an http:// scheme"
  assert_eq "https://origin.example.net" "$(validate_decoy_proxy "https://origin.example.net")" \
    "decoy proxy https URL is preserved"
  run_expect_failure "reject non-http decoy scheme" validate_decoy_proxy "ftp://127.0.0.1:8080"
  run_expect_failure "reject decoy proxy without a host" validate_decoy_proxy "https://"

  rm -rf "$tmp_dir"
}

test_random_hex_and_site_name_helpers() {
  local tmp_dir value
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  value="$(random_hex 8)"
  assert_eq "16" "${#value}" "random_hex 8 returns 16 characters"
  if ! printf '%s' "$value" | grep -Eq '^[0-9a-f]+$'; then
    fail "random_hex returned non-hex characters: $value"
  fi
  TESTS_RUN=$((TESTS_RUN + 1))

  assert_eq "Tsunami" "$(derive_site_name "tsunami.io")" "site name derives first label"
  assert_eq "My Site" "$(derive_site_name "my-site.tsunami.io")" "site name replaces hyphens"
  assert_eq "Edge" "$(derive_site_name "edge.tsunami.io")" "site name title-cases first label"

  rm -rf "$tmp_dir"
}

test_write_config_manual_json_and_client_hint() {
  local tmp_dir front_path front_secret state_path state_secret state_tls password
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  password="test-password-1234567890"
  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="$password"
  export TSUNAMI_DECOY_PROXY="http://127.0.0.1:8080"
  export TSUNAMI_FRONT_PATH="/static/js/main.0123abcd.js"

  write_config 2>/dev/null

  assert_file_contains "$CONFIG_FILE" '"fronting": {' "config contains fronting block"
  assert_file_contains "$CONFIG_FILE" '"enabled": true' "config enables fronting"
  assert_file_contains "$CONFIG_FILE" '"path": "/static/js/main.0123abcd.js"' "config uses front path override"
  assert_file_contains "$CONFIG_FILE" '"site_name": "Edge"' "config derives site name"
  assert_file_contains "$CONFIG_FILE" '"decoy_proxy": ""' "config keeps decoy_proxy empty for compatibility"
  assert_file_contains "$CONFIG_FILE" '"mode": "remote"' "decoy env maps to remote site mode"
  assert_file_contains "$CONFIG_FILE" '"upstream": "http://127.0.0.1:8080"' "decoy env becomes the site upstream"
  assert_file_contains "$CONFIG_FILE" '"preserve_host": false' "remote site rewrites the Host header"
  assert_file_contains "$CONFIG_FILE" '"insecure_skip_verify": false' "site upstream verification stays enabled"
  assert_file_contains "$CONFIG_FILE" "\"cert\": \"$tmp_dir/certs/fullchain.pem\"" "config records certificate"
  assert_file_contains "$CONFIG_FILE" "\"key\": \"$tmp_dir/certs/privkey.pem\"" "config records key"
  assert_file_not_contains "$CONFIG_FILE" '"padding_scheme"' "config omits padding_scheme by default"

  state_path="$(state_value TSUNAMI_FRONT_PATH)"
  state_secret="$(state_value TSUNAMI_FRONT_SECRET)"
  state_tls="$(state_value TSUNAMI_TLS_MODE)"
  assert_eq "/static/js/main.0123abcd.js" "$state_path" "state records front path"
  assert_eq "manual" "$state_tls" "state records TLS mode"
  assert_ne "$password" "$state_secret" "front secret is separate from the password"
  assert_eq "32" "${#state_secret}" "generated front secret is 32 hex characters"
  assert_file_contains "$CONFIG_FILE" "\"secret\": \"$state_secret\"" "config records the separate fronting secret"
  assert_eq "1" "$(grep -c '^TSUNAMI_DECOY_PROXY=' "$STATE_FILE")" "state records decoy proxy for compatibility"
  assert_eq "remote" "$(state_value TSUNAMI_SITE_MODE)" "state records site mode"
  assert_eq "http://127.0.0.1:8080" "$(state_value TSUNAMI_SITE_UPSTREAM)" "state records site upstream"
  assert_eq "false" "$(state_value TSUNAMI_SITE_PRESERVE_HOST)" "state records preserve_host"

  assert_file_contains "$CLIENT_FILE" "--fronting " "client command enables fronting"
  assert_file_contains "$CLIENT_FILE" "--front-path /static/js/main.0123abcd.js" "client command passes front path"
  assert_file_contains "$CLIENT_FILE" "--front-secret $state_secret" "client command passes front secret"
  assert_file_contains "$CLIENT_FILE" "--sni edge.tsunami.io" "client command passes SNI"
  assert_file_not_contains "$CLIENT_FILE" "--skip-verify" "manual TLS client command does not skip verification"
  assert_line_count "1" "$CLIENT_FILE" "manual TLS client command is a single line"

  if permissions_enforced; then
    assert_eq "700" "$(file_mode "$CONFIG_DIR")" "config directory is 0700"
    assert_eq "600" "$(file_mode "$CONFIG_FILE")" "config file is 0600"
    assert_eq "600" "$(file_mode "$STATE_FILE")" "state file is 0600"
    assert_eq "600" "$(file_mode "$CLIENT_FILE")" "client command file is 0600"
  fi

  local output
  output="$(print_connection_info)"
  case "$output" in *"/static/js/main.0123abcd.js"*) ;; *) fail "connection info missed front path: $output" ;; esac
  case "$output" in *"Site         : remote -> http://127.0.0.1:8080"*) ;; *) fail "connection info missed site camouflage: $output" ;; esac
  case "$output" in *"test-p...7890"*) ;; *) fail "connection info did not mask the password in the box: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 3))

  rm -rf "$tmp_dir"
}

test_write_config_requires_tls_decision() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  export TSUNAMI_ASSUME_YES=1

  run_expect_failure "non-interactive write_config without TLS decision fails" write_config
  assert_output_contains "TSUNAMI_LETSENCRYPT=y" "error lists the Let's Encrypt option"
  assert_output_contains "TSUNAMI_TLS_MODE=self-signed" "error lists the explicit self-signed option"
  assert_output_contains "TSUNAMI_DEV=1" "error requires TSUNAMI_DEV for self-signed"
  if [ -f "$CONFIG_FILE" ]; then
    fail "failed TLS decision wrote a config file"
  fi
  TESTS_RUN=$((TESTS_RUN + 1))

  rm -rf "$tmp_dir"
}

test_write_config_self_signed_requires_dev() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=self-signed
  export TSUNAMI_PUBLIC_HOST="dev.tsunami.io"
  export TSUNAMI_PASSWORD="dev-password-123456"

  run_expect_failure "self-signed without TSUNAMI_DEV fails" write_config
  assert_output_contains "TSUNAMI_DEV=1" "self-signed failure explains TSUNAMI_DEV"
  if [ -f "$CONFIG_FILE" ]; then
    fail "rejected self-signed config was written"
  fi
  TESTS_RUN=$((TESTS_RUN + 1))

  rm -rf "$tmp_dir"
}

test_write_config_self_signed_dev_skip_verify() {
  local tmp_dir front_path
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=self-signed
  export TSUNAMI_DEV=1
  export TSUNAMI_PUBLIC_HOST="dev.tsunami.io"
  export TSUNAMI_PASSWORD="dev-password-123456"

  write_config 2>/dev/null

  assert_file_contains "$CONFIG_FILE" '"cert": ""' "self-signed config has no certificate path"
  assert_file_contains "$CONFIG_FILE" '"enabled": true' "self-signed config keeps fronting enabled"
  front_path="$(state_value TSUNAMI_FRONT_PATH)"
  assert_eq "/" "${front_path:0:1}" "self-signed config has a generated front path"
  assert_file_contains "$CLIENT_FILE" "--skip-verify" "dev client command skips verification"
  assert_file_contains "$CLIENT_FILE" "DEV ONLY" "dev client command is labelled DEV ONLY"
  assert_eq "1" "$(state_value TSUNAMI_DEV)" "state records dev mode"

  rm -rf "$tmp_dir"
}

test_write_config_fronting_disabled() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=self-signed
  export TSUNAMI_DEV=1
  export TSUNAMI_PUBLIC_HOST="dev.tsunami.io"
  export TSUNAMI_PASSWORD="dev-password-123456"
  export TSUNAMI_FRONTING=0

  write_config 2>/dev/null

  assert_file_contains "$CONFIG_FILE" '"enabled": false' "raw mode writes fronting.enabled=false"
  assert_file_contains "$CLIENT_FILE" "--fronting=false" "raw mode client command disables fronting"
  assert_file_not_contains "$CLIENT_FILE" "--front-path" "raw mode client command has no front path"

  run_expect_failure "security check fails for raw mode" security_check
  assert_output_contains "Fronting: disabled" "security check reports disabled fronting"

  rm -rf "$tmp_dir"
}

test_write_config_padding_override_and_default_omitted() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"

  export TSUNAMI_PADDING_SCHEME_JSON='stop=8
0=30-30'
  write_config 2>/dev/null
  assert_file_contains "$CONFIG_FILE" '"padding_scheme":' "explicit padding override is written"
  assert_file_contains "$CONFIG_FILE" 'stop=8\n0=30-30' "padding newlines are JSON-escaped"

  unset TSUNAMI_PADDING_SCHEME_JSON
  rm -f "$CONFIG_FILE"
  write_config 2>/dev/null
  assert_file_not_contains "$CONFIG_FILE" '"padding_scheme"' "default config omits padding_scheme"

  rm -rf "$tmp_dir"
}

test_write_config_rejects_placeholder_host() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_PUBLIC_HOST="example.com"

  run_expect_failure "placeholder public host is rejected" write_config
  assert_output_contains "placeholder or reserved example domain" "placeholder rejection is explained"
  if [ -f "$CONFIG_FILE" ]; then
    fail "rejected placeholder host wrote a config file"
  fi
  TESTS_RUN=$((TESTS_RUN + 1))

  rm -rf "$tmp_dir"
}

test_interactive_tls_prompt_defaults_to_letsencrypt() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  is_interactive() { return 0; }
  obtain_letsencrypt_cert() { printf 'certbot-stub\n'; }
  write_certbot_hooks() { :; }

  export TSUNAMI_LISTEN=:443
  export TSUNAMI_FALLBACK=127.0.0.1:8080
  export TSUNAMI_MAX_CONNECTIONS=4
  export TSUNAMI_THRESHOLD=8
  export TSUNAMI_PUBLIC_HOST="cdn.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_SITE_MODE=builtin

  output="$(printf '\n\ny\n' | write_config 2>&1)"

  case "$output" in *"certbot-stub"*) ;; *) fail "default TLS choice did not select Let's Encrypt: $output" ;; esac
  case "$output" in *"Self-signed (INSECURE"*) ;; *) fail "interactive TLS prompt did not offer the self-signed option: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 2))
  assert_file_contains "$CONFIG_FILE" '"cert": "/etc/letsencrypt/live/cdn.tsunami.io/fullchain.pem"' "Let's Encrypt certificate path is written"
  assert_eq "letsencrypt" "$(state_value TSUNAMI_TLS_MODE)" "state records Let's Encrypt mode"

  rm -rf "$tmp_dir"
}

test_interactive_self_signed_requires_confirmation() {
  local tmp_dir output status
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  is_interactive() { return 0; }
  export TSUNAMI_LISTEN=:443
  export TSUNAMI_FALLBACK=127.0.0.1:8080
  export TSUNAMI_MAX_CONNECTIONS=4
  export TSUNAMI_THRESHOLD=8
  export TSUNAMI_PUBLIC_HOST="dev.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_SITE_MODE=builtin

  # Enter accepts the domain default, "3" selects self-signed, "n" declines.
  set +e
  output="$(printf '\n3\nn\n' | write_config 2>&1)"
  status=$?
  set -e

  if [ "$status" -eq 0 ]; then
    fail "declined self-signed confirmation unexpectedly succeeded: $output"
  fi
  case "$output" in *"trivially detected"*) ;; *) fail "self-signed warning block is missing: $output" ;; esac
  if [ -f "$CONFIG_FILE" ]; then
    fail "declined self-signed confirmation wrote a config file"
  fi
  TESTS_RUN=$((TESTS_RUN + 1))

  rm -rf "$tmp_dir"
}

test_security_check_missing_and_unsafe() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  run_expect_failure "security check fails without config" security_check
  assert_output_contains "config file is missing" "missing config failure is explained"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_DECOY_PROXY="http://127.0.0.1:8080"
  write_config 2>/dev/null

  # A --skip-verify flag with a real certificate must fail the self-check.
  printf ' --skip-verify\n' >>"$CLIENT_FILE"
  run_expect_failure "security check fails when a real certificate client skips verification" security_check
  assert_output_contains "skip-verify present with a real certificate" "skip-verify failure is explained"

  # A world-readable state file must fail on platforms that enforce permissions.
  if permissions_enforced; then
    chmod 0644 "$STATE_FILE"
    run_expect_failure "security check fails on world-readable state file" security_check
    chmod 0600 "$STATE_FILE"
  fi

  rm -rf "$tmp_dir"
}

test_security_check_generated_config_ok() {
  local tmp_dir output status
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_FRONT_PATH="/static/js/main.0123abcd.js"
  export TSUNAMI_DECOY_PROXY="http://127.0.0.1:8080"

  write_config 2>/dev/null

  set +e
  output="$(security_check 2>&1)"
  status=$?
  set -e

  assert_eq "0" "$status" "security check passes for the generated safe config"
  case "$output" in *"PASS  TLS: certificate configured"*) ;; *) fail "security check missing TLS pass: $output" ;; esac
  case "$output" in *"SNI: edge.tsunami.io"*) ;; *) fail "security check missing SNI pass: $output" ;; esac
  case "$output" in *"Fronting: enabled"*) ;; *) fail "security check missing fronting pass: $output" ;; esac
  case "$output" in *"Site camouflage: remote upstream http://127.0.0.1:8080"*) ;; *) fail "security check missing remote site pass: $output" ;; esac
  case "$output" in *"Result: UNSAFE"*) fail "security check reported UNSAFE for safe config: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 5))

  rm -rf "$tmp_dir"
}

test_security_check_permission_branch() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_DECOY_PROXY="http://127.0.0.1:8080"
  write_config 2>/dev/null

  # Simulate a platform that enforces chmod (CI runs the real checks on Linux).
  permissions_enforced() { return 0; }
  file_mode() { printf '0644'; }
  run_expect_failure "security check fails when files are world-readable (simulated)" security_check
  assert_output_contains "tighten to 0600" "permission failure explains the required mode"

  file_mode() { printf '0600'; }
  run_security_check_quiet() { security_check >/dev/null 2>&1; }
  assert_success "security check passes with 0600 files (simulated)" run_security_check_quiet

  rm -rf "$tmp_dir"
}

test_print_connection_info_handles_long_values() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_FRONT_PATH="/static/js/main.0123456789abcdef0123456789abcdef.js"
  export TSUNAMI_DECOY_PROXY="https://a-very-long-decoy-origin-name.internal.example.net:8443"
  export TSUNAMI_SITE_NAME="A Long Decoy Site Name"

  write_config 2>/dev/null

  output="$(print_connection_info)"
  case "$output" in *"/static/js/main.0123456789abcdef0123456789abcdef.js"*) ;; *) fail "info panel missed long front path: $output" ;; esac
  case "$output" in *"https://a-very-long-decoy-origin-name.internal.example.net:8443"*) ;; *) fail "info panel missed long decoy origin: $output" ;; esac
  case "$output" in *"TSUNAMI Deploy Complete"*) ;; *) fail "info panel missed the title: $output" ;; esac
  case "$output" in *"test-p...3456"*) ;; *) fail "info panel box did not mask the password: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 4))

  rm -rf "$tmp_dir"
}

test_quick_wizard_single_confirm() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  is_interactive() { return 0; }
  obtain_letsencrypt_cert() { printf 'certbot-stub\n'; }
  write_certbot_hooks() { :; }
  # ask prompts are not visible when stdin is a pipe; mark them explicitly.
  ask() { printf 'ASK:%s\n' "$1" >&2; printf '%s' "$2"; }
  confirm() { printf 'CONFIRM:%s\n' "$1"; return 0; }

  export TSUNAMI_PUBLIC_HOST="cdn.tsunami.dev"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_SITE_UPSTREAM="http://127.0.0.1:3000"

  # TLS Enter (default) and site Enter (local default); ask() supplies the rest.
  output="$(printf '\n\n' | write_config 2>&1)"

  assert_eq "1" "$(text_count "$output" 'CONFIRM:Press Enter to install')" \
    "quick wizard asks exactly one final confirmation"
  case "$output" in *"ASK:Public domain"*) ;; *) fail "quick wizard did not ask for the domain: $output" ;; esac
  case "$output" in *"Site camouflage: what should unauthenticated visitors see?"*) ;; *) fail "site camouflage prompt missing: $output" ;; esac
  case "$output" in *"Local real site on this server"*) ;; *) fail "site camouflage choices missing: $output" ;; esac
  case "$output" in *"ASK:Listen address"*) fail "quick wizard asked an advanced listen prompt: $output" ;; esac
  case "$output" in *"ASK:Fallback backend"*) fail "quick wizard asked an advanced fallback prompt: $output" ;; esac
  case "$output" in *"ASK:Fronting path override"*) fail "quick wizard asked an advanced fronting prompt: $output" ;; esac
  case "$output" in *"ASK:Surge max connections"*) fail "quick wizard asked an advanced surge prompt: $output" ;; esac
  case "$output" in *"ASK:Padding scheme JSON override"*) fail "quick wizard asked an advanced padding prompt: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 8))

  assert_file_contains "$CONFIG_FILE" '"mode": "local"' "quick wizard writes local site mode"
  assert_file_contains "$CONFIG_FILE" '"upstream": "http://127.0.0.1:3000"' "quick wizard uses the configured local upstream"
  assert_file_contains "$CONFIG_FILE" '"preserve_host": true' "local site preserves the Host header"
  assert_file_contains "$CONFIG_FILE" '"cert": "/etc/letsencrypt/live/cdn.tsunami.dev/fullchain.pem"' "LE certificate path written"
  assert_file_contains "$CLIENT_FILE" "--front-path" "quick wizard client command carries front path"
  assert_file_contains "$CLIENT_FILE" "--front-secret" "quick wizard client command carries front secret"
  assert_file_not_contains "$CLIENT_FILE" "--skip-verify" "LE quick wizard client command verifies certificates"

  rm -rf "$tmp_dir"
}

test_quick_wizard_uses_fqdn_default() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  is_interactive() { return 0; }
  default_public_domain() { printf 'auto.tsunami.io'; }
  ask() { printf '%s' "$2"; }
  confirm() { printf 'CONFIRM\n'; return 0; }
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_SITE_MODE=builtin
  export TSUNAMI_PASSWORD="test-password-123456"

  write_config >/dev/null 2>&1

  assert_eq "auto.tsunami.io" "$(state_value TSUNAMI_PUBLIC_HOST)" "domain prompt default comes from the FQDN"
  assert_file_contains "$CONFIG_FILE" '"site_name": "Auto"' "site name derives from the FQDN default"

  rm -rf "$tmp_dir"
}

test_write_config_site_modes_json() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_FRONT_PATH="/static/js/main.0123abcd.js"
  export TSUNAMI_FRONT_SECRET="0123456789abcdef0123456789abcdef"

  export TSUNAMI_SITE_MODE=local
  export TSUNAMI_SITE_UPSTREAM="http://127.0.0.1:3000"
  write_config 2>/dev/null
  assert_file_contains "$CONFIG_FILE" '"mode": "local"' "local site mode written"
  assert_file_contains "$CONFIG_FILE" '"upstream": "http://127.0.0.1:3000"' "local site upstream written"
  assert_file_contains "$CONFIG_FILE" '"preserve_host": true' "local site preserve_host true"
  assert_file_contains "$CONFIG_FILE" '"insecure_skip_verify": false' "local site insecure_skip_verify false"

  export TSUNAMI_SITE_MODE=local
  unset TSUNAMI_SITE_UPSTREAM
  set +e
  ( write_config 2>/dev/null >/dev/null )
  status=$?
  set -e
  assert_eq "1" "$status" "local site mode requires an explicit upstream"

  export TSUNAMI_SITE_MODE=remote
  export TSUNAMI_SITE_UPSTREAM="https://www.wikipedia.org"
  write_config 2>/dev/null
  assert_file_contains "$CONFIG_FILE" '"mode": "remote"' "remote site mode written"
  assert_file_contains "$CONFIG_FILE" '"upstream": "https://www.wikipedia.org"' "remote site upstream written"
  assert_file_contains "$CONFIG_FILE" '"preserve_host": false' "remote site preserve_host false"

  export TSUNAMI_SITE_MODE=builtin
  unset TSUNAMI_SITE_UPSTREAM
  write_config 2>/dev/null
  assert_file_contains "$CONFIG_FILE" '"mode": "builtin"' "builtin site mode written"
  assert_file_contains "$CONFIG_FILE" '"upstream": ""' "builtin site has empty upstream"

  # State round-trip for the new site fields.
  load_saved_settings
  assert_eq "builtin" "$(saved_setting TSUNAMI_SITE_MODE)" "state round-trips site mode"
  assert_eq "" "$(saved_setting TSUNAMI_SITE_UPSTREAM)" "state round-trips empty site upstream"

  rm -rf "$tmp_dir"
}

test_interactive_remote_empty_upstream_falls_back() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  is_interactive() { return 0; }
  confirm() { printf 'CONFIRM:%s\n' "$1"; return 0; }
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"

  # domain Enter, site choice 2 (remote), empty upstream -> builtin fallback.
  output="$(printf '\n2\n\n' | write_config 2>&1)"

  case "$output" in *"falling back to the built-in page"*) ;; *) fail "remote empty upstream warning missing: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 1))
  assert_file_contains "$CONFIG_FILE" '"mode": "builtin"' "empty remote upstream falls back to builtin"
  assert_file_contains "$CONFIG_FILE" '"upstream": ""' "fallback site has empty upstream"

  rm -rf "$tmp_dir"
}

test_advanced_prompts_only_with_flag() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  is_interactive() { return 0; }
  ask() {
    printf 'ASK:%s\n' "$1" >&2
    case "$1" in
      "Fronting path override, empty = generated") printf '/static/js/custom.12345678.js' ;;
      *) printf '%s' "$2" ;;
    esac
  }
  confirm() { printf 'CONFIRM\n'; return 0; }
  export TSUNAMI_ADVANCED=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_SITE_MODE=builtin

  output="$(write_config 2>&1)"

  case "$output" in *"ASK:Listen address"*) ;; *) fail "advanced listen prompt missing: $output" ;; esac
  case "$output" in *"ASK:Fallback backend"*) ;; *) fail "advanced fallback prompt missing: $output" ;; esac
  case "$output" in *"ASK:Surge max connections"*) ;; *) fail "advanced surge prompt missing: $output" ;; esac
  case "$output" in *"ASK:Surge threshold"*) ;; *) fail "advanced surge threshold prompt missing: $output" ;; esac
  case "$output" in *"ASK:Fronting path override"*) ;; *) fail "advanced front-path prompt missing: $output" ;; esac
  case "$output" in *"ASK:Fronting secret override"*) ;; *) fail "advanced front-secret prompt missing: $output" ;; esac
  case "$output" in *"ASK:Decoy site name override"*) ;; *) fail "advanced site-name prompt missing: $output" ;; esac
  case "$output" in *"ASK:Padding scheme JSON override"*) ;; *) fail "advanced padding prompt missing: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 8))
  assert_file_contains "$CONFIG_FILE" '"path": "/static/js/custom.12345678.js"' "advanced custom front path is written"

  rm -rf "$tmp_dir"
}

test_main_advanced_command_enables_flag() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  install_all() { printf '%s\n' "${TSUNAMI_ADVANCED:-unset}"; }

  output="$(main advanced)"
  assert_eq "1" "$output" "advanced command sets TSUNAMI_ADVANCED=1 for the install"

  rm -rf "$tmp_dir"
}

# ── Reconfigure wizard ───────────────────────────────────────────────────

# Writes a first deployment non-interactively. Extra NAME=VALUE arguments
# override or add settings.
seed_deployment() {
  local tmp_dir="$1"
  shift
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"
  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="first-password-123456"
  export TSUNAMI_SITE_MODE=remote
  export TSUNAMI_SITE_UPSTREAM="https://www.wikipedia.org"
  local assignment
  for assignment in "$@"; do
    export "${assignment?}"
  done
  write_config >/dev/null 2>&1
}

# Starts an interactive session with a clean environment, the way a later
# `tsunami-manage config` run starts. PROMPT_LOG collects every question; each
# one is answered by the next line on stdin.
PROMPT_LOG=""
begin_reconfigure() {
  local tmp_dir="$1"
  source_installer "$tmp_dir"
  PROMPT_LOG="$tmp_dir/prompts.log"
  : >"$PROMPT_LOG"
  is_interactive() { return 0; }
  read_prompt() {
    printf '%s\n' "$1" >>"$PROMPT_LOG"
    local answer
    IFS= read -r answer || return 1
    printf '%s' "$answer"
  }
  # Record which menu is shown and which entry is preselected, then run the
  # real menu.
  eval "real_$(declare -f prompt_choice)"
  prompt_choice() {
    printf 'MENU:%s default=%s\n' "$1" "$2" >>"$PROMPT_LOG"
    real_prompt_choice "$@"
  }
}

prompt_count() {
  grep -Fc -- "$1" "$PROMPT_LOG" || true
}

config_value() {
  sed -n "s/^ *\"$1\": *//p" "$CONFIG_FILE" | sed -n '1p' | sed 's/,$//'
}

# A re-run must never be all-or-nothing: every setting is offered with its
# current value, and pressing Enter throughout changes nothing.
test_reconfigure_enter_keeps_every_setting() {
  local tmp_dir path_before secret_before
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir"
  path_before="$(state_value TSUNAMI_FRONT_PATH)"
  secret_before="$(state_value TSUNAMI_FRONT_SECRET)"

  begin_reconfigure "$tmp_dir"
  printf '\n\n\n\n\n\n\n\n' | write_config >/dev/null 2>&1

  assert_eq "0" "$(prompt_count 'Reuse them')" "no all-or-nothing reuse question"
  assert_eq "1" "$(prompt_count 'Public domain')" "the domain is offered"
  assert_file_contains "$PROMPT_LOG" "[edge.tsunami.io]" "the current domain is the default"
  assert_eq "1" "$(prompt_count 'MENU:TLS mode default=2')" "the TLS step is offered with the current mode (manual) preselected"
  assert_file_contains "$PROMPT_LOG" "TLS certificate file (fullchain.pem) [$tmp_dir/certs/fullchain.pem]" "the current certificate is the default"
  assert_file_contains "$PROMPT_LOG" "TLS private key file (privkey.pem) [$tmp_dir/certs/privkey.pem]" "the current key is the default"
  assert_eq "1" "$(prompt_count 'MENU:Site camouflage: what should unauthenticated visitors see? default=2')" "the site step is offered with the current mode (remote) preselected"
  assert_file_contains "$PROMPT_LOG" "[https://www.wikipedia.org]" "the current site upstream is the default"
  assert_eq "1" "$(prompt_count 'Press Enter to install')" "one final confirmation"

  assert_eq "edge.tsunami.io" "$(state_value TSUNAMI_PUBLIC_HOST)" "Enter keeps the domain"
  assert_eq "manual" "$(state_value TSUNAMI_TLS_MODE)" "Enter keeps the TLS mode"
  assert_eq "$tmp_dir/certs/fullchain.pem" "$(state_value TSUNAMI_CERT_FILE)" "Enter keeps the certificate"
  assert_eq "remote" "$(state_value TSUNAMI_SITE_MODE)" "Enter keeps the site mode"
  assert_eq "https://www.wikipedia.org" "$(state_value TSUNAMI_SITE_UPSTREAM)" "Enter keeps the site upstream"
  assert_eq "$path_before" "$(state_value TSUNAMI_FRONT_PATH)" "Enter keeps the fronting path"
  assert_eq "$secret_before" "$(state_value TSUNAMI_FRONT_SECRET)" "Enter keeps the fronting secret"
  assert_eq "first-password-123456" "$(state_value TSUNAMI_PASSWORD)" "Enter keeps the password"
  assert_file_contains "$CLIENT_FILE" "--front-path $path_before" "the client command is unchanged"

  rm -rf "$tmp_dir"
}

test_reconfigure_can_change_the_site_and_keep_the_rest() {
  local tmp_dir path_before
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir"
  path_before="$(state_value TSUNAMI_FRONT_PATH)"

  begin_reconfigure "$tmp_dir"
  # domain, TLS, certificate, key: Enter. Site 1 (local), new upstream, install.
  printf '\n\n\n\n1\n127.0.0.1:9000\n\n\n' | write_config >/dev/null 2>&1

  assert_eq "local" "$(state_value TSUNAMI_SITE_MODE)" "site mode changed"
  assert_eq "http://127.0.0.1:9000" "$(state_value TSUNAMI_SITE_UPSTREAM)" "site upstream changed"
  assert_file_contains "$CONFIG_FILE" '"preserve_host": true' "the Host default follows the new mode"
  assert_eq "edge.tsunami.io" "$(state_value TSUNAMI_PUBLIC_HOST)" "domain kept"
  assert_eq "manual" "$(state_value TSUNAMI_TLS_MODE)" "TLS mode kept"
  assert_eq "$path_before" "$(state_value TSUNAMI_FRONT_PATH)" "fronting path kept"

  rm -rf "$tmp_dir"
}

test_reconfigure_can_change_the_domain_and_tls_mode() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir"

  begin_reconfigure "$tmp_dir"
  obtain_letsencrypt_cert() { printf 'certbot:%s\n' "$1" >>"$PROMPT_LOG"; }
  write_certbot_hooks() { :; }
  # new domain, TLS 1 (Let's Encrypt), site Enter, upstream Enter, install Enter.
  printf 'new.tsunami.io\n1\n\n\n\n\n' | write_config >/dev/null 2>&1

  assert_eq "new.tsunami.io" "$(state_value TSUNAMI_PUBLIC_HOST)" "domain changed"
  assert_eq "new.tsunami.io" "$(state_value TSUNAMI_SNI)" "SNI follows the new domain"
  assert_eq "letsencrypt" "$(state_value TSUNAMI_TLS_MODE)" "TLS mode changed"
  assert_file_contains "$PROMPT_LOG" "certbot:new.tsunami.io" "the certificate is requested for the new domain"
  assert_eq "/etc/letsencrypt/live/new.tsunami.io/fullchain.pem" "$(state_value TSUNAMI_CERT_FILE)" "certificate path follows the new domain"
  assert_file_contains "$CONFIG_FILE" '"site_name": "New"' "the derived site name follows the new domain"
  assert_eq "remote" "$(state_value TSUNAMI_SITE_MODE)" "site mode kept"
  assert_file_contains "$CLIENT_FILE" "--sni new.tsunami.io" "the client command uses the new domain"

  rm -rf "$tmp_dir"
}

# A site name the operator chose must survive a domain change; a derived one
# must not pin the old domain's name.
test_reconfigure_keeps_a_custom_site_name_across_a_domain_change() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir" TSUNAMI_SITE_NAME="My Shop"

  begin_reconfigure "$tmp_dir"
  printf 'new.tsunami.io\n\n\n\n\n\n' | write_config >/dev/null 2>&1

  assert_eq "new.tsunami.io" "$(state_value TSUNAMI_PUBLIC_HOST)" "domain changed"
  assert_file_contains "$CONFIG_FILE" '"site_name": "My Shop"' "the custom site name is kept"

  rm -rf "$tmp_dir"
}

# The management menu sources the state file into the running shell when it
# shows the client information. Those values are saved settings, not operator
# overrides, so the wizard must still ask.
test_reconfigure_asks_after_the_menu_showed_client_info() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir"

  begin_reconfigure "$tmp_dir"
  print_connection_info >/dev/null 2>&1
  # domain, TLS, certificate, key: Enter. Site 3 (built-in), install.
  printf '\n\n\n\n3\n\n\n' | write_config >/dev/null 2>&1

  assert_eq "1" "$(prompt_count 'MENU:TLS mode')" "TLS is still offered after client info was shown"
  assert_eq "1" "$(prompt_count 'MENU:Site camouflage')" "the site is still offered after client info was shown"
  assert_eq "builtin" "$(state_value TSUNAMI_SITE_MODE)" "the site can still be changed from the menu"

  rm -rf "$tmp_dir"
}

# Settings fixed on the command line are honoured without a question.
test_reconfigure_env_override_wins_without_asking() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir"

  begin_reconfigure "$tmp_dir"
  export TSUNAMI_SITE_MODE=builtin
  printf '\n\n\n\n\n\n' | write_config >/dev/null 2>&1

  assert_eq "0" "$(prompt_count 'MENU:Site camouflage')" "an explicit TSUNAMI_SITE_MODE skips the site question"
  assert_eq "0" "$(prompt_count 'site upstream')" "no upstream question for an explicit builtin site"
  assert_eq "builtin" "$(state_value TSUNAMI_SITE_MODE)" "the environment value wins over the saved one"
  assert_eq "1" "$(prompt_count 'MENU:TLS mode default=2')" "TLS is still offered"

  rm -rf "$tmp_dir"
}

# Fallback, Surge and the padding scheme used to be dropped by any re-run
# because they were never written to the state file.
test_reconfigure_preserves_fallback_surge_and_padding() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir" TSUNAMI_FALLBACK=127.0.0.1:8080 TSUNAMI_MAX_CONNECTIONS=6 \
    TSUNAMI_THRESHOLD=12 TSUNAMI_LISTEN=:8443 "TSUNAMI_PADDING_SCHEME_JSON=stop=2
c0=~80-200"

  assert_eq '"127.0.0.1:8080"' "$(config_value fallback)" "seed wrote the fallback"

  begin_reconfigure "$tmp_dir"
  printf '\n\n\n\n\n\n' | write_config >/dev/null 2>&1

  assert_eq '":8443"' "$(config_value listen)" "the listen address survives a re-run"
  assert_eq '"127.0.0.1:8080"' "$(config_value fallback)" "the fallback survives a re-run"
  assert_eq "6" "$(config_value max_connections)" "Surge max connections survive a re-run"
  assert_eq "12" "$(config_value threshold)" "the Surge threshold survives a re-run"
  assert_file_contains "$CONFIG_FILE" 'stop=2\nc0=~80-200' "the padding scheme survives a re-run"

  # And a second re-run still has them: they are stored, not just carried once.
  begin_reconfigure "$tmp_dir"
  printf '\n\n\n\n\n\n' | write_config >/dev/null 2>&1
  assert_eq "6" "$(config_value max_connections)" "Surge max connections survive two re-runs"
  assert_file_contains "$CONFIG_FILE" 'stop=2\nc0=~80-200' "the padding scheme survives two re-runs"

  rm -rf "$tmp_dir"
}

# json_unescape must reverse json_escape exactly: a recovered padding scheme is
# written back to config.json, so any drift would corrupt it a little more on
# every re-run.
test_json_unescape_reverses_json_escape() {
  local tmp_dir value backslash
  backslash="$(printf '\134')"
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  for value in 'plain' $'two\nlines' 'quote " inside' 'back\slash' \
    'backslash-n \n that is not a newline' $'mixed \\" and \\\\ and\nnewline' \
    "trailing backslash$backslash" 'percent %s and %b' ''; do
    assert_eq "$value" "$(json_unescape "$(json_escape "$value")")" "round trip of <$value>"
  done

  rm -rf "$tmp_dir"
}

# Deployments written before those settings were stored only have them in
# config.json; the first re-run must pick them up from there.
test_reconfigure_recovers_settings_from_a_legacy_state_file() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir" TSUNAMI_FALLBACK=127.0.0.1:8080 TSUNAMI_MAX_CONNECTIONS=6 TSUNAMI_THRESHOLD=12 \
    "TSUNAMI_PADDING_SCHEME_JSON=stop=2
c0=~80-200"
  # Drop the entries an older installer never wrote. The padding value is one
  # line in the state file because printf %q quotes the newline as $'...'.
  sed -i -e '/^TSUNAMI_FALLBACK=/d' -e '/^TSUNAMI_MAX_CONNECTIONS=/d' -e '/^TSUNAMI_THRESHOLD=/d' \
    -e '/^TSUNAMI_PADDING_SCHEME_JSON=/d' "$STATE_FILE"
  assert_file_not_contains "$STATE_FILE" "80-200" "the legacy state file has no padding entry"

  begin_reconfigure "$tmp_dir"
  printf '\n\n\n\n\n\n' | write_config >/dev/null 2>&1

  assert_eq '"127.0.0.1:8080"' "$(config_value fallback)" "fallback recovered from config.json"
  assert_eq "6" "$(config_value max_connections)" "Surge max connections recovered from config.json"
  assert_eq "12" "$(config_value threshold)" "Surge threshold recovered from config.json"
  assert_eq "6" "$(state_value TSUNAMI_MAX_CONNECTIONS)" "the recovered value is now stored in the state file"
  assert_file_contains "$CONFIG_FILE" 'stop=2\nc0=~80-200' "the padding scheme is recovered from config.json"

  # A second re-run reads the now-complete state file and changes nothing.
  begin_reconfigure "$tmp_dir"
  printf '\n\n\n\n\n\n' | write_config >/dev/null 2>&1
  assert_file_contains "$CONFIG_FILE" 'stop=2\nc0=~80-200' "the recovered padding scheme is stable across re-runs"
  assert_eq "1" "$(grep -c '"padding_scheme"' "$CONFIG_FILE")" "exactly one padding_scheme entry"

  rm -rf "$tmp_dir"
}

# With no usable upstream and no more input, the local-site prompt used to
# repeat forever. It must give up with an error.
test_local_site_upstream_prompt_gives_up() {
  local tmp_dir status pid waited=0
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir" TSUNAMI_SITE_MODE=builtin TSUNAMI_SITE_UPSTREAM=

  begin_reconfigure "$tmp_dir"
  set +e
  # domain, TLS, certificate, key: Enter. Site 1 (local), then the input ends.
  ( printf '\n\n\n\n1\n' | write_config >/dev/null 2>"$tmp_dir/err.txt" ) &
  pid=$!
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt 100 ]; do
    sleep 0.1
    waited=$((waited + 1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
    set -e
    fail "the local site upstream prompt did not give up within 10 seconds"
  fi
  wait "$pid"
  status=$?
  set -e

  assert_ne "0" "$status" "an unanswered upstream prompt is an error"
  assert_file_contains "$tmp_dir/err.txt" "too many" "the error explains why it gave up"
  assert_eq "builtin" "$(state_value TSUNAMI_SITE_MODE)" "the saved configuration is untouched"

  rm -rf "$tmp_dir"
}

# An answer that is not a valid domain must not turn a real deployment into the
# self-signed test mode: the next attempt still offers the current domain, and
# clearing the domain on an existing deployment has to be confirmed.
test_reconfigure_invalid_domain_answer_keeps_the_current_default() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir"

  begin_reconfigure "$tmp_dir"
  # A typo ("n"), then Enter at every question.
  printf 'n\n\n\n\n\n\n\n\n\n' | write_config >/dev/null 2>&1

  assert_eq "2" "$(prompt_count 'Public domain for this server')" "the domain is asked again after an invalid answer"
  assert_eq "2" "$(prompt_count '[edge.tsunami.io]')" "the retry still offers the current domain"
  assert_eq "edge.tsunami.io" "$(state_value TSUNAMI_PUBLIC_HOST)" "the domain is kept"
  assert_eq "manual" "$(state_value TSUNAMI_TLS_MODE)" "the TLS mode is kept"
  assert_eq "0" "$(prompt_count 'self-signed certificate?')" "the wizard never drifts into the self-signed flow"

  rm -rf "$tmp_dir"
}

# The first-install wizard still treats an empty domain as the self-signed test
# deployment; that shortcut is unchanged.
test_first_install_empty_domain_still_selects_self_signed() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  begin_reconfigure "$tmp_dir"
  default_public_domain() { printf ''; }
  # empty domain, yes to the self-signed warning, site 3 (built-in), install.
  printf '\ny\n3\n\n\n' | write_config >/dev/null 2>&1

  assert_eq "self-signed" "$(state_value TSUNAMI_TLS_MODE)" "an empty domain on a first install selects self-signed"
  assert_eq "dev.tsunami.lan" "$(state_value TSUNAMI_PUBLIC_HOST)" "the placeholder test host is used"
  assert_eq "1" "$(prompt_count 'self-signed certificate?')" "the self-signed warning is confirmed once"

  rm -rf "$tmp_dir"
}

test_advanced_reconfigure_defaults_to_current_values() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  seed_deployment "$tmp_dir" TSUNAMI_FALLBACK=127.0.0.1:8080 TSUNAMI_MAX_CONNECTIONS=6 TSUNAMI_THRESHOLD=12

  begin_reconfigure "$tmp_dir"
  export TSUNAMI_ADVANCED=1
  printf '\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n' | write_config >/dev/null 2>&1

  assert_file_contains "$PROMPT_LOG" "Fallback backend, empty to use built-in page [127.0.0.1:8080]" "advanced fallback default is the current value"
  assert_file_contains "$PROMPT_LOG" "Surge max connections [6]" "advanced Surge default is the current value"
  assert_file_contains "$PROMPT_LOG" "Surge threshold [12]" "advanced threshold default is the current value"
  assert_eq "6" "$(config_value max_connections)" "advanced Enter keeps Surge max connections"
  assert_eq "remote" "$(state_value TSUNAMI_SITE_MODE)" "advanced Enter keeps the site mode"

  rm -rf "$tmp_dir"
}

test_security_check_site_mode_branches() {
  local tmp_dir output status
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_FRONT_PATH="/static/js/main.0123abcd.js"

  export TSUNAMI_SITE_MODE=remote
  export TSUNAMI_SITE_UPSTREAM="https://www.wikipedia.org"
  write_config 2>/dev/null
  set +e
  output="$(security_check 2>&1)"
  status=$?
  set -e
  assert_eq "0" "$status" "remote site security check passes"
  case "$output" in *"Site camouflage: remote upstream https://www.wikipedia.org"*) ;; *) fail "remote site PASS missing: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 1))

  export TSUNAMI_SITE_MODE=local
  export TSUNAMI_SITE_UPSTREAM="http://127.0.0.1:3000"
  write_config 2>/dev/null
  set +e
  output="$(security_check 2>&1)"
  status=$?
  set -e
  assert_eq "0" "$status" "local site security check passes"
  case "$output" in *"Site camouflage: local upstream http://127.0.0.1:3000"*) ;; *) fail "local site PASS missing: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 1))

  export TSUNAMI_SITE_MODE=builtin
  unset TSUNAMI_SITE_UPSTREAM
  write_config 2>/dev/null
  set +e
  output="$(security_check 2>&1)"
  status=$?
  set -e
  assert_eq "0" "$status" "builtin site security check only warns"
  case "$output" in *"easy to cluster"*) ;; *) fail "builtin site warning missing: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 1))

  # Invalid stored config: local without an upstream must fail.
  sed -i 's/^TSUNAMI_SITE_MODE=.*/TSUNAMI_SITE_MODE=local/' "$STATE_FILE"
  sed -i 's/^TSUNAMI_SITE_UPSTREAM=.*/TSUNAMI_SITE_UPSTREAM=/' "$STATE_FILE"
  set +e
  output="$(security_check 2>&1)"
  status=$?
  set -e
  assert_eq "1" "$status" "local site without upstream fails the check"
  case "$output" in *"local mode has no upstream"*) ;; *) fail "local missing upstream failure missing: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 1))

  sed -i 's/^TSUNAMI_SITE_MODE=.*/TSUNAMI_SITE_MODE=weird/' "$STATE_FILE"
  set +e
  output="$(security_check 2>&1)"
  status=$?
  set -e
  assert_eq "1" "$status" "invalid site mode fails the check"
  case "$output" in *"invalid mode 'weird'"*) ;; *) fail "invalid site mode failure missing: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 1))

  rm -rf "$tmp_dir"
}

test_default_dispatch_context
test_main_explicit_command_bypasses_menu
test_main_default_menu_path
test_main_default_pipe_path
test_main_piped_with_prompt_tty_opens_menu
test_prompt_choice_defaults_and_retries
test_ask_reads_input_when_prompt_tty_exists
test_confirm_defaults_and_assume_yes
test_load_saved_settings_reads_previous_values
test_load_saved_settings_leaves_the_environment_alone
test_config_summary_masks_password
test_menu_first_entry_is_quick_install
test_menu_action_dispatches_existing_commands
test_interactive_wrappers_do_not_double_confirm
test_write_config_cancel_skips_config_file
test_require_domain_accepts_valid_hosts
test_require_domain_rejects_invalid_values
test_front_path_generation_and_override
test_random_hex_and_site_name_helpers
test_write_config_manual_json_and_client_hint
test_write_config_requires_tls_decision
test_write_config_self_signed_requires_dev
test_write_config_self_signed_dev_skip_verify
test_write_config_fronting_disabled
test_write_config_padding_override_and_default_omitted
test_write_config_rejects_placeholder_host
test_interactive_tls_prompt_defaults_to_letsencrypt
test_interactive_self_signed_requires_confirmation
test_security_check_missing_and_unsafe
test_security_check_generated_config_ok
test_security_check_permission_branch
test_print_connection_info_handles_long_values
test_quick_wizard_single_confirm
test_quick_wizard_uses_fqdn_default
test_write_config_site_modes_json
test_interactive_remote_empty_upstream_falls_back
test_advanced_prompts_only_with_flag
test_main_advanced_command_enables_flag
test_reconfigure_enter_keeps_every_setting
test_reconfigure_can_change_the_site_and_keep_the_rest
test_reconfigure_can_change_the_domain_and_tls_mode
test_reconfigure_keeps_a_custom_site_name_across_a_domain_change
test_reconfigure_asks_after_the_menu_showed_client_info
test_reconfigure_env_override_wins_without_asking
test_reconfigure_preserves_fallback_surge_and_padding
test_json_unescape_reverses_json_escape
test_reconfigure_recovers_settings_from_a_legacy_state_file
test_local_site_upstream_prompt_gives_up
test_reconfigure_invalid_domain_answer_keeps_the_current_default
test_first_install_empty_domain_still_selects_self_signed
test_advanced_reconfigure_defaults_to_current_values
test_security_check_site_mode_branches

printf 'ok - %s install script tests\n' "$TESTS_RUN"
