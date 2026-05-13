// Package client provides the TSUNAMI client API.
package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/padding"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
	"golang.org/x/net/http2"
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
func (c *Client) OpenStream(target string) (*protocol.Stream, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("tsunami: client is closed")
	}
	c.mu.Unlock()

	// Get session from Surge controller (handles Layer 1/2 logic)
	session, err := c.surgeCtrl.GetSession()
	if err != nil {
		return nil, fmt.Errorf("tsunami: get session: %w", err)
	}

	// Open stream on the session
	stream, err := session.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("tsunami: open stream: %w", err)
	}

	// Send proxy target address as first data
	addrData, err := encodeSocksAddr(target)
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("tsunami: encode address: %w", err)
	}

	if _, err := stream.Write(addrData); err != nil {
		stream.Close()
		return nil, fmt.Errorf("tsunami: send target addr: %w", err)
	}

	return stream, nil
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

// dialSession creates a new TLS connection, authenticates, and returns a Session.
func (c *Client) dialSession() (*protocol.Session, error) {
	if c.config.Fronting.Enabled {
		return c.dialFrontedSession()
	}

	// TCP dial
	tcpConn, err := net.DialTimeout("tcp", c.config.ServerAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("TCP dial: %w", err)
	}

	// Apply TCP tuning
	if tc, ok := tcpConn.(*net.TCPConn); ok {
		c.config.TCP.ApplyTCPOptions(tc)
	}

	// TLS handshake (with uTLS fingerprint mimicry when configured)
	tlsConn, err := transport.DialUTLS(tcpConn, &c.config.TLS)
	if err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
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
	default:
		conn, err = c.dialFrontedH2(cfg)
	}
	if err != nil {
		return nil, err
	}
	return c.startSession(conn)
}

func (c *Client) dialFrontedH2(cfg fronting.Config) (net.Conn, error) {
	endpoint := cfg.URL(c.config.ServerAddr)
	pr, pw := io.Pipe()

	req, err := http.NewRequest(http.MethodPost, endpoint, pr)
	if err != nil {
		pw.Close()
		return nil, err
	}
	applyFrontingHost(req, cfg, c.config)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if err := fronting.SignRequest(req, c.frontingKey(cfg), time.Now()); err != nil {
		pw.Close()
		return nil, err
	}

	h2Transport := &http2.Transport{
		TLSClientConfig:  c.config.TLS.BuildClientTLSConfig(),
		MaxReadFrameSize: fronting.H2MaxFrameSize,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			tcpConn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := tcpConn.(*net.TCPConn); ok {
				c.config.TCP.ApplyTCPOptions(tc)
			}
			tlsCfg := c.config.TLS
			tlsCfg.ALPN = []string{"h2"}
			conn, err := transport.DialUTLS(tcpConn, &tlsCfg)
			if err != nil {
				tcpConn.Close()
				return nil, err
			}
			return conn, nil
		},
	}

	respCh := make(chan fronting.HTTPResponseResult, 1)
	go func() {
		resp, err := h2Transport.RoundTrip(req)
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
	}()

	return fronting.NewPendingHTTPClientConn(respCh, pw, c.config.ServerAddr), nil
}

func (c *Client) dialFrontedWebSocket(cfg fronting.Config) (net.Conn, error) {
	tcpConn, err := net.DialTimeout("tcp", c.config.ServerAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("TCP dial: %w", err)
	}
	if tc, ok := tcpConn.(*net.TCPConn); ok {
		c.config.TCP.ApplyTCPOptions(tc)
	}

	tlsCfg := c.config.TLS
	tlsCfg.ALPN = []string{"http/1.1"}
	// uTLS browser presets may advertise h2 even when this transport needs
	// an HTTP/1.1 Upgrade handshake. Force the standard TLS stack here so the
	// server selects http/1.1 instead of routing the connection to HTTP/2.
	tlsCfg.Fingerprint = transport.FingerprintNone
	tlsConn, err := transport.DialUTLS(tcpConn, &tlsCfg)
	if err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}

	endpointURL, err := url.Parse(cfg.URL(c.config.ServerAddr))
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	host := frontingHost(cfg, c.config)
	conn, err := fronting.ClientWebSocketHandshake(tlsConn, endpointURL, host, c.frontingKey(cfg))
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	return conn, nil
}

func (c *Client) startSession(tlsConn net.Conn) (*protocol.Session, error) {
	// Send authentication
	scheme := c.currentPaddingScheme()
	padding0Size := 30 // default from scheme rule 0
	if segs := scheme.GetSegments(0); len(segs) > 0 {
		padding0Size = padding.RandomInRange(segs[0].MinSize, segs[0].MaxSize)
	}
	padding0 := make([]byte, padding0Size)

	if err := protocol.EncodeAuthRequest(tlsConn, c.passwordHash, padding0); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("send auth: %w", err)
	}

	// Create session
	seq := c.pool.NextSeq()
	session := protocol.NewSession(tlsConn, seq)

	// Wire padding system into session write path
	pw := padding.NewWriter(tlsConn, scheme)
	session.SetPaddingWriteFn(func(frames []*protocol.Frame) error {
		return pw.WriteFramesWithPadding(frames)
	})
	session.SetOnPaddingSchemeUpdate(func(data []byte) error {
		return c.applyPaddingSchemeUpdate(data, pw)
	})

	// Send client settings
	settings := &protocol.ClientSettings{
		Version:    protocol.CurrentVersion,
		Client:     "tsunami/1.0.0",
		PaddingMD5: scheme.MD5(),
	}
	if err := session.SendSettings(settings); err != nil {
		session.Close()
		return nil, fmt.Errorf("send settings: %w", err)
	}

	// Wait for auth confirmation: server sends CmdServerSettings on success.
	// If auth failed, server does fallback/close → we get EOF or non-frame data.
	tlsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	frame, err := protocol.DecodeFrame(tlsConn)
	tlsConn.SetReadDeadline(time.Time{})
	if err != nil {
		session.Close()
		authErr := newAuthReadError(c.config.ServerAddr, err)
		log.Printf("tsunami client: auth failed server=%s reason=%s hint=%q detail=%v",
			authErr.ServerAddr, authErr.Reason, authErr.Hint(), authErr.Err)
		return nil, authErr
	}
	if frame.Command != protocol.CmdServerSettings {
		session.Close()
		authErr := newUnexpectedAuthFrameError(c.config.ServerAddr, frame)
		log.Printf("tsunami client: auth failed server=%s reason=%s hint=%q command=%d stream_id=%d data_len=%d",
			authErr.ServerAddr, authErr.Reason, authErr.Hint(), authErr.Command, authErr.StreamID, authErr.DataLen)
		return nil, authErr
	}

	// Process server settings
	if frame.Data != nil {
		if ss, err := protocol.DecodeServerSettings(frame.Data); err == nil {
			log.Printf("tsunami: server version=%d surge-mode=%s", ss.Version, ss.SurgeMode)
		}
	}

	// Start session event loop in background
	go func() {
		if err := session.RunEventLoop(); err != nil {
			log.Printf("tsunami: session %d event loop ended: %v", seq, err)
		}
	}()

	return session, nil
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

	port := 0
	fmt.Sscanf(portStr, "%d", &port)

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
