// Package server provides the TSUNAMI server implementation.
package server

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/fallback"
	"github.com/tsunami-protocol/tsunami/pkg/padding"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
	"github.com/tsunami-protocol/tsunami/pkg/uot"
)

// Config holds the TSUNAMI server configuration.
type Config struct {
	// Listen address (e.g., ":443")
	Listen string

	// TLS settings
	TLS transport.TLSConfig
	// TCP tuning settings
	TCP transport.TCPConfig

	// Users list
	Users []*protocol.UserInfo

	// Surge settings
	SurgeMode      string // "auto" or "none"
	MaxConnections int
	SurgeThreshold int

	// Fallback backend address (e.g., "127.0.0.1:8080")
	FallbackAddr string

	// Padding scheme text
	PaddingScheme string
}

// Server is the TSUNAMI proxy server.
type Server struct {
	config    Config
	auth      *protocol.Authenticator
	fallback  *fallback.Handler
	scheme    *padding.Scheme
	listener  net.Listener

	mu     sync.Mutex
	closed bool
}

// New creates a new TSUNAMI server.
func New(config Config) (*Server, error) {
	// Parse padding scheme
	var scheme *padding.Scheme
	var err error
	if config.PaddingScheme != "" {
		scheme, err = padding.Parse(config.PaddingScheme)
		if err != nil {
			return nil, fmt.Errorf("tsunami: parse padding scheme: %w", err)
		}
	} else {
		scheme = padding.DefaultScheme()
	}

	// Create authenticator
	auth := protocol.NewAuthenticator(config.Users)

	// Create fallback handler
	var fb *fallback.Handler
	if config.FallbackAddr != "" {
		fb = fallback.NewHandler(config.FallbackAddr)
	}

	// Set defaults
	if config.MaxConnections == 0 {
		config.MaxConnections = 4
	}
	if config.SurgeThreshold == 0 {
		config.SurgeThreshold = 8
	}
	if config.SurgeMode == "" {
		config.SurgeMode = "auto"
	}

	return &Server{
		config:   config,
		auth:     auth,
		fallback: fb,
		scheme:   scheme,
	}, nil
}

// Start starts the TSUNAMI server.
func (s *Server) Start() error {
	// Build TLS config
	tlsCfg, err := s.config.TLS.BuildServerTLSConfig()
	if err != nil {
		return fmt.Errorf("tsunami server: build TLS config: %w", err)
	}

	// Listen on raw TCP so we can access *net.TCPConn for tuning before TLS wrap
	ln, err := net.Listen("tcp", s.config.Listen)
	if err != nil {
		return fmt.Errorf("tsunami server: listen %s: %w", s.config.Listen, err)
	}
	s.listener = ln

	log.Printf("tsunami server: listening on %s", s.config.Listen)

	// Accept loop
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			log.Printf("tsunami server: accept error: %v", err)
			continue
		}

		// Apply TCP tuning on the raw TCPConn BEFORE TLS handshake
		if tc, ok := conn.(*net.TCPConn); ok {
			s.config.TCP.ApplyTCPOptions(tc)
		}

		// Wrap with TLS and handle
		tlsConn := tls.Server(conn, tlsCfg)
		go s.handleTLSConn(tlsConn)
	}
}

// handleTLSConn performs the TLS handshake and delegates to handleConn.
func (s *Server) handleTLSConn(tlsConn *tls.Conn) {
	// TLS handshake with timeout
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("tsunami server: TLS handshake error: %v", err)
		tlsConn.Close()
		return
	}
	tlsConn.SetDeadline(time.Time{})

	s.handleConn(tlsConn)
}

// handleConn handles a single incoming TLS connection.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// Set read deadline for auth phase
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// Capture all bytes read during auth for potential fallback replay
	var authBuf bytes.Buffer
	teeReader := io.TeeReader(conn, &authBuf)

	// Read authentication request
	authReq, err := protocol.DecodeAuthRequest(teeReader)
	if err != nil {
		log.Printf("tsunami server: auth decode error: %v", err)
		s.doFallback(conn, authBuf.Bytes())
		return
	}

	// Authenticate
	user := s.auth.Authenticate(authReq.Hash)
	if user == nil {
		log.Printf("tsunami server: auth failed from %s", conn.RemoteAddr())
		s.doFallback(conn, authBuf.Bytes())
		return
	}

	// Clear read deadline after successful auth
	conn.SetReadDeadline(time.Time{})

	log.Printf("tsunami server: user '%s' authenticated from %s", user.Name, conn.RemoteAddr())

	// Read client settings (first frame after auth, before session event loop)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	settingsFrame, err := protocol.DecodeFrame(conn)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		log.Printf("tsunami server: read client settings: %v", err)
		return
	}
	var clientSettings *protocol.ClientSettings
	if settingsFrame.Command == protocol.CmdSettings && settingsFrame.Data != nil {
		clientSettings, err = protocol.DecodeClientSettings(settingsFrame.Data)
		if err != nil {
			log.Printf("tsunami server: decode client settings: %v", err)
		} else {
			log.Printf("tsunami server: client v=%d client=%s", clientSettings.Version, clientSettings.Client)
		}
	}

	// Create session
	session := protocol.NewSession(conn, 0)

	// Set up stream handler
	session.SetOnStreamOpen(func(stream *protocol.Stream) {
		s.handleStream(stream, user)
	})

	// Wire padding system into session write path
	pw := padding.NewWriter(conn, s.scheme)
	session.SetPaddingWriteFn(func(f *protocol.Frame) error {
		return pw.WriteFramesWithPadding([]*protocol.Frame{f})
	})

	// Send server settings as auth-success confirmation.
	// The client blocks waiting for this frame after sending auth+settings.
	serverSettings := &protocol.ServerSettings{
		Version:        protocol.CurrentVersion,
		SurgeMode:      protocol.SurgeMode(s.config.SurgeMode),
		MaxConnections: s.config.MaxConnections,
		Threshold:      s.config.SurgeThreshold,
	}
	if err := session.SendServerSettings(serverSettings); err != nil {
		log.Printf("tsunami server: send server settings: %v", err)
		return
	}

	// Push padding scheme if client's version differs
	if clientSettings != nil && clientSettings.PaddingFingerprint != "" && clientSettings.PaddingFingerprint != s.scheme.Fingerprint() {
		log.Printf("tsunami server: pushing updated padding scheme to client (client fingerprint=%s, server fingerprint=%s)",
			clientSettings.PaddingFingerprint, s.scheme.Fingerprint())
		schemeData := []byte(s.scheme.Encode())
		if err := session.WriteFrame(protocol.NewFrame(protocol.CmdUpdatePaddingScheme, 0, schemeData)); err != nil {
			log.Printf("tsunami server: send padding scheme: %v", err)
		}
	}

	// Start keepalive generator if configured, and connect stream count tracking
	var kg *padding.KeepaliveGenerator
	if s.scheme.Keepalive != nil {
		kg = padding.NewKeepaliveGenerator(s.scheme.Keepalive, func(f *protocol.Frame) error {
			return session.WriteFrame(f)
		})
		kg.Start()
		defer kg.Stop()
	}

	// Track active/idle state for keepalive
	session.SetOnStreamCountChange(func(activeCount int) {
		if kg != nil {
			kg.SetActive(activeCount > 0)
		}
	})

	// Run session event loop (blocks until closed)
	if err := session.RunEventLoop(); err != nil {
		log.Printf("tsunami server: session error for user '%s': %v", user.Name, err)
	}
}

// handleStream handles a new incoming stream (proxy request).
func (s *Server) handleStream(stream *protocol.Stream, user *protocol.UserInfo) {
	defer stream.Close()

	// Read target address (SOCKS5 format)
	addrBuf := make([]byte, 512)
	n, err := stream.Read(addrBuf)
	if err != nil {
		log.Printf("tsunami server: read target addr: %v", err)
		return
	}

	target, err := decodeSocksAddr(addrBuf[:n])
	if err != nil {
		log.Printf("tsunami server: decode target addr: %v", err)
		return
	}

	// Check for UDP-over-TCP magic address
	if strings.HasPrefix(target, protocol.UoTMagicAddress) {
		log.Printf("tsunami server: UoT relay for user '%s'", user.Name)
		relay := uot.NewRelay(stream)
		if err := relay.Run(); err != nil {
			log.Printf("tsunami server: UoT relay error: %v", err)
		}
		return
	}

	// Connect to target
	targetConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("tsunami server: connect to %s: %v", target, err)
		return
	}
	defer targetConn.Close()

	// Bidirectional relay
	var wg sync.WaitGroup
	wg.Add(2)

	// Stream → Target
	go func() {
		defer wg.Done()
		buf := make([]byte, 256*1024) // 256KB buffer to reduce write syscalls
		io.CopyBuffer(targetConn, stream, buf)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// Target → Stream
	go func() {
		defer wg.Done()
		buf := make([]byte, 256*1024)
		io.CopyBuffer(stream, targetConn, buf)
	}()

	wg.Wait()
}

// doFallback handles failed authentication by forwarding to the fallback backend.
func (s *Server) doFallback(conn net.Conn, preReadData []byte) {
	if s.fallback != nil {
		s.fallback.Handle(conn, preReadData)
	} else {
		fallback.HandleDefault(conn)
	}
}

// Close shuts down the server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// decodeSocksAddr decodes a SOCKS5 format address to "host:port" string.
func decodeSocksAddr(data []byte) (string, error) {
	if len(data) < 2 {
		return "", fmt.Errorf("address too short")
	}

	atyp := data[0]
	switch atyp {
	case protocol.AtypIPv4:
		if len(data) < 1+4+2 {
			return "", fmt.Errorf("IPv4 address too short")
		}
		ip := net.IP(data[1:5])
		port := int(data[5])<<8 | int(data[6])
		return fmt.Sprintf("%s:%d", ip.String(), port), nil

	case protocol.AtypDomain:
		if len(data) < 2 {
			return "", fmt.Errorf("domain address too short")
		}
		domainLen := int(data[1])
		if len(data) < 2+domainLen+2 {
			return "", fmt.Errorf("domain address truncated")
		}
		domain := string(data[2 : 2+domainLen])
		port := int(data[2+domainLen])<<8 | int(data[2+domainLen+1])
		return fmt.Sprintf("%s:%d", domain, port), nil

	case protocol.AtypIPv6:
		if len(data) < 1+16+2 {
			return "", fmt.Errorf("IPv6 address too short")
		}
		ip := net.IP(data[1:17])
		port := int(data[17])<<8 | int(data[18])
		return fmt.Sprintf("[%s]:%d", ip.String(), port), nil

	default:
		return "", fmt.Errorf("unknown ATYP: 0x%02x", atyp)
	}
}
