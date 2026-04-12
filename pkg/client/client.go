// Package client provides the TSUNAMI client API.
package client

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/padding"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
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

	pool           *mux.Pool
	surgeCtrl      *surge.Controller
	paddingScheme  *padding.Scheme
	passwordHash   [protocol.AuthHashLen]byte

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
	// TCP dial
	tcpConn, err := net.DialTimeout("tcp", c.config.ServerAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("TCP dial: %w", err)
	}

	// Apply TCP tuning
	if tc, ok := tcpConn.(*net.TCPConn); ok {
		c.config.TCP.ApplyTCPOptions(tc)
	}

	// TLS handshake
	tlsConn := tls.Client(tcpConn, c.config.TLS.BuildClientTLSConfig())
	if err := tlsConn.Handshake(); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}

	// Send authentication
	padding0Size := 30 // default from scheme rule 0
	if segs := c.paddingScheme.GetSegments(0); len(segs) > 0 {
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
	pw := padding.NewWriter(tlsConn, c.paddingScheme)
	session.SetPaddingWriteFn(func(f *protocol.Frame) error {
		return pw.WriteFramesWithPadding([]*protocol.Frame{f})
	})

	// Send client settings
	settings := &protocol.ClientSettings{
		Version:    protocol.CurrentVersion,
		Client:     "tsunami/1.0.0",
		PaddingMD5: c.paddingScheme.MD5(),
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
		return nil, fmt.Errorf("authentication failed: %w", err)
	}
	if frame.Command != protocol.CmdServerSettings {
		session.Close()
		return nil, protocol.ErrAuthFailed
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paddingScheme = scheme
}
