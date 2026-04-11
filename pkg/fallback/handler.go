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

// DefaultPage returns a minimal static HTML page for fallback
// when no backend is configured.
var DefaultPage = []byte(`HTTP/1.1 200 OK
Content-Type: text/html; charset=utf-8
Content-Length: 75
Connection: close

<html><head><title>Welcome</title></head><body><h1>It works!</h1></body></html>`)

// HandleDefault sends a static default page and drains the connection.
func HandleDefault(conn net.Conn) {
	conn.Write(DefaultPage)
	// Drain remaining data to avoid RST
	io.Copy(io.Discard, conn)
	conn.Close()
}
