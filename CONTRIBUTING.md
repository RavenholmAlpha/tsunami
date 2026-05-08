# Contributing to TSUNAMI

> **English** | [中文](CONTRIBUTING.zh.md) | [日本語](CONTRIBUTING.ja.md)

Thank you for your interest in contributing to TSUNAMI! This guide will help you get started.

## Development Setup

### Prerequisites

- [Go 1.25+](https://go.dev/dl/)
- Git

### Build

```bash
# Clone
git clone https://github.com/RavenholmAlpha/tsunami.git
cd tsunami

# Build
go build -trimpath -ldflags="-s -w" -o tsunami-server ./cmd/tsunami-server/
go build -trimpath -ldflags="-s -w" -o tsunami-client ./cmd/tsunami-client/

# Or use the Makefile (cross-platform)
cd build && make
```

### Test

```bash
# Unit tests (with race detection)
go test -race ./pkg/...

# Integration tests
go test ./tests/...

# All tests
go test ./...

# Lint
go vet ./...
```

## Project Structure

```
tsunami/
├── cmd/                  CLI entry points
│   ├── tsunami-server/
│   └── tsunami-client/
├── pkg/                  Library packages
│   ├── protocol/         Wire format, frames, auth, sessions, streams
│   ├── padding/          Programmable padding engine
│   ├── mux/              Session pool & multiplexing
│   ├── surge/            Adaptive connection scaling
│   ├── fallback/         Auth failure fallback handler
│   ├── uot/              UDP-over-TCP relay
│   ├── transport/        TLS/TCP config, uTLS fingerprinting
│   ├── proxy/            SOCKS5 & HTTP proxy servers
│   ├── client/           Client-side API
│   ├── server/           Server implementation
│   ├── control/          Control plane (middleware, user store)
│   └── config/           Configuration loading
├── scripts/              Deployment scripts
├── tests/                Integration tests
├── build/                Cross-platform build scripts
└── docs/                 Design documents
```

## Submitting Changes

### Pull Request Process

1. **Fork** the repository
2. **Create a branch** from `main`: `git checkout -b feat/my-feature`
3. **Make your changes** with clear, descriptive commits
4. **Run tests**: `go test -race ./...`
5. **Run vet**: `go vet ./...`
6. **Push** and open a Pull Request

### Commit Messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add UDP relay support
fix: resolve session leak on timeout
docs: update deployment guide
test: add padding scheme edge cases
refactor: simplify frame decoder
```

### Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Keep packages focused and loosely coupled
- Add tests for new functionality
- Preserve existing comments and documentation

## Reporting Issues

- Use [GitHub Issues](https://github.com/RavenholmAlpha/tsunami/issues)
- For **security vulnerabilities**, see [SECURITY.md](SECURITY.md)

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
