# Surge: Layered Congestion Control

## Overview

Surge is TSUNAMI's adaptive connection scaling system. It manages the number of underlying TLS connections based on real-time stream concurrency, balancing between single-connection efficiency and multi-connection throughput.

### Why Surge?

Single-connection multiplexing is ideal for stealth (one connection = one HTTPS session). But under heavy load (many concurrent streams), a single TLS connection becomes a bottleneck:

- **Head-of-line blocking**: A slow TLS write on one stream blocks all others
- **TCP fairness**: One connection gets one "fair share" of bandwidth; multiple connections can aggregate bandwidth
- **Keep-alive overhead**: Many idle streams on one connection generate unnecessary frame processing

Surge solves this by dynamically switching between single-connection (Layer 1) and multi-connection (Layer 2) based on actual demand.

## Design Principles

1. **Default to single connection** — Minimize resource usage and fingerprint under low load
2. **Scale automatically** — Upgrade to multiple connections only when needed
3. **No packet reordering** — Each stream is pinned to a single TLS connection for its entire lifetime
4. **Single port** — All connections use the same port (typically 443)
5. **Gradual teardown** — Idle connections are closed only after a configurable idle timeout

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
         AND sessions reduced to 1
```

- **Upgrade** (1 → 2): Triggered when active stream count exceeds the threshold. The controller proactively creates additional sessions up to `max-connections` asynchronously.
- **Downgrade** (2 → 1): Triggered when active streams drop to `threshold/2` or below. Idle sessions (idle longer than `idle-downgrade-time`) are closed, keeping at least 1. Layer 1 is restored only after the session count reaches 1.

The controller evaluates layer transitions every **2 seconds**.

### Upgrade Behavior

When upgrading to Layer 2, Surge doesn't just create one additional connection — it **proactively expands** to `max-connections`:

```
evaluate() detects streams (13) > threshold (8)
  → Store Layer 2
  → Async: create sessions until pool has max-connections (4)
    → Session 2 created (2/4)
    → Session 3 created (3/4)
    → Session 4 created (4/4)
```

If stream pressure continues during Layer 2, the controller continues to attempt expansion each evaluation cycle until `max-connections` is reached.

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

### Tuning Guidelines

| Scenario | Recommendation |
|:---------|:---------------|
| Light browsing (< 8 tabs) | Default settings (threshold=8, max=4) work well |
| Heavy concurrent downloads | Increase threshold to 16; increase max-connections to 8 |
| Mobile / metered connection | Set mode to `none` to stay on single connection |
| High-latency link | Lower threshold to 4 to upgrade sooner |

## Session Selection

### Layer 1

Uses the standard session pool logic: prefer idle sessions (newest first), fall back to any non-closed session for multiplexing. Creates a new session only if no sessions exist.

### Layer 2

Uses **least-loaded** distribution: new streams are assigned to the session with the fewest active streams. If no sessions are available, a new one is created (up to `max-connections`).

This ensures even stream distribution across connections without active migration (streams never move between sessions).

## Session Pool

The session pool (`pkg/mux`) manages the lifecycle of all TLS connections:

| Behavior | Detail |
|:---------|:-------|
| **Idle checking** | Every 30 seconds, close sessions idle longer than 60s (keep at least 1) |
| **Sequence tracking** | Each session has a monotonically increasing sequence number for ordering |
| **Automatic creation** | When no suitable session is available, the pool dials a new TLS connection |
| **Graceful closure** | Active streams complete naturally; only idle sessions are eligible for cleanup |

## Wire Protocol

Surge uses the `SURGE_CTRL` (`0x0B`) frame for control signaling:

| Action Code | Name | Description |
|:------------|:-----|:------------|
| `0x01` | `ReportThroughput` | Report current throughput metrics |
| `0x02` | `RequestMoreConn` | Request the client to open additional connections |
| `0x03` | `ReduceConn` | Signal the client to reduce connection count |
| `0x04` | `BandwidthLimit` | Set a bandwidth limit |

Bandwidth statistics are exchanged periodically via `BW_REPORT` (`0x0C`) frames.

## Comparison with Alternatives

| Approach | Pros | Cons |
|:---------|:-----|:-----|
| **Always single connection** | Minimal fingerprint, simple | Head-of-line blocking, bandwidth bottleneck |
| **Always multi-connection** | High throughput | Large fingerprint, wasted resources under low load |
| **Surge (adaptive)** | Best of both: stealth under low load, throughput under high load | Slight complexity in layer transition logic |
