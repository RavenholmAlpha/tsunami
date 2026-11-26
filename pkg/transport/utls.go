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
// When cfg.UTLSSessionCache is configured, a PreSharedKey extension with
// OmitEmptyPsk=true is attached so the first ClientHello remains byte-identical
// to the browser fingerprint while subsequent connections resume TLS 1.3 via PSK.
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
		ServerName:                         cfg.ServerName,
		InsecureSkipVerify:                 cfg.SkipVerify,
		NextProtos:                         cfg.ALPN,
		MinVersion:                         cfg.MinVersion,
		ClientSessionCache:                 cfg.UTLSSessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}

	var uconn *utls.UConn
	if cfg.UTLSSessionCache != nil && *helloID != utls.HelloRandomized {
		if spec, err := utls.UTLSIdToSpec(*helloID); err == nil {
			ensurePSKExtension(&spec)
			uconn = utls.UClient(conn, utlsCfg, utls.HelloCustom)
			if err := uconn.ApplyPreset(&spec); err != nil {
				return nil, fmt.Errorf("tsunami: uTLS apply spec (%s): %w", cfg.Fingerprint, err)
			}
		}
	}
	if uconn == nil {
		uconn = utls.UClient(conn, utlsCfg, *helloID)
	}
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
		ServerName:                         cfg.ServerName,
		InsecureSkipVerify:                 cfg.SkipVerify,
		NextProtos:                         alpn,
		MinVersion:                         cfg.MinVersion,
		ClientSessionCache:                 cfg.UTLSSessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}

	// Resolve the fingerprint ClientHelloSpec first, patch its ALPN extension,
	// then apply it through a HelloCustom connection so the patch survives the
	// handshake's ClientHello rebuild.
	spec, err := utls.UTLSIdToSpec(*helloID)
	if err != nil {
		return nil, fmt.Errorf("tsunami: uTLS resolve spec (%s): %w", fingerprint, err)
	}
	patchALPN(&spec, alpn)
	if cfg.UTLSSessionCache != nil {
		ensurePSKExtension(&spec)
	}

	uconn := utls.UClient(conn, utlsCfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		return nil, fmt.Errorf("tsunami: uTLS apply spec: %w", err)
	}

	if err := uconn.Handshake(); err != nil {
		return nil, fmt.Errorf("tsunami: uTLS handshake (%s, alpn=%v): %w", fingerprint, alpn, err)
	}

	return uconn, nil
}

// ensurePSKExtension appends a concealed-when-empty TLS 1.3 PSK extension as
// the last extension (RFC 8446 Section 4.2.11) when the spec supports PSK modes
// but omits PreSharedKeyExtension. With OmitEmptyPsk=true, the first ClientHello
// omits the extension entirely (preserving the exact fingerprint), while resumed
// handshakes legitimately include a real TLS 1.3 PSK ticket like real browsers.
func ensurePSKExtension(spec *utls.ClientHelloSpec) {
	hasPSKModes := false
	for _, ext := range spec.Extensions {
		if _, ok := ext.(utls.PreSharedKeyExtension); ok {
			return
		}
		if _, ok := ext.(*utls.PSKKeyExchangeModesExtension); ok {
			hasPSKModes = true
		}
	}
	if hasPSKModes {
		spec.Extensions = append(spec.Extensions, &utls.UtlsPreSharedKeyExtension{OmitEmptyPsk: true})
	}
}

// patchALPN replaces the ALPN extension in a ClientHelloSpec with the given protocols
// and removes HTTP/2-only ALPS extensions when "h2" is not in alpn.
func patchALPN(spec *utls.ClientHelloSpec, alpn []string) {
	hasH2 := false
	for _, p := range alpn {
		if p == "h2" {
			hasH2 = true
			break
		}
	}

	foundALPN := false
	filtered := spec.Extensions[:0]
	for _, ext := range spec.Extensions {
		switch ext.(type) {
		case *utls.ALPNExtension:
			filtered = append(filtered, &utls.ALPNExtension{AlpnProtocols: alpn})
			foundALPN = true
		case *utls.ApplicationSettingsExtension, *utls.ApplicationSettingsExtensionNew:
			if hasH2 {
				filtered = append(filtered, ext)
			}
		default:
			filtered = append(filtered, ext)
		}
	}
	if !foundALPN {
		filtered = append(filtered, &utls.ALPNExtension{AlpnProtocols: alpn})
	}
	spec.Extensions = filtered
}

// dialStandardTLS wraps conn with the standard Go crypto/tls stack.
func dialStandardTLS(conn net.Conn, cfg *TLSConfig) (net.Conn, error) {
	tlsConn := tls.Client(conn, cfg.BuildClientTLSConfig())
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("tsunami: TLS handshake: %w", err)
	}
	return tlsConn, nil
}
