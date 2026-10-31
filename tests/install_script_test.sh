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

test_load_state_defaults_reuses_previous_values() {
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
STATE
  source_installer "$tmp_dir"

  load_state_defaults

  assert_eq ":8443" "$TSUNAMI_LISTEN" "state default listen"
  assert_eq "secret-value" "$TSUNAMI_PASSWORD" "state default password"
  assert_eq "alice" "$TSUNAMI_USER" "state default user"
  assert_eq "edge.tsunami.io" "$TSUNAMI_PUBLIC_HOST" "state default public host"
  assert_eq "letsencrypt" "$TSUNAMI_TLS_MODE" "state default TLS mode"
  assert_eq "0" "$TSUNAMI_DEV" "state default dev flag"
  assert_eq "true" "$TSUNAMI_FRONTING" "state default fronting flag"
  assert_eq "/static/js/main.deadbeef.js" "$TSUNAMI_FRONT_PATH" "state default front path"
  assert_eq "front-secret-value" "$TSUNAMI_FRONT_SECRET" "state default front secret"
  assert_eq "Edge" "$TSUNAMI_SITE_NAME" "state default site name"
  assert_eq "" "$TSUNAMI_DECOY_PROXY" "state default decoy proxy"
  assert_eq "edge.tsunami.io" "$TSUNAMI_SNI" "state default SNI"
  assert_eq "local" "$TSUNAMI_SITE_MODE" "state default site mode"
  assert_eq "http://127.0.0.1:3000" "$TSUNAMI_SITE_UPSTREAM" "state default site upstream"
  assert_eq "true" "$TSUNAMI_SITE_PRESERVE_HOST" "state default site preserve_host"
  assert_eq "false" "$TSUNAMI_SITE_INSECURE_SKIP_VERIFY" "state default site insecure_skip_verify"

  rm -rf "$tmp_dir"
}

test_load_state_defaults_preserves_env_values() {
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
  TSUNAMI_FRONT_PATH=/env/path.22222222.js
  TSUNAMI_FRONT_SECRET=env-secret
  TSUNAMI_SITE_MODE=remote
  TSUNAMI_SITE_UPSTREAM=https://www.wikipedia.org
  load_state_defaults

  assert_eq "env.tsunami.io" "$TSUNAMI_PUBLIC_HOST" "environment public host wins over state"
  assert_eq "/env/path.22222222.js" "$TSUNAMI_FRONT_PATH" "environment front path wins over state"
  assert_eq "env-secret" "$TSUNAMI_FRONT_SECRET" "environment front secret wins over state"
  assert_eq "remote" "$TSUNAMI_SITE_MODE" "environment site mode wins over state"
  assert_eq "https://www.wikipedia.org" "$TSUNAMI_SITE_UPSTREAM" "environment site upstream wins over state"

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
  detect_local_site() { printf 'http://127.0.0.1:3000'; }
  # ask prompts are not visible when stdin is a pipe; mark them explicitly.
  ask() { printf 'ASK:%s\n' "$1" >&2; printf '%s' "$2"; }
  confirm() { printf 'CONFIRM:%s\n' "$1"; return 0; }

  export TSUNAMI_PUBLIC_HOST="cdn.tsunami.dev"
  export TSUNAMI_PASSWORD="test-password-123456"

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
  assert_file_contains "$CONFIG_FILE" '"upstream": "http://127.0.0.1:3000"' "quick wizard uses the detected local upstream"
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
  write_config 2>/dev/null
  assert_file_contains "$CONFIG_FILE" '"upstream": "http://127.0.0.1:8080"' "local site defaults to 127.0.0.1:8080"

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
  unset TSUNAMI_SITE_MODE TSUNAMI_SITE_UPSTREAM
  load_state_defaults
  assert_eq "builtin" "$TSUNAMI_SITE_MODE" "state round-trips site mode"
  assert_eq "" "$TSUNAMI_SITE_UPSTREAM" "state round-trips empty site upstream"

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

test_reuse_existing_settings_skips_prompts() {
  local tmp_dir output path_before secret_before
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"
  make_cert_files "$tmp_dir/certs"

  export TSUNAMI_ASSUME_YES=1
  export TSUNAMI_TLS_MODE=manual
  export TSUNAMI_CERT_FILE="$tmp_dir/certs/fullchain.pem"
  export TSUNAMI_KEY_FILE="$tmp_dir/certs/privkey.pem"
  export TSUNAMI_PUBLIC_HOST="edge.tsunami.io"
  export TSUNAMI_PASSWORD="test-password-123456"
  export TSUNAMI_SITE_MODE=local
  export TSUNAMI_SITE_UPSTREAM="http://127.0.0.1:3000"
  write_config 2>/dev/null
  path_before="$(state_value TSUNAMI_FRONT_PATH)"
  secret_before="$(state_value TSUNAMI_FRONT_SECRET)"

  # Re-run interactively: accept reuse, then the single install confirmation.
  unset TSUNAMI_ASSUME_YES
  is_interactive() { return 0; }
  confirm() { printf 'CONFIRM:%s\n' "$1"; return 0; }
  ask() { printf 'UNEXPECTED-ASK\n'; printf '%s' "$2"; }
  prompt_choice() { printf 'UNEXPECTED-PROMPT-CHOICE\n'; printf '%s' "$1"; }

  output="$(write_config 2>&1)"

  assert_eq "2" "$(text_count "$output" 'CONFIRM:')" "reuse path asks exactly reuse + install confirmation"
  case "$output" in *"CONFIRM:Existing TSUNAMI settings found. Reuse them?"*) ;; *) fail "reuse prompt missing: $output" ;; esac
  case "$output" in *"CONFIRM:Press Enter to install"*) ;; *) fail "install confirmation missing on reuse: $output" ;; esac
  case "$output" in *"UNEXPECTED"*) fail "reuse path prompted for settings: $output" ;; esac
  TESTS_RUN=$((TESTS_RUN + 4))

  assert_eq "$path_before" "$(state_value TSUNAMI_FRONT_PATH)" "reuse keeps the generated front path"
  assert_eq "$secret_before" "$(state_value TSUNAMI_FRONT_SECRET)" "reuse keeps the generated front secret"
  assert_eq "local" "$(state_value TSUNAMI_SITE_MODE)" "reuse keeps the site mode"

  rm -rf "$tmp_dir"
}

test_local_site_detection_helper() {
  local tmp_dir listing
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  listing="State Recv-Q Send-Q Local Address:Port Peer Address:Port
LISTEN 0 4096 127.0.0.1:8080 0.0.0.0:*"
  assert_eq "http://127.0.0.1:8080" "$(local_site_upstream_from_listing "$listing")" "detection finds 127.0.0.1:8080"

  listing="LISTEN 0 511 *:3000 *:*"
  assert_eq "http://127.0.0.1:3000" "$(local_site_upstream_from_listing "$listing")" "detection finds wildcard :3000"

  listing="LISTEN 0 4096 0.0.0.0:80 0.0.0.0:*"
  assert_eq "http://127.0.0.1:80" "$(local_site_upstream_from_listing "$listing")" "detection finds port 80"

  assert_failure "detection ignores unlisted ports" local_site_upstream_from_listing "LISTEN 0 4096 127.0.0.1:8081 0.0.0.0:*"
  assert_failure "detection ignores loopback SSH" local_site_upstream_from_listing "LISTEN 0 4096 127.0.0.1:22 0.0.0.0:*"

  # With neither ss nor lsof the detector returns an empty (successful) result.
  has_cmd() { case "$1" in ss | lsof) return 1 ;; *) command -v "$1" >/dev/null 2>&1 ;; esac; }
  assert_eq "" "$(detect_local_site)" "detection returns empty without ss/lsof"

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
test_load_state_defaults_reuses_previous_values
test_load_state_defaults_preserves_env_values
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
test_reuse_existing_settings_skips_prompts
test_local_site_detection_helper
test_security_check_site_mode_branches

printf 'ok - %s install script tests\n' "$TESTS_RUN"
