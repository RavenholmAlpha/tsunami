// Package fallback implements the TSUNAMI fallback mechanism.
// When authentication fails, the connection is transparently proxied to
// a configured HTTP backend, making the server indistinguishable from
// a normal web server to active probers.
package fallback

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Handler manages fallback connections to a backend HTTP server.
type Handler struct {
	// BackendAddr is the address of the fallback backend (e.g., "127.0.0.1:8080").
	BackendAddr string
	// Timeout for connecting to the backend.
	DialTimeout time.Duration
}

// NewHandler creates a new fallback handler.
func NewHandler(backendAddr string) *Handler {
	return &Handler{
		BackendAddr: backendAddr,
		DialTimeout: 5 * time.Second,
	}
}

// Handle proxies the TLS connection to the fallback backend.
// The preReadData contains bytes already read from the connection
// (the failed authentication attempt), which must be forwarded
// to the backend to maintain behavioral consistency.
func (h *Handler) Handle(clientConn net.Conn, preReadData []byte) error {
	// Connect to the fallback backend
	backendConn, err := net.DialTimeout("tcp", h.BackendAddr, h.DialTimeout)
	if err != nil {
		return fmt.Errorf("tsunami fallback: dial backend %s: %w", h.BackendAddr, err)
	}
	defer backendConn.Close()

	// Forward the pre-read data first (including the failed auth bytes)
	if len(preReadData) > 0 {
		if _, err := backendConn.Write(preReadData); err != nil {
			return fmt.Errorf("tsunami fallback: write pre-read data: %w", err)
		}
	}

	// Bidirectional relay
	var wg sync.WaitGroup
	wg.Add(2)

	// Client → Backend
	go func() {
		defer wg.Done()
		io.Copy(backendConn, clientConn)
		// Half-close
		if tc, ok := backendConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// Backend → Client
	go func() {
		defer wg.Done()
		io.Copy(clientConn, backendConn)
		// Half-close
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
	return nil
}

// DefaultPage mimics a Caddy server's default welcome page response.
var DefaultPage = buildDefaultPage()

func buildDefaultPage() []byte {
	body := `<!DOCTYPE html>
<html>
<head>
	<title>Caddy - Welcome</title>
	<style>
	body {
		font-family: system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
		text-align: center;
		padding: 50px;
		background: #fff;
		color: #434343;
	}
	h1 { font-size: 2em; font-weight: 300; }
	p { color: #666; }
	a { color: #0076d1; }
	</style>
</head>
<body>
	<h1>Caddy is working!</h1>
	<p>Congratulations, your Caddy web server is running.</p>
	<p><a href="https://caddyserver.com/docs/">View the Caddy documentation</a></p>
</body>
</html>
`
	date := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	header := fmt.Sprintf("HTTP/1.1 200 OK\r\nServer: Caddy\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\nDate: %s\r\nConnection: close\r\nX-Content-Type-Options: nosniff\r\n\r\n", len(body), date)
	return append([]byte(header), []byte(body)...)
}

// HandleDefault sends a Caddy-like default page and drains the connection.
func HandleDefault(conn net.Conn) {
	// Build a fresh response with current timestamp
	conn.Write(buildDefaultPage())
	// Drain remaining data to avoid RST
	io.Copy(io.Discard, conn)
	conn.Close()
}
