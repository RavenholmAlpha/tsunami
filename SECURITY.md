# Security Policy

## Supported Versions

| Version | Supported          |
|---------|-------------------|
| 1.1.x   | ✅ Active          |
| 1.0.x   | ❌ End of life     |

## Reporting a Vulnerability

If you discover a security vulnerability in TSUNAMI, please report it responsibly.

**Do NOT open a public GitHub issue for security vulnerabilities.**

### How to Report

1. **Email**: Send a detailed report to the project maintainers via GitHub private vulnerability reporting
2. **GitHub**: Use [GitHub's private vulnerability reporting](https://github.com/RavenholmAlpha/tsunami/security/advisories/new)

### What to Include

- Description of the vulnerability
- Steps to reproduce
- Potential impact assessment
- Suggested fix (if any)

### Response Timeline

- **Acknowledgment**: Within 48 hours
- **Initial assessment**: Within 7 days
- **Fix release**: As soon as practically possible, typically within 30 days

### Scope

The following are in scope for security reports:

- Authentication bypass or weakness
- TLS configuration vulnerabilities
- Information disclosure
- Denial of service
- Privilege escalation
- Code injection

### Out of Scope

- Social engineering attacks
- Physical attacks
- Issues in dependencies (report to the dependency maintainer directly)

## Security Best Practices

When deploying TSUNAMI in production:

1. **Use Let's Encrypt** or a valid TLS certificate — avoid `--skip-verify` in production
2. **Use strong passwords** — the install script generates 24-byte random passwords by default
3. **Keep updated** — run `tsunami-manage update` regularly
4. **Firewall rules** — only expose port 443; bind SOCKS5/HTTP proxies to `127.0.0.1`
5. **Monitor logs** — `journalctl -u tsunami-server -f`
