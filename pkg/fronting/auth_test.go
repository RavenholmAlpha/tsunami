package fronting

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSignAndVerifyRequest(t *testing.T) {
	key := KeyFromSecret("secret")
	now := time.Unix(1000, 0)

	req, err := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	if err := SignRequest(req, key, now); err != nil {
		t.Fatalf("sign request: %v", err)
	}
	if !VerifyRequest(req, [][32]byte{key}, now, ClockSkew) {
		t.Fatal("signed request did not verify")
	}
	for name := range req.Header {
		if strings.Contains(strings.ToLower(name), "tsunami") {
			t.Fatalf("signed request leaked project marker header %q", name)
		}
	}
}

func TestVerifyRequestRejectsWrongHost(t *testing.T) {
	key := KeyFromSecret("secret")
	now := time.Unix(1000, 0)

	req, err := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	if err := SignRequest(req, key, now); err != nil {
		t.Fatalf("sign request: %v", err)
	}

	req.Host = "probe.example.com"
	if VerifyRequest(req, [][32]byte{key}, now, ClockSkew) {
		t.Fatal("request with modified host verified")
	}
}

func TestVerifyRequestRejectsExpiredTimestamp(t *testing.T) {
	key := KeyFromSecret("secret")
	now := time.Unix(1000, 0)

	req, err := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	if err := SignRequest(req, key, now); err != nil {
		t.Fatalf("sign request: %v", err)
	}

	if VerifyRequest(req, [][32]byte{key}, now.Add(10*time.Minute), ClockSkew) {
		t.Fatal("expired request verified")
	}
}

func TestVerifyRequestRejectsReplay(t *testing.T) {
	key := KeyFromSecret("secret")
	now := time.Unix(1000, 0)
	cache := NewNonceCache(5 * time.Minute)

	req, err := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	if err := SignRequest(req, key, now); err != nil {
		t.Fatalf("sign request: %v", err)
	}

	if !VerifyRequest(req, [][32]byte{key}, now, ClockSkew, cache) {
		t.Fatal("first request should verify")
	}
	if VerifyRequest(req, [][32]byte{key}, now, ClockSkew, cache) {
		t.Fatal("replayed request should be rejected")
	}
}

func TestSignAndVerifySplitHeaders(t *testing.T) {
	key := KeyFromSecret("split-secret")
	now := time.Unix(2000, 0)

	req, err := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	req.Header.Set(HeaderSessionID, "0123456789abcdef0123456789abcdef")
	req.Header.Set(HeaderStreamSeq, "7")
	if err := SignRequest(req, key, now); err != nil {
		t.Fatalf("sign split request: %v", err)
	}
	if !VerifyRequest(req, [][32]byte{key}, now, ClockSkew) {
		t.Fatal("signed split request failed verification")
	}

	// Mutating sequence or session id must fail MAC verification.
	tamperedSeq := req.Clone(req.Context())
	tamperedSeq.Header.Set(HeaderStreamSeq, "8")
	if VerifyRequest(tamperedSeq, [][32]byte{key}, now, ClockSkew) {
		t.Fatal("tampered stream seq verified")
	}

	tamperedSID := req.Clone(req.Context())
	tamperedSID.Header.Set(HeaderSessionID, "fedcba9876543210fedcba9876543210")
	if VerifyRequest(tamperedSID, [][32]byte{key}, now, ClockSkew) {
		t.Fatal("tampered session id verified")
	}

	// Seq without session id must be rejected even if signed.
	orphanSeq, _ := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	orphanSeq.Host = "example.com"
	orphanSeq.Header.Set(HeaderStreamSeq, "0")
	if err := SignRequest(orphanSeq, key, now); err != nil {
		t.Fatal(err)
	}
	if VerifyRequest(orphanSeq, [][32]byte{key}, now, ClockSkew) {
		t.Fatal("seq without session id verified")
	}

	StripAuthHeaders(req.Header)
	for _, h := range []string{headerVersion, headerDate, headerNonce, headerAuth, HeaderSessionID, HeaderStreamSeq} {
		if req.Header.Get(h) != "" {
			t.Fatalf("StripAuthHeaders left %q=%q", h, req.Header.Get(h))
		}
	}
}

func TestNonceCacheFIFOEvictionExpiryAndUppercaseHex(t *testing.T) {
	cache := NewNonceCache(40 * time.Millisecond)
	cache.maxEntries = 3

	for _, n := range []string{"nonce-1-000000000000", "nonce-2-000000000000", "nonce-3-000000000000"} {
		if !cache.Add(n) {
			t.Fatalf("initial Add(%q) failed", n)
		}
	}
	// Adding a 4th entry must evict the oldest entry (nonce-1) in FIFO order.
	if !cache.Add("nonce-4-000000000000") {
		t.Fatal("Add 4th nonce failed")
	}
	if !cache.Add("nonce-1-000000000000") {
		t.Fatal("oldest evicted nonce-1 should be accepted again after FIFO eviction")
	}
	if cache.Add("nonce-4-000000000000") {
		t.Fatal("recent nonce-4 should still be blocked as replay")
	}

	time.Sleep(60 * time.Millisecond)
	if !cache.Add("nonce-4-000000000000") {
		t.Fatal("expired nonce-4 should be accepted after TTL")
	}

	// VerifyRequest must accept uppercase hex MAC signatures too.
	key := KeyFromSecret("case-secret")
	now := time.Unix(2000, 0)
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/assets/update", nil)
	req.Host = "EXAMPLE.COM"
	if err := SignRequest(req, key, now); err != nil {
		t.Fatal(err)
	}
	sig := strings.TrimPrefix(req.Header.Get(headerAuth), "HMAC-SHA256 Signature=")
	req.Header.Set(headerAuth, "HMAC-SHA256 Signature="+strings.ToUpper(sig))
	if !VerifyRequest(req, [][32]byte{key}, now, ClockSkew) {
		t.Fatal("uppercase hex MAC signature rejected")
	}
}

func TestComputeMACBytesMatchesStdlibHMAC(t *testing.T) {
	key := KeyFromSecret("hmac-cross-check-secret")
	cases := []struct {
		name                             string
		method, path, host, ts, nonce    string
		sid, seq                         string
	}{
		{"get", http.MethodGet, "/assets/main.js", "example.com", "1700000", "nonce-1234567890abcdef", "", ""},
		{"split-h2-upper-host", http.MethodPost, "/assets/update", "CDN.EXAMPLE.ORG", "1700001", "nonce-fedcba9876543210", "0123456789abcdef", "42"},
		{"long-path-heap-fallback", http.MethodPost, "/" + strings.Repeat("p", 640), "Long.Host.Example", "1700002", "nonce-longpath123456", "abcdef0123456789", "999"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeMACBytes(tc.method, tc.path, tc.host, tc.ts, tc.nonce, tc.sid, tc.seq, key)

			canonical := strings.Join([]string{tc.method, tc.path, strings.ToLower(tc.host), tc.ts, tc.nonce}, "\n")
			if tc.sid != "" || tc.seq != "" {
				canonical += "\n" + tc.sid + "\n" + tc.seq
			}
			mac := hmac.New(sha256.New, key[:])
			_, _ = mac.Write([]byte(canonical))
			var want [sha256.Size]byte
			mac.Sum(want[:0])

			if got != want {
				t.Fatalf("computeMACBytes mismatch:\n got=%x\nwant=%x", got, want)
			}
		})
	}
}
