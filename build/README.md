# TSUNAMI Build System

> **English** | [中文](README.zh.md) | [日本語](README.ja.md)

Automated cross-platform build scripts for `tsunami-server` and `tsunami-client`.

## Output Structure

```
build/
├── v1.0.0/                      # versioned release
│   ├── linux-amd64/
│   │   ├── tsunami-server
│   │   └── tsunami-client
│   ├── linux-arm64/
│   │   ├── tsunami-server
│   │   └── tsunami-client
│   ├── windows-amd64/
│   │   ├── tsunami-server.exe
│   │   └── tsunami-client.exe
│   ├── darwin-amd64/
│   │   ├── tsunami-server
│   │   └── tsunami-client
│   ├── darwin-arm64/
│   │   ├── tsunami-server
│   │   └── tsunami-client
│   └── checksums.sha256
├── v1.1.0/                      # another release
│   └── ...
├── build.ps1                    # Windows PowerShell script
├── build.sh                     # Linux/macOS Bash script
├── Makefile                     # Make-based build (Linux/macOS)
└── README.md
```

## Quick Start

### Windows (PowerShell)

```powershell
# Build all platforms, version 1.0.0
.\build.ps1 -Version 1.0.0

# Build specific platforms only
.\build.ps1 -Version 1.0.0 -Platforms "linux-amd64,windows-amd64"

# Clean + rebuild
.\build.ps1 -Version 1.0.0 -Clean
```

### Linux / macOS (Bash)

```bash
chmod +x build.sh

# Build all platforms, version 1.0.0
./build.sh -v 1.0.0

# Build specific platforms
./build.sh -v 1.0.0 -p linux-amd64,linux-arm64

# Clean + rebuild
./build.sh -v 1.0.0 -c
```

### Makefile

```bash
# Build all platforms
make VERSION=1.0.0

# Build Linux targets only
make VERSION=1.0.0 linux

# Build Windows targets only
make VERSION=1.0.0 windows

# Clean
make VERSION=1.0.0 clean
```

## Supported Platforms

| GOOS    | GOARCH | Target          |
|---------|--------|-----------------|
| linux   | amd64  | linux-amd64     |
| linux   | arm64  | linux-arm64     |
| windows | amd64  | windows-amd64   |
| darwin  | amd64  | darwin-amd64    |
| darwin  | arm64  | darwin-arm64    |

## Build Features

- **Version injection**: `main.version`, `main.commit`, `main.buildTime` are embedded via `-ldflags`
- **Stripped binaries**: `-s -w` flags remove debug info for smaller binaries
- **Reproducible builds**: `-trimpath` ensures deterministic output
- **SHA-256 checksums**: Auto-generated `checksums.sha256` for integrity verification
- **CGO disabled**: Pure Go static binaries for maximum portability

## Parameters

### PowerShell (`build.ps1`)

| Parameter       | Default                                                        | Description                 |
|-----------------|----------------------------------------------------------------|-----------------------------|
| `-Version`      | `dev`                                                          | Semantic version string     |
| `-Platforms`    | `linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64` | Comma-separated targets  |
| `-LDFlags`      | `""`                                                           | Extra ldflags               |
| `-Clean`        | `$false`                                                       | Remove output before build  |
| `-SkipChecksum` | `$false`                                                       | Skip checksum generation    |

### Bash (`build.sh`)

| Flag | Default                                                        | Description                 |
|------|----------------------------------------------------------------|-----------------------------|
| `-v` | `dev`                                                          | Semantic version string     |
| `-p` | `linux-amd64,linux-arm64,windows-amd64,darwin-amd64,darwin-arm64` | Comma-separated targets  |
| `-l` | `""`                                                           | Extra ldflags               |
| `-c` | off                                                            | Clean before build          |
| `-s` | off                                                            | Skip checksum generation    |

## Acknowledgments

Protocol design inspired by [anytls-go](https://github.com/anytls/anytls-go).
