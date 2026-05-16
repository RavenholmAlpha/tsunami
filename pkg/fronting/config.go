// Package fronting implements an HTTPS/HTTP2/WebSocket camouflage layer.
package fronting

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"path"
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
	// DecoyProxy is an optional HTTP origin for unauthenticated fronting
	// requests. When empty, the built-in decoy site is served.
	DecoyProxy string
}

// Normalize fills default values.
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
