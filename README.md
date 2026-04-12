# TSUNAMI

> A high-performance proxy protocol built on TLS 1.3 over TCP

TSUNAMI is a multiplexed proxy protocol implemented in Go, designed for high throughput, low overhead, and resistance to traffic analysis. It uses standard TLS 1.3 as its transport layer, making traffic indistinguishable from regular HTTPS.

## Motivation

Existing proxy protocols share a set of common weaknesses:

| Problem | Impact |
|:--------|:-------|
| **Custom encryption layers** | Duplicated crypto overhead on top of TLS; non-standard handshakes become easy fingerprints for DPI |
| **Static traffic patterns** | Fixed-size headers, predictable handshake sequences, and constant stream counts create detectable signatures |
| **One connection per stream** | High connection overhead; frequent TLS handshakes under heavy load; easily identifiable "many short connections" pattern |
| **No active-probing resistance** | An unauthenticated client gets a protocol-specific error, immediately confirming the server's identity |

TSUNAMI addresses these with a fundamentally different approach:

1. **TLS 1.3 as the only encryption layer** — No custom crypto. The wire format is indistinguishable from standard HTTPS traffic (ALPN `h2`, port 443). DPI sees a normal TLS 1.3 connection.

2. **Programmable, server-pushed padding** — The server controls the traffic shape. Padding rules (packet sizes, segmentation, idle keepalives) are defined on the server and pushed to clients at runtime. Traffic patterns can be changed across all clients without any client update, adaptation happens within a single RTT.

3. **Mandatory multiplexing with adaptive scaling (Surge)** — All streams share a single TLS connection by default (Layer 1). When concurrent load exceeds a threshold, additional connections are opened automatically (Layer 2). Each stream stays pinned to one connection — no packet reordering, no added complexity.

4. **Authentication-failure fallback** — Failed auth doesn't produce an error. Instead, the connection (including the bytes already consumed during the auth attempt) is transparently forwarded to a configured HTTP backend. Active probers see a normal web server.

5. **Zero external dependencies** — Pure Go with only `golang.org/x` standard extensions. Static binary, cross-compiles to all major platforms, deploys in seconds.

## Features

- **TLS 1.3 Transport** — Pure TCP transport encrypted with TLS 1.3 (forward secrecy, ALPN `h2` negotiation)
- **Programmable Padding** — Server-pushed packet padding schemes with per-packet size distribution rules; updated dynamically without client upgrades
- **Mandatory Multiplexing** — Session–Stream architecture: multiple proxy connections share a single TLS connection
- **Surge Congestion Control** — Layered connection management: single connection by default, automatic multi-connection when concurrent streams exceed a threshold; no packet reordering
- **Fallback** — Failed authentication transparently falls back to a standard HTTP backend, making the server resistant to active probing
- **UDP-over-TCP** — UDP relay via UoT v2 framing within multiplexed streams
- **Self-Signed TLS** — Automatic self-signed certificate generation (ECDSA P-256) when no certificate is provided
- **Zero Dependencies** — Pure Go implementation with only `golang.org/x` standard extensions

## Quick Start

### Build

```bash
# Build from source
go build -o tsunami-server ./cmd/tsunami-server/
go build -o tsunami-client ./cmd/tsunami-client/

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o tsunami-server ./cmd/tsunami-server/

# Or use the build system (versioned, multi-platform)
cd build
./build.sh -v 1.0.0                                   # Linux/macOS
powershell -File build.ps1 -Version 1.0.0              # Windows
```

See [build/README.md](build/README.md) for full build system documentation.

### Server

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
```

| Flag | Default | Description |
|:-----|:--------|:------------|
| `--listen` | `:443` | Server listen address |
| `--cert` | *(none)* | TLS certificate file (PEM) |
| `--key` | *(none)* | TLS private key file (PEM) |
| `--password` | *(required)* | Authentication password |
| `--fallback` | *(none)* | Fallback HTTP backend address |
| `--version` | | Print version and exit |

### Client

```bash
./tsunami-client \
  --server your-server.example.com:443 \
  --password "your-strong-password" \
  --skip-verify \
  --socks 127.0.0.1:1080 \
  --http 127.0.0.1:8080
```

| Flag | Default | Description |
|:-----|:--------|:------------|
| `--server` | *(required)* | TSUNAMI server address (host:port) |
| `--password` | *(required)* | Authentication password |
| `--sni` | *(server hostname)* | TLS SNI field |
| `--skip-verify` | `false` | Skip TLS certificate verification |
| `--socks` | `127.0.0.1:1080` | Local SOCKS5 proxy listen address |
| `--http` | `127.0.0.1:8080` | Local HTTP proxy listen address |
| `--max-connections` | `4` | Maximum TLS connections (Surge Layer 2) |
| `--threshold` | `8` | Concurrent stream threshold for Surge upgrade |
| `--version` | | Print version and exit |

### Verify

```bash
# Test SOCKS5 proxy
curl -x socks5h://127.0.0.1:1080 https://httpbin.org/ip

# Test HTTP proxy
curl -x http://127.0.0.1:8080 https://httpbin.org/ip

# Check binary version
./tsunami-server --version
./tsunami-client --version
```

## Architecture

```
                    ┌─────────────────────────────────────────────┐
                    │               TSUNAMI Client                │
                    │                                             │
  SOCKS5/HTTP ─────►  Stream  ──►  Session  ──►  TLS 1.3  ──►  TCP
                    │              │       │                      │
                    │        ┌─────┴───────┴─────┐               │
                    │        │   Session Pool     │               │
                    │        │  (Surge Controller)│               │
                    │        └───────────────────┘               │
                    └─────────────────────────────────────────────┘
```

### Protocol Stack

| Layer | Responsibility |
|:------|:---------------|
| **TLS 1.3** | Encryption, forward secrecy, ALPN negotiation |
| **Session** | Frame encoding (7-byte header), command dispatch, padding |
| **Stream** | Multiplexing, per-connection proxy lifecycle |
| **Surge** | Adaptive connection scaling (Layer 1 / Layer 2) |

### Connection Lifecycle

```
Client                                     Server
  │                                          │
  │──────── TLS 1.3 Handshake ──────────────►│
  │◄─────── (ALPN: h2) ────────────────────  │
  │                                          │
  │──────── SHA-256(password) + padding ───►│
  │                                          │  ← constant-time verify
  │◄─────── Settings + PaddingScheme ────────│     (fallback to HTTP on failure)
  │                                          │
  │──────── SYN (stream 1) ────────────────►│
  │◄─────── SYNACK ─────────────────────────│
  │──────── PSH (target addr) ─────────────►│  ← SOCKS5-style ATYP|addr|port
  │◄──────► PSH (bidirectional data) ───────►│
  │──────── FIN ───────────────────────────►│
  │                                          │
```

### Anti-Detection Design

| Aspect | Mechanism |
|:-------|:----------|
| **DPI resistance** | Standard TLS 1.3 + ALPN `h2` on port 443 — indistinguishable from HTTPS |
| **Active probing** | Auth failure → transparent fallback to real HTTP backend |
| **Traffic analysis** | Programmable server-pushed padding controls packet sizes and timing |
| **Connection fingerprint** | Single connection by default; multi-connection only under load |
| **Timing attacks** | Constant-time password comparison; random auth padding |

### Surge Layered Design

```
Layer 1 (default):  All streams → 1 TLS connection
Layer 2 (auto):     Concurrent streams > threshold → auto multi-connection
                    Each stream stays on a single connection (no reordering)
                    Up to 4 TLS connections (configurable)
```

### TCP Tuning

| Parameter | Default | Description |
|:----------|:--------|:------------|
| `TCP_NODELAY` | enabled | Disable Nagle's algorithm |
| `SO_SNDBUF` | 4 MB | Send buffer size |
| `SO_RCVBUF` | 4 MB | Receive buffer size |
| `TCP_KEEPALIVE` | 30s | Keep-alive interval |
| Congestion control | BBR | Linux only; set via `TCP_CONGESTION` |

## Project Structure

```
tsunami/
├── cmd/
│   ├── tsunami-server/       # Server binary
│   └── tsunami-client/       # Client binary (SOCKS5 + HTTP proxy)
├── pkg/
│   ├── protocol/             # Wire format: frames, commands, auth, sessions, streams
│   ├── padding/              # Programmable padding scheme engine
│   ├── mux/                  # Session pool and connection multiplexing
│   ├── surge/                # Surge layered congestion controller
│   ├── fallback/             # Authentication failure fallback handler
│   ├── uot/                  # UDP-over-TCP relay (UoT v2)
│   ├── transport/            # TLS/TCP configuration and tuning
│   ├── proxy/                # SOCKS5 and HTTP proxy servers
│   ├── client/               # Client-side API
│   ├── server/               # Server-side implementation
│   └── config/               # Configuration file loading
├── tests/                    # Integration and proxy tests
├── build/                    # Cross-platform build scripts
├── docs/                     # Protocol specification and design documents
└── go.mod
```

## Documentation

- [Protocol Specification](docs/protocol.md) — Wire format, frame structure, commands, authentication, and proxy protocol
- [Padding Scheme](docs/padding.md) — Programmable padding system syntax and configuration
- [Surge Design](docs/surge.md) — Layered congestion control architecture
- [Build System](build/README.md) — Multi-platform build scripts and versioned releases

## Testing

```bash
# Run unit tests (with race detection)
go test -race ./pkg/...

# Run integration tests
go test ./tests/...

# Run all tests
go test ./...
```

## License

MIT
