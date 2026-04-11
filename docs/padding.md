# Programmable Padding System

## Overview

TSUNAMI's programmable padding system allows the server to dynamically control how the first N packets of a session are padded and split. The padding scheme is pushed from the server to all connected clients via `cmdUpdatePaddingScheme` (`0x06`), enabling traffic pattern changes without any client-side updates.

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

Define how packet at the given index should be segmented. Each packet index maps to a list of segments that describe how to split and pad the outgoing data.

### Segment Types

| Type | Syntax | Description |
|:-----|:-------|:------------|
| Size range | `min-max` | Emit a segment of random size in `[min, max]` bytes |
| Check | `c` | If all user data has been consumed, stop here |

**Example**: `2=400-500,c,500-1000,c,500-1000`

For packet index 2:
1. Emit a segment of 400–500 random bytes
2. **Check**: if no user data remains, stop
3. Emit a segment of 500–1000 bytes
4. **Check**: if no user data remains, stop
5. Emit a final segment of 500–1000 bytes

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
- Ensures the first packet is exactly 30 bytes (authentication padding)
- Applies variable-length segmentation with check points for packets 2–7
- Sends tiny keepalive frames every 30–60 seconds during idle periods

## Scheme Versioning

Each scheme is identified by the MD5 hash of its raw text. The client includes its current `padding-md5` in the `SETTINGS` frame. If the server's scheme differs, it pushes the new scheme via `cmdUpdatePaddingScheme`.

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

### Padding Frames

Padding is implemented using `cmdWaste` (`0x00`) frames:

```
Frame {
    Command:  0x00 (WASTE)
    StreamID: 0
    Data:     <zero-filled bytes of specified size>
}
```

Receivers MUST read the data and silently discard it.

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
