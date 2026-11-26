package fronting

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

func TestUserAgentForFingerprint(t *testing.T) {
	if got := UserAgentForFingerprint(""); !strings.Contains(got, "Chrome/133") {
		t.Fatalf("default UA = %q, want Chrome/133", got)
	}
	if got := UserAgentForFingerprint("chrome"); !strings.Contains(got, "Chrome/133") {
		t.Fatalf("chrome UA = %q", got)
	}
	if got := UserAgentForFingerprint("firefox"); !strings.Contains(got, "Firefox/120") {
		t.Fatalf("firefox UA = %q", got)
	}
	if got := UserAgentForFingerprint("safari"); !strings.Contains(got, "Safari/") || !strings.Contains(got, "Version/16.0") {
		t.Fatalf("safari UA = %q", got)
	}

	h := make(http.Header)
	ApplyBrowserHeaders(h, "chrome")
	if h.Get("Sec-Fetch-Mode") != "cors" {
		t.Fatalf("chrome headers missing Sec-Fetch-Mode: %v", h)
	}
	hFF := make(http.Header)
	ApplyBrowserHeaders(hFF, "firefox")
	if hFF.Get("Sec-Fetch-Mode") != "" {
		t.Fatalf("firefox headers unexpectedly included Sec-Fetch-Mode: %v", hFF)
	}
}

func TestSplitSessionRegistryReordersOutOfOrderUplinks(t *testing.T) {
	reg := NewSplitSessionRegistry()
	defer reg.CloseAll()

	sid := "0123456789abcdef0123456789abcdef"
	ctx := context.Background()

	// seq > 0 without an existing session must be rejected.
	if err := reg.DeliverUplink(ctx, sid, 1, []byte("orphan")); !errors.Is(err, errSplitSessionNotFound) {
		t.Fatalf("seq=1 without session = %v, want errSplitSessionNotFound", err)
	}

	// seq=0 arrives slightly before GET AttachDownlink.
	if err := reg.DeliverUplink(ctx, sid, 0, []byte("part0-")); err != nil {
		t.Fatalf("deliver seq 0: %v", err)
	}
	// Duplicate seq=0 must be rejected.
	if err := reg.DeliverUplink(ctx, sid, 0, []byte("dup")); !errors.Is(err, errDuplicateSplitSeq) {
		t.Fatalf("duplicate seq=0 = %v, want errDuplicateSplitSeq", err)
	}
	// Too-large gap must be rejected.
	if err := reg.DeliverUplink(ctx, sid, 100, []byte("gap")); !errors.Is(err, errSplitGapTooLarge) {
		t.Fatalf("large gap = %v, want errSplitGapTooLarge", err)
	}

	// Deliver seq=2 before seq=1.
	if err := reg.DeliverUplink(ctx, sid, 2, []byte("part2")); err != nil {
		t.Fatalf("deliver seq 2: %v", err)
	}

	bw := newBufferedFlushWriter(4096)
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/assets/update", nil)
	conn, err := reg.AttachDownlink(sid, bw, req)
	if err != nil {
		t.Fatalf("AttachDownlink: %v", err)
	}
	defer conn.Close()

	// Reading now gets part0-, then waits for seq=1 before yielding part2.
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "part0-" {
		t.Fatalf("first Read = (%q, %v), want %q", buf[:n], err, "part0-")
	}

	if err := reg.DeliverUplink(ctx, sid, 1, []byte("part1-")); err != nil {
		t.Fatalf("deliver seq 1: %v", err)
	}

	var rest bytes.Buffer
	for rest.Len() < len("part1-part2") {
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("Read after seq 1: %v", err)
		}
		rest.Write(buf[:n])
	}
	if rest.String() != "part1-part2" {
		t.Fatalf("reordered tail = %q, want %q", rest.String(), "part1-part2")
	}
}

func TestCaddyLikeTLSConfigNegotiatesPQWithChromeUTLS(t *testing.T) {
	cert := generateSelfSignedForTest(t, "localhost")
	tlsCfg := CaddyLikeTLSConfig([]tls.Certificate{cert})

	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer ln.Close()

	serverCurveCh := make(chan tls.CurveID, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tc := conn.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			return
		}
		serverCurveCh <- tc.ConnectionState().CurveID
	}()

	rawConn, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()

	uconn := utls.UClient(rawConn, &utls.Config{
		ServerName:         "localhost",
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2", "http/1.1"},
		MinVersion:         tls.VersionTLS13,
	}, utls.HelloChrome_Auto)
	if err := uconn.Handshake(); err != nil {
		t.Fatalf("uTLS Chrome_Auto handshake failed against CaddyLikeTLSConfig: %v", err)
	}

	select {
	case curve := <-serverCurveCh:
		if curve != tls.X25519MLKEM768 {
			t.Fatalf("negotiated CurveID = %v, want %v (X25519MLKEM768)", curve, tls.X25519MLKEM768)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server handshake")
	}
}

func TestSplitH2ClientAndServerEndToEndOverHTTP2(t *testing.T) {
	cert := generateSelfSignedForTest(t, "localhost")
	tlsCfg := CaddyLikeTLSConfig([]tls.Certificate{cert})
	reg := NewSplitSessionRegistry()
	defer reg.CloseAll()

	key := KeyFromSecret("split-e2e-secret")
	acceptedConns := make(chan net.Conn, 4)

	mux := http.NewServeMux()
	mux.HandleFunc("/assets/update", func(w http.ResponseWriter, r *http.Request) {
		if !VerifyRequest(r, [][32]byte{key}, time.Now(), ClockSkew) {
			http.NotFound(w, r)
			return
		}
		sid := r.Header.Get(HeaderSessionID)
		if r.Method == http.MethodGet {
			conn, err := reg.AttachDownlink(sid, w, r)
			if err != nil {
				http.Error(w, "bad downlink", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			acceptedConns <- conn
			// Echo server loop
			defer conn.Close()
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					if _, wErr := conn.Write(buf[:n]); wErr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}
		if r.Method == http.MethodPost {
			seq, _ := strconvParseUint(r.Header.Get(HeaderStreamSeq))
			body, err := io.ReadAll(io.LimitReader(r.Body, MaxSplitChunkSize))
			if err != nil {
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			if err := reg.DeliverUplink(r.Context(), sid, seq, body); err != nil {
				http.Error(w, "deliver uplink", http.StatusBadRequest)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
		}
	})

	srv := &http.Server{Handler: mux, TLSConfig: tlsCfg}
	_ = http2.ConfigureServer(srv, &http2.Server{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(tls.NewListener(ln, tlsCfg)) }()
	defer srv.Close()

	rawTCP, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	uconn := utls.UClient(rawTCP, &utls.Config{
		ServerName:         "localhost",
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		MinVersion:         tls.VersionTLS13,
	}, utls.HelloChrome_Auto)
	if err := uconn.Handshake(); err != nil {
		t.Fatalf("uTLS handshake: %v", err)
	}

	h2Tr := &http2.Transport{}
	cc, err := h2Tr.NewClientConn(uconn)
	if err != nil {
		t.Fatalf("NewClientConn: %v", err)
	}

	endpoint, _ := url.Parse("https://" + ln.Addr().String() + "/assets/update")
	clientConn, err := NewSplitH2ClientConn(endpoint, "localhost", key, "chrome", cc.RoundTrip, func() {
		_ = cc.Close()
		h2Tr.CloseIdleConnections()
	})
	if err != nil {
		t.Fatalf("NewSplitH2ClientConn: %v", err)
	}
	defer clientConn.Close()

	// Test exact 4096-byte echo + 256 KiB multi-chunk pipelined echo.
	for _, size := range []int{4096, 256 * 1024} {
		payload := bytes.Repeat([]byte{byte(size % 251)}, size)
		go func() {
			_, _ = clientConn.Write(payload)
		}()
		got := make([]byte, size)
		if _, err := io.ReadFull(clientConn, got); err != nil {
			t.Fatalf("ReadFull size=%d: %v", size, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("echo mismatch for size=%d", size)
		}
	}

	// Test read deadline enforcement.
	if err := clientConn.SetReadDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	var tmp [8]byte
	_, err = clientConn.Read(tmp[:])
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read after deadline = %v, want os.ErrDeadlineExceeded", err)
	}

	// Clearing deadline after Reset must not expire spuriously.
	_ = cc.Close()
}

func TestReadSplitUplinkBodyAndClientRingWrap(t *testing.T) {
	// Pooled Content-Length <= 128 KiB path.
	smallPayload := bytes.Repeat([]byte("R"), 64*1024)
	body, poolBuf, err := ReadSplitUplinkBody(bytes.NewReader(smallPayload), int64(len(smallPayload)))
	if err != nil {
		t.Fatalf("ReadSplitUplinkBody small: %v", err)
	}
	if !bytes.Equal(body, smallPayload) {
		t.Fatal("small payload mismatch")
	}
	if poolBuf == nil {
		t.Fatal("expected pooled buffer for <=128 KiB Content-Length")
	}
	RecycleSplitUplinkBody(poolBuf)

	// Reject oversized Content-Length before allocating.
	if _, _, err := ReadSplitUplinkBody(bytes.NewReader(nil), MaxSplitChunkSize+1); err == nil {
		t.Fatal("expected error for oversized Content-Length")
	}

	// Verify SplitH2ClientConn ring buffer wrap-around and reusable write deadline reset.
	reg := NewSplitSessionRegistry()
	defer reg.CloseAll()
	cert := generateSelfSignedForTest(t, "localhost")
	tlsCfg := CaddyLikeTLSConfig([]tls.Certificate{cert})
	key := KeyFromSecret("ring-wrap-secret")

	mux := http.NewServeMux()
	mux.HandleFunc("/assets/update", func(w http.ResponseWriter, r *http.Request) {
		sid := r.Header.Get(HeaderSessionID)
		if r.Method == http.MethodGet {
			conn, err := reg.AttachDownlink(sid, w, r)
			if err != nil {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			defer conn.Close()
			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					if _, wErr := conn.Write(buf[:n]); wErr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}
		seq, _ := strconvParseUint(r.Header.Get(HeaderStreamSeq))
		data, pb, err := ReadSplitUplinkBody(r.Body, r.ContentLength)
		if err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if err := reg.DeliverUplinkPooled(r.Context(), sid, seq, data, pb); err != nil {
			http.Error(w, "bad", http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Handler: mux, TLSConfig: tlsCfg}
	_ = http2.ConfigureServer(srv, &http2.Server{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(tls.NewListener(ln, tlsCfg)) }()
	defer srv.Close()

	rawTCP, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	uconn := utls.UClient(rawTCP, &utls.Config{
		ServerName:         "localhost",
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		MinVersion:         tls.VersionTLS13,
	}, utls.HelloChrome_Auto)
	if err := uconn.Handshake(); err != nil {
		t.Fatal(err)
	}
	h2Tr := &http2.Transport{}
	cc, err := h2Tr.NewClientConn(uconn)
	if err != nil {
		t.Fatal(err)
	}
	ep, _ := url.Parse("https://" + ln.Addr().String() + "/assets/update")
	conn, err := NewSplitH2ClientConn(ep, "localhost", key, "chrome", cc.RoundTrip, func() {
		_ = cc.Close()
		h2Tr.CloseIdleConnections()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Exercise reusable deadline Set -> Clear -> Wait longer than original deadline -> still healthy.
	if err := conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)

	// Transfer > 512 KiB across multiple ring wrap-arounds with byte-pattern check.
	total := 600 * 1024
	want := make([]byte, total)
	for i := range want {
		want[i] = byte(i*17 + i>>8)
	}
	go func() {
		_, _ = conn.Write(want)
	}()
	got := make([]byte, total)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("ReadFull 600 KiB: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("ring wrap-around echo payload mismatch")
	}
}

func TestWebSocketLargeWriteChunkingAndDirectRead(t *testing.T) {
	cRaw, sRaw := net.Pipe()
	defer cRaw.Close()
	defer sRaw.Close()

	clientWS := NewWebSocketConn(cRaw, nil, true)
	serverWS := NewWebSocketConn(sRaw, nil, false)

	// 200 KiB exceeds wsMaxWriteFramePayload (64 KiB) and exercises multi-frame
	// chunking, single-pass xorCopyMaskBytes, and direct-into-p zero-copy RX.
	const total = 200 * 1024
	want := make([]byte, total)
	for i := range want {
		want[i] = byte(i*29 + i>>7)
	}

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, total)
		if _, err := io.ReadFull(serverWS, buf); err != nil {
			errCh <- err
			return
		}
		if _, err := serverWS.Write(buf); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	writeErrCh := make(chan error, 1)
	go func() {
		_, err := clientWS.Write(want)
		writeErrCh <- err
	}()

	got := make([]byte, total)
	if _, err := io.ReadFull(clientWS, got); err != nil {
		t.Fatalf("client ReadFull: %v", err)
	}
	if err := <-writeErrCh; err != nil {
		t.Fatalf("client Write: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("server echo: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("WebSocket 200 KiB chunked echo payload mismatch")
	}
}

func strconvParseUint(s string) (uint64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("invalid uint")
		}
		v = v*10 + uint64(c-'0')
	}
	return v, nil
}

func generateSelfSignedForTest(t *testing.T, dnsName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
