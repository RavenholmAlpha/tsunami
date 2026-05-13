// Package server provides the TSUNAMI server implementation.
package server

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control"
	"github.com/tsunami-protocol/tsunami/pkg/fallback"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/padding"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
	"github.com/tsunami-protocol/tsunami/pkg/uot"
	"golang.org/x/net/http2"
)

// UserAuthenticator authenticates a TSUNAMI auth hash.
type UserAuthenticator interface {
	Authenticate(hash [protocol.AuthHashLen]byte) *protocol.UserInfo
}

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
	// Authenticator allows dynamic control-plane user stores.
	Authenticator UserAuthenticator

	// Surge settings
	SurgeMode      string // "auto" or "none"
	MaxConnections int
	SurgeThreshold int

	// Fallback backend address (e.g., "127.0.0.1:8080")
	FallbackAddr string

	// Fronting enables the built-in Caddy-like HTTPS/HTTP2/WebSocket front.
	Fronting fronting.Config

	// Padding scheme text
	PaddingScheme string

	// Traffic applies per-user accounting and rate limiting.
	Traffic control.TrafficPolicy
}

// Server is the TSUNAMI proxy server.
type Server struct {
	config    Config
	auth      UserAuthenticator
	fallback  *fallback.Handler
	scheme    *padding.Scheme
	listener  net.Listener
	httpSrv   *http.Server
	decoy     *httputil.ReverseProxy
	traffic   control.TrafficPolicy
	frontKeys [][32]byte

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
	auth := config.Authenticator
	if auth == nil {
		userStore, err := control.NewUserStore(config.Users)
		if err != nil {
			return nil, fmt.Errorf("tsunami: create user store: %w", err)
		}
		auth = userStore
	}

	// Create fallback handler
	var fb *fallback.Handler
	if config.FallbackAddr != "" {
		fb = fallback.NewHandler(config.FallbackAddr)
	}

	config.Fronting.Normalize()
	frontKeys, err := buildFrontingKeys(config)
	if err != nil {
		return nil, err
	}
	decoyProxy, err := buildFrontingDecoyProxy(config.Fronting.DecoyProxy, config.Fronting.ServerHeader)
	if err != nil {
		return nil, err
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
	if config.Traffic.Limiter == nil {
		config.Traffic.Limiter = control.NewUserLimiter()
	}

	return &Server{
		config:    config,
		auth:      auth,
		fallback:  fb,
		scheme:    scheme,
		decoy:     decoyProxy,
		traffic:   config.Traffic,
		frontKeys: frontKeys,
	}, nil
}

// Start starts the TSUNAMI server.
func (s *Server) Start() error {
	if s.config.Fronting.Enabled {
		return s.startFronting()
	}

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

func (s *Server) startFronting() error {
	baseTLSCfg, err := s.config.TLS.BuildServerTLSConfig()
	if err != nil {
		return fmt.Errorf("tsunami fronting: build TLS config: %w", err)
	}
	tlsCfg := fronting.CaddyLikeTLSConfig(baseTLSCfg.Certificates)

	ln, err := net.Listen("tcp", s.config.Listen)
	if err != nil {
		return fmt.Errorf("tsunami fronting: listen %s: %w", s.config.Listen, err)
	}
	s.listener = ln

	httpSrv := &http.Server{
		Handler:           http.HandlerFunc(s.handleFrontingHTTP),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: time.Minute,
		IdleTimeout:       5 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	httpSrv.Protocols = new(http.Protocols)
	httpSrv.Protocols.SetHTTP1(true)
	httpSrv.Protocols.SetUnencryptedHTTP2(true)
	h2Server := &http2.Server{
		MaxReadFrameSize:             fronting.H2MaxFrameSize,
		MaxUploadBufferPerConnection: fronting.H2FlowControlWindow,
		MaxUploadBufferPerStream:     fronting.H2FlowControlWindow,
	}
	if err := http2.ConfigureServer(httpSrv, h2Server); err != nil {
		ln.Close()
		return fmt.Errorf("tsunami fronting: configure http2: %w", err)
	}
	s.httpSrv = httpSrv

	log.Printf("tsunami fronting: listening on %s path=%s", s.config.Listen, s.config.Fronting.Path)
	tuned := &tcpTuningListener{Listener: ln, tcp: &s.config.TCP}
	err = httpSrv.Serve(tls.NewListener(tuned, tlsCfg))
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// handleTLSConn performs the TLS handshake and delegates to handleConn.
func (s *Server) handleTLSConn(tlsConn *tls.Conn) {
	// TLS handshake with timeout
	tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("tsunami server: TLS handshake failed remote=%s err=%v", tlsConn.RemoteAddr(), err)
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
		s.doFallback(conn, "auth_decode_error", authBuf.Bytes(), err)
		return
	}

	// Authenticate
	user := s.auth.Authenticate(authReq.Hash)
	if user == nil {
		s.doFallback(conn, "auth_no_matching_user", authBuf.Bytes(), protocol.ErrAuthFailed)
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
	session.SetPaddingWriteFn(func(frames []*protocol.Frame) error {
		return pw.WriteFramesWithPadding(frames)
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
	if clientSettings != nil && clientSettings.PaddingMD5 != "" && clientSettings.PaddingMD5 != s.scheme.MD5() {
		log.Printf("tsunami server: pushing updated padding scheme to client (client md5=%s, server md5=%s)",
			clientSettings.PaddingMD5, s.scheme.MD5())
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
		relayStream := s.traffic.WrapReadWriteCloser(stream, user)
		relay := uot.NewRelay(relayStream)
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
		uploadReader := s.traffic.WrapReader(stream, user, control.DirectionUpload)
		io.CopyBuffer(targetConn, uploadReader, buf)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// Target → Stream
	go func() {
		defer wg.Done()
		buf := make([]byte, 256*1024)
		downloadReader := s.traffic.WrapReader(targetConn, user, control.DirectionDownload)
		io.CopyBuffer(stream, downloadReader, buf)
	}()

	wg.Wait()
}

// doFallback handles unauthenticated connections by forwarding them to the
// fallback backend and logging why the TSUNAMI auth path was not selected.
func (s *Server) doFallback(conn net.Conn, reason string, preReadData []byte, cause error) {
	backend := "default-page"
	if s.fallback != nil {
		backend = s.fallback.BackendAddr
	}

	if cause != nil {
		log.Printf("tsunami server: fallback route remote=%s reason=%s action=fallback backend=%s pre_read=%d cause=%v",
			conn.RemoteAddr(), reason, backend, len(preReadData), cause)
	} else {
		log.Printf("tsunami server: fallback route remote=%s reason=%s action=fallback backend=%s pre_read=%d",
			conn.RemoteAddr(), reason, backend, len(preReadData))
	}

	var err error
	if s.fallback != nil {
		err = s.fallback.Handle(conn, preReadData)
	} else {
		fallback.HandleDefault(conn)
	}
	if err != nil {
		log.Printf("tsunami server: fallback relay failed remote=%s reason=%s backend=%s err=%v",
			conn.RemoteAddr(), reason, backend, err)
		return
	}
	log.Printf("tsunami server: fallback relay finished remote=%s reason=%s backend=%s", conn.RemoteAddr(), reason, backend)
}

func (s *Server) handleFrontingHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Fronting
	cfg.Normalize()

	w.Header().Set("Server", cfg.ServerHeader)

	if r.URL.Path == cfg.Path && fronting.VerifyRequest(r, s.frontKeys, time.Now(), fronting.ClockSkew) {
		switch {
		case r.Method == http.MethodPost:
			s.handleFrontingHTTP2(w, r)
			return
		case r.Method == http.MethodGet && fronting.IsWebSocketUpgrade(r):
			s.handleFrontingWebSocket(w, r)
			return
		}
	}

	s.serveFrontingDecoy(w, r)
}

func (s *Server) handleFrontingHTTP2(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor < 2 {
		s.serveFrontingDecoy(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	conn := fronting.NewHTTPServerConn(w, r)
	s.handleConn(conn)
}

func (s *Server) handleFrontingWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := fronting.UpgradeServer(w, r, s.config.Fronting.ServerHeader)
	if err != nil {
		log.Printf("tsunami fronting: websocket upgrade failed remote=%s err=%v", r.RemoteAddr, err)
		s.serveFrontingDecoy(w, r)
		return
	}
	s.handleConn(conn)
}

func (s *Server) serveFrontingDecoy(w http.ResponseWriter, r *http.Request) {
	if s.decoy != nil {
		s.decoy.ServeHTTP(w, r)
		return
	}

	cfg := s.config.Fronting
	cfg.Normalize()
	w.Header().Set("Server", cfg.ServerHeader)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}

	switch r.URL.Path {
	case "/", "/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		body := fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
</head>
<body>
<main>
<h1>%s</h1>
<p>The site is running.</p>
</main>
</body>
</html>
`, cfg.SiteName, cfg.SiteName)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, body)
	case "/robots.txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, "User-agent: *\nDisallow:\n")
	case "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func buildFrontingDecoyProxy(rawOrigin, serverHeader string) (*httputil.ReverseProxy, error) {
	rawOrigin = strings.TrimSpace(rawOrigin)
	if rawOrigin == "" {
		return nil, nil
	}
	if !strings.Contains(rawOrigin, "://") {
		rawOrigin = "http://" + rawOrigin
	}
	origin, err := url.Parse(rawOrigin)
	if err != nil {
		return nil, fmt.Errorf("tsunami fronting: parse decoy proxy %q: %w", rawOrigin, err)
	}
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return nil, fmt.Errorf("tsunami fronting: decoy proxy must use http or https, got %q", origin.Scheme)
	}
	if origin.Host == "" {
		return nil, fmt.Errorf("tsunami fronting: decoy proxy %q has no host", rawOrigin)
	}

	proxy := httputil.NewSingleHostReverseProxy(origin)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		fronting.StripAuthHeaders(req.Header)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if serverHeader != "" {
			resp.Header.Set("Server", serverHeader)
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("tsunami fronting: decoy proxy error remote=%s origin=%s err=%v", r.RemoteAddr, origin.Redacted(), err)
		if serverHeader != "" {
			w.Header().Set("Server", serverHeader)
		}
		http.NotFound(w, r)
	}
	return proxy, nil
}

func buildFrontingKeys(config Config) ([][32]byte, error) {
	if !config.Fronting.Enabled {
		return nil, nil
	}
	var keys [][32]byte
	seen := make(map[[32]byte]struct{})
	add := func(key [32]byte) {
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	if config.Fronting.Secret != "" {
		add(fronting.KeyFromSecret(config.Fronting.Secret))
	}
	for _, user := range config.Users {
		if user == nil {
			continue
		}
		if user.Password != "" {
			add(fronting.KeyFromSecret(user.Password))
			continue
		}
		if user.TokenHash != "" {
			decoded, err := hex.DecodeString(strings.TrimSpace(user.TokenHash))
			if err != nil {
				return nil, fmt.Errorf("tsunami fronting: decode token hash for user %q: %w", user.Name, err)
			}
			if len(decoded) != protocol.AuthHashLen {
				return nil, fmt.Errorf("tsunami fronting: token hash for user %q has length %d", user.Name, len(decoded))
			}
			var key [32]byte
			copy(key[:], decoded)
			add(key)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("tsunami fronting: fronting_secret or static users are required")
	}
	return keys, nil
}

type tcpTuningListener struct {
	net.Listener
	tcp *transport.TCPConfig
}

func (l *tcpTuningListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok && l.tcp != nil {
		_ = l.tcp.ApplyTCPOptions(tc)
	}
	return conn, nil
}

// Close shuts down the server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
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
