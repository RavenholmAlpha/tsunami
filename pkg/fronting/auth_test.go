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
