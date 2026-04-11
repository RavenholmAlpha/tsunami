# Control Plane Adaptation

This document describes the extension points for connecting TSUNAMI nodes to
panels, board systems, and future subscription/client adapters.

## Goals

- Keep TSUNAMI protocol and relay code independent from any specific panel.
- Normalize users from Xboard, V2board, custom panels, or local files into one
  runtime model.
- Allow adapter-specific behavior through middleware instead of protocol changes.
- Support dynamic user validity, per-user speed limits, and usage reporting.

## Runtime Flow

```text
Panel or local config
  -> control.Adapter
  -> control.Middleware chain
  -> control.UserStore
  -> server.UserAuthenticator
  -> TSUNAMI sessions and streams
```

Adapters fetch external data. Middleware validates and transforms it. UserStore
applies full or incremental snapshots atomically, so active nodes can refresh
users without rebuilding the server.

## Adapter Contract

Adapters implement:

```go
type Adapter interface {
    Name() string
    FetchSnapshot(ctx context.Context) (*Snapshot, error)
}
```

Future adapters should live outside protocol code. Examples:

- `xboard.Adapter`: fetch users, quota, expiry, and speed limits from Xboard.
- `v2board.Adapter`: normalize V2board node API payloads.
- `panel.Adapter`: connect to a custom HTTPS or WebSocket control service.
- `subscription.Adapter`: produce client-facing config data without changing
  the server authentication path.

## Middleware Contract

Middleware implements:

```go
type Middleware interface {
    Name() string
    Apply(ctx context.Context, snapshot *Snapshot) error
}
```

Use middleware for board-specific cleanup and policy:

- fill missing IDs or names
- convert board speed units to bytes per second
- filter users not assigned to this node
- validate unique token hashes
- enforce node-level allow/deny rules

The built-in `NormalizeUsers` and `ValidateUsers` middleware are intentionally
generic.

## User Model

`protocol.UserInfo` now supports panel-oriented fields:

- `ID`, `Name`
- `Password` for local/static deployments
- `TokenHash` for panel deployments
- `Disabled`, `ExpiresAt`
- `Bandwidth` in Mbps for compatibility
- `SpeedLimitBps` for explicit byte-per-second limits
- `QuotaBytes`, `UsedUploadBytes`, `UsedDownloadBytes`
- `MaxSessions`, `MaxDevices`
- `Metadata`

Panels should prefer `TokenHash` and avoid sending plaintext passwords.

## Traffic Hooks

The server relay path uses `control.TrafficPolicy`.

- `UsageRecorder` receives per-user upload/download byte deltas.
- `Limiter` can enforce per-user global speed limits.
- `UserLimiter` is a generic per-user token bucket. It uses `SpeedLimitBps`
  first, then falls back to `Bandwidth` in Mbps.

Usage reporting to a panel should be implemented as a recorder that batches
deltas and sends them periodically.

## Implementation Guidance

Start adapters as thin translators. Keep board-specific quirks in middleware.
Do not add Xboard, V2board, Clash, or subscription details to `pkg/protocol` or
`pkg/server`.

Recommended future packages:

```text
pkg/control/adapters/xboard
pkg/control/adapters/v2board
pkg/control/adapters/panel
pkg/control/subscription
```

The TSUNAMI server should keep consuming only `UserAuthenticator` and
`TrafficPolicy`.
