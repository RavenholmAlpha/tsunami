# TSUNAMI Protocol Specification

> **English** | [中文](protocol.zh.md) | [日本語](protocol.ja.md)

> Version 3 (Current)

## Overview

TSUNAMI is a multiplexed proxy protocol that runs inside a TLS 1.3 connection. The protocol defines a lightweight binary framing layer for carrying multiple independent proxy streams over a single encrypted transport.

### Design Goals

| Goal | Approach |
|:-----|:---------|
| **Undetectable** | Pure TLS 1.3 with ALPN `h2` on port 443 — wire-identical to HTTPS |
| **Low overhead** | 7-byte frame header; no duplicate encryption layer |
| **Active-probe resistant** | Auth failure falls back to real HTTP backend transparently |
| **Adaptable traffic shape** | Server-pushed padding schemes control packet sizes without client updates |
| **High throughput** | BBR congestion control, 4 MB buffers, adaptive multi-connection scaling |

## Connection Lifecycle

```
Client                                     Server
  │                                          │
  │──────── TLS 1.3 Handshake ──────────────►│  ← ALPN: h2, min TLS 1.3
  │◄─────── (ServerHello, Certificate) ─────│
  │                                          │
  │──────── Auth: SHA-256(pwd) + pad ──────►│  ← constant-time verify
  │                                          │     ┌─ success: continue
  │                                          │     └─ failure: fallback to HTTP backend
  │◄─────── SETTINGS (padding scheme) ──────│
  │◄─────── SERVER_SETTINGS ────────────────│
  │                                          │
  │──────── SYN (stream N) ────────────────►│  ← monotonically increasing streamId
  │◄─────── SYNACK (stream N) ─────────────│
  │──────── PSH: ATYP|addr|port ──────────►│  ← first PSH = target address
  │◄──────► PSH (bidirectional data) ───────►│
  │──────── FIN (stream N) ────────────────►│  ← no FIN-ACK required
  │                                          │
```

## Authentication

Authentication is performed immediately after the TLS handshake, before any session frames.

### Auth Request (Client → Server)

```
+-------------------+-------------------+-----------+
| SHA-256(password) | padding length    | padding   |
+-------------------+-------------------+-----------+
| 32 bytes          | uint16 big-endian | variable  |
+-------------------+-------------------+-----------+
```

- **SHA-256(password)**: 32-byte hash of the user password
- **padding length**: Length of the following random padding (0–65535)
- **padding**: Random bytes to prevent fixed-size auth fingerprinting

The password hash is verified using **constant-time comparison** (`crypto/subtle.ConstantTimeCompare`) to prevent timing attacks.

### Auth Failure — Fallback

On authentication failure, the server does **not** send any error response. Instead, it transparently proxies the entire connection (including the bytes already read during the auth attempt) to a configured fallback HTTP backend.

This ensures:
- An active prober cannot distinguish the server from a normal HTTPS server
- No protocol-specific error messages are ever exposed
- The backend's response (including TLS certificate and HTTP behavior) matches a real website
- If no fallback backend is configured, the server responds with a minimal "It works!" HTML page and drains the connection

## Frame Format

All session-layer communication uses a fixed-size 7-byte frame header:

```
+----------+-------------------+-------------------+------+
| command  | streamId          | data length       | data |
+----------+-------------------+-------------------+------+
| uint8    | uint32 big-endian | uint16 big-endian | var  |
+----------+-------------------+-------------------+------+
         Total header overhead: 7 bytes
```

| Field | Size | Description |
|:------|:-----|:------------|
| `command` | 1 byte | Command type identifier |
| `streamId` | 4 bytes | Stream identifier (big-endian) |
| `data length` | 2 bytes | Payload length in bytes (big-endian), max 65535 |
| `data` | variable | Payload data (may be empty) |

## Commands

### Version 1

| Code | Name | Direction | Description |
|:-----|:-----|:----------|:------------|
| `0x00` | `WASTE` | bidirectional | Padding frame. Receivers MUST read and silently discard the data. |
| `0x01` | `SYN` | client → server | Open a new stream. `streamId` MUST be monotonically increasing within a session. |
| `0x02` | `PSH` | bidirectional | Carry stream payload data. |
| `0x03` | `FIN` | bidirectional | Close a stream. No reply FIN is required. |
| `0x04` | `SETTINGS` | client → server | Client settings, sent as the first frame of every new session. |
| `0x05` | `ALERT` | server → client | Server warning message; both sides close the session. |
| `0x06` | `UPDATE_PADDING` | server → client | Push a new padding scheme to the client. |

### Version 2

| Code | Name | Direction | Description |
|:-----|:-----|:----------|:------------|
| `0x07` | `SYNACK` | server → client | Confirm a stream is open; data field carries error if rejected. |
| `0x08` | `HEART_REQ` | bidirectional | Keep-alive ping. |
| `0x09` | `HEART_RSP` | bidirectional | Keep-alive pong (response to `HEART_REQ`). |
| `0x0A` | `SERVER_SETTINGS` | server → client | Server capabilities and configuration. |

### Version 3 (TSUNAMI Extensions)

| Code | Name | Direction | Description |
|:-----|:-----|:----------|:------------|
| `0x0B` | `SURGE_CTRL` | bidirectional | Surge congestion control signaling. |
| `0x0C` | `BW_REPORT` | bidirectional | Periodic bandwidth statistics. |
| `0x0D` | `STREAM_PRIORITY` | client → server | Set the priority of a stream (0=highest, 255=lowest). |

Unknown commands MUST be silently ignored for forward compatibility.

## Settings Exchange

### Client Settings (`SETTINGS`, `0x04`)

Sent as the first frame after authentication. Wire format is newline-delimited key-value pairs:

```
v=3
client=tsunami-client/1.0
padding-md5=a1b2c3d4e5f6...
surge-bandwidth=100
```

| Key | Type | Description |
|:----|:-----|:------------|
| `v` | int | Protocol version |
| `client` | string | Client identifier |
| `padding-md5` | string | MD5 hash of the client's current padding scheme |
| `surge-bandwidth` | int | Client bandwidth in Mbps (0 = disabled) |

### Server Settings (`SERVER_SETTINGS`, `0x0A`)

Sent in response to client settings:

```
v=3
surge-mode=auto
max-connections=4
threshold=8
```

| Key | Type | Description |
|:----|:-----|:------------|
| `v` | int | Protocol version |
| `surge-mode` | string | Surge mode: `none` or `auto` |
| `max-connections` | int | Maximum TLS connections allowed (Surge Layer 2) |
| `threshold` | int | Concurrent stream threshold for Layer 2 upgrade |

### Settings Negotiation Flow

1. Client sends `SETTINGS` with its protocol version and current padding scheme hash
2. Server responds with `SERVER_SETTINGS` containing Surge parameters
3. If the client's `padding-md5` differs from the server's scheme, the server sends `UPDATE_PADDING` with the full new scheme
4. The client applies the new padding scheme immediately

## Stream Lifecycle

1. **Open**: Client sends `SYN` with a new monotonically increasing `streamId`
2. **Confirm**: Server responds with `SYNACK` (empty data = success, non-empty = error message)
3. **Target**: Client sends the first `PSH` containing the SOCKS5-style target address
4. **Data**: Both sides exchange `PSH` frames carrying proxy payload
5. **Close**: Either side sends `FIN`; no acknowledgment required

Streams implement `io.ReadWriteCloser`. Data exceeding the maximum frame size (65535 bytes) is automatically split across multiple `PSH` frames.

## Proxy Protocol

### TCP Proxy

The first `PSH` frame on a new stream contains the SOCKS5-style target address:

```
+------+----------+-------+
| ATYP | address  | port  |
+------+----------+-------+
| 1B   | variable | u16be |
+------+----------+-------+
```

| ATYP | Value | Address Format |
|:-----|:------|:---------------|
| IPv4 | `0x01` | 4 bytes |
| Domain | `0x03` | 1-byte length + domain string |
| IPv6 | `0x04` | 16 bytes |

After the target address frame, all subsequent `PSH` frames carry raw TCP payload in both directions.

### UDP-over-TCP (UoT v2)

UDP traffic is carried over TCP streams using the UoT v2 framing. A stream targeting the magic domain `sp.v2.udp-over-tcp.arpa` signals UDP relay mode.

Each UDP packet is independently framed within the stream:

```
+------+----------+-------+--------+------+
| ATYP | address  | port  | length | data |
+------+----------+-------+--------+------+
| 1B   | variable | u16be | u16be  | var  |
+------+----------+-------+--------+------+
```

The ATYP/address/port fields specify the UDP destination for each individual packet, allowing multiplexed UDP traffic over a single stream.

## Transport

### TLS Configuration

| Parameter | Value | Rationale |
|:----------|:------|:----------|
| Minimum version | TLS 1.3 | Forward secrecy, no downgrade attacks |
| ALPN | `h2` | Mimics HTTP/2 traffic pattern |
| Cipher suites | TLS 1.3 defaults | AES-GCM / ChaCha20-Poly1305 |
| Certificate | User-provided or auto self-signed | ECDSA P-256 for auto-generated |

### TCP Tuning

| Parameter | Default | Description |
|:----------|:--------|:------------|
| `TCP_NODELAY` | enabled | Disable Nagle's algorithm for lower latency |
| `SO_SNDBUF` | kernel autotuning | TSUNAMI does not set a fixed socket send buffer unless configured by the caller |
| `SO_RCVBUF` | kernel autotuning | TSUNAMI does not set a fixed socket receive buffer unless configured by the caller |
| `TCP_KEEPALIVE` | 30s | Detect dead connections |
| Congestion control | system default | Linux can opt into per-connection BBR with `--force-bbr` |

> **Note**: BBR is best-effort when explicitly enabled. If the kernel doesn't have BBR loaded (`modprobe tcp_bbr`) or the socket option fails, the system default congestion control is used.

## Surge Control Actions

| Code | Name | Description |
|:-----|:-----|:------------|
| `0x01` | `ReportThroughput` | Report current throughput metrics |
| `0x02` | `RequestMoreConn` | Request additional TLS connections |
| `0x03` | `ReduceConn` | Signal to reduce connection count |
| `0x04` | `BandwidthLimit` | Set a bandwidth cap |

## Security Considerations

| Attack Vector | Mitigation |
|:--------------|:-----------|
| **Password brute force** | SHA-256 hash + constant-time comparison; fallback makes bruteforce indistinguishable from normal traffic |
| **Active probing** | Transparent fallback to HTTP backend; no protocol-specific error responses |
| **Traffic fingerprinting** | Programmable padding; randomized auth padding length |
| **Timing analysis** | Constant-time auth; random keepalive intervals |
| **Connection correlation** | Single connection by default; multi-connection only on demand |
| **Replay attacks** | TLS 1.3 provides built-in replay protection via unique per-session keys |
