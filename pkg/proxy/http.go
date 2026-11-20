package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/client"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
)

// HTTPProxyServer implements a local HTTP proxy (CONNECT method) that forwards through TSUNAMI.
type HTTPProxyServer struct {
	client   *client.Client
	listener net.Listener
	mu       sync.Mutex
	closed   bool
}

// NewHTTPProxyServer creates a new HTTP proxy server.
func NewHTTPProxyServer(c *client.Client) *HTTPProxyServer {
	return &HTTPProxyServer{client: c}
}

// ListenAndServe starts the HTTP proxy server on the given address.
func (h *HTTPProxyServer) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("http proxy: listen %s: %w", addr, err)
	}
	h.listener = ln
	log.Printf("http proxy: listening on %s", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			h.mu.Lock()
			closed := h.closed
			h.mu.Unlock()
			if closed {
				return nil
			}
			continue
		}
		go h.handleConn(conn)
	}
}

// Close shuts down the HTTP proxy server.
func (h *HTTPProxyServer) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	if h.listener != nil {
		return h.listener.Close()
	}
	return nil
}

// Addr returns the listener address.
func (h *HTTPProxyServer) Addr() net.Addr {
	if h.listener != nil {
		return h.listener.Addr()
	}
	return nil
}

// handleConn handles a single HTTP proxy client connection.
func (h *HTTPProxyServer) handleConn(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReader(conn)

	// Read the HTTP request
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	if req.Method == "CONNECT" {
		h.handleConnect(conn, br, req)
	} else {
		h.handleHTTP(conn, br, req)
	}
}

// handleConnect handles HTTPS tunneling via CONNECT method.
func (h *HTTPProxyServer) handleConnect(conn net.Conn, br *bufio.Reader, req *http.Request) {
	targetAddr := req.Host
	if !strings.Contains(targetAddr, ":") {
		targetAddr += ":443"
	}

	// Open TSUNAMI stream
	stream, err := h.client.OpenStream(targetAddr)
	if err != nil {
		log.Printf("http proxy: CONNECT to %s: %v", targetAddr, err)
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}

	// Send 200 Connection Established
	fmt.Fprintf(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	var src io.Reader = conn
	if br != nil && br.Buffered() > 0 {
		src = io.MultiReader(io.LimitReader(br, int64(br.Buffered())), conn)
	}

	// Bidirectional relay
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		bufp := protocol.RelayBufPool.Get().(*[]byte)
		io.CopyBuffer(stream, src, *bufp)
		protocol.RelayBufPool.Put(bufp)
		stream.Close()
	}()

	go func() {
		defer wg.Done()
		bufp := protocol.RelayBufPool.Get().(*[]byte)
		io.CopyBuffer(conn, stream, *bufp)
		protocol.RelayBufPool.Put(bufp)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}()

	wg.Wait()
}

// handleHTTP handles plain HTTP requests (non-CONNECT).
func (h *HTTPProxyServer) handleHTTP(conn net.Conn, br *bufio.Reader, req *http.Request) {
	targetAddr := req.Host
	if !strings.Contains(targetAddr, ":") {
		targetAddr += ":80"
	}

	// Open TSUNAMI stream
	stream, err := h.client.OpenStream(targetAddr)
	if err != nil {
		log.Printf("http proxy: HTTP to %s: %v", targetAddr, err)
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer stream.Close()

	// Forward the original request
	outReq := req.Clone(req.Context())
	outReq.RequestURI = ""
	if outReq.URL != nil {
		outReq.URL.Scheme = ""
		outReq.URL.Host = ""
	}
	outReq.Write(stream)

	// Relay response back
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		bufp := protocol.RelayBufPool.Get().(*[]byte)
		io.CopyBuffer(stream, br, *bufp)
		protocol.RelayBufPool.Put(bufp)
	}()

	go func() {
		defer wg.Done()
		bufp := protocol.RelayBufPool.Get().(*[]byte)
		io.CopyBuffer(conn, stream, *bufp)
		protocol.RelayBufPool.Put(bufp)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	}()

	wg.Wait()
}
