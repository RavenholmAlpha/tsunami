package transport

import (
	"crypto/tls"
	"fmt"
	"net"

	utls "github.com/refraction-networking/utls"
)

// Supported TLS fingerprint names.
const (
	FingerprintChrome  = "chrome"
	FingerprintFirefox = "firefox"
	FingerprintSafari  = "safari"
	FingerprintRandom  = "random"
	FingerprintNone    = "none"
)

// ResolveFingerprint maps a user-friendly fingerprint name to a uTLS
// ClientHelloID. Returns nil when the caller should fall back to the
// standard crypto/tls stack (i.e. "none" or unrecognised names).
func ResolveFingerprint(name string) *utls.ClientHelloID {
	switch name {
	case FingerprintChrome, "":
		id := utls.HelloChrome_Auto
		return &id
	case FingerprintFirefox:
		id := utls.HelloFirefox_Auto
		return &id
	case FingerprintSafari:
		id := utls.HelloSafari_Auto
		return &id
	case FingerprintRandom:
		id := utls.HelloRandomized
		return &id
	default:
		return nil
	}
}

// DialUTLS performs a TLS handshake on conn using a uTLS fingerprint.
//
// When cfg.Fingerprint is "none", it falls back to standard crypto/tls.
// Otherwise it uses the selected ClientHello fingerprint (default: Chrome).
//
// The returned net.Conn is either a *utls.UConn or a *tls.Conn, both of
// which implement io.ReadWriteCloser and can be used by protocol.Session.
func DialUTLS(conn net.Conn, cfg *TLSConfig) (net.Conn, error) {
	helloID := ResolveFingerprint(cfg.Fingerprint)
	if helloID == nil {
		// "none" or unrecognised → standard crypto/tls
		return dialStandardTLS(conn, cfg)
	}

	utlsCfg := &utls.Config{
		ServerName:         cfg.ServerName,
		InsecureSkipVerify: cfg.SkipVerify,
		NextProtos:         cfg.ALPN,
		MinVersion:         cfg.MinVersion,
	}

	uconn := utls.UClient(conn, utlsCfg, *helloID)
	if err := uconn.Handshake(); err != nil {
		return nil, fmt.Errorf("tsunami: uTLS handshake (%s): %w", cfg.Fingerprint, err)
	}

	return uconn, nil
}

// dialStandardTLS wraps conn with the standard Go crypto/tls stack.
func dialStandardTLS(conn net.Conn, cfg *TLSConfig) (net.Conn, error) {
	tlsConn := tls.Client(conn, cfg.BuildClientTLSConfig())
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("tsunami: TLS handshake: %w", err)
	}
	return tlsConn, nil
}
