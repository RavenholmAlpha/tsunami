<div align="center">

# 🌊 TSUNAMI

**High-performance multiplexed proxy protocol built on TLS 1.3**

[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue?style=flat-square)](LICENSE)
[![Release](https://img.shields.io/github/v/release/RavenholmAlpha/tsunami?style=flat-square&color=brightgreen)](https://github.com/RavenholmAlpha/tsunami/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/RavenholmAlpha/tsunami/ci-cd.yml?style=flat-square&label=CI)](https://github.com/RavenholmAlpha/tsunami/actions)
[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-Community-blue?style=flat-square)](https://linux.do)

*Wire-identical to standard HTTPS — DPI sees nothing but a regular TLS 1.3 connection.*

---

**English** | [中文](README.zh.md) | [日本語](README.ja.md)

[Features](#features) · [Quick Start](#quick-start) · [Deploy](#one-click-deploy) · [Architecture](#architecture) · [Documentation](#documentation)

</div>

## Why TSUNAMI?

| Problem with existing protocols | TSUNAMI's approach |
|:------|:------|
| Custom encryption on top of TLS — duplicated overhead, easy DPI fingerprints | **TLS 1.3 only** — no custom crypto, wire-identical to HTTPS |
| Fixed packet sizes and predictable handshakes | **Programmable padding** — server-pushed, per-packet size distributions |
| One connection per stream — frequent handshakes, detectable patterns | **Mandatory multiplexing** — all streams share one TLS connection |
| Active probing reveals protocol identity | **Transparent fallback** — failed auth proxies to real HTTP backend |

## Features

<table>
<tr>
<td width="50%">

🔐 **TLS 1.3 Transport**
Pure TCP + TLS 1.3 with ALPN `h2`, forward secrecy.

🎭 **uTLS Fingerprint**
Mimics Chrome / Firefox / Safari ClientHello. Defeats JA3/JA4.

🧩 **Mandatory Multiplexing**
Session–Stream architecture. All proxy connections share a single TLS connection.

⚡ **Surge Auto-Scaling**
Single connection by default; auto multi-connection under heavy load. No packet reordering.

</td>
<td width="50%">

🛡️ **Fallback Anti-Probe**
Failed auth → transparent proxy to real HTTP backend. Active probers see a normal website.

📐 **Programmable Padding**
Server-pushed padding schemes control packet sizes and idle keepalives. Changed across all clients without any client update.

🌐 **UDP-over-TCP**
UDP relay via UoT v2 framing within multiplexed streams.

📦 **Minimal Dependencies**
Pure Go + `golang.org/x` + uTLS. Single static binary, cross-compiles everywhere.

</td>
</tr>
</table>

## Quick Start

### One-Click Deploy

On any Linux server (Ubuntu / Debian / CentOS / RHEL), run the installer from an
interactive terminal:

```bash
# wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash

# or curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
```

The wizard is short — three questions and one confirmation:

1. **Domain** — must resolve to this server (the machine FQDN is the default).
2. **TLS mode** — `[1] Let's Encrypt (recommended) [2] existing certificate/key
   files [3] self-signed (INSECURE, testing only)`.
3. **Site camouflage** — what unauthenticated visitors see:

| Mode | Behaviour | Use when |
|:--|:--|:--|
| `local` | Reverse-proxies to a real site on this server (default `http://127.0.0.1:8080`, auto-detected listeners on ports 80/8080/3000/8000); Host header preserved | You run a real website on the same IP (recommended) |
| `remote` | Borrows another website's content (Host header rewritten to the upstream) | You do not run a real site locally and accept the origin's terms/availability |
| `builtin` | Generated placeholder page (least realistic, easiest to cluster) | Quick tests only |

Then press Enter once to install. Everything else is automatic: `:443`, Surge
defaults, random password, unique fronting path/secret, derived site name, and
the server's built-in padding scheme. Advanced knobs (listen/fallback/surge/
custom front path/secret/site name/padding) are only asked with
`TSUNAMI_ADVANCED=1` or `sudo bash install.sh advanced`. See
[Built-in Fronting](docs/fronting.md) for the server-side details.

A non-interactive install must choose TLS explicitly (site camouflage defaults
to the built-in page unless `TSUNAMI_SITE_MODE` is set); there is no silent
self-signed fallback.

**Non-interactive with Let's Encrypt (replace `example.com` with your real domain):**

```bash
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_SITE_MODE=local \
  TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:8080 \
  sudo -E bash
```

**Local testing only (INSECURE self-signed certificate):**

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  sudo env TSUNAMI_ASSUME_YES=1 TSUNAMI_TLS_MODE=self-signed TSUNAMI_DEV=1 \
  TSUNAMI_PUBLIC_HOST=dev.tsunami.lan TSUNAMI_PASSWORD='change-this-password' bash
```

The installer validates the hostname (no IPs or placeholder domains such as
`example.com`), generates a unique fronting path and separate fronting secret,
derives a site name, and writes the site-camouflage block
(`fronting.site.mode`/`upstream`/`preserve_host`). After install it prints a
ready-to-use client command:

```
╔═══════════════════════════════════════════════╗
║ TSUNAMI Deploy Complete                       ║
║ Server       : example.com:443                ║
║ Password     : xK9f2m...8kPq                  ║
║ TLS          : Let's Encrypt (auto-renew)     ║
║ Fronting     : enabled                        ║
║ Front path   : /static/js/main.1a2b3c4d.js    ║
║ Site         : local -> http://127.0.0.1:8080 ║
║ Decoy site   : Example                        ║
║ Status       : running                        ║
║ Config       : /etc/tsunami/config.json       ║
╚═══════════════════════════════════════════════╝

Client command (copy-paste, also saved at /etc/tsunami/client-command.txt):
tsunami-client --server example.com:443 --password '...' --sni example.com --fronting --front-path /static/js/main.1a2b3c4d.js --front-secret '...' --socks 127.0.0.1:1080 --http 127.0.0.1:8080
```

`sudo tsunami-manage security` audits the deployment and reports PASS/WARN/FAIL
for the certificate choice, SNI, fronting settings, site camouflage mode and
upstream, `--skip-verify` usage, and file permissions. A `builtin` site or a
`local` site without an upstream is flagged.

### Service Management

```bash
# Install the management script
sudo wget -qO /usr/local/bin/tsunami-manage \
  https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  && sudo chmod +x /usr/local/bin/tsunami-manage

# Commands
sudo tsunami-manage status       # Service status
sudo tsunami-manage client       # Show connection info
sudo tsunami-manage update       # Update to latest release
sudo tsunami-manage config       # Re-configure (quick wizard, offers reuse)
sudo tsunami-manage advanced     # Re-configure with the advanced prompts
sudo tsunami-manage restart      # Restart service
sudo tsunami-manage logs         # Follow logs
sudo tsunami-manage cert         # Let's Encrypt cert status
sudo tsunami-manage security     # Deployment security self-check
sudo tsunami-manage uninstall    # Remove everything
```

### Build from Source

```bash
go build -trimpath -ldflags="-s -w" -o tsunami-server ./cmd/tsunami-server/
go build -trimpath -ldflags="-s -w" -o tsunami-client ./cmd/tsunami-client/
```

### Manual Usage

<details>
<summary><b>Server</b></summary>

```bash
# Minimal — auto-generates self-signed certificate
./tsunami-server \
  --listen :443 \
  --password "your-strong-password" \
  --fallback 127.0.0.1:8080

# Fronting is enabled by default. Self-signed mode is for local testing only;
# public deployments should use a real domain certificate and decoy origin.

# Public deployment: real domain certificate + decoy origin
./tsunami-server \
  --listen :443 \
  --cert /path/to/cert.pem \
  --key /path/to/key.pem \
  --password "your-strong-password" \
  --front-decoy-proxy http://127.0.0.1:8080

# With JSON config
./tsunami-server --config /etc/tsunami/config.json
```

JSON configuration follows the same safe default: omitting
`server.fronting.enabled` enables fronting. Raw mode requires an explicit
opt-in inside the `server` object: `"fronting": { "enabled": false }`.

| Flag | Default | Description |
|:-----|:--------|:------------|
| `--listen` | `:443` | Server listen address |
| `--cert` | — | TLS certificate file (PEM) |
| `--key` | — | TLS private key file (PEM) |
| `--password` | *(required)* | Authentication password |
| `--fronting` | `true` | Serve the public listener as HTTPS/HTTP2/WebSocket fronting; use `--fronting=false` only for controlled raw-mode networks |
| `--front-decoy-proxy` | - | Optional HTTP(S) decoy origin for unauthenticated fronting requests |
| `--fallback` | — | Fallback HTTP backend |
| `--config` | — | JSON configuration file; omitted `server.fronting.enabled` defaults to `true` |

</details>

<details>
<summary><b>Client</b></summary>

```bash
./tsunami-client \
  --server your-server.com:443 \
  --password "your-strong-password" \
  --socks 127.0.0.1:1080 \
  --http 127.0.0.1:8080
```

HTTPS/HTTP2 fronting is enabled by default. Use `--fronting=false` only when
connecting to a controlled raw-mode server.

| Flag | Default | Description |
|:-----|:--------|:------------|
| `--server` | *(required)* | Server address `host:port` |
| `--password` | *(required)* | Authentication password |
| `--sni` | *(server host)* | TLS SNI override |
| `--skip-verify` | `false` | Skip TLS certificate verification |
| `--fingerprint` | `chrome` | `chrome` / `firefox` / `safari` / `random` / `none` |
| `--socks` | `127.0.0.1:1080` | Local SOCKS5 proxy address |
| `--http` | `127.0.0.1:8080` | Local HTTP proxy address |
| `--fronting` | `true` | Use HTTPS/HTTP2/WebSocket fronting; use `--fronting=false` only for controlled raw-mode networks |
| `--max-connections` | `4` | Max TLS connections (Surge) |
| `--threshold` | `8` | Stream threshold for Surge Layer 2 |

</details>

<details>
<summary><b>Verify</b></summary>

```bash
# Test SOCKS5
curl -x socks5h://127.0.0.1:1080 https://httpbin.org/ip

# Test HTTP proxy
curl -x http://127.0.0.1:8080 https://httpbin.org/ip

# Version
./tsunami-server --version
./tsunami-client --version
```

</details>

## Architecture

```
                      ┌──────────────────────────────────────────────┐
                      │                TSUNAMI Client                │
                      │                                              │
   App ──► SOCKS5 ────┤  Stream  ──►  Session  ──►  TLS 1.3  ──►  TCP
   App ──► HTTP   ────┤              │         │                     │
                      │        ┌─────┴─────────┴──────┐              │
                      │        │   Surge Controller    │              │
                      │        │  (adaptive scaling)   │              │
                      │        └──────────────────────┘              │
                      └──────────────────────────────────────────────┘
                                         │
                                    TLS 1.3 (ALPN: h2)
                                    port 443
                                         │
                      ┌──────────────────────────────────────────────┐
                      │                TSUNAMI Server                │
                      │                                              │
   TCP  ──►  TLS  ────┤  Auth ──► Session ──► Stream ──► Dial Target │
                      │    │                                         │
                      │    └─── fail? ──► Fallback HTTP Backend      │
                      └──────────────────────────────────────────────┘
```

### Protocol Stack

| Layer | Responsibility |
|:------|:--------------|
| **TLS 1.3** | Encryption, forward secrecy, ALPN negotiation |
| **Auth** | SHA-256 password hash + random padding → constant-time verify |
| **Session** | 7-byte frame header, command dispatch, padding engine |
| **Stream** | Multiplexed proxy connections, SOCKS5-style addressing |
| **Surge** | Adaptive Layer 1 → Layer 2 connection scaling |

### Connection Lifecycle

```
Client                                     Server
  │                                          │
  │──────── TLS 1.3 Handshake ──────────────►│  ALPN: h2
  │◄─────── (ServerHello) ──────────────────│
  │                                          │
  │──────── SHA-256(password) + padding ───►│  constant-time verify
  │                                          │  fail → fallback to HTTP
  │◄─────── Settings + PaddingScheme ───────│
  │                                          │
  │──────── SYN (stream 1) ────────────────►│
  │◄─────── SYNACK ────────────────────────│
  │──────── PSH (target addr) ─────────────►│  ATYP|addr|port
  │◄──────► PSH (bidirectional) ───────────►│
  │──────── FIN ───────────────────────────►│
  │                                          │
```

### Anti-Detection

| Attack Vector | Defense |
|:------|:------|
| **DPI** | Pure TLS 1.3 + ALPN `h2` on port 443 |
| **JA3/JA4 fingerprinting** | uTLS mimics real browser ClientHello |
| **Active probing** | Fallback to real HTTP backend on auth failure |
| **Traffic analysis** | Programmable server-pushed padding |
| **Timing attacks** | Constant-time password comparison |
| **Connection fingerprint** | Single connection default; multi only under load |

### Surge: Adaptive Connection Scaling

```
                       streams > threshold
  Layer 1  ─────────────────────────────────►  Layer 2
  (1 conn)  ◄─────────────────────────────────  (N conns)
                       streams ≤ threshold/4
```

- **Layer 1** (default): all streams → 1 TLS connection
- **Layer 2** (auto): concurrent streams exceed threshold → distribute across up to `max-connections` TLS connections
- Each stream stays pinned to one connection — zero packet reordering

## Project Structure

```
tsunami/
├── cmd/
│   ├── tsunami-server/        Server binary
│   └── tsunami-client/        Client binary (SOCKS5 + HTTP)
├── pkg/
│   ├── protocol/              Wire format, frames, auth, sessions, streams
│   ├── padding/               Programmable padding engine
│   ├── mux/                   Session pool & multiplexing
│   ├── surge/                 Adaptive connection scaling
│   ├── fallback/              Auth failure fallback handler
│   ├── uot/                   UDP-over-TCP relay (UoT v2)
│   ├── transport/             TLS/TCP config & tuning
│   ├── proxy/                 SOCKS5 & HTTP proxy servers
│   ├── client/                Client-side API
│   ├── server/                Server implementation
│   ├── control/               Control plane (adapter/middleware/user store)
│   └── config/                Configuration loading
├── scripts/
│   └── install.sh             One-click deployment script
├── tests/                     Integration tests
├── build/                     Cross-platform build scripts
├── docs/                      Design documents
└── go.mod
```

## Environment Variables

For non-interactive / automated deployment:

| Variable | Default | Description |
|:---------|:--------|:------------|
| `TSUNAMI_LISTEN` | `:443` | Server listen address |
| `TSUNAMI_PASSWORD` | *(auto-generated)* | Client password |
| `TSUNAMI_PUBLIC_HOST` | *(prompted)* | Real public domain (no IPs or placeholders) |
| `TSUNAMI_TLS_MODE` | `letsencrypt` | `letsencrypt`, `manual`, or `self-signed` |
| `TSUNAMI_DEV` | — | `1` required for non-interactive self-signed mode |
| `TSUNAMI_CERT_FILE` | — | Manual TLS certificate path |
| `TSUNAMI_KEY_FILE` | — | Manual TLS private key path |
| `TSUNAMI_FALLBACK` | — | Fallback HTTP backend address |
| `TSUNAMI_LETSENCRYPT` | *(prompted)* | `y` to select Let's Encrypt non-interactively |
| `TSUNAMI_ACME_EMAIL` | — | Email for Let's Encrypt notifications |
| `TSUNAMI_FRONTING` | `1` | `0` disables fronting (not for public deployments) |
| `TSUNAMI_FRONT_PATH` | *(generated)* | Fronting HTTP path with a build-hashed name |
| `TSUNAMI_FRONT_SECRET` | *(generated)* | Separate fronting HMAC secret |
| `TSUNAMI_SITE_NAME` | *(derived)* | Decoy site name (derived from the domain) |
| `TSUNAMI_SITE_MODE` | `builtin` (non-interactive) | Site camouflage: `local`, `remote`, or `builtin` |
| `TSUNAMI_SITE_UPSTREAM` | `http://127.0.0.1:8080` for `local` | Upstream HTTP(S) origin for `local`/`remote` |
| `TSUNAMI_SITE_PRESERVE_HOST` | `true` for `local` | Preserve the visitor's `Host` header |
| `TSUNAMI_SITE_INSECURE_SKIP_VERIFY` | `false` | Skip TLS verification for an HTTPS upstream |
| `TSUNAMI_DECOY_PROXY` | — | Legacy alias: maps to `remote` + upstream |
| `TSUNAMI_SNI` | `TSUNAMI_PUBLIC_HOST` | TLS SNI used by the client command |
| `TSUNAMI_PADDING_SCHEME_JSON` | *(server default)* | Override the server default padding scheme |
| `TSUNAMI_ADVANCED` | — | `1` to ask the advanced prompts in the wizard |
| `TSUNAMI_VERSION` | `latest` | Release version to install (`v1.3.0`) |
| `TSUNAMI_ASSUME_YES` | — | `1` to skip all interactive prompts |

## Documentation

| Document | Description |
|:---------|:------------|
| [Protocol Specification](docs/protocol.md) | Wire format, frames, commands, authentication |
| [Padding Scheme](docs/padding.md) | Programmable padding system syntax & configuration |
| [Surge Design](docs/surge.md) | Adaptive connection scaling architecture |
| [Built-in Fronting](docs/fronting.md) | Caddy-like HTTPS/HTTP2/WebSocket fronting mode |
| [Deployment Guide](docs/deployment.md) | One-click install, Let's Encrypt, environment variables |
| [Control Plane](docs/control-plane.md) | Panel adapters, middleware, user store |
| [Build System](build/README.md) | Cross-platform build scripts |

## Testing

```bash
# Unit tests (with race detection)
go test -race ./pkg/...

# Integration tests
go test ./tests/...

# All tests
go test ./...
```

## License

[MIT](LICENSE)

## Acknowledgments

Protocol design inspired by [anytls-go](https://github.com/anytls/anytls-go). TSUNAMI's transport layer protocol (frame format, session multiplexing, padding scheme, and UDP-over-TCP signaling) is based on the [AnyTLS](https://github.com/anytls/anytls-go) protocol specification. TSUNAMI extends it with HTTPS/HTTP2/WebSocket fronting, Surge adaptive connection scaling, constant-rate traffic shaping, and a control plane for user management.
