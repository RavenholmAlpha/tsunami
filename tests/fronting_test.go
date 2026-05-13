package integration

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/client"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
	"golang.org/x/net/http2"
)

func startFrontingTsunamiServer(t *testing.T) (string, func()) {
	t.Helper()
	return startFrontingTsunamiServerWithFronting(t, fronting.Config{
		Enabled:  true,
		Path:     "/assets/update",
		SiteName: "Front Site",
	})
}

func startFrontingTsunamiServerWithFronting(t *testing.T, frontCfg fronting.Config) (string, func()) {
	t.Helper()

	addr := reserveTCPAddr(t)
	srv, err := server.New(server.Config{
		Listen: addr,
		TLS: transport.TLSConfig{
			ALPN:       []string{"h2", "http/1.1"},
			MinVersion: tls.VersionTLS12,
		},
		TCP: *transport.DefaultTCPConfig(),
		Users: []*protocol.UserInfo{
			{Name: "fronting-user", Password: testPassword},
		},
		SurgeMode: string(surge.ModeNone),
		Fronting:  frontCfg,
	})
	if err != nil {
		t.Fatalf("fronting server new: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()
	waitTCP(t, addr)

	return addr, func() {
		srv.Close()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("fronting server stopped with: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Log("fronting server stop timed out")
		}
	}
}

func TestFrontingH2EndToEnd(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	serverAddr, stopServer := startFrontingTsunamiServer(t)
	defer stopServer()

	c, err := client.New(frontingClientConfig(serverAddr, fronting.TransportH2))
	if err != nil {
		t.Fatalf("create fronting client: %v", err)
	}
	defer c.Close()

	stream, err := c.OpenStream(echoAddr)
	if err != nil {
		t.Fatalf("open fronting h2 stream: %v", err)
	}
	defer stream.Close()

	msg := []byte("hello over h2 fronting")
	if _, err := stream.Write(msg); err != nil {
		t.Fatalf("write h2 stream: %v", err)
	}
	buf := make([]byte, 256)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("read h2 stream: %v", err)
	}
	if got := string(buf[:n]); got != string(msg) {
		t.Fatalf("h2 echo = %q, want %q", got, msg)
	}
}

func TestFrontingWebSocketEndToEnd(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	serverAddr, stopServer := startFrontingTsunamiServer(t)
	defer stopServer()

	c, err := client.New(frontingClientConfig(serverAddr, fronting.TransportWebSocket))
	if err != nil {
		t.Fatalf("create websocket fronting client: %v", err)
	}
	defer c.Close()

	stream, err := c.OpenStream(echoAddr)
	if err != nil {
		t.Fatalf("open websocket fronting stream: %v", err)
	}
	defer stream.Close()

	msg := []byte("hello over websocket fronting")
	if _, err := stream.Write(msg); err != nil {
		t.Fatalf("write websocket stream: %v", err)
	}
	buf := make([]byte, 256)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("read websocket stream: %v", err)
	}
	if got := string(buf[:n]); got != string(msg) {
		t.Fatalf("websocket echo = %q, want %q", got, msg)
	}
}

func TestFrontingDecoyLooksLikeHTTPServer(t *testing.T) {
	serverAddr, stopServer := startFrontingTsunamiServer(t)
	defer stopServer()

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         "localhost",
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		},
		ForceAttemptHTTP2: true,
	}
	if err := http2.ConfigureTransport(tr); err != nil {
		t.Fatalf("configure h2 transport: %v", err)
	}
	httpClient := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := httpClient.Get("https://" + serverAddr + "/")
	if err != nil {
		t.Fatalf("decoy GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decoy status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Server") != fronting.DefaultServerHeader {
		t.Fatalf("server header = %q, want %q", resp.Header.Get("Server"), fronting.DefaultServerHeader)
	}
	if resp.ProtoMajor < 2 {
		t.Fatalf("decoy proto = %s, want h2", resp.Proto)
	}
	if !strings.Contains(string(body), "Front Site") {
		t.Fatalf("decoy body missing site name: %q", string(body))
	}

	probeResp, err := httpClient.Get("https://" + serverAddr + "/assets/update")
	if err != nil {
		t.Fatalf("probe GET: %v", err)
	}
	defer probeResp.Body.Close()
	if probeResp.StatusCode == http.StatusSwitchingProtocols || probeResp.StatusCode == http.StatusOK {
		t.Fatalf("unauthenticated probe got status %d", probeResp.StatusCode)
	}
}

func TestFrontingDecoyProxy(t *testing.T) {
	var sawSignatureHeader atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-Signature") != "" {
			sawSignatureHeader.Store(true)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("proxied decoy:" + r.URL.Path))
	}))
	defer origin.Close()

	serverAddr, stopServer := startFrontingTsunamiServerWithFronting(t, fronting.Config{
		Enabled:    true,
		Path:       "/assets/update",
		SiteName:   "Front Site",
		DecoyProxy: origin.URL,
	})
	defer stopServer()

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         "localhost",
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		},
		ForceAttemptHTTP2: true,
	}
	if err := http2.ConfigureTransport(tr); err != nil {
		t.Fatalf("configure h2 transport: %v", err)
	}
	httpClient := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodGet, "https://"+serverAddr+"/probe", nil)
	if err != nil {
		t.Fatalf("new decoy request: %v", err)
	}
	req.Header.Set("X-Request-Signature", "probe-value")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("decoy proxy GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("decoy proxy status = %d, want 202", resp.StatusCode)
	}
	if got := string(body); got != "proxied decoy:/probe" {
		t.Fatalf("decoy proxy body = %q", got)
	}
	if sawSignatureHeader.Load() {
		t.Fatal("fronting auth header leaked to decoy proxy")
	}
}

func frontingClientConfig(serverAddr, tunnelTransport string) client.Config {
	return client.Config{
		ServerAddr: serverAddr,
		Password:   testPassword,
		TLS: transport.TLSConfig{
			ServerName:  "localhost",
			SkipVerify:  true,
			Fingerprint: transport.FingerprintNone,
			ALPN:        []string{"h2", "http/1.1"},
			MinVersion:  tls.VersionTLS12,
		},
		TCP:   *transport.DefaultTCPConfig(),
		Surge: surge.Config{Mode: surge.ModeNone},
		Mux:   mux.DefaultPoolConfig(),
		Fronting: fronting.Config{
			Enabled:   true,
			Path:      "/assets/update",
			Host:      "localhost",
			Transport: tunnelTransport,
		},
	}
}

func reserveTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve tcp addr: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName:         "localhost",
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		})
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not start on %s", addr)
}
