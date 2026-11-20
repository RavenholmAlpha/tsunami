package fronting

import (
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
