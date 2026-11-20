package fronting

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

func BenchmarkNonceCacheAdd(b *testing.B) {
	const prefill = 20000
	cache := NewNonceCache(5 * time.Minute)
	var pre [16]byte
	var hexBuf [32]byte
	for i := 0; i < prefill; i++ {
		for k := 0; k < 8; k++ {
			pre[k] = byte(i >> (k * 8))
		}
		pre[15] = 0xAA
		hex.Encode(hexBuf[:], pre[:])
		cache.Add(string(hexBuf[:]))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for k := 0; k < 8; k++ {
			pre[k] = byte(i >> (k * 8))
		}
		pre[15] = 0xBB
		hex.Encode(hexBuf[:], pre[:])
		if !cache.Add(string(hexBuf[:])) {
			b.Fatalf("unexpected duplicate at %d", i)
		}
	}
}

func BenchmarkSignAndVerifySplitRequest(b *testing.B) {
	key := KeyFromSecret("bench-fronting-secret-0123456789")
	keys := [][32]byte{key}
	now := time.Unix(1700000000, 0)

	req, err := http.NewRequest(http.MethodPost, "https://updates.example.net/assets/update", nil)
	if err != nil {
		b.Fatal(err)
	}
	req.Host = "updates.example.net"
	req.Header.Set(HeaderSessionID, "0123456789abcdef0123456789abcdef")
	req.Header.Set(HeaderStreamSeq, "42")

	b.Run("Sign", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			req.Header.Set(HeaderStreamSeq, strconv.Itoa(i))
			if err := SignRequest(req, key, now); err != nil {
				b.Fatal(err)
			}
		}
	})

	if err := SignRequest(req, key, now); err != nil {
		b.Fatal(err)
	}
	b.Run("Verify", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if !VerifyRequest(req, keys, now, ClockSkew) {
				b.Fatal("verify failed")
			}
		}
	})
}

func BenchmarkHTTPClientConnSetWriteDeadline(b *testing.B) {
	pr, pw := io.Pipe()
	defer pr.Close()
	respCh := make(chan HTTPResponseResult, 1)
	conn := NewPendingHTTPClientConnWithCleanup(respCh, pw, "example.com:443", nil, nil)
	defer conn.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			b.Fatal(err)
		}
		if err := conn.SetWriteDeadline(time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
}

func generateSelfSignedForBench(b *testing.B, dnsName string) tls.Certificate {
	b.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		b.Fatalf("generate key: %v", err)
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
		b.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func BenchmarkSplitH2Throughput(b *testing.B) {
	cert := generateSelfSignedForBench(b, "localhost")
	tlsCfg := CaddyLikeTLSConfig([]tls.Certificate{cert})
	reg := NewSplitSessionRegistry()
	b.Cleanup(reg.CloseAll)

	key := KeyFromSecret("split-bench-secret")
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
			defer conn.Close()
			buf := make([]byte, 64*1024)
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
			body, err := io.ReadAll(io.LimitReader(r.Body, MaxSplitChunkSize+1))
			if err != nil || len(body) > MaxSplitChunkSize {
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
		b.Fatalf("listen: %v", err)
	}
	b.Cleanup(func() {
		_ = ln.Close()
		_ = srv.Close()
	})
	go func() { _ = srv.Serve(tls.NewListener(ln, tlsCfg)) }()

	rawTCP, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	uconn := utls.UClient(rawTCP, &utls.Config{
		ServerName:         "localhost",
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		MinVersion:         tls.VersionTLS13,
	}, utls.HelloChrome_Auto)
	if err := uconn.Handshake(); err != nil {
		b.Fatalf("uTLS handshake: %v", err)
	}

	h2Tr := &http2.Transport{}
	cc, err := h2Tr.NewClientConn(uconn)
	if err != nil {
		b.Fatalf("NewClientConn: %v", err)
	}
	endpoint, _ := url.Parse("https://" + ln.Addr().String() + "/assets/update")
	clientConn, err := NewSplitH2ClientConn(endpoint, "localhost", key, "chrome", cc.RoundTrip, func() {
		_ = cc.Close()
		h2Tr.CloseIdleConnections()
	})
	if err != nil {
		b.Fatalf("NewSplitH2ClientConn: %v", err)
	}
	b.Cleanup(func() { _ = clientConn.Close() })

	const payloadSize = 1 << 20 // 1 MiB duplex echo per iteration (2 MiB transferred)
	sendBuf := bytes.Repeat([]byte("S"), payloadSize)
	recvBuf := make([]byte, payloadSize)

	b.SetBytes(int64(payloadSize * 2))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		var writeErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, writeErr = clientConn.Write(sendBuf)
		}()
		if _, err := io.ReadFull(clientConn, recvBuf); err != nil {
			b.Fatalf("ReadFull: %v", err)
		}
		wg.Wait()
		if writeErr != nil {
			b.Fatalf("Write: %v", writeErr)
		}
	}
	b.StopTimer()
}
