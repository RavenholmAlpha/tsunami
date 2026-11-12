# TSUNAMI on OpenWrt

Each release carries a TSUNAMI **client** for OpenWrt routers. It is a static
binary (no libc or package dependencies) plus a procd init script, packed as a
`.tar.gz` that extracts onto the root filesystem. It is not an `.ipk`, so `opkg`
does not track it.

The package holds only `tsunami-client`. A router runs the client and exposes a
SOCKS5 and an HTTP proxy; run the server elsewhere.

## Choose the archive

Run `uname -m` on the router:

| `uname -m` | Archive | Notes |
|:--|:--|:--|
| `x86_64` | `tsunami-<version>-openwrt-x86_64.tar.gz` | |
| `i686`, `i586` | `tsunami-<version>-openwrt-x86.tar.gz` | Built with software floating point so it also runs on CPUs without SSE2. |
| `aarch64` | `tsunami-<version>-openwrt-aarch64.tar.gz` | |
| `armv7l` | `tsunami-<version>-openwrt-armv7.tar.gz` | Needs VFPv3: `grep -o vfpv3 /proc/cpuinfo` must print a match. |

MIPS, MIPS64 and ARMv5/ARMv6 devices are not built.

The archives are 2.6 to 2.9 MB. The client binary is 6.5 to 6.9 MB once
extracted, so check `df -h /` first on small flash.

## Install

Use the real version in place of `v1.2.3`. Check the download against
`checksums.sha256` from the same release before extracting it onto `/`:

```sh
cd /tmp
wget https://github.com/<owner>/<repo>/releases/download/v1.2.3/tsunami-v1.2.3-openwrt-aarch64.tar.gz
wget https://github.com/<owner>/<repo>/releases/download/v1.2.3/checksums.sha256
grep ' tsunami-v1.2.3-openwrt-aarch64.tar.gz$' checksums.sha256 | sha256sum -c -
tar -xzf tsunami-v1.2.3-openwrt-aarch64.tar.gz -C /
```

The archive installs three files:

| Path | Purpose |
|:--|:--|
| `/usr/bin/tsunami-client` | The client |
| `/etc/init.d/tsunami` | procd service script |
| `/usr/share/tsunami/tsunami.config.example` | UCI configuration to copy |

Installing never touches `/etc/config/tsunami`, so an upgrade keeps your settings.

## Configure and start

```sh
cp /usr/share/tsunami/tsunami.config.example /etc/config/tsunami
chmod 600 /etc/config/tsunami
vi /etc/config/tsunami          # set server, password and enabled '1'
/etc/init.d/tsunami enable
/etc/init.d/tsunami start
logread -e tsunami
```

`/etc/config/tsunami` options:

| Option | Default | Client flag |
|:--|:--|:--|
| `enabled` | `0` | The service does nothing until it is `1` |
| `server` | none, required | `--server` |
| `password` | none, required | `--password` |
| `socks` | `127.0.0.1:1080` | `--socks` |
| `http` | `127.0.0.1:8080` | `--http` |
| `fingerprint` | `chrome` | `--fingerprint` |
| `sni` | server host | `--sni` |
| `skip_verify` | `0` | `--skip-verify` when `1` |
| `front_path` | client default | `--front-path` |
| `front_secret` | the password | `--front-secret` |
| `front_transport` | `h2` | `--front-transport` |
| `max_connections` | `4` | `--max-connections` |

`/etc/init.d/tsunami reload` applies a changed configuration. procd restarts
the client if it exits.

## Security notes

- The client takes the password as a command-line argument, so any local user can
  read it from `/proc/<pid>/cmdline`. Keep the router's accounts to root only and
  the configuration file at mode `0600`.
- The proxies bind to the router's loopback by default. To serve LAN clients,
  bind to the LAN address, for example `192.168.1.1:1080`. Never bind to
  `0.0.0.0` on a router with a public WAN address: an open SOCKS5 or HTTP proxy
  is an open relay.

## Upgrade and remove

Upgrade by extracting the newer archive the same way and running
`/etc/init.d/tsunami restart`.

```sh
/etc/init.d/tsunami stop
/etc/init.d/tsunami disable
rm -f /usr/bin/tsunami-client /etc/init.d/tsunami
rm -rf /usr/share/tsunami /etc/config/tsunami
```

## Build one yourself

```sh
bash build/package.sh -v v1.2.3 -t openwrt-aarch64 -o dist
bash build/smoke.sh -t openwrt-aarch64 -v v1.2.3 dist/tsunami-v1.2.3-openwrt-aarch64.tar.gz
```

See [build/README.md](../build/README.md) for the release workflow.
