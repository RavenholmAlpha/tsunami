# Changelog

> **English** | [中文](CHANGELOG.zh.md) | [日本語](CHANGELOG.ja.md)

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[1.2.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/RavenholmAlpha/tsunami/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/RavenholmAlpha/tsunami/releases/tag/v1.0.0
