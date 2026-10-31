// Package fronting implements an HTTPS/HTTP2/WebSocket camouflage layer.
package fronting

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	// TransportH2 carries TSUNAMI over an HTTP/2 POST request/response stream.
	TransportH2 = "h2"
	// TransportWebSocket carries TSUNAMI over a HTTP/1.1 WebSocket upgrade.
	TransportWebSocket = "websocket"

	// DefaultPath is deliberately ordinary-looking, and can be overridden.
	DefaultPath = "/assets/update"
	// DefaultServerHeader matches Caddy's default HTTP Server header.
	DefaultServerHeader = "Caddy"

	// DefaultUserAgent mimics a recent Chrome browser to blend with normal traffic.
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// H2FlowControlWindow is set to 4 MiB per stream — within the range real
	// Go HTTP/2 servers use, while still supporting high throughput on
	// high-latency links.
	H2FlowControlWindow int32 = 4 << 20
	// H2ClientFlowControlWindow matches Go's net/http2 connection-level default.
	H2ClientFlowControlWindow = 4 << 20
	// H2MaxFrameSize uses 32 KiB — slightly above Go's 16 KiB default but
	// within the range CDN-fronted servers commonly use.
	H2MaxFrameSize uint32 = 32 << 10
	// HTTPFlushThreshold amortizes h2 flushes without delaying control frames.
	HTTPFlushThreshold = 1 << 20
)

// Config holds shared fronting settings.
type Config struct {
	Enabled bool

	// Path is the HTTP route that accepts authenticated TSUNAMI tunnels.
	Path string
	// Host optionally overrides the HTTP Host header on the client.
	Host string
	// Secret optionally separates HTTP-layer fronting auth from the TSUNAMI
	// protocol password. If empty, callers should fall back to the password.
	Secret string
	// Transport selects the client tunnel transport: h2 or websocket.
	Transport string

	// ServerHeader is emitted for all HTTP responses. Default: Caddy.
	ServerHeader string
	// SiteName is used by the built-in decoy website.
	SiteName string
	// DecoyProxy is deprecated: configure Site.Upstream instead. It is still
	// accepted for backward compatibility and migrated into Site by Normalize.
	DecoyProxy string
	// Site configures the site camouflage layer that answers unauthenticated
	// fronting requests (built-in page or Caddy-like reverse proxy).
	Site SiteConfig
}

// Normalize fills default values and normalizes the site camouflage block.
//
// Normalize cannot return an error without breaking existing callers, so site
// configuration errors are recorded and exposed through SiteError (use
// ValidateSite to get them directly).
func (c *Config) Normalize() {
	if c.Path == "" {
		c.Path = DefaultPath
	}
	if !strings.HasPrefix(c.Path, "/") {
		c.Path = "/" + c.Path
	}
	c.Path = path.Clean(c.Path)
	if c.Path == "." {
		c.Path = DefaultPath
	}
	if c.Transport == "" {
		c.Transport = TransportH2
	}
	if c.ServerHeader == "" {
		c.ServerHeader = DefaultServerHeader
	}
	if c.SiteName == "" {
		c.SiteName = "Welcome"
	}
	c.normalizeSite()
}

// SiteMode selects how the fronting layer answers unauthenticated requests.
type SiteMode string

const (
	// SiteModeBuiltin serves the generated zero-dependency decoy page.
	SiteModeBuiltin SiteMode = "builtin"
	// SiteModeLocal reverse proxies to a local HTTP endpoint. This is the
	// "same-IP real site" mode: the cover website runs next to TSUNAMI.
	SiteModeLocal SiteMode = "local"
	// SiteModeRemote reverse proxies to a third-party website. This is the
	// "borrowed content" mode: the cover site is another real site.
	SiteModeRemote SiteMode = "remote"
)

// SiteConfig configures site camouflage for unauthenticated fronting requests.
// It mirrors the semantics of Caddy's reverse_proxy directive.
type SiteConfig struct {
	// Mode selects builtin, local, or remote. Empty infers local for
	// loopback/private/link-local upstreams and remote otherwise.
	Mode SiteMode
	// Upstream is the origin URL (http or https) or a bare host:port.
	// Required for local and remote modes, ignored in builtin mode.
	Upstream string
	// PreserveHost keeps the client's Host header on upstream requests. Nil
	// selects the mode default: true for local, false for remote.
	PreserveHost *bool
	// InsecureSkipVerify disables TLS certificate verification for HTTPS
	// upstreams. Only use it when the upstream certificate cannot be fixed.
	InsecureSkipVerify bool

	upstream *url.URL
	err      error
}

// UpstreamURL returns the parsed site upstream URL, or nil in builtin mode.
func (s SiteConfig) UpstreamURL() *url.URL { return s.upstream }

// PreserveHostEnabled reports whether the client Host header is forwarded to
// the upstream. With no explicit override the mode default applies: local
// preserves the client Host (Caddy's default for same-host upstreams), remote
// rewrites it to the upstream host:port.
func (s SiteConfig) PreserveHostEnabled() bool {
	if s.PreserveHost != nil {
		return *s.PreserveHost
	}
	return s.Mode == SiteModeLocal
}

// PreserveHostEnabled reports the effective preserve_host setting for the
// site camouflage upstream.
func (c Config) PreserveHostEnabled() bool { return c.Site.PreserveHostEnabled() }

// SiteError returns the site camouflage error recorded by the most recent
// Normalize call on this configuration (nil when the site configuration is
// valid).
func (c *Config) SiteError() error { return c.Site.err }

// ValidateSite normalizes a copy of the configuration and reports the site
// camouflage validation error. It is safe to call before Normalize.
func (c *Config) ValidateSite() error {
	copied := *c
	copied.Normalize()
	return copied.Site.err
}

// normalizeSite resolves the deprecated DecoyProxy alias, infers defaults, and
// validates the site camouflage configuration.
func (c *Config) normalizeSite() {
	site := &c.Site
	if legacy := strings.TrimSpace(c.DecoyProxy); legacy != "" && strings.TrimSpace(site.Upstream) == "" {
		// The deprecated decoy_proxy key maps onto the same reverse proxy. The
		// previous implementation used httputil.NewSingleHostReverseProxy,
		// which forwards the client Host unchanged, so pin preserve_host=true
		// when migrating to keep existing configurations behaving identically.
		site.Upstream = legacy
		if site.PreserveHost == nil {
			preserve := true
			site.PreserveHost = &preserve
		}
	}
	site.err = site.normalize()
}

// normalize validates and resolves a site configuration in place.
func (s *SiteConfig) normalize() error {
	mode := SiteMode(strings.ToLower(strings.TrimSpace(string(s.Mode))))
	switch mode {
	case "", SiteModeBuiltin, SiteModeLocal, SiteModeRemote:
	default:
		s.Mode = mode
		s.upstream = nil
		return fmt.Errorf("fronting: unsupported site mode %q (want builtin, local, or remote)", s.Mode)
	}
	s.Mode = mode
	s.Upstream = strings.TrimSpace(s.Upstream)

	if s.Mode == SiteModeBuiltin {
		s.upstream = nil
		return nil
	}
	if s.Upstream == "" {
		if s.Mode == "" {
			s.Mode = SiteModeBuiltin
			s.upstream = nil
			return nil
		}
		s.upstream = nil
		return fmt.Errorf("fronting: site mode %q requires a site upstream", s.Mode)
	}

	upstream, err := parseSiteUpstream(s.Upstream)
	if err != nil {
		s.upstream = nil
		return err
	}
	s.upstream = upstream
	s.Upstream = upstream.String()
	if s.Mode == "" {
		s.Mode = SiteModeRemote
		if isLocalUpstreamHost(upstream.Hostname()) {
			s.Mode = SiteModeLocal
		}
	}
	return nil
}

// parseSiteUpstream validates and normalizes a site upstream. Bare host:port
// values are accepted and get a scheme inferred with Caddy's rule: https when
// the port is 443, otherwise http. Trailing slashes are trimmed so the path
// join behaves like Caddy's.
func parseSiteUpstream(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if !strings.Contains(value, "://") {
		value = inferUpstreamScheme(value) + "://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("fronting: parse site upstream %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("fronting: site upstream %q must use http or https, got %q", raw, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("fronting: site upstream %q has no host", raw)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("fronting: site upstream %q must not contain credentials", raw)
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, fmt.Errorf("fronting: site upstream %q has invalid port %q", raw, port)
		}
	}
	// Fragments are never forwarded to upstreams.
	parsed.Fragment = ""
	parsed.RawFragment = ""
	// Trim the trailing slash so the Caddy-style path join does not emit "//".
	if strings.HasSuffix(parsed.Path, "/") {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawPath = ""
	}
	return parsed, nil
}

// inferUpstreamScheme applies Caddy's scheme rule for bare upstreams: https
// when the port is 443, otherwise http.
func inferUpstreamScheme(value string) string {
	if port, ok := bareUpstreamPort(value); ok && port == "443" {
		return "https"
	}
	return "http"
}

// bareUpstreamPort extracts the port from a scheme-less upstream, ignoring any
// path, query, or fragment.
func bareUpstreamPort(value string) (string, bool) {
	hostport := value
	if i := strings.IndexAny(hostport, "/?#"); i >= 0 {
		hostport = hostport[:i]
	}
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", false
	}
	return port, true
}

// isLocalUpstreamHost reports whether host denotes the local machine (or a
// private/link-local network), which selects local site camouflage.
func isLocalUpstreamHost(host string) bool {
	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if normalized == "" {
		return false
	}
	if normalized == "localhost" || strings.HasSuffix(normalized, ".localhost") {
		return true
	}
	address, err := netip.ParseAddr(normalized)
	if err != nil {
		return false
	}
	address = address.Unmap()
	return address.IsLoopback() || address.IsPrivate() ||
		address.IsLinkLocalUnicast() || address.IsUnspecified()
}

// URL builds the HTTPS fronting endpoint URL.
func (c Config) URL(serverAddr string) string {
	c.Normalize()
	host := serverAddr
	if h, p, err := net.SplitHostPort(serverAddr); err == nil {
		if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
			host = "[" + h + "]:" + p
		}
	}
	u := url.URL{
		Scheme: "https",
		Host:   host,
		Path:   c.Path,
	}
	return u.String()
}

// KeyFromSecret derives the fixed-size HTTP-layer HMAC key.
func KeyFromSecret(secret string) [32]byte {
	return sha256.Sum256([]byte(secret))
}

// CaddyLikeTLSConfig returns TLS settings matching Caddy's standard Go TLS
// shape closely enough for this embedded fronting layer.
func CaddyLikeTLSConfig(certs []tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: certs,
		NextProtos:   []string{"h2", "http/1.1"},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS13,
		CipherSuites: caddyLikeCipherSuites(),
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
		},
	}
}

func caddyLikeCipherSuites() []uint16 {
	return []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	}
}

// ClockSkew is the accepted HTTP-layer auth timestamp window.
const ClockSkew = 2 * time.Minute

// ValidateTransport returns an error for unsupported fronting transports.
func ValidateTransport(transport string) error {
	switch transport {
	case "", TransportH2, TransportWebSocket:
		return nil
	default:
		return fmt.Errorf("fronting: unsupported transport %q", transport)
	}
}

// AuthJitter returns a random duration between 1-5ms to add to the decoy
// response path, making it indistinguishable from the authenticated path's
// time-to-first-byte.
func AuthJitter() time.Duration {
	var b [2]byte
	rand.Read(b[:])
	ms := 1 + int(binary.LittleEndian.Uint16(b[:]))%5
	return time.Duration(ms) * time.Millisecond
}
