package transport

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// captureClientHello starts a one-shot TLS server that reports the ALPN list
// offered by the client, then completes the handshake and writes a marker byte.
func captureClientHello(t *testing.T, nextProtos []string) (addr string, captured <-chan []string) {
	t.Helper()
	cert, err := GenerateSelfSignedCert()
	if err != nil {
		t.Fatalf("generate self-signed certificate: %v", err)
	}
	helloCh := make(chan []string, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		serverConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   nextProtos,
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				select {
				case helloCh <- hello.SupportedProtos:
				default:
				}
				return nil, nil
			},
		}
		server := tls.Server(conn, serverConfig)
		_ = server.SetDeadline(time.Now().Add(10 * time.Second))
		if err := server.Handshake(); err != nil {
			return
		}
		_, _ = server.Write([]byte("ok"))
	}()
	return listener.Addr().String(), helloCh
}

func dialTLSWithFingerprint(t *testing.T, addr string, alpn []string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cfg := &TLSConfig{
		ServerName:  "localhost",
		SkipVerify:  true,
		Fingerprint: FingerprintChrome,
		MinVersion:  tls.VersionTLS13,
	}
	var tlsConn net.Conn
	if alpn == nil {
		tlsConn, err = DialUTLS(conn, cfg)
	} else {
		tlsConn, err = DialUTLSWithALPN(conn, cfg, alpn)
	}
	if err != nil {
		conn.Close()
		t.Fatalf("tls handshake: %v", err)
	}
	t.Cleanup(func() { tlsConn.Close() })
	return tlsConn
}

func readMarker(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	marker := make([]byte, 2)
	if _, err := io.ReadFull(conn, marker); err != nil {
		t.Fatalf("read handshake marker: %v", err)
	}
}

func TestDialUTLSWithALPNOffersOnlyRequestedProtocols(t *testing.T) {
	addr, captured := captureClientHello(t, []string{"http/1.1"})
	conn := dialTLSWithFingerprint(t, addr, []string{"http/1.1"})
	readMarker(t, conn)

	select {
	case offered := <-captured:
		if len(offered) != 1 || offered[0] != "http/1.1" {
			t.Fatalf("client offered ALPN %v, want [http/1.1]", offered)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not capture a ClientHello")
	}
}

func TestDialUTLSOffersBrowserALPN(t *testing.T) {
	addr, captured := captureClientHello(t, []string{"h2", "http/1.1"})
	conn := dialTLSWithFingerprint(t, addr, nil)
	readMarker(t, conn)

	select {
	case offered := <-captured:
		hasH2 := false
		hasHTTP1 := false
		for _, proto := range offered {
			switch proto {
			case "h2":
				hasH2 = true
			case "http/1.1":
				hasHTTP1 = true
			}
		}
		if !hasH2 || !hasHTTP1 {
			t.Fatalf("browser fingerprint offered ALPN %v, want h2 and http/1.1", offered)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not capture a ClientHello")
	}
}
