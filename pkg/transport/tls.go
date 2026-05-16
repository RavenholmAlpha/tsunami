// Package transport configures TLS and TCP for TSUNAMI connections.
package transport

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"runtime"
	"time"
)

// TLSConfig holds TSUNAMI TLS configuration.
type TLSConfig struct {
	// Server-side
	CertFile string
	KeyFile  string

	// Client-side
	ServerName string
	SkipVerify bool

	// Fingerprint selects the TLS ClientHello fingerprint to mimic.
	// Supported values: "chrome" (default), "firefox", "safari", "random", "none".
	// When set to "none", the standard Go crypto/tls stack is used.
	Fingerprint string

	// Shared
	ALPN       []string
	MinVersion uint16
}

// DefaultTLSConfig returns the default TSUNAMI TLS settings.
func DefaultTLSConfig() *TLSConfig {
	return &TLSConfig{
		ALPN:       []string{"h2"},
		MinVersion: tls.VersionTLS13,
	}
}

// BuildClientTLSConfig creates a *tls.Config for the client.
func (c *TLSConfig) BuildClientTLSConfig() *tls.Config {
	return &tls.Config{
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.SkipVerify,
		NextProtos:         c.ALPN,
		MinVersion:         c.MinVersion,
	}
}

// BuildServerTLSConfig creates a *tls.Config for the server.
// If CertFile and KeyFile are empty, a self-signed certificate is generated automatically.
func (c *TLSConfig) BuildServerTLSConfig() (*tls.Config, error) {
	var cert tls.Certificate

	if c.CertFile == "" && c.KeyFile == "" {
		// Auto-generate self-signed certificate
		log.Println("tsunami: no TLS certificate provided, generating self-signed certificate")
		generated, err := GenerateSelfSignedCert()
		if err != nil {
			return nil, fmt.Errorf("tsunami: auto-generate TLS certificate: %w", err)
		}
		cert = generated
	} else {
		loaded, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("tsunami: load TLS certificate: %w", err)
		}
		cert = loaded
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   c.ALPN,
		MinVersion:   c.MinVersion,
	}, nil
}

// TCPConfig holds TCP tuning parameters.
type TCPConfig struct {
	// SendBufferSize is SO_SNDBUF in bytes.
	SendBufferSize int
	// RecvBufferSize is SO_RCVBUF in bytes.
	RecvBufferSize int
	// NoDelay enables TCP_NODELAY.
	NoDelay bool
	// KeepAlive interval.
	KeepAlive time.Duration
	// ForceBBR attempts to set TCP congestion control to BBR (Linux only).
	ForceBBR bool
}

// DefaultTCPConfig returns the default TCP tuning parameters.
func DefaultTCPConfig() *TCPConfig {
	return &TCPConfig{
		// Leave socket buffers on kernel autotuning by default. Manually
		// setting SO_RCVBUF can clamp the advertised receive window on Linux
		// and cap long-RTT throughput.
		SendBufferSize: 0,
		RecvBufferSize: 0,
		NoDelay:        true,
		KeepAlive:      30 * time.Second,
		ForceBBR:       false,
	}
}

// ApplyTCPOptions applies TCP tuning to a TCP connection.
func (c *TCPConfig) ApplyTCPOptions(conn *net.TCPConn) error {
	if c.NoDelay {
		if err := conn.SetNoDelay(true); err != nil {
			return fmt.Errorf("tsunami: set TCP_NODELAY: %w", err)
		}
	}

	if c.KeepAlive > 0 {
		if err := conn.SetKeepAlive(true); err != nil {
			return fmt.Errorf("tsunami: set keepalive: %w", err)
		}
		if err := conn.SetKeepAlivePeriod(c.KeepAlive); err != nil {
			return fmt.Errorf("tsunami: set keepalive period: %w", err)
		}
	}

	if err := applyTCPBuffers(conn, c); err != nil {
		fmt.Printf("tsunami: TCP buffer tuning warning: %v\n", err)
	}

	// Platform-specific: BBR congestion control (Linux only)
	if runtime.GOOS == "linux" {
		if err := applyLinuxTCPOptions(conn, c); err != nil {
			// Non-fatal: log and continue
			fmt.Printf("tsunami: linux TCP tuning warning: %v\n", err)
		}
	}

	return nil
}

// Dial creates a new TCP connection with tuning applied, then wraps with TLS
// using the configured uTLS fingerprint (default: Chrome).
func Dial(addr string, tlsCfg *TLSConfig, tcpCfg *TCPConfig) (net.Conn, error) {
	tcpConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("tsunami: TCP dial %s: %w", addr, err)
	}

	if tc, ok := tcpConn.(*net.TCPConn); ok {
		if err := tcpCfg.ApplyTCPOptions(tc); err != nil {
			tcpConn.Close()
			return nil, err
		}
	}

	tlsConn, err := DialUTLS(tcpConn, tlsCfg)
	if err != nil {
		tcpConn.Close()
		return nil, err
	}

	return tlsConn, nil
}
