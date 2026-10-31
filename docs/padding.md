# Programmable Padding System

> **English** | [中文](padding.zh.md) | [日本語](padding.ja.md)

## Overview

TSUNAMI's programmable padding system lets the server control how tunnel writes
are sized and padded. The padding scheme is pushed from the server to all
connected clients via `cmdUpdatePaddingScheme` (`0x06`), so traffic-pattern
changes do not require a client update.

Padding is applied **per stream and per direction**:

- Every proxied connection (stream) starts a fresh padding epoch. Even if one
  TLS session carries thousands of proxied connections over hours, the inner
  TLS handshake of **every** stream is masked, not just the first one.
- Each direction has its own writer and its own rules. The client masks its
  outbound data (SOCKS target address, inner ClientHello, application data);
  the server masks its outbound data (inner server flight, application data).
- Most rules use the `~min-max` **pad-to** segment: the whole write is emitted
  as a **single** TLS record whose size is a plausible random value in
  `[min, max]`, instead of splitting one write into several records of
  artificial sizes.
- Frames that do not belong to a single data stream (settings, SYN/SYNACK/FIN,
  heartbeats, padding updates, keepalive waste) use the `ctl` rule for the
  **whole life of the session**, so control traffic never degrades to tiny,
  constant-size records.

### Why programmable padding?

Traditional proxy protocols have fixed packet sizes and timing patterns that
statistical analysis can fingerprint. TSUNAMI takes a different approach:

- **Server-controlled**: The server defines all padding rules, not the client.
  Adjustments propagate to all connected clients within one RTT.
- **Per-stream granularity**: Every stream has its own write epoch, so each
  inner handshake gets its own masking.
- **Direction-aware**: `c<idx>` / `s<idx>` rules let one scheme express the
  client's handshake flight and the server's handshake flight.
- **Runtime updatable**: The server can push a new scheme at any time via the
  `UPDATE_PADDING` command. Clients apply it immediately.
- **Idle-period camouflage**: Background keepalive frames maintain traffic
  presence during idle periods, preventing "silence fingerprinting".

## How a write is classified

For every tunnel write, the padding writer first decides which bucket the write
belongs to:

| Bucket | Condition | Rule source | Epoch |
|:-------|:----------|:------------|:------|
| **Stream** | Every frame is `cmdPSH` **and** all frames carry the same non-zero stream ID | `<idx>`, `c<idx>`, `s<idx>` | Per stream: resets for every new stream |
| **Control** | Anything else: settings, SYN, SYNACK, FIN, heartbeat, padding-scheme update, keepalive waste, frame with stream ID 0, or a batch mixing commands/streams | `ctl` | None: the same rule applies to every control write for the session lifetime |

The per-stream epoch advances on every data write of that stream:
write 0 consults rule 0, write 1 rule 1, and so on. Padding stops for the stream
once it has emitted `stop` writes or `stream_bytes` bytes. Streams are forgotten
when they close (`Session.SetOnStreamClosed` → `Writer.ForgetStream`), so a
long-lived session does not accumulate state and a reused stream ID starts a
fresh epoch.

The control bucket has no epoch at all: the `ctl` rule is selected every time a
control frame is written, which is what keeps SYN/SYNACK/FIN and heartbeats
masked after the data-stream epochs have ended.

## Scheme Format

A padding scheme is a newline-delimited text block of key-value pairs:

```
stop=6
stream_bytes=12288
ctl=~40-160
c0=~80-200
c1=~900-1200
c2=~500-1200
s0=~2600-4200
s1=~700-1400
s2=~700-1400
3=~300-800
4=~300-800
5=~300-800
keepalive=45000-90000:4-8
```

## Keys

### `stop`

```
stop=<N>
```

Maximum number of data writes per **stream** that may be padded (0-indexed:
writes `0 .. N-1` consult rules). A stream's write `N` and later are sent
unmodified.

**Default**: `6`

### `stream_bytes`

```
stream_bytes=<N>
```

Per-stream padded-byte budget. Once the total bytes written for a stream reach
this value, that stream is sent unmodified regardless of how many epoch writes
remain. This prevents long downloads/uploads from paying padding overhead after
the handshake is masked. A value `<= 0` disables the budget.

**Default**: `12288`

### Packet Index Rules

```
<index>=<segment>[,<segment>,...]
```

Shared fallback rule for a stream's `<index>`-th data write. Used by both
directions when no directional override exists for that index.

### Directional Rules

```
c<index>=<segment>[,<segment>,...]    # client -> server override
s<index>=<segment>[,<segment>,...]    # server -> client override
```

Directional rules override the shared rule for the same index. Lookup order for
a stream write: **directional rule for `index` → shared rule for `index` →
no padding**.

The client's stream write order is:

1. SOCKS target address frame (first stream write),
2. inner TLS ClientHello,
3. application data.

The server's stream write order is:

1. inner TLS server flight (ServerHello, Certificate, ...),
2. application data.

This is why the default scheme masks the client's write 1 (`c1=~900-1200`, the
ClientHello) and the server's write 0 (`s0=~2600-4200`, the server flight).

### `ctl`

```
ctl=<segment>[,<segment>,...]
```

Rule applied to every **control** write (streamless frames and mixed batches)
for the whole session lifetime. Control writes are not limited by `stop` or
`stream_bytes`.

**Default**: `~40-160`

An empty value (`ctl=`) disables control-write padding.

### `keepalive`

```
keepalive=<interval_min>-<interval_max>:<size_min>-<size_max>
```

Configure idle-period background keepalive packets. When all streams in a
session are idle, the server sends small `cmdWaste` padding frames at random
intervals.

| Parameter | Description |
|:----------|:------------|
| `interval_min`–`interval_max` | Random interval between keepalive packets (milliseconds) |
| `size_min`–`size_max` | Random keepalive payload size (bytes); the record itself is still padded by `ctl` |

**Example**: `keepalive=45000-90000:4-8` sends a 4–8 byte waste frame every
45–90 seconds while idle.

## Segment Types

A rule is a comma-separated list of segments, applied in order:

| Type | Syntax | Description |
|:-----|:-------|:------------|
| Split range | `min-max` | Emit the next chunk of the write with a random size in `[min, max]` bytes |
| Check | `c` | If all payload bytes have been consumed, stop emitting segments |
| Pad-to | `~min-max` or `~N` | Emit the **whole remaining payload as one record**, padded to at least a random target in `[min, max]` |

### Splitting segments (`min-max`, `c`)

`2=400-500,c,500-1000,c,500-1000` for the second data write of a stream:

1. Emit one record of 400–500 random bytes (real data, then padding if needed)
2. **Check**: if no payload remains, stop — the write is done
3. Emit one record of 500–1000 bytes
4. **Check**: if no payload remains, stop
5. Emit a final record of 500–1000 bytes

If the payload is longer than the sum of all segments, the remaining bytes are
sent unmodified. Splitting is the legacy mode and remains fully supported.

### Pad-to segments (`~min-max`)

`~900-1200` emits the **entire** write as **one** TLS record:

1. Pick a random target in `[min, max]`. `~N` means exactly `N..N`.
2. Clamp the target into `[len(payload)+7, 16384]`:
   - at least `len(payload)+7` so a `cmdWaste` frame header fits inside the
     record;
   - at most `MaxPadTarget = 16384`, the TLS record plaintext limit.
3. If the payload is already at least as large as the requested target, write
   it unchanged. The same applies when the payload leaves no room for a waste
   header below the record limit.
4. Otherwise append one `cmdWaste` frame inside the same write, then write the
   whole record in a single call. Pad-to **never splits** a write.

A pad-to segment consumes the rest of the payload, so `~` is only meaningful as
the last segment of a rule; any segments after it in the same rule are ignored.

### Plausible record sizes

Pad-to targets are chosen from distributions that resemble real TLS records for
the same traffic class: a small handshake preamble, a ~1 KB ClientHello, a
2.6–4.2 KB server certificate flight, and 0.3–1.2 KB application-data records.
Exact sizes of the inner handshake therefore no longer show up in outer record
sizes.

## Default Scheme

```
stop=6
stream_bytes=12288
ctl=~40-160
c0=~80-200
c1=~900-1200
c2=~500-1200
s0=~2600-4200
s1=~700-1400
s2=~700-1400
3=~300-800
4=~300-800
5=~300-800
keepalive=45000-90000:4-8
```

This scheme:

- pads the first 6 data writes of **every** stream (fresh epoch per stream),
- masks the client's SOCKS address write (`c0`), inner ClientHello (`c1`) and
  first application-data write (`c2`),
- masks the server's inner handshake flight (`s0`) and first application-data
  writes (`s1`, `s2`),
- masks later application-data writes with the shared rules `3`, `4`, `5`,
- pads every control write with `ctl=~40-160` for the whole session lifetime,
- sends a tiny keepalive every 45–90 seconds during idle periods.

Rules are index-based, so the sizes above assume the usual write order. A client
that writes the SOCKS address and the first TLS bytes in a single `Write` call
will use `c0` for the combined write; that is safe, it only shifts which rule
masks which record.

## Scheme Versioning

Each scheme is identified by the **MD5 hash of its raw text**. The client
includes its current `padding-md5` in the `SETTINGS` frame. If the server's
scheme differs, it pushes the new scheme via `cmdUpdatePaddingScheme`.

This avoids unnecessary retransmission of unchanged schemes across reconnections.

## Wire Protocol

Padding does not add new frame types or change the frame format. The wire
protocol is unchanged and **no protocol version bump is required**.

### Pushing a New Scheme

The server sends `cmdUpdatePaddingScheme` (`0x06`) with the raw scheme text as
the frame data:

```
Frame {
    Command:  0x06 (UPDATE_PADDING)
    StreamID: 0
    Data:     <scheme text bytes>
}
```

The client parses the scheme, computes its MD5, and applies it immediately to
all future writes; existing per-stream epochs are preserved.

### Padding Frames

Padding is implemented using `cmdWaste` (`0x00`) frames:

```
Frame {
    Command:  0x00 (WASTE)
    StreamID: 0
    Data:     <zero-filled bytes of specified size>
}
```

Receivers MUST read the data and silently discard it. The waste frame data is
intentionally zero-filled (not random) to minimize CPU cost — the TLS layer
already encrypts it. A padded record may contain a real frame followed by a
`cmdWaste` frame (pad-to) or several records split from one write (legacy
splitting).

## Compatibility and Migration

- **New clients, old scheme text.** Existing schemes such as
  `stop=8 / 0=30-30 / ... / keepalive=30000-60000:4-8` still parse. On new
  clients `stop` and the data rules are interpreted **per stream** instead of
  per session, and the implicit `ctl=~40-160` rule applies because the old text
  has no `ctl` key. Old clients interpret the same text per session.
- **Wire format.** Padding only changes the sizes/counts of existing frames and
  records; it never changes their syntax. There is no protocol version bump.
- **Mixed versions and `~`.** New scheme text uses keys that old parsers ignore
  (`stream_bytes`, `ctl`, `c<idx>`, `s<idx>`), but old parsers **cannot parse**
  the `~min-max` segment syntax inside a rule value and would reject the pushed
  scheme. New clients therefore advertise `padding-v2=1` in their `SETTINGS`
  frame, and the server only pushes v2 scheme text to clients that advertise
  it: pre-v2 clients are never sent rules they cannot parse. They keep using
  their own scheme and the tunnel keeps working, because padding only adds
  `cmdWaste` frames. Upgrade clients first if you want every client on the same
  scheme.
- Because padding is applied per direction and the opposite side only needs to
  discard `cmdWaste` frames, two peers with different schemes can still talk;
  the MD5 negotiation only keeps them in sync when they support the same
  syntax.

## Comments

Lines starting with `#` are treated as comments and ignored:

```
# Custom padding for high-bandwidth connections
stop=4
stream_bytes=16384
ctl=~60-200
c0=~100-300
c1=~1000-1400
0=~400-1000
1=~400-1000
2=~400-1000
3=~400-1000
keepalive=60000-120000:4-8
```

## Examples

### Minimal padding (low overhead)

```
stop=2
stream_bytes=4096
ctl=~40-80
c0=~60-120
c1=~900-1200
keepalive=60000-120000:4-4
```

### Aggressive padding (high security)

```
stop=12
stream_bytes=32768
ctl=~80-320
c0=~120-320
c1=~900-1400
c2=~700-1600
s0=~2600-5200
s1=~900-1800
s2=~900-1800
3=~500-1200
4=~500-1200
5=~500-1200
6=~500-1200
7=~500-1200
8=~500-1200
9=~500-1200
10=~500-1200
11=~500-1200
keepalive=15000-30000:8-32
```

### Legacy-compatible scheme (split segments only)

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

## Tuning Guidelines

| Scenario | Adjustment |
|:---------|:-----------|
| High-bandwidth server | Increase `stop`/`stream_bytes`; use larger `~` ranges |
| Low-bandwidth / mobile | Reduce `stop`/`stream_bytes`; use smaller `~` ranges |
| High-security environment | Choose `~` ranges that match real TLS records; raise `ctl`; use shorter keepalive intervals |
| CDN-fronted deployment | Match `~` targets to typical HTTP/2 response record sizes |
| Legacy interoperability | Avoid `~` in rules; use `min-max` split segments only |
