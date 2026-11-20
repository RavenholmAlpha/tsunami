package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/client"
	configfile "github.com/tsunami-protocol/tsunami/pkg/config"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
	"golang.org/x/net/http2"
)

// strictSiteName is the only name on the certificate of the strict SNI
// servers below, the way a Caddy site block owns one hostname.
const strictSiteName = "site.example.test"

// writeSiteCertificate writes a self-signed certificate for dnsName and its
// key as PEM files, standing in for an operator-supplied certificate.
func writeSiteCertificate(t *testing.T, dnsName string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

func dialSite(addr, serverName string, protos ...string) (*tls.Conn, error) {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		ServerName:         serverName, // empty: the ClientHello carries no SNI
		InsecureSkipVerify: true,
		NextProtos:         protos,
	})
}

// startSiteCertFrontingServer starts a fronting server whose certificate is
// loaded from files and only names strictSiteName.
func startSiteCertFrontingServer(t *testing.T, strictSNI *bool) string {
	t.Helper()
	certFile, keyFile := writeSiteCertificate(t, strictSiteName)
	addr := reserveTCPAddr(t)
	srv, err := server.New(server.Config{
		Listen: addr,
		TLS: transport.TLSConfig{
			CertFile:   certFile,
			KeyFile:    keyFile,
			ALPN:       []string{"h2", "http/1.1"},
			MinVersion: tls.VersionTLS12,
		},
		TCP: *transport.DefaultTCPConfig(),
		Users: []*protocol.UserInfo{
			{Name: "fronting-user", Password: testPassword},
		},
		SurgeMode: string(surge.ModeNone),
		Fronting: fronting.Config{
			Enabled:   true,
			Path:      "/assets/update",
			SiteName:  "Front Site",
			StrictSNI: strictSNI,
		},
		AllowAll: true,
	})
	if err != nil {
		t.Fatalf("fronting server new: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Log("fronting server stop timed out")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := dialSite(addr, strictSiteName)
		if err == nil {
			conn.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// siteResponse is what a client saw for one request to the fronting server.
type siteResponse struct {
	status        int
	proto         int
	server        string
	contentLength string
	body          string
}

// getSite sends GET / to addr over a TLS connection that names strictSiteName,
// with the given Host header, over HTTP/2 or HTTP/1.1.
func getSite(t *testing.T, addr, host string, useHTTP2 bool) siteResponse {
	t.Helper()
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         strictSiteName,
			InsecureSkipVerify: true,
		},
	}
	if useHTTP2 {
		tr.ForceAttemptHTTP2 = true
		if err := http2.ConfigureTransport(tr); err != nil {
			t.Fatalf("configure h2 transport: %v", err)
		}
	}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequest(http.MethodGet, "https://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET with Host %q: %v", host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return siteResponse{
		status:        resp.StatusCode,
		proto:         resp.ProtoMajor,
		server:        resp.Header.Get("Server"),
		contentLength: resp.Header.Get("Content-Length"),
		body:          string(body),
	}
}

func TestFrontingStrictSNIHandshake(t *testing.T) {
	addr := startSiteCertFrontingServer(t, nil)

	conn, err := dialSite(addr, strictSiteName, "h2", "http/1.1")
	if err != nil {
		t.Fatalf("handshake with the site name failed: %v", err)
	}
	state := conn.ConnectionState()
	conn.Close()
	if state.NegotiatedProtocol != "h2" {
		t.Fatalf("negotiated protocol = %q, want h2", state.NegotiatedProtocol)
	}

	// A stock Caddy holds no certificate for these and aborts the handshake
	// with internal_error instead of presenting the site certificate.
	for _, serverName := range []string{"", "wrong.invalid", "localhost"} {
		conn, err := dialSite(addr, serverName, "h2", "http/1.1")
		if err == nil {
			names := conn.ConnectionState().PeerCertificates[0].DNSNames
			conn.Close()
			t.Fatalf("handshake with SNI %q completed and exposed the certificate for %v", serverName, names)
		}
		if !strings.Contains(err.Error(), "internal error") {
			t.Fatalf("handshake with SNI %q failed with %v, want the internal_error alert", serverName, err)
		}
	}
}

func TestFrontingStrictSNIUnknownHostGetsEmpty200(t *testing.T) {
	addr := startSiteCertFrontingServer(t, nil)

	for _, useHTTP2 := range []bool{true, false} {
		wantProto := 1
		if useHTTP2 {
			wantProto = 2
		}

		site := getSite(t, addr, strictSiteName, useHTTP2)
		if site.proto != wantProto {
			t.Fatalf("response proto = HTTP/%d, want HTTP/%d", site.proto, wantProto)
		}
		if site.status != http.StatusOK || !strings.Contains(site.body, "Front Site") {
			t.Fatalf("HTTP/%d site request = %d %q, want the site page", wantProto, site.status, site.body)
		}

		for _, host := range []string{"invalid.example", "invalid.example:443", "127.0.0.1"} {
			got := getSite(t, addr, host, useHTTP2)
			if got.status != http.StatusOK || got.body != "" {
				t.Fatalf("HTTP/%d Host %q = %d with body %q, want an empty 200", wantProto, host, got.status, got.body)
			}
			if got.contentLength != "0" {
				t.Fatalf("HTTP/%d Host %q Content-Length = %q, want 0", wantProto, host, got.contentLength)
			}
			if got.server != fronting.DefaultServerHeader {
				t.Fatalf("HTTP/%d Host %q Server = %q, want %q", wantProto, host, got.server, fronting.DefaultServerHeader)
			}
		}
	}
}

// The authenticated tunnel is checked before the Host header, so a client
// that sends another Host (for example behind a CDN) keeps working.
func TestFrontingStrictSNITunnelEndToEnd(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()
	addr := startSiteCertFrontingServer(t, nil)

	cases := []struct {
		name      string
		transport string
		host      string
	}{
		{"h2 with the site name as Host", fronting.TransportH2, strictSiteName},
		{"h2 with another Host", fronting.TransportH2, "cdn.example.test"},
		{"websocket with the site name as Host", fronting.TransportWebSocket, strictSiteName},
		{"websocket with another Host", fronting.TransportWebSocket, "cdn.example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := frontingClientConfig(addr, tc.transport)
			cfg.TLS.ServerName = strictSiteName
			cfg.Fronting.Host = tc.host
			c, err := client.New(cfg)
			if err != nil {
				t.Fatalf("create client: %v", err)
			}
			defer c.Close()

			stream, err := c.OpenStream(echoAddr)
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}
			defer stream.Close()

			msg := []byte("hello through strict SNI")
			if _, err := stream.Write(msg); err != nil {
				t.Fatalf("write stream: %v", err)
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(stream, buf); err != nil {
				t.Fatalf("read stream: %v", err)
			}
			if string(buf) != string(msg) {
				t.Fatalf("echo = %q, want %q", buf, msg)
			}
		})
	}
}

func TestFrontingStrictSNIDisabled(t *testing.T) {
	off := false
	addr := startSiteCertFrontingServer(t, &off)

	for _, serverName := range []string{"", "wrong.invalid"} {
		conn, err := dialSite(addr, serverName)
		if err != nil {
			t.Fatalf("handshake with SNI %q failed with strict SNI off: %v", serverName, err)
		}
		conn.Close()
	}
	got := getSite(t, addr, "invalid.example", true)
	if got.status != http.StatusOK || !strings.Contains(got.body, "Front Site") {
		t.Fatalf("Host invalid.example = %d %q, want the site page with strict SNI off", got.status, got.body)
	}
}

// The generated self-signed certificate only names localhost, so strict SNI
// must not lock out the clients of a test deployment.
func TestFrontingStrictSNISkipsGeneratedCertificate(t *testing.T) {
	addr, stop := startFrontingTsunamiServer(t)
	defer stop()

	for _, serverName := range []string{"", "wrong.invalid"} {
		conn, err := dialSite(addr, serverName)
		if err != nil {
			t.Fatalf("handshake with SNI %q failed on the generated certificate: %v", serverName, err)
		}
		conn.Close()
	}
}

func TestFileConfigStrictSNIMapping(t *testing.T) {
	cases := []struct {
		name     string
		fronting string
		want     bool
	}{
		{"omitted", `{"enabled": true}`, true},
		{"true", `{"enabled": true, "strict_sni": true}`, true},
		{"false", `{"enabled": true, "strict_sni": false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configJSON := `{"server": {"users": [{"name": "default", "password": "test-pass"}], "fronting": ` + tc.fronting + `}}`
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(configJSON), 0644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			fileConfig, err := configfile.LoadFile(path)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if got := fileConfig.ToServerConfig().Fronting.StrictSNIEnabled(); got != tc.want {
				t.Fatalf("StrictSNIEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
