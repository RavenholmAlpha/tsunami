// Package client provides the TSUNAMI client API.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/padding"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/shaping"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
	"golang.org/x/net/http2"
)

const (
	tlsHandshakeTimeout = 10 * time.Second
	authWriteTimeout    = 15 * time.Second
)

// Config holds the TSUNAMI client configuration.
type Config struct {
	// Server address (host:port)
	ServerAddr string
	// Password for authentication
	Password string

	// TLS settings
	TLS transport.TLSConfig
	// TCP tuning settings
	TCP transport.TCPConfig

	// Surge settings
	Surge surge.Config

	// Mux settings
	Mux mux.PoolConfig

	// Fronting enables HTTPS/HTTP2/WebSocket application fronting.
	Fronting fronting.Config

	// Shaping enables traffic shaping for anti-traffic-analysis.
	Shaping *shaping.Config

	// Enable UDP-over-TCP
	UDP bool
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		TLS:   *transport.DefaultTLSConfig(),
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.DefaultConfig(),
		Mux:   mux.DefaultPoolConfig(),
		UDP:   true,
	}
}

// Client is the TSUNAMI proxy client.
type Client struct {
	config Config

	pool          *mux.Pool
	surgeCtrl     *surge.Controller
	paddingScheme *padding.Scheme
	passwordHash  [protocol.AuthHashLen]byte

	mu     sync.Mutex
	closed bool
}

// New creates a new TSUNAMI client.
func New(config Config) (*Client, error) {
	if config.ServerAddr == "" {
		return nil, fmt.Errorf("tsunami: server address is required")
	}
	if config.Password == "" {
		return nil, fmt.Errorf("tsunami: password is required")
	}

	c := &Client{
		config:        config,
		paddingScheme: padding.DefaultScheme(),
		passwordHash:  protocol.PasswordHash(config.Password),
	}

	// Create session pool with dial function
	c.pool = mux.NewPool(config.Mux, c.dialSession)

	// Create Surge controller
	c.surgeCtrl = surge.NewController(config.Surge, c.pool)

	return c, nil
}

// OpenStream opens a new proxy stream to the given target address.
// The target should be in "host:port" format.
// If an existing pooled session has silently died (e.g., network switch or
// blackhole), it is evicted and a fresh session is dialed transparently once.
func (c *Client) OpenStream(target string) (*protocol.Stream, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("tsunami: client is closed")
	}
	c.mu.Unlock()

	addrData, err := encodeSocksAddr(target)
	if err != nil {
		return nil, fmt.Errorf("tsunami: encode address: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		session, err := c.surgeCtrl.GetSession()
		if err != nil {
			return nil, fmt.Errorf("tsunami: get session: %w", err)
		}

		stream, err := session.OpenStreamWithTarget(addrData)
		if err != nil {
			if errors.Is(err, protocol.ErrStreamRejected) || errors.Is(err, protocol.ErrTooManyStreams) {
				return nil, fmt.Errorf("tsunami: open stream: %w", err)
			}
			c.pool.RemoveSession(session)
			lastErr = fmt.Errorf("tsunami: open stream: %w", err)
			continue
		}

		return stream, nil
	}

	return nil, lastErr
}

// OpenUDPStream opens a UDP-over-TCP stream.
func (c *Client) OpenUDPStream() (*protocol.Stream, error) {
	return c.OpenStream(protocol.UoTMagicAddress + ":443")
}

// Close shuts down the client and all connections.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	c.surgeCtrl.Stop()
	c.pool.Close()

	return nil
}

// dialUTLSConn dials TCP, applies tuning, bounds the TLS handshake with a
// strict deadline, and clears the deadline once the handshake succeeds.
func (c *Client) dialUTLSConn(ctx context.Context, network, addr string, tlsCfg transport.TLSConfig, alpnOverride []string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: tlsHandshakeTimeout}
	tcpConn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("TCP dial: %w", err)
	}

	if tc, ok := tcpConn.(*net.TCPConn); ok {
		_ = c.config.TCP.ApplyTCPOptions(tc)
	}

	_ = tcpConn.SetDeadline(time.Now().Add(tlsHandshakeTimeout))
	var tlsConn net.Conn
	if len(alpnOverride) > 0 {
		tlsConn, err = transport.DialUTLSWithALPN(tcpConn, &tlsCfg, alpnOverride)
	} else {
		tlsConn, err = transport.DialUTLS(tcpConn, &tlsCfg)
	}
	if err != nil {
		_ = tcpConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	_ = tcpConn.SetDeadline(time.Time{})

	return tlsConn, nil
}

// dialSession creates a new TLS connection, authenticates, and returns a Session.
func (c *Client) dialSession() (*protocol.Session, error) {
	if c.config.Fronting.Enabled {
		return c.dialFrontedSession()
	}

	tlsConn, err := c.dialUTLSConn(context.Background(), "tcp", c.config.ServerAddr, c.config.TLS, nil)
	if err != nil {
		return nil, err
	}

	return c.startSession(tlsConn)
}

func (c *Client) dialFrontedSession() (*protocol.Session, error) {
	cfg := c.config.Fronting
	cfg.Normalize()
	if err := fronting.ValidateTransport(cfg.Transport); err != nil {
		return nil, err
	}

	var conn net.Conn
	var err error
	switch cfg.Transport {
	case fronting.TransportWebSocket:
		conn, err = c.dialFrontedWebSocket(cfg)
	case fronting.TransportH2Stream:
		conn, err = c.dialFrontedH2Stream(cfg)
	case fronting.TransportH2, fronting.TransportH2Split, "":
		conn, err = c.dialFrontedH2Split(cfg)
	default:
		return nil, fmt.Errorf("fronting: unsupported transport %q", cfg.Transport)
	}
	if err != nil {
		return nil, err
	}
	return c.startSession(conn)
}

func (c *Client) dialFrontedH2Split(cfg fronting.Config) (net.Conn, error) {
	endpointURL, err := url.Parse(cfg.URL(c.config.ServerAddr))
	if err != nil {
		return nil, err
	}

	tlsCfg := c.config.TLS
	tlsCfg.ALPN = []string{"h2"}
	tlsConn, err := c.dialUTLSConn(context.Background(), "tcp", c.config.ServerAddr, tlsCfg, nil)
	if err != nil {
		return nil, err
	}

	h2Transport := &http2.Transport{
		TLSClientConfig:    c.config.TLS.BuildClientTLSConfig(),
		DisableCompression: true,
		MaxReadFrameSize:   256 << 10,
		ReadIdleTimeout:    time.Duration(padding.RandomInRange(25000, 45000)) * time.Millisecond,
		PingTimeout:        15 * time.Second,
	}
	cc, err := h2Transport.NewClientConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		h2Transport.CloseIdleConnections()
		return nil, fmt.Errorf("fronting: split h2 client conn: %w", err)
	}

	var closeOnce sync.Once
	closeFn := func() {
		closeOnce.Do(func() {
			_ = cc.Close()
			_ = tlsConn.Close()
			h2Transport.CloseIdleConnections()
		})
	}

	host := frontingHost(cfg, c.config)
	conn, err := fronting.NewSplitH2ClientConn(
		endpointURL,
		host,
		c.frontingKey(cfg),
		c.config.TLS.Fingerprint,
		cc.RoundTrip,
		closeFn,
	)
	if err != nil {
		closeFn()
		return nil, err
	}
	return conn, nil
}

func (c *Client) dialFrontedH2Stream(cfg fronting.Config) (net.Conn, error) {
	endpoint := cfg.URL(c.config.ServerAddr)
	tlsCfg := c.config.TLS
	tlsCfg.ALPN = []string{"h2"}
	tlsConn, err := c.dialUTLSConn(context.Background(), "tcp", c.config.ServerAddr, tlsCfg, nil)
	if err != nil {
		return nil, err
	}

	h2Transport := &http2.Transport{
		TLSClientConfig:    c.config.TLS.BuildClientTLSConfig(),
		DisableCompression: true,
		MaxReadFrameSize:   256 << 10,
		ReadIdleTimeout:    time.Duration(padding.RandomInRange(25000, 45000)) * time.Millisecond,
		PingTimeout:        15 * time.Second,
	}
	cc, err := h2Transport.NewClientConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		h2Transport.CloseIdleConnections()
		return nil, fmt.Errorf("fronting: h2 stream client conn: %w", err)
	}

	var closeOnce sync.Once
	closeFn := func() {
		closeOnce.Do(func() {
			_ = cc.Close()
			_ = tlsConn.Close()
			h2Transport.CloseIdleConnections()
		})
	}

	pr, pw := io.Pipe()

	reqCtx, reqCancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, pr)
	if err != nil {
		reqCancel()
		pw.Close()
		closeFn()
		return nil, err
	}
	applyFrontingHost(req, cfg, c.config)
	fronting.ApplyBrowserHeaders(req.Header, c.config.TLS.Fingerprint)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Cache-Control", "no-store")
	if err := fronting.SignRequest(req, c.frontingKey(cfg), time.Now()); err != nil {
		reqCancel()
		pw.Close()
		closeFn()
		return nil, err
	}

	respCh := make(chan fronting.HTTPResponseResult, 1)
	conn := fronting.NewPendingHTTPClientConnWithCleanup(respCh, pw, c.config.ServerAddr, reqCancel, closeFn)

	go func() {
		resp, err := cc.RoundTrip(req)
		if err != nil {
			respCh <- fronting.HTTPResponseResult{Err: fmt.Errorf("fronting: h2 request: %w", err)}
			return
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			respCh <- fronting.HTTPResponseResult{Err: fmt.Errorf("fronting: h2 status %s", resp.Status)}
			return
		}
		if resp.ProtoMajor < 2 {
			resp.Body.Close()
			respCh <- fronting.HTTPResponseResult{Err: fmt.Errorf("fronting: expected HTTP/2, got %s", resp.Proto)}
			return
		}
		respCh <- fronting.HTTPResponseResult{Response: resp}
		if conn.IsClosed() {
			_ = resp.Body.Close()
		}
	}()

	return conn, nil
}

func (c *Client) dialFrontedWebSocket(cfg fronting.Config) (net.Conn, error) {
	tlsCfg := c.config.TLS
	tlsCfg.ALPN = []string{"http/1.1"}
	// Use a browser-like ClientHello but override ALPN to http/1.1 only,
	// so the server selects HTTP/1.1 for the WebSocket upgrade while the
	// TLS fingerprint still looks like a real browser.
	tlsConn, err := c.dialUTLSConn(context.Background(), "tcp", c.config.ServerAddr, tlsCfg, []string{"http/1.1"})
	if err != nil {
		return nil, err
	}

	endpointURL, err := url.Parse(cfg.URL(c.config.ServerAddr))
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	host := frontingHost(cfg, c.config)
	ua := fronting.UserAgentForFingerprint(c.config.TLS.Fingerprint)
	conn, err := fronting.ClientWebSocketHandshakeWithUserAgent(tlsConn, endpointURL, host, c.frontingKey(cfg), ua)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	return conn, nil
}

// Authentication padding bounds. The auth padding is intentionally decoupled
// from the padding scheme: scheme rule 0 now belongs to the first data write of
// each stream (the SOCKS target address), not to the session handshake.
const (
	authPaddingMin = 24
	authPaddingMax = 88
)

// authPaddingLen returns a random auth padding length in [authPaddingMin,
// authPaddingMax]. Kept well below protocol.MaxFrameDataLen.
func authPaddingLen() int {
	return padding.RandomInRange(authPaddingMin, authPaddingMax)
}

func (c *Client) startSession(tlsConn net.Conn) (*protocol.Session, error) {
	// Send authentication with bounded write timeout.
	scheme := c.currentPaddingScheme()
	padding0 := make([]byte, authPaddingLen())
	if _, err := rand.Read(padding0); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("generate auth padding: %w", err)
	}

	// Create session before writing so AuthRequest + CmdSettings can be sent
	// in a single coalesced write (saves 1 full network RTT on Split-H2 seq=0).
	seq := c.pool.NextSeq()
	session := protocol.NewSession(tlsConn, seq)
	// The client is the stream opener; unlimited local concurrency, while the
	// server still enforces its own per-session limit.
	session.SetMaxStreams(0)

	settings := &protocol.ClientSettings{
		Version:      protocol.CurrentVersion,
		Client:       "http-client/1.0",
		PaddingMD5:   scheme.MD5(),
		Shaping:      c.config.Shaping != nil,
		PaddingV2:    true,
		FlowControl:  true,
		StreamWindow: protocol.DefaultStreamWindow,
		FastOpen:     true,
	}
	settingsPayload := protocol.EncodeClientSettings(settings)
	var handshakeBuf bytes.Buffer
	handshakeBuf.Grow(protocol.AuthOverhead + len(padding0) + protocol.FrameHeaderLen + len(settingsPayload))
	if err := protocol.EncodeAuthRequest(&handshakeBuf, c.passwordHash, padding0); err != nil {
		session.Close()
		return nil, fmt.Errorf("encode auth: %w", err)
	}
	if err := protocol.EncodeFrame(&handshakeBuf, protocol.NewFrame(protocol.CmdSettings, 0, settingsPayload)); err != nil {
		session.Close()
		return nil, fmt.Errorf("encode settings: %w", err)
	}

	_ = tlsConn.SetWriteDeadline(time.Now().Add(authWriteTimeout))
	if _, err := tlsConn.Write(handshakeBuf.Bytes()); err != nil {
		session.Close()
		return nil, fmt.Errorf("send handshake: %w", err)
	}
	_ = tlsConn.SetWriteDeadline(time.Time{})

	// Wait for auth confirmation: server sends CmdServerSettings on success.
	// Tolerate CmdWaste or an early CmdUpdatePaddingScheme for interoperability.
	_ = tlsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var frame *protocol.Frame
	var err error
	for {
		frame, err = protocol.DecodeFrame(tlsConn)
		if err != nil {
			_ = tlsConn.SetReadDeadline(time.Time{})
			session.Close()
			authErr := newAuthReadError(c.config.ServerAddr, err)
			log.Printf("tsunami client: auth failed server=%s reason=%s hint=%q detail=%v",
				authErr.ServerAddr, authErr.Reason, authErr.Hint(), authErr.Err)
			return nil, authErr
		}
		if frame.Command == protocol.CmdWaste {
			continue
		}
		if frame.Command == protocol.CmdUpdatePaddingScheme {
			if updated, parseErr := padding.Parse(string(frame.Data)); parseErr == nil {
				c.UpdatePaddingScheme(updated)
				scheme = updated
			}
			continue
		}
		break
	}
	_ = tlsConn.SetReadDeadline(time.Time{})
	if frame.Command != protocol.CmdServerSettings || frame.StreamID != 0 || len(frame.Data) == 0 {
		session.Close()
		authErr := newUnexpectedAuthFrameError(c.config.ServerAddr, frame)
		log.Printf("tsunami client: auth failed server=%s reason=%s hint=%q command=%d stream_id=%d data_len=%d",
			authErr.ServerAddr, authErr.Reason, authErr.Hint(), authErr.Command, authErr.StreamID, authErr.DataLen)
		return nil, authErr
	}

	// Process server settings
	ss, err := protocol.DecodeServerSettings(frame.Data)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("decode server settings: %w", err)
	}
	if ss.Version != protocol.CurrentVersion {
		session.Close()
		return nil, fmt.Errorf("server version=%d: %w", ss.Version, protocol.ErrVersionMismatch)
	}
	log.Printf("tsunami: server version=%d surge-mode=%s", ss.Version, ss.SurgeMode)
	shapingConfirmed := ss.Shaping

	c.surgeCtrl.ApplyServerSettings(surge.Mode(ss.SurgeMode), ss.MaxConnections, ss.Threshold)
	if ss.FlowControl {
		session.EnableFlowControl(protocol.DefaultStreamWindow, protocol.ClampStreamWindow(ss.StreamWindow))
	}
	if ss.FastOpen {
		session.EnableFastOpen()
	}

	// Activate traffic shaping before binding the session padding writer so no
	// unshaped padding frames leak across the shaping boundary.
	activeConn := tlsConn
	if c.config.Shaping != nil && shapingConfirmed {
		shaped := shaping.Wrap(tlsConn, *c.config.Shaping)
		session.ReplaceConn(shaped)
		activeConn = shaped
		log.Printf("tsunami: traffic shaping enabled (frame=%d interval=%v burst=%d)",
			c.config.Shaping.FrameSize, c.config.Shaping.Interval, c.config.Shaping.BurstSlots)
	}

	// Wire padding system into session write path on top of the active connection.
	pw := padding.NewWriterForRole(activeConn, scheme, padding.RoleClient)
	bindClientPadding(session, pw)
	session.SetOnPaddingSchemeUpdate(func(data []byte) error {
		return c.applyPaddingSchemeUpdate(data, pw)
	})

	var kg *padding.KeepaliveGenerator
	if scheme.Keepalive != nil {
		kg = padding.NewKeepaliveGenerator(scheme.Keepalive, session.WriteFrame)
		kg.Start()
		session.SetOnStreamCountChange(func(activeCount int) {
			kg.SetActive(activeCount > 0)
		})
	}

	// Start session event loop in background
	go func() {
		if kg != nil {
			defer kg.Stop()
		}
		if err := session.RunEventLoop(); err != nil {
			log.Printf("tsunami: session %d event loop ended: %v", seq, err)
		}
	}()

	return session, nil
}

// bindClientPadding wires a padding writer into a session: frame writes go
// through the writer and closed streams drop their per-stream padding state.
// The writer is passed as a parameter (not captured from a variable) so that
// re-wrapping the connection later only rebinds the current writer.
func bindClientPadding(session *protocol.Session, pw *padding.Writer) {
	session.SetPaddingWriteFn(func(frames []*protocol.Frame) error {
		return pw.WriteFramesWithPadding(frames)
	})
	session.SetPaddingPSHWriteFn(pw.WritePSH)
	session.SetPaddingSYNTargetWriteFn(pw.WriteSYNWithTarget)
	session.SetOnStreamClosed(func(id uint32) {
		pw.ForgetStream(id)
	})
}

func (c *Client) frontingKey(cfg fronting.Config) [32]byte {
	secret := cfg.Secret
	if secret == "" {
		secret = c.config.Password
	}
	return fronting.KeyFromSecret(secret)
}

func applyFrontingHost(req *http.Request, cfg fronting.Config, clientCfg Config) {
	host := frontingHost(cfg, clientCfg)
	if host != "" {
		req.Host = host
	}
}

func frontingHost(cfg fronting.Config, clientCfg Config) string {
	if cfg.Host != "" {
		return cfg.Host
	}
	if clientCfg.TLS.ServerName != "" {
		return clientCfg.TLS.ServerName
	}
	host, _, err := net.SplitHostPort(clientCfg.ServerAddr)
	if err != nil {
		return clientCfg.ServerAddr
	}
	return host
}

// encodeSocksAddr encodes a target address in SOCKS5 format.
func encodeSocksAddr(target string) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("invalid target address: %w", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid target port %q", portStr)
	}

	// Try parsing as IP first
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			// IPv4
			buf := make([]byte, 1+4+2)
			buf[0] = protocol.AtypIPv4
			copy(buf[1:5], ip4)
			buf[5] = byte(port >> 8)
			buf[6] = byte(port)
			return buf, nil
		}
		// IPv6
		buf := make([]byte, 1+16+2)
		buf[0] = protocol.AtypIPv6
		copy(buf[1:17], ip.To16())
		buf[17] = byte(port >> 8)
		buf[18] = byte(port)
		return buf, nil
	}

	// Domain name
	if len(host) > 255 {
		return nil, fmt.Errorf("domain too long: %s", host)
	}
	buf := make([]byte, 1+1+len(host)+2)
	buf[0] = protocol.AtypDomain
	buf[1] = byte(len(host))
	copy(buf[2:2+len(host)], host)
	buf[2+len(host)] = byte(port >> 8)
	buf[2+len(host)+1] = byte(port)
	return buf, nil
}

// UpdatePaddingScheme updates the client's padding scheme.
func (c *Client) UpdatePaddingScheme(scheme *padding.Scheme) {
	if scheme == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paddingScheme = scheme
}

func (c *Client) currentPaddingScheme() *padding.Scheme {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paddingScheme == nil {
		c.paddingScheme = padding.DefaultScheme()
	}
	return c.paddingScheme
}

func (c *Client) applyPaddingSchemeUpdate(data []byte, writer *padding.Writer) error {
	scheme, err := padding.Parse(string(data))
	if err != nil {
		return fmt.Errorf("tsunami: parse padding scheme update: %w", err)
	}

	c.UpdatePaddingScheme(scheme)
	if writer != nil {
		writer.UpdateScheme(scheme)
	}
	log.Printf("tsunami: updated padding scheme md5=%s", scheme.MD5())
	return nil
}
