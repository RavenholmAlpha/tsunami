package integration

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsunami-protocol/tsunami/pkg/client"
	configfile "github.com/tsunami-protocol/tsunami/pkg/config"
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
		AllowAll:  true,
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
		if r.Header.Get("Authorization") != "" {
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
	req.Header.Set("Authorization", "HMAC-SHA256 Signature=probe-value")
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

func TestFrontingRejectsBadOrReplayedHMACAsDecoy(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "cover response:"+r.Method+":"+r.URL.Path)
	}))
	defer origin.Close()

	const frontSecret = "fronting-test-secret"
	serverAddr, stopServer := startFrontingTsunamiServerWithFronting(t, fronting.Config{
		Enabled:    true,
		Path:       "/assets/update",
		Secret:     frontSecret,
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

	newPost := func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, "https://"+serverAddr+"/assets/update", nil)
		if err != nil {
			t.Fatalf("new fronting POST: %v", err)
		}
		req.Host = "localhost"
		return req
	}
	do := func(label string, req *http.Request) (int, string) {
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("%s request: %v", label, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s response: %v", label, err)
		}
		return resp.StatusCode, string(body)
	}

	coverStatus, coverBody := do("ordinary decoy", newPost())
	if coverStatus != http.StatusAccepted {
		t.Fatalf("ordinary decoy status = %d, want %d", coverStatus, http.StatusAccepted)
	}

	bad := newPost()
	if err := fronting.SignRequest(bad, fronting.KeyFromSecret(frontSecret), time.Now()); err != nil {
		t.Fatalf("sign bad request: %v", err)
	}
	bad.Header.Set("Authorization", "HMAC-SHA256 Signature=0000000000000000000000000000000000000000000000000000000000000000")
	if status, body := do("bad HMAC", bad); status != coverStatus || body != coverBody {
		t.Fatalf("bad HMAC response = (%d, %q), want decoy (%d, %q)", status, body, coverStatus, coverBody)
	}

	signed := newPost()
	if err := fronting.SignRequest(signed, fronting.KeyFromSecret(frontSecret), time.Now()); err != nil {
		t.Fatalf("sign request: %v", err)
	}
	if status, _ := do("initial signed request", signed); status != http.StatusOK {
		t.Fatalf("initial signed request status = %d, want %d", status, http.StatusOK)
	}

	replayed := newPost()
	replayed.Header = signed.Header.Clone()
	if status, body := do("replayed HMAC", replayed); status != coverStatus || body != coverBody {
		t.Fatalf("replayed HMAC response = (%d, %q), want decoy (%d, %q)", status, body, coverStatus, coverBody)
	}
}

func TestCLIFrontingDefaultsToEnabled(t *testing.T) {
	for _, command := range []string{"./cmd/tsunami-server", "./cmd/tsunami-client"} {
		t.Run(command, func(t *testing.T) {
			cmd := exec.Command("go", "run", command, "--help")
			cmd.Dir = ".."
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("run %s --help: %v\n%s", command, err, out)
			}
			if !regexp.MustCompile(`(?s)-fronting\s+.*?\(default true\)`).Match(out) {
				t.Fatalf("%s --help does not show --fronting enabled by default:\n%s", command, out)
			}
		})
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

// --- Site camouflage (site reverse proxy) integration tests ---

// capturedSiteRequest is one request observed by a site camouflage upstream.
type capturedSiteRequest struct {
	method     string
	host       string
	requestURI string
	header     http.Header
}

// siteOrigin is a fake cover website used as the site camouflage upstream.
type siteOrigin struct {
	server   *httptest.Server
	requests chan capturedSiteRequest
}

func newSiteOrigin(t *testing.T, useTLS bool) *siteOrigin {
	t.Helper()
	origin := &siteOrigin{requests: make(chan capturedSiteRequest, 16)}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case origin.requests <- capturedSiteRequest{
			method:     r.Method,
			host:       r.Host,
			requestURI: r.URL.RequestURI(),
			header:     r.Header.Clone(),
		}:
		default:
		}
		w.Header().Set("Alt-Svc", `h3=":443"; ma=86400`)
		w.Header().Set("X-Site-Origin", "origin")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "origin:"+r.Method+":"+r.URL.RequestURI())
	})
	if useTLS {
		origin.server = httptest.NewTLSServer(handler)
	} else {
		origin.server = httptest.NewServer(handler)
	}
	t.Cleanup(origin.server.Close)
	return origin
}

func (o *siteOrigin) url() string { return o.server.URL }

func (o *siteOrigin) next(t *testing.T) capturedSiteRequest {
	t.Helper()
	select {
	case request := <-o.requests:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("site camouflage upstream received no request")
		return capturedSiteRequest{}
	}
}

func (o *siteOrigin) pending() int { return len(o.requests) }

// newSiteHTTPClient returns an HTTP/2 client that trusts the fronting
// server's self-signed certificate.
func newSiteHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			ServerName:         "localhost",
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		},
		ForceAttemptHTTP2: true,
	}
	if err := http2.ConfigureTransport(tr); err != nil {
		t.Fatalf("configure site h2 transport: %v", err)
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func startFrontingSiteServer(t *testing.T, site fronting.SiteConfig, secret string) string {
	t.Helper()
	serverAddr, stopServer := startFrontingTsunamiServerWithFronting(t, fronting.Config{
		Enabled:  true,
		Path:     "/assets/update",
		Secret:   secret,
		SiteName: "Front Site",
		Site:     site,
	})
	t.Cleanup(stopServer)
	return serverAddr
}

func TestFrontingSiteLocalModeProxiesOrigin(t *testing.T) {
	origin := newSiteOrigin(t, false)
	serverAddr := startFrontingSiteServer(t, fronting.SiteConfig{
		Mode:     fronting.SiteModeLocal,
		Upstream: origin.url(),
	}, "")

	httpClient := newSiteHTTPClient(t)
	req, err := http.NewRequest(http.MethodGet, "https://"+serverAddr+"/assets/app.js?v=2", nil)
	if err != nil {
		t.Fatalf("new site request: %v", err)
	}
	// Fronting-only metadata must never reach the upstream.
	req.Header.Set("Authorization", "HMAC-SHA256 Signature=probe-value")
	req.Header.Set("X-Api-Version", "v1")
	req.Header.Set("X-Date", "1700000000")
	req.Header.Set("X-Trace-Id", "0123456789abcdef")

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("site proxy GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read site proxy body: %v", err)
	}

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("site proxy status = %d, want 202 (origin status)", resp.StatusCode)
	}
	if got, want := string(body), "origin:GET:/assets/app.js?v=2"; got != want {
		t.Fatalf("site proxy body = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Server"); got != fronting.DefaultServerHeader {
		t.Fatalf("site proxy Server header = %q, want %q", got, fronting.DefaultServerHeader)
	}
	// A Caddy-like front emits exactly one Server header; ReverseProxy copies
	// upstream headers with Add, so the pre-set fronting header must be removed.
	if values := resp.Header.Values("Server"); len(values) != 1 {
		t.Fatalf("site proxy emitted %d Server headers (%v), want exactly 1", len(values), values)
	}
	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Fatalf("site proxy leaked Alt-Svc %q", got)
	}

	upstreamRequest := origin.next(t)
	if upstreamRequest.host != serverAddr {
		t.Fatalf("upstream Host = %q, want preserved client host %q", upstreamRequest.host, serverAddr)
	}
	for _, header := range []string{"Authorization", "X-Api-Version", "X-Date", "X-Trace-Id"} {
		if got := upstreamRequest.header.Get(header); got != "" {
			t.Fatalf("fronting header %s leaked to upstream: %q", header, got)
		}
	}
	if got := upstreamRequest.header.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want https", got)
	}
	if got := upstreamRequest.header.Get("X-Forwarded-Host"); got != serverAddr {
		t.Fatalf("X-Forwarded-Host = %q, want %q", got, serverAddr)
	}
	if got := upstreamRequest.header.Get("X-Forwarded-For"); got == "" {
		t.Fatal("X-Forwarded-For was not set for the upstream request")
	}
}

func TestFrontingSiteRemoteModeHostHandling(t *testing.T) {
	origin := newSiteOrigin(t, true) // HTTPS, so the proxy transport needs to skip verification
	upstreamHost := strings.TrimPrefix(origin.url(), "https://")
	preserveHost := true

	t.Run("remote rewrites host to the upstream", func(t *testing.T) {
		serverAddr := startFrontingSiteServer(t, fronting.SiteConfig{
			Mode:               fronting.SiteModeRemote,
			Upstream:           origin.url(),
			InsecureSkipVerify: true,
		}, "")

		httpClient := newSiteHTTPClient(t)
		resp, err := httpClient.Get("https://" + serverAddr + "/neighbour")
		if err != nil {
			t.Fatalf("remote site GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusAccepted || string(body) != "origin:GET:/neighbour" {
			t.Fatalf("remote site response = (%d, %q)", resp.StatusCode, string(body))
		}
		if got := resp.Header.Get("Server"); got != fronting.DefaultServerHeader {
			t.Fatalf("remote site Server header = %q, want %q", got, fronting.DefaultServerHeader)
		}

		request := origin.next(t)
		if request.host != upstreamHost {
			t.Fatalf("upstream Host = %q, want rewritten upstream %q", request.host, upstreamHost)
		}
	})

	t.Run("preserve_host keeps the client host", func(t *testing.T) {
		serverAddr := startFrontingSiteServer(t, fronting.SiteConfig{
			Mode:               fronting.SiteModeRemote,
			Upstream:           origin.url(),
			PreserveHost:       &preserveHost,
			InsecureSkipVerify: true,
		}, "")

		httpClient := newSiteHTTPClient(t)
		resp, err := httpClient.Get("https://" + serverAddr + "/neighbour")
		if err != nil {
			t.Fatalf("remote site GET: %v", err)
		}
		resp.Body.Close()

		request := origin.next(t)
		if request.host != serverAddr {
			t.Fatalf("upstream Host = %q, want preserved client host %q", request.host, serverAddr)
		}
	})
}

func TestFrontingSiteUpstreamFailureServesBuiltinPage(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	upstream := origin.URL
	origin.Close() // the cover site is down

	serverAddr := startFrontingSiteServer(t, fronting.SiteConfig{
		Mode:     fronting.SiteModeLocal,
		Upstream: upstream,
	}, "")

	httpClient := newSiteHTTPClient(t)
	for _, path := range []string{"/", "/deep/link?x=1"} {
		resp, err := httpClient.Get("https://" + serverAddr + path)
		if err != nil {
			t.Fatalf("site failure fallback GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("site failure fallback %s status = %d, want 200", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), "Front Site") {
			t.Fatalf("site failure fallback %s body missing site name: %q", path, string(body))
		}
		if strings.Contains(string(body), "502") || strings.Contains(string(body), "Bad Gateway") {
			t.Fatalf("site failure fallback leaked proxy error: %q", string(body))
		}
		if got := resp.Header.Get("Server"); got != fronting.DefaultServerHeader {
			t.Fatalf("site failure fallback Server header = %q, want %q", got, fronting.DefaultServerHeader)
		}
	}
}

func TestFrontingSiteLocalModeKeepsTunnelPath(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	origin := newSiteOrigin(t, false)
	serverAddr := startFrontingSiteServer(t, fronting.SiteConfig{
		Mode:     fronting.SiteModeLocal,
		Upstream: origin.url(),
	}, "")

	// Unauthenticated probes are served by the cover site.
	httpClient := newSiteHTTPClient(t)
	resp, err := httpClient.Get("https://" + serverAddr + "/")
	if err != nil {
		t.Fatalf("local site GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || string(body) != "origin:GET:/" {
		t.Fatalf("cover site response = (%d, %q)", resp.StatusCode, string(body))
	}
	origin.next(t)

	// The authenticated tunnel still wins over the site handler.
	c, err := client.New(frontingClientConfig(serverAddr, fronting.TransportH2))
	if err != nil {
		t.Fatalf("create fronting client: %v", err)
	}
	defer c.Close()

	stream, err := c.OpenStream(echoAddr)
	if err != nil {
		t.Fatalf("open fronting stream: %v", err)
	}
	defer stream.Close()

	msg := []byte("site camouflage tunnel")
	if _, err := stream.Write(msg); err != nil {
		t.Fatalf("write fronting stream: %v", err)
	}
	buf := make([]byte, 256)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("read fronting stream: %v", err)
	}
	if got := string(buf[:n]); got != string(msg) {
		t.Fatalf("tunnel echo = %q, want %q", got, msg)
	}
	if origin.pending() != 0 {
		t.Fatalf("authenticated tunnel traffic leaked to the site upstream (%d requests)", origin.pending())
	}
}

func TestFrontingSiteBadHMACPostIsProxied(t *testing.T) {
	origin := newSiteOrigin(t, false)
	const frontSecret = "fronting-site-secret"
	serverAddr := startFrontingSiteServer(t, fronting.SiteConfig{
		Mode:     fronting.SiteModeLocal,
		Upstream: origin.url(),
	}, frontSecret)

	httpClient := newSiteHTTPClient(t)
	newPost := func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, "https://"+serverAddr+"/assets/update", nil)
		if err != nil {
			t.Fatalf("new fronting POST: %v", err)
		}
		req.Host = "localhost"
		return req
	}

	// A POST with a bad HMAC is ordinary traffic: proxy it, exactly like Caddy.
	bad := newPost()
	if err := fronting.SignRequest(bad, fronting.KeyFromSecret(frontSecret), time.Now()); err != nil {
		t.Fatalf("sign bad request: %v", err)
	}
	bad.Header.Set("Authorization", "HMAC-SHA256 Signature="+strings.Repeat("0", 64))

	resp, err := httpClient.Do(bad)
	if err != nil {
		t.Fatalf("bad HMAC POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || string(body) != "origin:POST:/assets/update" {
		t.Fatalf("bad HMAC POST response = (%d, %q), want the cover site response", resp.StatusCode, string(body))
	}
	proxied := origin.next(t)
	if proxied.method != http.MethodPost || proxied.requestURI != "/assets/update" {
		t.Fatalf("proxied request = %s %s", proxied.method, proxied.requestURI)
	}
	if got := proxied.header.Get("Authorization"); got != "" {
		t.Fatalf("fronting auth header leaked to the cover site: %q", got)
	}

	// A correctly signed POST still opens the tunnel and is never proxied.
	signed := newPost()
	if err := fronting.SignRequest(signed, fronting.KeyFromSecret(frontSecret), time.Now()); err != nil {
		t.Fatalf("sign request: %v", err)
	}
	tunnelResp, err := httpClient.Do(signed)
	if err != nil {
		t.Fatalf("signed POST: %v", err)
	}
	if tunnelResp.StatusCode != http.StatusOK {
		t.Fatalf("signed POST status = %d, want 200 tunnel", tunnelResp.StatusCode)
	}
	tunnelResp.Body.Close()
	if origin.pending() != 0 {
		t.Fatalf("authenticated tunnel POST leaked to the site upstream")
	}
}

func TestFrontingSiteWebSocketUpgradePassthrough(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fronting.IsWebSocketUpgrade(r) {
			http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
			return
		}
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			return
		}
		if err := buffered.Flush(); err != nil {
			return
		}
		// Echo upgraded frames back so the test can prove the tunnel is live.
		_, _ = io.Copy(conn, buffered)
	}))
	defer origin.Close()

	serverAddr := startFrontingSiteServer(t, fronting.SiteConfig{
		Mode:     fronting.SiteModeLocal,
		Upstream: origin.URL,
	}, "")

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", serverAddr, &tls.Config{
		ServerName:         "localhost",
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"http/1.1"},
	})
	if err != nil {
		t.Fatalf("dial fronting server: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	upgradeRequest := "GET /live HTTP/1.1\r\n" +
		"Host: " + serverAddr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, upgradeRequest); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgrade status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("websocket upgrade status = %q, want 101", strings.TrimSpace(statusLine))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read upgrade headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write upgraded payload: %v", err)
	}
	echo := make([]byte, len("ping"))
	if _, err := io.ReadFull(reader, echo); err != nil {
		t.Fatalf("read upgraded payload: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("websocket echo = %q, want %q", string(echo), "ping")
	}
}

func TestFileConfigSiteCamouflageMapping(t *testing.T) {
	configJSON := `{
  "server": {
    "users": [{"name": "default", "password": "test-pass"}],
    "fronting": {
      "enabled": true,
      "path": "/static/js/main.1a2b3c4d.js",
      "secret": "front-secret",
      "site_name": "Example",
      "site": {
        "mode": "local",
        "upstream": "http://127.0.0.1:8080",
        "preserve_host": false,
        "insecure_skip_verify": true
      }
    }
  }
}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(configJSON), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	fileConfig, err := configfile.LoadFile(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	serverConfig := fileConfig.ToServerConfig()
	site := serverConfig.Fronting.Site
	if site.Mode != fronting.SiteModeLocal || site.Upstream != "http://127.0.0.1:8080" {
		t.Fatalf("mapped site = %+v", site)
	}
	if site.PreserveHost == nil || *site.PreserveHost {
		t.Fatalf("preserve_host pointer not mapped: %+v", site.PreserveHost)
	}
	if !site.InsecureSkipVerify {
		t.Fatal("insecure_skip_verify not mapped")
	}

	// The deprecated decoy_proxy key still maps to a reverse proxy and keeps
	// the pre-existing Host-preserving behavior.
	legacyJSON := `{
  "server": {
    "users": [{"name": "default", "password": "test-pass"}],
    "fronting": {"enabled": true, "decoy_proxy": "http://127.0.0.1:8081"}
  }
}`
	legacyPath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(legacyJSON), 0644); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	legacyConfig, err := configfile.LoadFile(legacyPath)
	if err != nil {
		t.Fatalf("load legacy config: %v", err)
	}
	legacyServerConfig := legacyConfig.ToServerConfig()
	if legacyServerConfig.Fronting.DecoyProxy != "http://127.0.0.1:8081" {
		t.Fatalf("legacy decoy_proxy = %q", legacyServerConfig.Fronting.DecoyProxy)
	}
	legacyServerConfig.Fronting.Normalize()
	if err := legacyServerConfig.Fronting.SiteError(); err != nil {
		t.Fatalf("legacy decoy_proxy config rejected: %v", err)
	}
	if got := legacyServerConfig.Fronting.Site.Mode; got != fronting.SiteModeLocal {
		t.Fatalf("legacy decoy_proxy mode = %q, want local", got)
	}
	if !legacyServerConfig.Fronting.PreserveHostEnabled() {
		t.Fatal("legacy decoy_proxy must keep preserving the client Host header")
	}
}

func TestServerNewRejectsInvalidSiteConfig(t *testing.T) {
	_, err := server.New(server.Config{
		Users: []*protocol.UserInfo{{Name: "site-user", Password: testPassword}},
		Fronting: fronting.Config{
			Enabled: true,
			Site:    fronting.SiteConfig{Mode: fronting.SiteModeLocal},
		},
	})
	if err == nil {
		t.Fatal("server.New accepted a local site without an upstream")
	}
	if !strings.Contains(err.Error(), "site") {
		t.Fatalf("server.New error = %v, want a site camouflage message", err)
	}
}

// TestFrontingSiteFrontingPathFallbacks verifies that requests which pass the
// fronting HMAC but cannot start a tunnel (a non-HTTP/2 POST, or a broken
// WebSocket handshake) are handed to the site handler instead of the built-in
// page.
func TestFrontingSiteFrontingPathFallbacks(t *testing.T) {
	origin := newSiteOrigin(t, false)
	const frontSecret = "fronting-fallback-secret"
	serverAddr := startFrontingSiteServer(t, fronting.SiteConfig{
		Mode:     fronting.SiteModeLocal,
		Upstream: origin.url(),
	}, frontSecret)

	doHTTP1 := func(t *testing.T, prepare func(*http.Request)) (int, string) {
		t.Helper()
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp", serverAddr, &tls.Config{
			ServerName:         "localhost",
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
			NextProtos:         []string{"http/1.1"},
		})
		if err != nil {
			t.Fatalf("dial fronting server: %v", err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatalf("set deadline: %v", err)
		}

		req, err := http.NewRequest(http.MethodGet, "https://"+serverAddr+"/assets/update", nil)
		if err != nil {
			t.Fatalf("new fallback request: %v", err)
		}
		req.Host = "localhost"
		prepare(req)
		if err := req.Write(conn); err != nil {
			t.Fatalf("write fallback request: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			t.Fatalf("read fallback response: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read fallback body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	t.Run("non-HTTP/2 signed POST is proxied", func(t *testing.T) {
		status, body := doHTTP1(t, func(req *http.Request) {
			req.Method = http.MethodPost
			req.ContentLength = 0
			if err := fronting.SignRequest(req, fronting.KeyFromSecret(frontSecret), time.Now()); err != nil {
				t.Fatalf("sign request: %v", err)
			}
		})
		if status != http.StatusAccepted || body != "origin:POST:/assets/update" {
			t.Fatalf("fallback response = (%d, %q), want the cover site response", status, body)
		}
		if proxied := origin.next(t); proxied.method != http.MethodPost {
			t.Fatalf("proxied method = %s, want POST", proxied.method)
		}
	})

	t.Run("broken websocket upgrade is proxied", func(t *testing.T) {
		status, body := doHTTP1(t, func(req *http.Request) {
			if err := fronting.SignRequest(req, fronting.KeyFromSecret(frontSecret), time.Now()); err != nil {
				t.Fatalf("sign request: %v", err)
			}
			// Upgrade without Sec-WebSocket-Key: the TSUNAMI upgrade fails, so
			// the request must fall through to the cover site.
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Connection", "Upgrade")
		})
		if status != http.StatusAccepted || body != "origin:GET:/assets/update" {
			t.Fatalf("fallback response = (%d, %q), want the cover site response", status, body)
		}
		if proxied := origin.next(t); proxied.requestURI != "/assets/update" {
			t.Fatalf("proxied request URI = %q", proxied.requestURI)
		}
	})
}
