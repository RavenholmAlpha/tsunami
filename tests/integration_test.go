// Package integration provides end-to-end tests for the TSUNAMI protocol.
//
// Test architecture:
//
//	Echo Server (TCP)  ←→  TSUNAMI Server  ←→  TSUNAMI Client  ←→  Test Code
//	   :randomPort           :randomPort         (in-process)
package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/client"
	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
)

const testPassword = "test-integration-password"

// --- Test Helpers ---

// generateSelfSignedCert creates an in-memory self-signed TLS certificate.
func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}

// startEchoServer starts a TCP server that echoes all received data back.
// Returns the listener address and a stop function.
func startEchoServer(t *testing.T) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo server listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) // echo
			}(conn)
		}
	}()

	return ln.Addr().String(), func() { ln.Close() }
}

// startTsunamiServer starts a TSUNAMI server with self-signed TLS.
// Returns the server address and a stop function.
func startTsunamiServer(t *testing.T) (string, func()) {
	cert, err := generateSelfSignedCert()
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"},
		MinVersion:   tls.VersionTLS13,
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tsunami server listen: %v", err)
	}

	auth := protocol.NewAuthenticator([]*protocol.UserInfo{
		{Name: "test-user", Password: testPassword},
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleTestConn(conn, auth)
		}
	}()

	return ln.Addr().String(), func() { ln.Close() }
}

// handleTestConn is a minimal TSUNAMI server connection handler for testing.
func handleTestConn(conn net.Conn, auth *protocol.Authenticator) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Read auth
	authReq, err := protocol.DecodeAuthRequest(conn)
	if err != nil {
		return
	}

	user := auth.Authenticate(authReq.Hash)
	if user == nil {
		// Auth failed — just close
		return
	}

	conn.SetReadDeadline(time.Time{})

	// Create session
	session := protocol.NewSession(conn, 0)

	// Send server settings as auth-success confirmation
	serverSettings := &protocol.ServerSettings{
		Version:   protocol.CurrentVersion,
		SurgeMode: protocol.SurgeModeAuto,
	}
	if err := session.SendServerSettings(serverSettings); err != nil {
		return
	}

	session.SetOnStreamOpen(func(stream *protocol.Stream) {
		go handleTestStream(stream)
	})

	session.RunEventLoop()
}

// handleTestStream proxies a stream to its target.
func handleTestStream(stream *protocol.Stream) {
	defer stream.Close()

	// Read target address
	addrBuf := make([]byte, 512)
	n, err := stream.Read(addrBuf)
	if err != nil {
		return
	}

	target, err := decodeSocksAddrTest(addrBuf[:n])
	if err != nil {
		return
	}

	// Connect to target
	targetConn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer targetConn.Close()

	// Bidirectional relay
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(targetConn, stream)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		io.Copy(stream, targetConn)
	}()

	wg.Wait()
}

func decodeSocksAddrTest(data []byte) (string, error) {
	if len(data) < 2 {
		return "", fmt.Errorf("too short")
	}
	atyp := data[0]
	switch atyp {
	case protocol.AtypIPv4:
		if len(data) < 7 {
			return "", fmt.Errorf("too short")
		}
		ip := net.IP(data[1:5])
		port := int(data[5])<<8 | int(data[6])
		return fmt.Sprintf("%s:%d", ip.String(), port), nil
	case protocol.AtypDomain:
		dlen := int(data[1])
		if len(data) < 2+dlen+2 {
			return "", fmt.Errorf("too short")
		}
		domain := string(data[2 : 2+dlen])
		port := int(data[2+dlen])<<8 | int(data[2+dlen+1])
		return fmt.Sprintf("%s:%d", domain, port), nil
	case protocol.AtypIPv6:
		if len(data) < 19 {
			return "", fmt.Errorf("too short")
		}
		ip := net.IP(data[1:17])
		port := int(data[17])<<8 | int(data[18])
		return fmt.Sprintf("[%s]:%d", ip.String(), port), nil
	default:
		return "", fmt.Errorf("unknown atyp: 0x%02x", atyp)
	}
}

// --- Integration Tests ---

// TestEndToEndEcho verifies core protocol flow:
// Client → authenticate → open stream → proxy to echo server → verify data round-trip
func TestEndToEndEcho(t *testing.T) {
	// 1. Start echo server
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	// 2. Start TSUNAMI server
	serverAddr, stopServer := startTsunamiServer(t)
	defer stopServer()

	// 3. Create TSUNAMI client
	cfg := client.Config{
		ServerAddr: serverAddr,
		Password:   testPassword,
		TLS: transport.TLSConfig{
			ServerName: "localhost",
			SkipVerify: true,
			ALPN:       []string{"h2"},
			MinVersion: tls.VersionTLS13,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
	}

	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer c.Close()

	// 4. Open stream to echo server
	stream, err := c.OpenStream(echoAddr)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	// 5. Send test data
	testData := []byte("Hello, TSUNAMI Protocol! 你好，海啸协议！")
	if _, err := stream.Write(testData); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 6. Read echoed data
	buf := make([]byte, len(testData)*2)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	received := string(buf[:n])
	if received != string(testData) {
		t.Errorf("echo mismatch:\n  got:  %q\n  want: %q", received, string(testData))
	}

	stream.Close()
	t.Logf("✅ End-to-end echo test passed (%d bytes round-tripped)", n)
}

// TestMultipleStreams verifies multiplexing: multiple concurrent streams on one session.
func TestMultipleStreams(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	serverAddr, stopServer := startTsunamiServer(t)
	defer stopServer()

	cfg := client.Config{
		ServerAddr: serverAddr,
		Password:   testPassword,
		TLS: transport.TLSConfig{
			ServerName: "localhost",
			SkipVerify: true,
			ALPN:       []string{"h2"},
			MinVersion: tls.VersionTLS13,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
	}

	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer c.Close()

	const numStreams = 5
	var wg sync.WaitGroup
	errors := make(chan error, numStreams)

	for i := 0; i < numStreams; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			stream, err := c.OpenStream(echoAddr)
			if err != nil {
				errors <- fmt.Errorf("stream %d open: %w", idx, err)
				return
			}
			defer stream.Close()

			msg := fmt.Sprintf("stream-%d-data", idx)
			if _, err := stream.Write([]byte(msg)); err != nil {
				errors <- fmt.Errorf("stream %d write: %w", idx, err)
				return
			}

			buf := make([]byte, 256)
			n, err := stream.Read(buf)
			if err != nil {
				errors <- fmt.Errorf("stream %d read: %w", idx, err)
				return
			}

			if string(buf[:n]) != msg {
				errors <- fmt.Errorf("stream %d: got %q, want %q", idx, string(buf[:n]), msg)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}

	t.Logf("✅ Multiple streams test passed (%d concurrent streams)", numStreams)
}

// TestAuthFailure verifies that wrong passwords are rejected.
func TestAuthFailure(t *testing.T) {
	serverAddr, stopServer := startTsunamiServer(t)
	defer stopServer()

	cfg := client.Config{
		ServerAddr: serverAddr,
		Password:   "wrong-password",
		TLS: transport.TLSConfig{
			ServerName: "localhost",
			SkipVerify: true,
			ALPN:       []string{"h2"},
			MinVersion: tls.VersionTLS13,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
	}

	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer c.Close()

	// Try to open a stream — should fail because auth is rejected
	_, err = c.OpenStream("127.0.0.1:9999")
	if err == nil {
		t.Error("expected error with wrong password, got nil")
	} else {
		t.Logf("✅ Auth failure correctly rejected: %v", err)
	}
}

// TestSessionReuse verifies that a session is reused for sequential streams.
func TestSessionReuse(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	serverAddr, stopServer := startTsunamiServer(t)
	defer stopServer()

	cfg := client.Config{
		ServerAddr: serverAddr,
		Password:   testPassword,
		TLS: transport.TLSConfig{
			ServerName: "localhost",
			SkipVerify: true,
			ALPN:       []string{"h2"},
			MinVersion: tls.VersionTLS13,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
	}

	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer c.Close()

	// Open, use, and close 3 streams sequentially
	for i := 0; i < 3; i++ {
		stream, err := c.OpenStream(echoAddr)
		if err != nil {
			t.Fatalf("stream %d open: %v", i, err)
		}

		msg := []byte(fmt.Sprintf("reuse-test-%d", i))
		stream.Write(msg)

		buf := make([]byte, 256)
		n, err := stream.Read(buf)
		if err != nil {
			t.Fatalf("stream %d read: %v", i, err)
		}

		if string(buf[:n]) != string(msg) {
			t.Errorf("stream %d: got %q, want %q", i, string(buf[:n]), string(msg))
		}

		stream.Close()

		// Small delay to let session go idle
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("✅ Session reuse test passed (3 sequential streams)")
}

// TestLargeDataTransfer verifies that large payloads are correctly proxied.
func TestLargeDataTransfer(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	serverAddr, stopServer := startTsunamiServer(t)
	defer stopServer()

	cfg := client.Config{
		ServerAddr: serverAddr,
		Password:   testPassword,
		TLS: transport.TLSConfig{
			ServerName: "localhost",
			SkipVerify: true,
			ALPN:       []string{"h2"},
			MinVersion: tls.VersionTLS13,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
	}

	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer c.Close()

	stream, err := c.OpenStream(echoAddr)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer stream.Close()

	// Send 1MB of data
	dataSize := 1024 * 1024
	sendData := make([]byte, dataSize)
	for i := range sendData {
		sendData[i] = byte(i % 256)
	}

	// Write in a goroutine
	go func() {
		stream.Write(sendData)
	}()

	// Read all echoed data
	received := make([]byte, 0, dataSize)
	buf := make([]byte, 32*1024)
	for len(received) < dataSize {
		n, err := stream.Read(buf)
		if err != nil {
			t.Fatalf("read at %d/%d: %v", len(received), dataSize, err)
		}
		received = append(received, buf[:n]...)
	}

	// Verify data integrity
	for i := 0; i < dataSize; i++ {
		if received[i] != sendData[i] {
			t.Fatalf("data mismatch at byte %d: got 0x%02x, want 0x%02x", i, received[i], sendData[i])
		}
	}

	t.Logf("✅ Large data transfer test passed (%d bytes)", dataSize)
}
