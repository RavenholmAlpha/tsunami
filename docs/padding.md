# Programmable Padding System

> **English** | [中文](padding.zh.md) | [日本語](padding.ja.md)

## Overview

TSUNAMI's programmable padding system allows the server to dynamically control how the first N packets of a session are padded and split. The padding scheme is pushed from the server to all connected clients via `cmdUpdatePaddingScheme` (`0x06`), enabling traffic pattern changes without any client-side updates.

### Why Programmable Padding?

Traditional proxy protocols have fixed packet sizes and timing patterns that statistical analysis can fingerprint. TSUNAMI takes a different approach:

- **Server-controlled**: The server defines all padding rules, not the client. Adjustments propagate to all connected clients within one RTT.
- **Per-packet granularity**: Each of the first N packets can have its own size distribution and segmentation rules.
- **Runtime updatable**: The server can push a new padding scheme at any time via the `UPDATE_PADDING` command. The client applies it immediately.
- **Idle-period camouflage**: Background keepalive frames maintain traffic presence during idle periods, preventing "silence fingerprinting."

## Scheme Format

A padding scheme is defined as a newline-delimited text block with key-value pairs:

```
stop=8
0=30-30
1=100-400
2=400-500,c,500-1000,c,500-1000,c,500-1000,c,500-1000
3=9-9,500-1000
4=500-1000
5=500-1000
6=500-1000
7=500-1000
keepalive=30000-60000:4-8
```

## Keys

### `stop`

```
stop=<N>
```

Stop applying padding rules after the first N packets (0-indexed). Packets at index ≥ N are sent unmodified.

**Default**: `8`

### Packet Index Rules

```
<index>=<segment>[,<segment>,...]
```

Define how the packet at the given index should be segmented. Each packet index maps to a list of segments that describe how to split and pad the outgoing data.

### Segment Types

| Type | Syntax | Description |
|:-----|:-------|:------------|
| Size range | `min-max` | Emit a segment of random size in `[min, max]` bytes |
| Check | `c` | If all user data has been consumed, stop emitting segments |

**Example**: `2=400-500,c,500-1000,c,500-1000`

For packet index 2:
1. Emit a segment of 400–500 random bytes (filled with real data + padding)
2. **Check**: if no user data remains, stop — the packet is done
3. Emit a segment of 500–1000 bytes
4. **Check**: if no user data remains, stop
5. Emit a final segment of 500–1000 bytes

The check (`c`) mechanism allows short payloads to produce smaller packets while long payloads can fill the entire segmented frame. This creates natural variance that resists statistical analysis.

### `keepalive`

```
keepalive=<interval_min>-<interval_max>:<size_min>-<size_max>
```

Configure idle-period background keepalive packets. When all streams in a session are idle, the client sends small `cmdWaste` padding frames at random intervals.

| Parameter | Description |
|:----------|:------------|
| `interval_min`–`interval_max` | Random interval between keepalive packets (milliseconds) |
| `size_min`–`size_max` | Random keepalive packet size (bytes) |

**Example**: `keepalive=30000-60000:4-8` sends a 4–8 byte waste frame every 30–60 seconds while idle.

## Default Scheme

The built-in default scheme:

```
stop=8
0=30-30
1=100-400
2=400-500,c,500-1000,c,500-1000,c,500-1000,c,500-1000
3=9-9,500-1000
4=500-1000
5=500-1000
6=500-1000
7=500-1000
keepalive=30000-60000:4-8
```

This scheme:
- Pads the first 8 packets of each session
- Ensures the first packet is exactly 30 bytes (matches auth padding size)
- Applies variable-length segmentation with check points for packets 2–7
- Sends tiny keepalive frames every 30–60 seconds during idle periods

### Tuning Guidelines

| Scenario | Adjustment |
|:---------|:-----------|
| High-bandwidth server | Increase `stop` (e.g., 12–16); use larger size ranges |
| Low-bandwidth / mobile | Reduce `stop` (e.g., 4); use smaller size ranges to reduce overhead |
| High-security environment | Add more segments with `c` checkpoints; use shorter keepalive intervals |
| CDN-fronted deployment | Match padding sizes to typical HTTP/2 response patterns |

## Scheme Versioning

Each scheme is identified by the **MD5 hash** of its raw text. The client includes its current `padding-md5` in the `SETTINGS` frame. If the server's scheme differs, it pushes the new scheme via `cmdUpdatePaddingScheme`.

This avoids unnecessary retransmission of unchanged schemes across reconnections.

## Wire Protocol

### Pushing a New Scheme

The server sends `cmdUpdatePaddingScheme` (`0x06`) with the raw scheme text as the frame data:

```
Frame {
    Command:  0x06 (UPDATE_PADDING)
    StreamID: 0
    Data:     <scheme text bytes>
}
```

The client parses the scheme, computes its MD5, and applies it immediately to all future packets.

### Padding Frames

Padding is implemented using `cmdWaste` (`0x00`) frames:

```
Frame {
    Command:  0x00 (WASTE)
    StreamID: 0
    Data:     <zero-filled bytes of specified size>
}
```

Receivers MUST read the data and silently discard it. The waste frame data is intentionally zero-filled (not random) to minimize CPU cost — the TLS layer already encrypts it.

## Comments

Lines starting with `#` are treated as comments and ignored:

```
# Custom padding for high-bandwidth connections
stop=4
0=50-50
1=200-800
2=500-2000
3=1000-4000
keepalive=60000-120000:4-8
```

## Examples

### Minimal padding (low overhead)

```
stop=2
0=30-30
1=100-200
keepalive=60000-120000:4-4
```

### Aggressive padding (high security)

```
stop=16
0=30-30
1=200-600
2=500-1500,c,500-1500,c,500-1500
3=500-2000,c,1000-3000,c,1000-3000
4=1000-4000
5=1000-4000
6=1000-4000
7=1000-4000
8=500-2000
9=500-2000
10=500-2000
11=500-2000
12=500-1000
13=500-1000
14=500-1000
15=500-1000
keepalive=15000-30000:8-32
```
