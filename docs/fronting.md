# Built-in Fronting

TSUNAMI can expose a Caddy-like HTTPS front instead of accepting the raw
TSUNAMI protocol immediately after TLS.

In this mode the public listener is a normal Go `net/http` server with HTTP/2
enabled, Caddy-style `Server: Caddy` responses, and a small decoy website for
ordinary requests. Only requests that match the configured path and pass the
HTTP-layer HMAC check are upgraded into a TSUNAMI tunnel.

## Public deployment requirements

For censorship-resistance deployments, treat fronting as required:

- Use `--fronting` on the public listener. The raw post-TLS protocol mode is
  for testing or controlled networks, not for adversarial probing.
- Use a real domain name and a publicly trusted certificate for that domain.
  Do not expose the auto-generated self-signed certificate on the public
  Internet.
- Use HTTP/2 fronting as the default transport. WebSocket remains available,
  but it is not currently browser-identical.
- Prefer a real decoy origin with `--front-decoy-proxy` when possible. The
  built-in decoy stays available as the zero-dependency default, but it is only
  a minimal fallback page.
- Keep `--front-secret` separate from the user password when operationally
  possible.

## Why this exists

The legacy fallback path happens after TSUNAMI has already completed the TLS
handshake. That means active probes still see TSUNAMI's TLS server behavior.

Fronting moves the protocol decision into HTTP:

```text
probe/browser -> HTTPS + HTTP/2 -> decoy website
client        -> HTTPS + HTTP/2/WebSocket + HMAC -> TSUNAMI session
```

This makes the visible server behavior closer to Caddy because the public entry
point uses the same Go TLS and HTTP/2 stack shape instead of a custom post-TLS
binary protocol.

## Server

```bash
tsunami-server \
  --listen :443 \
  --password "your-strong-password" \
  --cert /path/to/cert.pem \
  --key /path/to/key.pem \
  --fronting \
  --front-path /assets/update
```

Optional flags:

| Flag | Description |
|:--|:--|
| `--front-secret` | HTTP-layer HMAC secret. Defaults to the user password for CLI configs. |
| `--front-site-name` | Title/body text used by the built-in decoy page. |
| `--front-decoy-proxy` | Optional HTTP(S) origin for unauthenticated fronting requests, for example `http://127.0.0.1:8080`. Defaults to the built-in decoy. |

JSON config:

```json
{
  "server": {
    "listen": ":443",
    "users": [{ "name": "alice", "password": "alice-pass" }],
    "fronting": {
      "enabled": true,
      "path": "/assets/update",
      "secret": "optional-front-secret",
      "server_header": "Caddy",
      "site_name": "Welcome",
      "decoy_proxy": "http://127.0.0.1:8080"
    }
  }
}
```

## Client

HTTP/2 streaming transport:

```bash
tsunami-client \
  --server example.com:443 \
  --password "your-strong-password" \
  --sni example.com \
  --fronting \
  --front-path /assets/update \
  --front-transport h2
```

WebSocket transport:

```bash
tsunami-client \
  --server example.com:443 \
  --password "your-strong-password" \
  --sni example.com \
  --fronting \
  --front-path /assets/update \
  --front-transport websocket
```

Optional flags:

| Flag | Description |
|:--|:--|
| `--front-host` | Overrides the HTTP Host header. Defaults to SNI. |
| `--front-secret` | HTTP-layer HMAC secret. Defaults to the TSUNAMI password. |

## Notes

- Fronting keeps the original TSUNAMI authentication inside the tunnel.
- Invalid or unauthenticated requests are handled as ordinary website traffic.
- HTTP/2 is the preferred transport for Caddy-like behavior; WebSocket is
  available for environments where an upgrade-style tunnel is easier to route.
- The visible TLS and HTTP/2 record shape is provided by the fronting layer.
  The inner session should not try to look like a second HTTPS connection; that
  would create an unusual HTTPS-over-HTTPS pattern instead of making the public
  connection more browser-like.
