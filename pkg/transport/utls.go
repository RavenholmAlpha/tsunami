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

// DialUTLSWithALPN performs a uTLS handshake using a browser fingerprint but
// overrides the ALPN extension to advertise only the specified protocols.
// This is needed for WebSocket transport which requires http/1.1 negotiation
// while still presenting a browser-like ClientHello to defeat TLS fingerprinting.
//
// uTLS only honors a custom ClientHelloSpec when the connection is created with
// utls.HelloCustom (ApplyPreset is documented for that mode). Creating the
// connection with the real fingerprint ID first and patching afterwards does
// not work: the handshake rebuilds the fingerprint ClientHello and silently
// discards the patch, which previously made the WebSocket transport offer the
// browser ALPN list (h2, http/1.1) while speaking HTTP/1.1 to the server.
func DialUTLSWithALPN(conn net.Conn, cfg *TLSConfig, alpn []string) (net.Conn, error) {
	fingerprint := cfg.Fingerprint
	if fingerprint == "" {
		fingerprint = FingerprintChrome
	}
	helloID := ResolveFingerprint(fingerprint)
	if helloID == nil {
		return dialStandardTLS(conn, cfg)
	}

	utlsCfg := &utls.Config{
		ServerName:         cfg.ServerName,
		InsecureSkipVerify: cfg.SkipVerify,
		NextProtos:         alpn,
		MinVersion:         cfg.MinVersion,
	}

	// Resolve the fingerprint ClientHelloSpec first, patch its ALPN extension,
	// then apply it through a HelloCustom connection so the patch survives the
	// handshake's ClientHello rebuild.
	spec, err := utls.UTLSIdToSpec(*helloID)
	if err != nil {
		return nil, fmt.Errorf("tsunami: uTLS resolve spec (%s): %w", fingerprint, err)
	}
	patchALPN(&spec, alpn)

	uconn := utls.UClient(conn, utlsCfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		return nil, fmt.Errorf("tsunami: uTLS apply spec: %w", err)
	}

	if err := uconn.Handshake(); err != nil {
		return nil, fmt.Errorf("tsunami: uTLS handshake (%s, alpn=%v): %w", fingerprint, alpn, err)
	}

	return uconn, nil
}

// patchALPN replaces the ALPN extension in a ClientHelloSpec with the given protocols.
func patchALPN(spec *utls.ClientHelloSpec, alpn []string) {
	for i, ext := range spec.Extensions {
		if _, ok := ext.(*utls.ALPNExtension); ok {
			spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: alpn}
			return
		}
	}
	spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: alpn})
}

// dialStandardTLS wraps conn with the standard Go crypto/tls stack.
func dialStandardTLS(conn net.Conn, cfg *TLSConfig) (net.Conn, error) {
	tlsConn := tls.Client(conn, cfg.BuildClientTLSConfig())
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("tsunami: TLS handshake: %w", err)
	}
	return tlsConn, nil
}
