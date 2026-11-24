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
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	h.listener = ln
	h.mu.Unlock()
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
	h.mu.Lock()
	ln := h.listener
	h.mu.Unlock()
	if ln != nil {
		return ln.Addr()
	}
	return nil
}

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func ensureDefaultPort(hostPort, defaultPort string) string {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(hostPort); err == nil {
		return hostPort
	}
	if strings.HasPrefix(hostPort, "[") && strings.HasSuffix(hostPort, "]") {
		return hostPort + ":" + defaultPort
	}
	if strings.Count(hostPort, ":") > 1 {
		return "[" + hostPort + "]:" + defaultPort
	}
	return net.JoinHostPort(hostPort, defaultPort)
}

func stripHopByHopHeaders(hdr http.Header) {
	if hdr == nil {
		return
	}
	for _, connVal := range hdr.Values("Connection") {
		for _, token := range strings.Split(connVal, ",") {
			if name := strings.TrimSpace(token); name != "" {
				hdr.Del(name)
			}
		}
	}
	for _, h := range hopByHopHeaders {
		hdr.Del(h)
	}
}

// handleConn handles a single HTTP proxy client connection, supporting both
// CONNECT tunnels and persistent HTTP/1.1 Keep-Alive plain HTTP requests.
func (h *HTTPProxyServer) handleConn(conn net.Conn) {
	defer conn.Close()

	br := bufio.NewReader(conn)
	for reqIdx := 0; ; reqIdx++ {
		if reqIdx > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		}
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Time{})

		if req.Method == http.MethodConnect {
			h.handleConnect(conn, br, req)
			return
		}
		if !h.handleHTTP(conn, req) {
			return
		}
	}
}

// handleConnect handles HTTPS tunneling via CONNECT method.
func (h *HTTPProxyServer) handleConnect(conn net.Conn, br *bufio.Reader, req *http.Request) {
	targetAddr := ensureDefaultPort(req.Host, "443")
	if targetAddr == "" && req.URL != nil {
		targetAddr = ensureDefaultPort(req.URL.Host, "443")
	}
	if targetAddr == "" {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return
	}

	// Open TSUNAMI stream
	stream, err := h.client.OpenStream(targetAddr)
	if err != nil {
		log.Printf("http proxy: CONNECT to %s: %v", targetAddr, err)
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
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

// handleHTTP forwards a single plain HTTP request over its own TSUNAMI stream
// and reports whether the client connection can be reused for subsequent requests.
func (h *HTTPProxyServer) handleHTTP(conn net.Conn, req *http.Request) bool {
	if req.Body != nil {
		defer req.Body.Close()
	}

	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	targetAddr := ensureDefaultPort(host, "80")
	if targetAddr == "" {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
		return false
	}

	isUpgrade := strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
	clientKeepAlive := !req.Close &&
		req.ProtoAtLeast(1, 1) &&
		!strings.EqualFold(req.Header.Get("Proxy-Connection"), "close")

	stream, err := h.client.OpenStream(targetAddr)
	if err != nil {
		log.Printf("http proxy: HTTP to %s: %v", targetAddr, err)
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
		return false
	}
	defer stream.Close()

	outReq := req.Clone(req.Context())
	outReq.RequestURI = ""
	if outReq.URL != nil {
		outReq.URL.Scheme = ""
		outReq.URL.Host = ""
		if outReq.URL.Path == "" {
			outReq.URL.Path = "/"
		}
	}
	if isUpgrade {
		outReq.Header.Del("Proxy-Connection")
		outReq.Header.Del("Proxy-Authenticate")
		outReq.Header.Del("Proxy-Authorization")
	} else {
		stripHopByHopHeaders(outReq.Header)
		outReq.Close = true
	}

	if err := outReq.Write(stream); err != nil {
		return false
	}

	streamReader := bufio.NewReader(stream)
	resp, err := http.ReadResponse(streamReader, outReq)
	if err != nil {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
		return false
	}
	defer resp.Body.Close()

	if isUpgrade && resp.StatusCode == http.StatusSwitchingProtocols {
		if err := resp.Write(conn); err != nil {
			return false
		}
		var srcStream io.Reader = stream
		if streamReader.Buffered() > 0 {
			srcStream = io.MultiReader(io.LimitReader(streamReader, int64(streamReader.Buffered())), stream)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			bufp := protocol.RelayBufPool.Get().(*[]byte)
			_, _ = io.CopyBuffer(stream, conn, *bufp)
			protocol.RelayBufPool.Put(bufp)
			_ = stream.Close()
		}()
		go func() {
			defer wg.Done()
			bufp := protocol.RelayBufPool.Get().(*[]byte)
			_, _ = io.CopyBuffer(conn, srcStream, *bufp)
			protocol.RelayBufPool.Put(bufp)
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}()
		wg.Wait()
		return false
	}

	stripHopByHopHeaders(resp.Header)
	resp.Close = !clientKeepAlive
	if clientKeepAlive {
		resp.Header.Set("Connection", "keep-alive")
	}
	if err := resp.Write(conn); err != nil {
		return false
	}
	return clientKeepAlive
}
