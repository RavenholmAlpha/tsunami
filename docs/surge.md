# Surge: Layered Congestion Control

## Overview

Surge is TSUNAMI's adaptive connection scaling system. It manages the number of underlying TLS connections based on real-time stream concurrency, balancing between single-connection efficiency and multi-connection throughput.

## Design Principles

1. **Default to single connection** — Minimize resource usage and fingerprint under low load
2. **Scale automatically** — Upgrade to multiple connections only when needed
3. **No packet reordering** — Each stream is pinned to a single TLS connection for its entire lifetime
4. **Single port** — All connections use the same port (typically 443)

## Layered Architecture

### Layer 1 (Default)

All streams are multiplexed over a single TLS connection.

```
Stream A ─┐
Stream B ─┤──► Session 0 ──► TLS Connection 0 ──► :443
Stream C ─┘
```

**Active when**: concurrent streams ≤ threshold (default: 8)

### Layer 2 (Auto-Upgrade)

When concurrent stream count exceeds the threshold, new TLS connections are opened and streams are distributed across them using a least-loaded strategy.

```
Stream A ─┐
Stream B ─┤──► Session 0 ──► TLS Connection 0 ──┐
Stream C ─┘                                      │
                                                  ├──► :443
Stream D ─┐                                      │
Stream E ─┤──► Session 1 ──► TLS Connection 1 ──┘
Stream F ─┘
```

**Active when**: concurrent streams > threshold

### Layer Transitions

```
         streams > threshold
Layer 1 ─────────────────────────► Layer 2
         ◄─────────────────────────
         streams ≤ threshold/2
         AND sessions ≤ 1
```

- **Upgrade** (1 → 2): Triggered when active stream count exceeds the threshold and the current session count is below the maximum
- **Downgrade** (2 → 1): Triggered when active streams drop to half the threshold or below, and only one session remains

The controller evaluates layer transitions every 2 seconds.

## Configuration

| Parameter | Default | Description |
|:----------|:--------|:------------|
| `mode` | `auto` | Operating mode: `none` (single connection only) or `auto` |
| `threshold` | `8` | Concurrent stream count that triggers Layer 2 |
| `max-connections` | `4` | Maximum number of TLS connections in Layer 2 |
| `idle-downgrade-time` | `30s` | How long a session must be idle before being eligible for closure during downgrade |

### Client CLI

```bash
./tsunami-client \
  --server example.com:443 \
  --password "..." \
  --max-connections 4 \
  --threshold 8
```

## Session Selection

### Layer 1

Uses the standard session pool logic: return the newest idle session, or create a new one if none are available.

### Layer 2

Uses **least-loaded** distribution: new streams are assigned to the session with the fewest active streams. If no sessions are available, a new one is created (up to `max-connections`).

## Session Pool

The session pool (`pkg/mux`) manages the lifecycle of all TLS connections:

- **Idle checking**: Every 30 seconds, the pool checks for sessions that have been idle longer than the idle timeout (60 seconds) and closes them, while maintaining at least one idle session
- **Sequence tracking**: Each session has a monotonically increasing sequence number for ordering
- **Automatic creation**: When no suitable session is available, the pool dials a new TLS connection

## Wire Protocol

Surge uses the `SURGE_CTRL` (`0x0B`) frame for control signaling:

| Action Code | Name | Description |
|:------------|:-----|:------------|
| `0x01` | `ReportThroughput` | Report current throughput metrics |
| `0x02` | `RequestMoreConn` | Request the client to open additional connections |
| `0x03` | `ReduceConn` | Signal the client to reduce connection count |
| `0x04` | `BandwidthLimit` | Set a bandwidth limit |

Bandwidth statistics are exchanged periodically via `BW_REPORT` (`0x0C`) frames.
