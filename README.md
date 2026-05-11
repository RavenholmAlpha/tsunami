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

On any Linux server (Ubuntu / Debian / CentOS / RHEL):

```bash
# wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash

# or curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
```

**Non-interactive with Let's Encrypt:**

```bash
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=your-domain.com \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  sudo -E bash
```

After install, the script prints a ready-to-use client command:

```
╔══════════════════════════════════════════════════════════════╗
║           TSUNAMI  Deploy Complete ✓                        ║
╠══════════════════════════════════════════════════════════════╣
║  Server : example.com:443                                   ║
║  Password: xK9f2m...8kPq                                   ║
║  TLS    : Let's Encrypt (auto-renew)                        ║
║  Status : ● running                                         ║
╠══════════════════════════════════════════════════════════════╣
║  Client command:                                            ║
║  tsunami-client \                                           ║
║    --server example.com:443 \                               ║
║    --password '...' \                                       ║
║    --sni example.com \                                      ║
║    --socks 127.0.0.1:1080 \                                 ║
║    --http 127.0.0.1:8080                                    ║
╚══════════════════════════════════════════════════════════════╝
```

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
sudo tsunami-manage config       # Re-configure
sudo tsunami-manage restart      # Restart service
sudo tsunami-manage logs         # Follow logs
sudo tsunami-manage cert         # Let's Encrypt cert status
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

# With TLS certificate
./tsunami-server \
  --listen :443 \
  --cert /path/to/cert.pem \
  --key /path/to/key.pem \
  --password "your-strong-password" \
  --fallback 127.0.0.1:8080

# With JSON config
./tsunami-server --config /etc/tsunami/config.json
```

| Flag | Default | Description |
|:-----|:--------|:------------|
| `--listen` | `:443` | Server listen address |
| `--cert` | — | TLS certificate file (PEM) |
| `--key` | — | TLS private key file (PEM) |
| `--password` | *(required)* | Authentication password |
| `--fallback` | — | Fallback HTTP backend |
| `--config` | — | JSON configuration file |

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

| Flag | Default | Description |
|:-----|:--------|:------------|
| `--server` | *(required)* | Server address `host:port` |
| `--password` | *(required)* | Authentication password |
| `--sni` | *(server host)* | TLS SNI override |
| `--skip-verify` | `false` | Skip TLS certificate verification |
| `--fingerprint` | `chrome` | `chrome` / `firefox` / `safari` / `random` / `none` |
| `--socks` | `127.0.0.1:1080` | Local SOCKS5 proxy address |
| `--http` | `127.0.0.1:8080` | Local HTTP proxy address |
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
| `TSUNAMI_PUBLIC_HOST` | *(prompted)* | Server domain / public hostname |
| `TSUNAMI_CERT_FILE` | — | Manual TLS certificate path |
| `TSUNAMI_KEY_FILE` | — | Manual TLS private key path |
| `TSUNAMI_FALLBACK` | — | Fallback HTTP backend address |
| `TSUNAMI_LETSENCRYPT` | *(prompted)* | `y` to auto-approve Let's Encrypt |
| `TSUNAMI_ACME_EMAIL` | — | Email for Let's Encrypt notifications |
| `TSUNAMI_VERSION` | `latest` | Release version to install (`v1.2.3`) |
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

Protocol design inspired by [anytls-go](https://github.com/anytls/anytls-go).
