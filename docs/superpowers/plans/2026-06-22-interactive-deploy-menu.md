# Interactive Deployment Menu Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a full interactive management menu to `scripts/install.sh` while preserving command-mode and pipe-install compatibility.

**Architecture:** Keep the deployment script as the single operational entrypoint. Add small Bash helpers for dispatch, choices, confirmations, state defaults, and summary output, then call the existing install/config/update/status/client/cert/uninstall functions from menu wrappers.

**Tech Stack:** Bash installer, Git Bash/MSYS Bash for local validation, existing Go project tests for regression coverage.

---

## File Structure

- Modify: `scripts/install.sh`
  - Add a test-source guard so shell tests can source functions without executing `main`.
  - Add interactive helper functions near the existing `ask` helper.
  - Add menu wrappers near the existing command functions.
  - Update `main` to dispatch no-argument TTY runs to the menu and non-TTY runs to install.
- Create: `tests/install_script_test.sh`
  - Bash tests for dispatch helpers, prompt helpers, state defaults, and menu action dispatch.
- Modify: `docs/deployment.md`
  - Document that local no-argument runs open the interactive menu.
- Modify: `docs/deployment.zh.md`
  - Mirror the English deployment guide behavior update.
- Modify: `docs/deployment.ja.md`
  - Mirror the English deployment guide behavior update.

---

### Task 1: Add Failing Dispatch Tests

**Files:**
- Create: `tests/install_script_test.sh`
- Modify later: `scripts/install.sh`

- [ ] **Step 1: Write the failing tests**

Create `tests/install_script_test.sh` with this executable Bash test harness:

```bash
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
  TSUNAMI_TEST_SOURCE=1 \
  TSUNAMI_CONFIG_DIR="$tmp_dir/etc/tsunami" \
  TSUNAMI_SERVICE_NAME="tsunami-test" \
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

test_default_dispatch_context
test_main_explicit_command_bypasses_menu
test_main_default_menu_path
test_main_default_pipe_path

printf 'ok - %s install script tests\n' "$TESTS_RUN"
```

- [ ] **Step 2: Run tests and verify RED**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: FAIL because `scripts/install.sh` runs `main` while being sourced or because `default_command_for_context` is not defined.

- [ ] **Step 3: Implement the minimal dispatch support**

In `scripts/install.sh`, add:

```bash
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
```

Change `main` so zero-argument runs select `menu` or `install`, while explicit arguments keep the existing command behavior:

```bash
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
```

Guard the final entrypoint:

```bash
if [ "${TSUNAMI_TEST_SOURCE:-0}" != "1" ]; then
  main "$@"
fi
```

Add a temporary `interactive_menu` stub returning usage or replace it with the final implementation from Task 4 if doing both in one pass.

- [ ] **Step 4: Run tests and verify GREEN for dispatch**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: PASS for the dispatch tests once `interactive_menu` is callable.

---

### Task 2: Add Prompt and Confirmation Tests

**Files:**
- Modify: `tests/install_script_test.sh`
- Modify later: `scripts/install.sh`

- [ ] **Step 1: Extend tests for choices and confirmations**

Append these tests before the final test calls:

```bash
test_prompt_choice_defaults_and_retries() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  output="$(printf '\n' | prompt_choice "Pick one" "2" "First" "Second")"
  assert_eq "2" "$output" "prompt_choice returns default on empty input"

  output="$(printf 'x\n3\n' | prompt_choice "Pick one" "" "First" "Second" "Third" 2>/dev/null)"
  assert_eq "3" "$output" "prompt_choice retries invalid input"

  rm -rf "$tmp_dir"
}

test_confirm_defaults_and_assume_yes() {
  local tmp_dir
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  assert_success "confirm default yes" bash -c 'printf "\n" | confirm "Proceed?" "y"' 
  assert_failure "confirm explicit no" bash -c 'printf "n\n" | confirm "Proceed?" "y"'
  TSUNAMI_ASSUME_YES=1
  assert_success "confirm assume yes" confirm "Proceed?" "n"
  unset TSUNAMI_ASSUME_YES

  rm -rf "$tmp_dir"
}
```

Call both tests before printing the final success line:

```bash
test_prompt_choice_defaults_and_retries
test_confirm_defaults_and_assume_yes
```

- [ ] **Step 2: Run tests and verify RED**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: FAIL because `prompt_choice` and `confirm` are not implemented.

- [ ] **Step 3: Implement prompt helpers**

Add these helper functions near `ask`:

```bash
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
  local answer i

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
    read -r -p "Choice${default:+ [$default]}: " answer
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
  if ! [ -t 0 ]; then
    normalize_yes "$default"
    return
  fi

  if normalize_yes "$default"; then
    suffix="Y/n"
  else
    suffix="y/N"
  fi

  while true; do
    read -r -p "$prompt [$suffix]: " answer
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
```

- [ ] **Step 4: Run tests and verify GREEN**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: PASS for dispatch, choice, and confirmation behavior.

---

### Task 3: Add State Defaults and Config Summary

**Files:**
- Modify: `tests/install_script_test.sh`
- Modify later: `scripts/install.sh`

- [ ] **Step 1: Add failing tests for state loading and summary masking**

Append:

```bash
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
  assert_eq "letsencrypt" "$TSUNAMI_PREVIOUS_TLS_MODE" "state default previous TLS mode"

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
  case "$output" in
    *"example.com"*":443"*"letsencrypt"*) ;;
    *) fail "summary missed expected fields: $output" ;;
  esac

  rm -rf "$tmp_dir"
}
```

Call both tests before the final success line.

- [ ] **Step 2: Run tests and verify RED**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: FAIL because `load_state_defaults` and `show_config_summary` are not implemented.

- [ ] **Step 3: Implement state and summary helpers**

Add:

```bash
load_state_defaults() {
  if [ -f "$STATE_FILE" ]; then
    . "$STATE_FILE" 2>/dev/null || true
    TSUNAMI_PREVIOUS_TLS_MODE="${TSUNAMI_TLS_MODE:-}"
  fi
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
```

Call `load_state_defaults` at the start of interactive install/config wrappers before collecting values.

- [ ] **Step 4: Run tests and verify GREEN**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: PASS through state and summary tests.

---

### Task 4: Add Interactive Menu and Action Wrappers

**Files:**
- Modify: `tests/install_script_test.sh`
- Modify later: `scripts/install.sh`

- [ ] **Step 1: Add failing menu dispatch test**

Append:

```bash
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
  output="$(run_menu_action 8)"
  assert_eq "uninstall" "$output" "menu uninstall dispatch"
  output="$(run_menu_action 9)"
  assert_eq "exit" "$output" "menu exit dispatch"

  rm -rf "$tmp_dir"
}
```

Call the test before the final success line.

- [ ] **Step 2: Run tests and verify RED**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: FAIL because `run_menu_action` is not implemented.

- [ ] **Step 3: Implement menu wrappers**

Add:

```bash
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
  while true; do
    local choice
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
```

Add initial interactive wrappers:

```bash
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
```

- [ ] **Step 4: Run tests and verify GREEN**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: PASS through menu dispatch tests.

---

### Task 5: Integrate Guided Config Confirmation

**Files:**
- Modify: `scripts/install.sh`
- Modify: `tests/install_script_test.sh`

- [ ] **Step 1: Add tests for interactive config confirmation**

Add a test that stubs the write path and confirms the wrapper does not write when declined:

```bash
test_interactive_configure_cancel_skips_write() {
  local tmp_dir output
  tmp_dir="$(mktemp -d)"
  source_installer "$tmp_dir"

  configure_all() { printf 'configured\n'; }

  output="$(printf 'n\n' | interactive_configure_all)"
  assert_eq "" "$output" "cancelled config produces no write"

  rm -rf "$tmp_dir"
}
```

Call it before the final success line.

- [ ] **Step 2: Run tests and verify RED or behavior gap**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
```

Expected: FAIL until `confirm` can read piped input for tests and the wrapper returns without calling `configure_all`.

- [ ] **Step 3: Refine interactive wrappers and config flow**

Update `write_config` to call `load_state_defaults` only for interactive runs and use the existing `ask` defaults:

```bash
if is_interactive; then
  load_state_defaults
fi
```

Before writing the JSON config, call:

```bash
if is_interactive; then
  show_config_summary "$listen" "$domain" "$tls_mode" "$fallback" "$max_conn" "$threshold" "$password"
  confirm "Write this configuration?" "y" || die "configuration cancelled"
fi
```

When Let's Encrypt is selected interactively, print:

```bash
log "Let's Encrypt requires DNS to point at this server and port 80 to be reachable."
```

- [ ] **Step 4: Run tests and syntax checks**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
"C:/Program Files/Git/bin/bash.exe" -n scripts/install.sh
```

Expected: PASS.

---

### Task 6: Update Documentation and Run Final Verification

**Files:**
- Modify: `docs/deployment.md`
- Modify: `docs/deployment.zh.md`
- Modify: `docs/deployment.ja.md`

- [ ] **Step 1: Document the menu behavior**

In each deployment guide, update the local/management section to state:

```markdown
Running the management script without a command in an interactive terminal opens
the menu. Explicit commands such as `status`, `client`, and `update` continue to
work for automation.
```

Use equivalent Chinese and Japanese wording in localized files.

- [ ] **Step 2: Run script tests**

Run:

```bash
"C:/Program Files/Git/bin/bash.exe" tests/install_script_test.sh
"C:/Program Files/Git/bin/bash.exe" -n scripts/install.sh
```

Expected: PASS.

- [ ] **Step 3: Run Go regression tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 4: Inspect diff**

Run:

```bash
git diff --check
git status --short
```

Expected: no whitespace errors; only intentional files changed.

---

## Self-Review

- Spec coverage: the plan covers TTY/no-TTY entry behavior, menu actions, environment-variable compatibility, state default loading, config summary masking, confirmations, TLS warnings, docs, and verification commands.
- Placeholder scan: no open-ended placeholder tasks remain; every behavior has a file, test, and concrete implementation sketch.
- Scope check: this is a single-script feature with documentation updates, not multiple independent subsystems.
