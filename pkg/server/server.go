// Package server provides the TSUNAMI server implementation.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/control"
	"github.com/tsunami-protocol/tsunami/pkg/fallback"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/padding"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/shaping"
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

	// Shaping enables constant-rate traffic shaping when the client requests it.
	Shaping *shaping.Config

	// Padding scheme text
	PaddingScheme string

	// Traffic applies per-user accounting and rate limiting.
	Traffic control.TrafficPolicy

	// AllowAll disables the private-IP blocklist, allowing proxying to any address.
	AllowAll bool

	// SessionAdmission authorizes a connection after protocol authentication but
	// before the server acknowledges it. It is used by panel-managed nodes for
	// device and connection limits.
	SessionAdmission control.SessionAdmission

	// AdmissionTimeout bounds each panel lease request. Zero uses 10 seconds.
	AdmissionTimeout time.Duration
	// SessionRenewInterval controls the lease refresh cadence. Zero uses 90
	// seconds, which is comfortably below XBoard's five-minute lease TTL.
	SessionRenewInterval time.Duration
}

// Server is the TSUNAMI proxy server.
type Server struct {
	config     Config
	auth       UserAuthenticator
	fallback   *fallback.Handler
	scheme     *padding.Scheme
	listener   net.Listener
	httpSrv    *http.Server
	site       http.Handler
	traffic    control.TrafficPolicy
	frontKeys  [][32]byte
	nonceCache *fronting.NonceCache

	mu     sync.Mutex
	closed bool

	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
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
	if err := config.Fronting.SiteError(); err != nil {
		return nil, fmt.Errorf("tsunami fronting: site camouflage: %w", err)
	}
	frontKeys, err := buildFrontingKeys(config)
	if err != nil {
		return nil, err
	}
	siteHandler, err := newFrontingSiteHandler(config.Fronting)
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
	if config.AdmissionTimeout <= 0 {
		config.AdmissionTimeout = 10 * time.Second
	}
	if config.SessionRenewInterval <= 0 {
		config.SessionRenewInterval = 90 * time.Second
	}

	return &Server{
		config:      config,
		auth:        auth,
		fallback:    fb,
		scheme:      scheme,
		site:        siteHandler,
		traffic:     config.Traffic,
		frontKeys:   frontKeys,
		nonceCache:  fronting.NewNonceCache(fronting.ClockSkew + 30*time.Second),
		connections: make(map[net.Conn]struct{}),
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
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listener = ln
	s.mu.Unlock()

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
	httpSrv := &http.Server{
		Handler:           http.HandlerFunc(s.handleFrontingHTTP),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
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
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listener = ln
	s.httpSrv = httpSrv
	s.mu.Unlock()

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
	s.trackConnection(conn)
	defer s.untrackConnection(conn)
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
	if settingsFrame.Command != protocol.CmdSettings || settingsFrame.StreamID != 0 || len(settingsFrame.Data) == 0 {
		log.Printf("tsunami server: invalid client settings frame command=%s stream=%d data_len=%d", settingsFrame.Command, settingsFrame.StreamID, len(settingsFrame.Data))
		return
	}
	clientSettings, err := protocol.DecodeClientSettings(settingsFrame.Data)
	if err != nil {
		log.Printf("tsunami server: decode client settings: %v", err)
		return
	}
	if clientSettings.Version != protocol.CurrentVersion {
		log.Printf("tsunami server: reject client version=%d expected=%d", clientSettings.Version, protocol.CurrentVersion)
		return
	}
	log.Printf("tsunami server: client v=%d client=%s", clientSettings.Version, clientSettings.Client)

	var lease control.SessionLease
	if s.config.SessionAdmission != nil {
		clientIP, err := normalizedRemoteIP(conn.RemoteAddr())
		if err != nil {
			log.Printf("tsunami server: reject user '%s': invalid client address %q: %v", user.Name, conn.RemoteAddr(), err)
			return
		}
		sessionID, err := newSessionID()
		if err != nil {
			log.Printf("tsunami server: create session id for user '%s': %v", user.Name, err)
			return
		}
		admissionCtx, cancel := context.WithTimeout(context.Background(), s.config.AdmissionTimeout)
		lease, err = s.config.SessionAdmission.Admit(admissionCtx, user, clientIP, sessionID)
		cancel()
		if err != nil {
			if errors.Is(err, control.ErrAdmissionDenied) {
				log.Printf("tsunami server: reject user '%s' from %s: device or connection limit reached", user.Name, clientIP)
			} else {
				log.Printf("tsunami server: admission failed for user '%s' from %s: %v", user.Name, clientIP, err)
			}
			return
		}
		if lease == nil {
			log.Printf("tsunami server: admission returned no lease for user '%s'", user.Name)
			return
		}
		defer s.releaseSessionLease(user, lease)
	}

	// Create session
	session := protocol.NewSession(conn, 0)
	if lease != nil {
		stopRenewal := s.startLeaseRenewal(session, user, lease)
		defer stopRenewal()
	}

	// Set up stream handler
	session.SetOnStreamOpen(func(stream *protocol.Stream) {
		s.handleStream(stream, user)
	})

	// Wire padding system into session write path
	pw := padding.NewWriterForRole(conn, s.scheme, padding.RoleServer)
	bindServerPadding(session, pw)

	// Determine if shaping should be activated (both sides must agree)
	shapingEnabled := s.config.Shaping != nil && clientSettings != nil && clientSettings.Shaping

	// Send server settings as auth-success confirmation.
	// The client blocks waiting for this frame after sending auth+settings.
	serverSettings := &protocol.ServerSettings{
		Version:        protocol.CurrentVersion,
		SurgeMode:      protocol.SurgeMode(s.config.SurgeMode),
		MaxConnections: s.config.MaxConnections,
		Threshold:      s.config.SurgeThreshold,
		Shaping:        shapingEnabled,
	}
	if err := session.SendServerSettings(serverSettings); err != nil {
		log.Printf("tsunami server: send server settings: %v", err)
		return
	}

	// Activate traffic shaping after settings exchange
	if shapingEnabled {
		shaped := shaping.Wrap(conn, *s.config.Shaping)
		session.ReplaceConn(shaped)
		pw = padding.NewWriterForRole(shaped, s.scheme, padding.RoleServer)
		bindServerPadding(session, pw)
		log.Printf("tsunami server: traffic shaping enabled for user '%s'", user.Name)
	}

	// Push the padding scheme when the client's version differs. Clients that
	// do not advertise padding-v2 support cannot parse the v2 syntax (`~`
	// pad-to segments, ctl/stream_bytes/c<n>/s<n> keys) and would fail the
	// scheme update and close the session, so they are never pushed it. The
	// tunnel stays functional either way: padding only adds cmdWaste frames,
	// which the peer discards regardless of which scheme it runs.
	if clientSettings != nil && !clientSettings.PaddingV2 && clientSettings.PaddingMD5 != "" && clientSettings.PaddingMD5 != s.scheme.MD5() {
		log.Printf("tsunami server: client predates padding-v2; not pushing scheme (client md5=%s, server md5=%s)",
			clientSettings.PaddingMD5, s.scheme.MD5())
	} else if clientSettings != nil && clientSettings.PaddingMD5 != "" && clientSettings.PaddingMD5 != s.scheme.MD5() {
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

// bindServerPadding wires a padding writer into a session: frame writes go
// through the writer and closed streams drop their per-stream padding state.
// The writer is passed as a parameter (not captured from a variable) so that
// re-wrapping the connection later only rebinds the current writer.
func bindServerPadding(session *protocol.Session, pw *padding.Writer) {
	session.SetPaddingWriteFn(func(frames []*protocol.Frame) error {
		return pw.WriteFramesWithPadding(frames)
	})
	session.SetOnStreamClosed(func(id uint32) {
		pw.ForgetStream(id)
	})
}

func (s *Server) startLeaseRenewal(session *protocol.Session, user *protocol.UserInfo, lease control.SessionLease) func() {
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(s.config.SessionRenewInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), s.config.AdmissionTimeout)
				err := lease.Renew(ctx)
				cancel()
				if err == nil {
					continue
				}
				log.Printf("tsunami server: lease renewal failed for user '%s': %v; closing session", user.Name, err)
				_ = session.Close()
				return
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

func (s *Server) releaseSessionLease(user *protocol.UserInfo, lease control.SessionLease) {
	ctx, cancel := context.WithTimeout(context.Background(), s.config.AdmissionTimeout)
	err := lease.Release(ctx)
	cancel()
	if err != nil {
		log.Printf("tsunami server: lease release failed for user '%s': %v", user.Name, err)
	}
}

func normalizedRemoteIP(remote net.Addr) (string, error) {
	if remote == nil {
		return "", errors.New("missing remote address")
	}
	if tcp, ok := remote.(*net.TCPAddr); ok {
		if tcp.IP == nil {
			return "", errors.New("TCP address has no IP")
		}
		address, ok := netip.AddrFromSlice(tcp.IP)
		if !ok {
			return "", errors.New("TCP address has invalid IP")
		}
		return address.Unmap().String(), nil
	}

	raw := strings.TrimSpace(remote.String())
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	} else if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
	}
	address, err := netip.ParseAddr(raw)
	if err != nil {
		return "", fmt.Errorf("parse IP: %w", err)
	}
	return address.Unmap().String(), nil
}

func newSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Server) trackConnection(conn net.Conn) {
	if conn == nil {
		return
	}
	s.connectionsMu.Lock()
	s.connections[conn] = struct{}{}
	s.connectionsMu.Unlock()
}

func (s *Server) untrackConnection(conn net.Conn) {
	if conn == nil {
		return
	}
	s.connectionsMu.Lock()
	delete(s.connections, conn)
	s.connectionsMu.Unlock()
}

func (s *Server) closeConnections() error {
	s.connectionsMu.Lock()
	connections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.connectionsMu.Unlock()

	var errs []error
	for _, conn := range connections {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// handleStream handles a new incoming stream (proxy request).
func (s *Server) handleStream(stream *protocol.Stream, user *protocol.UserInfo) {
	defer stream.Close()

	// Read the target incrementally so it remains correct across short reads
	// and PSH frame boundaries.
	target, err := protocol.ReadSocksAddr(stream)
	if err != nil {
		log.Printf("tsunami server: decode target addr: %v", err)
		return
	}

	// Check for UDP-over-TCP magic address
	if isUoTTarget(target) {
		log.Printf("tsunami server: UoT relay for user '%s'", user.Name)
		relayStream := s.traffic.WrapReadWriteCloser(stream, user)
		var allowAddr func(string) bool
		if !s.config.AllowAll {
			allowAddr = func(addr string) bool {
				return !isPrivateTarget(addr)
			}
		}
		relay := uot.NewRelay(relayStream, allowAddr)
		if err := relay.Run(); err != nil {
			log.Printf("tsunami server: UoT relay error: %v", err)
		}
		return
	}

	// Check private IP blocklist
	if !s.config.AllowAll && isPrivateTarget(target) {
		log.Printf("tsunami server: blocked connection to private address %s for user '%s'", target, user.Name)
		return
	}

	// Connect to target
	targetConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("tsunami server: connect to %s: %v", target, err)
		return
	}
	defer targetConn.Close()

	if tc, ok := targetConn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	// Bidirectional relay
	var wg sync.WaitGroup
	wg.Add(2)

	// Stream → Target
	go func() {
		defer wg.Done()
		bufp := protocol.RelayBufPool.Get().(*[]byte)
		uploadReader := s.traffic.WrapReader(stream, user, control.DirectionUpload)
		io.CopyBuffer(targetConn, uploadReader, *bufp)
		protocol.RelayBufPool.Put(bufp)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// Target → Stream
	go func() {
		defer wg.Done()
		bufp := protocol.RelayBufPool.Get().(*[]byte)
		downloadReader := s.traffic.WrapReader(targetConn, user, control.DirectionDownload)
		io.CopyBuffer(stream, downloadReader, *bufp)
		protocol.RelayBufPool.Put(bufp)
	}()

	wg.Wait()
}

func isUoTTarget(target string) bool {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host == protocol.UoTMagicAddress || host == protocol.LegacyUoTMagicAddress
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

	w.Header().Set("Server", cfg.ServerHeader)

	// The authenticated tunnel always wins: only requests that fail the
	// fronting HMAC (or use an unsupported method/protocol) reach the site
	// handler below.
	if r.URL.Path == cfg.Path && fronting.VerifyRequest(r, s.frontKeys, time.Now(), fronting.ClockSkew, s.nonceCache) {
		switch {
		case r.Method == http.MethodPost:
			s.handleFrontingHTTP2(w, r)
			return
		case r.Method == http.MethodGet && fronting.IsWebSocketUpgrade(r):
			s.handleFrontingWebSocket(w, r)
			return
		}
	}

	// Add jitter before serving decoy to align timing with the authenticated
	// path, preventing an observer from distinguishing auth success vs failure
	// based on time-to-first-byte.
	time.Sleep(fronting.AuthJitter())

	s.site.ServeHTTP(w, r)
}

func (s *Server) handleFrontingHTTP2(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor < 2 {
		s.site.ServeHTTP(w, r)
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
		s.site.ServeHTTP(w, r)
		return
	}
	s.handleConn(conn)
}

// Fronting site proxy constants. They mirror Caddy's reverse_proxy defaults
// closely enough for a camouflage layer while bounding how long a probe can
// be held open by a broken upstream.
const (
	frontingSiteDialTimeout           = 5 * time.Second
	frontingSiteTLSHandshakeTimeout   = 5 * time.Second
	frontingSiteResponseHeaderTimeout = 10 * time.Second
	frontingSiteIdleConnTimeout       = 90 * time.Second
	frontingSiteMaxIdlePerHost        = 16
	frontingSiteKeepAlive             = 30 * time.Second
)

// frontingSiteHandler answers unauthenticated fronting traffic with either the
// built-in decoy page or a Caddy-like reverse proxy to the configured local or
// remote upstream.
type frontingSiteHandler struct {
	builtin   http.Handler
	proxy     *httputil.ReverseProxy
	transport *http.Transport
}

// ServeHTTP dispatches to the reverse proxy when one is configured.
func (h *frontingSiteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.proxy != nil {
		// The fronting entry point already set the Server header for the
		// tunnel path. httputil.ReverseProxy *copies* upstream headers with
		// Header.Add, so leaving the pre-set value in place would emit two
		// Server header lines on proxied responses (Caddy emits one). The
		// proxy's ModifyResponse sets the single authoritative value.
		w.Header().Del("Server")
		h.proxy.ServeHTTP(w, r)
		return
	}
	h.builtin.ServeHTTP(w, r)
}

// CloseIdleConnections releases idle upstream connections held by the site
// proxy transport.
func (h *frontingSiteHandler) CloseIdleConnections() {
	if h.transport != nil {
		h.transport.CloseIdleConnections()
	}
}

// newFrontingSiteHandler builds the handler for unauthenticated fronting
// requests: the built-in decoy page (builtin mode) or an httputil.ReverseProxy
// with Caddy reverse_proxy semantics for local and remote upstreams.
//
// The upstream is always operator-configured, never derived from the request,
// so the proxy does not introduce an SSRF surface.
func newFrontingSiteHandler(cfg fronting.Config) (http.Handler, error) {
	cfg.Normalize()
	if err := cfg.SiteError(); err != nil {
		return nil, fmt.Errorf("tsunami fronting: site camouflage: %w", err)
	}
	handler := &frontingSiteHandler{}
	handler.builtin = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveBuiltinDecoy(w, r, cfg)
	})
	if cfg.Site.Mode != fronting.SiteModeLocal && cfg.Site.Mode != fronting.SiteModeRemote {
		return handler, nil
	}

	upstream := cfg.Site.UpstreamURL()
	if upstream == nil {
		return nil, fmt.Errorf("tsunami fronting: site mode %q has no upstream", cfg.Site.Mode)
	}
	serverHeader := cfg.ServerHeader
	preserveHost := cfg.Site.PreserveHostEnabled()
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   frontingSiteDialTimeout,
			KeepAlive: frontingSiteKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   frontingSiteMaxIdlePerHost,
		IdleConnTimeout:       frontingSiteIdleConnTimeout,
		TLSHandshakeTimeout:   frontingSiteTLSHandshakeTimeout,
		ResponseHeaderTimeout: frontingSiteResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.Site.InsecureSkipVerify,
			MinVersion:         tls.VersionTLS12,
		},
	}

	handler.transport = transport
	handler.proxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			clientHost := req.Host
			forwardedProto := "http"
			if req.TLS != nil {
				forwardedProto = "https"
			}

			// Caddy sets the forwarding headers itself and ignores inbound
			// values from untrusted peers, so drop them before ReverseProxy
			// appends the real client address to X-Forwarded-For.
			req.Header.Del("X-Forwarded-For")
			joinSiteUpstreamURL(req.URL, upstream)
			if !preserveHost {
				req.Host = upstream.Host
			}
			req.Header.Set("X-Forwarded-Proto", forwardedProto)
			if clientHost != "" {
				req.Header.Set("X-Forwarded-Host", clientHost)
			}
			// Never forward our internal fronting auth metadata.
			fronting.StripAuthHeaders(req.Header)
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if serverHeader != "" {
				resp.Header.Set("Server", serverHeader)
			}
			// We do not serve HTTP/3, so advertising h3 via Alt-Svc would be a
			// fingerprint (and would send probes to a dead endpoint).
			resp.Header.Del("Alt-Svc")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("tsunami fronting: site proxy error remote=%s upstream=%s path=%s err=%v",
				r.RemoteAddr, upstream.Redacted(), r.URL.Path, err)
			// An unreachable upstream must look like the built-in site, never
			// like a proxy or an internal wiring failure.
			time.Sleep(fronting.AuthJitter())
			writeBuiltinDecoyPage(w, r, cfg, http.StatusOK)
		},
	}
	return handler, nil
}

// joinSiteUpstreamURL rewrites reqURL to target, joining paths and queries with
// the same rules as httputil.NewSingleHostReverseProxy. Trailing slashes were
// already trimmed from the upstream during fronting.Config normalization.
func joinSiteUpstreamURL(reqURL, upstream *url.URL) {
	reqURL.Scheme = upstream.Scheme
	reqURL.Host = upstream.Host
	reqURL.Path, reqURL.RawPath = joinURLPath(upstream, reqURL)
	if upstream.RawQuery == "" || reqURL.RawQuery == "" {
		reqURL.RawQuery = upstream.RawQuery + reqURL.RawQuery
	} else {
		reqURL.RawQuery = upstream.RawQuery + "&" + reqURL.RawQuery
	}
}

// joinURLPath mirrors net/http/httputil.joinURLPath.
func joinURLPath(a, b *url.URL) (path, rawpath string) {
	if a.RawPath == "" && b.RawPath == "" {
		return singleJoiningSlash(a.Path, b.Path), ""
	}
	apath := a.EscapedPath()
	bpath := b.EscapedPath()
	aslash := strings.HasSuffix(apath, "/")
	bslash := strings.HasPrefix(bpath, "/")
	switch {
	case aslash && bslash:
		return a.Path + b.Path[1:], apath + bpath[1:]
	case !aslash && !bslash:
		return a.Path + "/" + b.Path, apath + "/" + bpath
	}
	return a.Path + b.Path, apath + bpath
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

// serveBuiltinDecoy serves the generated decoy website for the builtin site
// mode. It is intentionally minimal: a landing page, robots.txt and favicon,
// with ordinary 404s for everything else.
func serveBuiltinDecoy(w http.ResponseWriter, r *http.Request, cfg fronting.Config) {
	w.Header().Set("Server", cfg.ServerHeader)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}

	switch r.URL.Path {
	case "/", "/index.html":
		writeBuiltinDecoyPage(w, r, cfg, http.StatusOK)
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

// writeBuiltinDecoyPage renders the generated decoy landing page with the given
// status code. It is also the site proxy's upstream-failure fallback, so proxy
// failures never surface as 502s or internal errors.
func writeBuiltinDecoyPage(w http.ResponseWriter, r *http.Request, cfg fronting.Config, status int) {
	w.Header().Set("Server", cfg.ServerHeader)
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
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
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
	s.closed = true
	httpSrv := s.httpSrv
	listener := s.listener
	s.mu.Unlock()

	var errs []error
	if httpSrv != nil {
		if err := httpSrv.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	if listener != nil {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if err := s.closeConnections(); err != nil {
		errs = append(errs, err)
	}
	if closer, ok := s.site.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	return errors.Join(errs...)
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
