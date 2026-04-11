# TSUNAMI Protocol Specification

> Version 3 (Current)

## Overview

TSUNAMI is a multiplexed proxy protocol that runs inside a TLS 1.3 connection. The protocol defines a lightweight binary framing layer for carrying multiple independent proxy streams over a single encrypted transport.

## Connection Lifecycle

```
Client                                     Server
  │                                          │
  │──────── TLS 1.3 Handshake ──────────────►│
  │◄─────── (ALPN: h2) ────────────────────  │
  │                                          │
  │──────── Auth: SHA-256(password) + pad ──►│
  │                                          │  ← Verify hash
  │◄─────── cmdSettings (padding scheme) ────│     (fallback on failure)
  │◄─────── cmdServerSettings ───────────────│
  │                                          │
  │──────── cmdSYN (stream 1) ──────────────►│
  │◄─────── cmdSYNACK (stream 1) ────────────│
  │──────── cmdPSH (proxy data) ────────────►│
  │◄─────── cmdPSH (proxy data) ─────────────│
  │──────── cmdFIN (stream 1) ──────────────►│
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

The password hash is verified using constant-time comparison to prevent timing attacks.

### Auth Failure

On authentication failure, the server transparently proxies the connection (including the bytes already read) to the configured fallback HTTP backend. This ensures the server behaves identically to a normal HTTPS server from the perspective of an active prober.

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
| `padding-md5` | string | MD5 of the client's current padding scheme |
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

## Stream Lifecycle

1. **Open**: Client sends `SYN` with a new monotonically increasing `streamId`
2. **Confirm**: Server responds with `SYNACK` (empty data = success, non-empty = error)
3. **Data**: Both sides exchange `PSH` frames carrying proxy payload
4. **Close**: Either side sends `FIN`; no acknowledgment required

Streams implement `io.ReadWriteCloser`. Data exceeding the maximum frame size (65535 bytes) is automatically split across multiple `PSH` frames.

## Proxy Protocol

### TCP Proxy

When a new stream is opened, the first `PSH` frame from the client contains the SOCKS5-style target address:

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

## Transport

### TLS Configuration

| Parameter | Value |
|:----------|:------|
| Minimum version | TLS 1.3 |
| ALPN | `h2` |
| Certificate | User-provided or auto-generated self-signed |

### TCP Tuning

| Parameter | Default | Description |
|:----------|:--------|:------------|
| `TCP_NODELAY` | enabled | Disable Nagle's algorithm |
| `SO_SNDBUF` | 4 MB | Send buffer size |
| `SO_RCVBUF` | 4 MB | Receive buffer size |
| `TCP_KEEPALIVE` | 30s | Keep-alive interval |
| Congestion control | BBR | Linux only; set via `TCP_CONGESTION` |

## Surge Control Actions

| Code | Name | Description |
|:-----|:-----|:------------|
| `0x01` | `ReportThroughput` | Report current throughput metrics |
| `0x02` | `RequestMoreConn` | Request additional TLS connections |
| `0x03` | `ReduceConn` | Signal to reduce connection count |
| `0x04` | `BandwidthLimit` | Set a bandwidth cap |
