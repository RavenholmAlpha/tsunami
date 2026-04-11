# TSUNAMI

> A high-performance proxy protocol built on TLS 1.3 over TCP

TSUNAMI is a multiplexed proxy protocol implemented in Go, designed for high throughput, low overhead, and resistance to traffic analysis. It uses standard TLS 1.3 as its transport layer, making traffic indistinguishable from regular HTTPS.

## Features

- **TLS 1.3 Transport** — Pure TCP transport encrypted with TLS 1.3 (forward secrecy, ALPN `h2` negotiation)
- **Programmable Padding** — Server-pushed packet padding schemes with per-packet size distribution rules; updated dynamically without client upgrades
- **Mandatory Multiplexing** — Session–Stream architecture: multiple proxy connections share a single TLS connection
- **Surge Congestion Control** — Layered connection management: single connection by default, automatic multi-connection when concurrent streams exceed a threshold; no packet reordering
- **Fallback** — Failed authentication transparently falls back to a standard HTTP backend, making the server resistant to active probing
- **UDP-over-TCP** — UDP relay via UoT v2 framing within multiplexed streams
- **Self-Signed TLS** — Automatic self-signed certificate generation when no certificate is provided
- **Zero Dependencies** — Pure Go implementation with only `golang.org/x` standard extensions

## Quick Start

### Build

```bash
# Build server
go build -o tsunami-server ./cmd/tsunami-server/

# Build client
go build -o tsunami-client ./cmd/tsunami-client/

# Cross-compile for Linux
GOOS=linux GOARCH=amd64 go build -o tsunami-server ./cmd/tsunami-server/
```

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

### Verify

```bash
# Test SOCKS5 proxy
curl -x socks5h://127.0.0.1:1080 https://httpbin.org/ip

# Test HTTP proxy
curl -x http://127.0.0.1:8080 https://httpbin.org/ip
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

### Surge Layered Design

```
Layer 1 (default):  All streams → 1 TLS connection
Layer 2 (auto):     Concurrent streams > threshold → auto multi-connection
                    Each stream stays on a single connection (no reordering)
                    Up to 4 TLS connections (configurable)
```

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
├── docs/                     # Protocol specification and design documents
└── go.mod
```

## Documentation

- [Protocol Specification](docs/protocol.md) — Wire format, frame structure, and command reference
- [Padding Scheme](docs/padding.md) — Programmable padding system syntax and configuration
- [Surge Design](docs/surge.md) — Layered congestion control architecture

## Testing

```bash
# Run all tests
go test ./...

# Run integration tests only
go test ./tests/...
```

## License

MIT
