# Built-in Fronting

TSUNAMI can expose a Caddy-like HTTPS front instead of accepting the raw
TSUNAMI protocol immediately after TLS.

In this mode the public listener is a normal Go `net/http` server with HTTP/2
enabled, Caddy-style `Server: Caddy` responses, and a cover website for
ordinary requests. Only requests that match the configured path and pass the
HTTP-layer HMAC check are upgraded into a TSUNAMI tunnel. Everything else is
served as an ordinary website visit.

## Public deployment requirements

For censorship-resistance deployments, treat fronting as required:

- Use `--fronting` on the public listener. The raw post-TLS protocol mode is
  for testing or controlled networks, not for adversarial probing.
- Use a real domain name and a publicly trusted certificate for that domain.
  Do not expose the auto-generated self-signed certificate on the public
  Internet.
- Use HTTP/2 fronting as the default transport. WebSocket remains available, but
  it is not browser-identical: the ClientHello keeps a real browser fingerprint
  while the ALPN list is restricted to `http/1.1` so the HTTP/1.1 `Upgrade`
  handshake works (a real browser advertises `h2,http/1.1`, which changes JA4).
- Serve a real cover website with site camouflage (`local` or `remote` mode;
  see below) instead of the minimal built-in page. The built-in page stays
  available as the zero-dependency default and as the upstream-failure
  fallback.
- Keep `--front-secret` separate from the user password when operationally
  possible.

## Why this exists

The legacy fallback path happens after TSUNAMI has already completed the TLS
handshake. That means active probes still see TSUNAMI's TLS server behavior.

Fronting moves the protocol decision into HTTP:

```text
probe/browser -> HTTPS + HTTP/2 -> cover website (site camouflage)
client        -> HTTPS + HTTP/2/WebSocket + HMAC -> TSUNAMI session
```

This makes the visible server behavior closer to Caddy because the public entry
point uses the same Go TLS and HTTP/2 stack shape instead of a custom post-TLS
binary protocol.

## Site camouflage

Site camouflage answers every unauthenticated request with a real website,
selected by the operator. It is a reverse proxy with the same semantics as
Caddy's `reverse_proxy` directive. Three modes exist:

| Mode | Cover website | Caddy equivalent |
|:--|:--|:--|
| `builtin` | Generated single-page decoy site (zero dependencies). | `respond` |
| `local` | A website running on the same machine, for example `http://127.0.0.1:8080`. This is the "same-IP real site" mode. | `reverse_proxy 127.0.0.1:8080` |
| `remote` | Another website, for example `https://www.example.com`. This is the "borrow a neighbour's content" mode. | `reverse_proxy https://www.example.com { header_up Host {upstream_hostport} }` |

Only requests that fail the fronting HMAC check reach the site handler. The
authenticated tunnel path is always evaluated first, so the cover website
never sees valid TSUNAMI traffic.

### JSON configuration

```json
{
  "server": {
    "listen": ":443",
    "users": [{ "name": "alice", "password": "alice-pass" }],
    "fronting": {
      "enabled": true,
      "path": "/static/js/main.1a2b3c4d.js",
      "secret": "optional-front-secret",
      "server_header": "Caddy",
      "site_name": "Example",
      "site": {
        "mode": "local",
        "upstream": "http://127.0.0.1:8080",
        "preserve_host": true,
        "insecure_skip_verify": false
      }
    }
  }
}
```

The `site` object is fixed:

| Field | Values | Description |
|:--|:--|:--|
| `mode` | `builtin`, `local`, `remote` | Cover website selection. Inferred when omitted (see below). |
| `upstream` | URL or `host:port` | Cover website origin. Required for `local` and `remote`, ignored for `builtin`. |
| `preserve_host` | `true` / `false` | Keep the client `Host` header. Default: `true` for `local`, `false` for `remote`. |
| `insecure_skip_verify` | `true` / `false` | Skip TLS verification of an HTTPS upstream. Default `false`. |

### CLI flags

| Flag | Description |
|:--|:--|
| `--front-site-mode` | `builtin`, `local`, or `remote`. Default: inferred from the upstream. |
| `--front-site-upstream` | Cover website target. Accepts a full URL (`https://www.example.com`) or a bare `host:port` (`127.0.0.1:8080`). |
| `--front-site-preserve-host` | Keep the client `Host` header. Default: `local` -> true, `remote` -> false. |
| `--front-site-insecure-skip-verify` | Skip TLS verification of an HTTPS upstream. Prefer fixing the upstream certificate; this is for internal CAs only. |
| `--front-decoy-proxy` | Deprecated alias for `--front-site-upstream`. |

Same-IP real site (`local`):

```bash
tsunami-server \
  --listen :443 \
  --password "your-strong-password" \
  --cert /path/to/cert.pem \
  --key /path/to/key.pem \
  --fronting \
  --front-path /static/js/main.1a2b3c4d.js \
  --front-site-mode local \
  --front-site-upstream 127.0.0.1:8080
```

Borrowed content (`remote`):

```bash
tsunami-server \
  --listen :443 \
  --password "your-strong-password" \
  --cert /path/to/cert.pem \
  --key /path/to/key.pem \
  --fronting \
  --front-path /static/js/main.1a2b3c4d.js \
  --front-site-mode remote \
  --front-site-upstream https://www.example.com
```

Both flags apply on top of `--config`, so a config file can be overridden
without editing it. Invalid values (unknown mode, non-HTTP upstream, missing
host, invalid port, credentials in the upstream URL, or a `local`/`remote` mode
without an upstream) abort startup with a clear error.

### Mode and scheme inference

When `mode` is omitted:

1. No upstream -> `builtin`.
2. Upstream host is `localhost`, `*.localhost`, or a loopback / private /
   link-local / unspecified IP (`127.0.0.0/8`, `::1`, RFC 1918, `fc00::/7`,
   `169.254.0.0/16`, `fe80::/10`, ...) -> `local`.
3. Otherwise -> `remote`.

When `upstream` has no scheme, Caddy's rule applies: `https` when the port is
`443`, otherwise `http`. A trailing slash is trimmed. An explicit `mode` always
wins over inference.

### Behavior

The site handler is an `httputil.ReverseProxy` configured to look like Caddy:

- **Host header.** `local` preserves the client `Host` (Caddy's default for
  same-host upstreams). `remote` rewrites `Host` to the upstream `host:port`,
  matching
  `reverse_proxy https://host { header_up Host {upstream_hostport} }`.
  `preserve_host` overrides either default.
- **Forwarding headers.** `X-Forwarded-Proto` (inbound scheme),
  `X-Forwarded-Host` (original `Host`), and `X-Forwarded-For` (client IP) are
  set by TSUNAMI. Inbound values for these headers are ignored, so clients
  cannot spoof them.
- **Streaming.** Responses are flushed immediately (`FlushInterval: -1`), so
  Server-Sent Events and streaming downloads behave like a normal Caddy proxy.
- **Server header.** Every response carries the configured `Server` header
  (`Caddy` by default), including proxied responses and WebSocket upgrades.
- **Alt-Svc.** The header is stripped because TSUNAMI does not serve HTTP/3;
  forwarding it would advertise a dead endpoint and fingerprint the setup.
- **Methods, status, body.** Untouched. `POST`, `PUT`, arbitrary paths and
  query strings are proxied exactly like Caddy would proxy them.
- **WebSocket.** HTTP/1.1 `Upgrade` requests pass through to the upstream, so
  the cover site can run its own real-time features.
- **Auth isolation.** `Authorization`, `X-Api-Version`, `X-Date`, and
  `X-Trace-Id` are fronting-internal and are stripped before proxying, so a
  cover website never sees TSUNAMI metadata.
- **Upstream failure.** If the upstream is down or unreachable, the built-in
  site page is served with `200 OK` and the normal auth jitter. Proxy errors
  are logged server-side only; probes never see a `502`, a Go error page, or
  wiring details.
- **TLS verification.** HTTPS upstreams are verified against the system roots
  by default. `insecure_skip_verify` exists for internal CAs and for
  operators who cannot fix an upstream certificate chain.
- **No SSRF surface.** The upstream is operator configuration, never derived
  from the request. Clients control only the forwarded path, query, method,
  and body; the destination host is fixed.

The upstream transport uses a 5s dial timeout, 5s TLS handshake timeout, 10s
response-header timeout, 90s idle-connection timeout, HTTP/2 enabled, and 16
idle connections per host. Idle upstream connections are closed when the
server shuts down.

### Deprecated `decoy_proxy`

The previous `"decoy_proxy": "http://127.0.0.1:8080"` key and
`--front-decoy-proxy` flag keep working. They are migrated into the site
configuration on load and behave exactly as before: the origin is proxied and
the client `Host` header is preserved. `decoy_proxy` only applies when no
`site.upstream` is configured; the site block wins when both are present.
New deployments should use the `site` object.

### Operational notes

- For `local` mode, run the cover website next to TSUNAMI and bind it to
  loopback (`127.0.0.1:8080`). The certificate, domain, and page content then
  form a coherent story for an observer, and the cover site stays reachable
  even if TSUNAMI is restarted.
- For `remote` mode, the upstream's absolute links still point at the upstream
  domain, so a browser that follows a link leaves the camouflage. Copying a
  site into `local` mode is usually the stronger option.
- If the upstream is unreachable, visitors get the built-in page instead of an
  error. That fallback is deliberate: a `502` from a "normal website" is a
  strong fingerprint.
- The cover website must not depend on the TSUNAMI path: requests to the
  fronting path with a valid HMAC are tunnels, and those requests never reach
  the upstream.

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
| `--front-site-mode` | Site camouflage mode: `builtin`, `local`, or `remote`. |
| `--front-site-upstream` | Site camouflage upstream URL or `host:port`. |
| `--front-site-preserve-host` | Override `preserve_host`. |
| `--front-site-insecure-skip-verify` | Skip TLS verification of an HTTPS upstream. |
| `--front-decoy-proxy` | Deprecated alias for `--front-site-upstream`. |

`scripts/install.sh` writes all of these into `/etc/tsunami/config.json` with
per-deployment values: a generated build-hashed path (for example
`/static/js/main.1a2b3c4d.js`), a fronting secret separate from the client
password, a decoy site name derived from the domain, and a site camouflage
block. The generated client command already carries the matching
`--front-path` and `--front-secret`, so clients and server stay in sync.

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
  The same is true for a request to the fronting path with a bad or replayed
  HMAC signature: it is proxied to the cover site exactly like any other
  visitor request.
- The authenticated tunnel path is checked before the site handler, and the
  non-HTTP/2 fallback and WebSocket upgrade failure paths hand the request to
  the site handler, so the cover website is always what an observer sees on
  failure.
- HTTP/2 is the preferred transport for Caddy-like behavior; WebSocket is
  available for environments where an upgrade-style tunnel is easier to route.
- The visible TLS and HTTP/2 record shape is provided by the fronting layer.
  The inner session should not try to look like a second HTTPS connection; that
  would create an unusual HTTPS-over-HTTPS pattern instead of making the public
  connection more browser-like.
