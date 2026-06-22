# Interactive Deployment Menu Design

## Goal

Improve `scripts/install.sh` into a full interactive management script while keeping the current command-line and non-interactive deployment behavior stable.

The script should be convenient for an operator running it manually, but still safe for automation, remote pipe installs, and environment-variable driven deployments.

## Scope

This design covers the deployment script only:

- Add an interactive management menu.
- Improve the install/config prompts as a guided flow.
- Reuse existing configuration defaults when reconfiguring.
- Add explicit confirmations for write, restart, and uninstall paths.
- Preserve existing commands and environment variable behavior.

It does not change the Go server/client binaries, protocol behavior, release packaging, or systemd service semantics beyond what the installer already manages.

## Entry Behavior

`scripts/install.sh` will choose behavior using arguments and TTY detection:

- If a command argument is provided, run that command exactly as today.
- If no command is provided and stdin is a TTY, show the interactive menu.
- If no command is provided and stdin is not a TTY, run the existing default install path.

This preserves `curl | sudo bash` and `wget | sudo bash` installs, while making `sudo bash scripts/install.sh` and `sudo tsunami-manage` open the management menu for humans.

## Menu

The menu will offer these actions:

1. Install or reinstall the service.
2. Reconfigure and restart.
3. Update binary and restart.
4. Show service status.
5. Show client connection information.
6. Follow service logs.
7. Show Let's Encrypt certificate status.
8. Uninstall.
9. Exit.

The menu should use numeric choices, show a clear default where useful, and handle invalid input with a concise retry prompt.

Long-running commands such as log following should hand control directly to the underlying command instead of wrapping it in a loop.

## Configuration Flow

The configuration flow will keep environment variables as the highest priority. Interactive prompts only fill values that are not already provided through `TSUNAMI_*` variables.

When `/etc/tsunami/install.env` exists, reconfiguration should load it and use those values as prompt defaults. This makes `config` useful for small changes instead of forcing the operator to re-enter known values.

The guided configuration flow will ask for:

- Listen address, defaulting to the previous value or `:443`.
- Public host/domain.
- TLS mode, derived from existing cert/key variables or Let's Encrypt selection.
- Optional fallback backend.
- Surge max connections.
- Surge threshold.

Before writing `/etc/tsunami/config.json`, the script will show a summary with listen address, public host, TLS mode, fallback, and Surge settings. It should not print the full raw password in the summary; the existing final client panel can continue to mask it and write the raw command to the protected client command file.

## TLS Handling

The TLS branches remain compatible with the current script:

- Manual certificate mode requires both `TSUNAMI_CERT_FILE` and `TSUNAMI_KEY_FILE`.
- Let's Encrypt mode obtains a certificate through certbot and writes renewal hooks.
- Self-signed mode remains the fallback when no certificate and no Let's Encrypt domain are used.

When the operator selects Let's Encrypt interactively, the script should briefly warn that DNS must already point to the server and port 80 must be reachable.

## Confirmations

The script should require confirmation before:

- Writing configuration in an interactive flow.
- Restarting the service after reconfiguration.
- Installing or reinstalling from the menu.
- Updating the binary from the menu.
- Uninstalling.
- Removing `/etc/tsunami`.

Uninstall should keep configuration by default. Removing `/etc/tsunami` should require an explicit affirmative choice.

Non-interactive flows using `TSUNAMI_ASSUME_YES=1` should not block on prompts.

## Implementation Boundaries

The change should stay inside `scripts/install.sh` unless documentation needs to be updated after implementation.

Expected helper functions:

- `is_interactive`
- `prompt_choice`
- `confirm`
- `load_state_defaults`
- `show_config_summary`
- small wrappers for menu action dispatch

The existing `install_all`, `configure_all`, `update_binary`, `status_service`, `logs_service`, `show_client`, `cert_status`, and `uninstall_all` functions should remain the command-mode API. The menu should call these or small interactive wrappers around them.

## Error Handling

Keep current fail-fast behavior with `set -Eeuo pipefail`.

Validation should cover:

- Positive integer checks for Surge settings.
- Manual cert/key pair consistency.
- Unsupported architecture.
- Missing required tools for release download, checksum verification, certbot, and systemd operations.

Invalid menu choices or prompt responses should retry instead of exiting where retrying is safe.

## Verification

Implementation should be verified with:

- `bash -n scripts/install.sh`
- A non-TTY no-argument invocation still selecting the install path.
- A TTY no-argument invocation selecting the menu path.
- Explicit command invocations bypassing the menu, including `status`, `client`, and `help`.
- `TSUNAMI_ASSUME_YES=1` avoiding blocking prompts.

Where possible, tests should avoid mutating the host system by using shell-level dry checks, function-level dispatch checks, or temporary environment overrides.
