package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/client"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
)

func TestHTTPConnectPreservesPipelinedBufferedBytes(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	srvLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srvAddr := srvLn.Addr().String()
	_ = srvLn.Close()

	srv, err := server.New(server.Config{
		Listen: srvAddr,
		TLS: transport.TLSConfig{
			ALPN:       []string{"h2", "http/1.1"},
			MinVersion: tls.VersionTLS12,
		},
		TCP:       *transport.DefaultTCPConfig(),
		Users:     []*protocol.UserInfo{{Name: "test", Password: "secret-password"}},
		SurgeMode: string(surge.ModeNone),
		Fronting:  fronting.Config{Enabled: true, Path: "/assets/update"},
		AllowAll:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()

	// Wait briefly for server listener.
	for i := 0; i < 40; i++ {
		if c, err := net.DialTimeout("tcp", srvAddr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	cli, err := client.New(client.Config{
		ServerAddr: srvAddr,
		Password:   "secret-password",
		TLS: transport.TLSConfig{
			ServerName:  "localhost",
			SkipVerify:  true,
			Fingerprint: transport.FingerprintNone,
			ALPN:        []string{"h2", "http/1.1"},
			MinVersion:  tls.VersionTLS12,
		},
		TCP:      *transport.DefaultTCPConfig(),
		Surge:    surge.Config{Mode: surge.ModeNone},
		Mux:      mux.DefaultPoolConfig(),
		Fronting: fronting.Config{Enabled: true, Path: "/assets/update", Host: "localhost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	httpProxy := NewHTTPProxyServer(cli)
	go func() { _ = httpProxy.ListenAndServe("127.0.0.1:0") }()
	defer httpProxy.Close()

	var proxyAddr string
	for i := 0; i < 40; i++ {
		if addr := httpProxy.Addr(); addr != nil {
			proxyAddr = addr.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if proxyAddr == "" {
		t.Fatal("HTTP proxy did not start")
	}

	conn, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	target := echoLn.Addr().String()
	// Send CONNECT request + pipelined payload in one write so bufio.Reader buffers it.
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\nPIPELINED-HELLO"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}

	wantResp := "HTTP/1.1 200 Connection Established\r\n\r\nPIPELINED-HELLO"
	got := make([]byte, len(wantResp))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("ReadFull pipelined CONNECT response: %v (got %q)", err, string(got))
	}
	if string(got) != wantResp {
		t.Fatalf("got %q, want %q", string(got), wantResp)
	}
}

func TestHTTPProxyPlainKeepAliveMultipleRequests(t *testing.T) {
	originLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer originLn.Close()

	originSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.RequestURI, "http://") {
				http.Error(w, "unexpected absolute proxy-form RequestURI", http.StatusBadRequest)
				return
			}
			if r.Header.Get("Proxy-Connection") != "" || r.Header.Get("Proxy-Authorization") != "" {
				http.Error(w, "hop-by-hop proxy header leaked to origin", http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "OK:%s:%s:%s", r.Method, r.URL.Path, string(body))
		}),
	}
	go func() { _ = originSrv.Serve(originLn) }()
	defer originSrv.Close()

	srvLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srvAddr := srvLn.Addr().String()
	_ = srvLn.Close()

	srv, err := server.New(server.Config{
		Listen: srvAddr,
		TLS: transport.TLSConfig{
			ALPN:       []string{"h2", "http/1.1"},
			MinVersion: tls.VersionTLS12,
		},
		TCP:       *transport.DefaultTCPConfig(),
		Users:     []*protocol.UserInfo{{Name: "test", Password: "secret-password"}},
		SurgeMode: string(surge.ModeNone),
		Fronting:  fronting.Config{Enabled: true, Path: "/assets/update"},
		AllowAll:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Start() }()
	defer srv.Close()

	for i := 0; i < 40; i++ {
		if c, err := net.DialTimeout("tcp", srvAddr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	cli, err := client.New(client.Config{
		ServerAddr: srvAddr,
		Password:   "secret-password",
		TLS: transport.TLSConfig{
			ServerName:  "localhost",
			SkipVerify:  true,
			Fingerprint: transport.FingerprintNone,
			ALPN:        []string{"h2", "http/1.1"},
			MinVersion:  tls.VersionTLS12,
		},
		TCP:      *transport.DefaultTCPConfig(),
		Surge:    surge.Config{Mode: surge.ModeNone},
		Mux:      mux.DefaultPoolConfig(),
		Fronting: fronting.Config{Enabled: true, Path: "/assets/update", Host: "localhost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	httpProxy := NewHTTPProxyServer(cli)
	go func() { _ = httpProxy.ListenAndServe("127.0.0.1:0") }()
	defer httpProxy.Close()

	var proxyAddr string
	for i := 0; i < 40; i++ {
		if addr := httpProxy.Addr(); addr != nil {
			proxyAddr = addr.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if proxyAddr == "" {
		t.Fatal("HTTP proxy did not start")
	}

	conn, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	originHost := originLn.Addr().String()

	for i := 1; i <= 3; i++ {
		rawReq := fmt.Sprintf(
			"GET http://%s/page-%d HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\nProxy-Authorization: Basic dGVzdDp0ZXN0\r\n\r\n",
			originHost, i, originHost,
		)
		if _, err := io.WriteString(conn, rawReq); err != nil {
			t.Fatalf("req %d write: %v", i, err)
		}
		reqDummy, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/page-%d", originHost, i), nil)
		resp, err := http.ReadResponse(br, reqDummy)
		if err != nil {
			t.Fatalf("req %d ReadResponse: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("req %d read body: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("req %d status=%d body=%q", i, resp.StatusCode, string(body))
		}
		want := fmt.Sprintf("OK:GET:/page-%d:", i)
		if string(body) != want {
			t.Fatalf("req %d body=%q, want %q", i, string(body), want)
		}
	}
}
