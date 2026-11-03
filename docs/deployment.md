# One-Click Deployment

> **English** | [中文](deployment.zh.md) | [日本語](deployment.ja.md)

This project includes a Linux installer at `scripts/install.sh`. It installs
`tsunami-server`, writes `/etc/tsunami/config.json`, creates a systemd unit,
starts the service, and prints a ready-to-use client command.

## Interactive Install

From GitHub (recommended):

```bash
# wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash

# curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | sudo bash
```

When run from an interactive terminal, these piped commands open the management
menu by reading prompts from `/dev/tty`. In automation or any environment
without a controlling TTY, the same commands keep the non-interactive install
path, **which requires an explicit TLS choice** (site camouflage defaults to the
built-in page unless `TSUNAMI_SITE_MODE` is set; see the environment variables
below). Without a TLS choice it fails with a clear message instead of silently
generating a self-signed certificate.

From a local source checkout:

```bash
sudo bash scripts/install.sh
```

In an interactive terminal, running the script without a command opens the
management menu. Choose **Install / reinstall (quick)** to deploy from the
menu, or run `sudo bash scripts/install.sh install` to install immediately.

### Quick wizard

A fresh interactive install is three questions and one confirmation:

1. **Domain** — must resolve to this server; the machine FQDN is offered as the
   default. IP addresses and placeholder/reserved names such as `example.com`,
   `your-domain.example` and `localhost` are rejected.
2. **TLS mode** — `[1] Let's Encrypt (recommended, default) [2] existing
   certificate and key files [3] self-signed (INSECURE, testing only)`.
3. **Site camouflage** — what unauthenticated visitors see (see the next
   section).
4. **Press Enter to install** — the only confirmation. Then the client panel is
   printed.

Everything else is automatic: listen `:443`, Surge defaults (4/8), random
password, empty fallback, generated fronting path/secret/site name, and the
server's built-in padding scheme. If an `install.env` from a previous deployment
exists, the wizard first offers to **reuse the existing settings** and skips the
questions.

The installer **never falls back to a self-signed certificate silently**.
Let's Encrypt mode requests a certificate with certbot after the domain has
been validated; manual mode uses the certificate paths you provide; self-signed
mode prints an explicit warning ("trivially detected, no MITM protection") and
requires confirmation. In non-interactive runs, self-signed mode additionally
requires `TSUNAMI_TLS_MODE=self-signed TSUNAMI_DEV=1`.

#### Advanced prompts

The old low-level questions (listen address, fallback backend, Surge
max connections/threshold, custom fronting path/secret/site name, padding
scheme JSON) are only asked with `TSUNAMI_ADVANCED=1` or the `advanced`
command:

```bash
sudo bash scripts/install.sh advanced
sudo TSUNAMI_ADVANCED=1 bash scripts/install.sh config
```

Every `TSUNAMI_*` environment override keeps working in the quick flow, so
automation never needs the advanced prompts.

## Site Camouflage

Fronting serves a `fronting.site` block for ordinary (unauthenticated) requests.
The installer writes it automatically from the third wizard question:

| Mode | Behaviour | Use when |
|:--|:--|:--|
| `local` | Reverse-proxies to a real site on this server; default upstream `http://127.0.0.1:8080`, detected listeners on ports 80/8080/3000/8000 are pre-selected. `preserve_host: true` | You run a real website on the same IP (recommended) |
| `remote` | Borrows another website's content; `preserve_host: false` rewrites the Host header to the upstream | No local real site; accept the origin's terms and availability |
| `builtin` | Generated placeholder page (least realistic, easiest to cluster) | Quick tests only |

JSON shape written to `/etc/tsunami/config.json`:

```json
{
  "server": {
    "fronting": {
      "enabled": true,
      "path": "/static/js/main.1a2b3c4d.js",
      "secret": "0123456789abcdef0123456789abcdef",
      "site_name": "Example",
      "decoy_proxy": "",
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

Environment overrides: `TSUNAMI_SITE_MODE`, `TSUNAMI_SITE_UPSTREAM`,
`TSUNAMI_SITE_PRESERVE_HOST`, `TSUNAMI_SITE_INSECURE_SKIP_VERIFY`.
`TSUNAMI_DECOY_PROXY` remains as a legacy alias and maps to
`remote` + upstream.

## Public Safety Baseline

For public censorship-resistance deployments, use the fronting mode documented
in [Built-in Fronting](fronting.md):

- Use a real domain and a publicly trusted certificate.
- Do not expose the self-signed mode publicly; it is only for local testing.
- Do not expose the raw post-TLS protocol mode as the public entry point
  (`TSUNAMI_FRONTING=0` is rejected by the security self-check).
- Prefer HTTP/2 fronting and configure site camouflage: a `local` real site or a
  `remote` upstream (`fronting.site`) instead of the built-in page.
- Keep the generated fronting path and separate `fronting.secret`; they make
  each deployment unique.

The installer now sets this baseline automatically, but review the generated
`/etc/tsunami/config.json` before using it in an adversarial network.

## Let's Encrypt (Automatic TLS)

When you choose Let's Encrypt and provide a real domain name during
installation:

1. certbot is installed automatically (via apt/yum/dnf/snap)
2. A certificate is requested using `certbot certonly --standalone`
3. Renewal hooks are configured so TSUNAMI stops/starts during renewal
4. Certificate paths are written to `config.json`

**Requirements:**
- Port 80 must be open (for HTTP-01 challenge)
- DNS must point to this server before running the installer
- Renewal happens automatically (~every 60 days, <10s downtime)

### Non-Interactive with Let's Encrypt

The examples below use `example.com` as a placeholder. Replace it with your real
domain; the installer rejects reserved/placeholder names and IP addresses.

```bash
# Using wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_SITE_MODE=local \
  TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:8080 \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash

# Using curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_SITE_MODE=local \
  TSUNAMI_SITE_UPSTREAM=http://127.0.0.1:8080 \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash
```

### Non-Interactive with Manual Certificate

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  | sudo env \
      TSUNAMI_ASSUME_YES=1 \
      TSUNAMI_TLS_MODE=manual \
      TSUNAMI_PUBLIC_HOST=example.com \
      TSUNAMI_LISTEN=:443 \
      TSUNAMI_PASSWORD='change-this-password' \
      TSUNAMI_CERT_FILE=/etc/letsencrypt/live/example.com/fullchain.pem \
      TSUNAMI_KEY_FILE=/etc/letsencrypt/live/example.com/privkey.pem \
      TSUNAMI_SITE_MODE=remote \
      TSUNAMI_SITE_UPSTREAM=https://www.wikipedia.org \
      TSUNAMI_FALLBACK=127.0.0.1:8080 \
      bash
```

### Non-Interactive Self-Signed (Local Testing Only)

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  sudo env \
      TSUNAMI_ASSUME_YES=1 \
      TSUNAMI_TLS_MODE=self-signed \
      TSUNAMI_DEV=1 \
      TSUNAMI_PUBLIC_HOST=dev.tsunami.lan \
      TSUNAMI_PASSWORD='change-this-password' \
      bash
```

This mode is insecure by design: the client command gets `--skip-verify`, which
disables certificate validation and provides no MITM protection. Never expose a
self-signed deployment publicly.

To install a specific release:

```bash
sudo env TSUNAMI_VERSION=v1.3.0 bash scripts/install.sh
```

When run inside a source checkout, the installer builds the current source if
Go is available. Otherwise it uses `build/linux-*` if present, then falls back
to GitHub Releases.

## Management

After remote install, download the management script once:

```bash
sudo wget -qO /usr/local/bin/tsunami-manage \
  https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  && sudo chmod +x /usr/local/bin/tsunami-manage
```

Then use it:

```bash
sudo tsunami-manage           # open interactive menu
sudo tsunami-manage status
sudo tsunami-manage config     # quick wizard (offers to reuse settings)
sudo tsunami-manage advanced   # advanced prompts
sudo tsunami-manage update
sudo tsunami-manage restart
sudo tsunami-manage logs
sudo tsunami-manage client    # show connection info panel
sudo tsunami-manage cert      # show Let's Encrypt certificate status
sudo tsunami-manage security  # run the deployment security self-check
sudo tsunami-manage uninstall
```

Explicit commands continue to work for automation; a no-argument interactive
run opens the menu.

Or from a source checkout:

```bash
sudo bash scripts/install.sh status
```

Generated files:

```text
/usr/local/bin/tsunami-server
/usr/local/bin/tsunami-client
/etc/tsunami/config.json
/etc/tsunami/client-command.txt
/etc/tsunami/install.env
/etc/systemd/system/tsunami-server.service
```

`/etc/tsunami/config.json` stores only the SHA-256 token hash. The raw client
password is written to `/etc/tsunami/client-command.txt` and
`/etc/tsunami/install.env`; the config directory is mode `0700` and those files
are mode `0600`. The generated fronting path/secret, site name and
`server.fronting.site` camouflage block live in `config.json`.

The `security` command reports PASS/WARN/FAIL for the certificate choice, SNI,
fronting state, fronting path uniqueness, separate fronting secret, site
camouflage mode and upstream, `--skip-verify` in the client command, and file
permissions. It exits non-zero when the configuration is unsafe. In detail:

- `local` site → PASS, noting that the same IP should serve a real site.
- `remote` site → PASS, noting the upstream's terms and availability.
- `builtin` site → WARN ("deployment is easy to cluster").
- `local`/`remote` without an upstream, or a malformed mode → FAIL.

## Client Example

After install, show the connection info panel:

```bash
sudo tsunami-manage client
```

The panel shows the server address, password (masked in the box), TLS mode,
fronting path, site camouflage mode/upstream, service status, and a
ready-to-copy client command:

```
╔═══════════════════════════════════════════════╗
║ TSUNAMI Deploy Complete                       ║
║ Server       : example.com:443                ║
║ Password     : xK9f2m...8kPq                  ║
║ TLS          : Let's Encrypt (auto-renew)     ║
║ Fronting     : enabled                        ║
║ Front path   : /static/js/main.1a2b3c4d.js    ║
║ Site         : local -> http://127.0.0.1:8080 ║
║ Decoy site   : Example                        ║
║ Status       : running                        ║
║ Config       : /etc/tsunami/config.json       ║
╚═══════════════════════════════════════════════╝

Client command (copy-paste, also saved at /etc/tsunami/client-command.txt):
tsunami-client --server example.com:443 --password '...' --sni example.com --fronting --front-path /static/js/main.1a2b3c4d.js --front-secret '...' --socks 127.0.0.1:1080 --http 127.0.0.1:8080
```

## uTLS Fingerprint

The client uses [uTLS](https://github.com/refraction-networking/utls) to mimic
a Chrome TLS ClientHello fingerprint by default. This makes the connection
indistinguishable from Chrome HTTPS traffic to DPI systems.

Supported fingerprints:

| Flag Value | Description |
|:-----------|:------------|
| `chrome` | Chrome (default) |
| `firefox` | Firefox |
| `safari` | Safari |
| `random` | Randomized |
| `none` | Standard Go crypto/tls (no mimicry) |

```bash
# Use Firefox fingerprint
tsunami-client --server example.com:443 --password '...' --fingerprint firefox

# Disable fingerprint mimicry
tsunami-client --server example.com:443 --password '...' --fingerprint none
```

## Environment Variables

| Variable | Default | Description |
|:---------|:--------|:------------|
| `TSUNAMI_LISTEN` | `:443` | Server listen address |
| `TSUNAMI_PASSWORD` | *(auto-generated)* | Client password |
| `TSUNAMI_PUBLIC_HOST` | *(prompted)* | Real public domain; never an IP or placeholder |
| `TSUNAMI_TLS_MODE` | `letsencrypt` | `letsencrypt`, `manual`, or `self-signed` |
| `TSUNAMI_DEV` | *(none)* | `1` required for non-interactive self-signed mode |
| `TSUNAMI_CERT_FILE` | *(none)* | Manual TLS certificate path |
| `TSUNAMI_KEY_FILE` | *(none)* | Manual TLS private key path |
| `TSUNAMI_FALLBACK` | *(none)* | Fallback HTTP backend |
| `TSUNAMI_LETSENCRYPT` | *(prompted)* | `y` to select Let's Encrypt non-interactively |
| `TSUNAMI_ACME_EMAIL` | *(none)* | Email for Let's Encrypt notifications |
| `TSUNAMI_FRONTING` | `1` | `0` disables fronting (not for public deployments) |
| `TSUNAMI_FRONT_PATH` | *(generated)* | Fronting HTTP path, e.g. `/assets/app.1a2b3c4d.js` |
| `TSUNAMI_FRONT_SECRET` | *(generated)* | Separate fronting HMAC secret |
| `TSUNAMI_SITE_NAME` | *(derived)* | Site name shown by the generated page |
| `TSUNAMI_SITE_MODE` | `builtin` (non-interactive) | Site camouflage: `local`, `remote`, or `builtin` |
| `TSUNAMI_SITE_UPSTREAM` | `http://127.0.0.1:8080` for `local` | Upstream HTTP(S) origin for `local`/`remote` |
| `TSUNAMI_SITE_PRESERVE_HOST` | `true` for `local` | Preserve the visitor's `Host` header |
| `TSUNAMI_SITE_INSECURE_SKIP_VERIFY` | `false` | Skip TLS verification for an HTTPS upstream |
| `TSUNAMI_DECOY_PROXY` | *(none)* | Legacy alias: maps to `remote` + upstream |
| `TSUNAMI_SNI` | `TSUNAMI_PUBLIC_HOST` | TLS SNI used by the client command |
| `TSUNAMI_PADDING_SCHEME_JSON` | *(server default)* | Override the server padding scheme |
| `TSUNAMI_ADVANCED` | *(none)* | `1` to ask the advanced prompts in the wizard |
| `TSUNAMI_VERSION` | `latest` | Release version to install |
| `TSUNAMI_ASSUME_YES` | *(none)* | `1` to skip all interactive prompts |
