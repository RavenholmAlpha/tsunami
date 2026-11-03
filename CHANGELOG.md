# Changelog

> **English** | [中文](CHANGELOG.zh.md) | [日本語](CHANGELOG.ja.md)

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.3.0] - 2026-11-03

### Added
- **Site camouflage** for unauthenticated fronting traffic, with the same reverse-proxy semantics as Caddy's `reverse_proxy`: `local` proxies to a local HTTP endpoint (the same IP also serves a real site), `remote` proxies to another website (borrowed content, Host rewritten to the upstream by default), and `builtin` keeps the generated page. Configured with `fronting.site` (`mode`, `upstream`, `preserve_host`, `insecure_skip_verify`) or the new `--front-site-mode` / `--front-site-upstream` / `--front-site-preserve-host` / `--front-site-insecure-skip-verify` flags. The old `decoy_proxy` key and `--front-decoy-proxy` flag remain as deprecated aliases.
- Proxied camouflage responses keep the fronting shape: fronting auth headers are stripped, `Server` is forced to the configured value, `Alt-Svc` is removed (no HTTP/3), WebSocket upgrades pass through, and upstream failures fall back to the built-in page with 200 instead of leaking proxy errors.
- Installer `advanced` command / `TSUNAMI_ADVANCED=1` exposes the detailed prompts (listen, fallback, Surge, custom fronting values, padding scheme) without cluttering the quick flow.

### Changed
- Padding is applied **per stream** instead of per session. Every proxied stream starts its own padding epoch, so each new inner TLS handshake (client ClientHello, server certificate flight) is masked, not only the first one on a long-lived session.
- Padding scheme v2 adds `stream_bytes` (per-stream padded-byte budget), `ctl` (rule for control writes, applied for the whole session lifetime), directional `c<idx>`/`s<idx>` overrides, and `~min-max` pad-to segments that emit one record padded to a plausible size instead of splitting it. The new default scheme masks the ClientHello at 0.9–1.2 KiB, the server flight at 2.6–4.2 KiB and control frames at 40–160 B.
- Clients advertise `padding-v2=1` in their `SETTINGS` frame; servers only push v2 scheme text to clients that advertise support, so mixed-version sessions keep working.
- The client session pool no longer keeps a permanent idle connection: `MinIdleSession` defaults to 0, idle sessions close after a randomized timeout, and application heartbeats are jittered (25–55 s) and suppressed while traffic flows instead of a fixed 15 s ping.
- Installer hardening: Let's Encrypt is the default TLS mode, self-signed requires an explicit dev opt-in, hostnames/SNI are validated, and a unique fronting path, separate fronting secret and derived decoy site name are written to both the server config and the client command. The hardcoded padding scheme was removed so the server's built-in default applies.
- New `security` installer command (menu entry 9) runs a deployment self-check and exits non-zero on unsafe findings.
- Installer quick wizard: a fresh interactive install now asks only domain → TLS mode → site camouflage and ends with a single "Press Enter to install" confirmation. Existing installs offer to reuse their saved settings, the local real-site upstream is auto-detected from `ss`/`lsof`, and everything else (listen address, Surge limits, fallback, fronting path/secret, decoy site name) is generated. The `security` check also reports the site camouflage mode and upstream.
- README, deployment and padding docs updated for the new installer flow and the per-stream padding model.

### Fixed
- The installer can no longer silently produce a self-signed certificate, a placeholder SNI or a `--skip-verify` client command for a public deployment.
- Padding no longer stops after the first eight writes of a session and leaves later streams' inner TLS handshakes unmasked.
- WebSocket fronting now actually advertises only `http/1.1` in the TLS ALPN extension. The uTLS ClientHello patch was silently discarded before the handshake, so clients offered the browser default `h2,http/1.1` and then spoke HTTP/1.1, which broke WebSocket tunnels against HTTP/2-capable servers (including TSUNAMI's own server). Added regression tests for both the WebSocket ALPN override and the normal browser ALPN list.
- Fronting replay protection now inserts a nonce only after the request signature is verified, and the replay cache is bounded, so unauthenticated probes can no longer grow it without limit (memory DoS). The server also derives a single fronting key (explicit `fronting.secret`, or the first static user's password as a deprecated fallback) instead of accepting every user password, removing the O(users) HMAC cost per unauthenticated request.
- Destination policy now resolves the target once and dials the validated IP, and rejects CGNAT/cloud-metadata, benchmarking, multicast, IPv6 zone and unspecified/broadcast ranges; this closes DNS-rebinding/SSRF bypasses. UoT applies the same resolved-address validation.
- Sessions are limited to 256 concurrent streams by default (`--max-streams` / `max_streams`), per-stream receive queues are bounded to four frames, relay buffers are 64 KiB, and a stream that never sends a destination address is closed after 10 seconds. A peer opening streams without using them can no longer exhaust memory or goroutines.

## [1.2.1] - 2026-06-22

### Fixed
- Piped installer commands such as `wget -qO- ... | sudo bash` now open the interactive menu when a controlling terminal is available.
- Installer prompts read from `/dev/tty` for piped interactive runs, while fully non-interactive environments still use the non-interactive install path.

## [1.2.0] - 2026-06-22

### Added
- Interactive deployment management menu for local `scripts/install.sh` and `tsunami-manage` runs.
- Guided configuration summary with masked password display before writing config.
- Confirmation prompts for install, reconfigure, update, uninstall, and config removal flows.
- Installer behavior tests covering menu dispatch, confirmation handling, state defaults, and non-interactive compatibility.

### Changed
- No-argument interactive runs now open the management menu; pipe/non-TTY installs still run the existing install path.
- Reconfiguration reuses previous `/etc/tsunami/install.env` values as prompt defaults while preserving `TSUNAMI_*` environment variable priority.
- Deployment documentation now explains menu usage and explicit command mode.

## [1.1.0] - 2026-05-08

### Added
- **uTLS Fingerprint Mimicry** — client mimics Chrome/Firefox/Safari TLS ClientHello to defeat JA3/JA4 fingerprinting (`--fingerprint` flag)
- **Let's Encrypt Auto-Cert** — one-click deployment with automatic certificate issuance and renewal via certbot
- **Connection Info Panel** — install script prints a ready-to-use client command after deployment
- `CHANGELOG.md`, `SECURITY.md`, `CONTRIBUTING.md`
- CI/CD: `golangci-lint`, `shellcheck`, `govulncheck` security scanning
- CI/CD: test coverage reporting via Codecov
- Build scripts and documentation now tracked in git (`build/`)

### Changed
- CI/CD pipeline upgraded from 2-stage to 4-stage (Lint → Test → Security → Release)
- Integration tests now block releases (removed `continue-on-error`)
- `.gitignore` refined — build scripts/docs are tracked, only build output is ignored
- Install script rewritten with Let's Encrypt support, certbot renewal hooks, and management commands

### Fixed
- Tar packaging self-inclusion risk in CI/CD release job
- Missing LICENSE file (MIT)

### Security
- Removed scripts containing hardcoded server credentials
- Added `govulncheck` to CI/CD pipeline

## [1.0.0] - 2026-05-07

### Added
- Initial release
- TLS 1.3 transport with ALPN `h2` and forward secrecy
- Mandatory multiplexing (Session–Stream architecture)
- Surge adaptive connection scaling (Layer 1 → Layer 2)
- Programmable server-pushed padding scheme
- Transparent fallback on auth failure (anti-active-probing)
- UDP-over-TCP relay (UoT v2)
- SOCKS5 and HTTP proxy support
- One-click Linux deployment script (`install.sh`)
- Cross-platform builds (linux/amd64, linux/arm64, windows/amd64, darwin/amd64, darwin/arm64)
- Integration test suite (E2E, multi-stream, auth failure, session reuse, large data transfer)

[Unreleased]: https://github.com/RavenholmAlpha/tsunami/compare/v1.2.1...HEAD
[1.2.1]: https://github.com/RavenholmAlpha/tsunami/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/RavenholmAlpha/tsunami/releases/tag/v1.0.0
