# One-Click Deployment

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

From a local source checkout:

```bash
sudo bash scripts/install.sh
```

The installer asks for:

- listen address, default `:443`
- server domain (triggers Let's Encrypt if provided)
- fallback backend, optional
- Surge max connections and threshold

If a domain is provided, the installer **automatically requests a Let's Encrypt
certificate** using certbot. If no domain is given and no certificate paths are
set, the server uses a self-signed certificate.

## Let's Encrypt (Automatic TLS)

When you provide a domain name during installation:

1. certbot is installed automatically (via apt/yum/dnf/snap)
2. A certificate is requested using `certbot certonly --standalone`
3. Renewal hooks are configured so TSUNAMI stops/starts during renewal
4. Certificate paths are written to `config.json`

**Requirements:**
- Port 80 must be open (for HTTP-01 challenge)
- DNS must point to this server before running the installer
- Renewal happens automatically (~every 60 days, <10s downtime)

### Non-Interactive with Let's Encrypt

```bash
# Using wget
wget -qO- https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash

# Using curl
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh | \
  TSUNAMI_PUBLIC_HOST=example.com \
  TSUNAMI_PASSWORD='change-this-password' \
  TSUNAMI_LETSENCRYPT=y \
  TSUNAMI_ACME_EMAIL=you@example.com \
  TSUNAMI_FALLBACK=127.0.0.1:8080 \
  sudo -E bash
```

### Non-Interactive with Manual Certificate

```bash
curl -fsSL https://raw.githubusercontent.com/RavenholmAlpha/tsunami/main/scripts/install.sh \
  | sudo env \
      TSUNAMI_ASSUME_YES=1 \
      TSUNAMI_PUBLIC_HOST=example.com \
      TSUNAMI_LISTEN=:443 \
      TSUNAMI_PASSWORD='change-this-password' \
      TSUNAMI_CERT_FILE=/etc/letsencrypt/live/example.com/fullchain.pem \
      TSUNAMI_KEY_FILE=/etc/letsencrypt/live/example.com/privkey.pem \
      TSUNAMI_FALLBACK=127.0.0.1:8080 \
      bash
```

To install a specific release:

```bash
sudo env TSUNAMI_VERSION=v1.2.3 bash scripts/install.sh
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
sudo tsunami-manage status
sudo tsunami-manage config
sudo tsunami-manage update
sudo tsunami-manage restart
sudo tsunami-manage logs
sudo tsunami-manage client    # show connection info panel
sudo tsunami-manage cert      # show Let's Encrypt certificate status
sudo tsunami-manage uninstall
```

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
`/etc/tsunami/install.env`; both files are mode `0600`.

## Client Example

After install, show the connection info panel:

```bash
sudo tsunami-manage client
```

The panel shows the server address, password, TLS mode, service status,
and a ready-to-copy client command:

```
╔══════════════════════════════════════════════════════════════╗
║           TSUNAMI  Deploy Complete ✓                        ║
╠══════════════════════════════════════════════════════════════╣
║  Server : example.com:443                                   ║
║  Password: xK9f2m...8kPq                                   ║
║  TLS    : Let's Encrypt (auto-renew)                        ║
║  Status : ● running                                         ║
╠══════════════════════════════════════════════════════════════╣
║  Client command:                                            ║
║  tsunami-client \                                           ║
║    --server example.com:443 \                               ║
║    --password 'xK9f2m...' \                                 ║
║    --sni example.com \                                      ║
║    --socks 127.0.0.1:1080 \                                 ║
║    --http 127.0.0.1:8080                                    ║
╚══════════════════════════════════════════════════════════════╝
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
| `TSUNAMI_PUBLIC_HOST` | *(prompted)* | Server domain / public hostname |
| `TSUNAMI_CERT_FILE` | *(none)* | Manual TLS certificate path |
| `TSUNAMI_KEY_FILE` | *(none)* | Manual TLS private key path |
| `TSUNAMI_FALLBACK` | *(none)* | Fallback HTTP backend |
| `TSUNAMI_LETSENCRYPT` | *(prompted)* | `y` to auto-approve Let's Encrypt |
| `TSUNAMI_ACME_EMAIL` | *(none)* | Email for Let's Encrypt notifications |
| `TSUNAMI_VERSION` | `latest` | Release version to install |
| `TSUNAMI_ASSUME_YES` | *(none)* | `1` to skip all interactive prompts |
