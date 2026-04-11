package integration

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/client"
	"github.com/tsunami-protocol/tsunami/pkg/mux"
	tsunamiProxy "github.com/tsunami-protocol/tsunami/pkg/proxy"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"

	netproxy "golang.org/x/net/proxy"
)

// startHTTPTarget starts a simple HTTP server that returns a known response.
func startHTTPTarget(t *testing.T) (string, func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tsunami", "ok")
		fmt.Fprintf(w, "Hello from TSUNAMI target! Method=%s Path=%s", r.Method, r.URL.Path)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http target listen: %v", err)
	}

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	return ln.Addr().String(), func() {
		srv.Close()
		ln.Close()
	}
}


// TestSOCKS5ProxyEcho tests SOCKS5 proxy → TSUNAMI → echo server
func TestSOCKS5ProxyEcho(t *testing.T) {
	// Start echo server
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	// Start TSUNAMI server
	tsAddr, stopServer := startTsunamiServer(t)
	defer stopServer()

	// Create client
	cfg := client.Config{
		ServerAddr: tsAddr,
		Password:   testPassword,
		TLS: transport.TLSConfig{
			ServerName: "localhost",
			SkipVerify: true,
			ALPN:       []string{"h2"},
			MinVersion: tls.VersionTLS13,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer c.Close()

	// Start SOCKS5 proxy
	socks5 := tsunamiProxy.NewSOCKS5Server(c)
	socksLn, _ := net.Listen("tcp", "127.0.0.1:0")
	socksAddr := socksLn.Addr().String()
	socksLn.Close()
	go socks5.ListenAndServe(socksAddr)
	defer socks5.Close()
	time.Sleep(50 * time.Millisecond)

	// Connect through SOCKS5
	dialer, err := netproxy.SOCKS5("tcp", socksAddr, nil, netproxy.Direct)
	if err != nil {
		t.Fatalf("create SOCKS5 dialer: %v", err)
	}

	conn, err := dialer.Dial("tcp", echoAddr)
	if err != nil {
		t.Fatalf("SOCKS5 dial to echo: %v", err)
	}
	defer conn.Close()

	// Send and receive
	testMsg := "Hello via SOCKS5 → TSUNAMI → Echo!"
	conn.Write([]byte(testMsg))

	buf := make([]byte, 256)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}

	got := string(buf[:n])
	if got != testMsg {
		t.Errorf("echo mismatch:\n  got:  %q\n  want: %q", got, testMsg)
	}

	t.Logf("✅ SOCKS5 proxy echo test passed (%d bytes)", n)
}

// TestSOCKS5ProxyHTTP tests SOCKS5 proxy → TSUNAMI → HTTP server
func TestSOCKS5ProxyHTTP(t *testing.T) {
	// Start HTTP target
	httpAddr, stopHTTP := startHTTPTarget(t)
	defer stopHTTP()

	// Start TSUNAMI server
	tsAddr, stopServer := startTsunamiServer(t)
	defer stopServer()

	// Create client
	cfg := client.Config{
		ServerAddr: tsAddr,
		Password:   testPassword,
		TLS: transport.TLSConfig{
			ServerName: "localhost",
			SkipVerify: true,
			ALPN:       []string{"h2"},
			MinVersion: tls.VersionTLS13,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer c.Close()

	// Start SOCKS5 proxy on random port
	socks5 := tsunamiProxy.NewSOCKS5Server(c)
	socksLn, _ := net.Listen("tcp", "127.0.0.1:0")
	socksAddr := socksLn.Addr().String()
	socksLn.Close()
	go socks5.ListenAndServe(socksAddr)
	defer socks5.Close()
	time.Sleep(50 * time.Millisecond)

	// Create HTTP client that goes through SOCKS5
	dialer, err := netproxy.SOCKS5("tcp", socksAddr, nil, netproxy.Direct)
	if err != nil {
		t.Fatalf("create SOCKS5 dialer: %v", err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			Dial: dialer.Dial,
		},
		Timeout: 10 * time.Second,
	}

	// Make HTTP request through the proxy chain
	url := fmt.Sprintf("http://%s/test", httpAddr)
	resp, err := httpClient.Get(url)
	if err != nil {
		t.Fatalf("HTTP GET through SOCKS5: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Verify response
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Tsunami") != "ok" {
		t.Errorf("missing X-Tsunami header")
	}
	if !strings.Contains(bodyStr, "Hello from TSUNAMI target") {
		t.Errorf("unexpected body: %q", bodyStr)
	}

	t.Logf("✅ SOCKS5 HTTP proxy test passed (status=%d, body=%q)", resp.StatusCode, bodyStr)
}

// TestHTTPProxyCONNECT tests HTTP CONNECT proxy → TSUNAMI → echo server
func TestHTTPProxyCONNECT(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	tsAddr, stopServer := startTsunamiServer(t)
	defer stopServer()

	cfg := client.Config{
		ServerAddr: tsAddr,
		Password:   testPassword,
		TLS: transport.TLSConfig{
			ServerName: "localhost",
			SkipVerify: true,
			ALPN:       []string{"h2"},
			MinVersion: tls.VersionTLS13,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer c.Close()

	// Start HTTP proxy
	httpProxy := tsunamiProxy.NewHTTPProxyServer(c)
	proxyLn, _ := net.Listen("tcp", "127.0.0.1:0")
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()
	go httpProxy.ListenAndServe(proxyAddr)
	defer httpProxy.Close()
	time.Sleep(50 * time.Millisecond)

	// Connect to echo server via HTTP CONNECT
	proxyConn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer proxyConn.Close()

	// Send CONNECT request
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	proxyConn.Write([]byte(connectReq))

	// Read 200 response
	buf := make([]byte, 1024)
	proxyConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	response := string(buf[:n])
	if !strings.Contains(response, "200") {
		t.Fatalf("expected 200, got: %q", response)
	}

	// Now the tunnel is established — send echo data
	testMsg := "Hello via HTTP CONNECT → TSUNAMI → Echo!"
	proxyConn.Write([]byte(testMsg))

	proxyConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = proxyConn.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}

	got := string(buf[:n])
	if got != testMsg {
		t.Errorf("echo mismatch:\n  got:  %q\n  want: %q", got, testMsg)
	}

	t.Logf("✅ HTTP CONNECT proxy test passed (%d bytes)", n)
}
